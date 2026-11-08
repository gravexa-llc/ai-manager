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

// A subagent's token count comes from agent_list: the latest stands, up
// or down, and a snapshot without one keeps it. It outlives the agent's
// turn and an aimd restart.
func TestAgentTokens(t *testing.T) {
	h := newHarness(t, nil)
	snap := func(at time.Duration, tokens int64, status string) events.Event {
		l := ev(events.AgentList, "s1", t0.Add(at))
		l.Agents = []events.Agent{{ID: "a1", Status: status, Type: "general-purpose", Tokens: tokens}}
		return l
	}
	h.post(start("s1", t0), snap(time.Second, 41_200, events.AgentRunning))
	if a := h.agents("s1")["a1"]; a.Tokens != 41_200 {
		t.Fatalf("a1 tokens = %d, want 41200", a.Tokens)
	}
	// Compaction shrinks the context: the count goes down with it.
	h.post(snap(30*time.Second, 286_000, events.AgentRunning), snap(60*time.Second, 52_000, events.AgentRunning))
	if a := h.agents("s1")["a1"]; a.Tokens != 52_000 {
		t.Errorf("a1 tokens after compaction = %d, want 52000", a.Tokens)
	}
	// An older mod's snapshot carries none; the agent ends.
	h.post(snap(90*time.Second, 0, events.AgentCompleted))
	if a := h.agents("s1")["a1"]; a.Tokens != 52_000 || a.State != events.AgentCompleted {
		t.Errorf("a1 after a snapshot without tokens = %+v", a)
	}
	// agent_list never gives the main loop one (TestMainLoopTokens).
	if a := h.agents("s1")[""]; a.Tokens != 0 {
		t.Errorf("lead tokens = %d, want 0", a.Tokens)
	}

	// After a restart the count is seeded from the row, and kept by an event
	// that says nothing of it.
	ctx := context.Background()
	if err := h.st.UpsertSessions(ctx, store.Session{Key: "pid:8", SessionID: "s2", PID: 8, Kind: "interactive", Cwd: "/r/app", State: "working"}); err != nil {
		t.Fatal(err)
	}
	if err := h.st.UpsertAgents(ctx, store.Agent{SessionID: "s2", AgentID: "b1", Kind: store.AgentSubagent, State: events.AgentRunning, Tokens: 9_900, StartedAt: t0}); err != nil {
		t.Fatal(err)
	}
	h.post(tool("s2", "b1", "Bash: x", "1", t0.Add(time.Minute)))
	if a := h.agents("s2")["b1"]; a.Tokens != 9_900 || a.LastAction != "Bash: x" {
		t.Errorf("b1 after a tool event = %+v", a)
	}
}

// The main loop's token count comes on its own tool and turn_complete
// events, onto its agent row: the latest stands, up or down; an older
// event's is ignored, a subagent's event gives it none, and one without a
// count keeps it. It outlives an aimd restart, and /clear forgets it.
func TestMainLoopTokens(t *testing.T) {
	h := newHarness(t, nil)
	sized := func(e events.Event, n int64) events.Event {
		e.Tokens = n
		return e
	}
	lead := func(h *harness) int64 { return h.agents("lead-session")[""].Tokens }
	sub := ev(events.AgentStart, "lead-session", t0.Add(time.Second))
	sub.AgentID, sub.AgentType = "a1111111111111111", "general-purpose"
	h.post(start("lead-session", t0), sub, tool("lead-session", "", "Bash: Run the tests", "1", t0.Add(2*time.Second)))
	if n := lead(h); n != 0 {
		t.Fatalf("lead tokens before a count = %d", n)
	}
	h.post(sized(tool("lead-session", "", "Bash: Run the tests", "2", t0.Add(3*time.Second)), 61_200))
	if n := lead(h); n != 61_200 {
		t.Fatalf("lead tokens = %d, want 61200", n)
	}
	done := sized(ev(events.TurnComplete, "lead-session", t0.Add(4*time.Second)), 62_000)
	done.Reason = events.ReasonAnswer
	h.post(done)
	if n := lead(h); n != 62_000 {
		t.Errorf("lead tokens after its turn = %d, want 62000", n)
	}
	// Compaction: down. Then an event sent before that one, and a
	// subagent's: neither changes it, and the subagent gets none from it.
	h.post(sized(tool("lead-session", "", "Bash: ls", "3", t0.Add(6*time.Second)), 20_000))
	h.post(sized(tool("lead-session", "", "Bash: old", "4", t0.Add(5*time.Second)), 99_000),
		sized(tool("lead-session", "a1111111111111111", "Bash: ls", "5", t0.Add(7*time.Second)), 77_000))
	if n, a := lead(h), h.agents("lead-session")["a1111111111111111"]; n != 20_000 || a.Tokens != 0 {
		t.Errorf("lead tokens = %d (want 20000), subagent's = %d (want 0)", n, a.Tokens)
	}
	// No count keeps it, across an aimd restart too.
	h2 := newHarnessOn(t, h.st)
	h2.post(tool("lead-session", "", "Read: README.md", "6", t0.Add(8*time.Second)))
	if n := lead(h2); n != 20_000 {
		t.Errorf("lead tokens after a restart = %d, want 20000", n)
	}
	// /clear: a new conversation, with no count until its first response;
	// a late event of the old one doesn't bring it back.
	end := ev(events.SessionEnd, "lead-session", t0.Add(9*time.Second))
	end.Reason = events.ReasonClear
	h2.post(end, sized(tool("lead-session", "", "Bash: late", "7", t0.Add(8500*time.Millisecond)), 21_000))
	if n := lead(h2); n != 0 {
		t.Errorf("lead tokens after /clear = %d, want 0", n)
	}
	h2.post(sized(tool("lead-session", "", "Bash: ls", "8", t0.Add(10*time.Second)), 4_100))
	if n := lead(h2); n != 4_100 {
		t.Errorf("lead tokens after /clear's first response = %d, want 4100", n)
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
