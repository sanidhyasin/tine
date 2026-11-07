package vm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sanidhyasin/tine/internal/state"
)

// restart makes a new manager over the same store, as tined does when it
// starts again, with a fresh fake host.
func restart(t *testing.T, f *fixture) (*Manager, *fakeHost) {
	t.Helper()
	h := newFakeHost()
	h.dead = map[string]bool{}
	m, err := newManager(f.m.cfg, f.store, h)
	if err != nil {
		t.Fatal(err)
	}
	return m, h
}

func record(t *testing.T, f *fixture, id string, slot int, states ...state.VMState) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.store.CreateVM(ctx, newVMRecord(id, slot)); err != nil {
		t.Fatal(err)
	}
	for _, s := range states {
		if _, err := f.store.Transition(ctx, id, s, "test"); err != nil {
			t.Fatal(err)
		}
	}
}

func newVMRecord(id string, slot int) state.VM {
	return state.VM{ID: id, Image: "base", VCPUs: 2, MemoryMiB: 1024, Slot: slot, UID: 200_000 + slot, ExpiresAt: time.Now().Add(time.Hour)}
}

func TestReconcile(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	record(t, f, "vm-creating0", 0)                                            // tined stopped mid-create
	record(t, f, "vm-destroyng", 1, state.StateRunning, state.StateDestroying) // tined stopped mid-destroy
	record(t, f, "vm-alive0000", 2, state.StateRunning)                        // still running
	record(t, f, "vm-dead00000", 3, state.StateRunning)                        // Firecracker died while tined was down
	record(t, f, "vm-failed000", 4, state.StateRunning, state.StateFailed)     // failed earlier

	m, h := restart(t, f)
	h.dead["vm-dead00000"] = true
	r := m.Reconcile(ctx)

	if strings.Join(r.Kept, ",") != "vm-alive0000,vm-failed000" || strings.Join(r.Cleaned, ",") != "vm-creating0,vm-dead00000" ||
		strings.Join(r.Finished, ",") != "vm-destroyng" || len(r.Errors) != 0 {
		t.Errorf("report = %s", r)
	}
	want := map[string]struct {
		state state.VMState
		err   string
	}{
		"vm-creating0": {state.StateDestroyed, "tined stopped while the VM was being created"},
		"vm-destroyng": {state.StateDestroyed, ""},
		"vm-alive0000": {state.StateRunning, ""},
		"vm-dead00000": {state.StateDestroyed, "wasn't running when tined started"},
		"vm-failed000": {state.StateFailed, ""},
	}
	for id, w := range want {
		vm, _ := f.store.GetVM(ctx, id)
		if vm.State != w.state || !strings.Contains(vm.Error, w.err) {
			t.Errorf("%s: %s with error %q; want %s with %q", id, vm.State, vm.Error, w.state, w.err)
		}
	}
	// The three it tore down each had Firecracker stopped (by cgroup), jail and network removed.
	calls := strings.Join(h.takeCalls(), ",")
	if strings.Count(calls, "killFirecracker,cleanupJail,destroyNetwork") != 3 {
		t.Errorf("host calls = %s; want three full teardowns", calls)
	}
	// Their slots are free again; the kept VMs' slots aren't.
	if m.slots.InUse() != 2 {
		t.Errorf("%d slots in use after reconcile, want 2", m.slots.InUse())
	}
}

func TestReconcileRemovesOrphans(t *testing.T) {
	f := newFixture(t, nil)
	record(t, f, "vm-alive0000", 7, state.StateRunning)
	m, h := restart(t, f)
	h.inv = inventory{
		ids:   []string{"vm-alive0000", "vm-orphan000"},
		veths: []string{"tine7", "tine99", "tineweird"},
	}
	r := m.Reconcile(context.Background())

	calls := strings.Join(h.takeCalls(), ",")
	// The live VM's jail only has its snapshot directory cleared.
	if calls != "prepareSnapshot,inventory,removeOrphan:vm-orphan000,removeVeth:tine99,removeVeth:tineweird" {
		t.Errorf("host calls = %s; want only the orphans removed", calls)
	}
	if strings.Join(r.Orphans, ",") != "vm-orphan000,veth tine99,veth tineweird" {
		t.Errorf("report = %s", r)
	}
}

func TestReconcileCarriesOnPastErrors(t *testing.T) {
	f := newFixture(t, nil)
	m, h := restart(t, f)
	h.inv = inventory{ids: []string{"vm-orphan001", "vm-orphan002"}}
	h.fail("removeOrphan:vm-orphan001", errors.New("busy"))
	r := m.Reconcile(context.Background())
	if len(r.Errors) != 1 || !strings.Contains(r.Errors[0], "busy") || strings.Join(r.Orphans, ",") != "vm-orphan002" {
		t.Errorf("report = %s; want one error and the other orphan still removed", r)
	}
}

func TestReaper(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	old, _ := f.m.Create(ctx, CreateRequest{})
	fresh, _ := f.m.Create(ctx, CreateRequest{})
	if _, err := f.store.UpdateVM(ctx, old.ID, "", func(v *state.VM) error { v.ExpiresAt = time.Now().Add(-time.Second); return nil }); err != nil {
		t.Fatal(err)
	}

	if n := f.m.reapOnce(ctx, time.Now()); n != 1 {
		t.Errorf("reapOnce destroyed %d VMs, want 1", n)
	}
	if v, _ := f.m.Get(ctx, old.ID); v.State != state.StateDestroyed {
		t.Errorf("expired VM is %s, want destroyed", v.State)
	}
	if v, _ := f.m.Get(ctx, fresh.ID); v.State != state.StateRunning {
		t.Errorf("unexpired VM is %s, want running", v.State)
	}
	events, _ := f.store.Events(ctx, old.ID)
	found := false
	for _, e := range events {
		if e.To == state.StateDestroying && strings.Contains(e.Detail, "expired") {
			found = true
		}
	}
	if !found {
		t.Errorf("no event records the expiry: %+v", events)
	}

	// Records of long-destroyed VMs are purged; live ones stay.
	f.m.purgeOnce(ctx, time.Now().Add(8*24*time.Hour))
	if _, err := f.m.Get(ctx, old.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("long-destroyed VM's record = %v, want purged", err)
	}
	if _, err := f.m.Get(ctx, fresh.ID); err != nil {
		t.Errorf("live VM's record was purged: %v", err)
	}
}

func TestRunReaperStopsWithItsContext(t *testing.T) {
	f := newFixture(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	vm, _ := f.m.Create(ctx, CreateRequest{})
	_, _ = f.store.UpdateVM(ctx, vm.ID, "", func(v *state.VM) error { v.ExpiresAt = time.Now(); return nil })

	done := make(chan struct{})
	go func() { f.m.RunReaper(ctx, 10*time.Millisecond); close(done) }()
	eventually(t, "the reaper to destroy the expired VM", func() bool {
		v, _ := f.m.Get(context.Background(), vm.ID)
		return v.State == state.StateDestroyed
	})
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunReaper didn't return after its context ended")
	}
}

func TestTeardownLooksInTheCgroupEvenWithoutAPID(t *testing.T) {
	// A create that stopped before saving the PID still has its Firecracker
	// stopped: teardown always looks in the VM's cgroup.
	f := newFixture(t, nil)
	record(t, f, "vm-nopid0000", 0)
	m, h := restart(t, f)
	m.Reconcile(context.Background())
	if calls := strings.Join(h.takeCalls(), ","); !strings.HasPrefix(calls, "killFirecracker,") {
		t.Errorf("host calls = %s; want killFirecracker first", calls)
	}
}
