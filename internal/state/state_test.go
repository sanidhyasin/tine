package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"
)

// clock is a fake time.Now that only moves when told.
type clock struct{ t atomic.Int64 }

func newClock() *clock {
	c := &clock{}
	c.t.Store(time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC).UnixNano())
	return c
}
func (c *clock) now() time.Time          { return time.Unix(0, c.t.Load()).UTC() }
func (c *clock) advance(d time.Duration) { c.t.Add(int64(d)) }
func openTest(t *testing.T) (*Store, *clock) {
	t.Helper()
	c := newClock()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "tine.db"), WithClock(c.now))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, c
}

func newVM(id string, slot int) VM {
	return VM{ID: id, Image: "base", VCPUs: 2, MemoryMiB: 1024, Slot: slot, UID: 200_000 + slot}
}

func mustCreate(t *testing.T, s *Store, vm VM) VM {
	t.Helper()
	got, err := s.CreateVM(context.Background(), vm)
	if err != nil {
		t.Fatalf("CreateVM %s: %v", vm.ID, err)
	}
	return got
}

func TestOpenMigratesAndReopens(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sub", "tine.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	ms, err := loadMigrations(migrationFiles)
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.SchemaVersion(ctx)
	if err != nil || v != len(ms) || v < 2 {
		t.Errorf("schema version = %d, %v; want %d, one per migration", v, err, len(ms))
	}
	if _, err := s.CreateVM(ctx, newVM("vm-a", 0)); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("database file: %v, %v; want mode 0600", info, err)
	}

	// Reopening keeps the data and doesn't migrate again.
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = s.Close() }()
	if _, err := s.GetVM(ctx, "vm-a"); err != nil {
		t.Errorf("VM lost across reopen: %v", err)
	}
}

func TestOpenRefusesANewerDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tine.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if s, err := Open(ctx, path); err == nil {
		_ = s.Close()
		t.Fatal("Open of a database from a newer tine: want an error")
	}
}

func TestFailedMigrationChangesNothing(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tine.db")
	files := fstest.MapFS{
		"migrations/0001_init.sql": {Data: []byte("CREATE TABLE a (x INTEGER);")},
		"migrations/0002_bad.sql":  {Data: []byte("CREATE TABLE b (x INTEGER); THIS IS NOT SQL;")},
	}
	if s, err := open(ctx, path, files); err == nil {
		_ = s.Close()
		t.Fatal("open with a broken migration: want an error")
	}
	// Migration 1 stuck; migration 2 left no trace.
	s, err := open(ctx, path, fstest.MapFS{"migrations/0001_init.sql": files["migrations/0001_init.sql"]})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if v, _ := s.SchemaVersion(ctx); v != 1 {
		t.Errorf("schema version = %d, want 1", v)
	}
	var tables int
	_ = s.db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'b'").Scan(&tables)
	if tables != 0 {
		t.Error("the failed migration's table exists")
	}
}

