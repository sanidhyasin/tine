// Package image builds and stores the disk images tine boots VMs from.
//
// An image is a directory under the images directory (in tine,
// /srv/tine/images, on the same XFS volume as the jails):
//
//	<images>/<name>/
//	├── vmlinux        the guest kernel; hard-linked into each VM's jail
//	├── rootfs.ext4    the guest's disk; reflink-cloned for each VM
//	└── image.json     what it was built from (Metadata)
//
// Import builds one from Firecracker's CI Ubuntu image (a squashfs file):
// it unpacks it into a directory, changes files there (Customize), and packs
// the directory into an ext4 disk image with mkfs.ext4. Nothing gets mounted,
// so a failed import leaves no mounts or loop devices behind.
package image

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	// DefaultImagesDir is where tined keeps images.
	DefaultImagesDir = "/srv/tine/images"
	// DefaultSizeBytes is the size of an image's disk, and so of each VM's
	// disk. The file is sparse and VMs get reflink clones, so unused space
	// costs nothing.
	DefaultSizeBytes = 4 << 30

	// KernelFile, RootfsFile and MetadataFile are the files in an image's directory.
	KernelFile   = "vmlinux"
	RootfsFile   = "rootfs.ext4"
	MetadataFile = "image.json"

	// MetadataVersion is the version of image.json's format.
	MetadataVersion = 1

	// toolTimeout bounds each external tool run (unsquashfs, mkfs.ext4, ...).
	toolTimeout = 10 * time.Minute
)

var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ErrExists means an image of that name is already there and Force wasn't set.
var ErrExists = errors.New("image already exists")

// Metadata is an image's image.json.
type Metadata struct {
	FormatVersion int       `json:"format_version"`
	Name          string    `json:"name"`
	CreatedAt     time.Time `json:"created_at"`
	// BuildSeconds is how long Import took.
	BuildSeconds float64 `json:"build_seconds"`

	Kernel KernelInfo `json:"kernel"`
	Rootfs RootfsInfo `json:"rootfs"`
	Agent  AgentInfo  `json:"agent"`
}

// KernelInfo describes an image's kernel.
type KernelInfo struct {
	// Source is the file it was copied from, e.g. "vmlinux-6.18.56".
	Source string `json:"source"`
	SHA256 string `json:"sha256"`
}

// RootfsInfo describes an image's disk.
type RootfsInfo struct {
	SizeBytes int64 `json:"size_bytes"`
	// Source is the squashfs it was built from, e.g. "ubuntu-24.04.squashfs".
	Source       string `json:"source"`
	SourceSHA256 string `json:"source_sha256"`
}

// AgentInfo describes the tine-agent inside an image.
type AgentInfo struct {
	// Version is what the agent printed for --version, if it could run here.
	Version string `json:"version,omitempty"`
	SHA256  string `json:"sha256"`
}

// Image is an image on disk.
type Image struct {
	Name string
	Dir  string
	Meta Metadata
}

// KernelPath is the image's kernel.
func (i *Image) KernelPath() string { return filepath.Join(i.Dir, KernelFile) }

// RootfsPath is the image's disk.
func (i *Image) RootfsPath() string { return filepath.Join(i.Dir, RootfsFile) }

// ImportOptions says what to build.
type ImportOptions struct {
	// Name is the image's name: lowercase letters, digits and hyphens, up to 32.
	Name string
	// ImagesDir defaults to DefaultImagesDir.
	ImagesDir string
	// Kernel is an uncompressed guest kernel (vmlinux).
	Kernel string
	// RootfsSquashfs is the guest filesystem to start from, as a squashfs.
	RootfsSquashfs string
	// Agent is the tine-agent binary to install, built for the guest (linux/amd64).
	Agent string
	// SizeBytes defaults to DefaultSizeBytes.
	SizeBytes int64
	// Force replaces an existing image of the same name. VMs already
	// running from it are unaffected.
	Force bool
	// Log receives progress. Default: discard.
	Log *slog.Logger
}

