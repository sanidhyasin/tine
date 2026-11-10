package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/sanidhyasin/tine/internal/api"
	"github.com/sanidhyasin/tine/internal/version"
)

const (
	// createTimeout covers a cold boot, with room to spare.
	createTimeout = 3 * time.Minute
	// requestTimeout covers every other call.
	requestTimeout = 30 * time.Second
)

var fullID = regexp.MustCompile(`^vm-[a-z2-7]{10}$`)

func newCreateCmd(g *globals) *cobra.Command {
	var (
		req    api.CreateVMRequest
		memory string
		ttl    time.Duration
		quiet  bool
	)
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a VM and wait until it's running",
		Long: `Create starts a VM and waits until it's ready to run commands.

If the image has a template of that size (see 'tine image warm'), the VM is
restored from it, in milliseconds. Otherwise, or with --cold, it boots.

--network none leaves the VM with loopback only: no internet, nothing beyond
itself. Commands and files still reach it, through its agent.`,
		Example: `  tine create
  tine create --vcpus 4 --memory 2G --ttl 30m
  tine create --cold
  tine create --network none
  ID=$(tine create -q)`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if memory != "" {
				mib, err := parseMemory(memory)
				if err != nil {
					return err
				}
				req.MemoryMiB = mib
			}
			req.TTLSeconds = int(ttl.Seconds())
			ctx, cancel := context.WithTimeout(cmd.Context(), createTimeout)
			defer cancel()
			vm, err := g.client().CreateVM(ctx, req)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			switch {
			case g.json:
				return printJSON(out, vm)
			case quiet:
				_, err = fmt.Fprintln(out, vm.ID)
			default:
				_, err = fmt.Fprintf(out, "%s  %s  (%s in %s: %s)\n", vm.ID, vm.State, describeStart(vm),
					formatMillis(vm.TimingsMs["total"]), describeTimings(vm.TimingsMs, vm.Source))
			}
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&req.Image, "image", "", "image to boot (default: tined's, normally base)")
	f.IntVar(&req.VCPUs, "vcpus", 0, "vCPUs: 1 or an even number (default: tined's, normally 2)")
	f.StringVar(&memory, "memory", "", `memory, e.g. "512M" or "2G" (default: tined's, normally 1G)`)
	f.DurationVar(&ttl, "ttl", 0, "destroy the VM automatically after this long (default: tined's, normally 1h)")
	f.BoolVarP(&quiet, "quiet", "q", false, "print only the VM's ID")
	f.BoolVar(&req.Cold, "cold", false, "boot the VM, even if its image has a template to start from")
	f.StringVar(&req.Strategy, "strategy", "", `how a VM restored from a template gets its memory: "share" (the default), "copy", "uffd" or "uffd-prefetch"`)
	f.StringVar(&req.Network, "network", "", `"default" (its own network, with the internet) or "none" (loopback only)`)
	return cmd
}

