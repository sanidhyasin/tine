package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sanidhyasin/tine/internal/agentproto"
	"github.com/sanidhyasin/tine/internal/image"
	"github.com/sanidhyasin/tine/internal/logging"
	"github.com/sanidhyasin/tine/internal/metrics"
	"github.com/sanidhyasin/tine/internal/snapshot"
	"github.com/sanidhyasin/tine/internal/state"
	"github.com/sanidhyasin/tine/internal/vm"
)

// fakeBackend is an in-memory Backend.
type fakeBackend struct {
	mu      sync.Mutex
	vms     map[string]state.VM
	next    int
	err     error // returned by every call, when set
	lastReq vm.CreateRequest
	panicOn string
	// lastAttrs are the log attributes the context of the last Get carried.
	lastAttrs []slog.Attr
}

func newFakeBackend() *fakeBackend { return &fakeBackend{vms: map[string]state.VM{}} }

func (*fakeBackend) Templates(context.Context) ([]vm.Template, error) {
	return []vm.Template{
		{Snapshot: state.Snapshot{ID: "snap-tmplaaaaaa", Kind: state.SnapshotKindTemplate, Image: "base", VCPUs: 2, MemoryMiB: 1024, CreatedAt: time.Now().Add(-time.Hour)}},
		{Snapshot: state.Snapshot{ID: "snap-tmplbbbbbb", Kind: state.SnapshotKindTemplate, Image: "base", VCPUs: 4, MemoryMiB: 4096, CreatedAt: time.Now()}, Problem: "image base was imported again since"},
	}, nil
}

func (*fakeBackend) WarmTemplate(_ context.Context, image string, vcpus, memoryMiB int) (state.Snapshot, error) {
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

func (f *fakeBackend) Snapshots(_ context.Context, flt state.SnapshotFilter) ([]vm.SnapshotInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	all := []vm.SnapshotInfo{
		{Snapshot: state.Snapshot{ID: "snap-aaaaaaaaaa", Kind: state.SnapshotKindVM, State: state.SnapshotReady, VMID: "vm-1", Image: "base", VCPUs: 2, MemoryMiB: 1024}, UsedBy: []string{"vm-2"}},
		{Snapshot: state.Snapshot{ID: "snap-tmplaaaaaa", Kind: state.SnapshotKindTemplate, State: state.SnapshotReady, Image: "base", VCPUs: 2, MemoryMiB: 1024}},
	}
	var out []vm.SnapshotInfo
	for _, s := range all {
		if (flt.VMID == "" || s.VMID == flt.VMID) && (flt.Kind == "" || s.Kind == flt.Kind) {
			out = append(out, s)
		}
	}
	return out, nil
}

func (f *fakeBackend) GetSnapshot(ctx context.Context, id string) (vm.SnapshotInfo, error) {
	all, _ := f.Snapshots(ctx, state.SnapshotFilter{})
	for _, s := range all {
		if s.ID == id {
			return s, nil
		}
	}
	return vm.SnapshotInfo{}, fmt.Errorf("snapshot %s: %w", id, state.ErrNotFound)
}

func (f *fakeBackend) DeleteSnapshot(ctx context.Context, id string) (state.Snapshot, error) {
	s, err := f.GetSnapshot(ctx, id)
	if err != nil {
		return state.Snapshot{}, err
	}
	if len(s.UsedBy) > 0 {
		return state.Snapshot{}, fmt.Errorf("snapshot %s: %w", id, state.ErrInUse)
	}
	s.State = state.SnapshotDeleting
	return s.Snapshot, nil
}

func (f *fakeBackend) Rollback(_ context.Context, id, snapID string) (state.VM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.vms[id]
	if !ok {
		return state.VM{}, fmt.Errorf("VM %s: %w", id, state.ErrNotFound)
	}
	if snapID != "snap-0000000001" {
		return state.VM{}, fmt.Errorf("snapshot %s: %w", snapID, state.ErrNotFound)
	}
	v.SnapshotID, v.Timings["rollback"] = snapID, 180.5
	f.vms[id] = v
	return v, nil
}

func (f *fakeBackend) Snapshot(_ context.Context, id string) (state.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return state.Snapshot{}, f.err
	}
	v, ok := f.vms[id]
	if !ok {
		return state.Snapshot{}, fmt.Errorf("VM %s: %w", id, state.ErrNotFound)
	}
	return state.Snapshot{
		ID: "snap-0000000001", Kind: state.SnapshotKindVM, State: state.SnapshotReady, VMID: id, Image: v.Image,
		VCPUs: v.VCPUs, MemoryMiB: v.MemoryMiB, SizeBytes: 1 << 30, CreatedAt: time.Now(),
		Metadata: json.RawMessage(`{"host":{"firecracker_version":"1.17.0"},"timings_ms":{"paused":120.5,"total":150}}`),
	}, nil
}

