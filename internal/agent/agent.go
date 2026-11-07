// Package agent is tine-agent's server. It runs inside every VM and serves
// the agentproto protocol to tined: a health check, and running commands
// with their output streamed back while they run.
//
// The guest is the sandbox, so commands run as the agent's user (root) with
// no further restriction. What the agent does guard against is resource
// trouble on the host's side of the connection: output is capped, and a
// command is killed, with everything it started, when it times out or when
// tined disconnects.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mdlayher/vsock"
	"golang.org/x/sys/unix"

	"github.com/sanidhyasin/tine/internal/agentproto"
)

const (
	// DefaultMaxOutputBytes caps one command's stdout and stderr together.
	DefaultMaxOutputBytes = 10 << 20
	// DefaultOutputGrace is how long to wait for output after a command exits.
	// A background process it started can keep stdout open forever; after
	// this long, the agent stops waiting and reports the exit anyway.
	DefaultOutputGrace = time.Second
	// maxRequestBytes caps an /exec request, which can carry stdin.
	maxRequestBytes = 16 << 20
	// shutdownGrace is how long Serve waits for running commands on shutdown.
	shutdownGrace = 5 * time.Second
)

// baseEnv is every command's starting environment. ExecRequest.Env adds to it.
var baseEnv = []string{
	"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	"HOME=/root",
	"LANG=C.UTF-8",
}

// Config configures a Server. Zero values get sensible defaults.
type Config struct {
	// Version is reported by /health.
	Version string
	// MaxOutputBytes caps each command's output. Default DefaultMaxOutputBytes.
	MaxOutputBytes int64
	// OutputGrace: see DefaultOutputGrace.
	OutputGrace time.Duration
	// Logger receives one line per command (its name, never its arguments,
	// which can hold secrets) and any serving errors. Default: discard.
	Logger *slog.Logger
	// System changes the guest for /sync. Default: the real one (Linux only).
	System System
}

// Server serves the agent protocol.
type Server struct {
	cfg     Config
	log     *slog.Logger
	started time.Time
}

// New returns a server with cfg.
func New(cfg Config) *Server {
	if cfg.MaxOutputBytes <= 0 {
		cfg.MaxOutputBytes = DefaultMaxOutputBytes
	}
	if cfg.OutputGrace <= 0 {
		cfg.OutputGrace = DefaultOutputGrace
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.System == nil {
		cfg.System = OSSystem{Root: "/"}
	}
	return &Server{cfg: cfg, log: log, started: time.Now()}
}

// Handler is the agent's HTTP handler.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			if !allowMethod(w, r, http.MethodGet) {
				return
			}
			s.handleHealth(w)
		case "/exec":
			if !allowMethod(w, r, http.MethodPost) {
				return
			}
			s.handleExec(w, r)
		case "/sync":
			if !allowMethod(w, r, http.MethodPost) {
				return
			}
			s.handleSync(w, r)
		default:
			writeError(w, http.StatusNotFound, agentproto.CodeNotFound, "no such endpoint: "+r.URL.Path)
		}
	})
}

// Serve serves on ln until ctx is done, then stops accepting and gives
// running commands a few seconds to finish.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(&retryListener{Listener: ln, log: s.log}) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			s.log.Warn("shutdown didn't finish cleanly", "err", err)
		}
		return nil
	}
}

// Listen opens the listener tine-agent serves on, from a spec like
// "vsock:1024", "unix:/run/tine-agent.sock" or "tcp:127.0.0.1:8080".
func Listen(ctx context.Context, spec string) (net.Listener, error) {
	kind, addr, ok := strings.Cut(spec, ":")
	if !ok || addr == "" {
		return nil, fmt.Errorf("listen %q: want vsock:<port>, unix:<path> or tcp:<host:port>", spec)
	}
	var lc net.ListenConfig
	switch kind {
	case "vsock":
		port, err := strconv.ParseUint(addr, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("listen %q: bad vsock port: %w", spec, err)
		}
		return vsock.Listen(uint32(port), nil)
	case "unix":
		// A socket file left over from a previous run would block the listen.
		if info, err := os.Lstat(addr); err == nil && info.Mode()&os.ModeSocket != 0 {
			_ = os.Remove(addr)
		}
		return lc.Listen(ctx, "unix", addr)
	case "tcp":
		return lc.Listen(ctx, "tcp", addr)
	default:
		return nil, fmt.Errorf("listen %q: unknown kind %q", spec, kind)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, agentproto.Health{
		Status:        "ok",
		AgentVersion:  s.cfg.Version,
		Protocol:      agentproto.Version,
		UptimeSeconds: time.Since(s.started).Seconds(),
	})
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	var req agentproto.ExecRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, agentproto.CodeBadRequest, "bad exec request: "+err.Error())
		return
	}
	if len(req.Args) == 0 || req.Args[0] == "" {
		writeError(w, http.StatusBadRequest, agentproto.CodeBadRequest, "exec request has no command (args is empty)")
		return
	}
	if req.TimeoutMs < 0 || req.MaxOutputBytes < 0 {
		writeError(w, http.StatusBadRequest, agentproto.CodeBadRequest, "timeout_ms and max_output_bytes can't be negative")
		return
	}

	limit := s.cfg.MaxOutputBytes
	if req.MaxOutputBytes > 0 && req.MaxOutputBytes < limit {
		limit = req.MaxOutputBytes
	}
	// The command stops if tined disconnects (r.Context ends) or at its timeout.
	ctx := r.Context()
	cancel := func() {}
	if req.TimeoutMs > 0 {
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutMs)*time.Millisecond)
	}
	defer cancel()

	cmd := exec.CommandContext(ctx, req.Args[0], req.Args[1:]...) //nolint:gosec // running the caller's command is the agent's job; the guest is the sandbox
	cmd.Env = append(append([]string{}, baseEnv...), req.Env...)
	cmd.Dir = workDir(req.Dir)
	if len(req.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(req.Stdin)
	}
	// Its own process group, so stopping it also stops everything it started.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = s.cfg.OutputGrace

	out := newFrameStream(w, limit)
	cmd.Stdout = out.writer(agentproto.StreamStdout)
	cmd.Stderr = out.writer(agentproto.StreamStderr)

	start := time.Now()
	if err := cmd.Start(); err != nil {
		writeError(w, http.StatusUnprocessableEntity, agentproto.CodeStartFailed, err.Error())
		s.log.Info("exec couldn't start", "command", req.Args[0], "err", err)
		return
	}
	out.begin()

	waitErr := cmd.Wait()
	status := agentproto.ExitStatus{
		Code:            -1,
		DurationMs:      float64(time.Since(start).Microseconds()) / 1000,
		OutputTruncated: out.wasTruncated(),
	}
	if ps := cmd.ProcessState; ps != nil {
		status.Code = ps.ExitCode()
		if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			status.Signal = ws.Signal().String()
		}
	}
	status.TimedOut = req.TimeoutMs > 0 && errors.Is(ctx.Err(), context.DeadlineExceeded) && r.Context().Err() == nil
	if waitErr != nil && cmd.ProcessState == nil {
		s.log.Warn("exec wait failed", "command", req.Args[0], "err", waitErr)
	}
	out.write(agentproto.Frame{Exit: &status})

	s.log.Info("exec",
		"command", req.Args[0], "code", status.Code, "signal", status.Signal,
		"timed_out", status.TimedOut, "truncated", status.OutputTruncated,
		"ms", status.DurationMs, "client_gone", r.Context().Err() != nil)
}

