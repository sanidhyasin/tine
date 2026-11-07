package api

import (
	"context"
	"errors"
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
	"github.com/sanidhyasin/tine/internal/image"
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
}

func newFakeBackend() *fakeBackend { return &fakeBackend{vms: map[string]state.VM{}} }

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

func (f *fakeBackend) Get(_ context.Context, id string) (state.VM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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
func serve(t *testing.T, b Backend) (*Client, string) {
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
	srv := &http.Server{Handler: NewServer(b, "v-test", nil), ReadHeaderTimeout: 5 * time.Second}
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
		t.Errorf("Images = %+v, %v", imgs, err)
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
