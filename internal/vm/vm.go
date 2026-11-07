// Package vm runs VMs: it creates and destroys them, step by step, using
// the network (internal/netns), jail (internal/jailer), Firecracker
// (internal/firecracker) and agent (internal/agentproto) packages, and keeps
// their records in the store (internal/state).
//
// Creating a VM is a list of steps, and every step that changes the host has
// an undo. If a step fails, or the create is cancelled, the steps already
// done are undone in reverse order:
//
//	allocate ID, slot, uid → record (creating)
//	network                 ↺ remove the network
//	jail                    ↺ remove the jail
//	start Firecracker       ↺ kill it
//	configure and boot
//	wait for the agent      → record (running)
//
// If the undo works, the VM ends up destroyed, with the reason kept in its
// record. If the undo itself fails, the VM stays failed and keeps its slot,
// so nothing can reuse resources that may still exist; destroying it again
// retries the cleanup.
package vm

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sanidhyasin/tine/internal/agentproto"
	"github.com/sanidhyasin/tine/internal/idalloc"
	"github.com/sanidhyasin/tine/internal/image"
	"github.com/sanidhyasin/tine/internal/jailer"
	"github.com/sanidhyasin/tine/internal/logging"
	"github.com/sanidhyasin/tine/internal/metrics"
	"github.com/sanidhyasin/tine/internal/netns"
	"github.com/sanidhyasin/tine/internal/snapshot"
	"github.com/sanidhyasin/tine/internal/state"
)

// Paths inside every VM's jail.
const (
	kernelPath         = "/vmlinux"
	rootfsPath         = "/rootfs.ext4"
	vsockPath          = "/vsock.sock"
	firecrackerLogPath = "/firecracker.log"
	guestCID           = 3
)

var (
	// ErrNotFound means there's no such VM.
	ErrNotFound = state.ErrNotFound
	// ErrNotRunning means the VM exists but isn't running.
	ErrNotRunning = errors.New("VM isn't running")
	// ErrInvalidRequest means the request was malformed, e.g. an odd number of vCPUs.
	ErrInvalidRequest = errors.New("invalid request")
	// ErrImageNotFound means the requested image isn't installed.
	ErrImageNotFound = errors.New("image not found")
	// ErrCapacity means the host can't take another VM right now.
	ErrCapacity = errors.New("at capacity")
	// ErrBusy means the VM is doing something that can't overlap with what
	// was asked, such as a snapshot while commands run in it.
	ErrBusy = errors.New("busy")
	// ErrNoSpace means the disk is short of room, for example for a snapshot.
	ErrNoSpace = errors.New("not enough disk space")
)

// Config is how the manager runs VMs. Zero values get the defaults in
// DefaultConfig.
type Config struct {
	JailerBin      string
	FirecrackerBin string
	// ChrootBaseDir holds the jails, ImagesDir the images, and SnapshotsDir
	// the snapshots. All three must be on the same filesystem (the XFS data
	// volume), for hard links and reflinks between them.
	ChrootBaseDir string
	ImagesDir     string
	SnapshotsDir  string
	// CgroupParent holds every VM's cgroup.
	CgroupParent string
	// UIDBase: a VM in network slot N runs as uid and gid UIDBase+N.
	UIDBase int

	DefaultVCPUs     int
	DefaultMemoryMiB int
	DefaultTTL       time.Duration
	// MaxVMs is how many VMs may exist at once.
	MaxVMs int
	// MemoryOverheadMiB is added to a VM's memory for its cgroup limit, to
	// cover Firecracker itself and the host page cache it uses. The limit
	// also has room for one snapshot's memory file in the page cache (the
	// VM's memory again): writing a snapshot charges that cache to the VM.
	MemoryOverheadMiB int
	// MaxPIDs caps the threads of each VM's Firecracker process.
	MaxPIDs int
	// BootArgs is the guest kernel command line, before the network settings.
	BootArgs string
	// AgentTimeout is how long a new VM's agent gets to answer.
	AgentTimeout time.Duration
	// ReaperInterval is how often expired VMs are looked for.
	ReaperInterval time.Duration
	// DestroyedRetention is how long destroyed VMs' records are kept.
	DestroyedRetention time.Duration
	// MaxSnapshotsPerVM is how many snapshots one VM may have.
	MaxSnapshotsPerVM int
	// MinFreeMiB is the disk space a snapshot must leave free.
	MinFreeMiB int

	// Metrics, if set, records how operations went. Nil records nothing.
	Metrics *metrics.Metrics

	Log *slog.Logger
}

