package live

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/events"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// apiSession returns a session as GET /api/sessions?all=1 gives it to a
// caller with aimd's token, which may see waiting_on.
func (h *harness) apiSession(id string) api.Session {
	h.t.Helper()
	return h.apiSessionAs(id, map[string]string{web.TokenHeader: testToken})
}

// apiSessionAs returns a session as GET /api/sessions?all=1 gives it to a
// request with those headers.
func (h *harness) apiSessionAs(id string, hdr map[string]string) api.Session {
	h.t.Helper()
	rec := h.request(http.MethodGet, "/api/sessions?all=1", nil, hdr)
	if rec.Code != http.StatusOK {
		h.t.Fatalf("GET /api/sessions = %d %s", rec.Code, rec.Body)
	}
	var out api.Sessions
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		h.t.Fatal(err)
	}
	for _, s := range out.Sessions {
		if s.SessionID == id {
			return s
		}
	}
	h.t.Fatalf("session %s not in /api/sessions", id)
	return api.Session{}
}

// The recorded interactive run, replayed: each dialog shows in
// /api/sessions with what it waits on while it is open, and the detail goes
// when it closes.
func TestReplayInteractiveDialogsWaitingOn(t *testing.T) {
	h := newHarness(t, nil)
	evs := fixture(t, "interactive-dialogs-clear.jsonl")
	sid := evs[0].SessionID
	var first []events.Event
	rest := evs

	// next replays up to and including the next needs_you with a wait of
	// the kind, and returns what /api/sessions says then.
	next := func(want string) api.Session {
		t.Helper()
		first, rest = split(t, rest, func(e events.Event) bool {
			return e.Kind == events.NeedsYou && e.Wait != nil && e.Wait.Kind == want
		})
		h.replay(first)
		return h.apiSession(sid)
	}
	answered := func() {
		t.Helper()
		first, rest = split(t, rest, kind(events.Tool))
		h.replay(first)
		if s := h.apiSession(sid); s.NeedsYou || s.WaitingOn != nil {
			t.Errorf("after the dialog closed: needs_you %v, waiting_on %+v", s.NeedsYou, s.WaitingOn)
		}
	}

	s := next(events.WaitPermission)
	if w := s.WaitingOn; !s.NeedsYou || w == nil || w.Kind != "permission" || w.Tool != "Bash" || w.Summary != "mkdir perm-main" ||
		w.AgentID != "" || w.ToolUseID != "toolu_01Sq2kkzaHUdre5VjErZuQ69" || !w.Since.Equal(s.NeedsYouSince) {
		t.Errorf("permission: %+v waiting_on %+v", s, s.WaitingOn)
	}
	if s.StateReason != "permission prompt: Bash: Create directory perm-main" {
		t.Errorf("state_reason = %q", s.StateReason)
	}
	answered()

	s = next(events.WaitQuestion)
	want := []api.WaitQuestion{{Header: "Color", Question: "Do you prefer red or blue?", Options: []string{"Red", "Blue"}}}
	if w := s.WaitingOn; w == nil || w.Kind != "question" || !reflect.DeepEqual(w.Questions, want) || s.StateReason != "question: AskUserQuestion" {
		t.Errorf("question: %q waiting_on %+v", s.StateReason, s.WaitingOn)
	}
	answered()

	s = next(events.WaitPlan)
	if w := s.WaitingOn; w == nil || w.Kind != "plan" || w.Plan != "[plan text]" || s.StateReason != "plan approval: ExitPlanMode" {
		t.Errorf("plan: %q waiting_on %+v", s.StateReason, s.WaitingOn)
	}
	answered()

	// The background subagent's permission: attributed to it.
	first, rest = split(t, rest, func(e events.Event) bool { return e.Kind == events.NeedsYou && e.AgentID != "" })
	h.replay(first)
	s = h.apiSession(sid)
	if w := s.WaitingOn; w == nil || w.AgentID != "a6cabd35f43276105" || w.Summary != "mkdir perm-sub && echo done" {
		t.Errorf("subagent permission: waiting_on %+v", s.WaitingOn)
	}

	// The rest, /clear included: the session goes on under its new id,
	// waiting on nothing, and ends.
	first, rest = split(t, rest, func(e events.Event) bool { return e.Kind == events.Idle })
	h.replay(first)
	if s := h.apiSession(sid); s.NeedsYou || s.WaitingOn != nil {
		t.Errorf("idle: needs_you %v, waiting_on %+v", s.NeedsYou, s.WaitingOn)
	}
	h.replay(rest)
	if newID := evs[len(evs)-1].SessionID; newID == sid || h.session(newID).PrevSessionID != sid {
		t.Errorf("after /clear: %s, prev %q", newID, h.session(newID).PrevSessionID)
	}
}

