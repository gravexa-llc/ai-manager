// Package api holds the JSON shapes aimd's read API serves: the bodies of
// GET /api/sessions, /api/worktrees, /api/prs and /api/status, and the
// change events on /api/stream. It has no dependencies beyond the standard
// library, so the aim CLI can decode responses without linking the daemon.
//
// Conventions: field names are snake_case; times are RFC 3339 and left out
// when unknown; durations are whole seconds in fields ending in _seconds.
package api

import "time"

// Sessions is the body of GET /api/sessions.
type Sessions struct {
	Sessions []Session `json:"sessions"`
	// HiddenHeadless counts the headless `claude -p` entries left out
	// because ?all=1 wasn't given.
	HiddenHeadless int `json:"hidden_headless"`
	// Monitor is config sessions.monitor (internal/monitor): "opt-in" or
	// "all".
	Monitor string `json:"monitor"`
	// NotMonitored counts the listed sessions that aren't monitored.
	NotMonitored int `json:"not_monitored"`
}

// Session is one live Claude Code session.
type Session struct {
	// Key is the poller's stable key: "job:<id>" for a background job,
	// "pid:<pid>" otherwise.
	Key           string `json:"key"`
	SessionID     string `json:"session_id,omitempty"`
	JobID         string `json:"job_id,omitempty"`
	PrevSessionID string `json:"prev_session_id,omitempty"`
	Kind          string `json:"kind"`
	// Headless marks a `claude -p` run: listed for less than one poll
	// interval, or with its cwd under $TMPDIR.
	Headless bool   `json:"headless"`
	Name     string `json:"name,omitempty"`
	Host     string `json:"host,omitempty"`
	Cwd      string `json:"cwd"`
	// WorktreePath, Repo (the main checkout's path) and Branch are where
	// Cwd is, from the last worktree scan.
	WorktreePath   string `json:"worktree_path,omitempty"`
	Repo           string `json:"repo,omitempty"`
	Branch         string `json:"branch,omitempty"`
	PID            int    `json:"pid,omitempty"`
	ItermSessionID string `json:"iterm_session_id,omitempty"`
	// Terminal is the terminal app the session runs in, read from its
	// process tree at the last session poll (internal/term): "iterm2",
	// "terminal" (Terminal.app), "tmux", "other" (another app: VS Code,
	// Ghostty, Warp, SSH, …) or "none" (no process, no tty, or a
	// background session). TerminalApp names it for people: "iTerm2",
	// "Terminal", "tmux", or the other app's name when aimd could tell.
	Terminal    string `json:"terminal"`
	TerminalApp string `json:"terminal_app,omitempty"`

	// State is the session state (internal/sessions' State values) and
	// StateReason why, e.g. the `waitingFor` text of a permission prompt.
	State       string `json:"state"`
	StateReason string `json:"state_reason,omitempty"`

	// NeedsYou is State == "needs_you". NeedsYouSince is when aimd first saw
	// it (the JSON has no "since"), and NeedsYouAgeSeconds how long ago that
	// was.
	NeedsYou           bool      `json:"needs_you"`
	NeedsYouSince      time.Time `json:"needs_you_since,omitzero"`
	NeedsYouAgeSeconds int64     `json:"needs_you_age_seconds,omitempty"`
	// NeedsYouDismissed is true when the current needs-you spell was
	// dismissed.
	NeedsYouDismissed bool `json:"needs_you_dismissed,omitempty"`
	// WaitingOn is what the session waits on: the permission,
	// question or plan approval of its oldest pending ask, as the mod
	// reported it. Present only while it needs you and the mod saw the ask;
	// kept in aimd's memory, so an aimd restart forgets it. Only for a
	// request with aimd's token in X-AIM-Token (docs/api.md).
	WaitingOn *WaitingOn `json:"waiting_on,omitempty"`

	LastAction string `json:"last_action,omitempty"`
	LastSaid   string `json:"last_said,omitempty"`

	StartedAt   time.Time `json:"started_at,omitzero"`
	FirstSeenAt time.Time `json:"first_seen_at,omitzero"`
	LastSeenAt  time.Time `json:"last_seen_at,omitzero"`
	LastEventAt time.Time `json:"last_event_at,omitzero"`

	// Monitored is true when aimd monitors the session (internal/monitor): Needs
	// you and the notifications follow only monitored sessions.
	// MonitoredWhy says why, most explicit first; it is [] when it isn't
	// monitored.
	Monitored    bool           `json:"monitored"`
	MonitoredWhy []MonitoredWhy `json:"monitored_why"`
}

