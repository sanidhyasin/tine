package image

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Paths inside the guest.
const (
	// AgentPath is where tine-agent is installed.
	AgentPath = "/usr/local/bin/tine-agent"
	// AgentUnitPath is its systemd unit.
	AgentUnitPath = "/etc/systemd/system/tine-agent.service"
	// ImageInfoPath tells programs in the guest which image they run on.
	ImageInfoPath = "/etc/tine-image"
	// Nameservers go into the guest's static /etc/resolv.conf.
	Nameservers = "1.1.1.1 8.8.8.8"
)

// agentUnit starts tine-agent as early in boot as possible, since tined
// waits for it before a VM counts as ready, and keeps it running.
const agentUnit = `[Unit]
Description=tine guest agent: runs commands for tined over vsock
# Start early, without waiting for the usual boot steps: the agent only needs
# the root filesystem and /dev/vsock.
DefaultDependencies=no
After=systemd-remount-fs.service
Conflicts=shutdown.target
Before=shutdown.target
# Never give up restarting it.
StartLimitIntervalSec=0

[Service]
ExecStart=` + AgentPath + `
Restart=always
RestartSec=200ms
# If the guest runs out of memory, kill the user's processes before the agent.
OOMScoreAdjust=-900

[Install]
WantedBy=sysinit.target
`

// maskedUnits are switched off in every tine guest.
var maskedUnits = []string{
	// No SSH server: tined talks to the agent over vsock instead.
	"ssh.service",
	"ssh.socket",
	// Static DNS (see Nameservers) instead of a resolver service.
	"systemd-resolved.service",
	// Don't hold up boot waiting for the network; it's configured on the
	// kernel command line before anything starts.
	"systemd-networkd-wait-online.service",
	// Sandboxes never update themselves in the background.
	"apt-daily.timer",
	"apt-daily-upgrade.timer",
	"unattended-upgrades.service",
	"motd-news.timer",
}

// Customize turns an unpacked Ubuntu root filesystem at root into a tine
// guest: it installs the agent from agentBin and starts it at boot, turns
// off the SSH server and background updates, and writes static DNS, the
// hostname and ImageInfoPath.
//
// It never follows a symlink while writing: in an unpacked image, a link
// like /etc/resolv.conf -> /run/... points at the host's files.
func Customize(root, agentBin string, meta Metadata) error {
	g := guestFS{root: root}

	if err := g.copyIn(agentBin, AgentPath, 0o755); err != nil {
		return fmt.Errorf("install tine-agent: %w", err)
	}
	if err := g.writeFile(AgentUnitPath, agentUnit, 0o644); err != nil {
		return fmt.Errorf("tine-agent service: %w", err)
	}
	if err := g.symlink(AgentUnitPath, "/etc/systemd/system/sysinit.target.wants/tine-agent.service"); err != nil {
		return fmt.Errorf("enable tine-agent: %w", err)
	}
	for _, unit := range maskedUnits {
		if err := g.symlink("/dev/null", "/etc/systemd/system/"+unit); err != nil {
			return fmt.Errorf("turn off %s: %w", unit, err)
		}
	}

	var resolv strings.Builder
	resolv.WriteString("# Written by tine. Static on purpose: there's no resolver service in the guest.\n")
	for _, ns := range strings.Fields(Nameservers) {
		resolv.WriteString("nameserver " + ns + "\n")
	}
	// glibc asks for A and AAAA records in parallel; over NAT without IPv6 the
	// AAAA query can stall a lookup for seconds.
	resolv.WriteString("options single-request-reopen\n")
	if err := g.writeFile("/etc/resolv.conf", resolv.String(), 0o644); err != nil {
		return fmt.Errorf("static DNS: %w", err)
	}

	if err := g.writeFile("/etc/hostname", "tine\n", 0o644); err != nil {
		return fmt.Errorf("hostname: %w", err)
	}

	info, err := json.MarshalIndent(map[string]any{
		"image":         meta.Name,
		"created_at":    meta.CreatedAt,
		"kernel":        meta.Kernel.Source,
		"rootfs_source": meta.Rootfs.Source,
		"agent_version": meta.Agent.Version,
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := g.writeFile(ImageInfoPath, string(info)+"\n", 0o644); err != nil {
		return fmt.Errorf("image info: %w", err)
	}

	// Changing /etc makes systemd think the system was just updated, so every
	// VM's boot would rerun ldconfig and friends (units with
	// ConditionNeedsUpdate=). These stamps, newer than /usr, say they've run.
	for _, stamp := range []string{"/etc/.updated", "/var/.updated"} {
		if err := g.writeFile(stamp, "# Written by tine: the image is up to date.\n", 0o644); err != nil {
			return fmt.Errorf("update stamp %s: %w", stamp, err)
		}
	}
	return nil
}

// guestFS writes into an unpacked guest filesystem without ever following
// a symlink, so nothing can be written outside it.
type guestFS struct{ root string }

// path turns an absolute guest path into the host path, creating missing
// parent directories. It fails if any parent is a symlink.
func (g guestFS) path(guestPath string) (string, error) {
	clean := filepath.Clean("/" + guestPath)
	if clean == "/" {
		return "", errors.New("can't write the guest's root directory")
	}
	dir := g.root
	parts := strings.Split(strings.TrimPrefix(filepath.Dir(clean), "/"), "/")
	for _, part := range parts {
		if part == "" {
			continue
		}
		dir = filepath.Join(dir, part)
		info, err := os.Lstat(dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if err := os.Mkdir(dir, 0o755); err != nil { //nolint:gosec // standard permissions for a guest system directory
				return "", err
			}
		case err != nil:
			return "", err
		case info.Mode()&os.ModeSymlink != 0:
			return "", fmt.Errorf("%s in the guest is a symlink; refusing to write through it", strings.TrimPrefix(dir, g.root))
		case !info.IsDir():
			return "", fmt.Errorf("%s in the guest isn't a directory", strings.TrimPrefix(dir, g.root))
		}
	}
	return filepath.Join(dir, filepath.Base(clean)), nil
}

// replace removes whatever is at the guest path, without following it.
func (g guestFS) replace(guestPath string) (string, error) {
	p, err := g.path(guestPath)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s in the guest is a directory", guestPath)
	}
	return p, os.Remove(p)
}

func (g guestFS) writeFile(guestPath, content string, perm os.FileMode) error {
	p, err := g.replace(guestPath)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm) //nolint:gosec // p was checked to stay inside the guest
	if err != nil {
		return err
	}
	_, err = f.WriteString(content)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Chmod(p, perm)
}

func (g guestFS) copyIn(src, guestPath string, perm os.FileMode) error {
	p, err := g.replace(guestPath)
	if err != nil {
		return err
	}
	_, err = copyFile(src, p, perm)
	return err
}

func (g guestFS) symlink(target, guestPath string) error {
	p, err := g.replace(guestPath)
	if err != nil {
		return err
	}
	return os.Symlink(target, p)
}
