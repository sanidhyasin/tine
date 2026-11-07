package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

var testMetadata = json.RawMessage(`{"format_version":1}`)

func mustSnapshot(t *testing.T, s *Store, snap Snapshot, ready bool) Snapshot {
	t.Helper()
	ctx := context.Background()
	got, err := s.CreateSnapshot(ctx, snap)
	if err != nil {
		t.Fatalf("CreateSnapshot %s: %v", snap.ID, err)
	}
	if ready {
		if got, err = s.SnapshotReady(ctx, snap.ID, 1<<30, testMetadata); err != nil {
			t.Fatalf("SnapshotReady %s: %v", snap.ID, err)
		}
	}
	return got
}

func vmSnapshot(id, vmID string) Snapshot {
	return Snapshot{ID: id, Kind: SnapshotKindVM, VMID: vmID, Image: "base", VCPUs: 2, MemoryMiB: 1024}
}

func template(id, image string, memoryMiB int) Snapshot {
	return Snapshot{ID: id, Kind: SnapshotKindTemplate, Image: image, VCPUs: 2, MemoryMiB: memoryMiB}
}

func destroy(t *testing.T, s *Store, id string) {
	t.Helper()
	ctx := context.Background()
	if _, err := s.Transition(ctx, id, StateDestroying, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(ctx, id, StateDestroyed, ""); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotLifecycle(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	mustCreate(t, s, newVM("vm-parent", 0))

	snap := mustSnapshot(t, s, vmSnapshot("snap-1", "vm-parent"), false)
	if snap.State != SnapshotCreating || !snap.CreatedAt.Equal(c.now()) {
		t.Errorf("new snapshot = %+v, want creating, created now", snap)
	}
	// Nothing can be restored from a snapshot that isn't ready.
	child := newVM("vm-child", 1)
	child.SnapshotID = "snap-1"
	if _, err := s.CreateVM(ctx, child); !errors.Is(err, ErrNotFound) {
		t.Errorf("CreateVM from a snapshot being created = %v, want ErrNotFound", err)
	}

	c.advance(time.Second)
	snap, err := s.SnapshotReady(ctx, "snap-1", 1234, testMetadata)
	if err != nil || snap.State != SnapshotReady || snap.SizeBytes != 1234 || string(snap.Metadata) != string(testMetadata) {
		t.Fatalf("SnapshotReady = %+v, %v", snap, err)
	}
	if _, err := s.SnapshotReady(ctx, "snap-1", 1, testMetadata); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("SnapshotReady twice = %v, want ErrInvalidTransition", err)
	}
	if got, err := s.GetSnapshot(ctx, "snap-1"); err != nil || got.VMID != "vm-parent" || !got.UpdatedAt.Equal(c.now()) {
		t.Errorf("GetSnapshot = %+v, %v", got, err)
	}

	// A VM restored from it holds it: it can't be deleted.
	if got := mustCreate(t, s, child); got.SnapshotID != "snap-1" {
		t.Errorf("child's SnapshotID = %q", got.SnapshotID)
	}
	if got, _ := s.GetVM(ctx, "vm-child"); got.SnapshotID != "snap-1" {
		t.Errorf("stored SnapshotID = %q", got.SnapshotID)
	}
	if users, err := s.SnapshotUsers(ctx, "snap-1"); err != nil || fmt.Sprint(users) != "[vm-child]" {
		t.Errorf("SnapshotUsers = %v, %v", users, err)
	}
	if _, err := s.MarkSnapshotDeleting(ctx, "snap-1"); !errors.Is(err, ErrInUse) {
		t.Errorf("MarkSnapshotDeleting in use = %v, want ErrInUse", err)
	}
	if err := s.DeleteSnapshotRecord(ctx, "snap-1"); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("DeleteSnapshotRecord of a ready snapshot = %v, want ErrInvalidTransition", err)
	}

	// Once that VM is destroyed, it can.
	destroy(t, s, "vm-child")
	if snap, err = s.MarkSnapshotDeleting(ctx, "snap-1"); err != nil || snap.State != SnapshotDeleting {
		t.Fatalf("MarkSnapshotDeleting = %+v, %v", snap, err)
	}
	if snap, err = s.MarkSnapshotDeleting(ctx, "snap-1"); err != nil || snap.State != SnapshotDeleting {
		t.Errorf("MarkSnapshotDeleting again = %+v, %v; want it unchanged", snap, err)
	}
	another := newVM("vm-late", 2)
	another.SnapshotID = "snap-1"
	if _, err := s.CreateVM(ctx, another); !errors.Is(err, ErrNotFound) {
		t.Errorf("CreateVM from a snapshot being deleted = %v, want ErrNotFound", err)
	}
	if err := s.DeleteSnapshotRecord(ctx, "snap-1"); err != nil {
		t.Fatalf("DeleteSnapshotRecord: %v", err)
	}
	if _, err := s.GetSnapshot(ctx, "snap-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetSnapshot after delete = %v, want ErrNotFound", err)
	}
	if err := s.DeleteSnapshotRecord(ctx, "snap-1"); err != nil {
		t.Errorf("DeleteSnapshotRecord twice = %v, want nil", err)
	}
	if _, err := s.MarkSnapshotDeleting(ctx, "snap-nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("MarkSnapshotDeleting of nothing = %v, want ErrNotFound", err)
	}
}

