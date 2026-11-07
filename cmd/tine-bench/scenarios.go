package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sanidhyasin/tine/internal/api"
)

// bench runs scenarios against tined and writes a record per run.
type bench struct {
	c      *api.Client
	out    *writer
	log    io.Writer
	runs   int // runs per data point, after the warm-up ones
	warmup int
	// dropCaches empties the page cache before each run of a scenario that
	// reads snapshots, to measure them as after a reboot.
	dropCaches bool
	ttl        int
}

// point runs fn warmup+runs times as one data point, writing a record for
// each, and prints a one-line summary of the key timing.
func (b *bench) point(ctx context.Context, scenario string, params map[string]any, key string, runs int,
	fn func(ctx context.Context) Record) {
	quiet(ctx)
	if runs == 0 {
		runs = b.runs
	}
	var vals []float64
	failed := 0
	for i := -b.warmup; i < runs; i++ {
		if ctx.Err() != nil {
			return
		}
		r := fn(ctx)
		r.Kind, r.Scenario, r.Params, r.Run, r.Warmup = "run", scenario, params, i, i < 0
		b.out.write(r)
		if r.Warmup {
			continue
		}
		if !r.OK {
			failed++
			continue
		}
		vals = append(vals, r.TimingsMs[key])
	}
	_, _ = fmt.Fprintf(b.log, "%-18s %-44s %s %s", scenario, describe(params), key, summary(vals))
	if failed > 0 {
		_, _ = fmt.Fprintf(b.log, "  (%d failed)", failed)
	}
	_, _ = fmt.Fprintln(b.log)
}

// create: one VM at a time, created and destroyed.
func (b *bench) create(ctx context.Context, cold bool, vcpus, memoryMiB int) {
	name := "create-template"
	if cold {
		name = "create-cold"
	}
	if b.dropCaches {
		name += "-cold-cache"
	}
	params := map[string]any{"vcpus": vcpus, "memory_mib": memoryMiB}
	b.point(ctx, name, params, "total", 0, func(ctx context.Context) Record {
		if b.dropCaches {
			dropCaches()
		}
		start := time.Now()
		vm, err := b.c.CreateVM(ctx, api.CreateVMRequest{VCPUs: vcpus, MemoryMiB: memoryMiB, TTLSeconds: b.ttl, Cold: cold})
		client := ms(time.Since(start))
		if err != nil {
			return Record{Error: err.Error()}
		}
		r := Record{OK: true, TimingsMs: copyTimings(vm.TimingsMs), Extra: map[string]any{"source": vm.Source}}
		r.TimingsMs["client"] = client
		if gone, err := b.c.DestroyVM(ctx, vm.ID); err == nil {
			r.TimingsMs["destroy"] = gone.TimingsMs["destroy"]
		}
		if !cold && vm.Source != "template" {
			r.OK, r.Error = false, "booted, not restored: no usable template of this size"
		}
		return r
	})
}

// exec: `true`, again and again, in one VM.
func (b *bench) exec(ctx context.Context) {
	vm, err := b.c.CreateVM(ctx, api.CreateVMRequest{TTLSeconds: b.ttl})
	if err != nil {
		b.fail("exec", err)
		return
	}
	defer b.destroy(vm.ID)
	b.point(ctx, "exec", map[string]any{"command": "true"}, "client", b.runs*2, func(ctx context.Context) Record {
		start := time.Now()
		st, err := b.c.Exec(ctx, vm.ID, api.ExecRequest{Args: []string{"true"}}, nil)
		client := ms(time.Since(start))
		if err != nil || st.Code != 0 {
			return Record{Error: fmt.Sprint(err, st.Code)}
		}
		return Record{OK: true, TimingsMs: map[string]float64{"client": client, "guest": st.DurationMs}}
	})
}

