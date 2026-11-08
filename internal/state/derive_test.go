package state

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/store"
)

// TestLifecycle walks lifecycle's order (derive.go), one situation per
// case, on A2 (which needs A1) unless the case says otherwise.
func TestLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(in *Input)
		id    string
		want  Lifecycle
		by    []string // BlockedBy
	}{
		{"nothing linked, need not done", func(in *Input) {}, "A2", Blocked, []string{"A1"}},
		{"nothing linked, no needs", func(in *Input) {}, "A1", Ready, nil},
		{"need done by a merged PR", func(in *Input) {
			in.PRs = []store.PR{merged(pr(1, "aim/a1-x", ""))}
		}, "A2", Ready, nil},
		{"need done by ✅", func(in *Input) { in.Plans[0].Packages[0].DoneInPlan = true }, "A2", Ready, nil},
		{"need done by Mark done", func(in *Input) { in.Plans[0].Packages[0].DoneManual = true }, "A2", Ready, nil},
		{"need ✅ but a follow-up PR is open: still done for A2", func(in *Input) {
			in.Plans[0].Packages[0].DoneInPlan = true
			in.PRs = []store.PR{pr(1, "aim/a1-follow-up", "")}
		}, "A2", Ready, nil},
		{"need merged once but another PR is open: not done", func(in *Input) {
			in.PRs = []store.PR{merged(pr(1, "aim/a1-x", "")), pr(2, "aim/a1-y", "")}
		}, "A2", Blocked, []string{"A1"}},
		{"open PR", func(in *Input) { in.PRs = []store.PR{pr(1, "aim/a2-x", "")} }, "A2", InReview, []string{"A1"}},
		{"open PR wins over merged and ✅", func(in *Input) {
			in.Plans[0].Packages[1].DoneInPlan = true
			in.PRs = []store.PR{merged(pr(1, "aim/a2-x", "")), pr(2, "aim/a2-y", "")}
		}, "A2", InReview, []string{"A1"}},
		{"merged PR", func(in *Input) { in.PRs = []store.PR{merged(pr(1, "aim/a2-x", ""))} }, "A2", Done, []string{"A1"}},
		{"merged and closed PRs", func(in *Input) {
			in.PRs = []store.PR{merged(pr(1, "aim/a2-x", "")), closed(pr(2, "aim/a2-y", ""))}
		}, "A2", Done, []string{"A1"}},
		{"✅ only", func(in *Input) { in.Plans[0].Packages[1].DoneInPlan = true }, "A2", Done, []string{"A1"}},
		{"Mark done only", func(in *Input) { in.Plans[0].Packages[1].DoneManual = true }, "A2", Done, []string{"A1"}},
		{"your own step isn't done by a merged PR", func(in *Input) {
			in.PRs = []store.PR{merged(pr(1, "aim/y1-x", ""))}
		}, "Y1", Ready, nil},
		{"your own step is done by ✅", func(in *Input) { pkgByID(in, "Y1").DoneInPlan = true }, "Y1", Done, nil},
		{"live agent", func(in *Input) {
			in.Links = []store.Link{{PlanID: 1, PkgID: "A2", Kind: store.LinkAgent, SessionID: "s1", AgentID: "ag1", MatchMethod: store.MatchSpawn, Confidence: store.ConfidenceExact}}
			in.Sessions = []store.Session{session("s1", repoPath)}
			in.Agents = []store.Agent{{SessionID: "s1", AgentID: "ag1", Kind: "subagent", State: "running"}}
		}, "A2", Working, []string{"A1"}},
		{"finished agent", func(in *Input) {
			in.Links = []store.Link{{PlanID: 1, PkgID: "A2", Kind: store.LinkAgent, SessionID: "s1", AgentID: "ag1", MatchMethod: store.MatchSpawn, Confidence: store.ConfidenceExact}}
			in.Sessions = []store.Session{session("s1", repoPath)}
			in.Agents = []store.Agent{{SessionID: "s1", AgentID: "ag1", Kind: "subagent", State: "completed"}}
		}, "A2", Blocked, []string{"A1"}},
		{"agent of an ended session", func(in *Input) {
			in.Links = []store.Link{{PlanID: 1, PkgID: "A2", Kind: store.LinkAgent, SessionID: "s1", AgentID: "ag1", MatchMethod: store.MatchSpawn, Confidence: store.ConfidenceExact}}
			s := session("s1", repoPath)
			s.EndedAt = now.Add(-time.Hour)
			in.Sessions = []store.Session{s}
			in.Agents = []store.Agent{{SessionID: "s1", AgentID: "ag1", Kind: "subagent", State: "running"}}
		}, "A2", Blocked, []string{"A1"}},
		{"linked branch with commits", func(in *Input) {
			in.Worktrees = append(in.Worktrees, withCommits(worktree("a2", "aim/a2-x"), now.Add(-time.Hour)))
		}, "A2", Working, []string{"A1"}},
		{"linked branch ahead of its upstream", func(in *Input) {
			w := worktree("a2", "aim/a2-x")
			w.UnpushedCount = 2
			in.Worktrees = append(in.Worktrees, w)
		}, "A2", Working, []string{"A1"}},
		{"linked branch without commits", func(in *Input) {
			in.Worktrees = append(in.Worktrees, worktree("a2", "aim/a2-x"))
		}, "A2", Blocked, []string{"A1"}},
		{"removed worktree with commits", func(in *Input) {
			w := withCommits(worktree("a2", "aim/a2-x"), now.Add(-time.Hour))
			w.RemovedAt = now.Add(-time.Minute)
			in.Worktrees = append(in.Worktrees, w)
		}, "A2", Blocked, []string{"A1"}},
		{"closed PR", func(in *Input) { in.PRs = []store.PR{closed(pr(1, "aim/a2-x", ""))} }, "A2", Dropped, []string{"A1"}},
		{"closed PR but the branch has commits", func(in *Input) {
			in.PRs = []store.PR{closed(pr(1, "aim/a2-x", ""))}
			in.Worktrees = append(in.Worktrees, withCommits(worktree("a2", "aim/a2-x"), now.Add(-time.Hour)))
		}, "A2", Working, []string{"A1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInput(t)
			tc.setup(&in)
			p := pkgOf(t, derive(in), tc.id)
			if p.Lifecycle != tc.want {
				t.Errorf("%s = %s, want %s (links %q)", tc.id, p.Lifecycle, tc.want, refs(p))
			}
			deepEq(t, "blocked by", p.BlockedBy, tc.by)
		})
	}
}

