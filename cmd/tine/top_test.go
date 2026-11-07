package main

import (
	"strings"
	"testing"
)

func TestTop(t *testing.T) {
	sock, _ := startTined(t)
	tine(t, sock, "", "create")
	if r := tine(t, sock, "", "fork", "vm-aaaaaaaaab", "-n", "2"); r.code != 0 {
		t.Fatalf("fork: %s", r.stderr)
	}
	r := tine(t, sock, "", "top", "-n", "2", "--interval", "10ms")
	if r.code != 0 {
		t.Fatalf("top: exit %d, %s", r.code, r.stderr)
	}
	if n := strings.Count(r.stdout, "3 VMs: 3 running"); n != 2 {
		t.Errorf("want two frames of 3 running VMs, got %d:\n%s", n, r.stdout)
	}
	if !strings.Contains(r.stdout, "(2 forked)") || strings.Contains(r.stdout, "\x1b[") {
		t.Errorf("want the tree, without colors when not a terminal:\n%s", r.stdout)
	}
}

func TestColorLine(t *testing.T) {
	for line, want := range map[string]string{
		"vm-a  failed  base\n":   "\x1b[31mvm-a  failed  base\x1b[0m\n",
		"vm-b  running  base\n":  "vm-b  running  base\n",
		"└─ snap-x (3 forked)\n": "\x1b[2m└─ snap-x (3 forked)\x1b[0m\n",
	} {
		if got := colorLine(line, true); got != want {
			t.Errorf("colorLine(%q) = %q, want %q", line, got, want)
		}
	}
	if got := colorLine("vm-a  failed  base\n", false); got != "vm-a  failed  base\n" {
		t.Errorf("without color: %q", got)
	}
}