// Import builds an image and installs it under ImagesDir. It works in a
// temporary directory next to the result and moves the finished image into
// place in one rename, so an image is either complete or not there. It needs
// root, unsquashfs and mkfs.ext4.
func Import(ctx context.Context, opts ImportOptions) (*Image, error) {
	start := time.Now()
	if err := opts.fill(); err != nil {
		return nil, err
	}
	log := opts.Log
	final := filepath.Join(opts.ImagesDir, opts.Name)
	if _, err := os.Stat(final); err == nil && !opts.Force {
		return nil, fmt.Errorf("import %s: %w at %s (use Force to replace it)", opts.Name, ErrExists, final)
	}

	if err := os.MkdirAll(opts.ImagesDir, 0o750); err != nil {
		return nil, fmt.Errorf("import %s: %w", opts.Name, err)
	}
	work, err := os.MkdirTemp(opts.ImagesDir, ".import-"+opts.Name+"-")
	if err != nil {
		return nil, fmt.Errorf("import %s: %w", opts.Name, err)
	}
	defer func() { _ = os.RemoveAll(work) }()
	out := filepath.Join(work, "image")
	if err := os.Mkdir(out, 0o755); err != nil { //nolint:gosec // images are read by every VM's jail
		return nil, err
	}

	meta := Metadata{FormatVersion: MetadataVersion, Name: opts.Name, CreatedAt: time.Now().UTC()}

	log.Info("copying kernel", "from", opts.Kernel)
	if meta.Kernel.SHA256, err = copyFile(opts.Kernel, filepath.Join(out, KernelFile), 0o644); err != nil {
		return nil, fmt.Errorf("import %s: kernel: %w", opts.Name, err)
	}
	meta.Kernel.Source = filepath.Base(opts.Kernel)

	if meta.Rootfs.SourceSHA256, err = hashFile(opts.RootfsSquashfs); err != nil {
		return nil, fmt.Errorf("import %s: rootfs: %w", opts.Name, err)
	}
	meta.Rootfs.Source = filepath.Base(opts.RootfsSquashfs)
	meta.Rootfs.SizeBytes = opts.SizeBytes
	if meta.Agent.SHA256, err = hashFile(opts.Agent); err != nil {
		return nil, fmt.Errorf("import %s: agent: %w", opts.Name, err)
	}
	meta.Agent.Version = agentVersion(ctx, opts.Agent)

	root := filepath.Join(work, "root")
	log.Info("unpacking", "squashfs", opts.RootfsSquashfs)
	if err := run(ctx, "unsquashfs", "-no-progress", "-d", root, opts.RootfsSquashfs); err != nil {
		return nil, fmt.Errorf("import %s: unpack rootfs: %w", opts.Name, err)
	}
	log.Info("customizing the guest")
	if err := Customize(root, opts.Agent, meta); err != nil {
		return nil, fmt.Errorf("import %s: %w", opts.Name, err)
	}

	disk := filepath.Join(out, RootfsFile)
	log.Info("building the disk image", "size_bytes", opts.SizeBytes)
	if err := createSparse(disk, opts.SizeBytes); err != nil {
		return nil, fmt.Errorf("import %s: %w", opts.Name, err)
	}
	if err := run(ctx, "mkfs.ext4", "-q", "-F", "-L", "tine-root", "-d", root, disk); err != nil {
		return nil, fmt.Errorf("import %s: build disk image: %w", opts.Name, err)
	}
	if err := run(ctx, "e2fsck", "-fn", disk); err != nil {
		return nil, fmt.Errorf("import %s: the new disk image doesn't check out: %w", opts.Name, err)
	}
	// Only tined (root) reads the source disk, to clone it.
	if err := os.Chmod(disk, 0o600); err != nil {
		return nil, err
	}

	meta.BuildSeconds = time.Since(start).Seconds()
	if err := writeMetadata(filepath.Join(out, MetadataFile), meta); err != nil {
		return nil, fmt.Errorf("import %s: %w", opts.Name, err)
	}
	if err := install(out, final, opts.Force); err != nil {
		return nil, fmt.Errorf("import %s: %w", opts.Name, err)
	}
	meta.BuildSeconds = time.Since(start).Seconds()
	log.Info("imported", "image", opts.Name, "dir", final, "seconds", meta.BuildSeconds)
	return &Image{Name: opts.Name, Dir: final, Meta: meta}, nil
}

