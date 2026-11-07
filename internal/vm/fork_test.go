package vm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sanidhyasin/tine/internal/jailer"
	"github.com/sanidhyasin/tine/internal/state"
)

func TestFork(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, func(c *Config) { c.ForkParallelism = 2 })
	parent := f.running(t)
	f.host.loadDelay = 20 * time.Millisecond

	res, err := f.m.Fork(ctx, parent.ID, ForkRequest{Count: 5, TTL: 10 * time.Minute})
	if err != nil {
		t.Fatalf("Fork: %v", err)
	}
	if res.Snapshot.Kind != state.SnapshotKindFork || res.Snapshot.VMID != parent.ID || res.Failed() != 0 || len(res.Children) != 5 {
		t.Fatalf("result: snapshot %+v, %d children, %d failed", res.Snapshot, len(res.Children), res.Failed())
	}
	seen := map[string]bool{}
	for i, c := range res.Children {
		v := c.VM
		if v.State != state.StateRunning || v.ParentID != parent.ID || v.SnapshotID != res.Snapshot.ID ||
			v.Source != state.SourceSnapshot || v.VCPUs != parent.VCPUs || v.MemoryMiB != parent.MemoryMiB || seen[v.ID] {
			t.Errorf("child %d = %+v", i, v)
		}
		seen[v.ID] = true
	}
	for _, k := range []string{"snapshot", "paused", "children", "total"} {
		if res.Timings[k] <= 0 {
			t.Errorf("timings = %v, want %s", res.Timings, k)
		}
	}
	if f.host.maxLoading != 2 {
		t.Errorf("%d children were restored at once, want ForkParallelism = 2", f.host.maxLoading)
	}
	// The parent carries on, and the snapshot is no longer pinned.
	if got, _ := f.store.GetVM(ctx, parent.ID); got.State != state.StateRunning || f.host.anyPaused() {
		t.Errorf("parent is %s (paused: %v)", got.State, f.host.anyPaused())
	}
	if f.m.pinned(res.Snapshot.ID) {
		t.Error("the fork's snapshot is still pinned")
	}
	// A child can roll back to the point it was forked at.
	if _, err := f.m.Rollback(ctx, res.Children[0].VM.ID, res.Snapshot.ID); err != nil {
		t.Errorf("rolling a child back to its fork: %v", err)
	}
}

// The copy strategy gives each child its own copy of the memory file, where
// the default shares one.
func TestForkStrategies(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		strategy ForkStrategy
		want     jailer.FileMode
	}{{"", jailer.Link}, {ForkShare, jailer.Link}, {ForkCopy, jailer.Copy}} {
		f := newFixture(t, nil)
		parent := f.running(t)
		res, err := f.m.Fork(ctx, parent.ID, ForkRequest{Count: 2, Strategy: tc.strategy})
		if err != nil || res.Failed() != 0 {
			t.Fatalf("Fork(%q): %v", tc.strategy, err)
		}
		for _, c := range res.Children {
			if got := f.host.memoryModes[c.VM.ID]; got != tc.want {
				t.Errorf("strategy %q: child %s got its memory by %v, want %v", tc.strategy, c.VM.ID, got, tc.want)
			}
		}
		// A child restored again later, e.g. after a rollback, shares it.
		if _, err := f.m.Rollback(ctx, res.Children[0].VM.ID, res.Snapshot.ID); err != nil {
			t.Fatal(err)
		}
		if got := f.host.memoryModes[res.Children[0].VM.ID]; got != jailer.Link {
			t.Errorf("strategy %q: after a rollback the memory came by %v, want link", tc.strategy, got)
		}
	}
	f := newFixture(t, nil)
	parent := f.running(t)
	if _, err := f.m.Fork(ctx, parent.ID, ForkRequest{Count: 1, Strategy: "teleport"}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("an unknown strategy = %v, want ErrInvalidRequest", err)
	}
}

func TestForkChildrenFailOnTheirOwn(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, func(c *Config) { c.ForkParallelism = 1 })
	parent := f.running(t)
	f.host.failTimes = map[string]int{"loadSnapshot": 1}
	res, err := f.m.Fork(ctx, parent.ID, ForkRequest{Count: 3})
	if err != nil {
		t.Fatal(err)
	}
	if res.Failed() != 1 || res.Children[0].Err == nil || res.Children[1].Err != nil || res.Children[2].Err != nil {
		t.Fatalf("children: %+v", res.Children)
	}
	// The failed one cleaned up after itself, and doesn't hold the snapshot.
	if failed, _ := f.store.GetVM(ctx, res.Children[0].VM.ID); failed.State != state.StateDestroyed {
		t.Errorf("the failed child is %s, want destroyed", failed.State)
	}
	if users, _ := f.store.SnapshotUsers(ctx, res.Snapshot.ID); len(users) != 2 {
		t.Errorf("snapshot users = %v, want the 2 live children", users)
	}
}

func TestForkSnapshotGoesWithItsLastChild(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, nil)
	parent := f.running(t)
	res, err := f.m.Fork(ctx, parent.ID, ForkRequest{Count: 2})
	if err != nil {
		t.Fatal(err)
	}
	if n := f.m.collectGarbage(ctx); n != 0 {
		t.Errorf("collected %d while children run from it", n)
	}
	for _, c := range res.Children {
		if _, err := f.m.Destroy(ctx, c.VM.ID); err != nil {
			t.Fatal(err)
		}
	}
	// The parent still runs, but nothing needs its fork's snapshot now.
	if n := f.m.collectGarbage(ctx); n != 1 {
		t.Errorf("collected %d, want the fork's snapshot", n)
	}
	// A VM's fork snapshots don't count towards its snapshot limit.
	if snaps, _ := f.m.Snapshots(ctx, state.SnapshotFilter{VMID: parent.ID}); len(snaps) != 0 {
		t.Errorf("left: %+v", snaps)
	}
}

