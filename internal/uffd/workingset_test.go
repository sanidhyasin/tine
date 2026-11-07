package uffd

import "testing"

func TestWorkingSet(t *testing.T) {
	w := NewWorkingSet(1<<20+1, 4096) // 257 pages, the last one partial
	if w.Pages() != 257 {
		t.Fatalf("pages = %d, want 257", w.Pages())
	}
	for _, i := range []uint64{0, 1, 2, 63, 64, 200, 256, 257, 9999} {
		w.Add(i)
	}
	if w.Len() != 7 || !w.Has(256) || w.Has(257) || w.Has(3) {
		t.Errorf("len %d; has 256 %v, 257 %v, 3 %v", w.Len(), w.Has(256), w.Has(257), w.Has(3))
	}
	type run struct{ first, n uint64 }
	var runs []run
	w.Runs(func(first, n uint64) { runs = append(runs, run{first, n}) })
	want := []run{{0, 3}, {63, 2}, {200, 1}, {256, 1}}
	if len(runs) != len(want) {
		t.Fatalf("runs = %v, want %v", runs, want)
	}
	for i := range want {
		if runs[i] != want[i] {
			t.Errorf("runs = %v, want %v", runs, want)
		}
	}

	data, _ := w.MarshalBinary()
	var got WorkingSet
	if err := got.UnmarshalBinary(data); err != nil {
		t.Fatal(err)
	}
	if got.PageSize != 4096 || got.Pages() != 257 || got.Len() != 7 || !got.Has(63) {
		t.Errorf("decoded: page size %d, %d pages, %d set", got.PageSize, got.Pages(), got.Len())
	}
	for _, bad := range [][]byte{nil, []byte("nope"), data[:len(data)-1], append([]byte("tine-ws1"), make([]byte, 16)...)} {
		if err := new(WorkingSet).UnmarshalBinary(bad); err == nil {
			t.Errorf("decoded %q", bad)
		}
	}
}
