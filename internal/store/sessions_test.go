package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/sessions"
)

func TestSessionKeyMatchesSessionsPackage(t *testing.T) {
	for _, s := range []sessions.Session{
		{JobID: "job-1", PID: 42, SessionID: "s1", Kind: sessions.Background},
		{JobID: "job-2", Kind: sessions.Background},
		{PID: 42, SessionID: "s1", Kind: sessions.Interactive},
		{SessionID: "s1"},
	} {
		if got, want := SessionKey(s.JobID, s.PID, s.SessionID), string(s.Key()); got != want {
			t.Errorf("SessionKey(%q, %d, %q) = %q, sessions.Key() = %q", s.JobID, s.PID, s.SessionID, got, want)
		}
	}
	if got := SessionKey("", 0, ""); got != "" {
		t.Errorf("SessionKey of nothing = %q, want \"\"", got)
	}
}

func TestSessionsRoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	in := Session{
		Key:           "job:j1",
		SessionID:     "s1",
		JobID:         "j1",
		Kind:          "background",
		Headless:      true,
		Name:          "fix the thing",
		Host:          "laptop",
		Cwd:           "/repos/app/.claude/worktrees/agent-1",
		WorktreePath:  "/repos/app/.claude/worktrees/agent-1",
		Repo:          "/repos/app",
		Branch:        "aim/b1-store",
		PID:           4242,
		State:         "needs_you",
		StateReason:   "permission prompt",
		NeedsYouSince: at(time.Minute),
		StartedAt:     at(-time.Hour),
		LastSeenAt:    at(time.Minute),
	}
	must(t, s.UpsertSessions(ctx, in))

	got, err := s.GetSession(ctx, "job:j1")
	must(t, err)
	want := in
	want.ID = got.ID
	want.FirstSeenAt = in.LastSeenAt
	equal(t, "session", got, want)
	if got.ID == 0 {
		t.Error("ID not set")
	}

	list, err := s.ListSessions(ctx, SessionFilter{})
	must(t, err)
	equal(t, "ListSessions", list, []Session{want})
}

func TestUpsertSessionsUpdates(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	must(t, s.UpsertSessions(ctx, Session{Key: "job:j1", JobID: "j1", SessionID: "s1", State: "working", LastSeenAt: at(0)}))
	must(t, s.UpdateSessionActivity(ctx, "job:j1", SessionActivity{LastAction: "Bash: make check", LastSaid: "running checks", ItermSessionID: "w0t1p0", At: at(time.Second)}))
	must(t, s.DismissNeedsYou(ctx, "job:j1", at(2*time.Second)))

	// Needs you: the first time sticks while it lasts.
	must(t, s.UpsertSessions(ctx, Session{Key: "job:j1", JobID: "j1", SessionID: "s1", State: "needs_you", NeedsYouSince: at(time.Minute), LastSeenAt: at(time.Minute)}))
	must(t, s.UpsertSessions(ctx, Session{Key: "job:j1", JobID: "j1", SessionID: "s1", State: "needs_you", NeedsYouSince: at(2 * time.Minute), LastSeenAt: at(2 * time.Minute)}))
	got, err := s.GetSession(ctx, "job:j1")
	must(t, err)
	equal(t, "NeedsYouSince while still waiting", got.NeedsYouSince, at(time.Minute))

	// The job respawns under a new session id, no longer waiting. The
	// poll's empty fields must not erase what the mod and the dashboard
	// wrote, and an empty job id must not erase the stored one.
	must(t, s.UpsertSessions(ctx, Session{Key: "job:j1", SessionID: "s2", State: "working", Name: "renamed", LastSeenAt: at(3 * time.Minute)}))
	got, err = s.GetSession(ctx, "job:j1")
	must(t, err)
	equal(t, "SessionID", got.SessionID, "s2")
	equal(t, "PrevSessionID", got.PrevSessionID, "s1")
	equal(t, "JobID", got.JobID, "j1")
	equal(t, "Name", got.Name, "renamed")
	equal(t, "NeedsYouSince after waiting", got.NeedsYouSince, time.Time{})
	equal(t, "FirstSeenAt", got.FirstSeenAt, at(0))
	equal(t, "LastSeenAt", got.LastSeenAt, at(3*time.Minute))
	equal(t, "LastAction", got.LastAction, "Bash: make check")
	equal(t, "LastSaid", got.LastSaid, "running checks")
	equal(t, "ItermSessionID", got.ItermSessionID, "w0t1p0")
	equal(t, "LastEventAt", got.LastEventAt, at(time.Second))
	equal(t, "NeedsYouDismissedAt", got.NeedsYouDismissedAt, at(2*time.Second))

	// An empty session id doesn't erase the stored one or shift prev.
	must(t, s.UpsertSessions(ctx, Session{Key: "job:j1", State: "working"}))
	got, err = s.GetSession(ctx, "job:j1")
	must(t, err)
	equal(t, "SessionID after empty", got.SessionID, "s2")
	equal(t, "PrevSessionID after empty", got.PrevSessionID, "s1")
	equal(t, "LastSeenAt defaults to now", got.LastSeenAt, t0)

	// An activity update with no iTerm id keeps the stored one.
	must(t, s.UpdateSessionActivity(ctx, "job:j1", SessionActivity{LastAction: "Edit store.go"}))
	got, err = s.GetSession(ctx, "job:j1")
	must(t, err)
	equal(t, "ItermSessionID kept", got.ItermSessionID, "w0t1p0")
	equal(t, "LastAction", got.LastAction, "Edit store.go")
	equal(t, "LastSaid cleared", got.LastSaid, "")
	equal(t, "LastEventAt defaults to now", got.LastEventAt, t0)
}

