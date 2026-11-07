package agent

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sanidhyasin/tine/internal/agentproto"
)

// fakeSystem records what /sync changes, and fails on demand.
type fakeSystem struct {
	mu      sync.Mutex
	calls   []string
	clock   time.Time
	entropy []byte
	failAt  string
}

func (f *fakeSystem) record(call string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	if strings.HasPrefix(call, f.failAt+" ") || call == f.failAt {
		return errors.New("not allowed")
	}
	return nil
}

func (f *fakeSystem) SetClock(t time.Time) error {
	f.mu.Lock()
	f.clock = t
	f.mu.Unlock()
	return f.record("clock")
}

func (f *fakeSystem) AddEntropy(b []byte) error {
	f.mu.Lock()
	f.entropy = append([]byte(nil), b...)
	f.mu.Unlock()
	return f.record("entropy")
}
func (f *fakeSystem) SetHostname(name string) error { return f.record("hostname " + name) }
func (f *fakeSystem) SetMachineID(id string) error  { return f.record("machine-id " + id) }

func TestSync(t *testing.T) {
	sys := &fakeSystem{}
	c, _ := startAgent(t, Config{System: sys})
	host := time.Now().Add(-90 * time.Second) // the guest is 90s ahead, say
	res, err := c.Sync(context.Background(), agentproto.SyncRequest{
		TimeUnixNano: host.UnixNano(), Entropy: []byte("0123456789abcdef"),
		Hostname: "vm-abcdefghij", MachineID: strings.Repeat("ab", 16),
	})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	// The clock goes first, so everything after happens at the right time.
	if got := strings.Join(sys.calls, ","); got != "clock,entropy,hostname vm-abcdefghij,machine-id "+strings.Repeat("ab", 16) {
		t.Errorf("calls = %s", got)
	}
	if !sys.clock.Equal(time.Unix(0, host.UnixNano())) || string(sys.entropy) != "0123456789abcdef" {
		t.Errorf("clock %v, entropy %q", sys.clock, sys.entropy)
	}
	if res.ClockOffsetMs < 89_000 || res.ClockOffsetMs > 91_000 || res.DurationMs < 0 {
		t.Errorf("result = %+v, want the clock about 90000ms ahead", res)
	}
}

func TestSyncDoesOnlyWhatItsAsked(t *testing.T) {
	sys := &fakeSystem{}
	c, _ := startAgent(t, Config{System: sys})
	if _, err := c.Sync(context.Background(), agentproto.SyncRequest{Hostname: "h"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(sys.calls, ","); got != "hostname h" {
		t.Errorf("calls = %s", got)
	}
}

func TestSyncErrors(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name   string
		req    agentproto.SyncRequest
		failAt string
		status int
		code   string
	}{
		{"bad hostname", agentproto.SyncRequest{Hostname: "no spaces"}, "", http.StatusBadRequest, agentproto.CodeBadRequest},
		{"hostname too long", agentproto.SyncRequest{Hostname: strings.Repeat("a", 64)}, "", http.StatusBadRequest, agentproto.CodeBadRequest},
		{"bad machine ID", agentproto.SyncRequest{MachineID: "ABC"}, "", http.StatusBadRequest, agentproto.CodeBadRequest},
		{"clock refused", agentproto.SyncRequest{TimeUnixNano: 1, Hostname: "h"}, "clock", http.StatusInternalServerError, agentproto.CodeSyncFailed},
		{"hostname refused", agentproto.SyncRequest{Hostname: "h"}, "hostname", http.StatusInternalServerError, agentproto.CodeSyncFailed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			sys := &fakeSystem{failAt: tt.failAt}
			c, _ := startAgent(t, Config{System: sys})
			_, err := c.Sync(ctx, tt.req)
			var ae *agentproto.Error
			if !errors.As(err, &ae) || ae.StatusCode != tt.status || ae.Code != tt.code {
				t.Fatalf("Sync = %#v, want %d %s", err, tt.status, tt.code)
			}
			if tt.failAt == "clock" && len(sys.calls) != 1 {
				t.Errorf("it carried on after the clock failed: %v", sys.calls)
			}
		})
	}
}

func TestOSSystemFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o750); err != nil {
		t.Fatal(err)
	}
	o := OSSystem{Root: root}
	id := strings.Repeat("0f", 16)
	if err := o.SetMachineID(id); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, "etc/machine-id")) //nolint:gosec // the test's own temporary directory
	if err != nil || string(got) != id+"\n" {
		t.Errorf("machine-id = %q, %v", got, err)
	}
	if fi, _ := os.Stat(filepath.Join(root, "etc/machine-id")); fi.Mode().Perm() != 0o444 {
		t.Errorf("machine-id mode = %v, want 0444", fi.Mode().Perm())
	}
	// Setting it again replaces the read-only file.
	if err := o.SetMachineID(strings.Repeat("1e", 16)); err != nil {
		t.Errorf("SetMachineID again: %v", err)
	}
}

func TestWithHostname(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"", "127.0.0.1\tlocalhost\n127.0.1.1\tvm-a\n"},
		{"127.0.0.1 localhost\n", "127.0.0.1 localhost\n127.0.1.1\tvm-a\n"},
		{"127.0.0.1 localhost\n127.0.1.1 tine\n::1 ip6-localhost\n", "127.0.0.1 localhost\n127.0.1.1\tvm-a\n::1 ip6-localhost\n"},
		{"127.0.1.1 old\n127.0.1.1 older\n", "127.0.1.1\tvm-a\n"},
	} {
		if got := withHostname(tt.in, "vm-a"); got != tt.want {
			t.Errorf("withHostname(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
