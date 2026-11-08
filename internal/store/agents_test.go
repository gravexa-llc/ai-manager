package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestAgentsRoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	lead := Agent{SessionID: "s1", Kind: AgentLead, Model: "opus", State: "working", StartedAt: at(0)}
	sub := Agent{
		SessionID:     "s1",
		AgentID:       "a26d366017ab8ca43",
		Kind:          AgentSubagent,
		AgentType:     "general-purpose",
		Background:    true,
		Model:         "opus",
		ParentAgentID: "",
		WorktreePath:  "/repos/app/.claude/worktrees/agent-a26d366017ab8ca43",
		State:         "working",
		LastAction:    "Bash: go test",
		Tokens:        41_250,
		StartedAt:     at(time.Minute),
	}
	other := Agent{SessionID: "s2", Kind: AgentLead}
	must(t, s.UpsertAgents(ctx, sub, other, lead))

	got, err := s.ListAgents(ctx, "s1")
	must(t, err)
	equal(t, "agents of s1, lead first", got, []Agent{lead, sub})

	all, err := s.ListAgents(ctx, "")
	must(t, err)
	equal(t, "all agents", all, []Agent{lead, sub, other})

	// An update rewrites every field.
	sub.State = "done"
	sub.Background = false
	sub.EndedAt = at(time.Hour)
	sub.LastAction = ""
	sub.Tokens = 52_900
	must(t, s.UpsertAgents(ctx, sub))
	got, err = s.ListAgents(ctx, "s1")
	must(t, err)
	equal(t, "agents after update", got, []Agent{lead, sub})
}

// Migration 0013 ends the subagent rows an older aimd wrote for loops
// nothing named (Claude Code's forks of an agent): no type, no worktree of
// their own, still running. Every other row is left as it was.
func TestMigrate0013EndsUnnamedAgents(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), FileName)
	db := rawDBAt(t, path)
	ms, err := loadMigrations(migrationFS)
	must(t, err)
	if len(ms) < 13 || ms[12].name != "0013_unnamed_agents.sql" {
		t.Fatalf("migration 13 = %+v", ms[12])
	}
	must(t, migrate(ctx, db, ms[:12]))
	const wt = "/work/app/.claude/worktrees/agent-a1111111111111111"
	start, first := t0.UnixMilli(), at(time.Minute).UnixMilli()
	for _, row := range []struct {
		id, kind, typ, wt, state string
		started                  any
	}{
		{"", AgentLead, "", "/work/app", "running", start},
		// The agent, finished, and one still running.
		{"a1111111111111111", AgentSubagent, "general-purpose", wt, "completed", start},
		{"a2222222222222222", AgentSubagent, "general-purpose", "/work/app/.claude/worktrees/agent-a2222222222222222", "running", start},
		// A worktree agent no agent list typed yet: its own worktree says it is one.
		{"a3333333333333333", AgentSubagent, "", "/work/app/.claude/worktrees/agent-A3333333333333333", "running", start},
		// Forks of a1111…: in its worktree, or the main checkout, or nowhere.
		{"b1111111111111111", AgentSubagent, "", wt, "running", first},
		{"b2222222222222222", AgentSubagent, "", "/work/app", "waiting", first},
		{"b3333333333333333", AgentSubagent, "", "", "running", nil},
	} {
		_, err := db.Exec(`INSERT INTO agents (session_id, agent_id, kind, agent_type, worktree_path, state, started_at) VALUES ('lead-session', ?, ?, ?, ?, ?, ?)`,
			row.id, row.kind, row.typ, row.wt, row.state, row.started)
		must(t, err)
	}
	must(t, db.Close())

	s, err := Open(ctx, path)
	must(t, err)
	defer s.Close()
	got, err := s.ListAgents(ctx, "lead-session")
	must(t, err)
	byID := map[string]Agent{}
	for _, a := range got {
		byID[a.AgentID] = a
	}
	for id, want := range map[string]string{
		"": "running", "a1111111111111111": "completed", "a2222222222222222": "running", "a3333333333333333": "running",
		"b1111111111111111": "completed", "b2222222222222222": "completed", "b3333333333333333": "completed",
	} {
		if byID[id].State != want {
			t.Errorf("%q state = %q, want %q", id, byID[id].State, want)
		}
	}
	for _, id := range []string{"", "a1111111111111111", "a2222222222222222", "a3333333333333333"} {
		if !byID[id].EndedAt.IsZero() {
			t.Errorf("%q ended at %v; it was left alone", id, byID[id].EndedAt)
		}
	}
	if e := byID["b1111111111111111"].EndedAt; !e.Equal(at(time.Minute)) {
		t.Errorf("fork ended at %v, want its first event %v", e, at(time.Minute))
	}
	if e := byID["b3333333333333333"].EndedAt; e.IsZero() || e.Before(t0) {
		t.Errorf("fork with no start ended at %v, want now", e)
	}
}

func TestUpsertAgentsNeedsSession(t *testing.T) {
	s := newStore(t)
	if err := s.UpsertAgents(context.Background(), Agent{AgentID: "a1"}); err == nil {
		t.Error("UpsertAgents with no session id succeeded")
	}
}
