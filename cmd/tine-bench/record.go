package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sanidhyasin/tine/internal/api"
)

// Record is one line of tine-bench's output. The first line is the host
// ("kind": "host"); every line after it is one run of a scenario.
type Record struct {
	Kind     string    `json:"kind"`
	Time     time.Time `json:"time"`
	Scenario string    `json:"scenario,omitempty"`
	// Params are what the scenario varied for this run, e.g. the number of
	// children and the strategy. Runs with equal params form one data point.
	Params map[string]any `json:"params,omitempty"`
	Run    int            `json:"run,omitempty"`
	// Warmup runs are kept, but left out of the analysis.
	Warmup bool   `json:"warmup,omitempty"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
	// TimingsMs are the run's stages, in milliseconds: tined's own, plus
	// "client", the whole call as tine-bench saw it.
	TimingsMs map[string]float64 `json:"timings_ms,omitempty"`
	// MemoryMiB are memory figures, such as children's mean RSS and PSS.
	MemoryMiB map[string]float64 `json:"memory_mib,omitempty"`
	// Extra holds anything else a scenario measured.
	Extra map[string]any `json:"extra,omitempty"`
	Host  *Host          `json:"host,omitempty"`
}

// Host is what a result depends on.
type Host struct {
	Hostname       string `json:"hostname"`
	InstanceType   string `json:"instance_type,omitempty"`
	Virtualization string `json:"virtualization"` // "none" on bare metal; e.g. "amazon" when nested
	CPUModel       string `json:"cpu_model"`
	CPUs           int    `json:"cpus"`
	MemoryMiB      int    `json:"memory_mib"`
	Kernel         string `json:"kernel"`
	Firecracker    string `json:"firecracker"`
	Tine           string `json:"tine"`
	DataDisk       string `json:"data_disk"` // the model of /srv/tine's disk
	DirtyRatio     string `json:"vm_dirty_ratio"`
	THP            string `json:"transparent_hugepages"`
	KSM            string `json:"ksm_run"`
	GoVersion      string `json:"go_version"`
	// Features are what tined runs with, such as "diff-snapshots".
	Features []string `json:"tined_features,omitempty"`
}

// writer writes records as JSON lines, safe for concurrent use.
type writer struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func newWriter(w io.Writer) *writer { return &writer{enc: json.NewEncoder(w)} }

func (w *writer) write(r Record) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if r.Time.IsZero() {
		r.Time = time.Now().UTC()
	}
	_ = w.enc.Encode(r)
}

// describeHost gathers the host's details, as well as it can: what it
// can't find is left empty.
func describeHost(ctx context.Context, c *api.Client) *Host {
	h := &Host{
		CPUs:        runtime.NumCPU(),
		Kernel:      readTrim("/proc/sys/kernel/osrelease"),
		DirtyRatio:  readTrim("/proc/sys/vm/dirty_ratio"),
		THP:         selected(readTrim("/sys/kernel/mm/transparent_hugepage/enabled")),
		KSM:         readTrim("/sys/kernel/mm/ksm/run"),
		GoVersion:   runtime.Version(),
		Firecracker: firstLine(output(ctx, "firecracker", "--version")),
	}
	h.Hostname, _ = os.Hostname()
	h.Virtualization = strings.TrimSpace(output(ctx, "systemd-detect-virt"))
	if h.Virtualization == "" {
		h.Virtualization = "unknown"
	}
	for _, line := range strings.Split(readTrim("/proc/cpuinfo"), "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "model name" {
			h.CPUModel = strings.TrimSpace(v)
			break
		}
	}
	for _, line := range strings.Split(readTrim("/proc/meminfo"), "\n") {
		if f := strings.Fields(line); len(f) > 1 && f[0] == "MemTotal:" {
			kb, _ := strconv.Atoi(f[1])
			h.MemoryMiB = kb >> 10
		}
	}
	if health, err := c.Health(ctx); err == nil {
		h.Tine, h.Features = health.Version, health.Features
	}
	h.DataDisk = diskModel(ctx, "/srv/tine")
	h.InstanceType = ec2InstanceType(ctx)
	return h
}

// ec2InstanceType asks the EC2 metadata service (IMDSv2), if there is one.
func ec2InstanceType(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, "http://169.254.169.254/latest/api/token", nil)
	req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "60")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	token, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, "http://169.254.169.254/latest/meta-data/instance-type", nil)
	req.Header.Set("X-aws-ec2-metadata-token", string(token))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	typ, _ := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(typ))
}

// diskModel is the model of the disk a directory is on, e.g. "Amazon
// Elastic Block Store" or "Amazon EC2 NVMe Instance Storage".
func diskModel(ctx context.Context, dir string) string {
	src := strings.TrimSpace(output(ctx, "findmnt", "-no", "SOURCE", "--target", dir))
	if src == "" {
		return ""
	}
	dev := filepath.Base(src)
	model := readTrim("/sys/class/block/" + dev + "/device/model")
	if model == "" { // a partition: its disk's model
		model = readTrim("/sys/class/block/" + dev + "/../device/model")
	}
	return dev + ": " + model
}

func readTrim(path string) string {
	data, err := os.ReadFile(path) //nolint:gosec // fixed system paths
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func output(ctx context.Context, name string, args ...string) string {
	out, err := exec.CommandContext(ctx, name, args...).Output() //nolint:gosec // fixed commands
	if err != nil && len(out) == 0 {
		return ""
	}
	return string(out)
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

// selected picks the chosen value out of a sysfs setting like
// "always [madvise] never".
func selected(s string) string {
	if i, j := strings.Index(s, "["), strings.Index(s, "]"); i >= 0 && j > i {
		return s[i+1 : j]
	}
	return s
}
