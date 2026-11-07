//go:build e2e

package e2e

import (
	"context"
	"errors"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sanidhyasin/tine/internal/api"
)

// createDefault makes a VM of the default size, which starts from base's
// template, and registers its destruction for the end of the test.
func createDefault(t *testing.T) api.VM {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	vm, err := tined.CreateVM(ctx, api.CreateVMRequest{TTLSeconds: 600})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _, _ = tined.DestroyVM(context.Background(), vm.ID) })
	if vm.Source != "template" {
		t.Fatalf("%s started %s, want from the template", vm.ID, vm.Source)
	}
	return vm
}

func snapshotOf(t *testing.T, id string) api.Snapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	snap, err := tined.CreateSnapshot(ctx, id)
	if err != nil {
		t.Fatalf("snapshot %s: %v", id, err)
	}
	return snap
}

func apiCode(err error) (int, string) {
	var ae *api.Error
	if errors.As(err, &ae) {
		return ae.StatusCode, ae.Code
	}
	return 0, ""
}

// TestRollbackBringsAFileBack: write a file, snapshot, change it, roll back:
// the file is back, and the VM keeps its ID, network and hostname.
func TestRollbackBringsAFileBack(t *testing.T) {
	vm := createDefault(t)
	mustRun(t, vm.ID, "sh", "-c", "echo before > /root/file")
	addr := vethAddr(t, vm.ID, "self")
	snap := snapshotOf(t, vm.ID)
	mustRun(t, vm.ID, "sh", "-c", "echo after > /root/file; touch /root/new")

	got, err := tined.Rollback(context.Background(), vm.ID, snap.ID)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if out := strings.TrimSpace(mustRun(t, vm.ID, "cat", "/root/file")); out != "before" {
		t.Errorf("after the rollback /root/file = %q, want before", out)
	}
	if _, code, _ := run(context.Background(), vm.ID, "test", "-e", "/root/new"); code == 0 {
		t.Error("a file made after the snapshot survived the rollback")
	}
	if got.ID != vm.ID || got.SnapshotID != snap.ID || vethAddr(t, vm.ID, "self") != addr {
		t.Errorf("after the rollback: %+v, address %s (was %s)", got, vethAddr(t, vm.ID, "self"), addr)
	}
	if h := strings.TrimSpace(mustRun(t, vm.ID, "hostname")); h != vm.ID {
		t.Errorf("hostname = %q", h)
	}
	if _, code, _ := run(context.Background(), vm.ID, "timeout", "5", "bash", "-c", "echo > /dev/tcp/1.1.1.1/443"); code != 0 {
		t.Errorf("no internet after the rollback (exit %d)", code)
	}
	t.Logf("snapshot paused the VM %.0fms; rollback took %.0fms", snap.TimingsMs["paused"], got.TimingsMs["rollback"])
}

// TestRestoredGuestHasTheRightTime: a guest restored from a template taken a
// while ago has the host's time, not the template's.
func TestRestoredGuestHasTheRightTime(t *testing.T) {
	imgs, err := tined.Images(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, img := range imgs {
		for _, tm := range img.Templates {
			if age := time.Since(tm.CreatedAt); img.Name == "base" && age < 5*time.Second {
				time.Sleep(5*time.Second - age) // so a stale clock would show
			}
		}
	}
	vm := createDefault(t)
	guest, err := strconv.ParseFloat(strings.TrimSpace(mustRun(t, vm.ID, "date", "+%s.%N")), 64)
	host := float64(time.Now().UnixNano()) / 1e9
	if err != nil || math.Abs(host-guest) > 1 {
		t.Errorf("guest clock %f, host %f: off by %.3fs (%v)", guest, host, host-guest, err)
	}
	t.Logf("guest clock within %.0fms of the host's", math.Abs(host-guest)*1000)
}

// TestTwoRestoresAreDifferent: two VMs restored from one template, at once,
// read different random numbers and have their own identities.
func TestTwoRestoresAreDifferent(t *testing.T) {
	var vms [2]api.VM
	var wg sync.WaitGroup
	for i := range vms {
		wg.Add(1)
		go func() { defer wg.Done(); vms[i] = createDefault(t) }()
	}
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}
	read := func(id string) (random, hostname, machineID string) {
		out := mustRun(t, id, "sh", "-c", "head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \\n'; echo; hostname; cat /etc/machine-id")
		f := strings.Fields(out)
		if len(f) != 3 {
			t.Fatalf("%s: unexpected output %q", id, out)
		}
		return f[0], f[1], f[2]
	}
	r0, h0, m0 := read(vms[0].ID)
	r1, h1, m1 := read(vms[1].ID)
	if vms[0].SnapshotID != vms[1].SnapshotID {
		t.Fatalf("the two came from different templates: %s, %s", vms[0].SnapshotID, vms[1].SnapshotID)
	}
	if r0 == r1 {
		t.Errorf("two restores of %s read the same random bytes: %s", vms[0].SnapshotID, r0)
	}
	if h0 == h1 || m0 == m1 || h0 != vms[0].ID || h1 != vms[1].ID {
		t.Errorf("identities: %s %s / %s %s", h0, m0, h1, m1)
	}
}

