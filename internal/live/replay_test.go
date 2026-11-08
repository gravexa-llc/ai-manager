package live

import (
	"strings"
	"testing"

	"github.com/gravexa-llc/ai-manager/internal/events"
	"github.com/gravexa-llc/ai-manager/internal/store"
)

// The recorded scenarios, replayed through POST /events: each event log in
// testdata/events is translated into the events the mod sends
// (internal/events/eventstest) and posted in batches. The asserts are the
// session and agent states they derive to.

const repo = "/tmp/probe"

func wtPath(name string) string { return repo + "/.claude/worktrees/" + name }

// checkSession fails unless the session's row has the state (and reason,
// when want.StateReason is set).
func checkSession(t *testing.T, s store.Session, state, reason string) {
	t.Helper()
	if s.State != state || (reason != "" && s.StateReason != reason) {
		t.Errorf("session state = %q (%q), want %q (%q)", s.State, s.StateReason, state, reason)
	}
}

func checkAgent(t *testing.T, a store.Agent, ok bool, state string, ended bool) {
	t.Helper()
	if !ok {
		t.Errorf("agent missing")
		return
	}
	if a.State != state || a.EndedAt.IsZero() == ended {
		t.Errorf("agent %q: state %q ended %v, want %q ended %v", a.AgentID, a.State, a.EndedAt, state, ended)
	}
}

func TestReplayMinimalSession(t *testing.T) {
	h := newHarness(t, nil)
	evs := fixture(t, "print-session-minimal.jsonl")
	sid := evs[0].SessionID

	first, rest := split(t, evs, kind(events.SessionStart))
	h.replay(first)
	s := h.session(sid)
	checkSession(t, s, StateIdle, "")
	if s.Key != store.ModSessionKeyPrefix+sid || s.Cwd != repo || s.Repo != repo || s.WorktreePath != repo || !s.Headless {
		t.Errorf("new row = %+v", s)
	}

	first, rest = split(t, rest, kind(events.TurnStart))
	h.replay(first)
	checkSession(t, h.session(sid), StateWorking, "")

	first, rest = split(t, rest, kind(events.TurnComplete))
	h.replay(first)
	s = h.session(sid)
	checkSession(t, s, StateTurnDone, "answer")
	if s.LastSaid != "[assistant text]" || !s.LastEventAt.Equal(first[len(first)-1].At) {
		t.Errorf("activity = %q at %v", s.LastSaid, s.LastEventAt)
	}

	h.replay(rest)
	s = h.session(sid)
	checkSession(t, s, StateGone, "other")
	if s.EndedAt.IsZero() {
		t.Error("session not ended after session_end")
	}
	lead, ok := h.agents(sid)[""]
	checkAgent(t, lead, ok, events.AgentCompleted, true)
	if lead.Kind != store.AgentLead || lead.WorktreePath != repo {
		t.Errorf("lead = %+v", lead)
	}
	// One timeline row per event but the agent_list snapshot.
	if n := len(h.events(sid)); n != len(evs)-1 {
		t.Errorf("%d event rows, want %d", n, len(evs)-1)
	}
}

