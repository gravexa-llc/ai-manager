package web

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/monitor"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// fakePlans is a PlanService with fixed plans; the methods the app doesn't
// call are left nil.
type fakePlans struct {
	PlanService
	plans []api.Plan
	sums  map[string]api.PlanSummary
	err   error
}

type planNotFound struct{}

func (planNotFound) Error() string   { return "no such plan" }
func (planNotFound) HTTPStatus() int { return http.StatusNotFound }

func (f fakePlans) Plans(context.Context) (api.PlanList, error) {
	return api.PlanList{Plans: f.plans}, f.err
}

func (f fakePlans) Plan(_ context.Context, slug string) (api.Plan, error) {
	for _, p := range f.plans {
		if strings.EqualFold(p.Slug, slug) {
			return p, nil
		}
	}
	return api.Plan{}, planNotFound{}
}

func (f fakePlans) Summary(_ context.Context, slug string, _ time.Time) (api.PlanSummary, error) {
	if s, ok := f.sums[slug]; ok {
		return s, nil
	}
	return api.PlanSummary{}, planNotFound{}
}

// appPlans are two plans: sharing with 2 packages waiting on you, aim with
// none.
var appPlans = fakePlans{
	plans: []api.Plan{
		{Slug: "sharing", Title: "Pivot share links", Total: 15, Counts: map[string]int{"done": 4}},
		{Slug: "aim", Title: "ai-manager — build plan", Total: 32, Counts: map[string]int{"done": 25}},
	},
	sums: map[string]api.PlanSummary{"sharing": {NeedsYou: 2}, "aim": {}},
}

// newApp returns the app over seedDash's store, with appPlans and two
// worktrees safe to remove, changed by opt if given.
func newApp(t *testing.T, opt func(*Options)) (*Server, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	seedDash(t, st)
	srv := newDash(t, st, func(o *Options) {
		o.Plans = appPlans
		o.SafeToRemove = fakeVerdicts{v: map[string]Verdict{
			appRepo + "-old":  {Category: "safe", Removable: true},
			appRepo + "-gone": {Category: "prunable", Removable: true},
			appRepo + "-wt":   {Category: "locked", Reasons: []string{"locked by an agent"}},
		}}
		if opt != nil {
			opt(o)
		}
	})
	return srv, st
}

// appRoutes are the app's pages: path, menu entry, title and panes.
var appRoutes = []struct {
	path, page, title string
	panes             int
}{
	{"/app/", "overview", "Overview", 1},
	{"/app/inbox", "inbox", "Needs you", 2},
	{"/app/plans/sharing", "plans", "Pivot share links", 2},
	{"/app/sessions", "sessions", "Sessions", 2},
	{"/app/worktrees", "worktrees", "Worktrees &amp; cleanup", 2},
	{"/app/prs", "prs", "Pull requests", 2},
	{"/app/settings", "settings", "Settings", 1},
}

// menuHref is each menu entry's link.
var menuHref = map[string]string{
	"overview": "/app/", "inbox": "/app/inbox", "plans": "/app/plans/sharing", "sessions": "/app/sessions",
	"worktrees": "/app/worktrees", "prs": "/app/prs", "settings": "/app/settings",
}

// currentRE finds the menu entries marked as the current page.
var currentRE = regexp.MustCompile(`<a class="nav[^"]* on" aria-current="page" href="([^"]*)"`)

func TestAppPagesRender(t *testing.T) {
	srv, _ := newApp(t, nil)
	h := srv.Handler()
	for _, rt := range appRoutes {
		body := get(t, h, rt.path)
		wantIn(t, rt.path, body,
			`<title>(4) aim</title>`, "<h1>"+rt.title+"</h1>",
			`href="/static/app-shell.css?v=`+assetVersion+`"`, `src="/static/app-shell.js?v=`+assetVersion+`"`,
			`<meta name="aim-csrf" content="`, `data-page="`+rt.page+`"`,
			`<nav id="menu" class="menu" aria-label="Main menu" data-live="`+menuTopics+`">`,
			`id="summary"`, `Tab title: <span data-tab-title>(4) aim</span>`,
			`<span id="live" class="live" hidden>`, `<dialog id="help"`, `id="toasts"`,
		)
		wantNotIn(t, rt.path, body, `<div class="stub">`, "/static/app.js", "/static/app.css")
		// The header's filter is for a list: two panes only.
		if rt.panes == 2 {
			wantIn(t, rt.path, body, `<section id="list" class="pane list"`, `<aside id="detail" class="pane detail"`,
				`<input id="search" name="q" type="search" value="" placeholder="Filter this list"`)
			wantNotIn(t, rt.path, body, `id="content"`)
		} else {
			wantIn(t, rt.path, body, `<div id="content" class="content"`)
			wantNotIn(t, rt.path, body, `id="list"`, `id="detail"`, `id="search"`)
		}
		cur := currentRE.FindAllStringSubmatch(body, -1)
		if len(cur) != 1 || cur[0][1] != menuHref[rt.page] {
			t.Errorf("%s: current menu entries %q, want only %s", rt.path, cur, menuHref[rt.page])
		}
	}
}

