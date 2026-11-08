package prwait

import (
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/store"
)

var t0 = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

// Open PRs in the states the rule tells apart.
var (
	ready     = store.PR{Repo: "o/app", Number: 1, HeadBranch: "feat-1", State: store.PROpen, Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN", ChecksState: "SUCCESS"}
	readyNoCI = store.PR{Repo: "o/app", Number: 2, HeadBranch: "feat-2", State: store.PROpen, Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN", ChecksState: "NONE"}
	draft     = store.PR{Repo: "o/app", Number: 3, HeadBranch: "feat-3", State: store.PROpen, Draft: true, MergeStateStatus: "DRAFT"}
	red       = store.PR{Repo: "o/app", Number: 4, HeadBranch: "feat-4", State: store.PROpen, Mergeable: "MERGEABLE", MergeStateStatus: "UNSTABLE", ChecksState: "FAILURE"}
	conflict  = store.PR{Repo: "o/app", Number: 5, HeadBranch: "feat-5", State: store.PROpen, Mergeable: "CONFLICTING", MergeStateStatus: "DIRTY"}
	merged    = store.PR{Repo: "o/app", Number: 6, HeadBranch: "feat-6", State: store.PRMerged, Mergeable: "UNKNOWN", MergeStateStatus: "UNKNOWN"}
	closed    = store.PR{Repo: "o/app", Number: 7, HeadBranch: "feat-7", State: store.PRClosed}
	unread    = store.PR{Repo: "o/app", Number: 8, State: store.PROpen, URL: "https://github.com/o/app/pull/8"}
)

func requested(p store.PR) store.PR {
	p.ReviewRequestedAt = t0
	return p
}

func TestDecide(t *testing.T) {
	onPlan := func(p store.PR, merges string) Input {
		return Input{PR: p, Monitored: true, Plan: "sharing", Pkg: "A2", Merges: merges}
	}
	for _, tc := range []struct {
		name string
		in   Input
		want Verdict
	}{
		{"ready, the plan merges: you", onPlan(ready, MergesYou),
			Verdict{NeedsYou: ReadyToMerge, WaitingOn: WaitingYou, Merges: MergesYou, Reason: "ready to merge · this plan merges: you"}},
		{"ready with no CI counts as ready", onPlan(readyNoCI, MergesYou),
			Verdict{NeedsYou: ReadyToMerge, WaitingOn: WaitingYou, Merges: MergesYou, Reason: "ready to merge · this plan merges: you"}},
		{"no Merges value reads as you", onPlan(ready, ""),
			Verdict{NeedsYou: ReadyToMerge, WaitingOn: WaitingYou, Merges: MergesYou, Reason: "ready to merge · this plan merges: you"}},
		{"ready, the lead merges", onPlan(ready, MergesLead),
			Verdict{WaitingOn: WaitingLead, Merges: MergesLead, Reason: "ready to merge · this plan's lead merges it"}},
		{"Merges any case", onPlan(ready, "Lead"),
			Verdict{WaitingOn: WaitingLead, Merges: MergesLead, Reason: "ready to merge · this plan's lead merges it"}},
		{"review requested, the plan merges: you", onPlan(requested(red), MergesYou),
			Verdict{NeedsYou: ReviewRequested, WaitingOn: WaitingYou, Merges: MergesYou, Reason: "your review is requested"}},
		{"review requested on a lead-merged plan still needs you", onPlan(requested(ready), MergesLead),
			Verdict{NeedsYou: ReviewRequested, WaitingOn: WaitingYou, Merges: MergesLead, Reason: "your review is requested"}},
		{"review requested while an agent works", Input{PR: requested(ready), Monitored: true, Plan: "sharing", Merges: MergesLead, AgentWorking: true},
			Verdict{NeedsYou: ReviewRequested, WaitingOn: WaitingYou, Merges: MergesLead, Reason: "your review is requested"}},
		{"review requested on a draft", onPlan(requested(draft), MergesYou),
			Verdict{NeedsYou: ReviewRequested, WaitingOn: WaitingYou, Merges: MergesYou, Reason: "your review is requested"}},
		{"ready, an agent still works on it", Input{PR: ready, Monitored: true, Plan: "sharing", Merges: MergesYou, AgentWorking: true},
			Verdict{WaitingOn: WaitingAgent, Merges: MergesYou, Reason: "an agent is still working on it"}},
		{"CI red, an agent works on it", Input{PR: red, Monitored: true, Plan: "sharing", Merges: MergesYou, AgentWorking: true},
			Verdict{WaitingOn: WaitingAgent, Merges: MergesYou, Reason: "CI failing · an agent is still working on it"}},
		{"CI red, no agent: the plan's lead", onPlan(red, MergesYou),
			Verdict{WaitingOn: WaitingLead, Merges: MergesYou, Reason: "CI failing · no agent is working on it, so it's for this plan's lead"}},
		{"conflict, no agent: the plan's lead", onPlan(conflict, MergesLead),
			Verdict{WaitingOn: WaitingLead, Merges: MergesLead, Reason: "merge conflict · no agent is working on it, so it's for this plan's lead"}},
		{"draft, no agent: the plan's lead", onPlan(draft, MergesYou),
			Verdict{WaitingOn: WaitingLead, Merges: MergesYou, Reason: "draft · no agent is working on it, so it's for this plan's lead"}},
		{"draft, an agent works on it", Input{PR: draft, Monitored: true, Plan: "sharing", AgentWorking: true},
			Verdict{WaitingOn: WaitingAgent, Merges: MergesYou, Reason: "draft · an agent is still working on it"}},
		{"a monitored session's branch on no plan, ready: you merge", Input{PR: ready, Monitored: true},
			Verdict{NeedsYou: ReadyToMerge, WaitingOn: WaitingYou, Merges: MergesYou, Reason: "ready to merge · on no plan, so you merge it"}},
		{"a monitored session's branch on no plan, CI red", Input{PR: red, Monitored: true},
			Verdict{WaitingOn: WaitingNone, Merges: MergesYou, Reason: "CI failing · no agent is working on it"}},
		{"not on a plan: ready", Input{PR: ready},
			Verdict{WaitingOn: WaitingNone, Reason: "not on a registered plan or a monitored session's branch"}},
		// Someone asked you by name: a review request needs you on any PR,
		// even one that doesn't count (another person's project).
		{"not on a plan: review requested still needs you", Input{PR: requested(ready)},
			Verdict{NeedsYou: ReviewRequested, WaitingOn: WaitingYou, Reason: "your review is requested"}},
		{"not on a plan: review requested on a draft", Input{PR: requested(draft)},
			Verdict{NeedsYou: ReviewRequested, WaitingOn: WaitingYou, Reason: "your review is requested"}},
		{"merged", onPlan(requested(merged), MergesYou),
			Verdict{WaitingOn: WaitingNone, Merges: MergesYou, Reason: "merged"}},
		{"closed", onPlan(closed, MergesLead),
			Verdict{WaitingOn: WaitingNone, Merges: MergesLead, Reason: "closed without merging"}},
		{"merged, not on a plan", Input{PR: merged},
			Verdict{WaitingOn: WaitingNone, Reason: "merged"}},
		// A PR the link API stored by URL, never read from GitHub: its
		// OPEN is a guess, so it waits on nobody, agent or lead.
		{"not read yet, on a plan", onPlan(unread, MergesYou),
			Verdict{WaitingOn: WaitingNone, Merges: MergesYou, Reason: NotReadYet}},
		{"not read yet, an agent works on it", Input{PR: unread, Monitored: true, Plan: "sharing", Merges: MergesLead, AgentWorking: true},
			Verdict{WaitingOn: WaitingNone, Merges: MergesLead, Reason: NotReadYet}},
		{"not read yet, not on a plan", Input{PR: unread},
			Verdict{WaitingOn: WaitingNone, Reason: NotReadYet}},
	} {
		want := tc.want
		want.Monitored, want.Plan, want.Pkg = tc.in.Monitored, tc.in.Plan, tc.in.Pkg
		got := Decide(tc.in)
		if got != want {
			t.Errorf("%s:\n got %+v\nwant %+v", tc.name, got, want)
		}
		if (got.WaitingOn == WaitingYou) != (got.NeedsYou != "") {
			t.Errorf("%s: waiting on %q with needs_you %q", tc.name, got.WaitingOn, got.NeedsYou)
		}
		if got.NeedsYou != "" && got.Label() == "" || got.NeedsYou == "" && got.Label() != "" {
			t.Errorf("%s: Label() = %q for needs_you %q", tc.name, got.Label(), got.NeedsYou)
		}
	}
}

// IsReadyToMerge is the plain rule for "ready to merge", before what a PR
// waits on is worked out: open, not a draft, no red CI, no conflict, no
// review rule in the way, and CLEAN.
func TestReadyToMergeAndBlocker(t *testing.T) {
	for _, tc := range []struct {
		pr      store.PR
		ready   bool
		blocker string
	}{
		{ready, true, ""},
		{readyNoCI, true, ""},
		{store.PR{State: "OPEN", MergeStateStatus: "CLEAN"}, true, ""}, // CI "" (no rollup) is fine
		{draft, false, "draft"},
		{store.PR{State: "OPEN", Draft: true, MergeStateStatus: "CLEAN"}, false, "draft"},
		{red, false, "CI failing"},
		{store.PR{State: "OPEN", ChecksState: "ERROR", MergeStateStatus: "CLEAN"}, false, "CI failing"},
		{conflict, false, "merge conflict"},
		{store.PR{State: "OPEN", Mergeable: "CONFLICTING", MergeStateStatus: "UNKNOWN"}, false, "merge conflict"},
		{store.PR{State: "OPEN", ReviewDecision: "CHANGES_REQUESTED", MergeStateStatus: "BLOCKED"}, false, "changes requested"},
		{store.PR{State: "OPEN", ReviewDecision: "REVIEW_REQUIRED", MergeStateStatus: "BLOCKED"}, false, "review required"},
		{store.PR{State: "OPEN", ChecksState: "PENDING", MergeStateStatus: "BLOCKED"}, false, "CI running"},
		{store.PR{State: "OPEN", MergeStateStatus: "UNSTABLE"}, false, "some checks failing"},
		{store.PR{State: "OPEN", MergeStateStatus: "BEHIND"}, false, "behind its base branch"},
		{store.PR{State: "OPEN", MergeStateStatus: "BLOCKED"}, false, "blocked by a branch rule"},
		{store.PR{State: "OPEN", MergeStateStatus: "UNKNOWN", Mergeable: "UNKNOWN"}, false, "mergeability not known yet"},
		{store.PR{State: "OPEN"}, false, "mergeability not known yet"},
		{store.PR{State: "OPEN", MergeStateStatus: "HAS_HOOKS"}, false, "not mergeable yet (has_hooks)"},
		{merged, false, "mergeability not known yet"},
		{store.PR{State: "MERGED", MergeStateStatus: "CLEAN"}, false, ""},
	} {
		if got := IsReadyToMerge(tc.pr); got != tc.ready {
			t.Errorf("IsReadyToMerge(%+v) = %v, want %v", tc.pr, got, tc.ready)
		}
		if got := Blocker(tc.pr); got != tc.blocker {
			t.Errorf("Blocker(%+v) = %q, want %q", tc.pr, got, tc.blocker)
		}
	}
}
