package vm

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// Page servers: a VM restored with the Uffd memory backend gets its memory
// from tine-uffd, which copies each page from the snapshot's memory file as
// the guest touches it (internal/uffd). Its pages are its own, not shared,
// so a page server costs the host more memory than mapping the file. In
// return, it can copy pages in before the guest asks: the fork strategy
// uffd-prefetch has the first child record the pages a restore touches,
// its working set, and the others copy those in as they start.
//
// tine-uffd runs as the VM's user, in the VM's cgroup, so its memory and
// processes count towards the VM's limits, and stopping the VM stops it.

// pageServerSocket is where the page server listens, inside the jail.
const pageServerSocket = "/uffd.sock"

// workingSetFile is the working set recorded for a snapshot, stored next
// to its other files.
const workingSetFile = "workingset"

// pageServerSpec is how a page server is started.
type pageServerSpec struct {
	UID, GID int
	// Cluster is how many pages it copies in for each fault.
	Cluster int
	// Prefetch is a working set file for it to copy in at once.
	Prefetch string
}

// pageServer is a running tine-uffd.
type pageServer interface {
	// workingSet asks for the pages the guest has faulted on so far, as an
	// encoded uffd.WorkingSet.
	workingSet(ctx context.Context) ([]byte, error)
	// release lets go of it: it serves on until the VM's Firecracker exits.
	release()
	// kill stops it, for a VM that didn't start.
	kill()
}

// pageServed reports whether a fork strategy restores children with a
// page server.
func (s ForkStrategy) pageServed() bool { return s == ForkUffd || s == ForkUffdPrefetch }

// saveWorkingSet stores a working set recorded for a snapshot. One already
// there is kept.
func (m *Manager) saveWorkingSet(snapID string, data []byte) error {
	path := m.snaps.Path(snapID, workingSetFile)
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	tmp, err := os.CreateTemp(m.snaps.Path(snapID, ""), ".workingset-")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o400) // only tined reads it, and hands it to page servers
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("save the working set of snapshot %s: %w", snapID, err)
	}
	return nil
}

// workingSetOf is the path of a snapshot's working set, or empty if it has
// none.
func (m *Manager) workingSetOf(snapID string) string {
	path := m.snaps.Path(snapID, workingSetFile)
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) || err != nil {
		return ""
	}
	return path
}
