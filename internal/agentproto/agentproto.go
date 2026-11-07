// Package agentproto is the protocol between tined on the host and
// tine-agent inside each VM, with a client for the host side.
//
// The agent serves HTTP/1.1 on vsock port Port. The host reaches it through
// Firecracker's vsock socket: connect to the socket, send "CONNECT 1024\n",
// read "OK <port>\n", and from then on it's an ordinary HTTP connection.
//
//	GET  /health          → Health
//	POST /exec            ExecRequest → a stream of Frames, one JSON object per line
//	POST /sync            SyncRequest → SyncResult
//	GET  /files?path=P    → the file's bytes, its mode in HeaderFileMode
//	PUT  /files?path=P    the file's bytes (&mode=0644) → FileInfo
//	PUT  /archive?path=D  a tar stream, maybe gzipped, unpacked into D → ArchiveResult
//
// /exec streams output while the command runs and always ends with a frame
// carrying the ExitStatus. Errors before a command starts come back as a
// normal HTTP error with an ErrorResponse body.
package agentproto

import "fmt"

const (
	// Port is the vsock port tine-agent listens on.
	Port = 1024
	// Version is the protocol version. It changes when either side would
	// misunderstand the other. Version 2 added /sync, version 3 /files and
	// /archive.
	Version = 3

	// StreamStdout and StreamStderr name a Frame's output stream.
	StreamStdout = "stdout"
	StreamStderr = "stderr"

	// ContentTypeNDJSON is /exec's response type: newline-delimited JSON.
	ContentTypeNDJSON = "application/x-ndjson"

	// HeaderFileMode carries a file's permissions, in octal, with GET /files.
	HeaderFileMode = "X-Tine-File-Mode"
)

// FileInfo is a file that PUT /files wrote, or GET /files sent.
type FileInfo struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	// Mode is the file's permissions, in octal, like "0644".
	Mode string `json:"mode"`
}

// ArchiveResult says what PUT /archive unpacked.
type ArchiveResult struct {
	Files int   `json:"files"`
	Dirs  int   `json:"dirs"`
	Links int   `json:"links"`
	Bytes int64 `json:"bytes"`
}

// Health is the agent's answer to GET /health.
type Health struct {
	// Status is "ok" when the agent can run commands.
	Status       string `json:"status"`
	AgentVersion string `json:"agent_version"`
	// Protocol is the agent's protocol Version.
	Protocol int `json:"protocol"`
	// UptimeSeconds is how long the agent has been running. After a VM is
	// restored from a snapshot it continues from the snapshot's value.
	UptimeSeconds float64 `json:"uptime_seconds"`
}

// ExecRequest runs one command (POST /exec).
type ExecRequest struct {
	// Args is the command and its arguments. Args[0] is looked up in PATH.
	// It isn't run through a shell: use ["sh", "-c", "..."] for that.
	Args []string `json:"args"`
	// Env is extra environment variables as KEY=VALUE, on top of the agent's
	// base environment (PATH, HOME, LANG).
	Env []string `json:"env,omitempty"`
	// Dir is the working directory. Empty means /root, or / if that doesn't exist.
	Dir string `json:"dir,omitempty"`
	// Stdin is fed to the command's standard input. Without it, stdin is empty.
	Stdin []byte `json:"stdin,omitempty"`
	// TimeoutMs kills the command after this many milliseconds. Zero means no
	// limit from the agent; the command still stops if the client disconnects.
	TimeoutMs int64 `json:"timeout_ms,omitempty"`
	// MaxOutputBytes caps stdout and stderr together. Output past the cap is
	// dropped, not sent, and the command keeps running. Zero means the agent's
	// own cap, which is also the most a request can ask for.
	MaxOutputBytes int64 `json:"max_output_bytes,omitempty"`
}

// Frame is one line of /exec's response: either some output, or the final
// exit status.
type Frame struct {
	// Stream and Data carry output: Stream is StreamStdout or StreamStderr.
	Stream string `json:"stream,omitempty"`
	Data   []byte `json:"data,omitempty"`
	// Exit is set on the last frame only.
	Exit *ExitStatus `json:"exit,omitempty"`
}

// ExitStatus is how a command ended.
type ExitStatus struct {
	// Code is the exit code, or -1 if a signal killed the command.
	Code int `json:"code"`
	// Signal names the signal that killed the command, if one did.
	Signal string `json:"signal,omitempty"`
	// TimedOut means the agent killed the command at its TimeoutMs.
	TimedOut bool `json:"timed_out,omitempty"`
	// OutputTruncated means some output was dropped at MaxOutputBytes.
	OutputTruncated bool `json:"output_truncated,omitempty"`
	// DurationMs is how long the command ran, as measured inside the guest.
	DurationMs float64 `json:"duration_ms"`
}

// ErrorResponse is the body of every error the agent returns.
type ErrorResponse struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail describes an error. Code is stable and meant for programs;
// Message is for people.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// SyncRequest makes a guest current and unique after it's restored from a
// snapshot, or booted (POST /sync). A restored guest's clock is stuck at the
// moment of the snapshot, and its identity and kernel random state are the
// snapshot's. Every field is optional.
type SyncRequest struct {
	// TimeUnixNano is the host's wall-clock time as it sent the request. The
	// guest's clock is set to it.
	TimeUnixNano int64 `json:"time_unix_nano,omitempty"`
	// Entropy is random bytes from the host. They're added to the guest
	// kernel's entropy pool, and the kernel's generator is reseeded, so two
	// guests restored from one snapshot don't produce the same numbers.
	Entropy []byte `json:"entropy,omitempty"`
	// Hostname becomes the guest's hostname: in the kernel, /etc/hostname and
	// /etc/hosts.
	Hostname string `json:"hostname,omitempty"`
	// MachineID becomes /etc/machine-id: 32 lowercase hexadecimal characters.
	MachineID string `json:"machine_id,omitempty"`
}

// SyncResult says what /sync did.
type SyncResult struct {
	// ClockOffsetMs is how far the guest's clock was from the host's before
	// it was set: negative means it was behind.
	ClockOffsetMs float64 `json:"clock_offset_ms"`
	// DurationMs is how long the agent took, as measured inside the guest.
	DurationMs float64 `json:"duration_ms"`
}

// Error codes.
const (
	// CodeBadRequest: the request was malformed, e.g. no Args.
	CodeBadRequest = "bad_request"
	// CodeStartFailed: the command couldn't start, e.g. it doesn't exist.
	CodeStartFailed = "start_failed"
	// CodeSyncFailed: /sync couldn't change something it was asked to.
	CodeSyncFailed = "sync_failed"
	// CodeNotFound and CodeMethodNotAllowed: unknown path or method.
	CodeNotFound         = "not_found"
	CodeMethodNotAllowed = "method_not_allowed"
	// CodeFileNotFound: a file /files was asked for doesn't exist.
	CodeFileNotFound = "file_not_found"
	// CodeFileFailed: reading or writing a file failed in the guest.
	CodeFileFailed = "file_failed"
)

// Error is an error response from the agent, as the client returns it.
type Error struct {
	StatusCode int
	ErrorDetail
}

func (e *Error) Error() string {
	return fmt.Sprintf("agent: %d %s: %s", e.StatusCode, e.Code, e.Message)
}
