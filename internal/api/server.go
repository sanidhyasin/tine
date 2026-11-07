package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/sanidhyasin/tine/internal/agentproto"
	"github.com/sanidhyasin/tine/internal/image"
	"github.com/sanidhyasin/tine/internal/state"
	"github.com/sanidhyasin/tine/internal/vm"
)

const (
	// maxJSONBytes caps request bodies; exec's is larger, since it can carry stdin.
	maxJSONBytes = 1 << 20
	maxExecBytes = 16 << 20
)

// Backend is what the API serves. *vm.Manager is the real one.
type Backend interface {
	Create(ctx context.Context, req vm.CreateRequest) (state.VM, error)
	Get(ctx context.Context, id string) (state.VM, error)
	List(ctx context.Context, includeDestroyed bool) ([]state.VM, error)
	Destroy(ctx context.Context, id string) (state.VM, error)
	Exec(ctx context.Context, id string, req agentproto.ExecRequest, onOutput func(stream string, data []byte)) (agentproto.ExitStatus, error)
	Images(ctx context.Context) ([]*image.Image, error)
	Ping(ctx context.Context) error
}

// Server is the API's HTTP handler.
type Server struct {
	backend Backend
	version string
	log     *slog.Logger
	started time.Time
	mux     *http.ServeMux
}

// NewServer returns the API handler for backend.
func NewServer(backend Backend, version string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &Server{backend: backend, version: version, log: log, started: time.Now(), mux: http.NewServeMux()}
	// Methods are checked in the handlers, so wrong ones get a JSON error too.
	s.mux.HandleFunc("/healthz", s.health)
	s.mux.HandleFunc("/v1/images", s.images)
	s.mux.HandleFunc("/v1/vms", s.vms)
	s.mux.HandleFunc("/v1/vms/{id}", s.oneVM)
	s.mux.HandleFunc("/v1/vms/{id}/exec", s.exec)
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, CodeNotFound, "no such endpoint: "+r.Method+" "+r.URL.Path)
	})
	return s
}

// ServeHTTP logs every request and turns panics into 500s.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	defer func() {
		if p := recover(); p != nil {
			s.log.Error("handler panicked", "method", r.Method, "path", r.URL.Path, "panic", p, "stack", string(debug.Stack()))
			if !rec.wrote {
				writeError(rec, http.StatusInternalServerError, CodeInternal, "internal error")
			}
		}
		s.log.Info("request", "method", r.Method, "path", r.URL.Path, "status", rec.status,
			"ms", float64(time.Since(start).Microseconds())/1000)
	}()
	s.mux.ServeHTTP(rec, r)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if !allow(w, r, http.MethodGet) {
		return
	}
	if err := s.backend.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, CodeInternal, "database unavailable: "+err.Error())
		return
	}
	vms, err := s.backend.List(r.Context(), false)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, Health{Status: "ok", Version: s.version, VMs: len(vms), UptimeSeconds: time.Since(s.started).Seconds()})
}

