package web

// Your steps: the rule for a package of a registered plan that waits on you
// to take its step. The plan page's "needs you" badge (planBadgesOf) and
// the Needs you inbox's "Your step" items (yourStepEpisodes) both ask
// yourStep, so the badge and the inbox can't disagree.

import (
	"strings"

	"github.com/gravexa-llc/ai-manager/internal/state"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// yourStep reports whether package p is your step and ready, and why, in
// the badge's words: "your step is ready", or "your step is ready: <its
// you: step>". It is when
//
//   - it is yours: its Owner cell says you (p.Yours), or its Needs cell
//     has a "you:" step (p.NeedsYou) that an agent's package waits on;
//   - it is ready: everything it needs is done, and nothing works on it
//     yet (no live agent, commits or open PR);
//   - and it is a package of the plan file. A provisional package has no
//     row, so it has no Owner or Needs cell to make it yours; its missing
//     row is the plan_row item's business.
//
// A package that is done (✅, Mark done, or a merged PR for an agent's
// package), blocked again, or being worked on is not.
func yourStep(p api.Package) (why string, ok bool) {
	if p.Provisional || p.Lifecycle != string(state.Ready) || (!p.Yours && p.NeedsYou == "") {
		return "", false
	}
	why = "your step is ready"
	if note := strings.TrimSpace(p.NeedsYou); note != "" {
		why += ": " + note
	}
	return why, true
}

// yourStepStopped says why package p isn't your step and ready now, for
// the history of an item whose episode ends. file is the plan file's name.
func yourStepStopped(p api.Package, file string) string {
	switch {
	case p.Provisional:
		return "its row isn't in " + file
	case p.DoneManual:
		return "it was marked done"
	case p.DoneInPlan:
		return "it is ✅ in " + file + " now"
	case p.Lifecycle == string(state.Done):
		return "it is done: its PR merged"
	case p.Lifecycle == string(state.Blocked):
		if len(p.BlockedBy) > 0 {
			return "it is blocked again: it needs " + strings.Join(p.BlockedBy, ", ")
		}
		return "it is blocked again"
	case p.Lifecycle == string(state.Working):
		return "work on it began"
	case p.Lifecycle == string(state.InReview):
		return "it has a PR in review"
	case p.Lifecycle == string(state.Dropped):
		return "it was dropped: its PRs closed"
	case !p.Yours && p.NeedsYou == "":
		return "it isn't your step any more"
	}
	return "it is not ready"
}
