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
//	POST   /v1/vms                 CreateVMRequest → VM (201)
//	GET    /v1/vms[?all=true]      ListVMsResponse
//	GET    /v1/vms/{id}            VM
//	DELETE /v1/vms/{id}            VM
//	POST   /v1/vms/{id}/exec       ExecRequest → a stream of Frames
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
	// TimingsMs is how long each step of creating (and destroying) it took.
	TimingsMs map[string]float64 `json:"timings_ms,omitempty"`
}

// CreateVMRequest asks for a VM. Zero values mean tined's defaults.
type CreateVMRequest struct {
	Image      string `json:"image,omitempty"`
	VCPUs      int    `json:"vcpus,omitempty"`
	MemoryMiB  int    `json:"memory_mib,omitempty"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
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
	CodeMethodNotAllowed = "method_not_allowed"
	CodeInternal         = "internal"
)
