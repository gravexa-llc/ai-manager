package store

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestEventsRoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	var in []Event
	for i := range 5 {
		in = append(in, Event{SessionID: "s1", AgentID: "a1", Event: "tool.call", ToolName: "Bash", Summary: "Bash: step", At: at(time.Duration(i) * time.Second)})
	}
	in = append(in, Event{SessionID: "s2", Event: "session.end"}) // At zero: now
	must(t, s.InsertEvents(ctx, in...))

	got, err := s.ListSessionEvents(ctx, "s1", 3)
	must(t, err)
	if len(got) != 3 {
		t.Fatalf("got %d events, want 3", len(got))
	}
	for i, e := range got {
		want := in[4-i] // newest first
		want.ID = e.ID
		equal(t, "event", e, want)
		if e.ID == 0 {
			t.Error("event ID not set")
		}
	}

	other, err := s.ListSessionEvents(ctx, "s2", 0)
	must(t, err)
	if len(other) != 1 || other[0].At != t0 || other[0].Event != "session.end" {
		t.Errorf("s2 events = %+v", other)
	}
}

func TestListSessionEventsTiesNewestInsertFirst(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(t, s.InsertEvents(ctx,
		Event{SessionID: "s", Event: "first", At: at(0)},
		Event{SessionID: "s", Event: "second", At: at(0)},
	))
	got, err := s.ListSessionEvents(ctx, "s", 10)
	must(t, err)
	if len(got) != 2 || got[0].Event != "second" || got[1].Event != "first" {
		t.Errorf("events at the same time = %+v, want second then first", got)
	}
}

func TestInsertEventsValidates(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	ok := Event{SessionID: "s", Event: "tool.call"}
	for name, e := range map[string]Event{
		"no session":    {Event: "tool.call"},
		"no event":      {SessionID: "s"},
		"long event":    {SessionID: "s", Event: strings.Repeat("e", MaxEventLen+1)},
		"long tool":     {SessionID: "s", Event: "tool.call", ToolName: strings.Repeat("t", MaxToolNameLen+1)},
		"long summary":  {SessionID: "s", Event: "tool.call", Summary: strings.Repeat("x", MaxSummaryLen+1)},
		"long unicode":  {SessionID: "s", Event: "tool.call", Summary: strings.Repeat("é", MaxSummaryLen+1)},
		"file contents": {SessionID: "s", Event: "tool.result", Summary: strings.Repeat("package main\n", 30)},
	} {
		if err := s.InsertEvents(ctx, ok, e); err == nil {
			t.Errorf("%s: InsertEvents succeeded", name)
		}
	}
	got, err := s.ListSessionEvents(ctx, "s", 0)
	must(t, err)
	if len(got) != 0 {
		t.Errorf("rejected batches left %d events", len(got))
	}

	// The limits count characters, not bytes.
	must(t, s.InsertEvents(ctx, Event{SessionID: "s", Event: "tool.call", Summary: strings.Repeat("é", MaxSummaryLen)}))
}

// The table itself refuses what InsertEvents would, so a later caller that
// writes SQL directly can't get around the limits.
func TestEventsTableChecks(t *testing.T) {
	s := newStore(t)
	for name, q := range map[string]string{
		"long summary": `INSERT INTO events (session_id, event, summary, at) VALUES ('s', 'e', '` + strings.Repeat("x", MaxSummaryLen+1) + `', 1)`,
		"long event":   `INSERT INTO events (session_id, event, at) VALUES ('s', '` + strings.Repeat("e", MaxEventLen+1) + `', 1)`,
		"long tool":    `INSERT INTO events (session_id, event, tool_name, at) VALUES ('s', 'e', '` + strings.Repeat("t", MaxToolNameLen+1) + `', 1)`,
		"no session":   `INSERT INTO events (session_id, event, at) VALUES ('', 'e', 1)`,
		"no event":     `INSERT INTO events (session_id, event, at) VALUES ('s', '', 1)`,
	} {
		if _, err := s.w.Exec(q); err == nil {
			t.Errorf("%s: the table accepted it", name)
		}
	}
}

// Events hold summaries only. This pins the table's columns so that a
// migration adding somewhere to put a prompt, file contents or tool output
// has to change this test, and say why.
func TestEventsTableHasNoPayloadColumn(t *testing.T) {
	s := newStore(t)
	rows, err := s.r.Query(`SELECT name FROM pragma_table_info('events') ORDER BY cid`)
	must(t, err)
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		must(t, rows.Scan(&c))
		cols = append(cols, c)
	}
	must(t, rows.Err())
	want := []string{"id", "session_id", "agent_id", "event", "tool_name", "summary", "at"}
	if !slices.Equal(cols, want) {
		t.Errorf("events columns = %q, want %q", cols, want)
	}
}

func TestPurgeEvents(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	s.purgeBatch = 3 // exercise several batches

	day := 24 * time.Hour
	var in []Event
	for i := range 10 { // 10 old events, 31 to 40 days back
		in = append(in, Event{SessionID: "s", Event: "old", At: at(-day*31 - time.Duration(i)*day)})
	}
	in = append(in,
		Event{SessionID: "s", Event: "edge", At: at(-day * 30)}, // exactly at the cutoff: kept
		Event{SessionID: "s", Event: "recent", At: at(-day)},
		Event{SessionID: "s", Event: "now", At: at(0)},
	)
	must(t, s.InsertEvents(ctx, in...))

	n, err := s.PurgeEvents(ctx, t0, 30)
	must(t, err)
	if n != 10 {
		t.Errorf("PurgeEvents deleted %d, want 10", n)
	}
	got, err := s.ListSessionEvents(ctx, "s", 0)
	must(t, err)
	var names []string
	for _, e := range got {
		names = append(names, e.Event)
	}
	equal(t, "events left", names, []string{"now", "recent", "edge"})

	n, err = s.PurgeEvents(ctx, t0, 30)
	must(t, err)
	if n != 0 {
		t.Errorf("second PurgeEvents deleted %d, want 0", n)
	}

	// Zero now means the store's clock; a shorter retention purges more.
	n, err = s.PurgeEvents(ctx, time.Time{}, 1)
	must(t, err)
	if n != 1 {
		t.Errorf("PurgeEvents(1 day) deleted %d, want 1", n)
	}

	if _, err := s.PurgeEvents(ctx, t0, 0); err == nil {
		t.Error("PurgeEvents with 0 days succeeded")
	}
}