func (f *fakeBackend) Create(_ context.Context, req vm.CreateRequest) (state.VM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.panicOn == "create" {
		panic("boom")
	}
	if f.err != nil {
		return state.VM{}, f.err
	}
	f.lastReq = req
	f.next++
	v := state.VM{ID: fmt.Sprintf("vm-%010d", f.next), State: state.StateRunning, Image: "base", VCPUs: 2, MemoryMiB: 1024,
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour), Timings: map[string]float64{"total": 1869.1}}
	f.vms[v.ID] = v
	return v, nil
}

func (f *fakeBackend) Get(ctx context.Context, id string) (state.VM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastAttrs = logging.Attrs(ctx)
	v, ok := f.vms[id]
	if !ok {
		return state.VM{}, fmt.Errorf("VM %s: %w", id, vm.ErrNotFound)
	}
	return v, nil
}

func (f *fakeBackend) List(_ context.Context, all bool) ([]state.VM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	var out []state.VM
	for _, v := range f.vms {
		if all || v.State != state.StateDestroyed {
			out = append(out, v)
		}
	}
	return out, nil
}

func (f *fakeBackend) Destroy(_ context.Context, id string) (state.VM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.vms[id]
	if !ok {
		return state.VM{}, fmt.Errorf("VM %s: %w", id, vm.ErrNotFound)
	}
	v.State, v.DestroyedAt = state.StateDestroyed, time.Now()
	f.vms[id] = v
	return v, nil
}

func (f *fakeBackend) Exec(_ context.Context, id string, req agentproto.ExecRequest, onOutput func(string, []byte)) (agentproto.ExitStatus, error) {
	v, err := f.Get(context.Background(), id)
	if err != nil {
		return agentproto.ExitStatus{}, err
	}
	if v.State != state.StateRunning {
		return agentproto.ExitStatus{}, fmt.Errorf("VM %s is %s: %w", id, v.State, vm.ErrNotRunning)
	}
	switch req.Args[0] {
	case "missing":
		return agentproto.ExitStatus{}, &agentproto.Error{StatusCode: http.StatusUnprocessableEntity, ErrorDetail: agentproto.ErrorDetail{Code: agentproto.CodeStartFailed, Message: "no such file"}}
	case "cut":
		onOutput(agentproto.StreamStdout, []byte("partial"))
		return agentproto.ExitStatus{}, agentproto.ErrIncomplete
	}
	onOutput(agentproto.StreamStdout, []byte(strings.Join(req.Args[1:], " ")+"\n"))
	onOutput(agentproto.StreamStderr, []byte("warning\n"))
	return agentproto.ExitStatus{Code: 7, DurationMs: 1.5}, nil
}

func (f *fakeBackend) Images(context.Context) ([]*image.Image, error) {
	return []*image.Image{{Name: "base", Meta: image.Metadata{Kernel: image.KernelInfo{Source: "vmlinux-6.18.56"}, Rootfs: image.RootfsInfo{SizeBytes: 4 << 30}, Agent: image.AgentInfo{Version: "v1"}}}}, nil
}

func (f *fakeBackend) Ping(context.Context) error { return nil }

// serve runs the API on a Unix socket for the test and returns a client.
func serve(t *testing.T, b Backend, opts ...Option) (*Client, string) {
	return serveWithLog(t, b, nil, opts...)
}

func serveWithLog(t *testing.T, b Backend, log *slog.Logger, opts ...Option) (*Client, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "api") // short: socket paths are limited to about 100 bytes
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
	srv := &http.Server{Handler: NewServer(b, "v-test", log, opts...), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return NewUnixClient(sock), sock
}