func TestAppMenu(t *testing.T) {
	srv, st := newApp(t, nil)
	h := srv.Handler()
	menu := get(t, h, "/app/sessions?fragment=menu")
	wantIn(t, "menu", menu,
		// Needs you: the old dashboard's count (2 sessions, 2 PRs).
		`href="/app/inbox" data-key="2" aria-keyshortcuts="2" data-needs-count="4"`,
		`Needs you<span class="count"><span class="vh">: </span>4</span>`,
		// One entry per plan, the first on 3, with its needs-you count.
		`<a class="nav sub" href="/app/plans/sharing" data-key="3" aria-keyshortcuts="3">`,
		`<span class="nav-label">Pivot share links</span><span class="mini" title="2 waiting on you">`,
		`<a class="nav sub" href="/app/plans/aim">`, `<span class="nav-label">ai-manager — build plan</span></a>`,
		// Sessions: without a monitor every session is monitored (headless
		// runs aside); PRs: the open ones, drafts included.
		`Sessions<span class="mini" title="5 monitored"><span class="vh">: </span>5</span>`,
		`Pull requests<span class="mini" title="4 open"><span class="vh">: </span>4</span>`,
		// The worktrees safe to remove.
		`Worktrees &amp; cleanup<span class="mini" title="2 safe to remove">`,
		`href="/app/" data-key="1"`, `href="/app/sessions" data-key="4"`, `href="/app/worktrees" data-key="5"`, `href="/app/prs" data-key="6"`,
		`href="/app/settings"`, `data-open="help" data-key="?"`,
	)
	if n := strings.Count(menu, `aria-current="page"`); n != 1 {
		t.Errorf("menu fragment marks %d entries current, want 1", n)
	}

	// The plan's own entry is marked on its page, whatever the slug's case.
	plan := get(t, h, "/app/plans/SHARING?fragment=menu")
	wantIn(t, "plan menu", plan, `<a class="nav sub on" aria-current="page" href="/app/plans/sharing" data-key="3"`)

	// The counts follow the store: a dismissed session leaves the badge.
	must(t, st.DismissNeedsYou(context.Background(), "pid:11", t0))
	wantIn(t, "after dismiss", get(t, h, "/app/sessions?fragment=menu"), `data-needs-count="3"`)
	wantIn(t, "after dismiss", get(t, h, "/app/sessions"), `<title>(3) aim</title>`)

	// Nothing needs you: no badge, and the tab says just aim.
	quiet := newDash(t, newTestStore(t), nil).Handler()
	page := get(t, quiet, "/app/")
	wantIn(t, "quiet", page, `<title>aim</title>`, `data-needs-count="0"`, "Plans aren&#39;t available.")
	wantNotIn(t, "quiet", page, `class="count"`, `title="0 safe to remove"`, `class="mini" title`)

	// No plans, or the plan service failing, say so.
	none, _ := newApp(t, func(o *Options) { o.Plans = fakePlans{} })
	wantIn(t, "no plans", get(t, none.Handler(), "/app/?fragment=menu"), `<p class="navnote">No plans registered.</p>`)
	broken, _ := newApp(t, func(o *Options) { o.Plans = fakePlans{err: errors.New("boom")} })
	wantIn(t, "plans failing", get(t, broken.Handler(), "/app/?fragment=menu"), "aimd couldn&#39;t read the plans.")
}

// TestAppMenuFollowsMonitoring checks the menu against the monitoring
// rules (internal/monitor), in its monitored-sessions world
// (newMonitorEnv): Needs you counts what waits on
// you there (needsYouTotal agrees on the sessions and PRs; the plan's
// your steps, aimSteps, are the inbox's alone), and Sessions only the
// monitored sessions.
func TestAppMenuFollowsMonitoring(t *testing.T) {
	for _, c := range []struct {
		mode               string
		needs, mon, notMon int
	}{
		// opt-in: the watched session; the stranger and nobody's PR left
		// out. Neither monitored PR needs you: #21 (on a package)
		// waits on D1's agent, #31 on the watched session still working on
		// its branch.
		{monitor.ModeOptIn, 1, 3, 3},
		// all: both waiting sessions, and #30, ready on no plan.
		{monitor.ModeAll, 3, 6, 0},
	} {
		e := newMonitorEnv(t, c.mode)
		sn, err := e.srv.load(context.Background())
		must(t, err)
		n := strconv.Itoa(c.needs + aimSteps)
		if got := needsYouTotal(sn); got != c.needs {
			t.Fatalf("%s: needsYouTotal = %d, want %d", c.mode, got, c.needs)
		}
		menu := get(t, e.h, "/app/inbox?fragment=menu")
		wantIn(t, c.mode+" menu", menu,
			`data-needs-count="`+n+`"`, `Needs you<span class="count"><span class="vh">: </span>`+n+`</span>`,
			`Sessions<span class="mini" title="`+strconv.Itoa(c.mon)+` monitored">`)
		wantIn(t, c.mode+" title", get(t, e.h, "/app/sessions"), `<title>(`+n+`) aim</title>`,
			"<p>"+strconv.Itoa(c.mon)+" monitored · "+strconv.Itoa(c.notMon)+" not monitored</p>")

		// The same split as GET /api/sessions gives, headless runs aside.
		mon, not := 0, 0
		for _, js := range sessionsJSON(t, e, "/api/sessions").Sessions {
			switch {
			case js.Headless:
			case js.Monitored:
				mon++
			default:
				not++
			}
		}
		if mon != c.mon || not != c.notMon {
			t.Errorf("%s: /api/sessions has %d monitored, %d not; the app says %d, %d", c.mode, mon, not, c.mon, c.notMon)
		}
	}
}

