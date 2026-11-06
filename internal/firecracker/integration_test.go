//go:build integration

// Integration tests run against a real Firecracker binary, so they need Linux
// with KVM. Run them on the dev server with `make test-integration`.
//
// Paths default to what scripts/host-setup.sh downloads into ~/tine/artifacts.
// Override them with TINE_FIRECRACKER, TINE_TEST_KERNEL and TINE_TEST_ROOTFS.
// Tests skip, rather than fail, when something they need isn't there.

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
	"testing"
	"time"
)

func TestRealFirecrackerBootPauseResume(t *testing.T) {
	bin, kernel, rootfs := requireArtifacts(t)
	dir := tempDir(t)
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

// requireArtifacts returns the Firecracker binary, kernel and rootfs to test
// with, or skips the test if they aren't available.
func requireArtifacts(t *testing.T) (bin, kernel, rootfs string) {
	t.Helper()
	home, _ := os.UserHomeDir()
	artifacts := filepath.Join(home, "tine", "artifacts")

	bin = envOr("TINE_FIRECRACKER", filepath.Join(artifacts, "bin", "firecracker"))
	rootfs = envOr("TINE_TEST_ROOTFS", filepath.Join(artifacts, "rootfs.ext4"))
	kernel = os.Getenv("TINE_TEST_KERNEL")
	if kernel == "" {
		matches, _ := filepath.Glob(filepath.Join(artifacts, "vmlinux-*"))
		sort.Strings(matches)
		if len(matches) > 0 {
			kernel = matches[len(matches)-1]
		}
	}

	for _, p := range []string{bin, kernel, rootfs} {
		if p == "" {
			t.Skip("no guest kernel found; run scripts/host-setup.sh or set TINE_TEST_KERNEL")
		}
		if _, err := os.Stat(p); err != nil { //nolint:gosec // paths come from the test's own environment variables

			t.Skipf("%v (run scripts/host-setup.sh, or set TINE_FIRECRACKER / TINE_TEST_KERNEL / TINE_TEST_ROOTFS)", err)
		}
	}
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("can't use KVM: %v", err)
	}
	_ = f.Close()
	return bin, kernel, rootfs
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// tempDir is a short temporary directory, because socket paths are limited to about 100 bytes.
func tempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "fcit")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
