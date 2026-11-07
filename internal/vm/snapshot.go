package vm

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/sanidhyasin/tine/internal/image"
	"github.com/sanidhyasin/tine/internal/logging"
	"github.com/sanidhyasin/tine/internal/snapshot"
	"github.com/sanidhyasin/tine/internal/state"
)

// Where Firecracker writes a new snapshot, inside the VM's jail. tined then
// moves the files into the snapshot store. Restored VMs read theirs from
// elsewhere (restorePath), so a snapshot can never be written over the files
// a VM runs from.
const (
	snapshotDir       = "/snapshot"
	snapshotStatePath = snapshotDir + "/" + snapshot.StateFile
	snapshotMemPath   = snapshotDir + "/" + snapshot.MemoryFile
)

// Where a VM restored from a snapshot finds the snapshot's state and memory,
// inside its jail: hard links to the files in the snapshot store.
const (
	restoreDir       = "/restore"
	restoreStatePath = restoreDir + "/" + snapshot.StateFile
	restoreMemPath   = restoreDir + "/" + snapshot.MemoryFile
)

// snapshotTimeout bounds a whole snapshot. Writing a large VM's memory is
// the slow part.
const snapshotTimeout = 5 * time.Minute

// Snapshot saves a running VM: its memory, its devices and its disk. The VM
// is paused while its memory and disk are copied, then carries on. The
// snapshot belongs to the VM, and is garbage once the VM is destroyed and no
// VM runs from it.
//
// It's refused with ErrBusy while commands run in the VM (taking a snapshot
// resets the guest's vsock connections, which would cut them off), with
// ErrCapacity when the VM has MaxSnapshotsPerVM already, and with ErrNoSpace
// when the disk is short of room for it.
func (m *Manager) Snapshot(ctx context.Context, id string) (state.Snapshot, error) {
	ctx = logging.WithVM(ctx, id)
	unlock := m.locks.lock(id)
	defer unlock()
	vm, err := m.store.GetVM(ctx, id)
	if err != nil {
		return state.Snapshot{}, err
	}
	if vm.State != state.StateRunning {
		return state.Snapshot{}, fmt.Errorf("VM %s is %s: %w", id, vm.State, ErrNotRunning)
	}
	existing, err := m.store.ListSnapshots(ctx, state.SnapshotFilter{VMID: id, Kind: state.SnapshotKindVM})
	if err != nil {
		return state.Snapshot{}, err
	}
	if n := countLive(existing); n >= m.cfg.MaxSnapshotsPerVM {
		return state.Snapshot{}, fmt.Errorf("%w: VM %s has %d snapshots, the most it may have; delete some first", ErrCapacity, id, n)
	}
	return m.takeSnapshot(ctx, vm, state.SnapshotKindVM)
}

