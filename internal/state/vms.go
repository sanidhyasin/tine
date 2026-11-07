package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// VMState is where a VM is in its life.
type VMState string

const (
	// StateCreating: tined is setting the VM up.
	StateCreating VMState = "creating"
	// StateRunning: the VM is up and its agent answers.
	StateRunning VMState = "running"
	// StateDestroying: tined is tearing it down.
	StateDestroying VMState = "destroying"
	// StateDestroyed: it's gone, and its slot and uid are free again. Kept for history.
	StateDestroyed VMState = "destroyed"
	// StateFailed: something went wrong. Its resources may still exist until
	// it's destroyed, which is always possible.
	StateFailed VMState = "failed"
)

// transitions is the VM state machine:
//
//	creating ──► running ──► destroying ──► destroyed
//	   │            │            │
//	   └─────► failed ◄──────────┘
//	              │
//	              └──► destroying
//
// A VM being created can also go straight to destroying, if it's destroyed
// before it finishes.
var transitions = map[VMState][]VMState{
	StateCreating:   {StateRunning, StateFailed, StateDestroying},
	StateRunning:    {StateDestroying, StateFailed},
	StateFailed:     {StateDestroying},
	StateDestroying: {StateDestroyed, StateFailed},
	StateDestroyed:  nil,
}

