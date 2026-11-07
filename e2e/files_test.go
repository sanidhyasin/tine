//go:build e2e

package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sanidhyasin/tine/internal/api"
)

// TestFilesInAndOut writes a file into a real VM, runs it, reads it back,
// unpacks a gzipped tar into it, and checks errors come back with codes.
func TestFilesInAndOut(t *testing.T) {
	vm := createDefault(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	script := "#!/bin/sh\necho \"hello from $(hostname)\"\n"
	info, err := tined.WriteFile(ctx, vm.ID, "/root/bin/hello.sh", 0o755, strings.NewReader(script))
	if err != nil || info.Mode != "0755" {
		t.Fatalf("WriteFile = %+v, %v", info, err)
	}
	if out, code, err := run(ctx, vm.ID, "/root/bin/hello.sh"); err != nil || code != 0 || out != "hello from "+vm.ID+"\n" {
		t.Errorf("running it: %q, exit %d, %v", out, code, err)
	}
	rc, got, err := tined.ReadFile(ctx, vm.ID, "/root/bin/hello.sh")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(data) != script || got.Mode != "0755" {
		t.Errorf("read back %q (mode %s)", data, got.Mode)
	}

	// 5 MiB through the agent, both ways.
	big := bytes.Repeat([]byte("tine"), 5<<18)
	if _, err := tined.WriteFile(ctx, vm.ID, "/tmp/big", 0o644, bytes.NewReader(big)); err != nil {
		t.Fatal(err)
	}
	rc, _, err = tined.ReadFile(ctx, vm.ID, "/tmp/big")
	if err != nil {
		t.Fatal(err)
	}
	data, _ = io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(data, big) {
		t.Errorf("5 MiB came back as %d bytes, or different", len(data))
	}

	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range map[string]string{"a.py": "print('a')\n", "pkg/b.py": "print('b')\n"} {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = zw.Close()
	res, err := tined.PutArchive(ctx, vm.ID, "/root/repo", &buf)
	if err != nil || res.Files != 2 {
		t.Fatalf("PutArchive = %+v, %v", res, err)
	}
	if out, _, _ := run(ctx, vm.ID, "python3", "/root/repo/pkg/b.py"); out != "b\n" {
		t.Errorf("the unpacked file ran as %q", out)
	}

	if _, _, err := tined.ReadFile(ctx, vm.ID, "/nope"); !isCode(err, http.StatusNotFound, "file_not_found") {
		t.Errorf("a missing file: %v", err)
	}
	if _, err := tined.WriteFile(ctx, vm.ID, "relative", 0o644, strings.NewReader("")); !isCode(err, http.StatusBadRequest, "bad_request") {
		t.Errorf("a relative path: %v", err)
	}
}

func isCode(err error, status int, code string) bool {
	s, c := apiCode(err)
	return s == status && c == code
}

// TestDialStdio reaches tined through "tine dial-stdio", as tine --host
// ssh://... does through ssh.
func TestDialStdio(t *testing.T) {
	bin, err := exec.LookPath("tine")
	if err != nil {
		t.Skip("tine isn't installed on this host")
	}
	c := api.NewCommandClient([]string{bin, "dial-stdio"})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	h, err := c.Health(ctx)
	if err != nil || h.Status != "ok" {
		t.Fatalf("Health through dial-stdio: %+v, %v", h, err)
	}
	vm := createDefault(t)
	st, err := c.Exec(ctx, vm.ID, api.ExecRequest{Args: []string{"true"}}, nil)
	if err != nil || st.Code != 0 {
		t.Errorf("exec through dial-stdio: %+v, %v", st, err)
	}
}
