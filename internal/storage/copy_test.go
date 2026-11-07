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
