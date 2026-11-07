package vm

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sanidhyasin/tine/internal/agent"
	"github.com/sanidhyasin/tine/internal/agentproto"
	"github.com/sanidhyasin/tine/internal/image"
	"github.com/sanidhyasin/tine/internal/jailer"
	"github.com/sanidhyasin/tine/internal/metrics"
	"github.com/sanidhyasin/tine/internal/netns"
	"github.com/sanidhyasin/tine/internal/snapshot"
	"github.com/sanidhyasin/tine/internal/state"
)

// fakeHost records what the manager asks of the host, and fails on demand.
type fakeHost struct {
	mu     sync.Mutex
	calls  []string
	failAt map[string]error
	// agentWaiting, if set, makes waitAgent signal it and then block until
	// its context ends, like a VM that never becomes ready.
	agentWaiting chan struct{}
	agentClient  *agentproto.Client
	nextPID      int
	lastProc     *fakeProc
	// For Reconcile: VMs whose Firecracker isn't running, and what the host holds.
	dead map[string]bool
	inv  inventory
	// isPaused is which VMs' Firecrackers have them paused.
	isPaused map[string]bool
	// holds make a step wait, once reached, until released (see holdAt).
	holds map[string]*hold
	// thisHost, if set, is what hostInfo reports instead of fakeSnapshotHost.
	thisHost *snapshot.Host
	lastSync agentproto.SyncRequest
	// busyUntil makes paused wait until then, like a Firecracker busy writing a snapshot.
	busyUntil time.Time
}

type hold struct {
	reached, release chan struct{}
	once             sync.Once
}

// holdAt makes the next call to op wait until release is called. reached
// is closed once op has been called.
func (f *fakeHost) holdAt(op string) (reached <-chan struct{}, release func()) {
	h := &hold{reached: make(chan struct{}), release: make(chan struct{})}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.holds == nil {
		f.holds = map[string]*hold{}
	}
	f.holds[op] = h
	var once sync.Once
	return h.reached, func() { once.Do(func() { close(h.release) }) }
}

func newFakeHost() *fakeHost {
	return &fakeHost{failAt: map[string]error{}, nextPID: 1000, isPaused: map[string]bool{}}
}

// fakeSnapshotHost is what describeHost reports.
var fakeSnapshotHost = snapshot.Host{Firecracker: "1.17.0", CPU: "Fake CPU", Kernel: "6.8.0-test"}

func (f *fakeHost) prepareSnapshot(*jailer.Jail) error { return f.record("prepareSnapshot") }
func (f *fakeHost) pause(_ context.Context, j *jailer.Jail) error {
	if err := f.record("pause"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.isPaused[j.ID] = true
	return nil
}
func (f *fakeHost) resume(_ context.Context, j *jailer.Jail) error {
	if err := f.record("resume"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.isPaused, j.ID)
	return nil
}
func (f *fakeHost) paused(ctx context.Context, j *jailer.Jail) (bool, error) {
	f.mu.Lock()
	busyUntil := f.busyUntil
	f.mu.Unlock()
	// A Firecracker still writing a snapshot doesn't answer until it's done.
	if wait := time.Until(busyUntil); wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.isPaused[j.ID], nil
}
func (f *fakeHost) writeSnapshot(context.Context, *jailer.Jail) error {
	return f.record("writeSnapshot")
}
func (f *fakeHost) cloneDisk(_ *jailer.Jail, dst string) error {
	if err := f.record("cloneDisk"); err != nil {
		return err
	}
	return os.WriteFile(dst, []byte("disk"), 0o600)
}
func (f *fakeHost) collectSnapshot(_ *jailer.Jail, b *snapshot.Building) error {
	if err := f.record("collectSnapshot"); err != nil {
		return err
	}
	for _, file := range []string{snapshot.StateFile, snapshot.MemoryFile} {
		if err := os.WriteFile(b.Path(file), []byte(file), 0o600); err != nil {
			return err
		}
	}
	return nil
}
func (f *fakeHost) flushSnapshot(string) error { return f.record("flushSnapshot") }
func (f *fakeHost) hostInfo(context.Context) (snapshot.Host, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.thisHost != nil {
		return *f.thisHost, nil
	}
	return fakeSnapshotHost, nil
}
func (f *fakeHost) loadSnapshot(context.Context, *jailer.Jail) error { return f.record("loadSnapshot") }
func (f *fakeHost) syncGuest(_ context.Context, _ *jailer.Jail, req agentproto.SyncRequest) (agentproto.SyncResult, error) {
	f.mu.Lock()
	f.lastSync = req
	f.mu.Unlock()
	return agentproto.SyncResult{}, f.record("syncGuest")
}
func (f *fakeHost) describeHost(context.Context, *jailer.Jail) (snapshot.Host, error) {
	return fakeSnapshotHost, f.record("describeHost")
}

func (f *fakeHost) record(op string) error {
	f.mu.Lock()
	f.calls = append(f.calls, op)
	err := f.failAt[op]
	h := f.holds[op]
	delete(f.holds, op)
	f.mu.Unlock()
	if h != nil {
		h.once.Do(func() { close(h.reached) })
		<-h.release
	}
	return err
}

func (f *fakeHost) fail(op string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil {
		delete(f.failAt, op)
	} else {
		f.failAt[op] = err
	}
}

func (f *fakeHost) takeCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.calls
	f.calls = nil
	return c
}