func pkgByID(in *Input, id string) *store.Package {
	for i := range in.Plans[0].Packages {
		if in.Plans[0].Packages[i].PkgID == id {
			return &in.Plans[0].Packages[i]
		}
	}
	panic("no package " + id)
}

func TestCycles(t *testing.T) {
	const md = `# Cycles

| Package | Needs |
|---|---|
| **A1 One** | A3 |
| **A2 Two** | A1 |
| **A3 Three** | A2 |
| **A4 Four** | A1 |
| **B1 Pair** | B2 |
| **B2 Pair** | B1 |
| **C1 Free** | — |
`
	in := Input{Now: now, Plans: []Plan{mdPlan(t, 1, md, registered, repo)}}
	in.Plans[0].Packages[4].DoneInPlan = true // B1 ✅: done wins over the cycle
	ps := derive(in)
	deepEq(t, "cycle warnings", ps.Warnings, []Warning{
		{Kind: WarnCycle, Line: 5, ID: "A1", Message: "dependency cycle A1 → A3 → A2 → A1: these packages stay blocked until one of them is done or their Needs change"},
		{Kind: WarnCycle, Line: 9, ID: "B1", Message: "dependency cycle B1 → B2 → B1: these packages stay blocked until one of them is done or their Needs change"},
	})
	for id, want := range map[string]struct {
		lc    Lifecycle
		cycle string
		by    string
	}{
		"A1": {Blocked, "A1 A3 A2 A1", "A3"},
		"A2": {Blocked, "A1 A3 A2 A1", "A1"},
		"A3": {Blocked, "A1 A3 A2 A1", "A2"},
		"A4": {Blocked, "", "A1"},
		"B1": {Done, "B1 B2 B1", "B2"},
		"B2": {Ready, "B1 B2 B1", ""}, // its need is done: the broken cycle releases it
		"C1": {Ready, "", ""},
	} {
		p := pkgOf(t, ps, id)
		if p.Lifecycle != want.lc || strings.Join(p.Cycle, " ") != want.cycle || strings.Join(p.BlockedBy, " ") != want.by {
			t.Errorf("%s = %s, cycle %v, blocked by %v; want %s, %q, %q", id, p.Lifecycle, p.Cycle, p.BlockedBy, want.lc, want.cycle, want.by)
		}
	}
}

