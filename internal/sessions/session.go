// Package sessions lists Claude Code sessions with `claude agents --json`,
// normalizes what it reports, and diffs one listing against the next.
//
// It is a library with no daemon of its own. The pieces are:
//
//   - [Lister] runs `claude agents --json` (plus `--all` by default) through
//     a [Runner], with a timeout and CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1.
//   - [Parse] reads its output into [Session] values without failing on
//     fields or values it doesn't know.
//   - [Session.Verdict] maps the raw `status` / `state` pair onto the session
//     states (State), including one needs-you verdict with its reason.
//   - [Diff] compares two snapshots by [Session.Key] and reports appeared,
//     changed and gone sessions.
//   - [Poller] runs the lister on an interval the caller can change at any
//     time, plus a debounced [Poller.Kick] for an immediate poll.
//   - [WatchDirs] and [Watch] turn file changes in Claude Code's session
//     registry and job store into kicks.
//
// Field meanings and values are as Claude Code 2.1.318 writes them; they
// aren't a documented API. The CLI's JSON is the parser of record; the files under
// ~/.claude are only watched, never parsed.
package sessions

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Kind is an entry's `kind`. Values other than the constants are kept as
// listed.
type Kind string

// Known kinds.
const (
	Interactive Kind = "interactive"
	Background  Kind = "background"
)

// Status is a live process's `status`. Interactive sessions always have
// one; background sessions have one while their process runs.
type Status string

// Known statuses. The CLI maps any other raw process status to busy, so
// these three should be all there is; anything else is kept as listed.
const (
	StatusIdle    Status = "idle"
	StatusBusy    Status = "busy"
	StatusWaiting Status = "waiting" // see Session.WaitingFor
)

// JobState is a background job's lifecycle `state`.
type JobState string

// Known job states.
const (
	JobWorking JobState = "working"
	JobBlocked JobState = "blocked" // ended its turn asking for a reply, or its process is waiting
	JobDone    JobState = "done"
	JobFailed  JobState = "failed"
	JobStopped JobState = "stopped"
)

// Known `waitingFor` values, present only with StatusWaiting. All of them
// mean a dialog is open in the session.
const (
	WaitingPermission = "permission prompt" // the CLI's default for a queued dialog
	WaitingInput      = "input needed"      // an elicitation or a question
	WaitingDialog     = "dialog open"
	WaitingSandbox    = "sandbox request"
	WaitingGoal       = "goal proposal"
)

// Session is one entry of `claude agents --json`. A field the entry
// doesn't have is left at its zero value.
type Session struct {
	// JobID is `id`, the background job id (`~/.claude/jobs/<id>/`). It is
	// stable over the job's life and is what background sessions are keyed
	// on. Empty for interactive sessions and for background processes that
	// have no job.
	JobID string

	// PID is `pid`: every interactive session, plus background sessions
	// whose process is still running.
	PID int

	// SessionID is `sessionId`. For a background job it changes when the
	// job is respawned, so it can differ from JobID.
	SessionID string

	// Name is `name`. For background jobs it can be the user's own words
	// (the job's intent).
	Name string

	// Cwd is `cwd`, an absolute path.
	Cwd string

	// Kind is `kind`.
	Kind Kind

	// StartedAt is `startedAt` (epoch milliseconds in the JSON). Zero when
	// absent or unreadable.
	StartedAt time.Time

	// Status is the process's `status`. Empty when there is no live process.
	Status Status

	// WaitingFor is `waitingFor`, the reason for StatusWaiting.
	WaitingFor string

	// State is a background job's `state`. Empty for entries with no job.
	State JobState

	// Extra holds every field this package doesn't know, verbatim, plus
	// any known field whose JSON type was unexpected.
	Extra map[string]json.RawMessage
}

// Key identifies a session across polls. Background sessions with a job
// are keyed on the job id ("job:<id>"), never on sessionId, which changes
// when the job is respawned. Everything else is keyed on its process id
// ("pid:<pid>"), which is what Claude Code's own registry is keyed on, so a
// sessionId change in the same process (after a /clear, say) is reported
// as a change, not as one session going and another appearing. A pid
// reused by a new process between two polls would look like a change too;
// at poll intervals of a minute or less that is very unlikely.
type Key string

