//go:build integration

package vm

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sanidhyasin/tine/internal/agentproto"
	"github.com/sanidhyasin/tine/internal/image"
	"github.com/sanidhyasin/tine/internal/netns"
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
	if out, err := exec.CommandContext(context.Background(), "pgrep", "-x", "firecracker").Output(); err == nil && len(out) > 0 {
		left = append(left, "firecracker pids "+strings.Join(strings.Fields(string(out)), ","))
	}
	if len(left) > 0 {
		t.Fatalf("leftovers %s: %v", when, left)
	}
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

func formatTimings(t map[string]float64) string {
	order := []string{"allocate", "network", "jail", "start", "configure", "agent", "total", "destroy"}
	var parts []string
	for _, k := range order {
		if v, ok := t[k]; ok {
			parts = append(parts, fmt.Sprintf("%s %.1f", k, v))
		}
	}
	return strings.Join(parts, " · ")
}
