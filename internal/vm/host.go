package vm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sanidhyasin/tine/internal/agentproto"
	"github.com/sanidhyasin/tine/internal/firecracker"
	"github.com/sanidhyasin/tine/internal/jailer"
	"github.com/sanidhyasin/tine/internal/netns"
	"github.com/sanidhyasin/tine/internal/snapshot"
	"github.com/sanidhyasin/tine/internal/storage"
)

// host is everything the manager does to the machine. Production uses
// hostOS; tests use a fake that records calls and fails on demand.
type host interface {
	setup(ctx context.Context) error
	createNetwork(ctx context.Context, n netns.Network, owner netns.Owner) error
	destroyNetwork(n netns.Network) error
	prepareJail(j *jailer.Jail) error
	cleanupJail(j *jailer.Jail) error
	startFirecracker(j *jailer.Jail) (process, error)
	configureAndBoot(ctx context.Context, j *jailer.Jail, boot bootSpec) error
	waitAgent(ctx context.Context, j *jailer.Jail) error
	// killFirecracker stops a Firecracker that a previous tined started,
	// knowing only its PID.
	killFirecracker(ctx context.Context, j *jailer.Jail, pid int) error
	agent(j *jailer.Jail) *agentproto.Client

	// For snapshots:
	// prepareSnapshot gives the jail an empty directory for a snapshot.
	prepareSnapshot(j *jailer.Jail) error
	pause(ctx context.Context, j *jailer.Jail) error
	resume(ctx context.Context, j *jailer.Jail) error
	// paused reports whether the VM's Firecracker has it paused.
	paused(ctx context.Context, j *jailer.Jail) (bool, error)
	// writeSnapshot has Firecracker write the paused VM's state and memory
	// into that directory: all of its memory, or with diff, only the pages
	// it changed since its last snapshot or restore.
	writeSnapshot(ctx context.Context, j *jailer.Jail, diff bool) error
	// cloneDisk makes dst a reflink copy of the VM's disk.
	cloneDisk(j *jailer.Jail, dst string) error
	// collectSnapshot moves the state and memory Firecracker wrote into b.
	// For a diff, it makes b's memory from merge.Base with the diff written
	// over it, and returns how many bytes the diff held.
	collectSnapshot(j *jailer.Jail, b *snapshot.Building, merge diffMerge) (int64, error)
	// flushSnapshot writes a snapshot file that's still in the page cache
	// out to disk.
	flushSnapshot(path string) error
	// hostInfo says what this host can restore: the Firecracker that new VMs
	// run, and this host's CPU and kernel.
	hostInfo(ctx context.Context) (snapshot.Host, error)
	// loadSnapshot has a new Firecracker load the snapshot in the VM's jail
	// (restoreStatePath, restoreMemPath) and resume it.
	loadSnapshot(ctx context.Context, j *jailer.Jail) error
	// syncGuest makes the guest current and unique, through its agent.
	syncGuest(ctx context.Context, j *jailer.Jail, req agentproto.SyncRequest) (agentproto.SyncResult, error)
	// memory reads how much host memory the VM's Firecracker uses.
	memory(j *jailer.Jail) (Memory, error)
	// prefetch reads a file into the page cache.
	prefetch(path string) error
	// hostMemory reads the host's total and available memory.
	hostMemory() (hostMemory, error)
	// describeHost says what a snapshot of the VM would depend on: its
	// Firecracker version, and this host's CPU and kernel.
	describeHost(ctx context.Context, j *jailer.Jail) (snapshot.Host, error)

	// For Reconcile:
	// inventory lists the VM resources that exist on the host.
	inventory(ctx context.Context) (inventory, error)
	// alive reports whether anything runs in the VM's cgroup.
	alive(ctx context.Context, j *jailer.Jail) (bool, error)
	// removeOrphan removes everything of a VM known only by its ID: its
	// processes, jail, cgroup and network namespace.
	removeOrphan(ctx context.Context, id string) error
	// removeVeth removes a host veth.
	removeVeth(name string) error
}

