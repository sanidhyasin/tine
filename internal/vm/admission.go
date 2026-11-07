package vm

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/sanidhyasin/tine/internal/state"
)

// ErrNoMemory means the host can't take the VMs asked for without risking
// running out of memory.
var ErrNoMemory = errors.New("not enough memory")

// Admission control: before a create or a fork touches the host, two rules
// must hold for the VMs it would add.
//
//  1. Commitment. Every live VM's guest memory, plus the new ones', must
//     stay within OvercommitRatio times the host's memory. VMs rarely use
//     all their memory, and forks share most of theirs, so committing more
//     than the host has is the point; the ratio bounds the worst case, of
//     every guest filling its memory at once.
//  2. Headroom. The memory the host has available now, less what the new
//     VMs are expected to use straight away, must stay above MinFreeMemoryMiB.
//
// Requests in flight hold a reservation, so two at once can't both take the
// last room. A request's guest memory leaves its reservation once its VM
// has a record (which the first rule counts), and its expected use once the
// VM is made (and shows in the host's available memory).

// hostMemory is the host's memory, in MiB, from /proc/meminfo.
type hostMemory struct {
	TotalMiB, AvailableMiB int
}

// reservation is admitted room for some VMs, given back as they're made.
type reservation struct {
	m                  *Manager
	commitMiB, useMiB  int // per VM
	committed, useLeft int // VMs whose room is still held
	mu                 sync.Mutex
}

// admit checks that count VMs of memoryMiB each, each expected to use
// useMiB at first, fit, and holds their room.
func (m *Manager) admit(ctx context.Context, count, memoryMiB, useMiB int) (*reservation, error) {
	host, err := m.host.hostMemory()
	if err != nil {
		return nil, fmt.Errorf("admission: %w", err)
	}
	live, err := m.store.ListVMs(ctx, state.ListOptions{})
	if err != nil {
		return nil, err
	}
	committed := 0
	for _, vm := range live {
		committed += vm.MemoryMiB
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	limit := int(m.cfg.OvercommitRatio * float64(host.TotalMiB))
	if total := committed + m.reservedCommitMiB + count*memoryMiB; total > limit {
		return nil, fmt.Errorf("%w: %d more VMs of %d MiB would commit %d MiB of guest memory, more than the limit of %d MiB "+
			"(%.1f times the host's %d MiB); the VMs there now commit %d MiB",
			ErrNoMemory, count, memoryMiB, total, limit, m.cfg.OvercommitRatio, host.TotalMiB, committed+m.reservedCommitMiB)
	}
	if left := host.AvailableMiB - m.reservedUseMiB - count*useMiB; left < m.cfg.MinFreeMemoryMiB {
		return nil, fmt.Errorf("%w: %d more VMs would use about %d MiB to start with, and the host has %d MiB available "+
			"(less %d MiB held for VMs being made), which would leave %d MiB, less than the %d MiB kept free",
			ErrNoMemory, count, count*useMiB, host.AvailableMiB, m.reservedUseMiB, left, m.cfg.MinFreeMemoryMiB)
	}
	m.reservedCommitMiB += count * memoryMiB
	m.reservedUseMiB += count * useMiB
	return &reservation{m: m, commitMiB: memoryMiB, useMiB: useMiB, committed: count, useLeft: count}, nil
}

// recorded says one of the reservation's VMs has a record now, which the
// commitment rule counts from here on.
func (r *reservation) recorded() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.committed > 0 {
		r.committed--
		r.m.mu.Lock()
		r.m.reservedCommitMiB -= r.commitMiB
		r.m.mu.Unlock()
	}
}

// made says one of the reservation's VMs is made, or failed: its expected
// use is in the host's available memory now, or never will be.
func (r *reservation) made() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.useLeft > 0 {
		r.useLeft--
		r.m.mu.Lock()
		r.m.reservedUseMiB -= r.useMiB
		r.m.mu.Unlock()
	}
}

// release gives back whatever the reservation still holds.
func (r *reservation) release() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	r.m.reservedCommitMiB -= r.committed * r.commitMiB
	r.m.reservedUseMiB -= r.useLeft * r.useMiB
	r.committed, r.useLeft = 0, 0
}

// expectedUseMiB is what a new VM is expected to use as it starts:
// measured on the dev server, a booted VM is charged about 170 MiB once
// idle, and one restored from a snapshot, sharing the snapshot's memory,
// under 60 MiB.
func (m *Manager) expectedUseMiB(restore bool) int {
	if restore {
		return m.cfg.RestoreUseMiB
	}
	return m.cfg.BootUseMiB
}
