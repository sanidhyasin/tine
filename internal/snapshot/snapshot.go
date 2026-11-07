// Package snapshot is the snapshot store: the files of every snapshot, one
// directory each.
//
//	/srv/tine/snapshots/<snap-id>/
//	├── vmstate        Firecracker's device and CPU state
//	├── memory         guest memory, which never changes once written
//	├── rootfs.ext4    reflink copy of the VM's disk at that moment
//	└── snapshot.json  Metadata
//
// A snapshot is gathered in a temporary directory and renamed into place
// when it's complete, so a directory named after a snapshot always holds a
// whole one. Its files are read-only and belong to tined's user (root).
// A VM restored from a snapshot gets hard links to vmstate and memory, which
// it can read but never change, and a reflink copy of the disk. Firecracker
// requires the memory file to stay unchanged while any VM runs from it.
//
// Which snapshots exist, and which VMs use them, is recorded in internal/state.
// This package only handles the files.
package snapshot

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

// DefaultDir is where tined keeps snapshots, on the data volume, so that hard
// links and reflinks into VM jails work.
const DefaultDir = "/srv/tine/snapshots"

// The files of a snapshot.
const (
	StateFile    = "vmstate"
	MemoryFile   = "memory"
	DiskFile     = "rootfs.ext4"
	MetadataFile = "snapshot.json"
)

// FormatVersion is the snapshot.json format this tine writes and reads.
const FormatVersion = 1

// buildingPrefix marks a snapshot that's still being gathered.
const buildingPrefix = ".new-"

// ErrIncompatible means a snapshot can't be restored on this host: Firecracker
// only restores snapshots taken by the same version on the same CPU model.
var ErrIncompatible = errors.New("incompatible with this host")

var validID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// Metadata is a snapshot's snapshot.json.
type Metadata struct {
	FormatVersion int    `json:"format_version"`
	ID            string `json:"id"`
	// Kind is "vm" or "template" (see internal/state).
	Kind string `json:"kind"`
	// VMID is the VM it was taken from.
	VMID  string `json:"vm_id,omitempty"`
	Image string `json:"image"`
	// ImageCreatedAt says which build of the image the VM ran. A template
	// made from an older build is out of date.
	ImageCreatedAt time.Time `json:"image_created_at"`
	VCPUs          int       `json:"vcpus"`
	MemoryMiB      int       `json:"memory_mib"`
	Host           Host      `json:"host"`
	CreatedAt      time.Time `json:"created_at"`
	// Bytes is the disk space each file takes, by file name.
	Bytes map[string]int64 `json:"bytes"`
	// TimingsMs is how long each step of taking it took, in milliseconds.
	TimingsMs map[string]float64 `json:"timings_ms,omitempty"`
	// Base is the snapshot whose memory this one's was made from, with a
	// diff of the pages the VM changed since written over it. Empty for a
	// full snapshot. The two are independent once made.
	Base string `json:"base,omitempty"`
	// DiffBytes is the memory the diff held: what the VM changed.
	DiffBytes int64 `json:"diff_bytes,omitempty"`
}

// Host is what a snapshot depends on in the host that took it.
type Host struct {
	Firecracker string `json:"firecracker_version"`
	CPU         string `json:"cpu_model"`
	Kernel      string `json:"kernel"`
}

// CheckCompatible returns an error wrapping ErrIncompatible unless the
// snapshot can be restored on host h. Firecracker requires the same
// software and hardware a snapshot was taken on, so the Firecracker
// version, CPU model and host kernel must all match.
func (m Metadata) CheckCompatible(h Host) error {
	var diffs []string
	if m.Host.Firecracker != h.Firecracker {
		diffs = append(diffs, fmt.Sprintf("taken with Firecracker %s, this is %s", m.Host.Firecracker, h.Firecracker))
	}
	if m.Host.CPU != h.CPU {
		diffs = append(diffs, fmt.Sprintf("taken on a %q CPU, this is a %q", m.Host.CPU, h.CPU))
	}
	if m.Host.Kernel != h.Kernel {
		diffs = append(diffs, fmt.Sprintf("taken on host kernel %s, this is %s", m.Host.Kernel, h.Kernel))
	}
	if len(diffs) > 0 {
		return fmt.Errorf("snapshot %s is %w: %s", m.ID, ErrIncompatible, strings.Join(diffs, "; "))
	}
	return nil
}

