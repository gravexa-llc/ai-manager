// Package store is aimd's SQLite database: the schema in numbered
// migrations, and typed queries over plain database/sql.
//
// # Engine and file
//
// The driver is modernc.org/sqlite (pure Go, no cgo). The database is one
// file, [FileName], in the ai-manager home ($AIM_HOME, else ~/.ai-manager;
// see internal/config). [OpenHome] creates the home (mode 0700) and the file
// (mode 0600) when they are missing; SQLite gives its -wal and -shm files the
// database file's mode.
//
// Every connection runs with foreign keys on and busy_timeout at
// [BusyTimeout]. The file is in WAL mode with synchronous=NORMAL: a crash
// or power cut can lose the last commits, never corrupt the file, and this
// database is a cache of state that the next poll rebuilds.
//
// # One writer, many readers
//
// A [Store] holds two database/sql pools on the same file:
//
//   - The writer pool has exactly one connection. Every method that changes
//     data uses it, so writes from this process queue in Go instead of
//     racing each other for SQLite's write lock. Its transactions start with
//     BEGIN IMMEDIATE, so a transaction holds the write lock from its first
//     statement and can't fail half-way with SQLITE_BUSY when it would
//     upgrade from reading to writing.
//   - The reader pool has up to [MaxReaders] connections, opened with
//     query_only so that a read can never write. Every method that only
//     reads uses it.
//
// In WAL mode readers never block the writer and the writer never blocks
// readers: a read sees the last committed state, and sees a commit as soon
// as it returns. busy_timeout covers what is left, which is other processes
// on the same file (the aim CLI reading while aimd writes, or a second
// writer): they wait for the lock rather than fail.
//
// Inside the package, a method that writes must do all its work, reads
// included, through the writer (or its transaction): the writer has one
// connection, so a nested call that asks the writer pool for another would
// wait forever.
//
// # Conventions
//
// Times are stored as INTEGER Unix milliseconds. A zero [time.Time] is
// stored as NULL and read back as the zero time; other times are truncated
// to the millisecond and read back in UTC. Optional text is stored as an
// empty string rather than NULL. Lists of strings (a plan's repos, a package's needs, a
// worktree's ignored files at risk) are JSON arrays in a TEXT column: they
// are always read and written whole with their row, they're short, order
// matters, and needs can name IDs the plan doesn't define (the parser warns
// about those), which a child table with a foreign key couldn't hold.
//
// Values from a fixed set that aimd itself defines (link kinds, match
// methods, confidences) are checked here, in Go. Values that come from
// outside (a session's kind or state, a PR's state) are stored as given, so
// a new value from Claude Code or GitHub doesn't break a write. Neither uses
// CHECK constraints, so a later change can add a value without rebuilding a
// table.
//
// # Migrations
//
// The schema lives in migrations/NNNN_name.sql, embedded in the binary. See
// [Open] for how they are applied. One change at a time adds a migration:
// a change adds its own next-numbered file and never edits one that has
// been merged.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"modernc.org/sqlite" // registers the "sqlite" driver
	sqlite3 "modernc.org/sqlite/lib"
)

// FileName is the database's file name inside the ai-manager home.
const FileName = "aim.db"

// BusyTimeout is how long a connection waits for a lock held by another
// connection before it gives up with SQLITE_BUSY.
const BusyTimeout = 5 * time.Second

// MaxReaders is the size of the reader pool.
const MaxReaders = 4

// ErrNotFound is returned when the row a call names doesn't exist.
var ErrNotFound = errors.New("store: not found")

// Store is an open database. Its methods are safe for concurrent use.
type Store struct {
	path string
	w    *sql.DB // the writer: one connection, BEGIN IMMEDIATE
	r    *sql.DB // the readers: query_only

	// now is the clock for times a caller leaves zero. Tests replace it.
	now func() time.Time
	// purgeBatch is how many events PurgeEvents deletes per statement.
	purgeBatch int
}

// OpenHome opens the database in the ai-manager home directory, creating
// the directory (mode 0700) if needed. home is usually config.Home().
func OpenHome(ctx context.Context, home string) (*Store, error) {
	if home == "" {
		return nil, errors.New("store: empty home directory")
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, fmt.Errorf("store: create %s: %w", home, err)
	}
	return Open(ctx, filepath.Join(home, FileName))
}

// Open opens the database at path, creating the file (mode 0600) if it
// doesn't exist; its directory must exist. It then brings the schema up to
// date: every embedded migration newer than the database's version is
// applied in order, all in one transaction, and recorded in the
// schema_migrations table. Opening an up-to-date database applies nothing.
// A database whose version is newer than this build knows is refused with
// an error wrapping [ErrSchemaTooNew], and left untouched.
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: empty database path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	if err := createFile(abs); err != nil {
		return nil, err
	}
	migrations, err := loadMigrations(migrationFS)
	if err != nil {
		return nil, err
	}

	s := &Store{path: abs, now: time.Now, purgeBatch: 5000}

	s.w, err = sql.Open("sqlite", dsn(abs, false))
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", abs, err)
	}
	s.w.SetMaxOpenConns(1)
	s.w.SetMaxIdleConns(1)
	s.w.SetConnMaxLifetime(0)
	s.w.SetConnMaxIdleTime(0)

	if err := setWAL(ctx, s.w); err != nil {
		s.w.Close()
		return nil, fmt.Errorf("store: open %s: %w", abs, err)
	}

	if err := migrate(ctx, s.w, migrations); err != nil {
		s.w.Close()
		return nil, fmt.Errorf("store: %s: %w", abs, err)
	}

	// The readers open after the migrations, so they never see an old schema.
	s.r, err = sql.Open("sqlite", dsn(abs, true))
	if err != nil {
		s.w.Close()
		return nil, fmt.Errorf("store: open %s: %w", abs, err)
	}
	s.r.SetMaxOpenConns(MaxReaders)
	s.r.SetMaxIdleConns(MaxReaders)
	if err := s.r.PingContext(ctx); err != nil {
		s.Close()
		return nil, fmt.Errorf("store: open %s: %w", abs, err)
	}
	return s, nil
}

