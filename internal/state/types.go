package state

import (
	"time"

	"github.com/gravexa-llc/ai-manager/internal/plans"

	"github.com/gravexa-llc/ai-manager/internal/store"
)

// Input is one snapshot of everything the matcher and Derive read. Load
// fills it from the store; tests build it by hand. Pass history too
// (removed worktrees, ended sessions and agents, closed PRs): links are
// worked out from all of it, so a package keeps its links after its agent
// ends or its worktree is removed.
type Input struct {
	// Now is the snapshot's time: ages, stalled and quiet timers, and when a
	// changed lifecycle changed.
	Now time.Time

	Plans      []Plan
	PRs        []store.PR
	Worktrees  []store.Worktree
	Sessions   []store.Session
	Agents     []store.Agent
	Links      []store.Link // every plan's stored links
	Tombstones []store.Tombstone

	// RepoNames maps a main checkout's path (store.Worktree.MainRepo) to its
	// GitHub nameWithOwner. Worktrees only join PRs, and only match plans
	// registered by GitHub name, through it. A worktree whose repo is
	// missing here is named by its main checkout's path instead.
	RepoNames map[string]string

	// DefaultBranches maps a GitHub nameWithOwner (any case) to the repo's
	// default branch, as stored from GitHub (store.Repo). Only that branch
	// and the branches PRs target are a repo's base branches; a repo missing
	// here (not read from GitHub yet, or named by a path) falls back to
	// main, master, trunk and develop.
	DefaultBranches map[string]string
}

// Plan is a registered plan and its packages, in plan order.
//
// Plan.Repos lists the repos the plan is registered for, each as GitHub's
// nameWithOwner ("owner/repo") or a main checkout's absolute path. Package
// IDs are only looked up in plans registered for the repo in question.
// Plan.SourceMD is re-parsed for the lanes and the parser's warnings.
type Plan struct {
	store.Plan
	Packages []store.Package
}

// Target names what a link points at: a link kind and its ref (see
// store.Link.Ref for the formats).
type Target struct {
	Kind string
	Ref  string
}

// IsZero reports whether t names nothing.
func (t Target) IsZero() bool { return t.Kind == "" && t.Ref == "" }

func (t Target) String() string {
	if t.IsZero() {
		return ""
	}
	return t.Kind + " " + t.Ref
}

// Matches is the matcher's answer.
type Matches struct {
	// Links holds, by plan id, the links the matcher infers: PRs by their
	// Plan: line, title or head branch, and branches without a PR by their
	// name. Store them with store.ReplaceInferredLinks. Links reached
	// through these (a PR's branch and worktree, the agent in it) are
	// worked out by Derive and not stored.
	Links map[int64][]store.Link
	// Notes says why a PR or branch in a plan's repo matched no package
	// ("ambiguous: …"), for the Unmatched lane.
	Notes map[Target]string
}

// Lifecycle is where a package is on the board.
type Lifecycle string

// Lifecycles, in the order they're decided: the first that applies wins.
const (
	InReview Lifecycle = "in_review" // a linked PR is open
	Done     Lifecycle = "done"      // a linked PR merged and none is open, ✅ in the plan, or Mark done
	Working  Lifecycle = "working"   // a linked agent is alive, or a linked branch has commits
	Dropped  Lifecycle = "dropped"   // every linked PR was closed without merging
	Ready    Lifecycle = "ready"     // every package it needs is done
	Blocked  Lifecycle = "blocked"   // a package it needs isn't done (in a cycle too: the members it needs)
)

// Lifecycles lists every lifecycle in board order (Blocked … Done).
var Lifecycles = []Lifecycle{Blocked, Ready, Working, InReview, Done, Dropped}

// BadgeKind classifies a Badge.
type BadgeKind string

// Badge kinds, in the order a card shows them.
const (
	BadgeNeedsYou         BadgeKind = "needs_you"         // a linked live agent or session waits on you
	BadgeCIFailing        BadgeKind = "ci_failing"        // an open PR's checks failed
	BadgeConflict         BadgeKind = "conflict"          // an open PR has a merge conflict (DIRTY)
	BadgeWaitingOnYou     BadgeKind = "waiting_on_you"    // an open PR is green and mergeable (CLEAN)
	BadgeChangesRequested BadgeKind = "changes_requested" // reviewDecision CHANGES_REQUESTED
	BadgeReviewRequested  BadgeKind = "review_requested"  // reviewDecision REVIEW_REQUIRED
	BadgeDraft            BadgeKind = "draft"             // an open PR is a draft
	BadgeStalled          BadgeKind = "stalled"           // commits, no live agent, no PR, for a while
	BadgeQuiet            BadgeKind = "quiet"             // a linked agent is working but has gone quiet
	BadgeCollision        BadgeKind = "collision"         // two live agents on this package
	BadgeLaneConflict     BadgeKind = "lane_conflict"     // two live agents in its single-writer lane
	BadgeLowConfidence    BadgeKind = "low_confidence"    // a link was made from a branch-name token
	BadgeYou              BadgeKind = "you"               // the Needs cell's `you:` step
)

// Badge is something a card flags on top of its lifecycle.
type Badge struct {
	Kind BadgeKind
	// Text is what the card shows: "needs you: permission prompt",
	// "you: run the check", "CI failing on #12".
	Text string
	// Ref is what the badge is about, if anything: a PR, an agent, a link.
	Ref Target
	// Since is when it started, if known: how long an agent has waited on
	// you, or the last commit of a stalled branch.
	Since time.Time
}

// AlertKind classifies an Alert.
type AlertKind string