// Where ThisHost reads the CPU model and kernel version. Tests change them.
var (
	cpuinfoPath   = "/proc/cpuinfo"
	osreleasePath = "/proc/sys/kernel/osrelease"
)

// ThisHost describes the current host, given the Firecracker version that
// runs VMs here.
func ThisHost(firecrackerVersion string) (Host, error) {
	h := Host{Firecracker: firecrackerVersion}
	cpuinfo, err := os.ReadFile(cpuinfoPath)
	if err != nil {
		return Host{}, fmt.Errorf("read the CPU model: %w", err)
	}
	for _, line := range strings.Split(string(cpuinfo), "\n") {
		if key, value, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(key) == "model name" {
			h.CPU = strings.TrimSpace(value)
			break
		}
	}
	if h.CPU == "" {
		return Host{}, fmt.Errorf("no CPU model name in %s", cpuinfoPath)
	}
	release, err := os.ReadFile(osreleasePath)
	if err != nil {
		return Host{}, fmt.Errorf("read the kernel version: %w", err)
	}
	h.Kernel = strings.TrimSpace(string(release))
	return h, nil
}

// Store is the snapshot directory.
type Store struct {
	dir string
}

// NewStore opens the store at dir, creating it if needed. Only tined's user
// can read it.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("snapshot store: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // a directory: its owner needs to enter it
		return nil, fmt.Errorf("snapshot store: %w", err)
	}
	return &Store{dir: dir}, nil
}

// Dir is the store's directory.
func (s *Store) Dir() string { return s.dir }

// Path is a file of a snapshot in the store.
func (s *Store) Path(id, file string) string { return filepath.Join(s.dir, id, file) }

// Building is a snapshot being gathered: put its state, memory and disk
// files at Path, then Commit, or Abort.
type Building struct {
	store *Store
	id    string
	dir   string
}

