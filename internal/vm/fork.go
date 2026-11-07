package vm

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sanidhyasin/tine/internal/logging"
	"github.com/sanidhyasin/tine/internal/state"
)

// Forking: one snapshot of a running VM, restored many times at once. The
// parent is only paused while the snapshot is taken. Every child's jail has
// hard links to the same memory file, which Firecracker maps privately, so
// the host's page cache holds one copy of each page until a child changes
// it.

// ForkRequest asks for children of a VM.
type ForkRequest struct {
	// Count is how many children: 1 to Config.MaxForkChildren.
	Count int
	// TTL is how long each child lives. Zero means the default.
	TTL time.Duration
}

// ForkChild is one child: the VM, or why it couldn't be made.
type ForkChild struct {
	VM  state.VM
	Err error
}

// ForkResult is what a fork made.
type ForkResult struct {
	// Snapshot is the parent's snapshot the children were restored from.
	Snapshot state.Snapshot
	// Children are in the order they were asked for. Each succeeded or
	// failed on its own; failed ones left nothing behind.
	Children []ForkChild
	// Timings: snapshot (the whole snapshot), paused (how long the parent
	// was frozen), children (restoring them all) and total, in milliseconds.
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
	if req.Count < 1 || req.Count > m.cfg.MaxForkChildren {
		return ForkResult{}, fmt.Errorf("%w: a fork makes 1 to %d children, not %d", ErrInvalidRequest, m.cfg.MaxForkChildren, req.Count)
	}
	if req.TTL == 0 {
		req.TTL = m.cfg.DefaultTTL
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
	// Check every child's request before pausing the parent.
	if _, err := m.normalize(ctx, CreateRequest{TTL: req.TTL, VCPUs: parent.VCPUs, MemoryMiB: parent.MemoryMiB}); err != nil {
		unlock()
		return ForkResult{}, err
	}
	snap, err := m.takeSnapshot(ctx, parent, state.SnapshotKindFork)
	unlock()
	if err != nil {
		return ForkResult{}, err
	}
	// takeSnapshot pinned it: until the children have claimed it, it would
	// look like garbage.
	defer m.unpin(snap.ID)
	t.mark("snapshot")

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Duration(req.Count)*m.cfg.AgentTimeout+time.Minute)
	defer cancel()
	children := make([]ForkChild, req.Count)
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range min(m.cfg.ForkParallelism, req.Count) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				vm, err := m.Create(ctx, CreateRequest{SnapshotID: snap.ID, TTL: req.TTL, ParentID: parent.ID})
				children[i] = ForkChild{VM: vm, Err: err}
			}
		}()
	}
	for i := range req.Count {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	t.mark("children")

	res := ForkResult{Snapshot: snap, Children: children, Timings: t.millis()}
	if meta, err := m.snaps.Load(snap.ID); err == nil {
		res.Timings["paused"] = meta.TimingsMs["paused"]
	}
	failed := res.Failed()
	m.cfg.Metrics.Forked(req.Count, failed, res.Timings)
	_ = m.store.Note(ctx, parent.ID, fmt.Sprintf("forked %d children (%d failed) from %s in %.0fms, paused %.0fms",
		req.Count-failed, failed, snap.ID, res.Timings["total"], res.Timings["paused"]))
	m.log.InfoContext(ctx, "forked", "vm", parent.ID, "snapshot", snap.ID, "children", req.Count, "failed", failed, "timings_ms", res.Timings)
	return res, nil
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
