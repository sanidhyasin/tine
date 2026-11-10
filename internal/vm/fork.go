package vm

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sanidhyasin/tine/internal/image"
	"github.com/sanidhyasin/tine/internal/logging"
	"github.com/sanidhyasin/tine/internal/state"
)

// Forking: one snapshot of a running VM, restored many times at once. The
// parent is only paused while the snapshot is taken. Every child's jail has
// hard links to the same memory file, which Firecracker maps privately, so
// the host's page cache holds one copy of each page until a child changes
// it. ForkSnapshot does the same from a snapshot that's already there.

// ForkRequest asks for children of a VM, or of a snapshot.
type ForkRequest struct {
	// Count is how many children: 1 to Config.MaxForkChildren.
	Count int
	// TTL is how long each child lives. Zero means the default.
	TTL time.Duration
	// Strategy is how the children get their memory. Empty means ForkShare.
	Strategy ForkStrategy
	// Network is the children's network: state.NetworkDefault (also when
	// empty) or state.NetworkNone. Children don't inherit their parent's.
	Network state.Network
}

// ForkStrategy is how a fork's children get their memory: one rung of the
// ladder that step 4 benchmarks.
type ForkStrategy string

const (
	// ForkShare: every child maps the one snapshot memory file, shared
	// through the page cache. The default (rung 3).
	ForkShare ForkStrategy = "share"
	// ForkCopy: every child gets its own full copy of the memory file, so
	// nothing is shared (rung 2, the naive way, for comparison).
	ForkCopy ForkStrategy = "copy"
	// ForkUffd: every child's memory is served by a page server, a page at
	// a time as it's touched, into memory of its own (rung 5; pages.go).
	ForkUffd ForkStrategy = "uffd"
	// ForkUffdPrefetch is ForkUffd, with the pages the first child touched
	// as it started copied in for the others before they ask.
	ForkUffdPrefetch ForkStrategy = "uffd-prefetch"
)

// validStrategy reports whether s is a fork strategy tined knows.
func validStrategy(s ForkStrategy) bool {
	switch s {
	case "", ForkShare, ForkCopy, ForkUffd, ForkUffdPrefetch:
		return true
	}
	return false
}

// ForkChild is one child: the VM, or why it couldn't be made.
type ForkChild struct {
	VM  state.VM
	Err error
}

// ForkResult is what a fork made.
type ForkResult struct {
	// Snapshot is the snapshot the children were restored from: the
	// parent's, taken for the fork, or the one ForkSnapshot was given.
	Snapshot state.Snapshot
	// Children are in the order they were asked for. Each succeeded or
	// failed on its own; failed ones left nothing behind.
	Children []ForkChild
	// Timings: snapshot (the whole snapshot), paused (how long the parent
	// was frozen), children (restoring them all) and total, in milliseconds.
	// A fork of a snapshot has no snapshot or paused.
	Timings map[string]float64
}

// Failed counts the children that couldn't be made.
func (r ForkResult) Failed() int {
	n := 0
	for _, c := range r.Children {
		if c.Err != nil {
			n++
		}
	}
	return n
}

