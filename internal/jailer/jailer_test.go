package jailer

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sanidhyasin/tine/internal/storage"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		JailerBin:      "/usr/local/bin/jailer",
		FirecrackerBin: "/usr/local/bin/firecracker",
		ChrootBaseDir:  t.TempDir(),
		CgroupParent:   "tine",
		CgroupRoot:     t.TempDir(),
	}
}

// testSpec uses this test process's own uid and gid, so Prepare can chown
// files without being root. Run as root (as the integration tests are on the
// dev server), it uses nobody's instead, since jails refuse uid 0.
func testSpec() Spec {
	uid, gid := os.Getuid(), os.Getgid()
	if uid == 0 {
		uid, gid = 65534, 65534
	}
	return Spec{
		ID:     "vm-test1",
		UID:    uid,
		GID:    gid,
		Limits: Limits{VCPUs: 2, MemoryBytes: 1 << 30, MaxPIDs: 64},
	}
}

func TestNewRejectsBadSpecs(t *testing.T) {
	cfg := testConfig(t)
	tests := []struct {
		name   string
		change func(*Spec)
	}{
		{"empty ID", func(s *Spec) { s.ID = "" }},
		{"path in ID", func(s *Spec) { s.ID = "../etc" }},
		{"slash in ID", func(s *Spec) { s.ID = "a/b" }},
		{"ID over 64 characters", func(s *Spec) { s.ID = strings.Repeat("a", 65) }},
		{"root uid", func(s *Spec) { s.UID = 0 }},
		{"root gid", func(s *Spec) { s.GID = 0 }},
		{"relative netns", func(s *Spec) { s.NetNS = "netns/tine" }},
		{"no CPU limit", func(s *Spec) { s.Limits.VCPUs = 0 }},
		{"no memory limit", func(s *Spec) { s.Limits.MemoryBytes = 0 }},
		{"no pids limit", func(s *Spec) { s.Limits.MaxPIDs = 0 }},
		{"relative target", func(s *Spec) { s.Files = []File{{Source: "/k", Target: "vmlinux"}} }},
		{"target escapes the jail", func(s *Spec) { s.Files = []File{{Source: "/k", Target: "/../etc/passwd"}} }},
		{"target is the root", func(s *Spec) { s.Files = []File{{Source: "/k", Target: "/"}} }},
		{"relative source", func(s *Spec) { s.Files = []File{{Source: "k", Target: "/vmlinux"}} }},
		{"two files on one target", func(s *Spec) {
			s.Files = []File{{Source: "/a", Target: "/x"}, {Source: "/b", Target: "/x", Mode: Clone}}
		}},
		{"unknown mode", func(s *Spec) { s.Files = []File{{Source: "/a", Target: "/x", Mode: FileMode(7)}} }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := testSpec()
			tt.change(&spec)
			if _, err := New(cfg, spec); err == nil {
				t.Error("New: want an error")
			}
		})
	}
	if _, err := New(cfg, testSpec()); err != nil {
		t.Errorf("New with a good spec: %v", err)
	}
}

func TestNewRejectsBadConfigs(t *testing.T) {
	tests := []struct {
		name   string
		change func(*Config)
	}{
		{"relative jailer", func(c *Config) { c.JailerBin = "jailer" }},
		{"relative firecracker", func(c *Config) { c.FirecrackerBin = "firecracker" }},
		{"relative chroot base", func(c *Config) { c.ChrootBaseDir = "srv/tine/jail" }},
		{"empty cgroup parent", func(c *Config) { c.CgroupParent = "" }},
		{"absolute cgroup parent", func(c *Config) { c.CgroupParent = "/tine" }},
		{"cgroup parent escapes", func(c *Config) { c.CgroupParent = "../tine" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testConfig(t)
			tt.change(&cfg)
			if _, err := New(cfg, testSpec()); err == nil {
				t.Error("New: want an error")
			}
		})
	}
}

func TestPaths(t *testing.T) {
	cfg := testConfig(t)
	cfg.ChrootBaseDir = "/srv/tine/jail"
	cfg.CgroupRoot = "/sys/fs/cgroup"
	j, err := New(cfg, testSpec())
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string][2]string{
		"Dir":               {j.Dir, "/srv/tine/jail/firecracker/vm-test1"},
		"Root":              {j.Root, "/srv/tine/jail/firecracker/vm-test1/root"},
		"APISocket":         {j.APISocket(), "/srv/tine/jail/firecracker/vm-test1/root/api.sock"},
		"ConsoleLogPath":    {j.ConsoleLogPath(), "/srv/tine/jail/firecracker/vm-test1/console.log"},
		"CgroupDir":         {j.CgroupDir(), "/sys/fs/cgroup/tine/vm-test1"},
		"HostPath":          {j.HostPath("/rootfs.ext4"), "/srv/tine/jail/firecracker/vm-test1/root/rootfs.ext4"},
		"HostPath stays in": {j.HostPath("/../../etc/passwd"), "/srv/tine/jail/firecracker/vm-test1/root/etc/passwd"},
	}
	for name, c := range checks {
		if c[0] != c[1] {
			t.Errorf("%s = %s, want %s", name, c[0], c[1])
		}
	}
}

