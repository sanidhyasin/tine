package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sanidhyasin/tine/internal/agentproto"
)

// startAgent serves an agent on a Unix socket for the length of the test and
// returns a client for it, plus the socket's path.
func startAgent(t *testing.T, cfg Config) (*agentproto.Client, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "ag") // short: socket paths are limited to about 100 bytes
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "agent.sock")

	ctx, cancel := context.WithCancel(context.Background())
	ln, err := Listen(ctx, "unix:"+sock)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- New(cfg).Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	client := agentproto.NewClient(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	})
	return client, sock
}

// output collects what Exec streams back.
type output struct {
	mu             sync.Mutex
	stdout, stderr bytes.Buffer
	first          time.Time
}

func (o *output) collect(stream string, data []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.first.IsZero() {
		o.first = time.Now()
	}
	switch stream {
	case agentproto.StreamStdout:
		o.stdout.Write(data)
	case agentproto.StreamStderr:
		o.stderr.Write(data)
	}
}

func (o *output) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.stdout.String()
}

func sh(script string) []string { return []string{"sh", "-c", script} }

func TestHealth(t *testing.T) {
	c, _ := startAgent(t, Config{Version: "v-test"})
	h, err := c.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if h.Status != "ok" || h.AgentVersion != "v-test" || h.Protocol != agentproto.Version || h.UptimeSeconds < 0 {
		t.Errorf("Health = %+v", h)
	}
}

func TestExecOutputAndExitCode(t *testing.T) {
	c, _ := startAgent(t, Config{})
	var out output
	st, err := c.Exec(context.Background(), agentproto.ExecRequest{Args: sh("echo out; echo err >&2; exit 3")}, out.collect)
	if err != nil {
		t.Fatal(err)
	}
	if out.stdout.String() != "out\n" || out.stderr.String() != "err\n" {
		t.Errorf("stdout %q, stderr %q; want \"out\\n\" and \"err\\n\"", out.stdout.String(), out.stderr.String())
	}
	if st.Code != 3 || st.Signal != "" || st.TimedOut || st.OutputTruncated || st.DurationMs <= 0 {
		t.Errorf("exit status = %+v, want code 3", st)
	}
}

func TestExecStdin(t *testing.T) {
	c, _ := startAgent(t, Config{})
	var out output
	st, err := c.Exec(context.Background(), agentproto.ExecRequest{Args: []string{"cat"}, Stdin: []byte("hello\x00world\n")}, out.collect)
	if err != nil || st.Code != 0 {
		t.Fatalf("Exec: %+v, %v", st, err)
	}
	if out.String() != "hello\x00world\n" {
		t.Errorf("cat echoed %q", out.String())
	}
}

func TestExecEnvironmentAndDirectory(t *testing.T) {
	c, _ := startAgent(t, Config{})
	dir := t.TempDir()
	realDir, _ := filepath.EvalSymlinks(dir)
	var out output
	st, err := c.Exec(context.Background(), agentproto.ExecRequest{
		Args: sh(`printf '%s\n' "$FOO" "$HOME"; pwd -P`),
		Env:  []string{"FOO=bar baz"},
		Dir:  dir,
	}, out.collect)
	if err != nil || st.Code != 0 {
		t.Fatalf("Exec: %+v, %v", st, err)
	}
	if want := "bar baz\n/root\n" + realDir + "\n"; out.String() != want {
		t.Errorf("output %q, want %q", out.String(), want)
	}
}

func TestExecStartFailures(t *testing.T) {
	c, _ := startAgent(t, Config{})
	for name, req := range map[string]agentproto.ExecRequest{
		"missing command":   {Args: []string{"tine-no-such-command"}},
		"missing directory": {Args: []string{"true"}, Dir: "/tine/no/such/dir"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := c.Exec(context.Background(), req, nil)
			var agentErr *agentproto.Error
			if !errors.As(err, &agentErr) || agentErr.StatusCode != http.StatusUnprocessableEntity || agentErr.Code != agentproto.CodeStartFailed {
				t.Errorf("error = %v, want a 422 %s", err, agentproto.CodeStartFailed)
			}
		})
	}
}

