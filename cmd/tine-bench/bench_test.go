package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestHelpers(t *testing.T) {
	if got := selected("always [madvise] never"); got != "madvise" {
		t.Errorf("selected = %q", got)
	}
	if got := ints("1, 10,x,30"); len(got) != 3 || got[2] != 30 {
		t.Errorf("ints = %v", got)
	}
	if got := describe(map[string]any{"vcpus": 1, "children": 10}); got != "children=10 vcpus=1" {
		t.Errorf("describe = %q", got)
	}
	if got := summary([]float64{3, 1, 2}); got != "p50 2.0  p99 3.0  (n=3)" {
		t.Errorf("summary = %q", got)
	}
	if got := summary(nil); got != "no successful runs" {
		t.Errorf("summary(nil) = %q", got)
	}
}

func TestRecordsAreJSONLines(t *testing.T) {
	var buf bytes.Buffer
	w := newWriter(&buf)
	w.write(Record{Kind: "host", OK: true, Host: &Host{Hostname: "h"}})
	w.write(Record{Kind: "run", Scenario: "fork", OK: true, TimingsMs: map[string]float64{"total": 1}})
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %q", lines)
	}
	var r Record
	if err := json.Unmarshal([]byte(lines[1]), &r); err != nil || r.Scenario != "fork" || r.Time.IsZero() {
		t.Errorf("record = %+v, %v", r, err)
	}
}