func TestCommand(t *testing.T) {
	cfg := testConfig(t)
	spec := testSpec()
	spec.UID, spec.GID = 200001, 200001
	spec.NetNS = "/var/run/netns/tine-vm-test1"
	spec.Limits = Limits{VCPUs: 2, MemoryBytes: 1 << 30, MaxPIDs: 64}
	spec.FirecrackerArgs = []string{"--enable-pci"}
	j, err := New(cfg, spec)
	if err != nil {
		t.Fatal(err)
	}

	cmd := j.Command()
	want := []string{
		"/usr/local/bin/jailer",
		"--id", "vm-test1",
		"--exec-file", "/usr/local/bin/firecracker",
		"--uid", "200001",
		"--gid", "200001",
		"--chroot-base-dir", cfg.ChrootBaseDir,
		"--cgroup-version", "2",
		"--parent-cgroup", "tine",
		"--cgroup", "cpu.max=200000 100000",
		"--cgroup", "memory.max=1073741824",
		"--cgroup", "pids.max=64",
		"--netns", "/var/run/netns/tine-vm-test1",
		"--", "--api-sock", "/api.sock", "--enable-pci",
	}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Errorf("Command args:\n got %q\nwant %q", cmd.Args, want)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		t.Error("Command should start the jailer in its own process group")
	}
	if cmd.Env == nil || len(cmd.Env) != 0 {
		t.Errorf("Command env = %q, want empty (not inherited)", cmd.Env)
	}
}

func TestPrepareLinksFiles(t *testing.T) {
	cfg := testConfig(t)
	src := filepath.Join(t.TempDir(), "vmlinux")
	if err := os.WriteFile(src, []byte("kernel"), 0o644); err != nil { //nolint:gosec // must be world-readable to link
		t.Fatal(err)
	}
	spec := testSpec()
	spec.Files = []File{{Source: src, Target: "/boot/vmlinux", Mode: Link}}
	j, err := New(cfg, spec)
	if err != nil {
		t.Fatal(err)
	}

	if err := j.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	a, _ := os.Stat(src)
	b, err := os.Stat(j.HostPath("/boot/vmlinux"))
	if err != nil || !os.SameFile(a, b) {
		t.Errorf("/boot/vmlinux in the jail isn't a hard link to %s (err %v)", src, err)
	}
	for _, dir := range []string{j.Root, j.HostPath("/boot")} {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Errorf("%s: %v, %v; want a directory with mode 0700", dir, info, err)
		}
	}

	if err := j.Prepare(); err == nil {
		t.Error("Prepare twice: want an error, so an ID is never reused by accident")
	}
}

