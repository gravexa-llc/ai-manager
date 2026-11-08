package events

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

var at = time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)

func TestNormalizeTruncatesToTheCaps(t *testing.T) {
	long := strings.Repeat("ü", 1000)
	e := Event{
		Kind: Tool, SessionID: " s1 ", At: at, AgentID: "a1",
		Tool: long, Label: long + "\nline two", Reason: "why\n\tso", LastSaid: "first line\nsecond line",
		OriginKind: long, Source: long, Name: long, ToolUseID: long, Cwd: strings.Repeat("/x", 3000),
		Agents: []Agent{{ID: " a2 ", Status: long, Type: long}, {ID: ""}},
	}
	if err := e.Normalize(); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		got string
		max int
	}{
		"tool": {e.Tool, MaxToolLen}, "label": {e.Label, MaxSummaryLen}, "origin_kind": {e.OriginKind, MaxKindLen},
		"source": {e.Source, MaxKindLen}, "name": {e.Name, MaxToolLen}, "tool_use_id": {e.ToolUseID, MaxToolLen},
		"cwd": {e.Cwd, MaxPathLen}, "status": {e.Agents[0].Status, MaxKindLen}, "type": {e.Agents[0].Type, MaxKindLen},
	} {
		if n := utf8.RuneCountInString(c.got); n != c.max {
			t.Errorf("%s: %d characters, want %d", name, n, c.max)
		}
	}
	if e.SessionID != "s1" || e.Reason != "why so" || e.LastSaid != "first line" {
		t.Errorf("session_id %q reason %q last_said %q", e.SessionID, e.Reason, e.LastSaid)
	}
	if strings.Contains(e.Label, "\n") {
		t.Errorf("label has a line break: %q", e.Label)
	}
	if len(e.Agents) != 1 || e.Agents[0].ID != "a2" {
		t.Errorf("agents = %+v, want the one with an id", e.Agents)
	}
}

func TestNormalizeErrors(t *testing.T) {
	for _, c := range []struct {
		e    Event
		want error
	}{
		{Event{Kind: "telepathy", SessionID: "s", At: at}, ErrUnknownKind},
		{Event{Kind: "", SessionID: "s", At: at}, ErrUnknownKind},
		{Event{Kind: Tool, At: at}, ErrInvalid},
		{Event{Kind: Tool, SessionID: "  ", At: at}, ErrInvalid},
		{Event{Kind: Tool, SessionID: "s"}, ErrInvalid},
	} {
		if err := c.e.Normalize(); !errors.Is(err, c.want) {
			t.Errorf("Normalize(%+v) = %v, want %v", c.e, err, c.want)
		}
	}
	for _, k := range Kinds {
		e := Event{Kind: k, SessionID: "s", At: at}
		if err := e.Normalize(); err != nil {
			t.Errorf("kind %s: %v", k, err)
		}
	}
}

