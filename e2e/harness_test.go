//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sanidhyasin/tine/internal/api"
	"github.com/sanidhyasin/tine/internal/netns"
)

// Where tined keeps things on the dev server (its defaults).
const (
	jailsDir     = "/srv/tine/jail/firecracker"
	cgroupsDir   = "/sys/fs/cgroup/tine"
	snapshotsDir = "/srv/tine/snapshots"
)

// gcWait is how long the end of the suite gives tined's reaper (every 15s)
// to delete the snapshots of destroyed VMs.
const gcWait = 45 * time.Second

var tined = api.NewUnixClient(api.DefaultSocket)

// TestMain checks the host is ready, starts a VM that must expire while the
// other tests run, and ends with the strict leak check.
func TestMain(m *testing.M) {
	if os.Geteuid() != 0 {
		fmt.Fprintln(os.Stderr, "e2e: needs root (make e2e runs it with sudo)")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	if err := waitHealthy(ctx); err != nil {
		cancel()
		fmt.Fprintln(os.Stderr, "e2e: tined isn't answering:", err)
		os.Exit(1)
	}
	// tined may be warming a template, which it does after a deploy.
	if err := waitIdle(ctx); err != nil {
		cancel()
		fmt.Fprintln(os.Stderr, "e2e: tined is still busy:", err)
		os.Exit(1)
	}
	if left := leftovers(); len(left) > 0 {
		cancel()
		fmt.Fprintln(os.Stderr, "e2e: the host isn't clean before starting:", left)
		os.Exit(1)
	}
	// The snapshot tests restore VMs from base's template.
	if err := ensureTemplate(ctx); err != nil {
		cancel()
		fmt.Fprintln(os.Stderr, "e2e: no usable template of base:", err)
		os.Exit(1)
	}
	// A VM with a one-minute life, created first so its expiry overlaps the
	// other tests. It must be gone by the end.
	ttlVM, err := tined.CreateVM(ctx, api.CreateVMRequest{VCPUs: 1, MemoryMiB: 256, TTLSeconds: 60})
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: create the TTL VM:", err)
		os.Exit(1)
	}
	ttlStart := time.Now()

	code := m.Run()

	if code == 0 {
		code = checkReaped(ttlVM.ID, ttlStart)
	}
	if code == 0 {
		// The reaper deletes destroyed VMs' snapshots every 15s.
		left := leftovers()
		for deadline := time.Now().Add(gcWait); len(left) > 0 && time.Now().Before(deadline); left = leftovers() {
			time.Sleep(time.Second)
		}
		if len(left) > 0 {
			fmt.Fprintln(os.Stderr, "e2e: LEAK CHECK FAILED, left on the host:", left)
			code = 1
		} else {
			fmt.Println("e2e: leak check passed: no VMs, namespaces, veths, jails, cgroups, Firecracker processes or snapshots left, except templates")
		}
	}
	os.Exit(code)
}

// checkReaped waits until the TTL VM is destroyed by the reaper, and checks
// that it was destroyed in time: by its TTL, plus the reaper's interval and
// a restart or two. The other tests may well outlast that, so it always
// looks at least once.
func checkReaped(id string, created time.Time) int {
	const allowed = 60*time.Second + 45*time.Second
	deadline := time.Now().Add(allowed)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		vm, err := tined.GetVM(ctx, id)
		cancel()
		if err == nil && vm.State == "destroyed" && vm.DestroyedAt != nil {
			took := vm.DestroyedAt.Sub(created)
			if took > allowed {
				fmt.Fprintf(os.Stderr, "e2e: TTL VM %s was reaped %v after it was created, later than %v\n", id, took.Round(time.Second), allowed)
				return 1
			}
			fmt.Printf("e2e: TTL VM %s was reaped %v after it was created\n", id, took.Round(time.Second))
			return 0
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "e2e: TTL VM %s isn't reaped: %v %v\n", id, vm.State, err)
			return 1
		}
		time.Sleep(time.Second)
	}
}

// leftovers lists VM resources on the host, and live VMs tined still knows.
func leftovers() []string {
	var left []string
	if ids, veths, err := netns.List(); err != nil {
		left = append(left, "can't list networking: "+err.Error())
	} else {
		for _, id := range ids {
			left = append(left, "namespace "+id)
		}
		for _, v := range veths {
			left = append(left, "veth "+v)
		}
	}
	for _, dir := range []string{jailsDir, cgroupsDir} {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), "vm-") {
				left = append(left, dir+"/"+e.Name())
			}
		}
	}
	if out, err := exec.CommandContext(context.Background(), "pgrep", "-x", "firecracker").Output(); err == nil && len(out) > 0 {
		left = append(left, "firecracker pids "+strings.Join(strings.Fields(string(out)), ","))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if vms, err := tined.ListVMs(ctx, false); err == nil {
		for _, vm := range vms {
			left = append(left, "live VM "+vm.ID+" ("+vm.State+")")
		}
	}
	// Only ready templates may stay, and the store holds exactly the
	// snapshots tined has records of.
	snaps, err := tined.ListSnapshots(ctx, api.SnapshotFilter{})
	if err != nil {
		return append(left, "can't list snapshots: "+err.Error())
	}
	known := map[string]bool{}
	for _, s := range snaps {
		known[s.ID] = true
		if s.Kind != "template" || s.State != "ready" {
			left = append(left, fmt.Sprintf("%s snapshot %s (%s, of %s)", s.Kind, s.ID, s.State, s.VMID))
		}
	}
	entries, _ := os.ReadDir(snapshotsDir)
	for _, e := range entries {
		if !known[e.Name()] {
			left = append(left, "snapshot files "+filepath.Join(snapshotsDir, e.Name()))
		}
	}
	return left
}