// TestCycleRelease checks C2 Q6's rule: a package in a cycle is blocked
// only by the members it needs that aren't done, so once one member is
// done the rest proceed normally, one after another.
func TestCycleRelease(t *testing.T) {
	const md = `# Cycle release

| Package | Needs |
|---|---|
| **A1 One** | A3 |
| **A2 Two** | A1 |
| **A3 Three** | A2, X1 |
| **X1 Outside** | — |
`
	check := func(step string, ps PlanState, want map[string]Lifecycle) {
		t.Helper()
		for id, lc := range want {
			if p := pkgOf(t, ps, id); p.Lifecycle != lc {
				t.Errorf("%s: %s = %s (blocked by %v), want %s", step, id, p.Lifecycle, p.BlockedBy, lc)
			}
		}
	}
	in := Input{Now: now, Plans: []Plan{mdPlan(t, 1, md, registered, repo)}}
	check("nothing done", derive(in), map[string]Lifecycle{"A1": Blocked, "A2": Blocked, "A3": Blocked, "X1": Ready})

	pkgByID(&in, "A1").DoneManual = true // Mark done breaks the cycle
	check("A1 done", derive(in), map[string]Lifecycle{"A1": Done, "A2": Ready, "A3": Blocked, "X1": Ready})
	if a3 := pkgOf(t, derive(in), "A3"); strings.Join(a3.BlockedBy, " ") != "A2 X1" || strings.Join(a3.Cycle, " ") != "A1 A3 A2 A1" {
		t.Errorf("A3 blocked by %v, cycle %v; want A2 X1 and the cycle still named", a3.BlockedBy, a3.Cycle)
	}

	pkgByID(&in, "A2").DoneInPlan = true
	check("A2 done too", derive(in), map[string]Lifecycle{"A3": Blocked, "X1": Ready}) // still needs X1, outside the cycle

	pkgByID(&in, "X1").DoneInPlan = true
	check("X1 done too", derive(in), map[string]Lifecycle{"A3": Ready})
}

// TestBadges checks each badge on its own, on package A1.
func TestBadges(t *testing.T) {
	agentOn := func(in *Input, s store.Session) {
		in.Worktrees = append(in.Worktrees, worktree("a1", "aim/a1-x"))
		s.WorktreePath, s.Cwd = wtRoot+"a1", wtRoot+"a1"
		in.Sessions = append(in.Sessions, s)
	}
	openPR := func(mod func(*store.PR)) func(in *Input) {
		return func(in *Input) {
			p := pr(1, "aim/a1-x", "")
			mod(&p)
			in.PRs = []store.PR{p}
		}
	}
	for _, tc := range []struct {
		name  string
		setup func(in *Input)
		want  []string // "kind: text"
	}{
		{"none", func(in *Input) {}, nil},
		{"needs you, with since", func(in *Input) {
			s := session("s1", "")
			s.State, s.StateReason, s.NeedsYouSince = "needs_you", "permission prompt", now.Add(-10*time.Minute)
			agentOn(in, s)
		}, []string{"needs_you: needs you: permission prompt"}},
		{"needs you, dismissed", func(in *Input) {
			s := session("s1", "")
			s.State, s.NeedsYouSince, s.NeedsYouDismissedAt = "needs_you", now.Add(-10*time.Minute), now.Add(-time.Minute)
			agentOn(in, s)
		}, nil},
		{"needs you again after a dismiss", func(in *Input) {
			s := session("s1", "")
			s.State, s.StateReason = "needs_you", "reply"
			s.NeedsYouDismissedAt, s.NeedsYouSince = now.Add(-time.Hour), now.Add(-time.Minute)
			agentOn(in, s)
		}, []string{"needs_you: needs you: reply"}},
		{"a waiting subagent", func(in *Input) {
			in.Worktrees = append(in.Worktrees, worktree("a1", "aim/a1-x"))
			in.Sessions = []store.Session{session("lead", repoPath)}
			in.Agents = []store.Agent{{SessionID: "lead", AgentID: "ag", Kind: "subagent", State: "waiting", WorktreePath: wtRoot + "a1"}}
		}, []string{"needs_you: needs you: waiting"}},
		{"CI failing", openPR(func(p *store.PR) { p.ChecksState = "FAILURE" }), []string{"ci_failing: CI failing on #1"}},
		{"CI error", openPR(func(p *store.PR) { p.ChecksState = "ERROR" }), []string{"ci_failing: CI failing on #1"}},
		{"CI pending is not failing", openPR(func(p *store.PR) { p.ChecksState = "PENDING" }), nil},
		{"conflict", openPR(func(p *store.PR) { p.MergeStateStatus = "DIRTY" }), []string{"conflict: conflict on #1"}},
		{"conflicting", openPR(func(p *store.PR) { p.Mergeable = "CONFLICTING" }), []string{"conflict: conflict on #1"}},
		{"waiting on you", openPR(func(p *store.PR) { p.MergeStateStatus, p.ChecksState = "CLEAN", "SUCCESS" }), []string{"waiting_on_you: waiting on you on #1"}},
		{"waiting on you with no CI (A2's NONE)", openPR(func(p *store.PR) { p.MergeStateStatus, p.ChecksState = "CLEAN", "NONE" }), []string{"waiting_on_you: waiting on you on #1"}},
		{"a CLEAN draft is a draft", openPR(func(p *store.PR) { p.MergeStateStatus, p.Draft = "CLEAN", true }), []string{"draft: draft on #1"}},
		{"changes requested", openPR(func(p *store.PR) { p.ReviewDecision = "CHANGES_REQUESTED" }), []string{"changes_requested: changes requested on #1"}},
		{"review requested", openPR(func(p *store.PR) { p.ReviewDecision = "REVIEW_REQUIRED" }), []string{"review_requested: review requested on #1"}},
		{"approved", openPR(func(p *store.PR) { p.ReviewDecision = "APPROVED" }), nil},
		{"merged PRs have no PR badges", func(in *Input) {
			p := merged(pr(1, "aim/a1-x", ""))
			p.ChecksState, p.MergeStateStatus = "FAILURE", "DIRTY"
			in.PRs = []store.PR{p}
		}, nil},
		{"stalled", func(in *Input) {
			in.Worktrees = append(in.Worktrees, withCommits(seenSince(worktree("a1", "aim/a1-x"), now.Add(-40*time.Hour)), now.Add(-30*time.Hour)))
		}, []string{"stalled: stalled: commits, no live agent and no PR for 30h"}},
		{"recent commits aren't stalled", func(in *Input) {
			in.Worktrees = append(in.Worktrees, withCommits(worktree("a1", "aim/a1-x"), now.Add(-10*time.Hour)))
		}, nil},
		{"commits with a live agent aren't stalled", func(in *Input) {
			in.Worktrees = append(in.Worktrees, withCommits(seenSince(worktree("a1", "aim/a1-x"), now.Add(-40*time.Hour)), now.Add(-30*time.Hour)))
			in.Sessions = []store.Session{session("s1", wtRoot+"a1")}
		}, nil},
		{"quiet: no mod event for a while", func(in *Input) {
			s := session("s1", "")
			s.LastEventAt = now.Add(-20 * time.Minute)
			agentOn(in, s)
		}, []string{"quiet: quiet"}},
		{"quiet: the session says so", func(in *Input) {
			s := session("s1", "")
			s.State = "quiet"
			agentOn(in, s)
		}, []string{"quiet: quiet"}},
		{"recent events aren't quiet", func(in *Input) {
			s := session("s1", "")
			s.LastEventAt = now.Add(-time.Minute)
			agentOn(in, s)
		}, nil},
		{"collision", func(in *Input) {
			agentOn(in, session("s1", ""))
			in.Links = []store.Link{{PlanID: 1, PkgID: "A1", Kind: store.LinkAgent, JobID: "j2", MatchMethod: store.MatchManual, Confidence: store.ConfidenceExact}}
			in.Sessions = append(in.Sessions, store.Session{JobID: "j2", Kind: "background", State: "working"})
		}, []string{"collision: collision: 2 live agents"}},
		{"a session and its own helper in one worktree aren't a collision", func(in *Input) {
			agentOn(in, session("s1", ""))
			in.Agents = []store.Agent{{SessionID: "s1", AgentID: "helper", Kind: "subagent", State: "running", WorktreePath: wtRoot + "a1"}}
		}, nil},
		{"low confidence", func(in *Input) {
			in.PRs = []store.PR{pr(1, "feature/a1-x", "")}
		}, []string{"low_confidence: low-confidence link: pr example-org/app#1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInput(t)
			tc.setup(&in)
			p := pkgOf(t, derive(in), "A1")
			var got []string
			for _, b := range p.Badges {
				got = append(got, string(b.Kind)+": "+b.Text)
			}
			deepEq(t, "badges", got, tc.want)
			if tc.name == "needs you, with since" && !p.Badges[0].Since.Equal(now.Add(-10*time.Minute)) {
				t.Errorf("since = %v", p.Badges[0].Since)
			}
		})
	}
}

