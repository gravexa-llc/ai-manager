// Package events is the contract between the mod (plugin/mod) and aimd's
// event receiver (internal/live): the events the mod POSTs to /events,
// their fields, and the limits aimd enforces. plugin/mod/report.ts is the
// same contract on the mod's side; keep the two in step.
//
// The mod sends summaries only: labels, short reasons, the first line of
// what the model said, cut to 300 characters. Never a prompt, file
// contents, tool input beyond a label, or tool output. aimd can't check
// that, but it enforces the length caps the store has (store.MaxSummaryLen
// and friends) by truncating, so an oversized field is shortened, never
// refused. The one place a needs_you goes past a label is its Wait:
// what the person is asked, redacted (Redact, here as well as in the
// mod) and cut to its caps, which aimd keeps in memory only.
//
// # Wire format
//
// One POST carries a batch: {"events": [ ... ]}, at most [MaxBatchEvents]
// events and [MaxBatchBytes] bytes. Each event is a flat JSON object with
// the common fields (kind, session_id, at, and agent_id / cwd where they
// apply) plus the fields of its kind. [DecodeBatch] reads a batch: events
// of a kind this package doesn't know, and events missing a common field,
// are dropped and counted, never fatal.
package events

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Batch limits. A POST over either is refused as a whole (413).
const (
	MaxBatchEvents = 200
	MaxBatchBytes  = 256 << 10
)

// Field caps, in characters (runes). Longer values are truncated. The
// first three are the store's caps on the events table (store.MaxEventLen,
// store.MaxToolNameLen, store.MaxSummaryLen); the rest bound what aimd
// keeps in memory and in other columns.
const (
	MaxKindLen    = 64   // kind, origin_kind, source, an agent's status and type
	MaxToolLen    = 128  // tool, name, ids, host
	MaxSummaryLen = 300  // last_said, label, reason
	MaxPathLen    = 4096 // cwd, repo, worktree
	MaxAgents     = 500  // agents in one agent_list

	// A needs_you's wait detail (Wait).
	MaxWaitQuestions = 4    // questions of one AskUserQuestion
	MaxWaitOptions   = 6    // options of one question
	MaxOptionLen     = 100  // an option's label
	MaxPlanLen       = 1000 // the plan's first lines, in all
	MaxPlanLines     = 12   // lines of the plan kept
)

// Kind is an event's kind.
type Kind string

// The event kinds. The mod's reporting hooks (plugin/mod/register.tsx)
// say when it sends each.
const (
	SessionStart   Kind = "session_start"
	Prompt         Kind = "prompt"
	TurnStart      Kind = "turn_start"
	TurnComplete   Kind = "turn_complete"
	Tool           Kind = "tool"
	NeedsYou       Kind = "needs_you"
	Idle           Kind = "idle"
	Error          Kind = "error"
	WorktreeCreate Kind = "worktree_create"
	AgentList      Kind = "agent_list"
	SessionEnd     Kind = "session_end"
)

// Kinds lists every kind, roughly in the order a session sends them.
var Kinds = []Kind{SessionStart, Prompt, TurnStart, TurnComplete, Tool, NeedsYou, Idle, Error, WorktreeCreate, AgentList, SessionEnd}

// Known reports whether k is one of Kinds.
func (k Kind) Known() bool { return slices.Contains(Kinds, k) }

// Values of Event.Source for a needs_you event: what raised it.
const (
	SourceToolCheck         = "tool_check"         // tool.check answered "ask"
	SourcePermissionRequest = "permission_request" // classic.PermissionRequest
	SourceAskUserQuestion   = "ask_user_question"  // tool.call on AskUserQuestion
	SourceExitPlanMode      = "exit_plan_mode"     // tool.call on ExitPlanMode
	SourceNotification      = "notification"       // classic.Notification
)

