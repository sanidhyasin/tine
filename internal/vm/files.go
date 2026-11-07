package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/sanidhyasin/tine/internal/agentproto"
	"github.com/sanidhyasin/tine/internal/logging"
)

// Files in a VM: read one, write one, or unpack a tar archive into a
// directory, through the VM's agent. Like a command, a transfer holds off
// snapshots until it's done, as they'd cut the agent's connection.

// ReadFile streams a file from a VM. The reader must be closed; until it
// is, the VM counts as running a command.
func (m *Manager) ReadFile(ctx context.Context, id, path string) (io.ReadCloser, agentproto.FileInfo, error) {
	agent, leave, err := m.agentOf(ctx, id)
	if err != nil {
		return nil, agentproto.FileInfo{}, err
	}
	rc, info, err := agent.ReadFile(logging.WithVM(ctx, id), path)
	if err != nil {
		leave()
		return nil, info, agentTooOld(err)
	}
	return &leavingReader{ReadCloser: rc, leave: leave}, info, nil
}

// WriteFile writes r to a file in a VM, with permissions mode, creating the
// directories it's in.
func (m *Manager) WriteFile(ctx context.Context, id, path string, mode os.FileMode, r io.Reader) (agentproto.FileInfo, error) {
	agent, leave, err := m.agentOf(ctx, id)
	if err != nil {
		return agentproto.FileInfo{}, err
	}
	defer leave()
	info, err := agent.WriteFile(logging.WithVM(ctx, id), path, mode, r)
	return info, agentTooOld(err)
}

// PutArchive unpacks a tar stream, gzipped or not, into a directory in a VM.
func (m *Manager) PutArchive(ctx context.Context, id, dir string, r io.Reader) (agentproto.ArchiveResult, error) {
	agent, leave, err := m.agentOf(ctx, id)
	if err != nil {
		return agentproto.ArchiveResult{}, err
	}
	defer leave()
	res, err := agent.PutArchive(logging.WithVM(ctx, id), dir, r)
	return res, agentTooOld(err)
}

// agentTooOld explains the error an agent from before file transfers gives.
func agentTooOld(err error) error {
	var ae *agentproto.Error
	if errors.As(err, &ae) && ae.StatusCode == http.StatusNotFound && strings.HasPrefix(ae.Message, "no such endpoint") {
		return fmt.Errorf("%w: the image's tine-agent is too old for file transfers; import the image again", ErrInvalidRequest)
	}
	return err
}

type leavingReader struct {
	io.ReadCloser
	once  sync.Once
	leave func()
}

func (r *leavingReader) Close() error {
	err := r.ReadCloser.Close()
	r.once.Do(r.leave)
	return err
}
