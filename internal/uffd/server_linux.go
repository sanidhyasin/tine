//go:build linux

package uffd

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The userfaultfd ioctls and events this needs (linux/userfaultfd.h, for
// 64-bit hosts). x/sys/unix doesn't have them.
const (
	uffdioAPI      = 0xC018AA3F // _IOWR(0xAA, 0x3F, struct uffdio_api)
	uffdioRegister = 0xC020AA00 // _IOWR(0xAA, 0x00, struct uffdio_register)
	uffdioWake     = 0x8010AA02 // _IOR(0xAA, 0x02, struct uffdio_range)
	uffdioCopy     = 0xC028AA03 // _IOWR(0xAA, 0x03, struct uffdio_copy)
	uffdioZeropage = 0xC020AA04 // _IOWR(0xAA, 0x04, struct uffdio_zeropage)

	eventPagefault = 0x12
	eventRemove    = 0x15
	eventUnmap     = 0x16

	msgSize = 32 // sizeof(struct uffd_msg)
)

// prefetchChunk is the most Prefetch copies in one ioctl.
const prefetchChunk = 4 << 20

type uffdioRange struct{ Start, Len uint64 }

type uffdioCopyArg struct {
	Dst, Src, Len, Mode uint64
	Copy                int64
}

type uffdioZeropageArg struct {
	Range    uffdioRange
	Mode     uint64
	Zeropage int64
}

func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	_, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
	if e != 0 {
		return e
	}
	return nil
}

// Region is one guest memory region, as Firecracker describes it in the
// handshake: where it is in Firecracker's address space, and where its
// contents are in the memory file.
type Region struct {
	BaseHostVirtAddr uint64 `json:"base_host_virt_addr"`
	Size             uint64 `json:"size"`
	Offset           uint64 `json:"offset"`
	// PageSize is in bytes: 4096, or 2 MiB with huge pages.
	PageSize uint64 `json:"page_size"`
}

// Receive reads Firecracker's handshake from a connection on the backend
// socket: a JSON array of the guest memory regions, with the userfaultfd
// attached. Firecracker sends nothing else, and expects no reply.
func Receive(conn *net.UnixConn, timeout time.Duration) ([]Region, int, error) {
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return nil, -1, err
	}
	buf := make([]byte, 64<<10)
	oob := make([]byte, unix.CmsgSpace(4))
	var data []byte
	fd := -1
	for fd < 0 || !json.Valid(data) {
		n, oobn, _, _, err := conn.ReadMsgUnix(buf, oob)
		if oobn > 0 && fd < 0 {
			fd, err = unixRights(oob[:oobn])
			if err != nil {
				return nil, -1, err
			}
		}
		data = append(data, buf[:n]...)
		if err != nil || (n == 0 && oobn == 0) {
			if fd >= 0 {
				_ = unix.Close(fd)
			}
			return nil, -1, fmt.Errorf("handshake: %w (after %d bytes)", errOrEOF(err), len(data))
		}
	}
	_ = conn.SetReadDeadline(time.Time{})
	var regions []Region
	if err := json.Unmarshal(data, &regions); err != nil {
		_ = unix.Close(fd)
		return nil, -1, fmt.Errorf("handshake: %w", err)
	}
	if err := checkRegions(regions); err != nil {
		_ = unix.Close(fd)
		return nil, -1, err
	}
	return regions, fd, nil
}

func errOrEOF(err error) error {
	if err == nil {
		return errors.New("connection closed")
	}
	return err
}

func unixRights(oob []byte) (int, error) {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return -1, fmt.Errorf("handshake: %w", err)
	}
	for _, m := range msgs {
		fds, err := unix.ParseUnixRights(&m)
		if err != nil {
			continue
		}
		for _, extra := range fds[1:] {
			_ = unix.Close(extra)
		}
		if len(fds) > 0 {
			return fds[0], nil
		}
	}
	return -1, errors.New("handshake: no file descriptor attached")
}

func checkRegions(regions []Region) error {
	if len(regions) == 0 {
		return errors.New("handshake: no memory regions")
	}
	ps := regions[0].PageSize
	if ps < 4096 || ps&(ps-1) != 0 {
		return fmt.Errorf("handshake: bad page size %d", ps)
	}
	for _, r := range regions {
		if r.PageSize != ps {
			return errors.New("handshake: regions with different page sizes")
		}
		if r.Size == 0 || r.Size%ps != 0 || r.BaseHostVirtAddr%ps != 0 || r.Offset%ps != 0 {
			return fmt.Errorf("handshake: region %+v isn't page-aligned", r)
		}
	}
	return nil
}

// Stats are what a Server has done.
type Stats struct {
	// Faults are page faults served; Copied, the pages copied in for them
	// (more than one each with a cluster).
	Faults int64 `json:"faults"`
	Copied int64 `json:"copied_pages"`
	// Prefetched pages were copied in before they were asked for.
	Prefetched int64   `json:"prefetched_pages"`
	PrefetchMs float64 `json:"prefetch_ms,omitempty"`
	// Zeroed pages were faulted after the guest gave them back (balloon).
	Zeroed   int64  `json:"zeroed_pages"`
	PageSize uint64 `json:"page_size"`
}