// snapshot: a VM of each size, snapshotted again and again. Each snapshot
// waits for the last to reach the disk (the "wait" stage), which "total"
// leaves out.
func (b *bench) snapshot(ctx context.Context, memoryMiB, runs int) {
	vm, err := b.c.CreateVM(ctx, api.CreateVMRequest{MemoryMiB: memoryMiB, Cold: true, TTLSeconds: b.ttl})
	if err != nil {
		b.fail("snapshot", err)
		return
	}
	defer b.destroy(vm.ID)
	time.Sleep(3 * time.Second)
	b.point(ctx, "snapshot", map[string]any{"memory_mib": memoryMiB}, "paused", runs, func(ctx context.Context) Record {
		start := time.Now()
		snap, err := b.c.CreateSnapshot(ctx, vm.ID)
		client := ms(time.Since(start))
		if err != nil {
			return Record{Error: err.Error()}
		}
		_, _ = b.c.DeleteSnapshot(ctx, snap.ID)
		t := copyTimings(snap.TimingsMs)
		t["client"] = client
		t["total"] -= t["wait"]
		return Record{OK: true, TimingsMs: t, Extra: map[string]any{"size_bytes": snap.SizeBytes}}
	})
}

// rollback: one VM, rolled back to one snapshot again and again.
func (b *bench) rollback(ctx context.Context) {
	vm, err := b.c.CreateVM(ctx, api.CreateVMRequest{TTLSeconds: b.ttl})
	if err != nil {
		b.fail("rollback", err)
		return
	}
	defer b.destroy(vm.ID)
	snap, err := b.c.CreateSnapshot(ctx, vm.ID)
	if err != nil {
		b.fail("rollback", err)
		return
	}
	b.point(ctx, "rollback", map[string]any{}, "rollback", 0, func(ctx context.Context) Record {
		start := time.Now()
		got, err := b.c.Rollback(ctx, vm.ID, snap.ID)
		client := ms(time.Since(start))
		if err != nil {
			return Record{Error: err.Error()}
		}
		return Record{OK: true, TimingsMs: map[string]float64{"rollback": got.TimingsMs["rollback"], "client": client}}
	})
}

// forkOptions are what one fork data point varies.
type forkOptions struct {
	Children  int
	VCPUs     int
	MemoryMiB int
	// Strategy is tined's fork strategy; empty means its default.
	Strategy string
	// DirtyMiB is memory the parent changes before the fork, with tine-dirty
	// (needs the bench image).
	DirtyMiB int
	// TouchMiB is memory the parent fills before the fork, which each child
	// then writes to with tine-touch, timing each page (needs the bench image).
	TouchMiB int
	Image    string
}

func (o forkOptions) params() map[string]any {
	p := map[string]any{"children": o.Children, "vcpus": o.VCPUs, "memory_mib": o.MemoryMiB}
	if o.Strategy != "" {
		p["strategy"] = o.Strategy
	}
	if o.DirtyMiB > 0 {
		p["dirty_mib"] = o.DirtyMiB
	}
	if o.TouchMiB > 0 {
		p["touch_mib"] = o.TouchMiB
	}
	return p
}