// Alert kinds.
const (
	AlertUnlocked      AlertKind = "unlocked"       // packages that became ready: dispatch the next wave
	AlertStalled       AlertKind = "stalled"        // a stalled package
	AlertLaneCollision AlertKind = "lane_collision" // two live agents in a single-writer lane
	AlertOffPlan       AlertKind = "off_plan"       // an agent in the plan's repo on a branch that matches no package
)

// Alert is something about a plan that needs your attention.
type Alert struct {
	Kind     AlertKind
	Packages []string // the packages concerned
	Lane     string   // lane_collision: the lane
	Ref      Target   // off_plan: the agent
	Message  string
	Since    time.Time
}

// Warning is a problem with the plan itself: one of the parser's warnings,
// or a dependency cycle.
type Warning struct {
	Kind    string // a plans.WarningKind, or WarnCycle
	Line    int    // 0 when unknown
	ID      string // the package concerned, if any
	Message string
}

// WarnCycle is the Warning kind for a dependency cycle.
const WarnCycle = "cycle"

// Agent is a session or subagent as the board shows it, put together from
// the sessions and agents tables (and, for a worktree-isolated subagent the
// mod hasn't reported, from its worktree's lock).
type Agent struct {
	Ref       Target // kind agent; ref store.AgentRef or store.JobRef
	SessionID string
	JobID     string
	AgentID   string
	// Kind is the session's kind (interactive, background) for a session,
	// or the agent row's kind (subagent, teammate) for a subagent.
	Kind  string
	Name  string
	State string
	// Live is true while it runs: the session hasn't ended, or the subagent
	// hasn't finished.
	Live bool
	// NeedsYou is true when it waits on you; NeedsYouSince is since when,
	// if known, and Reason why ("permission prompt").
	NeedsYou      bool
	NeedsYouSince time.Time
	Reason        string
	// LastAction is the session's (or subagent's) last action as the mod
	// reported it ("Bash: make check"); LastEventAt when its last event
	// arrived.
	LastAction   string
	LastEventAt  time.Time
	WorktreePath string
	// Repo and Branch are where a session is, as the session poller
	// resolved them. Empty for subagents.
	Repo   string
	Branch string
}

// Link is one link of a package as the board shows it: how it was made and
// what it points at.
type Link struct {
	Target
	Method     string // store.MatchSpawn … store.MatchManual
	Confidence string // store.ConfidenceExact, High or Low
	// ID is the stored link's id, or 0 for a link reached through another
	// one (Via) or not stored yet.
	ID int64
	// Inferred is true when the matcher made the link (or the one it was
	// reached through), false for a spawn or manual link.
	Inferred bool
	// Via is zero for a direct link. Otherwise the link was reached
	// through Via, another target linked to the package: a PR's head
	// branch, the worktree on that branch, the agent working in it. Method
	// and Confidence are those of the direct link it started from.
	Via Target

	// What the link points at, when the snapshot has it.
	PR       *store.PR
	Worktree *store.Worktree
	Agent    *Agent
}

// PackageState is one package on the board.
type PackageState struct {
	ID        string
	Title     string
	Repo      string
	Size      string
	Owner     string
	Lane      string
	Needs     []string
	NeedsNote string
	// NeedsYou is the Needs cell's `you:` step, shown as a BadgeYou.
	NeedsYou string
	// Yours is true for your own steps (Owner "you"): they go in the Yours
	// group and finish only by ✅ or Mark done.
	Yours bool
	// Provisional is true for a package the plan file's tables don't have
	// yet: work names it. It is on the board like any package,
	// but never ready to dispatch.
	Provisional bool

	Lifecycle Lifecycle
	// PrevLifecycle and Since are the lifecycle before this one and when
	// it changed. Changed is true when this derivation changed it (Run
	// saves those).
	PrevLifecycle Lifecycle
	Since         time.Time
	Changed       bool

	// BlockedBy lists the needed packages that aren't done, in Needs order.
	BlockedBy []string
	// Cycle is the dependency cycle the package is in, if any, as a path
	// that starts and ends with the same ID: ["A1", "B1", "A1"].
	Cycle []string

	Badges []Badge
	Links  []Link
}

// Unmatched is a PR or a branch in the plan's repos that matches no package
// of any plan, so it isn't lost.
type Unmatched struct {
	Target
	Repo   string
	Branch string
	Number int    // a PR's number
	Title  string // a PR's title
	State  string // a PR's state
	// Note says why it didn't match, when the matcher knows: "ambiguous: …",
	// "you unlinked it from …".
	Note string
}

// PlanState is a plan's board.
type PlanState struct {
	PlanID int64
	Slug   string
	Title  string

	Packages []PackageState // in plan order
	// Counts is how many packages are in each lifecycle.
	Counts map[Lifecycle]int
	// ReadyToDispatch lists the ready packages an agent can take (not
	// yours), in plan order.
	ReadyToDispatch []string
	// Lanes are the plan's lanes table rows, in file order (the board's
	// swimlanes; PackageState.Lane names one).
	Lanes []plans.Lane
	// Merges says who merges the plan's PRs: plans.MergesYou (the
	// default) or plans.MergesLead, from its Merges line.
	Merges string

	Unmatched []Unmatched
	Warnings  []Warning
	Alerts    []Alert
}

// Package returns the package with the given ID (any case).
func (p PlanState) Package(id string) (PackageState, bool) {
	k := canonID(id)
	for _, pkg := range p.Packages {
		if canonID(pkg.ID) == k {
			return pkg, true
		}
	}
	return PackageState{}, false
}
