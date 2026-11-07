package image

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// BuildOptions says how to build an image from a Dockerfile.
type BuildOptions struct {
	ImportOptions
	// Dockerfile, and the build context: its directory if empty.
	Dockerfile string
	Context    string
}

// Build makes an image from a Dockerfile: docker builds it, its filesystem
// is exported as a tar, and Import turns that into a guest, as it does a
// squashfs. The Dockerfile must give the guest an init (systemd, as in
// images/python), since tine-agent starts as a systemd service. It needs
// docker, and root.
func Build(ctx context.Context, opts BuildOptions) (*Image, error) {
	if !validName.MatchString(opts.Name) {
		return nil, fmt.Errorf("image name %q: use 1-32 lowercase letters, digits and hyphens, starting with a letter or digit", opts.Name)
	}
	if opts.Context == "" {
		opts.Context = filepath.Dir(opts.Dockerfile)
	}
	if opts.ImagesDir == "" {
		opts.ImagesDir = DefaultImagesDir
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if err := os.MkdirAll(opts.ImagesDir, 0o750); err != nil {
		return nil, err
	}
	work, err := os.MkdirTemp(opts.ImagesDir, ".build-"+opts.Name+"-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(work) }()

	tag := "tine-image-" + opts.Name
	opts.Log.Info("docker build", "dockerfile", opts.Dockerfile, "context", opts.Context)
	// On the host's network: Docker on a tine host has no bridge of its own,
	// so it leaves the firewall to tined (scripts/host-setup.sh).
	if err := docker(ctx, "build", "--pull", "--network", "host", "-t", tag, "-f", opts.Dockerfile, opts.Context); err != nil {
		return nil, fmt.Errorf("build %s: %w", opts.Name, err)
	}
	out, err := exec.CommandContext(ctx, "docker", "create", tag).Output() //nolint:gosec // G204: tine's own docker command, on an image name checked above
	if err != nil {
		return nil, fmt.Errorf("build %s: docker create: %w", opts.Name, err)
	}
	container := strings.TrimSpace(string(out))
	defer func() { _ = exec.CommandContext(context.WithoutCancel(ctx), "docker", "rm", "-f", container).Run() }() //nolint:gosec // G204: the container docker just made
	tarPath := filepath.Join(work, opts.Name+".tar")
	opts.Log.Info("docker export", "container", container)
	if err := docker(ctx, "export", "-o", tarPath, container); err != nil {
		return nil, fmt.Errorf("build %s: %w", opts.Name, err)
	}
	opts.RootfsTar, opts.RootfsSquashfs = tarPath, ""
	return Import(ctx, opts.ImportOptions)
}

func docker(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "docker", args...) //nolint:gosec // tine's own docker commands
	if out, err := cmd.CombinedOutput(); err != nil {
		tail := strings.TrimSpace(string(out))
		if len(tail) > 2000 {
			tail = "..." + tail[len(tail)-2000:]
		}
		return fmt.Errorf("docker %s: %w\n%s", args[0], err, tail)
	}
	return nil
}