func TestHealth(t *testing.T) {
	c, _ := serve(t, newFakeBackend())
	h, err := c.Health(context.Background())
	if err != nil || h.Status != "ok" || h.Version != "v-test" {
		t.Errorf("Health = %+v, %v", h, err)
	}
}

func TestVMLifecycleThroughTheAPI(t *testing.T) {
	b := newFakeBackend()
	c, _ := serve(t, b)
	ctx := context.Background()

	v, err := c.CreateVM(ctx, CreateVMRequest{VCPUs: 4, MemoryMiB: 2048, TTLSeconds: 600})
	if err != nil {
		t.Fatalf("CreateVM: %v", err)
	}
	if b.lastReq.VCPUs != 4 || b.lastReq.MemoryMiB != 2048 || b.lastReq.TTL != 10*time.Minute {
		t.Errorf("backend got %+v", b.lastReq)
	}
	if v.State != "running" || v.ExpiresAt == nil || v.DestroyedAt != nil || v.TimingsMs["total"] != 1869.1 {
		t.Errorf("created VM = %+v", v)
	}

	got, err := c.GetVM(ctx, v.ID)
	if err != nil || got.ID != v.ID {
		t.Errorf("GetVM = %+v, %v", got, err)
	}
	list, err := c.ListVMs(ctx, false)
	if err != nil || len(list) != 1 {
		t.Errorf("ListVMs = %+v, %v", list, err)
	}

	gone, err := c.DestroyVM(ctx, v.ID)
	if err != nil || gone.State != "destroyed" || gone.DestroyedAt == nil {
		t.Errorf("DestroyVM = %+v, %v", gone, err)
	}
	if list, _ := c.ListVMs(ctx, false); len(list) != 0 {
		t.Errorf("destroyed VM still listed: %+v", list)
	}
	if list, _ := c.ListVMs(ctx, true); len(list) != 1 {
		t.Errorf("?all=true doesn't include the destroyed VM: %+v", list)
	}
}

func TestEmptyListsAreArrays(t *testing.T) {
	_, sock := serve(t, newFakeBackend())
	if body := rawBody(t, sock, http.MethodGet, "/v1/vms", ""); !strings.Contains(body, `"vms":[]`) {
		t.Errorf("empty list = %s, want \"vms\":[] (not null)", body)
	}
}

