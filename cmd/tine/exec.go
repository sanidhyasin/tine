package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sanidhyasin/tine/internal/agentproto"
	"github.com/sanidhyasin/tine/internal/api"
)

// maxStdinBytes matches what tined accepts in one exec request.
const maxStdinBytes = 15 << 20

// signalNumbers maps the signal names the agent reports to their numbers,
// for the shell convention of exiting with 128 + the signal's number.
var signalNumbers = map[string]int{
	"hangup": 1, "interrupt": 2, "quit": 3, "illegal instruction": 4, "aborted": 6,
	"bus error": 7, "floating point exception": 8, "killed": 9, "segmentation fault": 11,
	"broken pipe": 13, "alarm clock": 14, "terminated": 15,
}

func newExecCmd(g *globals) *cobra.Command {
	var (
		env     []string
		dir     string
		timeout time.Duration
		stdin   bool
	)
	cmd := &cobra.Command{
		Use:   "exec [flags] VM [--] COMMAND [ARG...]",
		Short: "Run a command in a VM",
		Long: `Run a command in a VM. Its output streams back as it runs, and tine exits with
the command's exit code (128 + the signal's number if a signal killed it, or
124 if it timed out). Ctrl+C stops the command in the VM too.

Flags go before the VM. The command isn't run through a shell; for pipes or
redirects, run one: tine exec VM -- sh -c 'ls | wc -l'`,
		Example: `  tine exec vm-7f3k uname -r
  tine exec -e DEBUG=1 -w /tmp vm-7f3k -- python3 script.py
  echo hello | tine exec -i vm-7f3k cat`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			command := args[1:]
			// Flag parsing stops at the VM, so a "--" after it arrives as an argument.
			if command[0] == "--" {
				command = command[1:]
			}
			if len(command) == 0 {
				return fmt.Errorf("no command to run (see 'tine exec --help')")
			}
			req := api.ExecRequest{Args: command, Env: env, Dir: dir}
			if timeout > 0 {
				req.TimeoutMs = timeout.Milliseconds()
			}
			if stdin {
				data, err := io.ReadAll(io.LimitReader(cmd.InOrStdin(), maxStdinBytes+1))
				if err != nil {
					return fmt.Errorf("read stdin: %w", err)
				}
				if len(data) > maxStdinBytes {
					return fmt.Errorf("stdin is over %d MiB; copy large files in another way", maxStdinBytes>>20)
				}
				req.Stdin = data
			}

			ctx := cmd.Context()
			c := g.client()
			lookupCtx, cancel := context.WithTimeout(ctx, requestTimeout)
			id, err := resolveID(lookupCtx, c, args[0], false)
			cancel()
			if err != nil {
				return err
			}

			stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
			var collected struct{ stdout, stderr strings.Builder }
			onOutput := func(stream string, data []byte) {
				switch {
				case g.json && stream == agentproto.StreamStderr:
					collected.stderr.Write(data)
				case g.json:
					collected.stdout.Write(data)
				case stream == agentproto.StreamStderr:
					_, _ = stderr.Write(data)
				default:
					_, _ = stdout.Write(data)
				}
			}
			st, err := c.Exec(ctx, id, req, onOutput)
			if err != nil {
				return err
			}
			if g.json {
				if err := printJSON(stdout, map[string]any{
					"stdout": collected.stdout.String(), "stderr": collected.stderr.String(), "exit": st,
				}); err != nil {
					return err
				}
			}
			return exitFor(st, timeout)
		},
	}
	f := cmd.Flags()
	// Everything after the VM belongs to the command, even if it looks like
	// a flag: tine exec vm-7f3k ls -la works without "--".
	f.SetInterspersed(false)
	f.StringArrayVarP(&env, "env", "e", nil, "set an environment variable, KEY=VALUE (repeatable)")
	f.StringVarP(&dir, "workdir", "w", "", "working directory in the VM (default /root)")
	f.DurationVar(&timeout, "timeout", 0, "kill the command after this long (default: no limit)")
	f.BoolVarP(&stdin, "stdin", "i", false, "send tine's standard input to the command")
	return cmd
}

// exitFor turns a command's exit status into tine's exit, following the
// shell's conventions.
func exitFor(st api.ExitStatus, timeout time.Duration) error {
	var warning string
	if st.OutputTruncated {
		warning = "output was cut off at the agent's limit; "
	}
	switch {
	case st.TimedOut:
		return &exitError{code: 124, msg: fmt.Sprintf("%scommand timed out after %s and was killed", warning, timeout)}
	case st.Signal != "":
		code := 128 + signalNumbers[st.Signal]
		if signalNumbers[st.Signal] == 0 {
			code = 255
		}
		return &exitError{code: code, msg: warning + "command was killed by signal: " + st.Signal}
	case st.Code != 0:
		return &exitError{code: st.Code, msg: strings.TrimSuffix(warning, "; ")}
	case warning != "":
		return &exitError{code: 0, msg: strings.TrimSuffix(warning, "; ")}
	}
	return nil
}
