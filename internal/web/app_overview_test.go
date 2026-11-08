package web

// The Overview page (app_overview.go, templates/app/overview.html).

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/focus"
	"github.com/gravexa-llc/ai-manager/internal/monitor"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/term"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// ovBoards are two plans: aim with a package in each lifecycle (W3 linked
// to PR o/app#40, W4 to the subagent s-43/a-w4) and a parser warning, and
// sharing with no packages.
var ovBoards = fakePlans{
	plans: []api.Plan{
		{Slug: "aim", Title: "ai-manager", Total: 6, Counts: map[string]int{"done": 2}, Packages: []api.Package{
			{ID: "A1", Title: "Scanner", Lifecycle: "done"},
			{ID: "W3", Title: "Triggers", Lifecycle: "in_review", Links: []api.Link{{Target: api.Target{Kind: store.LinkPR, Ref: "o/app#40"}}}},
			{ID: "W9", Title: "Cutover", Lifecycle: "blocked"},
			{ID: "W4", Title: "Overview", Lifecycle: "working", Links: []api.Link{{Target: api.Target{Kind: store.LinkAgent, Ref: "s-43/a-w4"}}}},
			{ID: "A2", Title: "Client", Lifecycle: "done"},
			{ID: "W5", Title: "Plan pages", Lifecycle: "ready"},
		}, Warnings: []api.Warning{{Kind: "row", Line: 12, Message: "a row without an ID"}}},
		{Slug: "sharing", Title: "Pivot share links", Counts: map[string]int{}},
	},
	sums: map[string]api.PlanSummary{"aim": {NeedsYou: 3}, "sharing": {}},
}

// ovStatus is a daemon up 4 hours whose session poll and GitHub are fine.
var ovStatus = fakeStatus{api.Status{
	Version: "v1.2", StartedAt: t0.Add(-4 * time.Hour), Now: t0,
	Sources: []api.Source{
		{Name: api.SourceSessions, Runs: 40, LastRun: t0.Add(-3 * time.Second), LastOK: t0.Add(-3 * time.Second)},
		{Name: api.SourceGitHub, Runs: 4, LastOK: t0.Add(-time.Minute)},
	},
	GitHub: api.GitHubStatus{Budget: api.Budget{Verdict: "normal", Known: true, Remaining: 4961, Limit: 5000, ResetAt: t0.Add(40 * time.Minute)}},
}}

// plainStore is a Store that is only the interface: no inbox, no history.
type plainStore struct{ Store }

var strongRE = regexp.MustCompile(`<strong>([^<]*)</strong>`)

// feedWhats are the activity feed's entries, newest first: what each says.
func feedWhats(t *testing.T, page string) []string {
	t.Helper()
	var out []string
	for _, m := range strongRE.FindAllStringSubmatch(section(t, page, "ov-activity"), -1) {
		out = append(out, m[1])
	}
	return out
}

// ovRows are the agents card's rows, one string each.
func ovRows(t *testing.T, page string) []string {
	t.Helper()
	parts := strings.Split(section(t, page, "ov-agents"), `<li class="ovagent`)
	return parts[1:]
}

var nameRE = regexp.MustCompile(`class="ovagent-name"[^>]*>([^<]*)<`)

func rowNames(t *testing.T, rows []string) []string {
	t.Helper()
	var out []string
	for _, r := range rows {
		m := nameRE.FindStringSubmatch(r)
		if m == nil {
			t.Fatalf("row without a name: %s", r)
		}
		out = append(out, m[1])
	}
	return out
}

func sameList(t *testing.T, where string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s:\n got %q\nwant %q", where, got, want)
	}
}