// Values of Event.Reason for a turn_complete (turn.complete's reason).
const (
	ReasonAnswer  = "answer"
	ReasonAborted = "aborted"
	ReasonRefusal = "refusal"
	ReasonError   = "error"
)

// ReasonClear is a session_end's reason when /clear started a new session
// id in the same process, and SourceClear the following session_start's
// source.
const (
	ReasonClear = "clear"
	SourceClear = "clear"
)

// PersonOrigins are the prompt.submit origin kinds that are the person:
// only a prompt from one of these clears needs-you. Task notifications,
// peer messages and other origins don't.
var PersonOrigins = []string{"composer", "sdk", "bridge"}

// FromPerson reports whether a prompt's origin_kind is the person.
func FromPerson(originKind string) bool { return slices.Contains(PersonOrigins, originKind) }

// Event is one event. Kind, SessionID and At are required on every event;
// AgentID is set when it happened in a subagent's (or teammate's) loop and
// empty in the session's main loop. The other fields belong to particular
// kinds, as their comments say, and are ignored on the others.
type Event struct {
	Kind      Kind      `json:"kind"`
	SessionID string    `json:"session_id"`
	At        time.Time `json:"at"`
	AgentID   string    `json:"agent_id,omitempty"`
	// Cwd is the loop's working directory when the event happened
	// ($.session.cwd() in the hook), on session_start, turn_start and
	// tool. It is how aimd follows a session into and out of a worktree
	// and joins a subagent to its worktree. Leave it out where it isn't
	// reliable (turn_complete).
	Cwd string `json:"cwd,omitempty"`

	// session_start.
	Repo           string `json:"repo,omitempty"`             // main checkout root, $.session.repo().root
	Worktree       string `json:"worktree,omitempty"`         // worktree root, $.session.root()
	Branch         string `json:"branch,omitempty"`           // git rev-parse --abbrev-ref HEAD
	Host           string `json:"host,omitempty"`             // hostname
	ItermSessionID string `json:"iterm_session_id,omitempty"` // $ITERM_SESSION_ID
	Interactive    *bool  `json:"interactive,omitempty"`      // session.start's isInteractive
	// PrevSessionID, on a session_start whose Source is "clear", is the
	// session id before the /clear.
	PrevSessionID string `json:"prev_session_id,omitempty"`
	// Background, on session_start: a background session (`claude --bg`,
	// an Agent View dispatch; the mod reads CLAUDE_CODE_SESSION_KIND=bg).
	// Those say isInteractive too, so Interactive alone can't tell.
	Background *bool `json:"background,omitempty"`

	// Source: on session_start, classic.SessionStart's source (startup,
	// resume, clear, compact); on needs_you, what raised it (Source*).
	Source string `json:"source,omitempty"`

	// prompt: prompt.submit's origin.kind.
	OriginKind string `json:"origin_kind,omitempty"`

	// Reason: on turn_complete, turn.complete's reason (Reason*); on
	// needs_you, what is being asked (e.g. the label of the tool waiting
	// for approval, or the notification type); on error, the error (e.g.
	// "rate_limit"); on session_end, session.end's reason.
	Reason string `json:"reason,omitempty"`
	// LastSaid, on turn_complete: the first line of turn.complete's
	// answer, at most 300 characters.
	LastSaid string `json:"last_said,omitempty"`

	// tool: the tool's name and the last-action label ("Bash: run the
	// tests", "Edit internal/x.go").
	Tool  string `json:"tool,omitempty"`
	Label string `json:"label,omitempty"`
	// ToolUseID, on tool and needs_you: the tool call's tool_use_id. A
	// tool event clears a needs_you in the same loop only when their ids
	// match (or either has none), so a parallel call that finishes while
	// another waits for approval doesn't clear it.
	ToolUseID string `json:"tool_use_id,omitempty"`

	// Wait, on needs_you: what the loop waits on, when the
	// mod could tell: the tool and a one-line summary of its input, the
	// questions and their options, or the plan's first lines. aimd keeps it
	// in memory while the ask is pending and never stores it.
	Wait *Wait `json:"wait,omitempty"`

	// worktree_create: classic.WorktreeCreate's name ("agent-<agentId>"
	// for a worktree-isolated agent).
	Name string `json:"name,omitempty"`

	// agent_list: the $.agent.list() snapshot.
	Agents []Agent `json:"agents,omitempty"`

	// NewSessionID, on a session_end whose Reason is "clear": the new
	// session id, when the mod knows it.
	NewSessionID string `json:"new_session_id,omitempty"`
}

