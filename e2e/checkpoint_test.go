//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sanidhyasin/tine/internal/api"
	"github.com/sanidhyasin/tine/internal/netns"
)

// Checkpoints: an agent's VM snapshotted before each step and kept, and any
// step restored later, many times, offline if asked, after the VM is gone.

// retain takes a retained snapshot and deletes it at the end of the test.
func retain(t *testing.T, id string) api.Snapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	snap, err := tined.CreateSnapshotWith(ctx, id, api.SnapshotRequest{Retain: true})
	if err != nil {
		t.Fatalf("retained snapshot of %s: %v", id, err)
	}
	if !snap.Retained {
		t.Fatalf("snapshot %s isn't retained: %+v", snap.ID, snap)
	}
	t.Cleanup(func() { _, _ = tined.DeleteSnapshot(context.Background(), snap.ID) })
	return snap
}

// forkSnapshot restores count children from a snapshot, all of which must
// be made, and destroys them at the end of the test.
func forkSnapshot(t *testing.T, snapID string, req api.ForkRequest) (api.ForkResponse, []api.VM) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	res, err := tined.ForkSnapshot(ctx, snapID, req)
	if err != nil {
		t.Fatalf("fork of %s: %v", snapID, err)
	}
	var children []api.VM
	for _, c := range res.Children {
		if c.VM != nil {
			child := *c.VM
			t.Cleanup(func() { _, _ = tined.DestroyVM(context.Background(), child.ID) })
			children = append(children, child)
		}
	}
	for i, c := range res.Children {
		if c.VM == nil {
			t.Fatalf("child %d of %s couldn't be made: %+v", i+1, snapID, c.Error)
		}
	}
	return res, children
}

