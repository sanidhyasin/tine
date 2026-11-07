package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/sanidhyasin/tine/internal/agentproto"
)

// Client talks to tined. It's safe for concurrent use.
type Client struct {
	http *http.Client
	base string
}

// NewClient returns a client that reaches tined through dial.
func NewClient(dial func(ctx context.Context) (net.Conn, error)) *Client {
	return &Client{
		http: &http.Client{Transport: &http.Transport{
			DialContext:        func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
			DisableCompression: true,
		}},
		base: "http://tined",
	}
}

// NewUnixClient returns a client for tined listening on a Unix socket.
func NewUnixClient(socket string) *Client {
	return NewClient(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	})
}

// Error is an error response from tined.
type Error struct {
	StatusCode int
	ErrorDetail
}

func (e *Error) Error() string { return e.Message }

// Health checks tined is up.
func (c *Client) Health(ctx context.Context) (Health, error) {
	var h Health
	return h, c.do(ctx, http.MethodGet, "/healthz", nil, http.StatusOK, &h)
}

// Images lists the installed images.
func (c *Client) Images(ctx context.Context) ([]Image, error) {
	var r ListImagesResponse
	return r.Images, c.do(ctx, http.MethodGet, "/v1/images", nil, http.StatusOK, &r)
}

// CreateVM creates a VM and returns it once it's running.
func (c *Client) CreateVM(ctx context.Context, req CreateVMRequest) (VM, error) {
	var v VM
	return v, c.do(ctx, http.MethodPost, "/v1/vms", req, http.StatusCreated, &v)
}

// ListVMs lists the VMs that aren't destroyed, or all of them.
func (c *Client) ListVMs(ctx context.Context, all bool) ([]VM, error) {
	path := "/v1/vms"
	if all {
		path += "?all=true"
	}
	var r ListVMsResponse
	return r.VMs, c.do(ctx, http.MethodGet, path, nil, http.StatusOK, &r)
}

// GetVM returns one VM.
func (c *Client) GetVM(ctx context.Context, id string) (VM, error) {
	var v VM
	return v, c.do(ctx, http.MethodGet, "/v1/vms/"+url.PathEscape(id), nil, http.StatusOK, &v)
}

// CreateSnapshot snapshots a running VM.
func (c *Client) CreateSnapshot(ctx context.Context, vmID string) (Snapshot, error) {
	var snap Snapshot
	return snap, c.do(ctx, http.MethodPost, "/v1/vms/"+url.PathEscape(vmID)+"/snapshots", struct{}{}, http.StatusCreated, &snap)
}

// DestroyVM destroys a VM and returns its final record.
func (c *Client) DestroyVM(ctx context.Context, id string) (VM, error) {
	var v VM
	return v, c.do(ctx, http.MethodDelete, "/v1/vms/"+url.PathEscape(id), nil, http.StatusOK, &v)
}

// Exec runs a command in a VM. onOutput, if not nil, gets output as it
// arrives. A command that runs and fails isn't an error: see ExitStatus.
func (c *Client) Exec(ctx context.Context, id string, req ExecRequest, onOutput func(stream string, data []byte)) (ExitStatus, error) {
	resp, err := c.send(ctx, http.MethodPost, "/v1/vms/"+url.PathEscape(id)+"/exec", req)
	if err != nil {
		return ExitStatus{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ExitStatus{}, readError(resp)
	}
	return agentproto.ReadFrames(resp.Body, onOutput)
}

func (c *Client) do(ctx context.Context, method, path string, body any, want int, out any) error {
	resp, err := c.send(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != want {
		return readError(resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("tined %s %s: decode response: %w", method, path, err)
	}
	return nil
}

func (c *Client) send(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("can't reach tined: %w", err)
	}
	return resp, nil
}

func readError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var er ErrorResponse
	if err := json.Unmarshal(data, &er); err != nil || er.Error.Code == "" {
		return &Error{StatusCode: resp.StatusCode, ErrorDetail: ErrorDetail{Code: "unknown", Message: strings.TrimSpace(string(data))}}
	}
	return &Error{StatusCode: resp.StatusCode, ErrorDetail: er.Error}
}
