// Package state is tined's memory: the images it knows and every VM it has
// run, kept in SQLite so they survive restarts.
//
// The database lives at DefaultPath. Its schema is built from the numbered
// SQL files in migrations/, applied in order at Open, each in a transaction.
// PRAGMA user_version records how many have run, and Open refuses a database
// from a newer tine rather than misread it.
//
// VM states follow a fixed state machine (see CanTransition), and the store
// refuses changes outside it. Every state change is recorded as an Event.
package state

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// DefaultPath is where tined keeps its database.
const DefaultPath = "/var/lib/tine/tine.db"

//go:embed migrations/*.sql
var migrationFiles embed.FS

var (
	// ErrNotFound means no such image or VM.
	ErrNotFound = errors.New("not found")
	// ErrConflict means it would clash with something that exists: the same
	// ID, or a slot or uid that a live VM holds.
	ErrConflict = errors.New("conflict")
	// ErrInvalidTransition means the VM can't go from its current state to the
	// one asked for.
	ErrInvalidTransition = errors.New("invalid state transition")
	// ErrInUse means an image can't be removed while VMs use it.
	ErrInUse = errors.New("in use")
)

// Store is tined's database. It's safe for concurrent use.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Option configures Open.
type Option func(*Store)

// WithClock replaces time.Now, for tests.
func WithClock(now func() time.Time) Option { return func(s *Store) { s.now = now } }

// Open opens (or creates) the database at path and brings its schema up to date.
func Open(ctx context.Context, path string, opts ...Option) (*Store, error) {
	return open(ctx, path, migrationFiles, opts...)
}

func open(ctx context.Context, path string, migrations fs.FS, opts ...Option) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	// WAL lets readers work while a write is in progress, busy_timeout waits
	// for a lock instead of failing at once, and _txlock=immediate makes every
	// transaction take the write lock up front, so read-modify-write updates
	// can't interleave.
	dsn := path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("state: open %s: %w", path, err)
	}
	s := &Store{db: db, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	if err := s.migrate(ctx, migrations); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("state: %s: %w", path, err)
	}
	// It describes every VM on the host: only tined needs to read it.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("state: %w", err)
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// SchemaVersion is how many migrations the database has had.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	return v, err
}

type migration struct {
	version int
	name    string
	sql     string
}

func loadMigrations(files fs.FS) ([]migration, error) {
	paths, err := fs.Glob(files, "migrations/*.sql")
	if err != nil {
		return nil, err
	}
	var ms []migration
	for _, p := range paths {
		base := filepath.Base(p)
		num, _, ok := strings.Cut(base, "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil || v <= 0 {
			return nil, fmt.Errorf("migration %s: name must start with a positive number and an underscore", base)
		}
		data, err := fs.ReadFile(files, p)
		if err != nil {
			return nil, err
		}
		ms = append(ms, migration{version: v, name: base, sql: string(data)})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	for i, m := range ms {
		if m.version != i+1 {
			return nil, fmt.Errorf("migrations must be numbered 1, 2, 3, ... without gaps; %s is number %d", m.name, i+1)
		}
	}
	return ms, nil
}

func (s *Store) migrate(ctx context.Context, files fs.FS) error {
	ms, err := loadMigrations(files)
	if err != nil {
		return err
	}
	current, err := s.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	if current > len(ms) {
		return fmt.Errorf("the database's schema is version %d, but this tine only knows up to %d: it was written by a newer tine", current, len(ms))
	}
	for _, m := range ms[current:] {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
		// user_version changes with the transaction, so a failed migration
		// leaves the version where it was.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
	}
	return nil
}

// --- helpers shared by the tables ---

// timeFormat is RFC 3339 in UTC with all nine fractional digits. The fixed
// width matters: SQLite compares these as text, and with trailing zeros
// trimmed (as time.RFC3339Nano does) "12:00:00.001Z" would sort before
// "12:00:00Z".
const timeFormat = "2006-01-02T15:04:05.000000000Z"

func formatTime(t time.Time) string { return t.UTC().Format(timeFormat) }

func parseTime(s string) (time.Time, error) { return time.Parse(timeFormat, s) }

func nullTime(t time.Time) sql.NullString {
	if t.IsZero() {
		return sql.NullString{}
	}
	return sql.NullString{String: formatTime(t), Valid: true}
}

func parseNullTime(ns sql.NullString) (time.Time, error) {
	if !ns.Valid {
		return time.Time{}, nil
	}
	return parseTime(ns.String)
}

// isConstraint reports whether err is a SQLite constraint violation: a
// duplicate key or a clash on a unique index.
func isConstraint(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	switch se.Code() {
	case sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
		return true
	}
	return false
}

// Ping checks the database answers.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }
