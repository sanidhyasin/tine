package agent

import (
	"encoding/binary"
	"fmt"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func setClock(t time.Time) error {
	ts := unix.NsecToTimespec(t.UnixNano())
	if err := unix.ClockSettime(unix.CLOCK_REALTIME, &ts); err != nil {
		return fmt.Errorf("clock_settime: %w", err)
	}
	return nil
}

// addEntropy credits b to the kernel's entropy pool (RNDADDENTROPY, which a
// plain write to /dev/urandom doesn't do) and then reseeds the generator at
// once (RNDRESEEDCRNG), rather than at its next scheduled reseed.
func addEntropy(b []byte) error {
	f, err := os.OpenFile("/dev/urandom", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	// struct rand_pool_info { int entropy_count; int buf_size; __u32 buf[]; }
	info := make([]byte, 8+len(b))
	binary.NativeEndian.PutUint32(info[0:], uint32(len(b)*8)) //nolint:gosec // bounded by the request size limit
	binary.NativeEndian.PutUint32(info[4:], uint32(len(b)))   //nolint:gosec // as above
	copy(info[8:], b)
	// The pointer is converted in the call itself, so info stays alive for it.
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), unix.RNDADDENTROPY, uintptr(unsafe.Pointer(&info[0]))); errno != 0 { //nolint:gosec // the ioctl takes a pointer to rand_pool_info, laid out above
		return fmt.Errorf("RNDADDENTROPY: %w", errno)
	}
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), unix.RNDRESEEDCRNG, 0); errno != 0 {
		return fmt.Errorf("RNDRESEEDCRNG: %w", errno)
	}
	return nil
}

func sethostname(name string) error { return unix.Sethostname([]byte(name)) }