// A permission a person granted, for a tool that then runs a long time:
// a poll taken well after the ask, that finds the session busy, answers
// it; a poll taken right after it doesn't.
func TestPollAnswersAGrantedPermission(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	h.post(start("s1", t0), ev(events.TurnStart, "s1", t0.Add(time.Second)), subStart("s1", "sub1", t0.Add(time.Second)))
	ask := needs("s1", "sub1", "t1", t0.Add(2*time.Second))
	ask.Wait = &events.Wait{Kind: events.WaitPermission, Tool: "Bash", Summary: "npm test"}
	h.post(ask)
	if w := h.apiSession("s1").WaitingOn; w == nil || w.Summary != "npm test" || w.AgentID != "sub1" {
		t.Fatalf("waiting_on = %+v", w)
	}

	busy := func(at time.Time) store.Session {
		h.clock.set(at)
		out := h.recv.RefineSessions(ctx, []store.Session{{Key: "pid:42", SessionID: "s1", PID: 42, State: StateWorking, StateReason: "busy", LastSeenAt: at}}, at)
		if err := h.st.UpsertSessions(ctx, out...); err != nil {
			t.Fatal(err)
		}
		return out[0]
	}
	// Too soon: the poll may predate the dialog.
	if row := busy(t0.Add(4 * time.Second)); row.State != StateNeedsYou {
		t.Errorf("a poll 2 s after the ask cleared it: %+v", row)
	}
	// Not waiting, 8 s on: the dialog closed, though the tool still runs.
	row := busy(t0.Add(10 * time.Second))
	if row.State != StateWorking || !row.NeedsYouSince.IsZero() {
		t.Errorf("after a busy poll: %+v", row)
	}
	if s := h.apiSession("s1"); s.NeedsYou || s.WaitingOn != nil {
		t.Errorf("api: needs_you %v, waiting_on %+v", s.NeedsYou, s.WaitingOn)
	}
	if a := h.agents("s1")["sub1"]; a.State != events.AgentRunning {
		t.Errorf("sub1 = %q, want running", a.State)
	}
	// An unknown verdict says nothing about dialogs.
	ask2 := needs("s1", "", "t2", t0.Add(20*time.Second))
	h.post(ask2)
	h.clock.set(t0.Add(40 * time.Second))
	out := h.recv.RefineSessions(ctx, []store.Session{{Key: "pid:42", SessionID: "s1", PID: 42, State: "unknown"}}, t0.Add(40*time.Second))
	if out[0].State != StateNeedsYou {
		t.Errorf("an unknown poll verdict cleared the ask: %+v", out[0])
	}
}

// A later signal for the same ask fills in the detail; a notification
// doesn't replace the reason; an ask for another call drops a detail that
// was about the first.
func TestWaitFollowsTheAsk(t *testing.T) {
	h := newHarness(t, nil)
	h.post(start("s1", t0), ev(events.TurnStart, "s1", t0.Add(time.Second)))
	h.post(needs("s1", "", "t1", t0.Add(2*time.Second)))
	if w := h.apiSession("s1").WaitingOn; w != nil {
		t.Errorf("waiting_on without a detail = %+v", w)
	}
	later := needs("s1", "", "t1", t0.Add(4*time.Second))
	later.Source = events.SourcePermissionRequest
	later.Wait = &events.Wait{Kind: events.WaitPermission, Tool: "Bash", Summary: "./deploy.sh"}
	h.post(later)
	if w := h.apiSession("s1").WaitingOn; w == nil || w.Summary != "./deploy.sh" || !w.Since.Equal(t0.Add(2*time.Second)) {
		t.Errorf("waiting_on = %+v", w)
	}
	n := ev(events.NeedsYou, "s1", t0.Add(8*time.Second))
	n.Source, n.Reason = events.SourceNotification, "permission_prompt"
	h.post(n)
	if s := h.apiSession("s1"); s.StateReason != "permission prompt: Bash: deploy" || s.WaitingOn == nil {
		t.Errorf("after the notification: %q %+v", s.StateReason, s.WaitingOn)
	}
	h.post(needs("s1", "", "t2", t0.Add(9*time.Second)))
	if w := h.apiSession("s1").WaitingOn; w != nil {
		t.Errorf("waiting_on kept the first call's detail: %+v", w)
	}
}

// waiting_on carries what a person is asked, so GET /api/sessions shows
// it only to a caller with aimd's token; reads are otherwise open to any
// local process. A dashboard page's CSRF token doesn't count: every page
// embeds one. The rest of the session, needs-you included, stays open.
// aimd redacts the wait again on receipt, whatever the sender did.
func TestWaitingOnNeedsTheToken(t *testing.T) {
	h := newHarness(t, nil)
	h.post(start("s1", t0), ev(events.TurnStart, "s1", t0.Add(time.Second)))
	ask := needs("s1", "", "t1", t0.Add(2*time.Second))
	ask.Wait = &events.Wait{Kind: events.WaitPermission, Tool: "Bash", Summary: "GITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz0123 gh pr list"}
	h.post(ask)

	for name, hdr := range map[string]map[string]string{
		"no credentials":           nil,
		"a wrong token":            {web.TokenHeader: "nope"},
		"a dashboard's CSRF token": {web.CSRFHeader: web.NewCSRF(testToken, testPort).Token()},
		"the token as CSRF":        {web.CSRFHeader: testToken},
	} {
		s := h.apiSessionAs("s1", hdr)
		if s.WaitingOn != nil || !s.NeedsYou || s.StateReason == "" {
			t.Errorf("%s: needs_you %v (%q), waiting_on %+v", name, s.NeedsYou, s.StateReason, s.WaitingOn)
		}
	}
	w := h.apiSessionAs("s1", map[string]string{web.TokenHeader: testToken}).WaitingOn
	if w == nil || w.Summary != "GITHUB_TOKEN=[redacted] gh pr list" {
		t.Errorf("with the token: waiting_on %+v", w)
	}
}

// A background session says so at session_start (it says interactive
// too), so the row the mod makes first is a background one, not headless.
func TestBackgroundSessionKind(t *testing.T) {
	h := newHarness(t, nil)
	evs := fixture(t, "background-session-brief.jsonl")
	h.replay(evs[:1])
	s := h.session(evs[0].SessionID)
	if s.Kind != "background" || s.Headless {
		t.Errorf("row = kind %q headless %v", s.Kind, s.Headless)
	}
}
