//go:build linux

package storage

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// cloneFile asks the kernel to share in's data with out (the FICLONE ioctl).
func cloneFile(out, in *os.File) error {
	err := unix.IoctlFileClone(int(out.Fd()), int(in.Fd()))
	if err == nil {
		return nil
	}
	if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EXDEV) || errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOTTY) {
		return fmt.Errorf("%w: %w", ErrReflinkUnsupported, err)
	}
	return err
}
