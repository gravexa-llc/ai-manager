package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// rawDB opens a bare writer pool on a fresh file, for driving migrate with
// synthetic migrations.
func rawDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "raw.db")
	must(t, createFile(path))
	db, err := sql.Open("sqlite", dsn(path, false))
	must(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func tables(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	must(t, err)
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		must(t, rows.Scan(&n))
		names = append(names, n)
	}
	must(t, rows.Err())
	return names
}

func versions(t *testing.T, db *sql.DB) []int {
	t.Helper()
	rows, err := db.Query(`SELECT version FROM schema_migrations ORDER BY version`)
	must(t, err)
	defer rows.Close()
	var vs []int
	for rows.Next() {
		var v int
		must(t, rows.Scan(&v))
		vs = append(vs, v)
	}
	must(t, rows.Err())
	return vs
}

func synthetic(t *testing.T, files map[string]string) []migration {
	t.Helper()
	fsys := fstest.MapFS{}
	for name, body := range files {
		fsys["migrations/"+name] = &fstest.MapFile{Data: []byte(body)}
	}
	ms, err := loadMigrations(fsys)
	must(t, err)
	return ms
}

func TestMigrateFromEmpty(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	v, err := s.SchemaVersion(ctx)
	must(t, err)
	if v != LatestVersion() || v < 1 {
		t.Errorf("SchemaVersion() = %d, want LatestVersion() = %d", v, LatestVersion())
	}
	want := []string{"agents", "events", "inbox_items", "inbox_moves", "lifecycle_history", "link_tombstones", "links", "monitored_sessions", "packages", "plan_alerts", "plan_dismissals", "plan_merges", "plans", "provisional_packages", "provisional_sources", "prs", "removal_log", "repos", "schema_migrations", "sessions", "settings", "unmatched_hidden", "worktrees"}
	equal(t, "tables", tables(t, s.w), want)

	var name string
	must(t, s.r.QueryRow(`SELECT name FROM schema_migrations WHERE version = 1`).Scan(&name))
	if name != "0001_init.sql" {
		t.Errorf("version 1 recorded as %q", name)
	}
}

func TestMigrateRerunIsNoOp(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), FileName)
	s, err := Open(ctx, path)
	must(t, err)
	_, err = s.InsertPlan(ctx, Plan{Slug: "keep"})
	must(t, err)
	var before []string
	rows, err := s.r.Query(`SELECT version || ':' || name || ':' || applied_at FROM schema_migrations ORDER BY version`)
	must(t, err)
	for rows.Next() {
		var r string
		must(t, rows.Scan(&r))
		before = append(before, r)
	}
	rows.Close()
	must(t, s.Close())

	for range 2 {
		s, err = Open(ctx, path)
		must(t, err)
		var after []string
		rows, err := s.r.Query(`SELECT version || ':' || name || ':' || applied_at FROM schema_migrations ORDER BY version`)
		must(t, err)
		for rows.Next() {
			var r string
			must(t, rows.Scan(&r))
			after = append(after, r)
		}
		rows.Close()
		equal(t, "schema_migrations after reopening", after, before)
		if _, err := s.GetPlanBySlug(ctx, "keep"); err != nil {
			t.Errorf("data lost on reopen: %v", err)
		}
		must(t, s.Close())
	}
}

func TestMigrateTooNew(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), FileName)
	s, err := Open(ctx, path)
	must(t, err)
	future := LatestVersion() + 1
	_, err = s.w.Exec(`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, '9999_future.sql', 0)`, future)
	must(t, err)
	must(t, s.Close())

	_, err = Open(ctx, path)
	if !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("Open() error = %v, want ErrSchemaTooNew", err)
	}
	for _, want := range []string{path, fmt.Sprintf("schema version %d", future), fmt.Sprintf("up to %d", LatestVersion()), "newer aim"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q doesn't mention %q", err, want)
		}
	}

	// The database is left as it was.
	db := rawDB(t)
	_, err = db.Exec(`ATTACH DATABASE ? AS old`, path)
	must(t, err)
	var n int
	must(t, db.QueryRow(`SELECT count(*) FROM old.schema_migrations`).Scan(&n))
	if n != LatestVersion()+1 {
		t.Errorf("schema_migrations has %d rows after the refused open, want %d", n, LatestVersion()+1)
	}
}

func TestMigrateAppliesOnlyNewOnes(t *testing.T) {
	db := rawDB(t)
	ctx := context.Background()
	one := synthetic(t, map[string]string{
		"0001_one.sql": `CREATE TABLE t1 (x INTEGER);`,
	})
	two := synthetic(t, map[string]string{
		"0001_one.sql": `CREATE TABLE t1 (x INTEGER);`, // would fail if run twice
		"0002_two.sql": `CREATE TABLE t2 (y INTEGER); INSERT INTO t2 VALUES (1), (2);`,
	})

	must(t, migrate(ctx, db, one))
	equal(t, "versions after one", versions(t, db), []int{1})
	must(t, migrate(ctx, db, two))
	equal(t, "versions after two", versions(t, db), []int{1, 2})
	equal(t, "tables", tables(t, db), []string{"schema_migrations", "t1", "t2"})
	var n int
	must(t, db.QueryRow(`SELECT count(*) FROM t2`).Scan(&n)) // both statements of 0002 ran
	if n != 2 {
		t.Errorf("t2 has %d rows, want 2", n)
	}
	must(t, migrate(ctx, db, two))
	equal(t, "versions after rerun", versions(t, db), []int{1, 2})
}