func (s *Server) images(w http.ResponseWriter, r *http.Request) {
	if !allow(w, r, http.MethodGet) {
		return
	}
	imgs, err := s.backend.Images(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	out := ListImagesResponse{Images: []Image{}}
	for _, img := range imgs {
		out.Images = append(out.Images, Image{
			Name: img.Name, CreatedAt: img.Meta.CreatedAt, Kernel: img.Meta.Kernel.Source,
			DiskBytes: img.Meta.Rootfs.SizeBytes, AgentVersion: img.Meta.Agent.Version,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) vms(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := s.backend.List(r.Context(), r.URL.Query().Get("all") == "true")
		if err != nil {
			s.fail(w, err)
			return
		}
		out := ListVMsResponse{VMs: []VM{}}
		for _, v := range list {
			out.VMs = append(out.VMs, toAPI(v))
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var req CreateVMRequest
		if !decode(w, r, maxJSONBytes, &req) {
			return
		}
		if req.TTLSeconds < 0 {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "ttl_seconds can't be negative")
			return
		}
		created, err := s.backend.Create(r.Context(), vm.CreateRequest{
			Image: req.Image, VCPUs: req.VCPUs, MemoryMiB: req.MemoryMiB, TTL: time.Duration(req.TTLSeconds) * time.Second,
		})
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, toAPI(created))
	default:
		methodNotAllowed(w, r, http.MethodGet, http.MethodPost)
	}
}

func (s *Server) oneVM(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var (
		v   state.VM
		err error
	)
	switch r.Method {
	case http.MethodGet:
		v, err = s.backend.Get(r.Context(), id)
	case http.MethodDelete:
		v, err = s.backend.Destroy(r.Context(), id)
	default:
		methodNotAllowed(w, r, http.MethodGet, http.MethodDelete)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPI(v))
}

func (s *Server) exec(w http.ResponseWriter, r *http.Request) {
	if !allow(w, r, http.MethodPost) {
		return
	}
	var req ExecRequest
	if !decode(w, r, maxExecBytes, &req) {
		return
	}
	out := &frameWriter{w: w}
	st, err := s.backend.Exec(r.Context(), r.PathValue("id"), req, func(stream string, data []byte) {
		out.write(Frame{Stream: stream, Data: data})
	})
	if err != nil {
		if !out.started() {
			s.fail(w, err)
			return
		}
		// The stream has begun, so an HTTP error is no longer possible. Ending
		// it without an exit frame tells the client it was cut short.
		s.log.Warn("exec stream broke", "vm", r.PathValue("id"), "err", err)
		return
	}
	out.write(Frame{Exit: &st})
}

// fail answers with the error's status and code.
func (s *Server) fail(w http.ResponseWriter, err error) {
	status, code := http.StatusInternalServerError, CodeInternal
	var agentErr *agentproto.Error
	switch {
	case errors.As(err, &agentErr):
		status, code = agentErr.StatusCode, agentErr.Code
	case errors.Is(err, vm.ErrImageNotFound):
		status, code = http.StatusNotFound, CodeImageNotFound
	case errors.Is(err, vm.ErrNotFound):
		status, code = http.StatusNotFound, CodeNotFound
	case errors.Is(err, vm.ErrInvalidRequest):
		status, code = http.StatusBadRequest, CodeInvalidRequest
	case errors.Is(err, vm.ErrCapacity):
		status, code = http.StatusServiceUnavailable, CodeCapacity
	case errors.Is(err, vm.ErrNotRunning):
		status, code = http.StatusConflict, CodeNotRunning
	case errors.Is(err, state.ErrInvalidTransition), errors.Is(err, state.ErrConflict):
		status, code = http.StatusConflict, CodeConflict
	}
	if status >= 500 {
		s.log.Error("request failed", "err", err)
	}
	writeError(w, status, code, err.Error())
}

func toAPI(v state.VM) VM {
	out := VM{
		ID: v.ID, State: string(v.State), Image: v.Image, VCPUs: v.VCPUs, MemoryMiB: v.MemoryMiB,
		CreatedAt: v.CreatedAt, Error: v.Error, TimingsMs: v.Timings,
	}
	if !v.ExpiresAt.IsZero() {
		t := v.ExpiresAt
		out.ExpiresAt = &t
	}
	if !v.DestroyedAt.IsZero() {
		t := v.DestroyedAt
		out.DestroyedAt = &t
	}
	return out
}

// --- HTTP helpers ---

func allow(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method == method {
		return true
	}
	methodNotAllowed(w, r, method)
	return false
}

func methodNotAllowed(w http.ResponseWriter, r *http.Request, allowed ...string) {
	w.Header().Set("Allow", strings.Join(allowed, ", "))
	writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed,
		fmt.Sprintf("%s isn't allowed on %s; use %s", r.Method, r.URL.Path, strings.Join(allowed, " or ")))
}

func decode(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "bad request body: "+err.Error())
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, ErrorResponse{Error: ErrorDetail{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// frameWriter streams exec frames, sending the 200 header with the first one.
type frameWriter struct {
	mu     sync.Mutex
	w      http.ResponseWriter
	begun  bool
	broken bool
}

func (f *frameWriter) started() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.begun
}

func (f *frameWriter) write(fr Frame) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.begun {
		f.begun = true
		f.w.Header().Set("Content-Type", agentproto.ContentTypeNDJSON)
		f.w.WriteHeader(http.StatusOK)
	}
	if f.broken {
		return
	}
	if err := json.NewEncoder(f.w).Encode(fr); err != nil {
		f.broken = true
		return
	}
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
}

// statusRecorder notes the status a handler sent, for the request log.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if !r.wrote {
		r.status, r.wrote = status, true
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.ResponseWriter.Write(b)
}

// Flush passes flushes through, so exec output still streams.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
