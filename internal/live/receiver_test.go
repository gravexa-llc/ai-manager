package live

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/events"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web"
)

var t0 = time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)

func ev(kind events.Kind, sid string, at time.Time) events.Event {
	return events.Event{Kind: kind, SessionID: sid, At: at}
}

func start(sid string, at time.Time) events.Event {
	e := ev(events.SessionStart, sid, at)
	e.Cwd, e.Repo, e.Worktree, e.Branch, e.Host, e.Source = "/r/app", "/r/app", "/r/app", "main", "laptop", "startup"
	e.Interactive = ptr(true)
	e.ItermSessionID = "w0t0p0:ABC"
	return e
}

func tool(sid, agent, label, id string, at time.Time) events.Event {
	e := ev(events.Tool, sid, at)
	e.AgentID, e.Tool, e.Label, e.ToolUseID = agent, strings.SplitN(label, ":", 2)[0], label, id
	return e
}

// subStart is a subagent's turn_start: the start of one of its runs.
func subStart(sid, agent string, at time.Time) events.Event {
	e := ev(events.TurnStart, sid, at)
	e.AgentID = agent
	return e
}

// needs is a permission dialog's needs_you for a call.
func needs(sid, agent, id string, at time.Time) events.Event {
	e := ev(events.NeedsYou, sid, at)
	e.AgentID, e.Source, e.Reason, e.ToolUseID = agent, events.SourcePermissionRequest, "Bash: deploy", id
	return e
}

func TestPostEventsNeedsToken(t *testing.T) {
	h := newHarness(t, nil)
	body, _ := json.Marshal(map[string]any{"events": []events.Event{start("s1", t0)}})
	for name, hdr := range map[string]map[string]string{
		"no token":    nil,
		"wrong token": {web.TokenHeader: "nope"},
	} {
		if rec := h.request(http.MethodPost, "/events", body, hdr); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s: %d %s, want 401", name, rec.Code, rec.Body)
		}
	}
	if rec := h.request(http.MethodPost, "/events", body, map[string]string{web.TokenHeader: testToken, "Host": "evil.example:7777"}); rec.Code != http.StatusForbidden {
		t.Errorf("foreign Host: %d, want 403", rec.Code)
	}
	req := map[string]string{web.TokenHeader: testToken}
	if rec := h.request(http.MethodGet, "/events", nil, req); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /events = %d, want 405", rec.Code)
	}
	h.sync()
	if _, err := h.st.FindSession(context.Background(), "s1"); !notFound(err) {
		t.Errorf("an unauthorized batch was applied: %v", err)
	}
	if st := h.status().Events; st.Batches != 0 || st.ModLive {
		t.Errorf("stats after refused auth = %+v", st)
	}
	if calls := h.modLiveCalls(); len(calls) != 0 {
		t.Errorf("SetModLive calls = %v, want none", calls)
	}
}

func TestPostEventsLimits(t *testing.T) {
	h := newHarness(t, nil)
	tok := map[string]string{web.TokenHeader: testToken}

	many := make([]events.Event, events.MaxBatchEvents+1)
	for i := range many {
		many[i] = tool("s1", "", "Bash: x", fmt.Sprint(i), t0.Add(time.Duration(i)*time.Second))
	}
	body, _ := json.Marshal(map[string]any{"events": many})
	if rec := h.request(http.MethodPost, "/events", body, tok); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("%d events: %d %s, want 413", len(many), rec.Code, rec.Body)
	}

	// Exactly 200 is fine.
	body, _ = json.Marshal(map[string]any{"events": many[:events.MaxBatchEvents]})
	if rec := h.request(http.MethodPost, "/events", body, tok); rec.Code != http.StatusOK {
		t.Errorf("200 events: %d %s, want 200", rec.Code, rec.Body)
	}

	big := tool("s1", "", "Bash: x", "big", t0)
	big.Cwd = strings.Repeat("/x", events.MaxBatchBytes/2)
	body, _ = json.Marshal(map[string]any{"events": []events.Event{big}})
	if rec := h.request(http.MethodPost, "/events", body, tok); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("%d bytes: %d %s, want 413", len(body), rec.Code, rec.Body)
	}
	for _, bad := range []string{`not json`, `[]`, `{"events": 3}`, `{}`} {
		if rec := h.request(http.MethodPost, "/events", []byte(bad), tok); rec.Code != http.StatusBadRequest {
			t.Errorf("body %q: %d, want 400", bad, rec.Code)
		}
	}
	h.sync()
	if st := h.status().Events; st.RefusedBatches != 6 || st.Batches != 1 || st.Events != events.MaxBatchEvents {
		t.Errorf("stats = %+v, want 6 refused and the one batch of 200", st)
	}
}