func TestAppFragments(t *testing.T) {
	srv, _ := newApp(t, nil)
	h := srv.Handler()
	for _, c := range []struct {
		target string
		want   []string
	}{
		{"/app/inbox?fragment=list", []string{`<nav class="tabs" aria-label="Inbox tabs">`}},
		{"/app/inbox?item=999&fragment=detail", []string{"That item is gone."}},
		{"/app/inbox?fragment=detail", []string{"Select an item to see what it's waiting on."}},
		{"/app/inbox?fragment=summary", []string{"<h1>Needs you</h1>", "<p>4 open · grouped by plan</p>"}},
		{"/app/inbox?fragment=menu", []string{`aria-current="page" href="/app/inbox"`, `data-needs-count="4"`}},
		{"/app/?fragment=content", []string{`<section id="ov-plans" class="card"`, "Plan progress", "Cleanup &amp; health"}},
		{"/app/worktrees?fragment=summary", []string{"<p>6 worktrees · 2 safe to remove · 1 record whose folder is gone</p>"}},
		{"/app/plans/sharing?fragment=summary", []string{"<h1>Pivot share links</h1>", "<p>15 packages · 4 done</p>"}},
	} {
		rec := do(t, h, "GET", c.target, nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d: %s", c.target, rec.Code, rec.Body)
		}
		body := rec.Body.String()
		wantIn(t, c.target, body, c.want...)
		// Only the region: no page around it, and no other region.
		wantNotIn(t, c.target, body, "<html", "<head", "<body", `id="menu"`, `id="list"`, `id="detail"`, `id="content"`, `id="summary"`, `<dialog`)
		if tok := rec.Header().Get(appCSRFHeader); !srv.CSRF().Valid(tok) {
			t.Errorf("%s: %s %q isn't a valid token", c.target, appCSRFHeader, tok)
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: Cache-Control %q", c.target, rec.Header().Get("Cache-Control"))
		}
	}
	// A fragment the page doesn't have is a 404; so is the shell or a
	// partial that isn't a region.
	for _, target := range []string{
		"/app/?fragment=list", "/app/?fragment=detail", "/app/inbox?fragment=content", "/app/inbox?fragment=nope",
		"/app/inbox?fragment=shell", "/app/inbox?fragment=help", "/app/inbox?fragment=toast", "/app/inbox?fragment=stub",
		"/app/settings?fragment=list",
	} {
		if rec := do(t, h, "GET", target, nil, nil); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", target, rec.Code)
		}
	}
}

func TestAppPlanPage(t *testing.T) {
	srv, _ := newApp(t, nil)
	h := srv.Handler()
	wantIn(t, "plan", get(t, h, "/app/plans/aim"), "<h1>ai-manager — build plan</h1>", "<p>32 packages · 25 done</p>")
	if rec := do(t, h, "GET", "/app/plans/nope", nil, nil); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), `no plan "nope" is registered`) {
		t.Errorf("unknown plan = %d %s, want 404", rec.Code, rec.Body)
	}
	bare := newDash(t, newTestStore(t), nil).Handler()
	if rec := do(t, bare, "GET", "/app/plans/aim", nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("plan page without a plan service = %d, want 404", rec.Code)
	}

	// With the real plan service (app_plans_test.go has the rest).
	e := newBoardEnv(t)
	e.registerSharing()
	page := get(t, e.h, "/app/plans/sharing")
	wantIn(t, "real plan", page, `<a class="nav sub on" aria-current="page" href="/app/plans/sharing"`, `id="plan-graph"`)
}

