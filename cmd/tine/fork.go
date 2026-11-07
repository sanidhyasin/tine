package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/sanidhyasin/tine/internal/api"
)

func newForkCmd(g *globals) *cobra.Command {
	var (
		count int
		ttl   time.Duration
		quiet bool
	)
	cmd := &cobra.Command{
		Use:   "fork VM -n COUNT",
		Short: "Make running copies of a VM that share its memory",
		Long: `Fork snapshots a running VM and restores the snapshot COUNT times. The VM is
paused only while the snapshot is taken, and carries on as before.

Each child starts exactly where the VM was, programs and all, with its own ID,
network, disk, hostname and machine ID. The children share the VM's memory as
it was until they change it ('tine ls --tree' shows how much).`,
		Example: `  tine fork vm-7f3k -n 5
  for id in $(tine fork -q vm-7f3k -n 10); do tine exec $id -- ./try.sh & done`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := g.client()
			ctx, cancel := context.WithTimeout(cmd.Context(), snapshotTimeout+time.Duration(count)*10*time.Second)
			defer cancel()
			id, err := resolveID(ctx, c, args[0], false)
			if err != nil {
				return err
			}
			res, err := c.Fork(ctx, id, api.ForkRequest{Count: count, TTLSeconds: int(ttl.Seconds())})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			failed := 0
			for _, ch := range res.Children {
				if ch.Error != nil {
					failed++
				}
			}
			switch {
			case g.json:
				if err := printJSON(out, res); err != nil {
					return err
				}
			case quiet:
				for _, ch := range res.Children {
					if ch.VM != nil {
						_, _ = fmt.Fprintln(out, ch.VM.ID)
					}
				}
			default:
				t := res.TimingsMs
				_, _ = fmt.Fprintf(out, "forked %s into %d in %s (parent paused %s; snapshot %s · children %s)\n",
					id, len(res.Children)-failed, formatMillis(t["total"]), formatMillis(t["paused"]),
					formatMillis(t["snapshot"]), formatMillis(t["children"]))
				for _, ch := range res.Children {
					if ch.VM != nil {
						_, _ = fmt.Fprintf(out, "  %s  %s  (%s)\n", ch.VM.ID, ch.VM.State, formatMillis(ch.VM.TimingsMs["total"]))
					}
				}
			}
			for _, ch := range res.Children {
				if ch.Error != nil {
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "tine: a child couldn't be made: %s\n", ch.Error.Message)
				}
			}
			if failed > 0 {
				return &exitError{code: 1, msg: fmt.Sprintf("%d of %d children couldn't be made", failed, len(res.Children))}
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.IntVarP(&count, "count", "n", 1, "how many children")
	f.DurationVar(&ttl, "ttl", 0, "how long each child lives (default: tined's, normally 1h)")
	f.BoolVarP(&quiet, "quiet", "q", false, "print only the children's IDs, one a line")
	return cmd
}