func (f *fakeHost) setup(context.Context) error { return f.record("setup") }
func (f *fakeHost) createNetwork(context.Context, netns.Network, netns.Owner) error {
	return f.record("createNetwork")
}
func (f *fakeHost) destroyNetwork(netns.Network) error { return f.record("destroyNetwork") }
func (f *fakeHost) prepareJail(*jailer.Jail) error     { return f.record("prepareJail") }
func (f *fakeHost) cleanupJail(*jailer.Jail) error     { return f.record("cleanupJail") }
func (f *fakeHost) configureAndBoot(context.Context, *jailer.Jail, bootSpec) error {
	return f.record("configureAndBoot")
}
func (f *fakeHost) killFirecracker(context.Context, *jailer.Jail, int) error {
	return f.record("killFirecracker")
}
func (f *fakeHost) agent(*jailer.Jail) *agentproto.Client { return f.agentClient }
func (f *fakeHost) inventory(context.Context) (inventory, error) {
	if err := f.record("inventory"); err != nil {
		return inventory{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inv, nil
}
func (f *fakeHost) alive(_ context.Context, j *jailer.Jail) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.dead[j.ID], nil
}
func (f *fakeHost) removeOrphan(_ context.Context, id string) error {
	return f.record("removeOrphan:" + id)
}
func (f *fakeHost) removeVeth(name string) error { return f.record("removeVeth:" + name) }

func (f *fakeHost) startFirecracker(*jailer.Jail) (process, error) {
	if err := f.record("startFirecracker"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextPID++
	p := &fakeProc{host: f, pidv: f.nextPID, exited: make(chan struct{})}
	f.lastProc = p
	return p, nil
}

func (f *fakeHost) waitAgent(ctx context.Context, _ *jailer.Jail) error {
	if err := f.record("waitAgent"); err != nil {
		return err
	}
	if f.agentWaiting != nil {
		close(f.agentWaiting)
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

type fakeProc struct {
	host *fakeHost
	pidv int
	once sync.Once
	err  error

	exited chan struct{}
}

func (p *fakeProc) pid() int              { return p.pidv }
func (p *fakeProc) done() <-chan struct{} { return p.exited }
func (p *fakeProc) exitErr() error        { return p.err }
func (p *fakeProc) kill() error {
	_ = p.host.record("kill")
	p.exit(errors.New("signal: killed"))
	return nil
}
func (p *fakeProc) exit(err error) { p.once.Do(func() { p.err = err; close(p.exited) }) }

// --- setup ---

type fixture struct {
	m     *Manager
	host  *fakeHost
	store *state.Store
}

func newFixture(t *testing.T, change func(*Config)) *fixture {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	images := filepath.Join(dir, "images")
	makeImage(t, images, "base")

	store, err := state.Open(ctx, filepath.Join(dir, "tine.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := Config{
		ImagesDir: images, ChrootBaseDir: filepath.Join(dir, "jail"), SnapshotsDir: filepath.Join(dir, "snapshots"),
		AgentTimeout: 5 * time.Second, MinFreeMiB: 1, SettleCommand: []string{"true"},
	}
	if change != nil {
		change(&cfg)
	}
	h := newFakeHost()
	m, err := newManager(cfg, store, h)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{m: m, host: h, store: store}
}

// makeImage installs a minimal image that image.Open accepts.
func makeImage(t *testing.T, imagesDir, name string) {
	t.Helper()
	dir := filepath.Join(imagesDir, name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(image.Metadata{FormatVersion: image.MetadataVersion, Name: name})
	for file, content := range map[string][]byte{image.MetadataFile: meta, image.KernelFile: []byte("k"), image.RootfsFile: []byte("r")} {
		if err := os.WriteFile(filepath.Join(dir, file), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func eventSummary(t *testing.T, s *state.Store, id string) string {
	t.Helper()
	events, err := s.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var parts []string
	for _, e := range events {
		if e.From != "" && e.From == e.To {
			continue // a note, not a state change
		}
		parts = append(parts, string(e.To))
	}
	return strings.Join(parts, " > ")
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// --- tests ---

func TestCreateRunsEveryStepInOrder(t *testing.T) {
	f := newFixture(t, nil)
	vm, err := f.m.Create(context.Background(), CreateRequest{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got, want := strings.Join(f.host.takeCalls(), ","), "createNetwork,prepareJail,startFirecracker,configureAndBoot,waitAgent,syncGuest"; got != want {
		t.Errorf("host calls:\n got %s\nwant %s", got, want)
	}
	if vm.State != state.StateRunning || vm.PID == 0 || vm.Image != "base" || vm.VCPUs != 2 || vm.MemoryMiB != 1024 {
		t.Errorf("VM = %+v", vm)
	}
	if vm.UID != 200_000+vm.Slot {
		t.Errorf("uid %d, want UIDBase+slot = %d", vm.UID, 200_000+vm.Slot)
	}
	if time.Until(vm.ExpiresAt) < 59*time.Minute {
		t.Errorf("expires %v, want about an hour from now", vm.ExpiresAt)
	}
	for _, stage := range []string{"allocate", "network", "jail", "start", "configure", "agent", "total"} {
		if _, ok := vm.Timings[stage]; !ok {
			t.Errorf("timings lack %q: %v", stage, vm.Timings)
		}
	}
	if !regexp.MustCompile(`^vm-[a-z2-7]{10}$`).MatchString(vm.ID) {
		t.Errorf("ID %q doesn't look like vm-<10 base32 characters>", vm.ID)
	}
	if got := eventSummary(t, f.store, vm.ID); got != "creating > running" {
		t.Errorf("events = %s", got)
	}
}

func TestCreateUndoesWhenAStepFails(t *testing.T) {
	tests := []struct {
		failAt    string
		wantCalls string
	}{
		{"createNetwork", "createNetwork"},
		{"prepareJail", "createNetwork,prepareJail,destroyNetwork"},
		{"startFirecracker", "createNetwork,prepareJail,startFirecracker,cleanupJail,destroyNetwork"},
		{"configureAndBoot", "createNetwork,prepareJail,startFirecracker,configureAndBoot,kill,cleanupJail,destroyNetwork"},
		{"waitAgent", "createNetwork,prepareJail,startFirecracker,configureAndBoot,waitAgent,kill,cleanupJail,destroyNetwork"},
		{"syncGuest", "createNetwork,prepareJail,startFirecracker,configureAndBoot,waitAgent,syncGuest,kill,cleanupJail,destroyNetwork"},
	}
	for _, tt := range tests {
		t.Run(tt.failAt, func(t *testing.T) {
			f := newFixture(t, nil)
			f.host.fail(tt.failAt, errors.New("boom"))
			vm, err := f.m.Create(context.Background(), CreateRequest{})
			if err == nil || !strings.Contains(err.Error(), "boom") {
				t.Fatalf("Create = %v, want the step's error", err)
			}
			if got := strings.Join(f.host.takeCalls(), ","); got != tt.wantCalls {
				t.Errorf("host calls (steps, then undo in reverse):\n got %s\nwant %s", got, tt.wantCalls)
			}
			// Cleaned up: destroyed, with the reason kept, and the slot free again.
			if vm.State != state.StateDestroyed || !strings.Contains(vm.Error, "boom") {
				t.Errorf("VM after a failed create = %s, error %q; want destroyed with the error", vm.State, vm.Error)
			}
			if got := eventSummary(t, f.store, vm.ID); got != "creating > failed > destroying > destroyed" {
				t.Errorf("events = %s", got)
			}
			if f.m.slots.InUse() != 0 {
				t.Errorf("%d slots still in use", f.m.slots.InUse())
			}
		})
	}
}

func TestFailedCleanupKeepsTheVMFailed(t *testing.T) {
	f := newFixture(t, nil)
	f.host.fail("configureAndBoot", errors.New("boot failed"))
	f.host.fail("cleanupJail", errors.New("jail busy"))
	vm, err := f.m.Create(context.Background(), CreateRequest{})
	if err == nil {
		t.Fatal("Create: want an error")
	}
	// Resources may still exist, so the VM stays failed and keeps its slot.
	if vm.State != state.StateFailed || f.m.slots.InUse() != 1 {
		t.Fatalf("after a failed cleanup: state %s, %d slots in use; want failed and 1", vm.State, f.m.slots.InUse())
	}
	// Once the problem is gone, destroying it finishes the job.
	f.host.fail("cleanupJail", nil)
	vm, err = f.m.Destroy(context.Background(), vm.ID)
	if err != nil || vm.State != state.StateDestroyed || f.m.slots.InUse() != 0 {
		t.Errorf("Destroy = %s, %v, %d slots in use; want destroyed and 0", vm.State, err, f.m.slots.InUse())
	}
}

func TestDestroy(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	vm, err := f.m.Create(ctx, CreateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	f.host.takeCalls()

	vm, err = f.m.Destroy(ctx, vm.ID)
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if got, want := strings.Join(f.host.takeCalls(), ","), "kill,cleanupJail,destroyNetwork"; got != want {
		t.Errorf("host calls:\n got %s\nwant %s", got, want)
	}
	if vm.State != state.StateDestroyed || vm.DestroyedAt.IsZero() || f.m.slots.InUse() != 0 {
		t.Errorf("after Destroy: %+v, %d slots in use", vm, f.m.slots.InUse())
	}
	if _, ok := vm.Timings["destroy"]; !ok {
		t.Error("no destroy timing")
	}
	// Destroying again changes nothing.
	again, err := f.m.Destroy(ctx, vm.ID)
	if err != nil || again.State != state.StateDestroyed || len(f.host.takeCalls()) != 0 {
		t.Errorf("second Destroy = %s, %v; want destroyed with no host calls", again.State, err)
	}
	if _, err := f.m.Destroy(ctx, "vm-nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Destroy of a missing VM = %v, want ErrNotFound", err)
	}
}

func TestFailedDestroyCanBeRetried(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	vm, _ := f.m.Create(ctx, CreateRequest{})
	f.host.fail("destroyNetwork", errors.New("namespace busy"))
	vm, err := f.m.Destroy(ctx, vm.ID)
	if err == nil || vm.State != state.StateFailed || f.m.slots.InUse() != 1 {
		t.Fatalf("failed Destroy = %s, %v, %d slots; want failed, an error, the slot kept", vm.State, err, f.m.slots.InUse())
	}
	f.host.fail("destroyNetwork", nil)
	if vm, err = f.m.Destroy(ctx, vm.ID); err != nil || vm.State != state.StateDestroyed {
		t.Errorf("retried Destroy = %s, %v", vm.State, err)
	}
}

func TestDestroyDuringCreate(t *testing.T) {
	f := newFixture(t, nil)
	f.host.agentWaiting = make(chan struct{}) // the VM never becomes ready
	type result struct {
		vm  state.VM
		err error
	}
	created := make(chan result, 1)
	go func() {
		vm, err := f.m.Create(context.Background(), CreateRequest{})
		created <- result{vm, err}
	}()
	<-f.host.agentWaiting

	live, _ := f.m.List(context.Background(), false)
	if len(live) != 1 {
		t.Fatalf("%d live VMs while creating, want 1", len(live))
	}
	destroyed, err := f.m.Destroy(context.Background(), live[0].ID)
	if err != nil || destroyed.State != state.StateDestroyed {
		t.Fatalf("Destroy during create = %s, %v; want destroyed", destroyed.State, err)
	}
	r := <-created
	if !errors.Is(r.err, context.Canceled) {
		t.Errorf("Create = %v, want it cancelled", r.err)
	}
	calls := strings.Join(f.host.takeCalls(), ",")
	if !strings.HasSuffix(calls, "waitAgent,kill,cleanupJail,destroyNetwork") {
		t.Errorf("host calls %s: the cancelled create should have undone its steps", calls)
	}
	if f.m.slots.InUse() != 0 {
		t.Errorf("%d slots in use", f.m.slots.InUse())
	}
}

func TestUnexpectedExitMarksTheVMFailed(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	vm, err := f.m.Create(ctx, CreateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	f.host.lastProc.exit(errors.New("signal: segmentation fault"))
	eventually(t, "the VM to be marked failed", func() bool {
		got, _ := f.m.Get(ctx, vm.ID)
		return got.State == state.StateFailed && strings.Contains(got.Error, "exited unexpectedly")
	})
	// It can still be destroyed: the manager no longer owns a process, so it
	// stops it by PID (already gone here) and cleans up.
	f.host.takeCalls()
	if vm, err = f.m.Destroy(ctx, vm.ID); err != nil || vm.State != state.StateDestroyed {
		t.Fatalf("Destroy = %s, %v", vm.State, err)
	}
	if got, want := strings.Join(f.host.takeCalls(), ","), "killFirecracker,cleanupJail,destroyNetwork"; got != want {
		t.Errorf("host calls:\n got %s\nwant %s", got, want)
	}
}

func TestCreateValidatesRequests(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	for name, tc := range map[string]struct {
		req  CreateRequest
		want error
	}{
		"missing image":   {CreateRequest{Image: "nope"}, ErrImageNotFound},
		"odd vcpus":       {CreateRequest{VCPUs: 3}, ErrInvalidRequest},
		"too many vcpus":  {CreateRequest{VCPUs: 34}, ErrInvalidRequest},
		"too little RAM":  {CreateRequest{MemoryMiB: 64}, ErrInvalidRequest},
		"too short a TTL": {CreateRequest{TTL: time.Second}, ErrInvalidRequest},
		"negative vcpus":  {CreateRequest{VCPUs: -2}, ErrInvalidRequest},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := f.m.Create(ctx, tc.req); !errors.Is(err, tc.want) {
				t.Errorf("Create = %v, want %v", err, tc.want)
			}
		})
	}
	if calls := f.host.takeCalls(); len(calls) != 0 {
		t.Errorf("invalid requests reached the host: %v", calls)
	}
	if vms, _ := f.m.List(ctx, true); len(vms) != 0 {
		t.Errorf("invalid requests left %d records", len(vms))
	}
}

func TestCapacity(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.MaxVMs = 1 })
	ctx := context.Background()
	if _, err := f.m.Create(ctx, CreateRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Create(ctx, CreateRequest{}); !errors.Is(err, ErrCapacity) {
		t.Errorf("second Create = %v, want ErrCapacity", err)
	}
}

func TestNewKeepsSlotsOfExistingVMs(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	vm, _ := f.m.Create(ctx, CreateRequest{})

	// A new manager over the same store, as after a tined restart.
	m2, err := newManager(f.m.cfg, f.store, newFakeHost())
	if err != nil {
		t.Fatal(err)
	}
	if m2.slots.InUse() != 1 {
		t.Fatalf("%d slots reserved after restart, want 1", m2.slots.InUse())
	}
	next, err := m2.slots.Allocate()
	if err != nil || next == vm.Slot {
		t.Errorf("after restart, Allocate = %d, %v; must not hand out the running VM's slot %d", next, err, vm.Slot)
	}
}

func TestExec(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	f.host.agentClient = startAgent(t)
	vm, err := f.m.Create(ctx, CreateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	st, err := f.m.Exec(ctx, vm.ID, agentproto.ExecRequest{Args: []string{"echo", "hi"}}, func(_ string, d []byte) { out.Write(d) })
	if err != nil || st.Code != 0 || out.String() != "hi\n" {
		t.Errorf("Exec = %q, %+v, %v", out.String(), st, err)
	}
	if _, err := f.m.Destroy(ctx, vm.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Exec(ctx, vm.ID, agentproto.ExecRequest{Args: []string{"true"}}, nil); !errors.Is(err, ErrNotRunning) {
		t.Errorf("Exec on a destroyed VM = %v, want ErrNotRunning", err)
	}
	if _, err := f.m.Exec(ctx, "vm-nope", agentproto.ExecRequest{Args: []string{"true"}}, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("Exec on a missing VM = %v, want ErrNotFound", err)
	}
}

func TestSetupAndSyncImages(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	if err := f.m.Setup(ctx); err != nil {
		t.Fatal(err)
	}
	if calls := f.host.takeCalls(); len(calls) != 1 || calls[0] != "setup" {
		t.Errorf("Setup host calls = %v", calls)
	}
	img, err := f.store.GetImage(ctx, "base")
	if err != nil || !strings.HasSuffix(img.Dir, "/base") {
		t.Errorf("synced image = %+v, %v", img, err)
	}
}

func TestIDsAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 10_000; i++ {
		id := newID()
		if seen[id] {
			t.Fatalf("duplicate ID %s", id)
		}
		seen[id] = true
	}
}

// startAgent runs a real agent on a Unix socket and returns a client for it.
func startAgent(t *testing.T) *agentproto.Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "vm") // short: socket paths are limited to about 100 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "a.sock")
	ctx, cancel := context.WithCancel(context.Background())
	ln, err := agent.Listen(ctx, "unix:"+sock)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = agent.New(agent.Config{}).Serve(ctx, ln); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return agentproto.NewClient(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	})
}

func TestDestroyKeepsTheNetworkIfTheJailCantBeCleaned(t *testing.T) {
	f := newFixture(t, nil)
	ctx := context.Background()
	vm, _ := f.m.Create(ctx, CreateRequest{})
	f.host.takeCalls()
	f.host.fail("cleanupJail", errors.New("cgroup busy"))
	if _, err := f.m.Destroy(ctx, vm.ID); err == nil {
		t.Fatal("Destroy: want an error")
	}
	// The VM may still be running, so its network must stay.
	if got, want := strings.Join(f.host.takeCalls(), ","), "kill,cleanupJail"; got != want {
		t.Errorf("host calls:\n got %s\nwant %s", got, want)
	}
}

func TestMetricsRecordCreatesFailuresAndDestroys(t *testing.T) {
	m := metrics.New("v")
	f := newFixture(t, func(c *Config) { c.Metrics = m })
	ctx := context.Background()
	vm, err := f.m.Create(ctx, CreateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Destroy(ctx, vm.ID); err != nil {
		t.Fatal(err)
	}
	f.host.fail("waitAgent", errors.New("boom"))
	_, _ = f.m.Create(ctx, CreateRequest{})

	for series, want := range map[string]float64{
		`tine_vm_creates_total{result="ok"}`:           1,
		`tine_vm_creates_total{result="failed"}`:       1,
		`tine_vm_create_failures_total{stage="agent"}`: 1,
		`tine_vm_destroys_total{result="ok"}`:          1,
	} {
		if got := counterValue(t, m, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}
}

// counterValue reads one series from the metrics' text exposition.
func counterValue(t *testing.T, m *metrics.Metrics, series string) float64 {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if v, ok := strings.CutPrefix(line, series+" "); ok {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				t.Fatal(err)
			}
			return f
		}
	}
	return 0
}