// process is a running Firecracker.
type process interface {
	pid() int
	kill() error
	// done is closed once the process has exited.
	done() <-chan struct{}
	// exitErr is how it exited; valid once done is closed.
	exitErr() error
}

// bootSpec is what the VM boots with.
type bootSpec struct {
	VCPUs     int
	MemoryMiB int
	BootArgs  string
}

// hostOS is the real host.
type hostOS struct {
	cfg Config

	mu      sync.Mutex
	fcInfo  snapshot.Host // what hostInfo found last time
	fcStamp string        // the Firecracker binary's size and mtime then
}

func (h *hostOS) setup(ctx context.Context) error {
	if err := h.cfg.jailerConfig().EnsureCgroupParent(); err != nil {
		return fmt.Errorf("cgroups: %w", err)
	}
	// Snapshots stall VMs on a host that throttles dirty page cache early
	// (scripts/host-setup.sh explains).
	if data, err := os.ReadFile("/proc/sys/vm/dirty_ratio"); err == nil {
		if ratio, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && ratio < 40 {
			h.cfg.Log.Warn("vm.dirty_ratio is low: snapshots of big VMs will pause them for long, and stall them after; "+
				"scripts/host-setup.sh sets 60", "dirty_ratio", ratio)
		}
	}
	if err := os.MkdirAll(h.cfg.ChrootBaseDir, 0o700); err != nil {
		return err
	}
	if h.cfg.DiffSnapshots {
		if err := storage.CheckHoles(h.cfg.SnapshotsDir); err != nil {
			return fmt.Errorf("diff snapshots need a filesystem that reports holes: %w", err)
		}
	}
	return netns.Setup(ctx)
}

func (h *hostOS) createNetwork(ctx context.Context, n netns.Network, owner netns.Owner) error {
	return netns.Create(ctx, n, owner)
}

func (h *hostOS) destroyNetwork(n netns.Network) error { return netns.Destroy(n) }

func (h *hostOS) prepareJail(j *jailer.Jail) error {
	if err := j.Prepare(); err != nil {
		return err
	}
	return j.CreateFile(firecrackerLogPath)
}

func (h *hostOS) cleanupJail(j *jailer.Jail) error { return j.Cleanup() }

func (h *hostOS) startFirecracker(j *jailer.Jail) (process, error) {
	// The guest's console goes to a file, not a pipe, so it doesn't depend
	// on tined staying up.
	console, err := os.OpenFile(j.ConsoleLogPath(), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer func() { _ = console.Close() }() // the child keeps its own copy
	cmd := j.Command()
	cmd.Stdout, cmd.Stderr = console, console
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start jailer: %w", err)
	}
	p := &osProcess{cmd: cmd, exited: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		close(p.exited)
	}()
	return p, nil
}

// firecracker returns a client for the VM's Firecracker that records every call in the metrics.
func (h *hostOS) firecracker(j *jailer.Jail) *firecracker.Client {
	return firecracker.New(j.APISocket(), firecracker.WithObserver(func(ci firecracker.CallInfo) {
		h.cfg.Metrics.FirecrackerCall(ci.Endpoint, ci.Duration, ci.Err)
	}))
}

func (h *hostOS) configureAndBoot(ctx context.Context, j *jailer.Jail, boot bootSpec) error {
	c := h.firecracker(j)
	readyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := c.WaitReady(readyCtx, 2*time.Millisecond); err != nil {
		return err
	}
	steps := []struct {
		what string
		do   func() error
	}{
		{"logger", func() error {
			return c.PutLogger(ctx, firecracker.Logger{LogPath: firecrackerLogPath, Level: firecracker.LogLevelInfo})
		}},
		{"machine", func() error {
			return c.PutMachineConfig(ctx, firecracker.MachineConfig{VCPUCount: boot.VCPUs, MemSizeMiB: boot.MemoryMiB,
				TrackDirtyPages: h.cfg.DiffSnapshots})
		}},
		{"kernel", func() error {
			return c.PutBootSource(ctx, firecracker.BootSource{KernelImagePath: kernelPath, BootArgs: boot.BootArgs})
		}},
		{"disk", func() error {
			return c.PutDrive(ctx, firecracker.Drive{DriveID: "rootfs", PathOnHost: rootfsPath, IsRootDevice: true})
		}},
		{"network", func() error {
			return c.PutNetworkInterface(ctx, firecracker.NetworkInterface{IfaceID: "eth0", HostDevName: netns.TapDevice, GuestMAC: netns.GuestMAC})
		}},
		{"vsock", func() error { return c.PutVsock(ctx, firecracker.Vsock{GuestCID: guestCID, UDSPath: vsockPath}) }},
		{"entropy", func() error { return c.PutEntropy(ctx, firecracker.EntropyDevice{}) }},
		{"start", func() error { return c.StartInstance(ctx) }},
	}
	for _, s := range steps {
		if err := s.do(); err != nil {
			return fmt.Errorf("%s: %w", s.what, err)
		}
	}
	return nil
}

