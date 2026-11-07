package agentproto

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	// handshakeTimeout bounds the vsock CONNECT exchange when ctx has no
	// earlier deadline.
	handshakeTimeout = 5 * time.Second
	// maxFrameBytes caps one line of /exec's response. Agent frames carry at
	// most 32 KiB of output, which base64 makes about 43 KiB.
	maxFrameBytes = 1 << 20
	// maxErrorBodyBytes caps how much of an error response is read.
	maxErrorBodyBytes = 64 << 10
)

// ErrIncomplete means the connection ended before the command's exit status
// arrived. The VM may have stopped, or been snapshotted, which resets vsock
// connections.
var ErrIncomplete = errors.New("agent connection closed before the command finished")

// DialVsock connects to a guest's vsock port through Firecracker's vsock
// socket at udsPath (the host side of the VM's vsock device).
func DialVsock(ctx context.Context, udsPath string, port uint32) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", udsPath)
	if err != nil {
		return nil, fmt.Errorf("vsock %s: %w", udsPath, err)
	}
	deadline := time.Now().Add(handshakeTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("vsock %s: send CONNECT: %w", udsPath, err)
	}
	// Read the reply one byte at a time: a buffered reader could swallow the
	// start of what comes next on the connection.
	line, err := readLine(conn, 64)
	if err != nil {
		_ = conn.Close()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("vsock %s: nothing is listening on guest port %d (Firecracker closed the connection)", udsPath, port)
		}
		return nil, fmt.Errorf("vsock %s: read CONNECT reply: %w", udsPath, err)
	}
	if !strings.HasPrefix(line, "OK ") {
		_ = conn.Close()
		return nil, fmt.Errorf("vsock %s: unexpected CONNECT reply %q", udsPath, line)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func readLine(r io.Reader, limit int) (string, error) {
	var buf []byte
	b := make([]byte, 1)
	for len(buf) < limit {
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		if b[0] == '\n' {
			return string(buf), nil
		}
		buf = append(buf, b[0])
	}
	return "", fmt.Errorf("line longer than %d bytes", limit)
}

// Client talks to one VM's agent. It's safe for concurrent use: every call
// uses a connection of its own.
type Client struct {
	http *http.Client
}

// NewClient returns a client that reaches the agent through dial.
func NewClient(dial func(ctx context.Context) (net.Conn, error)) *Client {
	return &Client{http: &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
			// One connection per call: vsock connections don't survive a
			// snapshot, so pooling them would only hand out dead ones.
			DisableKeepAlives:  true,
			DisableCompression: true,
		},
	}}
}

// NewVsockClient returns a client for the agent behind a VM's vsock socket.
func NewVsockClient(udsPath string) *Client {
	return NewClient(func(ctx context.Context) (net.Conn, error) {
		return DialVsock(ctx, udsPath, Port)
	})
}

// Health asks the agent whether it's ready.
func (c *Client) Health(ctx context.Context) (Health, error) {
	var h Health
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://agent/health", nil)
	if err != nil {
		return h, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return h, fmt.Errorf("agent health: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return h, readError(resp)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxErrorBodyBytes)).Decode(&h); err != nil {
		return h, fmt.Errorf("agent health: decode: %w", err)
	}
	return h, nil
}

// WaitHealthy polls Health every interval until the agent answers "ok", for
// example while a VM boots. It gives up when ctx is done.
func (c *Client) WaitHealthy(ctx context.Context, interval time.Duration) (Health, error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		attempt, cancel := context.WithTimeout(ctx, 2*time.Second)
		h, err := c.Health(attempt)
		cancel()
		if err == nil && h.Status == "ok" {
			return h, nil
		}
		if err == nil {
			err = fmt.Errorf("agent status is %q", h.Status)
		}
		select {
		case <-ctx.Done():
			return Health{}, fmt.Errorf("agent never became healthy: %w (last error: %w)", ctx.Err(), err)
		case <-ticker.C:
		}
	}
}

