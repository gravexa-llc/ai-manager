package api

import "time"

// The plan API's JSON shapes (internal/planapi; docs/api.md is the
// contract). Requests
// are JSON bodies; responses follow the conventions in this package's
// doc. Lifecycles, badge, alert and link kinds, match methods and
// confidences are internal/state's and internal/store's values.

// TopicPlans is the /api/stream topic sent when a plan's derived state
// changes: a board, a summary or a band may read differently.
const TopicPlans = "plans"

// PlanList is the body of GET /api/plans: every registered plan's board,
// in the order they were registered.
type PlanList struct {
	Plans []Plan `json:"plans"`
}

// Plan is one plan's board: the body of GET /api/plans/{slug}.
type Plan struct {
	Slug  string `json:"slug"`
	Title string `json:"title"`
	// Path is the plan file, symlinks resolved.
	Path string `json:"path"`
	// Repos are the repos the plan is registered for: GitHub
	// nameWithOwner, or a main checkout's path for a repo with no GitHub
	// remote.
	Repos               []string  `json:"repos"`
	RegisteredInSession string    `json:"registered_in_session,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
	// Merges says who merges the plan's PRs, from its Merges line: "you"
	// (the default) or "lead" (the lead session merges its agents' PRs,
	// so a ready PR waits on the lead, not on you).
	Merges string `json:"merges"`

	// Band is the one-line summary the mod shows above the prompt: the
	// summary's band, its need-you count read when the board is.
	Band string `json:"band"`
	// Total counts the packages; Counts has every lifecycle, zeros
	// included. Both count the provisional packages, which
	// Provisional counts alone.
	Total       int            `json:"total"`
	Counts      map[string]int `json:"counts"`
	Provisional int            `json:"provisional"`
	// ReadyToDispatch lists the ready packages an agent can take (not
	// yours), in plan order.
	ReadyToDispatch []string `json:"ready_to_dispatch"`

	Packages []Package `json:"packages"`
	// Lanes are the plan's lanes table, in file order: the board's
	// swimlanes. A package's Lane names one.
	Lanes     []Lane      `json:"lanes"`
	Unmatched []Unmatched `json:"unmatched"`
	Warnings  []Warning   `json:"warnings"`
	Alerts    []Alert     `json:"alerts"`

	// DerivedAt is when the state engine last ran.
	DerivedAt time.Time `json:"derived_at"`
}

// Package is one package on the board.
type Package struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Repo, Size, Owner and Lane are the plan's cells as written.
	Repo  string `json:"repo,omitempty"`
	Size  string `json:"size,omitempty"`
	Owner string `json:"owner,omitempty"`
	Lane  string `json:"lane,omitempty"`
	// Needs are the IDs it depends on; NeedsNote is the rest of the cell,
	// and NeedsYou its `you:` step.
	Needs     []string `json:"needs"`
	NeedsNote string   `json:"needs_note,omitempty"`
	NeedsYou  string   `json:"needs_you,omitempty"`
	// Yours is true for your own steps (Owner "you").
	Yours bool `json:"yours,omitempty"`
	// DoneInPlan is a ✅ in the plan; DoneManual is Mark done.
	DoneInPlan bool `json:"done_in_plan,omitempty"`
	DoneManual bool `json:"done_manual,omitempty"`

	// Lifecycle is blocked, ready, working, in_review, done or dropped;
	// PrevLifecycle the one before it and Since when it changed.
	Lifecycle     string    `json:"lifecycle"`
	PrevLifecycle string    `json:"prev_lifecycle,omitempty"`
	Since         time.Time `json:"since,omitzero"`
	// BlockedBy lists the needed packages that aren't done; Cycle the
	// dependency cycle it's in, as a path ["A1", "B1", "A1"].
	BlockedBy []string `json:"blocked_by,omitempty"`
	Cycle     []string `json:"cycle,omitempty"`

	Badges []Badge `json:"badges"`
	Links  []Link  `json:"links"`

	// The convention block for an agent working on it.
	Convention

	// Provisional is true for a package the plan file's tables don't have
	// yet: a spawn brief's Package: line, a PR's Plan: line
	// or a "<slug>/<id>-…" branch names it. It is on the board until the
	// file gains its row, and never ready to dispatch. The other fields
	// below are set only on a provisional package.
	Provisional bool `json:"provisional,omitempty"`
	// SuggestedRow is the row to paste into the plan file, in the format
	// of its last package table: "| **<ID> <title>** |  |  |  |", blank
	// where aimd can't know, with a ✅ when the work is done.
	SuggestedRow string `json:"suggested_row,omitempty"`
	// FirstSeen is when aimd first saw its work; Sources what named it.
	FirstSeen time.Time           `json:"first_seen,omitzero"`
	Sources   []ProvisionalSource `json:"sources,omitempty"`
}

// ProvisionalSource is one thing that named a provisional package.
type ProvisionalSource struct {
	// Kind is spawn (a spawn brief's Package: line; Ref is the agent,
	// "session_id/agent_id", or a remote agent's session URL), branch
	// (Ref "owner/repo:branch") or pr (Ref "owner/repo#123").
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
	// Title is the title it gave: the spawn's description, the PR's
	// title, the branch's words.
	Title     string    `json:"title,omitempty"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

// Lane is one row of a plan's lanes table.
type Lane struct {
	Name string `json:"name"`
	// Packages are the IDs the row lists, in order.
	Packages []string `json:"packages"`
	// Rule is the row's other cells as written; SingleWriter is true when
	// it says at most one agent at a time works in the lane.
	Rule         string `json:"rule,omitempty"`
	SingleWriter bool   `json:"single_writer,omitempty"`
}

// Convention is what the mod adds to an agent's brief for a package: the
// suggested branch and the PR body's Plan: line.
type Convention struct {
	// SuggestedBranch is "<slug>/<id>-<short-name>", all lower case.
	SuggestedBranch string `json:"suggested_branch"`
	// PlanLine is "Plan: <slug> · <ID>".
	PlanLine string `json:"plan_line"`
}

// Target names what a link points at.
type Target struct {
	// Kind is agent, branch, pr, worktree or cloud_session.
	Kind string `json:"kind"`
	// Ref is "owner/repo#123" (pr), "owner/repo:branch" (branch), a path
	// (worktree), "session_id/agent_id" or "job:<id>" (agent), or a URL
	// (cloud_session).
	Ref string `json:"ref"`
}

// Badge is something a card flags on top of its lifecycle.
type Badge struct {
	Kind  string    `json:"kind"`
	Text  string    `json:"text"`
	Ref   *Target   `json:"ref,omitempty"`
	Since time.Time `json:"since,omitzero"`
}

// Link is one of a package's links and how it was made.
type Link struct {
	Target
	// Method is spawn, plan_line, title, branch or manual; Confidence is
	// exact, high or low.
	Method     string `json:"method"`
	Confidence string `json:"confidence"`
	// ID is the stored link's id; 0 for a link reached through another.
	ID int64 `json:"id,omitempty"`
	// Inferred is true when the matcher made the link (or the one it was
	// reached through).
	Inferred bool `json:"inferred"`
	// Via is set for a link reached through another target linked to the
	// package (a PR's branch, the worktree on it, the agent in it).
	Via *Target `json:"via,omitempty"`

	// What it points at, when aimd knows it.
	PR       *LinkPR       `json:"pr,omitempty"`
	Worktree *LinkWorktree `json:"worktree,omitempty"`
	Agent    *LinkAgent    `json:"agent,omitempty"`
}

// LinkPR is a linked pull request.
type LinkPR struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Title  string `json:"title,omitempty"`
	URL    string `json:"url,omitempty"`
	// State is OPEN, MERGED or CLOSED.
	State            string    `json:"state"`
	Draft            bool      `json:"draft,omitempty"`
	HeadBranch       string    `json:"head_branch,omitempty"`
	ChecksState      string    `json:"checks_state,omitempty"`
	Mergeable        string    `json:"mergeable,omitempty"`
	MergeStateStatus string    `json:"merge_state_status,omitempty"`
	ReviewDecision   string    `json:"review_decision,omitempty"`
	MergedAt         time.Time `json:"merged_at,omitzero"`
	ClosedAt         time.Time `json:"closed_at,omitzero"`
}

