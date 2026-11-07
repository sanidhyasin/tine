// Package jailer runs Firecracker inside Firecracker's jailer, one jail per VM.
//
// A jail is a directory that becomes the VM's whole filesystem:
//
//	<ChrootBaseDir>/firecracker/<id>/     the jail's directory (Jail.Dir)
//	├── console.log                       the guest's serial console, if the caller writes it here
//	└── root/                             "/" as Firecracker sees it (Jail.Root)
//	    ├── firecracker                   copied in by the jailer
//	    ├── firecracker.pid               written by the jailer
//	    ├── vmlinux, rootfs.ext4, …       placed by Prepare
//	    └── api.sock, vsock.sock          created by Firecracker
//
// Inside its jail every VM sees the same paths (/vmlinux, /rootfs.ext4,
// /api.sock), but each one maps to that VM's own files on the host. That's
// what lets one snapshot be restored many times (step 3), because Firecracker
// requires the same paths on restore.
//
// The jailer also runs Firecracker as an unprivileged uid and gid of the VM's
// own, in a cgroup with CPU, memory and process limits, and optionally in a
// network namespace. The jailer itself must run as root.
//
// Lifecycle: New, then Prepare, then start Command's process; once that
// process has exited, Cleanup removes the jail and its cgroup.
package jailer

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/sanidhyasin/tine/internal/storage"
)

const (
	// APISocketPath is where Firecracker's API socket lives inside every jail.
	APISocketPath = "/api.sock"

	// DefaultCgroupRoot is where cgroup v2 is mounted on Linux.
	DefaultCgroupRoot = "/sys/fs/cgroup"

	// cpuPeriodMicros is the cgroup CPU accounting period: a quota of
	// cpuPeriodMicros per period is one full CPU.
	cpuPeriodMicros = 100_000
)

// validID matches what the jailer accepts as an ID.
var validID = regexp.MustCompile(`^[a-zA-Z0-9-]{1,64}$`)

// Config is the host-wide jailer setup, the same for every VM.
type Config struct {
	// JailerBin and FirecrackerBin must come from the same Firecracker release.
	JailerBin      string
	FirecrackerBin string
	// ChrootBaseDir holds every jail. Files are hard-linked and cloned into
	// jails, so it must be on the same filesystem as their sources (in tine,
	// the XFS volume at /srv/tine). It must be owned by root and not writable
	// by anyone else.
	ChrootBaseDir string
	// CgroupParent is the cgroup, relative to CgroupRoot, that holds one
	// child cgroup per VM, e.g. "tine".
	CgroupParent string
	// CgroupRoot is where cgroup v2 is mounted. Empty means DefaultCgroupRoot.
	CgroupRoot string
}

// Spec describes one VM's jail.
type Spec struct {
	// ID names the jail, its directory and its cgroup: letters, digits and
	// hyphens, up to 64 characters.
	ID string
	// UID and GID are what Firecracker runs as. Give every VM its own, so a
	// compromised Firecracker can't touch other VMs' files.
	UID, GID int
	// NetNS is the path of a network namespace for Firecracker to join, e.g.
	// /var/run/netns/tine-<id>. Empty means the host's namespace.
	NetNS string
	// Limits are the VM's cgroup limits. All three are required.
	Limits Limits
	// Files are placed into the jail by Prepare.
	Files []File
	// FirecrackerArgs are extra arguments for Firecracker, after --api-sock.
	// Paths in them are inside the jail.
	FirecrackerArgs []string
}

// Limits are a VM's cgroup limits.
type Limits struct {
	// VCPUs is how many full CPUs the VM's Firecracker process may use (cpu.max).
	VCPUs int
	// MemoryBytes caps the process's memory, including guest memory (memory.max).
	MemoryBytes int64
	// MaxPIDs caps the process's threads (pids.max).
	MaxPIDs int
}

// FileMode says how Prepare places a file into a jail.
type FileMode int

