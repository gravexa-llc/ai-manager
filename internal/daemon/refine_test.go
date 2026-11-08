package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/sessions"
	"github.com/gravexa-llc/ai-manager/internal/store"
)

// RefineSessions sees a poll's rows before they're written, and what it
// returns is what gets written: changed rows as changed, dropped rows not
// at all.
func TestRefineSessionsHook(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	r.d.runCtx = ctx
	now := time.Now().Truncate(time.Millisecond)
	var seen []store.Session
	var seenAt time.Time
	r.d.refine = func(_ context.Context, rows []store.Session, at time.Time) []store.Session {
		seen, seenAt = append([]store.Session(nil), rows...), at
		var out []store.Session
		for _, row := range rows {
			switch row.SessionID {
			case "s1":
				row.State, row.StateReason = "turn_done", "answer"
				out = append(out, row)
			case "s2": // the mod said it ended: dropped
			default:
				out = append(out, row)
			}
		}
		return out
	}
	snap := sessions.Snapshot{At: now, All: true, Sessions: []sessions.Session{
		{PID: 1, SessionID: "s1", Cwd: "/r/a", Kind: sessions.Interactive, StartedAt: now.Add(-time.Hour), Status: sessions.StatusIdle},
		{PID: 2, SessionID: "s2", Cwd: "/r/a", Kind: sessions.Interactive, StartedAt: now.Add(-time.Hour), Status: sessions.StatusBusy},
		{PID: 3, SessionID: "s3", Cwd: "/r/a", Kind: sessions.Interactive, StartedAt: now.Add(-time.Hour), Status: sessions.StatusBusy},
	}}
	r.d.onSessions(sessions.Result{Snapshot: snap, Events: sessions.Diff(sessions.Snapshot{}, snap)})

	if len(seen) != 3 || !seenAt.Equal(now) {
		t.Fatalf("refine saw %d rows at %v, want 3 at %v", len(seen), seenAt, now)
	}
	live := r.st.liveSessions()
	if s := live["pid:1"]; s.State != "turn_done" || s.StateReason != "answer" {
		t.Errorf("refined row = %+v", s)
	}
	if _, ok := live["pid:2"]; ok {
		t.Error("a dropped row was written")
	}
	if s := live["pid:3"]; s.State != "working" {
		t.Errorf("untouched row = %+v", s)
	}
}

// A row the event receiver created after the poll's snapshot isn't ended
// by that poll, though the poll doesn't list it; an older one is.
func TestPollKeepsRowsNewerThanItsSnapshot(t *testing.T) {
	r := newRig(t, nil)
	ctx := context.Background()
	r.d.runCtx = ctx
	now := time.Now().Truncate(time.Millisecond)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(r.st.UpsertSessions(ctx,
		store.Session{Key: "session:new", SessionID: "new", FirstSeenAt: now.Add(time.Second), LastSeenAt: now.Add(time.Second)},
		store.Session{Key: "session:old", SessionID: "old", FirstSeenAt: now.Add(-time.Minute), LastSeenAt: now.Add(-time.Minute)},
	))
	snap := sessions.Snapshot{At: now, All: true}
	r.d.onSessions(sessions.Result{Snapshot: snap})
	live := r.st.liveSessions()
	if _, ok := live["session:new"]; !ok {
		t.Error("the row created after the snapshot was ended")
	}
	if _, ok := live["session:old"]; ok {
		t.Error("the row from before the snapshot is still live")
	}
}