func TestAppRequestChecks(t *testing.T) {
	srv, _ := newApp(t, nil)
	h := srv.Handler()
	for _, target := range []string{"/app/", "/app/inbox", "/app/inbox?fragment=list", "/app/plans/sharing?fragment=menu", "/app/settings"} {
		rec := do(t, h, "GET", target, nil, nil)
		hd := rec.Header()
		if hd.Get("Content-Security-Policy") != ContentSecurityPolicy || hd.Get("X-Frame-Options") != "DENY" || hd.Get("Referrer-Policy") != "no-referrer" {
			t.Errorf("GET %s: security headers %v", target, hd)
		}
		for k := range hd {
			if strings.HasPrefix(k, "Access-Control-") {
				t.Errorf("GET %s sent %s", target, k)
			}
		}
		for _, host := range []string{"evil.example:7777", "127.0.0.1:8888", "localhost"} {
			if rec := do(t, h, "GET", target, nil, map[string]string{"Host": host}); rec.Code != http.StatusForbidden {
				t.Errorf("GET %s with Host %s = %d, want 403", target, host, rec.Code)
			}
		}
	}
	if rec := do(t, h, "GET", "/app", nil, nil); rec.Code != http.StatusMovedPermanently || rec.Header().Get("Location") != "/app/" {
		t.Errorf("GET /app = %d to %q, want 301 to /app/", rec.Code, rec.Header().Get("Location"))
	}
	if rec := do(t, h, "GET", "/app/nope", nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("GET /app/nope = %d, want 404", rec.Code)
	}

	// A page's POST routes are forms: CSRF.Protect guards them.
	addToastRoute(srv)
	form := "to=done&csrf_token=" + url.QueryEscape(srv.CSRF().Token())
	for name, hdr := range map[string]map[string]string{
		"no token":     {"Content-Type": "application/x-www-form-urlencoded"},
		"cross-origin": {"Content-Type": "application/x-www-form-urlencoded", "Origin": "http://evil.example", CSRFHeader: srv.CSRF().Token()},
		"cross-site":   {"Content-Type": "application/x-www-form-urlencoded", "Sec-Fetch-Site": "cross-site", CSRFHeader: srv.CSRF().Token()},
	} {
		body := "to=done"
		if name != "no token" {
			body = form
		}
		if rec := do(t, h, "POST", "/app/test/move", strings.NewReader(body), hdr); rec.Code != http.StatusForbidden {
			t.Errorf("POST %s = %d, want 403", name, rec.Code)
		}
	}
}

// addToastRoute adds a page-style form route that answers with the toast
// protocol: to=done moves item 5 (with an Undo), to=open undoes it, and
// to=fail is refused.
func addToastRoute(s *Server) {
	s.mux.HandleFunc("POST /app/test/move", func(w http.ResponseWriter, r *http.Request) {
		switch r.PostFormValue("to") {
		case "fail":
			s.appRefuse(w, r, http.StatusConflict, "/app/inbox?tab=open", "It moved meanwhile.")
		case "open":
			s.appReply(w, r, appReturn(r), &appToast{Text: "Moved back to Open"})
		case "evil":
			s.appReply(w, r, "https://evil.example/x", &appToast{Text: "x", Undo: &appUndo{Action: "https://evil.example/undo"}})
		default:
			s.appReply(w, r, "/app/inbox?item=5", &appToast{Text: "Moved to Done", Undo: &appUndo{
				Action: "/app/test/move", Fields: map[string]string{"item": "5", "to": "open", "return": "/app/inbox?item=5"},
			}})
		}
	})
}

func TestAppToastProtocol(t *testing.T) {
	srv, _ := newApp(t, nil)
	addToastRoute(srv)
	h := srv.Handler()
	formHdr := map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": "null", "Sec-Fetch-Site": "same-origin"}
	jsonHdr := map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Accept": "application/json", CSRFHeader: srv.CSRF().Token()}
	postForm := func(vals url.Values, hdr map[string]string) *httptest.ResponseRecorder {
		vals.Set(CSRFField, srv.CSRF().Token())
		return do(t, h, "POST", "/app/test/move", strings.NewReader(vals.Encode()), hdr)
	}

	// The script gets JSON: where to go and the toast with its Undo.
	rec := postForm(url.Values{"to": {"done"}}, jsonHdr)
	var a appAnswer
	if rec.Code != http.StatusOK {
		t.Fatalf("JSON post = %d: %s", rec.Code, rec.Body)
	}
	decode(t, rec, &a)
	if a.Location != "/app/inbox?item=5" || a.Toast == nil || a.Toast.Text != "Moved to Done" || a.Toast.Undo == nil ||
		a.Toast.Undo.Action != "/app/test/move" || a.Toast.Undo.Fields["to"] != "open" || a.Error != "" {
		t.Errorf("JSON answer %+v (toast %+v)", a, a.Toast)
	}
	rec = postForm(url.Values{"to": {"fail"}}, jsonHdr)
	a = appAnswer{}
	decode(t, rec, &a)
	if rec.Code != http.StatusConflict || a.Error != "It moved meanwhile." || a.Toast != nil {
		t.Errorf("JSON refusal = %d %+v", rec.Code, a)
	}
	// Never a location or an Undo outside the app.
	rec = postForm(url.Values{"to": {"evil"}}, jsonHdr)
	a = appAnswer{}
	decode(t, rec, &a)
	if a.Location != "/app/" || a.Toast == nil || a.Toast.Undo != nil {
		t.Errorf("evil answer %+v (toast %+v)", a, a.Toast)
	}

	// A plain form gets a 303 with the toast sealed in ?toast=; the page
	// shows it, with its Undo as a form and a close link.
	rec = postForm(url.Values{"to": {"done"}}, formHdr)
	loc := rec.Header().Get("Location")
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "/app/inbox?item=5&toast=") {
		t.Fatalf("form post = %d to %q", rec.Code, loc)
	}
	page := get(t, h, loc)
	toasts := section(t, page, "toasts")
	wantIn(t, "toast", toasts,
		`<div class="toast" role="status" data-toast>`, "<span>Moved to Done</span>",
		`<form method="post" action="/app/test/move" data-pane>`, `<input type="hidden" name="csrf_token" value="`,
		`<input type="hidden" name="item" value="5">`, `<input type="hidden" name="to" value="open">`,
		`<button type="submit" data-key="u">Undo</button>`, `<a class="toast-x" href="/app/inbox?item=5" aria-label="Close" data-close-toast>`,
	)
	// Fragments never carry it, and the page's return target drops it.
	wantNotIn(t, "list fragment", get(t, h, loc+"&fragment=list"), "Moved to Done")

	// The Undo form posts back; its answer says so, without an Undo.
	rec = postForm(url.Values{"to": {"open"}, "item": {"5"}, "return": {"/app/inbox?item=5"}}, formHdr)
	wantIn(t, "undone", section(t, get(t, h, rec.Header().Get("Location")), "toasts"), "<span>Moved back to Open</span>")
	wantNotIn(t, "undone", section(t, get(t, h, rec.Header().Get("Location")), "toasts"), "<form")

	// A refusal comes back as an error toast.
	rec = postForm(url.Values{"to": {"fail"}}, formHdr)
	wantIn(t, "refused", section(t, get(t, h, rec.Header().Get("Location")), "toasts"),
		`<div class="toast error" role="alert" data-toast>`, "<span>It moved meanwhile.</span>")

	// Forged, tampered or old toasts aren't shown.
	sealed := srv.sealToast(appToast{Text: "Genuine"})
	b64, sig, _ := strings.Cut(sealed, ".")
	forged := srv.sealToast(appToast{Text: "Forged"})
	fb64, _, _ := strings.Cut(forged, ".")
	for name, v := range map[string]string{
		"no signature": b64, "garbage": "abc.def", "swapped body": fb64 + "." + sig, "empty": "",
	} {
		wantNotIn(t, name, section(t, get(t, h, "/app/inbox?toast="+url.QueryEscape(v)), "toasts"), "data-toast")
	}
	wantIn(t, "genuine", get(t, h, "/app/inbox?toast="+url.QueryEscape(sealed)), "<span>Genuine</span>")
}