// WaitingOn is what a session's pending ask waits on, as the mod reports
// it (events.Wait in internal/events): the event's wait detail, which loop
// asks, and since when.
type WaitingOn struct {
	// Kind is "permission", "question" or "plan".
	Kind string `json:"kind"`
	// AgentID names the subagent whose dialog it is; empty for the main
	// loop. (In Claude Code 2.1.319 subagents can raise permissions only:
	// questions and plan approvals are always the main loop's.)
	AgentID string `json:"agent_id,omitempty"`
	// Tool is the tool asked about, and ToolUseID the call.
	Tool      string `json:"tool,omitempty"`
	ToolUseID string `json:"tool_use_id,omitempty"`
	// Summary, for a permission: one line saying what the call does, e.g.
	// the Bash command (at most 300 characters).
	Summary string `json:"summary,omitempty"`
	// Questions, for a question: each question and its option labels.
	Questions []WaitQuestion `json:"questions,omitempty"`
	// Plan, for a plan approval: the plan's first lines.
	Plan  string    `json:"plan,omitempty"`
	Since time.Time `json:"since"`
}

// WaitQuestion is one question an AskUserQuestion call asks.
type WaitQuestion struct {
	Header      string   `json:"header,omitempty"`
	Question    string   `json:"question"`
	Options     []string `json:"options,omitempty"`
	MultiSelect bool     `json:"multi_select,omitempty"`
}

// MonitoredWhy is one reason a session is monitored (internal/monitor).
type MonitoredWhy struct {
	// Reason is "plan" (it registered the plan), "watch" (you asked to
	// watch it: /aim watch or the dashboard's Watch), "agent" (an agent of
	// a registered plan's package, or of a monitored lead) or "all"
	// (sessions.monitor is "all" and no other reason holds).
	Reason string `json:"reason"`
	// Plan is the plan's slug, where there is one: the plan it registered,
	// the package's plan, or its lead's plan.
	Plan string `json:"plan,omitempty"`
	// Pkg is the package an "agent" is linked to, and Subagent is set when
	// the link names one of the session's subagents (the session is their
	// lead).
	Pkg      string `json:"pkg,omitempty"`
	Subagent bool   `json:"subagent,omitempty"`
	// Lead is the key of the monitored session an "agent" belongs to by
	// lineage (the worktree it works in was locked for that session's
	// agent).
	Lead string `json:"lead,omitempty"`
	// Since is when a "plan" or "watch" began.
	Since time.Time `json:"since,omitzero"`
	// Text says it for people: "registered plan sharing", "you asked to
	// watch it", "agent of T1".
	Text string `json:"text"`
}

// WatchRequest is the body of POST and DELETE /api/sessions/watch.
type WatchRequest struct {
	// SessionID is Claude Code's session id (the mod's $.session.id()).
	SessionID string `json:"session_id"`
}

// WatchResult answers POST and DELETE /api/sessions/watch.
type WatchResult struct {
	SessionID string `json:"session_id"`
	// Watched says whether the session is watched now, and Changed
	// whether this call changed that (false: it already was, or already
	// wasn't). WatchedSince is when the watch began.
	Watched      bool      `json:"watched"`
	Changed      bool      `json:"changed"`
	WatchedSince time.Time `json:"watched_since,omitzero"`
	// Known is true when aimd lists a live session with this id (or with
	// it before its last /clear); Key and Title then name that session.
	Known bool   `json:"known"`
	Key   string `json:"key,omitempty"`
	Title string `json:"title,omitempty"`
	// Monitored and MonitoredWhy are as in GET /api/sessions, for the
	// session as aimd sees it now.
	Monitored    bool           `json:"monitored"`
	MonitoredWhy []MonitoredWhy `json:"monitored_why"`
	// Monitor is sessions.monitor: "opt-in" or "all".
	Monitor string `json:"monitor"`
	// Message says what happened in one line, for the mod to show.
	Message string `json:"message"`
}