func TestReplaySubagentsDefaultBackground(t *testing.T) {
	h := newHarness(t, nil)
	evs := fixture(t, "subagents-default-background.jsonl")
	sid := evs[0].SessionID
	const fg, bg, wt = "a61cc06fc9f51079d", "aec280c05a51a0117", "a5bd483a78c5f4783"

	// All three spawned: the session works, the agents run in the
	// background, the isolated one in its worktree.
	first, rest := split(t, evs, func(e events.Event) bool { return e.Kind == events.AgentList && len(e.Agents) == 3 })
	h.replay(first)
	checkSession(t, h.session(sid), StateWorking, "")
	ag := h.agents(sid)
	if len(ag) != 4 {
		t.Fatalf("%d agent rows, want the lead and 3 subagents", len(ag))
	}
	for _, id := range []string{fg, bg, wt} {
		a := ag[id]
		checkAgent(t, a, true, events.AgentRunning, false)
		if a.Kind != store.AgentSubagent || !a.Background || a.ParentAgentID != "" || a.AgentType != "general-purpose" {
			t.Errorf("agent %s = %+v", id, a)
		}
	}
	if ag[wt].WorktreePath != wtPath("agent-"+wt) || ag[fg].WorktreePath != repo || ag[bg].WorktreePath != repo {
		t.Errorf("worktrees: wt %q fg %q bg %q", ag[wt].WorktreePath, ag[fg].WorktreePath, ag[bg].WorktreePath)
	}

	// The lead's first turn ends while its agents still run.
	first, rest = split(t, rest, func(e events.Event) bool { return e.Kind == events.TurnComplete && e.AgentID == "" })
	h.replay(first)
	checkSession(t, h.session(sid), StateWorking, "")
	if lead := h.agents(sid)[""]; lead.State != events.AgentIdle {
		t.Errorf("lead = %q, want idle", lead.State)
	}

	// The worktree agent finishes; snapshots that still say "running" for
	// it don't bring it back.
	first, rest = split(t, rest, func(e events.Event) bool { return e.Kind == events.TurnComplete && e.AgentID == wt })
	h.replay(first)
	first, rest = split(t, rest, func(e events.Event) bool { return e.Kind == events.TurnComplete && e.AgentID == "" })
	h.replay(first)
	ag = h.agents(sid)
	checkAgent(t, ag[wt], true, events.AgentCompleted, true)
	checkAgent(t, ag[bg], true, events.AgentRunning, false)
	checkSession(t, h.session(sid), StateWorking, "")

	// Everything finished, then the session ends.
	before, last := rest[:len(rest)-1], rest[len(rest)-1:]
	h.replay(before)
	checkSession(t, h.session(sid), StateTurnDone, "answer")
	for id, a := range h.agents(sid) {
		if id != "" {
			checkAgent(t, a, true, events.AgentCompleted, true)
		}
	}
	h.replay(last)
	checkSession(t, h.session(sid), StateGone, "")
}

func TestReplayForegroundAndWorktreeAgents(t *testing.T) {
	h := newHarness(t, nil)
	evs := fixture(t, "subagents-foreground-worktrees.jsonl")
	sid := evs[0].SessionID
	const fg, clean, dirty = "a2aad852c90fc06ec", "a49df8b5bfe161908", "ad96f56a6a4ee2505"

	// The foreground agent runs while the lead waits on it.
	first, rest := split(t, evs, func(e events.Event) bool { return e.Kind == events.AgentList && len(e.Agents) == 1 })
	h.replay(first)
	checkSession(t, h.session(sid), StateWorking, "")
	checkAgent(t, h.agents(sid)[fg], true, events.AgentRunning, false)

	h.replay(rest[:len(rest)-1])
	checkSession(t, h.session(sid), StateTurnDone, "answer")
	ag := h.agents(sid)
	for _, id := range []string{fg, clean, dirty} {
		a, ok := ag[id]
		checkAgent(t, a, ok, events.AgentCompleted, true)
		if a.Background {
			t.Errorf("agent %s is background, want foreground", id)
		}
	}
	if ag[clean].WorktreePath != wtPath("agent-"+clean) || ag[dirty].WorktreePath != wtPath("agent-"+dirty) || ag[fg].WorktreePath != repo {
		t.Errorf("worktrees: clean %q dirty %q fg %q", ag[clean].WorktreePath, ag[dirty].WorktreePath, ag[fg].WorktreePath)
	}
	h.replay(rest[len(rest)-1:])
	checkSession(t, h.session(sid), StateGone, "")
}

func TestReplayExplicitForeground(t *testing.T) {
	h := newHarness(t, nil)
	evs := fixture(t, "subagent-explicit-foreground.jsonl")
	sid := evs[0].SessionID
	const id = "a2743fbbaf44b95d0"
	first, rest := split(t, evs, func(e events.Event) bool { return e.Kind == events.TurnStart && e.AgentID == id })
	h.replay(first)
	checkSession(t, h.session(sid), StateWorking, "")
	h.replay(rest[:len(rest)-1])
	a, ok := h.agents(sid)[id]
	checkAgent(t, a, ok, events.AgentCompleted, true)
	if a.Background {
		t.Error("explicit run_in_background: false gave a background agent")
	}
	checkSession(t, h.session(sid), StateTurnDone, "")
}