func TestUnknownKindsAreDroppedAndCounted(t *testing.T) {
	h := newHarness(t, nil)
	body := []byte(`{"events": [
		{"kind": "session_start", "session_id": "s1", "at": "2026-11-06T09:00:00.000Z", "cwd": "/r/app"},
		{"kind": "telepathy", "session_id": "s1", "at": "2026-11-06T09:00:01.000Z"},
		{"kind": "telepathy", "session_id": "s1", "at": "2026-11-06T09:00:02.000Z"},
		{"kind": "future_kind", "session_id": "s1", "at": "yesterday"},
		{"kind": "tool", "at": "2026-11-06T09:00:03.000Z", "tool": "Bash"},
		{"kind": "tool", "session_id": "s1", "at": "not a time"},
		{"kind": "tool", "session_id": "s1", "at": "2026-11-06T09:00:04.000Z", "tool": "Bash", "label": "Bash: ok"}
	]}`)
	rec := h.request(http.MethodPost, "/events", body, map[string]string{web.TokenHeader: testToken})
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "{}" {
		t.Fatalf("POST = %d %s, want 200 {}", rec.Code, rec.Body)
	}
	h.sync()
	st := h.status().Events
	if st.UnknownKinds != 3 || st.UnknownByKind["telepathy"] != 2 || st.UnknownByKind["future_kind"] != 1 || st.Invalid != 2 || st.Applied != 2 {
		t.Errorf("stats = %+v", st)
	}
	if s := h.session("s1"); s.LastAction != "Bash: ok" {
		t.Errorf("the valid events weren't applied: %+v", s)
	}
}

func TestDedupe(t *testing.T) {
	h := newHarness(t, nil)
	h.post(start("s1", t0))
	a := tool("s1", "", "Bash: make", "t1", t0.Add(time.Second))
	b := a
	b.At = a.At.Add(400 * time.Millisecond) // same call, reported twice
	c := a
	c.At = a.At.Add(2 * time.Second) // a second later: a new event
	d := tool("s1", "", "Bash: other", "t2", a.At.Add(100*time.Millisecond))
	sub := a
	sub.AgentID = "sub1" // another loop
	h.post(a, b)
	h.post(a) // resent whole
	h.post(c, d, sub)
	var tools int
	for _, e := range h.events("s1") {
		if e.Event == string(events.Tool) {
			tools++
		}
	}
	if tools != 4 {
		t.Errorf("%d tool rows, want 4 (a, c, d, sub)", tools)
	}
	if st := h.status().Events; st.Deduped != 2 {
		t.Errorf("deduped = %d, want 2", st.Deduped)
	}

	// Two snapshots in the same second both count.
	l1 := ev(events.AgentList, "s1", t0.Add(10*time.Second))
	l1.Agents = []events.Agent{{ID: "x", Status: events.AgentRunning}}
	l2 := l1
	l2.At = l1.At.Add(200 * time.Millisecond)
	l2.Agents = []events.Agent{{ID: "x", Status: events.AgentRunning}, {ID: "y", Status: events.AgentRunning}}
	h.post(l1, l2)
	// sub1, known only from a tool call, is no agent: it gets no row.
	if ag := h.agents("s1"); len(ag) != 3 { // lead, x, y
		t.Errorf("agents = %d, want 3", len(ag))
	}
}

func TestQuietTimer(t *testing.T) {
	h := newHarness(t, nil)
	h.post(start("s1", t0), ev(events.TurnStart, "s1", t0.Add(time.Second)))
	checkSession(t, h.session("s1"), StateWorking, "")

	h.clock.set(t0.Add(14 * time.Minute))
	h.tick()
	checkSession(t, h.session("s1"), StateWorking, "")

	h.clock.set(t0.Add(16 * time.Minute))
	h.tick()
	checkSession(t, h.session("s1"), StateQuiet, "")

	// Any event wakes it.
	h.post(tool("s1", "", "Bash: make", "t1", t0.Add(16*time.Minute)))
	checkSession(t, h.session("s1"), StateWorking, "")

	// Quiet comes only from working.
	h.post(func() events.Event {
		e := ev(events.TurnComplete, "s1", t0.Add(17*time.Minute))
		e.Reason = events.ReasonAnswer
		return e
	}())
	h.clock.set(t0.Add(2 * time.Hour))
	h.tick()
	checkSession(t, h.session("s1"), StateTurnDone, "")
}