// Worktrees is the body of GET /api/worktrees.
type Worktrees struct {
	Worktrees []Worktree `json:"worktrees"`
}

// Worktree is one worktree the scanner has seen, main checkouts included
// (Path equals MainRepo).
type Worktree struct {
	Path     string `json:"path"`
	MainRepo string `json:"main_repo"`
	Branch   string `json:"branch,omitempty"`
	Detached bool   `json:"detached,omitempty"`
	HeadSHA  string `json:"head_sha,omitempty"`

	// StatusKnown is false when git status couldn't be read (a missing,
	// prunable or bare entry, or a failed status); the counts are then null,
	// which means unknown, not clean.
	StatusKnown    bool     `json:"status_known"`
	DirtyCount     *int     `json:"dirty_count"`
	UntrackedCount *int     `json:"untracked_count"`
	IgnoredAtRisk  []string `json:"ignored_at_risk,omitempty"`

	// UpstreamState is "ok", "gone", "none", or empty when unknown.
	UpstreamState string    `json:"upstream_state,omitempty"`
	UnpushedCount int       `json:"unpushed_count,omitempty"`
	LastCommitAt  time.Time `json:"last_commit_at,omitzero"`

	Locked         bool   `json:"locked,omitempty"`
	LockedReason   string `json:"locked_reason,omitempty"`
	LockAgentID    string `json:"lock_agent_id,omitempty"`
	LockPID        int    `json:"lock_pid,omitempty"`
	PrunableReason string `json:"prunable_reason,omitempty"`

	SessionID string `json:"session_id,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`

	FirstSeen time.Time `json:"first_seen,omitzero"`
	LastSeen  time.Time `json:"last_seen,omitzero"`
	RemovedAt time.Time `json:"removed_at,omitzero"`
}

// PRs is the body of GET /api/prs.
type PRs struct {
	PRs []PR `json:"prs"`
}

// PR is one pull request. Enum fields are as GitHub sends them.
type PR struct {
	// Repo is GitHub's nameWithOwner.
	Repo              string `json:"repo"`
	Number            int    `json:"number"`
	Title             string `json:"title"`
	URL               string `json:"url"`
	Author            string `json:"author,omitempty"`
	State             string `json:"state"`
	Draft             bool   `json:"draft"`
	HeadBranch        string `json:"head_branch"`
	HeadRefOID        string `json:"head_ref_oid,omitempty"`
	BaseRef           string `json:"base_ref,omitempty"`
	IsCrossRepository bool   `json:"is_cross_repository,omitempty"`
	Mergeable         string `json:"mergeable,omitempty"`
	MergeStateStatus  string `json:"merge_state_status,omitempty"`
	ReviewDecision    string `json:"review_decision,omitempty"`
	// ChecksState is statusCheckRollup.state, or "NONE" when the head
	// commit has no checks at all.
	ChecksState string `json:"checks_state,omitempty"`
	// PlanSlug and PlanPkg come from the body's `Plan: <slug> · <ID>` line.
	PlanSlug string `json:"plan_slug,omitempty"`
	PlanPkg  string `json:"plan_pkg,omitempty"`

	// ReviewRequested is set while your review is requested on the PR, as
	// the last review-request search found, and ReviewRequestedSince
	// is when aimd first saw the request.
	ReviewRequested      bool      `json:"review_requested,omitempty"`
	ReviewRequestedSince time.Time `json:"review_requested_since,omitzero"`

	// What the PR waits on (internal/prwait). NeedsYou is
	// "review_requested", "ready_to_merge" or "": the PR is in Needs you
	// only when it is set. WaitingOn is "you" (exactly when NeedsYou is
	// set), "lead", "agent" or "none". Merges is who merges it, "you" or
	// "lead": the Settings page's override, else its plan's Merges
	// line ("you" on no plan); "" when it doesn't count. Reason says it
	// all in one line for people.
	NeedsYou  string `json:"needs_you"`
	WaitingOn string `json:"waiting_on"`
	Merges    string `json:"merges,omitempty"`
	Reason    string `json:"reason"`
	// Monitored is true when the PR counts for Needs you: a registered
	// plan's board links it, or it is a monitored session's branch. Plan
	// and Pkg are the registered plan and package it is on, if any (the
	// board that links it, or the plan of the monitored session on its
	// branch); PlanSlug and PlanPkg are only what its body says.
	Monitored bool   `json:"monitored"`
	Plan      string `json:"plan,omitempty"`
	Pkg       string `json:"pkg,omitempty"`

	UpdatedAt time.Time `json:"updated_at,omitzero"`
	MergedAt  time.Time `json:"merged_at,omitzero"`
	ClosedAt  time.Time `json:"closed_at,omitzero"`
}