func TestYouBadge(t *testing.T) {
	in := baseInput(t)
	ps := derive(in)
	r1 := pkgOf(t, ps, "R1")
	if r1.NeedsYou != "run the check" || len(r1.Badges) != 1 || r1.Badges[0].Text != "you: run the check" {
		t.Errorf("R1 = %+v", r1)
	}
	if y1 := pkgOf(t, ps, "Y1"); !y1.Yours {
		t.Error("Y1 isn't yours")
	}
	deepEq(t, "ready to dispatch (not Y1)", ps.ReadyToDispatch, []string{"A1", "B1", "B2", "B3b-2", "C1", "C2"})
	pkgByID(&in, "R1").DoneManual = true
	if r1 := pkgOf(t, derive(in), "R1"); len(r1.Badges) != 0 {
		t.Errorf("a done package keeps its you: badge: %+v", r1.Badges)
	}
}

// Two live agents in the single-writer Store lane, one each on B1 and B2;
// the Other lane isn't single-writer.
func TestLaneCollision(t *testing.T) {
	in := baseInput(t)
	in.Worktrees = append(in.Worktrees, worktree("b1", "aim/b1-x"), worktree("b2", "aim/b2-x"),
		worktree("c1", "aim/c1-x"), worktree("c2", "aim/c2-x"))
	for _, n := range []string{"b1", "b2", "c1", "c2"} {
		in.Sessions = append(in.Sessions, session("s-"+n, wtRoot+n))
	}
	ps := derive(in)
	for id, want := range map[string][]string{
		"B1": {"lane_conflict"}, "B2": {"lane_conflict"}, "C1": nil, "C2": nil,
	} {
		deepEq(t, id+" badges", badgeKinds(pkgOf(t, ps, id)), want)
	}
	deepEq(t, "alerts", ps.Alerts, []Alert{{Kind: AlertLaneCollision, Lane: "Store", Packages: []string{"B1", "B2"},
		Message: "Lane Store is single-writer, and 2 live agents are in it (B1, B2)"}})
	if b := pkgOf(t, ps, "B1").Badges[0]; b.Text != "lane Store is single-writer and has 2 live agents" {
		t.Errorf("badge text %q", b.Text)
	}

	// One of them finishing ends it.
	in.Sessions[0].EndedAt = now
	ps = derive(in)
	if len(ps.Alerts) != 0 || len(pkgOf(t, ps, "B2").Badges) != 0 {
		t.Errorf("after one ended: alerts %+v, B2 badges %+v", ps.Alerts, pkgOf(t, ps, "B2").Badges)
	}
}