// Agent is one item of an agent_list snapshot.
type Agent struct {
	ID     string `json:"id"`
	Status string `json:"status"` // pending | running | waiting | idle | completed | failed | killed
	Type   string `json:"type,omitempty"`
	// Background is agent.spawn's background flag, from the mod's own map;
	// left out when the mod doesn't know.
	Background *bool `json:"background,omitempty"`
	// ParentID is the agent that spawned it ($.agent.list()'s parentId,
	// agent.spawn's parentAgentId); empty for the main loop.
	ParentID string `json:"parent_id,omitempty"`
}

// Wait is what a needs_you waits on: a permission's tool and
// a summary of its input, an AskUserQuestion's questions, or the plan an
// ExitPlanMode asks to approve.
type Wait struct {
	Kind string `json:"kind"` // WaitPermission, WaitQuestion or WaitPlan
	// Tool is the tool asked about.
	Tool string `json:"tool,omitempty"`
	// Summary, for a permission: one line saying what the call does (the
	// Bash command, the file path, the URL), at most MaxSummaryLen, with
	// secrets redacted (Redact: by the mod, and again by Normalize).
	Summary string `json:"summary,omitempty"`
	// Questions, for a question: what AskUserQuestion asks.
	Questions []Question `json:"questions,omitempty"`
	// Plan, for a plan approval: the plan's first lines (MaxPlanLines,
	// MaxPlanLen).
	Plan string `json:"plan,omitempty"`
}

// Question is one question of an AskUserQuestion call.
type Question struct {
	Header      string   `json:"header,omitempty"` // the short chip, e.g. "Library"
	Question    string   `json:"question"`
	Options     []string `json:"options,omitempty"` // the option labels
	MultiSelect bool     `json:"multi_select,omitempty"`
}

// Wait kinds.
const (
	WaitPermission = "permission"
	WaitQuestion   = "question"
	WaitPlan       = "plan"
)

// normalize redacts a wait's text (Redact: the mod did, but an older mod
// or another sender may not have) and truncates it to its caps, in place.
// It reports false for a wait aimd can't show: an unknown kind, or no
// detail for its kind.
func (w *Wait) normalize() bool {
	w.Kind = strings.TrimSpace(w.Kind)
	w.Tool = Truncate(strings.TrimSpace(w.Tool), MaxToolLen)
	w.Summary = RedactLine(w.Summary, MaxSummaryLen)
	w.Plan = PlanLines(Redact(PlanLines(w.Plan)))
	if len(w.Questions) > MaxWaitQuestions {
		w.Questions = w.Questions[:MaxWaitQuestions]
	}
	var qs []Question
	for _, q := range w.Questions {
		q.Header = RedactLine(q.Header, MaxKindLen)
		q.Question = RedactLine(q.Question, MaxSummaryLen)
		var opts []string
		for _, o := range q.Options {
			if o = RedactLine(o, MaxOptionLen); o != "" && len(opts) < MaxWaitOptions {
				opts = append(opts, o)
			}
		}
		q.Options = opts
		if q.Question != "" || len(q.Options) > 0 {
			qs = append(qs, q)
		}
	}
	w.Questions = qs
	switch w.Kind {
	case WaitPermission:
		w.Questions, w.Plan = nil, ""
		return w.Tool != "" || w.Summary != ""
	case WaitQuestion:
		w.Summary, w.Plan = "", ""
		return len(w.Questions) > 0
	case WaitPlan:
		w.Summary, w.Questions = "", nil
		return w.Plan != ""
	}
	return false
}

