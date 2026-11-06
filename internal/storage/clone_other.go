//go:build !linux

package storage

import "os"

// cloneFile isn't implemented outside Linux: tine's hosts are Linux.
func cloneFile(_, _ *os.File) error {
	return ErrReflinkUnsupported
}
