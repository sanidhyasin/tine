// Package firecracker is a small client for the HTTP API that a Firecracker
// process serves on its Unix socket.
//
// It covers what tine needs to configure, start, pause and resume a microVM.
// Snapshot calls arrive in step 2 (docs/PLAN.md, task 2.1). Request and
// response types follow Firecracker v1.17's API spec
// (src/firecracker/swagger/firecracker.yaml).
//
// Every call is timed. Pass WithObserver to receive a CallInfo for each one,
// for example to feed metrics or a VM's timing breakdown.
package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	// DefaultTimeout applies to calls whose context has no deadline.
	DefaultTimeout = 10 * time.Second

	// maxResponseBytes caps how much of a response body is read. Firecracker's
	// responses are tiny; this protects tined from a misbehaving process.
	maxResponseBytes = 1 << 20
)

// Client talks to one Firecracker process through its API socket.
// It's safe for concurrent use.
type Client struct {
	socketPath string
	http       *http.Client
	timeout    time.Duration
	observe    func(CallInfo)
}

// Option configures a Client.
type Option func(*Client)

// WithTimeout sets the timeout for calls whose context has no deadline.
// Zero means no timeout. The default is DefaultTimeout.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.timeout = d }
}

// WithObserver calls fn after every API call, successful or not.
// fn runs on the calling goroutine and should return quickly.
func WithObserver(fn func(CallInfo)) Option {
	return func(c *Client) { c.observe = fn }
}

// CallInfo describes one finished API call.
type CallInfo struct {
	// Endpoint is the method and route template, e.g. "PUT /drives/{drive_id}".
	// It's stable across calls, so it suits metric labels.
	Endpoint string
	// Path is the actual request path, e.g. "/drives/rootfs".
	Path string
	// Status is the HTTP status code, or 0 if no response arrived.
	Status int
	// Duration covers the whole call, from sending the request to reading the response.
	Duration time.Duration
	// Err is the call's error, or nil.
	Err error
}

// New returns a client for the Firecracker process listening on socketPath.
// It doesn't connect until the first call.
func New(socketPath string, opts ...Option) *Client {
	c := &Client{socketPath: socketPath, timeout: DefaultTimeout}
	for _, opt := range opts {
		opt(c)
	}
	var dialer net.Dialer
	c.http = &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "unix", socketPath)
			},
			// A fresh connection per call: connecting to a local socket costs
			// microseconds, and it rules out reusing a connection to a
			// Firecracker process that has since exited.
			DisableKeepAlives:  true,
			DisableCompression: true,
		},
	}
	return c
}

// SocketPath returns the API socket this client talks to.
func (c *Client) SocketPath() string { return c.socketPath }

// WaitReady blocks until Firecracker accepts connections on its socket, which
// happens shortly after the process starts. It checks every interval and
// gives up when ctx is done.
func (c *Client) WaitReady(ctx context.Context, interval time.Duration) error {
	var dialer net.Dialer
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		conn, err := dialer.DialContext(ctx, "unix", c.socketPath)
		if err == nil {
			return conn.Close()
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("firecracker socket %s never became ready: %w (last error: %w)", c.socketPath, ctx.Err(), err)
		case <-ticker.C:
		}
	}
}

// APIError is an error response from Firecracker.
type APIError struct {
	Endpoint   string // e.g. "PUT /machine-config"
	StatusCode int
	// FaultMessage is Firecracker's explanation, from the "fault_message" field.
	FaultMessage string
}

func (e *APIError) Error() string {
	msg := e.FaultMessage
	if msg == "" {
		msg = http.StatusText(e.StatusCode)
	}
	return fmt.Sprintf("firecracker %s: %d: %s", e.Endpoint, e.StatusCode, msg)
}

// do sends one request. route is the path template used in Endpoint (e.g.
// "/drives/{drive_id}"); path is the actual, already-escaped path. body, if
// not nil, is sent as JSON. out, if not nil, receives the JSON response.
func (c *Client) do(ctx context.Context, method, route, path string, body, out any) (err error) {
	endpoint := method + " " + route
	start := time.Now()
	status := 0
	if c.observe != nil {
		defer func() {
			c.observe(CallInfo{Endpoint: endpoint, Path: path, Status: status, Duration: time.Since(start), Err: err})
		}()
	}

	if _, ok := ctx.Deadline(); !ok && c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("firecracker %s: encode request: %w", endpoint, err)
		}
		reqBody = bytes.NewReader(data)
	}

	// The host part is ignored: the transport always dials the socket.
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, reqBody)
	if err != nil {
		return fmt.Errorf("firecracker %s: build request: %w", endpoint, err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("firecracker %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	status = resp.StatusCode

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("firecracker %s: read response: %w", endpoint, err)
	}

	if status < 200 || status > 299 {
		return newAPIError(endpoint, status, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("firecracker %s: decode response: %w", endpoint, err)
		}
	}
	return nil
}

func newAPIError(endpoint string, status int, body []byte) *APIError {
	var parsed struct {
		FaultMessage string `json:"fault_message"`
	}
	msg := ""
	if json.Unmarshal(body, &parsed) == nil {
		msg = parsed.FaultMessage
	}
	if msg == "" {
		// Not Firecracker's usual JSON error: keep the start of whatever came back.
		msg = strings.TrimSpace(string(body))
		if len(msg) > 200 {
			msg = msg[:200] + "…"
		}
	}
	return &APIError{Endpoint: endpoint, StatusCode: status, FaultMessage: msg}
}
