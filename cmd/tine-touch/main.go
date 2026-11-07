// Command tine-touch measures how much slower a VM's memory is right after
// a fork: the cost of the copy-on-write faults a forked child takes the
// first time it writes to a page it shares with its parent.
//
// In the parent, before the fork:
//
//	tine-touch start -mb 128     fills 128 MiB and keeps it, in a daemon
//
// In each child, after it:
//
//	tine-touch measure           writes one byte of every page, timing each,
//	                             and prints the result as JSON
//
// It's built for the guest and baked into tine's "bench" image.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sanidhyasin/tine/internal/workload"
)

const (
	pidFile    = "/run/tine-touch/pid"
	resultFile = "/run/tine-touch/result.json"
	readyFile  = "/run/tine-touch/ready"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: tine-touch start -mb N | measure [-read]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	mb := fs.Int("mb", 128, "MiB to fill")
	read := fs.Bool("read", false, "read each page instead of writing it")
	_ = fs.Parse(os.Args[2:])
	var err error
	switch os.Args[1] {
	case "start":
		err = workload.Daemon(readyFile, 2*time.Minute, "serve", "-mb", fmt.Sprint(*mb))
		if err == nil {
			fmt.Printf("{\"mb\": %d}\n", *mb)
		}
	case "serve":
		err = serve(*mb)
	case "measure":
		err = measure(*read)
	default:
		err = fmt.Errorf("no command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "tine-touch:", err)
		os.Exit(1)
	}
}

// serve fills the memory, then times a pass over it at every SIGUSR1
// (SIGUSR2 for reading).
func serve(mb int) error {
	mem, err := workload.Alloc(mb)
	if err != nil {
		return err
	}
	workload.Fill(mem, uint64(os.Getpid())) //nolint:gosec // any seed will do; a PID is positive
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGUSR1, syscall.SIGUSR2)
	if err := workload.WritePID(pidFile); err != nil {
		return err
	}
	if err := os.WriteFile(readyFile, nil, 0o644); err != nil { //nolint:gosec // a marker
		return err
	}
	for s := range sig {
		r := workload.Touch(mem, s == syscall.SIGUSR1)
		if err := workload.WriteJSON(resultFile, r); err != nil {
			return err
		}
	}
	return nil
}

func measure(read bool) error {
	pid, err := workload.ReadPID(pidFile)
	if err != nil {
		return fmt.Errorf("no tine-touch running (start it before the fork): %w", err)
	}
	_ = os.Remove(resultFile)
	sig := syscall.SIGUSR1
	if read {
		sig = syscall.SIGUSR2
	}
	if err := syscall.Kill(pid, sig); err != nil {
		return err
	}
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(resultFile); err == nil {
			_, err = os.Stdout.Write(data)
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("no result within 2 minutes")
}