// takeSnapshot snapshots vm, which the caller has locked and checked is
// running. Whatever happens, a VM it paused is resumed.
func (m *Manager) takeSnapshot(ctx context.Context, vm state.VM, kind state.SnapshotKind) (state.Snapshot, error) {
	leave, err := m.gate.enterExclusive(vm.ID, "snapshot")
	if err != nil {
		return state.Snapshot{}, err
	}
	defer leave()
	// Once the VM is paused, the snapshot must run to the end and resume
	// it, even if the caller gives up.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), snapshotTimeout)
	defer cancel()
	log := m.log.With("vm", vm.ID)

	need := uint64(vm.MemoryMiB+m.cfg.MinFreeMiB) << 20 //nolint:gosec // both are small and positive
	free, err := m.snaps.FreeBytes()
	if err != nil {
		return state.Snapshot{}, err
	}
	if free < need {
		return state.Snapshot{}, fmt.Errorf("%w: a snapshot of VM %s needs %d MiB, plus %d MiB kept free, and %d MiB is free",
			ErrNoSpace, vm.ID, vm.MemoryMiB, m.cfg.MinFreeMiB, free>>20)
	}
	built, err := m.imageBuild(vm)
	if err != nil {
		return state.Snapshot{}, err
	}
	_, j, err := m.resources(vm)
	if err != nil {
		return state.Snapshot{}, err
	}
	host, err := m.host.describeHost(ctx, j)
	if err != nil {
		return state.Snapshot{}, fmt.Errorf("snapshot VM %s: %w", vm.ID, err)
	}

	rec, err := m.store.CreateSnapshot(ctx, state.Snapshot{
		ID: newSnapshotID(), Kind: kind, VMID: vm.ID, Image: vm.Image, VCPUs: vm.VCPUs, MemoryMiB: vm.MemoryMiB,
	})
	if err != nil {
		return state.Snapshot{}, err
	}
	log = log.With("snapshot", rec.ID)
	if kind == state.SnapshotKindFork {
		// Ready but not yet claimed by any child, it would look like
		// garbage. The fork unpins it once its children are made.
		m.pin(rec.ID)
	}
	b, err := m.snaps.Begin(rec.ID)
	if err != nil {
		m.forgetSnapshot(ctx, rec.ID)
		if kind == state.SnapshotKindFork {
			m.unpin(rec.ID)
		}
		return state.Snapshot{}, err
	}
	merge := m.planDiff(vm, kind, b)
	wrote := false
	fail := func(stage string, cause error) (state.Snapshot, error) {
		if kind == state.SnapshotKindFork {
			defer m.unpin(rec.ID)
		}
		if wrote {
			// Firecracker may have reset its dirty-page tracking: the next
			// snapshot must be a full one.
			m.rebase(vm.ID, "")
		}
		log.WarnContext(ctx, "snapshot failed", "stage", stage, "err", cause)
		m.cfg.Metrics.SnapshotFailed(string(kind), stage)
		_ = b.Abort()
		_ = m.host.prepareSnapshot(j) // empties the jail's snapshot directory
		m.forgetSnapshot(ctx, rec.ID)
		return state.Snapshot{}, fmt.Errorf("snapshot VM %s: %s: %w", vm.ID, stage, cause)
	}

	t := newTimer()
	// The VM's last snapshot must be on disk first: then its pages in the
	// cache are clean, and the kernel can drop them at once to make room for
	// this one's, instead of writing them out while the VM is paused.
	flushed, err := m.waitForFlush(ctx, vm.ID)
	if err != nil {
		return fail("wait", err)
	}
	// Just after a big flush, the kernel throttles the next writer to the
	// disk's speed: a 256 MiB VM's snapshot started then paused it for up
	// to 1.3 s, instead of 50 ms. So the pause waits until the flush is a
	// little older, while the VM runs.
	if d := time.Until(flushed.Add(m.cfg.FlushSettle)); d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return fail("wait", ctx.Err())
		}
	}
	t.mark("wait")
	if err := m.host.prepareSnapshot(j); err != nil {
		return fail("prepare", err)
	}
	t.mark("prepare")
	pausedAt := time.Now()
	if err := m.host.pause(ctx, j); err != nil {
		// A failed pause may have paused it anyway.
		_ = m.host.resume(ctx, j)
		return fail("pause", err)
	}
	t.mark("pause")
	// The VM is frozen: its memory and its disk are consistent with each other.
	wrote = true
	writeErr := m.host.writeSnapshot(ctx, j, merge.Base != "")
	t.mark("memory")
	var diskErr error
	if writeErr == nil {
		diskErr = m.host.cloneDisk(j, b.Path(snapshot.DiskFile))
		t.mark("disk")
	}
	resumeErr := m.host.resume(ctx, j)
	t.mark("resume")
	paused := time.Since(pausedAt)
	switch {
	case resumeErr != nil:
		// A VM that can't be resumed is no use to anyone.
		reason := "couldn't resume it after a snapshot: " + resumeErr.Error()
		_, _ = m.store.Transition(ctx, vm.ID, state.StateFailed, reason)
		return fail("resume", resumeErr)
	case writeErr != nil:
		return fail("memory", writeErr)
	case diskErr != nil:
		return fail("disk", diskErr)
	}
	// With a diff, this makes the whole memory from the base's, while the VM runs.
	diffBytes, err := m.host.collectSnapshot(j, b, merge)
	if err != nil {
		return fail("collect", err)
	}
	if merge.Base != "" {
		t.mark("merge")
	} else {
		t.mark("collect")
	}
	// Taking the snapshot reset the guest's vsock connections. Its agent
	// keeps listening, so it should answer straight away.
	agentCtx, agentCancel := context.WithTimeout(ctx, 10*time.Second)
	if err := m.host.waitAgent(agentCtx, j); err != nil {
		log.WarnContext(ctx, "the agent didn't answer after the snapshot", "err", err)
	}
	agentCancel()
	t.mark("agent")

	timings := t.millis()
	timings["paused"] = ms(paused)
	meta := snapshot.Metadata{
		Kind: string(kind), VMID: vm.ID, Image: vm.Image, ImageCreatedAt: built,
		VCPUs: vm.VCPUs, MemoryMiB: vm.MemoryMiB, Host: host, CreatedAt: rec.CreatedAt, TimingsMs: timings,
	}
	if merge.Base != "" {
		meta.Base, meta.DiffBytes = merge.BaseID, diffBytes
	}
	size, err := b.Commit(&meta)
	if err != nil {
		return fail("store", err)
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return fail("store", err)
	}
	ready, err := m.store.SnapshotReady(ctx, rec.ID, size, data)
	if err != nil {
		_ = m.snaps.Remove(rec.ID)
		return fail("record", err)
	}
	m.cfg.Metrics.SnapshotTaken(string(kind), timings, size)
	m.rebase(vm.ID, ready.ID)
	m.flushInBackground(vm.ID, ready.ID)
	_ = m.store.Note(ctx, vm.ID, fmt.Sprintf("%s snapshot %s taken (paused %.0fms)", kind, ready.ID, timings["paused"]))
	log.InfoContext(ctx, "snapshot taken", "kind", kind, "bytes", size, "timings_ms", timings)
	return ready, nil
}

