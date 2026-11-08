package live

import (
	"context"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/events"
	"github.com/gravexa-llc/ai-manager/internal/plans"
	"github.com/gravexa-llc/ai-manager/internal/state"
	"github.com/gravexa-llc/ai-manager/internal/store"
)

// A lead that spawned a background agent in its own worktree, sent it
// review fixes once it had finished (SendMessage resumes it under the same
// id), and whose session saw tool calls from loops no agent list names:
// Claude Code's forks of the agent (compaction, memory), in the agent's
// cwd. Such a fork's tool attempt is refused, but it fires tool.call, so
// the mod reports it like any other; nothing ever starts or ends its loop.

const (
	leadID  = "lead-session"
	agentA1 = "a1111111111111111"
	appDir  = "/work/app"
	wtA1    = appDir + "/.claude/worktrees/agent-" + agentA1
	// Forks of agentA1: their ids are made up the way Claude Code's are.
	forkB1 = "b1111111111111111" // one event: a handback, as the run ends
	forkB2 = "b2222222222222222" // a few tool attempts after the review fixes
	forkB3 = "b3333333333333333" // one event: a handback, as the resume starts
	forkB4 = "b4444444444444444" // an AskUserQuestion attempt
)

func leadStart(at time.Time) events.Event {
	e := ev(events.SessionStart, leadID, at)
	e.Cwd, e.Repo, e.Worktree, e.Branch, e.Source = appDir, appDir, appDir, "main", "startup"
	e.Interactive = ptr(true)
	return e
}

// in puts an event in a loop, with the loop's cwd.
func in(e events.Event, agent, cwd string) events.Event {
	e.AgentID, e.Cwd = agent, cwd
	return e
}

func agentStarts(agent, cwd string, at time.Time) events.Event {
	e := in(ev(events.AgentStart, leadID, at), agent, cwd)
	e.AgentType = "general-purpose"
	return e
}

func runEnds(agent string, at time.Time) events.Event {
	e := in(ev(events.TurnComplete, leadID, at), agent, "")
	e.Reason = events.ReasonAnswer
	return e
}

func listsA1(status string, at time.Time) events.Event {
	e := ev(events.AgentList, leadID, at)
	e.Agents = []events.Agent{{ID: agentA1, Status: status, Type: "general-purpose", Background: ptr(true)}}
	return e
}

// leadTool is a tool event in a loop, in a cwd.
func leadTool(agent, label, id, cwd string, at time.Time) events.Event {
	return in(tool(leadID, agent, label, id, at), agent, cwd)
}

// resumes reports the start of a run of agentA1 as the mod does: its
// turn_start and (newMod) the agent_start from classic.SubagentStart. A
// resumed run starts in the session's cwd, not the agent's worktree.
func resumes(newMod bool, cwd string, at time.Time) []events.Event {
	evs := []events.Event{in(ev(events.TurnStart, leadID, at), agentA1, cwd)}
	if newMod {
		evs = append(evs, agentStarts(agentA1, cwd, at.Add(5*time.Millisecond)))
	}
	return evs
}

