package vm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sanidhyasin/tine/internal/snapshot"
	"github.com/sanidhyasin/tine/internal/state"
)

// Checkpoints: a VM snapshotted before every step, retained, and any step
// forked later, offline if asked, even once the VM is gone.

func TestRetainedSnapshotOutlivesItsVM(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, nil)
	agent := f.running(t)
	kept, err := f.m.Snapshot(ctx, agent.ID, SnapshotRequest{Retain: true})
	if err != nil || !kept.Retained || kept.Kind != state.SnapshotKindVM {
		t.Fatalf("a retained snapshot = %+v, %v", kept, err)
	}
	plain, err := f.m.Snapshot(ctx, agent.ID, SnapshotRequest{})
	if err != nil || plain.Retained {
		t.Fatalf("a plain snapshot = %+v, %v", plain, err)
	}
	if _, err := f.m.Destroy(ctx, agent.ID); err != nil {
		t.Fatal(err)
	}
	if n := f.m.collectGarbage(ctx); n != 1 {
		t.Errorf("collected %d once the VM was destroyed, want only the snapshot that isn't retained", n)
	}
	if _, err := f.m.GetSnapshot(ctx, plain.ID); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("the plain snapshot after GC: %v, want it gone", err)
	}

	// Restore it, its VM long gone: the children are the VM as it was.
	res, err := f.m.ForkSnapshot(ctx, kept.ID, ForkRequest{Count: 2})
	if err != nil || res.Failed() != 0 {
		t.Fatalf("ForkSnapshot: %v, %+v", err, res.Children)
	}
	for i, c := range res.Children {
		if v := c.VM; v.State != state.StateRunning || v.SnapshotID != kept.ID || v.ParentID != agent.ID ||
			v.Source != state.SourceSnapshot || v.Network != state.NetworkDefault {
			t.Errorf("child %d = %+v", i, v)
		}
	}
	if _, err := f.m.DeleteSnapshot(ctx, kept.ID); !errors.Is(err, state.ErrInUse) {
		t.Errorf("deleting it while VMs run from it = %v, want ErrInUse", err)
	}
	for _, c := range res.Children {
		if _, err := f.m.Destroy(ctx, c.VM.ID); err != nil {
			t.Fatal(err)
		}
	}
	if n := f.m.collectGarbage(ctx); n != 0 {
		t.Errorf("collected %d once its children were gone; a retained snapshot stays", n)
	}

	// And across a restart.
	m, _ := restart(t, f)
	if r := m.Reconcile(ctx); len(r.Errors) != 0 {
		t.Fatalf("reconcile: %s", r)
	}
	if n := m.collectGarbage(ctx); n != 0 {
		t.Errorf("collected %d after a restart", n)
	}
	if got, err := m.GetSnapshot(ctx, kept.ID); err != nil || !got.Retained || got.State != state.SnapshotReady {
		t.Errorf("after a restart: %+v, %v", got, err)
	}
	if _, err := m.DeleteSnapshot(ctx, kept.ID); err != nil {
		t.Errorf("deleting it on request: %v", err)
	}
	if _, err := os.Stat(m.snaps.Path(kept.ID, "")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("its files are still there: %v", err)
	}
}

// Retained snapshots don't count against MaxSnapshotsPerVM, so an agent can
// take one before each of many steps; they have a limit of their own.
func TestRetainedSnapshotsHaveTheirOwnLimit(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, func(c *Config) { c.MaxSnapshotsPerVM = 2; c.MaxRetainedSnapshotsPerVM = 40 })
	vm := f.running(t)
	for i := range 2 {
		if _, err := f.m.Snapshot(ctx, vm.ID, SnapshotRequest{}); err != nil {
			t.Fatalf("plain snapshot %d: %v", i+1, err)
		}
	}
	var last state.Snapshot
	for i := range 40 {
		var err error
		if last, err = f.m.Snapshot(ctx, vm.ID, SnapshotRequest{Retain: true}); err != nil {
			t.Fatalf("retained snapshot %d of 40: %v", i+1, err)
		}
	}
	if _, err := f.m.Snapshot(ctx, vm.ID, SnapshotRequest{Retain: true}); !errors.Is(err, ErrCapacity) {
		t.Errorf("retained snapshot 41 = %v, want ErrCapacity", err)
	}
	if _, err := f.m.Snapshot(ctx, vm.ID, SnapshotRequest{}); !errors.Is(err, ErrCapacity) {
		t.Errorf("plain snapshot 3 = %v, want ErrCapacity", err)
	}
	// Deleting one makes room for another.
	if _, err := f.m.DeleteSnapshot(ctx, last.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Snapshot(ctx, vm.ID, SnapshotRequest{Retain: true}); err != nil {
		t.Errorf("a retained snapshot after deleting one: %v", err)
	}
	if DefaultConfig().MaxRetainedSnapshotsPerVM < 100 {
		t.Errorf("the default limit of retained snapshots is %d: too few for a long agent run", DefaultConfig().MaxRetainedSnapshotsPerVM)
	}
}

