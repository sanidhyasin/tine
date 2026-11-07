package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sanidhyasin/tine/internal/agentproto"
)

func TestFilesRoundTrip(t *testing.T) {
	client, _ := startAgent(t, Config{})
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "hello.sh")
	data := bytes.Repeat([]byte("echo hi\n"), 100_000) // bigger than any buffer

	info, err := client.WriteFile(ctx, path, 0o755, bytes.NewReader(data))
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if info.Path != path || info.Size != int64(len(data)) || info.Mode != "0755" {
		t.Errorf("WriteFile = %+v", info)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("on disk: %v, %v; want mode 0755", fi, err)
	}

	rc, got, err := client.ReadFile(ctx, path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	read, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(read, data) || got.Size != int64(len(data)) || got.Mode != "0755" {
		t.Errorf("ReadFile: %d bytes, %+v", len(read), got)
	}

	// A second write replaces it whole, and no temporary file is left.
	if _, err := client.WriteFile(ctx, path, 0o600, strings.NewReader("short")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "short" { //nolint:gosec // a test file
		t.Errorf("after the second write: %q", b)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("the directory holds %d entries, want just the file", len(entries))
	}
}

func TestFilesErrors(t *testing.T) {
	client, _ := startAgent(t, Config{})
	ctx := context.Background()
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		do   func() error
		code string
	}{
		{"missing", func() error { _, _, err := client.ReadFile(ctx, filepath.Join(dir, "nope")); return err }, agentproto.CodeFileNotFound},
		{"a directory", func() error { _, _, err := client.ReadFile(ctx, dir); return err }, agentproto.CodeBadRequest},
		{"relative", func() error { _, _, err := client.ReadFile(ctx, "etc/passwd"); return err }, agentproto.CodeBadRequest},
		{"over a directory", func() error { _, err := client.WriteFile(ctx, dir, 0o644, strings.NewReader("x")); return err }, agentproto.CodeBadRequest},
		{"relative archive", func() error { _, err := client.PutArchive(ctx, "tmp", strings.NewReader("")); return err }, agentproto.CodeBadRequest},
	} {
		var ae *agentproto.Error
		if err := tc.do(); !errors.As(err, &ae) || ae.Code != tc.code {
			t.Errorf("%s: %v, want %s", tc.name, err, tc.code)
		}
	}
}

type entry struct {
	name, body, link string
	typ              byte
	mode             int64
}

func tarOf(t *testing.T, entries []entry, gz bool) []byte {
	t.Helper()
	var buf bytes.Buffer
	var w io.Writer = &buf
	var zw *gzip.Writer
	if gz {
		zw = gzip.NewWriter(&buf)
		w = zw
	}
	tw := tar.NewWriter(w)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: e.mode, Linkname: e.link, ModTime: time.Unix(1_700_000_000, 0)}
		if e.typ == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if zw != nil {
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

func TestPutArchive(t *testing.T) {
	client, _ := startAgent(t, Config{})
	ctx := context.Background()
	for _, gz := range []bool{false, true} {
		dir := filepath.Join(t.TempDir(), "repo")
		archive := tarOf(t, []entry{
			{name: "./", typ: tar.TypeDir, mode: 0o755},
			{name: "src/", typ: tar.TypeDir, mode: 0o755},
			{name: "src/main.py", typ: tar.TypeReg, mode: 0o644, body: "print('hi')\n"},
			{name: "run.sh", typ: tar.TypeReg, mode: 0o755, body: "#!/bin/sh\n"},
			{name: "main.py", typ: tar.TypeSymlink, link: "src/main.py"},
			{name: "copy.py", typ: tar.TypeLink, link: "src/main.py"},
			{name: "deep/er/file", typ: tar.TypeReg, mode: 0o600, body: "x"},
		}, gz)
		res, err := client.PutArchive(ctx, dir, bytes.NewReader(archive))
		if err != nil {
			t.Fatalf("gzip %v: PutArchive: %v", gz, err)
		}
		if res.Files != 3 || res.Dirs != 1 || res.Links != 2 {
			t.Errorf("gzip %v: result %+v", gz, res)
		}
		for path, want := range map[string]string{"src/main.py": "print('hi')\n", "main.py": "print('hi')\n", "copy.py": "print('hi')\n", "deep/er/file": "x"} {
			if b, err := os.ReadFile(filepath.Join(dir, path)); err != nil || string(b) != want { //nolint:gosec // a test file
				t.Errorf("gzip %v: %s = %q, %v", gz, path, b, err)
			}
		}
		if fi, _ := os.Stat(filepath.Join(dir, "run.sh")); fi == nil || fi.Mode().Perm() != 0o755 || fi.ModTime().Unix() != 1_700_000_000 {
			t.Errorf("gzip %v: run.sh = %v", gz, fi)
		}
	}
}

func TestPutArchiveStaysInItsDirectory(t *testing.T) {
	client, _ := startAgent(t, Config{})
	ctx := context.Background()
	outside := t.TempDir()
	for name, entries := range map[string][]entry{
		"dot dot":        {{name: "../escaped", typ: tar.TypeReg, mode: 0o644, body: "x"}},
		"absolute":       {{name: filepath.Join(outside, "escaped"), typ: tar.TypeReg, mode: 0o644, body: "x"}},
		"through a link": {{name: "out", typ: tar.TypeSymlink, link: outside}, {name: "out/escaped", typ: tar.TypeReg, mode: 0o644, body: "x"}},
		"hard link out":  {{name: "h", typ: tar.TypeLink, link: "../../etc/passwd"}},
		"a device":       {{name: "dev", typ: tar.TypeChar}},
	} {
		dir := filepath.Join(t.TempDir(), "d")
		_, err := client.PutArchive(ctx, dir, bytes.NewReader(tarOf(t, entries, false)))
		var ae *agentproto.Error
		if !errors.As(err, &ae) || ae.Code != agentproto.CodeBadRequest {
			t.Errorf("%s: %v, want bad_request", name, err)
		}
		if _, err := os.Stat(filepath.Join(outside, "escaped")); err == nil {
			t.Fatalf("%s: a file was written outside the directory", name)
		}
	}
}