func (h *hostOS) prepareSnapshot(j *jailer.Jail) error {
	// Anything left from a snapshot that didn't finish goes first.
	if err := os.RemoveAll(j.HostPath(snapshotDir)); err != nil {
		return err
	}
	return j.MkdirAll(snapshotDir)
}

func (h *hostOS) pause(ctx context.Context, j *jailer.Jail) error { return h.firecracker(j).Pause(ctx) }
func (h *hostOS) resume(ctx context.Context, j *jailer.Jail) error {
	return h.firecracker(j).Resume(ctx)
}

func (h *hostOS) paused(ctx context.Context, j *jailer.Jail) (bool, error) {
	info, err := h.firecracker(j).DescribeInstance(ctx)
	return info.State == firecracker.StatePaused, err
}

func (h *hostOS) writeSnapshot(ctx context.Context, j *jailer.Jail, diff bool) error {
	typ := firecracker.SnapshotFull
	if diff {
		typ = firecracker.SnapshotDiff
	}
	return h.firecracker(j).CreateSnapshot(ctx, firecracker.SnapshotCreate{
		SnapshotPath: snapshotStatePath,
		MemFilePath:  snapshotMemPath,
		SnapshotType: typ,
		// Snapshots needn't survive the host crashing, and waiting for the
		// disk would keep the VM paused for longer.
		SyncSnapshotFiles: firecracker.Bool(false),
	})
}

func (h *hostOS) cloneDisk(j *jailer.Jail, dst string) error {
	return storage.Clone(j.HostPath(rootfsPath), dst, 0o600)
}

func (h *hostOS) collectSnapshot(j *jailer.Jail, b *snapshot.Building, merge diffMerge) (int64, error) {
	// The jail and the store are on the same filesystem, so these are renames.
	if err := os.Rename(j.HostPath(snapshotStatePath), b.Path(snapshot.StateFile)); err != nil {
		return 0, err
	}
	mem, dst := j.HostPath(snapshotMemPath), b.Path(snapshot.MemoryFile)
	if merge.Base == "" {
		return 0, os.Rename(mem, dst)
	}
	var err error
	if merge.Copy {
		err = storage.Copy(merge.Base, dst, 0o600)
	} else {
		err = storage.Clone(merge.Base, dst, 0o600)
	}
	if err != nil {
		return 0, fmt.Errorf("diff snapshot: base: %w", err)
	}
	n, err := storage.ApplyDiff(dst, mem)
	if err != nil {
		return n, fmt.Errorf("diff snapshot: %w", err)
	}
	// The diff and the link to the base are done with.
	if err := os.Remove(mem); err != nil {
		return n, err
	}
	return n, os.Remove(merge.Base)
}

func (h *hostOS) flushSnapshot(path string) error {
	f, err := os.Open(path) //nolint:gosec // a path in tined's own snapshot store
	if err != nil {
		return err
	}
	err = f.Sync()
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}