// TestSnapshotsInUseStayAndUnusedOnesGo: a template a VM runs from can't be
// deleted, and a destroyed VM's snapshot is deleted by tined.
func TestSnapshotsInUseStayAndUnusedOnesGo(t *testing.T) {
	ctx := context.Background()
	vm := createDefault(t)
	if _, err := tined.DeleteSnapshot(ctx, vm.SnapshotID); func() bool { s, c := apiCode(err); return s != http.StatusConflict || c != api.CodeInUse }() {
		t.Errorf("deleting the template %s runs from = %v, want 409 in_use", vm.ID, err)
	}
	snap := snapshotOf(t, vm.ID)
	if _, err := tined.DestroyVM(ctx, vm.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(gcWait)
	for {
		_, err := tined.GetSnapshot(ctx, snap.ID)
		if s, _ := apiCode(err); s == http.StatusNotFound {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("snapshot %s of destroyed VM %s is still there after %v (%v)", snap.ID, vm.ID, gcWait, err)
		}
		time.Sleep(time.Second)
	}
	if _, err := os.Stat(filepath.Join(snapshotsDir, snap.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("its files are still there: %v", err)
	}
}

// TestSnapshotRefusedWhileACommandRuns: a snapshot would cut a running
// command off, so it's refused.
func TestSnapshotRefusedWhileACommandRuns(t *testing.T) {
	vm := createDefault(t)
	done := make(chan struct{})
	go func() { _, _, _ = run(context.Background(), vm.ID, "sleep", "2"); close(done) }()
	time.Sleep(300 * time.Millisecond)
	_, err := tined.CreateSnapshot(context.Background(), vm.ID)
	if s, c := apiCode(err); s != http.StatusConflict || c != api.CodeBusy {
		t.Errorf("snapshot during a command = %v, want 409 busy", err)
	}
	<-done
	snapshotOf(t, vm.ID) // and fine once it's finished
}

// TestTinedKilledMidSnapshot: tined dies while it's snapshotting a 4 GiB VM.
// When it's back, the VM is running again, and nothing of the snapshot is
// left half-done.
func TestTinedKilledMidSnapshot(t *testing.T) {
	ctx := context.Background()
	vm, err := tined.CreateVM(ctx, api.CreateVMRequest{VCPUs: 2, MemoryMiB: 4096, TTLSeconds: 600})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = tined.DestroyVM(context.Background(), vm.ID) })
	result := make(chan error, 1)
	go func() { _, err := tined.CreateSnapshot(ctx, vm.ID); result <- err }()
	time.Sleep(200 * time.Millisecond) // writing 4 GiB of memory takes about a second
	killTined(t)
	err = <-result
	t.Logf("the snapshot request ended with: %v", err)

	got := waitState(t, vm.ID, "running", 10*time.Second)
	if out := strings.TrimSpace(mustRun(t, vm.ID, "echo", "alive")); out != "alive" {
		t.Errorf("the VM doesn't answer after the restart: %q", out)
	}
	snaps, err := tined.ListSnapshots(ctx, api.SnapshotFilter{VMID: vm.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range snaps {
		if s.State != "ready" {
			t.Errorf("snapshot %s is %s after the restart", s.ID, s.State)
		}
	}
	if left, _ := os.ReadDir(filepath.Join(jailsDir, vm.ID, "root", "snapshot")); len(left) > 0 {
		t.Errorf("the interrupted snapshot's files are still in the jail: %v", left)
	}
	entries, _ := os.ReadDir(snapshotsDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".new-") {
			t.Errorf("a half-gathered snapshot is in the store: %s", e.Name())
		}
	}
	t.Logf("after the kill: %s is %s, with %d ready snapshots", got.ID, got.State, len(snaps))
}
