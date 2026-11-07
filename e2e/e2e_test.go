//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sanidhyasin/tine/internal/api"
)

// TestLifecycle50 creates, uses and destroys 50 VMs, five at a time.
func TestLifecycle50(t *testing.T) {
	const total, workers = 50, 5
	var (
		mu                       sync.Mutex
		creates, execs, destroys []time.Duration
		next                     int
		wg                       sync.WaitGroup
	)
	take := func() (int, bool) {
		mu.Lock()
		defer mu.Unlock()
		if next == total {
			return 0, false
		}
		next++
		return next, true
	}
	record := func(list *[]time.Duration, d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		*list = append(*list, d)
	}
	start := time.Now()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i, ok := take()
				if !ok {
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				t0 := time.Now()
				vm, err := tined.CreateVM(ctx, api.CreateVMRequest{VCPUs: 1, MemoryMiB: 512, TTLSeconds: 600})
				cancel()
				if err != nil {
					t.Errorf("VM %d: create: %v", i, err)
					continue
				}
				record(&creates, time.Since(t0))

				t0 = time.Now()
				out, code, err := run(context.Background(), vm.ID, "sh", "-c", fmt.Sprintf("echo %d $(hostname)", i))
				record(&execs, time.Since(t0))
				if err != nil || code != 0 || out != fmt.Sprintf("%d tine\n", i) {
					t.Errorf("VM %d (%s): exec = %q, exit %d, %v", i, vm.ID, out, code, err)
				}

				t0 = time.Now()
				gone, err := tined.DestroyVM(context.Background(), vm.ID)
				record(&destroys, time.Since(t0))
				if err != nil || gone.State != "destroyed" {
					t.Errorf("VM %d (%s): destroy = %s, %v", i, vm.ID, gone.State, err)
				}
				requireGone(t, vm.ID)
			}
		}()
	}
	wg.Wait()
	t.Logf("%d VMs, %d at a time, in %v", total, workers, time.Since(start).Round(time.Second))
	t.Logf("create (through the API, to running): %s", percentiles(creates))
	t.Logf("exec round trip:                      %s", percentiles(execs))
	t.Logf("destroy:                              %s", percentiles(destroys))
}

// TestManyExecsInOneVM runs 20 commands in one VM at once.
func TestManyExecsInOneVM(t *testing.T) {
	vm := create(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, code, err := run(context.Background(), vm.ID, "sh", "-c", fmt.Sprintf("sleep 0.2; echo $((%d * %d))", i, i))
			if err != nil || code != 0 || strings.TrimSpace(out) != strconv.Itoa(i*i) {
				t.Errorf("exec %d = %q, exit %d, %v", i, out, code, err)
			}
		}()
	}
	wg.Wait()
}

// TestDestroyDuringCreate destroys a VM while tined is still creating it.
func TestDestroyDuringCreate(t *testing.T) {
	created := make(chan error, 1)
	go func() {
		_, err := tined.CreateVM(context.Background(), api.CreateVMRequest{VCPUs: 1, MemoryMiB: 512})
		created <- err
	}()
	vm := waitForCreating(t, 10*time.Second)

	gone, err := tined.DestroyVM(context.Background(), vm.ID)
	if err != nil || gone.State != "destroyed" {
		t.Fatalf("destroy during create = %s, %v; want destroyed", gone.State, err)
	}
	if err := <-created; err == nil {
		t.Error("the create should fail once its VM is destroyed")
	}
	requireGone(t, vm.ID)
}

// TestTinedKilledMidCreate SIGKILLs tined while it's creating a VM. After
// systemd restarts it, reconcile must clean up the half-made VM.
func TestTinedKilledMidCreate(t *testing.T) {
	created := make(chan error, 1)
	go func() {
		_, err := tined.CreateVM(context.Background(), api.CreateVMRequest{VCPUs: 1, MemoryMiB: 512})
		created <- err
	}()
	vm := waitForCreating(t, 10*time.Second)
	// Wait until its Firecracker is up (its cgroup exists), so there's
	// something real to clean up.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(cgroupsDir, vm.ID)); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	down := killTined(t)
	if err := <-created; err == nil {
		t.Error("a create cut off by a crash should report an error")
	}
	got := waitState(t, vm.ID, "destroyed", 10*time.Second)
	if !strings.Contains(got.Error, "tined stopped while the VM was being created") {
		t.Errorf("record's error = %q, want it to say why", got.Error)
	}
	requireGone(t, vm.ID)
	t.Logf("tined was back %v after the kill, and had cleaned up %s", down.Round(time.Millisecond), vm.ID)
}

// TestRunningVMsSurviveATinedCrash SIGKILLs tined with VMs running.
func TestRunningVMsSurviveATinedCrash(t *testing.T) {
	var vms []api.VM
	for i := 0; i < 3; i++ {
		vm := create(t)
		mustRun(t, vm.ID, "sh", "-c", fmt.Sprintf("echo before-the-crash-%d > /root/marker", i))
		vms = append(vms, vm)
	}

	down := killTined(t)

	for i, vm := range vms {
		got, err := tined.GetVM(context.Background(), vm.ID)
		if err != nil || got.State != "running" {
			t.Errorf("%s after the crash: %s, %v; want running", vm.ID, got.State, err)
			continue
		}
		if out := mustRun(t, vm.ID, "cat", "/root/marker"); out != fmt.Sprintf("before-the-crash-%d\n", i) {
			t.Errorf("%s's marker = %q", vm.ID, out)
		}
	}
	for _, vm := range vms {
		if _, err := tined.DestroyVM(context.Background(), vm.ID); err != nil {
			t.Errorf("destroy %s after the crash: %v", vm.ID, err)
		}
		requireGone(t, vm.ID)
	}
	t.Logf("tined was back %v after the kill; all 3 VMs kept running", down.Round(time.Millisecond))
}

// TestVMThatDiesWhileTinedIsDown kills a VM's Firecracker while tined is
// stopped. When tined starts, it must clean the VM up and say why.
func TestVMThatDiesWhileTinedIsDown(t *testing.T) {
	vm := create(t)
	systemctl(t, "stop", "tined")
	started := false
	defer func() {
		if !started {
			systemctl(t, "start", "tined")
		}
	}()
	if err := os.WriteFile(filepath.Join(cgroupsDir, vm.ID, "cgroup.kill"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if procs, _ := os.ReadFile(filepath.Join(cgroupsDir, vm.ID, "cgroup.procs")); len(strings.TrimSpace(string(procs))) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	systemctl(t, "start", "tined")
	started = true
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := waitHealthy(ctx); err != nil {
		t.Fatal(err)
	}

	got, err := tined.GetVM(context.Background(), vm.ID)
	if err != nil || got.State != "destroyed" || !strings.Contains(got.Error, "wasn't running when tined started") {
		t.Errorf("after restart: %s with error %q (%v); want destroyed, saying why", got.State, got.Error, err)
	}
	requireGone(t, vm.ID)
}