func TestExecThroughTheAPI(t *testing.T) {
	b := newFakeBackend()
	c, _ := serve(t, b)
	ctx := context.Background()
	v, _ := c.CreateVM(ctx, CreateVMRequest{})

	var stdout, stderr strings.Builder
	st, err := c.Exec(ctx, v.ID, ExecRequest{Args: []string{"echo", "hello", "world"}}, func(stream string, d []byte) {
		if stream == agentproto.StreamStdout {
			stdout.Write(d)
		} else {
			stderr.Write(d)
		}
	})
	if err != nil || st.Code != 7 || stdout.String() != "hello world\n" || stderr.String() != "warning\n" {
		t.Errorf("Exec = %+v, %v; stdout %q, stderr %q", st, err, stdout.String(), stderr.String())
	}

	// The agent's own errors pass through with its status and code.
	_, err = c.Exec(ctx, v.ID, ExecRequest{Args: []string{"missing"}}, nil)
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnprocessableEntity || apiErr.Code != agentproto.CodeStartFailed {
		t.Errorf("Exec of a missing command = %#v", err)
	}

	// A stream that breaks after output began ends without an exit frame.
	var got string
	if _, err := c.Exec(ctx, v.ID, ExecRequest{Args: []string{"cut"}}, func(_ string, d []byte) { got += string(d) }); !errors.Is(err, agentproto.ErrIncomplete) || got != "partial" {
		t.Errorf("cut-short Exec = %v (output %q), want ErrIncomplete after \"partial\"", err, got)
	}

	if _, err := c.DestroyVM(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	_, err = c.Exec(ctx, v.ID, ExecRequest{Args: []string{"echo"}}, nil)
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusConflict || apiErr.Code != CodeNotRunning {
		t.Errorf("Exec on a destroyed VM = %#v, want 409 not_running", err)
	}
}

func TestSnapshotAPI(t *testing.T) {
	ctx := context.Background()
	c, _ := serve(t, newFakeBackend())
	v, err := c.CreateVM(ctx, CreateVMRequest{})
	if err != nil {
		t.Fatal(err)
	}
	snap, err := c.CreateSnapshot(ctx, v.ID)
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if snap.ID != "snap-0000000001" || snap.VMID != v.ID || snap.Kind != "vm" || snap.State != "ready" ||
		snap.SizeBytes != 1<<30 || snap.FirecrackerVersion != "1.17.0" || snap.TimingsMs["paused"] != 120.5 {
		t.Errorf("snapshot = %+v", snap)
	}
	_, err = c.CreateSnapshot(ctx, "vm-nope")
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound {
		t.Errorf("snapshot of a missing VM = %#v, want 404", err)
	}
}

func TestRollbackAPI(t *testing.T) {
	ctx := context.Background()
	c, sock := serve(t, newFakeBackend())
	v, err := c.CreateVM(ctx, CreateVMRequest{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Rollback(ctx, v.ID, "snap-0000000001")
	if err != nil || got.ID != v.ID || got.SnapshotID != "snap-0000000001" || got.TimingsMs["rollback"] != 180.5 {
		t.Errorf("Rollback = %+v, %v", got, err)
	}
	var apiErr *Error
	if _, err := c.Rollback(ctx, v.ID, "snap-nope"); !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound {
		t.Errorf("rollback to a missing snapshot = %#v", err)
	}
	if status, _, _ := rawStatus(t, sock, http.MethodPost, "/v1/vms/"+v.ID+"/rollback", `{}`); status != http.StatusBadRequest {
		t.Errorf("rollback without a snapshot = %d, want 400", status)
	}
}

func TestSnapshotListing(t *testing.T) {
	ctx := context.Background()
	c, sock := serve(t, newFakeBackend())
	all, err := c.ListSnapshots(ctx, SnapshotFilter{})
	if err != nil || len(all) != 2 || all[0].UsedBy[0] != "vm-2" {
		t.Fatalf("ListSnapshots = %+v, %v", all, err)
	}
	if got, _ := c.ListSnapshots(ctx, SnapshotFilter{Kind: "template"}); len(got) != 1 || got[0].ID != "snap-tmplaaaaaa" {
		t.Errorf("templates = %+v", got)
	}
	var vmOnes ListSnapshotsResponse
	if status := getJSON(t, sock, "/v1/vms/vm-1/snapshots", &vmOnes); status != http.StatusOK || len(vmOnes.Snapshots) != 1 {
		t.Errorf("GET /v1/vms/vm-1/snapshots = %d %+v", status, vmOnes)
	}
	if got, err := c.GetSnapshot(ctx, "snap-aaaaaaaaaa"); err != nil || got.VMID != "vm-1" {
		t.Errorf("GetSnapshot = %+v, %v", got, err)
	}
	var apiErr *Error
	if _, err := c.DeleteSnapshot(ctx, "snap-aaaaaaaaaa"); !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusConflict || apiErr.Code != CodeInUse {
		t.Errorf("deleting a snapshot in use = %#v, want 409 in_use", err)
	}
	if got, err := c.DeleteSnapshot(ctx, "snap-tmplaaaaaa"); err != nil || got.State != "deleting" {
		t.Errorf("DeleteSnapshot = %+v, %v", got, err)
	}
	if _, err := c.GetSnapshot(ctx, "snap-nope"); !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound {
		t.Errorf("GetSnapshot of nothing = %#v", err)
	}
}

func TestErrorsMapToStatusCodes(t *testing.T) {
	tests := []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("x: %w", vm.ErrInvalidRequest), http.StatusBadRequest, CodeInvalidRequest},
		{fmt.Errorf("x: %w", vm.ErrImageNotFound), http.StatusNotFound, CodeImageNotFound},
		{fmt.Errorf("x: %w", vm.ErrCapacity), http.StatusServiceUnavailable, CodeCapacity},
		{fmt.Errorf("x: %w", state.ErrInvalidTransition), http.StatusConflict, CodeConflict},
		{fmt.Errorf("x: %w", vm.ErrBusy), http.StatusConflict, CodeBusy},
		{fmt.Errorf("x: %w", state.ErrInUse), http.StatusConflict, CodeInUse},
		{fmt.Errorf("x: %w", snapshot.ErrIncompatible), http.StatusConflict, CodeIncompatible},
		{fmt.Errorf("x: %w", vm.ErrNoSpace), http.StatusInsufficientStorage, CodeNoSpace},
		{errors.New("disk on fire"), http.StatusInternalServerError, CodeInternal},
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			b := newFakeBackend()
			b.err = tt.err
			c, _ := serve(t, b)
			_, err := c.CreateVM(context.Background(), CreateVMRequest{})
			var apiErr *Error
			if !errors.As(err, &apiErr) || apiErr.StatusCode != tt.status || apiErr.Code != tt.code {
				t.Errorf("error = %#v, want %d %s", err, tt.status, tt.code)
			}
		})
	}
	t.Run("missing VM", func(t *testing.T) {
		c, _ := serve(t, newFakeBackend())
		_, err := c.GetVM(context.Background(), "vm-nope")
		var apiErr *Error
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound || apiErr.Code != CodeNotFound {
			t.Errorf("GetVM of a missing VM = %#v", err)
		}
	})
}

