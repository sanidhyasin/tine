package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
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
	ReadFile(ctx context.Context, id, path string) (io.ReadCloser, agentproto.FileInfo, error)
	WriteFile(ctx context.Context, id, path string, mode os.FileMode, r io.Reader) (agentproto.FileInfo, error)
	PutArchive(ctx context.Context, id, dir string, r io.Reader) (agentproto.ArchiveResult, error)
	Snapshot(ctx context.Context, id string, req vm.SnapshotRequest) (state.Snapshot, error)
	Snapshots(ctx context.Context, f state.SnapshotFilter) ([]vm.SnapshotInfo, error)
	GetSnapshot(ctx context.Context, id string) (vm.SnapshotInfo, error)
	DeleteSnapshot(ctx context.Context, id string) (state.Snapshot, error)
	Rollback(ctx context.Context, id, snapshotID string) (state.VM, error)
	Fork(ctx context.Context, id string, req vm.ForkRequest) (vm.ForkResult, error)
	ForkSnapshot(ctx context.Context, snapID string, req vm.ForkRequest) (vm.ForkResult, error)
	Memory(ctx context.Context, id string) (vm.Memory, error)
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
	// features are reported by GET /healthz.
	features []string
}

// Option configures a Server.
type Option func(*Server)

// WithMetrics serves GET /metrics and records every request in m.
func WithMetrics(m *metrics.Metrics) Option { return func(s *Server) { s.metrics = m } }

// WithFeatures lists optional behaviours tined runs with in GET /healthz.
func WithFeatures(f ...string) Option { return func(s *Server) { s.features = f } }

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
	s.mux.HandleFunc("/v1/vms/{id}/files", s.files)
	s.mux.HandleFunc("/v1/vms/{id}/archive", s.archive)
	s.mux.HandleFunc("/v1/vms/{id}/snapshots", s.vmSnapshots)
	s.mux.HandleFunc("/v1/vms/{id}/rollback", s.rollback)
	s.mux.HandleFunc("/v1/vms/{id}/fork", s.fork)
	s.mux.HandleFunc("/v1/snapshots", s.snapshots)
	s.mux.HandleFunc("/v1/snapshots/{id}", s.oneSnapshot)
	s.mux.HandleFunc("/v1/snapshots/{id}/fork", s.snapshotFork)
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
	writeJSON(w, http.StatusOK, Health{Status: "ok", Version: s.version, VMs: len(vms), UptimeSeconds: time.Since(s.started).Seconds(),
		Features: s.features})
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
		withMemory := r.URL.Query().Get("memory") == "true"
		out := ListVMsResponse{VMs: []VM{}}
		for _, v := range list {
			a := toAPI(v)
			if withMemory {
				a.Memory = s.memory(r.Context(), v)
			}
			out.VMs = append(out.VMs, a)
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
			Strategy: vm.ForkStrategy(req.Strategy), Network: state.Network(req.Network),
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
	out := toAPI(v)
	if r.Method == http.MethodGet {
		out.Memory = s.memory(r.Context(), v)
	}
	writeJSON(w, http.StatusOK, out)
}

// memory is a running VM's memory, or nil if it isn't running or can't be read.
func (s *Server) memory(ctx context.Context, v state.VM) *Memory {
	if v.State != state.StateRunning {
		return nil
	}
	m, err := s.backend.Memory(ctx, v.ID)
	if err != nil {
		s.log.DebugContext(ctx, "couldn't read a VM's memory", "vm", v.ID, "err", err)
		return nil
	}
	return &Memory{RSSBytes: m.RSS, PSSBytes: m.PSS, PrivateBytes: m.Private, SharedBytes: m.Shared}
}

// maxUploadBytes caps a file or an archive sent to a VM.
const maxUploadBytes = 8 << 30

// files: GET /v1/vms/{id}/files?path=P sends a file from the VM; PUT
// writes one (&mode=0644).
func (s *Server) files(w http.ResponseWriter, r *http.Request) {
	id, path := r.PathValue("id"), r.URL.Query().Get("path")
	ctx := logging.WithVM(r.Context(), id)
	switch r.Method {
	case http.MethodGet:
		rc, info, err := s.backend.ReadFile(ctx, id, path)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		defer func() { _ = rc.Close() }()
		w.Header().Set("Content-Type", "application/octet-stream")
		if info.Size >= 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
		}
		w.Header().Set(agentproto.HeaderFileMode, info.Mode)
		if _, err := io.Copy(w, rc); err != nil {
			s.log.WarnContext(ctx, "sending a file broke off", "err", err)
		}
	case http.MethodPut:
		mode := os.FileMode(0o644)
		if m := r.URL.Query().Get("mode"); m != "" {
			v, err := strconv.ParseUint(m, 8, 32)
			if err != nil || v > 0o7777 {
				writeError(w, http.StatusBadRequest, CodeInvalidRequest, fmt.Sprintf("mode %q: want octal, like 0644", m))
				return
			}
			mode = os.FileMode(v)
		}
		info, err := s.backend.WriteFile(ctx, id, path, mode, http.MaxBytesReader(w, r.Body, maxUploadBytes))
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, info)
	default:
		methodNotAllowed(w, r, http.MethodGet, http.MethodPut)
	}
}