func TestQuietAfterOption(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.QuietAfter = time.Minute })
	h.post(start("s1", t0), ev(events.TurnStart, "s1", t0))
	h.clock.set(t0.Add(61 * time.Second))
	h.tick()
	checkSession(t, h.session("s1"), StateQuiet, "")
}

func TestModLiveSwitch(t *testing.T) {
	h := newHarness(t, nil)
	h.post(start("s1", t0))
	if calls := h.modLiveCalls(); len(calls) != 1 || !calls[0] {
		t.Fatalf("after the first batch: SetModLive %v, want [true]", calls)
	}
	h.post(tool("s1", "", "Bash: a", "1", t0.Add(time.Minute)))
	if calls := h.modLiveCalls(); len(calls) != 1 {
		t.Errorf("a second batch called SetModLive again: %v", calls)
	}
	if !h.status().Events.ModLive {
		t.Error("status says the mod isn't live")
	}

	h.clock.set(t0.Add(time.Minute + 4*time.Minute + 59*time.Second))
	h.tick()
	if calls := h.modLiveCalls(); len(calls) != 1 {
		t.Errorf("switched off before 5 minutes: %v", calls)
	}
	h.clock.set(t0.Add(6 * time.Minute))
	h.tick()
	if calls := h.modLiveCalls(); len(calls) != 2 || calls[1] {
		t.Errorf("after 5 quiet minutes: %v, want [true false]", calls)
	}
	if h.status().Events.ModLive {
		t.Error("status still says live")
	}
	h.tick()
	if calls := h.modLiveCalls(); len(calls) != 2 {
		t.Errorf("switched off twice: %v", calls)
	}

	h.post(tool("s1", "", "Bash: b", "2", t0.Add(7*time.Minute)))
	if calls := h.modLiveCalls(); len(calls) != 3 || !calls[2] {
		t.Errorf("events again: %v, want true again", calls)
	}
}

// After /clear the process goes on under a new session id: the row, its
// agents, its links and the plans it registered follow, prev_session_id
// names the old id, and late events for the old id land on the new one.
func TestClearContinuity(t *testing.T) {
	for _, tc := range []struct {
		name string
		// how the mod tells: the new id on session_end, the old one on
		// session_start, or both
		newOnEnd, prevOnStart bool
	}{
		{"prev_session_id on session_start", false, true},
		{"new_session_id on session_end", true, false},
		{"both", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			ctx := context.Background()
			h.post(start("old", t0), tool("old", "", "Bash: a", "1", t0.Add(time.Second)))
			planID := seedPlan(t, h.st, "old")
			ln := store.Link{PlanID: planID, PkgID: "A1", Kind: store.LinkAgent, SessionID: "old", AgentID: "", MatchMethod: store.MatchSpawn, Confidence: store.ConfidenceExact}
			if _, err := h.st.UpsertLink(ctx, ln); err != nil {
				t.Fatal(err)
			}
			end := ev(events.SessionEnd, "old", t0.Add(time.Minute))
			end.Reason = events.ReasonClear
			if tc.newOnEnd {
				end.NewSessionID = "new"
			}
			next := start("new", t0.Add(time.Minute+10*time.Millisecond))
			next.Source = events.SourceClear
			if tc.prevOnStart {
				next.PrevSessionID = "old"
			}
			h.post(end)
			if !tc.newOnEnd {
				// Until the new id is known the old row stays, at the prompt.
				s := h.session("old")
				checkSession(t, s, StateIdle, "")
				if !s.EndedAt.IsZero() {
					t.Fatal("a /clear ended the session")
				}
			}
			h.post(next)
			late := tool("old", "", "Bash: late", "2", t0.Add(time.Minute+time.Second))
			h.post(late)

			s := h.session("new")
			if s.PrevSessionID != "old" || !s.EndedAt.IsZero() || s.Key != store.ModSessionKeyPrefix+"old" {
				t.Errorf("new row = %+v", s)
			}
			if s.LastAction != "Bash: late" {
				t.Errorf("late event for the old id: last action %q", s.LastAction)
			}
			checkSession(t, s, StateIdle, "")
			if _, err := h.st.FindSession(ctx, "old"); !notFound(err) {
				t.Errorf("a row still has the old id: %v", err)
			}
			all, _ := h.st.ListSessions(ctx, store.SessionFilter{IncludeEnded: true})
			if len(all) != 1 {
				t.Errorf("%d session rows, want 1", len(all))
			}
			links, _ := h.st.ListLinks(ctx, planID)
			if len(links) != 1 || links[0].SessionID != "new" || links[0].Ref != "new/" {
				t.Errorf("links = %+v", links)
			}
			if p, _ := h.st.GetPlan(ctx, planID); p.RegisteredInSession != "new" {
				t.Errorf("plan registered in %q", p.RegisteredInSession)
			}
			if ag := h.agents("new"); len(ag) != 1 || ag[""].Kind != store.AgentLead {
				t.Errorf("agents of new = %+v", ag)
			}
			if ag := h.agents("old"); len(ag) != 0 {
				t.Errorf("agents left under old: %+v", ag)
			}
		})
	}
}

