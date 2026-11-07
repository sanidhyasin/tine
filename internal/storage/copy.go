package storage

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
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

// ApplyDiff writes the data of diff, a sparse file, over dst at the same
// offsets, and leaves dst as it is wherever diff has a hole. It returns how
// many bytes it wrote. diff may be shorter than dst, but not longer.
//
// That's how a diff snapshot is merged onto the snapshot it was taken
// since: Firecracker writes only the pages that changed into the diff's
// memory file, at their offsets, and leaves holes for the rest.
//
// It's only right on a filesystem that reports holes, such as XFS or ext4.
// On one that doesn't, the holes read as data and zeros would be written
// over dst: CheckHoles tells them apart.
func ApplyDiff(dst, diff string) (int64, error) {
	in, err := os.Open(diff) //nolint:gosec // a file tined's own Firecracker wrote
	if err != nil {
		return 0, fmt.Errorf("apply diff: %w", err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY, 0) //nolint:gosec // as above
	if err != nil {
		return 0, fmt.Errorf("apply diff: %w", err)
	}
	written, err := applyDiff(out, in)
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return written, fmt.Errorf("apply diff %s to %s: %w", diff, dst, err)
	}
	return written, nil
}

func applyDiff(out, in *os.File) (int64, error) {
	inInfo, err := in.Stat()
	if err != nil {
		return 0, err
	}
	outInfo, err := out.Stat()
	if err != nil {
		return 0, err
	}
	size := inInfo.Size()
	if size > outInfo.Size() {
		return 0, fmt.Errorf("the diff is %d bytes, longer than the %d it applies to", size, outInfo.Size())
	}
	fd := int(in.Fd())
	buf := make([]byte, 1<<20)
	var written int64
	for off := int64(0); off < size; {
		start, err := unix.Seek(fd, off, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			break // nothing but holes from off to the end
		}
		if err != nil {
			return written, fmt.Errorf("seek data: %w", err)
		}
		end, err := unix.Seek(fd, start, unix.SEEK_HOLE) // the end of the file counts as a hole
		if err != nil {
			return written, fmt.Errorf("seek hole: %w", err)
		}
		end = min(end, size)
		for p := start; p < end; {
			n, err := in.ReadAt(buf[:min(int64(len(buf)), end-p)], p)
			if n > 0 {
				if _, werr := out.WriteAt(buf[:n], p); werr != nil {
					return written, werr
				}
				p += int64(n)
				written += int64(n)
			}
			if err != nil && (!errors.Is(err, io.EOF) || p < end) {
				return written, err
			}
		}
		off = end
	}
	return written, nil
}

// CheckHoles returns an error unless the filesystem dir is on reports the
// holes in sparse files, which ApplyDiff needs. It writes a small file the
// way Firecracker writes a diff, with data at both ends and a hole between,
// and checks the hole is seen once the file is reopened. It removes the file.
func CheckHoles(dir string) error {
	const size, page = 1 << 20, 4096
	f, err := os.CreateTemp(dir, ".holes-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() { _ = os.Remove(name) }()
	err = f.Truncate(size)
	for _, at := range []int64{0, size - page} {
		if err == nil {
			_, err = f.WriteAt(make([]byte, page), at)
		}
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	f, err = os.Open(name) //nolint:gosec // the file just written
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	fd := int(f.Fd())
	hole, err := unix.Seek(fd, 0, unix.SEEK_HOLE)
	if err != nil {
		return fmt.Errorf("%s: seek hole: %w", dir, err)
	}
	data, err := unix.Seek(fd, page, unix.SEEK_DATA)
	if err != nil {
		return fmt.Errorf("%s: seek data: %w", dir, err)
	}
	if hole != page || data != size-page {
		return fmt.Errorf("%s: the filesystem doesn't report the holes in sparse files (a hole at %d and data at %d, want %d and %d)",
			dir, hole, data, page, size-page)
	}
	return nil
}
