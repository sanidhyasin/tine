package vm

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/sanidhyasin/tine/internal/agentproto"
	"github.com/sanidhyasin/tine/internal/jailer"
	"github.com/sanidhyasin/tine/internal/logging"
	"github.com/sanidhyasin/tine/internal/state"
)

// syncTimeout bounds syncing a guest after it starts.
const syncTimeout = 10 * time.Second

// checkRestorable fails unless the snapshot is ready, its files are all
// there, and this host can restore it (ErrIncompatible from the snapshot
// package otherwise).
func (m *Manager) checkRestorable(ctx context.Context, snapID string) error {
	rec, err := m.store.GetSnapshot(ctx, snapID)
	if err != nil {
		return err
	}
	if rec.State != state.SnapshotReady {
		return fmt.Errorf("snapshot %s is %s, not ready: %w", snapID, rec.State, ErrNotFound)
	}
	meta, err := m.snaps.Load(snapID)
	if err != nil {
		return fmt.Errorf("snapshot %s: %w", snapID, err)
	}
	host, err := m.host.hostInfo(ctx)
	if err != nil {
		return fmt.Errorf("check snapshot %s: %w", snapID, err)
	}
	return meta.CheckCompatible(host)
}

// sync makes a guest current and unique through its agent: the host's time,
// fresh entropy, and the VM's identity: its ID as its hostname, and a
// machine ID made from its ID. A restored guest needs all of these; a booted
// one gets them too, so every VM is the same in these ways however it
// started, and keeps its identity when rolled back.
func (m *Manager) sync(ctx context.Context, vm state.VM, j *jailer.Jail) error {
	entropy := make([]byte, 64)
	if _, err := rand.Read(entropy); err != nil {
		return err
	}
	machineID := sha256.Sum256([]byte("tine machine-id " + vm.ID))
	ctx, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()
	res, err := m.host.syncGuest(ctx, j, agentproto.SyncRequest{
		TimeUnixNano: time.Now().UnixNano(),
		Entropy:      entropy,
		Hostname:     vm.ID,
		MachineID:    hex.EncodeToString(machineID[:16]),
	})
	if err != nil {
		return err
	}
	m.log.InfoContext(ctx, "synced the guest", "vm", vm.ID, "clock_offset_ms", res.ClockOffsetMs)
	_ = m.store.Note(ctx, vm.ID, "guest synced: its clock was "+describeOffset(res.ClockOffsetMs))
	return nil
}

// describeOffset is e.g. "3.2s behind", for how far a guest's clock was off.
func describeOffset(ms float64) string {
	d := time.Duration(ms * float64(time.Millisecond)).Round(time.Millisecond)
	switch {
	case d < 0:
		return (-d).String() + " behind"
	case d > 0:
		return d.String() + " ahead"
	}
	return "right"
}

// rollbackTimeout bounds a whole rollback. Once the old Firecracker is gone,
// the rollback must finish, whether or not the caller is still waiting.
const rollbackTimeout = 2 * time.Minute

// Rollback replaces a running VM's state with a snapshot's: its memory,
// devices and disk. The VM keeps its ID, network and address, and its
// identity (hostname and machine ID); everything since the snapshot is lost.
// The snapshot must be one taken of this VM, or the one it was started from.
//
// It's refused with ErrBusy while commands run in the VM. If the rollback
// fails once the old VM is stopped, the VM is marked failed.
func (m *Manager) Rollback(ctx context.Context, id, snapID string) (state.VM, error) {
	ctx = logging.WithVM(ctx, id)
	unlock := m.locks.lock(id)
	defer unlock()
	vm, err := m.store.GetVM(ctx, id)
	if err != nil {
		return state.VM{}, err
	}
	if vm.State != state.StateRunning {
		return state.VM{}, fmt.Errorf("VM %s is %s: %w", id, vm.State, ErrNotRunning)
	}
	snap, err := m.store.GetSnapshot(ctx, snapID)
	if err != nil {
		return state.VM{}, err
	}
	if snap.VMID != vm.ID && snap.ID != vm.SnapshotID {
		return state.VM{}, fmt.Errorf("%w: snapshot %s was taken of %s, not %s, and %s didn't start from it",
			ErrInvalidRequest, snap.ID, snap.VMID, vm.ID, vm.ID)
	}
	if err := m.checkRestorable(ctx, snapID); err != nil {
		return state.VM{}, err
	}
	leave, err := m.gate.enterExclusive(id, "rollback")
	if err != nil {
		return state.VM{}, err
	}
	defer leave()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()
	log := m.log.With("vm", id, "snapshot", snapID)

	// Claim the snapshot first: from here it can't be deleted.
	old := vm
	if vm, err = m.store.UpdateVM(ctx, id, "rolling back", func(v *state.VM) error {
		v.SnapshotID = snapID
		return nil
	}); err != nil {
		return state.VM{}, err
	}
	t := newTimer()
	fail := func(stage string, cause error) (state.VM, error) {
		m.untrack(id)
		reason := fmt.Sprintf("rollback to %s failed at %s: %v", snapID, stage, cause)
		log.ErrorContext(ctx, "rollback failed; the VM is marked failed", "stage", stage, "err", cause)
		failed, _ := m.store.Transition(ctx, id, state.StateFailed, reason)
		return failed, fmt.Errorf("roll back VM %s: %s: %w", id, stage, cause)
	}

	// Stop the VM as it is now, and remove its jail. Its network stays.
	_, oldJail, err := m.resources(old)
	if err != nil {
		return fail("plan", err)
	}
	m.mu.Lock()
	tr := m.procs[id]
	m.mu.Unlock()
	if tr != nil {
		err = m.stopOwned(id, tr)
	} else {
		err = m.host.killFirecracker(ctx, oldJail, old.PID)
	}
	if err != nil {
		return fail("stop", err)
	}
	if err := m.host.cleanupJail(oldJail); err != nil {
		return fail("stop", err)
	}
	t.mark("stop")

	// Start it again from the snapshot, in a new jail.
	_, j, err := m.resources(vm)
	if err != nil {
		return fail("plan", err)
	}
	if err := m.host.prepareJail(j); err != nil {
		return fail("jail", err)
	}
	t.mark("jail")
	proc, err := m.host.startFirecracker(j)
	if err != nil {
		return fail("start", err)
	}
	m.track(id, proc)
	t.mark("start")
	if err := m.host.loadSnapshot(ctx, j); err != nil {
		return fail("restore", err)
	}
	t.mark("restore")
	agentCtx, agentCancel := context.WithTimeout(ctx, m.cfg.AgentTimeout)
	err = m.host.waitAgent(agentCtx, j)
	agentCancel()
	if err != nil {
		return fail("agent", err)
	}
	t.mark("agent")
	if err := m.sync(ctx, vm, j); err != nil {
		return fail("sync", err)
	}
	t.mark("sync")

	timings := t.millis()
	vm, err = m.store.UpdateVM(ctx, id, "rolled back", func(v *state.VM) error {
		v.PID = proc.pid()
		if v.Timings == nil {
			v.Timings = map[string]float64{}
		}
		v.Timings["rollback"] = timings["total"]
		return nil
	})
	if err != nil {
		return fail("record", err)
	}
	m.tracking(id, snapID)
	_ = m.store.Note(ctx, id, fmt.Sprintf("rolled back to %s in %.0fms", snapID, timings["total"]))
	m.cfg.Metrics.RolledBack(timings)
	log.InfoContext(ctx, "rolled back", "timings_ms", timings)
	return vm, nil
}