func TestSnapshotValidation(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	if _, err := s.CreateSnapshot(ctx, Snapshot{ID: "bad id!", Kind: SnapshotKindVM, Image: "base", VCPUs: 1, MemoryMiB: 1}); err == nil {
		t.Error("bad ID: want an error")
	}
	if _, err := s.CreateSnapshot(ctx, Snapshot{ID: "snap-1", Kind: "other", Image: "base", VCPUs: 1, MemoryMiB: 1}); err == nil {
		t.Error("bad kind: want an error")
	}
	mustSnapshot(t, s, vmSnapshot("snap-1", "vm-x"), false)
	if _, err := s.CreateSnapshot(ctx, vmSnapshot("snap-1", "vm-x")); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate ID = %v, want ErrConflict", err)
	}
	if _, err := s.SnapshotReady(ctx, "snap-1", 1, json.RawMessage("nope")); err == nil {
		t.Error("SnapshotReady with bad metadata: want an error")
	}
}

func TestUpdateVMSnapshotMustBeReady(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	mustCreate(t, s, newVM("vm-1", 0))
	mustSnapshot(t, s, vmSnapshot("snap-wip", "vm-1"), false)
	mustSnapshot(t, s, vmSnapshot("snap-ok", "vm-1"), true)

	set := func(id string) error {
		_, err := s.UpdateVM(ctx, "vm-1", "rolled back", func(vm *VM) error { vm.SnapshotID = id; return nil })
		return err
	}
	if err := set("snap-wip"); !errors.Is(err, ErrNotFound) {
		t.Errorf("SnapshotID = a snapshot being created: %v, want ErrNotFound", err)
	}
	if err := set("snap-ok"); err != nil {
		t.Errorf("SnapshotID = a ready snapshot: %v", err)
	}
	if err := set(""); err != nil {
		t.Errorf("clearing SnapshotID: %v", err)
	}
}