// TestRetainedSnapshotOutlivesItsVM: a file on disk, a file in memory
// (tmpfs) and a running process, then a retained snapshot and a plain one,
// and the VM is destroyed. The plain one is collected, the retained one
// stays, through a restart of tined too, and a VM restored from it has all
// three. Restores of 1 and of 10 at once are timed.
func TestRetainedSnapshotOutlivesItsVM(t *testing.T) {
	ctx := context.Background()
	agent := createDefault(t)
	token := strings.TrimSpace(mustRun(t, agent.ID, "sh", "-c", "head -c 8 /dev/urandom | od -An -tx1 | tr -d ' \\n'"))
	mustRun(t, agent.ID, "sh", "-c", "mkdir -p /root/work && echo "+token+" > /root/work/step && echo "+token+" > /dev/shm/in-memory")
	mustRun(t, agent.ID, "sh", "-c", "setsid sleep 100000 >/dev/null 2>&1 < /dev/null & echo $! > /root/work/sleeper")
	pid := strings.TrimSpace(mustRun(t, agent.ID, "cat", "/root/work/sleeper"))

	kept := retain(t, agent.ID)
	plain := snapshotOf(t, agent.ID)
	t.Logf("retained snapshot %s: paused %.0fms, %s", kept.ID, kept.TimingsMs["paused"], strconv.FormatInt(kept.SizeBytes>>20, 10)+" MiB")
	if _, err := tined.DestroyVM(ctx, agent.ID); err != nil {
		t.Fatal(err)
	}

	// The reaper collects the plain one; the retained one stays.
	deadline := time.Now().Add(gcWait)
	for {
		_, err := tined.GetSnapshot(ctx, plain.ID)
		if s, _ := apiCode(err); s == http.StatusNotFound {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the plain snapshot %s of destroyed VM %s is still there after %v", plain.ID, agent.ID, gcWait)
		}
		time.Sleep(time.Second)
	}
	systemctl(t, "restart", "tined")
	rctx, cancel := context.WithTimeout(ctx, time.Minute)
	if err := waitHealthy(rctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	if got, err := tined.GetSnapshot(ctx, kept.ID); err != nil || !got.Retained || got.State != "ready" {
		t.Fatalf("the retained snapshot, once its VM was destroyed and tined restarted: %+v, %v", got, err)
	}

	check := func(id string) {
		t.Helper()
		if got := strings.TrimSpace(mustRun(t, id, "cat", "/root/work/step")); got != token {
			t.Errorf("%s: /root/work/step = %q, want %q", id, got, token)
		}
		if got := strings.TrimSpace(mustRun(t, id, "cat", "/dev/shm/in-memory")); got != token {
			t.Errorf("%s: /dev/shm/in-memory = %q, want %q (memory, not disk)", id, got, token)
		}
		if out, code, err := run(ctx, id, "sh", "-c", "kill -0 "+pid+" && cat /proc/"+pid+"/cmdline | tr '\\0' ' '"); err != nil || code != 0 || !strings.Contains(out, "sleep 100000") {
			t.Errorf("%s: the process started before the snapshot isn't running: %q, exit %d, %v", id, out, code, err)
		}
		if host := strings.TrimSpace(mustRun(t, id, "hostname")); host != id {
			t.Errorf("%s: hostname %q, want its own ID", id, host)
		}
	}

	one, children := forkSnapshot(t, kept.ID, api.ForkRequest{Count: 1, TTLSeconds: 600})
	c := children[0]
	if c.ParentID != agent.ID || c.SnapshotID != kept.ID || c.Source != "snapshot" || c.Network != "default" {
		t.Errorf("the restored VM = %+v", c)
	}
	check(c.ID)
	t.Logf("restore of 1 from a retained snapshot: total %.0fms (the VM: %s)", one.TimingsMs["total"], describe(c.TimingsMs))
	if _, err := tined.DeleteSnapshot(ctx, kept.ID); func() bool { s, code := apiCode(err); return s != http.StatusConflict || code != api.CodeInUse }() {
		t.Errorf("deleting a retained snapshot a VM runs from = %v, want 409 in_use", err)
	}

	ten, children := forkSnapshot(t, kept.ID, api.ForkRequest{Count: 10, TTLSeconds: 600})
	var wg sync.WaitGroup
	for _, c := range children {
		wg.Add(1)
		go func() { defer wg.Done(); check(c.ID) }()
	}
	wg.Wait()
	var each []time.Duration
	for _, c := range children {
		each = append(each, time.Duration(c.TimingsMs["total"]*float64(time.Millisecond)))
	}
	t.Logf("restore of 10 from a retained snapshot: total %.0fms, children %.0fms; each child %s",
		ten.TimingsMs["total"], ten.TimingsMs["children"], percentiles(each))
}

// TestOfflineVMs: a VM with network none can't reach the internet, the
// metadata service or the host, and finds out at once; commands and files
// still work. A fork's children have the network asked for.
func TestOfflineVMs(t *testing.T) {
	ctx := context.Background()
	_, vethsBefore, err := netns.List()
	if err != nil {
		t.Fatal(err)
	}
	vm, err := tined.CreateVM(ctx, api.CreateVMRequest{TTLSeconds: 600, Network: "none"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = tined.DestroyVM(context.Background(), vm.ID) })
	if vm.Network != "none" {
		t.Fatalf("network = %q, want none", vm.Network)
	}
	if _, veths, _ := netns.List(); len(veths) != len(vethsBefore) {
		t.Errorf("host veths went from %d to %d for an offline VM, want no new one", len(vethsBefore), len(veths))
	}
	requireOffline(t, vm.ID)
	if got := strings.TrimSpace(mustRun(t, vm.ID, "sh", "-c", "echo hello > /root/f && cat /root/f")); got != "hello" {
		t.Errorf("exec in an offline VM: %q", got)
	}

	// Its children, online and off.
	online := createDefault(t)
	res, err := tined.Fork(ctx, online.ID, api.ForkRequest{Count: 1, TTLSeconds: 600, Network: "none"})
	if err != nil || res.Children[0].VM == nil {
		t.Fatalf("offline fork of an online VM: %v %+v", err, res)
	}
	offChild := *res.Children[0].VM
	t.Cleanup(func() { _, _ = tined.DestroyVM(context.Background(), offChild.ID) })
	requireOffline(t, offChild.ID)
	if code := tcpExit(t, online.ID, "1.1.1.1:443", 10); code != 0 {
		t.Errorf("the online parent lost the internet (exit %d)", code)
	}

	res, err = tined.Fork(ctx, vm.ID, api.ForkRequest{Count: 1, TTLSeconds: 600})
	if err != nil || res.Children[0].VM == nil {
		t.Fatalf("default fork of an offline VM: %v %+v", err, res)
	}
	onChild := *res.Children[0].VM
	t.Cleanup(func() { _, _ = tined.DestroyVM(context.Background(), onChild.ID) })
	if onChild.Network != "default" || tcpExit(t, onChild.ID, "1.1.1.1:443", 10) != 0 {
		t.Errorf("the default child of an offline VM (%s) can't reach the internet", onChild.Network)
	}

	snap := retain(t, online.ID)
	_, children := forkSnapshot(t, snap.ID, api.ForkRequest{Count: 2, TTLSeconds: 600, Network: "none"})
	for _, c := range children {
		if c.Network != "none" {
			t.Errorf("%s: network %q, want none", c.ID, c.Network)
		}
		requireOffline(t, c.ID)
	}
}

// requireOffline checks a VM reaches nothing beyond itself, but its loopback works.
func requireOffline(t *testing.T, id string) {
	t.Helper()
	for _, target := range []string{"1.1.1.1:443", "8.8.8.8:53", "169.254.169.254:80", "172.16.0.1:22"} {
		start := time.Now()
		if code := tcpExit(t, id, target, 5); code == 0 {
			t.Errorf("%s, offline, reached %s", id, target)
		} else {
			t.Logf("%s, offline, can't reach %s: exit %d after %v", id, target, code, time.Since(start).Round(time.Millisecond))
		}
	}
	out, code, err := run(context.Background(), id, "sh", "-c", "timeout 5 bash -c 'echo > /dev/tcp/1.1.1.1/443' 2>&1")
	if err != nil || code == 0 || !strings.Contains(out, "unreachable") {
		t.Errorf("%s: connecting to the internet didn't fail with \"network unreachable\": %q, exit %d, %v", id, out, code, err)
	}
	// Loopback works: a connection to a closed port on it is refused at once.
	if out, _, err := run(context.Background(), id, "sh", "-c", "timeout 5 bash -c 'echo > /dev/tcp/127.0.0.1/1' 2>&1"); err != nil || !strings.Contains(out, "refused") {
		t.Errorf("%s: loopback doesn't work: %q, %v", id, out, err)
	}
}

// TestFortyRetainedSnapshots: an agent run of 40 steps, a retained snapshot
// before each, past the limit of 16 plain ones. Every step can be restored,
// with what was written before it, once the VM is gone.
func TestFortyRetainedSnapshots(t *testing.T) {
	ctx := context.Background()
	vm, err := tined.CreateVM(ctx, api.CreateVMRequest{VCPUs: 1, MemoryMiB: 256, TTLSeconds: 1800})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = tined.DestroyVM(context.Background(), vm.ID) })
	const steps = 40
	snaps := make([]api.Snapshot, steps)
	var paused, took []time.Duration
	start := time.Now()
	for i := range steps {
		mustRun(t, vm.ID, "sh", "-c", fmt.Sprintf("echo %d > /root/step", i+1))
		before := time.Now()
		snaps[i] = retain(t, vm.ID)
		took = append(took, time.Since(before))
		paused = append(paused, time.Duration(snaps[i].TimingsMs["paused"]*float64(time.Millisecond)))
	}
	t.Logf("%d retained snapshots of a 256 MiB VM in %v: each took %s, paused it %s", steps, time.Since(start).Round(time.Second),
		percentiles(took), percentiles(paused))

	list, err := tined.ListSnapshots(ctx, api.SnapshotFilter{VMID: vm.ID})
	if err != nil {
		t.Fatal(err)
	}
	var retained int
	var bytes int64
	for _, s := range list {
		if s.Retained {
			retained++
			bytes += s.SizeBytes
		}
	}
	if retained != steps {
		t.Errorf("%d retained snapshots listed, want %d", retained, steps)
	}
	t.Logf("they take %d MiB of disk", bytes>>20)
	// Plain snapshots still have their limit, and are unaffected.
	if _, err := tined.CreateSnapshot(ctx, vm.ID); err != nil {
		t.Errorf("a plain snapshot after 40 retained ones: %v", err)
	}

	if _, err := tined.DestroyVM(ctx, vm.ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(gcWait) // the reaper has run at least twice
	for _, step := range []int{1, 7, 40} {
		_, children := forkSnapshot(t, snaps[step-1].ID, api.ForkRequest{Count: 1, TTLSeconds: 600, Network: "none"})
		if got := strings.TrimSpace(mustRun(t, children[0].ID, "cat", "/root/step")); got != strconv.Itoa(step) {
			t.Errorf("restored step %d: /root/step = %q", step, got)
		}
		if _, err := tined.DestroyVM(ctx, children[0].ID); err != nil {
			t.Error(err)
		}
	}
	// Deleting them all leaves nothing (the suite's leak check makes sure).
	for _, s := range snaps {
		if _, err := tined.DeleteSnapshot(ctx, s.ID); err != nil {
			t.Errorf("delete %s: %v", s.ID, err)
		}
	}
}

// describe is a VM's create timings, e.g. "restore 13ms · agent 30ms · sync 25ms".
func describe(timings map[string]float64) string {
	var parts []string
	for _, k := range []string{"network", "jail", "start", "restore", "agent", "sync", "total"} {
		if v, ok := timings[k]; ok {
			parts = append(parts, fmt.Sprintf("%s %.1fms", k, v))
		}
	}
	return strings.Join(parts, " · ")
}
