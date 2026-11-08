package live

import (
	"context"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/events"
	"github.com/gravexa-llc/ai-manager/internal/store"
)

// Lineage comes from agent_list's parent_id.
func TestLineage(t *testing.T) {
	h := newHarness(t, nil)
	l := ev(events.AgentList, "s1", t0.Add(time.Second))
	l.Agents = []events.Agent{
		{ID: "a1", Status: events.AgentRunning, Type: "general-purpose", Background: ptr(true)},
		{ID: "a2", Status: events.AgentRunning, Type: "Explore", ParentID: "a1", Background: ptr(false)},
	}
	h.post(start("s1", t0), l)
	ag := h.agents("s1")
	if ag["a1"].ParentAgentID != "" || ag["a2"].ParentAgentID != "a1" || ag["a2"].AgentType != "Explore" || !ag["a1"].Background || ag["a2"].Background {
		t.Errorf("agents = %+v", ag)
	}
	// A later snapshot without background or type keeps what was known.
	l2 := ev(events.AgentList, "s1", t0.Add(5*time.Second))
	l2.Agents = []events.Agent{{ID: "a1", Status: events.AgentRunning}}
	h.post(l2)
	if a := h.agents("s1")["a1"]; !a.Background || a.AgentType != "general-purpose" {
		t.Errorf("a1 after a sparse snapshot = %+v", a)
	}
}

// The agent ↔ worktree join, in this order: worktree_create's
// name first, then the cwd in the agent's own events, resolved against the
// worktrees the scanner found.
func TestWorktreeJoin(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	if err := h.st.UpsertWorktrees(ctx,
		store.Worktree{Path: "/r/app", MainRepo: "/r/app", Branch: "main", LastSeen: t0},
		store.Worktree{Path: "/r/app-feat", MainRepo: "/r/app", Branch: "feat", LastSeen: t0},
	); err != nil {
		t.Fatal(err)
	}
	wc := ev(events.WorktreeCreate, "s1", t0.Add(time.Second))
	wc.Name = "agent-iso1"
	inIso := ev(events.TurnStart, "s1", t0.Add(2*time.Second))
	inIso.AgentID, inIso.Cwd = "iso1", "/r/app-feat" // the name wins over the cwd
	sib := ev(events.TurnStart, "s1", t0.Add(3*time.Second))
	sib.AgentID, sib.Cwd = "sib1", "/r/app-feat/internal"
	h.post(start("s1", t0), wc, inIso, sib)
	ag := h.agents("s1")
	if ag["iso1"].WorktreePath != "/r/app/.claude/worktrees/agent-iso1" {
		t.Errorf("isolated agent's worktree = %q", ag["iso1"].WorktreePath)
	}
	if ag["sib1"].WorktreePath != "/r/app-feat" {
		t.Errorf("agent in a sibling worktree = %q", ag["sib1"].WorktreePath)
	}
	if s := h.session("s1"); s.WorktreePath != "/r/app" || s.Branch != "main" {
		t.Errorf("the lead stays put: %q %q", s.WorktreePath, s.Branch)
	}
	// The lead moves into the sibling worktree.
	cd := tool("s1", "", "Bash: cd", "1", t0.Add(4*time.Second))
	cd.Cwd = "/r/app-feat"
	h.post(cd)
	if s := h.session("s1"); s.WorktreePath != "/r/app-feat" || s.Branch != "feat" || s.Cwd != "/r/app-feat" {
		t.Errorf("lead in the sibling: %q %q %q", s.Cwd, s.WorktreePath, s.Branch)
	}
}

// After aimd restarts, the first event for a session the store knows picks
// up its row and its agents rather than starting over.
func TestSeedFromStore(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	if err := h.st.UpsertSessions(ctx, store.Session{Key: "pid:7", SessionID: "s1", PID: 7, Kind: "interactive", Cwd: "/r/app", Repo: "/r/app", State: "idle"}); err != nil {
		t.Fatal(err)
	}
	if err := h.st.UpsertAgents(ctx,
		store.Agent{SessionID: "s1", Kind: store.AgentLead, State: events.AgentIdle},
		store.Agent{SessionID: "s1", AgentID: "a1", Kind: store.AgentSubagent, AgentType: "general-purpose", Background: true,
			Model: "m", WorktreePath: "/r/app/.claude/worktrees/agent-a1", State: events.AgentWaiting, StartedAt: t0},
	); err != nil {
		t.Fatal(err)
	}
	e := tool("s1", "a1", "Bash: x", "1", t0.Add(time.Minute))
	e.Cwd = "/r/app/elsewhere"
	h.post(e)
	s := h.session("s1")
	if s.Key != "pid:7" || s.LastAction != "Bash: x" {
		t.Errorf("row = %+v", s)
	}
	a := h.agents("s1")["a1"]
	if a.State != events.AgentRunning || a.Model != "m" || !a.Background || a.WorktreePath != "/r/app/.claude/worktrees/agent-a1" || !a.StartedAt.Equal(t0) {
		t.Errorf("a1 = %+v", a)
	}
	all, _ := h.st.ListSessions(ctx, store.SessionFilter{IncludeEnded: true})
	if len(all) != 1 {
		t.Errorf("%d rows, want the poller's only", len(all))
	}
}
