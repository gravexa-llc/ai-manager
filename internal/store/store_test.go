package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenHomeCreatesDirAndFile(t *testing.T) {
	home := filepath.Join(t.TempDir(), "aim-home")
	s, err := OpenHome(context.Background(), home)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if want := filepath.Join(home, FileName); s.Path() != want {
		t.Errorf("Path() = %q, want %q", s.Path(), want)
	}
	for path, want := range map[string]os.FileMode{home: 0o700, s.Path(): 0o600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s has mode %#o, want %#o", path, got, want)
		}
	}

	// A write creates the -wal file; SQLite gives it the database's mode.
	must(t, s.InsertEvents(context.Background(), Event{SessionID: "s", Event: "e"}))
	info, err := os.Stat(s.Path() + "-wal")
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("-wal has mode %#o, want 0600", got)
	}
}

func TestOpenErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := Open(ctx, ""); err == nil {
		t.Error("Open(\"\") succeeded")
	}
	if _, err := OpenHome(ctx, ""); err == nil {
		t.Error("OpenHome(\"\") succeeded")
	}
	if _, err := Open(ctx, filepath.Join(t.TempDir(), "missing", "aim.db")); err == nil {
		t.Error("Open in a missing directory succeeded")
	}
}

// A path with characters that mean something in a URI still opens the file
// it names.
func TestOpenOddPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a dir?with#odd&chars%20")
	must(t, os.Mkdir(dir, 0o700))
	path := filepath.Join(dir, "x.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	must(t, s.InsertEvents(context.Background(), Event{SessionID: "s", Event: "e"}))
	if _, err := os.Stat(path); err != nil {
		t.Errorf("database not at %s: %v", path, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "x.db") {
			t.Errorf("unexpected file %q in %s", e.Name(), dir)
		}
	}
}

func TestPragmas(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	check := func(pool, pragma string, want int) {
		t.Helper()
		db := s.w
		if pool == "reader" {
			db = s.r
		}
		var got int
		must(t, db.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got))
		if got != want {
			t.Errorf("%s: PRAGMA %s = %d, want %d", pool, pragma, got, want)
		}
	}
	var mode string
	must(t, s.w.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode))
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
	check("writer", "foreign_keys", 1) // switched back on after the migrations
	check("writer", "busy_timeout", int(BusyTimeout.Milliseconds()))
	check("writer", "synchronous", 1) // NORMAL
	check("writer", "query_only", 0)
	check("reader", "foreign_keys", 1)
	check("reader", "busy_timeout", int(BusyTimeout.Milliseconds()))
	check("reader", "query_only", 1)
}

func TestReadersCannotWrite(t *testing.T) {
	s := newStore(t)
	_, err := s.r.ExecContext(context.Background(), `INSERT INTO events (session_id, event, at) VALUES ('s', 'e', 1)`)
	if err == nil {
		t.Fatal("a write through the reader pool succeeded")
	}
}

func TestListJSON(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want string
	}{
		{nil, "[]"},
		{[]string{}, "[]"},
		{[]string{"a", `b"c`}, `["a","b\"c"]`},
	} {
		got, err := listJSON(tc.in)
		must(t, err)
		if got != tc.want {
			t.Errorf("listJSON(%q) = %s, want %s", tc.in, got, tc.want)
		}
		var back []string
		must(t, jsonList{&back}.Scan(got))
		if len(tc.in) == 0 && back != nil || len(tc.in) > 0 && strings.Join(back, "|") != strings.Join(tc.in, "|") {
			t.Errorf("round trip of %q = %q", tc.in, back)
		}
	}
	var v []string
	if err := (jsonList{&v}).Scan(int64(1)); err == nil {
		t.Error("jsonList scanned an integer")
	}
	if err := (jsonList{&v}).Scan("{"); err == nil {
		t.Error("jsonList scanned bad JSON")
	}
}
