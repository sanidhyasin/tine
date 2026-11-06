package firecracker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeFirecracker is an HTTP server on a Unix socket that records requests
// and answers them with handler.
type fakeFirecracker struct {
	socket string

	mu       sync.Mutex
	requests []recordedRequest
}

type recordedRequest struct {
	Method      string
	Path        string // escaped, exactly as sent
	ContentType string
	Body        any // decoded JSON, or nil for no body
}

func newFakeFirecracker(t *testing.T, handler http.HandlerFunc) *fakeFirecracker {
	t.Helper()
	f := &fakeFirecracker{socket: shortSocketPath(t)}
	ln, err := listenUnix(f.socket)
	if err != nil {
		t.Fatalf("listen on %s: %v", f.socket, err)
	}
	srv := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			f.record(t, r)
			handler(w, r)
		}),
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return f
}

func (f *fakeFirecracker) record(t *testing.T, r *http.Request) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("fake firecracker: read body: %v", err)
	}
	var body any
	if len(data) > 0 {
		if err := json.Unmarshal(data, &body); err != nil {
			t.Errorf("fake firecracker: request body isn't JSON: %q", data)
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, recordedRequest{
		Method:      r.Method,
		Path:        r.URL.EscapedPath(),
		ContentType: r.Header.Get("Content-Type"),
		Body:        body,
	})
}

func (f *fakeFirecracker) last(t *testing.T) recordedRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		t.Fatal("fake firecracker received no requests")
	}
	return f.requests[len(f.requests)-1]
}

// shortSocketPath returns a socket path in a fresh temporary directory.
// t.TempDir() can exceed the 104-byte limit on socket paths on macOS.
func shortSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "fc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "api.sock")
}

func noContent(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }

func jsonBody(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("bad JSON in test: %v", err)
	}
	return v
}