// createFile creates the database file with mode 0600 if it is missing, so
// SQLite never creates it with the umask's wider default. An existing file
// is left as it is.
func createFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: create %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("store: create %s: %w", path, err)
	}
	return nil
}

// setWAL switches the database to WAL mode, which is persistent: it is
// stored in the file, so every later connection uses it.
//
// Switching a new file needs an exclusive lock, and when another connection
// holds a lock at that moment (two processes opening a new database
// together) SQLite can answer SQLITE_BUSY at once instead of waiting out
// busy_timeout; TestConcurrentOpenMigratesOnce hit that. So the switch is
// retried here, for up to BusyTimeout. On a file already in WAL mode it is a
// no-op.
func setWAL(ctx context.Context, db *sql.DB) error {
	deadline := time.Now().Add(BusyTimeout)
	for {
		var mode string
		err := db.QueryRowContext(ctx, "PRAGMA journal_mode = WAL").Scan(&mode)
		switch {
		case err == nil && strings.EqualFold(mode, "wal"):
			return nil
		case err == nil:
			return fmt.Errorf("journal_mode is %q, want wal (is the file on a network volume?)", mode)
		case !isBusy(err) || time.Now().After(deadline):
			return fmt.Errorf("set WAL mode: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// isBusy reports whether err is SQLITE_BUSY or one of its extended codes.
func isBusy(err error) bool {
	var e *sqlite.Error
	return errors.As(err, &e) && e.Code()&0xff == sqlite3.SQLITE_BUSY
}

// dsn builds the connection string for the file at the absolute path. The
// file: form escapes characters such as '?' and '#' that a plain path would
// misread.
func dsn(path string, readOnly bool) string {
	q := url.Values{}
	q.Set("_busy_timeout", fmt.Sprint(BusyTimeout.Milliseconds()))
	q.Set("_foreign_keys", "1")
	if readOnly {
		q.Set("_query_only", "1")
	} else {
		q.Set("_synchronous", "NORMAL")
		q.Set("_txlock", "immediate")
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: q.Encode()}
	return u.String()
}

// Path returns the database file's absolute path.
func (s *Store) Path() string { return s.path }

// Close closes both pools.
func (s *Store) Close() error {
	var errs []error
	if s.r != nil {
		errs = append(errs, s.r.Close())
	}
	if s.w != nil {
		errs = append(errs, s.w.Close())
	}
	return errors.Join(errs...)
}

// writeTx runs fn in a transaction on the writer and commits it if fn
// returns nil. The transaction starts with BEGIN IMMEDIATE.
func (s *Store) writeTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// stamp returns t, or the store's clock when t is zero.
func (s *Store) stamp(t time.Time) time.Time {
	if t.IsZero() {
		return s.now()
	}
	return t
}

// ms converts a time to the stored form: Unix milliseconds, or NULL for the
// zero time.
func ms(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

// msTime scans a stored time (Unix milliseconds or NULL) into a time.Time.
type msTime struct{ t *time.Time }

func (m msTime) Scan(v any) error {
	switch v := v.(type) {
	case nil:
		*m.t = time.Time{}
	case int64:
		*m.t = time.UnixMilli(v).UTC()
	default:
		return fmt.Errorf("store: time column holds %T, want an integer", v)
	}
	return nil
}

// listJSON converts a list to the stored form, a JSON array. nil is "[]".
func listJSON(v []string) (string, error) {
	if len(v) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("store: encode list: %w", err)
	}
	return string(b), nil
}

// jsonList scans a stored JSON array into a []string. An empty array reads
// as nil.
type jsonList struct{ v *[]string }

func (j jsonList) Scan(src any) error {
	var b []byte
	switch src := src.(type) {
	case string:
		b = []byte(src)
	case []byte:
		b = src
	default:
		return fmt.Errorf("store: list column holds %T, want JSON text", src)
	}
	var v []string
	if err := json.Unmarshal(b, &v); err != nil {
		return fmt.Errorf("store: decode list: %w", err)
	}
	if len(v) == 0 {
		v = nil
	}
	*j.v = v
	return nil
}

// scanner is a *sql.Row or *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

// notFound turns sql.ErrNoRows into ErrNotFound with some context.
func notFound(err error, format string, args ...any) error {
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrNotFound, fmt.Sprintf(format, args...))
	}
	return err
}

// mustAffect returns ErrNotFound when res changed no row.
func mustAffect(res sql.Result, format string, args ...any) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, fmt.Sprintf(format, args...))
	}
	return nil
}