// The poller lists the session that the receiver created a row for, under
// another id after the /clear: the row adopts it without a duplicate.
func TestClearAfterThePollerMoved(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	h.post(start("old", t0))
	// The poller already shows the new id for the same pid.
	rows := h.recv.RefineSessions(ctx, []store.Session{{Key: "pid:9", SessionID: "old", Kind: "interactive", PID: 9, State: "idle"}}, t0.Add(time.Second))
	if err := h.st.UpsertSessions(ctx, rows...); err != nil {
		t.Fatal(err)
	}
	if err := h.st.UpsertSessions(ctx, store.Session{Key: "pid:9", SessionID: "new", Kind: "interactive", PID: 9, State: "idle"}); err != nil {
		t.Fatal(err)
	}
	next := start("new", t0.Add(time.Minute))
	next.Source, next.PrevSessionID = events.SourceClear, "old"
	h.post(next)
	all, _ := h.st.ListSessions(ctx, store.SessionFilter{IncludeEnded: true})
	if len(all) != 1 || all[0].Key != "pid:9" || all[0].SessionID != "new" || all[0].PrevSessionID != "old" {
		t.Errorf("rows = %+v", all)
	}
}

func seedPlan(t *testing.T, st *store.Store, session string) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := st.InsertPlan(ctx, store.Plan{Slug: "demo", Title: "Demo", RegisteredInSession: session})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InsertPackage(ctx, store.Package{PlanID: id, PkgID: "A1", Title: "First"}); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestRefineSessions(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	h.post(start("s1", t0), ev(events.TurnStart, "s1", t0.Add(time.Second)))

	poll := t0.Add(10 * time.Second)
	h.clock.set(poll)
	in := []store.Session{
		{Key: "pid:42", SessionID: "s1", Kind: "interactive", PID: 42, Cwd: "/r/app/sub", Headless: true, State: "idle", StateReason: "idle", LastSeenAt: poll},
		{Key: "pid:43", SessionID: "other", Kind: "interactive", PID: 43, State: "working", LastSeenAt: poll},
	}
	out := h.recv.RefineSessions(ctx, in, poll)
	if len(out) != 2 {
		t.Fatalf("got %d rows", len(out))
	}
	s1 := out[0]
	if s1.State != StateWorking || s1.Headless || s1.Cwd != "/r/app" || s1.WorktreePath != "/r/app" || s1.Branch != "main" {
		t.Errorf("refined row = %+v", s1)
	}
	if out[1].State != "working" || out[1].SessionID != "other" {
		t.Errorf("a session the mod doesn't report changed: %+v", out[1])
	}
	if err := h.st.UpsertSessions(ctx, out...); err != nil {
		t.Fatal(err)
	}
	// The receiver's row was adopted: one row for s1, keyed by the poller.
	all, _ := h.st.ListSessions(ctx, store.SessionFilter{IncludeEnded: true})
	if len(all) != 2 {
		t.Errorf("%d rows, want 2: %+v", len(all), all)
	}
	if got := h.session("s1"); got.Key != "pid:42" || got.PID != 42 || got.ItermSessionID != "w0t0p0:ABC" {
		t.Errorf("adopted row = %+v", got)
	}
	// Events after the adoption land on the poller's row.
	h.post(tool("s1", "", "Bash: test", "t9", t0.Add(11*time.Second)))
	if got := h.session("s1"); got.Key != "pid:42" || got.LastAction != "Bash: test" {
		t.Errorf("after adoption: %+v", got)
	}

	// A needs-you only the poll sees counts until an event comes after it.
	poll2 := t0.Add(20 * time.Second)
	out = h.recv.RefineSessions(ctx, []store.Session{{Key: "pid:42", SessionID: "s1", PID: 42, State: StateNeedsYou, StateReason: "input needed", NeedsYouSince: poll2}}, poll2)
	if out[0].State != StateNeedsYou || out[0].StateReason != "input needed" || !out[0].NeedsYouSince.Equal(poll2) {
		t.Errorf("poll's needs-you = %+v", out[0])
	}
	h.st.UpsertSessions(ctx, out...)
	h.clock.set(poll2.Add(time.Second))
	h.tick() // the tick agrees with the poll
	checkSession(t, h.session("s1"), StateNeedsYou, "input needed")
	h.post(tool("s1", "", "Bash: go on", "t10", poll2.Add(5*time.Second)))
	checkSession(t, h.session("s1"), StateWorking, "")

	// The mod said the session ended: a poll that still lists it doesn't
	// bring it back.
	h.post(func() events.Event {
		e := ev(events.SessionEnd, "s1", poll2.Add(10*time.Second))
		e.Reason = "logout"
		return e
	}())
	out = h.recv.RefineSessions(ctx, []store.Session{{Key: "pid:42", SessionID: "s1", PID: 42, State: "idle"}}, poll2.Add(11*time.Second))
	if len(out) != 0 {
		t.Errorf("an ended session's row came back: %+v", out)
	}
}