// CanTransition reports whether a VM may go from one state to another.
func CanTransition(from, to VMState) bool {
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// Live reports whether a VM in this state still holds host resources.
func (s VMState) Live() bool { return s != StateDestroyed }

// VM is one VM's record.
type VM struct {
	ID        string
	State     VMState
	Image     string
	VCPUs     int
	MemoryMiB int
	// Slot is its network slot, and UID its Firecracker's uid and gid. Both
	// are unique among VMs that aren't destroyed.
	Slot int
	UID  int
	// PID is Firecracker's process ID, or 0 before it starts.
	PID int
	// Error says why the VM failed.
	Error string
	// SnapshotID is the snapshot the VM was restored from and runs from, or
	// empty for a VM that was booted from scratch.
	SnapshotID string
	// Timings is how long each stage of creating it took, in milliseconds.
	Timings map[string]float64

	CreatedAt   time.Time
	UpdatedAt   time.Time
	ExpiresAt   time.Time // zero means never
	DestroyedAt time.Time // zero until destroyed
}

// Event is one change of a VM's state.
type Event struct {
	ID     int64
	VMID   string
	At     time.Time
	From   VMState // empty for the event that created the VM
	To     VMState
	Detail string
}

var validVMID = regexp.MustCompile(`^[a-zA-Z0-9-]{1,64}$`)

// CreateVM records a new VM in StateCreating. Its ID, slot and uid must not
// clash with an existing VM's (ErrConflict). If it has a SnapshotID, that
// snapshot must be ready (ErrNotFound otherwise): checking in the same
// transaction means it can't be deleted in between.
func (s *Store) CreateVM(ctx context.Context, vm VM) (VM, error) {
	if !validVMID.MatchString(vm.ID) {
		return VM{}, fmt.Errorf("create VM: bad ID %q", vm.ID)
	}
	now := s.now()
	vm.State = StateCreating
	vm.CreatedAt, vm.UpdatedAt = now, now
	vm.DestroyedAt = time.Time{}
	timings, err := encodeTimings(vm.Timings)
	if err != nil {
		return VM{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return VM{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if vm.SnapshotID != "" {
		if err := requireReadySnapshot(ctx, tx, vm.SnapshotID); err != nil {
			return VM{}, fmt.Errorf("create VM %s: %w", vm.ID, err)
		}
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO vms (id, state, image, vcpus, memory_mib, slot, uid, pid, error, snapshot_id, timings, created_at, updated_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		vm.ID, vm.State, vm.Image, vm.VCPUs, vm.MemoryMiB, vm.Slot, vm.UID,
		nullInt(vm.PID), nullString(vm.Error), nullString(vm.SnapshotID), timings,
		formatTime(vm.CreatedAt), formatTime(vm.UpdatedAt), nullTime(vm.ExpiresAt))
	if err != nil {
		if isConstraint(err) {
			return VM{}, fmt.Errorf("create VM %s: its ID, slot %d or uid %d is taken: %w", vm.ID, vm.Slot, vm.UID, ErrConflict)
		}
		return VM{}, fmt.Errorf("create VM %s: %w", vm.ID, err)
	}
	if err := insertEvent(ctx, tx, vm.ID, now, "", vm.State, "created"); err != nil {
		return VM{}, err
	}
	if err := tx.Commit(); err != nil {
		return VM{}, err
	}
	return vm, nil
}

// GetVM returns one VM.
func (s *Store) GetVM(ctx context.Context, id string) (VM, error) {
	return getVM(ctx, s.db, id)
}

// ListOptions filters ListVMs.
type ListOptions struct {
	// IncludeDestroyed adds VMs that are gone. By default only live ones are listed.
	IncludeDestroyed bool
}

// ListVMs returns VMs, oldest first.
func (s *Store) ListVMs(ctx context.Context, opts ListOptions) ([]VM, error) {
	q := "SELECT " + vmColumns + " FROM vms"
	if !opts.IncludeDestroyed {
		q += " WHERE state != 'destroyed'"
	}
	return queryVMs(ctx, s.db, q+" ORDER BY created_at, id")
}

// ExpiredVMs returns VMs whose time is up at now and that nobody is already
// tearing down: the reaper's to-do list.
func (s *Store) ExpiredVMs(ctx context.Context, now time.Time) ([]VM, error) {
	return queryVMs(ctx, s.db, "SELECT "+vmColumns+` FROM vms
		WHERE expires_at IS NOT NULL AND expires_at <= ? AND state NOT IN ('destroying', 'destroyed')
		ORDER BY expires_at, id`, formatTime(now))
}

// UpdateVM changes a VM in one write transaction. It loads the VM, lets change
// edit it, checks that any state change is allowed (ErrInvalidTransition
// otherwise), saves it, and records an Event with detail if the state
// changed. If change returns an error, nothing is saved. A new SnapshotID
// must be a ready snapshot (ErrNotFound otherwise).
//
// Moving to StateDestroyed sets DestroyedAt, and moving to StateFailed with
// no Error uses detail as the Error.
func (s *Store) UpdateVM(ctx context.Context, id, detail string, change func(vm *VM) error) (VM, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return VM{}, err
	}
	defer func() { _ = tx.Rollback() }()

	vm, err := getVM(ctx, tx, id)
	if err != nil {
		return VM{}, err
	}
	before := vm
	if err := change(&vm); err != nil {
		return VM{}, err
	}
	if vm.ID != before.ID || vm.CreatedAt != before.CreatedAt {
		return VM{}, fmt.Errorf("update VM %s: its ID and creation time can't change", id)
	}
	if vm.SnapshotID != before.SnapshotID && vm.SnapshotID != "" {
		if err := requireReadySnapshot(ctx, tx, vm.SnapshotID); err != nil {
			return VM{}, fmt.Errorf("update VM %s: %w", id, err)
		}
	}
	now := s.now()
	if vm.State != before.State {
		if !CanTransition(before.State, vm.State) {
			return VM{}, fmt.Errorf("VM %s: %s -> %s: %w", id, before.State, vm.State, ErrInvalidTransition)
		}
		switch vm.State {
		case StateDestroyed:
			vm.DestroyedAt = now
		case StateFailed:
			if vm.Error == "" {
				vm.Error = detail
			}
		}
	}
	vm.UpdatedAt = now
	timings, err := encodeTimings(vm.Timings)
	if err != nil {
		return VM{}, err
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE vms SET state = ?, image = ?, vcpus = ?, memory_mib = ?, slot = ?, uid = ?, pid = ?,
			error = ?, snapshot_id = ?, timings = ?, updated_at = ?, expires_at = ?, destroyed_at = ?
		WHERE id = ?`,
		vm.State, vm.Image, vm.VCPUs, vm.MemoryMiB, vm.Slot, vm.UID, nullInt(vm.PID),
		nullString(vm.Error), nullString(vm.SnapshotID), timings, formatTime(vm.UpdatedAt), nullTime(vm.ExpiresAt), nullTime(vm.DestroyedAt),
		vm.ID)
	if err != nil {
		if isConstraint(err) {
			return VM{}, fmt.Errorf("update VM %s: slot %d or uid %d is taken: %w", id, vm.Slot, vm.UID, ErrConflict)
		}
		return VM{}, fmt.Errorf("update VM %s: %w", id, err)
	}
	if vm.State != before.State {
		if err := insertEvent(ctx, tx, id, now, before.State, vm.State, detail); err != nil {
			return VM{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return VM{}, err
	}
	return vm, nil
}

// Transition moves a VM to a new state, recording detail as the reason. The
// state must actually change: asking for the state a VM is already in is
// ErrInvalidTransition, so of two callers racing to make the same change,
// exactly one wins.
func (s *Store) Transition(ctx context.Context, id string, to VMState, detail string) (VM, error) {
	return s.UpdateVM(ctx, id, detail, func(vm *VM) error {
		if vm.State == to {
			return fmt.Errorf("VM %s is already %s: %w", id, to, ErrInvalidTransition)
		}
		vm.State = to
		return nil
	})
}

// Note records something that happened to a VM without changing its state,
// such as a snapshot being taken, as an Event from its state to itself.
func (s *Store) Note(ctx context.Context, id, detail string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	vm, err := getVM(ctx, tx, id)
	if err != nil {
		return err
	}
	if err := insertEvent(ctx, tx, id, s.now(), vm.State, vm.State, detail); err != nil {
		return err
	}
	return tx.Commit()
}

// Events returns a VM's state changes, and the notes about it, oldest first.
func (s *Store) Events(ctx context.Context, vmID string) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT id, vm_id, at, from_state, to_state, detail FROM vm_events WHERE vm_id = ? ORDER BY id", vmID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var events []Event
	for rows.Next() {
		var (
			e    Event
			at   string
			from sql.NullString
		)
		if err := rows.Scan(&e.ID, &e.VMID, &at, &from, &e.To, &e.Detail); err != nil {
			return nil, err
		}
		if e.At, err = parseTime(at); err != nil {
			return nil, err
		}
		e.From = VMState(from.String)
		events = append(events, e)
	}
	return events, rows.Err()
}

// PurgeDestroyed deletes the records (and events) of VMs destroyed before
// cutoff, and returns how many went.
func (s *Store) PurgeDestroyed(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		"DELETE FROM vms WHERE state = 'destroyed' AND destroyed_at < ?", formatTime(cutoff))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// --- row mapping ---

const vmColumns = "id, state, image, vcpus, memory_mib, slot, uid, pid, error, snapshot_id, timings, created_at, updated_at, expires_at, destroyed_at"

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type scanner interface{ Scan(dest ...any) error }

func getVM(ctx context.Context, q querier, id string) (VM, error) {
	vm, err := scanVM(q.QueryRowContext(ctx, "SELECT "+vmColumns+" FROM vms WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return VM{}, fmt.Errorf("VM %s: %w", id, ErrNotFound)
	}
	return vm, err
}

func queryVMs(ctx context.Context, q querier, query string, args ...any) ([]VM, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var vms []VM
	for rows.Next() {
		vm, err := scanVM(rows)
		if err != nil {
			return nil, err
		}
		vms = append(vms, vm)
	}
	return vms, rows.Err()
}

func scanVM(row scanner) (VM, error) {
	var (
		vm                 VM
		pid                sql.NullInt64
		errText, timings   sql.NullString
		snapshotID         sql.NullString
		created, updated   string
		expires, destroyed sql.NullString
		state              string
	)
	if err := row.Scan(&vm.ID, &state, &vm.Image, &vm.VCPUs, &vm.MemoryMiB, &vm.Slot, &vm.UID,
		&pid, &errText, &snapshotID, &timings, &created, &updated, &expires, &destroyed); err != nil {
		return VM{}, err
	}
	vm.State = VMState(state)
	vm.PID = int(pid.Int64)
	vm.Error = errText.String
	vm.SnapshotID = snapshotID.String
	var err error
	if timings.Valid && timings.String != "" {
		if err := json.Unmarshal([]byte(timings.String), &vm.Timings); err != nil {
			return VM{}, fmt.Errorf("VM %s: bad timings: %w", vm.ID, err)
		}
	}
	if vm.CreatedAt, err = parseTime(created); err != nil {
		return VM{}, err
	}
	if vm.UpdatedAt, err = parseTime(updated); err != nil {
		return VM{}, err
	}
	if vm.ExpiresAt, err = parseNullTime(expires); err != nil {
		return VM{}, err
	}
	if vm.DestroyedAt, err = parseNullTime(destroyed); err != nil {
		return VM{}, err
	}
	return vm, nil
}

func insertEvent(ctx context.Context, tx *sql.Tx, vmID string, at time.Time, from, to VMState, detail string) error {
	_, err := tx.ExecContext(ctx,
		"INSERT INTO vm_events (vm_id, at, from_state, to_state, detail) VALUES (?, ?, ?, ?, ?)",
		vmID, formatTime(at), nullString(string(from)), to, strings.TrimSpace(detail))
	if err != nil {
		return fmt.Errorf("record event for VM %s: %w", vmID, err)
	}
	return nil
}

func encodeTimings(t map[string]float64) (sql.NullString, error) {
	if len(t) == 0 {
		return sql.NullString{}, nil
	}
	data, err := json.Marshal(t)
	if err != nil {
		return sql.NullString{}, err
	}
	return sql.NullString{String: string(data), Valid: true}, nil
}

func nullInt(n int) sql.NullInt64 {
	if n == 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(n), Valid: true}
}

func nullString(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