func TestCallsSendTheRightRequests(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name       string
		call       func(*Client) error
		wantMethod string
		wantPath   string
		wantBody   string // JSON; empty means no body
	}{
		{
			name: "logger",
			call: func(c *Client) error {
				return c.PutLogger(ctx, Logger{LogPath: "/fc.log", Level: LogLevelDebug, ShowLevel: true})
			},
			wantMethod: http.MethodPut, wantPath: "/logger",
			wantBody: `{"log_path": "/fc.log", "level": "Debug", "show_level": true}`,
		},
		{
			name:       "metrics",
			call:       func(c *Client) error { return c.PutMetrics(ctx, Metrics{MetricsPath: "/metrics.json"}) },
			wantMethod: http.MethodPut, wantPath: "/metrics",
			wantBody: `{"metrics_path": "/metrics.json"}`,
		},
		{
			name:       "machine config leaves optional fields to Firecracker",
			call:       func(c *Client) error { return c.PutMachineConfig(ctx, MachineConfig{VCPUCount: 2, MemSizeMiB: 1024}) },
			wantMethod: http.MethodPut, wantPath: "/machine-config",
			wantBody: `{"vcpu_count": 2, "mem_size_mib": 1024}`,
		},
		{
			name: "machine config with dirty-page tracking",
			call: func(c *Client) error {
				return c.PutMachineConfig(ctx, MachineConfig{VCPUCount: 1, MemSizeMiB: 256, TrackDirtyPages: true, HugePages: HugePagesNone})
			},
			wantMethod: http.MethodPut, wantPath: "/machine-config",
			wantBody: `{"vcpu_count": 1, "mem_size_mib": 256, "track_dirty_pages": true, "huge_pages": "None"}`,
		},
		{
			name: "boot source",
			call: func(c *Client) error {
				return c.PutBootSource(ctx, BootSource{KernelImagePath: "/vmlinux", BootArgs: "console=ttyS0 reboot=k panic=1"})
			},
			wantMethod: http.MethodPut, wantPath: "/boot-source",
			wantBody: `{"kernel_image_path": "/vmlinux", "boot_args": "console=ttyS0 reboot=k panic=1"}`,
		},
		{
			name: "drive always sends the required flags, even when false",
			call: func(c *Client) error {
				return c.PutDrive(ctx, Drive{DriveID: "rootfs", PathOnHost: "/rootfs.ext4", IsRootDevice: true})
			},
			wantMethod: http.MethodPut, wantPath: "/drives/rootfs",
			wantBody: `{"drive_id": "rootfs", "path_on_host": "/rootfs.ext4", "is_root_device": true, "is_read_only": false}`,
		},
		{
			name: "drive ID is escaped in the path",
			call: func(c *Client) error {
				return c.PutDrive(ctx, Drive{DriveID: "a b/c", PathOnHost: "/d.ext4"})
			},
			wantMethod: http.MethodPut, wantPath: "/drives/a%20b%2Fc",
			wantBody: `{"drive_id": "a b/c", "path_on_host": "/d.ext4", "is_root_device": false, "is_read_only": false}`,
		},
		{
			name: "network interface",
			call: func(c *Client) error {
				return c.PutNetworkInterface(ctx, NetworkInterface{IfaceID: "eth0", HostDevName: "tap0", GuestMAC: "06:00:AC:10:00:02"})
			},
			wantMethod: http.MethodPut, wantPath: "/network-interfaces/eth0",
			wantBody: `{"iface_id": "eth0", "host_dev_name": "tap0", "guest_mac": "06:00:AC:10:00:02"}`,
		},
		{
			name:       "vsock",
			call:       func(c *Client) error { return c.PutVsock(ctx, Vsock{GuestCID: 3, UDSPath: "/vsock.sock"}) },
			wantMethod: http.MethodPut, wantPath: "/vsock",
			wantBody: `{"guest_cid": 3, "uds_path": "/vsock.sock"}`,
		},
		{
			name:       "entropy device with defaults",
			call:       func(c *Client) error { return c.PutEntropy(ctx, EntropyDevice{}) },
			wantMethod: http.MethodPut, wantPath: "/entropy",
			wantBody: `{}`,
		},
		{
			name: "entropy device with a rate limit",
			call: func(c *Client) error {
				return c.PutEntropy(ctx, EntropyDevice{RateLimiter: &RateLimiter{Bandwidth: &TokenBucket{Size: 1000, RefillTimeMs: 100}}})
			},
			wantMethod: http.MethodPut, wantPath: "/entropy",
			wantBody: `{"rate_limiter": {"bandwidth": {"size": 1000, "refill_time": 100}}}`,
		},
		{
			name:       "start",
			call:       func(c *Client) error { return c.StartInstance(ctx) },
			wantMethod: http.MethodPut, wantPath: "/actions",
			wantBody: `{"action_type": "InstanceStart"}`,
		},
		{
			name:       "ctrl-alt-del",
			call:       func(c *Client) error { return c.SendCtrlAltDel(ctx) },
			wantMethod: http.MethodPut, wantPath: "/actions",
			wantBody: `{"action_type": "SendCtrlAltDel"}`,
		},
		{
			name:       "flush metrics",
			call:       func(c *Client) error { return c.FlushMetrics(ctx) },
			wantMethod: http.MethodPut, wantPath: "/actions",
			wantBody: `{"action_type": "FlushMetrics"}`,
		},
		{
			name:       "pause",
			call:       func(c *Client) error { return c.Pause(ctx) },
			wantMethod: http.MethodPatch, wantPath: "/vm",
			wantBody: `{"state": "Paused"}`,
		},
		{
			name:       "resume",
			call:       func(c *Client) error { return c.Resume(ctx) },
			wantMethod: http.MethodPatch, wantPath: "/vm",
			wantBody: `{"state": "Resumed"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeFirecracker(t, noContent)
			if err := tt.call(New(fake.socket)); err != nil {
				t.Fatalf("call failed: %v", err)
			}
			got := fake.last(t)
			if got.Method != tt.wantMethod || got.Path != tt.wantPath {
				t.Errorf("sent %s %s, want %s %s", got.Method, got.Path, tt.wantMethod, tt.wantPath)
			}
			if got.ContentType != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got.ContentType)
			}
			if want := jsonBody(t, tt.wantBody); !reflect.DeepEqual(got.Body, want) {
				gotJSON, _ := json.Marshal(got.Body)
				t.Errorf("body = %s, want %s", gotJSON, tt.wantBody)
			}
		})
	}
}

func TestGetCallsDecodeResponses(t *testing.T) {
	fake := newFakeFirecracker(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/":
			_, _ = io.WriteString(w, `{"app_name": "Firecracker", "id": "vm-1", "state": "Running", "vmm_version": "1.17.0"}`)
		case "/version":
			_, _ = io.WriteString(w, `{"firecracker_version": "1.17.0"}`)
		case "/machine-config":
			_, _ = io.WriteString(w, `{"vcpu_count": 2, "mem_size_mib": 1024, "smt": false, "track_dirty_pages": true, "huge_pages": "None"}`)
		default:
			http.NotFound(w, r)
		}
	})
	c := New(fake.socket)
	ctx := context.Background()

	info, err := c.DescribeInstance(ctx)
	if err != nil {
		t.Fatalf("DescribeInstance: %v", err)
	}
	if want := (InstanceInfo{AppName: "Firecracker", ID: "vm-1", State: StateRunning, VMMVersion: "1.17.0"}); info != want {
		t.Errorf("DescribeInstance = %+v, want %+v", info, want)
	}
	if got := fake.last(t); got.Method != http.MethodGet || got.Body != nil || got.ContentType != "" {
		t.Errorf("GET sent method %s, body %v, Content-Type %q; want GET with no body", got.Method, got.Body, got.ContentType)
	}

	version, err := c.Version(ctx)
	if err != nil || version != "1.17.0" {
		t.Errorf("Version = %q, %v; want 1.17.0", version, err)
	}

	mc, err := c.GetMachineConfig(ctx)
	if err != nil {
		t.Fatalf("GetMachineConfig: %v", err)
	}
	if want := (MachineConfig{VCPUCount: 2, MemSizeMiB: 1024, TrackDirtyPages: true, HugePages: HugePagesNone}); mc != want {
		t.Errorf("GetMachineConfig = %+v, want %+v", mc, want)
	}
}

func TestErrorResponses(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		body        string
		wantMessage string
	}{
		{"fault message", http.StatusBadRequest, `{"fault_message": "The requested operation is not supported after starting the microVM."}`, "The requested operation is not supported after starting the microVM."},
		{"plain text body", http.StatusInternalServerError, "something broke\n", "something broke"},
		{"empty body", http.StatusBadRequest, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeFirecracker(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			})
			err := New(fake.socket).PutMachineConfig(context.Background(), MachineConfig{VCPUCount: 1, MemSizeMiB: 128})

			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error = %v, want an *APIError", err)
			}
			if apiErr.StatusCode != tt.status || apiErr.FaultMessage != tt.wantMessage || apiErr.Endpoint != "PUT /machine-config" {
				t.Errorf("APIError = %+v, want status %d, message %q, endpoint PUT /machine-config", apiErr, tt.status, tt.wantMessage)
			}
			if !strings.Contains(err.Error(), "PUT /machine-config") {
				t.Errorf("error text %q doesn't name the endpoint", err)
			}
		})
	}
}

func TestMissingIDsAreRejectedBeforeSending(t *testing.T) {
	fake := newFakeFirecracker(t, noContent)
	c := New(fake.socket)
	if err := c.PutDrive(context.Background(), Drive{PathOnHost: "/d.ext4"}); err == nil {
		t.Error("PutDrive without a DriveID: want an error")
	}
	if err := c.PutNetworkInterface(context.Background(), NetworkInterface{HostDevName: "tap0"}); err == nil {
		t.Error("PutNetworkInterface without an IfaceID: want an error")
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) != 0 {
		t.Errorf("sent %d requests, want none", len(fake.requests))
	}
}

func TestDefaultTimeoutAppliesWithoutADeadline(t *testing.T) {
	fake := newFakeFirecracker(t, func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // never answer
	})
	c := New(fake.socket, WithTimeout(50*time.Millisecond))

	start := time.Now()
	err := c.Pause(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("call took %v, want about 50ms", elapsed)
	}
}

func TestCallerDeadlineWins(t *testing.T) {
	fake := newFakeFirecracker(t, func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	c := New(fake.socket, WithTimeout(time.Hour))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := c.Resume(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
}

func TestObserverSeesEveryCall(t *testing.T) {
	fake := newFakeFirecracker(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/boot-source" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"fault_message": "no such kernel"}`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	var calls []CallInfo
	c := New(fake.socket, WithObserver(func(ci CallInfo) { calls = append(calls, ci) }))
	ctx := context.Background()

	_ = c.PutDrive(ctx, Drive{DriveID: "rootfs", PathOnHost: "/r.ext4"})
	_ = c.PutBootSource(ctx, BootSource{KernelImagePath: "/missing"})

	if len(calls) != 2 {
		t.Fatalf("observer saw %d calls, want 2", len(calls))
	}
	ok, failed := calls[0], calls[1]
	if ok.Endpoint != "PUT /drives/{drive_id}" || ok.Path != "/drives/rootfs" || ok.Status != http.StatusNoContent || ok.Err != nil || ok.Duration <= 0 {
		t.Errorf("successful call info = %+v", ok)
	}
	if failed.Endpoint != "PUT /boot-source" || failed.Status != http.StatusBadRequest || failed.Err == nil {
		t.Errorf("failed call info = %+v", failed)
	}
}

func TestConnectionErrorsAreReported(t *testing.T) {
	var info CallInfo
	c := New(filepath.Join(os.TempDir(), "tine-no-such-firecracker.sock"), WithObserver(func(ci CallInfo) { info = ci }))
	if _, err := c.DescribeInstance(context.Background()); err == nil {
		t.Fatal("DescribeInstance with nothing listening: want an error")
	}
	if info.Status != 0 || info.Err == nil {
		t.Errorf("call info = %+v, want status 0 and an error", info)
	}
}

func TestWaitReady(t *testing.T) {
	t.Run("returns once something listens", func(t *testing.T) {
		socket := shortSocketPath(t)
		go func() {
			time.Sleep(100 * time.Millisecond)
			ln, err := listenUnix(socket)
			if err != nil {
				t.Errorf("listen: %v", err)
				return
			}
			t.Cleanup(func() { _ = ln.Close() })
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := New(socket).WaitReady(ctx, 10*time.Millisecond); err != nil {
			t.Fatalf("WaitReady: %v", err)
		}
	})

	t.Run("gives up when the context ends", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		err := New(shortSocketPath(t)).WaitReady(ctx, 10*time.Millisecond)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want context.DeadlineExceeded", err)
		}
	})
}

func listenUnix(path string) (net.Listener, error) {
	var lc net.ListenConfig
	return lc.Listen(context.Background(), "unix", path)
}