// DefaultConfig is the configuration for tine's standard host layout.
func DefaultConfig() Config {
	return Config{
		JailerBin:         "/usr/local/bin/jailer",
		FirecrackerBin:    "/usr/local/bin/firecracker",
		ChrootBaseDir:     "/srv/tine/jail",
		ImagesDir:         image.DefaultImagesDir,
		SnapshotsDir:      snapshot.DefaultDir,
		CgroupParent:      "tine",
		UIDBase:           200_000,
		DefaultVCPUs:      2,
		DefaultMemoryMiB:  1024,
		DefaultTTL:        time.Hour,
		MaxVMs:            64,
		MemoryOverheadMiB: 256,
		MaxPIDs:           128,
		// rw: mount the disk read-write at boot rather than remounting it later.
		// i8042.*: skip probing for a PS/2 mouse and keyboard extras that a
		// Firecracker VM doesn't have.
		BootArgs:           "console=ttyS0 reboot=k panic=1 rw i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd",
		AgentTimeout:       60 * time.Second,
		ReaperInterval:     15 * time.Second,
		DestroyedRetention: 7 * 24 * time.Hour,
		MaxSnapshotsPerVM:  16,
		MinFreeMiB:         2048,
	}
}

func (c Config) withDefaults() Config {
	d := DefaultConfig()
	set := func(v *string, def string) {
		if *v == "" {
			*v = def
		}
	}
	setInt := func(v *int, def int) {
		if *v == 0 {
			*v = def
		}
	}
	set(&c.JailerBin, d.JailerBin)
	set(&c.FirecrackerBin, d.FirecrackerBin)
	set(&c.ChrootBaseDir, d.ChrootBaseDir)
	set(&c.ImagesDir, d.ImagesDir)
	set(&c.SnapshotsDir, d.SnapshotsDir)
	set(&c.CgroupParent, d.CgroupParent)
	set(&c.BootArgs, d.BootArgs)
	setInt(&c.UIDBase, d.UIDBase)
	setInt(&c.DefaultVCPUs, d.DefaultVCPUs)
	setInt(&c.DefaultMemoryMiB, d.DefaultMemoryMiB)
	setInt(&c.MaxVMs, d.MaxVMs)
	setInt(&c.MemoryOverheadMiB, d.MemoryOverheadMiB)
	setInt(&c.MaxPIDs, d.MaxPIDs)
	setInt(&c.MaxSnapshotsPerVM, d.MaxSnapshotsPerVM)
	setInt(&c.MinFreeMiB, d.MinFreeMiB)
	if c.DefaultTTL == 0 {
		c.DefaultTTL = d.DefaultTTL
	}
	if c.AgentTimeout == 0 {
		c.AgentTimeout = d.AgentTimeout
	}
	if c.ReaperInterval == 0 {
		c.ReaperInterval = d.ReaperInterval
	}
	if c.DestroyedRetention == 0 {
		c.DestroyedRetention = d.DestroyedRetention
	}
	if c.Log == nil {
		c.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return c
}

func (c Config) jailerConfig() jailer.Config {
	return jailer.Config{
		JailerBin:      c.JailerBin,
		FirecrackerBin: c.FirecrackerBin,
		ChrootBaseDir:  c.ChrootBaseDir,
		CgroupParent:   c.CgroupParent,
	}
}

// Manager creates, tracks and destroys VMs. It's safe for concurrent use.
type Manager struct {
	cfg   Config
	store *state.Store
	host  host
	log   *slog.Logger
	slots *idalloc.Range
	snaps *snapshot.Store
	gate  *gate

	mu    sync.Mutex
	procs map[string]*tracked // Firecracker processes this manager started
	ops   map[string]*createOp
	locks keyedMutex
	// flushes are closed when the VM's last snapshot is on disk.
	flushes map[string]chan struct{}
}

type tracked struct {
	proc     process
	stopping bool // set before tined kills it on purpose
}

type createOp struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// New returns a manager for the real host.
func New(cfg Config, store *state.Store) (*Manager, error) {
	cfg = cfg.withDefaults()
	return newManager(cfg, store, &hostOS{cfg: cfg})
}

func newManager(cfg Config, store *state.Store, h host) (*Manager, error) {
	cfg = cfg.withDefaults()
	slots, err := idalloc.NewRange(0, netns.MaxSlots)
	if err != nil {
		return nil, err
	}
	snaps, err := snapshot.NewStore(cfg.SnapshotsDir)
	if err != nil {
		return nil, err
	}
	m := &Manager{
		cfg: cfg, store: store, host: h, log: cfg.Log, slots: slots, snaps: snaps, gate: newGate(),
		procs: map[string]*tracked{}, ops: map[string]*createOp{}, flushes: map[string]chan struct{}{},
	}
	// Slots held by VMs from before a restart stay theirs.
	live, err := store.ListVMs(context.Background(), state.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, vm := range live {
		if err := slots.Reserve(vm.Slot); err != nil {
			return nil, fmt.Errorf("reserve slot of VM %s: %w", vm.ID, err)
		}
	}
	return m, nil
}

// Setup prepares the host for VMs: the cgroup that holds them, the network
// rules, and the image records. tined runs it at startup.
func (m *Manager) Setup(ctx context.Context) error {
	if err := m.host.setup(ctx); err != nil {
		return err
	}
	return m.SyncImages(ctx)
}

// SyncImages records every installed image in the store.
func (m *Manager) SyncImages(ctx context.Context) error {
	images, err := image.List(m.cfg.ImagesDir)
	if err != nil {
		return fmt.Errorf("list images: %w", err)
	}
	for _, img := range images {
		meta, err := encodeJSON(img.Meta)
		if err != nil {
			return err
		}
		if _, err := m.store.PutImage(ctx, state.Image{Name: img.Name, Dir: img.Dir, Metadata: meta}); err != nil {
			return err
		}
	}
	return nil
}

// CreateRequest describes a VM to create. Zero values get the manager's defaults.
type CreateRequest struct {
	Image     string
	VCPUs     int
	MemoryMiB int
	// TTL is how long the VM lives before the reaper destroys it.
	TTL time.Duration
}

func (m *Manager) normalize(req CreateRequest) (CreateRequest, error) {
	if req.Image == "" {
		req.Image = "base"
	}
	if req.VCPUs == 0 {
		req.VCPUs = m.cfg.DefaultVCPUs
	}
	if req.MemoryMiB == 0 {
		req.MemoryMiB = m.cfg.DefaultMemoryMiB
	}
	if req.TTL == 0 {
		req.TTL = m.cfg.DefaultTTL
	}
	switch {
	case req.VCPUs < 1 || req.VCPUs > 32 || (req.VCPUs > 1 && req.VCPUs%2 == 1):
		return req, fmt.Errorf("%w: vcpus must be 1 or an even number up to 32, got %d", ErrInvalidRequest, req.VCPUs)
	case req.MemoryMiB < 128 || req.MemoryMiB > 64<<10:
		return req, fmt.Errorf("%w: memory must be 128 MiB to 64 GiB, got %d MiB", ErrInvalidRequest, req.MemoryMiB)
	case req.TTL < time.Minute || req.TTL > 7*24*time.Hour:
		return req, fmt.Errorf("%w: ttl must be 1 minute to 7 days, got %v", ErrInvalidRequest, req.TTL)
	}
	return req, nil
}

// Create starts a VM and waits until its agent answers. On failure it
// undoes everything it did (see the package doc) and returns the error.
func (m *Manager) Create(ctx context.Context, req CreateRequest) (state.VM, error) {
	t := newTimer()
	req, err := m.normalize(req)
	if err != nil {
		return state.VM{}, err
	}
	if _, err := image.Open(m.cfg.ImagesDir, req.Image); err != nil {
		return state.VM{}, fmt.Errorf("%w: %s: %w", ErrImageNotFound, req.Image, err)
	}
	live, err := m.store.ListVMs(ctx, state.ListOptions{})
	if err != nil {
		return state.VM{}, err
	}
	if len(live) >= m.cfg.MaxVMs {
		return state.VM{}, fmt.Errorf("%w: %d VMs exist, the limit is %d", ErrCapacity, len(live), m.cfg.MaxVMs)
	}
	slot, err := m.slots.Allocate()
	if err != nil {
		return state.VM{}, fmt.Errorf("%w: %w", ErrCapacity, err)
	}
	vm, err := m.store.CreateVM(ctx, state.VM{
		ID: newID(), Image: req.Image, VCPUs: req.VCPUs, MemoryMiB: req.MemoryMiB,
		Slot: slot, UID: m.cfg.UIDBase + slot, ExpiresAt: time.Now().Add(req.TTL),
	})
	if err != nil {
		m.slots.Release(slot)
		return state.VM{}, err
	}
	ctx = logging.WithVM(ctx, vm.ID)
	log := m.log.With("vm", vm.ID)
	t.mark("allocate")

	// Destroy can cancel a create that's still running.
	ctx, cancel := context.WithCancel(ctx)
	op := &createOp{cancel: cancel, done: make(chan struct{})}
	m.mu.Lock()
	m.ops[vm.ID] = op
	m.mu.Unlock()
	defer func() {
		cancel()
		m.mu.Lock()
		delete(m.ops, vm.ID)
		m.mu.Unlock()
		close(op.done)
	}()

	var undo []func() error
	fail := func(stage string, cause error) (state.VM, error) {
		if ctx.Err() != nil && !errors.Is(cause, context.Canceled) {
			cause = fmt.Errorf("%w (cancelled: %w)", cause, ctx.Err())
		}
		log.WarnContext(ctx, "create failed; undoing", "stage", stage, "err", cause)
		m.cfg.Metrics.CreateFailed(stage)
		failed, _ := m.store.Transition(context.Background(), vm.ID, state.StateFailed, stage+": "+cause.Error())
		var cleanupErrs []error
		for i := len(undo) - 1; i >= 0; i-- {
			if err := undo[i](); err != nil {
				cleanupErrs = append(cleanupErrs, err)
			}
		}
		createErr := fmt.Errorf("create VM %s: %s: %w", vm.ID, stage, cause)
		if err := errors.Join(cleanupErrs...); err != nil {
			log.ErrorContext(ctx, "cleanup after a failed create failed too; the VM stays failed", "err", err)
			return failed, errors.Join(createErr, fmt.Errorf("cleanup: %w", err))
		}
		if _, err := m.store.Transition(context.Background(), vm.ID, state.StateDestroying, "cleaning up after a failed create"); err == nil {
			if done, err := m.store.Transition(context.Background(), vm.ID, state.StateDestroyed, "cleaned up after a failed create"); err == nil {
				failed = done
			}
		}
		m.slots.Release(vm.Slot)
		return failed, createErr
	}

	n, j, err := m.resources(vm)
	if err != nil {
		return fail("plan", err)
	}

	if err := m.host.createNetwork(ctx, n, netns.Owner{UID: vm.UID, GID: vm.UID}); err != nil {
		return fail("network", err)
	}
	undo = append(undo, func() error { return m.host.destroyNetwork(n) })
	t.mark("network")

	if err := m.host.prepareJail(j); err != nil {
		return fail("jail", err)
	}
	undo = append(undo, func() error { return m.host.cleanupJail(j) })
	t.mark("jail")

	proc, err := m.host.startFirecracker(j)
	if err != nil {
		return fail("start", err)
	}
	tr := m.track(vm.ID, proc)
	undo = append(undo, func() error { return m.stopOwned(vm.ID, tr) })
	t.mark("start")

	if err := m.host.configureAndBoot(ctx, j, m.bootSpec(vm)); err != nil {
		return fail("boot", err)
	}
	t.mark("configure")

	agentCtx, agentCancel := context.WithTimeout(ctx, m.cfg.AgentTimeout)
	err = m.host.waitAgent(agentCtx, j)
	agentCancel()
	if err != nil {
		return fail("agent", err)
	}
	t.mark("agent")

	vm, err = m.store.UpdateVM(ctx, vm.ID, "agent is ready", func(v *state.VM) error {
		v.State = state.StateRunning
		v.PID = proc.pid()
		v.Timings = t.millis()
		return nil
	})
	if err != nil {
		return fail("record", err)
	}
	log.InfoContext(ctx, "created", "timings_ms", vm.Timings)
	m.cfg.Metrics.CreateSucceeded(vm.Timings)
	return vm, nil
}

// Destroy stops a VM and removes everything it used. Destroying a VM that's
// already destroyed returns it unchanged. A VM still being created is
// cancelled first, and undoes itself.
func (m *Manager) Destroy(ctx context.Context, id string) (state.VM, error) {
	return m.destroy(ctx, id, "destroy requested")
}

// destroy is Destroy, recording reason as why it happened.
func (m *Manager) destroy(ctx context.Context, id, reason string) (state.VM, error) {
	ctx = logging.WithVM(ctx, id)
	m.mu.Lock()
	op := m.ops[id]
	m.mu.Unlock()
	if op != nil {
		op.cancel()
		select {
		case <-op.done:
		case <-ctx.Done():
			return state.VM{}, ctx.Err()
		}
	}

	unlock := m.locks.lock(id)
	defer unlock()

	vm, err := m.store.GetVM(ctx, id)
	if err != nil {
		return state.VM{}, err
	}
	if vm.State == state.StateDestroyed {
		return vm, nil
	}
	start := time.Now()
	if vm.State != state.StateDestroying { // destroying means an earlier destroy was interrupted: carry on
		if vm, err = m.store.Transition(ctx, id, state.StateDestroying, reason); err != nil {
			return vm, err
		}
	}
	if err := m.teardown(ctx, vm); err != nil {
		m.cfg.Metrics.Destroyed(time.Since(start), err)
		m.log.ErrorContext(ctx, "destroy failed; the VM is marked failed", "vm", id, "err", err)
		failed, _ := m.store.Transition(context.Background(), id, state.StateFailed, "destroy: "+err.Error())
		return failed, fmt.Errorf("destroy VM %s: %w", id, err)
	}
	m.slots.Release(vm.Slot)
	vm, err = m.store.UpdateVM(context.Background(), id, "destroyed", func(v *state.VM) error {
		v.State = state.StateDestroyed
		if v.Timings == nil {
			v.Timings = map[string]float64{}
		}
		v.Timings["destroy"] = ms(time.Since(start))
		return nil
	})
	m.cfg.Metrics.Destroyed(time.Since(start), err)
	if err == nil {
		m.log.InfoContext(ctx, "destroyed", "vm", id, "reason", reason, "ms", vm.Timings["destroy"])
	}
	return vm, err
}

// teardown stops the VM's Firecracker and removes its jail and network.
func (m *Manager) teardown(ctx context.Context, vm state.VM) error {
	n, j, err := m.resources(vm)
	if err != nil {
		return err
	}
	m.mu.Lock()
	tr := m.procs[vm.ID]
	m.mu.Unlock()
	if tr != nil {
		if err := m.stopOwned(vm.ID, tr); err != nil {
			return err
		}
	} else {
		// Not started by this tined (or not recorded yet: a create can stop
		// before the PID is saved). Stop whatever runs in the VM's cgroup; if
		// nothing does, this does nothing.
		if err := m.host.killFirecracker(ctx, j, vm.PID); err != nil {
			return fmt.Errorf("stop Firecracker: %w", err)
		}
	}
	// In order, stopping at the first failure: the jail and its cgroup only
	// go once Firecracker has exited, and the network only after that.
	if err := m.host.cleanupJail(j); err != nil {
		return err
	}
	return m.host.destroyNetwork(n)
}

// Get returns a VM's record.
func (m *Manager) Get(ctx context.Context, id string) (state.VM, error) {
	return m.store.GetVM(ctx, id)
}

// List returns the VMs that aren't destroyed, or all of them.
func (m *Manager) List(ctx context.Context, includeDestroyed bool) ([]state.VM, error) {
	return m.store.ListVMs(ctx, state.ListOptions{IncludeDestroyed: includeDestroyed})
}

// Exec runs a command in a running VM through its agent. If the VM is being
// snapshotted or rolled back, it waits for that to finish first.
func (m *Manager) Exec(ctx context.Context, id string, req agentproto.ExecRequest, onOutput func(stream string, data []byte)) (agentproto.ExitStatus, error) {
	leave, err := m.gate.enterExec(ctx, id)
	if err != nil {
		return agentproto.ExitStatus{}, err
	}
	defer leave()
	vm, err := m.store.GetVM(ctx, id)
	if err != nil {
		return agentproto.ExitStatus{}, err
	}
	if vm.State != state.StateRunning {
		return agentproto.ExitStatus{}, fmt.Errorf("VM %s is %s: %w", id, vm.State, ErrNotRunning)
	}
	_, j, err := m.resources(vm)
	if err != nil {
		return agentproto.ExitStatus{}, err
	}
	start := time.Now()
	st, err := m.host.agent(j).Exec(logging.WithVM(ctx, id), req, onOutput)
	m.cfg.Metrics.Exec(time.Since(start), err)
	return st, err
}

// Images lists the installed images.
func (m *Manager) Images(context.Context) ([]*image.Image, error) {
	return image.List(m.cfg.ImagesDir)
}

// Ping checks the manager can reach its database.
func (m *Manager) Ping(ctx context.Context) error { return m.store.Ping(ctx) }

// --- helpers ---

// resources works out a VM's network and jail from its record. It's
// deterministic, so it works for VMs created before a restart too.
func (m *Manager) resources(vm state.VM) (netns.Network, *jailer.Jail, error) {
	n, err := netns.Plan(vm.ID, vm.Slot)
	if err != nil {
		return netns.Network{}, nil, err
	}
	imgDir := filepath.Join(m.cfg.ImagesDir, vm.Image)
	j, err := jailer.New(m.cfg.jailerConfig(), jailer.Spec{
		ID:     vm.ID,
		UID:    vm.UID,
		GID:    vm.UID,
		NetNS:  n.NamespacePath(),
		Limits: jailer.Limits{VCPUs: vm.VCPUs, MemoryBytes: memoryLimit(vm.MemoryMiB, m.cfg.MemoryOverheadMiB), MaxPIDs: m.cfg.MaxPIDs},
		Files: []jailer.File{
			{Source: filepath.Join(imgDir, image.KernelFile), Target: kernelPath, Mode: jailer.Link},
			{Source: filepath.Join(imgDir, image.RootfsFile), Target: rootfsPath, Mode: jailer.Clone},
		},
		FirecrackerArgs: []string{"--enable-pci"},
	})
	return n, j, err
}

// memoryLimit is a VM's cgroup memory limit: its memory, Firecracker's
// overhead, and room for one snapshot's memory file in the page cache.
//
// Firecracker writes snapshots through the page cache, which the kernel
// charges to the VM's cgroup. Without the room, a snapshot of a VM whose
// limit is already full of cache makes the kernel write the old pages out
// first, while the VM is paused, or kill Firecracker if it can't do so in
// time.
func memoryLimit(memoryMiB, overheadMiB int) int64 {
	return int64(2*memoryMiB+overheadMiB) << 20
}

func (m *Manager) bootSpec(vm state.VM) bootSpec {
	return bootSpec{
		VCPUs:     vm.VCPUs,
		MemoryMiB: vm.MemoryMiB,
		BootArgs:  strings.TrimSpace(m.cfg.BootArgs + " " + netns.GuestKernelArgs()),
	}
}

// track watches a Firecracker process this manager started. If it exits
// without tined stopping it, the VM is marked failed.
func (m *Manager) track(id string, p process) *tracked {
	tr := &tracked{proc: p}
	m.mu.Lock()
	m.procs[id] = tr
	m.mu.Unlock()
	go func() {
		<-p.done()
		m.mu.Lock()
		stopping := tr.stopping
		if m.procs[id] == tr {
			delete(m.procs, id)
		}
		m.mu.Unlock()
		if stopping {
			return
		}
		reason := fmt.Sprintf("Firecracker exited unexpectedly: %v", p.exitErr())
		m.log.Error("VM died", "vm", id, "err", p.exitErr())
		_, _ = m.store.UpdateVM(context.Background(), id, reason, func(v *state.VM) error {
			if v.State == state.StateRunning || v.State == state.StateCreating {
				v.State = state.StateFailed
				v.Error = reason
			}
			return nil
		})
	}()
	return tr
}

// stopOwned kills a Firecracker process this manager started and waits for it.
func (m *Manager) stopOwned(id string, tr *tracked) error {
	m.mu.Lock()
	tr.stopping = true
	m.mu.Unlock()
	if err := tr.proc.kill(); err != nil {
		select {
		case <-tr.proc.done(): // it had already exited
		default:
			return fmt.Errorf("kill Firecracker: %w", err)
		}
	}
	select {
	case <-tr.proc.done():
		return nil
	case <-time.After(10 * time.Second):
		return fmt.Errorf("VM %s: Firecracker didn't exit within 10s of being killed", id)
	}
}

// newID makes a VM ID: "vm-" and 10 random characters (50 bits).
func newID() string {
	b := make([]byte, 7)
	_, _ = rand.Read(b)
	return "vm-" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))[:10]
}

// timer records how long each step of an operation takes.
type timer struct {
	last   time.Time
	start  time.Time
	stages map[string]float64
}

func newTimer() *timer {
	now := time.Now()
	return &timer{last: now, start: now, stages: map[string]float64{}}
}

func (t *timer) mark(stage string) {
	now := time.Now()
	t.stages[stage] = ms(now.Sub(t.last))
	t.last = now
}

func (t *timer) millis() map[string]float64 {
	out := make(map[string]float64, len(t.stages)+1)
	for k, v := range t.stages {
		out[k] = v
	}
	out["total"] = ms(time.Since(t.start))
	return out
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// keyedMutex is one mutex per key, freed once nobody holds or waits for it.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*keyedEntry
}

type keyedEntry struct {
	mu   sync.Mutex
	refs int
}

func (k *keyedMutex) lock(key string) (unlock func()) {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*keyedEntry{}
	}
	e := k.locks[key]
	if e == nil {
		e = &keyedEntry{}
		k.locks[key] = e
	}
	e.refs++
	k.mu.Unlock()

	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		k.mu.Lock()
		e.refs--
		if e.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}