func TestAppToastExpiry(t *testing.T) {
	now := t0
	s := newDash(t, newTestStore(t), func(o *Options) { o.Now = func() time.Time { return now } })
	sealed := s.sealToast(appToast{Text: "hi"})
	if s.openToast(sealed) == nil {
		t.Fatal("a fresh toast doesn't open")
	}
	now = t0.Add(2 * time.Hour)
	if s.openToast(sealed) != nil {
		t.Error("a 2 hour old toast still opens")
	}
	// Long text is cut; an Undo with too many fields is dropped.
	fields := map[string]string{}
	for i := range maxToastFields + 1 {
		fields[string(rune('a'+i))] = "x"
	}
	bt := s.boundToast(appToast{Text: strings.Repeat("x", maxToastText+50), Undo: &appUndo{Action: "/app/x", Fields: fields}})
	if len(bt.Text) > maxToastText+len("…") || bt.Undo != nil {
		t.Errorf("bounded toast: %d bytes, undo %+v", len(bt.Text), bt.Undo)
	}
}

func TestAppLocation(t *testing.T) {
	for in, want := range map[string]string{
		"/app/":                        "/app/",
		"/app":                         "/app",
		"/app/inbox?item=5&tab=open":   "/app/inbox?item=5&tab=open",
		"/app/inbox?item=5#x":          "/app/inbox?item=5#x",
		"/app/inbox?fragment=list&a=1": "/app/inbox?a=1",
		"/app/inbox?toast=abc":         "/app/inbox",
		"/app/plans/sharing/":          "/app/plans/sharing/",
		"":                             "/app/",
		"/":                            "/app/",
		"/plans/sharing":               "/app/",
		"/app/../plans":                "/app/",
		"/application":                 "/app/",
		"//evil.example/app/":          "/app/",
		"https://evil.example/app/":    "/app/",
		"javascript:alert(1)":          "/app/",
		`/app\..\x`:                    "/app/",
		"http:/app/":                   "/app/",
	} {
		if got := appLocation(in); got != want {
			t.Errorf("appLocation(%q) = %q, want %q", in, got, want)
		}
	}
}

// inlineRE finds what default-src 'self' forbids: an inline style or
// script, an event handler attribute, or a javascript: URL.
var inlineRE = regexp.MustCompile(`(?i)<style|\sstyle\s*=|\son[a-z]+\s*=|javascript:`)

// scriptRE finds script tags; only <script src="/static/…"> is allowed.
var scriptRE = regexp.MustCompile(`(?i)<script\b[^>]*>`)

// pageScriptRE is a script tag a page may have: the shell's app-shell.js,
// or its own static/app-<page>.js (APP.md).
var pageScriptRE = regexp.MustCompile(`^<script src="/static/app-[a-z]+\.js\?v=[0-9a-f]+" defer>$`)

