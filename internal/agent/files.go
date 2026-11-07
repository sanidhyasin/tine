package agent

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/sanidhyasin/tine/internal/agentproto"
)

// File transfer. GET /files sends one file's bytes, PUT /files writes one,
// and PUT /archive unpacks a tar stream into a directory. The guest is the
// sandbox, so any path in it may be read or written; what's checked is that
// requests make sense, and that an archive's entries stay in its directory.

// maxFileBytes caps one file, or one archive, written in a request.
const maxFileBytes = 8 << 30

func (s *Server) handleFiles(w http.ResponseWriter, r *http.Request) {
	path, ok := absPath(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.readFile(w, path)
	case http.MethodPut:
		s.writeFile(w, r, path)
	default:
		w.Header().Set("Allow", "GET, PUT")
		writeError(w, http.StatusMethodNotAllowed, agentproto.CodeMethodNotAllowed, r.Method+" isn't allowed here")
	}
}

// absPath reads the request's ?path=, which must be absolute.
func absPath(w http.ResponseWriter, r *http.Request) (string, bool) {
	path := r.URL.Query().Get("path")
	if !filepath.IsAbs(path) {
		writeError(w, http.StatusBadRequest, agentproto.CodeBadRequest, fmt.Sprintf("path %q must be absolute", path))
		return "", false
	}
	return filepath.Clean(path), true
}

func (s *Server) readFile(w http.ResponseWriter, path string) {
	f, err := os.Open(path) //nolint:gosec // reading the caller's file is the point; the guest is the sandbox
	if err != nil {
		fileError(w, err)
		return
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		fileError(w, err)
		return
	}
	if !fi.Mode().IsRegular() {
		writeError(w, http.StatusBadRequest, agentproto.CodeBadRequest, path+" isn't a regular file")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	w.Header().Set(agentproto.HeaderFileMode, fmt.Sprintf("%04o", fi.Mode().Perm()))
	if _, err := io.Copy(w, f); err != nil {
		s.log.Warn("sending a file broke off", "err", err)
	}
}

func (s *Server) writeFile(w http.ResponseWriter, r *http.Request, path string) {
	mode := os.FileMode(0o644)
	if m := r.URL.Query().Get("mode"); m != "" {
		v, err := strconv.ParseUint(m, 8, 32)
		if err != nil || v > 0o7777 {
			writeError(w, http.StatusBadRequest, agentproto.CodeBadRequest, fmt.Sprintf("mode %q: want octal, like 0644", m))
			return
		}
		mode = os.FileMode(v)
	}
	if fi, err := os.Stat(path); err == nil && fi.IsDir() { //nolint:gosec // G703: any path in the guest may be written; the guest is the sandbox
		writeError(w, http.StatusBadRequest, agentproto.CodeBadRequest, path+" is a directory")
		return
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // an ordinary directory in the guest
		fileError(w, err)
		return
	}
	// Written beside it, then renamed over it: a reader never sees half a file.
	tmp, err := os.CreateTemp(dir, ".tine-upload-*")
	if err != nil {
		fileError(w, err)
		return
	}
	n, err := io.Copy(tmp, http.MaxBytesReader(w, r.Body, maxFileBytes))
	if err == nil {
		err = tmp.Chmod(mode)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path) //nolint:gosec // G703: as above
	}
	if err != nil {
		_ = os.Remove(tmp.Name()) //nolint:gosec // G703: as above
		fileError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, agentproto.FileInfo{Path: path, Size: n, Mode: fmt.Sprintf("%04o", mode.Perm())})
}