func (o *ImportOptions) fill() error {
	if !validName.MatchString(o.Name) {
		return fmt.Errorf("image name %q: use 1-32 lowercase letters, digits and hyphens, starting with a letter or digit", o.Name)
	}
	if o.ImagesDir == "" {
		o.ImagesDir = DefaultImagesDir
	}
	if o.SizeBytes == 0 {
		o.SizeBytes = DefaultSizeBytes
	}
	if o.SizeBytes < 256<<20 {
		return fmt.Errorf("image %s: size %d bytes is too small; use at least 256 MiB", o.Name, o.SizeBytes)
	}
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	for flag, p := range map[string]string{"kernel": o.Kernel, "rootfs squashfs": o.RootfsSquashfs, "agent": o.Agent} {
		info, err := os.Stat(p)
		if err != nil {
			return fmt.Errorf("image %s: %s: %w", o.Name, flag, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("image %s: %s %s isn't a regular file", o.Name, flag, p)
		}
	}
	return nil
}

// install moves the finished image into place. With force, an existing
// image is moved aside first and removed afterwards.
func install(built, final string, force bool) error {
	var old string
	if _, err := os.Stat(final); err == nil {
		if !force {
			return fmt.Errorf("%w at %s", ErrExists, final)
		}
		old = filepath.Join(filepath.Dir(final), ".old-"+filepath.Base(final)+"-"+randomSuffix())
		if err := os.Rename(final, old); err != nil {
			return fmt.Errorf("move the old image aside: %w", err)
		}
	}
	if err := os.Rename(built, final); err != nil {
		if old != "" {
			_ = os.Rename(old, final) // put the old one back
		}
		return fmt.Errorf("install: %w", err)
	}
	if old != "" {
		_ = os.RemoveAll(old)
	}
	return nil
}

// Open loads an installed image.
func Open(imagesDir, name string) (*Image, error) {
	if !validName.MatchString(name) {
		return nil, fmt.Errorf("image name %q isn't valid", name)
	}
	dir := filepath.Join(imagesDir, name)
	data, err := os.ReadFile(filepath.Join(dir, MetadataFile)) //nolint:gosec // path is built from a validated name
	if err != nil {
		return nil, fmt.Errorf("open image %s: %w", name, err)
	}
	var meta Metadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, fmt.Errorf("open image %s: bad %s: %w", name, MetadataFile, err)
	}
	if meta.FormatVersion != MetadataVersion {
		return nil, fmt.Errorf("open image %s: metadata format %d, this tine reads %d", name, meta.FormatVersion, MetadataVersion)
	}
	img := &Image{Name: name, Dir: dir, Meta: meta}
	for _, p := range []string{img.KernelPath(), img.RootfsPath()} {
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("open image %s: %w", name, err)
		}
	}
	return img, nil
}

// List returns every installed image, sorted by name. Unfinished imports
// and unreadable directories are skipped.
func List(imagesDir string) ([]*Image, error) {
	entries, err := os.ReadDir(imagesDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var images []*Image
	for _, e := range entries {
		if !e.IsDir() || !validName.MatchString(e.Name()) {
			continue
		}
		if img, err := Open(imagesDir, e.Name()); err == nil {
			images = append(images, img)
		}
	}
	sort.Slice(images, func(i, j int) bool { return images[i].Name < images[j].Name })
	return images, nil
}

// --- helpers ---

func writeMetadata(path string, meta Metadata) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644) //nolint:gosec // metadata isn't secret
}

// copyFile copies src to dst with mode perm and returns src's SHA-256.
func copyFile(src, dst string, perm os.FileMode) (string, error) {
	in, err := os.Open(src) //nolint:gosec // an operator-given input file
	if err != nil {
		return "", err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm) //nolint:gosec // dst is inside the import's own directory
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(out, h), in)
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if err := os.Chmod(dst, perm); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // an operator-given input file
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func createSparse(path string, size int64) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640) //nolint:gosec // inside the import's own directory
	if err != nil {
		return err
	}
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// agentVersion asks the agent binary for its version. It only works where
// the binary can run (on the Linux host); otherwise it's left empty.
func agentVersion(ctx context.Context, agent string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, agent, "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(out)), "tine-agent"))
}

// run runs an external tool, returning its output with any error.
func run(ctx context.Context, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput() //nolint:gosec // fixed tools, arguments from tine
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 2000 {
			msg = msg[len(msg)-2000:]
		}
		return fmt.Errorf("%s: %w: %s", name, err, msg)
	}
	return nil
}

func randomSuffix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