const (
	// Link hard-links the file: the VM shares it read-only, e.g. a kernel or
	// a snapshot's memory. Its owner and permissions stay as they are, so it
	// must be readable by everyone.
	Link FileMode = iota
	// Clone makes a reflink copy that belongs to the VM: its own writable
	// copy, e.g. its disk. The copy is owned by the VM's uid with mode 0600.
	Clone
)

func (m FileMode) String() string {
	switch m {
	case Link:
		return "link"
	case Clone:
		return "clone"
	default:
		return "FileMode(" + strconv.Itoa(int(m)) + ")"
	}
}

// File is one file to place into a jail.
type File struct {
	// Source is the file's path on the host.
	Source string
	// Target is its path inside the jail, e.g. "/rootfs.ext4".
	Target string
	Mode   FileMode
}

// Jail is one VM's jail.
type Jail struct {
	// ID is the jail's ID, as given in Spec.
	ID string
	// Dir is the jail's directory on the host. Cleanup removes it.
	Dir string
	// Root is the directory Firecracker sees as "/".
	Root string

	cfg  Config
	spec Spec
}

// New checks the config and spec and works out the jail's paths. It doesn't
// touch the filesystem.
func New(cfg Config, spec Spec) (*Jail, error) {
	if cfg.CgroupRoot == "" {
		cfg.CgroupRoot = DefaultCgroupRoot
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if err := spec.validate(); err != nil {
		return nil, err
	}
	dir := filepath.Join(cfg.ChrootBaseDir, filepath.Base(cfg.FirecrackerBin), spec.ID)
	return &Jail{ID: spec.ID, Dir: dir, Root: filepath.Join(dir, "root"), cfg: cfg, spec: spec}, nil
}

func (c Config) validate() error {
	for name, p := range map[string]string{"JailerBin": c.JailerBin, "FirecrackerBin": c.FirecrackerBin, "ChrootBaseDir": c.ChrootBaseDir, "CgroupRoot": c.CgroupRoot} {
		if !filepath.IsAbs(p) {
			return fmt.Errorf("jailer config: %s must be an absolute path, got %q", name, p)
		}
	}
	if err := checkRelativePath(c.CgroupParent); err != nil {
		return fmt.Errorf("jailer config: CgroupParent: %w", err)
	}
	return nil
}

func (s Spec) validate() error {
	if !validID.MatchString(s.ID) {
		return fmt.Errorf("jail ID %q: must be 1-64 letters, digits and hyphens", s.ID)
	}
	if s.UID <= 0 || s.GID <= 0 {
		return fmt.Errorf("jail %s: UID and GID must be positive (never root), got %d:%d", s.ID, s.UID, s.GID)
	}
	if s.NetNS != "" && !filepath.IsAbs(s.NetNS) {
		return fmt.Errorf("jail %s: NetNS must be an absolute path, got %q", s.ID, s.NetNS)
	}
	if s.Limits.VCPUs <= 0 || s.Limits.MemoryBytes <= 0 || s.Limits.MaxPIDs <= 0 {
		return fmt.Errorf("jail %s: every limit must be set, got %+v", s.ID, s.Limits)
	}
	seen := map[string]bool{}
	for _, f := range s.Files {
		if err := checkJailPath(f.Target); err != nil {
			return fmt.Errorf("jail %s: file %s: %w", s.ID, f.Source, err)
		}
		if seen[f.Target] {
			return fmt.Errorf("jail %s: two files go to %s", s.ID, f.Target)
		}
		seen[f.Target] = true
		if f.Mode != Link && f.Mode != Clone {
			return fmt.Errorf("jail %s: file %s: unknown mode %v", s.ID, f.Source, f.Mode)
		}
		if !filepath.IsAbs(f.Source) {
			return fmt.Errorf("jail %s: file source %q must be an absolute path", s.ID, f.Source)
		}
	}
	return nil
}

// checkJailPath accepts clean absolute paths inside the jail, other than "/".
func checkJailPath(p string) error {
	if !path.IsAbs(p) || path.Clean(p) != p || p == "/" {
		return fmt.Errorf("target %q must be a clean absolute path inside the jail", p)
	}
	return nil
}

// checkRelativePath accepts clean relative paths that stay where they start.
func checkRelativePath(p string) error {
	if p == "" || path.IsAbs(p) || path.Clean(p) != p || p == "." || strings.HasPrefix(p, "../") || p == ".." {
		return fmt.Errorf("%q must be a clean relative path", p)
	}
	return nil
}

// HostPath turns a path inside the jail into the same file's path on the host.
func (j *Jail) HostPath(jailPath string) string {
	return filepath.Join(j.Root, filepath.FromSlash(path.Clean("/"+jailPath)))
}

// APISocket is the host path of Firecracker's API socket.
func (j *Jail) APISocket() string { return j.HostPath(APISocketPath) }

// ConsoleLogPath is a place for the guest's console log: inside the jail's
// directory, but outside its root, so the jailed Firecracker can't reopen it.
func (j *Jail) ConsoleLogPath() string { return filepath.Join(j.Dir, "console.log") }

// CgroupDir is the VM's cgroup, which the jailer creates when it starts.
func (j *Jail) CgroupDir() string {
	return filepath.Join(j.cfg.CgroupRoot, filepath.FromSlash(j.cfg.CgroupParent), j.ID)
}

// Prepare creates the jail and places its files. If anything fails, it
// removes what it created. It fails if the jail already exists, so an ID can't
// be used twice by accident.
func (j *Jail) Prepare() (err error) {
	// The jailer creates <base>/firecracker itself, but making it here first
	// lets jail directories be created exclusively. Only root (the jailer and
	// tined) ever goes through it; jailed Firecracker never sees host paths.
	if err := os.MkdirAll(filepath.Dir(j.Dir), 0o700); err != nil {
		return fmt.Errorf("prepare jail %s: %w", j.ID, err)
	}
	if err := os.Mkdir(j.Dir, 0o700); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("prepare jail %s: %s already exists", j.ID, j.Dir)
		}
		return fmt.Errorf("prepare jail %s: %w", j.ID, err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(j.Dir)
		}
	}()
	// The jailer hands the root directory to the VM's uid when it starts.
	if err := os.Mkdir(j.Root, 0o700); err != nil {
		return fmt.Errorf("prepare jail %s: %w", j.ID, err)
	}
	for _, f := range j.spec.Files {
		if err := j.place(f); err != nil {
			return fmt.Errorf("prepare jail %s: %w", j.ID, err)
		}
	}
	return nil
}