func (s *Server) handleArchive(w http.ResponseWriter, r *http.Request) {
	if !allowMethod(w, r, http.MethodPut) {
		return
	}
	dir, ok := absPath(w, r)
	if !ok {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // an ordinary directory in the guest
		fileError(w, err)
		return
	}
	body := bufio.NewReader(http.MaxBytesReader(w, r.Body, maxFileBytes))
	var in io.Reader = body
	if magic, _ := body.Peek(2); len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(body)
		if err != nil {
			writeError(w, http.StatusBadRequest, agentproto.CodeBadRequest, "bad gzip: "+err.Error())
			return
		}
		defer func() { _ = gz.Close() }()
		in = gz
	}
	res, err := unpack(tar.NewReader(in), dir)
	if err != nil {
		var bad *badEntryError
		if errors.As(err, &bad) {
			writeError(w, http.StatusBadRequest, agentproto.CodeBadRequest, err.Error())
			return
		}
		fileError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// badEntryError is an archive entry that can't be unpacked: it would land
// outside the directory, or it's a kind of file tine doesn't unpack.
type badEntryError struct{ name, why string }

func (e *badEntryError) Error() string { return fmt.Sprintf("archive entry %q: %s", e.name, e.why) }

// unpack writes a tar stream's entries into dir: directories, regular files,
// symlinks and hard links. Each lands inside dir, and is never written
// through a symlink, even one an earlier entry made.
func unpack(tr *tar.Reader, dir string) (agentproto.ArchiveResult, error) {
	var res agentproto.ArchiveResult
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return res, nil
		}
		if err != nil {
			return res, &badEntryError{name: "", why: err.Error()}
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		if name == "" || name == "." {
			continue // the directory itself
		}
		if !filepath.IsLocal(name) {
			return res, &badEntryError{name: hdr.Name, why: "it would land outside the directory"}
		}
		target := filepath.Join(dir, name)
		if err := noSymlinkBetween(dir, filepath.Dir(target)); err != nil {
			return res, &badEntryError{name: hdr.Name, why: err.Error()}
		}
		mode := os.FileMode(hdr.Mode).Perm() //nolint:gosec // tar modes fit
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, mode|0o700); err != nil { //nolint:gosec // G703: target is checked to be inside dir above
				return res, err
			}
			res.Dirs++
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil { //nolint:gosec // an ordinary directory in the guest
				return res, err
			}
			n, err := writeEntry(target, mode, tr)
			res.Bytes += n
			if err != nil {
				return res, err
			}
			_ = os.Chtimes(target, hdr.ModTime, hdr.ModTime)
			res.Files++
		case tar.TypeSymlink:
			_ = os.Remove(target) //nolint:gosec // G703: target is checked to be inside dir above
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return res, err
			}
			res.Links++
		case tar.TypeLink:
			if !filepath.IsLocal(hdr.Linkname) {
				return res, &badEntryError{name: hdr.Name, why: "a hard link to outside the directory"}
			}
			_ = os.Remove(target)                                                     //nolint:gosec // G703: target is checked to be inside dir above
			if err := os.Link(filepath.Join(dir, hdr.Linkname), target); err != nil { //nolint:gosec // G305: Linkname is checked with filepath.IsLocal above
				return res, err
			}
			res.Links++
		default:
			return res, &badEntryError{name: hdr.Name, why: fmt.Sprintf("tar type %q isn't unpacked", hdr.Typeflag)}
		}
	}
}

// writeEntry writes one file, replacing whatever is there, but never
// following a symlink at its name.
func writeEntry(target string, mode os.FileMode, r io.Reader) (int64, error) {
	if fi, err := os.Lstat(target); err == nil && !fi.Mode().IsRegular() { //nolint:gosec // G703: the caller checked target is inside its directory
		if err := os.Remove(target); err != nil { //nolint:gosec // G703: as above
			return 0, err
		}
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, mode) //nolint:gosec // a path checked to be inside the directory
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, r)
	if err == nil {
		err = f.Chmod(mode)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return n, err
}

// noSymlinkBetween checks that no directory from dir down to sub is a
// symlink, so a file written in sub stays inside dir.
func noSymlinkBetween(dir, sub string) error {
	rel, err := filepath.Rel(dir, sub)
	if err != nil || rel == "." {
		return err
	}
	p := dir
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		p = filepath.Join(p, part)
		fi, err := os.Lstat(p) //nolint:gosec // G703: only looks, at a path under dir
		if errors.Is(err, fs.ErrNotExist) {
			return nil // made fresh below: no symlinks
		}
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink", p)
		}
	}
	return nil
}

// fileError answers with the status that fits a filesystem error.
func fileError(w http.ResponseWriter, err error) {
	var tooBig *http.MaxBytesError
	switch {
	case errors.Is(err, fs.ErrNotExist):
		writeError(w, http.StatusNotFound, agentproto.CodeFileNotFound, err.Error())
	case errors.Is(err, fs.ErrPermission):
		writeError(w, http.StatusForbidden, agentproto.CodeBadRequest, err.Error())
	case errors.As(err, &tooBig):
		writeError(w, http.StatusRequestEntityTooLarge, agentproto.CodeBadRequest, fmt.Sprintf("more than %d bytes", tooBig.Limit))
	default:
		writeError(w, http.StatusInternalServerError, agentproto.CodeFileFailed, err.Error())
	}
}