// A poll that overwrites the state between a refine and its write is
// repaired by the next tick.
func TestTickRepairsOverwrittenState(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	h.post(start("s1", t0), ev(events.TurnStart, "s1", t0.Add(time.Second)))
	if err := h.st.UpsertSessions(ctx, store.Session{Key: store.ModSessionKeyPrefix + "s1", SessionID: "s1", State: "idle"}); err != nil {
		t.Fatal(err)
	}
	checkSession(t, h.session("s1"), "idle", "")
	h.tick()
	checkSession(t, h.session("s1"), StateWorking, "")
}

// A tool call that finishes while another one in the same loop waits for
// approval doesn't clear the ask; the asked call finishing does.
func TestParallelToolKeepsTheAsk(t *testing.T) {
	h := newHarness(t, nil)
	h.post(start("s1", t0), ev(events.TurnStart, "s1", t0.Add(time.Second)))
	h.post(needs("s1", "", "ask", t0.Add(2*time.Second)))
	h.post(tool("s1", "", "Read x.go", "other", t0.Add(3*time.Second)))
	checkSession(t, h.session("s1"), StateNeedsYou, "permission prompt: Bash: deploy")
	h.post(tool("s1", "", "Bash: deploy", "ask", t0.Add(30*time.Second)))
	checkSession(t, h.session("s1"), StateWorking, "")
}

// needs_you is sent at once and other events are batched, so an ask can
// arrive after the event that answered it: it is ignored then.
func TestLateAskIsIgnored(t *testing.T) {
	h := newHarness(t, nil)
	h.post(start("s1", t0), ev(events.TurnStart, "s1", t0.Add(time.Second)), tool("s1", "", "Bash: deploy", "ask", t0.Add(5*time.Second)))
	h.post(needs("s1", "", "ask", t0.Add(2*time.Second)))
	checkSession(t, h.session("s1"), StateWorking, "")
}

func TestPersonPromptClearsNeedsYou(t *testing.T) {
	h := newHarness(t, nil)
	h.post(start("s1", t0), ev(events.TurnStart, "s1", t0.Add(time.Second)), subStart("s1", "sub1", t0.Add(time.Second)))
	h.post(needs("s1", "sub1", "a", t0.Add(2*time.Second)))
	notif := ev(events.Prompt, "s1", t0.Add(3*time.Second))
	notif.OriginKind = "task-notification"
	h.post(notif)
	checkSession(t, h.session("s1"), StateNeedsYou, "")
	person := ev(events.Prompt, "s1", t0.Add(4*time.Second))
	person.OriginKind = "composer"
	h.post(person)
	checkSession(t, h.session("s1"), StateWorking, "")
	if a := h.agents("s1")["sub1"]; a.State != events.AgentRunning {
		t.Errorf("sub1 = %q after the person's prompt", a.State)
	}
}

