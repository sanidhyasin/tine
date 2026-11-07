package vm

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func diffFixture(t *testing.T, merge string) *fixture {
	t.Helper()
	return newFixture(t, func(c *Config) { c.DiffSnapshots, c.DiffMerge = true, merge })
}

// writes lists the writeSnapshot calls since the last takeCalls.
func (f *fakeHost) writes() string {
	var w []string
	for _, c := range f.takeCalls() {
		if strings.HasPrefix(c, "writeSnapshot") {
			w = append(w, c)
		}
	}
	return strings.Join(w, ",")
}

func TestDiffSnapshots(t *testing.T) {
	ctx := context.Background()
	f := diffFixture(t, "")
	vm := f.running(t) // booted: no base yet
	if base, ok := f.m.diffBaseOf(vm.ID); !ok || base != "" {
		t.Fatalf("a booted VM's base = %q, %v; want tracked, with none", base, ok)
	}

	first, err := f.m.Snapshot(ctx, vm.ID)
	if err != nil {
		t.Fatal(err)
	}
	if w := f.host.writes(); w != "writeSnapshot" {
		t.Errorf("first snapshot: %s, want a full one", w)
	}
	second, err := f.m.Snapshot(ctx, vm.ID)
	if err != nil {
		t.Fatal(err)
	}
	if w := f.host.writes(); w != "writeSnapshot diff" {
		t.Errorf("second snapshot: %s, want a diff", w)
	}
	meta, err := f.m.snaps.Load(second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Base != first.ID || meta.DiffBytes != 4096 || meta.TimingsMs["merge"] <= 0 {
		t.Errorf("second snapshot: base %q, diff %d bytes, timings %v; want base %s", meta.Base, meta.DiffBytes, meta.TimingsMs, first.ID)
	}
	last := f.host.merges[len(f.host.merges)-1]
	if last.BaseID != first.ID || last.Copy {
		t.Errorf("merge = %+v, want the first snapshot reflinked", last)
	}

	// A fork's snapshot is a diff too, copied through the page cache, and
	// its children's base is it.
	res, err := f.m.Fork(ctx, vm.ID, ForkRequest{Count: 1})
	if err != nil {
		t.Fatal(err)
	}
	if last := f.host.merges[len(f.host.merges)-1]; last.BaseID != second.ID || !last.Copy {
		t.Errorf("fork merge = %+v, want the second snapshot copied", last)
	}
	if res.Timings["merge"] <= 0 {
		t.Errorf("fork timings %v, want merge", res.Timings)
	}
	if base, _ := f.m.diffBaseOf(res.Children[0].VM.ID); base != res.Snapshot.ID {
		t.Errorf("child's base = %q, want the fork's snapshot %s", base, res.Snapshot.ID)
	}
	if base, _ := f.m.diffBaseOf(vm.ID); base != res.Snapshot.ID {
		t.Errorf("parent's base = %q, want the fork's snapshot %s", base, res.Snapshot.ID)
	}

	// A restored VM's first snapshot is a diff of the one it came from.
	restored, err := f.m.Create(ctx, CreateRequest{SnapshotID: second.ID})
	if err != nil {
		t.Fatal(err)
	}
	f.host.takeCalls()
	if _, err := f.m.Snapshot(ctx, restored.ID); err != nil {
		t.Fatal(err)
	}
	if w, last := f.host.writes(), f.host.merges[len(f.host.merges)-1]; w != "writeSnapshot diff" || last.BaseID != second.ID {
		t.Errorf("restored VM's first snapshot: %s from %q, want a diff of %s", w, last.BaseID, second.ID)
	}
	// After a rollback, the base is the snapshot rolled back to.
	if _, err := f.m.Rollback(ctx, restored.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	if base, _ := f.m.diffBaseOf(restored.ID); base != second.ID {
		t.Errorf("base after a rollback = %q, want %s", base, second.ID)
	}
	if _, err := f.m.Destroy(ctx, restored.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.m.diffBaseOf(restored.ID); ok {
		t.Error("a destroyed VM is still tracked")
	}
}

func TestDiffSnapshotFallsBackToFull(t *testing.T) {
	ctx := context.Background()
	f := diffFixture(t, "")
	vm := f.running(t)
	first, err := f.m.Snapshot(ctx, vm.ID)
	if err != nil {
		t.Fatal(err)
	}

	// A snapshot that fails once Firecracker began writing it leaves the
	// tracking unknown: the next one is full.
	f.host.fail("cloneDisk", errors.New("disk trouble"))
	if _, err := f.m.Snapshot(ctx, vm.ID); err == nil {
		t.Fatal("snapshot with a failing disk clone succeeded")
	}
	f.host.fail("cloneDisk", nil)
	f.host.takeCalls()
	second, err := f.m.Snapshot(ctx, vm.ID)
	if err != nil {
		t.Fatal(err)
	}
	if w := f.host.writes(); w != "writeSnapshot" {
		t.Errorf("after a failed snapshot: %s, want a full one", w)
	}

	// So is one whose base was deleted.
	if _, err := f.m.DeleteSnapshot(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Snapshot(ctx, vm.ID); err != nil {
		t.Fatal(err)
	}
	if w := f.host.writes(); w != "writeSnapshot" {
		t.Errorf("with the base deleted: %s, want a full one", w)
	}
	if meta, err := f.m.snaps.Load(first.ID); err != nil || meta.Base != "" {
		t.Errorf("the first snapshot: base %q, %v; want a full one", meta.Base, err)
	}
}

func TestDiffSnapshotsOffAndMergeChoice(t *testing.T) {
	ctx := context.Background()
	// Off: never tracked, always full.
	f := newFixture(t, nil)
	vm := f.running(t)
	for range 2 {
		if _, err := f.m.Snapshot(ctx, vm.ID); err != nil {
			t.Fatal(err)
		}
	}
	if w := f.host.writes(); w != "writeSnapshot,writeSnapshot" {
		t.Errorf("without diff snapshots: %s", w)
	}
	if _, ok := f.m.diffBaseOf(vm.ID); ok {
		t.Error("a VM is tracked with diff snapshots off")
	}

	// DiffMerge decides for forks too.
	for merge, wantCopy := range map[string]bool{mergeReflink: false, mergeCopy: true} {
		f := diffFixture(t, merge)
		vm := f.running(t)
		if _, err := f.m.Snapshot(ctx, vm.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.m.Fork(ctx, vm.ID, ForkRequest{Count: 1}); err != nil {
			t.Fatal(err)
		}
		if last := f.host.merges[len(f.host.merges)-1]; last.Base == "" || last.Copy != wantCopy {
			t.Errorf("DiffMerge %q: fork merge = %+v, want copy %v", merge, last, wantCopy)
		}
	}
	if _, err := newManager(Config{DiffMerge: "teleport"}, f.store, newFakeHost()); err == nil {
		t.Error("an unknown DiffMerge was accepted")
	}
}
