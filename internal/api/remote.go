package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Reaching tined on another machine, the way Docker's ssh:// hosts work:
// each connection is a command, such as "ssh host tine dial-stdio", whose
// standard input and output are joined to tined's socket over there. No
// port is opened, and SSH does the authentication.

// NewSSHClient returns a client for tined on the machine a URL names:
// ssh://[user@]host[:port], optionally with the socket's path, as in
// ssh://me@tine-dev/run/tine/tine.sock. The remote machine needs tine
// installed, and the user must be able to use tined's socket.
func NewSSHClient(target string) (*Client, error) {
	argv, err := sshCommand(target)
	if err != nil {
		return nil, err
	}
	return NewCommandClient(argv), nil
}

// sshCommand is the command a connection to an ssh:// host runs.
func sshCommand(target string) ([]string, error) {
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "ssh" || u.Hostname() == "" {
		return nil, fmt.Errorf("host %q: want ssh://[user@]host[:port][/socket]", target)
	}
	argv := []string{"ssh", "-T", "-o", "BatchMode=yes"}
	if p := u.Port(); p != "" {
		argv = append(argv, "-p", p)
	}
	dest := u.Hostname()
	if u.User != nil && u.User.Username() != "" {
		dest = u.User.Username() + "@" + dest
	}
	argv = append(argv, "--", dest, "tine", "dial-stdio")
	if u.Path != "" && u.Path != "/" {
		argv = append(argv, "--socket", u.Path)
	}
	return argv, nil
}

// NewCommandClient returns a client whose every connection is a new run of
// argv, speaking HTTP on its standard input and output. Its standard error
// goes to this process's.
func NewCommandClient(argv []string) *Client {
	return NewClient(func(ctx context.Context) (net.Conn, error) {
		return dialCommand(ctx, argv)
	})
}

func dialCommand(ctx context.Context, argv []string) (net.Conn, error) {
	if len(argv) == 0 {
		return nil, errors.New("no command to connect through")
	}
	// The connection outlives the dial, so the command isn't tied to ctx.
	cmd := exec.CommandContext(context.WithoutCancel(ctx), argv[0], argv[1:]...) //nolint:gosec // the user's own ssh command
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("connect through %s: %w", strings.Join(argv, " "), err)
	}
	return &commandConn{cmd: cmd, stdin: stdin, stdout: stdout}, nil
}

// commandConn is a net.Conn over a command's standard input and output.
// Deadlines aren't supported; the HTTP client's own timeouts and contexts
// still apply.
type commandConn struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	closeOnce sync.Once
	closeErr  error
}

func (c *commandConn) Read(p []byte) (int, error)  { return c.stdout.Read(p) }
func (c *commandConn) Write(p []byte) (int, error) { return c.stdin.Write(p) }

// Close ends the command: closing its input lets it finish, and it's
// killed if it hasn't within a second.
func (c *commandConn) Close() error {
	c.closeOnce.Do(func() {
		_ = c.stdin.Close()
		done := make(chan error, 1)
		go func() { done <- c.cmd.Wait() }()
		// The command's exit status is ssh's business: the HTTP exchange
		// over it already succeeded or failed on its own.
		select {
		case c.closeErr = <-done:
		case <-time.After(time.Second):
			_ = c.cmd.Process.Kill()
			c.closeErr = <-done
		}
		var exit *exec.ExitError
		if errors.As(c.closeErr, &exit) {
			c.closeErr = nil
		}
	})
	return c.closeErr
}

func (c *commandConn) LocalAddr() net.Addr              { return commandAddr{} }
func (c *commandConn) RemoteAddr() net.Addr             { return commandAddr{} }
func (c *commandConn) SetDeadline(time.Time) error      { return nil }
func (c *commandConn) SetReadDeadline(time.Time) error  { return nil }
func (c *commandConn) SetWriteDeadline(time.Time) error { return nil }

type commandAddr struct{}

func (commandAddr) Network() string { return "command" }
func (commandAddr) String() string  { return "command" }

// DialStdio joins in and out to tined's socket until either side closes:
// the far end of NewSSHClient.
func DialStdio(ctx context.Context, socket string, in io.Reader, out io.Writer) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	uc := conn.(*net.UnixConn)
	sent, answered := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := io.Copy(uc, in)
		_ = uc.CloseWrite() // the request is done; the answer may still come
		sent <- err
	}()
	go func() {
		_, err := io.Copy(out, uc)
		answered <- err
	}()
	select {
	case err := <-answered:
		// tined closed the connection: done, whatever the input does.
		return err
	case err := <-sent:
		if err != nil {
			return err
		}
		select {
		case err := <-answered:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	case <-ctx.Done():
		return ctx.Err()
	}
}
