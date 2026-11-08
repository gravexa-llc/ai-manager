package store

import (
	"context"
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
	must(t, s.UpsertAgents(ctx, sub))
	got, err = s.ListAgents(ctx, "s1")
	must(t, err)
	equal(t, "agents after update", got, []Agent{lead, sub})
}

func TestUpsertAgentsNeedsSession(t *testing.T) {
	s := newStore(t)
	if err := s.UpsertAgents(context.Background(), Agent{AgentID: "a1"}); err == nil {
		t.Error("UpsertAgents with no session id succeeded")
	}
}
