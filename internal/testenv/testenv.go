// Package testenv finds what tine's integration tests need on the dev server,
// and skips a test cleanly when something is missing (on a Mac, or in CI).
//
// It's only imported by tests.
package testenv

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// Artifacts are the files scripts/host-setup.sh downloads.
type Artifacts struct {
	Firecracker string
	Jailer      string
	Kernel      string
	Rootfs      string
}

// RequireArtifacts finds the artifacts in $TINE_ARTIFACTS (by default
// ~/tine/artifacts), or skips the test if any is missing.
func RequireArtifacts(t testing.TB) Artifacts {
	t.Helper()
	dir := os.Getenv("TINE_ARTIFACTS")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, "tine", "artifacts")
	}
	kernels, _ := filepath.Glob(filepath.Join(dir, "vmlinux-*"))
	sort.Strings(kernels)
	if len(kernels) == 0 {
		t.Skipf("no guest kernel in %s: run scripts/host-setup.sh, or set TINE_ARTIFACTS", dir)
	}
	a := Artifacts{
		Firecracker: filepath.Join(dir, "bin", "firecracker"),
		Jailer:      filepath.Join(dir, "bin", "jailer"),
		Kernel:      kernels[len(kernels)-1],
		Rootfs:      filepath.Join(dir, "rootfs.ext4"),
	}
	for _, p := range []string{a.Firecracker, a.Jailer, a.Rootfs} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("%v: run scripts/host-setup.sh, or set TINE_ARTIFACTS", err)
		}
	}
	return a
}

// RequireKVM skips the test unless this process can use /dev/kvm.
func RequireKVM(t testing.TB) {
	t.Helper()
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("can't use KVM: %v", err)
	}
	_ = f.Close()
}

// RequireRoot skips the test unless it runs as root, as the jailer and
// network setup need. `make test-integration` runs tests with sudo.
func RequireRoot(t testing.TB) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("needs root: run with sudo (make test-integration does)")
	}
}

// RequireDataDir returns tine's data volume ($TINE_DATA_DIR, by default
// /srv/tine), or skips the test unless it's a mounted XFS filesystem.
func RequireDataDir(t testing.TB) string {
	t.Helper()
	dir := os.Getenv("TINE_DATA_DIR")
	if dir == "" {
		dir = "/srv/tine"
	}
	fstype, ok := mountType(dir)
	if !ok {
		t.Skipf("%s isn't mounted: run ./scripts/dev-server.sh setup", dir)
	}
	if fstype != "xfs" {
		t.Skipf("%s is %s, not XFS", dir, fstype)
	}
	return dir
}

// mountType returns the filesystem type mounted exactly at dir.
func mountType(dir string) (string, bool) {
	f, err := os.Open("/proc/self/mounts")
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) >= 3 && fields[1] == dir {
			return fields[2], true
		}
	}
	return "", false
}

// TempDir makes a short-named temporary directory under parent (or the
// system's temp directory if parent is empty) and removes it when the test
// ends. Short names matter: socket paths are limited to about 100 bytes.
func TempDir(t testing.TB, parent string) string {
	t.Helper()
	dir, err := os.MkdirTemp(parent, "t")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// CachedCopy copies src into <dataDir>/test-cache once, and returns the
// copy's path. Tests that hard-link or clone files need them on the data
// volume, and keeping the copy saves seconds on every run. The copy is made
// world-readable so it can be shared into jails.
func CachedCopy(t testing.TB, dataDir, src string) string {
	t.Helper()
	cache := filepath.Join(dataDir, "test-cache")
	dst := filepath.Join(cache, filepath.Base(src))
	srcInfo, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	if dstInfo, err := os.Stat(dst); err == nil && dstInfo.Size() == srcInfo.Size() {
		return dst
	}
	if err := os.MkdirAll(cache, 0o750); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	tmp := dst + ".partial"
	// cp keeps sparse files sparse, which matters for multi-GB disk images.
	if out, err := exec.CommandContext(ctx, "cp", "--sparse=always", src, tmp).CombinedOutput(); err != nil { //nolint:gosec // test-only copy of a known file
		t.Fatalf("copy %s to the test cache: %v: %s", src, err, out)
	}
	if err := os.Chmod(tmp, 0o644); err != nil { //nolint:gosec // shared into jails, so it must be world-readable
		t.Fatal(err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		t.Fatal(err)
	}
	return dst
}
