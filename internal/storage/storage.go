// Package storage places files for VMs: hard links for files that VMs share
// read-only, and reflink clones for files each VM gets its own copy of.
//
// Both need the source and destination on the same filesystem. In tine,
// that's the XFS data volume at /srv/tine, where clones are instant: the
// copy shares the original's data until one of them changes.
package storage

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// ErrReflinkUnsupported means the filesystem can't make reflink clones,
// for example because it isn't XFS or btrfs, or because the source and
// destination are on different filesystems.
var ErrReflinkUnsupported = errors.New("reflink clones aren't supported here")

// Link makes dst a hard link to src: the same file under a second name.
// dst must not exist yet.
func Link(src, dst string) error {
	if err := os.Link(src, dst); err != nil {
		if errors.Is(err, syscall.EXDEV) {
			return fmt.Errorf("hard link %s to %s: they're on different filesystems: %w", dst, src, err)
		}
		return fmt.Errorf("hard link %s to %s: %w", dst, src, err)
	}
	return nil
}

// Clone makes dst an independent copy of src that shares src's data on disk
// until either of them changes. It takes milliseconds whatever the file's
// size. dst must not exist yet, and is created with permissions perm.
//
// If the filesystem can't clone, Clone returns an error wrapping
// ErrReflinkUnsupported and leaves no dst behind. It never falls back to a
// slow full copy.
func Clone(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src) //nolint:gosec // tined decides which files to clone; they aren't user input
	if err != nil {
		return fmt.Errorf("clone %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm) //nolint:gosec // as above
	if err != nil {
		return fmt.Errorf("clone %s to %s: %w", src, dst, err)
	}

	err = cloneFile(out, in)
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("clone %s to %s: %w", src, dst, err)
	}
	// O_CREATE applies the umask; set exactly what was asked for.
	if err := os.Chmod(dst, perm); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("clone %s to %s: %w", src, dst, err)
	}
	return nil
}