// Key returns the session's stable key.
func (s Session) Key() Key {
	switch {
	case s.JobID != "":
		return Key("job:" + s.JobID)
	case s.PID > 0:
		return Key("pid:" + strconv.Itoa(s.PID))
	case s.SessionID != "":
		// Not seen: every entry without a job has a pid.
		return Key("session:" + s.SessionID)
	default:
		ms := int64(0)
		if !s.StartedAt.IsZero() {
			ms = s.StartedAt.UnixMilli()
		}
		return Key(fmt.Sprintf("entry:%s:%s@%d", s.Kind, s.Cwd, ms))
	}
}

// Active reports whether `claude agents --json` would list the session
// without `--all`: every interactive session, and background sessions
// whose process is alive or whose state is working or blocked. The rest
// are finished jobs that only `--all` lists.
func (s Session) Active() bool {
	if s.Kind != Background {
		return true
	}
	return s.PID > 0 || s.State == JobWorking || s.State == JobBlocked
}

// State is a session state. The JSON alone can produce every state except
// quiet, which needs mod events.
type State string

// Session states, in priority order, plus StateUnknown.
const (
	StateNeedsYou State = "needs_you"
	StateError    State = "error"
	StateWorking  State = "working"
	StateTurnDone State = "turn_done"
	StateIdle     State = "idle"
	StateGone     State = "gone"
	// StateUnknown is a status or state value this package doesn't know.
	// Verdict.Reason holds the raw value.
	StateUnknown State = "unknown"
)

// Need says what a needs-you session is waiting for.
type Need string

// Needs.
const (
	// NeedPrompt: a dialog is open in the session (a permission prompt, a
	// question, a sandbox request, a goal proposal). It came from
	// `status: waiting`; Verdict.Reason is the `waitingFor` text.
	NeedPrompt Need = "prompt"
	// NeedReply: a background job ended its turn asking for a reply
	// (`state: blocked` without `status: waiting`). This is not a
	// permission prompt. Verdict.Reason is ReasonReply.
	NeedReply Need = "reply"
)

// Reasons the verdict gives when no raw value says it better.
const (
	ReasonReply   = "reply"   // NeedReply
	ReasonWaiting = "waiting" // `status: waiting` with no `waitingFor`
)

// Verdict is a session's normalized state from one listing.
type Verdict struct {
	State State
	// Need is set only when State is StateNeedsYou.
	Need Need
	// Reason says why. For NeedPrompt it is the `waitingFor` text (or
	// ReasonWaiting); for NeedReply it is ReasonReply. Otherwise it is the
	// raw value that decided: "busy", "idle", "working", "done", "failed",
	// "stopped", or an unknown value as listed.
	Reason string
}

// NeedsYou reports whether the session needs you.
func (v Verdict) NeedsYou() bool { return v.State == StateNeedsYou }

func (v Verdict) String() string {
	switch {
	case v.Need != "":
		return fmt.Sprintf("%s (%s: %s)", v.State, v.Need, v.Reason)
	case v.Reason != "":
		return fmt.Sprintf("%s (%s)", v.State, v.Reason)
	default:
		return string(v.State)
	}
}