// TestAppTemplatesNeedNoInlineCode reads every app template, and every
// page as rendered, for inline styles, inline scripts and on* handlers:
// CSP (default-src 'self') would block them.
func TestAppTemplatesNeedNoInlineCode(t *testing.T) {
	files, err := fs.Glob(appTemplates, "templates/app/*.html")
	must(t, err)
	if len(files) < 8 {
		t.Fatalf("found %d app templates: %v", len(files), files)
	}
	check := func(name, body string) {
		t.Helper()
		if m := inlineRE.FindString(body); m != "" {
			t.Errorf("%s: inline style, handler or javascript: URL (%q)", name, m)
		}
		for _, tag := range scriptRE.FindAllString(body, -1) {
			if !regexp.MustCompile(`^<script src="/static/[a-z-]+\.js\?v=[^"]*" defer>$`).MatchString(tag) {
				t.Errorf("%s: inline script %s", name, tag)
			}
		}
	}
	for _, f := range files {
		b, err := fs.ReadFile(appTemplates, f)
		must(t, err)
		check(f, string(b))
	}
	srv, _ := newApp(t, nil)
	addToastRoute(srv)
	h := srv.Handler()
	rec := do(t, h, "POST", "/app/test/move", strings.NewReader("to=done&csrf_token="+url.QueryEscape(srv.CSRF().Token())),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": "null", "Sec-Fetch-Site": "same-origin"})
	pages := []string{rec.Header().Get("Location")}
	for _, rt := range appRoutes {
		pages = append(pages, rt.path, rt.path+"?fragment=menu")
	}
	for _, p := range pages {
		check(p, get(t, h, p))
	}
	// The rule catches what it should.
	for _, bad := range []string{`<p style="x">`, `<a onclick="x">`, `<b ONMOUSEOVER = "x">`, `<style>`, `href="javascript:x"`} {
		if !inlineRE.MatchString(bad) {
			t.Errorf("inlineRE misses %s", bad)
		}
	}
}

// TestAppEscapes checks that untrusted names (plan titles) are escaped in
// the menu and the header.
func TestAppEscapes(t *testing.T) {
	evil := `<script>alert(1)</script><img src=x onerror=alert(2)>`
	srv, _ := newApp(t, func(o *Options) {
		o.Plans = fakePlans{
			plans: []api.Plan{{Slug: `x"><script>`, Title: evil, Total: 1, Counts: map[string]int{}}},
			sums:  map[string]api.PlanSummary{},
		}
	})
	h := srv.Handler()
	for _, p := range []string{"/app/", "/app/plans/" + url.PathEscape(`x"><script>`), "/app/inbox?q=" + url.QueryEscape(evil)} {
		body := get(t, h, p)
		wantNotIn(t, p, body, "<script>alert", "<img src=x", `"><script>`)
		// Only the shell's script and a page's own (APP.md: app-<page>.js).
		for _, tag := range scriptRE.FindAllString(body, -1) {
			if !pageScriptRE.MatchString(tag) {
				t.Errorf("%s has a script tag that isn't the shell's or the page's: %s", p, tag)
			}
		}
	}
}

func TestAppStoreErrorIs500(t *testing.T) {
	srv, st := newApp(t, nil)
	h := srv.Handler()
	st.Close()
	for _, p := range []string{"/app/", "/app/inbox", "/app/inbox?fragment=menu"} {
		rec := do(t, h, "GET", p, nil, nil)
		if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "sql") {
			t.Errorf("GET %s on a closed store = %d %s, want a 500 without details", p, rec.Code, rec.Body)
		}
	}
}

func TestAppStaticAssets(t *testing.T) {
	h := newDash(t, newTestStore(t), nil).Handler()
	for file, ctype := range map[string]string{
		"app-shell.js":  "text/javascript; charset=utf-8",
		"app-shell.css": "text/css; charset=utf-8",
		"icon.svg":      "image/svg+xml",
	} {
		rec := do(t, h, "GET", "/static/"+file, nil, nil)
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != ctype || rec.Body.Len() == 0 {
			t.Errorf("GET /static/%s = %d %q (%d bytes)", file, rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
			continue
		}
		etag := rec.Header().Get("ETag")
		if rec := do(t, h, "GET", "/static/"+file, nil, map[string]string{"If-None-Match": etag}); rec.Code != http.StatusNotModified {
			t.Errorf("GET /static/%s with its ETag = %d, want 304", file, rec.Code)
		}
	}
	for _, p := range []string{"/static/nope.js", "/static/", "/static/../templates/app/shell.html"} {
		if rec := do(t, h, "GET", p, nil, nil); rec.Code == http.StatusOK {
			t.Errorf("GET %s = 200", p)
		}
	}
	js := string(staticFiles["app-shell.js"].data)
	// Small enough to review by reading it.
	if n := len(js); n > 32<<10 {
		t.Errorf("app-shell.js is %d bytes; keep it small", n)
	}
	// It talks to aimd only, and builds no HTML from strings of its own.
	wantIn(t, "app-shell.js", js, `new EventSource("/api/stream")`, `"fragment"`, `"X-AIM-CSRF"`, `"X-CSRF-Token"`,
		`Accept: "application/json"`, "pushState", `"data-pane"`, `"data-key"`, "data-needs-count")
	wantNotIn(t, "app-shell.js", js, "http://", "https://", "eval(", "new Function", "document.write", "insertAdjacentHTML", "outerHTML")
	if n := strings.Count(js, "innerHTML"); n != 1 {
		t.Errorf("app-shell.js sets innerHTML %d times; only swap may (with aimd's escaped fragments)", n)
	}
	css := string(staticFiles["app-shell.css"].data)
	wantIn(t, "app-shell.css", css, "#1B1F24", "#5B6470", "#E3E6EA", "#F6F7F9", "#16191D", "#C9CED6", "#2C323A", "#2557D6", "#C2410C",
		"-apple-system, BlinkMacSystemFont", "ui-monospace", ":focus-visible", "@media (max-width: 900px)",
		".lc-blocked", ".lc-ready", ".lc-working", ".lc-in_review", ".lc-done",
		".kind-permission", ".kind-question", ".kind-plan", ".kind-merge", ".kind-review", ".kind-reply")
	wantNotIn(t, "app-shell.css", css, "@import", "url(http", "fonts.googleapis")
}

// TestAppScriptParses checks every script's syntax with node, when it is
// installed.
func TestAppScriptParses(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node isn't installed")
	}
	for name, f := range staticFiles {
		if !strings.HasSuffix(name, ".js") {
			continue
		}
		path := filepath.Join(t.TempDir(), name)
		must(t, os.WriteFile(path, f.data, 0o644))
		if out, err := exec.Command(node, "--check", path).CombinedOutput(); err != nil {
			t.Errorf("node --check %s: %v\n%s", name, err, out)
		}
	}
}