// LinkWorktree is a linked worktree.
type LinkWorktree struct {
	Path         string    `json:"path"`
	MainRepo     string    `json:"main_repo"`
	Branch       string    `json:"branch,omitempty"`
	Detached     bool      `json:"detached,omitempty"`
	Locked       bool      `json:"locked,omitempty"`
	LastCommitAt time.Time `json:"last_commit_at,omitzero"`
	RemovedAt    time.Time `json:"removed_at,omitzero"`
}

// LinkAgent is a linked session or subagent.
type LinkAgent struct {
	SessionID string `json:"session_id,omitempty"`
	JobID     string `json:"job_id,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`
	// Kind is interactive or background for a session, subagent or
	// teammate for a subagent.
	Kind  string `json:"kind,omitempty"`
	Name  string `json:"name,omitempty"`
	State string `json:"state,omitempty"`
	// Live is true while it runs.
	Live          bool      `json:"live"`
	NeedsYou      bool      `json:"needs_you,omitempty"`
	NeedsYouSince time.Time `json:"needs_you_since,omitzero"`
	Reason        string    `json:"reason,omitempty"`
	// LastAction is its last action as the mod reported it ("Bash: make
	// check"), and LastEventAt when its last event arrived.
	LastAction   string    `json:"last_action,omitempty"`
	LastEventAt  time.Time `json:"last_event_at,omitzero"`
	WorktreePath string    `json:"worktree_path,omitempty"`
}

