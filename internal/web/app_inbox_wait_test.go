package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// fakeWaits is a WaitSource: what the mod reported each session waits on,
// by session id.
type fakeWaits map[string]api.WaitingOn

func (f fakeWaits) WaitingOn(sessionID string) (api.WaitingOn, bool) {
	w, ok := f[sessionID]
	return w, ok
}

// waitReports are the mod's wait reports for the sessions seedWaits adds.
var waitReports = fakeWaits{
	"s-perm": {Kind: "permission", Tool: "Bash", Summary: "xcodebuild -scheme App -token [redacted] build", Since: t0.Add(-5 * time.Minute)},
	"s-ask": {Kind: "question", Tool: "AskUserQuestion", Since: t0.Add(-6 * time.Minute), Questions: []api.WaitQuestion{
		{Header: "Take", Question: "Keep take 2, or record a third?", Options: []string{"Keep take 2", "Record a third"}},
		{Header: "Speed only"},
	}},
	"s-plan": {Kind: "plan", Tool: "ExitPlanMode", Since: t0.Add(-7 * time.Minute),
		Plan: "# Monitored sessions\n\n1. Add a table (migration 0006)\n2) POST and DELETE /api/sessions/watch\n- Filter Needs you\n"},
	// The reply's session has a report of another kind: it isn't shown.
	"s-odd": {Kind: "question", Questions: []api.WaitQuestion{{Question: "Stale question?"}}},
}

// seedWaits adds a session waiting in each way the mod reports, and one
// whose report doesn't fit its wait.
func seedWaits(t *testing.T, st *store.Store) {
	t.Helper()
	s := func(key, sid, name, reason string, ago time.Duration) store.Session {
		return store.Session{Key: key, SessionID: sid, Kind: "interactive", Name: name, Cwd: "/w/" + sid,
			State: needsYou, StateReason: reason, NeedsYouSince: t0.Add(-ago), StartedAt: t0.Add(-time.Hour), LastSeenAt: t0}
	}
	must(t, st.UpsertSessions(context.Background(),
		s("pid:71", "s-perm", "perm session", "permission prompt: Bash: Build the app", 5*time.Minute),
		s("pid:72", "s-ask", "ask session", "question: AskUserQuestion", 6*time.Minute),
		s("pid:73", "s-plan", "plan session", "plan approval: ExitPlanMode", 7*time.Minute),
		s("pid:74", "s-odd", "odd session", "permission prompt: Edit: Change main.go", 8*time.Minute),
	))
}

// The detail pane shows what an item waits on from the mod's report: the
// tool and its redacted command, the questions and their options, the
// plan's steps. Without a report, or with one of another kind, it is what
// the needs-you reason says.
func TestInboxWaitDetail(t *testing.T) {
	for _, reported := range []bool{true, false} {
		e := newInboxEnv(t)
		if reported {
			e.srv.opts.Waits = waitReports
		}
		seedWaits(t, e.st)
		get(t, e.h, "/app/inbox") // a pass opens the items
		detail := func(key string) string {
			t.Helper()
			return get(t, e.h, "/app/inbox?item="+e.id(key)+"&fragment=detail")
		}
		perm, ask, plan, odd := detail("pid:71"), detail("pid:72"), detail("pid:73"), detail("pid:74")
		wantIn(t, "permission", perm, `<p class="label">Wants to run</p>`, hintPermission)
		wantIn(t, "question", ask, `<p class="label">Asks</p>`, hintQuestion)
		wantIn(t, "plan", plan, `<p class="label">Plan to approve</p>`, hintPlan)
		// A report of another kind than the wait is never shown.
		wantIn(t, "odd", odd, `<span class="tool">Edit</span>  Change main.go</div>`)
		wantNotIn(t, "odd", odd, "Stale question?")
		if reported {
			wantIn(t, "permission", perm, `<div class="code"><span class="tool">Bash</span>  xcodebuild -scheme App -token [redacted] build</div>`)
			// (The reason stays in the history.)
			wantNotIn(t, "permission", perm, `</span>  Build the app</div>`)
			wantIn(t, "question", ask, `<p class="iask">Keep take 2, or record a third?</p>`,
				`<ul class="iopts"><li>Keep take 2</li><li>Record a third</li></ul>`,
				// A question with no text shows its header.
				`<p class="iask">Speed only</p>`)
			wantNotIn(t, "question", ask, `<p class="iask">AskUserQuestion</p>`)
			wantIn(t, "plan", plan, `<p class="iask">Monitored sessions</p>`,
				`<ol class="isteps"><li>Add a table (migration 0006)</li><li>POST and DELETE /api/sessions/watch</li><li>Filter Needs you</li></ol>`)
			wantNotIn(t, "plan", plan, `<p class="iask">ExitPlanMode</p>`)
		} else {
			wantIn(t, "permission", perm, `<div class="code"><span class="tool">Bash</span>  Build the app</div>`)
			wantIn(t, "question", ask, `<p class="iask">AskUserQuestion</p>`)
			wantNotIn(t, "question", ask, `class="iopts"`)
			wantIn(t, "plan", plan, `<p class="iask">ExitPlanMode</p>`)
			wantNotIn(t, "plan", plan, `class="isteps"`)
		}
		// The list row keeps its subtitle (the spec's: where it works, how
		// it is related); the detail is in the pane only.
		list := get(t, e.h, "/app/inbox?fragment=list")
		wantNotIn(t, "list", list, "xcodebuild", "Keep take 2", "Add a table")
	}

	// An item that stopped waiting shows no detail, even with a report.
	e := newInboxEnv(t)
	e.srv.opts.Waits = waitReports
	seedWaits(t, e.st)
	get(t, e.h, "/app/inbox")
	id := e.id("pid:71")
	must(t, e.st.UpsertSessions(context.Background(), store.Session{Key: "pid:71", SessionID: "s-perm", Kind: "interactive",
		Name: "perm session", Cwd: "/w/s-perm", State: "working", StartedAt: t0.Add(-time.Hour), LastSeenAt: t0}))
	d := get(t, e.h, "/app/inbox?item="+id+"&fragment=detail")
	wantNotIn(t, "stopped", d, "Wants to run", "xcodebuild")
}

