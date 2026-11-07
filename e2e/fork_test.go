//go:build e2e

package e2e

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sanidhyasin/tine/internal/api"
)

// tcpExit tries a TCP connection from inside a VM and returns the exit
// code: 0 connected, 124 timed out (dropped), anything else refused.
func tcpExit(t *testing.T, id, addr string, seconds int) int {
	t.Helper()
	host, port, _ := strings.Cut(addr, ":")
	_, code, err := run(context.Background(), id, "timeout", strconv.Itoa(seconds), "bash", "-c", "echo > /dev/tcp/"+host+"/"+port)
	if err != nil {
		t.Errorf("%s: exec: %v", id, err)
		return -1
	}
	return code
}

// TestFork10 forks a running VM into 10 children, which each write a file
// of their own. No child sees another's, none can reach another, each has
// its own random numbers and shares the parent's memory, and the parent
// carries on as it was.
func TestFork10(t *testing.T) {
	ctx := context.Background()
	parent := createDefault(t)
	mustRun(t, parent.ID, "sh", "-c", "mkdir /root/work; echo parent > /root/work/parent; setsid sleep 100000 >/dev/null 2>&1 < /dev/null & echo $! > /root/sleeper")

	res, err := tined.Fork(ctx, parent.ID, api.ForkRequest{Count: 10, TTLSeconds: 600})
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	var children []api.VM
	for i, c := range res.Children {
		if c.VM == nil {
			t.Fatalf("child %d couldn't be made: %+v", i+1, c.Error)
		}
		child := *c.VM
		t.Cleanup(func() { _, _ = tined.DestroyVM(context.Background(), child.ID) })
		children = append(children, child)
	}
	t.Logf("fork of 10: %.0fms, parent paused %.0fms (snapshot %.0fms, children %.0fms)",
		res.TimingsMs["total"], res.TimingsMs["paused"], res.TimingsMs["snapshot"], res.TimingsMs["children"])

	// Each child writes a file of its own, all at once.
	var wg sync.WaitGroup
	for _, c := range children {
		wg.Add(1)
		go func() { defer wg.Done(); mustRun(t, c.ID, "sh", "-c", "echo "+c.ID+" > /root/work/"+c.ID) }()
	}
	wg.Wait()

	random := map[string]string{}
	for i, c := range children {
		if c.State != "running" || c.ParentID != parent.ID || c.SnapshotID != res.Snapshot.ID {
			t.Errorf("child %d = %+v", i+1, c)
		}
		if got := strings.Fields(mustRun(t, c.ID, "ls", "/root/work")); strings.Join(got, " ") != strings.Join(sorted("parent", c.ID), " ") {
			t.Errorf("%s sees %v in /root/work, want only the parent's file and its own", c.ID, got)
		}
		if out := strings.TrimSpace(mustRun(t, c.ID, "sh", "-c", "kill -0 $(cat /root/sleeper) && hostname")); out != c.ID {
			t.Errorf("%s: the parent's process or its own hostname is missing: %q", c.ID, out)
		}
		r := strings.TrimSpace(mustRun(t, c.ID, "sh", "-c", "head -c 16 /dev/urandom | od -An -tx1 | tr -d ' '"))
		if other, dup := random[r]; dup {
			t.Errorf("%s and %s read the same random bytes", c.ID, other)
		}
		random[r] = c.ID
	}

	// Children reach the internet, and not each other.
	addrs := make([]string, len(children))
	for i, c := range children {
		addrs[i] = vethAddr(t, c.ID, "self")
	}
	for i, c := range children {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if code := tcpExit(t, c.ID, "1.1.1.1:443", 5); code != 0 {
				t.Errorf("%s can't reach the internet (exit %d)", c.ID, code)
			}
			next := children[(i+1)%len(children)]
			if code := tcpExit(t, c.ID, addrs[(i+1)%len(children)]+":9", 2); code != 124 {
				t.Errorf("%s -> %s's network: exit %d, want 124 (dropped)", c.ID, next.ID, code)
			}
		}()
	}
	wg.Wait()

	// They share the parent's memory: PSS splits the shared pages among them.
	var rss, pss int64
	for _, c := range children {
		got, err := tined.GetVM(ctx, c.ID)
		if err != nil || got.Memory == nil {
			t.Fatalf("GetVM %s: %+v, %v", c.ID, got.Memory, err)
		}
		if got.Memory.PSSBytes >= got.Memory.RSSBytes {
			t.Errorf("%s: PSS %d >= RSS %d", c.ID, got.Memory.PSSBytes, got.Memory.RSSBytes)
		}
		rss, pss = rss+got.Memory.RSSBytes, pss+got.Memory.PSSBytes
	}
	t.Logf("10 children: RSS %d MiB in all, PSS %d MiB in all", rss>>20, pss>>20)

	// The parent carries on, untouched.
	if got := strings.Fields(mustRun(t, parent.ID, "ls", "/root/work")); strings.Join(got, " ") != "parent" {
		t.Errorf("the parent sees %v in /root/work, want only its own file", got)
	}
	if out := strings.TrimSpace(mustRun(t, parent.ID, "sh", "-c", "kill -0 $(cat /root/sleeper) && hostname")); out != parent.ID {
		t.Errorf("the parent's process or hostname changed: %q", out)
	}
}

// TestForkPastTheLimitsIsRefused: a fork the host has no room for is
// refused before the parent is paused, and leaves nothing behind.
func TestForkPastTheLimitsIsRefused(t *testing.T) {
	ctx := context.Background()
	parent := createDefault(t)
	before, _ := tined.ListSnapshots(ctx, api.SnapshotFilter{VMID: parent.ID})
	_, err := tined.Fork(ctx, parent.ID, api.ForkRequest{Count: 100, TTLSeconds: 600})
	// The parent's 1 GiB, 101 times, is more than 4 times the dev server's 16 GiB.
	if s, c := apiCode(err); s != http.StatusServiceUnavailable || c != api.CodeNoMemory {
		t.Fatalf("a fork of 100 = %v, want 503 no_memory", err)
	}
	t.Logf("refused: %v", err)
	after, _ := tined.ListSnapshots(ctx, api.SnapshotFilter{VMID: parent.ID})
	if len(after) != len(before) {
		t.Errorf("the refused fork took a snapshot: %+v", after)
	}
	if out := strings.TrimSpace(mustRun(t, parent.ID, "echo", "fine")); out != "fine" {
		t.Errorf("the parent: %q", out)
	}
}

func sorted(s ...string) []string {
	slices.Sort(s)
	return s
}
