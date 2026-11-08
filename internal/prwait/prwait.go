// Package prwait decides what a pull request waits on: whether
// it needs you, and if not, who has the next move. It is the one rule GET
// /api/prs, GET /api/status and the dashboard's Needs you and Pull
// requests pages share. internal/monitor gathers its inputs (Set.PRWait): whether
// the PR counts at all, the plan it is on and who merges that plan's PRs,
// and whether an agent still works on it.
//
// # The rule
//
// A PR needs you only when it waits on you:
//
//   - review_requested: your review is requested on it (a review request to
//     you or a team you're in, from GitHub's review-requested:@me search).
//     This holds on any open PR, whether or not it counts (below): on
//     another person's project, a draft or a lead-merged plan's PR alike,
//     because someone asked you by name (the user's decision, 2026-11-06);
//   - ready_to_merge: it is ready to merge (ReadyToMerge) and you merge it:
//     its plan's Merges line says "you", or it is on no plan (a monitored
//     session's branch), so nobody else would.
//
// Apart from review requests, only an open PR that counts
// (Input.Monitored: on a registered plan's board, or on a monitored
// session's branch) can need you. Otherwise, WaitingOn says who has the
// next move:
//
//   - agent: a live agent still works on its package or branch;
//   - lead: it is on a plan, and the next move is the plan's lead session's:
//     merging it on a plan that says "Merges: lead", or, when no agent works
//     on it, sending it back to one (red CI, a conflict, changes requested,
//     a draft, …);
//   - none: nobody's, as far as aimd can tell: it is merged or closed, it
//     doesn't count, or it isn't ready, no agent works on it and no plan
//     has a lead for it.
//
// A PR aimd has never read from GitHub (store.PR.Unread: stored by the
// link API with only its URL) waits on nobody and never needs you; its
// Reason is NotReadYet. Its stored state, OPEN, is only a guess, so it
// isn't shown as an open PR that waits on an agent or the lead.
//
// You are WaitingOn exactly when NeedsYou is set.
package prwait

import (
	"strings"

	"github.com/gravexa-llc/ai-manager/internal/plans"
	"github.com/gravexa-llc/ai-manager/internal/store"
)

// NeedsYou values: why a PR is in Needs you. "" means it isn't.
const (
	ReviewRequested = "review_requested"
	ReadyToMerge    = "ready_to_merge"
)

// NotReadYet is the Reason of a PR aimd has never read from GitHub.
const NotReadYet = "not read from GitHub yet"

// WaitingOn values: who has the next move.
const (
	WaitingYou   = "you"
	WaitingLead  = "lead"
	WaitingAgent = "agent"
	WaitingNone  = "none"
)

// Merges values, as the plan's Merges line says them (internal/plans).
const (
	MergesYou  = plans.MergesYou
	MergesLead = plans.MergesLead
)

// Input is what Decide decides from.
type Input struct {
	PR store.PR
	// Monitored is true when the PR counts for Needs you (internal/monitor
	// Set.PR): a registered plan's board links it, or it is a same-repo PR
	// on the branch a monitored session works on.
	Monitored bool
	// Plan and Pkg are the registered plan (and package) the PR is on, if
	// any: the board that links it, or else the plan of the monitored
	// session on its branch. Pkg may be empty.
	Plan, Pkg string
	// Merges is Plan's Merges value, MergesYou or MergesLead. Empty reads
	// as MergesYou.
	Merges string
	// AgentWorking is true when a live agent still works on the PR's
	// package or branch: it runs, and hasn't finished its turn.
	AgentWorking bool
}

// Verdict is what a PR waits on.
type Verdict struct {
	// NeedsYou is ReviewRequested, ReadyToMerge or "".
	NeedsYou string
	// WaitingOn is WaitingYou, WaitingLead, WaitingAgent or WaitingNone.
	WaitingOn string
	// Merges is who merges the PR, MergesYou or MergesLead: its plan's
	// Merges line, or MergesYou for a monitored session's branch on no
	// plan. It is "" for a PR that doesn't count.
	Merges string
	// Reason says it in one short line for people: "ready to merge · this
	// plan merges: you", "ready to merge · this plan's lead merges it",
	// "CI failing · an agent is still working on it".
	Reason string
	// Monitored, Plan and Pkg are the Input's, for the PR's page.
	Monitored bool
	Plan, Pkg string
}