func TestBadRequests(t *testing.T) {
	_, sock := serve(t, newFakeBackend())
	tests := []struct {
		method, path, body string
		status             int
		allow              string
	}{
		{http.MethodPost, "/v1/vms", "{nope", http.StatusBadRequest, ""},
		{http.MethodPost, "/v1/vms", `{"cpus": 2}`, http.StatusBadRequest, ""}, // unknown field: a typo of vcpus
		{http.MethodPost, "/v1/vms", `{"ttl_seconds": -1}`, http.StatusBadRequest, ""},
		{http.MethodPut, "/v1/vms", "", http.StatusMethodNotAllowed, "GET, POST"},
		{http.MethodPost, "/v1/vms/x", "", http.StatusMethodNotAllowed, "GET, DELETE"},
		{http.MethodGet, "/v1/vms/x/exec", "", http.StatusMethodNotAllowed, "POST"},
		{http.MethodGet, "/v2/nope", "", http.StatusNotFound, ""},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path+" "+tt.body, func(t *testing.T) {
			status, allow, ct := rawStatus(t, sock, tt.method, tt.path, tt.body)
			if status != tt.status || allow != tt.allow || ct != "application/json" {
				t.Errorf("got %d (Allow %q, %s), want %d (Allow %q, application/json)", status, allow, ct, tt.status, tt.allow)
			}
		})
	}
}

func TestPanicsBecome500s(t *testing.T) {
	b := newFakeBackend()
	b.panicOn = "create"
	c, _ := serve(t, b)
	_, err := c.CreateVM(context.Background(), CreateVMRequest{})
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("CreateVM with a panicking backend = %#v, want a 500", err)
	}
	if _, err := c.Health(context.Background()); err != nil {
		t.Errorf("the server should survive a panic: %v", err)
	}
}

func TestImages(t *testing.T) {
	c, _ := serve(t, newFakeBackend())
	imgs, err := c.Images(context.Background())
	if err != nil || len(imgs) != 1 || imgs[0].Name != "base" || imgs[0].Kernel != "vmlinux-6.18.56" || imgs[0].DiskBytes != 4<<30 {
		t.Fatalf("Images = %+v, %v", imgs, err)
	}
	tm := imgs[0].Templates
	if len(tm) != 2 || tm[0].SnapshotID != "snap-tmplaaaaaa" || tm[0].Problem != "" || tm[1].MemoryMiB != 4096 || tm[1].Problem == "" {
		t.Errorf("templates = %+v", tm)
	}
}

func TestWarmTemplate(t *testing.T) {
	ctx := context.Background()
	c, _ := serve(t, newFakeBackend())
	snap, err := c.WarmTemplate(ctx, "base", WarmRequest{VCPUs: 4})
	if err != nil || snap.Kind != "template" || snap.VCPUs != 4 || snap.MemoryMiB != 1024 || snap.Image != "base" {
		t.Errorf("WarmTemplate = %+v, %v", snap, err)
	}
	_, err = c.WarmTemplate(ctx, "nope", WarmRequest{})
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound || apiErr.Code != CodeImageNotFound {
		t.Errorf("WarmTemplate of a missing image = %#v", err)
	}
}

