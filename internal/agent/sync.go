package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/sanidhyasin/tine/internal/agentproto"
)

// maxSyncBytes caps a /sync request.
const maxSyncBytes = 64 << 10

// System is what /sync changes in the guest. OSSystem is the real one; tests
// use fakes.
type System interface {
	// SetClock sets the wall clock.
	SetClock(t time.Time) error
	// AddEntropy adds random bytes to the kernel's pool, crediting them, and
	// reseeds the kernel's generator.
	AddEntropy(b []byte) error
	SetHostname(name string) error
	SetMachineID(id string) error
}

var (
	// A hostname is one DNS label: letters, digits and hyphens.
	validHostname  = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)
	validMachineID = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var req agentproto.SyncRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSyncBytes)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, agentproto.CodeBadRequest, "bad sync request: "+err.Error())
		return
	}
	if req.Hostname != "" && !validHostname.MatchString(req.Hostname) {
		writeError(w, http.StatusBadRequest, agentproto.CodeBadRequest, fmt.Sprintf("bad hostname %q", req.Hostname))
		return
	}
	if req.MachineID != "" && !validMachineID.MatchString(req.MachineID) {
		writeError(w, http.StatusBadRequest, agentproto.CodeBadRequest, "machine_id must be 32 lowercase hexadecimal characters")
		return
	}

	var res agentproto.SyncResult
	// The clock first, so everything after it happens at the right time.
	if req.TimeUnixNano != 0 {
		host := time.Unix(0, req.TimeUnixNano)
		res.ClockOffsetMs = float64(time.Since(host).Microseconds()) / 1000
		if err := s.cfg.System.SetClock(host); err != nil {
			s.syncFailed(w, "set the clock", err)
			return
		}
	}
	if len(req.Entropy) > 0 {
		if err := s.cfg.System.AddEntropy(req.Entropy); err != nil {
			s.syncFailed(w, "add entropy", err)
			return
		}
	}
	if req.Hostname != "" {
		if err := s.cfg.System.SetHostname(req.Hostname); err != nil {
			s.syncFailed(w, "set the hostname", err)
			return
		}
	}
	if req.MachineID != "" {
		if err := s.cfg.System.SetMachineID(req.MachineID); err != nil {
			s.syncFailed(w, "set the machine ID", err)
			return
		}
	}
	res.DurationMs = float64(time.Since(start).Microseconds()) / 1000
	s.log.Info("synced", "clock_offset_ms", res.ClockOffsetMs, "hostname", req.Hostname, "entropy_bytes", len(req.Entropy))
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) syncFailed(w http.ResponseWriter, what string, err error) {
	s.log.Error("sync failed", "step", what, "err", err)
	writeError(w, http.StatusInternalServerError, agentproto.CodeSyncFailed, what+": "+err.Error())
}

// OSSystem is the real guest. Root is the filesystem root, "/" in a VM.
type OSSystem struct{ Root string }

func (o OSSystem) SetClock(t time.Time) error { return setClock(t) }
func (o OSSystem) AddEntropy(b []byte) error  { return addEntropy(b) }

// SetHostname sets the kernel's hostname, and writes it to /etc/hostname
// and to /etc/hosts, so the name resolves to this machine.
func (o OSSystem) SetHostname(name string) error {
	if err := sethostname(name); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(o.Root, "etc/hostname"), []byte(name+"\n"), 0o644); err != nil {
		return err
	}
	hostsPath := filepath.Join(o.Root, "etc/hosts")
	hosts, err := os.ReadFile(hostsPath) //nolint:gosec // a fixed path under the guest's root
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writeAtomic(hostsPath, []byte(withHostname(string(hosts), name)), 0o644)
}

// SetMachineID writes /etc/machine-id.
func (o OSSystem) SetMachineID(id string) error {
	return writeAtomic(filepath.Join(o.Root, "etc/machine-id"), []byte(id+"\n"), 0o444)
}

// withHostname returns hosts with a "127.0.1.1 name" line, replacing any
// earlier 127.0.1.1 line, as Debian and Ubuntu do for the machine's own name.
func withHostname(hosts, name string) string {
	var out []string
	replaced := false
	for _, line := range strings.Split(strings.TrimRight(hosts, "\n"), "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] == "127.0.1.1" {
			if !replaced {
				out = append(out, "127.0.1.1\t"+name)
				replaced = true
			}
			continue
		}
		if line != "" || len(out) > 0 {
			out = append(out, line)
		}
	}
	if !replaced {
		if len(out) == 0 {
			out = append(out, "127.0.0.1\tlocalhost")
		}
		out = append(out, "127.0.1.1\t"+name)
	}
	return strings.Join(out, "\n") + "\n"
}

// writeAtomic replaces path with data, so a reader never sees half of it.
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tine-sync"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
