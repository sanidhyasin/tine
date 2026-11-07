//go:build integration

// Integration tests run against a real Firecracker binary, so they need Linux
// with KVM. Run them on the dev server with `make test-integration`.
//
// They use what scripts/host-setup.sh downloads (see internal/testenv), and
// skip, rather than fail, when something they need isn't there.

package firecracker

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sanidhyasin/tine/internal/testenv"
)

func TestRealFirecrackerBootPauseResume(t *testing.T) {
	testenv.RequireKVM(t)
	a := testenv.RequireArtifacts(t)
	bin, kernel, rootfs := a.Firecracker, a.Kernel, a.Rootfs
	dir := testenv.TempDir(t, "")
	socket := filepath.Join(dir, "api.sock")

	// Firecracker prints the guest's serial console on stdout. Cancelling
	// procCtx kills the process.
	var console bytes.Buffer
	procCtx, stop := context.WithCancel(context.Background())
	cmd := exec.CommandContext(procCtx, bin, "--api-sock", socket, "--enable-pci") //nolint:gosec // the binary path comes from the test's own config
	cmd.Stdout, cmd.Stderr = &console, &console
	if err := cmd.Start(); err != nil {
		stop()
		t.Fatalf("start firecracker: %v", err)
	}
	t.Cleanup(func() {
		stop()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("firecracker output:\n%s", console.String())
		}
	})

	var (
		mu    sync.Mutex
		calls []CallInfo
	)
	c := New(socket, WithObserver(func(ci CallInfo) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, ci)
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	wantState := func(want InstanceState) {
		t.Helper()
		info, err := c.DescribeInstance(ctx)
		must("DescribeInstance", err)
		if info.State != want {
			t.Fatalf("state = %q, want %q", info.State, want)
		}
	}

	must("WaitReady", c.WaitReady(ctx, 5*time.Millisecond))

	version, err := c.Version(ctx)
	must("Version", err)
	t.Logf("Firecracker %s", version)
	wantState(StateNotStarted)

	logPath := filepath.Join(dir, "firecracker.log")
	must("create log file", os.WriteFile(logPath, nil, 0o600))
	must("PutLogger", c.PutLogger(ctx, Logger{LogPath: logPath, Level: LogLevelInfo}))

	want := MachineConfig{VCPUCount: 2, MemSizeMiB: 256, TrackDirtyPages: true}
	must("PutMachineConfig", c.PutMachineConfig(ctx, want))
	got, err := c.GetMachineConfig(ctx)
	must("GetMachineConfig", err)
	if got.VCPUCount != want.VCPUCount || got.MemSizeMiB != want.MemSizeMiB || !got.TrackDirtyPages {
		t.Errorf("GetMachineConfig = %+v, want vCPUs, memory and dirty tracking from %+v", got, want)
	}

	must("PutBootSource", c.PutBootSource(ctx, BootSource{KernelImagePath: kernel, BootArgs: "console=ttyS0 reboot=k panic=1"}))
	// Read-only, so the shared test image is never changed. The guest won't
	// boot cleanly from it, but the VM itself runs, which is all this test needs.
	must("PutDrive", c.PutDrive(ctx, Drive{DriveID: "rootfs", PathOnHost: rootfs, IsRootDevice: true, IsReadOnly: true}))
	must("PutVsock", c.PutVsock(ctx, Vsock{GuestCID: 3, UDSPath: filepath.Join(dir, "vsock.sock")}))
	must("PutEntropy", c.PutEntropy(ctx, EntropyDevice{}))

	must("StartInstance", c.StartInstance(ctx))
	wantState(StateRunning)

	must("Pause", c.Pause(ctx))
	wantState(StatePaused)
	must("Resume", c.Resume(ctx))
	wantState(StateRunning)
	must("Resume while running", c.Resume(ctx)) // documented as a no-op

	// Real Firecracker refuses boot-time configuration after start: check that
	// the error comes back as an APIError with Firecracker's explanation.
	err = c.PutMachineConfig(ctx, MachineConfig{VCPUCount: 1, MemSizeMiB: 128})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest || apiErr.FaultMessage == "" {
		t.Errorf("PutMachineConfig after start = %v, want a 400 APIError with a fault message", err)
	} else {
		t.Logf("expected refusal: %s", apiErr.FaultMessage)
	}

	must("SendCtrlAltDel", c.SendCtrlAltDel(ctx))

	// Report how long each kind of call took, as a first look at real latencies.
	mu.Lock()
	defer mu.Unlock()
	byEndpoint := map[string][]time.Duration{}
	for _, ci := range calls {
		byEndpoint[ci.Endpoint] = append(byEndpoint[ci.Endpoint], ci.Duration)
	}
	endpoints := make([]string, 0, len(byEndpoint))
	for e := range byEndpoint {
		endpoints = append(endpoints, e)
	}
	sort.Strings(endpoints)
	for _, e := range endpoints {
		ds := byEndpoint[e]
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		t.Logf("%-32s calls=%d  median=%v", e, len(ds), ds[len(ds)/2].Round(time.Microsecond))
	}
}

