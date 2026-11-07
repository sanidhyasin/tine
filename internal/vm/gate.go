package vm

import (
	"context"
	"fmt"
	"sync"
)

// gate lets a VM run many execs at once, or one snapshot or rollback, but
// never both.
//
// Taking a snapshot resets the guest's vsock connections, which would cut a
// running exec off mid-stream, and a rollback replaces the VM under it. So a
// snapshot or rollback is refused with ErrBusy while execs run, and an exec
// that arrives during one waits for it to finish.
type gate struct {
	mu        sync.Mutex
	execs     map[string]int
	exclusive map[string]chan struct{} // closed when the snapshot or rollback ends
}

func newGate() *gate {
	return &gate{execs: map[string]int{}, exclusive: map[string]chan struct{}{}}
}

// enterExec waits until no snapshot or rollback of the VM is running, then
// counts an exec in. Call leave when the exec is over.
func (g *gate) enterExec(ctx context.Context, id string) (leave func(), err error) {
	g.mu.Lock()
	for {
		busy := g.exclusive[id]
		if busy == nil {
			break
		}
		g.mu.Unlock()
		select {
		case <-busy:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		g.mu.Lock()
	}
	g.execs[id]++
	g.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.execs[id]--; g.execs[id] <= 0 {
				delete(g.execs, id)
			}
		})
	}, nil
}

// enterExclusive starts what (a snapshot or a rollback) on the VM, or
// fails with ErrBusy if execs are running in it. New execs wait until leave
// is called.
func (g *gate) enterExclusive(id, what string) (leave func(), err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if n := g.execs[id]; n > 0 {
		return nil, fmt.Errorf("%w: %d commands are running in VM %s; try the %s again once they finish", ErrBusy, n, id, what)
	}
	if g.exclusive[id] != nil {
		return nil, fmt.Errorf("%w: VM %s is already being snapshotted or rolled back", ErrBusy, id)
	}
	done := make(chan struct{})
	g.exclusive[id] = done
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			delete(g.exclusive, id)
			close(done)
		})
	}, nil
}
