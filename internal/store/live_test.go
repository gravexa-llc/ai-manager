package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestFindSession(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.FindSession(ctx, "s1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("FindSession of nothing = %v, want ErrNotFound", err)
	}
	must(t, s.UpsertSessions(ctx, Session{Key: "pid:1", SessionID: "s1", LastSeenAt: at(0)}))
	must(t, s.EndSession(ctx, "pid:1", at(time.Minute)))
	got, err := s.FindSession(ctx, "s1")
	if err != nil || got.Key != "pid:1" || got.EndedAt.IsZero() {
		t.Errorf("only an ended row: %+v, %v", got, err)
	}
	// A live row wins over an ended one, and the poller's over the mod's.
	must(t, s.UpsertSessions(ctx, Session{Key: "session:s1", SessionID: "s1", LastSeenAt: at(2 * time.Minute)}))
	if got, _ := s.FindSession(ctx, "s1"); got.Key != "session:s1" {
		t.Errorf("live mod row: got %q", got.Key)
	}
	must(t, s.UpsertSessions(ctx, Session{Key: "pid:2", SessionID: "s1", LastSeenAt: at(3 * time.Minute)}))
	if got, _ := s.FindSession(ctx, "s1"); got.Key != "pid:2" {
		t.Errorf("live poller row: got %q", got.Key)
	}
}

