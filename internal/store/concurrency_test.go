package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// Readers are neither blocked by an open write transaction nor see its
// uncommitted rows; once it commits, they see them.
func TestReadersDuringWrite(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(t, s.UpsertSessions(ctx, Session{PID: 1, SessionID: "s1"}))
	must(t, s.InsertEvents(ctx, Event{SessionID: "s1", Event: "committed"}))

	// Hold the writer in a transaction with an uncommitted event.
	tx, err := s.w.BeginTx(ctx, nil)
	must(t, err)
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT INTO events (session_id, event, at) VALUES ('s1', 'uncommitted', 1)`)
	must(t, err)
	_, err = tx.Exec(`UPDATE sessions SET name = 'uncommitted' WHERE key = 'pid:1'`)
	must(t, err)

	// Each read gets well under BusyTimeout: a reader that waited for the
	// writer's lock would blow it. The deadline is per read, not one for
	// all 400: together they can take longer than that under -race on a
	// loaded machine without any of them waiting for a lock.
	read := func() error {
		readCtx, cancel := context.WithTimeout(ctx, BusyTimeout/5)
		defer cancel()
		evs, err := s.ListSessionEvents(readCtx, "s1", 0)
		if err != nil {
			return err
		}
		if len(evs) != 1 || evs[0].Event != "committed" {
			return fmt.Errorf("reader saw %+v, want only the committed event", evs)
		}
		ss, err := s.ListSessions(readCtx, SessionFilter{})
		if err != nil {
			return err
		}
		if len(ss) != 1 || ss[0].Name != "" {
			return fmt.Errorf("reader saw sessions %+v", ss)
		}
		return nil
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			for range 25 {
				if err := read(); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	must(t, tx.Commit())
	evs, err := s.ListSessionEvents(ctx, "s1", 0)
	must(t, err)
	if len(evs) != 2 {
		t.Errorf("after commit, readers see %d events, want 2", len(evs))
	}
	got, err := s.GetSession(ctx, "pid:1")
	must(t, err)
	if got.Name != "uncommitted" {
		t.Errorf("after commit, session name = %q", got.Name)
	}
}

// Many goroutines writing and reading through one Store: writes queue on
// the single writer connection, reads run beside them, nothing fails with
// SQLITE_BUSY, and nothing is lost. Run with -race.
func TestConcurrentReadersAndWriters(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	const writers, readers, rounds = 4, 8, 50

	var wg sync.WaitGroup
	errs := make(chan error, writers+readers)
	for w := range writers {
		wg.Go(func() {
			for i := range rounds {
				err := s.InsertEvents(ctx,
					Event{SessionID: "s", Event: "tool.call", Summary: fmt.Sprintf("w%d #%d", w, i)},
					Event{SessionID: "s", Event: "tool.done"})
				if err == nil {
					err = s.UpsertSessions(ctx, Session{PID: 100 + w, State: fmt.Sprint(i)})
				}
				if err == nil {
					err = s.UpsertPRs(ctx, PR{Repo: "example-org/app", Number: 1 + i%5, State: PROpen})
				}
				if err != nil {
					errs <- err
					return
				}
			}
		})
	}
	for range readers {
		wg.Go(func() {
			for range rounds {
				if _, err := s.ListSessionEvents(ctx, "s", 20); err != nil {
					errs <- err
					return
				}
				if _, err := s.ListSessions(ctx, SessionFilter{}); err != nil {
					errs <- err
					return
				}
				if _, err := s.ListPRs(ctx, PRFilter{State: PROpen}); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	var n int
	must(t, s.r.QueryRow(`SELECT count(*) FROM events`).Scan(&n))
	if want := writers * rounds * 2; n != want {
		t.Errorf("%d events, want %d", n, want)
	}
	ss, err := s.ListSessions(ctx, SessionFilter{})
	must(t, err)
	if len(ss) != writers {
		t.Errorf("%d sessions, want %d", len(ss), writers)
	}
	for _, x := range ss {
		if x.State != fmt.Sprint(rounds-1) {
			t.Errorf("session %s state = %q, want the last write", x.Key, x.State)
		}
	}
}

// Two Stores on one file stand in for two processes (aimd writing while
// the aim CLI runs): writes from both wait for each other through
// busy_timeout instead of failing.
func TestTwoStoresOnOneFile(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), FileName)
	a, err := Open(ctx, path)
	must(t, err)
	defer a.Close()
	b, err := Open(ctx, path)
	must(t, err)
	defer b.Close()

	const rounds = 100
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for _, st := range []*Store{a, b} {
		wg.Go(func() {
			for i := range rounds {
				if err := st.InsertEvents(ctx, Event{SessionID: "s", Event: "e", At: time.UnixMilli(int64(i))}); err != nil {
					errs <- err
					return
				}
			}
		})
		wg.Go(func() {
			for range rounds {
				if _, err := st.ListSessionEvents(ctx, "s", 5); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var n int
	must(t, b.r.QueryRow(`SELECT count(*) FROM events`).Scan(&n))
	if n != 2*rounds {
		t.Errorf("%d events, want %d", n, 2*rounds)
	}
}
