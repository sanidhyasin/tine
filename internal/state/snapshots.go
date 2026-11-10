package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SnapshotKind says what a snapshot is for.
type SnapshotKind string

const (
	// SnapshotKindVM is a save point of a VM, taken on request. It belongs to
	// that VM, and goes when the VM does, unless VMs still run from it or
	// it's retained.
	SnapshotKindVM SnapshotKind = "vm"
	// SnapshotKindTemplate is an image booted once and saved when ready, so
	// new VMs can start from it instead of booting. Only the newest template
	// for an image and size is used; older ones go once nothing runs from them.
	SnapshotKindTemplate SnapshotKind = "template"
	// SnapshotKindFork is taken to fork a VM: its children are restored from
	// it. It goes as soon as none of them runs from it.
	SnapshotKindFork SnapshotKind = "fork"
)

// SnapshotState is where a snapshot is in its life.
type SnapshotState string

const (
	// SnapshotCreating: its files are being written. It can't be used yet.
	SnapshotCreating SnapshotState = "creating"
	// SnapshotReady: complete, and VMs can be restored from it.
	SnapshotReady SnapshotState = "ready"
	// SnapshotDeleting: its files are being removed. Its record goes after.
	SnapshotDeleting SnapshotState = "deleting"
)

// Snapshot is tined's record of a snapshot. Its files are in the snapshot store.
type Snapshot struct {
	ID    string
	Kind  SnapshotKind
	State SnapshotState
	// VMID is the VM it was taken from. For a template, that's the VM that
	// was booted to make it, which is destroyed straight after.
	VMID  string
	Image string
	// VCPUs and MemoryMiB are the VM's size, which every VM restored from
	// the snapshot has too.
	VCPUs     int
	MemoryMiB int
	// SizeBytes is how much disk its files take.
	SizeBytes int64
	// Retained snapshots are kept until they're deleted, even once their VM
	// is destroyed. Only a VM's snapshot (SnapshotKindVM) can be retained.
	Retained bool
	// Metadata is its snapshot.json, set when it becomes ready.
	Metadata json.RawMessage

	CreatedAt time.Time
	UpdatedAt time.Time
}

// CreateSnapshot records a new snapshot in SnapshotCreating.
func (s *Store) CreateSnapshot(ctx context.Context, snap Snapshot) (Snapshot, error) {
	if !validVMID.MatchString(snap.ID) {
		return Snapshot{}, fmt.Errorf("create snapshot: bad ID %q", snap.ID)
	}
	if snap.Kind != SnapshotKindVM && snap.Kind != SnapshotKindTemplate && snap.Kind != SnapshotKindFork {
		return Snapshot{}, fmt.Errorf("create snapshot %s: bad kind %q", snap.ID, snap.Kind)
	}
	if snap.Retained && snap.Kind != SnapshotKindVM {
		return Snapshot{}, fmt.Errorf("create snapshot %s: only a VM's snapshot can be retained, not a %s", snap.ID, snap.Kind)
	}
	now := s.now()
	snap.State = SnapshotCreating
	snap.CreatedAt, snap.UpdatedAt = now, now
	snap.SizeBytes, snap.Metadata = 0, nil
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO snapshots (id, kind, state, vm_id, image, vcpus, memory_mib, retained, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		snap.ID, snap.Kind, snap.State, nullString(snap.VMID), snap.Image, snap.VCPUs, snap.MemoryMiB, snap.Retained,
		formatTime(now), formatTime(now))
	if err != nil {
		if isConstraint(err) {
			return Snapshot{}, fmt.Errorf("create snapshot %s: the ID is taken: %w", snap.ID, ErrConflict)
		}
		return Snapshot{}, fmt.Errorf("create snapshot %s: %w", snap.ID, err)
	}
	return snap, nil
}

// GetSnapshot returns one snapshot.
func (s *Store) GetSnapshot(ctx context.Context, id string) (Snapshot, error) {
	return getSnapshot(ctx, s.db, id)
}

// SnapshotFilter narrows ListSnapshots. Zero fields match everything.
type SnapshotFilter struct {
	VMID  string
	Kind  SnapshotKind
	Image string
}

// ListSnapshots returns snapshots, oldest first.
func (s *Store) ListSnapshots(ctx context.Context, f SnapshotFilter) ([]Snapshot, error) {
	var (
		where []string
		args  []any
	)
	if f.VMID != "" {
		where, args = append(where, "vm_id = ?"), append(args, f.VMID)
	}
	if f.Kind != "" {
		where, args = append(where, "kind = ?"), append(args, f.Kind)
	}
	if f.Image != "" {
		where, args = append(where, "image = ?"), append(args, f.Image)
	}
	q := "SELECT " + snapshotColumns + " FROM snapshots"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	return querySnapshots(ctx, s.db, q+" ORDER BY created_at, id", args...)
}

