// Package api is tined's HTTP API: the request and response types, the
// server, and a Go client for it.
//
// tined serves it on a Unix socket (DefaultSocket). Every response is JSON,
// except exec's, which streams newline-delimited JSON frames in the same
// format as tine-agent (see internal/agentproto). Every error is an
// ErrorResponse with an HTTP status to match:
//
//	GET    /healthz                Health
//	GET    /v1/images              ListImagesResponse
//	POST   /v1/images/{name}/warm  WarmRequest → Snapshot (201), the new template
//	POST   /v1/vms                 CreateVMRequest → VM (201)
//	GET    /v1/vms[?all=true][&memory=true]  ListVMsResponse
//	GET    /v1/vms/{id}            VM, with its memory if it's running
//	DELETE /v1/vms/{id}            VM
//	POST   /v1/vms/{id}/exec       ExecRequest → a stream of Frames
//	POST   /v1/vms/{id}/snapshots  Snapshot (201)
//	GET    /v1/vms/{id}/snapshots  ListSnapshotsResponse, the VM's
//	POST   /v1/vms/{id}/rollback   RollbackRequest → VM
//	POST   /v1/vms/{id}/fork       ForkRequest → ForkResponse (201)
//	GET    /v1/snapshots[?vm=&kind=&image=]  ListSnapshotsResponse
//	GET    /v1/snapshots/{id}      Snapshot
//	DELETE /v1/snapshots/{id}      Snapshot (409 in_use while VMs run from it)
package api

import (
	"time"

	"github.com/sanidhyasin/tine/internal/agentproto"
)

// DefaultSocket is where tined listens.
const DefaultSocket = "/run/tine/tine.sock"

// VM is a VM as the API shows it.
type VM struct {
	ID          string     `json:"id"`
	State       string     `json:"state"`
	Image       string     `json:"image"`
	VCPUs       int        `json:"vcpus"`
	MemoryMiB   int        `json:"memory_mib"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	DestroyedAt *time.Time `json:"destroyed_at,omitempty"`
	// Error says why the VM failed, if it did.
	Error string `json:"error,omitempty"`
	// Source is how the VM started: "cold" (booted), "template" (restored
	// from its image's template) or "snapshot".
	Source string `json:"source,omitempty"`
	// SnapshotID is the snapshot the VM was restored from, and runs from. It's
	// empty for a VM that was booted from scratch.
	SnapshotID string `json:"snapshot_id,omitempty"`
	// ParentID is the VM this one was forked from.
	ParentID string `json:"parent_id,omitempty"`
	// Memory is how much host memory it uses now: on GET /v1/vms/{id}, and
	// on lists with ?memory=true, for running VMs.
	Memory *Memory `json:"memory,omitempty"`
	// TimingsMs is how long each step of creating (and destroying) it took.
	TimingsMs map[string]float64 `json:"timings_ms,omitempty"`
}

// Memory is how much host memory a VM's Firecracker uses, in bytes. PSS
// divides each shared page between the VMs sharing it: forks of one parent
// each have a PSS well below their RSS, and PSS is what a VM really costs.
type Memory struct {
	RSSBytes     int64 `json:"rss_bytes"`
	PSSBytes     int64 `json:"pss_bytes"`
	PrivateBytes int64 `json:"private_bytes"`
	SharedBytes  int64 `json:"shared_bytes"`
}

// Snapshot is a saved state of a VM, which VMs can be restored from.
type Snapshot struct {
	ID string `json:"id"`
	// Kind is "vm", a save point of a VM, or "template", an image booted
	// and saved so new VMs can start from it.
	Kind string `json:"kind"`
	// State is "creating", "ready" or "deleting".
	State string `json:"state"`
	// VMID is the VM it was taken from.
	VMID  string `json:"vm_id,omitempty"`
	Image string `json:"image"`
	// VCPUs and MemoryMiB are the size of every VM restored from it.
	VCPUs     int `json:"vcpus"`
	MemoryMiB int `json:"memory_mib"`
	// SizeBytes is the disk space its files take.
	SizeBytes int64     `json:"size_bytes"`
	CreatedAt time.Time `json:"created_at"`
	// UsedBy lists the VMs that run from it. It can't be deleted while any do.
	UsedBy []string `json:"used_by,omitempty"`
	// FirecrackerVersion is what it was taken with, and needs to be restored.
	FirecrackerVersion string `json:"firecracker_version,omitempty"`
	// TimingsMs is how long each step of taking it took. "paused" is how long
	// the VM was frozen.
	TimingsMs map[string]float64 `json:"timings_ms,omitempty"`
}

// CreateVMRequest asks for a VM. Zero values mean tined's defaults.
type CreateVMRequest struct {
	Image      string `json:"image,omitempty"`
	VCPUs      int    `json:"vcpus,omitempty"`
	MemoryMiB  int    `json:"memory_mib,omitempty"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
	// Cold boots the VM even if its image has a template it could start from.
	Cold bool `json:"cold,omitempty"`
}

