package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sanidhyasin/tine/internal/api"
)

func newTopCmd(g *globals) *cobra.Command {
	var (
		interval time.Duration
		count    int
	)
	cmd := &cobra.Command{
		Use:   "top",
		Short: "Watch VMs and their forks, live",
		Long: `Show every VM as a tree of forks, with its memory, redrawn every second:
running VMs as they are, new ones yellow, failed ones red. Ctrl+C quits.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			color := isTerminal(out)
			for i := 0; count == 0 || i < count; i++ {
				if i > 0 {
					select {
					case <-cmd.Context().Done():
						return nil
					case <-time.After(interval):
					}
				}
				ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
				vms, err := g.client().ListVMsWith(ctx, api.ListOptions{Memory: true})
				cancel()
				if errors.Is(err, context.Canceled) {
					return nil
				}
				var frame bytes.Buffer
				if err != nil {
					fmt.Fprintf(&frame, "tine top: %v\n", err)
				} else if err := topFrame(&frame, vms, color); err != nil {
					return err
				}
				if color {
					_, _ = fmt.Fprint(out, "\x1b[H\x1b[2J") // home, clear
				}
				if _, err := out.Write(frame.Bytes()); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().DurationVar(&interval, "interval", time.Second, "how often to redraw")
	cmd.Flags().IntVarP(&count, "count", "n", 0, "redraw this many times, then quit (0: until Ctrl+C)")
	return cmd
}

// topFrame is one screen: a summary line, then the tree.
func topFrame(w *bytes.Buffer, vms []api.VM, color bool) error {
	states := map[string]int{}
	var pss int64
	for _, v := range vms {
		states[v.State]++
		if v.Memory != nil {
			pss += v.Memory.PSSBytes
		}
	}
	fmt.Fprintf(w, "tine top · %s · %d VMs: %d running", time.Now().Format("15:04:05"), len(vms), states["running"])
	for _, s := range []string{"creating", "failed"} {
		if states[s] > 0 {
			fmt.Fprintf(w, ", %d %s", states[s], s)
		}
	}
	fmt.Fprintf(w, " · host memory (PSS) %s\n\n", formatBytes(pss))
	var tree bytes.Buffer
	if err := printTree(&tree, vms); err != nil {
		return err
	}
	for _, line := range strings.SplitAfter(tree.String(), "\n") {
		w.WriteString(colorLine(line, color))
	}
	return nil
}

// colorLine colors a line of the tree by its VM's state, once tabwriter
// has laid it out (escapes inside would throw the columns off).
func colorLine(line string, color bool) string {
	if !color || line == "" {
		return line
	}
	code := ""
	switch {
	case strings.Contains(line, "  failed  "):
		code = "31" // red
	case strings.Contains(line, "  creating  "):
		code = "33" // yellow
	case strings.Contains(line, " forked)"):
		code = "2" // dim: a snapshot, not a VM
	}
	if code == "" {
		return line
	}
	return "\x1b[" + code + "m" + strings.TrimSuffix(line, "\n") + "\x1b[0m" + line[len(strings.TrimSuffix(line, "\n")):]
}

func isTerminal(w any) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
