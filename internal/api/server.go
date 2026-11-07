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
	"github.com/sanidhyasin/tine/internal/logging"
	"github.com/sanidhyasin/tine/internal/metrics"
	"github.com/sanidhyasin/tine/internal/snapshot"
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
	Snapshot(ctx context.Context, id string) (state.Snapshot, error)
	Snapshots(ctx context.Context, f state.SnapshotFilter) ([]vm.SnapshotInfo, error)
	GetSnapshot(ctx context.Context, id string) (vm.SnapshotInfo, error)
	DeleteSnapshot(ctx context.Context, id string) (state.Snapshot, error)
	Rollback(ctx context.Context, id, snapshotID string) (state.VM, error)
	Images(ctx context.Context) ([]*image.Image, error)
	Templates(ctx context.Context) ([]vm.Template, error)
	WarmTemplate(ctx context.Context, image string, vcpus, memoryMiB int) (state.Snapshot, error)
	Ping(ctx context.Context) error
}

// Server is the API's HTTP handler.
type Server struct {
	backend Backend
	version string
	log     *slog.Logger
	metrics *metrics.Metrics
	started time.Time
	mux     *http.ServeMux
}

// Option configures a Server.
type Option func(*Server)

// WithMetrics serves GET /metrics and records every request in m.
func WithMetrics(m *metrics.Metrics) Option { return func(s *Server) { s.metrics = m } }

// RequestIDHeader carries a request's ID: tined answers with it, and uses
// the caller's if one is sent.
const RequestIDHeader = "X-Request-Id"

// NewServer returns the API handler for backend.
func NewServer(backend Backend, version string, log *slog.Logger, opts ...Option) *Server {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &Server{backend: backend, version: version, log: log, started: time.Now(), mux: http.NewServeMux()}
	for _, opt := range opts {
		opt(s)
	}
	if s.metrics != nil {
		s.mux.Handle("GET /metrics", s.metrics.Handler())
	}
	// Methods are checked in the handlers, so wrong ones get a JSON error too.
	s.mux.HandleFunc("/healthz", s.health)
	s.mux.HandleFunc("/v1/images", s.images)
	s.mux.HandleFunc("/v1/images/{name}/warm", s.warm)
	s.mux.HandleFunc("/v1/vms", s.vms)
	s.mux.HandleFunc("/v1/vms/{id}", s.oneVM)
	s.mux.HandleFunc("/v1/vms/{id}/exec", s.exec)
	s.mux.HandleFunc("/v1/vms/{id}/snapshots", s.vmSnapshots)
	s.mux.HandleFunc("/v1/vms/{id}/rollback", s.rollback)
	s.mux.HandleFunc("/v1/snapshots", s.snapshots)
	s.mux.HandleFunc("/v1/snapshots/{id}", s.oneSnapshot)
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, CodeNotFound, "no such endpoint: "+r.Method+" "+r.URL.Path)
	})
	return s
}

// ServeHTTP gives every request an ID, logs it, records it in the metrics,
// and turns panics into 500s.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	id := r.Header.Get(RequestIDHeader)
	if !validRequestID(id) {
		id = logging.NewRequestID()
	}
	w.Header().Set(RequestIDHeader, id)
	r = r.WithContext(logging.With(r.Context(), slog.String("request_id", id)))

	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	defer func() {
		if p := recover(); p != nil {
			s.log.ErrorContext(r.Context(), "handler panicked", "method", r.Method, "path", r.URL.Path, "panic", p, "stack", string(debug.Stack()))
			if !rec.wrote {
				writeError(rec, http.StatusInternalServerError, CodeInternal, "internal error")
			}
		}
		// The mux records the matched pattern on r, e.g. "/v1/vms/{id}": a
		// metric label that doesn't grow with every VM ID.
		route := r.Pattern
		if route == "" || route == "/" {
			route = "other"
		}
		took := time.Since(start)
		s.metrics.APIRequest(r.Method, route, rec.status, took)
		attrs := []any{"method", r.Method, "path", r.URL.Path, "route", route, "status", rec.status,
			"ms", float64(took.Microseconds()) / 1000}
		if vm := r.PathValue("id"); vm != "" {
			attrs = append(attrs, "vm", vm)
		}
		s.log.InfoContext(r.Context(), "request", attrs...)
	}()
	s.mux.ServeHTTP(rec, r)
}

// validRequestID accepts short, plain IDs from callers, so they can't
// inject anything into the logs.
func validRequestID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
		if !ok {
			return false
		}
	}
	return true
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
		s.fail(w, r, err)
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
		s.fail(w, r, err)
		return
	}
	templates, err := s.backend.Templates(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	byImage := map[string][]Template{}
	for _, t := range templates {
		byImage[t.Snapshot.Image] = append(byImage[t.Snapshot.Image], Template{
			SnapshotID: t.Snapshot.ID, VCPUs: t.Snapshot.VCPUs, MemoryMiB: t.Snapshot.MemoryMiB,
			CreatedAt: t.Snapshot.CreatedAt, Problem: t.Problem,
		})
	}
	out := ListImagesResponse{Images: []Image{}}
	for _, img := range imgs {
		out.Images = append(out.Images, Image{
			Name: img.Name, CreatedAt: img.Meta.CreatedAt, Kernel: img.Meta.Kernel.Source,
			DiskBytes: img.Meta.Rootfs.SizeBytes, AgentVersion: img.Meta.Agent.Version, Templates: byImage[img.Name],
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) warm(w http.ResponseWriter, r *http.Request) {
	if !allow(w, r, http.MethodPost) {
		return
	}
	var req WarmRequest
	if !decode(w, r, maxJSONBytes, &req) {
		return
	}
	snap, err := s.backend.WarmTemplate(r.Context(), r.PathValue("name"), req.VCPUs, req.MemoryMiB)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toAPISnapshot(snap, nil))
}

func (s *Server) vms(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		list, err := s.backend.List(r.Context(), r.URL.Query().Get("all") == "true")
		if err != nil {
			s.fail(w, r, err)
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
			Image: req.Image, VCPUs: req.VCPUs, MemoryMiB: req.MemoryMiB, TTL: time.Duration(req.TTLSeconds) * time.Second, Cold: req.Cold,
		})
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, toAPI(created))
	default:
		methodNotAllowed(w, r, http.MethodGet, http.MethodPost)
	}
}