func TestCreateCanAskForAColdBoot(t *testing.T) {
	b := newFakeBackend()
	c, _ := serve(t, b)
	if _, err := c.CreateVM(context.Background(), CreateVMRequest{Cold: true}); err != nil {
		t.Fatal(err)
	}
	if !b.lastReq.Cold {
		t.Error("cold didn't reach the backend")
	}
}

// --- raw HTTP helpers ---

func rawClient(sock string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}}
}

func rawStatus(t *testing.T, sock, method, path, body string) (status int, allow, contentType string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, "http://tined"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rawClient(sock).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Allow"), resp.Header.Get("Content-Type")
}

func rawBody(t *testing.T, sock, method, path, body string) string {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), method, "http://tined"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rawClient(sock).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String()
}

func TestMetricsEndpointAndRequestMetrics(t *testing.T) {
	m := metrics.New("v-test")
	c, sock := serve(t, newFakeBackend(), WithMetrics(m))
	ctx := context.Background()
	v, _ := c.CreateVM(ctx, CreateVMRequest{})
	_, _ = c.GetVM(ctx, v.ID)
	_, _ = c.GetVM(ctx, "vm-nope")

	body := rawBody(t, sock, http.MethodGet, "/metrics", "")
	for _, want := range []string{
		`tine_api_requests_total{code="201",method="POST",route="/v1/vms"} 1`,
		// One series for every VM ID: the route pattern, not the path.
		`tine_api_requests_total{code="200",method="GET",route="/v1/vms/{id}"} 1`,
		`tine_api_requests_total{code="404",method="GET",route="/v1/vms/{id}"} 1`,
		`tine_build_info{version="v-test"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics lacks %q", want)
		}
	}
	if strings.Contains(body, v.ID) {
		t.Error("a VM ID leaked into a metric label")
	}
}

func TestNoMetricsEndpointWithoutMetrics(t *testing.T) {
	_, sock := serve(t, newFakeBackend())
	if status, _, _ := rawStatus(t, sock, http.MethodGet, "/metrics", ""); status != http.StatusNotFound {
		t.Errorf("/metrics without metrics = %d, want 404", status)
	}
}

func TestRequestIDs(t *testing.T) {
	_, sock := serve(t, newFakeBackend())
	get := func(id string) string {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://tined/healthz", nil)
		if id != "" {
			req.Header.Set(RequestIDHeader, id)
		}
		resp, err := rawClient(sock).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.Header.Get(RequestIDHeader)
	}
	if got := get(""); len(got) != 12 {
		t.Errorf("generated request ID %q, want 12 hex characters", got)
	}
	if got := get("trace-123_ab"); got != "trace-123_ab" {
		t.Errorf("caller's request ID came back as %q", got)
	}
	if got := get("evil\"id"); got == "evil\"id" || len(got) != 12 {
		t.Errorf("a request ID with odd characters was kept: %q", got)
	}
}

// Every log line for a request carries its request ID, and lines about one
// VM carry its ID, including lines written deep inside the backend.
func TestRequestAndVMIDsReachTheBackendAndTheLog(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(logging.NewHandler(slog.NewJSONHandler(&buf, nil)))
	b := newFakeBackend()
	c, sock := serveWithLog(t, b, log)
	v, _ := c.CreateVM(context.Background(), CreateVMRequest{})
	buf.Reset()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://tined/v1/vms/"+v.ID, nil)
	req.Header.Set(RequestIDHeader, "req-42")
	resp, err := rawClient(sock).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	got := map[string]string{}
	for _, a := range b.lastAttrs {
		got[a.Key] = a.Value.String()
	}
	if got["request_id"] != "req-42" || got["vm"] != v.ID {
		t.Errorf("the backend's context carried %v; want request_id and vm", got)
	}
	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("request log isn't one JSON line: %q", buf.String())
	}
	if line["request_id"] != "req-42" || line["vm"] != v.ID || line["route"] != "/v1/vms/{id}" || line["status"] != float64(200) {
		t.Errorf("request log line = %v", line)
	}
}

// getJSON GETs path from the server on sock and decodes the answer into out.
func getJSON(t *testing.T, sock, path string, out any) int {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://tined"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rawClient(sock).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode
}