// SnapshotReady marks a snapshot that was being created as complete, with
// the size of its files and its snapshot.json.
func (s *Store) SnapshotReady(ctx context.Context, id string, sizeBytes int64, metadata json.RawMessage) (Snapshot, error) {
	if !json.Valid(metadata) {
		return Snapshot{}, fmt.Errorf("snapshot %s: metadata isn't valid JSON", id)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Snapshot{}, err
	}
	defer func() { _ = tx.Rollback() }()
	snap, err := getSnapshot(ctx, tx, id)
	if err != nil {
		return Snapshot{}, err
	}
	if snap.State != SnapshotCreating {
		return Snapshot{}, fmt.Errorf("snapshot %s is %s, not creating: %w", id, snap.State, ErrInvalidTransition)
	}
	snap.State, snap.SizeBytes, snap.Metadata, snap.UpdatedAt = SnapshotReady, sizeBytes, metadata, s.now()
	if _, err := tx.ExecContext(ctx, "UPDATE snapshots SET state = ?, size_bytes = ?, metadata = ?, updated_at = ? WHERE id = ?",
		snap.State, snap.SizeBytes, string(metadata), formatTime(snap.UpdatedAt), id); err != nil {
		return Snapshot{}, fmt.Errorf("snapshot %s: %w", id, err)
	}
	return snap, tx.Commit()
}

// MarkSnapshotDeleting starts deleting a snapshot. It's refused with ErrInUse
// while any VM that isn't destroyed runs from it. A snapshot already being
// deleted is returned as it is.
//
// The check and the change are one transaction, and so is a restore's claim
// on a snapshot (CreateVM, UpdateVM), so a snapshot can never be deleted
// from under a restore that has just picked it.
func (s *Store) MarkSnapshotDeleting(ctx context.Context, id string) (Snapshot, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Snapshot{}, err
	}
	defer func() { _ = tx.Rollback() }()
	snap, err := getSnapshot(ctx, tx, id)
	if err != nil {
		return Snapshot{}, err
	}
	if snap.State == SnapshotDeleting {
		return snap, nil
	}
	users, err := snapshotUsers(ctx, tx, id)
	if err != nil {
		return Snapshot{}, err
	}
	if len(users) > 0 {
		return Snapshot{}, fmt.Errorf("snapshot %s: %d VMs run from it (%s): %w", id, len(users), strings.Join(users, ", "), ErrInUse)
	}
	snap.State, snap.UpdatedAt = SnapshotDeleting, s.now()
	if _, err := tx.ExecContext(ctx, "UPDATE snapshots SET state = ?, updated_at = ? WHERE id = ?",
		snap.State, formatTime(snap.UpdatedAt), id); err != nil {
		return Snapshot{}, fmt.Errorf("snapshot %s: %w", id, err)
	}
	return snap, tx.Commit()
}