// ensureTemplate warms a template of base at the default size, unless
// there's a usable one.
func ensureTemplate(ctx context.Context) error {
	imgs, err := tined.Images(ctx)
	if err != nil {
		return err
	}
	for _, img := range imgs {
		for _, t := range img.Templates {
			if img.Name == "base" && t.VCPUs == 2 && t.MemoryMiB == 1024 && t.Problem == "" {
				return nil
			}
		}
	}
	snap, err := tined.WarmTemplate(ctx, "base", api.WarmRequest{})
	if err == nil {
		fmt.Println("e2e: warmed template", snap.ID)
	}
	return err
}

// waitIdle waits until tined has no live VMs, as when it's warming a template.
func waitIdle(ctx context.Context) error {
	for {
		vms, err := tined.ListVMs(ctx, false)
		if err == nil && len(vms) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %d VMs still live", ctx.Err(), len(vms))
		case <-time.After(time.Second):
		}
	}
}

// requireGone fails unless nothing of the VM is left on the host.
func requireGone(t *testing.T, id string) {
	t.Helper()
	var left []string
	if ids, _, err := netns.List(); err == nil {
		for _, n := range ids {
			if n == id {
				left = append(left, "network namespace")
			}
		}
	}
	for _, p := range []string{filepath.Join(jailsDir, id), filepath.Join(cgroupsDir, id)} {
		if _, err := os.Stat(p); err == nil {
			left = append(left, p)
		}
	}
	if len(left) > 0 {
		t.Errorf("VM %s left: %v", id, left)
	}
}

// waitHealthy waits until tined answers.
func waitHealthy(ctx context.Context) error {
	var last error
	for {
		attempt, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := tined.Health(attempt)
		cancel()
		if err == nil {
			return nil
		}
		last = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w (last error: %w)", ctx.Err(), last)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// killTined SIGKILLs tined, as a crash would, and waits for systemd to bring
// it back. It returns how long tined was unreachable.
func killTined(t *testing.T) time.Duration {
	t.Helper()
	ctx := context.Background()
	before, err := tined.Health(ctx)
	if err != nil {
		t.Fatalf("tined isn't healthy before the kill: %v", err)
	}
	start := time.Now()
	systemctl(t, "kill", "--signal=SIGKILL", "tined")
	// Wait for a new tined: its uptime starts again from zero.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		attempt, cancel := context.WithTimeout(ctx, 2*time.Second)
		h, err := tined.Health(attempt)
		cancel()
		if err == nil && h.UptimeSeconds < before.UptimeSeconds && h.UptimeSeconds < time.Since(start).Seconds() {
			return time.Since(start)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("tined didn't come back within 30s of being killed")
	return 0
}

func systemctl(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.CommandContext(context.Background(), "systemctl", args...).CombinedOutput(); err != nil { //nolint:gosec // fixed test commands
		t.Fatalf("systemctl %s: %v: %s", strings.Join(args, " "), err, out)
	}
}

// run runs a command in a VM and returns its stdout and exit code.
func run(ctx context.Context, id string, args ...string) (string, int, error) {
	var out strings.Builder
	st, err := tined.Exec(ctx, id, api.ExecRequest{Args: args, TimeoutMs: 30_000}, func(stream string, data []byte) {
		if stream == "stdout" {
			out.Write(data)
		}
	})
	return out.String(), st.Code, err
}

// mustRun is run, failing the test unless the command exits 0.
func mustRun(t *testing.T, id string, args ...string) string {
	t.Helper()
	out, code, err := run(context.Background(), id, args...)
	if err != nil || code != 0 {
		t.Fatalf("%s: %q: exit %d, %v (output %q)", id, args, code, err, out)
	}
	return out
}

// create makes a small VM and registers its destruction for the end of the test.
func create(t *testing.T) api.VM {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	vm, err := tined.CreateVM(ctx, api.CreateVMRequest{VCPUs: 1, MemoryMiB: 512, TTLSeconds: 600})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _, _ = tined.DestroyVM(context.Background(), vm.ID) })
	return vm
}

// waitState waits until a VM reaches want.
func waitState(t *testing.T, id, want string, timeout time.Duration) api.VM {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var vm api.VM
	var err error
	for time.Now().Before(deadline) {
		vm, err = tined.GetVM(context.Background(), id)
		if err == nil && vm.State == want {
			return vm
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s didn't become %s within %v: it's %q (%v)", id, want, timeout, vm.State, err)
	return vm
}

// waitForCreating returns the first VM that tined is creating.
func waitForCreating(t *testing.T, timeout time.Duration) api.VM {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		vms, err := tined.ListVMs(context.Background(), false)
		if err == nil {
			for _, vm := range vms {
				if vm.State == "creating" {
					return vm
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no VM appeared in the creating state")
	return api.VM{}
}

// percentiles summarizes durations as "p50 X, p99 Y, max Z".
func percentiles(ds []time.Duration) string {
	if len(ds) == 0 {
		return "no samples"
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	at := func(p float64) time.Duration { return s[int(p*float64(len(s)-1)+0.5)] }
	return fmt.Sprintf("p50 %v, p99 %v, max %v (n=%d)", at(0.5).Round(time.Millisecond), at(0.99).Round(time.Millisecond), s[len(s)-1].Round(time.Millisecond), len(s))
}