// fork: a fresh parent for every run, forked into Children; then each
// child's memory, and with TouchMiB, how fast each child writes to memory
// it shares with its parent.
func (b *bench) fork(ctx context.Context, o forkOptions, runs int) {
	b.point(ctx, "fork", o.params(), "total", runs, func(ctx context.Context) Record {
		parent, err := b.c.CreateVM(ctx, api.CreateVMRequest{Image: o.Image, VCPUs: o.VCPUs, MemoryMiB: o.MemoryMiB, Cold: true, TTLSeconds: b.ttl})
		if err != nil {
			return Record{Error: "parent: " + err.Error()}
		}
		ids := []string{parent.ID}
		defer func() { b.destroyAll(ids) }()
		if o.TouchMiB > 0 {
			if err := b.run(ctx, parent.ID, "tine-touch", "start", "-mb", strconv.Itoa(o.TouchMiB)); err != nil {
				return Record{Error: "tine-touch start: " + err.Error()}
			}
		}
		if o.DirtyMiB > 0 {
			if err := b.run(ctx, parent.ID, "tine-dirty", "-mb", strconv.Itoa(o.DirtyMiB)); err != nil {
				return Record{Error: "tine-dirty: " + err.Error()}
			}
		}
		if b.dropCaches {
			dropCaches()
		}
		start := time.Now()
		res, err := b.c.Fork(ctx, parent.ID, api.ForkRequest{Count: o.Children, TTLSeconds: b.ttl, Strategy: o.Strategy})
		client := ms(time.Since(start))
		if err != nil {
			return Record{Error: err.Error()}
		}
		r := Record{OK: true, TimingsMs: copyTimings(res.TimingsMs), MemoryMiB: map[string]float64{}, Extra: map[string]any{}}
		r.TimingsMs["client"] = client
		var restores []float64
		var children []string
		failed := 0
		for _, c := range res.Children {
			if c.VM == nil {
				failed++
				continue
			}
			ids = append(ids, c.VM.ID)
			children = append(children, c.VM.ID)
			restores = append(restores, c.VM.TimingsMs["total"])
		}
		if failed > 0 {
			r.OK, r.Error = false, fmt.Sprintf("%d of %d children failed", failed, o.Children)
		}
		if len(restores) > 0 {
			sort.Float64s(restores)
			r.TimingsMs["child_p50"] = restores[len(restores)/2]
			r.TimingsMs["child_max"] = restores[len(restores)-1]
		}
		if o.TouchMiB > 0 {
			b.touch(ctx, children, &r)
		}
		time.Sleep(3 * time.Second) // let the children settle before reading their memory
		b.childMemory(ctx, parent.ID, &r)
		return r
	})
}