func TestStalledAlert(t *testing.T) {
	in := baseInput(t)
	in.Worktrees = append(in.Worktrees, withCommits(seenSince(worktree("c1", "aim/c1-x"), now.Add(-60*time.Hour)), now.Add(-50*time.Hour)))
	ps := Derive(in, Match(in), Options{})[0]
	deepEq(t, "alerts", ps.Alerts, []Alert{{Kind: AlertStalled, Packages: []string{"C1"}, Since: now.Add(-50 * time.Hour),
		Message: "C1 is stalled: commits, no live agent and no PR for 2d"}})
	// A longer threshold (config state.stalled_after) clears it.
	ps = Derive(in, Match(in), Options{StalledAfter: 72 * time.Hour})[0]
	if len(ps.Alerts) != 0 {
		t.Errorf("alerts with a 72h threshold: %+v", ps.Alerts)
	}
}

func TestOffPlanAlert(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(in *Input)
		want  string // the alert's message, or ""
	}{
		{"agent on an unmatched branch", func(in *Input) {
			in.Worktrees = append(in.Worktrees, worktree("x", "feature/shiny"))
			in.Sessions = []store.Session{named(session("s1", wtRoot+"x"), "shiny tab")}
		}, "shiny tab is working in example-org/app on feature/shiny, which matches no package of aim"},
		{"agent on a matched branch", func(in *Input) {
			in.Worktrees = append(in.Worktrees, worktree("x", "aim/a1-x"))
			in.Sessions = []store.Session{session("s1", wtRoot+"x")}
		}, ""},
		{"spawn-linked agent on an engine branch", func(in *Input) {
			in.Worktrees = append(in.Worktrees, worktree("agent-ag1", "worktree-agent-ag1"))
			in.Sessions = []store.Session{session("lead", repoPath)}
			in.Agents = []store.Agent{{SessionID: "lead", AgentID: "ag1", Kind: "subagent", State: "running", WorktreePath: wtRoot + "agent-ag1"}}
			in.Links = []store.Link{{PlanID: 1, PkgID: "C2", Kind: store.LinkAgent, SessionID: "lead", AgentID: "ag1", MatchMethod: store.MatchSpawn, Confidence: store.ConfidenceExact}}
		}, ""},
		{"unlinked agent on an engine branch", func(in *Input) {
			in.Worktrees = append(in.Worktrees, worktree("agent-ag1", "worktree-agent-ag1"))
			in.Sessions = []store.Session{session("lead", repoPath)}
			in.Agents = []store.Agent{{SessionID: "lead", AgentID: "ag1", Kind: "subagent", AgentType: "general-purpose", State: "running", WorktreePath: wtRoot + "agent-ag1"}}
		}, "general-purpose is working in example-org/app on worktree-agent-ag1, which matches no package of aim"},
		{"the lead on main", func(in *Input) {
			s := session("lead", repoPath)
			s.Repo, s.Branch = repo, "main"
			in.Sessions = []store.Session{s}
		}, ""},
		{"a session known only by repo and branch", func(in *Input) {
			s := session("s1", "/elsewhere")
			s.WorktreePath, s.Repo, s.Branch = "", repo, "hotfix/thing"
			in.Sessions = []store.Session{s}
		}, "s1/ is working in example-org/app on hotfix/thing, which matches no package of aim"},
		{"an ended session", func(in *Input) {
			in.Worktrees = append(in.Worktrees, worktree("x", "feature/shiny"))
			s := session("s1", wtRoot+"x")
			s.EndedAt = now
			in.Sessions = []store.Session{s}
		}, ""},
		{"a headless run", func(in *Input) {
			in.Worktrees = append(in.Worktrees, worktree("x", "feature/shiny"))
			s := session("s1", wtRoot+"x")
			s.Headless = true
			in.Sessions = []store.Session{s}
		}, ""},
		{"another repo", func(in *Input) {
			in.Worktrees = append(in.Worktrees, store.Worktree{Path: "/work/web-x", MainRepo: "/work/web", Branch: "feature/shiny", FirstSeen: now})
			in.Sessions = []store.Session{session("s1", "/work/web-x")}
		}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInput(t)
			tc.setup(&in)
			ps := derive(in)
			var got string
			for _, a := range ps.Alerts {
				if a.Kind == AlertOffPlan {
					got = a.Message
				}
			}
			if got != tc.want {
				t.Errorf("off-plan alert = %q, want %q", got, tc.want)
			}
		})
	}
}

