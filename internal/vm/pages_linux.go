//go:build linux

package vm

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/sanidhyasin/tine/internal/jailer"
)

func (h *hostOS) startPageServer(ctx context.Context, j *jailer.Jail, spec pageServerSpec) (pageServer, error) {
	// The jailer makes the VM's cgroup before it starts Firecracker, which
	// is ready once its API socket answers.
	if err := h.firecracker(j).WaitReady(ctx, 2*time.Millisecond); err != nil {
		return nil, fmt.Errorf("page server: waiting for Firecracker: %w", err)
	}
	cgroup, err := os.Open(j.CgroupDir())
	if err != nil {
		return nil, fmt.Errorf("page server: the VM's cgroup: %w", err)
	}
	defer func() { _ = cgroup.Close() }()

	// tined opens everything, and hands the files over: the page server
	// runs as the VM's user, which can't reach into tined's directories.
	sock := j.HostPath(pageServerSocket)
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		return nil, fmt.Errorf("page server: listen: %w", err)
	}
	l.SetUnlinkOnClose(false)
	listener, err := l.File()
	_ = l.Close()
	if err != nil {
		return nil, err
	}
	var files []*os.File
	defer func() {
		for _, f := range files {
			_ = f.Close()
		}
	}()
	files = append(files, listener)
	// Firecracker, as the VM's user, connects to it.
	if err := os.Chown(sock, spec.UID, spec.GID); err != nil {
		return nil, err
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		return nil, err
	}
	mem, err := os.Open(j.HostPath(restoreMemPath))
	if err != nil {
		return nil, fmt.Errorf("page server: %w", err)
	}
	files = append(files, mem)
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	files = append(files, os.NewFile(uintptr(pair[1]), "control"))
	ourEnd := os.NewFile(uintptr(pair[0]), "page-server-control")
	ours, err := net.FileConn(ourEnd) // a copy, with deadlines
	_ = ourEnd.Close()
	if err != nil {
		return nil, err
	}
	args := []string{"-cluster", strconv.Itoa(spec.Cluster)}
	if spec.Prefetch != "" {
		ws, err := os.Open(spec.Prefetch)
		if err != nil {
			_ = ours.Close()
			return nil, fmt.Errorf("page server: %w", err)
		}
		files = append(files, ws)
		args = append(args, "-prefetch")
	}
	logFile, err := os.OpenFile(filepath.Join(j.Dir, "uffd.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		_ = ours.Close()
		return nil, err
	}
	files = append(files, logFile)

	// It outlives the request that starts it: it serves the VM's pages until the VM goes.
	cmd := exec.CommandContext(context.WithoutCancel(ctx), h.cfg.PageServerBin, args...) //nolint:gosec // tined's configured page server
	cmd.ExtraFiles = files[:len(files)-1]                                                // fds 3, 4, 5 and maybe 6
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential:  &syscall.Credential{Uid: uint32(spec.UID), Gid: uint32(spec.GID)}, //nolint:gosec // VM uids are small
		UseCgroupFD: true,
		CgroupFD:    int(cgroup.Fd()),
		Setsid:      true, // its own session: tined stopping mustn't stop it
	}
	if err := cmd.Start(); err != nil {
		_ = ours.Close()
		return nil, fmt.Errorf("start %s: %w", h.cfg.PageServerBin, err)
	}
	go func() { _ = cmd.Wait() }()
	return &osPageServer{proc: cmd.Process, ctl: ours}, nil
}

type osPageServer struct {
	proc *os.Process
	ctl  net.Conn
}

func (p *osPageServer) workingSet(ctx context.Context) ([]byte, error) {
	deadline := time.Now().Add(10 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := p.ctl.SetDeadline(deadline); err != nil {
		return nil, err
	}
	if _, err := p.ctl.Write([]byte("working-set\n")); err != nil {
		return nil, fmt.Errorf("page server: %w", err)
	}
	r := bufio.NewReader(p.ctl)
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("page server: %w", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || n < 0 || n > 1<<30 {
		return nil, fmt.Errorf("page server: %q", strings.TrimSpace(line))
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, fmt.Errorf("page server: %w", err)
	}
	return data, nil
}

func (p *osPageServer) release() { _ = p.ctl.Close() }

func (p *osPageServer) kill() {
	_ = p.ctl.Close()
	_ = p.proc.Kill()
}