// Status is the body of GET /api/status: poller health, the GitHub budget
// and a few counts.
type Status struct {
	Version   string    `json:"version"`
	StartedAt time.Time `json:"started_at"`
	Now       time.Time `json:"now"`
	// Watchers is how many /api/stream clients are connected.
	Watchers int `json:"watchers"`
	// ModLive is true once the mod reports sessions live (POST /events).
	ModLive bool `json:"mod_live"`
	// Sources are the pollers, in a fixed order: sessions, worktrees,
	// github, github_search, retention.
	Sources []Source     `json:"sources"`
	GitHub  GitHubStatus `json:"github"`
	Counts  Counts       `json:"counts"`
	// Events are the event receiver's counters (POST /events).
	Events *EventStats `json:"events,omitempty"`
}

// EventStats counts what POST /events received from the mod since aimd
// started (internal/live).
type EventStats struct {
	// Batches counts the POSTs accepted, and Events the events in them
	// that passed the contract's checks.
	Batches int64 `json:"batches"`
	Events  int64 `json:"events"`
	// Applied counts the events applied to session state, Deduped those
	// dropped as repeats (same session, agent, kind, within a second).
	Applied int64 `json:"applied"`
	Deduped int64 `json:"deduped"`
	// UnknownKinds counts events dropped for a kind aimd doesn't know,
	// UnknownByKind the same by kind (32 kinds at most; the rest are
	// "(other)").
	UnknownKinds  int64            `json:"unknown_kinds"`
	UnknownByKind map[string]int64 `json:"unknown_by_kind,omitempty"`
	// Invalid counts events dropped for a missing session id or time, or
	// a field of the wrong type.
	Invalid int64 `json:"invalid"`
	// RefusedBatches counts POSTs refused whole (over 200 events or
	// 256 KB, or not a batch); DroppedBatches those dropped because the
	// queue was full.
	RefusedBatches int64 `json:"refused_batches"`
	DroppedBatches int64 `json:"dropped_batches"`
	// StoreErrors counts failed database writes.
	StoreErrors int64 `json:"store_errors"`
	// LastBatchAt is when the last POST arrived, LastEventAt the time of
	// the latest event applied.
	LastBatchAt time.Time `json:"last_batch_at,omitzero"`
	LastEventAt time.Time `json:"last_event_at,omitzero"`
	// LiveSessions counts the sessions the receiver follows.
	LiveSessions int `json:"live_sessions"`
	// ModLive is true from the first batch until five minutes pass
	// without one.
	ModLive bool `json:"mod_live"`
}

// Source names.
const (
	SourceSessions     = "sessions"
	SourceWorktrees    = "worktrees"
	SourceGitHub       = "github"        // PRs of tracked branches, one query per tick
	SourceGitHubSearch = "github_search" // open PRs by you across owners
	SourceRetention    = "retention"     // the daily event purge
)