// archive: PUT /v1/vms/{id}/archive?path=D unpacks a tar stream, gzipped or
// not, into the directory D in the VM.
func (s *Server) archive(w http.ResponseWriter, r *http.Request) {
	if !allow(w, r, http.MethodPut) {
		return
	}
	id := r.PathValue("id")
	res, err := s.backend.PutArchive(logging.WithVM(r.Context(), id), id, r.URL.Query().Get("path"), http.MaxBytesReader(w, r.Body, maxUploadBytes))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
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
		// The body is optional: clients from before it had a field sent {}
		// or nothing.
		var req SnapshotRequest
		if !decodeOptional(w, r, maxJSONBytes, &req) {
			return
		}
		snap, err := s.backend.Snapshot(ctx, r.PathValue("id"), vm.SnapshotRequest{Retain: req.Retain})
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

func (s *Server) fork(w http.ResponseWriter, r *http.Request) {
	req, ok := s.forkRequest(w, r)
	if !ok {
		return
	}
	ctx := logging.WithVM(r.Context(), r.PathValue("id"))
	res, err := s.backend.Fork(ctx, r.PathValue("id"), req)
	s.writeFork(w, r, res, err)
}

// snapshotFork: POST /v1/snapshots/{id}/fork restores children from a
// snapshot that's already there, with the same request and answer as a
// fork of a running VM.
func (s *Server) snapshotFork(w http.ResponseWriter, r *http.Request) {
	req, ok := s.forkRequest(w, r)
	if !ok {
		return
	}
	res, err := s.backend.ForkSnapshot(r.Context(), r.PathValue("id"), req)
	s.writeFork(w, r, res, err)
}

// forkRequest reads a fork's request. If it's no good, it has answered.
func (s *Server) forkRequest(w http.ResponseWriter, r *http.Request) (vm.ForkRequest, bool) {
	if !allow(w, r, http.MethodPost) {
		return vm.ForkRequest{}, false
	}
	var req ForkRequest
	if !decode(w, r, maxJSONBytes, &req) {
		return vm.ForkRequest{}, false
	}
	if req.TTLSeconds < 0 {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "ttl_seconds can't be negative")
		return vm.ForkRequest{}, false
	}
	return vm.ForkRequest{
		Count: req.Count, TTL: time.Duration(req.TTLSeconds) * time.Second, Strategy: vm.ForkStrategy(req.Strategy),
		Network: state.Network(req.Network),
	}, true
}

// writeFork answers a fork: 201 with its children if any was made.
func (s *Server) writeFork(w http.ResponseWriter, r *http.Request, res vm.ForkResult, err error) {
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if res.Failed() == len(res.Children) && len(res.Children) > 0 {
		s.fail(w, r, fmt.Errorf("no child could be made: %w", res.Children[0].Err))
		return
	}
	out := ForkResponse{Snapshot: toAPISnapshot(res.Snapshot, nil), Children: []ForkChild{}, TimingsMs: res.Timings}
	for _, c := range res.Children {
		if c.Err != nil {
			_, code := errorStatus(c.Err)
			out.Children = append(out.Children, ForkChild{Error: &ErrorDetail{Code: code, Message: c.Err.Error()}})
			continue
		}
		v := toAPI(c.VM)
		out.Children = append(out.Children, ForkChild{VM: &v})
	}
	writeJSON(w, http.StatusCreated, out)
}

// fail answers with the error's status and code.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	status, code := errorStatus(err)
	if status >= 500 {
		s.log.ErrorContext(r.Context(), "request failed", "err", err)
	}
	writeError(w, status, code, err.Error())
}

// errorStatus is the HTTP status and error code for err.
func errorStatus(err error) (int, string) {
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
	case errors.Is(err, vm.ErrNoMemory):
		status, code = http.StatusServiceUnavailable, CodeNoMemory
	case errors.Is(err, vm.ErrNoCPU):
		status, code = http.StatusServiceUnavailable, CodeNoCPU
	case errors.Is(err, state.ErrInvalidTransition), errors.Is(err, state.ErrConflict):
		status, code = http.StatusConflict, CodeConflict
	}
	return status, code
}

func toAPI(v state.VM) VM {
	out := VM{
		ID: v.ID, State: string(v.State), Image: v.Image, VCPUs: v.VCPUs, MemoryMiB: v.MemoryMiB,
		CreatedAt: v.CreatedAt, Error: v.Error, Source: string(v.Source), SnapshotID: v.SnapshotID, ParentID: v.ParentID,
		Network: string(v.Network), TimingsMs: v.Timings,
	}
	if out.Network == "" {
		out.Network = string(state.NetworkDefault)
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
		VCPUs: s.VCPUs, MemoryMiB: s.MemoryMiB, SizeBytes: s.SizeBytes, Retained: s.Retained, CreatedAt: s.CreatedAt, UsedBy: usedBy,
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

// decodeOptional is decode for a body that may be left out: an empty one
// leaves v as it is.
func decodeOptional(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
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
