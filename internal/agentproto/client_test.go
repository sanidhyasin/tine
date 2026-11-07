package agentproto

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ap") // short: socket paths are limited to about 100 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

func listen(t *testing.T, path string) net.Listener {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// fakeVsock acts like Firecracker's vsock socket: it reads the CONNECT line,
// then lets reply decide what happens on the connection.
func fakeVsock(t *testing.T, reply func(conn net.Conn, connectLine string)) string {
	t.Helper()
	path := socketPath(t)
	ln := listen(t, path)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				line, err := bufio.NewReader(conn).ReadString('\n')
				if err != nil {
					return
				}
				reply(conn, strings.TrimSuffix(line, "\n"))
			}()
		}
	}()
	return path
}

func TestDialVsockHandshake(t *testing.T) {
	var got atomic.Value
	path := fakeVsock(t, func(conn net.Conn, line string) {
		got.Store(line)
		// The reply and the first bytes after it arrive together; the dialer
		// must not swallow "hello".
		_, _ = io.WriteString(conn, "OK 1073741824\nhello")
	})
	conn, err := DialVsock(context.Background(), path, 1024)
	if err != nil {
		t.Fatalf("DialVsock: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if got.Load() != "CONNECT 1024" {
		t.Errorf("sent %q, want %q", got.Load(), "CONNECT 1024")
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "hello" {
		t.Errorf("read %q, %v after the handshake; want \"hello\"", buf, err)
	}
}

func TestDialVsockFailures(t *testing.T) {
	t.Run("nothing listening in the guest", func(t *testing.T) {
		path := fakeVsock(t, func(net.Conn, string) {}) // Firecracker just closes the connection
		_, err := DialVsock(context.Background(), path, 1024)
		if err == nil || !strings.Contains(err.Error(), "nothing is listening") {
			t.Errorf("error = %v, want one saying nothing is listening", err)
		}
	})
	t.Run("unexpected reply", func(t *testing.T) {
		path := fakeVsock(t, func(conn net.Conn, _ string) { _, _ = io.WriteString(conn, "NOPE\n") })
		if _, err := DialVsock(context.Background(), path, 1024); err == nil {
			t.Error("want an error")
		}
	})
	t.Run("no reply at all", func(t *testing.T) {
		path := fakeVsock(t, func(net.Conn, string) { time.Sleep(time.Second) })
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		start := time.Now()
		if _, err := DialVsock(ctx, path, 1024); err == nil {
			t.Error("want a timeout")
		}
		if time.Since(start) > 900*time.Millisecond {
			t.Error("the handshake didn't respect the context's deadline")
		}
	})
	t.Run("no socket", func(t *testing.T) {
		if _, err := DialVsock(context.Background(), filepath.Join(t.TempDir(), "none"), 1024); err == nil {
			t.Error("want an error")
		}
	})
}

// serve runs handler on a Unix socket and returns a Client for it.
func serve(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	path := socketPath(t)
	ln := listen(t, path)
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return NewClient(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	})
}

func TestVsockClientEndToEnd(t *testing.T) {
	// A fake Firecracker vsock socket in front of a fake agent.
	agentPath := socketPath(t)
	agentLn := listen(t, agentPath)
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(Health{Status: "ok", Protocol: Version})
	})}
	go func() { _ = srv.Serve(agentLn) }()
	t.Cleanup(func() { _ = srv.Close() })
	vsockPath := fakeVsock(t, func(conn net.Conn, line string) {
		if line != "CONNECT 1024" {
			return
		}
		var d net.Dialer
		up, err := d.DialContext(context.Background(), "unix", agentPath)
		if err != nil {
			return
		}
		defer func() { _ = up.Close() }()
		_, _ = io.WriteString(conn, "OK 1073741824\n")
		go func() { _, _ = io.Copy(up, conn) }()
		_, _ = io.Copy(conn, up)
	})

	h, err := NewVsockClient(vsockPath).Health(context.Background())
	if err != nil || h.Status != "ok" {
		t.Fatalf("Health through vsock = %+v, %v", h, err)
	}
}

func TestExecStreamCutShort(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", ContentTypeNDJSON)
		_ = json.NewEncoder(w).Encode(Frame{Stream: StreamStdout, Data: []byte("partial")})
		// ...and then the connection ends without an exit frame, as it would
		// if the VM were snapshotted mid-command.
	})
	var got string
	_, err := c.Exec(context.Background(), ExecRequest{Args: []string{"x"}}, func(_ string, d []byte) { got += string(d) })
	if !errors.Is(err, ErrIncomplete) {
		t.Errorf("error = %v, want ErrIncomplete", err)
	}
	if got != "partial" {
		t.Errorf("output before the cut = %q, want \"partial\"", got)
	}
}

func TestExecBadFrame(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "{not json\n") })
	if _, err := c.Exec(context.Background(), ExecRequest{Args: []string{"x"}}, nil); err == nil || errors.Is(err, ErrIncomplete) {
		t.Errorf("error = %v, want a bad-frame error", err)
	}
}

func TestErrorResponses(t *testing.T) {
	t.Run("agent error", func(t *testing.T) {
		c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(ErrorResponse{Error: ErrorDetail{Code: CodeStartFailed, Message: "no such file"}})
		})
		_, err := c.Exec(context.Background(), ExecRequest{Args: []string{"x"}}, nil)
		var e *Error
		if !errors.As(err, &e) || e.StatusCode != http.StatusUnprocessableEntity || e.Code != CodeStartFailed || e.Message != "no such file" {
			t.Errorf("error = %#v", err)
		}
	})
	t.Run("not the agent's format", func(t *testing.T) {
		c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "proxy said no", http.StatusBadGateway)
		})
		_, err := c.Health(context.Background())
		var e *Error
		if !errors.As(err, &e) || e.StatusCode != http.StatusBadGateway || e.Code != "unknown" || e.Message != "proxy said no" {
			t.Errorf("error = %#v", err)
		}
	})
}

func TestWaitHealthy(t *testing.T) {
	var calls atomic.Int32
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable) // still booting
			return
		}
		_ = json.NewEncoder(w).Encode(Health{Status: "ok", Protocol: Version})
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h, err := c.WaitHealthy(ctx, 10*time.Millisecond)
	if err != nil || h.Status != "ok" || calls.Load() != 3 {
		t.Errorf("WaitHealthy = %+v, %v after %d calls; want ok on the 3rd", h, err, calls.Load())
	}

	never := serve(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	if _, err := never.WaitHealthy(ctx2, 10*time.Millisecond); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("WaitHealthy on a never-ready agent = %v, want DeadlineExceeded", err)
	}
}
