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
	"sync"
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