// spawnAndReview plays the whole run: the spawn, the agent's first run and
// a fork at its handback, the resume with the review fixes and the forks
// around it. check is called after each step with what it is.
func spawnAndReview(h *harness, newMod bool, check func(step string)) {
	wc := ev(events.WorktreeCreate, leadID, t0.Add(2*time.Second))
	wc.Name = "agent-" + agentA1
	first := append([]events.Event{
		leadStart(t0), ev(events.TurnStart, leadID, t0.Add(time.Second)),
		wc, listsA1(events.AgentRunning, t0.Add(2*time.Second)),
	}, resumes(newMod, wtA1, t0.Add(2*time.Second))...)
	h.post(append(first, runEnds("", t0.Add(3*time.Second)))...) // the lead waits on its agent
	check("spawned")

	h.post(
		leadTool(agentA1, "Bash: Run make check", "t1", wtA1, t0.Add(time.Minute)),
		leadTool(agentA1, "SubagentHandback", "t2", wtA1, t0.Add(2*time.Minute)),
		leadTool(forkB1, "SubagentHandback", "f1", wtA1, t0.Add(2*time.Minute+5*time.Millisecond)),
		runEnds(agentA1, t0.Add(2*time.Minute+10*time.Millisecond)),
		// Taken just after the run ended, the snapshot still says running.
		listsA1(events.AgentRunning, t0.Add(2*time.Minute+20*time.Millisecond)),
	)
	check("finished")

	h.post(append(resumes(newMod, appDir, t0.Add(10*time.Minute)),
		leadTool(forkB3, "SubagentHandback", "f3", wtA1, t0.Add(10*time.Minute)))...)
	check("resumed")

	ask := in(needs(leadID, forkB4, "f5", t0.Add(11*time.Minute)), forkB4, wtA1)
	ask.Source = events.SourceAskUserQuestion
	h.post(
		leadTool(agentA1, "Bash: Run make all after review fixes", "t3", appDir, t0.Add(11*time.Minute)),
		leadTool(forkB2, "Bash: Run make all after review fixes", "f2", wtA1, t0.Add(11*time.Minute+time.Millisecond)),
		ask, leadTool(forkB4, "AskUserQuestion", "f5", wtA1, t0.Add(11*time.Minute+time.Second)),
		leadTool(forkB2, "ArtifactComments", "f4", wtA1, t0.Add(12*time.Minute)),
	)
	check("fixing")

	h.post(
		leadTool(agentA1, "SubagentHandback", "t4", appDir, t0.Add(13*time.Minute)),
		runEnds(agentA1, t0.Add(13*time.Minute+5*time.Millisecond)),
		listsA1(events.AgentCompleted, t0.Add(13*time.Minute+10*time.Millisecond)),
	)
	check("done")
}

// The agent is running exactly while one of its runs is; the forks never
// are, get no agent row and never make the session working or waiting.
// An older mod (no agent_start) gets the same.
func TestForkLoopsAreNotAgents(t *testing.T) {
	for _, newMod := range []bool{true, false} {
		name := "older mod"
		if newMod {
			name = "agent_start"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil)
			spawnAndReview(h, newMod, func(step string) {
				ag := h.agents(leadID)
				if len(ag) != 2 {
					t.Errorf("%s: agent rows %v, want the lead and %s only", step, keys(ag), agentA1)
				}
				a, ok := ag[agentA1]
				s := h.session(leadID)
				switch step {
				case "spawned", "resumed", "fixing":
					checkAgent(t, a, ok, events.AgentRunning, false)
					checkSession(t, s, StateWorking, "")
				default:
					checkAgent(t, a, ok, events.AgentCompleted, true)
					checkSession(t, s, StateTurnDone, "")
				}
				if a.AgentType != "general-purpose" || a.WorktreePath != wtA1 {
					t.Errorf("%s: %s = %+v", step, agentA1, a)
				}
				if step == "fixing" && s.LastAction != "Bash: Run make all after review fixes" {
					t.Errorf("%s: session's last action = %q, want the agent's", step, s.LastAction)
				}
			})
			// The forks' calls are in the timeline all the same.
			seen := map[string]bool{}
			for _, e := range h.events(leadID) {
				seen[e.AgentID] = true
			}
			for _, f := range []string{forkB1, forkB2, forkB3, forkB4} {
				if !seen[f] {
					t.Errorf("no timeline row from fork %s", f)
				}
			}
		})
	}
}

func keys(m map[string]store.Agent) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// classic.SubagentStart alone (the run's turn_start not heard, as for an
// agent whose spawn was nested in the hook) starts the agent and gives
// its type; its run's end ends it. An agent_start with no agent id is
// ignored.
func TestAgentStartAlone(t *testing.T) {
	h := newHarness(t, nil)
	stray := ev(events.AgentStart, leadID, t0.Add(time.Second))
	stray.Cwd = "/elsewhere"
	h.post(leadStart(t0), stray, agentStarts(agentA1, wtA1, t0.Add(2*time.Second)))
	a, ok := h.agents(leadID)[agentA1]
	checkAgent(t, a, ok, events.AgentRunning, false)
	if a.AgentType != "general-purpose" || a.WorktreePath != wtA1 {
		t.Errorf("%s = %+v", agentA1, a)
	}
	if s := h.session(leadID); s.State != StateWorking || s.Cwd != appDir {
		t.Errorf("session = %q in %q", s.State, s.Cwd)
	}
	h.post(runEnds(agentA1, t0.Add(time.Minute)))
	a, ok = h.agents(leadID)[agentA1]
	checkAgent(t, a, ok, events.AgentCompleted, true)
}

// planMD is a plan with one package, A1, whose branch is the agent's.
const planMD = `# App plan

Plan slug: app

| Package | Owner | Needs |
|---|---|---|
| **A1 One** | | — |
`

