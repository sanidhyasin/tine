package vm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/sanidhyasin/tine/internal/agentproto"
	"github.com/sanidhyasin/tine/internal/jailer"
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

// sync makes a new guest current and unique through its agent: the host's
// time, fresh entropy, the VM's ID as its hostname, and a new machine ID. A
// restored guest needs all of these; a booted one gets them too, so every VM
// is the same in these ways however it started.
func (m *Manager) sync(ctx context.Context, vm state.VM, j *jailer.Jail) error {
	entropy := make([]byte, 64)
	machineID := make([]byte, 16)
	if _, err := rand.Read(entropy); err != nil {
		return err
	}
	if _, err := rand.Read(machineID); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()
	res, err := m.host.syncGuest(ctx, j, agentproto.SyncRequest{
		TimeUnixNano: time.Now().UnixNano(),
		Entropy:      entropy,
		Hostname:     vm.ID,
		MachineID:    hex.EncodeToString(machineID),
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
