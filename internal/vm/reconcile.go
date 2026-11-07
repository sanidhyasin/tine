package vm

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sanidhyasin/tine/internal/netns"
	"github.com/sanidhyasin/tine/internal/state"
)

// Report says what Reconcile found and did, by VM ID or resource.
type Report struct {
	// Kept: running VMs whose Firecracker is alive, and failed VMs, which
	// stay (with their console logs) until someone destroys them or they expire.
	Kept []string
	// Finished: destroys that a restart interrupted, now done.
	Finished []string
	// Cleaned: VMs found half-created, or whose Firecracker had died, now
	// destroyed. Their records keep the reason.
	Cleaned []string
	// Orphans: resources on the host that no live VM owns, now removed.
	Orphans []string
	// Errors: what couldn't be done. Reconcile carries on past them.
	Errors []string
}

func (r Report) String() string {
	part := func(name string, items []string) string {
		if len(items) == 0 {
			return name + " 0"
		}
		return fmt.Sprintf("%s %d (%s)", name, len(items), strings.Join(items, ", "))
	}
	return strings.Join([]string{
		part("kept", r.Kept), part("finished", r.Finished), part("cleaned", r.Cleaned),
		part("orphans removed", r.Orphans), part("errors", r.Errors),
	}, "; ")
}

// inventory is what VM resources exist on the host.
type inventory struct {
	// ids are the VMs that have a network namespace, jail or cgroup.
	ids []string
	// veths are the host veths (tine<N>).
	veths []string
}

// Reconcile brings the records and the host back in line, after tined
// starts. It runs before tined serves requests:
//
//   - creating:   tined stopped mid-create; what exists is undone
//   - destroying: tined stopped mid-destroy; the destroy is finished
//   - running:    kept if its Firecracker is alive, else cleaned up as failed
//   - failed:     kept for inspection
//
// Then anything on the host that no live VM owns (a namespace, jail, cgroup
// with its processes, or veth) is removed.
func (m *Manager) Reconcile(ctx context.Context) (r Report) {
	defer func() {
		m.cfg.Metrics.Reconciled("kept", len(r.Kept))
		m.cfg.Metrics.Reconciled("finished", len(r.Finished))
		m.cfg.Metrics.Reconciled("cleaned", len(r.Cleaned))
		m.cfg.Metrics.Reconciled("orphan", len(r.Orphans))
	}()
	errorf := func(format string, args ...any) { r.Errors = append(r.Errors, fmt.Sprintf(format, args...)) }

	live, err := m.store.ListVMs(ctx, state.ListOptions{})
	if err != nil {
		errorf("list VMs: %v", err)
		return r
	}
	for _, vm := range live {
		switch vm.State {
		case state.StateCreating:
			_, _ = m.store.Transition(ctx, vm.ID, state.StateFailed, "tined stopped while the VM was being created")
			if _, err := m.destroy(ctx, vm.ID, "cleaning up after tined stopped mid-create"); err != nil {
				errorf("%s: %v", vm.ID, err)
			} else {
				r.Cleaned = append(r.Cleaned, vm.ID)
			}
		case state.StateDestroying:
			if _, err := m.destroy(ctx, vm.ID, "finishing a destroy that tined's restart interrupted"); err != nil {
				errorf("%s: %v", vm.ID, err)
			} else {
				r.Finished = append(r.Finished, vm.ID)
			}
		case state.StateRunning:
			_, j, err := m.resources(vm)
			if err != nil {
				errorf("%s: %v", vm.ID, err)
				continue
			}
			alive, err := m.host.alive(ctx, j)
			if err != nil {
				errorf("%s: check Firecracker: %v", vm.ID, err)
				continue
			}
			if alive {
				r.Kept = append(r.Kept, vm.ID)
				continue
			}
			_, _ = m.store.Transition(ctx, vm.ID, state.StateFailed, "its Firecracker wasn't running when tined started")
			if _, err := m.destroy(ctx, vm.ID, "cleaning up a VM whose Firecracker had died"); err != nil {
				errorf("%s: %v", vm.ID, err)
			} else {
				r.Cleaned = append(r.Cleaned, vm.ID)
			}
		case state.StateFailed:
			r.Kept = append(r.Kept, vm.ID)
		}
	}

	// Orphans: compare the host with the VMs that are still live now.
	live, err = m.store.ListVMs(ctx, state.ListOptions{})
	if err != nil {
		errorf("list VMs: %v", err)
		return r
	}
	ids, slots := map[string]bool{}, map[int]bool{}
	for _, vm := range live {
		ids[vm.ID] = true
		slots[vm.Slot] = true
	}
	inv, err := m.host.inventory(ctx)
	if err != nil {
		errorf("list host resources: %v", err)
		return r
	}
	for _, id := range inv.ids {
		if ids[id] {
			continue
		}
		if err := m.host.removeOrphan(ctx, id); err != nil {
			errorf("orphan %s: %v", id, err)
		} else {
			r.Orphans = append(r.Orphans, id)
		}
	}
	for _, v := range inv.veths {
		if slot, ok := netns.SlotOfVeth(v); ok && slots[slot] {
			continue
		}
		if err := m.host.removeVeth(v); err != nil {
			errorf("orphan veth %s: %v", v, err)
		} else {
			r.Orphans = append(r.Orphans, "veth "+v)
		}
	}
	return r
}

// RunReaper destroys VMs whose time to live has run out, every interval,
// and once an hour purges the records of VMs destroyed longer ago than
// DestroyedRetention. It returns when ctx ends.
func (m *Manager) RunReaper(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var lastPurge time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			m.reapOnce(ctx, now)
			if now.Sub(lastPurge) >= time.Hour {
				m.purgeOnce(ctx, now)
				lastPurge = now
			}
		}
	}
}

// reapOnce destroys the VMs that have expired at now, and returns how many it destroyed.
func (m *Manager) reapOnce(ctx context.Context, now time.Time) int {
	expired, err := m.store.ExpiredVMs(ctx, now)
	if err != nil {
		m.log.Error("reaper: list expired VMs", "err", err)
		return 0
	}
	n := 0
	for _, vm := range expired {
		if _, err := m.destroy(ctx, vm.ID, "expired: its time to live ran out"); err != nil {
			m.log.Error("reaper: destroy failed", "vm", vm.ID, "err", err)
			continue
		}
		m.log.Info("reaper: destroyed an expired VM", "vm", vm.ID, "expired_at", vm.ExpiresAt)
		n++
	}
	m.cfg.Metrics.Reaped(n)
	return n
}

func (m *Manager) purgeOnce(ctx context.Context, now time.Time) {
	n, err := m.store.PurgeDestroyed(ctx, now.Add(-m.cfg.DestroyedRetention))
	if err != nil {
		m.log.Error("reaper: purge old records", "err", err)
		return
	}
	if n > 0 {
		m.log.Info("reaper: purged records of long-destroyed VMs", "count", n)
	}
}
