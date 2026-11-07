package vm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"github.com/sanidhyasin/tine/internal/agentproto"
	"github.com/sanidhyasin/tine/internal/firecracker"
	"github.com/sanidhyasin/tine/internal/jailer"
	"github.com/sanidhyasin/tine/internal/netns"
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
type hostOS struct{ cfg Config }

func (h *hostOS) setup(ctx context.Context) error {
	if err := h.cfg.jailerConfig().EnsureCgroupParent(); err != nil {
		return fmt.Errorf("cgroups: %w", err)
	}
	if err := os.MkdirAll(h.cfg.ChrootBaseDir, 0o700); err != nil {
		return err
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

func (h *hostOS) configureAndBoot(ctx context.Context, j *jailer.Jail, boot bootSpec) error {
	c := firecracker.New(j.APISocket())
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
			return c.PutMachineConfig(ctx, firecracker.MachineConfig{VCPUCount: boot.VCPUs, MemSizeMiB: boot.MemoryMiB})
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

func (h *hostOS) waitAgent(ctx context.Context, j *jailer.Jail) error {
	_, err := h.agent(j).WaitHealthy(ctx, 10*time.Millisecond)
	return err
}

func (h *hostOS) agent(j *jailer.Jail) *agentproto.Client {
	return agentproto.NewVsockClient(j.HostPath(vsockPath))
}

// killFirecracker kills pid, but only after checking it really is this
// jail's Firecracker: PIDs get reused, and the wrong process must never be
// killed.
func (h *hostOS) killFirecracker(ctx context.Context, j *jailer.Jail, pid int) error {
	cmdline, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if errors.Is(err, fs.ErrNotExist) {
		return nil // already gone
	}
	if err != nil {
		return err
	}
	if !bytes.Contains(cmdline, []byte("--id="+j.ID+"\x00")) {
		return nil // the PID now belongs to something else: ours is gone
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	return fmt.Errorf("pid %d didn't exit within 10s of SIGKILL", pid)
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