// RedactLine is s on one line, redacted and cut to n characters. It is cut
// to 1000 first, so a huge value costs little to redact.
func RedactLine(s string, n int) string {
	return Truncate(OneLine(Redact(OneLine(Truncate(s, 1000)))), n)
}

// PlanLines cuts a plan to what a wait keeps: its first MaxPlanLines
// lines and MaxPlanLen characters, with blank lines at either end and
// trailing spaces dropped.
func PlanLines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(strings.Trim(s, "\n \t"), "\n")
	if len(lines) > MaxPlanLines {
		lines = lines[:MaxPlanLines]
	}
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	return strings.TrimRight(Truncate(strings.Join(lines, "\n"), MaxPlanLen), "\n \t")
}

// Agent statuses ($.agent.list()).
const (
	AgentPending   = "pending"
	AgentRunning   = "running"
	AgentWaiting   = "waiting"
	AgentIdle      = "idle"
	AgentCompleted = "completed"
	AgentFailed    = "failed"
	AgentKilled    = "killed"
)

// AgentEnded reports whether an agent status is final.
func AgentEnded(status string) bool {
	return status == AgentCompleted || status == AgentFailed || status == AgentKilled
}

// Errors from Normalize, and what DecodeBatch counts.
var (
	ErrUnknownKind = errors.New("unknown event kind")
	ErrInvalid     = errors.New("invalid event")
)

// Normalize checks the common fields and truncates every field to its cap.
// It returns ErrUnknownKind (wrapped) for a kind it doesn't know and
// ErrInvalid (wrapped) for an event without a session id or time. LastSaid
// is also cut at its first line break.
func (e *Event) Normalize() error {
	e.Kind = Kind(strings.TrimSpace(string(e.Kind)))
	if !e.Kind.Known() {
		return fmt.Errorf("%w %q", ErrUnknownKind, Truncate(string(e.Kind), MaxKindLen))
	}
	e.SessionID = Truncate(strings.TrimSpace(e.SessionID), MaxToolLen)
	if e.SessionID == "" {
		return fmt.Errorf("%w: %s without session_id", ErrInvalid, e.Kind)
	}
	if e.At.IsZero() {
		return fmt.Errorf("%w: %s without at", ErrInvalid, e.Kind)
	}
	e.AgentID = Truncate(strings.TrimSpace(e.AgentID), MaxToolLen)
	e.Cwd = Truncate(e.Cwd, MaxPathLen)
	e.Repo = Truncate(e.Repo, MaxPathLen)
	e.Worktree = Truncate(e.Worktree, MaxPathLen)
	e.Branch = Truncate(e.Branch, MaxPathLen)
	e.Host = Truncate(e.Host, MaxToolLen)
	e.ItermSessionID = Truncate(e.ItermSessionID, MaxToolLen)
	e.PrevSessionID = Truncate(strings.TrimSpace(e.PrevSessionID), MaxToolLen)
	e.Source = Truncate(e.Source, MaxKindLen)
	e.OriginKind = Truncate(e.OriginKind, MaxKindLen)
	e.Reason = Truncate(OneLine(e.Reason), MaxSummaryLen)
	e.LastSaid = Truncate(FirstLine(e.LastSaid), MaxSummaryLen)
	e.Tool = Truncate(e.Tool, MaxToolLen)
	e.Label = Truncate(OneLine(e.Label), MaxSummaryLen)
	e.ToolUseID = Truncate(strings.TrimSpace(e.ToolUseID), MaxToolLen)
	e.Name = Truncate(e.Name, MaxToolLen)
	e.NewSessionID = Truncate(strings.TrimSpace(e.NewSessionID), MaxToolLen)
	if e.Wait != nil {
		// A copy: the caller's event keeps its own slices.
		w := *e.Wait
		w.Questions = append([]Question(nil), w.Questions...)
		e.Wait = nil
		if e.Kind == NeedsYou && w.normalize() {
			e.Wait = &w
		}
	}
	if len(e.Agents) > MaxAgents {
		e.Agents = e.Agents[:MaxAgents]
	}
	agents := e.Agents[:0]
	for _, a := range e.Agents {
		a.ID = Truncate(strings.TrimSpace(a.ID), MaxToolLen)
		if a.ID == "" {
			continue
		}
		a.Status = Truncate(a.Status, MaxKindLen)
		a.Type = Truncate(a.Type, MaxKindLen)
		a.ParentID = Truncate(strings.TrimSpace(a.ParentID), MaxToolLen)
		agents = append(agents, a)
	}
	e.Agents = agents
	return nil
}