// Source is one poller's health.
type Source struct {
	Name string `json:"name"`
	// IntervalSeconds is how often it runs now.
	IntervalSeconds int64 `json:"interval_seconds,omitempty"`
	Runs            int   `json:"runs"`
	// LastRun is when it last ran (or was skipped), LastOK when it last
	// succeeded.
	LastRun time.Time `json:"last_run,omitzero"`
	LastOK  time.Time `json:"last_ok,omitzero"`
	// FailingSince is set while it is failing: when the current streak of
	// failures began. Error is the last failure.
	FailingSince time.Time `json:"failing_since,omitzero"`
	Error        string    `json:"error,omitempty"`
	// Stale is true while FailingSince is set: its data is as of LastOK.
	Stale bool `json:"stale"`
	// PartialErrors counts parts of the last run that failed while the run
	// as a whole worked: repos git or GitHub couldn't read, owners whose
	// search failed.
	PartialErrors int `json:"partial_errors,omitempty"`
	// Skipped says why the last tick didn't run, e.g. the GitHub budget.
	Skipped string `json:"skipped,omitempty"`
}

// GitHubStatus is the GitHub poller's state.
type GitHubStatus struct {
	Budget Budget `json:"budget"`
	// Tick counts GitHub ticks since start; every IdleEvery-th tick (tick 0
	// included) also polls the idle repos.
	Tick      int `json:"tick"`
	IdleEvery int `json:"idle_every"`
	// ActiveRepos and IdleRepos are how the last tick classified the repos
	// it tracks.
	ActiveRepos int `json:"active_repos"`
	IdleRepos   int `json:"idle_repos"`
	// LastQueries and LastCost are what the last tick sent and was charged.
	LastQueries int `json:"last_queries"`
	LastCost    int `json:"last_cost"`
}

// Budget is GitHub's rate-limit verdict (internal/github Budget).
type Budget struct {
	// Verdict is "normal", "idle-only" or "stop".
	Verdict string `json:"verdict"`
	// Known is false until GitHub has reported a rate limit.
	Known     bool      `json:"known"`
	Remaining int       `json:"remaining,omitempty"`
	Limit     int       `json:"limit,omitempty"`
	ResetAt   time.Time `json:"reset_at,omitzero"`
	// ResumeAt is when a "stop" verdict ends.
	ResumeAt time.Time `json:"resume_at,omitzero"`
	LastCost int       `json:"last_cost,omitempty"`
}

// Counts summarizes the store for `aim status`.
type Counts struct {
	// Sessions counts live sessions shown by default (headless ones aside).
	Sessions int `json:"sessions"`
	// NotMonitored counts those that aren't monitored.
	NotMonitored int `json:"not_monitored"`
	// SessionsNeedingYou counts the monitored ones needing you whose alert
	// isn't dismissed, expired background jobs aside (their conversation
	// is gone, so they never need you), and OldestNeedsYouSince is when
	// the longest wait began.
	SessionsNeedingYou  int       `json:"sessions_needing_you"`
	OldestNeedsYouSince time.Time `json:"oldest_needs_you_since,omitzero"`
	HeadlessSessions    int       `json:"headless_sessions"`
	Worktrees           int       `json:"worktrees"`
	OpenPRs             int       `json:"open_prs"`
	// PRsNeedingYou counts the open PRs needing you (internal/prwait's
	// rule; with sessions.monitor = "opt-in", the ones linked to a plan's
	// package or on a monitored session's branch, and any whose review is
	// requested from you).
	PRsNeedingYou int `json:"prs_needing_you"`
	// StaleSources names the sources whose last run failed.
	StaleSources []string `json:"stale_sources"`
}

// Change is the data of a "change" event on /api/stream: something in the
// named part of the store (or the status) changed, so a client should
// fetch it again.
type Change struct {
	// Topic is "sessions", "worktrees", "prs" or "status".
	Topic string    `json:"topic"`
	Seq   uint64    `json:"seq"`
	At    time.Time `json:"at"`
}

// Change topics.
const (
	TopicSessions  = "sessions"
	TopicWorktrees = "worktrees"
	TopicPRs       = "prs"
	TopicStatus    = "status"
)