func TestOfflineVMs(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, nil)
	offline, err := f.m.Create(ctx, CreateRequest{Network: state.NetworkNone})
	if err != nil {
		t.Fatal(err)
	}
	online := f.running(t)
	if offline.Network != state.NetworkNone || online.Network != state.NetworkDefault {
		t.Errorf("networks: offline %q, online %q", offline.Network, online.Network)
	}
	if !f.host.networks[offline.ID].Offline || f.host.networks[online.ID].Offline {
		t.Errorf("the host was asked for %+v and %+v", f.host.networks[offline.ID], f.host.networks[online.ID])
	}
	if _, err := f.m.Create(ctx, CreateRequest{Network: "bridged"}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("an unknown network = %v, want ErrInvalidRequest", err)
	}

	// A fork's children have the network asked for, not the parent's.
	for _, tc := range []struct {
		parent state.VM
		ask    state.Network
		want   state.Network
	}{
		{online, state.NetworkNone, state.NetworkNone},
		{offline, "", state.NetworkDefault},
		{offline, state.NetworkNone, state.NetworkNone},
	} {
		res, err := f.m.Fork(ctx, tc.parent.ID, ForkRequest{Count: 2, Network: tc.ask})
		if err != nil || res.Failed() != 0 {
			t.Fatalf("Fork(%q): %v", tc.ask, err)
		}
		for _, c := range res.Children {
			if c.VM.Network != tc.want || f.host.networks[c.VM.ID].Offline != (tc.want == state.NetworkNone) {
				t.Errorf("a child of a %s VM, asked for %q, has %q (host network %+v)", tc.parent.Network, tc.ask, c.VM.Network, f.host.networks[c.VM.ID])
			}
		}
	}
	if _, err := f.m.Fork(ctx, online.ID, ForkRequest{Count: 1, Network: "bridged"}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("a fork with an unknown network = %v, want ErrInvalidRequest", err)
	}

	// A rollback keeps the VM offline, and the network is the same one.
	snap, err := f.m.Snapshot(ctx, offline.ID, SnapshotRequest{})
	if err != nil {
		t.Fatal(err)
	}
	rolled, err := f.m.Rollback(ctx, offline.ID, snap.ID)
	if err != nil || rolled.Network != state.NetworkNone {
		t.Errorf("Rollback = %+v, %v", rolled, err)
	}
	// Commands still reach it: they go through vsock, not the network.
	if _, j, err := f.m.resources(rolled); err != nil || j == nil {
		t.Errorf("resources of an offline VM: %v", err)
	}
	if n, _, _ := f.m.resources(rolled); !n.Offline {
		t.Error("an offline VM's network, planned again from its record, isn't offline")
	}
}

