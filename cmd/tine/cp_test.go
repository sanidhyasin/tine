package main

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sanidhyasin/tine/internal/agentproto"
)

func (b *backend) ReadFile(ctx context.Context, id, p string) (io.ReadCloser, agentproto.FileInfo, error) {
	if _, err := b.Get(ctx, id); err != nil {
		return nil, agentproto.FileInfo{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	data, ok := b.files[id+":"+p]
	if !ok {
		return nil, agentproto.FileInfo{}, &agentproto.Error{StatusCode: http.StatusNotFound,
			ErrorDetail: agentproto.ErrorDetail{Code: agentproto.CodeFileNotFound, Message: "open " + p + ": no such file or directory"}}
	}
	return io.NopCloser(bytes.NewReader(data)), agentproto.FileInfo{Path: p, Size: int64(len(data)), Mode: fmt.Sprintf("%04o", b.modes[id+":"+p])}, nil
}

func (b *backend) WriteFile(ctx context.Context, id, p string, mode os.FileMode, r io.Reader) (agentproto.FileInfo, error) {
	if _, err := b.Get(ctx, id); err != nil {
		return agentproto.FileInfo{}, err
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return agentproto.FileInfo{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.files == nil {
		b.files, b.modes = map[string][]byte{}, map[string]os.FileMode{}
	}
	b.files[id+":"+p], b.modes[id+":"+p] = data, mode
	return agentproto.FileInfo{Path: p, Size: int64(len(data)), Mode: fmt.Sprintf("%04o", mode)}, nil
}

func (b *backend) PutArchive(ctx context.Context, id, dir string, r io.Reader) (agentproto.ArchiveResult, error) {
	var res agentproto.ArchiveResult
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return res, nil
		}
		if err != nil {
			return res, err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			res.Dirs++
		case tar.TypeSymlink:
			res.Links++
		case tar.TypeReg:
			info, err := b.WriteFile(ctx, id, path.Join(dir, hdr.Name), os.FileMode(hdr.Mode).Perm(), tr) //nolint:gosec // test modes fit
			if err != nil {
				return res, err
			}
			res.Files++
			res.Bytes += info.Size
		}
	}
}

func TestCp(t *testing.T) {
	sock, b := startTined(t)
	tine(t, sock, "", "create")
	local := t.TempDir()
	script := filepath.Join(local, "run.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}

	// A file in, keeping its mode; a VM ID prefix works.
	r := tine(t, sock, "", "cp", script, "vm-aaaa:/root/run.sh")
	if r.code != 0 || r.stdout != "vm-aaaaaaaaab:/root/run.sh  18 B\n" {
		t.Fatalf("cp in: exit %d, %q %q", r.code, r.stdout, r.stderr)
	}
	if b.modes["vm-aaaaaaaaab:/root/run.sh"] != 0o755 {
		t.Errorf("mode = %v, want 0755", b.modes["vm-aaaaaaaaab:/root/run.sh"])
	}
	// stdin in, and the file back out, to a directory and to stdout.
	if r := tine(t, sock, "notes\n", "cp", "-", "vm-aaaaaaaaab:/tmp/notes.txt"); r.code != 0 {
		t.Fatalf("cp - : exit %d, %q", r.code, r.stderr)
	}
	if r := tine(t, sock, "", "cp", "vm-aaaaaaaaab:/tmp/notes.txt", "-"); r.stdout != "notes\n" {
		t.Errorf("cp to stdout: %q %q", r.stdout, r.stderr)
	}
	if r := tine(t, sock, "", "cp", "vm-aaaaaaaaab:/root/run.sh", local+"/"); r.code != 0 {
		t.Fatalf("cp out: exit %d, %q", r.code, r.stderr)
	}
	if fi, err := os.Stat(script); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("the copy out: %v, %v", fi, err)
	}

	// A directory, as a tar, with its layout and symlinks.
	repo := filepath.Join(local, "repo")
	for p, body := range map[string]string{"a.py": "print(1)", "pkg/b.py": "print(2)", "pkg/sub/c.txt": "c"} {
		if err := os.MkdirAll(filepath.Join(repo, filepath.Dir(p)), 0o755); err != nil { //nolint:gosec // a test directory
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, p), []byte(body), 0o644); err != nil { //nolint:gosec // a test file
			t.Fatal(err)
		}
	}
	if err := os.Symlink("a.py", filepath.Join(repo, "link.py")); err != nil {
		t.Fatal(err)
	}
	r = tine(t, sock, "", "cp", repo, "vm-aaaaaaaaab:/root/repo")
	if r.code != 0 || !strings.Contains(r.stdout, "3 files, 3 directories") {
		t.Fatalf("cp dir: exit %d, %q %q", r.code, r.stdout, r.stderr)
	}
	if string(b.files["vm-aaaaaaaaab:/root/repo/pkg/sub/c.txt"]) != "c" {
		t.Errorf("files in the VM: %v", b.files)
	}

	for name, args := range map[string][]string{
		"both local":    {"cp", "a", "b"},
		"both remote":   {"cp", "vm-aaaaaaaaab:/a", "vm-aaaaaaaaab:/b"},
		"relative":      {"cp", script, "vm-aaaaaaaaab:root/x"},
		"no such file":  {"cp", "vm-aaaaaaaaab:/nope", local},
		"no such local": {"cp", filepath.Join(local, "nope"), "vm-aaaaaaaaab:/x"},
	} {
		if r := tine(t, sock, "", args...); r.code == 0 || r.stderr == "" {
			t.Errorf("%s: exit %d, %q", name, r.code, r.stderr)
		}
	}
	if _, _, ok := splitRemote("./vm-x:/a"); ok {
		t.Error(`"./vm-x:/a" was taken for a VM`)
	}
}