// Begin starts gathering snapshot id. A leftover from an earlier attempt at
// the same ID is removed first.
func (s *Store) Begin(id string) (*Building, error) {
	if !validID.MatchString(id) {
		return nil, fmt.Errorf("snapshot: bad ID %q", id)
	}
	if _, err := os.Stat(filepath.Join(s.dir, id)); err == nil {
		return nil, fmt.Errorf("snapshot %s already exists: %w", id, fs.ErrExist)
	}
	dir := filepath.Join(s.dir, buildingPrefix+id)
	if err := os.RemoveAll(dir); err != nil {
		return nil, fmt.Errorf("snapshot %s: remove a leftover: %w", id, err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, fmt.Errorf("snapshot %s: %w", id, err)
	}
	return &Building{store: s, id: id, dir: dir}, nil
}

// Path is where file goes in the snapshot being built.
func (b *Building) Path(file string) string { return filepath.Join(b.dir, file) }

// Commit makes the snapshot permanent. Its files become read-only and owned
// by tined's user, meta gets the format version, ID and file sizes, and is
// written as snapshot.json, and the directory is renamed into place. It
// returns the disk space the snapshot takes. On error, the building
// directory is left for Abort.
func (b *Building) Commit(meta *Metadata) (int64, error) {
	meta.FormatVersion, meta.ID = FormatVersion, b.id
	meta.Bytes = map[string]int64{}
	var total int64
	for _, f := range []struct {
		name string
		mode os.FileMode
	}{
		// Restored VMs read the state and memory through hard links, as their own uid.
		{StateFile, 0o444},
		{MemoryFile, 0o444},
		// The disk is only ever cloned, by tined.
		{DiskFile, 0o400},
	} {
		path := b.Path(f.name)
		// Firecracker wrote these as the VM's uid; nothing in a VM may change them now.
		if err := os.Lchown(path, os.Getuid(), os.Getgid()); err != nil {
			return 0, fmt.Errorf("snapshot %s: %w", b.id, err)
		}
		if err := os.Chmod(path, f.mode); err != nil {
			return 0, fmt.Errorf("snapshot %s: %w", b.id, err)
		}
		fi, err := os.Lstat(path)
		if err != nil {
			return 0, fmt.Errorf("snapshot %s: %w", b.id, err)
		}
		if !fi.Mode().IsRegular() {
			return 0, fmt.Errorf("snapshot %s: %s isn't a regular file", b.id, f.name)
		}
		meta.Bytes[f.name] = allocated(fi)
		total += meta.Bytes[f.name]
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return 0, err
	}
	if err := os.WriteFile(b.Path(MetadataFile), append(data, '\n'), 0o444); err != nil { //nolint:gosec // metadata isn't secret, and the store is root-only
		return 0, fmt.Errorf("snapshot %s: %w", b.id, err)
	}
	final := filepath.Join(b.store.dir, b.id)
	if _, err := os.Stat(final); err == nil {
		return 0, fmt.Errorf("snapshot %s already exists: %w", b.id, fs.ErrExist)
	}
	if err := os.Rename(b.dir, final); err != nil {
		return 0, fmt.Errorf("snapshot %s: %w", b.id, err)
	}
	return total, nil
}

// Abort removes a snapshot that won't be committed.
func (b *Building) Abort() error {
	if err := os.RemoveAll(b.dir); err != nil {
		return fmt.Errorf("snapshot %s: %w", b.id, err)
	}
	return nil
}

// Load reads a snapshot's metadata and checks that its files are there.
// A snapshot that doesn't exist is an error wrapping fs.ErrNotExist.
func (s *Store) Load(id string) (Metadata, error) {
	if !validID.MatchString(id) {
		return Metadata{}, fmt.Errorf("snapshot: bad ID %q", id)
	}
	data, err := os.ReadFile(s.Path(id, MetadataFile))
	if err != nil {
		return Metadata{}, fmt.Errorf("snapshot %s: %w", id, err)
	}
	var meta Metadata
	if err := json.Unmarshal(data, &meta); err != nil {
		return Metadata{}, fmt.Errorf("snapshot %s: bad %s: %w", id, MetadataFile, err)
	}
	if meta.FormatVersion != FormatVersion {
		return Metadata{}, fmt.Errorf("snapshot %s: format version %d, this tine reads %d", id, meta.FormatVersion, FormatVersion)
	}
	if meta.ID != id {
		return Metadata{}, fmt.Errorf("snapshot %s: its %s says it's %q", id, MetadataFile, meta.ID)
	}
	for _, f := range []string{StateFile, MemoryFile, DiskFile} {
		if _, err := os.Stat(s.Path(id, f)); err != nil {
			return Metadata{}, fmt.Errorf("snapshot %s: %w", id, err)
		}
	}
	return meta, nil
}

// Remove deletes a snapshot's files, and any unfinished attempt at it.
// Removing one that's already gone isn't an error. VMs running from it keep
// their own hard links, but Remove should only be called once none do.
func (s *Store) Remove(id string) error {
	if !validID.MatchString(id) {
		return fmt.Errorf("snapshot: bad ID %q", id)
	}
	for _, dir := range []string{filepath.Join(s.dir, id), filepath.Join(s.dir, buildingPrefix+id)} {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("snapshot %s: %w", id, err)
		}
	}
	return nil
}

// List returns the IDs of the snapshots in the store, and of any being built
// (or left over from a build that never finished), each sorted.
func (s *Store) List() (ids, building []string, err error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, nil, fmt.Errorf("snapshot store: %w", err)
	}
	for _, e := range entries {
		name := e.Name()
		switch {
		case !e.IsDir():
		case strings.HasPrefix(name, buildingPrefix):
			building = append(building, strings.TrimPrefix(name, buildingPrefix))
		case validID.MatchString(name):
			ids = append(ids, name)
		}
	}
	sort.Strings(ids)
	sort.Strings(building)
	return ids, building, nil
}

// FreeBytes is how much space is free for the store's files.
func (s *Store) FreeBytes() (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(s.dir, &st); err != nil {
		return 0, fmt.Errorf("snapshot store: free space: %w", err)
	}
	return st.Bavail * uint64(st.Bsize), nil //nolint:gosec // block sizes are small and positive
}

// allocated is the disk space a file takes, which for a sparse file is less
// than its size.
func allocated(fi os.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Blocks * 512
	}
	return fi.Size()
}
