package version

import (
	"runtime"
	"strings"
	"testing"
)

func TestString(t *testing.T) {
	oldVersion, oldCommit := Version, Commit
	t.Cleanup(func() { Version, Commit = oldVersion, oldCommit })

	Version, Commit = "v1.2.3", "abc1234"
	got := String()

	for _, want := range []string{"v1.2.3", "abc1234", runtime.Version(), runtime.GOOS + "/" + runtime.GOARCH} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, want it to contain %q", got, want)
		}
	}
}
