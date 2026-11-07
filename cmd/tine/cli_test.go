package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sanidhyasin/tine/internal/agentproto"
	"github.com/sanidhyasin/tine/internal/api"
	"github.com/sanidhyasin/tine/internal/image"
	"github.com/sanidhyasin/tine/internal/state"
	"github.com/sanidhyasin/tine/internal/vm"
)

// backend is a small in-memory tined, behind the real API server.
type backend struct {
	mu      sync.Mutex
	vms     []state.VM
	lastReq vm.CreateRequest
	lastRun agentproto.ExecRequest
	// nextSource, if "template", makes Create look like a restore.
	nextSource string
}

func (b *backend) Create(_ context.Context, req vm.CreateRequest) (state.VM, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastReq = req
	v := state.VM{
		ID: fmt.Sprintf("vm-%010d", len(b.vms)+1), State: state.StateRunning, Image: "base", VCPUs: 2, MemoryMiB: 1024,
		CreatedAt: time.Now().Add(-90 * time.Second), ExpiresAt: time.Now().Add(time.Hour),
		Timings: map[string]float64{"allocate": 0.3, "network": 5.1, "jail": 0.9, "start": 0.1, "configure": 21.1, "agent": 1819.6, "total": 1847.3},
	}
	v.ID = strings.NewReplacer("0", "a", "1", "b", "2", "c", "3", "d").Replace(v.ID) // IDs use base32 letters
	if b.nextSource == "template" && !req.Cold {
		v.Source, v.SnapshotID = state.SourceTemplate, "snap-tmplaaaaaa"
		v.Timings = map[string]float64{"allocate": 0.3, "network": 5.1, "jail": 0.9, "start": 0.1, "restore": 15.2, "agent": 9.2, "sync": 21.0, "total": 52.1}
	}
	b.vms = append(b.vms, v)
	return v, nil
}

func (b *backend) find(id string) (int, error) {
	for i, v := range b.vms {
		if v.ID == id {
			return i, nil
		}
	}
	return 0, fmt.Errorf("VM %s: %w", id, vm.ErrNotFound)
}

func (b *backend) Get(_ context.Context, id string) (state.VM, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	i, err := b.find(id)
	if err != nil {
		return state.VM{}, err
	}
	return b.vms[i], nil
}

func (b *backend) List(_ context.Context, all bool) ([]state.VM, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []state.VM
	for _, v := range b.vms {
		if all || v.State != state.StateDestroyed {
			out = append(out, v)
		}
	}
	return out, nil
}

func (b *backend) Destroy(_ context.Context, id string) (state.VM, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	i, err := b.find(id)
	if err != nil {
		return state.VM{}, err
	}
	b.vms[i].State = state.StateDestroyed
	b.vms[i].Timings["destroy"] = 28.9
	return b.vms[i], nil
}

// Exec plays the guest: the command's name says what to do.
func (b *backend) Exec(ctx context.Context, id string, req agentproto.ExecRequest, onOutput func(string, []byte)) (agentproto.ExitStatus, error) {
	b.mu.Lock()
	b.lastRun = req
	b.mu.Unlock()
	if _, err := b.Get(context.Background(), id); err != nil {
		return agentproto.ExitStatus{}, err
	}
	switch req.Args[0] {
	case "echo":
		onOutput(agentproto.StreamStdout, []byte(strings.Join(req.Args[1:], " ")+"\n"))
		return agentproto.ExitStatus{}, nil
	case "cat":
		onOutput(agentproto.StreamStdout, req.Stdin)
		return agentproto.ExitStatus{}, nil
	case "fail":
		onOutput(agentproto.StreamStdout, []byte("out\n"))
		onOutput(agentproto.StreamStderr, []byte("err\n"))
		return agentproto.ExitStatus{Code: 3}, nil
	case "sleep":
		return agentproto.ExitStatus{Code: -1, Signal: "killed", TimedOut: true}, nil
	case "crash":
		return agentproto.ExitStatus{Code: -1, Signal: "segmentation fault"}, nil
	case "hang": // runs until tine hangs up
		<-ctx.Done()
		return agentproto.ExitStatus{}, ctx.Err()
	case "flood":
		onOutput(agentproto.StreamStdout, []byte("lots"))
		return agentproto.ExitStatus{OutputTruncated: true}, nil
	default:
		return agentproto.ExitStatus{}, &agentproto.Error{StatusCode: http.StatusUnprocessableEntity,
			ErrorDetail: agentproto.ErrorDetail{Code: agentproto.CodeStartFailed, Message: fmt.Sprintf("exec: %q: executable file not found in $PATH", req.Args[0])}}
	}
}