func TestOverviewPage(t *testing.T) {
	srv, _ := newApp(t, nil)
	h := srv.Handler()
	page := get(t, h, "/app/")
	wantIn(t, "overview", page,
		`<link rel="stylesheet" href="/static/app-overview.css?v=`+assetVersion+`">`,
		"<h1>Overview</h1>", "<p>Your plans, agents and repos at a glance</p>",
		`<div id="content" class="content" data-live="">`,
		`<section id="ov-plans" class="card" aria-labelledby="ov-plans-h" data-live="plans inbox">`,
		`<section id="ov-agents" class="card" aria-labelledby="ov-agents-h" data-live="sessions plans">`,
		`<section id="ov-activity" class="card" aria-labelledby="ov-activity-h" data-live="prs plans sessions">`,
		`<section id="ov-health" class="card" aria-labelledby="ov-health-h" data-live="status worktrees prs sessions plans">`,
		`<h2 id="ov-plans-h">Plan progress</h2>`, `<h2 id="ov-agents-h">Agents running now</h2>`,
		`<h2 id="ov-activity-h">Activity, last 24 hours</h2>`, `<h2 id="ov-health-h">Cleanup &amp; health</h2>`,
	)
	wantNotIn(t, "overview", page, "Built by W4", `class="stub"`)

	// Each card is a fragment: its own region, and no other.
	cards := []string{"ov-plans", "ov-agents", "ov-activity", "ov-health"}
	for _, id := range cards {
		rec := do(t, h, "GET", "/app/?fragment="+id, nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("fragment %s = %d: %s", id, rec.Code, rec.Body)
		}
		body := rec.Body.String()
		wantIn(t, id, body, `<h2 id="`+id+`-h">`)
		wantNotIn(t, id, body, "<html", `id="menu"`, `id="content"`, `<section`)
		for _, other := range cards {
			if other != id {
				wantNotIn(t, id, body, `<h2 id="`+other+`-h">`)
			}
		}
		if !srv.CSRF().Valid(rec.Header().Get(appCSRFHeader)) {
			t.Errorf("fragment %s: no fresh CSRF token", id)
		}
		// The fragment is what the page shows in that card.
		if inner := section(t, page, id); !strings.Contains(inner, strings.TrimSpace(body)) {
			t.Errorf("fragment %s differs from the page's card:\n%s\n---\n%s", id, body, inner)
		}
	}
	content := get(t, h, "/app/?fragment=content")
	for _, id := range cards {
		wantIn(t, "content", content, `<section id="`+id+`"`)
	}
	wantIn(t, "summary", get(t, h, "/app/?fragment=summary"), "<h1>Overview</h1>")
	if rec := do(t, h, "GET", "/app/?fragment=ov-nope", nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("an unknown card = %d, want 404", rec.Code)
	}

	// Its stylesheet is served, and is only classes.
	rec := do(t, h, "GET", "/static/app-overview.css", nil, nil)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/css; charset=utf-8" {
		t.Errorf("GET /static/app-overview.css = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	wantNotIn(t, "app-overview.css", rec.Body.String(), "@import", "url(", "fonts.googleapis")
}

func TestOverviewPlanProgress(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	// Two PRs need you, one on each plan; a session needing you is on none.
	must(t, st.UpsertPRs(ctx,
		store.PR{Repo: "o/app", Number: 40, Title: "Triggers (W3)", State: store.PROpen, HeadBranch: "aim/w3", MergeStateStatus: "CLEAN",
			ChecksState: "SUCCESS", PlanSlug: "aim", PlanPkg: "W3", URL: "https://github.com/o/app/pull/40", UpdatedAt: t0.Add(-time.Hour)},
		store.PR{Repo: "o/web", Number: 41, Title: "Copy", State: store.PROpen, HeadBranch: "t1", MergeStateStatus: "BLOCKED",
			PlanSlug: "sharing", PlanPkg: "T1", URL: "https://github.com/o/web/pull/41", UpdatedAt: t0.Add(-time.Hour)},
	))
	_, err := st.SetReviewRequests(ctx, []string{"o"}, []store.PRKey{{Repo: "o/web", Number: 41}}, t0.Add(-time.Hour))
	must(t, err)
	must(t, st.UpsertSessions(ctx, store.Session{Key: "pid:9", Kind: "interactive", Name: "asks", State: needsYou,
		StateReason: "permission prompt", NeedsYouSince: t0.Add(-time.Minute), LastSeenAt: t0}))
	h := newDash(t, st, func(o *Options) { o.Plans = ovBoards }).Handler()
	card := get(t, h, "/app/?fragment=ov-plans")

	plans := strings.Split(card, `<li><a class="ovplan"`)
	if len(plans) != 3 {
		t.Fatalf("want 2 plans, got:\n%s", card)
	}
	aim, sharing := plans[1], plans[2]
	wantIn(t, "aim", aim, ` href="/app/plans/aim">`, "<strong>ai-manager</strong>", `<span class="ovplan-sum">2 of 6 done</span>`,
		`aria-label="2 done, 1 in review, 1 working, 1 ready, 1 blocked"`,
		`<span class="legend lc-done"><span class="sw" aria-hidden="true"></span>done 2</span>`,
		`<span class="legend lc-in_review"><span class="sw" aria-hidden="true"></span>in review 1</span>`,
		`<span class="legend lc-blocked"><span class="sw" aria-hidden="true"></span>blocked 1</span>`,
		// Its Open inbox items: #40, ready to merge.
		`<span class="legend ovneeds">1 needs you</span>`)
	// One segment per package, done first, then in review, working, ready
	// and blocked; never an inline width.
	segRE := regexp.MustCompile(`<span class="(lc-[a-z_]+)" title="([^"]*)"></span>`)
	var segs []string
	for _, m := range segRE.FindAllStringSubmatch(aim, -1) {
		segs = append(segs, m[1]+" "+m[2])
	}
	sameList(t, "aim's bar", segs, []string{
		"lc-done A1 Scanner: done", "lc-done A2 Client: done", "lc-in_review W3 Triggers: in review",
		"lc-working W4 Overview: working", "lc-ready W5 Plan pages: ready", "lc-blocked W9 Cutover: blocked",
	})
	wantIn(t, "sharing", sharing, ` href="/app/plans/sharing">`, "0 of 0 done", `aria-label="no packages"`,
		// #41: your review is requested.
		`<span class="legend ovneeds">1 needs you</span>`)
	wantNotIn(t, "sharing", sharing, `class="legend lc-`)

	// Without an inbox, the count is the plan summary's, as in the menu.
	bare := newDash(t, plainStore{st}, func(o *Options) { o.Plans = ovBoards }).Handler()
	wantIn(t, "no inbox", get(t, bare, "/app/?fragment=ov-plans"), `<span class="legend ovneeds">3 need you</span>`)

	// The empty states.
	for name, c := range map[string]struct {
		plans PlanService
		want  string
	}{
		"no service": {nil, "Plans aren&#39;t available: this aimd runs without its plan service."},
		"none":       {fakePlans{}, "No plans registered."},
		"failing":    {fakePlans{err: errors.New("boom")}, "aimd couldn&#39;t read the plans; see aimd.log."},
	} {
		h := newDash(t, newTestStore(t), func(o *Options) { o.Plans = c.plans }).Handler()
		card := get(t, h, "/app/?fragment=ov-plans")
		wantIn(t, name, card, `<p class="empty">`+c.want)
		wantNotIn(t, name, card, `class="ovplan"`)
	}
}

// ovAgentsWorld is the agents card's world: sessions in each state and
// terminal, live and ended subagents, and a Go service that remembers.
func ovAgentsWorld(t *testing.T, opt func(*Options)) (*Server, *store.Store, *stubGo) {
	t.Helper()
	st := newTestStore(t)
	ctx := context.Background()
	must(t, st.UpsertSessions(ctx,
		store.Session{Key: "pid:42", SessionID: "s-42", PID: 42, Kind: "interactive", Name: "needy tab", Cwd: "/w/a",
			State: needsYou, StateReason: "agent a-live: permission prompt", NeedsYouSince: t0.Add(-5 * time.Minute),
			StartedAt: t0.Add(-time.Hour), LastSeenAt: t0},
		store.Session{Key: "pid:43", SessionID: "s-43", PID: 43, Kind: "interactive", Name: "busy tab", Cwd: "/w/b",
			State: "working", StartedAt: t0.Add(-2 * time.Hour), LastSeenAt: t0},
		store.Session{Key: "job:bg1", JobID: "bg1", SessionID: "s-bg", Kind: "background", Name: "blocked job", Cwd: "/w/c",
			State: needsYou, StateReason: "reply", NeedsYouSince: t0.Add(-time.Hour), LastSeenAt: t0},
		store.Session{Key: "pid:44", SessionID: "s-44", PID: 44, Kind: "interactive", Name: "tmux run", Cwd: "/w/d",
			State: "idle", StartedAt: t0.Add(-30 * time.Minute), LastSeenAt: t0},
		store.Session{Key: "pid:45", SessionID: "s-45", PID: 45, Kind: "interactive", Headless: true, Name: "headless run",
			Cwd: "/tmp/h", State: "working", StartedAt: t0.Add(-time.Minute), LastSeenAt: t0},
	))
	must(t, st.UpdateSessionLive(ctx, "pid:43", store.SessionLive{State: "working", LastAction: "Bash: make check"}))
	must(t, st.UpsertAgents(ctx,
		store.Agent{SessionID: "s-42", AgentID: "a-live", Kind: store.AgentSubagent, AgentType: "general-purpose", State: "running",
			LastAction: "Edit: x.go", StartedAt: t0.Add(-20 * time.Minute)},
		store.Agent{SessionID: "s-42", AgentID: "a-done", Kind: store.AgentSubagent, AgentType: "Explore", State: "completed",
			StartedAt: t0.Add(-40 * time.Minute), EndedAt: t0.Add(-10 * time.Minute)},
		store.Agent{SessionID: "s-43", AgentID: "a-w4", Kind: store.AgentSubagent, State: "running", StartedAt: t0.Add(-15 * time.Minute)},
	))
	g := &stubGo{res: api.GoResult{Key: "pid:42", Action: api.GoFocused, App: "iTerm2", Message: "Focused needy tab in iTerm2."}}
	srv := newDash(t, st, func(o *Options) {
		o.Plans = ovBoards
		o.Go = g
		o.Terminals = fakeTerms{"pid:42": appITerm2, "pid:43": appTerminal, "pid:44": appTmux, "pid:45": appITerm2}
		if opt != nil {
			opt(o)
		}
	})
	return srv, st, g
}

func TestOverviewAgents(t *testing.T) {
	srv, _, _ := ovAgentsWorld(t, nil)
	h := srv.Handler()
	page := get(t, h, "/app/")
	rows := ovRows(t, page)
	// Waiting on you first (longest wait first), then working, then the
	// rest; each session's live subagents under it. The headless run and
	// the finished subagent are left out.
	sameList(t, "agents", rowNames(t, rows), []string{
		"blocked job", "needy tab", "Agent · general-purpose", "busy tab", "Agent · W4 Overview", "tmux run",
	})
	job, needy, sub, busy, w4, tmux := rows[0], rows[1], rows[2], rows[3], rows[4], rows[5]

	wantIn(t, "job", job, `<span class="dot kind-reply"`, "No plan · waiting for your reply · ", `title="waiting since 2026-11-05T11:00:00Z">1h<`,
		`<span class="tag term-none"`, ">no terminal<",
		`<button type="button" class="btn sm" disabled title="`+goOffBackground("bg1")+`">Go<span class="vh"> (unavailable: `)
	wantIn(t, "needy", needy, `<span class="dot kind-permission"`, "No plan · waiting on you: permission prompt (from its agent) · ",
		`title="waiting since 2026-11-05T11:55:00Z">5m<`, `<span class="tag term-iterm2" title="Runs in an iTerm2 tab">iTerm2</span>`,
		`<form method="post" action="/app/overview/go" data-pane="ov-agents">`, `<input type="hidden" name="key" value="pid:42">`,
		`<input type="hidden" name="return" value="/app/">`, `<button type="submit" class="btn sm" title="Switch to its terminal tab">Go</button>`)
	// The subagent asking is the one the lead's reason names; Go goes to
	// its lead's tab.
	wantIn(t, "subagent", sub, ` sub">`, "No plan · in needy tab · waiting on you: permission prompt · ",
		`<span class="tag term-iterm2" title="Runs inside its lead session, needy tab. Runs in an iTerm2 tab">iTerm2 · lead&#39;s tab</span>`,
		`<input type="hidden" name="key" value="s-42/a-live">`)
	wantIn(t, "busy", busy, `<span class="dot lc-working"`, "No plan · working: Bash: make check · ",
		`title="running since 2026-11-05T10:00:00Z">2h<`, `>Terminal</span>`, `name="key" value="pid:43"`)
	// A subagent a plan links is named by its package.
	wantIn(t, "w4", w4, "ai-manager · agent of W4 · working · ", `>15m<`, `name="key" value="s-43/a-w4"`)
	wantIn(t, "tmux", tmux, `<span class="dot tone-quiet"`, "No plan · idle · ", `>tmux</span>`,
		`disabled title="`+goOffTmux+`"`)
	for i, r := range rows {
		if strings.Contains(r, "disabled") == strings.Contains(r, "<form") {
			t.Errorf("row %d has both or neither of a Go form and a disabled Go: %s", i, r)
		}
	}
	wantNotIn(t, "agents", section(t, page, "ov-agents"), "headless run", "Explore", `class="hint"`)

	// Without Go, no buttons at all.
	noGo, _, _ := ovAgentsWorld(t, func(o *Options) { o.Go = nil })
	card := get(t, noGo.Handler(), "/app/?fragment=ov-agents")
	wantNotIn(t, "no Go", card, "<button", "<form")
	wantIn(t, "no Go", card, "needy tab", "iTerm2")

	// Nothing running.
	empty := newDash(t, newTestStore(t), nil).Handler()
	wantIn(t, "empty", get(t, empty, "/app/?fragment=ov-agents"), `<p class="empty">No session is running.</p>`)
}

// TestOverviewAgentsFollowMonitoring checks the card against the
// monitoring rules (internal/monitor): only the monitored sessions, and a
// count of the rest.
func TestOverviewAgentsFollowMonitoring(t *testing.T) {
	e := newMonitorEnv(t, monitor.ModeOptIn)
	page := get(t, e.h, "/app/")
	rows := ovRows(t, page)
	names := rowNames(t, rows)
	sameList(t, "monitored", names, []string{"watched one", "aim lead", "Agent · D1 Event receiver and live state", "c6 agent"})
	byName := map[string]string{}
	for i, n := range names {
		byName[n] = rows[i]
	}
	wantIn(t, "watched", byName["watched one"], "No plan · watched · waiting on you: permission prompt")
	wantIn(t, "c6", byName["c6 agent"], "· agent of C6 · working: Bash: make check")
	wantIn(t, "lead", byName["aim lead"], " · lead · working")
	wantIn(t, "d1", byName["Agent · D1 Event receiver and live state"], "· agent of D1 · working: Edit: live.go")
	// The stranger, the session with no id yet and the loose one aren't
	// monitored; the headless run isn't counted.
	card := section(t, page, "ov-agents")
	wantIn(t, "hint", card, `<p class="hint">3 more sessions aren't monitored. <a href="/app/sessions">See Sessions</a>.</p>`)
	wantNotIn(t, "hint", card, "waiting stranger", "loose session", "headless run")
}

func TestOverviewGo(t *testing.T) {
	srv, _, g := ovAgentsWorld(t, nil)
	h := srv.Handler()
	post := func(key string, hdr map[string]string) (int, appAnswer, string) {
		t.Helper()
		vals := url.Values{"key": {key}, "return": {"/app/"}, CSRFField: {srv.CSRF().Token()}}
		rec := do(t, h, "POST", "/app/overview/go", strings.NewReader(vals.Encode()), hdr)
		var a appAnswer
		if strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
			decode(t, rec, &a)
		}
		return rec.Code, a, rec.Header().Get("Location")
	}
	jsonHdr := map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Accept": "application/json", CSRFHeader: srv.CSRF().Token()}

	// A session in iTerm2, and a subagent (its lead's tab).
	for _, key := range []string{"pid:42", "s-42/a-live", "pid:43"} {
		g.keys = nil
		code, a, _ := post(key, jsonHdr)
		if code != http.StatusOK || a.Location != "/app/" || a.Toast == nil || a.Toast.Text != "Focused needy tab in iTerm2." || a.Toast.Undo != nil {
			t.Errorf("Go %s = %d %+v (toast %+v)", key, code, a, a.Toast)
		}
		if len(g.keys) != 1 || g.keys[0] != key {
			t.Errorf("Go %s asked the service for %v", key, g.keys)
		}
	}

	// Refusals: Go's rules, a session gone, no key. The service isn't asked.
	g.keys = nil
	for key, want := range map[string]struct {
		code int
		msg  string
	}{
		"job:bg1":      {http.StatusUnprocessableEntity, goOffBackground("bg1")},
		"pid:44":       {http.StatusUnprocessableEntity, goOffTmux},
		"pid:45":       {http.StatusUnprocessableEntity, goOffHeadless},
		"pid:999":      {http.StatusConflict, "That session isn't running any more."},
		"s-999/a-live": {http.StatusConflict, "That session isn't running any more."},
		"needy":        {http.StatusConflict, "That session isn't running any more."},
		"":             {http.StatusBadRequest, "Go where? The form names no session."},
	} {
		code, a, _ := post(key, jsonHdr)
		if code != want.code || a.Error != want.msg {
			t.Errorf("Go %q = %d %q, want %d %q", key, code, a.Error, want.code, want.msg)
		}
	}
	if len(g.keys) != 0 {
		t.Errorf("refused Go asked the service for %v", g.keys)
	}

	// The service's own refusal comes back with its status.
	g.err = &focus.Error{Status: http.StatusUnprocessableEntity, Msg: "no iTerm2 tab has needy tab"}
	if code, a, _ := post("pid:42", jsonHdr); code != http.StatusUnprocessableEntity || a.Error != "Go: no iTerm2 tab has needy tab" {
		t.Errorf("service refusal = %d %+v", code, a)
	}
	g.err = errors.New("osascript on fire")
	if code, a, _ := post("pid:42", jsonHdr); code != http.StatusInternalServerError || a.Error != "Go failed; see aimd.log." {
		t.Errorf("service failure = %d %+v", code, a)
	}
	g.err = nil

	// A plain form: a 303 back to the Overview, with the toast.
	code, _, loc := post("pid:42", formHeaders)
	if code != http.StatusSeeOther || !strings.HasPrefix(loc, "/app/?toast=") {
		t.Fatalf("plain Go = %d to %q", code, loc)
	}
	wantIn(t, "plain Go", section(t, get(t, h, loc), "toasts"), "<span>Focused needy tab in iTerm2.</span>")
	code, _, loc = post("pid:44", formHeaders)
	if code != http.StatusSeeOther {
		t.Fatalf("plain refusal = %d", code)
	}
	wantIn(t, "plain refusal", section(t, get(t, h, loc), "toasts"), `<div class="toast error" role="alert" data-toast>`, "tmux")

	// Without a token, CSRF.Protect refuses it.
	if rec := do(t, h, "POST", "/app/overview/go", strings.NewReader("key=pid:42"), formHeaders); rec.Code != http.StatusForbidden {
		t.Errorf("Go without a token = %d, want 403", rec.Code)
	}
	// Without a Go service, it says so.
	noGo, _, _ := ovAgentsWorld(t, func(o *Options) { o.Go = nil })
	vals := url.Values{"key": {"pid:42"}, CSRFField: {noGo.CSRF().Token()}}
	rec := do(t, noGo.Handler(), "POST", "/app/overview/go", strings.NewReader(vals.Encode()),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Accept": "application/json", CSRFHeader: noGo.CSRF().Token()})
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `Go isn't available`) {
		t.Errorf("Go without a service = %d %s", rec.Code, rec.Body)
	}
}

// ovActivityWorld stores a day of activity, and some from before it.
func ovActivityWorld(t *testing.T) *store.Store {
	t.Helper()
	st := newTestStore(t)
	ctx := context.Background()
	ago := func(d time.Duration) time.Time { return t0.Add(-d) }

	// Plans: aim registered 3 hours ago, old three days ago.
	aim, err := st.InsertPlan(ctx, store.Plan{Slug: "aim", Title: "stored aim title", CreatedAt: ago(3 * time.Hour)})
	must(t, err)
	old, err := st.InsertPlan(ctx, store.Plan{Slug: "old", Title: "Old plan", CreatedAt: ago(72 * time.Hour)})
	must(t, err)
	for i, id := range []string{"A1", "W3", "W4", "V1", "D1"} {
		must(t, st.InsertPackage(ctx, store.Package{PlanID: aim, PkgID: id, Seq: i}))
	}
	must(t, st.InsertPackage(ctx, store.Package{PlanID: old, PkgID: "X1"}))
	// The first pass (A1 already done) isn't news; W3 turning done is.
	must(t, st.SetLifecycles(ctx, aim, []store.PackageLifecycle{
		{PkgID: "A1", Lifecycle: "done", At: ago(3 * time.Hour)},
		{PkgID: "W3", Lifecycle: "in_review", At: ago(3 * time.Hour)},
	}))
	must(t, st.SetLifecycles(ctx, aim, []store.PackageLifecycle{{PkgID: "W3", Lifecycle: "done", Prev: "in_review", At: ago(time.Hour)}}))
	must(t, st.SetLifecycles(ctx, old, []store.PackageLifecycle{{PkgID: "X1", Lifecycle: "done", Prev: "working", At: ago(30 * time.Hour)}}))
	// Links: one by hand, one spawn, one the matcher made, and an old one.
	for _, l := range []store.Link{
		{PlanID: aim, PkgID: "V1", Kind: store.LinkPR, Ref: "o/app#7", MatchMethod: store.MatchManual, Confidence: store.ConfidenceExact, CreatedAt: ago(45 * time.Minute)},
		{PlanID: aim, PkgID: "D1", Kind: store.LinkAgent, SessionID: "s-lead", AgentID: "a-d1", MatchMethod: store.MatchSpawn, Confidence: store.ConfidenceExact, CreatedAt: ago(20 * time.Minute)},
		// A background job dispatched for W4: the spawn link names it by its job.
		{PlanID: aim, PkgID: "W4", Kind: store.LinkAgent, JobID: "bg9", MatchMethod: store.MatchSpawn, Confidence: store.ConfidenceExact, CreatedAt: ago(4*time.Hour + time.Minute)},
		{PlanID: aim, PkgID: "W4", Kind: store.LinkBranch, Ref: "o/app:aim/w4", MatchMethod: store.MatchBranch, Confidence: store.ConfidenceLow, CreatedAt: ago(10 * time.Minute)},
		{PlanID: aim, PkgID: "A1", Kind: store.LinkPR, Ref: "o/app#1", MatchMethod: store.MatchManual, Confidence: store.ConfidenceExact, CreatedAt: ago(30 * time.Hour)},
	} {
		_, err := st.UpsertLink(ctx, l)
		must(t, err)
	}

	// PRs: W3's opened (its board link names it), M4's merged (its Plan:
	// line), one on no plan merged, one opened before GitHub's createdAt
	// was stored, and an old merge.
	must(t, st.UpsertPRs(ctx,
		store.PR{Repo: "o/app", Number: 40, Title: "Triggers (W3)", State: store.PROpen, URL: "https://github.com/o/app/pull/40",
			CreatedAt: ago(2 * time.Hour), FirstSeenAt: ago(2 * time.Hour)},
		store.PR{Repo: "o/app", Number: 31, Title: "aim up reload", State: store.PRMerged, PlanSlug: "aim", PlanPkg: "M4",
			URL: "https://github.com/o/app/pull/31", CreatedAt: ago(50 * time.Hour), MergedAt: ago(30 * time.Minute)},
		store.PR{Repo: "o/web", Number: 5, Title: "Tidy", State: store.PRMerged, URL: "https://github.com/o/web/pull/5",
			CreatedAt: ago(26 * time.Hour), MergedAt: ago(90 * time.Minute)},
		store.PR{Repo: "o/web", Number: 6, Title: "Older row", State: store.PROpen, URL: "https://evil.example/pull/6", FirstSeenAt: ago(5 * time.Hour)},
		store.PR{Repo: "o/app", Number: 20, Title: "Yesterday's", State: store.PRMerged, CreatedAt: ago(80 * time.Hour), MergedAt: ago(30 * time.Hour)},
	))

	// Sessions: the lead (started before the window) with its subagents,
	// one started and one ended in the window, a background job, a
	// headless run and one that ended yesterday.
	must(t, st.UpsertSessions(ctx,
		store.Session{Key: "pid:50", SessionID: "s-lead", PID: 50, Kind: "interactive", Name: "aim lead", Branch: "main", State: "working",
			StartedAt: ago(6 * time.Hour), LastSeenAt: t0},
		store.Session{Key: "pid:51", SessionID: "s-quick", PID: 51, Kind: "interactive", Name: "quick look", State: "idle",
			StartedAt: ago(15 * time.Minute), LastSeenAt: t0},
		store.Session{Key: "pid:52", SessionID: "s-old", PID: 52, Kind: "interactive", Name: "old work", Branch: "fix", State: "idle",
			StartedAt: ago(5 * time.Hour), LastSeenAt: ago(10 * time.Minute)},
		store.Session{Key: "job:bg9", JobID: "bg9", SessionID: "s-bg9", Kind: "background", Name: "nightly", State: "working",
			StartedAt: ago(4 * time.Hour), LastSeenAt: t0},
		store.Session{Key: "job:bg8", JobID: "bg8", SessionID: "s-bg8", Kind: "background", Name: "weekly", State: "idle",
			StartedAt: ago(7 * time.Hour), LastSeenAt: t0},
		store.Session{Key: "pid:53", SessionID: "s-h", PID: 53, Kind: "interactive", Headless: true, Name: "headless run", State: "working",
			StartedAt: ago(5 * time.Minute), LastSeenAt: t0},
		store.Session{Key: "pid:54", SessionID: "s-y", PID: 54, Kind: "interactive", Name: "yesterday", State: "idle",
			StartedAt: ago(50 * time.Hour), LastSeenAt: ago(40 * time.Hour)},
	))
	must(t, st.EndSession(ctx, "pid:52", ago(10*time.Minute)))
	must(t, st.EndSession(ctx, "pid:54", ago(40*time.Hour)))
	must(t, st.UpsertAgents(ctx,
		// D1's agent: its spawn link says it started, a minute before its row.
		store.Agent{SessionID: "s-lead", AgentID: "a-d1", Kind: store.AgentSubagent, State: "running", StartedAt: ago(19 * time.Minute)},
		store.Agent{SessionID: "s-lead", AgentID: "a-x", Kind: store.AgentSubagent, AgentType: "Explore", State: "completed",
			StartedAt: ago(50 * time.Minute), EndedAt: ago(40 * time.Minute)},
		store.Agent{SessionID: "s-lead", AgentID: "a-f", Kind: store.AgentSubagent, State: "failed",
			StartedAt: ago(25 * time.Hour), EndedAt: ago(3 * time.Hour)},
		store.Agent{SessionID: "s-h", AgentID: "a-h", Kind: store.AgentSubagent, State: "running", StartedAt: ago(4 * time.Minute)},
	))
	return st
}

func TestOverviewActivity(t *testing.T) {
	st := ovActivityWorld(t)
	h := newDash(t, st, func(o *Options) { o.Plans = ovBoards }).Handler()
	page := get(t, h, "/app/")
	// Newest first; at the same time, a finish before a registration and
	// a PR before a session's start.
	sameList(t, "feed", feedWhats(t, page), []string{
		"Session old work finished",    // 10m
		"Session quick look started",   // 15m
		"D1 agent started",             // 20m: the spawn link and the agent row are one start
		"M4 merged",                    // 30m
		"Agent (Explore) finished",     // 40m
		"o/app#7 linked to V1 by hand", // 45m
		"Agent (Explore) started",      // 50m
		"W3 done",                      // 1h
		"PR merged",                    // 1h30
		"W3 PR opened",                 // 2h
		"Agent failed",                 // 3h
		"Plan registered",              // 3h
		"W4 agent started",             // 4h: the job's spawn link and its row are one start
		"PR opened",                    // 5h: o/web#6, first stored then
		"Session old work started",     // 5h
		"Session aim lead started",     // 6h
		"Background job weekly started",
	})
	feed := section(t, page, "ov-activity")
	wantIn(t, "feed", feed,
		`<time class="when" datetime="2026-11-05T11:50:00Z" title="11:50">10m</time>`,
		// A PR links to GitHub, in a new tab; its plan is the board's.
		`<strong>W3 PR opened</strong> <a href="https://github.com/o/app/pull/40" target="_blank" rel="noopener noreferrer">ai-manager · o/app#40 · Triggers (W3)</a>`,
		`<strong>M4 merged</strong> <a href="https://github.com/o/app/pull/31" target="_blank" rel="noopener noreferrer">ai-manager · o/app#31 · aim up reload</a>`,
		`<strong>PR merged</strong> <a href="https://github.com/o/web/pull/5"`,
		// Not a GitHub link: no link.
		`<strong>PR opened</strong> <span class="ovfeed-where">o/web#6 · Older row</span>`,
		`<strong>W3 done</strong> <a href="/app/plans/aim?pkg=W3">ai-manager · W3 Triggers</a>`,
		`<strong>Plan registered</strong> <a href="/app/plans/aim">ai-manager</a>`,
		`<strong>o/app#7 linked to V1 by hand</strong> <a href="/app/plans/aim?pkg=V1">ai-manager</a>`,
		`<strong>D1 agent started</strong> <a href="/app/plans/aim?pkg=D1">ai-manager · D1</a>`,
		`<strong>Agent (Explore) finished</strong> <span class="ovfeed-where">in aim lead</span>`,
		`<strong>Session old work finished</strong> <span class="ovfeed-where">No plan · fix</span>`,
	)
	wantNotIn(t, "feed", feed, "headless", "yesterday", "Yesterday", "X1", "Old plan", "nightly", "Branch", "#1 ", "#20", "Show ", "evil.example")

	// The fragment says the same.
	sameList(t, "fragment", feedWhats(t, `<div id="ov-activity">`+get(t, h, "/app/?fragment=ov-activity")+`</div>`), feedWhats(t, page))

	// Without the store's history (another Store), PRs and sessions still show.
	bare := newDash(t, plainStore{st}, func(o *Options) { o.Plans = ovBoards }).Handler()
	got := feedWhats(t, get(t, bare, "/app/"))
	if len(got) != 14 || got[2] != "Agent started" || !slices.Contains(got, "Background job nightly started") {
		t.Errorf("without history: %q", got)
	}
	for _, w := range got {
		if w == "W3 done" || w == "Plan registered" || strings.Contains(w, "by hand") {
			t.Errorf("without history the feed still has %q", w)
		}
	}

	// Nothing in the window.
	empty := newDash(t, newTestStore(t), nil).Handler()
	wantIn(t, "empty", get(t, empty, "/app/?fragment=ov-activity"), `<p class="empty">Nothing happened in the last 24 hours.</p>`)
}

// TestOverviewActivityFollowsMonitoring checks that sessions and PRs
// monitoring doesn't count stay out of the feed, but for a review
// requested from you.
func TestOverviewActivityFollowsMonitoring(t *testing.T) {
	for _, c := range []struct {
		mode string
		all  bool
	}{{monitor.ModeOptIn, false}, {monitor.ModeAll, true}} {
		e := newMonitorEnv(t, c.mode)
		ctx := context.Background()
		pr := func(repo string, n int, branch, title string) store.PR {
			return store.PR{Repo: repo, Number: n, HeadBranch: branch, Title: title, State: store.PROpen, BaseRef: "main",
				URL: "https://github.com/" + repo + "/pull/" + strconv.Itoa(n), CreatedAt: t0.Add(-20 * time.Minute)}
		}
		// A1's PR (on the board) merges; #61 is on no plan and no
		// monitored branch; on another owner's repo your review is
		// requested. #31 is on the watched session's branch.
		a1 := pr(aimRepo, 7, "aim/a1-x", "Work (A1)")
		a1.State, a1.MergedAt, a1.CreatedAt, a1.PlanSlug, a1.PlanPkg = store.PRMerged, t0.Add(-10*time.Minute), t0.Add(-48*time.Hour), "aim", "A1"
		must(t, e.st.UpsertPRs(ctx, a1, pr(aimRepo, 61, "random", "Stray work"), pr("x/other", 62, "fix", "Their fix")))
		_, err := e.st.SetReviewRequests(ctx, []string{"x"}, []store.PRKey{{Repo: "x/other", Number: 62}}, t0.Add(-15*time.Minute))
		must(t, err)

		page := get(t, e.h, "/app/")
		got := feedWhats(t, page)
		feed := section(t, page, "ov-activity")
		// The C6 session is linked by hand: it starts as C6's agent. D1's
		// agent is a subagent of the plan's lead.
		for _, w := range []string{"C6 agent started", "D1 agent started", "Session aim lead started", "A1 merged"} {
			if !slices.Contains(got, w) {
				t.Errorf("%s: feed %q lacks %q", c.mode, got, w)
			}
		}
		wantIn(t, c.mode, feed, aimRepo+"#31 · Watched work", "x/other#62 · Their fix")
		if slices.Contains(got, "Session loose session started") != c.all {
			t.Errorf("%s: feed %q, loose session shown: %v", c.mode, got, !c.all)
		}
		if strings.Contains(feed, aimRepo+"#61") != c.all {
			t.Errorf("%s: #61, on no plan or monitored branch, shown: %v", c.mode, !c.all)
		}
	}
}

func TestOverviewActivityCap(t *testing.T) {
	st := newTestStore(t)
	var prs []store.PR
	for i := range activityCap + 10 {
		prs = append(prs, store.PR{Repo: "o/app", Number: 100 + i, Title: "PR " + strconv.Itoa(i), State: store.PROpen,
			CreatedAt: t0.Add(-time.Duration(i+1) * time.Minute)})
	}
	must(t, st.UpsertPRs(context.Background(), prs...))
	h := newDash(t, st, nil).Handler()

	page := get(t, h, "/app/")
	if n := len(feedWhats(t, page)); n != activityCap {
		t.Errorf("the feed shows %d entries, want %d", n, activityCap)
	}
	feed := section(t, page, "ov-activity")
	wantIn(t, "capped", feed, `<p class="ovmore"><a href="/app/?feed=all" data-pane="ov-activity">Show 10 more</a></p>`, "PR 29")
	wantNotIn(t, "capped", feed, "PR 30", "Show the newest only")

	// ?feed=all shows them all, whole or as the card's fragment.
	for _, target := range []string{"/app/?feed=all", "/app/?feed=all&fragment=content"} {
		all := section(t, get(t, h, target), "ov-activity")
		if n := strings.Count(all, `<li class="feed">`); n != activityCap+10 {
			t.Errorf("%s shows %d entries", target, n)
		}
		wantIn(t, target, all, `<a href="/app/" data-pane="ov-activity">Show the newest only</a>`, "PR 39")
		wantNotIn(t, target, all, "more</a>")
	}
	frag := get(t, h, "/app/?feed=all&fragment=ov-activity")
	if n := strings.Count(frag, `<li class="feed">`); n != activityCap+10 {
		t.Errorf("the ?feed=all fragment shows %d entries", n)
	}
}

func TestOverviewHealth(t *testing.T) {
	srv, _ := newApp(t, func(o *Options) {
		o.Status = ovStatus
		o.Plans = ovBoards
		// Expired jobs: job:j1's conversation is gone; job:j2's isn't.
		o.Jobs = fakeJobs{"j1": 30}
	})
	page := get(t, srv.Handler(), "/app/")
	card := section(t, page, "ov-health")
	// The counts, each linking its page: seedDash's -old and -gone are safe
	// to remove, -gone's folder is gone, o/web#3 is a stale draft, and
	// job:j1 has expired.
	wantIn(t, "counts", card,
		`<a class="stat" href="/app/worktrees" title="aim prune&#39;s rules: merged, clean, nothing local only"><strong>2</strong><span>worktrees safe to remove</span></a>`,
		`<a class="stat" href="/app/worktrees" title="git still lists them; git worktree prune clears them"><strong>1</strong><span>records whose folder is gone</span></a>`,
		`<a class="stat" href="/app/prs" title="Open drafts with no update for 30 days"><strong>1</strong><span>stale draft PRs</span></a>`,
		`<a class="stat" href="/app/sessions" title="Background jobs whose conversation Claude Code deleted: Sessions shows each one&#39;s claude rm"><strong>1</strong><span>expired background jobs</span></a>`,
	)
	wantIn(t, "chips", card,
		`<li class="tag tone-ok" title="started 2026-11-05T08:00:00Z · version v1.2">aimd running · 4h</li>`,
		`<li class="tag tone-ok" title="last good poll 2026-11-05T11:59:57Z">Sessions polled 3s ago</li>`,
		`<li class="tag tone-ok" title="resets at 12:40">GitHub 4,961 / 5,000 points</li>`,
		`<li class="tag tone-warn" title="ai-manager: 1">2 plans · 1 parser warning</li>`,
		`<li class="tag" title="This aimd doesn&#39;t take the mod&#39;s events.">Plugin reporting: no receiver</li>`,
	)
	// The expired job is counted here, not listed as running; the other
	// waiting job still is.
	agents := section(t, page, "ov-agents")
	wantNotIn(t, "agents", agents, "sharing copy")
	wantIn(t, "agents", agents, "dismissed job")

	// Without the expired-jobs check, the count is unknown.
	noJobs, _ := newApp(t, nil)
	wantIn(t, "no jobs", get(t, noJobs.Handler(), "/app/?fragment=ov-health"),
		`<a class="stat" href="/app/sessions" title="This aimd doesn&#39;t check background jobs for a deleted conversation."><strong>—</strong><span>expired background jobs</span></a>`)

	// The safe-to-remove count while it is unknown.
	for name, sr := range map[string]SafeToRemove{
		"pending": fakeVerdicts{err: ErrVerdictsPending}, "failing": fakeVerdicts{err: errors.New("git broke")}, "not wired": nil,
	} {
		srv, _ := newApp(t, func(o *Options) { o.SafeToRemove = sr })
		card := get(t, srv.Handler(), "/app/?fragment=ov-health")
		wantIn(t, name, card, `<strong>—</strong><span>worktrees safe to remove</span>`)
	}

	// Nothing wired at all.
	bare := get(t, newDash(t, newTestStore(t), nil).Handler(), "/app/?fragment=ov-health")
	wantIn(t, "bare", bare, `<strong>0</strong><span>records whose folder is gone</span>`, `<strong>0</strong><span>stale draft PRs</span>`,
		">aimd running</li>", ">Sessions: no poller</li>", ">GitHub budget unknown</li>", ">Plans unavailable</li>", ">Plugin reporting: no receiver</li>")
}

func TestOverviewHealthChips(t *testing.T) {
	src := func(s api.Source) api.Status { return api.Status{Sources: []api.Source{s}} }
	for _, c := range []struct {
		name string
		st   api.Status
		chip func(api.Status, time.Time) ovChip
		want ovChip
	}{
		{"poll failing", src(api.Source{Name: api.SourceSessions, FailingSince: t0.Add(-10 * time.Minute), Error: "claude agents: exit 1"}), sessionPollChip,
			ovChip{Text: "Session poll failing since 11:50", Tone: "warn", Title: "claude agents: exit 1"}},
		{"poll not yet", src(api.Source{Name: api.SourceSessions}), sessionPollChip,
			ovChip{Text: "Sessions not polled yet", Title: "aimd's first poll of claude agents hasn't finished."}},
		{"poll late", src(api.Source{Name: api.SourceSessions, LastOK: t0.Add(-12 * time.Minute)}), sessionPollChip,
			ovChip{Text: "Sessions last polled 12m ago", Tone: "warn", Title: "last good poll 2026-11-05T11:48:00Z"}},
		{"budget stopped", api.Status{GitHub: api.GitHubStatus{Budget: api.Budget{Known: true, Verdict: "stop", Remaining: 10, Limit: 5000, ResumeAt: t0.Add(30 * time.Minute)}}},
			githubChip, ovChip{Text: "GitHub 10 / 5,000 points · stopped until 12:30", Tone: "warn"}},
		{"budget idle only", api.Status{GitHub: api.GitHubStatus{Budget: api.Budget{Known: true, Verdict: "idle-only", Remaining: 900, Limit: 5000}}},
			githubChip, ovChip{Text: "GitHub 900 / 5,000 points · idle repos only", Tone: "warn"}},
		{"github failing", api.Status{
			Sources: []api.Source{{Name: api.SourceGitHub, FailingSince: t0.Add(-time.Hour), Error: "401"}},
			GitHub:  api.GitHubStatus{Budget: api.Budget{Known: true, Verdict: "normal", Remaining: 5000, Limit: 5000}}},
			githubChip, ovChip{Text: "GitHub 5,000 / 5,000 points · failing since 11:00", Tone: "warn", Title: "401"}},
		{"a repo unreadable", api.Status{GitHub: api.GitHubStatus{Budget: api.Budget{Known: true, Verdict: "normal", Remaining: 4900, Limit: 5000},
			Unreadable: []api.RepoError{{Repo: "acme/app", Error: "github: not found"}}}},
			githubChip, ovChip{Text: "GitHub 4,900 / 5,000 points · 1 repo unreadable", Tone: "warn", Title: "acme/app: github: not found"}},
		{"repos unreadable, and the budget's reset", api.Status{GitHub: api.GitHubStatus{
			Budget:     api.Budget{Known: true, Verdict: "normal", Remaining: 4900, Limit: 5000, ResetAt: t0.Add(20 * time.Minute)},
			Unreadable: []api.RepoError{{Repo: "acme/app", Error: "github: not found"}, {Repo: "example-org/web", Error: "github: auth"}}}},
			githubChip, ovChip{Text: "GitHub 4,900 / 5,000 points · 2 repos unreadable", Tone: "warn",
				Title: "resets at 12:20; acme/app: github: not found; example-org/web: github: auth"}},
		// The latest of the last batch's arrival and the last event's time.
		{"plugin live", api.Status{Events: &api.EventStats{ModLive: true, Events: 1234, LastBatchAt: t0.Add(-4 * time.Second), LastEventAt: t0.Add(-5 * time.Second)}}, pluginChip,
			ovChip{Text: "Plugin reporting · live · last event 4s ago", Tone: "ok", Title: "1234 events since aimd started · last 2026-11-05T11:59:56Z"}},
		{"plugin live by event time", api.Status{Events: &api.EventStats{ModLive: true, Events: 3, LastBatchAt: t0.Add(-time.Minute), LastEventAt: t0.Add(-2 * time.Second)}}, pluginChip,
			ovChip{Text: "Plugin reporting · live · last event 2s ago", Tone: "ok", Title: "3 events since aimd started · last 2026-11-05T11:59:58Z"}},
		{"plugin quiet", api.Status{Events: &api.EventStats{Events: 9, LastBatchAt: t0.Add(-2 * time.Hour)}}, pluginChip,
			ovChip{Text: "Plugin reporting · waiting for events", Title: "Last event 2h ago (2026-11-05T10:00:00Z): no session has done anything for 5 minutes."}},
		{"plugin none yet", api.Status{Events: &api.EventStats{}}, pluginChip,
			ovChip{Text: "Plugin reporting · waiting for events", Title: "No event has reached aimd since it started."}},
		{"no receiver", api.Status{}, pluginChip,
			ovChip{Text: "Plugin reporting: no receiver", Title: "This aimd doesn't take the mod's events."}},
	} {
		if got := c.chip(c.st, t0); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
	for n, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 4961: "4,961", 1234567: "1,234,567", -1234: "-1,234"} {
		if got := thousands(n); got != want {
			t.Errorf("thousands(%d) = %q, want %q", n, got, want)
		}
	}
}

// TestOverviewNoInlineCode reads a full Overview (bars, Go forms and
// disabled buttons, a feed with links) for what CSP forbids.
func TestOverviewNoInlineCode(t *testing.T) {
	srv, st, _ := ovAgentsWorld(t, nil)
	must(t, st.UpsertPRs(context.Background(), store.PR{Repo: "o/app", Number: 40, Title: "Triggers", State: store.PROpen,
		URL: "https://github.com/o/app/pull/40", CreatedAt: t0.Add(-time.Hour)}))
	h := srv.Handler()
	for _, target := range []string{"/app/", "/app/?feed=all", "/app/?fragment=ov-agents", "/app/?fragment=ov-plans"} {
		body := get(t, h, target)
		if m := inlineRE.FindString(body); m != "" {
			t.Errorf("%s: inline style, handler or javascript: URL (%q)", target, m)
		}
		for _, tag := range scriptRE.FindAllString(body, -1) {
			if !regexp.MustCompile(`^<script src="/static/[a-z-]+\.js\?v=[^"]*" defer>$`).MatchString(tag) {
				t.Errorf("%s: inline script %s", target, tag)
			}
		}
	}
	page := get(t, h, "/app/")
	wantIn(t, "rich page", page, `<span class="bar" role="img"`, `<form method="post" action="/app/overview/go"`, `disabled title="`)
}

// TestOverviewEscapes checks that session names, branches and PR titles
// are escaped on the Overview.
func TestOverviewEscapes(t *testing.T) {
	evil := `<script>alert(1)</script><img src=x onerror=alert(2)>`
	st := newTestStore(t)
	ctx := context.Background()
	must(t, st.UpsertSessions(ctx, store.Session{Key: "pid:7", SessionID: "s-7", PID: 7, Kind: "interactive", Name: evil, Branch: evil,
		State: needsYou, StateReason: "permission prompt: " + evil, NeedsYouSince: t0.Add(-time.Minute), StartedAt: t0.Add(-time.Minute), LastSeenAt: t0}))
	must(t, st.UpsertPRs(ctx, store.PR{Repo: "o/app", Number: 1, Title: evil, State: store.PROpen, URL: "https://github.com/o/app/pull/1",
		CreatedAt: t0.Add(-time.Minute)}))
	h := newDash(t, st, func(o *Options) {
		o.Go = &stubGo{}
		o.Terminals = fakeTerms{"pid:7": term.App{Kind: term.KindOther, Name: evil}}
	}).Handler()
	body := get(t, h, "/app/")
	wantNotIn(t, "escapes", body, "<script>alert", "<img src=x")
	wantIn(t, "escapes", body, "&lt;script&gt;alert(1)&lt;/script&gt;")
}
