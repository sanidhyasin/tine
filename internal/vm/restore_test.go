package vm

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sanidhyasin/tine/internal/snapshot"
	"github.com/sanidhyasin/tine/internal/state"
)

// snapshotted returns a running VM and a snapshot of it.
func (f *fixture) snapshotted(t *testing.T, req CreateRequest) (state.VM, state.Snapshot) {
	t.Helper()
	ctx := context.Background()
	vm, err := f.m.Create(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := f.m.Snapshot(ctx, vm.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.waitForFlush(ctx, vm.ID); err != nil {
		t.Fatal(err)
	}
	f.host.takeCalls()
	return vm, snap
}

func TestCreateFromASnapshot(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, nil)
	parent, snap := f.snapshotted(t, CreateRequest{VCPUs: 4, MemoryMiB: 2048})

	before := time.Now()
	vm, err := f.m.Create(ctx, CreateRequest{SnapshotID: snap.ID})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got, want := strings.Join(f.host.takeCalls(), ","), "createNetwork,prepareJail,startFirecracker,loadSnapshot,waitAgent,syncGuest"; got != want {
		t.Errorf("host calls:\n got %s\nwant %s", got, want)
	}
	if vm.SnapshotID != snap.ID || vm.VCPUs != 4 || vm.MemoryMiB != 2048 || vm.ID == parent.ID || vm.Slot == parent.Slot {
		t.Errorf("VM = %+v; want the snapshot's size, its own ID and slot", vm)
	}
	if _, ok := vm.Timings["restore"]; !ok || vm.Timings["configure"] != 0 {
		t.Errorf("timings = %v, want a restore and no configure", vm.Timings)
	}

	// The guest was made current and unique.
	sync := f.host.lastSync
	if sync.Hostname != vm.ID || len(sync.Entropy) != 64 || !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(sync.MachineID) ||
		time.Unix(0, sync.TimeUnixNano).Before(before) {
		t.Errorf("sync = %+v", sync)
	}

	// It holds the snapshot: it can't be deleted until the VM is gone.
	if _, err := f.store.MarkSnapshotDeleting(ctx, snap.ID); !errors.Is(err, state.ErrInUse) {
		t.Errorf("deleting a snapshot a VM runs from = %v, want ErrInUse", err)
	}
	if _, err := f.m.Destroy(ctx, vm.ID); err != nil {
		t.Fatal(err)
	}
	if users, _ := f.store.SnapshotUsers(ctx, snap.ID); len(users) != 0 {
		t.Errorf("after destroying its VM, the snapshot's users = %v", users)
	}
}

func TestCreateFromASnapshotChecksFirst(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, nil)
	_, snap := f.snapshotted(t, CreateRequest{})

	if _, err := f.m.Create(ctx, CreateRequest{SnapshotID: snap.ID, MemoryMiB: 512}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("a different size = %v, want ErrInvalidRequest", err)
	}
	if _, err := f.m.Create(ctx, CreateRequest{SnapshotID: "snap-nope"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("no such snapshot = %v, want ErrNotFound", err)
	}
	other := fakeSnapshotHost
	other.Firecracker = "1.18.0"
	f.host.thisHost = &other
	if _, err := f.m.Create(ctx, CreateRequest{SnapshotID: snap.ID}); !errors.Is(err, snapshot.ErrIncompatible) {
		t.Errorf("another Firecracker = %v, want ErrIncompatible", err)
	}
	if calls := f.host.takeCalls(); len(calls) != 0 {
		t.Errorf("refused creates touched the host: %v", calls)
	}
	if vms, _ := f.store.ListVMs(ctx, state.ListOptions{}); len(vms) != 1 {
		t.Errorf("live VMs = %d, want only the parent", len(vms))
	}
}

func TestFailedRestoreUndoesAndReleasesTheSnapshot(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, nil)
	_, snap := f.snapshotted(t, CreateRequest{})
	f.host.fail("loadSnapshot", errors.New("boom"))
	vm, err := f.m.Create(ctx, CreateRequest{SnapshotID: snap.ID})
	if err == nil || !strings.Contains(err.Error(), "restore: boom") {
		t.Fatalf("Create = %v", err)
	}
	if got, want := strings.Join(f.host.takeCalls(), ","), "createNetwork,prepareJail,startFirecracker,loadSnapshot,kill,cleanupJail,destroyNetwork"; got != want {
		t.Errorf("host calls:\n got %s\nwant %s", got, want)
	}
	if vm.State != state.StateDestroyed {
		t.Errorf("VM is %s, want destroyed", vm.State)
	}
	if users, _ := f.store.SnapshotUsers(ctx, snap.ID); len(users) != 0 {
		t.Errorf("the snapshot is still held by %v", users)
	}
}