func (b *backend) Images(context.Context) ([]*image.Image, error) {
	return []*image.Image{{Name: "base", Meta: image.Metadata{CreatedAt: time.Now().Add(-2 * time.Hour),
		Kernel: image.KernelInfo{Source: "vmlinux-6.18.56"}, Rootfs: image.RootfsInfo{SizeBytes: 4 << 30}, Agent: image.AgentInfo{Version: "a5f7186"}}}}, nil
}

func (b *backend) Ping(context.Context) error { return nil }

func (*backend) Templates(context.Context) ([]vm.Template, error) {
	return []vm.Template{
		{Snapshot: state.Snapshot{ID: "snap-tmplaaaaaa", Kind: state.SnapshotKindTemplate, Image: "base", VCPUs: 2, MemoryMiB: 1024, CreatedAt: time.Now().Add(-time.Hour)}},
		{Snapshot: state.Snapshot{ID: "snap-tmplbbbbbb", Kind: state.SnapshotKindTemplate, Image: "base", VCPUs: 4, MemoryMiB: 4096, CreatedAt: time.Now()}, Problem: "image base was imported again since"},
	}, nil
}

func (*backend) WarmTemplate(_ context.Context, image string, vcpus, memoryMiB int) (state.Snapshot, error) {
	if image != "base" {
		return state.Snapshot{}, fmt.Errorf("%w: %s", vm.ErrImageNotFound, image)
	}
	if vcpus == 0 {
		vcpus = 2
	}
	if memoryMiB == 0 {
		memoryMiB = 1024
	}
	return state.Snapshot{
		ID: "snap-tmplcccccc", Kind: state.SnapshotKindTemplate, State: state.SnapshotReady, VMID: "vm-warmwarmwa", Image: image,
		VCPUs: vcpus, MemoryMiB: memoryMiB, SizeBytes: int64(memoryMiB) << 20, CreatedAt: time.Now(),
		Metadata: json.RawMessage(`{"timings_ms":{"paused":250,"total":260}}`),
	}, nil
}

func (b *backend) Rollback(_ context.Context, id, snapID string) (state.VM, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	i, err := b.find(id)
	if err != nil {
		return state.VM{}, err
	}
	if !strings.HasPrefix(snapID, "snap-") {
		return state.VM{}, fmt.Errorf("snapshot %s: %w", snapID, state.ErrNotFound)
	}
	b.vms[i].SnapshotID = snapID
	b.vms[i].Timings["rollback"] = 180.5
	return b.vms[i], nil
}

func (b *backend) Snapshot(_ context.Context, id string) (state.Snapshot, error) {
	v, err := b.Get(context.Background(), id)
	if err != nil {
		return state.Snapshot{}, err
	}
	if v.State != state.StateRunning {
		return state.Snapshot{}, fmt.Errorf("VM %s is %s: %w", id, v.State, vm.ErrNotRunning)
	}
	return state.Snapshot{
		ID: "snap-abcdefghij", Kind: state.SnapshotKindVM, State: state.SnapshotReady, VMID: id, Image: v.Image,
		VCPUs: v.VCPUs, MemoryMiB: v.MemoryMiB, SizeBytes: 1 << 30, CreatedAt: time.Now(),
		Metadata: json.RawMessage(`{"timings_ms":{"prepare":0.1,"pause":0.4,"memory":510.2,"disk":2.3,"resume":0.3,"paused":513.4,"total":520.9}}`),
	}, nil
}

