package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/sanidhyasin/tine/internal/image"
)

func newImageCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "image",
		Short: "Build and list the images VMs boot from",
	}
	cmd.AddCommand(newImageImportCmd(), newImageListCmd())
	return cmd
}

func newImageImportCmd() *cobra.Command {
	var (
		opts image.ImportOptions
		size string
	)
	cmd := &cobra.Command{
		Use:   "import NAME",
		Short: "Build an image from a guest kernel and an Ubuntu squashfs, with tine-agent installed",
		Long: `Build an image from a guest kernel and an Ubuntu root filesystem (a squashfs,
like the ones scripts/host-setup.sh downloads from Firecracker's CI), install
tine-agent in it, and store it in the images directory. Needs root.

Example, on the dev server:

  sudo tined image import base \
    --kernel ~/tine/artifacts/vmlinux-6.18.56 \
    --rootfs ~/tine/artifacts/ubuntu-24.04.squashfs`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.Name = args[0]
			n, err := parseSize(size)
			if err != nil {
				return err
			}
			opts.SizeBytes = n
			opts.Log = slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), nil))
			img, err := image.Import(cmd.Context(), opts)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "imported %s in %.1fs: kernel %s, %s disk from %s, agent %s\n  %s\n",
				img.Name, img.Meta.BuildSeconds, img.Meta.Kernel.Source, formatSize(img.Meta.Rootfs.SizeBytes),
				img.Meta.Rootfs.Source, orUnknown(img.Meta.Agent.Version), img.Dir)
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&opts.Kernel, "kernel", "", "uncompressed guest kernel (vmlinux)")
	f.StringVar(&opts.RootfsSquashfs, "rootfs", "", "guest root filesystem, as a squashfs")
	f.StringVar(&opts.Agent, "agent", defaultAgentPath(), "tine-agent binary to install, built for linux/amd64")
	f.StringVar(&size, "size", "4G", `disk size, e.g. "4G" or "512M" (sparse: unused space costs nothing)`)
	f.StringVar(&opts.ImagesDir, "images-dir", image.DefaultImagesDir, "where images are stored")
	f.BoolVar(&opts.Force, "force", false, "replace an existing image of the same name")
	_ = cmd.MarkFlagRequired("kernel")
	_ = cmd.MarkFlagRequired("rootfs")
	return cmd
}

func newImageListCmd() *cobra.Command {
	var imagesDir string
	cmd := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List images",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			images, err := image.List(imagesDir)
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "NAME\tCREATED\tKERNEL\tDISK\tAGENT")
			for _, img := range images {
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", img.Name, img.Meta.CreatedAt.Local().Format(time.DateTime),
					img.Meta.Kernel.Source, formatSize(img.Meta.Rootfs.SizeBytes), orUnknown(img.Meta.Agent.Version))
			}
			return w.Flush()
		},
	}
	cmd.Flags().StringVar(&imagesDir, "images-dir", image.DefaultImagesDir, "where images are stored")
	return cmd
}

// defaultAgentPath is the tine-agent installed next to tined, as `make deploy` does.
func defaultAgentPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "/usr/local/bin/tine-agent"
	}
	return filepath.Join(filepath.Dir(exe), "tine-agent")
}

// parseSize reads sizes like "4G", "512M", "1.5G" or a plain number of bytes.
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	mult := int64(1)
	for suffix, m := range map[string]int64{"K": 1 << 10, "M": 1 << 20, "G": 1 << 30, "T": 1 << 40} {
		if strings.HasSuffix(s, suffix) || strings.HasSuffix(s, suffix+"B") || strings.HasSuffix(s, suffix+"IB") {
			s = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(s, "IB"), "B"), suffix)
			mult = m
			break
		}
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("size %q: want something like 4G, 512M or a number of bytes", s)
	}
	return int64(v * float64(mult)), nil
}

func formatSize(n int64) string {
	switch {
	case n >= 1<<30:
		return strconv.FormatFloat(float64(n)/(1<<30), 'f', -1, 64) + " GiB"
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', -1, 64) + " MiB"
	default:
		return strconv.FormatInt(n, 10) + " bytes"
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "(unknown)"
	}
	return s
}
