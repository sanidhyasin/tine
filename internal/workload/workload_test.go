package workload

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFillAndTouch(t *testing.T) {
	mem, err := Alloc(4)
	if err != nil {
		t.Fatal(err)
	}
	Fill(mem, 42)
	seen := map[byte]bool{}
	for off := 0; off < len(mem); off += PageSize {
		if mem[off] == 0 {
			t.Fatalf("page at %d is still zero", off)
		}
		seen[mem[off]] = true
	}
	if len(seen) < 2 {
		t.Error("every page got the same value")
	}
	r := Touch(mem, true)
	if r.Mode != "write" || r.Pages != 1024 || r.NsPerPage <= 0 || r.P50Ns > r.MaxNs {
		t.Errorf("Touch = %+v", r)
	}
	if r := Touch(mem, false); r.Mode != "read" || r.Pages != 1024 {
		t.Errorf("Touch(read) = %+v", r)
	}
	if _, err := Alloc(0); err == nil {
		t.Error("Alloc(0): want an error")
	}
}

func TestPIDFiles(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x", "pid")
	if err := WritePID(p); err != nil {
		t.Fatal(err)
	}
	if pid, err := ReadPID(p); err != nil || pid != os.Getpid() {
		t.Errorf("ReadPID = %d, %v", pid, err)
	}
}