// Unmatched is a PR or branch in the plan's repos that matches no
// package of any plan.
type Unmatched struct {
	Target
	Repo   string `json:"repo"`
	Branch string `json:"branch,omitempty"`
	Number int    `json:"number,omitempty"`
	Title  string `json:"title,omitempty"`
	State  string `json:"state,omitempty"`
	// Note says why it didn't match, when the matcher knows.
	Note string `json:"note,omitempty"`
}

// Warning is a problem with the plan itself: a parser warning or a
// dependency cycle.
type Warning struct {
	Kind    string `json:"kind"`
	Line    int    `json:"line,omitempty"`
	ID      string `json:"id,omitempty"`
	Message string `json:"message"`
}

// Alert is something about a plan that needs your attention: unlocked,
// stalled, lane_collision or off_plan.
type Alert struct {
	Kind     string    `json:"kind"`
	Packages []string  `json:"packages,omitempty"`
	Lane     string    `json:"lane,omitempty"`
	Ref      *Target   `json:"ref,omitempty"`
	Message  string    `json:"message"`
	Since    time.Time `json:"since,omitzero"`
}

// PlanSummary is the body of GET /api/plans/{slug}/summary: what the band
// and the plan_status tool need.
type PlanSummary struct {
	Slug  string `json:"slug"`
	Title string `json:"title"`
	// Merges says who merges the plan's PRs: "you" or "lead", as on the
	// board.
	Merges string `json:"merges"`
	// Band is "<slug> · <done>/<total> done · <ready> ready · <n> need
	// you" ("1 needs you" for one).
	Band   string         `json:"band"`
	Total  int            `json:"total"`
	Done   int            `json:"done"`
	Counts map[string]int `json:"counts"`
	// Ready lists the packages ready to dispatch (not yours).
	Ready []SummaryPackage `json:"ready"`
	// NeedsYou counts what waits on you on this plan: its Open items in
	// the Needs you inbox. It is the menu's per-plan count and the band's
	// "N need you". NeedsYouItems lists those items, oldest first, and
	// NeedsYouPackages the packages they are on, each with its items'
	// reasons; an item on no package of the plan (the lead's own prompt,
	// say) is in NeedsYouItems only.
	NeedsYou         int              `json:"needs_you"`
	NeedsYouItems    []SummaryItem    `json:"needs_you_items"`
	NeedsYouPackages []SummaryPackage `json:"needs_you_packages"`
	// ToMerge counts the plan's open PRs that wait for its lead to merge
	// them: ready to merge on a plan that says "Merges: lead", with
	// no agent still working on them (internal/prwait's "waiting on
	// lead"). It is 0 on a plan you merge, where a ready PR is a Needs
	// you item instead. ToMergePRs lists them ("owner/repo#123").
	ToMerge    int      `json:"to_merge"`
	ToMergePRs []string `json:"to_merge_prs"`
	// Since echoes ?since=; Unlocked lists the packages that went from
	// blocked to ready after it (every such package when it is absent).
	Since    time.Time        `json:"since,omitzero"`
	Unlocked []SummaryPackage `json:"unlocked"`
	// Packages is every package, compactly, in plan order.
	Packages []SummaryPackage `json:"packages"`
	// Provisional lists the provisional packages: work names them,
	// the plan file's tables don't have them yet. Each has its suggested
	// row, for the lead to add.
	Provisional []SummaryPackage `json:"provisional"`
	Warnings    int              `json:"warnings"`
	Alerts      []Alert          `json:"alerts"`
	// DerivedAt is when the state engine last ran.
	DerivedAt time.Time `json:"derived_at"`
}

// SummaryPackage is a package in a summary.
type SummaryPackage struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Lifecycle string   `json:"lifecycle"`
	Yours     bool     `json:"yours,omitempty"`
	BlockedBy []string `json:"blocked_by,omitempty"`
	// PRs are the linked PRs ("owner/repo#123"), open ones first.
	PRs []string `json:"prs,omitempty"`
	// Reasons say why it waits on you (needs_you_packages only): one per
	// Open Needs you item on it.
	Reasons []string `json:"reasons,omitempty"`
	// Provisional and SuggestedRow are as on the board.
	Provisional  bool   `json:"provisional,omitempty"`
	SuggestedRow string `json:"suggested_row,omitempty"`
}