// jsFunc is the source of the shell script's function name: from its
// declaration to the closing brace at its indent.
func jsFunc(t *testing.T, js, name string) string {
	t.Helper()
	i := strings.Index(js, "\n  function "+name+"(")
	if i < 0 {
		t.Fatalf("app-shell.js has no function %s", name)
	}
	end := strings.Index(js[i+1:], "\n  }\n")
	if end < 0 {
		t.Fatalf("function %s isn't closed", name)
	}
	return js[i+1 : i+1+end+4]
}

// The shell's script. It runs in a browser, so this checks what aimd
// sends the script and the script's own code paths.
func TestAppShellFollowUps(t *testing.T) {
	js := string(staticFiles["app-shell.js"].data)
	srv, _ := newApp(t, nil)
	h := srv.Handler()

	// SVG links take data-pane: their href is read as an attribute (an SVG
	// <a>'s href is an SVGAnimatedString), and a click or Enter on one
	// loads the panes; SVG elements have no click().
	wantIn(t, "hrefOf", jsFunc(t, js, "hrefOf"), `el.getAttribute("href")`)
	wantNotIn(t, "app-shell.js", js, "new URL(el.href)", "navigate(el.href")
	wantIn(t, "press", jsFunc(t, js, "press"), `typeof el.click === "function"`, `new MouseEvent("click"`)
	wantIn(t, "app-shell.js", js, `t instanceof SVGElement && t.closest("a[data-pane]")`, "samePage(new URL(hrefOf(el)))")
	wantNotIn(t, "app-plan.js", string(staticFiles["app-plan.js"].data), "data-plan-pane", "AIM.navigate")

	// An open dialog is busy: a live refresh waits for it, as for typing
	// and an open menu, so the Worktrees page no longer reopens its dialog.
	wantIn(t, "busy", jsFunc(t, js, "busy"), `el.querySelector("dialog[open]")`, "typing(a)", "details[open]")
	wantNotIn(t, "app-worktrees.js", string(staticFiles["app-worktrees.js"].data), "confirming", "stays open across")

	// A swap keeps the scroll of the boxes inside the region, by id (the
	// plan graph's sideways scroll), and a picked item's pane keeps its own.
	swap := jsFunc(t, js, "swap")
	wantIn(t, "swap", swap, "x.scrollLeft || x.scrollTop", "x.scrollLeft = scrolled[id][0]", "x.scrollTop = scrolled[id][1]")
	wantIn(t, "navigate", jsFunc(t, js, "navigate"), "p[0] === stay")
	wantIn(t, "app-shell.js", js, `el.hasAttribute("data-item") && el.closest("#list, #detail, #content")`)

	// The shell has no start position of its own any more: app-plan.js
	// places a plan's views when they first show (TestPlanGraphOpensOnWork).
	wantNotIn(t, "app-shell.js", js, "data-scroll-start", "scrollStart", "x.scrollWidth - x.clientWidth")

	// The settings flags ride on the menu, which refreshes live: the script
	// copies them onto <html> after every swap.
	wantIn(t, "menu", get(t, h, "/app/prs?fragment=menu"), `<span hidden data-flags data-tab-count="on" data-chime="on" data-keys="on" data-theme="system"></span>`)
	flags := jsFunc(t, js, "flags")
	wantIn(t, "flags", flags, `"#menu [data-flags]"`, `"data-tab-count"`, `"data-chime"`, `"data-keys"`, ".tab-preview", "[data-keys-hint]")
	wantIn(t, "after", jsFunc(t, js, "after"), "flags();", "filter();", "tabTitle();")
	if _, ok := staticFiles["app-settings.js"]; ok {
		t.Error("app-settings.js is left: the menu carries the flags")
	}

	// The header's filter: the list's rows by their text and data-search,
	// a group's header with its last row, a graph's boxes dimmed; Esc
	// clears it, and with it the URL's ?q=.
	filter := jsFunc(t, js, "filter")
	wantIn(t, "filter", filter, `$("list")`, `$("search")`, `"data-search"`, `classList.add("filtered")`, `"ghead"`, `searchParams.get("q")`, "textContent")
	wantNotIn(t, "filter", filter, "innerHTML")
	wantIn(t, "clearFilter", jsFunc(t, js, "clearFilter"), `u.searchParams.delete("q")`)
	wantIn(t, "escape", jsFunc(t, js, "escape"), `t.id === "search" && t.value`, "clearFilter()")
	wantIn(t, "items", jsFunc(t, js, "items"), `!x.closest(".filtered")`)
	css := string(staticFiles["app-shell.css"].data)
	wantIn(t, "app-shell.css", css, ".filtered { display: none !important; }", "svg .filtered { display: inline !important; opacity: .25; }")
	for p, row := range map[string]string{
		"/app/sessions?fragment=list": `data-item="pid:10" data-search="main">`,
		"/app/prs?fragment=list":      `data-item="o/web#4" data-search="p">`,
		"/app/inbox?fragment=list":    `data-search="feat">`,
	} {
		wantIn(t, p, get(t, h, p), row)
	}

	// The old pages' anchors reach their app pages.
	wantIn(t, "legacyAnchor", jsFunc(t, js, "legacyAnchor"), `"prs-needing-you": "/app/prs"`, `"home-sessions": "/app/sessions"`,
		`"home-cleanup": "/app/worktrees"`, `searchParams.set("pkg", h.slice(4))`, "location.replace(to)")
}