func TestIdleNotification(t *testing.T) {
	h := newHarness(t, nil)
	done := ev(events.TurnComplete, "s1", t0.Add(2*time.Second))
	done.Reason = events.ReasonAnswer
	h.post(start("s1", t0), ev(events.TurnStart, "s1", t0.Add(time.Second)), done)
	checkSession(t, h.session("s1"), StateTurnDone, "")
	h.post(ev(events.Idle, "s1", t0.Add(time.Minute)))
	checkSession(t, h.session("s1"), StateIdle, "")
	h.post(ev(events.TurnStart, "s1", t0.Add(2*time.Minute)))
	checkSession(t, h.session("s1"), StateWorking, "")
}

// Events for a session that ended are kept on the timeline but don't
// bring it back; a session_start after the end (claude --resume) starts a
// new row.
func TestEndedSessionAndResume(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	end := ev(events.SessionEnd, "s1", t0.Add(time.Minute))
	end.Reason = "prompt_input_exit"
	h.post(start("s1", t0), end)
	h.post(tool("s1", "", "Bash: late", "x", t0.Add(30*time.Second)))
	s := h.session("s1")
	checkSession(t, s, StateGone, "prompt_input_exit")
	if s.EndedAt.IsZero() || s.LastAction == "Bash: late" {
		t.Errorf("ended row = %+v", s)
	}
	h.post(start("s1", t0.Add(time.Hour)))
	all, _ := h.st.ListSessions(ctx, store.SessionFilter{IncludeEnded: true})
	if len(all) != 2 {
		t.Fatalf("%d rows after the resume, want 2", len(all))
	}
	if s := h.session("s1"); !s.EndedAt.IsZero() || s.State != StateIdle {
		t.Errorf("resumed row = %+v", s)
	}
}

// What the receiver stores is capped like the store's events table.
func TestSummariesAreTruncated(t *testing.T) {
	h := newHarness(t, nil)
	long := strings.Repeat("é", 400)
	done := ev(events.TurnComplete, "s1", t0.Add(time.Second))
	done.Reason, done.LastSaid = events.ReasonAnswer, long+"\nsecond line"
	tl := tool("s1", "", long, "t", t0.Add(2*time.Second))
	tl.Tool = strings.Repeat("T", 200)
	h.post(start("s1", t0), done, tl)
	s := h.session("s1")
	if n := len([]rune(s.LastSaid)); n != store.MaxSummaryLen || strings.Contains(s.LastSaid, "second") {
		t.Errorf("last_said: %d runes", n)
	}
	for _, e := range h.events("s1") {
		if len([]rune(e.Summary)) > store.MaxSummaryLen || len([]rune(e.ToolName)) > store.MaxToolNameLen {
			t.Errorf("event row over the caps: %d / %d", len([]rune(e.Summary)), len([]rune(e.ToolName)))
		}
	}
}

func TestQueueFullDropsBatch(t *testing.T) {
	r := New(Options{Store: nil, QueueSize: 1, Now: func() time.Time { return t0 }})
	b := events.Batch{Events: []events.Event{start("s1", t0)}}
	r.Accept(b)
	r.Accept(b) // nobody is applying: the queue is full
	if st := r.Stats(); st.Batches != 2 || st.DroppedBatches != 1 {
		t.Errorf("stats = %+v", st)
	}
}

func TestSessionWithoutStartGetsARow(t *testing.T) {
	h := newHarness(t, nil)
	e := tool("s9", "", "Bash: x", "1", t0)
	e.Cwd = "/r/app"
	h.post(e)
	s := h.session("s9")
	if s.Key != store.ModSessionKeyPrefix+"s9" || s.Headless || s.Cwd != "/r/app" || s.LastAction != "Bash: x" {
		t.Errorf("row = %+v", s)
	}
}

func TestStatusShowsEventCounters(t *testing.T) {
	h := newHarness(t, nil)
	h.post(start("s1", t0))
	rec := h.request(http.MethodGet, "/api/status", nil, nil)
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"events":{"batches":1`)) {
		t.Errorf("status body = %s", rec.Body)
	}
	if st := h.status().Events; st.LiveSessions != 1 || st.LastEventAt.IsZero() || st.LastBatchAt.IsZero() {
		t.Errorf("stats = %+v", st)
	}
}