func TestMigrationNumbering(t *testing.T) {
	for name, files := range map[string]fstest.MapFS{
		"gap":       {"migrations/0001_a.sql": {}, "migrations/0003_c.sql": {}},
		"no number": {"migrations/init.sql": {}},
		"zero":      {"migrations/0000_a.sql": {}},
	} {
		if _, err := loadMigrations(files); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestCreateAndGetVM(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	in := newVM("vm-1", 3)
	in.ExpiresAt = c.now().Add(time.Hour)
	in.Timings = map[string]float64{"jail": 0.28, "network": 5.1}
	created := mustCreate(t, s, in)
	if created.State != StateCreating || !created.CreatedAt.Equal(c.now()) {
		t.Errorf("created = %+v", created)
	}

	got, err := s.GetVM(ctx, "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Image != "base" || got.VCPUs != 2 || got.MemoryMiB != 1024 || got.Slot != 3 || got.UID != 200_003 ||
		got.PID != 0 || !got.ExpiresAt.Equal(in.ExpiresAt) || got.Timings["network"] != 5.1 || !got.DestroyedAt.IsZero() {
		t.Errorf("GetVM = %+v", got)
	}
	if _, err := s.GetVM(ctx, "vm-nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetVM of a missing VM = %v, want ErrNotFound", err)
	}
	if _, err := s.CreateVM(ctx, newVM("bad id!", 9)); err == nil {
		t.Error("CreateVM with a bad ID: want an error")
	}
}

func TestSlotsAndUIDsAreUniqueAmongLiveVMs(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	mustCreate(t, s, newVM("vm-1", 5))

	for name, vm := range map[string]VM{
		"same ID":   newVM("vm-1", 6),
		"same slot": {ID: "vm-2", Image: "base", VCPUs: 1, MemoryMiB: 256, Slot: 5, UID: 299_999},
		"same uid":  {ID: "vm-3", Image: "base", VCPUs: 1, MemoryMiB: 256, Slot: 7, UID: 200_005},
	} {
		if _, err := s.CreateVM(ctx, vm); !errors.Is(err, ErrConflict) {
			t.Errorf("%s: CreateVM = %v, want ErrConflict", name, err)
		}
	}

	// Once vm-1 is destroyed, its slot and uid can be used again.
	for _, to := range []VMState{StateRunning, StateDestroying, StateDestroyed} {
		if _, err := s.Transition(ctx, "vm-1", to, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateVM(ctx, VM{ID: "vm-4", Image: "base", VCPUs: 1, MemoryMiB: 256, Slot: 5, UID: 200_005}); err != nil {
		t.Errorf("reusing a destroyed VM's slot and uid: %v", err)
	}
}

func TestLifecycleAndEvents(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	mustCreate(t, s, newVM("vm-1", 0))

	c.advance(time.Second)
	vm, err := s.UpdateVM(ctx, "vm-1", "agent ready", func(vm *VM) error {
		vm.State = StateRunning
		vm.PID = 4242
		vm.Timings = map[string]float64{"boot": 2200}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if vm.PID != 4242 || !vm.UpdatedAt.Equal(c.now()) {
		t.Errorf("after update: %+v", vm)
	}

	c.advance(time.Second)
	if _, err := s.Transition(ctx, "vm-1", StateDestroying, "user asked"); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Second)
	vm, err = s.Transition(ctx, "vm-1", StateDestroyed, "cleaned up")
	if err != nil {
		t.Fatal(err)
	}
	if !vm.DestroyedAt.Equal(c.now()) {
		t.Errorf("DestroyedAt = %v, want %v", vm.DestroyedAt, c.now())
	}

	events, err := s.Events(ctx, "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		from, to VMState
		detail   string
	}{
		{"", StateCreating, "created"},
		{StateCreating, StateRunning, "agent ready"},
		{StateRunning, StateDestroying, "user asked"},
		{StateDestroying, StateDestroyed, "cleaned up"},
	}
	if len(events) != len(want) {
		t.Fatalf("got %d events, want %d: %+v", len(events), len(want), events)
	}
	for i, w := range want {
		if events[i].From != w.from || events[i].To != w.to || events[i].Detail != w.detail {
			t.Errorf("event %d = %+v, want %+v", i, events[i], w)
		}
	}

	// Updates that don't change the state don't add events.
	if _, err := s.UpdateVM(ctx, "vm-1", "", func(vm *VM) error { vm.Error = "note"; return nil }); err != nil {
		t.Fatal(err)
	}
	if events, _ := s.Events(ctx, "vm-1"); len(events) != len(want) {
		t.Errorf("a non-state update added an event")
	}
}

func TestInvalidTransitionsAreRefused(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	mustCreate(t, s, newVM("vm-1", 0))

	for _, to := range []VMState{StateDestroyed, StateCreating, "flying"} {
		if _, err := s.Transition(ctx, "vm-1", to, ""); !errors.Is(err, ErrInvalidTransition) {
			t.Errorf("creating -> %s = %v, want ErrInvalidTransition", to, err)
		}
	}
	if vm, _ := s.GetVM(ctx, "vm-1"); vm.State != StateCreating {
		t.Errorf("a refused transition changed the state to %s", vm.State)
	}
	if _, err := s.Transition(ctx, "vm-nope", StateRunning, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("transition of a missing VM = %v, want ErrNotFound", err)
	}
}

func TestEveryStateCanBeDestroyed(t *testing.T) {
	// Whatever happens, a VM can always be torn down.
	for from := range transitions {
		if from == StateDestroyed || from == StateDestroying {
			continue
		}
		if !CanTransition(from, StateDestroying) {
			t.Errorf("a %s VM can't be destroyed", from)
		}
	}
	if len(transitions[StateDestroyed]) != 0 {
		t.Error("destroyed must be final")
	}
}

func TestFailingKeepsTheReason(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	mustCreate(t, s, newVM("vm-1", 0))
	vm, err := s.Transition(ctx, "vm-1", StateFailed, "jailer exited with status 1")
	if err != nil {
		t.Fatal(err)
	}
	if vm.Error != "jailer exited with status 1" {
		t.Errorf("Error = %q, want the transition's detail", vm.Error)
	}
	if _, err := s.Transition(ctx, "vm-1", StateRunning, ""); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("failed -> running = %v, want ErrInvalidTransition", err)
	}
}

func TestChangeErrorsSaveNothing(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	mustCreate(t, s, newVM("vm-1", 0))
	oops := errors.New("oops")
	if _, err := s.UpdateVM(ctx, "vm-1", "", func(vm *VM) error { vm.State = StateRunning; return oops }); !errors.Is(err, oops) {
		t.Fatalf("UpdateVM = %v, want the change's error", err)
	}
	if vm, _ := s.GetVM(ctx, "vm-1"); vm.State != StateCreating {
		t.Errorf("state changed to %s despite the error", vm.State)
	}
	if _, err := s.UpdateVM(ctx, "vm-1", "", func(vm *VM) error { vm.ID = "vm-2"; return nil }); err == nil {
		t.Error("changing a VM's ID: want an error")
	}
}

// TestConcurrentTransitionsHaveOneWinner: two requests to destroy the same
// VM at once must not both succeed.
func TestConcurrentTransitionsHaveOneWinner(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	mustCreate(t, s, newVM("vm-1", 0))
	if _, err := s.Transition(ctx, "vm-1", StateRunning, ""); err != nil {
		t.Fatal(err)
	}

	var (
		wg       sync.WaitGroup
		winners  atomic.Int32
		refused  atomic.Int32
		failures = make(chan error, 10)
	)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Transition(ctx, "vm-1", StateDestroying, fmt.Sprintf("request %d", i))
			switch {
			case err == nil:
				winners.Add(1)
			case errors.Is(err, ErrInvalidTransition):
				refused.Add(1)
			default:
				failures <- err
			}
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Errorf("unexpected error: %v", err)
	}
	if winners.Load() != 1 || refused.Load() != 9 {
		t.Errorf("%d winners and %d refused, want 1 and 9", winners.Load(), refused.Load())
	}
}

func TestListExpireAndPurge(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	soon := c.now().Add(time.Minute)
	for i, id := range []string{"vm-a", "vm-b", "vm-c", "vm-d"} {
		vm := newVM(id, i)
		if id != "vm-d" {
			vm.ExpiresAt = soon
		}
		mustCreate(t, s, vm)
		c.advance(time.Millisecond)
	}
	// vm-b is already being destroyed; vm-c is gone.
	_, _ = s.Transition(ctx, "vm-b", StateDestroying, "")
	_, _ = s.Transition(ctx, "vm-c", StateDestroying, "")
	_, _ = s.Transition(ctx, "vm-c", StateDestroyed, "")

	live, _ := s.ListVMs(ctx, ListOptions{})
	all, _ := s.ListVMs(ctx, ListOptions{IncludeDestroyed: true})
	if ids(live) != "vm-a,vm-b,vm-d" || ids(all) != "vm-a,vm-b,vm-c,vm-d" {
		t.Errorf("ListVMs = %s; with destroyed = %s", ids(live), ids(all))
	}

	if expired, _ := s.ExpiredVMs(ctx, c.now()); len(expired) != 0 {
		t.Errorf("before their time, ExpiredVMs = %s", ids(expired))
	}
	if expired, _ := s.ExpiredVMs(ctx, soon); ids(expired) != "vm-a" {
		t.Errorf("ExpiredVMs = %s, want vm-a (vm-b is already being destroyed, vm-c is gone, vm-d never expires)", ids(expired))
	}

	n, err := s.PurgeDestroyed(ctx, c.now().Add(time.Hour))
	if err != nil || n != 1 {
		t.Errorf("PurgeDestroyed = %d, %v; want 1", n, err)
	}
	if _, err := s.GetVM(ctx, "vm-c"); !errors.Is(err, ErrNotFound) {
		t.Error("purged VM still exists")
	}
	if events, _ := s.Events(ctx, "vm-c"); len(events) != 0 {
		t.Error("purged VM's events still exist")
	}
}

func TestImages(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	meta := json.RawMessage(`{"name":"base","format_version":1}`)

	if _, err := s.PutImage(ctx, Image{Name: "base", Dir: "/srv/tine/images/base", Metadata: meta}); err != nil {
		t.Fatal(err)
	}
	c.advance(time.Hour)
	// Putting it again updates it.
	if _, err := s.PutImage(ctx, Image{Name: "base", Dir: "/srv/tine/images/base", Metadata: json.RawMessage(`{"name":"base","format_version":1,"x":1}`)}); err != nil {
		t.Fatal(err)
	}
	img, err := s.GetImage(ctx, "base")
	if err != nil || !img.RegisteredAt.Equal(c.now()) || string(img.Metadata) != `{"name":"base","format_version":1,"x":1}` {
		t.Errorf("GetImage = %+v, %v", img, err)
	}
	if _, err := s.PutImage(ctx, Image{Name: "x", Dir: "/x", Metadata: json.RawMessage("not json")}); err == nil {
		t.Error("PutImage with bad metadata: want an error")
	}
	_, _ = s.PutImage(ctx, Image{Name: "alpha", Dir: "/a", Metadata: meta})
	if list, _ := s.ListImages(ctx); len(list) != 2 || list[0].Name != "alpha" {
		t.Errorf("ListImages = %+v", list)
	}

	// An image in use can't be deleted; once its VMs are gone, it can.
	mustCreate(t, s, newVM("vm-1", 0))
	if err := s.DeleteImage(ctx, "base"); !errors.Is(err, ErrInUse) {
		t.Errorf("DeleteImage in use = %v, want ErrInUse", err)
	}
	_, _ = s.Transition(ctx, "vm-1", StateDestroying, "")
	_, _ = s.Transition(ctx, "vm-1", StateDestroyed, "")
	if err := s.DeleteImage(ctx, "base"); err != nil {
		t.Errorf("DeleteImage: %v", err)
	}
	if err := s.DeleteImage(ctx, "base"); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteImage twice = %v, want ErrNotFound", err)
	}
}

func ids(vms []VM) string {
	s := ""
	for i, vm := range vms {
		if i > 0 {
			s += ","
		}
		s += vm.ID
	}
	return s
}

// SQLite compares stored times as text, so text order must be time order,
// even when one time has a fraction of a second and the other doesn't.
func TestStoredTimesSortChronologically(t *testing.T) {
	base := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	times := []time.Time{base, base.Add(time.Nanosecond), base.Add(time.Millisecond), base.Add(time.Second), base.Add(time.Hour)}
	for i := 1; i < len(times); i++ {
		a, b := formatTime(times[i-1]), formatTime(times[i])
		if a >= b {
			t.Errorf("%q should sort before %q", a, b)
		}
	}
	for _, tm := range times {
		got, err := parseTime(formatTime(tm))
		if err != nil || !got.Equal(tm) {
			t.Errorf("round trip of %v = %v, %v", tm, got, err)
		}
	}
	// Times in other zones are stored in UTC.
	ist := time.FixedZone("IST", 5*3600+1800)
	if got := formatTime(base.In(ist)); got != formatTime(base) {
		t.Errorf("%v stored as %q, want %q", base.In(ist), got, formatTime(base))
	}
}
