package vm

import (
	"context"
	"errors"
	"fmt"

	"github.com/sanidhyasin/tine/internal/state"
)

var (
	// ErrNoMemory means the host can't take the VMs asked for without
	// risking running out of memory.
	ErrNoMemory = errors.New("not enough memory")
	// ErrNoCPU means the VMs asked for would give the host more vCPUs than
	// it's allowed to run.
	ErrNoCPU = errors.New("not enough CPU")
)

// Admission control: before a create or a fork touches the host, four
// rules must hold for the VMs it would add.
//
//  1. Commitment. Every live VM's guest memory, plus the new ones', must
//     stay within OvercommitRatio times the host's memory. VMs rarely use
//     all their memory, and forks share most of theirs, so committing more
//     than the host has is the point; the ratio bounds the worst case, of
//     every guest filling its memory at once.
//  2. Headroom. The memory the host has available now, less what the new
//     VMs are expected to use straight away, must stay above MinFreeMemoryMiB.
//  3. vCPUs. Every live VM's vCPUs, plus the new ones', must stay within
//     CPUOvercommitRatio times the host's CPUs. Idle VMs use little CPU, but
//     a host can only run so many: under EC2's nested virtualization, KVM
//     starts failing to enter guests beyond about 60 vCPUs on 4 CPUs.
//  4. Count. Live VMs, plus the new ones, must stay within MaxVMs.
//
// Requests in flight hold a reservation, so two at once can't both take the
// last room. A request's guest memory, vCPUs and VM count leave its
// reservation once its VM has a record (which the rules count), and its
// expected use once the VM is made (and shows in the host's available
// memory).

// hostMemory is the host's memory, in MiB, from /proc/meminfo, and its CPUs.
type hostMemory struct {
	TotalMiB, AvailableMiB int
	CPUs                   int
}

// reservation is admitted room for some VMs, given back as they're made.
// Its counts, like the manager's reserved totals, are guarded by
// Manager.admitMu.
type reservation struct {
	m                        *Manager
	commitMiB, useMiB, vcpus int // per VM
	committed, useLeft       int // VMs whose room is still held
}

// admit checks that count VMs of vcpus and memoryMiB each, each expected to
// use useMiB at first, fit, and holds their room.
//
// Reading the live VMs and reserving happen under admitMu, as does each
// VM's record being created and its reservation given back (record), so
// every admission sees each VM in exactly one place: reserved, or recorded.
func (m *Manager) admit(ctx context.Context, count, vcpus, memoryMiB, useMiB int) (*reservation, error) {
	host, err := m.host.hostMemory()
	if err != nil {
		return nil, fmt.Errorf("admission: %w", err)
	}
	m.admitMu.Lock()
	defer m.admitMu.Unlock()
	live, err := m.store.ListVMs(ctx, state.ListOptions{})
	if err != nil {
		return nil, err
	}
	if total := len(live) + m.reservedVMs + count; total > m.cfg.MaxVMs {
		return nil, fmt.Errorf("%w: %d more VMs would make %d, more than the limit of %d; there are %d now",
			ErrCapacity, count, total, m.cfg.MaxVMs, len(live)+m.reservedVMs)
	}
	committed, liveVCPUs := 0, 0
	for _, vm := range live {
		committed += vm.MemoryMiB
		liveVCPUs += vm.VCPUs
	}
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
	cpuLimit := int(m.cfg.CPUOvercommitRatio * float64(host.CPUs))
	if total := liveVCPUs + m.reservedVCPUs + count*vcpus; total > cpuLimit {
		return nil, fmt.Errorf("%w: %d more VMs of %d vCPUs would make %d vCPUs, more than the limit of %d "+
			"(%.0f per host CPU, of %d); the VMs there now have %d",
			ErrNoCPU, count, vcpus, total, cpuLimit, m.cfg.CPUOvercommitRatio, host.CPUs, liveVCPUs+m.reservedVCPUs)
	}
	m.reservedCommitMiB += count * memoryMiB
	m.reservedUseMiB += count * useMiB
	m.reservedVCPUs += count * vcpus
	m.reservedVMs += count
	return &reservation{m: m, commitMiB: memoryMiB, useMiB: useMiB, vcpus: vcpus, committed: count, useLeft: count}, nil
}

// record creates one of the reservation's VMs' records with create and, if
// that works, gives back its guest memory, vCPUs and place in the count,
// which admission counts from the record from then on. Both happen at once, as far as admission
// can tell.
func (r *reservation) record(create func() (state.VM, error)) (state.VM, error) {
	r.m.admitMu.Lock()
	defer r.m.admitMu.Unlock()
	vm, err := create()
	if err == nil && r.committed > 0 {
		r.committed--
		r.m.reservedCommitMiB -= r.commitMiB
		r.m.reservedVCPUs -= r.vcpus
		r.m.reservedVMs--
	}
	return vm, err
}

// made says one of the reservation's VMs is made, or failed: its expected
// use is in the host's available memory now, or never will be.
func (r *reservation) made() {
	r.m.admitMu.Lock()
	defer r.m.admitMu.Unlock()
	if r.useLeft > 0 {
		r.useLeft--
		r.m.reservedUseMiB -= r.useMiB
	}
}

// release gives back whatever the reservation still holds.
func (r *reservation) release() {
	r.m.admitMu.Lock()
	defer r.m.admitMu.Unlock()
	r.m.reservedCommitMiB -= r.committed * r.commitMiB
	r.m.reservedVCPUs -= r.committed * r.vcpus
	r.m.reservedVMs -= r.committed
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
