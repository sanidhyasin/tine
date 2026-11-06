package idalloc

import (
	"errors"
	"sync"
	"testing"
)

func mustRange(t *testing.T, first, size int) *Range {
	t.Helper()
	r, err := NewRange(first, size)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAllocateHandsOutEveryNumberOnce(t *testing.T) {
	r := mustRange(t, 100, 3)
	seen := map[int]bool{}
	for i := 0; i < 3; i++ {
		n, err := r.Allocate()
		if err != nil {
			t.Fatalf("Allocate #%d: %v", i, err)
		}
		if n < 100 || n >= 103 || seen[n] {
			t.Fatalf("Allocate #%d = %d: out of range or repeated", i, n)
		}
		seen[n] = true
	}
	if _, err := r.Allocate(); !errors.Is(err, ErrExhausted) {
		t.Errorf("Allocate on a full range = %v, want ErrExhausted", err)
	}
}

func TestReleasedNumbersAreNotReusedImmediately(t *testing.T) {
	r := mustRange(t, 0, 3)
	a, _ := r.Allocate() // 0
	r.Release(a)
	b, _ := r.Allocate()
	if b == a {
		t.Errorf("got %d again straight after releasing it; want the next free number", b)
	}
	// Once the others are used, the released number comes back.
	c, _ := r.Allocate()
	d, err := r.Allocate()
	if err != nil || d != a {
		t.Errorf("after %d and %d, Allocate = %d, %v; want %d", b, c, d, err, a)
	}
}

func TestReserve(t *testing.T) {
	r := mustRange(t, 10, 5)
	if err := r.Reserve(12); err != nil {
		t.Fatalf("Reserve(12): %v", err)
	}
	if err := r.Reserve(12); err == nil {
		t.Error("Reserve(12) twice: want an error")
	}
	if err := r.Reserve(15); err == nil {
		t.Error("Reserve(15), outside [10, 15): want an error")
	}
	for i := 0; i < 4; i++ {
		n, err := r.Allocate()
		if err != nil || n == 12 {
			t.Fatalf("Allocate = %d, %v; must never return the reserved 12", n, err)
		}
	}
	if r.InUse() != 5 {
		t.Errorf("InUse = %d, want 5", r.InUse())
	}
}

func TestNewRangeValidates(t *testing.T) {
	for _, tc := range []struct{ first, size int }{{0, 0}, {0, -1}, {-1, 10}} {
		if _, err := NewRange(tc.first, tc.size); err == nil {
			t.Errorf("NewRange(%d, %d): want an error", tc.first, tc.size)
		}
	}
}

func TestConcurrentAllocateNeverDuplicates(t *testing.T) {
	r := mustRange(t, 200000, 1000)
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		seen = map[int]bool{}
	)
	for g := 0; g < 10; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				n, err := r.Allocate()
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				if seen[n] {
					t.Errorf("%d handed out twice", n)
				}
				seen[n] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != 1000 {
		t.Errorf("handed out %d numbers, want 1000", len(seen))
	}
}