func (j *Jail) place(f File) error {
	dst := j.HostPath(f.Target)
	if err := j.mkdirInJail(filepath.Dir(dst)); err != nil {
		return err
	}
	switch f.Mode {
	case Link:
		info, err := os.Stat(f.Source)
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0o004 == 0 {
			return fmt.Errorf("%s must be readable by everyone to be shared into jails (mode %v)", f.Source, info.Mode().Perm())
		}
		return storage.Link(f.Source, dst)
	case Clone:
		if err := storage.Clone(f.Source, dst, 0o600); err != nil {
			return err
		}
		return os.Chown(dst, j.spec.UID, j.spec.GID)
	default:
		return fmt.Errorf("unknown file mode %v", f.Mode)
	}
}

// CreateFile makes an empty file inside the jail, owned by the VM's uid with
// mode 0600, for Firecracker to write to (its log, for example). Call it
// after Prepare.
func (j *Jail) CreateFile(target string) error {
	if err := checkJailPath(target); err != nil {
		return fmt.Errorf("jail %s: %w", j.ID, err)
	}
	p := j.HostPath(target)
	if err := j.mkdirInJail(filepath.Dir(p)); err != nil {
		return fmt.Errorf("jail %s: %w", j.ID, err)
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // p was checked to be inside the jail
	if err != nil {
		return fmt.Errorf("jail %s: %w", j.ID, err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chown(p, j.spec.UID, j.spec.GID)
}

// mkdirInJail creates dir, which must be inside the jail's root, and any
// missing parents. They belong to the VM's uid with mode 0700: only that VM's
// Firecracker needs to go through them.
func (j *Jail) mkdirInJail(dir string) error {
	rel, err := filepath.Rel(j.Root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s isn't inside the jail", dir)
	}
	if rel == "." {
		return nil
	}
	cur := j.Root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		if err := os.Mkdir(cur, 0o700); err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return err
		}
		if err := os.Chown(cur, j.spec.UID, j.spec.GID); err != nil {
			return err
		}
	}
	return nil
}

// Command returns the jailer command that starts Firecracker in this jail.
// The caller starts it and owns the process. It isn't tied to a context on
// purpose: VMs keep running when tined restarts.
//
// Set the command's Stdout to a file (see ConsoleLogPath) rather than a pipe,
// so the guest's console output doesn't depend on tined staying up.
func (j *Jail) Command() *exec.Cmd {
	args := []string{
		"--id", j.ID,
		"--exec-file", j.cfg.FirecrackerBin,
		"--uid", strconv.Itoa(j.spec.UID),
		"--gid", strconv.Itoa(j.spec.GID),
		"--chroot-base-dir", j.cfg.ChrootBaseDir,
		"--cgroup-version", "2",
		"--parent-cgroup", j.cfg.CgroupParent,
	}
	for _, cg := range j.cgroupSettings() {
		args = append(args, "--cgroup", cg)
	}
	if j.spec.NetNS != "" {
		args = append(args, "--netns", j.spec.NetNS)
	}
	args = append(args, "--", "--api-sock", APISocketPath)
	args = append(args, j.spec.FirecrackerArgs...)

	cmd := exec.Command(j.cfg.JailerBin, args...) //nolint:gosec,noctx // the binary comes from tined's config; the VM must outlive any one request
	cmd.Env = []string{}                          // the jailer clears its environment anyway
	// Its own process group, so signals meant for tined (like Ctrl+C) don't reach VMs.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// cgroupSettings are the --cgroup values for this VM's limits.
func (j *Jail) cgroupSettings() []string {
	l := j.spec.Limits
	return []string{
		fmt.Sprintf("cpu.max=%d %d", l.VCPUs*cpuPeriodMicros, cpuPeriodMicros),
		fmt.Sprintf("memory.max=%d", l.MemoryBytes),
		fmt.Sprintf("pids.max=%d", l.MaxPIDs),
	}
}

// PID returns Firecracker's process ID, from the pid file the jailer writes.
func (j *Jail) PID() (int, error) {
	data, err := os.ReadFile(filepath.Join(j.Root, filepath.Base(j.cfg.FirecrackerBin)+".pid"))
	if err != nil {
		return 0, fmt.Errorf("jail %s: read pid file: %w", j.ID, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("jail %s: bad pid file contents %q", j.ID, data)
	}
	return pid, nil
}

// Cleanup removes the jail's cgroup and directory. Firecracker must have
// exited first: a cgroup can't be removed while a process is in it. Cleaning
// up a jail that's already gone is fine.
//
// If the cgroup can't be removed, something may still run in it, so the
// jail's files are left alone and Cleanup returns the error.
func (j *Jail) Cleanup() error {
	if err := removeCgroup(j.CgroupDir()); err != nil {
		return fmt.Errorf("clean up jail %s (its files are kept while the cgroup remains): %w", j.ID, err)
	}
	if err := os.RemoveAll(j.Dir); err != nil {
		return fmt.Errorf("clean up jail %s: remove %s: %w", j.ID, j.Dir, err)
	}
	return nil
}
