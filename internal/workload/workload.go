// Package workload holds what tine's benchmark programs share: memory they
// fill and keep, and running as a small daemon inside a guest, so the
// memory outlives the command that made it (and survives snapshots and
// forks, which is the point).
package workload

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// PageSize is the guest's page size.
const PageSize = 4096

// Alloc maps mb MiB of anonymous memory. Nothing is in it until it's touched.
func Alloc(mb int) ([]byte, error) {
	if mb <= 0 {
		return nil, errors.New("memory size must be positive")
	}
	return syscall.Mmap(-1, 0, mb<<20, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_ANON|syscall.MAP_PRIVATE)
}

// Fill writes to every page of mem: a different, non-zero value on each, so
// the kernel can neither leave a page as the shared zero page nor merge two
// pages that are alike (KSM).
func Fill(mem []byte, seed uint64) {
	x := seed | 1
	for off := 0; off < len(mem); off += PageSize {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		page := mem[off : off+PageSize]
		for i := 0; i < PageSize; i += 512 {
			page[i] = byte(x>>(i%56)) | 1 //nolint:gosec // keeping only the low byte is the point
		}
	}
}

// Result is how long touching each page of some memory took.
type Result struct {
	Mode      string  `json:"mode"` // "write" or "read"
	Pages     int     `json:"pages"`
	TotalMs   float64 `json:"total_ms"`
	NsPerPage float64 `json:"ns_per_page"`
	P50Ns     float64 `json:"p50_ns"`
	P99Ns     float64 `json:"p99_ns"`
	MaxNs     float64 `json:"max_ns"`
}

// Touch reads or writes one byte of every page of mem, timing each.
// Writing a page a forked VM shares with its parent makes the kernel copy it
// first; reading one only maps it.
func Touch(mem []byte, write bool) Result {
	per := make([]float64, 0, len(mem)/PageSize)
	var sink byte
	start := time.Now()
	for off := 0; off < len(mem); off += PageSize {
		t := time.Now()
		if write {
			mem[off]++
		} else {
			sink += mem[off]
		}
		per = append(per, float64(time.Since(t).Nanoseconds()))
	}
	total := time.Since(start)
	_ = sink
	sort.Float64s(per)
	r := Result{Mode: "read", Pages: len(per), TotalMs: float64(total.Microseconds()) / 1000}
	if write {
		r.Mode = "write"
	}
	if len(per) > 0 {
		r.NsPerPage = float64(total.Nanoseconds()) / float64(len(per))
		r.P50Ns = per[len(per)/2]
		r.P99Ns = per[int(float64(len(per)-1)*0.99)]
		r.MaxNs = per[len(per)-1]
	}
	return r
}

// Daemon runs this program again with args, in a session of its own (so
// tine-agent's end of the command doesn't kill it), and waits until it
// writes readyFile, or gives up after timeout.
func Daemon(readyFile string, timeout time.Duration, args ...string) error {
	_ = os.Remove(readyFile)
	self, err := os.Executable()
	if err != nil {
		return err
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer func() { _ = devnull.Close() }()
	cmd := exec.Command(self, args...) //nolint:gosec,noctx // this program, on purpose, outliving the call
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, devnull
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(readyFile); err == nil {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("the daemon didn't get ready within %v", timeout)
}

// WriteJSON writes v to path atomically.
func WriteJSON(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil { //nolint:gosec // results are meant to be read
		return err
	}
	return os.Rename(tmp, path)
}

// WritePID writes this process's ID to path.
func WritePID(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // a directory under /run
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o644) //nolint:gosec // a PID isn't secret
}

// ReadPID reads a PID written by WritePID.
func ReadPID(path string) (int, error) {
	data, err := os.ReadFile(path) //nolint:gosec // a path the program chose
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}