func named(s store.Session, name string) store.Session {
	s.Name = name
	return s
}

func TestUnlockedAlert(t *testing.T) {
	in := baseInput(t)
	in.PRs = []store.PR{merged(pr(1, "aim/a1-x", ""))}
	// A2 and R1 were blocked on A1 at the last derivation.
	for _, id := range []string{"A2", "R1"} {
		p := pkgByID(&in, id)
		p.Lifecycle, p.LifecyclePrev, p.LifecycleAt = "blocked", "", registered
	}
	ps := Derive(in, Match(in), Options{UnlockedSince: now.Add(-time.Hour)})[0]
	a2 := pkgOf(t, ps, "A2")
	if a2.Lifecycle != Ready || a2.PrevLifecycle != Blocked || !a2.Since.Equal(now) || !a2.Changed {
		t.Errorf("A2 = %s (prev %s since %v changed %v)", a2.Lifecycle, a2.PrevLifecycle, a2.Since, a2.Changed)
	}
	deepEq(t, "alerts", ps.Alerts, []Alert{{Kind: AlertUnlocked, Packages: []string{"A2", "R1"}, Since: now.Add(-time.Hour),
		Message: "Dependencies unlocked: A2, R1 ready to dispatch"}})
	// Packages seen ready on their first derivation weren't unlocked.
	if c1 := pkgOf(t, ps, "C1"); !c1.Changed || c1.PrevLifecycle != "" {
		t.Errorf("C1 = %+v", c1)
	}

	// Saved, and looked at again after you checked: nothing new.
	for _, id := range []string{"A2", "R1"} {
		p := pkgByID(&in, id)
		p.Lifecycle, p.LifecyclePrev, p.LifecycleAt = "ready", "blocked", now
	}
	in.Now = now.Add(time.Hour)
	ps = Derive(in, Match(in), Options{UnlockedSince: now.Add(time.Minute)})[0]
	if len(ps.Alerts) != 0 || pkgOf(t, ps, "A2").Changed {
		t.Errorf("after you checked: alerts %+v", ps.Alerts)
	}
	// With no "since", every package whose last change unlocked it is listed.
	ps = Derive(in, Match(in), Options{})[0]
	if len(ps.Alerts) != 1 || len(ps.Alerts[0].Packages) != 2 {
		t.Errorf("with no since: %+v", ps.Alerts)
	}
}

