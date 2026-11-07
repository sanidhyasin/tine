//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sanidhyasin/tine/internal/api"
)

// TestDensity forks one small VM until admission control refuses even one
// more child, for lack of memory or of CPU. Nothing may crash on the way:
// tined stays up, no VM fails, every child answers, and the kernel kills
// nothing for lack of memory. Then everything is destroyed, and the suite's
// leak check must be clean.
func TestDensity(t *testing.T) {
	ctx := context.Background()
	health, err := tined.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	oomBefore := oomKills(t)
	parent, err := tined.CreateVM(ctx, api.CreateVMRequest{VCPUs: 1, MemoryMiB: 256, TTLSeconds: 1800})
	if err != nil {
		t.Fatal(err)
	}
	var children []string
	t.Cleanup(func() { destroyAll(append(children, parent.ID)) })

	start := time.Now()
	var refusal error
	for _, batch := range []int{50, 10, 1} {
		for {
			res, err := tined.Fork(ctx, parent.ID, api.ForkRequest{Count: batch, TTLSeconds: 1800})
			if s, c := apiCode(err); s == http.StatusServiceUnavailable && (c == api.CodeNoMemory || c == api.CodeNoCPU) {
				refusal = err
				break // try a smaller batch
			}
			if err != nil {
				t.Fatalf("fork of %d after %d children: %v", batch, len(children), err)
			}
			for _, c := range res.Children {
				if c.VM == nil {
					t.Fatalf("a child of a fork that was admitted failed: %+v", c.Error)
				}
				children = append(children, c.VM.ID)
			}
			t.Logf("%d children: a fork of %d took %.0fms, parent paused %.0fms", len(children), batch,
				res.TimingsMs["total"], res.TimingsMs["paused"])
		}
	}
	took := time.Since(start)
	if refusal == nil || len(children) == 0 {
		t.Fatalf("no refusal after %d children", len(children))
	}
	t.Logf("refused after %d children, in %v: %v", len(children), took.Round(time.Second), refusal)

	// Nothing crashed: tined never restarted, every VM runs and answers.
	if h, err := tined.Health(ctx); err != nil || h.UptimeSeconds < health.UptimeSeconds {
		t.Errorf("tined restarted, or doesn't answer: %+v, %v", h, err)
	}
	vms, err := tined.ListVMsWith(ctx, api.ListOptions{Memory: true})
	if err != nil {
		t.Fatal(err)
	}
	var rss, pss int64
	for _, v := range vms {
		if v.State != "running" {
			t.Errorf("%s is %s: %s", v.ID, v.State, v.Error)
		}
		if v.ParentID == parent.ID && v.Memory != nil {
			rss, pss = rss+v.Memory.RSSBytes, pss+v.Memory.PSSBytes
		}
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, id := range children {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			if out, code, err := run(ctx, id, "echo", "ok"); err != nil || code != 0 || out != "ok\n" {
				t.Errorf("%s doesn't answer: %q %d %v", id, out, code, err)
			}
		}()
	}
	wg.Wait()
	if n := oomKills(t); n > oomBefore {
		t.Errorf("the kernel killed %d processes for lack of memory", n-oomBefore)
	}
	t.Logf("%d idle children of a 256 MiB VM: RSS %d MiB in all, PSS %d MiB in all (%.0f MiB each); host memory available: %d MiB",
		len(children), rss>>20, pss>>20, float64(pss>>20)/float64(len(children)), memAvailableMiB(t))
}

// destroyAll destroys VMs, eight at a time.
func destroyAll(ids []string) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, id := range ids {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			_, _ = tined.DestroyVM(context.Background(), id)
		}()
	}
	wg.Wait()
}

// oomKills counts the kernel's out-of-memory kills since boot.
func oomKills(t *testing.T) int {
	t.Helper()
	out, err := exec.CommandContext(context.Background(), "dmesg").Output()
	if err != nil {
		t.Fatalf("dmesg: %v", err)
	}
	return strings.Count(string(out), "Out of memory: Killed process") + strings.Count(string(out), "Memory cgroup out of memory: Killed process")
}

func memAvailableMiB(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if f := strings.Fields(line); len(f) > 1 && f[0] == "MemAvailable:" {
			kb, _ := strconv.Atoi(f[1])
			return kb >> 10
		}
	}
	return 0
}