// Sync makes the guest current and unique: its clock, entropy, hostname
// and machine ID (see SyncRequest).
func (c *Client) Sync(ctx context.Context, r SyncRequest) (SyncResult, error) {
	var res SyncResult
	body, err := json.Marshal(r)
	if err != nil {
		return res, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://agent/sync", bytes.NewReader(body))
	if err != nil {
		return res, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return res, fmt.Errorf("agent sync: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return res, readError(resp)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxErrorBodyBytes)).Decode(&res); err != nil {
		return res, fmt.Errorf("agent sync: decode: %w", err)
	}
	return res, nil
}

// Exec runs a command in the VM. onOutput, if not nil, receives output as it
// arrives, on the calling goroutine. Exec returns once the command has
// finished. Cancelling ctx makes the agent kill the command.
//
// A command that runs and fails isn't an error: its ExitStatus says how it
// ended. Errors mean the command couldn't start (*Error) or the connection
// broke (ErrIncomplete, or a network error).
func (c *Client) Exec(ctx context.Context, r ExecRequest, onOutput func(stream string, data []byte)) (ExitStatus, error) {
	body, err := json.Marshal(r)
	if err != nil {
		return ExitStatus{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://agent/exec", bytes.NewReader(body))
	if err != nil {
		return ExitStatus{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return ExitStatus{}, fmt.Errorf("agent exec: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ExitStatus{}, readError(resp)
	}
	st, err := ReadFrames(resp.Body, onOutput)
	if err != nil {
		return ExitStatus{}, fmt.Errorf("agent exec: %w", err)
	}
	return st, nil
}

// ReadFrames reads an exec stream: it passes output frames to onOutput (if
// not nil) and returns the exit status from the final frame. A stream that
// ends without one is ErrIncomplete. tined's API streams exec output in the
// same format, so its client reads it with this too.
func ReadFrames(r io.Reader, onOutput func(stream string, data []byte)) (ExitStatus, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), maxFrameBytes)
	for scanner.Scan() {
		var f Frame
		if err := json.Unmarshal(scanner.Bytes(), &f); err != nil {
			return ExitStatus{}, fmt.Errorf("bad frame: %w", err)
		}
		if f.Exit != nil {
			return *f.Exit, nil
		}
		if onOutput != nil && len(f.Data) > 0 {
			onOutput(f.Stream, f.Data)
		}
	}
	if err := scanner.Err(); err != nil {
		return ExitStatus{}, fmt.Errorf("%w: %w", ErrIncomplete, err)
	}
	return ExitStatus{}, ErrIncomplete
}

// ReadFile streams a file from the guest. The caller must close the
// reader. FileInfo has the file's size and mode.
func (c *Client) ReadFile(ctx context.Context, path string) (io.ReadCloser, FileInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://agent/files?"+url.Values{"path": {path}}.Encode(), nil)
	if err != nil {
		return nil, FileInfo{}, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, FileInfo{}, fmt.Errorf("agent read file: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		return nil, FileInfo{}, readError(resp)
	}
	return resp.Body, FileInfo{Path: path, Size: resp.ContentLength, Mode: resp.Header.Get(HeaderFileMode)}, nil
}

// WriteFile writes r to a file in the guest, with permissions mode,
// creating the directories it's in. The file is replaced whole.
func (c *Client) WriteFile(ctx context.Context, path string, mode os.FileMode, r io.Reader) (FileInfo, error) {
	q := url.Values{"path": {path}, "mode": {fmt.Sprintf("%04o", mode.Perm())}}
	var info FileInfo
	err := c.put(ctx, "http://agent/files?"+q.Encode(), r, &info)
	return info, err
}

// PutArchive unpacks a tar stream (gzipped or not) into a directory in the
// guest, creating it if need be.
func (c *Client) PutArchive(ctx context.Context, dir string, r io.Reader) (ArchiveResult, error) {
	var res ArchiveResult
	err := c.put(ctx, "http://agent/archive?"+url.Values{"path": {dir}}.Encode(), r, &res)
	return res, err
}

func (c *Client) put(ctx context.Context, u string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("agent: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return readError(resp)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxErrorBodyBytes)).Decode(out); err != nil {
		return fmt.Errorf("agent: decode: %w", err)
	}
	return nil
}

func readError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	var er ErrorResponse
	if err := json.Unmarshal(data, &er); err != nil || er.Error.Code == "" {
		return &Error{StatusCode: resp.StatusCode, ErrorDetail: ErrorDetail{Code: "unknown", Message: strings.TrimSpace(string(data))}}
	}
	return &Error{StatusCode: resp.StatusCode, ErrorDetail: er.Error}
}