func TestListSnapshotsAndLatestTemplate(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	mustSnapshot(t, s, template("snap-t1", "base", 1024), true)
	c.advance(time.Minute)
	mustSnapshot(t, s, template("snap-t2", "base", 1024), true)
	c.advance(time.Minute)
	mustSnapshot(t, s, template("snap-t3", "base", 1024), false) // still being made
	mustSnapshot(t, s, template("snap-big", "base", 4096), true)
	mustSnapshot(t, s, vmSnapshot("snap-v", "vm-1"), true)

	got, err := s.LatestTemplate(ctx, "base", 2, 1024)
	if err != nil || got.ID != "snap-t2" {
		t.Errorf("LatestTemplate(base, 2, 1024) = %s, %v; want snap-t2, the newest ready one", got.ID, err)
	}
	if got, err := s.LatestTemplate(ctx, "base", 2, 4096); err != nil || got.ID != "snap-big" {
		t.Errorf("LatestTemplate(base, 2, 4096) = %s, %v; want snap-big", got.ID, err)
	}
	if _, err := s.LatestTemplate(ctx, "base", 4, 1024); !errors.Is(err, ErrNotFound) {
		t.Errorf("LatestTemplate of another size = %v, want ErrNotFound", err)
	}

	list := func(f SnapshotFilter) string {
		t.Helper()
		snaps, err := s.ListSnapshots(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		out := ""
		for _, sn := range snaps {
			out += sn.ID + " "
		}
		return out
	}
	if got := list(SnapshotFilter{}); got != "snap-t1 snap-t2 snap-big snap-t3 snap-v " {
		t.Errorf("all = %q", got)
	}
	if got := list(SnapshotFilter{VMID: "vm-1"}); got != "snap-v " {
		t.Errorf("for vm-1 = %q", got)
	}
	if got := list(SnapshotFilter{Kind: SnapshotKindTemplate, Image: "base"}); got != "snap-t1 snap-t2 snap-big snap-t3 " {
		t.Errorf("templates = %q", got)
	}
}

func TestGarbageSnapshots(t *testing.T) {
	s, c := openTest(t)
	ctx := context.Background()
	for _, name := range []string{"base", "python"} {
		if _, err := s.PutImage(ctx, Image{Name: name, Dir: "/srv/tine/images/" + name, Metadata: testMetadata}); err != nil {
			t.Fatal(err)
		}
	}
	mustCreate(t, s, newVM("vm-live", 0))
	mustCreate(t, s, newVM("vm-gone", 1))
	destroy(t, s, "vm-gone")

	// VM snapshots.
	mustSnapshot(t, s, vmSnapshot("snap-of-live", "vm-live"), true)      // its VM is alive: kept
	mustSnapshot(t, s, vmSnapshot("snap-of-gone", "vm-gone"), true)      // its VM is gone: garbage
	mustSnapshot(t, s, vmSnapshot("snap-of-gone-used", "vm-gone"), true) // VM gone, but a VM runs from it: kept
	mustSnapshot(t, s, vmSnapshot("snap-of-gone-wip", "vm-gone"), false) // still being made: never garbage
	user := newVM("vm-user", 2)
	user.SnapshotID = "snap-of-gone-used"
	mustCreate(t, s, user)

	// Templates.
	mustSnapshot(t, s, template("snap-old", "base", 1024), true) // replaced: garbage
	c.advance(time.Minute)
	mustSnapshot(t, s, template("snap-old-used", "base", 1024), true) // replaced, but in use: kept
	user2 := newVM("vm-user2", 3)
	user2.SnapshotID = "snap-old-used"
	mustCreate(t, s, user2)
	c.advance(time.Minute)
	mustSnapshot(t, s, template("snap-new", "base", 1024), true)     // the current one: kept
	mustSnapshot(t, s, template("snap-other", "base", 512), true)    // the current one for its size: kept
	mustSnapshot(t, s, template("snap-orphan", "gone", 1024), true)  // its image is gone: the caller's to find
	mustSnapshot(t, s, template("snap-py", "python", 1024), true)    // kept
	mustSnapshot(t, s, template("snap-dying", "python", 1024), true) // being deleted: returned to finish
	if _, err := s.MarkSnapshotDeleting(ctx, "snap-dying"); err != nil {
		t.Fatal(err)
	}

	garbage, err := s.GarbageSnapshots(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, g := range garbage {
		got = append(got, g.ID)
	}
	if want := "[snap-of-gone snap-old snap-dying]"; fmt.Sprint(got) != want {
		t.Errorf("GarbageSnapshots = %v, want %s", got, want)
	}

	// Once its user is destroyed, the replaced template becomes garbage too.
	destroy(t, s, "vm-user2")
	garbage, _ = s.GarbageSnapshots(ctx)
	found := false
	for _, g := range garbage {
		found = found || g.ID == "snap-old-used"
	}
	if !found {
		t.Error("a replaced template whose last user is destroyed isn't garbage")
	}
}

// Deleting a snapshot and restoring a VM from it race safely: exactly one wins.
func TestDeleteAndRestoreRace(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	for i := range 20 {
		id := fmt.Sprintf("snap-%d", i)
		mustSnapshot(t, s, vmSnapshot(id, "vm-x"), true)
		vm := newVM(fmt.Sprintf("vm-%d", i), i)
		vm.SnapshotID = id

		var wg sync.WaitGroup
		var createErr, deleteErr error
		wg.Add(2)
		go func() { defer wg.Done(); _, createErr = s.CreateVM(ctx, vm) }()
		go func() { defer wg.Done(); _, deleteErr = s.MarkSnapshotDeleting(ctx, id) }()
		wg.Wait()

		switch {
		case createErr == nil && deleteErr == nil:
			t.Fatalf("round %d: the VM was created from a snapshot that was then deleted", i)
		case createErr == nil && !errors.Is(deleteErr, ErrInUse):
			t.Errorf("round %d: delete lost with %v, want ErrInUse", i, deleteErr)
		case deleteErr == nil && !errors.Is(createErr, ErrNotFound):
			t.Errorf("round %d: create lost with %v, want ErrNotFound", i, createErr)
		case createErr != nil && deleteErr != nil:
			t.Errorf("round %d: both failed: %v; %v", i, createErr, deleteErr)
		}
	}
}

// Migration 3 rebuilds the snapshots table: what's in it must survive.
func TestMigrationToForksKeepsSnapshots(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir() + "/tine.db"
	upTo2 := fstest.MapFS{}
	for _, name := range []string{"migrations/0001_init.sql", "migrations/0002_snapshots.sql"} {
		data, err := fs.ReadFile(migrationFiles, name)
		if err != nil {
			t.Fatal(err)
		}
		upTo2[name] = &fstest.MapFile{Data: data}
	}
	old, err := open(ctx, path, upTo2)
	if err != nil {
		t.Fatal(err)
	}
	mustSnapshot(t, old, template("snap-tmpl", "base", 1024), true)
	_ = old.Close()

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("migrating to 3: %v", err)
	}
	defer func() { _ = s.Close() }()
	got, err := s.GetSnapshot(ctx, "snap-tmpl")
	if err != nil || got.State != SnapshotReady || got.Kind != SnapshotKindTemplate || string(got.Metadata) != string(testMetadata) {
		t.Errorf("after the migration: %+v, %v", got, err)
	}
	if _, err := s.LatestTemplate(ctx, "base", 2, 1024); err != nil {
		t.Errorf("the template index went missing: %v", err)
	}
	if _, err := s.CreateSnapshot(ctx, Snapshot{ID: "snap-fork", Kind: SnapshotKindFork, VMID: "vm-p", Image: "base", VCPUs: 2, MemoryMiB: 1024}); err != nil {
		t.Errorf("a fork snapshot after the migration: %v", err)
	}
}

func TestForkSnapshotsAreGarbageOnceUnused(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	mustCreate(t, s, newVM("vm-parent", 0))
	mustSnapshot(t, s, Snapshot{ID: "snap-fork", Kind: SnapshotKindFork, VMID: "vm-parent", Image: "base", VCPUs: 2, MemoryMiB: 1024}, true)
	// Unused, although its parent lives: garbage.
	if g, _ := s.GarbageSnapshots(ctx); len(g) != 1 || g[0].ID != "snap-fork" {
		t.Errorf("garbage = %+v, want the unused fork snapshot", g)
	}
	child := newVM("vm-child", 1)
	child.SnapshotID, child.ParentID, child.Source = "snap-fork", "vm-parent", SourceSnapshot
	got := mustCreate(t, s, child)
	if got, _ := s.GetVM(ctx, got.ID); got.ParentID != "vm-parent" {
		t.Errorf("ParentID = %q", got.ParentID)
	}
	if g, _ := s.GarbageSnapshots(ctx); len(g) != 0 {
		t.Errorf("garbage with a child running = %+v", g)
	}
	if _, err := s.UpdateVM(ctx, "vm-child", "", func(v *VM) error { v.ParentID = "vm-other"; return nil }); err == nil {
		t.Error("changing a VM's parent: want an error")
	}
}
