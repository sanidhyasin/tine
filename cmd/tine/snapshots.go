package main

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/sanidhyasin/tine/internal/api"
)

// snapshotTimeout covers snapshotting a large VM, whose memory is written to disk.
const snapshotTimeout = 5 * time.Minute

func newSnapshotCmd(g *globals) *cobra.Command {
	var quiet, retain bool
	cmd := &cobra.Command{
		Use:   "snapshot VM",
		Short: "Save a running VM's memory, devices and disk; list and delete snapshots",
		Long: `Snapshot saves a running VM, so it can be rolled back to later. The VM is
paused while its memory and disk are copied, then carries on.

A snapshot belongs to its VM, and is deleted when nothing needs it any more:
once the VM is destroyed and no VM runs from it. With --retain, it's kept until
'tine snapshot rm', even once the VM is gone, to fork from later with
'tine fork SNAPSHOT'.

Snapshot IDs, like VM IDs, can be shortened to any unique prefix.`,
		Example: `  tine snapshot vm-7f3k
  SNAP=$(tine snapshot -q vm-7f3k)
  tine snapshot --retain vm-7f3k
  tine snapshot ls
  tine snapshot rm snap-q2w9`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := g.client()
			ctx, cancel := context.WithTimeout(cmd.Context(), snapshotTimeout)
			defer cancel()
			id, err := resolveID(ctx, c, args[0], false)
			if err != nil {
				return err
			}
			snap, err := c.CreateSnapshotWith(ctx, id, api.SnapshotRequest{Retain: retain})
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
				kept := ""
				if snap.Retained {
					kept = "  retained"
				}
				_, err = fmt.Fprintf(out, "%s  of %s  %s%s  (%s)\n", snap.ID, snap.VMID, formatBytes(snap.SizeBytes), kept, describeSnapshotTimings(snap))
			}
			return err
		},
	}
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "print only the snapshot's ID")
	cmd.Flags().BoolVar(&retain, "retain", false, "keep the snapshot until 'tine snapshot rm', even once the VM is destroyed")
	cmd.AddCommand(newSnapshotListCmd(g), newSnapshotInspectCmd(g), newSnapshotRemoveCmd(g))
	return cmd
}

func newSnapshotListCmd(g *globals) *cobra.Command {
	var f api.SnapshotFilter
	var vmArg string
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List snapshots and templates",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c := g.client()
			ctx, cancel := context.WithTimeout(cmd.Context(), requestTimeout)
			defer cancel()
			if vmArg != "" {
				id, err := resolveID(ctx, c, vmArg, true)
				if err != nil {
					return err
				}
				f.VMID = id
			}
			snaps, err := c.ListSnapshots(ctx, f)
			if err != nil {
				return err
			}
			if g.json {
				return printJSON(cmd.OutOrStdout(), snaps)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "ID\tKIND\tVM\tIMAGE\tVCPUS\tMEMORY\tDISK\tAGE\tPAUSED\tUSED BY")
			for _, s := range snaps {
				kind := s.Kind
				var notes []string
				if s.Retained {
					notes = append(notes, "retained")
				}
				if s.State != "ready" {
					notes = append(notes, s.State)
				}
				if len(notes) > 0 {
					kind += " (" + strings.Join(notes, ", ") + ")"
				}
				paused := "-"
				if v, ok := s.TimingsMs["paused"]; ok {
					paused = formatMillis(v)
				}
				usedBy := "-"
				if len(s.UsedBy) > 0 {
					usedBy = strings.Join(s.UsedBy, ",")
				}
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\t%s\t%s\t%s\n", s.ID, kind, s.VMID, s.Image, s.VCPUs,
					formatMiB(s.MemoryMiB), formatBytes(s.SizeBytes), humanDuration(time.Since(s.CreatedAt)), paused, usedBy)
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&vmArg, "vm", "", "only this VM's snapshots")
	cmd.Flags().StringVar(&f.Kind, "kind", "", `only "vm" snapshots or "template"s`)
	return cmd
}

func newSnapshotInspectCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect SNAPSHOT",
		Short: "Show everything about a snapshot, as JSON",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := g.client()
			ctx, cancel := context.WithTimeout(cmd.Context(), requestTimeout)
			defer cancel()
			id, err := resolveSnapshotID(ctx, c, args[0])
			if err != nil {
				return err
			}
			snap, err := c.GetSnapshot(ctx, id)
			if err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), snap)
		},
	}
}

func newSnapshotRemoveCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:     "rm SNAPSHOT...",
		Aliases: []string{"delete"},
		Short:   "Delete snapshots that no VM runs from",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := g.client()
			failures := 0
			var deleted []api.Snapshot
			for _, arg := range args {
				ctx, cancel := context.WithTimeout(cmd.Context(), requestTimeout)
				snap, err := func() (api.Snapshot, error) {
					id, err := resolveSnapshotID(ctx, c, arg)
					if err != nil {
						return api.Snapshot{}, err
					}
					return c.DeleteSnapshot(ctx, id)
				}()
				cancel()
				if err != nil {
					failures++
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "tine: %s: %s\n", arg, friendly(err))
					continue
				}
				deleted = append(deleted, snap)
				if !g.json {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "deleted %s\n", snap.ID)
				}
			}
			if g.json {
				if err := printJSON(cmd.OutOrStdout(), deleted); err != nil {
					return err
				}
			}
			if failures > 0 {
				return &exitError{code: 1}
			}
			return nil
		},
	}
}

// resolveSnapshotID turns a unique prefix of a snapshot's ID, with or
// without "snap-", into its ID.
func resolveSnapshotID(ctx context.Context, c *api.Client, arg string) (string, error) {
	if fullSnapshotID.MatchString(arg) {
		return arg, nil
	}
	snaps, err := c.ListSnapshots(ctx, api.SnapshotFilter{})
	if err != nil {
		return "", err
	}
	prefix := arg
	if !strings.HasPrefix(prefix, "snap-") {
		prefix = "snap-" + prefix
	}
	var matches []string
	for _, s := range snaps {
		if strings.HasPrefix(s.ID, prefix) {
			matches = append(matches, s.ID)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("no snapshot matches %q", arg)
	default:
		sort.Strings(matches)
		return "", fmt.Errorf("several snapshots match %q: %s", arg, strings.Join(matches, ", "))
	}
}

var fullSnapshotID = regexp.MustCompile(`^snap-[a-z2-7]{10}$`)

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
			snapID, err := resolveSnapshotID(ctx, c, to)
			if err != nil {
				return err
			}
			vm, err := c.Rollback(ctx, id, snapID)
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
