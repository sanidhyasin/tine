package vm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sanidhyasin/tine/internal/agentproto"
	"github.com/sanidhyasin/tine/internal/state"
)

func (f *fixture) running(t *testing.T) state.VM {
	t.Helper()
	vm, err := f.m.Create(context.Background(), CreateRequest{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	f.host.takeCalls()
	return vm
}

func TestSnapshotTakesEveryStepInOrder(t *testing.T) {
	f := newFixture(t, nil)
	vm := f.running(t)
	snap, err := f.m.Snapshot(context.Background(), vm.ID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if err := f.m.waitForFlush(context.Background(), vm.ID); err != nil {
		t.Fatal(err)
	}
	want := "describeHost,prepareSnapshot,pause,writeSnapshot,cloneDisk,resume,collectSnapshot,waitAgent,flushSnapshot"
	if got := strings.Join(f.host.takeCalls(), ","); got != want {
		t.Errorf("host calls:\n got %s\nwant %s", got, want)
	}
	if snap.State != state.SnapshotReady || snap.Kind != state.SnapshotKindVM || snap.VMID != vm.ID ||
		snap.VCPUs != vm.VCPUs || snap.MemoryMiB != vm.MemoryMiB || snap.SizeBytes <= 0 || !strings.HasPrefix(snap.ID, "snap-") {
		t.Errorf("snapshot = %+v", snap)
	}
	meta, err := f.m.snaps.Load(snap.ID)
	if err != nil {
		t.Fatalf("the snapshot's files: %v", err)
	}
	if meta.Host != fakeSnapshotHost || meta.VMID != vm.ID || meta.TimingsMs["paused"] <= 0 || meta.TimingsMs["total"] < meta.TimingsMs["paused"] {
		t.Errorf("metadata = %+v", meta)
	}
	if got, _ := f.store.GetVM(context.Background(), vm.ID); got.State != state.StateRunning {
		t.Errorf("after the snapshot the VM is %s, want running", got.State)
	}
	events, _ := f.store.Events(context.Background(), vm.ID)
	if last := events[len(events)-1]; !strings.Contains(last.Detail, snap.ID) || last.To != state.StateRunning {
		t.Errorf("last event = %+v, want a note about the snapshot", last)
	}
}

// A snapshot that fails anywhere after pausing still resumes the VM, and
// leaves no snapshot behind.
func TestFailedSnapshotsResumeAndCleanUp(t *testing.T) {
	for _, stage := range []string{"prepareSnapshot", "pause", "writeSnapshot", "cloneDisk", "collectSnapshot"} {
		t.Run(stage, func(t *testing.T) {
			f := newFixture(t, nil)
			vm := f.running(t)
			f.host.fail(stage, errors.New("boom"))
			if _, err := f.m.Snapshot(context.Background(), vm.ID); err == nil || !strings.Contains(err.Error(), "boom") {
				t.Fatalf("Snapshot = %v, want the failure", err)
			}
			calls := strings.Join(f.host.takeCalls(), ",")
			if stage != "prepareSnapshot" && !strings.Contains(calls, "resume") {
				t.Errorf("calls %s: the VM was never resumed", calls)
			}
			if f.host.anyPaused() {
				t.Error("the VM is still paused")
			}
			if got, _ := f.store.GetVM(context.Background(), vm.ID); got.State != state.StateRunning {
				t.Errorf("VM is %s, want still running", got.State)
			}
			if snaps, _ := f.store.ListSnapshots(context.Background(), state.SnapshotFilter{}); len(snaps) != 0 {
				t.Errorf("snapshot records left: %+v", snaps)
			}
			if ids, building, _ := f.m.snaps.List(); len(ids)+len(building) != 0 {
				t.Errorf("snapshot files left: %v %v", ids, building)
			}
		})
	}
}

// The next snapshot of a VM waits until the last one is on disk, before
// pausing the VM.
func TestSnapshotWaitsForTheLastOneToReachTheDisk(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, nil)
	vm := f.running(t)
	reached, release := f.host.holdAt("flushSnapshot")
	if _, err := f.m.Snapshot(ctx, vm.ID); err != nil {
		t.Fatal(err)
	}
	<-reached
	f.host.takeCalls()

	done := make(chan error, 1)
	go func() { _, err := f.m.Snapshot(ctx, vm.ID); done <- err }()
	time.Sleep(100 * time.Millisecond)
	for _, c := range f.host.takeCalls() {
		if c == "pause" {
			t.Fatal("the VM was paused before the last snapshot reached the disk")
		}
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotThatCantResumeFailsTheVM(t *testing.T) {
	f := newFixture(t, nil)
	vm := f.running(t)
	f.host.fail("resume", errors.New("stuck"))
	if _, err := f.m.Snapshot(context.Background(), vm.ID); err == nil {
		t.Fatal("Snapshot: want an error")
	}
	got, _ := f.store.GetVM(context.Background(), vm.ID)
	if got.State != state.StateFailed || !strings.Contains(got.Error, "resume") {
		t.Errorf("VM = %s (%q), want failed, saying it couldn't be resumed", got.State, got.Error)
	}
}

func TestSnapshotLimits(t *testing.T) {
	ctx := context.Background()
	t.Run("not running", func(t *testing.T) {
		f := newFixture(t, nil)
		vm := f.running(t)
		if _, err := f.m.Destroy(ctx, vm.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.m.Snapshot(ctx, vm.ID); !errors.Is(err, ErrNotRunning) {
			t.Errorf("Snapshot of a destroyed VM = %v, want ErrNotRunning", err)
		}
		if _, err := f.m.Snapshot(ctx, "vm-nope"); !errors.Is(err, ErrNotFound) {
			t.Errorf("Snapshot of no VM = %v, want ErrNotFound", err)
		}
	})
	t.Run("too many", func(t *testing.T) {
		f := newFixture(t, func(c *Config) { c.MaxSnapshotsPerVM = 2 })
		vm := f.running(t)
		for range 2 {
			if _, err := f.m.Snapshot(ctx, vm.ID); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := f.m.Snapshot(ctx, vm.ID); !errors.Is(err, ErrCapacity) {
			t.Errorf("third snapshot = %v, want ErrCapacity", err)
		}
	})
	t.Run("no space", func(t *testing.T) {
		f := newFixture(t, func(c *Config) { c.MinFreeMiB = 1 << 30 }) // a petabyte
		vm := f.running(t)
		if _, err := f.m.Snapshot(ctx, vm.ID); !errors.Is(err, ErrNoSpace) {
			t.Errorf("Snapshot = %v, want ErrNoSpace", err)
		}
		if calls := f.host.takeCalls(); len(calls) != 0 {
			t.Errorf("it touched the VM anyway: %v", calls)
		}
	})
}

// A snapshot is refused while a command runs, and a command that arrives
// during a snapshot waits for it.
func TestSnapshotsAndExecsDontOverlap(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, nil)
	f.host.agentClient = startAgent(t)
	vm := f.running(t)

	// A long command is running: the snapshot is refused.
	execDone := make(chan error, 1)
	go func() {
		_, err := f.m.Exec(ctx, vm.ID, agentproto.ExecRequest{Args: []string{"sleep", "0.5"}}, nil)
		execDone <- err
	}()
	eventually(t, "the exec to start", func() bool {
		f.m.gate.mu.Lock()
		defer f.m.gate.mu.Unlock()
		return f.m.gate.execs[vm.ID] == 1
	})
	if _, err := f.m.Snapshot(ctx, vm.ID); !errors.Is(err, ErrBusy) {
		t.Errorf("Snapshot during an exec = %v, want ErrBusy", err)
	}
	if err := <-execDone; err != nil {
		t.Fatalf("exec: %v", err)
	}

	// A snapshot is under way, the VM paused: a new command waits for it.
	reached, release := f.host.holdAt("writeSnapshot")
	snapDone := make(chan error, 1)
	go func() { _, err := f.m.Snapshot(ctx, vm.ID); snapDone <- err }()
	<-reached
	execStarted := make(chan time.Time, 1)
	go func() {
		_, _ = f.m.Exec(ctx, vm.ID, agentproto.ExecRequest{Args: []string{"true"}}, nil)
		execStarted <- time.Now()
	}()
	time.Sleep(100 * time.Millisecond)
	select {
	case <-execStarted:
		t.Fatal("an exec ran while the VM was being snapshotted")
	default:
	}
	releasedAt := time.Now()
	release()
	if err := <-snapDone; err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if ranAt := <-execStarted; ranAt.Before(releasedAt) {
		t.Error("the exec finished before the snapshot did")
	}
}

func TestReconcileCleansUpAfterAnInterruptedSnapshot(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, nil)
	vm := f.running(t)
	keep, err := f.m.Snapshot(ctx, vm.ID)
	if err != nil {
		t.Fatal(err)
	}

	// tined stopped mid-snapshot: the VM is paused, a record says
	// creating, and its files are half-gathered.
	if _, err := f.store.CreateSnapshot(ctx, state.Snapshot{ID: "snap-halfway", Kind: state.SnapshotKindVM, VMID: vm.ID, Image: "base", VCPUs: 2, MemoryMiB: 1024}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.snaps.Begin("snap-halfway"); err != nil {
		t.Fatal(err)
	}
	// A delete that was interrupted, and a directory nothing knows about.
	if _, err := f.m.Snapshot(ctx, vm.ID); err != nil {
		t.Fatal(err)
	}
	all, _ := f.store.ListSnapshots(ctx, state.SnapshotFilter{})
	dying := all[len(all)-1].ID
	if _, err := f.store.MarkSnapshotDeleting(ctx, dying); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.m.snaps.Dir(), "snap-stray"), 0o700); err != nil {
		t.Fatal(err)
	}
	f.host.mu.Lock()
	f.host.isPaused[vm.ID] = true
	f.host.mu.Unlock()

	r := f.m.Reconcile(ctx)
	if len(r.Errors) > 0 {
		t.Fatalf("Reconcile errors: %v", r.Errors)
	}
	if strings.Join(r.Resumed, ",") != vm.ID || strings.Join(r.Cleaned, ",") != "snap-halfway" ||
		strings.Join(r.Finished, ",") != dying || strings.Join(r.Orphans, ",") != "snapshot snap-stray" {
		t.Errorf("report = %s", r)
	}
	if f.host.anyPaused() {
		t.Error("the VM is still paused")
	}
	if ids, building, _ := f.m.snaps.List(); strings.Join(ids, ",") != keep.ID || len(building) != 0 {
		t.Errorf("store = %v, %v; want only %s", ids, building, keep.ID)
	}
	if snaps, _ := f.store.ListSnapshots(ctx, state.SnapshotFilter{}); len(snaps) != 1 || snaps[0].ID != keep.ID {
		t.Errorf("records = %+v, want only %s", snaps, keep.ID)
	}
	if _, err := f.m.snaps.Load(keep.ID); err != nil {
		t.Errorf("the good snapshot was harmed: %v", err)
	}
}

func (f *fakeHost) anyPaused() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.isPaused) > 0
}