// Verdict maps the entry's `status` and `state` onto a session state:
//
//	status: waiting (either kind)   needs_you, prompt, reason = waitingFor
//	state: blocked                  needs_you, reply
//	state: failed                   error
//	state: working / status: busy   working
//	state: done                     turn_done
//	state: stopped                  gone (stopped by the user)
//	status: idle                    idle (the JSON can't tell turn_done from idle)
//
// `status: waiting` is checked first, so a background job blocked on a
// permission dialog is a prompt, not a reply. Otherwise a background job's
// state decides, and the process status only decides for entries with no
// job. An unknown state still yields to a busy process; any other unknown
// value gives StateUnknown with the raw value as the reason.
func (s Session) Verdict() Verdict {
	if s.Status == StatusWaiting {
		reason := s.WaitingFor
		if reason == "" {
			reason = ReasonWaiting
		}
		return Verdict{State: StateNeedsYou, Need: NeedPrompt, Reason: reason}
	}
	switch s.State {
	case "":
		// No job: the process status decides.
	case JobBlocked:
		return Verdict{State: StateNeedsYou, Need: NeedReply, Reason: ReasonReply}
	case JobFailed:
		return Verdict{State: StateError, Reason: string(s.State)}
	case JobWorking:
		return Verdict{State: StateWorking, Reason: string(s.State)}
	case JobDone:
		return Verdict{State: StateTurnDone, Reason: string(s.State)}
	case JobStopped:
		return Verdict{State: StateGone, Reason: string(s.State)}
	default:
		if s.Status == StatusBusy {
			return Verdict{State: StateWorking, Reason: string(s.Status)}
		}
		return Verdict{State: StateUnknown, Reason: string(s.State)}
	}
	switch s.Status {
	case StatusBusy:
		return Verdict{State: StateWorking, Reason: string(s.Status)}
	case StatusIdle:
		return Verdict{State: StateIdle, Reason: string(s.Status)}
	default:
		return Verdict{State: StateUnknown, Reason: string(s.Status)}
	}
}

// Snapshot is one listing.
type Snapshot struct {
	// At is when the listing was requested. Parse leaves it zero.
	At time.Time
	// All reports whether `--all` was passed, so finished background jobs
	// are included. Parse leaves it false.
	All bool
	// Sessions are the entries in the order listed (by startedAt).
	Sessions []Session
	// Warnings describe input that was skipped or not understood. They
	// never stop a parse.
	Warnings []string
}

// Parse reads the output of `claude agents --json`.
//
// It fails only when there is no JSON array to read, so that a broken
// call can never look like every session going away. Inside the array it
// is lenient: unknown fields go to Session.Extra, unknown values are kept
// as strings, and entries or fields it can't read are skipped with a
// warning. Text before the array (a stray notice on stdout) is skipped
// with a warning too.
func Parse(data []byte) (Snapshot, error) {
	var snap Snapshot

	data = bytes.TrimSpace(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")))
	if len(data) == 0 {
		return snap, fmt.Errorf("sessions: empty output, want a JSON array")
	}
	if data[0] != '[' {
		i := bytes.Index(data, []byte("\n["))
		if i < 0 {
			return snap, fmt.Errorf("sessions: output is not a JSON array: %q", clip(data, 80))
		}
		snap.Warnings = append(snap.Warnings, fmt.Sprintf("skipped %d bytes before the JSON array", i+1))
		data = data[i+1:]
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	var raw []json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return Snapshot{}, fmt.Errorf("sessions: read JSON array: %w", err)
	}
	if rest := bytes.TrimSpace(data[dec.InputOffset():]); len(rest) > 0 {
		snap.Warnings = append(snap.Warnings, fmt.Sprintf("ignored %d bytes after the JSON array", len(rest)))
	}

	snap.Sessions = make([]Session, 0, len(raw))
	seen := make(map[Key]int, len(raw))
	for i, elem := range raw {
		s, warns, ok := parseEntry(elem)
		for _, w := range warns {
			snap.Warnings = append(snap.Warnings, fmt.Sprintf("entry %d: %s", i, w))
		}
		if !ok {
			continue
		}
		k := s.Key()
		if j, dup := seen[k]; dup {
			snap.Warnings = append(snap.Warnings, fmt.Sprintf("entry %d: duplicate key %s (first at entry %d); skipped", i, k, j))
			continue
		}
		seen[k] = i
		snap.Sessions = append(snap.Sessions, s)
	}
	return snap, nil
}

// parseEntry reads one array element. ok is false when the element isn't
// an object.
func parseEntry(elem json.RawMessage) (s Session, warns []string, ok bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(elem, &fields); err != nil || fields == nil {
		return Session{}, []string{fmt.Sprintf("not an object: %s; skipped", clip(elem, 60))}, false
	}

	// keep records a known field that couldn't be read, verbatim.
	keep := func(name string, v json.RawMessage, why string) {
		if s.Extra == nil {
			s.Extra = map[string]json.RawMessage{}
		}
		s.Extra[name] = v
		warns = append(warns, fmt.Sprintf("%s: %s: %s", name, why, clip(v, 60)))
	}
	str := func(name string, v json.RawMessage) string {
		got, ok := asString(v)
		if !ok {
			keep(name, v, "not a string")
		}
		return got
	}

	// Sorted, so warnings come out in a stable order.
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		v := fields[name]
		switch name {
		case "id":
			s.JobID = str(name, v)
		case "pid":
			pid, ok := asInt(v)
			if !ok {
				keep(name, v, "not an integer")
			}
			s.PID = pid
		case "sessionId":
			s.SessionID = str(name, v)
		case "name":
			s.Name = str(name, v)
		case "cwd":
			s.Cwd = str(name, v)
		case "kind":
			s.Kind = Kind(str(name, v))
		case "startedAt":
			t, ok := asTime(v)
			if !ok {
				keep(name, v, "not epoch milliseconds or a timestamp")
			}
			s.StartedAt = t
		case "status":
			s.Status = Status(str(name, v))
		case "waitingFor":
			s.WaitingFor = str(name, v)
		case "state":
			s.State = JobState(str(name, v))
		default:
			if s.Extra == nil {
				s.Extra = map[string]json.RawMessage{}
			}
			s.Extra[name] = v
		}
	}
	return s, warns, true
}