func TestForkSnapshot(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, func(c *Config) { c.ForkParallelism = 3 })
	agent := f.running(t)
	step, err := f.m.Snapshot(ctx, agent.ID, SnapshotRequest{Retain: true})
	if err != nil {
		t.Fatal(err)
	}
	f.host.loadDelay = 50 * time.Millisecond
	res, err := f.m.ForkSnapshot(ctx, step.ID, ForkRequest{Count: 6, TTL: 10 * time.Minute, Network: state.NetworkNone})
	if err != nil || res.Failed() != 0 || len(res.Children) != 6 {
		t.Fatalf("ForkSnapshot: %v, %+v", err, res)
	}
	if res.Snapshot.ID != step.ID {
		t.Errorf("the result's snapshot is %s, want %s", res.Snapshot.ID, step.ID)
	}
	for i, c := range res.Children {
		v := c.VM
		if v.State != state.StateRunning || v.ParentID != agent.ID || v.SnapshotID != step.ID || v.Network != state.NetworkNone ||
			v.VCPUs != agent.VCPUs || v.MemoryMiB != agent.MemoryMiB || time.Until(v.ExpiresAt) > 10*time.Minute {
			t.Errorf("child %d = %+v", i, v)
		}
	}
	for _, k := range []string{"check", "children", "total"} {
		if res.Timings[k] <= 0 {
			t.Errorf("timings = %v, want %s", res.Timings, k)
		}
	}
	if _, ok := res.Timings["paused"]; ok {
		t.Errorf("timings = %v: nothing was paused", res.Timings)
	}
	if f.host.maxLoading != 3 {
		t.Errorf("%d children were restored at once, want ForkParallelism = 3", f.host.maxLoading)
	}
	if f.m.pinned(step.ID) {
		t.Error("the snapshot is still pinned")
	}
	// Forking a snapshot doesn't touch the VM it was taken of.
	if got, _ := f.store.GetVM(ctx, agent.ID); got.State != state.StateRunning || f.host.anyPaused() {
		t.Errorf("the snapshot's VM is %s (paused: %v)", got.State, f.host.anyPaused())
	}

	// While the children are being made, the snapshot can't be deleted.
	reached, release := f.host.holdAt("loadSnapshot")
	done := make(chan error, 1)
	go func() {
		_, err := f.m.ForkSnapshot(ctx, step.ID, ForkRequest{Count: 1})
		done <- err
	}()
	<-reached
	if _, err := f.m.DeleteSnapshot(ctx, step.ID); !errors.Is(err, ErrBusy) {
		t.Errorf("deleting a snapshot being forked = %v, want ErrBusy", err)
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	// Children of a fork's snapshot, and of a template (which has no parent).
	forked, err := f.m.Fork(ctx, agent.ID, ForkRequest{Count: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := f.m.ForkSnapshot(ctx, forked.Snapshot.ID, ForkRequest{Count: 1}); err != nil || res.Children[0].VM.ParentID != agent.ID {
		t.Errorf("a fork of a fork's snapshot: %+v, %v", res, err)
	}
	f.host.agentClient = startAgent(t) // warming runs the settle command
	tmpl, err := f.m.WarmTemplate(ctx, "base", 2, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := f.m.ForkSnapshot(ctx, tmpl.ID, ForkRequest{Count: 1}); err != nil || res.Children[0].VM.ParentID != "" {
		t.Errorf("a fork of a template: %+v, %v", res, err)
	}
}

func TestForkSnapshotRefusals(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, func(c *Config) { c.MaxForkChildren = 10 })
	vm := f.running(t)
	snap, err := f.m.Snapshot(ctx, vm.ID, SnapshotRequest{Retain: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, req := range []ForkRequest{
		{Count: 0}, {Count: 11}, {Count: 1, Strategy: "teleport"}, {Count: 1, Network: "bridged"}, {Count: 1, TTL: time.Second},
	} {
		if _, err := f.m.ForkSnapshot(ctx, snap.ID, req); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("ForkSnapshot(%+v) = %v, want ErrInvalidRequest", req, err)
		}
	}
	if _, err := f.m.ForkSnapshot(ctx, "snap-nopenopeno", ForkRequest{Count: 1}); !errors.Is(err, ErrNotFound) {
		t.Errorf("a missing snapshot = %v, want ErrNotFound", err)
	}
	other := fakeSnapshotHost
	other.Firecracker = "1.18.0"
	f.host.thisHost = &other
	if _, err := f.m.ForkSnapshot(ctx, snap.ID, ForkRequest{Count: 1}); !errors.Is(err, snapshot.ErrIncompatible) {
		t.Errorf("a snapshot this host can't restore = %v, want ErrIncompatible", err)
	}
	f.host.thisHost = nil
	f.host.mem = hostMemory{TotalMiB: 2048, AvailableMiB: 1200}
	if _, err := f.m.ForkSnapshot(ctx, snap.ID, ForkRequest{Count: 10}); !errors.Is(err, ErrNoMemory) {
		t.Errorf("more children than the host has room for = %v, want ErrNoMemory", err)
	}
	f.host.mem = hostMemory{}
	if err := os.RemoveAll(filepath.Join(f.m.cfg.ImagesDir, "base")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.ForkSnapshot(ctx, snap.ID, ForkRequest{Count: 1}); !errors.Is(err, ErrImageNotFound) {
		t.Errorf("a snapshot whose image is gone = %v, want ErrImageNotFound", err)
	}
	if live, _ := f.store.ListVMs(ctx, state.ListOptions{}); len(live) != 1 {
		t.Errorf("live VMs = %d, want only the first", len(live))
	}
	if len(f.m.pins) != 0 {
		t.Errorf("pins left: %v", f.m.pins)
	}
}