// TestRealSnapshotRoundTrip boots a VM, snapshots it, kills it, and loads the
// snapshot into a new Firecracker process, which must carry on running it.
//
// Both processes run in the same directory and use relative paths, as they
// would inside a jail: a restore needs the disk and the vsock socket at the
// same paths, relative to the new process, as the original used.
func TestRealSnapshotRoundTrip(t *testing.T) {
	testenv.RequireKVM(t)
	a := testenv.RequireArtifacts(t)
	dir := testenv.TempDir(t, "")
	if err := os.Symlink(a.Rootfs, filepath.Join(dir, "rootfs.ext4")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	timed := func(what string, fn func() error) time.Duration {
		t.Helper()
		start := time.Now()
		must(what, fn())
		return time.Since(start)
	}

	// The original VM: boot it, and wait until its kernel has finished
	// booting. Firecracker warns that a snapshot taken earlier can crash on
	// restore.
	first, console := startFirecracker(t, a.Firecracker, dir, "api-1.sock")
	c := New(filepath.Join(dir, "api-1.sock"))
	must("WaitReady", c.WaitReady(ctx, 5*time.Millisecond))
	must("PutMachineConfig", c.PutMachineConfig(ctx, MachineConfig{VCPUCount: 1, MemSizeMiB: 256}))
	must("PutBootSource", c.PutBootSource(ctx, BootSource{KernelImagePath: a.Kernel, BootArgs: "console=ttyS0 reboot=k panic=1"}))
	must("PutDrive", c.PutDrive(ctx, Drive{DriveID: "rootfs", PathOnHost: "rootfs.ext4", IsRootDevice: true, IsReadOnly: true}))
	must("PutVsock", c.PutVsock(ctx, Vsock{GuestCID: 3, UDSPath: "vsock.sock"}))
	must("StartInstance", c.StartInstance(ctx))
	waitForOutput(t, console, "Run /sbin/init", 30*time.Second)
	time.Sleep(time.Second)

	pause := timed("Pause", func() error { return c.Pause(ctx) })
	create := timed("CreateSnapshot", func() error {
		return c.CreateSnapshot(ctx, SnapshotCreate{
			SnapshotPath: "vmstate", MemFilePath: "memory", SnapshotType: SnapshotFull, SyncSnapshotFiles: Bool(false),
		})
	})
	info, err := c.DescribeInstance(ctx)
	must("DescribeInstance", err)
	if info.State != StatePaused {
		t.Errorf("after the snapshot the VM is %q, want it still paused", info.State)
	}
	first.kill()

	mem, err := os.Stat(filepath.Join(dir, "memory"))
	must("stat memory", err)
	if mem.Size() != 256<<20 {
		t.Errorf("memory file is %d bytes, want 256 MiB", mem.Size())
	}
	t.Logf("snapshot of a 256 MiB VM: pause %v, create %v; memory file %d MiB, of which %d MiB allocated",
		pause.Round(time.Microsecond), create.Round(time.Millisecond), mem.Size()>>20, allocatedBytes(mem)>>20)

	// The new process binds the vsock socket at the same path, so the old
	// one has to go first, as it would with a fresh jail.
	must("remove the old vsock socket", os.Remove(filepath.Join(dir, "vsock.sock")))
	second, _ := startFirecracker(t, a.Firecracker, dir, "api-2.sock")
	defer second.kill()
	c2 := New(filepath.Join(dir, "api-2.sock"))
	must("WaitReady", c2.WaitReady(ctx, 5*time.Millisecond))
	load := timed("LoadSnapshot", func() error {
		return c2.LoadSnapshot(ctx, SnapshotLoad{
			SnapshotPath: "vmstate",
			MemBackend:   MemoryBackend{BackendType: MemoryBackendFile, BackendPath: "memory"},
			ResumeVM:     true,
		})
	})
	t.Logf("load and resume in a new process: %v", load.Round(time.Microsecond))

	// It must stay up: a guest that crashed on restore would reboot, and
	// with reboot=k, Firecracker would exit.
	time.Sleep(2 * time.Second)
	info, err = c2.DescribeInstance(ctx)
	must("DescribeInstance after the restore", err)
	if info.State != StateRunning {
		t.Errorf("restored VM is %q, want Running", info.State)
	}

	// A configuration call is refused in a process that loaded a snapshot.
	if err := c2.PutMachineConfig(ctx, MachineConfig{VCPUCount: 1, MemSizeMiB: 128}); err == nil {
		t.Error("PutMachineConfig after a load: want a refusal")
	}
}

type fcProcess struct {
	cmd  *exec.Cmd
	stop context.CancelFunc
}

func (p *fcProcess) kill() {
	p.stop()
	_ = p.cmd.Wait()
}

// startFirecracker starts Firecracker in dir with its API on socket (relative
// to dir), and returns it with a buffer of its console output.
func startFirecracker(t *testing.T, bin, dir, socket string) (*fcProcess, *syncBuffer) {
	t.Helper()
	console := &syncBuffer{}
	ctx, stop := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, bin, "--api-sock", socket, "--enable-pci") //nolint:gosec // the binary path comes from the test's own config
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = console, console
	if err := cmd.Start(); err != nil {
		stop()
		t.Fatalf("start firecracker: %v", err)
	}
	p := &fcProcess{cmd: cmd, stop: stop}
	t.Cleanup(func() {
		p.kill()
		if t.Failed() {
			t.Logf("firecracker (%s) output:\n%s", socket, console.String())
		}
	})
	return p, console
}

// syncBuffer is a bytes.Buffer that a process can write to while a test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitForOutput(t *testing.T, b *syncBuffer, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !strings.Contains(b.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("the console never printed %q within %v", want, timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// allocatedBytes is how much disk a file really uses, which for a sparse
// file is less than its size.
func allocatedBytes(fi os.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Blocks * 512
	}
	return fi.Size()
}
