//go:build integration

package jailer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sanidhyasin/tine/internal/firecracker"
	"github.com/sanidhyasin/tine/internal/testenv"
)

// TestJailedFirecracker boots a real VM inside a jail and checks everything
// the jail promises: Firecracker runs as the VM's own uid, in its own
// filesystem root, in its own cgroup with the limits applied, the guest
// kernel boots, and Cleanup leaves nothing behind.
func TestJailedFirecracker(t *testing.T) {
	testenv.RequireRoot(t)
	testenv.RequireKVM(t)
	a := testenv.RequireArtifacts(t)
	data := testenv.RequireDataDir(t)
	// Hard links and clones need their sources on the same filesystem as the jail.
	kernel := testenv.CachedCopy(t, data, a.Kernel)
	rootfs := testenv.CachedCopy(t, data, a.Rootfs)

	cfg := Config{
		JailerBin:      a.Jailer,
		FirecrackerBin: a.Firecracker,
		ChrootBaseDir:  testenv.TempDir(t, data),
		CgroupParent:   "tine-test",
	}
	if err := cfg.EnsureCgroupParent(); err != nil {
		t.Fatalf("EnsureCgroupParent: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(filepath.Join(DefaultCgroupRoot, cfg.CgroupParent)) })

	const uid = 299_990
	spec := Spec{
		ID:     "vm-itest-" + randomHex(t),
		UID:    uid,
		GID:    uid,
		Limits: Limits{VCPUs: 2, MemoryBytes: 512 << 20, MaxPIDs: 64},
		Files: []File{
			{Source: kernel, Target: "/vmlinux", Mode: Link},
			{Source: rootfs, Target: "/rootfs.ext4", Mode: Clone},
		},
		FirecrackerArgs: []string{"--enable-pci"},
	}
	j, err := New(cfg, spec)
	if err != nil {
		t.Fatal(err)
	}

	// --- Prepare ---
	start := time.Now()
	if err := j.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	prepared := time.Since(start)

	disk, err := os.Stat(j.HostPath("/rootfs.ext4"))
	if err != nil {
		t.Fatal(err)
	}
	if st := disk.Sys().(*syscall.Stat_t); st.Uid != uid || disk.Mode().Perm() != 0o600 {
		t.Errorf("VM disk owned by uid %d with mode %v; want uid %d and 0600", st.Uid, disk.Mode().Perm(), uid)
	}
	src, _ := os.Stat(kernel)
	if linked, err := os.Stat(j.HostPath("/vmlinux")); err != nil || !os.SameFile(src, linked) {
		t.Errorf("/vmlinux isn't a hard link to the shared kernel (%v)", err)
	}

	// --- Start ---
	console, err := os.OpenFile(j.ConsoleLogPath(), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	cmd := j.Command()
	cmd.Stdout, cmd.Stderr = console, console
	start = time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start jailer: %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = cmd.Process.Kill()
			t.Logf("Firecracker exited: %v", <-exited)
		}
		_ = console.Close()
		_ = j.Cleanup()
		if t.Failed() {
			t.Logf("console:\n%s", tail(j.ConsoleLogPath()))
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c := firecracker.New(j.APISocket())
	if err := c.WaitReady(ctx, 2*time.Millisecond); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	apiReady := time.Since(start)

	// Paths are inside the jail: every VM uses the same ones.
	steps := []struct {
		name string
		do   func() error
	}{
		{"machine config", func() error {
			return c.PutMachineConfig(ctx, firecracker.MachineConfig{VCPUCount: 2, MemSizeMiB: 512})
		}},
		{"boot source", func() error {
			return c.PutBootSource(ctx, firecracker.BootSource{KernelImagePath: "/vmlinux", BootArgs: "console=ttyS0 reboot=k panic=1"})
		}},
		{"drive", func() error {
			return c.PutDrive(ctx, firecracker.Drive{DriveID: "rootfs", PathOnHost: "/rootfs.ext4", IsRootDevice: true})
		}},
		{"start", func() error { return c.StartInstance(ctx) }},
	}
	for _, s := range steps {
		if err := s.do(); err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
	}
	started := time.Since(start)

	// --- What the jail promises ---
	pid := cmd.Process.Pid
	if got, err := j.PID(); err != nil || got != pid {
		t.Errorf("pid file says %d (%v); the jailer should have become Firecracker in place, pid %d", got, err, pid)
	}
	if uids := procStatusField(t, pid, "Uid"); uids[0] != strconv.Itoa(uid) || uids[1] != strconv.Itoa(uid) {
		t.Errorf("Firecracker runs as uid %v, want %d", uids, uid)
	}
	if _, err := os.Stat(fmt.Sprintf("/proc/%d/root%s", pid, APISocketPath)); err != nil {
		t.Errorf("Firecracker's / doesn't hold api.sock, so it isn't the jail: %v", err)
	}
	ours, _ := os.Readlink("/proc/self/ns/mnt")
	theirs, _ := os.Readlink(fmt.Sprintf("/proc/%d/ns/mnt", pid))
	if ours == theirs {
		t.Error("Firecracker shares tined's mount namespace; the jailer should have given it its own")
	}
	if got, want := readTrimmed(t, fmt.Sprintf("/proc/%d/cgroup", pid)), "0::/tine-test/"+j.ID; got != want {
		t.Errorf("Firecracker's cgroup = %q, want %q", got, want)
	}
	for file, want := range map[string]string{
		"cpu.max":    "200000 100000",
		"memory.max": strconv.Itoa(512 << 20),
		"pids.max":   "64",
	} {
		if got := readTrimmed(t, filepath.Join(j.CgroupDir(), file)); got != want {
			t.Errorf("%s = %q, want %q", file, got, want)
		}
	}

	// The guest kernel really boots inside the jail.
	if !waitForConsole(j.ConsoleLogPath(), "Linux version", 15*time.Second) {
		t.Fatal("the guest kernel never printed its banner")
	}
	booted := time.Since(start)

	// --- Stop ---
	// Ask the guest to shut down (Ctrl+Alt+Del, then reboot=k makes Firecracker
	// exit), but don't wait long: sandboxes are disposable, so tined will kill
	// them rather than wait on a guest. The log records which happened.
	shutdownStart := time.Now()
	if err := c.SendCtrlAltDel(ctx); err != nil {
		t.Fatalf("SendCtrlAltDel: %v", err)
	}
	shutdown := "graceful"
	select {
	case err := <-exited:
		t.Logf("Firecracker exited: %v", err)
	case <-time.After(5 * time.Second):
		shutdown = "guest didn't stop within 5s, killed"
		_ = cmd.Process.Kill()
		t.Logf("Firecracker exited: %v", <-exited)
	}
	stopped = true
	shutdownTook := time.Since(shutdownStart)

	if err := j.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	for _, p := range []string{j.Dir, j.CgroupDir()} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still exists after Cleanup", p)
		}
	}
	if err := j.Cleanup(); err != nil {
		t.Errorf("second Cleanup: %v", err)
	}

	t.Logf("prepare jail: %v", prepared.Round(time.Microsecond))
	t.Logf("jailer start -> API socket ready: %v", apiReady.Round(time.Microsecond))
	t.Logf("jailer start -> VM started: %v", started.Round(time.Microsecond))
	t.Logf("jailer start -> guest kernel banner: %v", booted.Round(time.Millisecond))
	t.Logf("shutdown (%s): %v", shutdown, shutdownTook.Round(time.Millisecond))
}

func randomHex(t *testing.T) string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// procStatusField returns a field's values from /proc/<pid>/status, e.g. the
// four uids on the "Uid:" line.
func procStatusField(t *testing.T, pid int, name string) []string {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, name+":") {
			if fields := strings.Fields(strings.TrimPrefix(line, name+":")); len(fields) >= 2 {
				return fields
			}
		}
	}
	t.Fatalf("no %s line in /proc/%d/status", name, pid)
	return nil
}

func readTrimmed(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test reads /proc and cgroup files
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

func waitForConsole(path, text string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && strings.Contains(string(data), text) { //nolint:gosec // test reads its own console log
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func tail(path string) string {
	data, err := os.ReadFile(path) //nolint:gosec // test reads its own console log
	if err != nil {
		return err.Error()
	}
	if len(data) > 4000 {
		data = data[len(data)-4000:]
	}
	return string(data)
}
