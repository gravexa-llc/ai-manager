package sessions

import (
	"bytes"
	"maps"
	"slices"
	"time"
)

// EventKind is what happened to a session between two snapshots.
type EventKind string

// Event kinds.
const (
	Appeared EventKind = "appeared"
	Changed  EventKind = "changed"
	Gone     EventKind = "gone"
)

// Event is one difference between two snapshots.
type Event struct {
	Kind EventKind
	Key  Key
	// At is the newer snapshot's time.
	At time.Time
	// Before is the session in the older snapshot; zero for Appeared.
	Before Session
	// After is the session in the newer snapshot; zero for Gone.
	After Session
	// Fields lists, for Changed, the JSON names of the fields that differ:
	// the known ones in a fixed order ("id", "pid", "sessionId", "name",
	// "cwd", "kind", "startedAt", "status", "waitingFor", "state"), then
	// any in Session.Extra, sorted.
	Fields []string
}

// VerdictChanged reports whether the event changes the session's verdict.
// It is true for every Appeared and Gone event.
func (e Event) VerdictChanged() bool {
	if e.Kind != Changed {
		return true
	}
	return e.Before.Verdict() != e.After.Verdict()
}

// Diff compares two snapshots by Session.Key. Changed and Appeared events
// come first, in next's order; Gone events follow, in prev's order.
//
// Both snapshots should be listed the same way (Snapshot.All): without
// `--all`, a finished background job drops out of the listing and is
// reported as gone; with it, the job stays and its state changes.
func Diff(prev, next Snapshot) []Event {
	before := make(map[Key]Session, len(prev.Sessions))
	for _, s := range prev.Sessions {
		k := s.Key()
		if _, dup := before[k]; !dup {
			before[k] = s
		}
	}

	var events []Event
	seen := make(map[Key]bool, len(next.Sessions))
	for _, s := range next.Sessions {
		k := s.Key()
		if seen[k] {
			continue
		}
		seen[k] = true
		old, ok := before[k]
		if !ok {
			events = append(events, Event{Kind: Appeared, Key: k, At: next.At, After: s})
			continue
		}
		if fields := changedFields(old, s); len(fields) > 0 {
			events = append(events, Event{Kind: Changed, Key: k, At: next.At, Before: old, After: s, Fields: fields})
		}
	}
	for _, s := range prev.Sessions {
		k := s.Key()
		if seen[k] {
			continue
		}
		seen[k] = true // a duplicate in prev goes once
		events = append(events, Event{Kind: Gone, Key: k, At: next.At, Before: s})
	}
	return events
}

// changedFields returns the JSON names of the fields that differ.
func changedFields(a, b Session) []string {
	var out []string
	add := func(name string, differ bool) {
		// A known field with an unexpected type sits in Extra too; name it once.
		if differ && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	add("id", a.JobID != b.JobID)
	add("pid", a.PID != b.PID)
	add("sessionId", a.SessionID != b.SessionID)
	add("name", a.Name != b.Name)
	add("cwd", a.Cwd != b.Cwd)
	add("kind", a.Kind != b.Kind)
	add("startedAt", !a.StartedAt.Equal(b.StartedAt))
	add("status", a.Status != b.Status)
	add("waitingFor", a.WaitingFor != b.WaitingFor)
	add("state", a.State != b.State)

	names := make(map[string]bool, len(a.Extra)+len(b.Extra))
	for n := range maps.Keys(a.Extra) {
		names[n] = true
	}
	for n := range maps.Keys(b.Extra) {
		names[n] = true
	}
	for _, n := range slices.Sorted(maps.Keys(names)) {
		va, inA := a.Extra[n]
		vb, inB := b.Extra[n]
		add(n, inA != inB || !bytes.Equal(bytes.TrimSpace(va), bytes.TrimSpace(vb)))
	}
	return out
}
