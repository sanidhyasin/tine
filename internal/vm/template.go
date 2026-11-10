package vm

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sanidhyasin/tine/internal/agentproto"
	"github.com/sanidhyasin/tine/internal/image"
	"github.com/sanidhyasin/tine/internal/snapshot"
	"github.com/sanidhyasin/tine/internal/state"
)

// Templates: an image booted once and snapshotted once it's ready, so new
// VMs of that image and size start from it in milliseconds instead of
// booting. A snapshot fixes a VM's size, so each template is for one image
// at one size. Create uses the newest template that fits, if this host can
// restore it and its image hasn't been imported again since; otherwise it
// boots the VM.

const (
	// warmTTL is the life of the VM booted to make a template. It's
	// destroyed as soon as the template is saved.
	warmTTL = 10 * time.Minute
	// settleTimeout is how long a template's VM gets to finish booting:
	// every service started, so restored VMs don't redo that work.
	settleTimeout = 2 * time.Minute
	// warmRetry is how long keepWarm waits after a failed warm before
	// trying that image again.
	warmRetry = 10 * time.Minute
)

// WarmTemplate boots an image at a size (zero values: the defaults), waits
// until the guest has finished booting, and saves it as that image and
// size's template. An older template it replaces is deleted once no VM runs
// from it.
func (m *Manager) WarmTemplate(ctx context.Context, imageName string, vcpus, memoryMiB int) (state.Snapshot, error) {
	req, err := m.normalize(ctx, CreateRequest{Image: imageName, VCPUs: vcpus, MemoryMiB: memoryMiB, TTL: warmTTL, Cold: true})
	if err != nil {
		return state.Snapshot{}, err
	}
	// One warm of an image and size at a time: two at once write two
	// memory files to the disk together, which slows both, and the older
	// would be replaced at once anyway. (No VM ID contains a colon.)
	unlockWarm := m.locks.lock(fmt.Sprintf("warm:%s:%d:%d", req.Image, req.VCPUs, req.MemoryMiB))
	defer unlockWarm()
	start := time.Now()
	vm, err := m.Create(ctx, req)
	if err != nil {
		return state.Snapshot{}, fmt.Errorf("warm %s: boot: %w", req.Image, err)
	}
	defer func() {
		if _, err := m.destroy(context.WithoutCancel(ctx), vm.ID, "its template is saved"); err != nil {
			m.log.Error("couldn't destroy a template's VM", "vm", vm.ID, "err", err)
		}
	}()
	// Wait for every service to start, so restored VMs are idle from the
	// first moment. A "degraded" boot (a unit failed) is still finished, so
	// the command's exit code doesn't matter.
	settle := agentproto.ExecRequest{Args: m.cfg.SettleCommand, TimeoutMs: settleTimeout.Milliseconds()}
	if _, err := m.Exec(ctx, vm.ID, settle, nil); err != nil {
		return state.Snapshot{}, fmt.Errorf("warm %s: wait for the boot to finish: %w", req.Image, err)
	}
	// systemd says the boot is finished as it starts its last units, so the
	// guest is busy for a moment after: saved then, every restore would
	// carry on with that work, and take longer to answer.
	select {
	case <-time.After(m.cfg.SettleQuiet):
	case <-ctx.Done():
		return state.Snapshot{}, ctx.Err()
	}
	settled := time.Since(start)

	unlock := m.locks.lock(vm.ID)
	snap, err := m.takeSnapshot(ctx, vm, state.SnapshotKindTemplate, false)
	unlock()
	if err != nil {
		return state.Snapshot{}, fmt.Errorf("warm %s: %w", req.Image, err)
	}
	m.log.InfoContext(ctx, "template ready", "image", req.Image, "snapshot", snap.ID, "vcpus", snap.VCPUs,
		"memory_mib", snap.MemoryMiB, "boot_ms", ms(settled), "total_ms", ms(time.Since(start)))
	return snap, nil
}

// Template is an image's newest template at one size, and whether creates
// can use it.
type Template struct {
	Snapshot state.Snapshot
	// Problem says why creates can't use it, or is empty if they can.
	Problem string
}

