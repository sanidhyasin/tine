package snapshot

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "snapshots"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// build gathers a snapshot with small stand-in files, as tined would after
// Firecracker wrote them.
func build(t *testing.T, s *Store, id string) *Building {
	t.Helper()
	b, err := s.Begin(id)
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	for _, f := range []string{StateFile, MemoryFile, DiskFile} {
		if err := os.WriteFile(b.Path(f), []byte(strings.Repeat(f, 1000)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

var testHost = Host{Firecracker: "1.17.0", CPU: "Intel(R) Xeon(R) 6975P-C", Kernel: "6.8.0-1015-aws"}

func TestCommitAndLoad(t *testing.T) {
	s := newStore(t)
	if fi, err := os.Stat(s.Dir()); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("store dir: %v, %v; want mode 0700", fi, err)
	}
	b := build(t, s, "snap-1")
	meta := Metadata{Kind: "vm", VMID: "vm-1", Image: "base", VCPUs: 2, MemoryMiB: 1024, Host: testHost,
		CreatedAt: time.Now().UTC(), TimingsMs: map[string]float64{"pause": 1}}
	size, err := b.Commit(&meta)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if size <= 0 || meta.Bytes[MemoryFile] <= 0 || meta.ID != "snap-1" || meta.FormatVersion != FormatVersion {
		t.Errorf("Commit = %d, meta %+v", size, meta)
	}
	if _, err := os.Stat(b.dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the building directory is still there: %v", err)
	}
	for file, want := range map[string]os.FileMode{StateFile: 0o444, MemoryFile: 0o444, DiskFile: 0o400, MetadataFile: 0o444} {
		if fi, err := os.Stat(s.Path("snap-1", file)); err != nil || fi.Mode().Perm() != want {
			t.Errorf("%s: %v, %v; want mode %v", file, fi, err, want)
		}
	}

	got, err := s.Load("snap-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.VMID != "vm-1" || got.Host != testHost || got.TimingsMs["pause"] != 1 || got.Bytes[DiskFile] != meta.Bytes[DiskFile] {
		t.Errorf("Load = %+v, want what was committed: %+v", got, meta)
	}
	if ids, building, err := s.List(); err != nil || len(ids) != 1 || ids[0] != "snap-1" || len(building) != 0 {
		t.Errorf("List = %v, %v, %v", ids, building, err)
	}
}

func TestCommitNeedsEveryFile(t *testing.T) {
	s := newStore(t)
	b := build(t, s, "snap-1")
	if err := os.Remove(b.Path(MemoryFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Commit(&Metadata{}); err == nil {
		t.Fatal("Commit without a memory file: want an error")
	}
	if _, err := s.Load("snap-1"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Load of a failed snapshot = %v, want ErrNotExist", err)
	}
	if _, building, _ := s.List(); len(building) != 1 || building[0] != "snap-1" {
		t.Errorf("List's building = %v, want the failed attempt, until it's aborted", building)
	}
	if err := b.Abort(); err != nil {
		t.Fatal(err)
	}
	if _, building, _ := s.List(); len(building) != 0 {
		t.Errorf("after Abort, building = %v", building)
	}
}

func TestBeginReplacesALeftoverButNotASnapshot(t *testing.T) {
	s := newStore(t)
	build(t, s, "snap-1") // never committed, as if tined died
	b := build(t, s, "snap-1")
	if _, err := os.Stat(b.Path(StateFile)); err != nil {
		t.Fatalf("the new attempt lost its file: %v", err)
	}
	if _, err := b.Commit(&Metadata{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Begin("snap-1"); !errors.Is(err, fs.ErrExist) {
		t.Errorf("Begin of an existing snapshot = %v, want ErrExist", err)
	}
	for _, id := range []string{"", "../x", "Snap-1", "a/b", strings.Repeat("a", 65)} {
		if _, err := s.Begin(id); err == nil {
			t.Errorf("Begin(%q): want an error", id)
		}
	}
}

func TestRemove(t *testing.T) {
	s := newStore(t)
	if _, err := build(t, s, "snap-1").Commit(&Metadata{}); err != nil {
		t.Fatal(err)
	}
	build(t, s, "snap-2")
	// A VM restored from it holds a hard link to its memory, which survives.
	link := filepath.Join(t.TempDir(), "memory")
	if err := os.Link(s.Path("snap-1", MemoryFile), link); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"snap-1", "snap-2", "snap-1"} {
		if err := s.Remove(id); err != nil {
			t.Errorf("Remove(%s): %v", id, err)
		}
	}
	if ids, building, _ := s.List(); len(ids)+len(building) != 0 {
		t.Errorf("after Remove: %v, %v", ids, building)
	}
	if _, err := os.Stat(link); err != nil {
		t.Errorf("a VM's hard link went with the snapshot: %v", err)
	}
}

func TestLoadRejectsBadMetadata(t *testing.T) {
	s := newStore(t)
	if _, err := build(t, s, "snap-1").Commit(&Metadata{}); err != nil {
		t.Fatal(err)
	}
	path := s.Path("snap-1", MetadataFile)
	write := func(v any) {
		t.Helper()
		data, _ := json.Marshal(v)
		_ = os.Chmod(path, 0o600)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(Metadata{FormatVersion: 99, ID: "snap-1"})
	if _, err := s.Load("snap-1"); err == nil || !strings.Contains(err.Error(), "format version") {
		t.Errorf("newer format: %v", err)
	}
	write(Metadata{FormatVersion: FormatVersion, ID: "snap-other"})
	if _, err := s.Load("snap-1"); err == nil {
		t.Error("metadata for another ID: want an error")
	}
	write(Metadata{FormatVersion: FormatVersion, ID: "snap-1"})
	_ = os.Remove(s.Path("snap-1", DiskFile))
	if _, err := s.Load("snap-1"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing disk = %v, want ErrNotExist", err)
	}
}

func TestCheckCompatible(t *testing.T) {
	meta := Metadata{ID: "snap-1", Host: testHost}
	if err := meta.CheckCompatible(testHost); err != nil {
		t.Errorf("same host: %v", err)
	}
	for name, h := range map[string]Host{
		"Firecracker": {Firecracker: "1.18.0", CPU: testHost.CPU, Kernel: testHost.Kernel},
		"CPU":         {Firecracker: testHost.Firecracker, CPU: "AMD EPYC 9R14", Kernel: testHost.Kernel},
		"kernel":      {Firecracker: testHost.Firecracker, CPU: testHost.CPU, Kernel: "6.8.0-2000-aws"},
	} {
		err := meta.CheckCompatible(h)
		if !errors.Is(err, ErrIncompatible) || !strings.Contains(err.Error(), name) {
			t.Errorf("different %s: %v, want ErrIncompatible naming it", name, err)
		}
	}
}

func TestThisHost(t *testing.T) {
	dir := t.TempDir()
	oldCPU, oldRelease := cpuinfoPath, osreleasePath
	t.Cleanup(func() { cpuinfoPath, osreleasePath = oldCPU, oldRelease })
	cpuinfoPath, osreleasePath = filepath.Join(dir, "cpuinfo"), filepath.Join(dir, "osrelease")
	_ = os.WriteFile(cpuinfoPath, []byte("processor\t: 0\nvendor_id\t: GenuineIntel\nmodel name\t: Intel(R) Xeon(R) 6975P-C\nflags\t: fpu\n\nprocessor\t: 1\nmodel name\t: Intel(R) Xeon(R) 6975P-C\n"), 0o600)
	_ = os.WriteFile(osreleasePath, []byte("6.8.0-1015-aws\n"), 0o600)
	h, err := ThisHost("1.17.0")
	if err != nil || h != testHost {
		t.Errorf("ThisHost = %+v, %v; want %+v", h, err, testHost)
	}
	_ = os.WriteFile(cpuinfoPath, []byte("processor\t: 0\n"), 0o600)
	if _, err := ThisHost("1.17.0"); err == nil {
		t.Error("cpuinfo without a model name: want an error")
	}
}

func TestFreeBytes(t *testing.T) {
	if free, err := newStore(t).FreeBytes(); err != nil || free == 0 {
		t.Errorf("FreeBytes = %d, %v", free, err)
	}
}