// startTined serves the API over a fake backend and returns its socket.
func startTined(t *testing.T) (string, *backend) {
	t.Helper()
	dir, err := os.MkdirTemp("", "cli") // short: socket paths are limited to about 100 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "tine.sock")
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	b := &backend{}
	srv := &http.Server{Handler: api.NewServer(b, "v-tined", nil), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock, b
}

type result struct {
	stdout, stderr string
	code           int
}

// tine runs the CLI, as main does, against the socket.
func tine(t *testing.T, sock, stdin string, args ...string) result {
	t.Helper()
	return tineCtx(t, context.Background(), sock, stdin, args...)
}

func tineCtx(t *testing.T, ctx context.Context, sock, stdin string, args ...string) result {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(ctx, append([]string{"--socket", sock}, args...), strings.NewReader(stdin), &out, &errOut)
	return result{out.String(), errOut.String(), code}
}

func TestCreate(t *testing.T) {
	sock, b := startTined(t)
	r := tine(t, sock, "", "create", "--vcpus", "4", "--memory", "2G", "--ttl", "30m")
	if r.code != 0 {
		t.Fatalf("exit %d: %s", r.code, r.stderr)
	}
	if b.lastReq.VCPUs != 4 || b.lastReq.MemoryMiB != 2048 || b.lastReq.TTL != 30*time.Minute {
		t.Errorf("tined got %+v", b.lastReq)
	}
	want := "vm-aaaaaaaaab  running  (booted in 1.85s: allocate 0.3ms · network 5.1ms · jail 0.9ms · start 0.1ms · configure 21ms · boot 1.82s)\n"
	if r.stdout != want {
		t.Errorf("output:\n got %q\nwant %q", r.stdout, want)
	}

	if r := tine(t, sock, "", "create", "-q"); r.stdout != "vm-aaaaaaaaac\n" {
		t.Errorf("create -q printed %q, want just the ID", r.stdout)
	}
	r = tine(t, sock, "", "create", "--json")
	var v api.VM
	if err := json.Unmarshal([]byte(r.stdout), &v); err != nil || v.State != "running" {
		t.Errorf("create --json = %q (%v)", r.stdout, err)
	}
	if r := tine(t, sock, "", "create", "--memory", "lots"); r.code != 1 || !strings.Contains(r.stderr, "memory") {
		t.Errorf("bad --memory: exit %d, %q", r.code, r.stderr)
	}
}

func TestList(t *testing.T) {
	sock, _ := startTined(t)
	tine(t, sock, "", "create")
	tine(t, sock, "", "create")
	tine(t, sock, "", "destroy", "vm-aaaaaaaaab")

	r := tine(t, sock, "", "ls")
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "ID") || !strings.Contains(lines[1], "vm-aaaaaaaaac  running  base") ||
		!strings.Contains(lines[1], "1 GiB") || !strings.Contains(lines[1], "1m") {
		t.Errorf("ls:\n%s", r.stdout)
	}
	if r := tine(t, sock, "", "ls", "--all"); strings.Count(r.stdout, "\n") != 3 || !strings.Contains(r.stdout, "destroyed") {
		t.Errorf("ls --all:\n%s", r.stdout)
	}
	var vms []api.VM
	if r := tine(t, sock, "", "ls", "--json"); json.Unmarshal([]byte(r.stdout), &vms) != nil || len(vms) != 1 {
		t.Errorf("ls --json = %q", r.stdout)
	}
}

