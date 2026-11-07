// Command tine is the command-line client for tined, the tine service.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/sanidhyasin/tine/internal/api"
	"github.com/sanidhyasin/tine/internal/version"
)

func main() {
	// Ctrl+C cancels the request; for exec, tined then stops the command too.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

// run runs tine with args and returns its exit code.
func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	root := newRootCmd()
	root.SetArgs(args)
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)
	err := root.ExecuteContext(ctx)
	if err != nil && ctx.Err() != nil {
		// Interrupted (Ctrl+C): exit like a shell does for SIGINT.
		_, _ = fmt.Fprintln(stderr, "tine: interrupted")
		return 130
	}
	return exitCode(err, stderr)
}

// globals are the flags every command shares.
type globals struct {
	socket string
	json   bool
}

func (g *globals) client() *api.Client { return api.NewUnixClient(g.socket) }

func newRootCmd() *cobra.Command {
	g := &globals{}
	root := &cobra.Command{
		Use:   "tine",
		Short: "Fork running microVMs in milliseconds",
		Long: `tine creates, runs commands in and destroys Firecracker microVMs, through
tined, the tine service on this host.

VM IDs can be shortened to any unique prefix, like docker's.`,
		Example: `  tine create
  tine exec vm-7f3k -- uname -r
  tine snapshot vm-7f3k
  tine ls
  tine destroy vm-7f3k`,
		Version:       version.String(),
		SilenceUsage:  true,
		SilenceErrors: true, // exitCode prints them
	}
	socket := os.Getenv("TINE_SOCKET")
	if socket == "" {
		socket = api.DefaultSocket
	}
	root.PersistentFlags().StringVar(&g.socket, "socket", socket, "tined's API socket (or set TINE_SOCKET)")
	root.PersistentFlags().BoolVar(&g.json, "json", false, "print JSON, for scripts")
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return fmt.Errorf("%w (see 'tine %s --help')", err, cmd.Name())
	})
	root.AddCommand(
		newCreateCmd(g), newListCmd(g), newInspectCmd(g), newExecCmd(g), newDestroyCmd(g),
		newSnapshotCmd(g), newRollbackCmd(g), newImagesCmd(g), newImageCmd(g), newVersionCmd(g),
	)
	return root
}

// exitError ends tine with a specific exit code, like the code of a command
// run by tine exec. msg, if set, is printed first.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

// exitCode prints err (if any) the way a person wants to read it, and
// returns tine's exit code.
func exitCode(err error, stderr io.Writer) int {
	if err == nil {
		return 0
	}
	var ee *exitError
	if errors.As(err, &ee) {
		if ee.msg != "" {
			_, _ = fmt.Fprintln(stderr, "tine:", ee.msg)
		}
		return ee.code
	}
	_, _ = fmt.Fprintln(stderr, "tine:", friendly(err))
	return 1
}

// friendly turns errors into advice where it can.
func friendly(err error) string {
	var apiErr *api.Error
	switch {
	case errors.As(err, &apiErr):
		return apiErr.Message
	case errors.Is(err, syscall.ENOENT), errors.Is(err, syscall.ECONNREFUSED):
		return "tined isn't running (" + err.Error() + ").\n  Start it on the host with: sudo systemctl start tined"
	case errors.Is(err, syscall.EACCES):
		return "permission denied on tined's socket.\n  Join the tine group (sudo usermod -aG tine $USER), then log in again; or use sudo"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	}
	return err.Error()
}