func TestReplayPermissionAskMainLoop(t *testing.T) {
	h := newHarness(t, nil)
	evs := fixture(t, "permission-ask-main-loop.jsonl")
	sid := evs[0].SessionID

	first, rest := split(t, evs, kind(events.NeedsYou))
	asked := first[len(first)-1]
	h.replay(first)
	s := h.session(sid)
	checkSession(t, s, StateNeedsYou, "permission prompt: Bash: Create directory s2-perm-dir")
	if !s.NeedsYouSince.Equal(asked.At) {
		t.Errorf("needs_you_since = %v, want %v", s.NeedsYouSince, asked.At)
	}
	if lead := h.agents(sid)[""]; lead.State != events.AgentWaiting {
		t.Errorf("lead = %q, want waiting", lead.State)
	}

	// The dialog (classic.PermissionRequest) is the only signal: tool.check's
	// ask raises none, since the mode's decider takes it first.
	if asked.Source != events.SourcePermissionRequest {
		t.Errorf("needs_you source = %q, want %q", asked.Source, events.SourcePermissionRequest)
	}
	n := 0
	for _, e := range evs {
		if e.Kind == events.NeedsYou {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d needs_you in the replay, want 1", n)
	}

	// The call finishes (denied, under -p): answered.
	first, rest = split(t, rest, kind(events.Tool))
	h.replay(first)
	s = h.session(sid)
	checkSession(t, s, StateWorking, "")
	if !s.NeedsYouSince.IsZero() || s.LastAction != "Bash: Create directory s2-perm-dir" {
		t.Errorf("after the tool: since %v, last action %q", s.NeedsYouSince, s.LastAction)
	}
	h.replay(rest)
	checkSession(t, h.session(sid), StateGone, "")
	if st := h.status().Events; st.Deduped != 0 {
		t.Errorf("deduped = %d, want 0", st.Deduped)
	}
}

func TestReplayPermissionAskSubagent(t *testing.T) {
	h := newHarness(t, nil)
	evs := fixture(t, "permission-ask-subagent.jsonl")
	sid := evs[0].SessionID
	const sub = "ad042043bfa112bd0"

	// The lead's turn is over; its agent still works.
	first, rest := split(t, evs, func(e events.Event) bool { return e.Kind == events.TurnComplete && e.AgentID == "" })
	h.replay(first)
	checkSession(t, h.session(sid), StateWorking, "")

	first, rest = split(t, rest, kind(events.NeedsYou))
	h.replay(first)
	checkSession(t, h.session(sid), StateNeedsYou, "agent "+sub+": permission prompt: Bash: Create directory s2-sub-dir")
	ag := h.agents(sid)
	checkAgent(t, ag[sub], true, events.AgentWaiting, false)
	if ag[""].State != events.AgentIdle {
		t.Errorf("lead = %q, want idle", ag[""].State)
	}

	first, rest = split(t, rest, kind(events.Tool))
	h.replay(first)
	checkSession(t, h.session(sid), StateWorking, "")
	checkAgent(t, h.agents(sid)[sub], true, events.AgentRunning, false)

	h.replay(rest[:len(rest)-1])
	checkSession(t, h.session(sid), StateTurnDone, "")
	checkAgent(t, h.agents(sid)[sub], true, events.AgentCompleted, true)
}

func TestReplayStopFailure(t *testing.T) {
	h := newHarness(t, nil)
	evs := fixture(t, "stop-failure-model-not-found.jsonl")
	sid := evs[0].SessionID

	first, rest := split(t, evs, kind(events.Error))
	h.replay(first)
	checkSession(t, h.session(sid), StateError, "model_not_found")

	h.replay(rest[:len(rest)-1])
	s := h.session(sid)
	checkSession(t, s, StateError, "model_not_found")
	if lead := h.agents(sid)[""]; lead.State != events.AgentFailed || !lead.EndedAt.IsZero() {
		t.Errorf("lead = %q ended %v, want failed, not ended", lead.State, lead.EndedAt)
	}
	h.replay(rest[len(rest)-1:])
	checkSession(t, h.session(sid), StateGone, "")
}

func TestReplayNamedAgentResumed(t *testing.T) {
	h := newHarness(t, nil)
	evs := fixture(t, "named-agent-sendmessage-resume.jsonl")
	sid := evs[0].SessionID
	const scout = "ab38c36cb55a33059"
	// classic.SubagentStart fired for both runs, under the one id.
	var starts []string
	for _, e := range evs {
		if e.Kind == events.AgentStart {
			starts = append(starts, e.AgentID+" "+e.AgentType)
		}
	}
	if len(starts) != 2 || starts[0] != starts[1] || !strings.HasPrefix(starts[0], scout+" ") {
		t.Errorf("agent_start events = %q, want two for %s", starts, scout)
	}
	isStatus := func(status string) func(events.Event) bool {
		return func(e events.Event) bool {
			return e.Kind == events.AgentList && len(e.Agents) == 1 && e.Agents[0].Status == status
		}
	}

	// Its first run ends; it stays alive, idle, between messages.
	first, rest := split(t, evs, isStatus(events.AgentIdle))
	h.replay(first)
	checkAgent(t, h.agents(sid)[scout], true, events.AgentIdle, false)
	checkSession(t, h.session(sid), StateTurnDone, "")

	// SendMessage wakes it: the same agent row runs again.
	first, rest = split(t, rest, func(e events.Event) bool { return e.Kind == events.TurnStart && e.AgentID == scout })
	h.replay(first)
	checkAgent(t, h.agents(sid)[scout], true, events.AgentRunning, false)
	checkSession(t, h.session(sid), StateWorking, "")

	first, rest = split(t, rest, isStatus(events.AgentIdle))
	h.replay(first)
	checkAgent(t, h.agents(sid)[scout], true, events.AgentIdle, false)

	h.replay(rest)
	ag := h.agents(sid)
	if len(ag) != 2 {
		t.Errorf("%d agent rows, want the lead and scout", len(ag))
	}
	checkAgent(t, ag[scout], true, events.AgentCompleted, true)
	if !ag[scout].Background {
		t.Error("scout isn't marked background")
	}
	checkSession(t, h.session(sid), StateGone, "")
}

func TestReplayEnterExitWorktree(t *testing.T) {
	h := newHarness(t, nil)
	evs := fixture(t, "session-enter-exit-worktree.jsonl")
	sid := evs[0].SessionID

	first, rest := split(t, evs, func(e events.Event) bool { return e.Kind == events.Tool && e.Tool == "EnterWorktree" })
	h.replay(first)
	s := h.session(sid)
	if s.Cwd != wtPath("s2ew") || s.WorktreePath != wtPath("s2ew") || s.Branch != "worktree-s2ew" || s.Repo != repo {
		t.Errorf("in the worktree: cwd %q worktree %q branch %q repo %q", s.Cwd, s.WorktreePath, s.Branch, s.Repo)
	}
	if lead := h.agents(sid)[""]; lead.WorktreePath != wtPath("s2ew") {
		t.Errorf("lead worktree = %q", lead.WorktreePath)
	}

	first, rest = split(t, rest, func(e events.Event) bool { return e.Kind == events.Tool && e.Tool == "ExitWorktree" })
	h.replay(first)
	s = h.session(sid)
	if s.Cwd != repo || s.WorktreePath != repo || s.Branch != "" {
		t.Errorf("back out: cwd %q worktree %q branch %q", s.Cwd, s.WorktreePath, s.Branch)
	}
	checkSession(t, s, StateWorking, "")
	h.replay(rest)
	checkSession(t, h.session(sid), StateGone, "")
}

func TestReplayWorktreeFlagSession(t *testing.T) {
	h := newHarness(t, nil)
	evs := fixture(t, "session-worktree-flag.jsonl")
	sid := evs[0].SessionID
	h.replay(evs[:len(evs)-1])
	s := h.session(sid)
	if s.WorktreePath != wtPath("s2w") || s.Repo != repo || s.Branch != "worktree-s2w" {
		t.Errorf("claude -w session: worktree %q repo %q branch %q", s.WorktreePath, s.Repo, s.Branch)
	}
	checkSession(t, s, StateTurnDone, "")
}