// flush is a snapshot's flush to disk: done is closed when it's over, at
// end.
type flush struct {
	done chan struct{}
	end  time.Time
}

// flushInBackground writes a new snapshot's memory out to disk, now that the
// VM is running again, so its pages in the cache become clean. The VM's next
// snapshot waits for it (waitForFlush).
func (m *Manager) flushInBackground(vmID, snapID string) {
	f := &flush{done: make(chan struct{})}
	m.mu.Lock()
	m.flushes[vmID] = f
	m.mu.Unlock()
	go func() {
		defer func() {
			f.end = time.Now()
			close(f.done)
		}()
		start := time.Now()
		if err := m.host.flushSnapshot(m.snaps.Path(snapID, snapshot.MemoryFile)); err != nil {
			m.log.Warn("couldn't flush a snapshot to disk", "vm", vmID, "snapshot", snapID, "err", err)
			return
		}
		m.cfg.Metrics.SnapshotFlushed(time.Since(start))
		m.log.Info("snapshot on disk", "vm", vmID, "snapshot", snapID, "ms", ms(time.Since(start)))
	}()
}

// waitForFlush waits until the VM's last snapshot, if any, is on disk, and
// returns when that flush ended (zero if there was none).
func (m *Manager) waitForFlush(ctx context.Context, vmID string) (time.Time, error) {
	m.mu.Lock()
	f := m.flushes[vmID]
	m.mu.Unlock()
	if f == nil {
		return time.Time{}, nil
	}
	select {
	case <-f.done:
		return f.end, nil
	case <-ctx.Done():
		return time.Time{}, fmt.Errorf("waiting for the last snapshot to reach the disk: %w", ctx.Err())
	}
}

// imageBuild says which build of its image a VM runs: that of the snapshot
// it was restored from, or else the image as installed now.
func (m *Manager) imageBuild(vm state.VM) (time.Time, error) {
	if vm.SnapshotID != "" {
		if meta, err := m.snaps.Load(vm.SnapshotID); err == nil {
			return meta.ImageCreatedAt, nil
		}
	}
	img, err := image.Open(m.cfg.ImagesDir, vm.Image)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: %s: %w", ErrImageNotFound, vm.Image, err)
	}
	return img.Meta.CreatedAt, nil
}

// forgetSnapshot removes a snapshot that never became ready: its files, and
// its record.
func (m *Manager) forgetSnapshot(ctx context.Context, id string) {
	_ = m.snaps.Remove(id)
	if _, err := m.store.MarkSnapshotDeleting(ctx, id); err != nil && !errors.Is(err, state.ErrNotFound) {
		m.log.Error("couldn't remove the record of a failed snapshot", "snapshot", id, "err", err)
		return
	}
	_ = m.store.DeleteSnapshotRecord(ctx, id)
}

// removeSnapshot finishes deleting a snapshot already marked deleting: its
// files, then its record.
func (m *Manager) removeSnapshot(ctx context.Context, id string) error {
	if err := m.snaps.Remove(id); err != nil {
		return err
	}
	return m.store.DeleteSnapshotRecord(ctx, id)
}

// countLive counts the snapshots that aren't being deleted.
func countLive(snaps []state.Snapshot) int {
	n := 0
	for _, s := range snaps {
		if s.State != state.SnapshotDeleting {
			n++
		}
	}
	return n
}