// board derives package A1 from the session and agent rows given, the
// worktrees on disk and planMD.
func board(t *testing.T, h *harness, agents []store.Agent) state.PackageState {
	t.Helper()
	ctx := context.Background()
	sessions, err := h.st.ListSessions(ctx, store.SessionFilter{IncludeEnded: true})
	if err != nil {
		t.Fatal(err)
	}
	parsed := plans.Parse([]byte(planMD))
	pkgs := state.StorePackages(parsed)
	for i := range pkgs {
		pkgs[i].PlanID = 1
	}
	registered := t0.Add(-time.Hour)
	in := state.Input{
		Now: h.clock.now(),
		Plans: []state.Plan{{Plan: store.Plan{ID: 1, Slug: plans.Slug("app-plan.md", []byte(planMD)), Title: parsed.Title,
			SourceMD: planMD, Repos: []string{"example-org/app"}, CreatedAt: registered}, Packages: pkgs}},
		Worktrees: []store.Worktree{
			{Path: appDir, MainRepo: appDir, Branch: "main", FirstSeen: registered, LastSeen: h.clock.now()},
			// The agent's branch; no commits of its own.
			{Path: wtA1, MainRepo: appDir, Branch: "app/a1-one", Locked: true, FirstSeen: t0, LastCommitAt: registered, LastSeen: h.clock.now()},
		},
		Sessions:  sessions,
		Agents:    agents,
		RepoNames: map[string]string{appDir: "example-org/app"},
	}
	ps := state.Derive(in, state.Match(in), state.Options{})
	if len(ps) != 1 {
		t.Fatalf("%d plans", len(ps))
	}
	for _, p := range ps[0].Packages {
		if p.ID == "A1" {
			return p
		}
	}
	t.Fatal("no package A1")
	return state.PackageState{}
}

func liveAgentsOn(p state.PackageState) []string {
	var out []string
	for _, l := range p.Links {
		if l.Agent != nil && l.Agent.Live {
			out = append(out, l.Agent.AgentID)
		}
	}
	return out
}

func badgeTexts(p state.PackageState) []string {
	var out []string
	for _, b := range p.Badges {
		out = append(out, b.Text)
	}
	return out
}

// Once the agent's runs are over, its package has no live agent and no
// collision, whatever its forks did in its worktree; with the rows an
// older aimd wrote for those forks it had both.
func TestForkLoopsLeaveThePackageAlone(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	spawnAndReview(h, true, func(step string) {
		if step != "fixing" {
			return
		}
		rows, err := h.st.ListAgents(ctx, leadID)
		if err != nil {
			t.Fatal(err)
		}
		p := board(t, h, rows)
		if got := liveAgentsOn(p); len(got) != 1 || got[0] != agentA1 || p.Lifecycle != state.Working {
			t.Errorf("while the review fixes run: %s with live agents %v, want working with %s", p.Lifecycle, got, agentA1)
		}
	})
	rows, err := h.st.ListAgents(ctx, leadID)
	if err != nil {
		t.Fatal(err)
	}
	p := board(t, h, rows)
	if got := liveAgentsOn(p); len(got) > 0 || p.Lifecycle == state.Working {
		t.Errorf("after the agent's last run: %s with live agents %v", p.Lifecycle, got)
	}
	for _, b := range p.Badges {
		if b.Kind == state.BadgeCollision {
			t.Errorf("badge %q", b.Text)
		}
	}

	// The same session as an older aimd stored it: a running row per fork,
	// no type, in the agent's worktree.
	legacy := append([]store.Agent(nil), rows...)
	for _, f := range []string{forkB1, forkB2} {
		legacy = append(legacy, store.Agent{SessionID: leadID, AgentID: f, Kind: store.AgentSubagent, WorktreePath: wtA1,
			State: events.AgentRunning, LastAction: "SubagentHandback", StartedAt: t0.Add(2 * time.Minute)})
	}
	old := board(t, h, legacy)
	if got := liveAgentsOn(old); len(got) != 2 || old.Lifecycle != state.Working {
		t.Errorf("legacy rows: %s with live agents %v, want working with the two forks", old.Lifecycle, got)
	}
	if got := badgeTexts(old); len(got) != 1 || got[0] != "collision: 2 live agents" {
		t.Errorf("legacy rows: badges %v, want the collision", got)
	}
}
