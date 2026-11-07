package api

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
	"strings"
	"sync"
	"testing"

	"github.com/sanidhyasin/tine/internal/agentproto"
	"github.com/sanidhyasin/tine/internal/vm"
)

// fakeFiles is the fake backend's file store: one map for every VM.
type fakeFiles struct {
	mu    sync.Mutex
	files map[string][]byte // "vmID:path"
	modes map[string]os.FileMode
}

var theFiles = &fakeFiles{files: map[string][]byte{}, modes: map[string]os.FileMode{}}

func (f *fakeBackend) ReadFile(_ context.Context, id, p string) (io.ReadCloser, agentproto.FileInfo, error) {
	if _, err := f.Get(context.Background(), id); err != nil {
		return nil, agentproto.FileInfo{}, err
	}
	theFiles.mu.Lock()
	defer theFiles.mu.Unlock()
	data, ok := theFiles.files[id+":"+p]
	if !ok {
		return nil, agentproto.FileInfo{}, &agentproto.Error{StatusCode: http.StatusNotFound,
			ErrorDetail: agentproto.ErrorDetail{Code: agentproto.CodeFileNotFound, Message: "no " + p}}
	}
	return io.NopCloser(bytes.NewReader(data)), agentproto.FileInfo{Path: p, Size: int64(len(data)), Mode: fmt.Sprintf("%04o", theFiles.modes[id+":"+p])}, nil
}

func (f *fakeBackend) WriteFile(_ context.Context, id, p string, mode os.FileMode, r io.Reader) (agentproto.FileInfo, error) {
	if _, err := f.Get(context.Background(), id); err != nil {
		return agentproto.FileInfo{}, err
	}
	if !path.IsAbs(p) {
		return agentproto.FileInfo{}, fmt.Errorf("%w: path %q must be absolute", vm.ErrInvalidRequest, p)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return agentproto.FileInfo{}, err
	}
	theFiles.mu.Lock()
	defer theFiles.mu.Unlock()
	theFiles.files[id+":"+p], theFiles.modes[id+":"+p] = data, mode
	return agentproto.FileInfo{Path: p, Size: int64(len(data)), Mode: fmt.Sprintf("%04o", mode)}, nil
}

func (f *fakeBackend) PutArchive(ctx context.Context, id, dir string, r io.Reader) (agentproto.ArchiveResult, error) {
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
		if hdr.Typeflag != tar.TypeReg {
			res.Dirs++
			continue
		}
		info, err := f.WriteFile(ctx, id, path.Join(dir, hdr.Name), os.FileMode(hdr.Mode), tr) //nolint:gosec // test modes fit
		if err != nil {
			return res, err
		}
		res.Files++
		res.Bytes += info.Size
	}
}

func TestFiles(t *testing.T) {
	b := newFakeBackend()
	c, _ := serve(t, b)
	ctx := context.Background()
	v, err := c.CreateVM(ctx, CreateVMRequest{})
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte("tine "), 100_000)
	info, err := c.WriteFile(ctx, v.ID, "/root/notes.txt", 0o600, bytes.NewReader(data))
	if err != nil || info.Size != int64(len(data)) || info.Mode != "0600" {
		t.Fatalf("WriteFile = %+v, %v", info, err)
	}
	rc, got, err := c.ReadFile(ctx, v.ID, "/root/notes.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	read, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(read, data) || got.Size != int64(len(data)) || got.Mode != "0600" {
		t.Errorf("ReadFile: %d bytes, %+v", len(read), got)
	}

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range map[string]string{"a.py": "print(1)", "pkg/b.py": "print(2)"} {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	res, err := c.PutArchive(ctx, v.ID, "/root/repo", &buf)
	if err != nil || res.Files != 2 {
		t.Fatalf("PutArchive = %+v, %v", res, err)
	}
	if rc, _, err := c.ReadFile(ctx, v.ID, "/root/repo/pkg/b.py"); err != nil {
		t.Errorf("the archive's file: %v", err)
	} else {
		b, _ := io.ReadAll(rc)
		_ = rc.Close()
		if string(b) != "print(2)" {
			t.Errorf("the archive's file = %q", b)
		}
	}

	// Errors come back with their codes.
	for name, do := range map[string]func() error{
		"no file":  func() error { _, _, err := c.ReadFile(ctx, v.ID, "/nope"); return err },
		"no VM":    func() error { _, _, err := c.ReadFile(ctx, "vm-nope", "/x"); return err },
		"bad mode": func() error { _, err := c.WriteFile(ctx, v.ID, "/x", 0o644, strings.NewReader("")); return err },
		"relative": func() error { _, err := c.WriteFile(ctx, v.ID, "x", 0o644, strings.NewReader("")); return err },
	} {
		err := do()
		var ae *Error
		switch name {
		case "bad mode":
			if err != nil {
				t.Errorf("%s: %v", name, err) // the client always sends a good mode
			}
		case "no file":
			if !errors.As(err, &ae) || ae.StatusCode != http.StatusNotFound || ae.Code != agentproto.CodeFileNotFound {
				t.Errorf("%s: %v", name, err)
			}
		case "no VM":
			if !errors.As(err, &ae) || ae.Code != CodeNotFound {
				t.Errorf("%s: %v", name, err)
			}
		case "relative":
			if !errors.As(err, &ae) || ae.Code != CodeInvalidRequest {
				t.Errorf("%s: %v", name, err)
			}
		}
	}
}