func TestPinnedSnapshotsStay(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, nil)
	parent := f.running(t)
	// A fork snapshot nobody runs from yet, as during a fork.
	unlock := f.m.locks.lock(parent.ID)
	snap, err := f.m.takeSnapshot(ctx, parent, state.SnapshotKindFork)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if !f.m.pinned(snap.ID) {
		t.Fatal("takeSnapshot didn't pin a fork snapshot")
	}
	if n := f.m.collectGarbage(ctx); n != 0 {
		t.Errorf("collected a pinned snapshot")
	}
	if _, err := f.m.DeleteSnapshot(ctx, snap.ID); !errors.Is(err, ErrBusy) {
		t.Errorf("deleting a pinned snapshot = %v, want ErrBusy", err)
	}
	f.m.unpin(snap.ID)
	if n := f.m.collectGarbage(ctx); n != 1 {
		t.Errorf("once unpinned, collected %d, want 1", n)
	}
}

func TestForkRefusals(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, func(c *Config) { c.MaxForkChildren = 10 })
	parent := f.running(t)
	for _, n := range []int{0, -1, 11} {
		if _, err := f.m.Fork(ctx, parent.ID, ForkRequest{Count: n}); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("Fork(%d) = %v, want ErrInvalidRequest", n, err)
		}
	}
	if _, err := f.m.Fork(ctx, parent.ID, ForkRequest{Count: 1, TTL: time.Second}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("a 1s TTL = %v, want ErrInvalidRequest", err)
	}
	if _, err := f.m.Fork(ctx, "vm-nope", ForkRequest{Count: 1}); !errors.Is(err, ErrNotFound) {
		t.Errorf("no parent = %v, want ErrNotFound", err)
	}
	f.host.fail("writeSnapshot", errors.New("disk full"))
	_, err := f.m.Fork(ctx, parent.ID, ForkRequest{Count: 2})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Errorf("Fork with a failing snapshot = %v", err)
	}
	f.host.fail("writeSnapshot", nil)
	if live, _ := f.store.ListVMs(ctx, state.ListOptions{}); len(live) != 1 {
		t.Errorf("live VMs = %d, want only the parent", len(live))
	}
	if len(f.m.pins) != 0 {
		t.Errorf("pins left: %v", f.m.pins)
	}
	if _, err := f.m.Destroy(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Fork(ctx, parent.ID, ForkRequest{Count: 1}); !errors.Is(err, ErrNotRunning) {
		t.Errorf("a destroyed parent = %v, want ErrNotRunning", err)
	}
}

func TestParseSmapsRollup(t *testing.T) {
	got := parseSmapsRollup(`55a5c0000000-7ffc00000000 ---p 00000000 00:00 0                          [rollup]
Rss:              118784 kB
Pss:               57344 kB
Pss_Dirty:          9216 kB
Shared_Clean:      98304 kB
Shared_Dirty:          0 kB
Private_Clean:      1024 kB
Private_Dirty:     19456 kB
Anonymous:         20480 kB
`)
	want := Memory{RSS: 118784 << 10, PSS: 57344 << 10, Private: (1024 + 19456) << 10, Shared: 98304 << 10}
	if got != want {
		t.Errorf("parseSmapsRollup = %+v, want %+v", got, want)
	}
}

func TestMemory(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, nil)
	vm := f.running(t)
	if mem, err := f.m.Memory(ctx, vm.ID); err != nil || mem.PSS != 60<<20 {
		t.Errorf("Memory = %+v, %v", mem, err)
	}
	if _, err := f.m.Destroy(ctx, vm.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Memory(ctx, vm.ID); !errors.Is(err, ErrNotRunning) {
		t.Errorf("Memory of a destroyed VM = %v, want ErrNotRunning", err)
	}
}

// Each child of a fork is made current and unique: its own hostname (its
// ID), machine ID and entropy, and the host's time.
func TestForkChildrenHaveTheirOwnIdentity(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, nil)
	parent := f.running(t)
	f.host.syncs = nil
	before := time.Now()
	res, err := f.m.Fork(ctx, parent.ID, ForkRequest{Count: 8})
	if err != nil || res.Failed() > 0 {
		t.Fatalf("Fork: %v", err)
	}
	if len(f.host.syncs) != 8 {
		t.Fatalf("%d syncs, want one per child", len(f.host.syncs))
	}
	children := map[string]bool{}
	for _, c := range res.Children {
		children[c.VM.ID] = true
	}
	hostnames, machineIDs, entropy := map[string]bool{}, map[string]bool{parent.ID: true}, map[string]bool{}
	for _, s := range f.host.syncs {
		if !children[s.Hostname] || hostnames[s.Hostname] || machineIDs[s.MachineID] || entropy[string(s.Entropy)] ||
			time.Unix(0, s.TimeUnixNano).Before(before) {
			t.Errorf("sync %+v: not a child's own", s)
		}
		hostnames[s.Hostname], machineIDs[s.MachineID], entropy[string(s.Entropy)] = true, true, true
	}
}