func (h *hostOS) hostInfo(ctx context.Context) (snapshot.Host, error) {
	fi, err := os.Stat(h.cfg.FirecrackerBin)
	if err != nil {
		return snapshot.Host{}, err
	}
	// Asking the binary takes a few milliseconds; do it again only if it changed.
	stamp := fmt.Sprintf("%d %d", fi.Size(), fi.ModTime().UnixNano())
	h.mu.Lock()
	if h.fcStamp == stamp {
		info := h.fcInfo
		h.mu.Unlock()
		return info, nil
	}
	h.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, h.cfg.FirecrackerBin, "--version").Output() //nolint:gosec // tined's configured Firecracker
	if err != nil {
		return snapshot.Host{}, fmt.Errorf("%s --version: %w", h.cfg.FirecrackerBin, err)
	}
	// The first line is "Firecracker v1.17.0"; GET /version says "1.17.0".
	first, _, _ := strings.Cut(string(out), "\n")
	version := strings.TrimPrefix(strings.TrimSpace(strings.TrimPrefix(first, "Firecracker")), "v")
	if version == "" {
		return snapshot.Host{}, fmt.Errorf("%s --version: no version in %q", h.cfg.FirecrackerBin, first)
	}
	info, err := snapshot.ThisHost(version)
	if err != nil {
		return snapshot.Host{}, err
	}
	h.mu.Lock()
	h.fcInfo, h.fcStamp = info, stamp
	h.mu.Unlock()
	return info, nil
}

func (h *hostOS) loadSnapshot(ctx context.Context, j *jailer.Jail) error {
	c := h.firecracker(j)
	readyCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := c.WaitReady(readyCtx, 2*time.Millisecond); err != nil {
		return err
	}
	// Only the logger and metrics may be set up before a load.
	if err := c.PutLogger(ctx, firecracker.Logger{LogPath: firecrackerLogPath, Level: firecracker.LogLevelInfo}); err != nil {
		return fmt.Errorf("logger: %w", err)
	}
	if err := c.LoadSnapshot(ctx, firecracker.SnapshotLoad{
		SnapshotPath:    restoreStatePath,
		MemBackend:      firecracker.MemoryBackend{BackendType: firecracker.MemoryBackendFile, BackendPath: restoreMemPath},
		ResumeVM:        true,
		ClockRealtime:   h.cfg.ClockRealtime,
		TrackDirtyPages: h.cfg.DiffSnapshots,
	}); err != nil {
		return fmt.Errorf("load: %w", err)
	}
	return nil
}

func (h *hostOS) syncGuest(ctx context.Context, j *jailer.Jail, req agentproto.SyncRequest) (agentproto.SyncResult, error) {
	res, err := h.agent(j).Sync(ctx, req)
	var ae *agentproto.Error
	if errors.As(err, &ae) && ae.StatusCode == http.StatusNotFound {
		return res, fmt.Errorf("the image's tine-agent is too old to sync the guest; import the image again: %w", err)
	}
	return res, err
}

func (h *hostOS) hostMemory() (hostMemory, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return hostMemory{}, err
	}
	var mem hostMemory
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		kb, err := strconv.Atoi(f[1])
		if err != nil {
			continue
		}
		switch f[0] {
		case "MemTotal:":
			mem.TotalMiB = kb >> 10
		case "MemAvailable:":
			mem.AvailableMiB = kb >> 10
		}
	}
	if mem.TotalMiB == 0 {
		return hostMemory{}, errors.New("no MemTotal in /proc/meminfo")
	}
	mem.CPUs = runtime.NumCPU()
	return mem, nil
}

func (h *hostOS) prefetch(path string) error {
	f, err := os.Open(path) //nolint:gosec // a path in tined's own snapshot store
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = io.Copy(io.Discard, f)
	return err
}

func (h *hostOS) memory(j *jailer.Jail) (Memory, error) {
	// The process in the VM's cgroup is its Firecracker, whoever started it.
	procs, err := os.ReadFile(filepath.Join(j.CgroupDir(), "cgroup.procs"))
	if err != nil {
		return Memory{}, err
	}
	pid, _, _ := strings.Cut(strings.TrimSpace(string(procs)), "\n")
	if pid == "" {
		return Memory{}, errors.New("nothing runs in the VM's cgroup")
	}
	data, err := os.ReadFile(filepath.Join("/proc", pid, "smaps_rollup")) //nolint:gosec // a PID read from the cgroup
	if err != nil {
		return Memory{}, err
	}
	return parseSmapsRollup(string(data)), nil
}