// ListVMsResponse is GET /v1/vms's answer.
type ListVMsResponse struct {
	VMs []VM `json:"vms"`
}

// Image is an installed image.
type Image struct {
	Name         string    `json:"name"`
	CreatedAt    time.Time `json:"created_at"`
	Kernel       string    `json:"kernel"`
	DiskBytes    int64     `json:"disk_bytes"`
	AgentVersion string    `json:"agent_version,omitempty"`
	// Templates are the image's newest templates, one for each size.
	Templates []Template `json:"templates,omitempty"`
}

// Template is an image booted once and saved, which new VMs of its size
// start from instead of booting.
type Template struct {
	SnapshotID string    `json:"snapshot_id"`
	VCPUs      int       `json:"vcpus"`
	MemoryMiB  int       `json:"memory_mib"`
	CreatedAt  time.Time `json:"created_at"`
	// Problem says why new VMs can't start from it, so they boot instead.
	// It's empty when they can.
	Problem string `json:"problem,omitempty"`
}

// ForkRequest asks for children of a running VM.
type ForkRequest struct {
	Count      int `json:"count"`
	TTLSeconds int `json:"ttl_seconds,omitempty"`
	// Strategy is how the children get their memory (see vm.ForkStrategy).
	// Empty means tined's default, "share".
	Strategy string `json:"strategy,omitempty"`
}

// ForkResponse is what a fork made. It's 201 Created if at least one child
// was made; each child either has a VM or an error.
type ForkResponse struct {
	// Snapshot is the parent's snapshot the children were restored from.
	Snapshot Snapshot    `json:"snapshot"`
	Children []ForkChild `json:"children"`
	// TimingsMs: snapshot, paused (how long the parent was frozen),
	// children (restoring them all) and total.
	TimingsMs map[string]float64 `json:"timings_ms"`
}

// ForkChild is one child of a fork: the VM, or why it couldn't be made.
type ForkChild struct {
	VM    *VM          `json:"vm,omitempty"`
	Error *ErrorDetail `json:"error,omitempty"`
}

// ListSnapshotsResponse is GET /v1/snapshots's answer.
type ListSnapshotsResponse struct {
	Snapshots []Snapshot `json:"snapshots"`
}

// RollbackRequest rolls a VM back to one of its snapshots, or to the
// template it started from.
type RollbackRequest struct {
	SnapshotID string `json:"snapshot_id"`
}

// WarmRequest asks for a template of an image. Zero values mean tined's
// default VM size.
type WarmRequest struct {
	VCPUs     int `json:"vcpus,omitempty"`
	MemoryMiB int `json:"memory_mib,omitempty"`
}

// ListImagesResponse is GET /v1/images's answer.
type ListImagesResponse struct {
	Images []Image `json:"images"`
}

// Health is GET /healthz's answer.
type Health struct {
	Status        string  `json:"status"`
	Version       string  `json:"version"`
	VMs           int     `json:"vms"`
	UptimeSeconds float64 `json:"uptime_seconds"`
}

// Exec uses the agent's types, so tined passes requests and frames through
// unchanged.
type (
	ExecRequest = agentproto.ExecRequest
	Frame       = agentproto.Frame
	ExitStatus  = agentproto.ExitStatus
)

// ErrorResponse is the body of every error.
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail describes an error. Code is stable, for programs; Message is
// for people.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error codes.
const (
	CodeNotFound         = "not_found"
	CodeImageNotFound    = "image_not_found"
	CodeInvalidRequest   = "invalid_request"
	CodeCapacity         = "capacity"
	CodeNotRunning       = "not_running"
	CodeConflict         = "conflict"
	CodeBusy             = "busy"         // e.g. a snapshot while commands run in the VM
	CodeInUse            = "in_use"       // e.g. deleting a snapshot that VMs run from
	CodeIncompatible     = "incompatible" // a snapshot this host can't restore
	CodeNoSpace          = "no_space"
	CodeNoMemory         = "no_memory" // admission control: the host is short of memory for more VMs
	CodeNoCPU            = "no_cpu"    // admission control: more vCPUs than the host may run
	CodeMethodNotAllowed = "method_not_allowed"
	CodeInternal         = "internal"
)