// newSnapshotID makes a snapshot ID: "snap-" and 10 random characters (50 bits).
func newSnapshotID() string {
	b := make([]byte, 7)
	_, _ = rand.Read(b)
	return "snap-" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))[:10]
}

// SnapshotInfo is a snapshot and the VMs that run from it.
type SnapshotInfo struct {
	state.Snapshot
	// UsedBy are the live VMs restored from it. It can't be deleted while
	// there are any.
	UsedBy []string
}

// Snapshots lists snapshots, oldest first.
func (m *Manager) Snapshots(ctx context.Context, f state.SnapshotFilter) ([]SnapshotInfo, error) {
	snaps, err := m.store.ListSnapshots(ctx, f)
	if err != nil {
		return nil, err
	}
	out := make([]SnapshotInfo, 0, len(snaps))
	for _, s := range snaps {
		users, err := m.store.SnapshotUsers(ctx, s.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, SnapshotInfo{Snapshot: s, UsedBy: users})
	}
	return out, nil
}

// GetSnapshot returns one snapshot.
func (m *Manager) GetSnapshot(ctx context.Context, id string) (SnapshotInfo, error) {
	s, err := m.store.GetSnapshot(ctx, id)
	if err != nil {
		return SnapshotInfo{}, err
	}
	users, err := m.store.SnapshotUsers(ctx, id)
	return SnapshotInfo{Snapshot: s, UsedBy: users}, err
}

// DeleteSnapshot deletes a snapshot's files and record. It's refused with
// ErrInUse while VMs run from it, and with ErrBusy while it's being taken.
func (m *Manager) DeleteSnapshot(ctx context.Context, id string) (state.Snapshot, error) {
	snap, err := m.store.GetSnapshot(ctx, id)
	if err != nil {
		return state.Snapshot{}, err
	}
	if snap.State == state.SnapshotCreating || m.pinned(id) {
		return state.Snapshot{}, fmt.Errorf("%w: snapshot %s is still being taken, or forked from", ErrBusy, id)
	}
	if snap, err = m.store.MarkSnapshotDeleting(ctx, id); err != nil {
		return state.Snapshot{}, err
	}
	if err := m.removeSnapshot(ctx, id); err != nil {
		return snap, fmt.Errorf("delete snapshot %s: %w", id, err)
	}
	m.cfg.Metrics.SnapshotDeleted("requested")
	m.log.InfoContext(ctx, "snapshot deleted", "snapshot", id)
	return snap, nil
}

// collectGarbage deletes the snapshots nothing needs any more (see
// state.Store.GarbageSnapshots): those of destroyed VMs, and templates
// that newer ones replaced or whose image is gone, once no VM runs from
// them. It returns how many it deleted.
func (m *Manager) collectGarbage(ctx context.Context) int {
	garbage, err := m.store.GarbageSnapshots(ctx)
	if err != nil {
		m.log.Error("snapshot GC: list garbage", "err", err)
		return 0
	}
	// Templates of images that are gone from the disk are garbage too.
	templates, err := m.store.ListSnapshots(ctx, state.SnapshotFilter{Kind: state.SnapshotKindTemplate})
	if err != nil {
		m.log.Error("snapshot GC: list templates", "err", err)
		return 0
	}
	for _, s := range templates {
		if _, err := image.Open(m.cfg.ImagesDir, s.Image); s.State == state.SnapshotReady && errors.Is(err, fs.ErrNotExist) {
			garbage = append(garbage, s)
		}
	}
	n := 0
	for _, s := range garbage {
		if m.pinned(s.ID) {
			continue // a fork is still making its children
		}
		// A VM may have claimed it since it was listed: then this refuses.
		if _, err := m.store.MarkSnapshotDeleting(ctx, s.ID); err != nil {
			if !errors.Is(err, state.ErrInUse) && !errors.Is(err, state.ErrNotFound) {
				m.log.Error("snapshot GC", "snapshot", s.ID, "err", err)
			}
			continue
		}
		if err := m.removeSnapshot(ctx, s.ID); err != nil {
			m.log.Error("snapshot GC: remove", "snapshot", s.ID, "err", err)
			continue
		}
		m.cfg.Metrics.SnapshotDeleted("garbage")
		m.log.Info("snapshot GC: deleted a snapshot nothing needs", "snapshot", s.ID, "kind", s.Kind, "vm", s.VMID)
		n++
	}
	return n
}
