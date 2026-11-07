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
	"os"
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
	return c.ListVMsWith(ctx, ListOptions{All: all})
}

// ListOptions says what ListVMsWith lists.
type ListOptions struct {
	// All includes destroyed VMs.
	All bool
	// Memory includes each running VM's memory use.
	Memory bool
}

// ListVMsWith lists VMs, oldest first.
func (c *Client) ListVMsWith(ctx context.Context, o ListOptions) ([]VM, error) {
	q := url.Values{}
	if o.All {
		q.Set("all", "true")
	}
	if o.Memory {
		q.Set("memory", "true")
	}
	path := "/v1/vms"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var r ListVMsResponse
	return r.VMs, c.do(ctx, http.MethodGet, path, nil, http.StatusOK, &r)
}

// GetVM returns one VM.
func (c *Client) GetVM(ctx context.Context, id string) (VM, error) {
	var v VM
	return v, c.do(ctx, http.MethodGet, "/v1/vms/"+url.PathEscape(id), nil, http.StatusOK, &v)
}

// WarmTemplate boots image and saves it as the template that new VMs of
// its size start from.
func (c *Client) WarmTemplate(ctx context.Context, image string, req WarmRequest) (Snapshot, error) {
	var snap Snapshot
	return snap, c.do(ctx, http.MethodPost, "/v1/images/"+url.PathEscape(image)+"/warm", req, http.StatusCreated, &snap)
}

// SnapshotFilter narrows ListSnapshots. Empty fields match everything.
type SnapshotFilter struct {
	VMID  string
	Kind  string // "vm" or "template"
	Image string
}

// ListSnapshots lists snapshots, oldest first.
func (c *Client) ListSnapshots(ctx context.Context, f SnapshotFilter) ([]Snapshot, error) {
	q := url.Values{}
	for k, v := range map[string]string{"vm": f.VMID, "kind": f.Kind, "image": f.Image} {
		if v != "" {
			q.Set(k, v)
		}
	}
	path := "/v1/snapshots"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out ListSnapshotsResponse
	return out.Snapshots, c.do(ctx, http.MethodGet, path, nil, http.StatusOK, &out)
}

// GetSnapshot returns one snapshot, with the VMs that run from it.
func (c *Client) GetSnapshot(ctx context.Context, id string) (Snapshot, error) {
	var snap Snapshot
	return snap, c.do(ctx, http.MethodGet, "/v1/snapshots/"+url.PathEscape(id), nil, http.StatusOK, &snap)
}

// DeleteSnapshot deletes a snapshot that no VM runs from.
func (c *Client) DeleteSnapshot(ctx context.Context, id string) (Snapshot, error) {
	var snap Snapshot
	return snap, c.do(ctx, http.MethodDelete, "/v1/snapshots/"+url.PathEscape(id), nil, http.StatusOK, &snap)
}

// Fork makes children of a running VM. It returns an error only if none
// could be made; otherwise each child has a VM or an error.
func (c *Client) Fork(ctx context.Context, vmID string, req ForkRequest) (ForkResponse, error) {
	var res ForkResponse
	return res, c.do(ctx, http.MethodPost, "/v1/vms/"+url.PathEscape(vmID)+"/fork", req, http.StatusCreated, &res)
}

// Rollback rolls a VM back to a snapshot.
func (c *Client) Rollback(ctx context.Context, vmID, snapshotID string) (VM, error) {
	var v VM
	return v, c.do(ctx, http.MethodPost, "/v1/vms/"+url.PathEscape(vmID)+"/rollback", RollbackRequest{SnapshotID: snapshotID}, http.StatusOK, &v)
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

// ReadFile streams a file from a VM. The caller must close the reader.
func (c *Client) ReadFile(ctx context.Context, id, path string) (io.ReadCloser, FileInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/v1/vms/"+url.PathEscape(id)+"/files?"+url.Values{"path": {path}}.Encode(), nil)
	if err != nil {
		return nil, FileInfo{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, FileInfo{}, fmt.Errorf("can't reach tined: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		return nil, FileInfo{}, readError(resp)
	}
	return resp.Body, FileInfo{Path: path, Size: resp.ContentLength, Mode: resp.Header.Get(agentproto.HeaderFileMode)}, nil
}

// WriteFile writes r to a file in a VM, with permissions mode, creating the
// directories it's in.
func (c *Client) WriteFile(ctx context.Context, id, path string, mode os.FileMode, r io.Reader) (FileInfo, error) {
	q := url.Values{"path": {path}, "mode": {fmt.Sprintf("%04o", mode.Perm())}}
	var info FileInfo
	err := c.upload(ctx, "/v1/vms/"+url.PathEscape(id)+"/files?"+q.Encode(), r, &info)
	return info, err
}

// PutArchive unpacks a tar stream, gzipped or not, into a directory in a VM.
func (c *Client) PutArchive(ctx context.Context, id, dir string, r io.Reader) (ArchiveResult, error) {
	var res ArchiveResult
	err := c.upload(ctx, "/v1/vms/"+url.PathEscape(id)+"/archive?"+url.Values{"path": {dir}}.Encode(), r, &res)
	return res, err
}

func (c *Client) upload(ctx context.Context, path string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("can't reach tined: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return readError(resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("tined PUT %s: decode response: %w", path, err)
	}
	return nil
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
