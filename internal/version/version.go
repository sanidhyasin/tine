// Package version holds build information for tine's binaries.
//
// The Makefile sets Version and Commit at link time with -ldflags "-X ...".
// Builds made with plain `go build`, `go run` or `go test` keep the defaults.
package version

import (
	"fmt"
	"runtime"
)

var (
	// Version is the release, from `git describe`, e.g. "v0.1.0" or "v0.1.0-3-ga1b2c3d-dirty".
	Version = "dev"
	// Commit is the short git commit the binary was built from.
	Commit = "unknown"
)

// String describes this build on one line, e.g. "v0.1.0 (a1b2c3d, go1.26.3 linux/amd64)".
func String() string {
	return fmt.Sprintf("%s (%s, %s %s/%s)", Version, Commit, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
