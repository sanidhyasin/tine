package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/sanidhyasin/tine/internal/api"
	"github.com/sanidhyasin/tine/internal/logging"
	"github.com/sanidhyasin/tine/internal/metrics"
	"github.com/sanidhyasin/tine/internal/state"
	"github.com/sanidhyasin/tine/internal/version"
	"github.com/sanidhyasin/tine/internal/vm"
)

// shutdownGrace is how long tined lets in-flight requests finish on stop.
const shutdownGrace = 30 * time.Second

type serveOptions struct {
	socket        string
	socketGroup   string
	metricsListen string
	dbPath        string
	logLevel      string
	vm            vm.Config
}

func newServeCmd() *cobra.Command {
	opts := serveOptions{vm: vm.DefaultConfig()}
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the tine service",
		Long: `Run the tine service: it owns every VM on this host and serves tine's API on
a Unix socket. It runs as root, normally under systemd (deploy/tined.service).

VMs keep running when tined stops or restarts: each one's Firecracker lives in
its own cgroup, outside tined's.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return serve(cmd.Context(), opts) },
	}
	f := cmd.Flags()
	f.StringVar(&opts.socket, "socket", api.DefaultSocket, "Unix socket to serve the API on")
	f.StringVar(&opts.socketGroup, "socket-group", "tine", "group allowed to use the socket (if it exists); everyone else is refused")
	f.StringVar(&opts.metricsListen, "metrics-listen", "", `also serve /metrics over TCP for Prometheus, e.g. "127.0.0.1:9101" (always on the socket)`)
	f.StringVar(&opts.dbPath, "db", state.DefaultPath, "state database")
	f.StringVar(&opts.logLevel, "log-level", "info", "debug, info, warn or error")
	f.StringVar(&opts.vm.ImagesDir, "images-dir", opts.vm.ImagesDir, "where images are stored")
	f.StringVar(&opts.vm.ChrootBaseDir, "jail-dir", opts.vm.ChrootBaseDir, "where VM jails are built (same filesystem as --images-dir)")
	f.StringVar(&opts.vm.SnapshotsDir, "snapshots-dir", opts.vm.SnapshotsDir, "where snapshots are stored (same filesystem as --images-dir)")
	f.IntVar(&opts.vm.MaxSnapshotsPerVM, "max-snapshots-per-vm", opts.vm.MaxSnapshotsPerVM, "how many snapshots one VM may have")
	f.IntVar(&opts.vm.MinFreeMiB, "min-free-mib", opts.vm.MinFreeMiB, "disk space, in MiB, that taking a snapshot must leave free")
	f.StringSliceVar(&opts.vm.WarmImages, "warm-images", opts.vm.WarmImages,
		"images to keep a template of at the default size, warming it again when it's out of date (empty: none)")
	f.Float64Var(&opts.vm.OvercommitRatio, "overcommit", opts.vm.OvercommitRatio,
		"the most guest memory all VMs together may have, as a multiple of the host's memory")
	f.Float64Var(&opts.vm.CPUOvercommitRatio, "cpu-overcommit", opts.vm.CPUOvercommitRatio,
		"the most vCPUs all VMs together may have, per host CPU")
	f.IntVar(&opts.vm.MinFreeMemoryMiB, "min-free-memory-mib", opts.vm.MinFreeMemoryMiB,
		"host memory, in MiB, that new VMs must leave available")
	f.IntVar(&opts.vm.MaxForkChildren, "max-fork-children", opts.vm.MaxForkChildren, "the most children one fork may make")
	f.IntVar(&opts.vm.ForkParallelism, "fork-parallelism", opts.vm.ForkParallelism, "how many of a fork's children are restored at once")
	f.BoolVar(&opts.vm.ClockRealtime, "clock-realtime", opts.vm.ClockRealtime,
		"have Firecracker advance a restored guest's clock (tine-agent sets it either way)")
	f.BoolVar(&opts.vm.DiffSnapshots, "diff-snapshots", opts.vm.DiffSnapshots,
		"track the pages VMs change, so snapshots after a VM's first save only those (Firecracker developer preview)")
	f.StringVar(&opts.vm.DiffMerge, "diff-merge", opts.vm.DiffMerge,
		`how a diff becomes a whole snapshot: "reflink" the base, or "copy" it into the page cache (default: copy for forks, reflink otherwise)`)
	f.StringVar(&opts.vm.JailerBin, "jailer", opts.vm.JailerBin, "jailer binary")
	f.StringVar(&opts.vm.FirecrackerBin, "firecracker", opts.vm.FirecrackerBin, "firecracker binary (same release as the jailer)")
	f.StringVar(&opts.vm.CgroupParent, "cgroup-parent", opts.vm.CgroupParent, "cgroup holding every VM's cgroup")
	f.IntVar(&opts.vm.MaxVMs, "max-vms", opts.vm.MaxVMs, "most VMs at once")
	f.IntVar(&opts.vm.DefaultVCPUs, "default-vcpus", opts.vm.DefaultVCPUs, "vCPUs when a request doesn't say")
	f.IntVar(&opts.vm.DefaultMemoryMiB, "default-memory-mib", opts.vm.DefaultMemoryMiB, "memory when a request doesn't say")
	f.DurationVar(&opts.vm.DefaultTTL, "default-ttl", opts.vm.DefaultTTL, "how long VMs live when a request doesn't say")
	f.DurationVar(&opts.vm.ReaperInterval, "reaper-interval", opts.vm.ReaperInterval, "how often to destroy VMs whose time is up")
	f.DurationVar(&opts.vm.DestroyedRetention, "keep-destroyed", opts.vm.DestroyedRetention, "how long to keep records of destroyed VMs")
	return cmd
}

func serve(ctx context.Context, opts serveOptions) error {
	log, err := newLogger(opts.logLevel)
	if err != nil {
		return err
	}
	if os.Geteuid() != 0 {
		return errors.New("tined serve must run as root: it creates network namespaces, jails and cgroups")
	}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, os.Interrupt)
	defer stop()

	store, err := state.Open(ctx, opts.dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	met := metrics.New(version.Version)
	met.WatchVMStates([]string{"creating", "running", "destroying", "failed"}, func() (map[string]int, error) {
		live, err := store.ListVMs(context.Background(), state.ListOptions{})
		counts := map[string]int{}
		for _, v := range live {
			counts[string(v.State)]++
		}
		return counts, err
	})
	opts.vm.Log = log
	opts.vm.Metrics = met
	manager, err := vm.New(opts.vm, store)
	if err != nil {
		return err
	}
	if err := manager.Setup(ctx); err != nil {
		return fmt.Errorf("set up the host: %w", err)
	}
	// Before serving: bring the records and the host back in line.
	report := manager.Reconcile(ctx)
	log.Info("reconciled", "report", report.String())
	for _, e := range report.Errors {
		log.Error("reconcile", "err", e)
	}
	go manager.RunReaper(ctx, opts.vm.ReaperInterval)

	ln, err := listenSocket(ctx, opts.socket, opts.socketGroup, log)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           api.NewServer(manager, version.Version, log, api.WithMetrics(met), api.WithFeatures(features(opts.vm)...)),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	errc := make(chan error, 2)
	go func() { errc <- srv.Serve(ln) }()

	// Prometheus can't scrape a Unix socket, so it can have a TCP port too.
	var metricsSrv *http.Server
	if opts.metricsListen != "" {
		var lc net.ListenConfig
		mln, err := lc.Listen(ctx, "tcp", opts.metricsListen)
		if err != nil {
			return fmt.Errorf("--metrics-listen: %w", err)
		}
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", met.Handler())
		metricsSrv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() { errc <- metricsSrv.Serve(mln) }()
		log.Info("serving metrics over TCP", "addr", mln.Addr().String())
	}

	log.Info("tined ready", "version", version.String(), "socket", opts.socket, "db", opts.dbPath)
	notifySystemd("READY=1\nSTATUS=serving on " + opts.socket)

	select {
	case err := <-errc:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}
	log.Info("stopping: finishing in-flight requests; VMs keep running")
	notifySystemd("STOPPING=1")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("some requests didn't finish in time", "err", err)
	}
	if metricsSrv != nil {
		_ = metricsSrv.Shutdown(shutdownCtx)
	}
	log.Info("stopped")
	return nil
}

// listenSocket listens on a Unix socket that only root and socketGroup may use.
func listenSocket(ctx context.Context, path, socketGroup string, log *slog.Logger) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // the directory must be traversable; the socket's own mode guards it
		return nil, err
	}
	// A socket left by a tined that didn't exit cleanly would block the listen.
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
		_ = os.Remove(path)
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	mode := os.FileMode(0o600)
	if g, err := user.LookupGroup(socketGroup); err == nil {
		gid, _ := strconv.Atoi(g.Gid)
		if err := os.Chown(path, 0, gid); err != nil {
			_ = ln.Close()
			return nil, err
		}
		mode = 0o660
	} else {
		log.Warn("socket group doesn't exist; only root can use the socket", "group", socketGroup)
	}
	if err := os.Chmod(path, mode); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return ln, nil
}

func newLogger(level string) (*slog.Logger, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("--log-level %q: %w", level, err)
	}
	// JSON on stderr: under systemd it goes to the journal. Lines also get
	// the request and VM IDs that their context carries.
	return slog.New(logging.NewHandler(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: l}))), nil
}

// notifySystemd tells systemd about tined's state (sd_notify), when it runs
// as a Type=notify service. Outside systemd it does nothing.
func notifySystemd(msg string) {
	addr := os.Getenv("NOTIFY_SOCKET")
	if addr == "" {
		return
	}
	if addr[0] == '@' {
		addr = "\x00" + addr[1:] // an abstract socket
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: addr, Net: "unixgram"})
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	_, _ = conn.Write([]byte(msg))
}

// features are the optional behaviours GET /v1/health reports.
func features(c vm.Config) []string {
	var f []string
	if c.DiffSnapshots {
		f = append(f, "diff-snapshots")
	}
	return f
}