// Templates lists the newest template of each image and size.
func (m *Manager) Templates(ctx context.Context) ([]Template, error) {
	all, err := m.store.ListSnapshots(ctx, state.SnapshotFilter{Kind: state.SnapshotKindTemplate})
	if err != nil {
		return nil, err
	}
	type size struct {
		image            string
		vcpus, memoryMiB int
	}
	newest := map[size]state.Snapshot{}
	var order []size
	for _, s := range all { // oldest first, so later ones win
		if s.State != state.SnapshotReady {
			continue
		}
		k := size{s.Image, s.VCPUs, s.MemoryMiB}
		if _, seen := newest[k]; !seen {
			order = append(order, k)
		}
		newest[k] = s
	}
	out := make([]Template, 0, len(order))
	for _, k := range order {
		s := newest[k]
		out = append(out, Template{Snapshot: s, Problem: m.templateProblem(ctx, s)})
	}
	return out, nil
}

// pickTemplate returns the template a create of req (already normalized)
// should restore from, or "" and, if there is a template, why it can't.
func (m *Manager) pickTemplate(ctx context.Context, req CreateRequest) (id, problem string) {
	snap, err := m.store.LatestTemplate(ctx, req.Image, req.VCPUs, req.MemoryMiB)
	if err != nil {
		if !errors.Is(err, state.ErrNotFound) {
			m.log.WarnContext(ctx, "couldn't look for a template", "image", req.Image, "err", err)
		}
		return "", ""
	}
	if problem := m.templateProblem(ctx, snap); problem != "" {
		return "", fmt.Sprintf("template %s: %s", snap.ID, problem)
	}
	return snap.ID, ""
}

// templateProblem says why a template can't be used, or "" if it can.
func (m *Manager) templateProblem(ctx context.Context, snap state.Snapshot) string {
	if err := m.checkRestorable(ctx, snap.ID); err != nil {
		return err.Error()
	}
	meta, err := m.snaps.Load(snap.ID)
	if err != nil {
		return err.Error()
	}
	img, err := image.Open(m.cfg.ImagesDir, snap.Image)
	if err != nil {
		return "its image is gone: " + err.Error()
	}
	if !meta.ImageCreatedAt.Equal(img.Meta.CreatedAt) {
		return fmt.Sprintf("image %s was imported again since (at %s)", snap.Image, img.Meta.CreatedAt.Format(time.RFC3339))
	}
	return ""
}

// keepWarm makes sure each image in WarmImages has a template at the
// default size that creates can use, and warms one in the background when
// it's missing or out of date: after a Firecracker or kernel upgrade, or the
// image being imported again. It runs at startup and with the reaper.
func (m *Manager) keepWarm(ctx context.Context) {
	for _, name := range m.cfg.WarmImages {
		if _, err := image.Open(m.cfg.ImagesDir, name); err != nil {
			continue // not installed (yet)
		}
		req := CreateRequest{Image: name, VCPUs: m.cfg.DefaultVCPUs, MemoryMiB: m.cfg.DefaultMemoryMiB}
		id, problem := m.pickTemplate(ctx, req)
		if id != "" {
			continue
		}
		m.mu.Lock()
		busy := m.warming[name] || time.Since(m.warmFailed[name]) < warmRetry
		if !busy {
			m.warming[name] = true
		}
		m.mu.Unlock()
		if busy {
			continue
		}
		why := "it has none"
		if problem != "" {
			why = problem
		}
		m.log.Info("warming a template", "image", name, "because", why)
		go func() {
			_, err := m.WarmTemplate(ctx, name, 0, 0)
			m.mu.Lock()
			delete(m.warming, name)
			if err != nil {
				m.warmFailed[name] = time.Now()
			} else {
				delete(m.warmFailed, name)
			}
			m.mu.Unlock()
			if err != nil && ctx.Err() == nil {
				m.log.Error("couldn't warm a template; trying again later", "image", name, "retry_in", warmRetry.String(), "err", err)
			}
		}()
	}
}

// prefetchTemplates reads every usable template's memory file into the page
// cache, so the first VMs restored from it after the host starts don't wait
// for the disk on every page the guest touches, nor do snapshots of them,
// which read all of a VM's memory while it's paused. tined does this once,
// at startup.
func (m *Manager) prefetchTemplates(ctx context.Context) {
	templates, err := m.Templates(ctx)
	if err != nil {
		m.log.Warn("couldn't list templates to prefetch", "err", err)
		return
	}
	for _, t := range templates {
		if t.Problem != "" || ctx.Err() != nil {
			continue
		}
		start := time.Now()
		if err := m.host.prefetch(m.snaps.Path(t.Snapshot.ID, snapshot.MemoryFile)); err != nil {
			m.log.Warn("couldn't prefetch a template", "snapshot", t.Snapshot.ID, "err", err)
			continue
		}
		m.log.Info("template's memory is in the page cache", "snapshot", t.Snapshot.ID, "image", t.Snapshot.Image, "ms", ms(time.Since(start)))
	}
}