// TestReach covers links reached through the join graph (graph.go).
func TestReach(t *testing.T) {
	spawn := func(pkg, sid, aid string) store.Link {
		return store.Link{PlanID: 1, PkgID: pkg, Kind: store.LinkAgent, SessionID: sid, AgentID: aid, MatchMethod: store.MatchSpawn, Confidence: store.ConfidenceExact}
	}
	t.Run("a spawn link reaches the agent's worktree, branch and PR", func(t *testing.T) {
		in := baseInput(t)
		in.Worktrees = append(in.Worktrees, worktree("agent-ag1", "worktree-agent-ag1"))
		in.Sessions = []store.Session{session("lead", repoPath)}
		in.Agents = []store.Agent{{SessionID: "lead", AgentID: "ag1", Kind: "subagent", State: "running", WorktreePath: wtRoot + "agent-ag1"}}
		in.Links = []store.Link{spawn("C2", "lead", "ag1")}
		in.PRs = []store.PR{pr(9, "worktree-agent-ag1", "Opened with --fill")}
		c2 := pkgOf(t, derive(in), "C2")
		deepEq(t, "links", refs(c2), []string{
			"agent lead/ag1",
			"worktree " + wtRoot + "agent-ag1 < lead/ag1",
			"branch example-org/app:worktree-agent-ag1 < " + wtRoot + "agent-ag1",
			"pr example-org/app#9 < example-org/app:worktree-agent-ag1",
		})
		if c2.Lifecycle != InReview || c2.Links[3].Method != store.MatchSpawn || c2.Links[3].Confidence != store.ConfidenceExact {
			t.Errorf("C2 = %s, %+v", c2.Lifecycle, c2.Links[3])
		}
	})
	t.Run("without the mod, a locked worktree's agent is found through the lead's pid", func(t *testing.T) {
		in := baseInput(t)
		w := worktree("agent-ag1", "aim/c2-x")
		w.Locked, w.LockAgentID, w.LockPID, w.AgentID = true, "ag1", 4242, "ag1"
		in.Worktrees = append(in.Worktrees, w)
		lead := session("lead", repoPath)
		lead.PID = 4242
		in.Sessions = []store.Session{lead}
		c2 := pkgOf(t, derive(in), "C2")
		deepEq(t, "links", refs(c2), []string{
			"branch example-org/app:aim/c2-x",
			"worktree " + wtRoot + "agent-ag1 < example-org/app:aim/c2-x",
			"agent lead/ag1 < " + wtRoot + "agent-ag1",
		})
		if c2.Lifecycle != Working {
			t.Errorf("C2 = %s", c2.Lifecycle)
		}
		// The lock released (the agent finished): no longer working.
		in.Worktrees[1].Locked = false
		if c2 := pkgOf(t, derive(in), "C2"); c2.Lifecycle != Ready {
			t.Errorf("after the lock went: %s", c2.Lifecycle)
		}
	})
	t.Run("nothing crosses the main checkout", func(t *testing.T) {
		in := baseInput(t)
		in.Sessions = []store.Session{session("lead", repoPath)}
		in.Agents = []store.Agent{
			{SessionID: "lead", AgentID: "ag1", Kind: "subagent", State: "running", WorktreePath: repoPath},
			{SessionID: "lead", AgentID: "ag2", Kind: "subagent", State: "running", WorktreePath: repoPath},
		}
		in.Links = []store.Link{spawn("C1", "lead", "ag1")}
		ps := derive(in)
		deepEq(t, "C1 links", refs(pkgOf(t, ps, "C1")), []string{"agent lead/ag1"})
		if c1 := pkgOf(t, ps, "C1"); len(c1.Badges) != 0 {
			t.Errorf("C1 badges %+v", c1.Badges) // no collision with the lead or ag2
		}
	})
	t.Run("a target linked to another package stops the reach", func(t *testing.T) {
		in := baseInput(t)
		in.Worktrees = append(in.Worktrees, worktree("x", "aim/a1-x"))
		in.Sessions = []store.Session{session("s1", wtRoot+"x")}
		in.Links = []store.Link{{PlanID: 1, PkgID: "B1", Kind: store.LinkAgent, SessionID: "s1", MatchMethod: store.MatchManual, Confidence: store.ConfidenceExact}}
		ps := derive(in)
		deepEq(t, "A1 links", refs(pkgOf(t, ps, "A1")), []string{
			"branch example-org/app:aim/a1-x",
			"worktree " + wtRoot + "x < example-org/app:aim/a1-x",
		})
		deepEq(t, "B1 links", refs(pkgOf(t, ps, "B1")), []string{
			"agent s1/",
			"worktree " + wtRoot + "x < s1/",
		})
	})
	t.Run("an unlinked target isn't reached", func(t *testing.T) {
		in := baseInput(t)
		in.Worktrees = append(in.Worktrees, worktree("x", "aim/a1-x"))
		in.Sessions = []store.Session{session("s1", wtRoot+"x")}
		in.Tombstones = []store.Tombstone{{PlanID: 1, PkgID: "A1", Kind: store.LinkWorktree, Ref: wtRoot + "x"}}
		a1 := pkgOf(t, derive(in), "A1")
		deepEq(t, "A1 links", refs(a1), []string{"branch example-org/app:aim/a1-x"})
		if a1.Lifecycle != Ready {
			t.Errorf("A1 = %s", a1.Lifecycle)
		}
	})
	t.Run("a session in a subdirectory of a worktree", func(t *testing.T) {
		in := baseInput(t)
		in.Worktrees = append(in.Worktrees, worktree("x", "aim/a1-x"))
		s := session("s1", wtRoot+"x/internal/state")
		s.WorktreePath = ""
		in.Sessions = []store.Session{s}
		a1 := pkgOf(t, derive(in), "A1")
		if a1.Lifecycle != Working || len(a1.Links) != 3 {
			t.Errorf("A1 = %s, %q", a1.Lifecycle, refs(a1))
		}
	})
	t.Run("a background job joins its main loop's agent row", func(t *testing.T) {
		in := baseInput(t)
		in.Worktrees = append(in.Worktrees, worktree("x", "aim/a1-x"))
		job := store.Session{JobID: "j1", SessionID: "s9", Kind: "background", State: "working", WorktreePath: wtRoot + "x"}
		in.Sessions = []store.Session{job}
		in.Agents = []store.Agent{{SessionID: "s9", Kind: "lead", State: "waiting"}}
		a1 := pkgOf(t, derive(in), "A1")
		deepEq(t, "A1 links", refs(a1), []string{
			"branch example-org/app:aim/a1-x",
			"worktree " + wtRoot + "x < example-org/app:aim/a1-x",
			"agent job:j1 < " + wtRoot + "x",
		})
		deepEq(t, "badges", badgeKinds(a1), []string{"needs_you"})
		// Its turn done, the job isn't live.
		in.Sessions[0].State = "turn_done"
		in.Agents = nil
		if a1 := pkgOf(t, derive(in), "A1"); a1.Lifecycle != Ready {
			t.Errorf("after its turn: %s", a1.Lifecycle)
		}
	})
	t.Run("links carry their stored ids", func(t *testing.T) {
		in := baseInput(t)
		in.PRs = []store.PR{pr(1, "x", "(A1)")}
		in.Links = []store.Link{{ID: 77, PlanID: 1, PkgID: "A1", Kind: store.LinkPR, Ref: prRef(1), MatchMethod: store.MatchTitle, Confidence: store.ConfidenceHigh, Inferred: true}}
		a1 := pkgOf(t, derive(in), "A1")
		if a1.Links[0].ID != 77 || !a1.Links[0].Inferred || a1.Links[0].PR == nil || a1.Links[0].PR.Number != 1 {
			t.Errorf("link = %+v", a1.Links[0])
		}
	})
}