// asString reads a JSON string. null reads as "". A number or boolean
// reads as its JSON text, so an id that turns numeric isn't lost.
func asString(v json.RawMessage) (string, bool) {
	v = bytes.TrimSpace(v)
	if len(v) == 0 || string(v) == "null" {
		return "", true
	}
	switch v[0] {
	case '"':
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return "", false
		}
		return s, true
	case '{', '[':
		return "", false
	default:
		return string(v), true
	}
}

// asInt reads a JSON integer, or a string holding one. null reads as 0.
func asInt(v json.RawMessage) (int, bool) {
	n, ok := asInt64(v)
	if !ok || n < math.MinInt || n > math.MaxInt {
		return 0, false
	}
	return int(n), true
}

func asInt64(v json.RawMessage) (int64, bool) {
	v = bytes.TrimSpace(v)
	if len(v) == 0 || string(v) == "null" {
		return 0, true
	}
	text := string(v)
	if v[0] == '"' {
		if err := json.Unmarshal(v, &text); err != nil {
			return 0, false
		}
		text = strings.TrimSpace(text)
	}
	if n, err := strconv.ParseInt(text, 10, 64); err == nil {
		return n, true
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil || f != math.Trunc(f) || math.Abs(f) > 1<<53 {
		return 0, false
	}
	return int64(f), true
}

// asTime reads epoch milliseconds (a number or a numeric string), or an
// RFC 3339 timestamp. null reads as the zero time.
func asTime(v json.RawMessage) (time.Time, bool) {
	v = bytes.TrimSpace(v)
	if len(v) == 0 || string(v) == "null" {
		return time.Time{}, true
	}
	text := string(v)
	if v[0] == '"' {
		if err := json.Unmarshal(v, &text); err != nil {
			return time.Time{}, false
		}
		text = strings.TrimSpace(text)
	} else if v[0] == '{' || v[0] == '[' {
		return time.Time{}, false
	}
	if ms, err := strconv.ParseInt(text, 10, 64); err == nil {
		return time.UnixMilli(ms), true
	}
	if f, err := strconv.ParseFloat(text, 64); err == nil && !math.IsInf(f, 0) && !math.IsNaN(f) {
		return time.UnixMilli(int64(f)), true
	}
	t, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// clip shortens b for an error message.
func clip(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