func TestExec(t *testing.T) {
	sock, b := startTined(t)
	tine(t, sock, "", "create")
	const id = "vm-aaaaaaaaab"

	t.Run("output and success", func(t *testing.T) {
		// No "--" needed: everything after the VM belongs to the command.
		r := tine(t, sock, "", "exec", "-e", "A=1", "-w", "/tmp", id, "echo", "-n", "hello")
		if r.code != 0 || r.stdout != "-n hello\n" {
			t.Errorf("exec = exit %d, %q, %q", r.code, r.stdout, r.stderr)
		}
		if b.lastRun.Dir != "/tmp" || len(b.lastRun.Env) != 1 || b.lastRun.Env[0] != "A=1" {
			t.Errorf("tined got %+v", b.lastRun)
		}
	})
	t.Run("streams split and exit code passes through", func(t *testing.T) {
		r := tine(t, sock, "", "exec", id, "--", "fail")
		if r.code != 3 || r.stdout != "out\n" || r.stderr != "err\n" {
			t.Errorf("exec fail = exit %d, stdout %q, stderr %q; want 3, \"out\\n\", \"err\\n\"", r.code, r.stdout, r.stderr)
		}
	})
	t.Run("stdin", func(t *testing.T) {
		if r := tine(t, sock, "piped in\n", "exec", "-i", id, "cat"); r.stdout != "piped in\n" {
			t.Errorf("exec -i cat = %q", r.stdout)
		}
	})
	t.Run("timeout exits 124", func(t *testing.T) {
		r := tine(t, sock, "", "exec", "--timeout", "2s", id, "sleep", "60")
		if r.code != 124 || !strings.Contains(r.stderr, "timed out after 2s") {
			t.Errorf("exec with a timeout = exit %d, %q", r.code, r.stderr)
		}
		if b.lastRun.TimeoutMs != 2000 {
			t.Errorf("timeout_ms = %d, want 2000", b.lastRun.TimeoutMs)
		}
	})
	t.Run("signal exits 128+n", func(t *testing.T) {
		if r := tine(t, sock, "", "exec", id, "crash"); r.code != 139 || !strings.Contains(r.stderr, "segmentation fault") {
			t.Errorf("exec crash = exit %d, %q; want 139", r.code, r.stderr)
		}
	})
	t.Run("truncated output warns", func(t *testing.T) {
		if r := tine(t, sock, "", "exec", id, "flood"); r.code != 0 || !strings.Contains(r.stderr, "cut off") {
			t.Errorf("exec flood = exit %d, %q", r.code, r.stderr)
		}
	})
	t.Run("a command that can't start", func(t *testing.T) {
		if r := tine(t, sock, "", "exec", id, "nope"); r.code != 1 || !strings.Contains(r.stderr, "executable file not found") {
			t.Errorf("exec nope = exit %d, %q", r.code, r.stderr)
		}
	})
	t.Run("json", func(t *testing.T) {
		r := tine(t, sock, "", "--json", "exec", id, "fail")
		var got struct {
			Stdout, Stderr string
			Exit           api.ExitStatus
		}
		if err := json.Unmarshal([]byte(r.stdout), &got); err != nil || got.Stdout != "out\n" || got.Stderr != "err\n" || got.Exit.Code != 3 || r.code != 3 {
			t.Errorf("exec --json = exit %d, %q (%v)", r.code, r.stdout, err)
		}
	})
}

func TestShortIDs(t *testing.T) {
	sock, _ := startTined(t)
	tine(t, sock, "", "create") // vm-aaaaaaaaab
	tine(t, sock, "", "create") // vm-aaaaaaaaac

	if r := tine(t, sock, "", "exec", "aaaaaaaaab", "echo", "hi"); r.code != 0 || r.stdout != "hi\n" {
		t.Errorf("exec with a short ID = exit %d, %q, %q", r.code, r.stdout, r.stderr)
	}
	if r := tine(t, sock, "", "exec", "vm-aaaa", "echo"); r.code != 1 || !strings.Contains(r.stderr, "several VMs match") {
		t.Errorf("ambiguous ID = exit %d, %q", r.code, r.stderr)
	}
	if r := tine(t, sock, "", "inspect", "zzz"); r.code != 1 || !strings.Contains(r.stderr, `no VM matches "zzz"`) {
		t.Errorf("unknown ID = exit %d, %q", r.code, r.stderr)
	}
}

func TestInspect(t *testing.T) {
	sock, _ := startTined(t)
	tine(t, sock, "", "create")
	r := tine(t, sock, "", "inspect", "vm-aaaaaaaaab")
	var v api.VM
	if err := json.Unmarshal([]byte(r.stdout), &v); err != nil || v.ID != "vm-aaaaaaaaab" || v.TimingsMs["agent"] != 1819.6 {
		t.Errorf("inspect = %q (%v)", r.stdout, err)
	}
}

