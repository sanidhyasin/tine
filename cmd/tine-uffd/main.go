// Command tine-uffd is a page server: it serves one Firecracker VM's
// memory from a snapshot's memory file, through userfaultfd (Firecracker's
// Uffd memory backend). tined starts one for each VM it restores this way,
// as the VM's own user and in the VM's cgroup, and hands it open files:
//
//	fd 3  the socket Firecracker connects to, already listening
//	fd 4  the snapshot's memory file, read-only
//	fd 5  a control socket to tined (optional)
//	fd 6  a working set to prefetch, with -prefetch
//
// It serves until Firecracker exits. On the control socket, tined can ask
// for "stats" (a JSON line) and the "working-set" faulted so far (its
// length in bytes on a line, then the bytes). If tined goes away, it keeps
// serving: the VM still needs its pages.
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	prefetch := flag.Bool("prefetch", false, "copy the working set on fd 6 in before the guest asks for it")
	cluster := flag.Int("cluster", 1, "pages to copy in for each fault, aligned around it")
	timeout := flag.Duration("timeout", defaultTimeout, "how long to wait for Firecracker to connect")
	flag.Parse()
	if err := run(*prefetch, *cluster, *timeout); err != nil {
		fmt.Fprintln(os.Stderr, "tine-uffd:", err)
		os.Exit(1)
	}
}
