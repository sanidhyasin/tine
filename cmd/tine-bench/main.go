// Command tine-bench measures tined: it runs scenarios through tined's API
// and writes one JSON line per run, after one describing the host, for
// bench/analyze.py to turn into charts and tables.
//
// It runs on the host, as root (to empty the page cache, and read tined's
// socket). make bench runs the standard suite on the dev server and fetches
// the results.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sanidhyasin/tine/internal/api"
)

const usage = `tine-bench [flags] SCENARIO...

Scenarios:
  create-cold      boot a VM and destroy it, one at a time
  create-template  restore a VM from its image's template, one at a time
  exec             run true in a VM
  snapshot         snapshot VMs of each --snapshot-memory
  rollback         roll a VM back to a snapshot
  fork             fork a fresh VM into each --children, with each --strategy
  fork-touch       fork, then time each child writing to its parent's memory
  fork-dirty       fork after the parent changes each --dirty MiB
  density          fork until admission control refuses
  suite            all of the above, with the defaults

Flags:
`

func main() {
	var (
		socket     = flag.String("socket", api.DefaultSocket, "tined's API socket")
		outPath    = flag.String("out", "", "file to write results to (default: stdout)")
		appendOut  = flag.Bool("append", false, "add to -out instead of replacing it")
		runs       = flag.Int("runs", 100, "runs per data point, after the warm-up")
		warmup     = flag.Int("warmup", 3, "warm-up runs per data point, recorded but left out of the analysis")
		forkRuns   = flag.Int("fork-runs", 10, "runs per fork data point (each boots a fresh parent)")
		snapRuns   = flag.Int("snapshot-runs", 10, "runs per snapshot data point (each waits for the last to reach the disk)")
		children   = flag.String("children", "1,10,30", "numbers of children to fork")
		strategies = flag.String("strategy", "share", "fork strategies to compare")
		forkMemory = flag.Int("fork-memory", 256, "the fork parent's memory, in MiB")
		snapMemory = flag.String("snapshot-memory", "256,1024,4096", "VM sizes to snapshot, in MiB")
		dirty      = flag.String("dirty", "0,64,256", "MiB the parent changes before fork-dirty")
		touch      = flag.Int("touch", 128, "MiB the parent fills before fork-touch, which each child then writes")
		benchImage = flag.String("image", "bench", "image with tine-touch and tine-dirty, for fork-touch and fork-dirty")
		drop       = flag.Bool("drop-caches", false, "empty the page cache before each create or fork, as after a reboot")
		ttl        = flag.Int("ttl", 1800, "time to live of every VM made, in seconds")
	)
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage); flag.PrintDefaults() }
	flag.Parse()
	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	out := os.Stdout
	if *outPath != "" {
		mode := os.O_TRUNC
		if *appendOut {
			mode = os.O_APPEND
		}
		f, err := os.OpenFile(*outPath, os.O_WRONLY|os.O_CREATE|mode, 0o644) //nolint:gosec // results are meant to be read
		if err != nil {
			fmt.Fprintln(os.Stderr, "tine-bench:", err)
			os.Exit(1)
		}
		defer func() { _ = f.Close() }()
		out = f
	}
	c := api.NewUnixClient(*socket)
	if _, err := c.Health(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "tine-bench: tined isn't answering:", err)
		os.Exit(1)
	}
	b := &bench{c: c, out: newWriter(out), log: os.Stderr, runs: *runs, warmup: *warmup, dropCaches: *drop, ttl: *ttl}
	host := describeHost(ctx, c)
	b.out.write(Record{Kind: "host", OK: true, Host: host})
	fmt.Fprintf(os.Stderr, "host: %s (%s), %s, %d CPUs, %d MiB, virtualization %s, disk %s; tine %s, %s\n",
		host.Hostname, host.InstanceType, host.CPUModel, host.CPUs, host.MemoryMiB, host.Virtualization, host.DataDisk, host.Tine, host.Firecracker)

	scenarios := flag.Args()
	if len(scenarios) == 1 && scenarios[0] == "suite" {
		scenarios = []string{"create-cold", "create-template", "exec", "rollback", "snapshot", "fork", "fork-touch", "fork-dirty", "density"}
	}
	start := time.Now()
	for _, s := range scenarios {
		if ctx.Err() != nil {
			break
		}
		switch s {
		case "create-cold":
			b.create(ctx, true, 2, 1024)
		case "create-template":
			b.create(ctx, false, 2, 1024)
		case "exec":
			b.exec(ctx)
		case "rollback":
			b.rollback(ctx)
		case "snapshot":
			for _, m := range ints(*snapMemory) {
				b.snapshot(ctx, m, *snapRuns)
			}
		case "fork":
			for _, st := range strings.Split(*strategies, ",") {
				for _, n := range ints(*children) {
					b.fork(ctx, forkOptions{Children: n, VCPUs: 1, MemoryMiB: *forkMemory, Strategy: st}, *forkRuns)
				}
			}
		case "fork-touch":
			for _, st := range strings.Split(*strategies, ",") {
				b.fork(ctx, forkOptions{Children: 10, VCPUs: 1, MemoryMiB: *forkMemory, Strategy: st, TouchMiB: *touch, Image: *benchImage}, *forkRuns)
			}
		case "fork-dirty":
			for _, st := range strings.Split(*strategies, ",") {
				for _, d := range ints(*dirty) {
					b.fork(ctx, forkOptions{Children: 1, VCPUs: 1, MemoryMiB: *forkMemory, Strategy: st, DirtyMiB: d, Image: *benchImage}, *forkRuns)
				}
			}
		case "density":
			b.density(ctx, 1, *forkMemory)
		default:
			fmt.Fprintf(os.Stderr, "tine-bench: no scenario %q\n", s)
			os.Exit(2)
		}
	}
	fmt.Fprintf(os.Stderr, "done in %v\n", time.Since(start).Round(time.Second))
}

func ints(s string) []int {
	var out []int
	for _, f := range strings.Split(s, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(f)); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func syncAll() { syscall.Sync() }
