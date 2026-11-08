package api

import "time"

// A plan's timeline: what happened to its packages, newest first. The
// dashboard shows it at GET /plans/{slug}/timeline; internal/planapi puts
// it together from the store.

// Timeline limits: how many entries a timeline shows unless asked for
// more, and the most it shows.
const (
	DefaultTimelineLimit = 200
	MaxTimelineLimit     = 5000
)

// TimelineQuery narrows a plan's timeline.
type TimelineQuery struct {
	// Pkg keeps one package's entries (any case); "" keeps all.
	Pkg string `json:"pkg,omitempty"`
	// Agent keeps one agent's entries: its events and its links. It is an
	// agent link's ref ("session_id/agent_id", "session_id/" or
	// "job:<id>"), as Timeline.Agents lists them.
	Agent string `json:"agent,omitempty"`
	// Limit caps the entries, newest first: DefaultTimelineLimit when
	// zero, never more than MaxTimelineLimit.
	Limit int `json:"limit,omitempty"`
}

// Timeline entry kinds.
const (
	// TimelineLifecycle: a package's lifecycle changed (Prev is "" for
	// its first derivation, when the plan was registered).
	TimelineLifecycle = "lifecycle"
	// TimelineLink: a link was stored: by a spawn, by hand, or by the
	// matcher (a PR's Plan: line, title or branch).
	TimelineLink = "link"
	// TimelinePR: a linked PR was opened (GitHub's createdAt, or when aimd
	// first stored a PR it has no createdAt for), merged or closed.
	TimelinePR = "pr"
	// TimelineEvent: an event the mod reported for an agent linked to a
	// package (a summary only: never a prompt or output).
	TimelineEvent = "event"
	// TimelineAlert: an alert was raised or cleared (stalled, lane
	// collision).
	TimelineAlert = "alert"
)

// Timeline is a plan's timeline.
type Timeline struct {
	Slug  string `json:"slug"`
	Title string `json:"title"`
	// Query is the query answered, with Limit filled in.
	Query TimelineQuery `json:"query"`
	// Entries are newest first.
	Entries []TimelineEntry `json:"entries"`
	// More is true when Limit left older entries out.
	More bool `json:"more"`
	// Agents are the agents linked to the plan's packages, for the agent
	// filter.
	Agents []TimelineAgent `json:"agents"`
}

// TimelineAgent is an agent linked to a plan's packages.
type TimelineAgent struct {
	// Ref is the agent link's ref.
	Ref string `json:"ref"`
	// Agent is what aimd knows of it, if anything.
	Agent *LinkAgent `json:"agent,omitempty"`
	// Packages are the packages it is linked to.
	Packages []string `json:"packages"`
}

// TimelineEntry is one thing that happened. Kind says which fields are
// set.
type TimelineEntry struct {
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`
	// ID is the source row's id (history, link, event, alert), or a PR's
	// number: with Kind and Action, it names the entry.
	ID int64 `json:"id,omitempty"`
	// Packages are the packages it concerns.
	Packages []string `json:"packages"`
	// Action is a PR's opened, merged or closed, or an alert's raised or
	// cleared.
	Action string `json:"action,omitempty"`

	// lifecycle
	Lifecycle string `json:"lifecycle,omitempty"`
	Prev      string `json:"prev,omitempty"`

	// link
	Link *LinkRef `json:"link,omitempty"`

	// pr
	PR *LinkPR `json:"pr,omitempty"`

	// event: the agent's ref (also set on an agent's link), the event's
	// kind, the tool and the summary.
	Agent   string `json:"agent,omitempty"`
	Event   string `json:"event,omitempty"`
	Tool    string `json:"tool,omitempty"`
	Summary string `json:"summary,omitempty"`

	// alert
	AlertKind string `json:"alert_kind,omitempty"`
	Message   string `json:"message,omitempty"`
}