// Fork snapshots a running VM and restores Count children from the
// snapshot, ForkParallelism at a time. The parent carries on as before.
// Each child has its own ID, network, disk and identity, and shares the
// parent's memory as it was at the snapshot until it changes it.
//
// Once the snapshot is taken, the fork runs to the end even if the caller
// gives up. It returns an error only if no child could be attempted: the
// request was bad, the parent isn't running, or the snapshot failed.
func (m *Manager) Fork(ctx context.Context, parentID string, req ForkRequest) (ForkResult, error) {
	ctx = logging.WithVM(ctx, parentID)
	req, err := m.checkFork(req)
	if err != nil {
		return ForkResult{}, err
	}
	t := newTimer()

	unlock := m.locks.lock(parentID)
	parent, err := m.store.GetVM(ctx, parentID)
	if err != nil {
		unlock()
		return ForkResult{}, err
	}
	if parent.State != state.StateRunning {
		unlock()
		return ForkResult{}, fmt.Errorf("VM %s is %s: %w", parentID, parent.State, ErrNotRunning)
	}
	// Check every child's request, and that the host has room for them,
	// before pausing the parent.
	if _, err := m.normalize(ctx, CreateRequest{TTL: req.TTL, VCPUs: parent.VCPUs, MemoryMiB: parent.MemoryMiB, Network: req.Network}); err != nil {
		unlock()
		return ForkResult{}, err
	}
	res, err := m.admit(ctx, req.Count, parent.VCPUs, parent.MemoryMiB, m.cfg.RestoreUseMiB)
	if err != nil {
		unlock()
		return ForkResult{}, err
	}
	defer res.release()
	snap, err := m.takeSnapshot(ctx, parent, state.SnapshotKindFork, false)
	unlock()
	if err != nil {
		return ForkResult{}, err
	}
	// takeSnapshot pinned it: until the children have claimed it, it would
	// look like garbage.
	defer m.unpin(snap.ID)
	t.mark("snapshot")

	children := m.restoreChildren(ctx, req, CreateRequest{SnapshotID: snap.ID, ParentID: parent.ID, admitted: res}, t)
	out := ForkResult{Snapshot: snap, Children: children, Timings: t.millis()}
	if meta, err := m.snaps.Load(snap.ID); err == nil {
		out.Timings["paused"] = meta.TimingsMs["paused"]
		if merge, ok := meta.TimingsMs["merge"]; ok {
			out.Timings["merge"] = merge
		}
	}
	failed := out.Failed()
	m.cfg.Metrics.Forked(req.Count, failed, out.Timings)
	_ = m.store.Note(context.WithoutCancel(ctx), parent.ID, fmt.Sprintf("forked %d children (%d failed) from %s in %.0fms, paused %.0fms",
		req.Count-failed, failed, snap.ID, out.Timings["total"], out.Timings["paused"]))
	m.log.InfoContext(ctx, "forked", "vm", parent.ID, "snapshot", snap.ID, "children", req.Count, "failed", failed,
		"network", req.Network, "timings_ms", out.Timings)
	return out, nil
}

// ForkSnapshot restores Count children from a ready snapshot, as Fork does
// from the snapshot it takes, ForkParallelism at a time. Nothing is paused.
// The snapshot can be any VM's, kept or retained, even once that VM is
// gone; a template's; or another fork's. Each child's ParentID is the VM
// the snapshot was taken of, unless it was a template's.
//
// The snapshot is pinned while the children are made, so it can't be
// deleted or collected from under them (DeleteSnapshot is ErrBusy). Once
// admitted, the fork runs to the end even if the caller gives up. It
// returns an error only if no child could be attempted.
func (m *Manager) ForkSnapshot(ctx context.Context, snapID string, req ForkRequest) (ForkResult, error) {
	req, err := m.checkFork(req)
	if err != nil {
		return ForkResult{}, err
	}
	t := newTimer()
	m.pin(snapID)
	defer m.unpin(snapID)
	snap, err := m.store.GetSnapshot(ctx, snapID)
	if err != nil {
		return ForkResult{}, err
	}
	if err := m.checkRestorable(ctx, snapID); err != nil {
		return ForkResult{}, err
	}
	if _, err := image.Open(m.cfg.ImagesDir, snap.Image); err != nil {
		return ForkResult{}, fmt.Errorf("%w: %s, the image of snapshot %s: %w", ErrImageNotFound, snap.Image, snapID, err)
	}
	if _, err := m.normalize(ctx, CreateRequest{SnapshotID: snapID, TTL: req.TTL, Network: req.Network}); err != nil {
		return ForkResult{}, err
	}
	res, err := m.admit(ctx, req.Count, snap.VCPUs, snap.MemoryMiB, m.cfg.RestoreUseMiB)
	if err != nil {
		return ForkResult{}, err
	}
	defer res.release()
	t.mark("check")

	parentID := snap.VMID
	if snap.Kind == state.SnapshotKindTemplate {
		parentID = "" // the VM booted to make it, destroyed straight after
	}
	children := m.restoreChildren(ctx, req, CreateRequest{SnapshotID: snapID, ParentID: parentID, admitted: res}, t)
	out := ForkResult{Snapshot: snap, Children: children, Timings: t.millis()}
	failed := out.Failed()
	m.cfg.Metrics.SnapshotForked(req.Count, failed, out.Timings)
	if parentID != "" {
		// The snapshot's VM may be destroyed, or its record purged: then there's no one to tell.
		_ = m.store.Note(context.WithoutCancel(ctx), parentID, fmt.Sprintf("%d children (%d failed) restored from its snapshot %s in %.0fms",
			req.Count-failed, failed, snapID, out.Timings["total"]))
	}
	m.log.InfoContext(ctx, "forked a snapshot", "snapshot", snapID, "vm", snap.VMID, "children", req.Count, "failed", failed,
		"network", req.Network, "timings_ms", out.Timings)
	return out, nil
}