func TestDestroy(t *testing.T) {
	sock, _ := startTined(t)
	tine(t, sock, "", "create")
	tine(t, sock, "", "create")
	r := tine(t, sock, "", "destroy", "vm-aaaaaaaaab", "vm-nopenopeno", "vm-aaaaaaaaac")
	if r.code != 1 {
		t.Errorf("destroy with one missing VM: exit %d, want 1", r.code)
	}
	if r.stdout != "destroyed vm-aaaaaaaaab (29ms)\ndestroyed vm-aaaaaaaaac (29ms)\n" {
		t.Errorf("destroy output = %q; the others should still be destroyed", r.stdout)
	}
	if !strings.Contains(r.stderr, "vm-nopenopeno: VM vm-nopenopeno: not found") {
		t.Errorf("destroy stderr = %q", r.stderr)
	}
}

func TestSnapshot(t *testing.T) {
	sock, _ := startTined(t)
	id := strings.TrimSpace(tine(t, sock, "", "create", "-q").stdout)
	r := tine(t, sock, "", "snapshot", id[3:7])
	if r.code != 0 || r.stdout != "snap-abcdefghij  of "+id+"  1 GiB  (paused 513ms: memory 510ms · disk 2.3ms; total 521ms)\n" {
		t.Errorf("snapshot = %d %q %q", r.code, r.stdout, r.stderr)
	}
	if r := tine(t, sock, "", "snapshot", "-q", id); r.stdout != "snap-abcdefghij\n" {
		t.Errorf("snapshot -q = %q", r.stdout)
	}
	var snap api.Snapshot
	r = tine(t, sock, "", "--json", "snapshot", id)
	if err := json.Unmarshal([]byte(r.stdout), &snap); err != nil || snap.VMID != id || snap.TimingsMs["paused"] != 513.4 {
		t.Errorf("snapshot --json = %q (%v)", r.stdout, err)
	}
	tine(t, sock, "", "destroy", id)
	if r := tine(t, sock, "", "snapshot", id); r.code != 1 || !strings.Contains(r.stderr, "destroyed") {
		t.Errorf("snapshot of a destroyed VM = %d %q", r.code, r.stderr)
	}
}

func TestCreateFromATemplate(t *testing.T) {
	sock, b := startTined(t)
	b.nextSource = "template"
	r := tine(t, sock, "", "create")
	want := "vm-aaaaaaaaab  running  (restored from template snap-tmplaaaaaa in 52ms: allocate 0.3ms · network 5.1ms · jail 0.9ms · start 0.1ms · restore 15ms · agent 9.2ms · sync 21ms)\n"
	if r.stdout != want {
		t.Errorf("output:\n got %q\nwant %q", r.stdout, want)
	}
	if r := tine(t, sock, "", "create", "--cold"); r.code != 0 || !b.lastReq.Cold {
		t.Errorf("create --cold: exit %d, request %+v", r.code, b.lastReq)
	}
}

func TestImageWarm(t *testing.T) {
	sock, _ := startTined(t)
	r := tine(t, sock, "", "image", "warm", "--vcpus", "4", "--memory", "4G")
	if r.code != 0 || r.stdout != "snap-tmplcccccc  template of base, 4 vCPUs, 4 GiB  (4 GiB, paused 250ms)\n" {
		t.Errorf("image warm = %d %q %q", r.code, r.stdout, r.stderr)
	}
	if r := tine(t, sock, "", "image", "warm", "nope"); r.code != 1 || !strings.Contains(r.stderr, "nope") {
		t.Errorf("warm of a missing image = %d %q", r.code, r.stderr)
	}
}

func TestRollback(t *testing.T) {
	sock, _ := startTined(t)
	id := strings.TrimSpace(tine(t, sock, "", "create", "-q").stdout)
	if r := tine(t, sock, "", "rollback", id[3:7], "--to", "snap-abcdefghij"); r.code != 0 || r.stdout != "rolled back "+id+" to snap-abcdefghij (180ms)\n" {
		t.Errorf("rollback = %d %q %q", r.code, r.stdout, r.stderr)
	}
	if r := tine(t, sock, "", "rollback", id); r.code != 1 || !strings.Contains(r.stderr, "--to") {
		t.Errorf("rollback without --to = %d %q", r.code, r.stderr)
	}
	if r := tine(t, sock, "", "rollback", id, "--to", "nope"); r.code != 1 || !strings.Contains(r.stderr, "nope") {
		t.Errorf("rollback to a missing snapshot = %d %q", r.code, r.stderr)
	}
}