// The ? sheet lists the shell's keys and the page's own, in the page's
// words: g is Go on the inbox and Open on GitHub on the PRs page; a
// page with no list has no j / k or filter; u is the toast's Undo where
// the page doesn't bind it.
func TestAppHelpSheet(t *testing.T) {
	srv, _ := newApp(t, nil)
	h := srv.Handler()
	help := func(p string) string { return section(t, get(t, h, p), "help") }
	key := func(k, label string) string { return "<dt><kbd>" + k + "</kbd></dt><dd>" + label + "</dd>" }

	inbox := help("/app/inbox")
	for _, k := range []string{"j", "k", "Enter", "g", "s", "d", "u", "/", "1", "6", "?", "Esc"} {
		wantIn(t, "inbox", inbox, "<kbd>"+k+"</kbd>")
	}
	wantIn(t, "inbox", inbox, key("g", "Go: switch to its terminal tab, open the PR on GitHub, or open your step in its plan"), key("s", "Snooze"), key("d", "Done"),
		key("u", "Undo, or move a Done item back to Open"), "<dd>Filter this list; <kbd>Esc</kbd> clears it</dd>")
	if n := strings.Count(inbox, "<kbd>u</kbd>"); n != 1 {
		t.Errorf("inbox lists u %d times", n)
	}

	prs := help("/app/prs")
	wantIn(t, "prs", prs, key("g", "Open the PR on GitHub"), key("u", "Undo the last change, while its toast shows"))
	wantNotIn(t, "prs", prs, "Go: switch", "<kbd>s</kbd>", "<kbd>d</kbd>")
	wantIn(t, "sessions", help("/app/sessions"), key("g", "Go: switch to its terminal tab"))
	wantIn(t, "plan", help("/app/plans/sharing"), key("g", "Go to the package&#39;s agent, or else the plan&#39;s lead"), key("d", "Mark done"))

	for _, p := range []string{"/app/", "/app/settings"} {
		one := help(p)
		wantIn(t, p, one, key("u", "Undo the last change, while its toast shows"), "<kbd>1</kbd> – <kbd>6</kbd>", "<kbd>?</kbd>", "<kbd>Esc</kbd>")
		wantNotIn(t, p, one, "<kbd>j</kbd>", "<kbd>Enter</kbd>", "<kbd>g</kbd>", "<kbd>/</kbd>")
	}
	// The worktrees page binds no key of its own.
	wt := help("/app/worktrees")
	wantIn(t, "worktrees", wt, "<kbd>j</kbd>", key("u", "Undo the last change, while its toast shows"))
	wantNotIn(t, "worktrees", wt, "<kbd>g</kbd>", "<kbd>s</kbd>", "<kbd>d</kbd>")
}

func TestTabTitle(t *testing.T) {
	for _, c := range []struct {
		n    int
		on   bool
		want string
	}{{0, true, "aim"}, {3, true, "(3) aim"}, {3, false, "aim"}, {-1, true, "aim"}} {
		if got := tabTitle(c.n, c.on); got != c.want {
			t.Errorf("tabTitle(%d, %v) = %q, want %q", c.n, c.on, got, c.want)
		}
	}
}

func TestAppDecodeAnswerShape(t *testing.T) {
	b, err := json.Marshal(appAnswer{Location: "/app/", Toast: &appToast{Text: "t", Undo: &appUndo{Action: "/app/x", Fields: map[string]string{"a": "1"}}}})
	must(t, err)
	want := `{"location":"/app/","toast":{"text":"t","undo":{"action":"/app/x","fields":{"a":"1"}}}}`
	if string(b) != want {
		t.Errorf("answer JSON = %s, want %s", b, want)
	}
}