func TestTruncate(t *testing.T) {
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{
		{"abc", 5, "abc"},
		{"abcdef", 3, "abc"},
		{"日本語テキスト", 3, "日本語"},
		{"ab\xffcd", 3, "ab�"},
		{"", 3, ""},
	} {
		if got := Truncate(c.in, c.n); got != c.want {
			t.Errorf("Truncate(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

func TestDecodeBatch(t *testing.T) {
	b, err := DecodeBatch(strings.NewReader(`{"events": [
		{"kind": "tool", "session_id": "s", "at": "2026-11-06T09:00:00.123Z", "tool": "Bash", "label": "Bash: x"},
		{"kind": "nope", "session_id": "s", "at": "2026-11-06T09:00:00Z"},
		{"kind": "tool", "session_id": "s", "at": 12345},
		{"kind": "nope2", "at": "bad"},
		"not an object",
		{"kind": "agent_list", "session_id": "s", "at": "2026-11-06T09:00:01+02:00", "agents": [{"id": "a1", "status": "running", "background": true}]}
	], "extra": "ignored"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Events) != 2 || b.Invalid != 2 || b.Unknown["nope"] != 1 || b.Unknown["nope2"] != 1 || b.Dropped() != 4 {
		t.Fatalf("batch = %+v", b)
	}
	if e := b.Events[0]; !e.At.Equal(at.Add(123*time.Millisecond)) || e.Label != "Bash: x" {
		t.Errorf("event 0 = %+v", e)
	}
	if a := b.Events[1].Agents; len(a) != 1 || a[0].Background == nil || !*a[0].Background {
		t.Errorf("agents = %+v", a)
	}

	for _, body := range []string{``, `nope`, `[]`, `{}`, `{"events": null}`, `{"events": {}}`} {
		if _, err := DecodeBatch(strings.NewReader(body)); !errors.Is(err, ErrMalformed) {
			t.Errorf("DecodeBatch(%q) = %v, want ErrMalformed", body, err)
		}
	}
	many := `{"events": [` + strings.TrimSuffix(strings.Repeat(`{"kind":"idle"},`, MaxBatchEvents+1), ",") + `]}`
	if _, err := DecodeBatch(strings.NewReader(many)); !errors.Is(err, ErrTooManyEvents) {
		t.Errorf("201 events: %v, want ErrTooManyEvents", err)
	}
	// A reader that stops at the size limit passes its error through.
	rec := httptest.NewRecorder()
	r := http.MaxBytesReader(rec, readCloser{strings.NewReader(strings.Repeat("x", 100))}, 10)
	var mbe *http.MaxBytesError
	if _, err := DecodeBatch(r); !errors.As(err, &mbe) {
		t.Errorf("over the size limit: %v, want *http.MaxBytesError", err)
	}
}

type readCloser struct{ *strings.Reader }

func (readCloser) Close() error { return nil }

func TestFromPerson(t *testing.T) {
	for origin, want := range map[string]bool{"composer": true, "sdk": true, "bridge": true, "task-notification": false, "peer": false, "": false} {
		if FromPerson(origin) != want {
			t.Errorf("FromPerson(%q) = %v", origin, !want)
		}
	}
}

func TestNormalizeCapsTheWait(t *testing.T) {
	long := strings.Repeat("ü", 1000)
	var qs []Question
	for range 6 {
		qs = append(qs, Question{Header: long, Question: long + "\nmore", Options: []string{long, "", "b", "c", "d", "e", "f", "g"}})
	}
	plan := strings.Repeat("step\n", 30)
	e := Event{Kind: NeedsYou, SessionID: "s", At: at, Wait: &Wait{Kind: WaitQuestion, Tool: long, Summary: "dropped", Questions: qs, Plan: plan}}
	if err := e.Normalize(); err != nil {
		t.Fatal(err)
	}
	w := e.Wait
	if w == nil || len(w.Questions) != MaxWaitQuestions || w.Summary != "" || w.Plan != "" || utf8.RuneCountInString(w.Tool) != MaxToolLen {
		t.Fatalf("wait = %+v", w)
	}
	q := w.Questions[0]
	if utf8.RuneCountInString(q.Header) != MaxKindLen || utf8.RuneCountInString(q.Question) != MaxSummaryLen ||
		len(q.Options) != MaxWaitOptions || utf8.RuneCountInString(q.Options[0]) != MaxOptionLen || q.Options[1] != "b" {
		t.Errorf("question = %+v", q)
	}
	if qs[0].Options[1] != "" || len(qs[0].Options) != 8 {
		t.Errorf("Normalize changed the caller's options: %v", qs[0].Options)
	}

	p := Event{Kind: NeedsYou, SessionID: "s", At: at, Wait: &Wait{Kind: WaitPlan, Plan: plan}}
	if err := p.Normalize(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(p.Wait.Plan, "\n") + 1; got != MaxPlanLines {
		t.Errorf("plan has %d lines, want %d", got, MaxPlanLines)
	}
	if one := PlanLines(strings.Repeat("x", 5000)); utf8.RuneCountInString(one) != MaxPlanLen {
		t.Errorf("a long plan line is %d characters", utf8.RuneCountInString(one))
	}

	for _, c := range []Event{
		{Kind: NeedsYou, SessionID: "s", At: at, Wait: &Wait{Kind: "telepathy", Summary: "x"}},
		{Kind: NeedsYou, SessionID: "s", At: at, Wait: &Wait{Kind: WaitQuestion}},
		{Kind: NeedsYou, SessionID: "s", At: at, Wait: &Wait{Kind: WaitPlan, Plan: " \n "}},
		{Kind: Tool, SessionID: "s", At: at, Wait: &Wait{Kind: WaitPermission, Tool: "Bash"}},
	} {
		if err := c.Normalize(); err != nil || c.Wait != nil {
			t.Errorf("Normalize kept a wait it can't show: %+v (%v)", c.Wait, err)
		}
	}
}
