package vm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sanidhyasin/tine/internal/agentproto"
	"github.com/sanidhyasin/tine/internal/snapshot"
	"github.com/sanidhyasin/tine/internal/state"
)

func TestRollback(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, nil)
	vm, snap := f.snapshotted(t, CreateRequest{})
	before := f.host.lastSync

	got, err := f.m.Rollback(ctx, vm.ID, snap.ID)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if calls, want := strings.Join(f.host.takeCalls(), ","), "kill,cleanupJail,prepareJail,startFirecracker,loadSnapshot,waitAgent,syncGuest"; calls != want {
		t.Errorf("host calls:\n got %s\nwant %s", calls, want)
	}
	// The same VM: ID, network slot, uid and identity. Only its state changed.
	if got.ID != vm.ID || got.Slot != vm.Slot || got.UID != vm.UID || got.State != state.StateRunning ||
		got.SnapshotID != snap.ID || got.PID == vm.PID || got.Timings["rollback"] <= 0 {
		t.Errorf("after the rollback: %+v (before %+v)", got, vm)
	}
	if after := f.host.lastSync; after.Hostname != vm.ID || after.MachineID != before.MachineID {
		t.Errorf("identity changed: hostname %q, machine ID %q -> %q", after.Hostname, before.MachineID, after.MachineID)
	}
	if _, err := f.store.MarkSnapshotDeleting(ctx, snap.ID); !errors.Is(err, state.ErrInUse) {
		t.Errorf("the snapshot it runs from could be deleted: %v", err)
	}
	// Again, and destroying it afterwards works as usual.
	if _, err := f.m.Rollback(ctx, vm.ID, snap.ID); err != nil {
		t.Fatalf("second Rollback: %v", err)
	}
	if _, err := f.m.Destroy(ctx, vm.ID); err != nil {
		t.Fatal(err)
	}
}

func TestRollbackToTheTemplateItStartedFrom(t *testing.T) {
	ctx := context.Background()
	f := warmFixture(t, nil)
	tmpl, err := f.m.WarmTemplate(ctx, "base", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	vm, err := f.m.Create(ctx, CreateRequest{})
	if err != nil || vm.SnapshotID != tmpl.ID {
		t.Fatalf("Create = %+v, %v", vm, err)
	}
	if _, err := f.m.Rollback(ctx, vm.ID, tmpl.ID); err != nil {
		t.Errorf("rolling back to its template: %v", err)
	}
}

func TestRollbackRefusals(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, nil)
	f.host.agentClient = startAgent(t)
	vm, snap := f.snapshotted(t, CreateRequest{})
	other, otherSnap := f.snapshotted(t, CreateRequest{})

	if _, err := f.m.Rollback(ctx, vm.ID, otherSnap.ID); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("another VM's snapshot = %v, want ErrInvalidRequest", err)
	}
	if _, err := f.m.Rollback(ctx, vm.ID, "snap-nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("no such snapshot = %v, want ErrNotFound", err)
	}
	if _, err := f.m.Destroy(ctx, other.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Rollback(ctx, other.ID, otherSnap.ID); !errors.Is(err, ErrNotRunning) {
		t.Errorf("a destroyed VM = %v, want ErrNotRunning", err)
	}
	incompatible := fakeSnapshotHost
	incompatible.Kernel = "7.0.0"
	f.host.thisHost = &incompatible
	if _, err := f.m.Rollback(ctx, vm.ID, snap.ID); !errors.Is(err, snapshot.ErrIncompatible) {
		t.Errorf("incompatible = %v, want ErrIncompatible", err)
	}
	f.host.thisHost = nil

	// Busy while a command runs.
	done := make(chan struct{})
	go func() {
		_, _ = f.m.Exec(ctx, vm.ID, agentproto.ExecRequest{Args: []string{"sleep", "0.3"}}, nil)
		close(done)
	}()
	eventually(t, "the exec to start", func() bool {
		f.m.gate.mu.Lock()
		defer f.m.gate.mu.Unlock()
		return f.m.gate.execs[vm.ID] == 1
	})
	if _, err := f.m.Rollback(ctx, vm.ID, snap.ID); !errors.Is(err, ErrBusy) {
		t.Errorf("during an exec = %v, want ErrBusy", err)
	}
	<-done
	f.host.takeCalls()
	if got, _ := f.store.GetVM(ctx, vm.ID); got.State != state.StateRunning || got.SnapshotID != "" {
		t.Errorf("a refused rollback changed the VM: %+v", got)
	}
}

func TestFailedRollbackFailsTheVM(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, nil)
	vm, snap := f.snapshotted(t, CreateRequest{})
	f.host.fail("loadSnapshot", errors.New("corrupt"))
	got, err := f.m.Rollback(ctx, vm.ID, snap.ID)
	if err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("Rollback = %v", err)
	}
	if got.State != state.StateFailed || !strings.Contains(got.Error, "rollback to "+snap.ID) {
		t.Errorf("VM = %s %q, want failed, saying the rollback did it", got.State, got.Error)
	}
	f.host.fail("loadSnapshot", nil)
	if _, err := f.m.Destroy(ctx, vm.ID); err != nil {
		t.Errorf("destroying the failed VM: %v", err)
	}
}

// A command that arrives during a rollback waits, then runs in the
// rolled-back VM.
func TestExecWaitsForARollback(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, nil)
	f.host.agentClient = startAgent(t)
	vm, snap := f.snapshotted(t, CreateRequest{})
	reached, release := f.host.holdAt("loadSnapshot")
	rolled := make(chan error, 1)
	go func() { _, err := f.m.Rollback(ctx, vm.ID, snap.ID); rolled <- err }()
	<-reached
	ran := make(chan time.Time, 1)
	go func() {
		_, _ = f.m.Exec(ctx, vm.ID, agentproto.ExecRequest{Args: []string{"true"}}, nil)
		ran <- time.Now()
	}()
	time.Sleep(100 * time.Millisecond)
	releasedAt := time.Now()
	release()
	if err := <-rolled; err != nil {
		t.Fatal(err)
	}
	if at := <-ran; at.Before(releasedAt) {
		t.Error("the exec ran in the middle of the rollback")
	}
}
