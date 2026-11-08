package store

import (
	"context"
	"testing"
	"time"
)

// monitoredIDs lists the explicit monitoring as "reason session plan".
func monitoredIDs(t *testing.T, s *Store) []string {
	t.Helper()
	ms, err := s.ListMonitored(context.Background())
	must(t, err)
	var out []string
	for _, m := range ms {
		out = append(out, m.Reason+" "+m.SessionID+" "+m.PlanSlug)
	}
	return out
}

func TestMigration0006(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if v, err := s.SchemaVersion(ctx); err != nil || v < 6 {
		t.Fatalf("SchemaVersion() = %d, %v; want at least 6", v, err)
	}
	var name string
	must(t, s.r.QueryRow(`SELECT name FROM schema_migrations WHERE version = 6`).Scan(&name))
	if name != "0006_monitored.sql" {
		t.Errorf("version 6 recorded as %q", name)
	}
	// The table's rules: a session id is required; one watch per session,
	// one lead row per session and plan; a plan must exist.
	if _, err := s.w.Exec(`INSERT INTO monitored_sessions (session_id, reason, since) VALUES ('', 'watch', 0)`); err == nil {
		t.Error("an empty session id was accepted")
	}
	_, err := s.w.Exec(`INSERT INTO monitored_sessions (session_id, reason, since) VALUES ('s1', 'watch', 0)`)
	must(t, err)
	if _, err := s.w.Exec(`INSERT INTO monitored_sessions (session_id, reason, since) VALUES ('s1', 'watch', 1)`); err == nil {
		t.Error("a second watch of one session was accepted")
	}
	if _, err := s.w.Exec(`INSERT INTO monitored_sessions (session_id, reason, plan_id, since) VALUES ('s1', 'plan', 42, 0)`); err == nil {
		t.Error("a lead row of a plan that doesn't exist was accepted")
	}
	// No lead is backfilled from plans registered before migration 0006.
	_, err = s.InsertPlan(ctx, Plan{Slug: "old", RegisteredInSession: "s-old"})
	must(t, err)
	equal(t, "monitored", monitoredIDs(t, s), []string{"watch s1 "})
}

func TestWatchSession(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	m, created, err := s.WatchSession(ctx, "s1", at(time.Minute))
	must(t, err)
	if !created || m.SessionID != "s1" || m.Reason != MonitorWatch || !m.Since.Equal(at(time.Minute)) || m.PlanID != 0 {
		t.Errorf("first watch = %+v, created %v", m, created)
	}
	// Watching again keeps the first watch.
	m, created, err = s.WatchSession(ctx, "s1", at(time.Hour))
	must(t, err)
	if created || !m.Since.Equal(at(time.Minute)) {
		t.Errorf("second watch = %+v, created %v", m, created)
	}
	if _, _, err := s.WatchSession(ctx, " ", time.Time{}); err == nil {
		t.Error("an empty session id was accepted")
	}
	_, _, err = s.WatchSession(ctx, "s2", time.Time{})
	must(t, err)
	equal(t, "watched", monitoredIDs(t, s), []string{"watch s2 ", "watch s1 "}) // s2 at t0, the store's now

	n, err := s.UnwatchSession(ctx, "s1", "", "nobody")
	must(t, err)
	if n != 1 {
		t.Errorf("UnwatchSession removed %d, want 1", n)
	}
	n, err = s.UnwatchSession(ctx, "s1")
	must(t, err)
	if n != 0 {
		t.Errorf("a second unwatch removed %d, want 0", n)
	}
	if n, err := s.UnwatchSession(ctx); err != nil || n != 0 {
		t.Errorf("UnwatchSession() = %d, %v", n, err)
	}
	equal(t, "after unwatch", monitoredIDs(t, s), []string{"watch s2 "})
}

func TestMonitorPlanLead(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	aim, err := s.InsertPlan(ctx, Plan{Slug: "aim"})
	must(t, err)
	sharing, err := s.InsertPlan(ctx, Plan{Slug: "sharing"})
	must(t, err)

	created, err := s.MonitorPlanLead(ctx, "lead", aim, at(time.Second))
	must(t, err)
	if !created {
		t.Error("the first lead row wasn't created")
	}
	if created, err := s.MonitorPlanLead(ctx, "lead", aim, at(time.Hour)); err != nil || created {
		t.Errorf("again: created %v, %v", created, err)
	}
	_, err = s.MonitorPlanLead(ctx, "lead", sharing, at(2*time.Second))
	must(t, err)
	_, _, err = s.WatchSession(ctx, "lead", at(3*time.Second))
	must(t, err)
	if _, err := s.MonitorPlanLead(ctx, "", aim, time.Time{}); err == nil {
		t.Error("an empty session id was accepted")
	}
	ms, err := s.ListMonitored(ctx)
	must(t, err)
	if len(ms) != 3 || ms[0].PlanID != aim || !ms[0].Since.Equal(at(time.Second)) {
		t.Fatalf("ListMonitored = %+v", ms)
	}
	equal(t, "monitored", monitoredIDs(t, s), []string{"plan lead aim", "plan lead sharing", "watch lead "})

	// Unwatching leaves the lead rows; unregistering a plan takes its own.
	_, err = s.UnwatchSession(ctx, "lead")
	must(t, err)
	must(t, s.DeletePlan(ctx, aim))
	equal(t, "after unwatch and unregister", monitoredIDs(t, s), []string{"plan lead sharing"})
}

