package storage

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestCopy(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	data := bytes.Repeat([]byte("tine"), 3<<18+5) // more than one buffer
	if err := os.WriteFile(src, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Copy(src, dst, 0o400); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	got, err := os.ReadFile(dst) //nolint:gosec // a test file
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("copy differs (%v)", err)
	}
	a, _ := os.Stat(src)
	b, _ := os.Stat(dst)
	if os.SameFile(a, b) || b.Mode().Perm() != 0o400 {
		t.Errorf("dst: same file %v, mode %v; want a separate file with mode 0400", os.SameFile(a, b), b.Mode().Perm())
	}
	if err := Copy(src, dst, 0o400); err == nil {
		t.Error("Copy over an existing file succeeded")
	}
	if err := Copy(filepath.Join(dir, "nope"), filepath.Join(dir, "x"), 0o400); err == nil {
		t.Error("Copy of a missing file succeeded")
	}
	if _, err := os.Stat(filepath.Join(dir, "x")); !os.IsNotExist(err) {
		t.Error("a failed Copy left its destination behind")
	}
}

func TestApplyDiff(t *testing.T) {
	dir := t.TempDir()
	if err := CheckHoles(dir); err != nil {
		t.Skipf("this filesystem can't test diffs: %v", err)
	}
	const size = 8 << 20
	base := bytes.Repeat([]byte{0xAA}, size)
	dst := filepath.Join(dir, "memory")
	if err := os.WriteFile(dst, base, 0o600); err != nil {
		t.Fatal(err)
	}
	// A diff as Firecracker writes one: the whole size, with only the
	// changed pages written, one of them all zeros.
	diff := filepath.Join(dir, "diff")
	f, err := os.Create(diff) //nolint:gosec // a test file
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	want := bytes.Clone(base)
	for _, w := range []struct {
		at int64
		b  byte
		n  int
	}{{0, 1, 4096}, {1 << 20, 0, 4096}, {3 << 20, 2, 3 << 20}, {size - 4096, 3, 4096}} {
		page := bytes.Repeat([]byte{w.b}, w.n)
		if _, err := f.WriteAt(page, w.at); err != nil {
			t.Fatal(err)
		}
		copy(want[w.at:], page)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	n, err := ApplyDiff(dst, diff)
	if err != nil {
		t.Fatalf("ApplyDiff: %v", err)
	}
	got, _ := os.ReadFile(dst) //nolint:gosec // a test file
	if !bytes.Equal(got, want) {
		t.Error("the merged file differs from the base with the diff's pages")
	}
	if wantN := int64(3*4096 + 3<<20); n != wantN {
		t.Errorf("wrote %d bytes, want %d: only the diff's data", n, wantN)
	}

	// A diff of nothing but holes changes nothing.
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(empty, size); err != nil {
		t.Fatal(err)
	}
	if n, err := ApplyDiff(dst, empty); err != nil || n != 0 {
		t.Errorf("an empty diff: %d bytes, %v", n, err)
	}
	// A diff longer than the file is refused.
	if err := os.Truncate(empty, size+4096); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyDiff(dst, empty); err == nil {
		t.Error("a diff longer than its base was applied")
	}
}
