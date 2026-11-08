package api

import "time"

// The Needs you inbox (internal/inbox): GET /api/inbox,
// GET /api/inbox/{id} and POST /api/inbox/{id}. docs/api.md describes
// them.

// TopicInbox is the /api/stream topic sent when a sync pass or an action
// changes the inbox.
const TopicInbox = "inbox"

// Inbox is GET /api/inbox: one tab's items, grouped by plan (the
// registered plans in order, other plans, then no plan), oldest episode
// first in each group.
type Inbox struct {
	// Tab is open, progress, snoozed, done, or all.
	Tab string `json:"tab"`
	// Counts has every state, zeros included.
	Counts map[string]int `json:"counts"`
	Items  []InboxItem    `json:"items"`
}

// InboxItem is one item: a waiting episode of a monitored session or a PR.
type InboxItem struct {
	ID int64 `json:"id"`
	// URL is its page: /app/inbox?item=<id>.
	URL string `json:"url"`
	// Source is session or pr; Ref names what waits ("session:<row id>",
	// "owner/repo#123").
	Source string `json:"source"`
	Ref    string `json:"ref"`
	// Kind is permission, question, plan, reply, prompt, review, merge or
	// pr; KindLabel says it in words.
	Kind      string `json:"kind"`
	KindLabel string `json:"kind_label"`
	// State is open, progress, snoozed or done.
	State string `json:"state"`
	// New is true until you first open it.
	New bool `json:"new"`

	Title    string `json:"title"`
	Subtitle string `json:"subtitle,omitempty"`
	// Detail is what it waits on, as far as aimd knows.
	Detail string `json:"detail,omitempty"`
	// WaitingOn is what a waiting session item waits on as the mod
	// reported it (as Session.WaitingOn): the permission's tool and
	// redacted summary, the questions and their options, or the plan.
	// Only while its session still needs you, and only for a request with
	// aimd's token in X-AIM-Token, as GET /api/sessions gives it.
	WaitingOn  *WaitingOn `json:"waiting_on,omitempty"`
	Plan       string     `json:"plan,omitempty"`
	Pkg        string     `json:"pkg,omitempty"`
	SessionKey string     `json:"session_key,omitempty"`
	JobID      string     `json:"job_id,omitempty"`
	Repo       string     `json:"repo,omitempty"`
	Branch     string     `json:"branch,omitempty"`
	Folder     string     `json:"folder,omitempty"`
	PR         string     `json:"pr,omitempty"`
	PRURL      string     `json:"pr_url,omitempty"`
	// Why says why a PR needs you.
	Why string `json:"why,omitempty"`

	// Since is when the wait began; StateAt when State was set.
	Since   time.Time `json:"since"`
	StateAt time.Time `json:"state_at"`
	// DueAt is when it moves by itself: In progress back to Open, a timed
	// snooze ending. Snooze is the snooze choice while snoozed: 15m, 1h,
	// tomorrow or change.
	DueAt  time.Time `json:"due_at,omitzero"`
	Snooze string    `json:"snooze,omitempty"`
	// EndedAt is when the episode stopped waiting, EndReason why.
	EndedAt   time.Time `json:"ended_at,omitzero"`
	EndReason string    `json:"end_reason,omitempty"`

	// History is the item's history, oldest first: only in
	// GET /api/inbox/{id} and the answer of POST /api/inbox/{id}.
	History []InboxMove `json:"history,omitempty"`
}

// InboxMove is one entry of an item's history. From and To are equal for
// a note (it changed, it stopped waiting) that didn't move it.
type InboxMove struct {
	ID   int64     `json:"id"`
	At   time.Time `json:"at"`
	By   string    `json:"by"` // you or aimd
	Text string    `json:"text"`
	From string    `json:"from,omitempty"`
	To   string    `json:"to"`
}

// InboxActRequest is POST /api/inbox/{id}'s body.
type InboxActRequest struct {
	// Action is go, snooze, done, open (back to Open), undo or seen.
	Action string `json:"action"`
	// Snooze is 15m, 1h, tomorrow or change (snooze).
	Snooze string `json:"snooze,omitempty"`
	// Move is the history entry to undo: the move an earlier answer gave.
	Move int64 `json:"move,omitempty"`
}

// InboxActResult is POST /api/inbox/{id}'s answer.
type InboxActResult struct {
	Item InboxItem `json:"item"`
	// Move is the history entry the action added; undo takes its id.
	Move *InboxMove `json:"move,omitempty"`
	// Message says what happened, as the dashboard's toast does.
	Message string `json:"message"`
}
