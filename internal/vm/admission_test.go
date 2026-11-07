package vm

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/sanidhyasin/tine/internal/state"
)

func (f *fixture) reservedNothing(t *testing.T) {
	t.Helper()
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	if f.m.reservedCommitMiB != 0 || f.m.reservedUseMiB != 0 || f.m.reservedVCPUs != 0 {
		t.Errorf("still reserved: %d MiB committed, %d MiB of use, %d vCPUs", f.m.reservedCommitMiB, f.m.reservedUseMiB, f.m.reservedVCPUs)
	}
}

func TestAdmissionLimitsCommitment(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, func(c *Config) { c.OvercommitRatio = 2 })
	f.host.mem = hostMemory{TotalMiB: 1024, AvailableMiB: 8192}
	for range 2 {
		if _, err := f.m.Create(ctx, CreateRequest{MemoryMiB: 1024}); err != nil {
			t.Fatalf("Create within the limit: %v", err)
		}
	}
	_, err := f.m.Create(ctx, CreateRequest{MemoryMiB: 1024})
	if !errors.Is(err, ErrNoMemory) || !strings.Contains(err.Error(), "limit of 2048 MiB") {
		t.Errorf("Create past the limit = %v, want ErrNoMemory naming the limit", err)
	}
	f.reservedNothing(t)
}

func TestAdmissionKeepsHeadroom(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, func(c *Config) { c.MinFreeMemoryMiB = 1024; c.BootUseMiB = 192; c.RestoreUseMiB = 64 })
	_, snap := f.snapshotted(t, CreateRequest{})
	// 1100 MiB available: a boot (192 MiB) would leave too little, a restore (64 MiB) wouldn't.
	f.host.mem = hostMemory{TotalMiB: 16384, AvailableMiB: 1100}
	_, err := f.m.Create(ctx, CreateRequest{Cold: true})
	if !errors.Is(err, ErrNoMemory) || !strings.Contains(err.Error(), "1024 MiB kept free") {
		t.Errorf("a boot = %v, want ErrNoMemory saying what's kept free", err)
	}
	if _, err := f.m.Create(ctx, CreateRequest{SnapshotID: snap.ID}); err != nil {
		t.Errorf("a restore, which needs less: %v", err)
	}
	f.reservedNothing(t)
}

// A fork that doesn't fit is refused before the parent is paused.
func TestForkIsAdmittedBeforeThePause(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, func(c *Config) { c.OvercommitRatio = 1 })
	f.host.mem = hostMemory{TotalMiB: 4096, AvailableMiB: 8192}
	parent := f.running(t) // 1 GiB of the 4 committed
	if _, err := f.m.Fork(ctx, parent.ID, ForkRequest{Count: 4}); !errors.Is(err, ErrNoMemory) {
		t.Fatalf("a fork of 4 more = %v, want ErrNoMemory", err)
	}
	if calls := f.host.takeCalls(); len(calls) != 0 {
		t.Errorf("a refused fork touched the host: %v", calls)
	}
	if snaps, _ := f.m.Snapshots(ctx, state.SnapshotFilter{}); len(snaps) != 0 {
		t.Errorf("a refused fork took a snapshot: %+v", snaps)
	}
	res, err := f.m.Fork(ctx, parent.ID, ForkRequest{Count: 3})
	if err != nil || res.Failed() != 0 {
		t.Errorf("a fork that fits: %v (%d failed)", err, res.Failed())
	}
	f.reservedNothing(t)
}

// Creates at once share the room: they can't all take the last of it.
func TestConcurrentCreatesShareTheRoom(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, func(c *Config) { c.OvercommitRatio = 1 })
	f.host.mem = hostMemory{TotalMiB: 3072, AvailableMiB: 8192}
	var wg sync.WaitGroup
	var mu sync.Mutex
	made, refused := 0, 0
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.m.Create(ctx, CreateRequest{MemoryMiB: 1024})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				made++
			case errors.Is(err, ErrNoMemory):
				refused++
			default:
				t.Errorf("Create: %v", err)
			}
		}()
	}
	wg.Wait()
	if made != 3 || refused != 3 {
		t.Errorf("made %d and refused %d, want 3 and 3", made, refused)
	}
	f.reservedNothing(t)
}

func TestFailedCreatesGiveTheirRoomBack(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, func(c *Config) { c.ForkParallelism = 1 })
	parent := f.running(t)
	f.host.fail("configureAndBoot", errors.New("boom"))
	if _, err := f.m.Create(ctx, CreateRequest{Cold: true}); err == nil {
		t.Fatal("Create: want the failure")
	}
	f.host.fail("configureAndBoot", nil)
	f.host.failTimes = map[string]int{"loadSnapshot": 2}
	if res, err := f.m.Fork(ctx, parent.ID, ForkRequest{Count: 3}); err != nil || res.Failed() != 2 {
		t.Fatalf("Fork = %v, %d failed", err, res.Failed())
	}
	f.reservedNothing(t)
}

func TestAdmissionLimitsVCPUs(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, func(c *Config) { c.CPUOvercommitRatio = 2 })
	f.host.mem = hostMemory{TotalMiB: 64 << 10, AvailableMiB: 60 << 10, CPUs: 4} // room for 8 vCPUs
	parent := f.running(t)                                                       // 2 vCPUs
	if _, err := f.m.Fork(ctx, parent.ID, ForkRequest{Count: 4}); !errors.Is(err, ErrNoCPU) || !strings.Contains(err.Error(), "limit of 8") {
		t.Errorf("4 children of 2 vCPUs on top = %v, want ErrNoCPU naming the limit", err)
	}
	if res, err := f.m.Fork(ctx, parent.ID, ForkRequest{Count: 3}); err != nil || res.Failed() != 0 {
		t.Errorf("3 children, filling it: %v", err)
	}
	if _, err := f.m.Create(ctx, CreateRequest{VCPUs: 1}); !errors.Is(err, ErrNoCPU) {
		t.Errorf("one more = %v, want ErrNoCPU", err)
	}
	f.reservedNothing(t)
}
