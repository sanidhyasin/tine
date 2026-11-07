//go:build linux

package vm

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// enableKSM makes all of tined's memory mergeable by KSM. The setting goes
// with the process: into children, and across exec (Linux 6.7 and later),
// so every jailer, and the Firecracker it becomes, has it too.
func enableKSM() error {
	if err := unix.Prctl(unix.PR_SET_MEMORY_MERGE, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("ksm: PR_SET_MEMORY_MERGE: %w", err)
	}
	return nil
}