// SummaryItem is one of a plan's Open items in the Needs you inbox
// (GET /api/inbox has the whole item).
type SummaryItem struct {
	ID int64 `json:"id"`
	// URL is its page: /app/inbox?item=<id>.
	URL string `json:"url"`
	// Kind is permission, question, plan, reply, prompt, review or merge;
	// KindLabel says it in words.
	Kind      string `json:"kind"`
	KindLabel string `json:"kind_label"`
	Title     string `json:"title"`
	// Pkg is the package it is on, if any; PR the PR it is about, if any.
	Pkg string `json:"pkg,omitempty"`
	PR  string `json:"pr,omitempty"`
	// Reason says what it waits on in a few words, as needs_you_packages'
	// reasons do: "permission prompt: Bash: make check", "review
	// requested on #12".
	Reason string    `json:"reason"`
	Since  time.Time `json:"since"`
}

// RegisterRequest is the body of POST /api/plans.
type RegisterRequest struct {
	// Path is the plan file's absolute path.
	Path string `json:"path"`
	// Repos, if given, are the repos to register the plan for ("owner/name"
	// or a repo's absolute path); they win over the plan's Repos: line and
	// the file's own repo.
	Repos []string `json:"repos,omitempty"`
	// SessionID is the registering session, if any.
	SessionID string `json:"session_id,omitempty"`
}

// RegisterResponse is the answer to POST /api/plans: 201 for a new plan,
// 200 when the file was registered already.
type RegisterResponse struct {
	Created bool   `json:"created"`
	Slug    string `json:"slug"`
	Title   string `json:"title"`
	Path    string `json:"path"`
	// Repos are the repos the plan is tracked in, and ReposFrom where they
	// came from: "request", "plan" (its Repos: line) or "file" (the repo
	// the plan file is in).
	Repos     []string `json:"repos"`
	ReposFrom string   `json:"repos_from"`
	Packages  int      `json:"packages"`
	Warnings  int      `json:"warnings"`
	// Backfill is "started" when aimd is reading the repos' PRs from GitHub
	// in the background (a "plans" change follows), or "off".
	Backfill string      `json:"backfill"`
	Summary  PlanSummary `json:"summary"`
}

// Unregistered is the answer to DELETE /api/plans/{slug}: the plan that
// was removed, and how much of aimd's own data went with it. The plan file,
// git and GitHub are never touched.
type Unregistered struct {
	Slug  string `json:"slug"`
	Title string `json:"title"`
	Path  string `json:"path"`
	// Packages and Links count the packages and stored links removed.
	Packages int `json:"packages"`
	Links    int `json:"links"`
}

// PathRequest is the body of POST /api/plans/detect and
// POST /api/plans/dismiss.
type PathRequest struct {
	Path string `json:"path"`
}

// Detection is the answer to POST /api/plans/detect.
type Detection struct {
	// Path is the file, symlinks resolved.
	Path string `json:"path"`
	// IsPlan is true when the file has a package table and at least two
	// package IDs.
	IsPlan   bool   `json:"is_plan"`
	Title    string `json:"title,omitempty"`
	Packages int    `json:"packages"`
	Lanes    int    `json:"lanes"`
	// Slug is the registered plan's slug, or the one it would get.
	Slug string `json:"slug"`
	// Registered is true when the file is a registered plan; Dismissed
	// when you said it isn't a plan, and DismissedAt when.
	Registered  bool      `json:"registered"`
	Dismissed   bool      `json:"dismissed"`
	DismissedAt time.Time `json:"dismissed_at,omitzero"`
	// Offer is true when the band should offer [Register] [Not a plan]: a
	// plan, not registered, not dismissed.
	Offer bool `json:"offer"`
}

// Dismissal is the answer to POST /api/plans/dismiss.
type Dismissal struct {
	Path        string    `json:"path"`
	Dismissed   bool      `json:"dismissed"`
	DismissedAt time.Time `json:"dismissed_at"`
}

