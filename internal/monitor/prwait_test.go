package monitor

import (
	"testing"

	"github.com/gravexa-llc/ai-manager/internal/prwait"
	"github.com/gravexa-llc/ai-manager/internal/store"
)

// PRWait gives internal/prwait the PR's plan, that plan's Merges line and
// whether an agent still works on it, in the fixture world of
// monitor_test.go.
func TestPRWait(t *testing.T) {
	clean := func(n int, branch string) store.PR {
		return store.PR{Repo: "o/app", Number: n, HeadBranch: branch, State: store.PROpen, Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN"}
	}
	type want struct{ needsYou, waitingOn, merges, plan, pkg string }
	check := func(what string, set *Set, p store.PR, w want) {
		t.Helper()
		v := set.PRWait(p)
		if got := (want{v.NeedsYou, v.WaitingOn, v.Merges, v.Plan, v.Pkg}); got != w {
			t.Errorf("%s: got %+v (%q), want %+v", what, got, v.Reason, w)
		}
		if v.Monitored != set.PR(p) {
			t.Errorf("%s: Monitored %v, PR() %v", what, v.Monitored, set.PR(p))
		}
	}

	in := input()
	in.Plans[0].Merges = prwait.MergesLead // aim: the lead merges
	in.Plans[1].Merges = prwait.MergesYou
	in.Sessions[6].Repo, in.Sessions[6].Branch = repo, "loose-branch"
	set := Compute(in)
	// #7 is linked to aim · T1, whose agent (pid:3) is live.
	check("linked, its agent live", set, clean(7, "x"), want{"", "agent", "lead", "aim", "T1"})
	check("linked, review requested on a lead-merged plan", set, store.PR{Repo: "o/app", Number: 7, State: store.PROpen, ReviewRequestedAt: t0},
		want{"review_requested", "you", "lead", "aim", "T1"})
	// #8 is on pid:3's branch (aim · T1's agent): on aim, through the
	// session; pid:3 isn't working (no state).
	check("a plan agent's branch", set, clean(8, "aim/t1-thing"), want{"", "lead", "lead", "aim", ""})
	// #9 is on the lineage agent's branch: its lead's plan.
	check("the lineage agent's branch", set, clean(9, "worktree-agent-a1"), want{"", "lead", "lead", "aim", ""})
	// Not monitored: a loose session's branch, nobody's, a fork.
	check("an unmonitored session's branch", set, clean(12, "loose-branch"), want{"", "none", "", "", ""})
	// A review request needs you on any PR, counted or not: someone asked
	// you by name (another person's project, say).
	check("nobody's, review requested", set, store.PR{Repo: "o/app", Number: 13, State: store.PROpen, ReviewRequestedAt: t0},
		want{"review_requested", "you", "", "", ""})
	check("from a fork", set, store.PR{Repo: "o/app", Number: 10, HeadBranch: "aim/t1-thing", IsCrossRepository: true, State: store.PROpen},
		want{"", "none", "", "", ""})

	// The T1 agent finishes its turn; aim has no Merges value.
	in = input()
	in.Plans[0].Packages[0].Links[2].Agent.State = "turn_done"
	set = Compute(in)
	check("linked, the plan merges: you", set, clean(7, "x"), want{"ready_to_merge", "you", "you", "aim", "T1"})
	check("linked, red CI, no agent", set, store.PR{Repo: "o/app", Number: 7, State: store.PROpen, ChecksState: "FAILURE"},
		want{"", "lead", "you", "aim", "T1"})
	// A live session works on #8's branch, then ends.
	in.Sessions[2].State = "working"
	check("its branch's session works", Compute(in), clean(8, "aim/t1-thing"), want{"", "agent", "you", "aim", ""})
	in.Sessions[2].State = "needs_you"
	check("its branch's session waits on you mid-turn", Compute(in), clean(8, "aim/t1-thing"), want{"", "agent", "you", "aim", ""})
	in.Sessions[2].State = "turn_done"
	check("its branch's session finished its turn", Compute(in), clean(8, "aim/t1-thing"), want{"ready_to_merge", "you", "you", "aim", ""})
	in.Sessions[2].State, in.Sessions[2].EndedAt = "working", t0
	check("its branch's session ended", Compute(in), clean(8, "aim/t1-thing"), want{"ready_to_merge", "you", "you", "aim", ""})

	// A watched session's branch, on no plan: you merge.
	in = input()
	in.Sessions[1].Repo, in.Sessions[1].Branch, in.Sessions[1].State = repo, "notes-fix", "idle"
	check("a watched session's branch, no plan", Compute(in), clean(14, "notes-fix"), want{"ready_to_merge", "you", "you", "", ""})

	// "all" mode and a nil set count every PR.
	in = input()
	in.Mode = ModeAll
	check("all mode, nobody's", Compute(in), clean(13, "old"), want{"ready_to_merge", "you", "you", "", ""})
	check("nil set", nil, clean(13, "old"), want{"ready_to_merge", "you", "you", "", ""})
}