// touch has every child write to all the memory its parent filled before
// the fork, and records the time per page: the cost of copy-on-write.
func (b *bench) touch(ctx context.Context, children []string, r *Record) {
	var mu sync.Mutex
	var perPage []float64
	var wg sync.WaitGroup
	for _, id := range children {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := b.output(ctx, id, "tine-touch", "measure")
			if err != nil {
				return
			}
			var res struct {
				NsPerPage float64 `json:"ns_per_page"`
			}
			if json.Unmarshal([]byte(out), &res) == nil {
				mu.Lock()
				perPage = append(perPage, res.NsPerPage)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(perPage) > 0 {
		sort.Float64s(perPage)
		r.Extra["touch_ns_per_page_p50"] = perPage[len(perPage)/2]
		r.Extra["touch_ns_per_page_max"] = perPage[len(perPage)-1]
	}
}

// childMemory records the children's mean RSS and PSS, and the parent's.
func (b *bench) childMemory(ctx context.Context, parentID string, r *Record) {
	vms, err := b.c.ListVMsWith(ctx, api.ListOptions{Memory: true})
	if err != nil {
		return
	}
	var rss, pss float64
	n := 0
	for _, v := range vms {
		if v.Memory == nil {
			continue
		}
		switch {
		case v.ID == parentID:
			r.MemoryMiB["parent_rss"] = mib(v.Memory.RSSBytes)
			r.MemoryMiB["parent_pss"] = mib(v.Memory.PSSBytes)
		case v.ParentID == parentID:
			rss, pss, n = rss+mib(v.Memory.RSSBytes), pss+mib(v.Memory.PSSBytes), n+1
		}
	}
	if n > 0 {
		r.MemoryMiB["child_rss"], r.MemoryMiB["child_pss"] = rss/float64(n), pss/float64(n)
	}
}

// density: one small VM, forked until admission control refuses even one
// more child.
func (b *bench) density(ctx context.Context, vcpus, memoryMiB int) {
	params := map[string]any{"vcpus": vcpus, "memory_mib": memoryMiB}
	b.point(ctx, "density", params, "children", 1, func(ctx context.Context) Record {
		parent, err := b.c.CreateVM(ctx, api.CreateVMRequest{VCPUs: vcpus, MemoryMiB: memoryMiB, Cold: true, TTLSeconds: b.ttl})
		if err != nil {
			return Record{Error: err.Error()}
		}
		ids := []string{parent.ID}
		defer func() { b.destroyAll(ids) }()
		start := time.Now()
		// Batches of 50 until one is refused, then of 10, then of 1.
		var refusal string
		for _, batch := range []int{50, 10, 1} {
			for {
				res, err := b.c.Fork(ctx, parent.ID, api.ForkRequest{Count: batch, TTLSeconds: b.ttl})
				var ae *api.Error
				if errors.As(err, &ae) && ae.StatusCode == http.StatusServiceUnavailable {
					refusal = ae.Code + ": " + ae.Message
					break
				}
				if err != nil {
					return Record{Error: err.Error()}
				}
				for _, c := range res.Children {
					if c.VM != nil {
						ids = append(ids, c.VM.ID)
					}
				}
			}
		}
		r := Record{OK: true, TimingsMs: map[string]float64{"children": float64(len(ids) - 1), "seconds": time.Since(start).Seconds()},
			MemoryMiB: map[string]float64{}, Extra: map[string]any{"refused": refusal}}
		time.Sleep(5 * time.Second)
		b.childMemory(ctx, parent.ID, &r)
		r.MemoryMiB["host_available"] = float64(memAvailableMiB())
		return r
	})
}

// --- helpers ---

func (b *bench) fail(scenario string, err error) {
	b.out.write(Record{Kind: "run", Scenario: scenario, Error: err.Error()})
	_, _ = fmt.Fprintf(b.log, "%-18s failed: %v\n", scenario, err)
}

func (b *bench) destroy(id string) { _, _ = b.c.DestroyVM(context.Background(), id) }

func (b *bench) destroyAll(ids []string) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, id := range ids {
		wg.Add(1)
		sem <- struct{}{}
		go func() { defer func() { <-sem; wg.Done() }(); b.destroy(id) }()
	}
	wg.Wait()
}

func (b *bench) run(ctx context.Context, id string, args ...string) error {
	_, err := b.output(ctx, id, args...)
	return err
}

func (b *bench) output(ctx context.Context, id string, args ...string) (string, error) {
	var out strings.Builder
	st, err := b.c.Exec(ctx, id, api.ExecRequest{Args: args, TimeoutMs: 120_000}, func(stream string, d []byte) {
		if stream == "stdout" {
			out.Write(d)
		}
	})
	if err == nil && st.Code != 0 {
		err = fmt.Errorf("exit %d", st.Code)
	}
	return out.String(), err
}

// quiet waits, up to 3 minutes, until the kernel has written dirty pages
// out: a snapshot's flush slows everything else that uses the disk.
func quiet(ctx context.Context) {
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		dirty := 0
		for _, line := range strings.Split(readTrim("/proc/meminfo"), "\n") {
			if f := strings.Fields(line); len(f) > 1 && (f[0] == "Dirty:" || f[0] == "Writeback:") {
				kb, _ := strconv.Atoi(f[1])
				dirty += kb
			}
		}
		if dirty < 32<<10 {
			return
		}
		time.Sleep(time.Second)
	}
}

// dropCaches writes dirty pages out and empties the page cache (needs root).
func dropCaches() {
	f, err := os.OpenFile("/proc/sys/vm/drop_caches", os.O_WRONLY, 0)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()
	syncAll()
	_, _ = f.WriteString("3")
}

func memAvailableMiB() int {
	for _, line := range strings.Split(readTrim("/proc/meminfo"), "\n") {
		if f := strings.Fields(line); len(f) > 1 && f[0] == "MemAvailable:" {
			kb, _ := strconv.Atoi(f[1])
			return kb >> 10
		}
	}
	return 0
}

func copyTimings(t map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(t)+1)
	for k, v := range t {
		out[k] = v
	}
	return out
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
func mib(b int64) float64        { return float64(b) / (1 << 20) }

func describe(p map[string]any) string {
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, p[k]))
	}
	return strings.Join(parts, " ")
}

// summary is "p50 X  p99 Y  (n=N)" in ms.
func summary(v []float64) string {
	if len(v) == 0 {
		return "no successful runs"
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	at := func(p float64) float64 { return s[int(p*float64(len(s)-1)+0.5)] }
	return fmt.Sprintf("p50 %.1f  p99 %.1f  (n=%d)", at(0.5), at(0.99), len(s))
}
