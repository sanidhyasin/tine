package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/sanidhyasin/tine/internal/api"
)

// printJSON writes v as indented JSON.
func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// timingLabels names creation stages for people, in the order they happen.
var timingLabels = []struct{ key, label string }{
	{"allocate", "allocate"},
	{"network", "network"},
	{"jail", "jail"},
	{"start", "start"},
	{"configure", "configure"},
	{"restore", "restore"},
	{"agent", "agent"},
	{"sync", "sync"},
}

// describeTimings is e.g. "network 5ms · jail 0.9ms · boot 1.82s". After a
// cold start, waiting for the agent is the guest booting, so it's "boot".
func describeTimings(t map[string]float64, source string) string {
	var parts []string
	for _, l := range timingLabels {
		label := l.label
		if l.key == "agent" && source != "template" && source != "snapshot" {
			label = "boot"
		}
		if v, ok := t[l.key]; ok {
			parts = append(parts, label+" "+formatMillis(v))
		}
	}
	return strings.Join(parts, " · ")
}

// describeStart is how a VM started, e.g. "booted" or "restored from template snap-x".
func describeStart(v api.VM) string {
	switch v.Source {
	case "template":
		return "restored from template " + v.SnapshotID
	case "snapshot":
		return "restored from " + v.SnapshotID
	}
	return "booted"
}

func formatMillis(ms float64) string {
	switch {
	case ms >= 1000:
		return strconv.FormatFloat(ms/1000, 'f', 2, 64) + "s"
	case ms >= 10:
		return strconv.FormatFloat(ms, 'f', 0, 64) + "ms"
	default:
		return strconv.FormatFloat(ms, 'f', 1, 64) + "ms"
	}
}

// humanDuration is e.g. "45s", "12m", "3h", "2d".
func humanDuration(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// formatMiB is e.g. "512 MiB" or "2 GiB".
func formatMiB(mib int) string {
	if mib >= 1024 && mib%1024 == 0 {
		return strconv.Itoa(mib/1024) + " GiB"
	}
	if mib >= 1024 {
		return strconv.FormatFloat(float64(mib)/1024, 'f', 1, 64) + " GiB"
	}
	return strconv.Itoa(mib) + " MiB"
}

// formatBytes is e.g. "512 MiB", "4 GiB" or "1.3 GiB".
func formatBytes(n int64) string {
	if n >= 1<<30 {
		s := strconv.FormatFloat(float64(n)/(1<<30), 'f', 1, 64)
		return strings.TrimSuffix(s, ".0") + " GiB"
	}
	return strconv.FormatInt(n>>20, 10) + " MiB"
}

// parseMemory reads "1G", "512M", "1.5GiB" or a plain number of MiB.
func parseMemory(s string) (int, error) {
	u := strings.ToUpper(strings.TrimSpace(s))
	u = strings.TrimSuffix(strings.TrimSuffix(u, "IB"), "B")
	mult := 1.0
	switch {
	case strings.HasSuffix(u, "G"):
		mult, u = 1024, strings.TrimSuffix(u, "G")
	case strings.HasSuffix(u, "M"):
		u = strings.TrimSuffix(u, "M")
	}
	v, err := strconv.ParseFloat(u, 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("memory %q: use something like 512M, 1G or a number of MiB", s)
	}
	return int(v * mult), nil
}