func TestExecBadRequests(t *testing.T) {
	c, sock := startAgent(t, Config{})
	for name, req := range map[string]agentproto.ExecRequest{
		"no args":          {},
		"empty command":    {Args: []string{""}},
		"negative timeout": {Args: []string{"true"}, TimeoutMs: -1},
		"negative cap":     {Args: []string{"true"}, MaxOutputBytes: -5},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := c.Exec(context.Background(), req, nil)
			var agentErr *agentproto.Error
			if !errors.As(err, &agentErr) || agentErr.StatusCode != http.StatusBadRequest || agentErr.Code != agentproto.CodeBadRequest {
				t.Errorf("error = %v, want a 400 %s", err, agentproto.CodeBadRequest)
			}
		})
	}
	t.Run("not JSON", func(t *testing.T) {
		resp := rawRequest(t, sock, http.MethodPost, "/exec", "{nope")
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status %d, want 400", resp.StatusCode)
		}
	})
}

func TestUnknownPathsAndMethods(t *testing.T) {
	_, sock := startAgent(t, Config{})
	if resp := rawRequest(t, sock, http.MethodGet, "/exec", ""); resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != http.MethodPost {
		t.Errorf("GET /exec: %d (Allow %q), want 405 with Allow: POST", resp.StatusCode, resp.Header.Get("Allow"))
	}
	if resp := rawRequest(t, sock, http.MethodPost, "/health", ""); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /health: %d, want 405", resp.StatusCode)
	}
	if resp := rawRequest(t, sock, http.MethodGet, "/nope", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET /nope: %d, want 404", resp.StatusCode)
	}
}

func TestTimeoutKillsEverythingTheCommandStarted(t *testing.T) {
	c, _ := startAgent(t, Config{})
	var out output
	start := time.Now()
	// The shell starts a background sleep, prints its pid, and waits.
	st, err := c.Exec(context.Background(), agentproto.ExecRequest{Args: sh("sleep 30 & echo $!; wait"), TimeoutMs: 300}, out.collect)
	if err != nil {
		t.Fatal(err)
	}
	if !st.TimedOut || st.Code != -1 || st.Signal == "" {
		t.Errorf("exit status = %+v, want timed out and killed by a signal", st)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("Exec took %v, want about 300ms", took)
	}
	requireGone(t, pidFrom(t, out.String()))
}

func TestDisconnectKillsTheCommand(t *testing.T) {
	c, _ := startAgent(t, Config{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pids := make(chan int, 1)
	_, err := c.Exec(ctx, agentproto.ExecRequest{Args: sh("sleep 30 & echo $!; wait")}, func(_ string, data []byte) {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
			pids <- pid
			cancel() // hang up while the command runs
		}
	})
	if err == nil {
		t.Fatal("Exec after hanging up: want an error")
	}
	select {
	case pid := <-pids:
		requireGone(t, pid)
	case <-time.After(time.Second):
		t.Fatalf("the command never printed its pid; Exec said: %v", err)
	}
}

func TestDefaultWorkingDirectory(t *testing.T) {
	c, _ := startAgent(t, Config{})
	var out output
	st, err := c.Exec(context.Background(), agentproto.ExecRequest{Args: []string{"pwd", "-P"}}, out.collect)
	if err != nil || st.Code != 0 {
		t.Fatalf("Exec: %+v, %v", st, err)
	}
	want := "/\n"
	if workDir("") == "/root" {
		want = "/root\n"
	}
	if out.String() != want {
		t.Errorf("default directory %q, want %q", out.String(), want)
	}
}

func TestOutputIsCapped(t *testing.T) {
	t.Run("request's own cap", func(t *testing.T) {
		c, _ := startAgent(t, Config{})
		var out output
		st, err := c.Exec(context.Background(), agentproto.ExecRequest{Args: sh("head -c 3000000 /dev/zero"), MaxOutputBytes: 100_000}, out.collect)
		if err != nil {
			t.Fatal(err)
		}
		if out.stdout.Len() != 100_000 || !st.OutputTruncated || st.Code != 0 {
			t.Errorf("got %d bytes, status %+v; want exactly 100000 bytes, truncated, code 0", out.stdout.Len(), st)
		}
	})
	t.Run("requests can't raise the agent's cap", func(t *testing.T) {
		c, _ := startAgent(t, Config{MaxOutputBytes: 50_000})
		var out output
		st, err := c.Exec(context.Background(), agentproto.ExecRequest{Args: sh("head -c 3000000 /dev/zero"), MaxOutputBytes: 1 << 30}, out.collect)
		if err != nil {
			t.Fatal(err)
		}
		if out.stdout.Len() != 50_000 || !st.OutputTruncated {
			t.Errorf("got %d bytes, truncated %v; want 50000, truncated", out.stdout.Len(), st.OutputTruncated)
		}
	})
}

func TestOutputStreamsWhileTheCommandRuns(t *testing.T) {
	c, _ := startAgent(t, Config{})
	var out output
	_, err := c.Exec(context.Background(), agentproto.ExecRequest{Args: sh("echo first; sleep 1; echo second")}, out.collect)
	if err != nil {
		t.Fatal(err)
	}
	done := time.Now()
	if gap := done.Sub(out.first); gap < 700*time.Millisecond {
		t.Errorf("first output arrived only %v before the end; it should arrive while the command runs", gap)
	}
	if out.String() != "first\nsecond\n" {
		t.Errorf("output %q", out.String())
	}
}

func TestBackgroundProcessDoesNotHangExec(t *testing.T) {
	c, _ := startAgent(t, Config{OutputGrace: 200 * time.Millisecond})
	var out output
	start := time.Now()
	// The background sleep inherits stdout and keeps it open after the shell exits.
	st, err := c.Exec(context.Background(), agentproto.ExecRequest{Args: sh("sleep 30 & echo $!")}, out.collect)
	if err != nil {
		t.Fatal(err)
	}
	pid := pidFrom(t, out.String())
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if st.Code != 0 {
		t.Errorf("exit status = %+v, want code 0", st)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("Exec took %v; the agent should stop waiting for output shortly after the shell exits", took)
	}
	if !alive(pid) {
		t.Error("the background process should keep running after a normal exit")
	}
}

func TestConcurrentExecs(t *testing.T) {
	c, _ := startAgent(t, Config{})
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var out output
			st, err := c.Exec(context.Background(), agentproto.ExecRequest{Args: []string{"echo", strconv.Itoa(i)}}, out.collect)
			if err != nil || st.Code != 0 || out.String() != fmt.Sprintf("%d\n", i) {
				t.Errorf("exec %d: %q, %+v, %v", i, out.String(), st, err)
			}
		}()
	}
	wg.Wait()
}