// The board carries the lanes table, and a linked agent its last action
// and start from the session row (or, for a subagent, its agent row), and
// its token count from its agent row: a subagent's own, a session's main
// loop's (none until the mod reports one).
func TestLanesAndLastAction(t *testing.T) {
	in := baseInput(t)
	in.Worktrees = append(in.Worktrees, worktree("a1", "aim/a1-x"), worktree("b1", "aim/b1-x"))
	in.Sessions = []store.Session{{SessionID: "s1", Kind: "interactive", State: "working", Cwd: wtRoot + "a1", WorktreePath: wtRoot + "a1",
		LastAction: "Bash: make check", LastEventAt: now.Add(-time.Minute), StartedAt: now.Add(-time.Hour)}}
	in.Agents = []store.Agent{{SessionID: "s1", AgentID: "b", Kind: "subagent", AgentType: "general-purpose", State: "running",
		WorktreePath: wtRoot + "b1", LastAction: "Edit: x.go", Tokens: 52_000, StartedAt: now.Add(-20 * time.Minute)}}
	ps := derive(in)

	var lanes []string
	for _, l := range ps.Lanes {
		lanes = append(lanes, fmt.Sprintf("%s %v %v", l.Name, l.Packages, l.SingleWriter))
	}
	deepEq(t, "lanes", lanes, []string{"Store [B1 B2] true", "Other [C1 C2] false"})

	agentOf := func(id string) *Agent {
		for _, l := range pkgOf(t, ps, id).Links {
			if l.Agent != nil {
				return l.Agent
			}
		}
		t.Fatalf("%s has no agent link", id)
		return nil
	}
	if a := agentOf("A1"); a.LastAction != "Bash: make check" || !a.LastEventAt.Equal(now.Add(-time.Minute)) ||
		!a.StartedAt.Equal(now.Add(-time.Hour)) || a.Tokens != 0 {
		t.Errorf("A1's agent = %+v", a)
	}
	if a := agentOf("B1"); a.LastAction != "Edit: x.go" || a.Name != "general-purpose" || a.Tokens != 52_000 || !a.StartedAt.Equal(now.Add(-20*time.Minute)) {
		t.Errorf("B1's agent = %+v", a)
	}

	// The session's main loop reports its count: A1's session link has it.
	in.Agents = append(in.Agents, store.Agent{SessionID: "s1", Kind: "lead", State: "running", LastAction: "Bash: make check", Tokens: 61_200, StartedAt: now.Add(-time.Hour)})
	ps = derive(in)
	if a := agentOf("A1"); a.Tokens != 61_200 || a.LastAction != "Bash: make check" || !a.StartedAt.Equal(now.Add(-time.Hour)) {
		t.Errorf("A1's agent with its main loop's count = %+v", a)
	}
	if a := agentOf("B1"); a.Tokens != 52_000 {
		t.Errorf("B1's agent took the main loop's count: %+v", a)
	}
}

// Parser warnings reach the plan's warnings.
func TestParserWarnings(t *testing.T) {
	const md = "# W\n<!-- warnings ok -->\n\n| Package | Needs |\n|---|---|\n| **A1 One** | Z9 |\n| **A2 Two** | — |\n"
	in := Input{Now: now, Plans: []Plan{mdPlan(t, 1, md, registered, repo)}}
	ps := derive(in)
	if len(ps.Warnings) != 1 || ps.Warnings[0].Kind != "unknown-id" || ps.Warnings[0].ID != "A1" || ps.Warnings[0].Line != 6 {
		t.Errorf("warnings = %+v", ps.Warnings)
	}
}