func (h *hostOS) describeHost(ctx context.Context, j *jailer.Jail) (snapshot.Host, error) {
	version, err := h.firecracker(j).Version(ctx)
	if err != nil {
		return snapshot.Host{}, err
	}
	return snapshot.ThisHost(version)
}

func (h *hostOS) waitAgent(ctx context.Context, j *jailer.Jail) error {
	_, err := h.agent(j).WaitHealthy(ctx, 10*time.Millisecond)
	return err
}

func (h *hostOS) agent(j *jailer.Jail) *agentproto.Client {
	return agentproto.NewVsockClient(j.HostPath(vsockPath))
}

// killFirecracker stops a VM's Firecracker that this tined didn't start.
//
// It doesn't trust the recorded PID, which the kernel may have given to
// another process since. It kills whatever is in the VM's own cgroup
// instead: the jailer put Firecracker there, and nothing else ever goes in
// it, so everything there is that VM's and nothing else is touched.
func (h *hostOS) killFirecracker(ctx context.Context, j *jailer.Jail, _ int) error {
	procsFile := filepath.Join(j.CgroupDir(), "cgroup.procs")
	procs := func() ([]byte, error) {
		data, err := os.ReadFile(procsFile) //nolint:gosec // a cgroup path built from a validated VM ID
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil // no cgroup: nothing of the VM's is running
		}
		return bytes.TrimSpace(data), err
	}
	left, err := procs()
	if err != nil || len(left) == 0 {
		return err
	}
	// cgroup.kill (Linux 5.14+) SIGKILLs every process in the cgroup at once.
	if err := os.WriteFile(filepath.Join(j.CgroupDir(), "cgroup.kill"), []byte("1"), 0o600); err != nil {
		return fmt.Errorf("kill the processes in %s: %w", j.CgroupDir(), err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		left, err := procs()
		if err != nil || len(left) == 0 {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("processes %s in %s survived SIGKILL for 10s", strings.Join(strings.Fields(string(left)), ","), j.CgroupDir())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// osProcess is a Firecracker started by this tined.
type osProcess struct {
	cmd    *exec.Cmd
	exited chan struct{}
	err    error
}

func (p *osProcess) pid() int              { return p.cmd.Process.Pid }
func (p *osProcess) kill() error           { return p.cmd.Process.Kill() }
func (p *osProcess) done() <-chan struct{} { return p.exited }
func (p *osProcess) exitErr() error        { return p.err }

func encodeJSON(v any) (json.RawMessage, error) {
	data, err := json.Marshal(v)
	return json.RawMessage(data), err
}

func (h *hostOS) inventory(context.Context) (inventory, error) {
	seen := map[string]bool{}
	var inv inventory
	add := func(ids []string) {
		for _, id := range ids {
			if !seen[id] {
				seen[id] = true
				inv.ids = append(inv.ids, id)
			}
		}
	}
	nets, veths, err := netns.List()
	if err != nil {
		return inv, fmt.Errorf("network namespaces: %w", err)
	}
	add(nets)
	inv.veths = veths
	jails, err := h.cfg.jailerConfig().ListIDs()
	if err != nil {
		return inv, fmt.Errorf("jails: %w", err)
	}
	add(jails)
	cgroups, err := h.cfg.jailerConfig().CgroupIDs()
	if err != nil {
		return inv, fmt.Errorf("cgroups: %w", err)
	}
	add(cgroups)
	return inv, nil
}

func (h *hostOS) alive(_ context.Context, j *jailer.Jail) (bool, error) { return j.HasProcesses() }

func (h *hostOS) removeOrphan(ctx context.Context, id string) error {
	j, err := jailer.Existing(h.cfg.jailerConfig(), id)
	if err != nil {
		return err
	}
	if err := h.killFirecracker(ctx, j, 0); err != nil {
		return err
	}
	if err := j.Cleanup(); err != nil {
		return err
	}
	return netns.DestroyNamespace(id)
}

func (h *hostOS) removeVeth(name string) error { return netns.DeleteVeth(name) }