// workDir picks the command's working directory: the requested one, else
// HOME (/root) if the agent may enter it, else /. In a guest the agent runs
// as root, so it's /root; elsewhere, as in tests, it may not be allowed in.
func workDir(requested string) string {
	if requested != "" {
		return requested
	}
	if unix.Access("/root", unix.X_OK) == nil {
		return "/root"
	}
	return "/"
}

// frameStream writes /exec's response: NDJSON frames, flushed one by one,
// with output capped at a byte limit.
type frameStream struct {
	mu        sync.Mutex
	w         http.ResponseWriter
	enc       *json.Encoder
	flusher   http.Flusher
	started   bool
	remaining int64
	truncated bool
	broken    bool // the client went away: stop writing
}

func newFrameStream(w http.ResponseWriter, limit int64) *frameStream {
	f, _ := w.(http.Flusher)
	return &frameStream{w: w, enc: json.NewEncoder(w), flusher: f, remaining: limit}
}

// begin sends the response headers, committing to a 200 stream.
func (s *frameStream) begin() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.beginLocked()
}

func (s *frameStream) beginLocked() {
	if s.started {
		return
	}
	s.started = true
	s.w.Header().Set("Content-Type", agentproto.ContentTypeNDJSON)
	s.w.WriteHeader(http.StatusOK)
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

func (s *frameStream) write(f agentproto.Frame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeLocked(f)
}

func (s *frameStream) writeLocked(f agentproto.Frame) {
	s.beginLocked()
	if s.broken {
		return
	}
	if err := s.enc.Encode(f); err != nil {
		s.broken = true
		return
	}
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

func (s *frameStream) wasTruncated() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.truncated
}

func (s *frameStream) writer(stream string) io.Writer { return &streamWriter{s: s, stream: stream} }

type streamWriter struct {
	s      *frameStream
	stream string
}

// Write sends p as a frame, up to the output limit. It always reports all
// of p written: an error would stop os/exec draining the command's pipe,
// and the command could block or die on a full pipe.
func (w *streamWriter) Write(p []byte) (int, error) {
	n := len(p)
	s := w.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.remaining <= 0 {
		if n > 0 {
			s.truncated = true
		}
		return n, nil
	}
	if int64(len(p)) > s.remaining {
		p = p[:s.remaining]
		s.truncated = true
	}
	s.remaining -= int64(len(p))
	s.writeLocked(agentproto.Frame{Stream: w.stream, Data: p})
	return n, nil
}

// retryListener keeps accepting after errors. Restoring a VM from a snapshot
// resets its vsock device; the listening socket survives that, and a single
// failed Accept must not stop the agent.
type retryListener struct {
	net.Listener
	log *slog.Logger
}

func (l *retryListener) Accept() (net.Conn, error) {
	backoff := 10 * time.Millisecond
	for {
		conn, err := l.Listener.Accept()
		if err == nil {
			return conn, nil
		}
		if errors.Is(err, net.ErrClosed) {
			return nil, err
		}
		l.log.Warn("accept failed; retrying", "err", err, "in", backoff)
		time.Sleep(backoff)
		backoff = min(backoff*2, time.Second)
	}
}

func allowMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	w.Header().Set("Allow", method)
	writeError(w, http.StatusMethodNotAllowed, agentproto.CodeMethodNotAllowed, r.Method+" isn't allowed here; use "+method)
	return false
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, agentproto.ErrorResponse{Error: agentproto.ErrorDetail{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