// Server serves one VM's page faults from its snapshot's memory file.
type Server struct {
	uffd     int
	regions  []Region
	mem      []byte // the memory file, mapped read-only and shared: one copy in the page cache for every VM
	pageSize uint64
	cluster  uint64 // pages copied per fault

	mu      sync.Mutex
	touched *WorkingSet // file pages the guest faulted on
	removed *WorkingSet // file pages the guest gave back

	faults, copied, prefetched, zeroed atomic.Int64
	prefetchNs                         atomic.Int64
	zeroBuf                            []byte // a zero page, for huge pages
}

// NewServer serves the regions from mem, through the userfaultfd uffd,
// copying cluster pages, aligned, for each fault (1 for just the page).
func NewServer(uffd int, regions []Region, mem *os.File, cluster int) (*Server, error) {
	if err := checkRegions(regions); err != nil {
		return nil, err
	}
	var size uint64
	for _, r := range regions {
		size = max(size, r.Offset+r.Size)
	}
	fi, err := mem.Stat()
	if err != nil {
		return nil, err
	}
	if uint64(fi.Size()) < size { //nolint:gosec // a file size is never negative
		return nil, fmt.Errorf("the memory file is %d bytes, and the guest's memory %d", fi.Size(), size)
	}
	data, err := unix.Mmap(int(mem.Fd()), 0, int(size), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("map the memory file: %w", err)
	}
	ps := regions[0].PageSize
	return &Server{
		uffd: uffd, regions: regions, mem: data, pageSize: ps, cluster: uint64(max(cluster, 1)),
		touched: NewWorkingSet(size, ps), removed: NewWorkingSet(size, ps),
	}, nil
}

// Close unmaps the memory file and closes the userfaultfd.
func (s *Server) Close() error {
	err := unix.Munmap(s.mem)
	if closeErr := unix.Close(s.uffd); err == nil {
		err = closeErr
	}
	return err
}

// Stats says what the server has done so far.
func (s *Server) Stats() Stats {
	return Stats{
		Faults: s.faults.Load(), Copied: s.copied.Load(), Prefetched: s.prefetched.Load(), Zeroed: s.zeroed.Load(),
		PrefetchMs: float64(s.prefetchNs.Load()) / 1e6, PageSize: s.pageSize,
	}
}

// WorkingSet is the pages the guest has faulted on so far.
func (s *Server) WorkingSet() *WorkingSet {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.touched.Clone()
}

// Serve handles faults until hangup, a file descriptor (Firecracker's end
// of the handshake connection), is closed or readable: Firecracker keeps it
// open, and sends nothing on it, until it exits. A negative hangup is
// never watched.
func (s *Server) Serve(hangup int) error {
	fds := []unix.PollFd{{Fd: int32(s.uffd), Events: unix.POLLIN}, {Fd: int32(hangup), Events: unix.POLLIN}} //nolint:gosec // fds fit
	buf := make([]byte, msgSize*64)
	for {
		fds[0].Revents, fds[1].Revents = 0, 0
		if _, err := unix.Poll(fds, -1); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return fmt.Errorf("poll: %w", err)
		}
		if fds[1].Revents != 0 {
			return nil // Firecracker is gone
		}
		if fds[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
			return nil
		}
		n, err := unix.Read(s.uffd, buf)
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read userfaultfd: %w", err)
		}
		for off := 0; off+msgSize <= n; off += msgSize {
			if err := s.handle(buf[off : off+msgSize]); err != nil {
				return err
			}
		}
	}
}

func (s *Server) handle(msg []byte) error {
	switch msg[0] {
	case eventPagefault:
		return s.fault(binary.LittleEndian.Uint64(msg[16:24]))
	case eventRemove, eventUnmap:
		// The guest gave pages back (the balloon): they're zero from now on,
		// not what the file says.
		s.remove(binary.LittleEndian.Uint64(msg[8:16]), binary.LittleEndian.Uint64(msg[16:24]))
	}
	return nil
}

// region finds the region holding addr.
func (s *Server) region(addr uint64) (Region, bool) {
	for _, r := range s.regions {
		if addr >= r.BaseHostVirtAddr && addr < r.BaseHostVirtAddr+r.Size {
			return r, true
		}
	}
	return Region{}, false
}

func (s *Server) fault(addr uint64) error {
	s.faults.Add(1)
	page := addr &^ (s.pageSize - 1)
	r, ok := s.region(page)
	if !ok {
		return fmt.Errorf("a fault at %#x, outside guest memory", addr)
	}
	filePage := (r.Offset + page - r.BaseHostVirtAddr) / s.pageSize
	s.mu.Lock()
	s.touched.Add(filePage)
	removed := s.removed.Has(filePage)
	s.mu.Unlock()
	if removed {
		return s.zero(page)
	}
	// The cluster of pages around it, within the region.
	span := s.cluster * s.pageSize
	start := max(r.BaseHostVirtAddr+(page-r.BaseHostVirtAddr)/span*span, r.BaseHostVirtAddr)
	end := min(start+span, r.BaseHostVirtAddr+r.Size)
	n, err := s.copyIn(r, start, end, page)
	s.copied.Add(n)
	return err
}