func newListCmd(g *globals) *cobra.Command {
	var all, tree bool
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List VMs",
		Long: `List the VMs that aren't destroyed, oldest first.

--tree shows forks: each VM, the snapshots it was forked from, and the
children of each, with how much host memory each uses. PSS splits pages that
VMs share between them, so a fork's children each have a PSS well below
their RSS.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), requestTimeout)
			defer cancel()
			vms, err := g.client().ListVMsWith(ctx, api.ListOptions{All: all, Memory: tree})
			if err != nil {
				return err
			}
			sort.Slice(vms, func(i, j int) bool { return vms[i].CreatedAt.Before(vms[j].CreatedAt) })
			if g.json {
				return printJSON(cmd.OutOrStdout(), vms)
			}
			if tree {
				return printTree(cmd.OutOrStdout(), vms)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "ID\tSTATE\tIMAGE\tVCPUS\tMEMORY\tAGE\tEXPIRES IN")
			now := time.Now()
			for _, vm := range vms {
				expires := "-"
				if vm.ExpiresAt != nil && vm.State != "destroyed" {
					expires = humanDuration(vm.ExpiresAt.Sub(now))
				}
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n", vm.ID, vm.State, vm.Image, vm.VCPUs,
					formatMiB(vm.MemoryMiB), humanDuration(now.Sub(vm.CreatedAt)), expires)
			}
			return w.Flush()
		},
	}
	cmd.Flags().BoolVarP(&all, "all", "a", false, "include destroyed VMs")
	cmd.Flags().BoolVarP(&tree, "tree", "t", false, "show forks as a tree, with each VM's memory")
	return cmd
}

// printTree prints VMs as a tree: each VM, then each snapshot that VMs
// were forked from, then those VMs, and so on down.
func printTree(out io.Writer, vms []api.VM) error {
	byID := map[string]bool{}
	for _, v := range vms {
		byID[v.ID] = true
	}
	// children[parent][snapshot] are the VMs forked from parent through
	// snapshot, oldest first (vms is sorted).
	children := map[string]map[string][]api.VM{}
	var snapOrder = map[string][]string{}
	var roots []api.VM
	for _, v := range vms {
		if v.ParentID == "" || !byID[v.ParentID] {
			roots = append(roots, v)
			continue
		}
		if children[v.ParentID] == nil {
			children[v.ParentID] = map[string][]api.VM{}
		}
		if len(children[v.ParentID][v.SnapshotID]) == 0 {
			snapOrder[v.ParentID] = append(snapOrder[v.ParentID], v.SnapshotID)
		}
		children[v.ParentID][v.SnapshotID] = append(children[v.ParentID][v.SnapshotID], v)
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "VM\tSTATE\tIMAGE\tMEMORY\tRSS\tPSS\tAGE")
	now := time.Now()
	var walk func(v api.VM, lead, branch string)
	walk = func(v api.VM, lead, branch string) {
		rss, pss := "-", "-"
		if v.Memory != nil {
			rss, pss = formatBytes(v.Memory.RSSBytes), formatBytes(v.Memory.PSSBytes)
		}
		name := v.ID
		if v.ParentID != "" && !byID[v.ParentID] {
			name += " (forked from " + v.ParentID + ")"
		}
		_, _ = fmt.Fprintf(w, "%s%s\t%s\t%s\t%s\t%s\t%s\t%s\n", lead+branch, name, v.State, v.Image,
			formatMiB(v.MemoryMiB), rss, pss, humanDuration(now.Sub(v.CreatedAt)))
		under := lead + map[string]string{"": "", "├─ ": "│  ", "└─ ": "   "}[branch]
		snaps := snapOrder[v.ID]
		for i, snapID := range snaps {
			snapBranch, snapUnder := "├─ ", under+"│  "
			if i == len(snaps)-1 {
				snapBranch, snapUnder = "└─ ", under+"   "
			}
			kids := children[v.ID][snapID]
			_, _ = fmt.Fprintf(w, "%s%s (%d forked)\t\t\t\t\t\t\n", under+snapBranch, snapID, len(kids))
			for j, kid := range kids {
				b := "├─ "
				if j == len(kids)-1 {
					b = "└─ "
				}
				walk(kid, snapUnder, b)
			}
		}
	}
	for _, r := range roots {
		walk(r, "", "")
	}
	return w.Flush()
}

func newInspectCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect VM",
		Short: "Show everything about a VM, as JSON",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), requestTimeout)
			defer cancel()
			c := g.client()
			id, err := resolveID(ctx, c, args[0], true)
			if err != nil {
				return err
			}
			vm, err := c.GetVM(ctx, id)
			if err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), vm)
		},
	}
}

func newDestroyCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:     "destroy VM...",
		Aliases: []string{"rm"},
		Short:   "Destroy VMs",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := g.client()
			var (
				destroyed []api.VM
				failures  int
			)
			for _, arg := range args {
				ctx, cancel := context.WithTimeout(cmd.Context(), requestTimeout)
				vm, err := func() (api.VM, error) {
					id, err := resolveID(ctx, c, arg, false)
					if err != nil {
						return api.VM{}, err
					}
					return c.DestroyVM(ctx, id)
				}()
				cancel()
				if err != nil {
					failures++
					_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "tine: %s: %s\n", arg, friendly(err))
					continue
				}
				destroyed = append(destroyed, vm)
				if !g.json {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "destroyed %s (%s)\n", vm.ID, formatMillis(vm.TimingsMs["destroy"]))
				}
			}
			if g.json {
				if err := printJSON(cmd.OutOrStdout(), destroyed); err != nil {
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

func newImagesCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "images",
		Short: "List the images VMs can boot",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), requestTimeout)
			defer cancel()
			imgs, err := g.client().Images(ctx)
			if err != nil {
				return err
			}
			if g.json {
				return printJSON(cmd.OutOrStdout(), imgs)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "NAME\tKERNEL\tDISK\tAGENT\tCREATED\tTEMPLATES")
			for _, img := range imgs {
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s ago\t%s\n", img.Name, img.Kernel, formatBytes(img.DiskBytes),
					img.AgentVersion, humanDuration(time.Since(img.CreatedAt)), describeTemplates(img.Templates))
			}
			return w.Flush()
		},
	}
}

func newVersionCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the versions of tine and tined",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			defer cancel()
			h, healthErr := g.client().Health(ctx)
			if g.json {
				out := map[string]any{"tine": version.Version}
				if healthErr == nil {
					out["tined"] = h.Version
				}
				return printJSON(cmd.OutOrStdout(), out)
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "tine", version.String())
			if healthErr != nil {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), "tined: not reachable:", strings.SplitN(friendly(healthErr), "\n", 2)[0])
				return err
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "tined %s (up %s, %d VMs)\n", h.Version, humanDuration(time.Duration(h.UptimeSeconds*float64(time.Second))), h.VMs)
			return err
		},
	}
}

// resolveID turns a full VM ID or a unique prefix of one ("vm-7f3k", or just
// "7f3k") into the full ID. includeDestroyed widens the search to destroyed
// VMs, for commands that only read.
func resolveID(ctx context.Context, c *api.Client, arg string, includeDestroyed bool) (string, error) {
	if fullID.MatchString(arg) {
		return arg, nil
	}
	vms, err := c.ListVMs(ctx, includeDestroyed)
	if err != nil {
		return "", err
	}
	prefix := arg
	if !strings.HasPrefix(prefix, "vm-") {
		prefix = "vm-" + prefix
	}
	var matches []string
	for _, vm := range vms {
		if strings.HasPrefix(vm.ID, prefix) {
			matches = append(matches, vm.ID)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("no VM matches %q", arg)
	default:
		sort.Strings(matches)
		return "", errors.New("several VMs match " + fmt.Sprintf("%q", arg) + ": " + strings.Join(matches, ", "))
	}
}
