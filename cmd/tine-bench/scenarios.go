package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
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
	// features are what tined and the host run with, beyond the defaults
	// (tined's features, and "thp" for transparent huge pages always on):
	// every record has them in its params, as "features", so runs with and
	// without them can be told apart in one results file.
	features []string
}

// point runs fn warmup+runs times as one data point, writing a record for
// each, and prints a one-line summary of the key timing.
func (b *bench) point(ctx context.Context, scenario string, params map[string]any, key string, runs int,
	fn func(ctx context.Context) Record) {
	if len(b.features) > 0 {
		p := map[string]any{"features": strings.Join(b.features, ",")}
		for k, v := range params {
			p[k] = v
		}
		params = p
	}
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
		v, ok := r.TimingsMs[key]
		if !ok {
			v = r.MemoryMiB[key]
		}
		vals = append(vals, v)
	}
	_, _ = fmt.Fprintf(b.log, "%-18s %-44s %s %s", scenario, describe(params), key, summary(vals))
	if failed > 0 {
		_, _ = fmt.Fprintf(b.log, "  (%d failed)", failed)
	}
	_, _ = fmt.Fprintln(b.log)
}

// create: one VM at a time, created and destroyed.
func (b *bench) create(ctx context.Context, cold bool, strategy string, vcpus, memoryMiB int) {
	name := "create-template"
	if cold {
		name = "create-cold"
	}
	if b.dropCaches {
		name += "-cold-cache"
	}
	params := map[string]any{"vcpus": vcpus, "memory_mib": memoryMiB}
	if strategy != "" && strategy != "share" {
		params["strategy"] = strategy
	}
	b.point(ctx, name, params, "total", 0, func(ctx context.Context) Record {
		if b.dropCaches {
			dropCaches()
		}
		start := time.Now()
		vm, err := b.c.CreateVM(ctx, api.CreateVMRequest{VCPUs: vcpus, MemoryMiB: memoryMiB, TTLSeconds: b.ttl, Cold: cold, Strategy: strategy})
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
	if err := b.settle(ctx, vm.ID); err != nil {
		b.fail("snapshot", err)
		return
	}
	// Each snapshot is kept until the next is taken: with diff snapshots,
	// it's the next one's base.
	var last string
	defer func() {
		if last != "" {
			_, _ = b.c.DeleteSnapshot(context.Background(), last)
		}
	}()
	b.point(ctx, "snapshot", map[string]any{"memory_mib": memoryMiB}, "paused", runs, func(ctx context.Context) Record {
		start := time.Now()
		snap, err := b.c.CreateSnapshot(ctx, vm.ID)
		client := ms(time.Since(start))
		if err != nil {
			return Record{Error: err.Error()}
		}
		if last != "" {
			_, _ = b.c.DeleteSnapshot(ctx, last)
		}
		last = snap.ID
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
	// Dirty: the parent is snapshotted before it changes DirtyMiB again, so
	// the fork's snapshot has a base for a diff (fork-dirty).
	Dirty bool
	// TouchMiB is memory the parent fills before the fork, which each child
	// then writes to with tine-touch, timing each page (needs the bench image).
	TouchMiB int
	// Read has the children read that memory instead of writing it, and
	// Evict drops the page cache first, as memory pressure would (fork-evict).
	Read, Evict bool
	Image       string
}

func (o forkOptions) params() map[string]any {
	p := map[string]any{"children": o.Children, "vcpus": o.VCPUs, "memory_mib": o.MemoryMiB}
	if o.Strategy != "" {
		p["strategy"] = o.Strategy
	}
	if o.Dirty {
		p["dirty_mib"] = o.DirtyMiB
	}
	if o.TouchMiB > 0 {
		p["touch_mib"] = o.TouchMiB
	}
	if o.Read {
		p["read"] = true
	}
	if o.Evict {
		p["evicted"] = true
	}
	return p
}

// fork: a fresh parent for every run, forked into Children; then each
// child's memory, and with TouchMiB, how fast each child writes to memory
// it shares with its parent.
func (b *bench) fork(ctx context.Context, o forkOptions, runs int) {
	if o.Strategy == strategyCold {
		if o.TouchMiB > 0 || o.Dirty {
			_, _ = fmt.Fprintf(b.log, "%-18s %-44s skipped: cold VMs share nothing with a parent\n", "fork", describe(o.params()))
			return
		}
		b.forkCold(ctx, o, runs)
		return
	}
	b.point(ctx, "fork", o.params(), "total", runs, func(ctx context.Context) Record {
		parent, err := b.c.CreateVM(ctx, api.CreateVMRequest{Image: o.Image, VCPUs: o.VCPUs, MemoryMiB: o.MemoryMiB, Cold: true, TTLSeconds: b.ttl})
		if err != nil {
			return Record{Error: "parent: " + err.Error()}
		}
		ids := []string{parent.ID}
		defer func() { b.destroyAll(ids) }()
		if err := b.settle(ctx, parent.ID); err != nil {
			return Record{Error: err.Error()}
		}
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
		if o.Dirty {
			// A snapshot first, then DirtyMiB changed since: what a diff
			// snapshot would have to save. Full ones save everything anyway.
			if _, err := b.c.CreateSnapshot(ctx, parent.ID); err != nil {
				return Record{Error: "base snapshot: " + err.Error()}
			}
			if o.DirtyMiB > 0 {
				if err := b.run(ctx, parent.ID, "tine-dirty", "again"); err != nil {
					return Record{Error: "tine-dirty again: " + err.Error()}
				}
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
			if o.Evict {
				dropCaches()
			}
			b.touch(ctx, children, o.Read, &r)
		}
		time.Sleep(3 * time.Second) // let the children settle before reading their memory
		b.childMemory(ctx, parent.ID, children, &r)
		return r
	})
}

// strategyCold isn't a fork at all: the baseline a fork is measured
// against, booting the same number of fresh VMs instead.
const strategyCold = "cold"

// forkCold boots Children fresh VMs, as many at once as tined restores a
// fork's children (one per CPU), and times them as one fork.
func (b *bench) forkCold(ctx context.Context, o forkOptions, runs int) {
	b.point(ctx, "fork", o.params(), "total", runs, func(ctx context.Context) Record {
		var (
			mu       sync.Mutex
			ids      []string
			restores []float64
			errs     []string
			wg       sync.WaitGroup
		)
		defer func() { b.destroyAll(ids) }()
		slots := make(chan struct{}, runtime.NumCPU())
		start := time.Now()
		for range o.Children {
			slots <- struct{}{}
			wg.Add(1)
			go func() {
				defer func() { <-slots; wg.Done() }()
				vm, err := b.c.CreateVM(ctx, api.CreateVMRequest{Image: o.Image, VCPUs: o.VCPUs, MemoryMiB: o.MemoryMiB, Cold: true, TTLSeconds: b.ttl})
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					errs = append(errs, err.Error())
					return
				}
				ids = append(ids, vm.ID)
				restores = append(restores, vm.TimingsMs["total"])
			}()
		}
		wg.Wait()
		total := ms(time.Since(start))
		r := Record{OK: true, TimingsMs: map[string]float64{"total": total, "children": total, "client": total},
			MemoryMiB: map[string]float64{}, Extra: map[string]any{}}
		if len(errs) > 0 {
			r.OK, r.Error = false, fmt.Sprintf("%d of %d VMs failed: %s", len(errs), o.Children, errs[0])
		}
		if len(restores) > 0 {
			sort.Float64s(restores)
			r.TimingsMs["child_p50"] = restores[len(restores)/2]
			r.TimingsMs["child_max"] = restores[len(restores)-1]
		}
		time.Sleep(3 * time.Second)
		b.childMemory(ctx, "", ids, &r)
		return r
	})
}

// touchChildren is how many children touch times, one after another: all
// of them at once would measure the CPUs being shared as much as the
// page faults.
const touchChildren = 4

// touch has children, one at a time, write (or read) all the memory their
// parent filled before the fork, and records the time per page: the cost
// of copy-on-write, or of reading the parent's memory.
func (b *bench) touch(ctx context.Context, children []string, read bool, r *Record) {
	var perPage []float64
	for _, id := range children[:min(len(children), touchChildren)] {
		args := []string{"tine-touch", "measure"}
		if read {
			args = append(args, "-read")
		}
		out, err := b.output(ctx, id, args...)
		if err != nil {
			continue
		}
		var res struct {
			NsPerPage float64 `json:"ns_per_page"`
		}
		if json.Unmarshal([]byte(out), &res) == nil {
			perPage = append(perPage, res.NsPerPage)
		}
	}
	if len(perPage) > 0 {
		sort.Float64s(perPage)
		r.Extra["touch_ns_per_page_p50"] = perPage[len(perPage)/2]
		r.Extra["touch_ns_per_page_max"] = perPage[len(perPage)-1]
	}
}

// childMemory records the children's mean RSS and PSS, and the parent's.
func (b *bench) childMemory(ctx context.Context, parentID string, children []string, r *Record) {
	vms, err := b.c.ListVMsWith(ctx, api.ListOptions{Memory: true})
	if err != nil {
		return
	}
	child := map[string]bool{}
	for _, id := range children {
		child[id] = true
	}
	var rss, pss float64
	n := 0
	for _, v := range vms {
		if v.Memory == nil {
			continue
		}
		switch {
		case parentID != "" && v.ID == parentID:
			r.MemoryMiB["parent_rss"] = mib(v.Memory.RSSBytes)
			r.MemoryMiB["parent_pss"] = mib(v.Memory.PSSBytes)
		case child[v.ID]:
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
		if err := b.settle(ctx, parent.ID); err != nil {
			return Record{Error: err.Error()}
		}
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
		b.childMemory(ctx, parent.ID, ids[1:], &r)
		r.MemoryMiB["host_available"] = float64(memAvailableMiB())
		return r
	})
}

// idle: VMs made with a strategy (cold boots them; the others fork one
// settled parent), then left idle, while what they cost the host is
// sampled every 10 seconds: their total PSS, the host's available memory,
// and the memory KSM saves by sharing pages.
func (b *bench) idle(ctx context.Context, strategy string, count, memoryMiB int, watch time.Duration) {
	params := map[string]any{"strategy": strategy, "vms": count, "memory_mib": memoryMiB}
	b.point(ctx, "idle", params, "pss_total", 1, func(ctx context.Context) Record {
		before := memAvailableMiB()
		var ids []string
		defer func() { b.destroyAll(ids) }()
		var parentID string
		if strategy == strategyCold {
			slots := make(chan struct{}, runtime.NumCPU())
			var mu sync.Mutex
			var wg sync.WaitGroup
			for range count {
				slots <- struct{}{}
				wg.Add(1)
				go func() {
					defer func() { <-slots; wg.Done() }()
					vm, err := b.c.CreateVM(ctx, api.CreateVMRequest{VCPUs: 1, MemoryMiB: memoryMiB, Cold: true, TTLSeconds: b.ttl})
					if err == nil {
						mu.Lock()
						ids = append(ids, vm.ID)
						mu.Unlock()
					}
				}()
			}
			wg.Wait()
		} else {
			parent, err := b.c.CreateVM(ctx, api.CreateVMRequest{VCPUs: 1, MemoryMiB: memoryMiB, Cold: true, TTLSeconds: b.ttl})
			if err != nil {
				return Record{Error: "parent: " + err.Error()}
			}
			parentID = parent.ID
			ids = append(ids, parent.ID)
			if err := b.settle(ctx, parent.ID); err != nil {
				return Record{Error: err.Error()}
			}
			res, err := b.c.Fork(ctx, parent.ID, api.ForkRequest{Count: count, TTLSeconds: b.ttl, Strategy: strategy})
			if err != nil {
				return Record{Error: err.Error()}
			}
			for _, c := range res.Children {
				if c.VM != nil {
					ids = append(ids, c.VM.ID)
				}
			}
		}
		made := len(ids)
		if parentID != "" {
			made--
		}
		if made < count {
			return Record{Error: fmt.Sprintf("only %d of %d VMs were made", made, count)}
		}
		var samples []map[string]float64
		start := time.Now()
		for {
			s := b.idleSample(ctx, ids, parentID)
			s["seconds"] = time.Since(start).Seconds()
			s["host_used"] = float64(before - memAvailableMiB())
			samples = append(samples, s)
			if time.Since(start) >= watch || ctx.Err() != nil {
				break
			}
			time.Sleep(10 * time.Second)
		}
		last := samples[len(samples)-1]
		return Record{OK: true, MemoryMiB: map[string]float64{
			"pss_total": last["pss_total"], "rss_total": last["rss_total"], "host_used": last["host_used"], "ksm_saved": last["ksm_saved"],
		}, Extra: map[string]any{"samples": samples}}
	})
}

// pressure: what breaks when the host runs out of memory. A parent of
// memoryMiB is forked into count children, which admission control admits
// (it counts memory as it is, not as the children might grow it). Then, in
// rounds, every child changes step MiB more of its memory, until children
// fail or a round makes no progress. Each round records the host's
// available memory, how many children are still running, the kernel's OOM
// kills so far, and how long an exec in a child takes.
func (b *bench) pressure(ctx context.Context, count, memoryMiB, step int) {
	params := map[string]any{"children": count, "memory_mib": memoryMiB, "step_mib": step}
	b.point(ctx, "pressure", params, "rounds", 1, func(ctx context.Context) Record {
		parent, err := b.c.CreateVM(ctx, api.CreateVMRequest{Image: "bench", VCPUs: 1, MemoryMiB: memoryMiB, Cold: true, TTLSeconds: b.ttl})
		if err != nil {
			return Record{Error: "parent: " + err.Error()}
		}
		ids := []string{parent.ID}
		defer func() { b.destroyAll(ids) }()
		if err := b.settle(ctx, parent.ID); err != nil {
			return Record{Error: err.Error()}
		}
		res, err := b.c.Fork(ctx, parent.ID, api.ForkRequest{Count: count, TTLSeconds: b.ttl})
		if err != nil {
			return Record{Error: err.Error()}
		}
		var children []string
		for _, c := range res.Children {
			if c.VM != nil {
				children = append(children, c.VM.ID)
				ids = append(ids, c.VM.ID)
			}
		}
		oomStart := vmstat("oom_kill")
		var rounds []map[string]float64
		for round := 1; ; round++ {
			start := time.Now()
			var wg sync.WaitGroup
			for _, id := range children {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_ = b.run(ctx, id, "tine-dirty", "-mb", strconv.Itoa(step))
				}()
			}
			wg.Wait()
			rd := map[string]float64{"round": float64(round), "dirty_mib": float64(round * step), "seconds": time.Since(start).Seconds()}
			rd["host_available_mib"] = float64(memAvailableMiB())
			rd["oom_kills"] = float64(vmstat("oom_kill") - oomStart)
			var alive []string
			if vms, err := b.c.ListVMs(ctx, true); err == nil {
				running := map[string]bool{}
				for _, v := range vms {
					running[v.ID] = v.State == "running"
				}
				for _, id := range children {
					if running[id] {
						alive = append(alive, id)
					}
				}
			}
			rd["alive"] = float64(len(alive))
			if len(alive) > 0 {
				t := time.Now()
				if err := b.run(ctx, alive[0], "true"); err == nil {
					rd["exec_ms"] = ms(time.Since(t))
				}
			}
			rounds = append(rounds, rd)
			_, _ = fmt.Fprintf(b.log, "pressure round %d: %d MiB each, %d of %d alive, %.0f MiB available, %.0f OOM kills, %.1fs\n",
				round, round*step, len(alive), count, rd["host_available_mib"], rd["oom_kills"], rd["seconds"])
			if len(alive) < len(children) || round*step >= memoryMiB || ctx.Err() != nil {
				break
			}
		}
		return Record{OK: true, TimingsMs: map[string]float64{"rounds": float64(len(rounds))},
			Extra: map[string]any{"rounds": rounds}}
	})
}

// vmstat reads a counter from /proc/vmstat.
func vmstat(name string) int {
	for _, line := range strings.Split(readTrim("/proc/vmstat"), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == name {
			n, _ := strconv.Atoi(f[1])
			return n
		}
	}
	return 0
}

// idleSample adds up the memory of the VMs in ids, leaving out parentID.
func (b *bench) idleSample(ctx context.Context, ids []string, parentID string) map[string]float64 {
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = id != parentID
	}
	s := map[string]float64{}
	if vms, err := b.c.ListVMsWith(ctx, api.ListOptions{Memory: true}); err == nil {
		for _, v := range vms {
			if v.Memory != nil && want[v.ID] {
				s["pss_total"] += mib(v.Memory.PSSBytes)
				s["rss_total"] += mib(v.Memory.RSSBytes)
			}
		}
	}
	if n, err := strconv.Atoi(readTrim("/sys/kernel/mm/ksm/pages_sharing")); err == nil {
		s["ksm_saved"] = float64(n) * 4096 / (1 << 20)
	}
	return s
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

// settle waits until a booted VM's guest has finished starting up, as
// tined waits before saving a template. Forking a guest that's still
// booting would have every child finish the boot, all at once.
func (b *bench) settle(ctx context.Context, id string) error {
	// "degraded" (a unit failed) exits 1, and is settled all the same.
	if _, err := b.c.Exec(ctx, id, api.ExecRequest{Args: []string{"systemctl", "is-system-running", "--wait"}, TimeoutMs: 120_000}, nil); err != nil {
		return fmt.Errorf("settle: %w", err)
	}
	// systemd reports the boot finished as it starts its last units; a fork
	// in the next second gives every child that work to do (agent ready in
	// about 180 ms instead of 50).
	time.Sleep(2 * time.Second)
	return nil
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
