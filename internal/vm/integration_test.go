//go:build integration

package vm

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sanidhyasin/tine/internal/agentproto"
	"github.com/sanidhyasin/tine/internal/image"
	"github.com/sanidhyasin/tine/internal/netns"
	"github.com/sanidhyasin/tine/internal/snapshot"
	"github.com/sanidhyasin/tine/internal/state"
	"github.com/sanidhyasin/tine/internal/testenv"
)

// realManager is a Manager on the real host, with its own store, image,
// jails and cgroup parent, all removed when the test ends.
func realManager(t *testing.T, change func(*Config)) *Manager {
	t.Helper()
	testenv.RequireRoot(t)
	testenv.RequireKVM(t)
	a := testenv.RequireArtifacts(t)
	agentBin := testenv.RequireAgentBinary(t)
	data := testenv.RequireDataDir(t)
	ctx := context.Background()

	dir := testenv.TempDir(t, data)
	images := filepath.Join(dir, "images")
	if _, err := image.Import(ctx, image.ImportOptions{Name: "base", ImagesDir: images, Kernel: a.Kernel, RootfsSquashfs: a.Squashfs, Agent: agentBin}); err != nil {
		t.Fatalf("import image: %v", err)
	}
	store, err := state.Open(ctx, filepath.Join(dir, "tine.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	cfg := Config{
		JailerBin:      a.Jailer,
		FirecrackerBin: a.Firecracker,
		ChrootBaseDir:  filepath.Join(dir, "jail"),
		ImagesDir:      images,
		SnapshotsDir:   filepath.Join(dir, "snapshots"),
		CgroupParent:   "tine-test",
		UIDBase:        290_000,
	}
	if change != nil {
		change(&cfg)
	}
	m, err := New(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Setup(ctx); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	t.Cleanup(func() {
		_ = netns.RemoveHostRules(context.Background())
		_ = os.Remove("/sys/fs/cgroup/tine-test")
	})
	requireClean(t, m, "before the test")
	return m
}

// requireClean fails if any VM resource exists on the host: network
// namespaces or veths, jails, VM cgroups, or Firecracker processes.
func requireClean(t *testing.T, m *Manager, when string) {
	t.Helper()
	var left []string
	nets, err := netns.Leftovers()
	if err != nil {
		t.Fatal(err)
	}
	left = append(left, nets...)
	jails, _ := os.ReadDir(filepath.Join(m.cfg.ChrootBaseDir, "firecracker"))
	for _, j := range jails {
		left = append(left, "jail "+j.Name())
	}
	cgroups, _ := os.ReadDir("/sys/fs/cgroup/" + m.cfg.CgroupParent)
	for _, c := range cgroups {
		if c.IsDir() {
			left = append(left, "cgroup "+c.Name())
		}
	}
	// A killed Firecracker that this test process started lingers as a zombie
	// until its watcher goroutine reaps it, so give that a moment.
	var pids string
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		out, err := exec.CommandContext(context.Background(), "pgrep", "-x", "firecracker").Output()
		if pids = strings.Join(strings.Fields(string(out)), ","); err != nil || pids == "" || time.Now().After(deadline) {
			break
		}
	}
	if pids != "" {
		left = append(left, "firecracker pids "+pids)
	}
	if len(left) > 0 {
		t.Fatalf("leftovers %s: %v", when, left)
	}
}

// execCode runs a command in a VM and returns its output and exit code.
func execCode(t *testing.T, m *Manager, id string, args ...string) (string, int) {
	t.Helper()
	var out strings.Builder
	st, err := m.Exec(context.Background(), id, agentproto.ExecRequest{Args: args, TimeoutMs: 20_000}, func(_ string, d []byte) { out.Write(d) })
	if err != nil {
		t.Fatalf("exec %q in %s: %v", args, id, err)
	}
	return out.String(), st.Code
}

func execOut(t *testing.T, m *Manager, id string, args ...string) string {
	t.Helper()
	var out strings.Builder
	st, err := m.Exec(context.Background(), id, agentproto.ExecRequest{Args: args, TimeoutMs: 20_000}, func(_ string, d []byte) { out.Write(d) })
	if err != nil || st.Code != 0 {
		t.Fatalf("exec %q in %s: %+v, %v (output %q)", args, id, st, err, out.String())
	}
	return out.String()
}

func TestRealCreateExecDestroy(t *testing.T) {
	m := realManager(t, nil)
	ctx := context.Background()

	vm, err := m.Create(ctx, CreateRequest{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _, _ = m.Destroy(context.Background(), vm.ID) })
	if vm.State != state.StateRunning || vm.PID == 0 {
		t.Fatalf("VM = %+v", vm)
	}

	if got := strings.TrimSpace(execOut(t, m, vm.ID, "id", "-u")); got != "0" {
		t.Errorf("id -u = %q", got)
	}
	if got := execOut(t, m, vm.ID, "cat", "/etc/tine-image"); !strings.Contains(got, `"image": "base"`) {
		t.Errorf("/etc/tine-image = %q", got)
	}
	if got := strings.TrimSpace(execOut(t, m, vm.ID, "nproc")); got != "2" {
		t.Errorf("nproc = %q, want 2 (the default)", got)
	}
	// The disk is mounted read-write from boot (the "rw" kernel argument).
	if got := execOut(t, m, vm.ID, "sh", "-c", "echo hello > /root/f && cat /root/f && grep ' / ' /proc/mounts"); !strings.Contains(got, "hello") || !strings.Contains(got, " rw") {
		t.Errorf("write to the disk: %q", got)
	}

	vm, err = m.Destroy(ctx, vm.ID)
	if err != nil || vm.State != state.StateDestroyed {
		t.Fatalf("Destroy = %+v, %v", vm, err)
	}
	requireClean(t, m, "after Destroy")
	t.Logf("create timings (ms): %s", formatTimings(vm.Timings))
}

// TestRealSnapshot snapshots a real, running VM twice, and checks the VM
// carries on (its agent answers again after the vsock reset) and the
// snapshot's files are complete, read-only and owned by root.
func TestRealSnapshot(t *testing.T) {
	m := realManager(t, nil)
	ctx := context.Background()
	vm, err := m.Create(ctx, CreateRequest{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _, _ = m.Destroy(context.Background(), vm.ID) })
	execOut(t, m, vm.ID, "sh", "-c", "echo before > /root/marker")

	for i := range 2 {
		snap, err := m.Snapshot(ctx, vm.ID)
		if err != nil {
			t.Fatalf("Snapshot %d: %v", i+1, err)
		}
		meta, err := m.snaps.Load(snap.ID)
		if err != nil {
			t.Fatalf("snapshot %s's files: %v", snap.ID, err)
		}
		t.Logf("snapshot %d of a %d MiB VM: %d MiB on disk; timings (ms): %s", i+1, vm.MemoryMiB, snap.SizeBytes>>20, formatTimings(meta.TimingsMs))
		if meta.Host.Firecracker == "" || meta.Host.CPU == "" || meta.Host.Kernel == "" {
			t.Errorf("host = %+v", meta.Host)
		}
		for file, mode := range map[string]os.FileMode{snapshot.StateFile: 0o444, snapshot.MemoryFile: 0o444, snapshot.DiskFile: 0o400} {
			fi, err := os.Stat(m.snaps.Path(snap.ID, file))
			if err != nil {
				t.Fatal(err)
			}
			st := fi.Sys().(*syscall.Stat_t)
			if fi.Mode().Perm() != mode || st.Uid != 0 {
				t.Errorf("%s: mode %v, owner %d; want %v, root", file, fi.Mode().Perm(), st.Uid, mode)
			}
		}
		if mem, _ := os.Stat(m.snaps.Path(snap.ID, snapshot.MemoryFile)); mem.Size() != int64(vm.MemoryMiB)<<20 {
			t.Errorf("memory file is %d bytes, want the VM's %d MiB", mem.Size(), vm.MemoryMiB)
		}
		// The VM carries on, and its agent answers again straight away.
		if got := strings.TrimSpace(execOut(t, m, vm.ID, "cat", "/root/marker")); got != "before" {
			t.Errorf("after snapshot %d, /root/marker = %q", i+1, got)
		}
	}
	_, j, _ := m.resources(vm)
	if left, _ := os.ReadDir(j.HostPath(snapshotDir)); len(left) != 0 {
		t.Errorf("files left in the jail's snapshot directory: %v", left)
	}
	if _, err := m.Destroy(ctx, vm.ID); err != nil {
		t.Fatal(err)
	}
	requireClean(t, m, "after Destroy")
}

// TestRealRestore restores a real VM from a snapshot, twice, while the
// original keeps running. Each copy has the snapshot's disk and memory (a
// process started before the snapshot is still running), its own identity,
// the right time, and a working network.
func TestRealRestore(t *testing.T) {
	m := realManager(t, nil)
	ctx := context.Background()
	parent, err := m.Create(ctx, CreateRequest{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _, _ = m.Destroy(context.Background(), parent.ID) })
	execOut(t, m, parent.ID, "sh", "-c", "echo before > /root/marker; setsid sleep 100000 >/dev/null 2>&1 < /dev/null & echo $! > /root/sleeper")
	snap, err := m.Snapshot(ctx, parent.ID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	execOut(t, m, parent.ID, "sh", "-c", "echo after > /root/marker")
	parentMachineID := execOut(t, m, parent.ID, "cat", "/etc/machine-id")

	var children []state.VM
	for i := range 2 {
		child, err := m.Create(ctx, CreateRequest{SnapshotID: snap.ID})
		if err != nil {
			t.Fatalf("restore %d: %v", i+1, err)
		}
		t.Cleanup(func() { _, _ = m.Destroy(context.Background(), child.ID) })
		children = append(children, child)
		t.Logf("restore %d: %s", i+1, formatTimings(child.Timings))

		if got := strings.TrimSpace(execOut(t, m, child.ID, "cat", "/root/marker")); got != "before" {
			t.Errorf("restore %d: /root/marker = %q, want the snapshot's \"before\"", i+1, got)
		}
		if out, code := execCode(t, m, child.ID, "sh", "-c", "kill -0 $(cat /root/sleeper)"); code != 0 {
			t.Errorf("restore %d: the process started before the snapshot isn't running: %q", i+1, out)
		}
		if got := strings.TrimSpace(execOut(t, m, child.ID, "hostname")); got != child.ID {
			t.Errorf("restore %d: hostname = %q, want %s", i+1, got, child.ID)
		}
		if got := execOut(t, m, child.ID, "cat", "/etc/machine-id"); got == parentMachineID {
			t.Errorf("restore %d has its parent's machine ID", i+1)
		}
		guest, err := strconv.ParseFloat(strings.TrimSpace(execOut(t, m, child.ID, "date", "+%s.%N")), 64)
		if host := float64(time.Now().UnixNano()) / 1e9; err != nil || math.Abs(host-guest) > 1 {
			t.Errorf("restore %d: guest clock %f, host %f (%v)", i+1, guest, host, err)
		}
		// Straight away, without waiting for the guest's ARP cache to expire.
		if out, code := execCode(t, m, child.ID, "timeout", "5", "bash", "-c", "echo > /dev/tcp/1.1.1.1/443"); code != 0 {
			t.Errorf("restore %d can't reach the internet: exit %d %q", i+1, code, out)
		}
	}
	// Two restores of one snapshot don't share random numbers.
	a := execOut(t, m, children[0].ID, "head", "-c", "16", "/dev/urandom")
	b := execOut(t, m, children[1].ID, "head", "-c", "16", "/dev/urandom")
	if a == b {
		t.Error("two restores read the same random bytes")
	}
	// The original still runs, with its own disk.
	if got := strings.TrimSpace(execOut(t, m, parent.ID, "cat", "/root/marker")); got != "after" {
		t.Errorf("parent's /root/marker = %q", got)
	}
	events, _ := m.store.Events(ctx, children[0].ID)
	for _, e := range events {
		if strings.HasPrefix(e.Detail, "guest synced") {
			t.Logf("restore 1: %s", e.Detail)
		}
	}
	for _, vm := range append(children, parent) {
		if _, err := m.Destroy(ctx, vm.ID); err != nil {
			t.Fatal(err)
		}
	}
	requireClean(t, m, "after destroying them all")
}

// TestRealRollbackToSnapshot breaks a real VM and rolls it back: its files
// come back, and it keeps its ID, network and identity.
func TestRealRollbackToSnapshot(t *testing.T) {
	m := realManager(t, nil)
	ctx := context.Background()
	vm, err := m.Create(ctx, CreateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.Destroy(context.Background(), vm.ID) })
	execOut(t, m, vm.ID, "sh", "-c", "echo v1 > /root/data")
	machineID := execOut(t, m, vm.ID, "cat", "/etc/machine-id")
	snap, err := m.Snapshot(ctx, vm.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		execOut(t, m, vm.ID, "sh", "-c", "rm /root/data; echo oops > /root/other")
		got, err := m.Rollback(ctx, vm.ID, snap.ID)
		if err != nil {
			t.Fatalf("Rollback %d: %v", i+1, err)
		}
		t.Logf("rollback %d: %.1fms", i+1, got.Timings["rollback"])
		if got.ID != vm.ID || got.Slot != vm.Slot || got.State != state.StateRunning {
			t.Errorf("after rollback %d: %+v", i+1, got)
		}
		if out := strings.TrimSpace(execOut(t, m, vm.ID, "cat", "/root/data")); out != "v1" {
			t.Errorf("rollback %d: /root/data = %q, want v1", i+1, out)
		}
		if _, code := execCode(t, m, vm.ID, "test", "-e", "/root/other"); code == 0 {
			t.Errorf("rollback %d: /root/other, written after the snapshot, is still there", i+1)
		}
		if got := strings.TrimSpace(execOut(t, m, vm.ID, "hostname")); got != vm.ID {
			t.Errorf("rollback %d: hostname = %q", i+1, got)
		}
		if got := execOut(t, m, vm.ID, "cat", "/etc/machine-id"); got != machineID {
			t.Errorf("rollback %d: machine ID changed: %q -> %q", i+1, machineID, got)
		}
		if out, code := execCode(t, m, vm.ID, "timeout", "5", "bash", "-c", "echo > /dev/tcp/1.1.1.1/443"); code != 0 {
			t.Errorf("rollback %d: no internet: exit %d %q", i+1, code, out)
		}
	}
	if _, err := m.Destroy(ctx, vm.ID); err != nil {
		t.Fatal(err)
	}
	requireClean(t, m, "after the test")
}

// TestRealFork forks a real VM into 4 children: each has the parent's disk
// and running processes as at the fork, and its own identity and network,
// and the parent carries on. The children's PSS shows the memory they share.
func TestRealFork(t *testing.T) {
	m := realManager(t, nil)
	ctx := context.Background()
	parent, err := m.Create(ctx, CreateRequest{MemoryMiB: 512})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = m.Destroy(context.Background(), parent.ID) })
	execOut(t, m, parent.ID, "sh", "-c", "echo parent > /root/who; setsid sleep 100000 >/dev/null 2>&1 < /dev/null & echo $! > /root/sleeper")

	res, err := m.Fork(ctx, parent.ID, ForkRequest{Count: 4})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	t.Logf("fork of 4: %s", formatTimings(res.Timings))
	if res.Failed() > 0 {
		t.Fatalf("failed children: %+v", res.Children)
	}
	machineIDs := map[string]bool{strings.TrimSpace(execOut(t, m, parent.ID, "cat", "/etc/machine-id")): true}
	for i, c := range res.Children {
		child := c.VM
		t.Cleanup(func() { _, _ = m.Destroy(context.Background(), child.ID) })
		if got := strings.TrimSpace(execOut(t, m, child.ID, "cat", "/root/who")); got != "parent" {
			t.Errorf("child %d: /root/who = %q", i+1, got)
		}
		if out, code := execCode(t, m, child.ID, "sh", "-c", "kill -0 $(cat /root/sleeper)"); code != 0 {
			t.Errorf("child %d: the parent's process isn't running in it: %q", i+1, out)
		}
		if got := strings.TrimSpace(execOut(t, m, child.ID, "hostname")); got != child.ID {
			t.Errorf("child %d: hostname = %q", i+1, got)
		}
		id := strings.TrimSpace(execOut(t, m, child.ID, "cat", "/etc/machine-id"))
		if machineIDs[id] {
			t.Errorf("child %d shares machine ID %s", i+1, id)
		}
		machineIDs[id] = true
		if out, code := execCode(t, m, child.ID, "timeout", "5", "bash", "-c", "echo > /dev/tcp/1.1.1.1/443"); code != 0 {
			t.Errorf("child %d: no internet: exit %d %q", i+1, code, out)
		}
		execOut(t, m, child.ID, "sh", "-c", "echo child > /root/who")
	}
	// Each child's writes are its own, and the parent is as it was.
	if got := strings.TrimSpace(execOut(t, m, parent.ID, "cat", "/root/who")); got != "parent" {
		t.Errorf("the parent's /root/who = %q", got)
	}
	for _, c := range res.Children {
		rss, pss := smapsRollup(t, c.VM)
		t.Logf("child %s: RSS %d MiB, PSS %d MiB", c.VM.ID, rss>>20, pss>>20)
		if pss >= rss {
			t.Errorf("child %s: PSS %d >= RSS %d: it shares no memory", c.VM.ID, pss, rss)
		}
	}
	for _, c := range res.Children {
		if _, err := m.Destroy(ctx, c.VM.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Destroy(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	requireClean(t, m, "after the test")
}

// smapsRollup reads a VM's Firecracker's RSS and PSS, in bytes.
func smapsRollup(t *testing.T, vm state.VM) (rss, pss int64) {
	t.Helper()
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/smaps_rollup", vm.PID))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		kb, _ := strconv.ParseInt(f[1], 10, 64)
		switch f[0] {
		case "Rss:":
			rss = kb << 10
		case "Pss:":
			pss = kb << 10
		}
	}
	return rss, pss
}

// TestRealTemplate warms a real template, and creates VMs from it and cold.
func TestRealTemplate(t *testing.T) {
	m := realManager(t, nil)
	ctx := context.Background()
	start := time.Now()
	tmpl, err := m.WarmTemplate(ctx, "base", 0, 0)
	if err != nil {
		t.Fatalf("WarmTemplate: %v", err)
	}
	t.Logf("template %s warmed in %v", tmpl.ID, time.Since(start).Round(time.Millisecond))
	for _, req := range []CreateRequest{{}, {}, {Cold: true}} {
		vm, err := m.Create(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%-8s %s", vm.Source, formatTimings(vm.Timings))
		if want := map[bool]state.Source{false: state.SourceTemplate, true: state.SourceCold}[req.Cold]; vm.Source != want {
			t.Errorf("source = %s, want %s", vm.Source, want)
		}
		if got := strings.TrimSpace(execOut(t, m, vm.ID, "hostname")); got != vm.ID {
			t.Errorf("hostname = %q", got)
		}
		if _, err := m.Destroy(ctx, vm.ID); err != nil {
			t.Fatal(err)
		}
	}
	requireClean(t, m, "after the test")
}

// TestRealRestoreClock compares a restore with and without Firecracker's
// clock_realtime, 3 seconds after the snapshot: how far behind the guest's
// clock is before tined sets it, and whether its uptime (monotonic clock)
// jumped.
func TestRealRestoreClock(t *testing.T) {
	for _, realtime := range []bool{false, true} {
		t.Run(fmt.Sprintf("clock_realtime=%v", realtime), func(t *testing.T) {
			m := realManager(t, func(c *Config) { c.ClockRealtime = realtime })
			ctx := context.Background()
			parent, err := m.Create(ctx, CreateRequest{})
			if err != nil {
				t.Fatal(err)
			}
			snap, err := m.Snapshot(ctx, parent.ID)
			if err != nil {
				t.Fatal(err)
			}
			uptimeAtSnapshot := uptime(t, m, parent.ID)
			if _, err := m.Destroy(ctx, parent.ID); err != nil {
				t.Fatal(err)
			}
			time.Sleep(3 * time.Second)
			child, err := m.Create(ctx, CreateRequest{SnapshotID: snap.ID})
			if err != nil {
				t.Fatal(err)
			}
			jump := uptime(t, m, child.ID) - uptimeAtSnapshot
			events, _ := m.store.Events(ctx, child.ID)
			for _, e := range events {
				if strings.HasPrefix(e.Detail, "guest synced") {
					t.Logf("%s; uptime moved %.1fs across the 3s gap", e.Detail, jump)
				}
			}
			if _, err := m.Destroy(ctx, child.ID); err != nil {
				t.Fatal(err)
			}
			requireClean(t, m, "after the test")
		})
	}
}

func uptime(t *testing.T, m *Manager, id string) float64 {
	t.Helper()
	f := strings.Fields(execOut(t, m, id, "cat", "/proc/uptime"))
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestRealParallelCreates(t *testing.T) {
	m := realManager(t, nil)
	ctx := context.Background()
	const count = 4

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		vms  []state.VM
		errs []error
	)
	start := time.Now()
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			vm, err := m.Create(ctx, CreateRequest{VCPUs: 1, MemoryMiB: 512})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			vms = append(vms, vm)
		}()
	}
	wg.Wait()
	allUp := time.Since(start)
	t.Cleanup(func() {
		for _, vm := range vms {
			_, _ = m.Destroy(context.Background(), vm.ID)
		}
	})
	if len(errs) > 0 {
		t.Fatalf("%d creates failed: %v", len(errs), errs)
	}

	// Each VM is its own machine: its own boot, its own disk.
	bootIDs := map[string]bool{}
	for i, vm := range vms {
		bootIDs[strings.TrimSpace(execOut(t, m, vm.ID, "cat", "/proc/sys/kernel/random/boot_id"))] = true
		execOut(t, m, vm.ID, "sh", "-c", fmt.Sprintf("echo %d > /root/me", i))
	}
	if len(bootIDs) != count {
		t.Errorf("%d distinct boot IDs across %d VMs", len(bootIDs), count)
	}
	for i, vm := range vms {
		if got := strings.TrimSpace(execOut(t, m, vm.ID, "cat", "/root/me")); got != fmt.Sprint(i) {
			t.Errorf("VM %d's disk holds %q; disks must be separate", i, got)
		}
	}

	start = time.Now()
	var dwg sync.WaitGroup
	for _, vm := range vms {
		dwg.Add(1)
		go func() {
			defer dwg.Done()
			if _, err := m.Destroy(ctx, vm.ID); err != nil {
				t.Errorf("Destroy %s: %v", vm.ID, err)
			}
		}()
	}
	dwg.Wait()
	allDown := time.Since(start)
	requireClean(t, m, "after destroying them all")

	var totals []float64
	for _, vm := range vms {
		totals = append(totals, vm.Timings["total"])
	}
	sort.Float64s(totals)
	t.Logf("%d VMs created in parallel in %v (each took %.0f-%.0f ms); destroyed in %v", count, allUp.Round(time.Millisecond), totals[0], totals[len(totals)-1], allDown.Round(time.Millisecond))
	t.Logf("one VM's timings (ms): %s", formatTimings(vms[0].Timings))
}

