// Package idalloc hands out numbers from a fixed range, such as the uid each
// VM's Firecracker process runs as, or each VM's network slot.
package idalloc

import (
	"errors"
	"fmt"
	"sync"
)

// ErrExhausted means every number in the range is in use.
var ErrExhausted = errors.New("no free numbers left")

// Range allocates numbers in [First, First+Size). It's safe for concurrent use.
type Range struct {
	first, size int

	mu   sync.Mutex
	used map[int]bool
	next int // where the next search starts, so freed numbers aren't reused straight away
}

// NewRange returns an allocator for [first, first+size).
func NewRange(first, size int) (*Range, error) {
	if size <= 0 {
		return nil, fmt.Errorf("idalloc: size must be positive, got %d", size)
	}
	if first < 0 || first > int(^uint32(0))-size {
		return nil, fmt.Errorf("idalloc: range [%d, %d) is out of bounds", first, first+size)
	}
	return &Range{first: first, size: size, used: map[int]bool{}}, nil
}

// Allocate returns a free number and marks it used.
//
// It searches round-robin from just after the last number handed out, so a
// number that was just released isn't immediately given to someone else.
// That makes it less likely that leftovers of a destroyed VM get mistaken for
// a new one.
func (r *Range) Allocate() (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := 0; i < r.size; i++ {
		n := r.first + (r.next+i)%r.size
		if !r.used[n] {
			r.used[n] = true
			r.next = (n - r.first + 1) % r.size
			return n, nil
		}
	}
	return 0, fmt.Errorf("idalloc: all %d numbers from %d are in use: %w", r.size, r.first, ErrExhausted)
}

// Reserve marks a specific number as used, for example one found on an
// existing VM when tined restarts.
func (r *Range) Reserve(n int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.contains(n) {
		return fmt.Errorf("idalloc: %d is outside [%d, %d)", n, r.first, r.first+r.size)
	}
	if r.used[n] {
		return fmt.Errorf("idalloc: %d is already in use", n)
	}
	r.used[n] = true
	return nil
}

// Release returns a number to the pool. Releasing a free number does nothing.
func (r *Range) Release(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.used, n)
}

// InUse reports how many numbers are allocated.
func (r *Range) InUse() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.used)
}

func (r *Range) contains(n int) bool { return n >= r.first && n < r.first+r.size }
