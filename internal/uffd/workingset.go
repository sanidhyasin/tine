// Package uffd serves a Firecracker VM's memory from a snapshot's memory
// file through userfaultfd, Firecracker's Uffd memory backend: the guest's
// memory starts empty, each page it touches faults, and the page server
// copies the page in from the file. That costs a trip through the page
// server per page, but lets it copy pages in before they're asked for: a
// working set recorded on an earlier restore, all at once.
//
// cmd/tine-uffd runs a Server for one VM. This file, the working-set
// format, is portable; the rest is Linux only.
package uffd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
)

// WorkingSet is a set of pages of a memory file: those a VM touched while
// it was being restored, say. Page i is the file's bytes from i*PageSize.
type WorkingSet struct {
	PageSize uint64
	pages    uint64
	bits     []uint64
}

// NewWorkingSet is an empty set over a file of size bytes.
func NewWorkingSet(size, pageSize uint64) *WorkingSet {
	n := (size + pageSize - 1) / pageSize
	return &WorkingSet{PageSize: pageSize, pages: n, bits: make([]uint64, (n+63)/64)}
}

// Pages is how many pages the file has.
func (w *WorkingSet) Pages() uint64 { return w.pages }

// Add puts page i in the set; pages past the end are ignored.
func (w *WorkingSet) Add(i uint64) {
	if i < w.pages {
		w.bits[i/64] |= 1 << (i % 64)
	}
}

// Has reports whether page i is in the set.
func (w *WorkingSet) Has(i uint64) bool { return i < w.pages && w.bits[i/64]&(1<<(i%64)) != 0 }

// Len is how many pages are in the set.
func (w *WorkingSet) Len() int {
	n := 0
	for _, b := range w.bits {
		n += bits.OnesCount64(b)
	}
	return n
}

// Runs calls fn for each run of consecutive pages in the set, in order.
func (w *WorkingSet) Runs(fn func(first, n uint64)) {
	for i := uint64(0); i < w.pages; {
		if !w.Has(i) {
			i++
			continue
		}
		j := i + 1
		for j < w.pages && w.Has(j) {
			j++
		}
		fn(i, j-i)
		i = j
	}
}

// Clone copies the set.
func (w *WorkingSet) Clone() *WorkingSet {
	return &WorkingSet{PageSize: w.PageSize, pages: w.pages, bits: append([]uint64(nil), w.bits...)}
}

const workingSetMagic = "tine-ws1"

// MarshalBinary encodes the set: a magic string, the page size and the
// number of pages, then the bitmap, all little-endian.
func (w *WorkingSet) MarshalBinary() ([]byte, error) {
	out := make([]byte, 0, len(workingSetMagic)+16+8*len(w.bits))
	out = append(out, workingSetMagic...)
	out = binary.LittleEndian.AppendUint64(out, w.PageSize)
	out = binary.LittleEndian.AppendUint64(out, w.pages)
	for _, b := range w.bits {
		out = binary.LittleEndian.AppendUint64(out, b)
	}
	return out, nil
}

// UnmarshalBinary decodes a set MarshalBinary encoded.
func (w *WorkingSet) UnmarshalBinary(data []byte) error {
	head := len(workingSetMagic) + 16
	if len(data) < head || string(data[:len(workingSetMagic)]) != workingSetMagic {
		return errors.New("not a tine working set")
	}
	pageSize := binary.LittleEndian.Uint64(data[len(workingSetMagic):])
	pages := binary.LittleEndian.Uint64(data[len(workingSetMagic)+8:])
	if pageSize == 0 || pageSize&(pageSize-1) != 0 {
		return fmt.Errorf("working set: bad page size %d", pageSize)
	}
	words := (pages + 63) / 64
	if uint64(len(data)-head) != 8*words { //nolint:gosec // G115: len(data) >= head, checked above
		return fmt.Errorf("working set: %d bytes of bitmap for %d pages", len(data)-head, pages)
	}
	w.PageSize, w.pages, w.bits = pageSize, pages, make([]uint64, words)
	for i := range w.bits {
		w.bits[i] = binary.LittleEndian.Uint64(data[head+8*i:])
	}
	return nil
}