// checkFork checks a fork's request, and fills in its defaults.
func (m *Manager) checkFork(req ForkRequest) (ForkRequest, error) {
	if !validStrategy(req.Strategy) {
		return req, fmt.Errorf("%w: no fork strategy %q", ErrInvalidRequest, req.Strategy)
	}
	if req.Count < 1 || req.Count > m.cfg.MaxForkChildren {
		return req, fmt.Errorf("%w: a fork makes 1 to %d children, not %d", ErrInvalidRequest, m.cfg.MaxForkChildren, req.Count)
	}
	network, err := normalizeNetwork(req.Network)
	if err != nil {
		return req, err
	}
	req.Network = network
	if req.TTL == 0 {
		req.TTL = m.cfg.DefaultTTL
	}
	return req, nil
}

// restoreChildren creates req.Count VMs from child, a create from the
// fork's snapshot that the fork has admitted, ForkParallelism at a time,
// and marks the timer's "children" (and "first", for a first child
// restored alone). The caller must keep the snapshot from being collected:
// a fork's pinned until its children have claimed it. Once it starts, it
// runs to the end even if ctx ends.
func (m *Manager) restoreChildren(ctx context.Context, req ForkRequest, child CreateRequest, t *timer) []ForkChild {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Duration(req.Count)*m.cfg.AgentTimeout+time.Minute)
	defer cancel()
	child.TTL, child.Strategy, child.Network = req.TTL, req.Strategy, req.Network
	children := make([]ForkChild, req.Count)
	first := 0
	if req.Strategy == ForkUffdPrefetch {
		if child.prefetch = m.workingSetOf(child.SnapshotID); child.prefetch == "" {
			// The first child alone, recording the pages it touches as it
			// starts; the others then copy those in.
			rec := child
			rec.record = true
			vm, err := m.Create(ctx, rec)
			children[0] = ForkChild{VM: vm, Err: err}
			first = 1
			child.prefetch = m.workingSetOf(child.SnapshotID)
			t.mark("first")
		}
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range min(m.cfg.ForkParallelism, req.Count-first) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				vm, err := m.Create(ctx, child)
				children[i] = ForkChild{VM: vm, Err: err}
			}
		}()
	}
	for i := first; i < req.Count; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	t.mark("children")
	return children
}

// pin keeps garbage collection and deletion away from a snapshot, such as
// a fork's while its children are being made. Pins nest.
func (m *Manager) pin(snapID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pins[snapID]++
}

func (m *Manager) unpin(snapID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pins[snapID]--; m.pins[snapID] <= 0 {
		delete(m.pins, snapID)
	}
}

func (m *Manager) pinned(snapID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pins[snapID] > 0
}

// Memory is how much host memory a VM's Firecracker uses, from
// /proc/<pid>/smaps_rollup, in bytes. RSS counts every page it has in
// memory; PSS divides each shared page between the processes that share
// it, so children of one fork, sharing their parent's pages, each have a
// PSS well below their RSS. Summed over VMs, PSS is what they really cost.
type Memory struct {
	RSS     int64
	PSS     int64
	Private int64 // pages only this VM has
	Shared  int64 // pages it shares with other processes
}

// Memory reads how much host memory a running VM uses.
func (m *Manager) Memory(ctx context.Context, id string) (Memory, error) {
	vm, err := m.store.GetVM(ctx, id)
	if err != nil {
		return Memory{}, err
	}
	if vm.State != state.StateRunning {
		return Memory{}, fmt.Errorf("VM %s is %s: %w", id, vm.State, ErrNotRunning)
	}
	_, j, err := m.resources(vm)
	if err != nil {
		return Memory{}, err
	}
	return m.host.memory(j)
}

// parseSmapsRollup reads the totals in /proc/<pid>/smaps_rollup.
func parseSmapsRollup(s string) Memory {
	var mem Memory
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 || f[2] != "kB" {
			continue
		}
		kb, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			continue
		}
		switch b := kb << 10; f[0] {
		case "Rss:":
			mem.RSS = b
		case "Pss:":
			mem.PSS = b
		case "Private_Clean:", "Private_Dirty:":
			mem.Private += b
		case "Shared_Clean:", "Shared_Dirty:":
			mem.Shared += b
		}
	}
	return mem
}
