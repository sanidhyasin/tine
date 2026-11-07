package main

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sanidhyasin/tine/internal/api"
)

func newCpCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cp SRC DST",
		Short: "Copy files into and out of a VM",
		Long: `Copy a file or a directory into a VM, or a file out of one. One side is
VM:PATH, with PATH absolute in the VM; the other a local path, or "-" for
stdin or stdout.

A directory is sent as a tar stream and unpacked into PATH, which it becomes:
"tine cp ./repo VM:/root/repo" puts repo's contents in /root/repo. Copying
out of a VM takes one file.`,
		Example: `  tine cp ./main.py vm-7f3k:/root/main.py
  tine cp ./repo vm-7f3k:/root/repo
  tine cp vm-7f3k:/root/result.json .
  echo hello | tine cp - vm-7f3k:/tmp/hello.txt`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			srcVM, srcPath, srcRemote := splitRemote(args[0])
			dstVM, dstPath, dstRemote := splitRemote(args[1])
			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()
			c := g.client()
			switch {
			case srcRemote && dstRemote:
				return errors.New("one side must be local: tine copies between your machine and a VM")
			case dstRemote:
				id, err := resolveID(ctx, c, dstVM, false)
				if err != nil {
					return err
				}
				return upload(ctx, cmd, c, args[0], id, dstPath)
			case srcRemote:
				id, err := resolveID(ctx, c, srcVM, false)
				if err != nil {
					return err
				}
				return download(ctx, cmd, c, id, srcPath, args[1])
			default:
				return errors.New("one side must be VM:PATH")
			}
		},
	}
	return cmd
}

// splitRemote splits "VM:PATH". Local paths never look like one: they're
// "-", or have a slash or dot before any colon, as a VM ID has neither.
func splitRemote(arg string) (vmID, path string, ok bool) {
	id, p, found := strings.Cut(arg, ":")
	if !found || id == "" || strings.ContainsAny(id, "/.") {
		return "", arg, false
	}
	return id, p, true
}

func upload(ctx context.Context, cmd *cobra.Command, c *api.Client, src, id, dst string) error {
	if !path.IsAbs(dst) {
		return fmt.Errorf("%q: the path in the VM must be absolute", dst)
	}
	out := cmd.OutOrStdout()
	if src == "-" {
		info, err := c.WriteFile(ctx, id, dst, 0o644, cmd.InOrStdin())
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "%s:%s  %s\n", id, info.Path, sizeOf(info.Size))
		return err
	}
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		f, err := os.Open(src) //nolint:gosec // the user's own file
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		info, err := c.WriteFile(ctx, id, dst, fi.Mode().Perm(), f)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "%s:%s  %s\n", id, info.Path, sizeOf(info.Size))
		return err
	}
	// A directory: tar it on the fly into the request.
	pr, pw := io.Pipe()
	go func() { _ = pw.CloseWithError(tarDir(pw, src)) }()
	res, err := c.PutArchive(ctx, id, dst, pr)
	_ = pr.Close()
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "%s:%s  %d files, %d directories, %s\n", id, dst, res.Files, res.Dirs+1, sizeOf(res.Bytes))
	return err
}

// tarDir writes dir's contents to w as a tar stream, with names relative
// to dir: regular files, directories and symlinks.
func tarDir(w io.Writer, dir string) error {
	tw := tar.NewWriter(w)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		if fi.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		} else if !fi.Mode().IsRegular() && !fi.IsDir() {
			return nil // sockets, devices and the like stay behind
		}
		hdr, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if fi.IsDir() {
			hdr.Name += "/"
		}
		hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname = 0, 0, "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(p) //nolint:gosec // the user's own files
		if err != nil {
			return err
		}
		_, err = io.Copy(tw, f)
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		return err
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

func download(ctx context.Context, cmd *cobra.Command, c *api.Client, id, src, dst string) error {
	rc, info, err := c.ReadFile(ctx, id, src)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	if dst == "-" {
		_, err := io.Copy(cmd.OutOrStdout(), rc)
		return err
	}
	if fi, err := os.Stat(dst); err == nil && fi.IsDir() {
		dst = filepath.Join(dst, path.Base(src))
	}
	mode := os.FileMode(0o644)
	if m, err := strconv.ParseUint(info.Mode, 8, 32); err == nil {
		mode = os.FileMode(m).Perm()
	}
	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode) //nolint:gosec // where the user asked for it
	if err != nil {
		return err
	}
	n, err := io.Copy(f, rc)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s  %s\n", dst, sizeOf(n))
	return err
}

// sizeOf is a byte count for people: "18 B", "4.0 KiB", "1.5 MiB".
func sizeOf(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	case n < 1<<30:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%.2f GiB", float64(n)/(1<<30))
	}
}

func newDialStdioCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "dial-stdio",
		Short: "Join standard input and output to tined's socket (for --host ssh://)",
		Long: `Join standard input and output to tined's socket, until either side closes.
It's what "tine --host ssh://HOST" runs on HOST, through ssh, for every
connection to tined: you don't run it yourself.`,
		Args:   cobra.NoArgs,
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return api.DialStdio(cmd.Context(), g.socket, cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
}