func TestImagesAndVersion(t *testing.T) {
	sock, _ := startTined(t)
	for _, args := range [][]string{{"images"}, {"image", "ls"}} {
		if r := tine(t, sock, "", args...); !strings.Contains(r.stdout, "base  vmlinux-6.18.56  4 GiB  a5f7186  2h ago") ||
			!strings.Contains(r.stdout, "  2 vCPUs 1 GiB, 4 vCPUs 4 GiB (unusable)\n") {
			t.Errorf("%v:\n%s", args, r.stdout)
		}
	}
	r := tine(t, sock, "", "version")
	if !strings.HasPrefix(r.stdout, "tine dev ") || !strings.Contains(r.stdout, "tined v-tined (up 0s, 0 VMs)") {
		t.Errorf("version:\n%s", r.stdout)
	}
}

func TestFriendlyErrors(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "none.sock")
	if r := tine(t, missing, "", "ls"); r.code != 1 || !strings.Contains(r.stderr, "tined isn't running") || !strings.Contains(r.stderr, "systemctl start tined") {
		t.Errorf("no socket: exit %d, %q", r.code, r.stderr)
	}
	if r := tine(t, missing, "", "version"); r.code != 0 || !strings.Contains(r.stdout, "tined: not reachable") {
		t.Errorf("version with no tined should still print tine's version: exit %d, %q", r.code, r.stdout)
	}
	sock, _ := startTined(t)
	if r := tine(t, sock, "", "create", "--bogus"); r.code != 1 || !strings.Contains(r.stderr, "see 'tine create --help'") {
		t.Errorf("unknown flag: exit %d, %q", r.code, r.stderr)
	}
}

func TestFormatting(t *testing.T) {
	for in, want := range map[float64]string{0.25: "0.2ms", 5.1: "5.1ms", 21.1: "21ms", 1819.6: "1.82s"} {
		if got := formatMillis(in); got != want {
			t.Errorf("formatMillis(%v) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]int{"512M": 512, "2G": 2048, "1.5GiB": 1536, "1024": 1024, "1gb": 1024} {
		if got, err := parseMemory(in); err != nil || got != want {
			t.Errorf("parseMemory(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for d, want := range map[time.Duration]string{30 * time.Second: "30s", 12 * time.Minute: "12m", 3 * time.Hour: "3h", 72 * time.Hour: "3d", -time.Second: "0s"} {
		if got := humanDuration(d); got != want {
			t.Errorf("humanDuration(%v) = %q, want %q", d, got, want)
		}
	}
	for n, want := range map[int64]string{300 << 20: "300 MiB", 4 << 30: "4 GiB", 1390083686: "1.3 GiB"} {
		if got := formatBytes(n); got != want {
			t.Errorf("formatBytes(%d) = %q, want %q", n, got, want)
		}
	}
	for mib, want := range map[int]string{512: "512 MiB", 1024: "1 GiB", 1536: "1.5 GiB"} {
		if got := formatMiB(mib); got != want {
			t.Errorf("formatMiB(%d) = %q, want %q", mib, got, want)
		}
	}
}

func TestExecNeedsACommand(t *testing.T) {
	sock, _ := startTined(t)
	tine(t, sock, "", "create")
	if r := tine(t, sock, "", "exec", "vm-aaaaaaaaab", "--"); r.code != 1 || !strings.Contains(r.stderr, "no command to run") {
		t.Errorf("exec with no command = exit %d, %q", r.code, r.stderr)
	}
}

func TestInterruptExitsWith130(t *testing.T) {
	sock, _ := startTined(t)
	tine(t, sock, "", "create")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond) // Ctrl+C after 200ms
	defer cancel()
	r := tineCtx(t, ctx, sock, "", "exec", "vm-aaaaaaaaab", "hang")
	if r.code != 130 || strings.TrimSpace(r.stderr) != "tine: interrupted" {
		t.Errorf("interrupted exec = exit %d, %q; want 130 and \"tine: interrupted\"", r.code, r.stderr)
	}
}
