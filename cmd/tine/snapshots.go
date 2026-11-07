package main

import (
	"context"
	"errors"
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

func newRollbackCmd(g *globals) *cobra.Command {
	var to string
	cmd := &cobra.Command{
		Use:   "rollback VM --to SNAPSHOT",
		Short: "Put a VM back the way it was when a snapshot was taken",
		Long: `Rollback replaces a running VM's memory, devices and disk with a snapshot's.
Everything since the snapshot is lost. The VM keeps its ID, its network and
its hostname.

The snapshot must be one taken of this VM, or the template it started from.`,
		Example: `  SNAP=$(tine snapshot -q vm-7f3k)
  tine exec vm-7f3k -- rm -rf /important
  tine rollback vm-7f3k --to $SNAP`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if to == "" {
				return errors.New("say which snapshot to roll back to, with --to")
			}
			c := g.client()
			ctx, cancel := context.WithTimeout(cmd.Context(), createTimeout)
			defer cancel()
			id, err := resolveID(ctx, c, args[0], false)
			if err != nil {
				return err
			}
			vm, err := c.Rollback(ctx, id, to)
			if err != nil {
				return err
			}
			if g.json {
				return printJSON(cmd.OutOrStdout(), vm)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "rolled back %s to %s (%s)\n", vm.ID, vm.SnapshotID, formatMillis(vm.TimingsMs["rollback"]))
			return err
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "the snapshot to roll back to")
	return cmd
}