// TestRealDestroyAfterRestart destroys a VM with a manager that didn't
// start it, as happens after tined restarts: it has to find and stop that
// VM's Firecracker by its cgroup.
func TestRealDestroyAfterRestart(t *testing.T) {
	m := realManager(t, nil)
	ctx := context.Background()
	vm, err := m.Create(ctx, CreateRequest{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	restarted, err := New(m.cfg, m.store) // a new manager owns no processes
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(execOut(t, restarted, vm.ID, "echo", "still here")); got != "still here" {
		t.Errorf("exec after the restart = %q", got)
	}
	gone, err := restarted.Destroy(ctx, vm.ID)
	if err != nil || gone.State != state.StateDestroyed {
		t.Fatalf("Destroy after restart = %s, %v", gone.State, err)
	}
	requireClean(t, m, "after destroying it from the restarted manager")
}

// TestRealOrphansAreRemoved: a VM that's running on the host but missing
// from the database (as if its record were lost) is an orphan. Reconcile
// must stop it and remove everything it used.
func TestRealOrphansAreRemoved(t *testing.T) {
	m := realManager(t, nil)
	ctx := context.Background()
	vm, err := m.Create(ctx, CreateRequest{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	empty, err := state.Open(ctx, filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = empty.Close() }()
	other, err := New(m.cfg, empty) // knows nothing about vm
	if err != nil {
		t.Fatal(err)
	}
	r := other.Reconcile(ctx)
	if len(r.Errors) > 0 {
		t.Fatalf("reconcile errors: %v", r.Errors)
	}
	if !strings.Contains(strings.Join(r.Orphans, ","), vm.ID) {
		t.Errorf("report = %s; want %s removed as an orphan", r, vm.ID)
	}
	requireClean(t, m, "after reconciling against an empty database")
	t.Logf("report: %s", r)
}

// TestRealRollback makes a real create fail late (the agent gets too little
// time to start) and checks the undo leaves the host clean.
func TestRealRollback(t *testing.T) {
	m := realManager(t, func(c *Config) { c.AgentTimeout = 50 * time.Millisecond })
	vm, err := m.Create(context.Background(), CreateRequest{})
	if err == nil {
		t.Fatal("Create with a 50ms agent timeout: want an error")
	}
	if vm.State != state.StateDestroyed || !strings.Contains(vm.Error, "agent") {
		t.Errorf("VM after the failed create = %s, error %q; want destroyed, failing at the agent step", vm.State, vm.Error)
	}
	requireClean(t, m, "after the rolled-back create")
	t.Logf("failed as planned: %v", err)
}

// formatTimings lists stage timings in the order the stages happen, and any
// it doesn't know after them.
func formatTimings(t map[string]float64) string {
	order := []string{"allocate", "network", "jail", "start", "configure", "restore", "prepare", "pause", "memory", "disk",
		"resume", "paused", "collect", "agent", "sync", "total", "destroy"}
	var parts []string
	seen := map[string]bool{}
	for _, k := range order {
		if v, ok := t[k]; ok {
			parts = append(parts, fmt.Sprintf("%s %.1f", k, v))
			seen[k] = true
		}
	}
	var rest []string
	for k := range t {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		parts = append(parts, fmt.Sprintf("%s %.1f", k, t[k]))
	}
	return strings.Join(parts, " · ")
}
