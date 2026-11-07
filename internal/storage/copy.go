package storage

import (
	"fmt"
	"io"
	"os"
)

// Copy makes dst a full copy of src, byte for byte, through the page cache.
// dst must not exist yet, and is created with permissions perm.
//
// It's plain reads and writes on purpose. io.Copy would let the kernel copy
// the file with copy_file_range, which on XFS shares the data like Clone
// does. Copy is for when sharing isn't wanted: to measure the naive way of
// giving a VM its memory, or to have the copy's pages in the page cache
// straight away, ready for VMs to map.
func Copy(src, dst string, perm os.FileMode) (err error) {
	in, err := os.Open(src) //nolint:gosec // tined decides which files to copy; they aren't user input
	if err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm) //nolint:gosec // as above
	if err != nil {
		return fmt.Errorf("copy %s to %s: %w", src, dst, err)
	}
	defer func() {
		if closeErr := out.Close(); err == nil {
			err = closeErr
		}
		if err == nil {
			err = os.Chmod(dst, perm) // O_CREATE applies the umask
		}
		if err != nil {
			_ = os.Remove(dst)
			err = fmt.Errorf("copy %s to %s: %w", src, dst, err)
		}
	}()
	buf := make([]byte, 1<<20)
	for {
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}