func TestMigrateFailureRollsBackEverything(t *testing.T) {
	db := rawDB(t)
	ms := synthetic(t, map[string]string{
		"0001_ok.sql":  `CREATE TABLE t1 (x INTEGER);`,
		"0002_bad.sql": `CREATE TABLE t2 (y INTEGER); THIS IS NOT SQL;`,
	})
	err := migrate(context.Background(), db, ms)
	if err == nil || !strings.Contains(err.Error(), "0002_bad.sql") {
		t.Fatalf("migrate() error = %v, want one naming 0002_bad.sql", err)
	}
	if got := tables(t, db); len(got) != 0 {
		t.Errorf("tables after a failed migration = %q, want none", got)
	}
}

func TestMigrateRefusesBrokenForeignKeys(t *testing.T) {
	db := rawDB(t)
	ms := synthetic(t, map[string]string{
		"0001_tables.sql": `
			CREATE TABLE parent (id INTEGER PRIMARY KEY);
			CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER NOT NULL REFERENCES parent (id));
			INSERT INTO child VALUES (1, 42);`,
	})
	err := migrate(context.Background(), db, ms)
	if err == nil || !strings.Contains(err.Error(), "foreign key") {
		t.Fatalf("migrate() error = %v, want a foreign key error", err)
	}
	if got := tables(t, db); len(got) != 0 {
		t.Errorf("tables after a refused migration = %q, want none", got)
	}
}

// A later migration can rebuild a parent table the way SQLite's ALTER TABLE
// docs describe without the DROP cascading into its children, because
// migrate runs with foreign keys off.
func TestMigrateTableRebuildKeepsChildren(t *testing.T) {
	db := rawDB(t)
	ctx := context.Background()
	base := map[string]string{
		"0001_tables.sql": `
			CREATE TABLE parent (id INTEGER PRIMARY KEY, name TEXT);
			CREATE TABLE child (id INTEGER PRIMARY KEY,
				parent_id INTEGER NOT NULL REFERENCES parent (id) ON DELETE CASCADE);
			INSERT INTO parent VALUES (1, 'p');
			INSERT INTO child VALUES (10, 1), (11, 1);`,
	}
	must(t, migrate(ctx, db, synthetic(t, base)))
	base["0002_rebuild.sql"] = `
		CREATE TABLE parent_new (id INTEGER PRIMARY KEY, name TEXT NOT NULL DEFAULT '');
		INSERT INTO parent_new SELECT id, name FROM parent;
		DROP TABLE parent;
		ALTER TABLE parent_new RENAME TO parent;`
	must(t, migrate(ctx, db, synthetic(t, base)))

	var n int
	must(t, db.QueryRow(`SELECT count(*) FROM child`).Scan(&n))
	if n != 2 {
		t.Errorf("child has %d rows after the rebuild, want 2", n)
	}
	var fk int
	must(t, db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk))
	if fk != 1 {
		t.Error("foreign keys left off after migrating")
	}
	// And the rebuilt table still enforces the key.
	if _, err := db.Exec(`INSERT INTO child VALUES (12, 99)`); err == nil {
		t.Error("a child row with no parent was accepted after the rebuild")
	}
}

func TestLoadMigrations(t *testing.T) {
	ms, err := loadMigrations(migrationFS)
	must(t, err)
	for i, m := range ms {
		if m.version != i+1 || strings.TrimSpace(m.sql) == "" {
			t.Errorf("embedded migration %d: %+v", i, m)
		}
	}
	if ms[0].name != "0001_init.sql" {
		t.Errorf("first migration is %s", ms[0].name)
	}

	for _, tc := range []struct {
		name  string
		files []string
		want  string
	}{
		{"none", nil, "no migrations"},
		{"short number", []string{"001_init.sql"}, "want a name like"},
		{"dash", []string{"0001-init.sql"}, "want a name like"},
		{"upper case", []string{"0001_Init.sql"}, "want a name like"},
		{"other file", []string{"0001_init.sql", "README.md"}, "want a name like"},
		{"gap", []string{"0001_a.sql", "0003_c.sql"}, "want version 0002"},
		{"repeat", []string{"0001_a.sql", "0001_b.sql"}, "want version 0002"},
		{"not from one", []string{"0002_b.sql"}, "want version 0001"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fsys := fstest.MapFS{"migrations": &fstest.MapFile{Mode: fs.ModeDir | 0o755}} // a dir, even when empty
			for _, f := range tc.files {
				fsys["migrations/"+f] = &fstest.MapFile{Data: []byte("SELECT 1;")}
			}
			_, err := loadMigrations(fsys)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("loadMigrations(%q) error = %v, want it to contain %q", tc.files, err, tc.want)
			}
		})
	}
}

// Two stores opening one new file at once both succeed, and the
// migrations run once.
func TestConcurrentOpenMigratesOnce(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), FileName)
	errs := make(chan error, 4)
	stores := make(chan *Store, 4)
	for range 4 {
		go func() {
			s, err := Open(ctx, path)
			errs <- err
			stores <- s
		}()
	}
	for range 4 {
		if err := <-errs; err != nil {
			t.Error(err)
		}
		if s := <-stores; s != nil {
			defer s.Close()
		}
	}
	db := rawDB(t)
	_, err := db.Exec(`ATTACH DATABASE ? AS shared`, path)
	must(t, err)
	var vs []int
	rows, err := db.Query(`SELECT version FROM shared.schema_migrations ORDER BY version`)
	must(t, err)
	for rows.Next() {
		var v int
		must(t, rows.Scan(&v))
		vs = append(vs, v)
	}
	rows.Close()
	want := make([]int, LatestVersion())
	for i := range want {
		want[i] = i + 1
	}
	if !slices.Equal(vs, want) {
		t.Errorf("versions = %v, want %v", vs, want)
	}
}
