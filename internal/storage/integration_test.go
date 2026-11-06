//go:build integration

package storage

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sanidhyasin/tine/internal/testenv"
)

// TestCloneOnTheDataVolume checks real reflink clones on the dev server's XFS
// volume: they're fast, and changing a clone leaves the original alone.
func TestCloneOnTheDataVolume(t *testing.T) {
	testenv.RequireRoot(t)
	dir := testenv.TempDir(t, testenv.RequireDataDir(t))
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")

	data := make([]byte, 64<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, data, 0o644); err != nil { //nolint:gosec // test file
		t.Fatal(err)
	}

	start := time.Now()
	if err := Clone(src, dst, 0o600); err != nil {
		t.Fatalf("Clone on XFS: %v", err)
	}
	took := time.Since(start)
	checkClone(t, src, dst, data, 0o600)

	// Copy-on-write: writing to the clone must not change the original.
	f, err := os.OpenFile(dst, os.O_WRONLY, 0) //nolint:gosec // test file
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("changed"), 0); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	got, _ := os.ReadFile(src) //nolint:gosec // test file
	if !bytes.Equal(got, data) {
		t.Error("writing to the clone changed the original")
	}

	t.Logf("cloned 64 MiB in %v", took.Round(time.Microsecond))
	if took > time.Second {
		t.Errorf("clone took %v; a reflink should take milliseconds", took)
	}
}