// copyIn copies the file's pages for [start, end) of region r into the
// guest, skipping pages that are there already, and returns how many it
// copied. If must, a page in the range, was there already, the threads
// waiting for it are woken: whoever copied it may not have.
func (s *Server) copyIn(r Region, start, end, must uint64) (int64, error) {
	var copied int64
	for p, tries := start, 0; p < end; {
		arg := uffdioCopyArg{Dst: p, Src: uint64(uintptr(unsafe.Pointer(&s.mem[r.Offset+p-r.BaseHostVirtAddr]))), Len: end - p} //nolint:gosec // G103: the kernel copies from our mapping of the file
		err := ioctl(s.uffd, uffdioCopy, unsafe.Pointer(&arg))                                                                  //nolint:gosec // G103: as above
		if arg.Copy > 0 {
			copied += arg.Copy / int64(s.pageSize) //nolint:gosec // page sizes are small
			p += uint64(arg.Copy)
			tries = 0
		}
		switch {
		case err == nil || (errors.Is(err, unix.EAGAIN) && arg.Copy > 0):
			// Done, or partly: carry on from where it stopped.
		case errors.Is(err, unix.EEXIST):
			if p == must {
				wake := uffdioRange{Start: p, Len: s.pageSize}
				_ = ioctl(s.uffd, uffdioWake, unsafe.Pointer(&wake)) //nolint:gosec // G103: an ioctl argument
			}
			p += s.pageSize
		case errors.Is(err, unix.EAGAIN) && tries < 1000:
			// The guest's memory map is changing (the balloon, say): try again.
			tries++
			time.Sleep(50 * time.Microsecond)
		case errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ESRCH):
			return copied, nil // Firecracker is exiting
		default:
			return copied, fmt.Errorf("copy %#x+%d: %w", p, end-p, err)
		}
	}
	return copied, nil
}

// zero maps a zero page at page.
func (s *Server) zero(page uint64) error {
	s.zeroed.Add(1)
	if s.pageSize == 4096 {
		arg := uffdioZeropageArg{Range: uffdioRange{Start: page, Len: s.pageSize}}
		err := ioctl(s.uffd, uffdioZeropage, unsafe.Pointer(&arg)) //nolint:gosec // G103: an ioctl argument
		if err == nil || errors.Is(err, unix.EEXIST) || errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("zero page %#x: %w", page, err)
	}
	// Huge pages have no zero page: copy one in.
	if s.zeroBuf == nil {
		buf, err := unix.Mmap(-1, 0, int(s.pageSize), unix.PROT_READ, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS) //nolint:gosec // a page size fits
		if err != nil {
			return err
		}
		s.zeroBuf = buf
	}
	arg := uffdioCopyArg{Dst: page, Src: uint64(uintptr(unsafe.Pointer(&s.zeroBuf[0]))), Len: s.pageSize}   //nolint:gosec // G103: an ioctl argument
	if err := ioctl(s.uffd, uffdioCopy, unsafe.Pointer(&arg)); err != nil && !errors.Is(err, unix.EEXIST) { //nolint:gosec // G103: as above
		return fmt.Errorf("zero huge page %#x: %w", page, err)
	}
	return nil
}

func (s *Server) remove(start, end uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for p := (start + s.pageSize - 1) &^ (s.pageSize - 1); p+s.pageSize <= end; p += s.pageSize {
		if r, ok := s.region(p); ok {
			s.removed.Add((r.Offset + p - r.BaseHostVirtAddr) / s.pageSize)
		}
	}
}

// Prefetch copies the pages of ws into the guest before it asks for them.
// It can run while Serve does: whichever copies a page first wins.
func (s *Server) Prefetch(ws *WorkingSet) error {
	if ws.PageSize != s.pageSize {
		return fmt.Errorf("the working set has %d-byte pages, the guest %d", ws.PageSize, s.pageSize)
	}
	start := time.Now()
	defer func() { s.prefetchNs.Add(time.Since(start).Nanoseconds()) }()
	var firstErr error
	for _, r := range s.regions {
		first, last := r.Offset/s.pageSize, (r.Offset+r.Size)/s.pageSize
		ws.Runs(func(page, n uint64) {
			// The part of the run in this region, minus what the guest gave back.
			lo, hi := max(page, first), min(page+n, last)
			for lo < hi && firstErr == nil {
				s.mu.Lock()
				for lo < hi && s.removed.Has(lo) {
					lo++
				}
				end := lo
				for end < hi && !s.removed.Has(end) {
					end++
				}
				s.mu.Unlock()
				// A few MiB at a time, so the guest's own faults get a turn.
				end = min(end, lo+max(prefetchChunk/s.pageSize, 1))
				if end > lo {
					base := r.BaseHostVirtAddr + lo*s.pageSize - r.Offset
					got, err := s.copyIn(r, base, base+(end-lo)*s.pageSize, ^uint64(0))
					s.prefetched.Add(got)
					if err != nil {
						firstErr = err
					}
				}
				lo = end
			}
		})
	}
	return firstErr
}
