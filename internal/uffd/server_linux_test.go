//go:build linux

package uffd

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

const userModeOnly = 1 // UFFD_USER_MODE_ONLY: enough for faults from this test's own code

// guest is anonymous memory registered with a userfaultfd, as Firecracker
// sets up a VM's memory, but in this process.
func guest(t *testing.T, size int) ([]byte, int) {
	t.Helper()
	fd, _, e := unix.Syscall(unix.SYS_USERFAULTFD, unix.O_CLOEXEC|unix.O_NONBLOCK|userModeOnly, 0, 0)
	if e != 0 {
		t.Skipf("no userfaultfd here: %v", e)
	}
	api := struct{ API, Features, Ioctls uint64 }{API: 0xAA, Features: 1 << 3} // UFFD_FEATURE_EVENT_REMOVE
	if err := ioctl(int(fd), uffdioAPI, unsafe.Pointer(&api)); err != nil {    //nolint:gosec // G103: an ioctl argument
		t.Skipf("UFFDIO_API: %v", err)
	}
	mem, err := unix.Mmap(-1, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		t.Fatal(err)
	}
	reg := struct {
		Range        uffdioRange
		Mode, Ioctls uint64
	}{Range: uffdioRange{Start: uint64(uintptr(unsafe.Pointer(&mem[0]))), Len: uint64(size)}, Mode: 1} //nolint:gosec // G103: the guest's address; mode MISSING
	if err := ioctl(int(fd), uffdioRegister, unsafe.Pointer(&reg)); err != nil { //nolint:gosec // G103: an ioctl argument
		t.Fatalf("UFFDIO_REGISTER: %v", err)
	}
	t.Cleanup(func() { _ = unix.Munmap(mem) })
	return mem, int(fd)
}

// handshake sends regions and fd over a socket pair, as Firecracker does,
// and receives them as tine-uffd does.
func handshake(t *testing.T, regions []Region, fd int) ([]Region, int, *net.UnixConn) {
	t.Helper()
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	firecracker := os.NewFile(uintptr(pair[0]), "firecracker")
	t.Cleanup(func() { _ = firecracker.Close() })
	server, err := net.FileConn(os.NewFile(uintptr(pair[1]), "server"))
	if err != nil {
		t.Fatal(err)
	}
	conn := server.(*net.UnixConn)
	t.Cleanup(func() { _ = conn.Close() })
	body, _ := json.Marshal(regions)
	if err := unix.Sendmsg(pair[0], body, unix.UnixRights(fd), nil, 0); err != nil {
		t.Fatal(err)
	}
	got, uffd, err := Receive(conn, 5*time.Second)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	return got, uffd, conn
}

func TestServer(t *testing.T) {
	const size, page = 4 << 20, 4096
	runtime.GOMAXPROCS(max(runtime.GOMAXPROCS(0), 2))
	contents := make([]byte, size)
	for i := range contents {
		contents[i] = byte(i/page + i%251)
	}
	path := filepath.Join(t.TempDir(), "memory")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path) //nolint:gosec // a test file
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()

	mem, fd := guest(t, size)
	base := uint64(uintptr(unsafe.Pointer(&mem[0]))) //nolint:gosec // G103: the guest memory's address, as Firecracker sends it
	// Two regions, the second from the middle of the file, like a guest with
	// memory above the 32-bit gap.
	sent := []Region{
		{BaseHostVirtAddr: base, Size: size / 2, Offset: 0, PageSize: page},
		{BaseHostVirtAddr: base + size/2, Size: size / 2, Offset: size / 2, PageSize: page},
	}
	regions, uffd, conn := handshake(t, sent, fd)
	_ = unix.Close(fd) // the server has its own copy now, as with Firecracker
	if len(regions) != 2 || regions[1] != sent[1] {
		t.Fatalf("regions = %+v", regions)
	}
	s, err := NewServer(uffd, regions, file, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	_ = conn
	served := make(chan error, 1)
	other, _ := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	go func() { served <- s.Serve(other[1]) }()

	ws := NewWorkingSet(size, page)
	for i := uint64(100); i < 110; i++ {
		ws.Add(i)
	}
	if err := s.Prefetch(ws); err != nil {
		t.Fatalf("Prefetch: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		runtime.LockOSThread()
		// Pages from both regions, prefetched and not: each must read as the file does.
		for _, p := range []int{0, 1, 7, 100, 105, 600, 1023} {
			if got := mem[p*page : (p+1)*page]; !bytes.Equal(got, contents[p*page:(p+1)*page]) {
				t.Errorf("page %d differs from the file", p)
			}
		}
		mem[3*page] = 0xFF // writable, and the guest's own
		// Giving pages back (MADV_DONTNEED, as the balloon does) makes them zero.
		if err := unix.Madvise(mem[8*page:12*page], unix.MADV_DONTNEED); err != nil {
			t.Errorf("madvise: %v", err)
			return
		}
		if got := mem[9*page : 10*page]; !bytes.Equal(got, make([]byte, page)) {
			t.Error("a page given back doesn't read as zero")
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the guest's faults weren't served")
	}

	st := s.Stats()
	if st.Prefetched != 10 || st.Faults < 4 || st.Zeroed < 1 || st.Copied < 4 {
		t.Errorf("stats = %+v", st)
	}
	got := s.WorkingSet()
	for _, p := range []uint64{0, 7, 600, 1023, 9} {
		if !got.Has(p) {
			t.Errorf("page %d isn't in the working set", p)
		}
	}
	if got.Has(105) || got.Has(500) {
		t.Error("pages the guest didn't fault on are in the working set")
	}

	// Firecracker exiting ends Serve.
	_ = unix.Close(other[0])
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve didn't return once its peer was gone")
	}
}

func TestReceiveRefusesBadHandshakes(t *testing.T) {
	for name, regions := range map[string][]Region{
		"none":         {},
		"unaligned":    {{BaseHostVirtAddr: 4096, Size: 100, PageSize: 4096}},
		"bad page":     {{BaseHostVirtAddr: 0, Size: 4096, PageSize: 3000}},
		"mixed pages":  {{Size: 4096, PageSize: 4096}, {Size: 2 << 20, PageSize: 2 << 20}},
		"unaligned at": {{BaseHostVirtAddr: 0, Size: 8192, Offset: 10, PageSize: 4096}},
	} {
		if err := checkRegions(regions); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// No file descriptor attached.
	pair, _ := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	c, _ := net.FileConn(os.NewFile(uintptr(pair[1]), "server"))
	defer func() { _ = c.Close() }()
	_, _ = unix.Write(pair[0], []byte(`[{"base_host_virt_addr":0,"size":4096,"offset":0,"page_size":4096}]`))
	_ = unix.Close(pair[0])
	if _, _, err := Receive(c.(*net.UnixConn), time.Second); err == nil {
		t.Error("a handshake without a userfaultfd was accepted")
	} else if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("waited for the deadline instead of seeing the hangup: %v", err)
	}
}