func TestUpsertSessionsDerivesKey(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(t, s.UpsertSessions(ctx,
		Session{JobID: "j1", PID: 10},
		Session{PID: 11, SessionID: "s11"},
		Session{SessionID: "s12"},
	))
	for _, key := range []string{"job:j1", "pid:11", "session:s12"} {
		if _, err := s.GetSession(ctx, key); err != nil {
			t.Errorf("GetSession(%q): %v", key, err)
		}
	}
}

func TestUpsertSessionsIsAtomic(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	err := s.UpsertSessions(ctx, Session{PID: 1}, Session{Name: "no key at all"})
	if err == nil {
		t.Fatal("UpsertSessions with a keyless session succeeded")
	}
	list, err := s.ListSessions(ctx, SessionFilter{IncludeEnded: true})
	must(t, err)
	if len(list) != 0 {
		t.Errorf("a failed batch left %d rows", len(list))
	}
}

// A pid reused after its session ended starts a new row and inherits
// nothing.
func TestEndSessionAndPIDReuse(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	must(t, s.UpsertSessions(ctx, Session{PID: 42, SessionID: "old", Kind: "interactive", StartedAt: at(-time.Hour), LastSeenAt: at(0)}))
	must(t, s.UpdateSessionActivity(ctx, "pid:42", SessionActivity{LastAction: "old action"}))
	old, err := s.GetSession(ctx, "pid:42")
	must(t, err)

	must(t, s.EndSession(ctx, "pid:42", at(time.Minute)))
	if _, err := s.GetSession(ctx, "pid:42"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetSession after EndSession: %v, want ErrNotFound", err)
	}
	if err := s.EndSession(ctx, "pid:42", at(time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Errorf("second EndSession: %v, want ErrNotFound", err)
	}

	must(t, s.UpsertSessions(ctx, Session{PID: 42, SessionID: "new", Kind: "interactive", StartedAt: at(time.Hour), LastSeenAt: at(time.Hour)}))
	cur, err := s.GetSession(ctx, "pid:42")
	must(t, err)
	if cur.ID == old.ID {
		t.Error("the reused pid updated the ended row")
	}
	equal(t, "new row's PrevSessionID", cur.PrevSessionID, "")
	equal(t, "new row's LastAction", cur.LastAction, "")
	equal(t, "new row's FirstSeenAt", cur.FirstSeenAt, at(time.Hour))

	live, err := s.ListSessions(ctx, SessionFilter{})
	must(t, err)
	if len(live) != 1 || live[0].ID != cur.ID {
		t.Errorf("live sessions = %+v, want only the new row", live)
	}
	all, err := s.ListSessions(ctx, SessionFilter{IncludeEnded: true})
	must(t, err)
	if len(all) != 2 || all[0].ID != cur.ID || all[1].ID != old.ID {
		t.Fatalf("all sessions = %+v, want the live row, then the ended one", all)
	}
	equal(t, "ended row's EndedAt", all[1].EndedAt, at(time.Minute))
	equal(t, "ended row's LastAction", all[1].LastAction, "old action")
}

func TestListSessionsOrder(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(t, s.UpsertSessions(ctx,
		Session{PID: 3, StartedAt: at(3 * time.Second)},
		Session{PID: 1, StartedAt: at(1 * time.Second)},
		Session{PID: 2, LastSeenAt: at(2 * time.Second)}, // no start time: first seen
	))
	list, err := s.ListSessions(ctx, SessionFilter{})
	must(t, err)
	var keys []string
	for _, ss := range list {
		keys = append(keys, ss.Key)
	}
	equal(t, "order", keys, []string{"pid:1", "pid:2", "pid:3"})
}

func TestSessionWritesNeedALiveRow(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for name, err := range map[string]error{
		"EndSession":            s.EndSession(ctx, "pid:9", time.Time{}),
		"DismissNeedsYou":       s.DismissNeedsYou(ctx, "pid:9", time.Time{}),
		"UpdateSessionActivity": s.UpdateSessionActivity(ctx, "pid:9", SessionActivity{}),
	} {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s on a missing session: %v, want ErrNotFound", name, err)
		}
	}
	if _, err := s.GetSession(ctx, "pid:9"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetSession on a missing session: %v, want ErrNotFound", err)
	}
}
