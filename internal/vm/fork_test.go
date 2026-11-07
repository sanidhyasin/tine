package vm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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