func TestPlanSteps(t *testing.T) {
	for _, c := range []struct {
		plan, title string
		steps       []string
	}{
		{"", "", nil},
		{"# Title\n\n## Steps\n1. One\n* Two\n  - Three", "Title", []string{"Steps", "One", "Two", "Three"}},
		{"Just a line\n# A later heading", "", []string{"Just a line", "A later heading"}},
		{"-\n#\n10) Ten", "", []string{"Ten"}},
	} {
		title, steps := planSteps(c.plan)
		if title != c.title || strings.Join(steps, "|") != strings.Join(c.steps, "|") {
			t.Errorf("planSteps(%q) = %q, %q; want %q, %q", c.plan, title, steps, c.title, c.steps)
		}
	}
}

// GET /api/inbox carries the mod's wait detail only for a request with
// aimd's token, as GET /api/sessions does: never for a page's CSRF token, a
// wrong token or none. GET /api/inbox/{id} and POST's answer follow suit.
func TestInboxAPIWaitDetailNeedsTheToken(t *testing.T) {
	e := newInboxEnv(t)
	e.srv.opts.Waits = waitReports
	seedWaits(t, e.st)
	get(t, e.h, "/app/inbox")
	perm := e.id("pid:71")

	read := func(target string, hdr map[string]string) (string, api.Inbox) {
		t.Helper()
		rec := do(t, e.h, "GET", target, nil, hdr)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", target, rec.Code, rec.Body)
		}
		var out api.Inbox
		if strings.HasPrefix(target, "/api/inbox?") || target == "/api/inbox" {
			decode(t, rec, &out)
		}
		return rec.Body.String(), out
	}
	for name, hdr := range map[string]map[string]string{
		"no header":  nil,
		"CSRF token": {CSRFHeader: e.srv.CSRF().Token()},
		"wrong":      {TokenHeader: "not-the-token"},
	} {
		body, out := read("/api/inbox", hdr)
		if strings.Contains(body, "waiting_on") || strings.Contains(body, "xcodebuild") {
			t.Errorf("%s: GET /api/inbox carries the wait detail: %s", name, body)
		}
		if len(out.Items) == 0 {
			t.Errorf("%s: no items", name)
		}
		if one, _ := read("/api/inbox/"+perm, hdr); strings.Contains(one, "waiting_on") {
			t.Errorf("%s: GET /api/inbox/{id} carries the wait detail: %s", name, one)
		}
	}

	tok := map[string]string{TokenHeader: testToken}
	_, out := read("/api/inbox", tok)
	got := map[string]*api.WaitingOn{}
	for _, it := range out.Items {
		got[it.Title] = it.WaitingOn
		// The old detail stays, token or not.
		if it.Title == "perm session" && it.Detail != "permission prompt: Bash: Build the app" {
			t.Errorf("perm session's detail = %q", it.Detail)
		}
	}
	if w := got["perm session"]; w == nil || w.Tool != "Bash" || w.Summary != "xcodebuild -scheme App -token [redacted] build" {
		t.Errorf("perm session's waiting_on = %+v", w)
	}
	if w := got["ask session"]; w == nil || len(w.Questions) != 2 || w.Questions[0].Options[1] != "Record a third" {
		t.Errorf("ask session's waiting_on = %+v", w)
	}
	if w := got["plan session"]; w == nil || !strings.HasPrefix(w.Plan, "# Monitored sessions") {
		t.Errorf("plan session's waiting_on = %+v", w)
	}
	// No report, a report of another kind, or a PR: no waiting_on.
	for _, title := range []string{"routes agent", "odd session", "app #7 · Routes (V1)"} {
		if w, ok := got[title]; !ok || w != nil {
			t.Errorf("%s: waiting_on = %+v (listed %v), want none", title, w, ok)
		}
	}
	if one, _ := read("/api/inbox/"+perm, tok); !strings.Contains(one, `"waiting_on":{"kind":"permission"`) {
		t.Errorf("GET /api/inbox/{id} with the token: %s", one)
	}
	rec := do(t, e.h, "POST", "/api/inbox/"+perm, strings.NewReader(`{"action": "seen"}`), tok)
	var res api.InboxActResult
	decode(t, rec, &res)
	if rec.Code != http.StatusOK || res.Item.WaitingOn == nil || res.Item.WaitingOn.Tool != "Bash" {
		t.Errorf("POST /api/inbox/{id} with the token = %d, item waiting_on %+v", rec.Code, res.Item.WaitingOn)
	}
}