// DeleteSnapshotRecord removes the record of a snapshot whose files are
// gone. It must be SnapshotDeleting. Deleting a record that's already gone
// isn't an error.
func (s *Store) DeleteSnapshotRecord(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM snapshots WHERE id = ? AND state = 'deleting'", id)
	if err != nil {
		return fmt.Errorf("delete snapshot %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		snap, err := s.GetSnapshot(ctx, id)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		return fmt.Errorf("delete snapshot %s: it's %s, not deleting: %w", id, snap.State, ErrInvalidTransition)
	}
	return nil
}

// SnapshotUsers returns the VMs that aren't destroyed and run from a snapshot.
func (s *Store) SnapshotUsers(ctx context.Context, id string) ([]string, error) {
	return snapshotUsers(ctx, s.db, id)
}

// LatestTemplate returns the newest ready template for an image and size,
// or ErrNotFound if there's none.
func (s *Store) LatestTemplate(ctx context.Context, image string, vcpus, memoryMiB int) (Snapshot, error) {
	snaps, err := querySnapshots(ctx, s.db, "SELECT "+snapshotColumns+` FROM snapshots
		WHERE kind = 'template' AND state = 'ready' AND image = ? AND vcpus = ? AND memory_mib = ?
		ORDER BY created_at DESC, id DESC LIMIT 1`, image, vcpus, memoryMiB)
	if err != nil {
		return Snapshot{}, err
	}
	if len(snaps) == 0 {
		return Snapshot{}, fmt.Errorf("no template for %s with %d vCPUs and %d MiB: %w", image, vcpus, memoryMiB, ErrNotFound)
	}
	return snaps[0], nil
}

// GarbageSnapshots returns the snapshots nothing needs any more, oldest
// first: ready snapshots that no live VM runs from and that are either
//
//   - a VM's snapshot that isn't retained, whose VM is destroyed or gone,
//   - a fork's snapshot, or
//   - a template that a newer one has replaced.
//
// A fork's snapshot is garbage from the moment it's ready until its
// children claim it, so the fork must keep the caller from collecting it
// in between.
//
// It also returns snapshots stuck in SnapshotDeleting, to finish them.
// (Templates whose image is gone are the caller's to find: images live on
// disk, and the images table only knows those registered at startup.)
func (s *Store) GarbageSnapshots(ctx context.Context) ([]Snapshot, error) {
	return querySnapshots(ctx, s.db, "SELECT "+snapshotColumns+` FROM snapshots s
		WHERE s.state = 'deleting'
		   OR (s.state = 'ready'
		       AND NOT EXISTS (SELECT 1 FROM vms v WHERE v.snapshot_id = s.id AND v.state != 'destroyed')
		       AND (s.kind = 'fork'
		         OR (s.kind = 'vm' AND s.retained = 0
		             AND NOT EXISTS (SELECT 1 FROM vms v WHERE v.id = s.vm_id AND v.state != 'destroyed'))
		         OR (s.kind = 'template'
		             AND EXISTS (SELECT 1 FROM snapshots n
		                         WHERE n.kind = 'template' AND n.state = 'ready' AND n.image = s.image
		                           AND n.vcpus = s.vcpus AND n.memory_mib = s.memory_mib
		                           AND (n.created_at > s.created_at OR (n.created_at = s.created_at AND n.id > s.id))))))
		ORDER BY s.created_at, s.id`)
}

// --- row mapping ---

const snapshotColumns = "id, kind, state, vm_id, image, vcpus, memory_mib, size_bytes, retained, metadata, created_at, updated_at"

func getSnapshot(ctx context.Context, q querier, id string) (Snapshot, error) {
	snap, err := scanSnapshot(q.QueryRowContext(ctx, "SELECT "+snapshotColumns+" FROM snapshots WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Snapshot{}, fmt.Errorf("snapshot %s: %w", id, ErrNotFound)
	}
	return snap, err
}

func querySnapshots(ctx context.Context, q querier, query string, args ...any) ([]Snapshot, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var snaps []Snapshot
	for rows.Next() {
		snap, err := scanSnapshot(rows)
		if err != nil {
			return nil, err
		}
		snaps = append(snaps, snap)
	}
	return snaps, rows.Err()
}

func scanSnapshot(row scanner) (Snapshot, error) {
	var (
		snap             Snapshot
		kind, st         string
		vmID, metadata   sql.NullString
		created, updated string
	)
	if err := row.Scan(&snap.ID, &kind, &st, &vmID, &snap.Image, &snap.VCPUs, &snap.MemoryMiB,
		&snap.SizeBytes, &snap.Retained, &metadata, &created, &updated); err != nil {
		return Snapshot{}, err
	}
	snap.Kind, snap.State, snap.VMID = SnapshotKind(kind), SnapshotState(st), vmID.String
	if metadata.Valid {
		snap.Metadata = json.RawMessage(metadata.String)
	}
	var err error
	if snap.CreatedAt, err = parseTime(created); err != nil {
		return Snapshot{}, err
	}
	if snap.UpdatedAt, err = parseTime(updated); err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}

// snapshotUsers lists the live VMs that run from a snapshot.
func snapshotUsers(ctx context.Context, q querier, id string) ([]string, error) {
	rows, err := q.QueryContext(ctx, "SELECT id FROM vms WHERE snapshot_id = ? AND state != 'destroyed' ORDER BY id", id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var vmID string
		if err := rows.Scan(&vmID); err != nil {
			return nil, err
		}
		ids = append(ids, vmID)
	}
	return ids, rows.Err()
}

// requireReadySnapshot fails with ErrNotFound unless the snapshot exists and is ready.
func requireReadySnapshot(ctx context.Context, q querier, id string) error {
	var st string
	err := q.QueryRowContext(ctx, "SELECT state FROM snapshots WHERE id = ?", id).Scan(&st)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("snapshot %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return err
	}
	if SnapshotState(st) != SnapshotReady {
		return fmt.Errorf("snapshot %s is %s, not ready: %w", id, st, ErrNotFound)
	}
	return nil
}
