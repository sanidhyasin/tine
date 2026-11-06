package storage

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil { //nolint:gosec // test file
		t.Fatal(err)
	}
}

func TestLink(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	writeFile(t, src, []byte("kernel"))

	if err := Link(src, dst); err != nil {
		t.Fatalf("Link: %v", err)
	}
	a, _ := os.Stat(src)
	b, _ := os.Stat(dst)
	if !os.SameFile(a, b) {
		t.Error("dst isn't the same file as src")
	}
	if err := Link(src, dst); err == nil {
		t.Error("Link onto an existing file: want an error")
	}
}

// Clone works on XFS and btrfs. Elsewhere, including macOS and CI's ext4,
// it must fail cleanly: ErrReflinkUnsupported and no file left behind.
// The integration tests check real clones on the dev server's XFS volume.
func TestCloneSucceedsOrFailsCleanly(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	data := bytes.Repeat([]byte("tine"), 4096)
	writeFile(t, src, data)

	err := Clone(src, dst, 0o600)
	if errors.Is(err, ErrReflinkUnsupported) {
		if _, statErr := os.Stat(dst); !os.IsNotExist(statErr) {
			t.Errorf("failed clone left %s behind", dst)
		}
		t.Skipf("no reflinks on this filesystem: %v", err)
	}
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	checkClone(t, src, dst, data, 0o600)
}

func TestCloneRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	writeFile(t, src, []byte("new"))
	writeFile(t, dst, []byte("keep me"))

	if err := Clone(src, dst, 0o600); err == nil {
		t.Fatal("Clone onto an existing file: want an error")
	}
	if got, _ := os.ReadFile(dst); string(got) != "keep me" { //nolint:gosec // test file
		t.Errorf("existing file was changed to %q", got)
	}
}

func TestCloneMissingSource(t *testing.T) {
	dir := t.TempDir()
	if err := Clone(filepath.Join(dir, "nope"), filepath.Join(dir, "dst"), 0o600); err == nil {
		t.Fatal("Clone of a missing file: want an error")
	}
}

func checkClone(t *testing.T, src, dst string, want []byte, perm os.FileMode) {
	t.Helper()
	got, err := os.ReadFile(dst) //nolint:gosec // test file
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Error("clone's contents differ from the source")
	}
	a, _ := os.Stat(src)
	b, _ := os.Stat(dst)
	if os.SameFile(a, b) {
		t.Error("clone is a hard link, not a separate file")
	}
	if b.Mode().Perm() != perm {
		t.Errorf("clone's permissions = %v, want %v", b.Mode().Perm(), perm)
	}
}
