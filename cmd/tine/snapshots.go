package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sanidhyasin/tine/internal/api"
)

// snapshotTimeout covers snapshotting a large VM, whose memory is written to disk.
const snapshotTimeout = 5 * time.Minute

func newSnapshotCmd(g *globals) *cobra.Command {
	var quiet bool
	cmd := &cobra.Command{
		Use:   "snapshot VM",
		Short: "Save a running VM's memory, devices and disk",
		Long: `Snapshot saves a running VM, so it can be rolled back to later. The VM is
paused while its memory and disk are copied, then carries on.

A snapshot belongs to its VM, and is deleted when nothing needs it any more:
once the VM is destroyed and no VM runs from it.`,
		Example: `  tine snapshot vm-7f3k
  SNAP=$(tine snapshot -q vm-7f3k)`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := g.client()
			ctx, cancel := context.WithTimeout(cmd.Context(), snapshotTimeout)
			defer cancel()
			id, err := resolveID(ctx, c, args[0], false)
			if err != nil {
				return err
			}
			snap, err := c.CreateSnapshot(ctx, id)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			switch {
			case g.json:
				return printJSON(out, snap)
			case quiet:
				_, err = fmt.Fprintln(out, snap.ID)
			default:
				_, err = fmt.Fprintf(out, "%s  of %s  %s  (%s)\n", snap.ID, snap.VMID, formatBytes(snap.SizeBytes), describeSnapshotTimings(snap))
			}
			return err
		},
	}
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "print only the snapshot's ID")
	return cmd
}

// describeSnapshotTimings is e.g. "paused 121ms: memory 118ms · disk 2ms; total 160ms".
func describeSnapshotTimings(s api.Snapshot) string {
	t := s.TimingsMs
	if len(t) == 0 {
		return "no timings"
	}
	var inside []string
	for _, stage := range []string{"memory", "disk"} {
		if v, ok := t[stage]; ok {
			inside = append(inside, stage+" "+formatMillis(v))
		}
	}
	return fmt.Sprintf("paused %s: %s; total %s", formatMillis(t["paused"]), strings.Join(inside, " · "), formatMillis(t["total"]))
}