// Label is NeedsYou in a few words, for a tag: "review requested" or
// "ready to merge"; "" when the PR doesn't need you.
func (v Verdict) Label() string {
	switch v.NeedsYou {
	case ReviewRequested:
		return "review requested"
	case ReadyToMerge:
		return "ready to merge"
	}
	return ""
}

// Decide applies the rule in the package comment.
func Decide(in Input) Verdict {
	p := in.PR
	v := Verdict{Monitored: in.Monitored, Plan: in.Plan, Pkg: in.Pkg, WaitingOn: WaitingNone}
	if in.Monitored {
		v.Merges = MergesYou
		if strings.EqualFold(in.Merges, MergesLead) {
			v.Merges = MergesLead
		}
	}
	switch {
	case p.State == store.PRMerged:
		v.Reason = "merged"
		return v
	case p.State != store.PROpen:
		v.Reason = "closed without merging"
		return v
	case p.Unread():
		// Only its URL is known: open is a guess, and nobody's move can be told.
		v.Reason = NotReadYet
		return v
	case p.ReviewRequested():
		// Before the Monitored check: a review request counts on any PR.
		v.NeedsYou, v.WaitingOn, v.Reason = ReviewRequested, WaitingYou, "your review is requested"
		return v
	case !in.Monitored:
		v.Reason = "not on a registered plan or a monitored session's branch"
		return v
	}

	blocker := Blocker(p)
	switch {
	case in.AgentWorking:
		v.WaitingOn = WaitingAgent
		v.Reason = "an agent is still working on it"
		if blocker != "" {
			v.Reason = blocker + " · " + v.Reason
		}
	case blocker == "" && v.Merges == MergesLead:
		v.WaitingOn, v.Reason = WaitingLead, "ready to merge · this plan's lead merges it"
	case blocker == "" && in.Plan != "":
		v.NeedsYou, v.WaitingOn, v.Reason = ReadyToMerge, WaitingYou, "ready to merge · this plan merges: you"
	case blocker == "":
		v.NeedsYou, v.WaitingOn, v.Reason = ReadyToMerge, WaitingYou, "ready to merge · on no plan, so you merge it"
	case in.Plan != "":
		v.WaitingOn, v.Reason = WaitingLead, blocker+" · no agent is working on it, so it's for this plan's lead"
	default:
		v.Reason = blocker + " · no agent is working on it"
	}
	return v
}

// IsReadyToMerge reports whether an open PR is ready to merge: not a
// draft, no red CI, no conflict, no changes requested or review required,
// and GitHub's mergeStateStatus is CLEAN (mergeable, with checks passing or
// none at all).
func IsReadyToMerge(p store.PR) bool {
	return p.State == store.PROpen && Blocker(p) == ""
}

// Blocker says in a few words why an open PR isn't ready to merge ("CI
// failing", "merge conflict", "draft", …), or "" when it is.
func Blocker(p store.PR) string {
	switch {
	case p.Draft:
		return "draft"
	case p.ChecksState == "FAILURE" || p.ChecksState == "ERROR":
		return "CI failing"
	case p.MergeStateStatus == "DIRTY" || p.Mergeable == "CONFLICTING":
		return "merge conflict"
	case p.ReviewDecision == "CHANGES_REQUESTED":
		return "changes requested"
	case p.ReviewDecision == "REVIEW_REQUIRED":
		return "review required"
	case p.MergeStateStatus == "CLEAN":
		return ""
	case p.ChecksState == "PENDING" || p.ChecksState == "EXPECTED":
		return "CI running"
	}
	switch p.MergeStateStatus {
	case "UNSTABLE":
		return "some checks failing"
	case "BEHIND":
		return "behind its base branch"
	case "BLOCKED":
		return "blocked by a branch rule"
	case "DRAFT":
		return "draft"
	case "", "UNKNOWN":
		return "mergeability not known yet"
	}
	return "not mergeable yet (" + strings.ToLower(p.MergeStateStatus) + ")"
}