// SpawnLinkRequest is the body of POST /api/links/spawn: the mod links an
// agent it saw spawn (or a remote agent's cloud session) to a package.
type SpawnLinkRequest struct {
	// SessionID is the lead's session; AgentID the new agent's id. A
	// remote agent has no AgentID: give CloudURL, its sessionUrl, instead.
	SessionID string `json:"session_id"`
	AgentID   string `json:"agent_id,omitempty"`
	CloudURL  string `json:"cloud_url,omitempty"`
	// Plan is the plan's slug; Pkg the package ID.
	Plan string `json:"plan"`
	Pkg  string `json:"pkg"`
	// Provisional asks aimd to make a provisional package when the
	// plan has no package Pkg: the mod sends it for a brief's Package:
	// line. Title is the package's title then (the spawn's description).
	// Without it an unknown package is a 404, as before.
	Provisional bool   `json:"provisional,omitempty"`
	Title       string `json:"title,omitempty"`
}

// PRLinkRequest is the body of POST /api/links/pr: a linked agent created
// a PR, so the PR joins the agent's package before the next GitHub poll.
type PRLinkRequest struct {
	SessionID string `json:"session_id"`
	AgentID   string `json:"agent_id,omitempty"`
	// URL is the PR's URL, https://github.com/<owner>/<repo>/pull/<n>.
	URL string `json:"url"`
}

// LinkRequest is the body of POST /api/links/manual and DELETE /api/links.
type LinkRequest struct {
	Plan string `json:"plan"`
	Pkg  string `json:"pkg"`
	// Kind is agent, branch, pr, worktree or cloud_session; Ref as in
	// Target (a PR may also be given by its URL).
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

// LinkResult is the answer to the link endpoints: the links written (or
// removed) and the packages they touch, freshly derived.
type LinkResult struct {
	Links    []LinkRef `json:"links"`
	Packages []Package `json:"packages"`
}

// LinkRef is a stored link, or the target of an unlink.
type LinkRef struct {
	Plan string `json:"plan"`
	Pkg  string `json:"pkg"`
	Target
	Method     string `json:"method,omitempty"`
	Confidence string `json:"confidence,omitempty"`
	// ID is the stored link's id; 0 for an unlink.
	ID int64 `json:"id,omitempty"`
}

// DoneResult is the answer to POST and DELETE
// /api/plans/{slug}/packages/{id}/done: the package, and the plan's
// summary, freshly derived.
type DoneResult struct {
	Package Package     `json:"package"`
	Summary PlanSummary `json:"summary"`
}

// Context is the body of GET /api/context?cwd=<path>: what the mod needs
// at session start for the directory a session works in.
type Context struct {
	// Cwd is the directory asked about.
	Cwd string `json:"cwd"`
	// Repo is the repo it's in; nil when it isn't in a repo aimd may look
	// at.
	Repo *ContextRepo `json:"repo"`
	// Worktree is the worktree it's in, if aimd's last scan knows it.
	Worktree *ContextWorktree `json:"worktree"`
	// Plans are the plans registered for the repo.
	Plans []ContextPlan `json:"plans"`
	// Package is the package the worktree or its branch is linked to, if
	// any: the convention block's data. Candidates lists every package so
	// linked when there is more than one (Package is then the first).
	Package    *ContextPackage  `json:"package"`
	Candidates []ContextPackage `json:"candidates,omitempty"`
}

// ContextRepo is a repo in a Context.
type ContextRepo struct {
	// Path is the main checkout; Name the GitHub nameWithOwner, if any.
	Path string `json:"path"`
	Name string `json:"name,omitempty"`
}

// ContextWorktree is a worktree in a Context.
type ContextWorktree struct {
	Path     string `json:"path"`
	Branch   string `json:"branch,omitempty"`
	Detached bool   `json:"detached,omitempty"`
	// Main is true for the repo's main checkout.
	Main bool `json:"main,omitempty"`
}

// ContextPlan is a plan registered for a Context's repo.
type ContextPlan struct {
	Slug  string `json:"slug"`
	Title string `json:"title"`
	// Path is the plan file (the mod names it when a brief's Package:
	// line names a package the file doesn't have).
	Path     string               `json:"path"`
	Band     string               `json:"band"`
	Packages []ContextPlanPackage `json:"packages"`
}

// ContextPlanPackage is a package of a ContextPlan.
type ContextPlanPackage struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Repo      string `json:"repo,omitempty"`
	Lifecycle string `json:"lifecycle"`
	Convention
	// Provisional is true for a package the plan file doesn't have yet
	// (a provisional package): the mod links to it, but still tells the
	// lead to add its row.
	Provisional bool `json:"provisional,omitempty"`
}

// ContextPackage is the package a Context's worktree or branch is linked
// to.
type ContextPackage struct {
	Plan      string `json:"plan"`
	ID        string `json:"id"`
	Title     string `json:"title"`
	Lifecycle string `json:"lifecycle"`
	Convention
	// Link is how the worktree or branch is tied to the package.
	Link Link `json:"link"`
}