// A /clear the mod reports (MoveSessionID) carries the watch and the lead
// rows over to the new id, as it does the plans the session registered.
func TestMoveSessionIDCarriesMonitoring(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	plan, err := s.InsertPlan(ctx, Plan{Slug: "aim", RegisteredInSession: "old"})
	must(t, err)
	must(t, s.UpsertSessions(ctx, Session{Key: "pid:1", SessionID: "old", PID: 1, LastSeenAt: at(0)}))
	_, _, err = s.WatchSession(ctx, "old", at(0))
	must(t, err)
	_, err = s.MonitorPlanLead(ctx, "old", plan, at(0))
	must(t, err)
	_, _, err = s.WatchSession(ctx, "other", at(0))
	must(t, err)

	moved, err := s.MoveSessionID(ctx, "old", "new")
	must(t, err)
	if !moved {
		t.Fatal("the live row didn't move")
	}
	equal(t, "after /clear", monitoredIDs(t, s), []string{"watch new ", "plan new aim", "watch other "})

	// Twice: the watch follows every /clear.
	_, err = s.MoveSessionID(ctx, "new", "newer")
	must(t, err)
	equal(t, "after a second /clear", monitoredIDs(t, s), []string{"watch newer ", "plan newer aim", "watch other "})

	// A row the new id already has is kept; the old one stays behind.
	_, _, err = s.WatchSession(ctx, "third", at(time.Hour))
	must(t, err)
	_, err = s.MoveSessionID(ctx, "newer", "third")
	must(t, err)
	got := monitoredIDs(t, s)
	equal(t, "into an id with its own watch", got, []string{"watch newer ", "plan third aim", "watch other ", "watch third "})
}

// A /clear (or a background job's respawn) seen only by the poller, as a
// new session id under the same key, carries the monitoring over too.
func TestUpsertSessionsCarriesMonitoring(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(t, s.UpsertSessions(ctx,
		Session{Key: "pid:1", SessionID: "a", PID: 1, LastSeenAt: at(0)},
		Session{Key: "job:j1", JobID: "j1", SessionID: "j-a", Kind: "background", LastSeenAt: at(0)},
	))
	_, _, err := s.WatchSession(ctx, "a", at(0))
	must(t, err)
	_, _, err = s.WatchSession(ctx, "j-a", at(0))
	must(t, err)

	// A poll that lists the same ids changes nothing.
	must(t, s.UpsertSessions(ctx, Session{Key: "pid:1", SessionID: "a", PID: 1, LastSeenAt: at(time.Minute)}))
	equal(t, "same ids", monitoredIDs(t, s), []string{"watch a ", "watch j-a "})

	// New ids under the same keys: /clear in pid 1, a respawn of job j1.
	must(t, s.UpsertSessions(ctx,
		Session{Key: "pid:1", SessionID: "b", PID: 1, LastSeenAt: at(2 * time.Minute)},
		Session{Key: "job:j1", JobID: "j1", SessionID: "j-b", Kind: "background", LastSeenAt: at(2 * time.Minute)},
	))
	equal(t, "new ids", monitoredIDs(t, s), []string{"watch b ", "watch j-b "})
	got, err := s.GetSession(ctx, "pid:1")
	must(t, err)
	if got.SessionID != "b" || got.PrevSessionID != "a" {
		t.Errorf("row = %s (prev %s)", got.SessionID, got.PrevSessionID)
	}

	// A poll with no session id keeps both the row's id and the watch.
	must(t, s.UpsertSessions(ctx, Session{Key: "pid:1", PID: 1, LastSeenAt: at(3 * time.Minute)}))
	equal(t, "no id", monitoredIDs(t, s), []string{"watch b ", "watch j-b "})

	// A new process that reuses an ended session's pid starts afresh: the
	// ended row's watch doesn't move to it.
	must(t, s.EndSession(ctx, "pid:1", at(4*time.Minute)))
	must(t, s.UpsertSessions(ctx, Session{Key: "pid:1", SessionID: "unrelated", PID: 1, LastSeenAt: at(5 * time.Minute)}))
	equal(t, "pid reused", monitoredIDs(t, s), []string{"watch b ", "watch j-b "})
}
