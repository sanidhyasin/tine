package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sanidhyasin/tine/internal/api"
)

func newImageCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "image",
		Short: "List images, and warm their templates",
	}
	ls := newImagesCmd(g)
	ls.Use, ls.Aliases = "ls", []string{"list"}
	cmd.AddCommand(ls, newWarmCmd(g))
	return cmd
}

func newWarmCmd(g *globals) *cobra.Command {
	var (
		req    api.WarmRequest
		memory string
	)
	cmd := &cobra.Command{
		Use:   "warm [IMAGE]",
		Short: "Boot an image once and save it, so new VMs start from it",
		Long: `Warm boots an image, waits until it has finished booting, and saves it as a
template. From then on, 'tine create' restores VMs of that image and size from
the template instead of booting them. Warming again replaces the template.

tined keeps the templates of the images it's told to (--warm-images, by
default "base") usable by itself, warming them again after an upgrade or after
the image is imported again.`,
		Example: `  tine image warm
  tine image warm base --vcpus 4 --memory 4G`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := "base"
			if len(args) == 1 {
				name = args[0]
			}
			if memory != "" {
				mib, err := parseMemory(memory)
				if err != nil {
					return err
				}
				req.MemoryMiB = mib
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), createTimeout+snapshotTimeout)
			defer cancel()
			snap, err := g.client().WarmTemplate(ctx, name, req)
			if err != nil {
				return err
			}
			if g.json {
				return printJSON(cmd.OutOrStdout(), snap)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s  template of %s, %d vCPUs, %s  (%s, paused %s)\n",
				snap.ID, snap.Image, snap.VCPUs, formatMiB(snap.MemoryMiB), formatBytes(snap.SizeBytes), formatMillis(snap.TimingsMs["paused"]))
			return err
		},
	}
	f := cmd.Flags()
	f.IntVar(&req.VCPUs, "vcpus", 0, "the size of VMs the template is for (default: tined's, normally 2)")
	f.StringVar(&memory, "memory", "", `the memory of VMs it's for, e.g. "4G" (default: tined's, normally 1G)`)
	return cmd
}

// describeTemplates is e.g. "2 vCPUs 1 GiB, 4 vCPUs 4 GiB (out of date)".
func describeTemplates(ts []api.Template) string {
	if len(ts) == 0 {
		return "-"
	}
	parts := make([]string, 0, len(ts))
	for _, t := range ts {
		p := fmt.Sprintf("%d vCPUs %s", t.VCPUs, formatMiB(t.MemoryMiB))
		if t.Problem != "" {
			p += " (unusable)"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, ", ")
}