func TestPrepareRefusesPrivateFilesForLinking(t *testing.T) {
	cfg := testConfig(t)
	src := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(src, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := testSpec()
	spec.Files = []File{{Source: src, Target: "/secret", Mode: Link}}
	j, _ := New(cfg, spec)

	if err := j.Prepare(); err == nil {
		t.Fatal("Prepare with a non-world-readable shared file: want an error")
	}
	if _, err := os.Stat(j.Dir); !os.IsNotExist(err) {
		t.Error("failed Prepare left the jail directory behind")
	}
}

func TestPrepareClonesFiles(t *testing.T) {
	cfg := testConfig(t)
	src := filepath.Join(cfg.ChrootBaseDir, "rootfs.ext4")                 // same filesystem as the jail
	if err := os.WriteFile(src, []byte("disk image"), 0o644); err != nil { //nolint:gosec // test file
		t.Fatal(err)
	}
	spec := testSpec()
	spec.Files = []File{{Source: src, Target: "/rootfs.ext4", Mode: Clone}}
	j, _ := New(cfg, spec)

	err := j.Prepare()
	if errors.Is(err, storage.ErrReflinkUnsupported) {
		if _, statErr := os.Stat(j.Dir); !os.IsNotExist(statErr) {
			t.Error("failed Prepare left the jail directory behind")
		}
		t.Skip("no reflinks on this filesystem; the integration test covers clones")
	}
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	info, err := os.Stat(j.HostPath("/rootfs.ext4"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("cloned disk: %v, %v; want mode 0600", info, err)
	}
}

func TestCleanup(t *testing.T) {
	cfg := testConfig(t)
	j, _ := New(cfg, testSpec())
	if err := j.Prepare(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(j.CgroupDir(), 0o750); err != nil {
		t.Fatal(err)
	}

	if err := j.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	for _, p := range []string{j.Dir, j.CgroupDir()} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still exists after Cleanup", p)
		}
	}
	if err := j.Cleanup(); err != nil {
		t.Errorf("second Cleanup: %v, want nil", err)
	}
}

func TestPID(t *testing.T) {
	cfg := testConfig(t)
	j, _ := New(cfg, testSpec())
	if _, err := j.PID(); err == nil {
		t.Error("PID with no pid file: want an error")
	}
	if err := j.Prepare(); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(j.Root, "firecracker.pid")
	for content, want := range map[string]int{"4242\n": 4242, "oops": 0, "-1": 0} {
		if err := os.WriteFile(pidFile, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		pid, err := j.PID()
		if want == 0 && err == nil {
			t.Errorf("pid file %q: want an error, got %d", content, pid)
		}
		if want != 0 && (err != nil || pid != want) {
			t.Errorf("pid file %q: got %d, %v; want %d", content, pid, err, want)
		}
	}
}

// fakeCgroup creates a directory that looks like a cgroup v2 group.
func fakeCgroup(t *testing.T, dir, available, enabled string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"cgroup.controllers": available, "cgroup.subtree_control": enabled} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil { //nolint:gosec // test file
			t.Fatal(err)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test file
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestEnsureCgroupParent(t *testing.T) {
	cfg := testConfig(t)
	cfg.CgroupParent = "tine/vms"
	root := cfg.CgroupRoot
	// The kernel creates these files in every cgroup; the fake has to.
	fakeCgroup(t, root, "cpuset cpu io memory hugetlb pids", "cpuset memory")
	fakeCgroup(t, filepath.Join(root, "tine"), "cpu memory pids", "cpu memory pids")
	fakeCgroup(t, filepath.Join(root, "tine", "vms"), "cpu memory pids", "")

	if err := cfg.EnsureCgroupParent(); err != nil {
		t.Fatalf("EnsureCgroupParent: %v", err)
	}
	// Only the missing controllers get written; real cgroup files add them to what's there.
	if got := readFile(t, filepath.Join(root, "cgroup.subtree_control")); got != "+cpu +pids" {
		t.Errorf("root subtree_control written as %q, want %q", got, "+cpu +pids")
	}
	if got := readFile(t, filepath.Join(root, "tine", "cgroup.subtree_control")); got != "cpu memory pids" {
		t.Errorf("tine subtree_control changed to %q although nothing was missing", got)
	}
	if got := readFile(t, filepath.Join(root, "tine", "vms", "cgroup.subtree_control")); got != "+cpu +memory +pids" {
		t.Errorf("tine/vms subtree_control written as %q, want %q", got, "+cpu +memory +pids")
	}
}

func TestEnsureCgroupParentNeedsTheControllers(t *testing.T) {
	cfg := testConfig(t)
	fakeCgroup(t, cfg.CgroupRoot, "cpu memory", "")
	if err := cfg.EnsureCgroupParent(); err == nil {
		t.Error("EnsureCgroupParent without the pids controller: want an error")
	}
}

func TestFileModeString(t *testing.T) {
	if Link.String() != "link" || Clone.String() != "clone" || FileMode(9).String() != "FileMode(9)" {
		t.Error("unexpected FileMode strings")
	}
}

func TestCreateFile(t *testing.T) {
	cfg := testConfig(t)
	j, _ := New(cfg, testSpec())
	if err := j.Prepare(); err != nil {
		t.Fatal(err)
	}
	if err := j.CreateFile("/logs/firecracker.log"); err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	info, err := os.Stat(j.HostPath("/logs/firecracker.log"))
	if err != nil || info.Size() != 0 || info.Mode().Perm() != 0o600 {
		t.Errorf("created file: %v, %v; want an empty file with mode 0600", info, err)
	}
	if err := j.CreateFile("/logs/firecracker.log"); err == nil {
		t.Error("CreateFile over an existing file: want an error")
	}
	if err := j.CreateFile("relative.log"); err == nil {
		t.Error("CreateFile with a relative path: want an error")
	}
}
