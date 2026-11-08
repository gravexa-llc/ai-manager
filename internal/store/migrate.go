package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"time"
)

// ErrSchemaTooNew is returned by Open when the database was migrated by a
// newer build than this one.
var ErrSchemaTooNew = errors.New("database schema is newer than this build")

//go:embed migrations/*.sql
var migrationFS embed.FS

// migration is one numbered SQL file.
type migration struct {
	version int
	name    string // file name, e.g. "0001_init.sql"
	sql     string
}

// migrationName is the file-name pattern: four digits, an underscore, a
// lowercase name, ".sql".
var migrationName = regexp.MustCompile(`^(\d{4})_[a-z0-9_]+\.sql$`)

// loadMigrations reads migrations/*.sql from fsys. Versions must run 1, 2,
// 3, … with no gaps or repeats, so two lanes adding the same number can't
// both merge unnoticed.
func loadMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, "migrations")
	if err != nil {
		return nil, fmt.Errorf("store: read migrations: %w", err)
	}
	var ms []migration
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := migrationName.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("store: migration %q: want a name like 0002_add_thing.sql", e.Name())
		}
		v, _ := strconv.Atoi(m[1])
		body, err := fs.ReadFile(fsys, "migrations/"+e.Name())
		if err != nil {
			return nil, fmt.Errorf("store: read migration %s: %w", e.Name(), err)
		}
		ms = append(ms, migration{version: v, name: e.Name(), sql: string(body)})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	for i, m := range ms {
		if m.version != i+1 {
			return nil, fmt.Errorf("store: migration %s: want version %04d (versions run 1, 2, 3, … with no gaps or repeats)", m.name, i+1)
		}
	}
	if len(ms) == 0 {
		return nil, errors.New("store: no migrations")
	}
	return ms, nil
}

// LatestVersion is the schema version this build migrates to: the number of
// the last embedded migration.
func LatestVersion() int {
	ms, err := loadMigrations(migrationFS)
	if err != nil {
		return 0
	}
	return ms[len(ms)-1].version
}

// SchemaVersion returns the database's schema version: the highest version
// recorded in schema_migrations.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v int
	err := s.r.QueryRowContext(ctx, `SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&v)
	return v, err
}

const createMigrationsTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    name       TEXT    NOT NULL,
    applied_at INTEGER NOT NULL
)`

// migrate applies the migrations newer than the database's version, in
// order, in one transaction on one connection of db.
//
// Foreign keys are switched off while the migrations run (SQLite ignores
// that pragma inside a transaction, so it happens before BEGIN) and the
// whole database is checked with foreign_key_check before the commit. That
// lets a later migration rebuild a table the way SQLite's ALTER TABLE
// documentation describes (create the new table, copy, drop, rename)
// without the drop cascading into child tables, while still refusing to
// commit rows that break a foreign key.
//
// The transaction starts with BEGIN IMMEDIATE (the writer's _txlock), so two
// processes opening the database at once take turns: the second reads the
// version only after the first has committed, and finds nothing to do.
func migrate(ctx context.Context, db *sql.DB, ms []migration) (err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return err
	}
	defer func() {
		if _, ferr := conn.ExecContext(context.WithoutCancel(ctx), `PRAGMA foreign_keys = ON`); ferr != nil && err == nil {
			err = ferr
		}
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() // a no-op after Commit

	if _, err := tx.ExecContext(ctx, createMigrationsTable); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	var have int
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(version), 0) FROM schema_migrations`).Scan(&have); err != nil {
		return err
	}
	latest := 0
	if len(ms) > 0 {
		latest = ms[len(ms)-1].version
	}
	if have > latest {
		return fmt.Errorf("%w: the database is at schema version %d and this build knows versions up to %d; run a newer aim/aimd, or move the database aside to start over", ErrSchemaTooNew, have, latest)
	}
	if have == latest {
		return nil // nothing to do; the deferred Rollback ends the read
	}

	now := time.Now().UnixMilli()
	for _, m := range ms[have:] {
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`, m.version, m.name, now); err != nil {
			return fmt.Errorf("record migration %s: %w", m.name, err)
		}
	}

	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	var bad []string
	for rows.Next() {
		var table, parent string
		var rowid sql.NullInt64
		var fkid int
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			rows.Close()
			return err
		}
		bad = append(bad, fmt.Sprintf("%s row %d → %s", table, rowid.Int64, parent))
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(bad) > 0 {
		return fmt.Errorf("migrations up to %s leave rows that break foreign keys: %v", ms[len(ms)-1].name, bad)
	}
	return tx.Commit()
}