func (s *Server) oneVM(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	r = r.WithContext(logging.WithVM(r.Context(), id))
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
		s.fail(w, r, err)
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
	ctx := logging.WithVM(r.Context(), r.PathValue("id"))
	st, err := s.backend.Exec(ctx, r.PathValue("id"), req, func(stream string, data []byte) {
		out.write(Frame{Stream: stream, Data: data})
	})
	if err != nil {
		if !out.started() {
			s.fail(w, r, err)
			return
		}
		// The stream has begun, so an HTTP error is no longer possible. Ending
		// it without an exit frame tells the client it was cut short.
		s.log.WarnContext(ctx, "exec stream broke", "err", err)
		return
	}
	out.write(Frame{Exit: &st})
}

func (s *Server) vmSnapshots(w http.ResponseWriter, r *http.Request) {
	ctx := logging.WithVM(r.Context(), r.PathValue("id"))
	switch r.Method {
	case http.MethodPost:
		snap, err := s.backend.Snapshot(ctx, r.PathValue("id"))
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, toAPISnapshot(snap, nil))
	case http.MethodGet:
		s.listSnapshots(w, r.WithContext(ctx), state.SnapshotFilter{VMID: r.PathValue("id")})
	default:
		methodNotAllowed(w, r, http.MethodGet, http.MethodPost)
	}
}

func (s *Server) snapshots(w http.ResponseWriter, r *http.Request) {
	if !allow(w, r, http.MethodGet) {
		return
	}
	q := r.URL.Query()
	s.listSnapshots(w, r, state.SnapshotFilter{VMID: q.Get("vm"), Kind: state.SnapshotKind(q.Get("kind")), Image: q.Get("image")})
}

func (s *Server) listSnapshots(w http.ResponseWriter, r *http.Request, f state.SnapshotFilter) {
	list, err := s.backend.Snapshots(r.Context(), f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := ListSnapshotsResponse{Snapshots: []Snapshot{}}
	for _, info := range list {
		out.Snapshots = append(out.Snapshots, toAPISnapshot(info.Snapshot, info.UsedBy))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) oneSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	switch r.Method {
	case http.MethodGet:
		info, err := s.backend.GetSnapshot(r.Context(), id)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, toAPISnapshot(info.Snapshot, info.UsedBy))
	case http.MethodDelete:
		snap, err := s.backend.DeleteSnapshot(r.Context(), id)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, toAPISnapshot(snap, nil))
	default:
		methodNotAllowed(w, r, http.MethodGet, http.MethodDelete)
	}
}

func (s *Server) rollback(w http.ResponseWriter, r *http.Request) {
	if !allow(w, r, http.MethodPost) {
		return
	}
	var req RollbackRequest
	if !decode(w, r, maxJSONBytes, &req) {
		return
	}
	if req.SnapshotID == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "snapshot_id is required")
		return
	}
	ctx := logging.WithVM(r.Context(), r.PathValue("id"))
	v, err := s.backend.Rollback(ctx, r.PathValue("id"), req.SnapshotID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPI(v))
}

// fail answers with the error's status and code.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
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
	case errors.Is(err, vm.ErrBusy):
		status, code = http.StatusConflict, CodeBusy
	case errors.Is(err, state.ErrInUse):
		status, code = http.StatusConflict, CodeInUse
	case errors.Is(err, snapshot.ErrIncompatible):
		status, code = http.StatusConflict, CodeIncompatible
	case errors.Is(err, vm.ErrNoSpace):
		status, code = http.StatusInsufficientStorage, CodeNoSpace
	case errors.Is(err, state.ErrInvalidTransition), errors.Is(err, state.ErrConflict):
		status, code = http.StatusConflict, CodeConflict
	}
	if status >= 500 {
		s.log.ErrorContext(r.Context(), "request failed", "err", err)
	}
	writeError(w, status, code, err.Error())
}

func toAPI(v state.VM) VM {
	out := VM{
		ID: v.ID, State: string(v.State), Image: v.Image, VCPUs: v.VCPUs, MemoryMiB: v.MemoryMiB,
		CreatedAt: v.CreatedAt, Error: v.Error, Source: string(v.Source), SnapshotID: v.SnapshotID, TimingsMs: v.Timings,
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

// toAPISnapshot shows a snapshot, with the VMs that run from it, if known.
func toAPISnapshot(s state.Snapshot, usedBy []string) Snapshot {
	out := Snapshot{
		ID: s.ID, Kind: string(s.Kind), State: string(s.State), VMID: s.VMID, Image: s.Image,
		VCPUs: s.VCPUs, MemoryMiB: s.MemoryMiB, SizeBytes: s.SizeBytes, CreatedAt: s.CreatedAt, UsedBy: usedBy,
	}
	var meta snapshot.Metadata
	if len(s.Metadata) > 0 && json.Unmarshal(s.Metadata, &meta) == nil {
		out.FirecrackerVersion, out.TimingsMs = meta.Host.Firecracker, meta.TimingsMs
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