func TestUpdateSessionLive(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(t, s.UpsertSessions(ctx, Session{
		Key: "pid:1", SessionID: "s1", Kind: "interactive", Headless: true, Host: "polled", Cwd: "/r/a",
		WorktreePath: "/r/a", Repo: "/r/a", Branch: "main", State: "idle", LastSeenAt: at(0),
	}))
	f := false
	must(t, s.UpdateSessionLive(ctx, "pid:1", SessionLive{
		State: "needs_you", StateReason: "permission prompt: Bash: x", NeedsYouSince: at(time.Second),
		LastAction: "Bash: x", LastSaid: "done", LastEventAt: at(time.Second),
		Cwd: "/r/a/.claude/worktrees/w", WorktreePath: "/r/a/.claude/worktrees/w", ItermSessionID: "w0", Kind: "background", Headless: &f,
	}))
	got, err := s.GetSession(ctx, "pid:1")
	must(t, err)
	if got.State != "needs_you" || got.StateReason != "permission prompt: Bash: x" || !got.NeedsYouSince.Equal(at(time.Second)) ||
		got.LastAction != "Bash: x" || got.LastSaid != "done" || !got.LastEventAt.Equal(at(time.Second)) || got.ItermSessionID != "w0" {
		t.Errorf("live fields = %+v", got)
	}
	// Empty values keep what's stored; the branch goes with the worktree;
	// the poller's kind wins; headless is written when given.
	if got.Cwd != "/r/a/.claude/worktrees/w" || got.WorktreePath != "/r/a/.claude/worktrees/w" || got.Branch != "" ||
		got.Repo != "/r/a" || got.Host != "polled" || got.Kind != "interactive" || got.Headless {
		t.Errorf("where fields = %+v", got)
	}
	must(t, s.UpdateSessionLive(ctx, "pid:1", SessionLive{State: "working", Branch: "ignored without a worktree"}))
	got, _ = s.GetSession(ctx, "pid:1")
	if got.State != "working" || !got.NeedsYouSince.IsZero() || got.Branch != "" || got.Headless || got.WorktreePath == "" {
		t.Errorf("after a sparse update = %+v", got)
	}
	if err := s.UpdateSessionLive(ctx, "pid:9", SessionLive{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("no live row: %v, want ErrNotFound", err)
	}
}

func TestAdoptSession(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	// No poller row yet: the mod's row takes the key.
	must(t, s.UpsertSessions(ctx, Session{Key: "session:s1", SessionID: "s1", State: "working", LastSeenAt: at(0)}))
	must(t, s.AdoptSession(ctx, "session:s1", "pid:1"))
	if got, err := s.GetSession(ctx, "pid:1"); err != nil || got.SessionID != "s1" || got.State != "working" {
		t.Errorf("rekeyed row = %+v, %v", got, err)
	}

	// Both exist: the poller's row takes the mod's fields; the mod's goes.
	must(t, s.UpsertSessions(ctx, Session{Key: "pid:2", SessionID: "s2", PID: 2, State: "idle", LastSeenAt: at(0)}))
	must(t, s.UpsertSessions(ctx, Session{Key: "session:s2", SessionID: "s2", LastSeenAt: at(0)}))
	must(t, s.UpdateSessionLive(ctx, "session:s2", SessionLive{State: "needs_you", StateReason: "question", NeedsYouSince: at(time.Second),
		LastAction: "AskUserQuestion", LastSaid: "hm", LastEventAt: at(time.Second), ItermSessionID: "w1"}))
	must(t, s.AdoptSession(ctx, "session:s2", "pid:2"))
	got, err := s.GetSession(ctx, "pid:2")
	must(t, err)
	if got.PID != 2 || got.State != "needs_you" || got.StateReason != "question" || !got.NeedsYouSince.Equal(at(time.Second)) ||
		got.LastAction != "AskUserQuestion" || got.LastSaid != "hm" || got.ItermSessionID != "w1" || !got.LastEventAt.Equal(at(time.Second)) {
		t.Errorf("adopting row = %+v", got)
	}
	all, _ := s.ListSessions(ctx, SessionFilter{IncludeEnded: true})
	if len(all) != 2 {
		t.Errorf("%d rows, want 2 (the mod's row is gone)", len(all))
	}
	// Nothing to adopt is fine.
	must(t, s.AdoptSession(ctx, "session:none", "pid:3"))
	must(t, s.AdoptSession(ctx, "pid:2", "pid:2"))
}

func TestMoveSessionID(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(t, s.UpsertSessions(ctx, Session{Key: "pid:1", SessionID: "old", LastSeenAt: at(0)}))
	must(t, s.UpsertAgents(ctx, Agent{SessionID: "old", Kind: AgentLead}, Agent{SessionID: "old", AgentID: "a1", Kind: AgentSubagent}))
	plan, err := s.InsertPlan(ctx, Plan{Slug: "p", RegisteredInSession: "old"})
	must(t, err)
	must(t, s.InsertPackage(ctx, Package{PlanID: plan, PkgID: "A1"}))
	must(t, s.InsertPackage(ctx, Package{PlanID: plan, PkgID: "A2"}))
	_, err = s.UpsertLink(ctx, Link{PlanID: plan, PkgID: "A1", Kind: LinkAgent, SessionID: "old", AgentID: "a1", MatchMethod: MatchSpawn, Confidence: ConfidenceExact})
	must(t, err)
	must(t, s.Unlink(ctx, Tombstone{PlanID: plan, PkgID: "A2", Kind: LinkAgent, SessionID: "old"}))

	moved, err := s.MoveSessionID(ctx, "old", "new")
	must(t, err)
	if !moved {
		t.Error("moved = false")
	}
	got, err := s.GetSession(ctx, "pid:1")
	if err != nil || got.SessionID != "new" || got.PrevSessionID != "old" {
		t.Errorf("row = %+v, %v", got, err)
	}
	if ag, _ := s.ListAgents(ctx, "new"); len(ag) != 2 {
		t.Errorf("agents under new: %d, want 2", len(ag))
	}
	links, _ := s.ListLinks(ctx, plan)
	if len(links) != 1 || links[0].SessionID != "new" || links[0].Ref != "new/a1" {
		t.Errorf("links = %+v", links)
	}
	ts, _ := s.ListTombstones(ctx, plan)
	if len(ts) != 1 || ts[0].SessionID != "new" || ts[0].Ref != "new/" {
		t.Errorf("tombstones = %+v", ts)
	}
	if p, _ := s.GetPlan(ctx, plan); p.RegisteredInSession != "new" {
		t.Errorf("plan registered in %q", p.RegisteredInSession)
	}

	// The poller already moved its row to the new id; a row the receiver
	// made for the old id is dropped as a duplicate.
	must(t, s.UpsertSessions(ctx, Session{Key: "pid:2", SessionID: "b-new", LastSeenAt: at(0)}))
	must(t, s.UpsertSessions(ctx, Session{Key: "session:b-old", SessionID: "b-old", LastSeenAt: at(0)}))
	moved, err = s.MoveSessionID(ctx, "b-old", "b-new")
	must(t, err)
	if moved {
		t.Error("moved over a live row with the new id")
	}
	if got, _ := s.GetSession(ctx, "pid:2"); got.PrevSessionID != "b-old" {
		t.Errorf("poller's row prev = %q", got.PrevSessionID)
	}
	if _, err := s.FindSession(ctx, "b-old"); !errors.Is(err, ErrNotFound) {
		t.Errorf("old duplicate row: %v", err)
	}

	if moved, err := s.MoveSessionID(ctx, "x", "x"); moved || err != nil {
		t.Errorf("same id: %v, %v", moved, err)
	}
}