func TestListenSpecs(t *testing.T) {
	ctx := context.Background()
	for _, bad := range []string{"", "vsock", "vsock:", "vsock:abc", "vsock:99999999999", "pipe:/x", "unix:"} {
		if ln, err := Listen(ctx, bad); err == nil {
			_ = ln.Close()
			t.Errorf("Listen(%q): want an error", bad)
		}
	}
	ln, err := Listen(ctx, "tcp:127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen tcp: %v", err)
	}
	_ = ln.Close()
}

func TestRetryListenerSurvivesAcceptErrors(t *testing.T) {
	want, other := net.Pipe()
	defer func() { _ = want.Close(); _ = other.Close() }()
	fl := &flakyListener{fails: 2, conn: want}
	got, err := (&retryListener{Listener: fl, log: New(Config{}).log}).Accept()
	if err != nil || got != want {
		t.Fatalf("Accept = %v, %v; want the connection after two failures", got, err)
	}
	fl.closed = true
	if _, err := (&retryListener{Listener: fl, log: New(Config{}).log}).Accept(); !errors.Is(err, net.ErrClosed) {
		t.Errorf("Accept on a closed listener = %v, want net.ErrClosed", err)
	}
}

type flakyListener struct {
	net.Listener
	fails  int
	closed bool
	conn   net.Conn
}

func (f *flakyListener) Accept() (net.Conn, error) {
	if f.closed {
		return nil, net.ErrClosed
	}
	if f.fails > 0 {
		f.fails--
		return nil, errors.New("transport reset")
	}
	return f.conn, nil
}

// --- helpers ---

// rawRequest sends one request to the agent and returns its status and headers.
type rawResponse struct {
	StatusCode int
	Header     http.Header
}

func rawRequest(t *testing.T, sock, method, path, body string) rawResponse {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}}
	req, err := http.NewRequestWithContext(context.Background(), method, "http://agent"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("%s %s: Content-Type %q, want application/json", method, path, ct)
	}
	return rawResponse{StatusCode: resp.StatusCode, Header: resp.Header}
}

func pidFrom(t *testing.T, s string) int {
	t.Helper()
	pid, err := strconv.Atoi(strings.TrimSpace(strings.Split(s, "\n")[0]))
	if err != nil {
		t.Fatalf("no pid in output %q", s)
	}
	return pid
}

// alive reports whether pid is a running (not zombie) process.
func alive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	if stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		// The state is the field after the parenthesized command name.
		if i := bytes.LastIndexByte(stat, ')'); i >= 0 && i+2 < len(stat) && stat[i+2] == 'Z' {
			return false
		}
	}
	return true
}

func requireGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for alive(pid) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("process %d, started by the command, is still running", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
