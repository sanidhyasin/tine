// Command tine-dirty changes a given amount of a VM's memory, and keeps it,
// so a benchmark controls how much memory a snapshot has to save.
//
//	tine-dirty -mb 64       fills 64 MiB with data and keeps it, in a daemon
//	tine-dirty again        has every tine-dirty daemon write all its pages again
//
// It's built for the guest and baked into tine's "bench" image.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/sanidhyasin/tine/internal/workload"
)

const dir = "/run/tine-dirty"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		if err := serve(os.Args[2], os.Args[3]); err != nil {
			fmt.Fprintln(os.Stderr, "tine-dirty:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "again" {
		if err := again(); err != nil {
			fmt.Fprintln(os.Stderr, "tine-dirty:", err)
			os.Exit(1)
		}
		return
	}
	mb := flag.Int("mb", 64, "MiB to change")
	flag.Parse()
	name := strconv.FormatInt(time.Now().UnixNano(), 10)
	start := time.Now()
	if err := workload.Daemon(filepath.Join(dir, name+".ready"), 2*time.Minute, "serve", strconv.Itoa(*mb), name); err != nil {
		fmt.Fprintln(os.Stderr, "tine-dirty:", err)
		os.Exit(1)
	}
	fmt.Printf("{\"mb\": %d, \"ms\": %.1f}\n", *mb, float64(time.Since(start).Microseconds())/1000)
}

// serve fills the memory and keeps it; at SIGUSR1 it writes every page again.
func serve(mbArg, name string) error {
	mb, err := strconv.Atoi(mbArg)
	if err != nil {
		return err
	}
	mem, err := workload.Alloc(mb)
	if err != nil {
		return err
	}
	seed := uint64(time.Now().UnixNano())
	workload.Fill(mem, seed)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGUSR1)
	if err := workload.WritePID(filepath.Join(dir, name+".pid")); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, name+".ready"), nil, 0o644); err != nil { //nolint:gosec // a marker
		return err
	}
	for range sig {
		seed++
		workload.Fill(mem, seed)
		_ = os.WriteFile(filepath.Join(dir, name+".again"), nil, 0o644) //nolint:gosec // a marker
	}
	return nil
}

// again signals every daemon and waits until each has written its pages.
func again() error {
	pids, err := filepath.Glob(filepath.Join(dir, "*.pid"))
	if err != nil || len(pids) == 0 {
		return fmt.Errorf("no tine-dirty running")
	}
	var marks []string
	for _, p := range pids {
		pid, err := workload.ReadPID(p)
		if err != nil {
			return err
		}
		mark := p[:len(p)-len(".pid")] + ".again"
		_ = os.Remove(mark)
		if err := syscall.Kill(pid, syscall.SIGUSR1); err != nil {
			return err
		}
		marks = append(marks, mark)
	}
	deadline := time.Now().Add(2 * time.Minute)
	for _, m := range marks {
		for {
			if _, err := os.Stat(m); err == nil {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("a daemon didn't finish within 2 minutes")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	return nil
}
