package vm

import (
	"fmt"

	"github.com/sanidhyasin/tine/internal/snapshot"
	"github.com/sanidhyasin/tine/internal/state"
	"github.com/sanidhyasin/tine/internal/storage"
)

// Diff snapshots (Config.DiffSnapshots). With dirty-page tracking on, KVM
// knows which pages a VM has changed since it was restored or last
// snapshotted. A diff snapshot has Firecracker write only those, into a
// sparse memory file. Once the VM runs again, tined makes the snapshot's
// whole memory: its base's memory, with the diff written over it. The VM
// is paused for as long as writing the changed pages takes, not all of them.
//
// A VM's base is the snapshot its memory has changed from: the one it was
// restored from, then each snapshot taken of it. Only VMs whose Firecracker
// this tined started with tracking on have a base. The others (VMs from
// before tined restarted, or booted rather than restored) get a full
// snapshot, which becomes their base. So does a VM whose last snapshot
// failed after Firecracker began writing it, since that may have reset the
// tracking.

// How a diff is merged onto its base's memory (Config.DiffMerge).
const (
	// mergeReflink clones the base, so the snapshot shares its unchanged
	// pages with the base on disk. The clone's pages aren't in the page
	// cache, though: VMs restored from it read them from the disk.
	mergeReflink = "reflink"
	// mergeCopy copies the base through the page cache, so VMs restored from
	// the snapshot find all of it there, at the cost of a full copy.
	mergeCopy = "copy"
)

func validMerge(s string) error {
	switch s {
	case "", mergeReflink, mergeCopy:
		return nil
	}
	return fmt.Errorf("diff merge %q: want %q or %q", s, mergeReflink, mergeCopy)
}

// baseLinkFile is a hard link to the base's memory inside a snapshot being
// built, so the base can't be deleted from under the merge.
const baseLinkFile = ".base-memory"

// diffMerge says how collectSnapshot makes a snapshot's memory file.
type diffMerge struct {
	// BaseID is the base snapshot's ID.
	BaseID string
	// Base is the base's memory, to apply the diff over. Empty for a full
	// snapshot, whose memory file is Firecracker's as it is.
	Base string
	// Copy copies the base through the page cache instead of cloning it.
	Copy bool
}

// tracking records that the VM's Firecracker, just started by this tined,
// tracks dirty pages since base: the snapshot it was restored from, or
// empty for a booted VM.
func (m *Manager) tracking(vmID, base string) {
	if !m.cfg.DiffSnapshots {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.diffBases[vmID] = base
}

// rebase records the VM's new base after a snapshot, if it's tracked. An
// empty base makes its next snapshot a full one.
func (m *Manager) rebase(vmID, base string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.diffBases[vmID]; ok {
		m.diffBases[vmID] = base
	}
}

// untrack forgets a VM that's gone, or whose Firecracker is.
func (m *Manager) untrack(vmID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.diffBases, vmID)
}

// planDiff decides whether the snapshot of vm being built in b can be a
// diff: if the VM has a base whose memory is still there. Then it links
// the base's memory into b.
func (m *Manager) planDiff(vm state.VM, kind state.SnapshotKind, b *snapshot.Building) diffMerge {
	m.mu.Lock()
	base := m.diffBases[vm.ID]
	m.mu.Unlock()
	if base == "" {
		return diffMerge{}
	}
	link := b.Path(baseLinkFile)
	if err := storage.Link(m.snaps.Path(base, snapshot.MemoryFile), link); err != nil {
		// Deleted since, most likely: then this snapshot is a full one.
		m.log.Info("diff snapshot: the base's memory is gone, taking a full snapshot", "vm", vm.ID, "base", base, "err", err)
		return diffMerge{}
	}
	// A fork's children restore from its snapshot straight away, so it had
	// better be in the page cache.
	copyBase := m.cfg.DiffMerge == mergeCopy || (m.cfg.DiffMerge == "" && kind == state.SnapshotKindFork)
	return diffMerge{BaseID: base, Base: link, Copy: copyBase}
}

// diffBaseOf is the VM's base, for tests.
func (m *Manager) diffBaseOf(vmID string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	base, ok := m.diffBases[vmID]
	return base, ok
}
