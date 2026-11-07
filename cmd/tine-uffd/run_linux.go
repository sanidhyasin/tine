//go:build linux

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/sanidhyasin/tine/internal/uffd"
)

const defaultTimeout = 30 * time.Second

// The files tined hands over.
const (
	listenFD   = 3
	memoryFD   = 4
	controlFD  = 5
	prefetchFD = 6
)

func run(prefetch bool, cluster int, timeout time.Duration) error {
	// A write to a control socket tined has closed must not kill the VM's pages.
	signal.Ignore(syscall.SIGPIPE)
	l, err := net.FileListener(os.NewFile(listenFD, "listener"))
	if err != nil {
		return fmt.Errorf("listener (fd %d): %w", listenFD, err)
	}
	mem := os.NewFile(memoryFD, "memory")
	if _, err := mem.Stat(); err != nil {
		return fmt.Errorf("memory file (fd %d): %w", memoryFD, err)
	}
	var ws *uffd.WorkingSet
	if prefetch {
		data, err := readAll(os.NewFile(prefetchFD, "working-set"))
		if err != nil {
			return fmt.Errorf("working set (fd %d): %w", prefetchFD, err)
		}
		ws = new(uffd.WorkingSet)
		if err := ws.UnmarshalBinary(data); err != nil {
			return err
		}
	}

	ul := l.(*net.UnixListener)
	if err := ul.SetDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	conn, err := ul.AcceptUnix()
	if err != nil {
		return fmt.Errorf("waiting for Firecracker: %w", err)
	}
	_ = l.Close() // one VM, one connection
	regions, fd, err := uffd.Receive(conn, 10*time.Second)
	if err != nil {
		return err
	}
	s, err := uffd.NewServer(fd, regions, mem, cluster)
	if err != nil {
		return err
	}
	hangup, err := conn.File()
	if err != nil {
		return err
	}

	if ctl := os.NewFile(controlFD, "control"); ctl != nil {
		if c, err := net.FileConn(ctl); err == nil {
			go control(c, s)
		}
	}
	if ws != nil {
		go func() {
			if err := s.Prefetch(ws); err != nil {
				fmt.Fprintln(os.Stderr, "tine-uffd: prefetch:", err)
			}
		}()
	}
	err = s.Serve(int(hangup.Fd()))
	st, _ := json.Marshal(s.Stats())
	fmt.Fprintf(os.Stderr, "tine-uffd: done: %s\n", st)
	return err
}

// control answers tined's requests until it hangs up.
func control(c net.Conn, s *uffd.Server) {
	defer func() { _ = c.Close() }()
	r := bufio.NewScanner(c)
	for r.Scan() {
		var reply []byte
		switch r.Text() {
		case "stats":
			st, _ := json.Marshal(s.Stats())
			reply = append(st, '\n')
		case "working-set":
			data, _ := s.WorkingSet().MarshalBinary()
			reply = append([]byte(strconv.Itoa(len(data))+"\n"), data...)
		default:
			reply = []byte("error: unknown request\n")
		}
		if _, err := c.Write(reply); err != nil {
			return
		}
	}
}

func readAll(f *os.File) ([]byte, error) {
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	data := make([]byte, fi.Size())
	_, err = f.ReadAt(data, 0)
	return data, err
}