// Truncate cuts s to at most n runes. Invalid UTF-8 is replaced first, so
// the result is always valid.
func Truncate(s string, n int) string {
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i, count := 0, 0
	for i = range s {
		if count == n {
			break
		}
		count++
	}
	return s[:i]
}

// FirstLine returns s up to its first line break, trimmed.
func FirstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// OneLine folds every run of whitespace (line breaks included) into one
// space and trims the ends.
func OneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// Batch is a decoded POST body: the events that passed Normalize, in the
// order sent, and what was dropped.
type Batch struct {
	Events []Event
	// Unknown counts events of a kind this package doesn't know, by kind.
	Unknown map[string]int
	// Invalid counts events that couldn't be read or lacked a session id
	// or time.
	Invalid int
}

// Dropped returns how many events the batch dropped.
func (b Batch) Dropped() int {
	n := b.Invalid
	for _, c := range b.Unknown {
		n += c
	}
	return n
}

// Errors from DecodeBatch for a body refused as a whole.
var (
	ErrTooManyEvents = fmt.Errorf("events: more than %d events in one batch", MaxBatchEvents)
	ErrMalformed     = errors.New("events: body is not {\"events\": [...]}")
)

// wireBatch is the body's shape. Events are decoded one by one, so one bad
// event doesn't spoil the rest.
type wireBatch struct {
	Events []json.RawMessage `json:"events"`
}

// DecodeBatch reads a batch from r. It refuses the whole body
// (ErrMalformed, ErrTooManyEvents, or the reader's error, such as
// *http.MaxBytesError) only when it isn't a batch at all or has too many
// events. Within the batch, an event that can't be decoded or fails
// Normalize is dropped and counted.
func DecodeBatch(r io.Reader) (Batch, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return Batch{}, err
	}
	var wb wireBatch
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&wb); err != nil {
		return Batch{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if wb.Events == nil {
		return Batch{}, ErrMalformed
	}
	if len(wb.Events) > MaxBatchEvents {
		return Batch{}, ErrTooManyEvents
	}
	b := Batch{Events: make([]Event, 0, len(wb.Events))}
	for _, raw := range wb.Events {
		var e Event
		if err := json.Unmarshal(raw, &e); err != nil {
			// A bad field (a time that isn't RFC 3339, say). Keep the kind
			// for the unknown count when it can be read.
			var k struct {
				Kind Kind `json:"kind"`
			}
			if json.Unmarshal(raw, &k) == nil && k.Kind != "" && !k.Kind.Known() {
				b.unknown(string(k.Kind))
			} else {
				b.Invalid++
			}
			continue
		}
		switch err := e.Normalize(); {
		case errors.Is(err, ErrUnknownKind):
			b.unknown(string(e.Kind))
		case err != nil:
			b.Invalid++
		default:
			b.Events = append(b.Events, e)
		}
	}
	return b, nil
}

func (b *Batch) unknown(kind string) {
	if b.Unknown == nil {
		b.Unknown = map[string]int{}
	}
	b.Unknown[Truncate(kind, MaxKindLen)]++
}
