package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gravexa-llc/ai-manager/internal/focus"
	"github.com/gravexa-llc/ai-manager/internal/monitor"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// planEnv is boardEnv with both fixture plans registered, served the way
// aimd serves the app: with a monitor source (so internal/prwait knows each
// plan's Merges line), a stub Go and the terminals' apps. In it D1's lead
// runs in iTerm2 (Go works), C6's session in tmux (Go is off), and E2 is
// worked on by a background job.
type planEnv struct {
	*boardEnv
	g     *stubGo
	terms fakeTerms
	noGo  bool
}

func newPlanEnv(t *testing.T) *planEnv {
	t.Helper()
	e := &planEnv{boardEnv: newBoardEnv(t), g: &stubGo{res: api.GoResult{Key: "pid:43", Action: api.GoFocused,
		Message: "Focused aim lead in iTerm2 (went to the lead of agent-d1)."}}, terms: fakeTerms{"pid:43": appITerm2, "pid:42": appTmux}}
	e.registerSharing()
	e.registerAim()
	must(t, e.st.UpsertSessions(context.Background(), store.Session{Key: "job:0a1b2c3d", JobID: "0a1b2c3d", SessionID: "bg-1", PID: 53221,
		Kind: "background", Name: "e2 job", Cwd: "/w/e2", State: "working", StartedAt: t0.Add(-time.Hour), LastSeenAt: t0}))
	e.api("POST", "/api/links/manual", api.LinkRequest{Plan: "aim", Pkg: "E2", Kind: "agent", Ref: "job:0a1b2c3d"}, http.StatusCreated)
	e.serve()
	return e
}

// serve (re)builds the server over the env's store and plan service.
func (e *planEnv) serve() {
	mon := &monitor.Source{Mode: monitor.ModeAll, Store: e.st, Plans: func(ctx context.Context) ([]api.Plan, error) {
		list, err := e.svc.Plans(ctx)
		return list.Plans, err
	}}
	o := Options{Port: testPort, Token: testToken, Store: e.st, Plans: e.svc, Terminals: e.terms, Monitor: mon,
		HomeDir: e.tmp, Now: func() time.Time { return t0 }}
	if !e.noGo {
		o.Go = e.g
	}
	e.srv = New(o)
	e.h = e.srv.Handler()
}

// postJSON posts a form the way app-shell.js does, and decodes its answer.
func (e *planEnv) postJSON(target string, vals url.Values) (*httptest.ResponseRecorder, appAnswer) {
	e.t.Helper()
	vals.Set(CSRFField, e.srv.CSRF().Token())
	rec := do(e.t, e.h, "POST", target, strings.NewReader(vals.Encode()), map[string]string{
		"Content-Type": "application/x-www-form-urlencoded", "Accept": "application/json", CSRFHeader: e.srv.CSRF().Token()})
	var a appAnswer
	decode(e.t, rec, &a)
	return rec, a
}

// board is the plan service's board.
func (e *planEnv) board(slug string) api.Plan {
	e.t.Helper()
	b, err := e.svc.Plan(context.Background(), slug)
	must(e.t, err)
	return b
}

// lifecycle is a package's lifecycle on the board now.
func (e *planEnv) lifecycle(slug, id string) string {
	e.t.Helper()
	for _, p := range e.board(slug).Packages {
		if strings.EqualFold(p.ID, id) {
			return p.Lifecycle
		}
	}
	e.t.Fatalf("plan %s has no package %s", slug, id)
	return ""
}

// itemOf returns the list item (a node, a card or a row) for package id:
// the <a> with data-item="id".
func itemOf(t *testing.T, html, id string) string {
	t.Helper()
	i := strings.Index(html, `data-item="`+id+`"`)
	if i < 0 {
		t.Fatalf("no item for %s", id)
	}
	start := strings.LastIndex(html[:i], "<a ")
	end := strings.Index(html[i:], "</a>")
	return html[start : i+end+len("</a>")]
}

// inner is a region's content: the element without its own tags.
func inner(t *testing.T, page, id string) string {
	t.Helper()
	el := element(t, page, id)
	el = el[strings.Index(el, ">")+1:]
	return strings.TrimSpace(el[:strings.LastIndex(el, "</")])
}

var csrfValueRE = regexp.MustCompile(`name="csrf_token" value="[^"]*"`)

// noTokens blanks the CSRF tokens, which differ between two renders.
func noTokens(s string) string { return csrfValueRE.ReplaceAllString(s, `name="csrf_token" value=""`) }

// checkNoInline fails on what the CSP forbids, as
// TestAppTemplatesNeedNoInlineCode checks the shell's pages.
func checkNoInline(t *testing.T, where, body string) {
	t.Helper()
	if m := inlineRE.FindString(body); m != "" {
		t.Errorf("%s: inline style, handler or javascript: URL (%q)", where, m)
	}
	for _, tag := range scriptRE.FindAllString(body, -1) {
		if !pageScriptRE.MatchString(tag) {
			t.Errorf("%s: script tag %s", where, tag)
		}
	}
}

var (
	kanbanColRE = regexp.MustCompile(`<section class="kb-col lc-([a-z_]+)"`)
	dataItemRE  = regexp.MustCompile(`data-item="([^"]+)"`)
)

// kanbanColumns maps each package on a Kanban to its column.
func kanbanColumns(list string) map[string]string {
	out := map[string]string{}
	cols := kanbanColRE.FindAllStringSubmatchIndex(list, -1)
	for i, c := range cols {
		end := len(list)
		if i+1 < len(cols) {
			end = cols[i+1][0]
		}
		for _, m := range dataItemRE.FindAllStringSubmatch(list[c[0]:end], -1) {
			out[m[1]] = list[c[2]:c[3]]
		}
	}
	return out
}

// Each view renders for both fixture plans: every package once, as a box,
// a card or a row, linking to itself in that view.
func TestAppPlanViews(t *testing.T) {
	e := newPlanEnv(t)
	for _, slug := range []string{"sharing", "aim"} {
		b := e.board(slug)
		for _, view := range []string{"graph", "kanban", "timeline"} {
			target := "/app/plans/" + slug + "?view=" + view
			page := get(t, e.h, target)
			checkNoInline(t, target, page)
			wantIn(t, target, page, `href="/static/app-plan.css?v=`+assetVersion+`"`, `src="/static/app-plan.js?v=`+assetVersion+`"`,
				`<a class="tab on" href="/app/plans/`+slug+`?view=`+view+`" data-pane="list detail" aria-current="page">`,
				`data-live="plans prs sessions inbox"`, `id="plan-`+view+`"`)
			list := inner(t, page, "list")
			if n := strings.Count(list, "data-item="); n != len(b.Packages) {
				t.Errorf("%s: %d items for %d packages", target, n, len(b.Packages))
			}
			for _, p := range b.Packages {
				it := itemOf(t, list, p.ID)
				wantIn(t, target+" "+p.ID, it, `href="/app/plans/`+slug+`?view=`+view+`&amp;pkg=`+p.ID+`"`, "lc-"+p.Lifecycle)
			}
			// Nothing selected: the plan's pane.
			wantIn(t, target, inner(t, page, "detail"), `id="plan-warnings"`, `id="plan-unmatched"`, "Ready to dispatch")
		}

		// The graph: one box per package, an arrow per dependency, in
		// columns by depth.
		g := planGraphOf(b, planQuery{slug: slug, view: planGraph}, nil)
		layout := layoutGraph(b.Packages)
		if len(g.Nodes) != len(b.Packages) || len(g.Edges) != len(layout.Edges) {
			t.Errorf("%s graph: %d boxes, %d arrows; want %d, %d", slug, len(g.Nodes), len(g.Edges), len(b.Packages), len(layout.Edges))
		}
		list := inner(t, get(t, e.h, "/app/plans/"+slug), "list")
		if n := strings.Count(list, `<path class="pe`); n != len(layout.Edges) {
			t.Errorf("%s: %d arrows drawn, want %d", slug, n, len(layout.Edges))
		}
		for _, nd := range layout.Nodes {
			x := strconv.Itoa(planGraphMetrics.pad + nd.Layer*(planGraphMetrics.nodeW+planGraphMetrics.gapX))
			wantIn(t, slug+" column of "+nd.ID, itemOf(t, list, nd.ID), `<rect class="pg-box" x="`+x+`"`)
		}

		// Kanban: each card in its lifecycle's column.
		cols := kanbanColumns(inner(t, get(t, e.h, "/app/plans/"+slug+"?view=kanban"), "list"))
		for _, p := range b.Packages {
			if cols[p.ID] != p.Lifecycle {
				t.Errorf("%s kanban: %s in column %q, want %q", slug, p.ID, cols[p.ID], p.Lifecycle)
			}
		}
	}

	// Lanes on the cards, as the board has them.
	kb := inner(t, get(t, e.h, "/app/plans/sharing?view=kanban"), "list")
	wantIn(t, "T1 card", itemOf(t, kb, "T1"), `<span class="tag">Access</span>`)
	wantIn(t, "V1 card", itemOf(t, kb, "V1"), `<span class="tag">Email</span>`)
	wantNotIn(t, "G1 card (no lane)", itemOf(t, kb, "G1"), `class="tag"`)
	wantIn(t, "kanban", kb, "<span><span class=\"sw\"></span> Blocked</span>", "<span><span class=\"sw\"></span> Done</span>")

	// The timeline: bars for what started, none for what hasn't.
	tl := inner(t, get(t, e.h, "/app/plans/aim?view=timeline"), "list")
	wantIn(t, "A1 (merged)", itemOf(t, tl, "A1"), `class="tl-bar done"`)
	wantIn(t, "D1 (in review)", itemOf(t, tl, "D1"), `class="tl-bar review"`)
	wantIn(t, "C6 (working)", itemOf(t, tl, "C6"), `class="tl-bar work"`, "Working since")
	wantNotIn(t, "C7 (blocked)", itemOf(t, tl, "C7"), "tl-bar")
	wantIn(t, "C7 (blocked)", itemOf(t, tl, "C7"), "Not started: blocked")
	wantIn(t, "axis", tl, `<svg class="tl-axis"`, `text-anchor="middle"`)

	// A plan without packages says so in every view.
	empty, _ := newApp(t, nil)
	for _, view := range []string{"graph", "kanban", "timeline"} {
		wantIn(t, "empty "+view, get(t, empty.Handler(), "/app/plans/sharing?view="+view), "This plan has no packages.")
	}
}

// Every selection is in the URL: the view, the package and the search.
func TestAppPlanSelection(t *testing.T) {
	e := newPlanEnv(t)

	// ?pkg= selects a box (any case), highlights its arrows and fills the
	// detail pane; the tabs keep it.
	page := get(t, e.h, "/app/plans/sharing?view=graph&pkg=t1")
	list := inner(t, page, "list")
	t1 := itemOf(t, list, "T1")
	// The box is an SVG link the shell loads into the panes.
	wantIn(t, "T1 box", t1, `class="pg-node lc-blocked sel"`, `aria-current="true"`, `data-pane="list detail" data-item="T1"`)
	wantNotIn(t, "T1 box", t1, "data-plan-pane")
	if n := strings.Count(list, `aria-current="true"`); n != 1 {
		t.Errorf("%d boxes selected, want 1", n)
	}
	// T1 needs A1 and G1 and unblocks Q1 and Q2: four arrows on.
	if n := strings.Count(list, `class="pe on"`) + strings.Count(list, `class="pe met on"`); n != 4 {
		t.Errorf("%d highlighted arrows, want 4", n)
	}
	wantIn(t, "arrows", list, `marker-end="url(#pg-h)"`, `class="pe off"`)
	if strings.LastIndex(list, `class="pe off"`) > strings.Index(list, `class="pe on"`) {
		t.Error("the selected package's arrows aren't drawn last, on top")
	}
	wantIn(t, "tabs", list, `href="/app/plans/sharing?view=kanban&amp;pkg=T1"`, `href="/app/plans/sharing?view=timeline&amp;pkg=T1"`)
	wantIn(t, "detail", inner(t, page, "detail"), "<h2>Tile filters</h2>", `<span class="nid pd-id">T1</span>`)

	// The same selection in the other views.
	wantIn(t, "kanban", itemOf(t, get(t, e.h, "/app/plans/sharing?view=kanban&pkg=T1"), "T1"), `class="kcard lc-blocked sel"`, `aria-current="true"`)
	wantIn(t, "timeline", itemOf(t, get(t, e.h, "/app/plans/sharing?view=timeline&pkg=T1"), "T1"), `class="tl-row lc-blocked sel"`, `aria-current="true"`)

	// No ?view= or an unknown one is the graph; an unknown package selects
	// nothing and says so.
	for _, target := range []string{"/app/plans/sharing", "/app/plans/sharing?view=nope"} {
		wantIn(t, target, get(t, e.h, target), `id="plan-graph"`, `<a class="tab on" href="/app/plans/sharing?view=graph"`)
	}
	missing := get(t, e.h, "/app/plans/sharing?pkg=Z9")
	wantIn(t, "unknown pkg", inner(t, missing, "detail"), "This plan has no package &#34;Z9&#34;.", `id="plan-unmatched"`)
	wantNotIn(t, "unknown pkg", inner(t, missing, "list"), `aria-current="true"`)

	// The detail's close link and the toolbar's notes select nothing.
	wantIn(t, "close", inner(t, page, "detail"), `<a class="btn sm pd-close" href="/app/plans/sharing?view=graph" data-pane="list detail"`)
	wantIn(t, "notes", list, `<a href="/app/plans/sharing?view=graph#plan-unmatched" data-pane="list detail">1 unmatched</a>`)

	// ?q= dims what it doesn't match, and the links keep it.
	q := inner(t, get(t, e.h, "/app/plans/sharing?view=kanban&q=re"), "list")
	wantIn(t, "search", q, "Showing packages matching “re”", `href="/app/plans/sharing?view=kanban&amp;pkg=V1&amp;q=re"`)
	wantNotIn(t, "V1 (matches)", itemOf(t, q, "V1"), " dim")
	wantIn(t, "A2 (Token revocation)", itemOf(t, q, "A2"), "kcard lc-in_review")
	wantNotIn(t, "A2 (Token revocation)", itemOf(t, q, "A2"), " dim")
	wantIn(t, "T1 (no match)", itemOf(t, q, "T1"), " dim")

	// A deep link renders each region as its fragment does.
	for _, target := range []string{"/app/plans/sharing?view=graph&pkg=T1", "/app/plans/aim?view=kanban&pkg=D1", "/app/plans/aim?view=timeline", "/app/plans/aim?pkg=C6&q=plan"} {
		whole := get(t, e.h, target)
		for _, id := range []string{"list", "detail", "summary"} {
			frag := get(t, e.h, target+"&fragment="+id)
			if noTokens(strings.TrimSpace(frag)) != noTokens(inner(t, whole, id)) {
				t.Errorf("%s: fragment %s differs from the page's region", target, id)
			}
		}
	}
}

func TestAppPlanFragments(t *testing.T) {
	e := newPlanEnv(t)
	for _, c := range []struct {
		target string
		want   []string
	}{
		{"/app/plans/sharing?view=kanban&fragment=list", []string{`id="plan-kanban"`, `class="kb-col lc-blocked"`}},
		{"/app/plans/sharing?view=timeline&pkg=V1&fragment=list", []string{`id="plan-timeline"`, `class="tl-row lc-in_review sel"`}},
		{"/app/plans/aim?pkg=D1&fragment=detail", []string{"<h2>Event receiver and live state</h2>"}},
		{"/app/plans/aim?fragment=detail", []string{`id="plan-unmatched"`, "#30"}},
		{"/app/plans/aim?fragment=summary", []string{"<h1>ai-manager — build plan</h1>", "<p>26 packages · 16 done</p>"}},
		{"/app/plans/aim?pkg=D1&fragment=menu", []string{`aria-current="page" href="/app/plans/aim"`}},
	} {
		rec := do(t, e.h, "GET", c.target, nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", c.target, rec.Code)
		}
		body := rec.Body.String()
		wantIn(t, c.target, body, c.want...)
		wantNotIn(t, c.target, body, "<html", `id="list"`, `id="detail"`, `id="menu"`, "app-plan.css")
		checkNoInline(t, c.target, body)
		if !e.srv.CSRF().Valid(rec.Header().Get(appCSRFHeader)) {
			t.Errorf("%s: no fresh CSRF token", c.target)
		}
	}
	// The page's partials aren't regions.
	for _, id := range []string{"plan-graph", "plan-kanban", "plan-timeline", "plan-pkg", "plan-overview", "plan-unregister", "plan-dep", "plan-link", "head"} {
		if rec := do(t, e.h, "GET", "/app/plans/aim?fragment="+id, nil, nil); rec.Code != http.StatusNotFound {
			t.Errorf("fragment %s = %d, want 404", id, rec.Code)
		}
	}
}

// The detail pane: lifecycle and ID, needs with their states, unblocks,
// PR, agent and branch, who merges, and the actions.
func TestAppPlanDetail(t *testing.T) {
	e := newPlanEnv(t)

	t1 := get(t, e.h, "/app/plans/sharing?pkg=T1&fragment=detail")
	wantIn(t, "T1", t1, `<span class="pill lc-blocked">Blocked</span>`, `<span class="nid pd-id">T1</span>`, "<h2>Tile filters</h2>",
		"Pivot share links: delivery plan · pivot · size M · lane Access",
		// Needs, each with its state, linking to it; then what it unblocks.
		`<a class="dep" href="/app/plans/sharing?view=graph&amp;pkg=A1" data-pane="list detail"><span class="dep-name"><span class="nid">A1</span> Share tokens</span><span class="dep-state lc-ready">Ready</span></a>`,
		`<span class="nid">G1</span> Redaction rules</span><span class="dep-state lc-ready">Ready</span>`,
		`href="/app/plans/sharing?view=graph&amp;pkg=Q1"`, `href="/app/plans/sharing?view=graph&amp;pkg=Q2"`,
		"<dt>PR</dt><dd>none yet</dd>", "<dt>Agent</dt><dd>none</dd>", "the conventions suggest <code>sharing/t1-tile-filters</code>",
		"<dt>Merges</dt><dd>you (Merges: you): a ready PR waits on you</dd>",
		// No agent: Go is off, with why; Mark done is on (no PR).
		`disabled data-key="g" data-off="No live agent on this package."`, `<p class="hint pd-why">No live agent on this package.</p>`,
		`<input type="hidden" name="done" value="1">`, `data-key="d"`,
		`<details id="pd-link-T1" class="pd-linker">`, `<option value="pr acme/pivot#219">`)
	if i, j := strings.Index(t1, "Needs</p>"), strings.Index(t1, "Unblocks</p>"); i < 0 || j < i || strings.Contains(t1[i:j], "pkg=Q1") {
		t.Error("T1's needs and unblocks are mixed up")
	}

	// A need the plan doesn't have, and a package with none.
	g1 := get(t, e.h, "/app/plans/sharing?pkg=G1&fragment=detail")
	wantIn(t, "G1", g1, "Nothing: it can start any time.", "Also needs: Priya &#43; Theo sign off")
	q3 := get(t, e.h, "/app/plans/sharing?pkg=Q3&fragment=detail")
	wantIn(t, "Q3", q3, "No package waits on it.")

	// D1: its PR with CI, its agent, its branch; the state engine's other
	// badges as signals; Mark done off (it has a PR).
	d1 := get(t, e.h, "/app/plans/aim?pkg=D1&fragment=detail")
	wantIn(t, "D1", d1, `<a href="https://github.com/example-user/ai-manager/pull/21" rel="noreferrer noopener" target="_blank"`,
		`<span class="ci ci-bad">CI red</span>`, "<strong>agent agent-d1</strong>", "<dt>Branch</dt><dd><code>aim/d1-events</code>",
		`<span class="tag sig-bad">CI failing on #21</span>`,
		`data-off="Mark done is for steps without a PR: this one is done when its PR merges."`,
		`<span class="badge b-working">working</span> <span>agent agent-d1 is working · last: Edit: live.go</span>`,
		// Links, with how each was made and Unlink.
		`<summary class="label">Links (3) and how they were made</summary>`, "spawn · exact",
		`<input type="hidden" name="ref" value="lead-1/agent-d1">`, `action="/app/plans/aim/unlink"`)

	// F1 is ✅ in the plan: no Mark done, and why.
	wantIn(t, "F1", get(t, e.h, "/app/plans/aim?pkg=F1&fragment=detail"), `data-off="It is marked ✅ in the plan file, so it is done."`)

	// The Link picker: the Unmatched lane and the sessions no package
	// links (C6's and the job are linked; the lead and the loose one
	// aren't).
	wantIn(t, "picker", d1, `<optgroup label="Unmatched">`, `<option value="pr example-user/ai-manager#30">PR #30 · Tidy the README (example-user/ai-manager)</option>`,
		`<optgroup label="Sessions">`, `<option value="agent loose-1">loose session (interactive)</option>`)
	wantNotIn(t, "picker", d1, `value="agent sess-c6"`, `value="agent job:0a1b2c3d"`)

	// Nothing selected: the plan's own pane.
	plan := get(t, e.h, "/app/plans/sharing?fragment=detail")
	wantIn(t, "plan pane", plan, "<h2>Pivot share links: delivery plan</h2>", "15 packages · 0 done",
		`<div class="bar" role="img"`, "Ready to dispatch", `href="/app/plans/sharing?view=graph&amp;pkg=Q4"`,
		`id="plan-warnings"`, `id="plan-unmatched"`, ">#219<", `<option value="T1">T1 Tile filters</option>`, `action="/app/plans/sharing/link"`,
		`<input type="hidden" name="ref" value="acme/pivot#219">`, "<code>acme/pivot</code>")
	// What needs you there is the page's badges (internal/prwait's rule).
	wantIn(t, "plan pane", plan, `<span class="badge b-waiting">waiting on you</span> #215 ready to merge · this plan merges: you`)
}

// The badges on boxes, cards and rows: needs you, waiting on you, review
// requested, waiting on lead, snoozed and working.
func TestAppPlanBadges(t *testing.T) {
	e := newPlanEnv(t)
	ctx := context.Background()
	// C6's session waits on a permission prompt; your review is requested
	// on W3's PR (pivot-web#32).
	must(t, e.st.UpdateSessionLive(ctx, "pid:42", store.SessionLive{State: "needs_you", StateReason: "permission prompt", NeedsYouSince: t0.Add(-4 * time.Minute)}))
	_, err := e.st.SetReviewRequests(ctx, []string{"acme"}, []store.PRKey{{Repo: "acme/pivot-web", Number: 32}}, t0)
	must(t, err)

	badge := func(slug, view, id string) string {
		t.Helper()
		return itemOf(t, inner(t, get(t, e.h, "/app/plans/"+slug+"?view="+view), "list"), id)
	}
	for _, c := range []struct {
		slug, id, kind, text string
	}{
		{"aim", "C6", "needs", "needs you"},            // its agent waits on you
		{"aim", "R1", "needs", "needs you"},            // your own step is ready
		{"sharing", "A2", "waiting", "waiting on you"}, // #215 ready to merge, and you merge
		{"sharing", "W3", "review", "review requested"},
		{"aim", "C4", "lead", "waiting on lead"}, // a draft no agent works on
		{"aim", "D1", "working", "working"},      // agent-d1 runs
	} {
		node := badge(c.slug, "graph", c.id)
		wantIn(t, c.slug+" "+c.id+" box", node, `<g class="pg-badge b-`+c.kind+`">`, `text-anchor="middle">`+c.text+`</text>`, ", "+c.text+`"`)
		wantIn(t, c.slug+" "+c.id+" card", badge(c.slug, "kanban", c.id), `<span class="badge b-`+c.kind+`" title="`)
		wantIn(t, c.slug+" "+c.id+" row", badge(c.slug, "timeline", c.id), "; "+c.text)
	}
	wantNotIn(t, "T1 (blocked, nothing)", badge("sharing", "graph", "T1"), "pg-badge")
	wantIn(t, "W3 detail", get(t, e.h, "/app/plans/sharing?pkg=W3&fragment=detail"),
		`<span class="badge b-review">review requested</span> <span>#32 your review is requested</span>`)
	wantIn(t, "C6 detail", get(t, e.h, "/app/plans/aim?pkg=C6&fragment=detail"),
		`<span class="badge b-needs">needs you</span> <span>c6 agent needs you: permission prompt · 4m</span>`)

	// Snoozed in the Needs you inbox: the badge says so instead.
	_, err = e.srv.SyncInbox(ctx)
	must(t, err)
	items, err := e.st.ListInbox(ctx, store.InboxFilter{Waiting: true})
	must(t, err)
	i := slices.IndexFunc(items, func(it store.InboxItem) bool { return it.Ref == "acme/pivot#215" })
	if i < 0 || items[i].PlanSlug != "sharing" || items[i].Pkg != "A2" {
		t.Fatalf("no inbox item for A2's #215 in %+v", items)
	}
	if rec, a := e.postJSON("/app/inbox/snooze", url.Values{"item": {strconv.FormatInt(items[i].ID, 10)}, "snooze": {"15m"}}); rec.Code != http.StatusOK {
		t.Fatalf("snooze = %d %+v", rec.Code, a)
	}
	a2 := badge("sharing", "graph", "A2")
	wantIn(t, "A2 snoozed", a2, `<g class="pg-badge b-snoozed">`, "snoozed: what waits on you here is snoozed in Needs you")
	wantNotIn(t, "A2 snoozed", a2, "waiting on you")
	wantIn(t, "A3 not snoozed", badge("sharing", "graph", "A3"), `<g class="pg-badge b-waiting">`)
	if s := e.srv.planSnoozed(ctx, "sharing"); len(s) != 1 || !s["A2"] {
		t.Errorf("planSnoozed = %v, want A2", s)
	}

	// On a plan whose lead merges, a ready PR waits on the lead, not on you.
	src, err := os.ReadFile(e.sharing)
	must(t, err)
	e.write(e.sharing, strings.Replace(string(src), "\n", "\n\nMerges: lead\n", 1))
	e.api("POST", "/api/plans", api.RegisterRequest{Path: e.sharing, Repos: []string{"acme/pivot", "acme/pivot-web"}}, http.StatusOK)
	// The next inbox pass (aimd's ticker) ends #215's snoozed episode: it
	// doesn't wait on you any more.
	_, err = e.srv.SyncInbox(ctx)
	must(t, err)
	a2 = badge("sharing", "graph", "A2")
	wantIn(t, "A2 on a lead-merged plan", a2, `<g class="pg-badge b-lead">`, "#215 ready to merge · this plan&#39;s lead merges it")
	wantNotIn(t, "A2 on a lead-merged plan", a2, "waiting on you")
	wantIn(t, "W3 on a lead-merged plan", badge("sharing", "graph", "W3"), `b-review`) // you were asked by name
	wantIn(t, "merges", get(t, e.h, "/app/plans/sharing?pkg=A2&fragment=detail"),
		"<dt>Merges</dt><dd>the lead session (Merges: lead): a ready PR waits on the lead, not on you</dd>")

	// Snoozed stands in for every badge that waits on you.
	sn, err := e.srv.load(ctx)
	must(t, err)
	fresh, err := e.srv.freshFacts(ctx, sn)
	must(t, err)
	b := fresh.apply(e.board("aim"))
	for _, p := range b.Packages {
		if p.ID != "C6" {
			continue
		}
		var kinds []string
		for _, bd := range planBadgesOf(sn, fresh.prs, p, true) {
			kinds = append(kinds, bd.Kind)
		}
		if !slices.Equal(kinds, []string{"snoozed"}) {
			t.Errorf("C6 snoozed: badges %v, want [snoozed]", kinds)
		}
	}
}

// Mark done and its Undo, from the script (JSON) and from a plain form.
func TestAppPlanMarkDone(t *testing.T) {
	e := newPlanEnv(t)

	// The script: a toast with an Undo, back to the same view.
	rec, a := e.postJSON("/app/plans/sharing/done", url.Values{"pkg": {"G1"}, "done": {"1"}, "return": {"/app/plans/sharing?view=kanban&pkg=G1"}})
	if rec.Code != http.StatusOK || a.Location != "/app/plans/sharing?pkg=G1&view=kanban" || a.Toast == nil ||
		a.Toast.Text != "Marked G1 done (the plan file is unchanged)." || a.Toast.Undo == nil {
		t.Fatalf("Mark done = %d %+v (toast %+v)", rec.Code, a, a.Toast)
	}
	if u := a.Toast.Undo; u.Action != "/app/plans/sharing/done" || u.Fields["pkg"] != "G1" || u.Fields["done"] != "0" || u.Fields["return"] != a.Location {
		t.Errorf("Mark done's Undo = %+v", u)
	}
	if lc := e.lifecycle("sharing", "G1"); lc != "done" {
		t.Errorf("G1 after Mark done: %s", lc)
	}
	detail := get(t, e.h, "/app/plans/sharing?pkg=G1&fragment=detail")
	wantIn(t, "G1 done", detail, `<span class="pill lc-done">Done</span>`, `<input type="hidden" name="done" value="0">`, `data-key="u"`, "Undo Mark done")

	// A second one unblocks T1 and says so.
	_, a = e.postJSON("/app/plans/sharing/done", url.Values{"pkg": {"a1"}, "done": {"1"}, "return": {"/app/plans/sharing"}})
	if a.Toast == nil || a.Toast.Text != "Marked A1 done (the plan file is unchanged). Now ready to dispatch: T1." {
		t.Errorf("second Mark done toast %+v", a.Toast)
	}
	if lc := e.lifecycle("sharing", "T1"); lc != "ready" {
		t.Errorf("T1 with A1 and G1 done: %s", lc)
	}

	// The Undo posts the toast's fields back.
	vals := url.Values{}
	for k, v := range a.Toast.Undo.Fields {
		vals.Set(k, v)
	}
	rec, a = e.postJSON(a.Toast.Undo.Action, vals)
	if rec.Code != http.StatusOK || a.Toast == nil || a.Toast.Text != "Undid Mark done on A1; it is ready now." || a.Toast.Undo != nil {
		t.Errorf("Undo = %d %+v", rec.Code, a.Toast)
	}
	if lc := e.lifecycle("sharing", "T1"); lc != "blocked" {
		t.Errorf("T1 after the undo: %s", lc)
	}

	// A plain form: a 303 with the toast, whose Undo is a form.
	page := e.follow(e.post("/app/plans/sharing/done", url.Values{"pkg": {"Q4"}, "done": {"1"}, "return": {"/app/plans/sharing?view=graph&pkg=Q4"}}),
		"/app/plans/sharing?pkg=Q4&toast=")
	toast := section(t, page, "toasts")
	wantIn(t, "toast", toast, "<span>Marked Q4 done (the plan file is unchanged).</span>", `<form method="post" action="/app/plans/sharing/done" data-pane>`,
		`<input type="hidden" name="done" value="0">`, `<input type="hidden" name="pkg" value="Q4">`)
	wantIn(t, "page after the form", inner(t, page, "detail"), `<span class="pill lc-done">Done</span>`)
	page = e.follow(e.post("/app/plans/sharing/done", url.Values{"pkg": {"Q4"}, "done": {"0"}, "return": {"/app/plans/sharing?view=graph&pkg=Q4"}}), "/app/plans/sharing?")
	wantIn(t, "undone", section(t, page, "toasts"), "Undid Mark done on Q4; it is ready now.")

	// A refusal: JSON's error, a plain form's error toast.
	rec, a = e.postJSON("/app/plans/sharing/done", url.Values{"pkg": {"Z9"}, "done": {"1"}, "return": {"/app/plans/sharing"}})
	if rec.Code != http.StatusNotFound || !strings.Contains(a.Error, "Not changed: ") || !strings.Contains(a.Error, "Z9") {
		t.Errorf("unknown package = %d %+v", rec.Code, a)
	}
	page = e.follow(e.post("/app/plans/sharing/done", url.Values{"pkg": {"Z9"}, "return": {"/app/plans/sharing"}}), "/app/plans/sharing?toast=")
	wantIn(t, "refused", section(t, page, "toasts"), `<div class="toast error" role="alert" data-toast>`, "Not changed: ")
	// A plan that's gone sends the browser to the Overview.
	rec, a = e.postJSON("/app/plans/nope/done", url.Values{"pkg": {"A1"}})
	if rec.Code != http.StatusNotFound || a.Location != "/app/" {
		t.Errorf("unknown plan = %d %+v", rec.Code, a)
	}
}

// Link from the picker or by hand, from the script and from a plain form,
// with an Undo that unlinks.
func TestAppPlanLink(t *testing.T) {
	e := newPlanEnv(t)

	// The picker's choice: the Unmatched PR onto T1.
	rec, a := e.postJSON("/app/plans/sharing/link", url.Values{"pkg": {"T1"}, "target": {"pr acme/pivot#219"}, "return": {"/app/plans/sharing?view=graph&pkg=T1"}})
	if rec.Code != http.StatusOK || a.Location != "/app/plans/sharing?pkg=T1&view=graph" || a.Toast == nil ||
		a.Toast.Text != "Linked PR acme/pivot#219 to T1." || a.Toast.Undo == nil || a.Toast.Undo.Action != "/app/plans/sharing/unlink" {
		t.Fatalf("link = %d %+v (toast %+v)", rec.Code, a, a.Toast)
	}
	if lc := e.lifecycle("sharing", "T1"); lc != "in_review" {
		t.Errorf("T1 after linking #219: %s", lc)
	}
	d := get(t, e.h, "/app/plans/sharing?pkg=T1&fragment=detail")
	wantIn(t, "T1 linked", d, ">#219<", "manual · exact")
	wantNotIn(t, "T1 linked", d, `value="pr acme/pivot#219"`) // no longer unmatched
	wantNotIn(t, "Unmatched", get(t, e.h, "/app/plans/sharing?fragment=detail"), ">#219<")

	// Its Undo unlinks it again.
	vals := url.Values{}
	for k, v := range a.Toast.Undo.Fields {
		vals.Set(k, v)
	}
	_, a = e.postJSON(a.Toast.Undo.Action, vals)
	if a.Toast == nil || !strings.HasPrefix(a.Toast.Text, "Unlinked PR acme/pivot#219 from T1.") || a.Toast.Undo == nil || a.Toast.Undo.Label != "Link again" {
		t.Errorf("unlink toast %+v", a.Toast)
	}
	if lc := e.lifecycle("sharing", "T1"); lc != "blocked" {
		t.Errorf("T1 after the undo: %s", lc)
	}

	// By hand, from a plain form on the plan's pane (an Unmatched row):
	// back to the page with the package selected.
	page := e.follow(e.post("/app/plans/aim/link", url.Values{"pkg": {"C5"}, "kind": {"pr"}, "ref": {"example-user/ai-manager#30"}, "return": {"/app/plans/aim?view=kanban"}}),
		"/app/plans/aim?pkg=C5&toast=")
	wantIn(t, "toast", section(t, page, "toasts"), "<span>Linked PR example-user/ai-manager#30 to C5.</span>",
		`<form method="post" action="/app/plans/aim/unlink" data-pane>`, `<input type="hidden" name="ref" value="example-user/ai-manager#30">`)
	wantIn(t, "C5 selected", inner(t, page, "detail"), `<span class="nid pd-id">C5</span>`, ">#30<")

	// A session from the picker, as an agent link.
	_, a = e.postJSON("/app/plans/aim/link", url.Values{"pkg": {"R1"}, "target": {"agent loose-1"}, "return": {"/app/plans/aim?pkg=R1"}})
	if a.Toast == nil || !strings.HasPrefix(a.Toast.Text, "Linked agent loose-1") || !strings.HasSuffix(a.Toast.Text, " to R1.") {
		t.Errorf("agent link toast %+v (%s)", a.Toast, a.Error)
	}
	wantIn(t, "R1 agent", get(t, e.h, "/app/plans/aim?pkg=R1&fragment=detail"), "<strong>loose session</strong>")

	// Refused: a bad ref, as JSON and as a plain form.
	rec, a = e.postJSON("/app/plans/sharing/link", url.Values{"pkg": {"T1"}, "kind": {"pr"}, "ref": {"not a pr"}, "return": {"/app/plans/sharing?pkg=T1"}})
	if rec.Code < 400 || rec.Code >= 500 || !strings.Contains(a.Error, "Not linked: a pr ref is owner/repo#123 or a pull request URL") {
		t.Errorf("bad ref = %d %+v", rec.Code, a)
	}
	page = e.follow(e.post("/app/plans/sharing/link", url.Values{"pkg": {"Z9"}, "target": {"pr acme/pivot#219"}, "return": {"/app/plans/sharing"}}), "/app/plans/sharing?")
	wantIn(t, "unknown package", section(t, page, "toasts"), "toast error", "plan sharing has no package Z9")
}

// Go to agent follows Go's rules, shows why it's off, and decides again on
// the server.
func TestAppPlanGo(t *testing.T) {
	e := newPlanEnv(t)

	// D1's agent is a subagent whose lead runs in iTerm2: on.
	d1 := get(t, e.h, "/app/plans/aim?view=kanban&pkg=D1&fragment=detail")
	wantIn(t, "D1", d1, `<form method="post" action="/app/plans/aim/go" data-pane="detail">`, `<input type="hidden" name="pkg" value="D1">`,
		`<input type="hidden" name="return" value="/app/plans/aim?pkg=D1&amp;view=kanban">`,
		`<button type="submit" class="btn primary" data-key="g" aria-keyshortcuts="g">Go to agent <kbd>g</kbd></button>`)
	wantNotIn(t, "D1", d1, "pd-why\">No", "data-off=\"Runs")

	// Off, with the reason shown and given to the g key.
	for id, why := range map[string]string{
		"C6": "Runs inside tmux; Go can only switch to iTerm2 and Terminal tabs.",
		"E2": "Background session: no terminal tab to switch to. Open it with `claude attach 0a1b2c3d`.",
		"A1": "No live agent on this package.",
	} {
		d := get(t, e.h, "/app/plans/aim?pkg="+id+"&fragment=detail")
		r := attr(t, why)
		wantIn(t, id, d, `<button type="button" class="btn primary" disabled data-key="g" data-off="`+r+`" title="`+r+`">`,
			`<p class="hint pd-why">`+r+`</p>`)
		wantNotIn(t, id, d, `action="/app/plans/aim/go"`)
	}

	// Pressed: from the script, a toast; from a plain form, a 303 with it.
	rec, a := e.postJSON("/app/plans/aim/go", url.Values{"pkg": {"D1"}, "return": {"/app/plans/aim?pkg=D1"}})
	if rec.Code != http.StatusOK || a.Toast == nil || a.Toast.Text != e.g.res.Message || a.Location != "/app/plans/aim?pkg=D1" {
		t.Errorf("Go = %d %+v", rec.Code, a)
	}
	if !slices.Equal(e.g.keys, []string{"lead-1/agent-d1"}) {
		t.Errorf("Go got keys %v", e.g.keys)
	}
	page := e.follow(e.post("/app/plans/aim/go", url.Values{"pkg": {"d1"}, "return": {"/app/plans/aim?pkg=D1"}}), "/app/plans/aim?pkg=D1&toast=")
	wantIn(t, "Go form", section(t, page, "toasts"), "<span>"+attr(t, e.g.res.Message)+"</span>")

	// The server decides: a package Go is off for is refused, whatever
	// the form says, and Go isn't called.
	e.g.keys = nil
	rec, a = e.postJSON("/app/plans/aim/go", url.Values{"pkg": {"C6"}, "key": {"pid:42"}, "return": {"/app/plans/aim?pkg=C6"}})
	if rec.Code != http.StatusConflict || a.Error != "Go: Runs inside tmux; Go can only switch to iTerm2 and Terminal tabs." || len(e.g.keys) != 0 {
		t.Errorf("Go on C6 = %d %+v, keys %v", rec.Code, a, e.g.keys)
	}
	if rec, a = e.postJSON("/app/plans/aim/go", url.Values{"pkg": {"Z9"}}); rec.Code != http.StatusNotFound || !strings.Contains(a.Error, "has no package") {
		t.Errorf("Go on Z9 = %d %+v", rec.Code, a)
	}
	// Go's own refusal comes back with its status.
	e.g.err = &focus.Error{Status: http.StatusUnprocessableEntity, Msg: "no iTerm2 tab has aim lead"}
	if rec, a = e.postJSON("/app/plans/aim/go", url.Values{"pkg": {"D1"}}); rec.Code != http.StatusUnprocessableEntity || a.Error != "Go: no iTerm2 tab has aim lead" {
		t.Errorf("Go refused = %d %+v", rec.Code, a)
	}

	// Without a Go service: off everywhere, and the route refuses.
	e.noGo = true
	e.serve()
	wantIn(t, "no Go", get(t, e.h, "/app/plans/aim?pkg=D1&fragment=detail"), `data-off="Go isn&#39;t available: this aimd runs without it."`)
	if rec, _ := e.postJSON("/app/plans/aim/go", url.Values{"pkg": {"D1"}}); rec.Code != http.StatusNotFound {
		t.Errorf("Go without a service = %d", rec.Code)
	}
}

// The forms need the CSRF token, like every app form.
func TestAppPlanFormsNeedCSRF(t *testing.T) {
	e := newPlanEnv(t)
	for _, target := range []string{"/app/plans/sharing/done", "/app/plans/sharing/link", "/app/plans/sharing/unlink", "/app/plans/aim/go",
		"/app/plans/sharing/unregister"} {
		vals := url.Values{"pkg": {"G1"}, "done": {"1"}, "kind": {"pr"}, "ref": {"acme/pivot#219"}, CSRFField: {"forged"}}
		if rec := e.post(target, vals); rec.Code != http.StatusForbidden {
			t.Errorf("%s with a forged token = %d", target, rec.Code)
		}
	}
	if lc := e.lifecycle("sharing", "G1"); lc != "ready" {
		t.Errorf("G1 = %s after refused forms", lc)
	}
	if len(e.g.keys) != 0 {
		t.Errorf("Go called by a refused form: %v", e.g.keys)
	}
	if rec := do(t, e.h, "GET", "/api/plans/sharing", nil, nil); rec.Code != http.StatusOK {
		t.Errorf("sharing after a refused Unregister = %d", rec.Code)
	}
}

// Unregister is on the plan's pane (nothing selected): a button that opens
// a confirmation saying what aimd forgets and what stays, with the command
// that registers the file again. Without the script, the button loads the
// page with ?unregister=1, which renders the dialog open.
func TestAppPlanUnregisterDialog(t *testing.T) {
	e := newPlanEnv(t)
	page := get(t, e.h, "/app/plans/aim")
	checkNoInline(t, "plan page", page)
	detail := inner(t, page, "detail")
	wantIn(t, "plan pane", detail,
		`<button type="button" class="btn js-only" data-open="plan-unregister">Unregister plan…</button>`,
		`<noscript><a class="btn" href="/app/plans/aim?view=graph&amp;unregister=1">Unregister plan…</a></noscript>`,
		`<dialog id="plan-unregister" class="dialog" aria-labelledby="plan-unregister-title">`,
		`<h2 id="plan-unregister-title">Unregister ai-manager — build plan?</h2>`,
		"aimd forgets this plan. There's no Undo.",
		"It leaves the dashboard and the menu.",
		"Its packages, links and history go: every link (the ones you made by hand too), your unlinks and Mark done, the timeline and its alerts",
		"Its lead session is no longer monitored as a lead",
		"The plan file stays as it is in the repo",
		"Registering the file again starts afresh",
		`<p class="code">/plan-register `+e.aimPlan+`</p>`,
		`<form method="post" action="/app/plans/aim/unregister" data-pane="list detail">`,
		`<input type="hidden" name="return" value="/app/plans/aim?view=graph">`,
		`<button type="button" class="btn js-only" data-close>Cancel</button>`,
		`<noscript><a class="btn" href="/app/plans/aim?view=graph">Cancel</a></noscript>`,
		`<button type="submit" class="btn primary">Unregister aim</button>`)
	wantIn(t, "plan pane", detail, `name="csrf_token" value="`)
	wantNotIn(t, "plan pane", detail, `aria-labelledby="plan-unregister-title" open>`)
	// The detail fragment draws the same; the view and search are kept.
	frag := get(t, e.h, "/app/plans/aim?view=kanban&q=mod&fragment=detail")
	checkNoInline(t, "detail fragment", frag)
	wantIn(t, "fragment", frag, `data-open="plan-unregister"`, `href="/app/plans/aim?view=kanban&amp;q=mod&amp;unregister=1"`,
		`<input type="hidden" name="return" value="/app/plans/aim?view=kanban&amp;q=mod">`)
	// A package's pane has no Unregister.
	wantNotIn(t, "package pane", get(t, e.h, "/app/plans/aim?pkg=D1&fragment=detail"), "plan-unregister", "/unregister")
	// Without the script: ?unregister=1 opens it.
	open := get(t, e.h, "/app/plans/aim?view=graph&unregister=1")
	checkNoInline(t, "open dialog", open)
	wantIn(t, "?unregister=1", inner(t, open, "detail"), `<dialog id="plan-unregister" class="dialog" aria-labelledby="plan-unregister-title" open>`)
}

// POST /app/plans/{slug}/unregister unregisters the plan with the plan
// service's logic:
// gone from the menu, the API and the page, its links and history with
// it, its lead no longer monitored, and its file unchanged. The answer
// sends the browser to the Overview with a toast that says how to register
// again, without an Undo, from the script and from a plain form.
func TestAppPlanUnregister(t *testing.T) {
	e := newPlanEnv(t)
	ctx := context.Background()
	repos := []string{"acme/pivot", "acme/pivot-web"}
	// The sharing plan's lead: a session that registers it.
	e.api("POST", "/api/plans", api.RegisterRequest{Path: e.sharing, Repos: repos, SessionID: "lead-es"}, http.StatusOK)
	before, err := os.ReadFile(e.sharing)
	must(t, err)
	fi, err := os.Stat(e.sharing)
	must(t, err)
	p, err := e.st.GetPlanBySlug(ctx, "sharing")
	must(t, err)
	links, err := e.st.ListLinks(ctx, p.ID)
	must(t, err)
	hist, err := e.st.ListLifecycleHistory(ctx, p.ID, store.HistoryFilter{})
	must(t, err)
	if len(links) == 0 || len(hist) == 0 || !leads(t, e.st)["lead-es"] {
		t.Fatalf("before: %d links, %d history rows, leads %v", len(links), len(hist), leads(t, e.st))
	}
	wantIn(t, "menu before", section(t, get(t, e.h, "/app/"), "menu"), `href="/app/plans/sharing"`)

	// From the script: JSON, to the Overview, with the command and no Undo.
	rec, a := e.postJSON("/app/plans/sharing/unregister", url.Values{"return": {"/app/plans/sharing?view=graph"}})
	want := "Unregistered Pivot share links: delivery plan. The plan file is unchanged; to register it again, run /plan-register " + e.sharing
	if rec.Code != http.StatusOK || a.Location != "/app/" || a.Toast == nil || a.Toast.Text != want || a.Toast.Undo != nil || a.Toast.Kind != "" {
		t.Fatalf("unregister = %d %+v (toast %+v)", rec.Code, a, a.Toast)
	}

	// Gone from the API, the page and the menu: the Overview's, where the
	// answer goes, and any page's menu refresh.
	var list api.PlanList
	decode(t, do(t, e.h, "GET", "/api/plans", nil, nil), &list)
	if len(list.Plans) != 1 || list.Plans[0].Slug != "aim" {
		t.Errorf("/api/plans = %d plans, want aim alone", len(list.Plans))
	}
	if rec := do(t, e.h, "GET", "/api/plans/sharing", nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("GET /api/plans/sharing = %d", rec.Code)
	}
	if rec := do(t, e.h, "GET", "/app/plans/sharing", nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("GET /app/plans/sharing = %d", rec.Code)
	}
	for _, target := range []string{"/app/", "/app/sessions?fragment=menu", "/app/plans/aim?fragment=menu"} {
		m := get(t, e.h, target)
		if !strings.Contains(target, "fragment=") {
			m = section(t, m, "menu")
		}
		wantNotIn(t, target, m, `href="/app/plans/sharing"`, "Pivot share links")
		wantIn(t, target, m, `href="/app/plans/aim"`)
	}

	// What the plan service removes, as the dialog says: links, history, the lead.
	if links, err := e.st.ListLinks(ctx, p.ID); err != nil || len(links) != 0 {
		t.Errorf("links after = %d (%v)", len(links), err)
	}
	if hist, err := e.st.ListLifecycleHistory(ctx, p.ID, store.HistoryFilter{}); err != nil || len(hist) != 0 {
		t.Errorf("history after = %d (%v)", len(hist), err)
	}
	if leads(t, e.st)["lead-es"] {
		t.Errorf("lead-es is still monitored as the plan's lead")
	}
	// The plan file is never touched.
	after, err := os.ReadFile(e.sharing)
	must(t, err)
	fi2, err := os.Stat(e.sharing)
	must(t, err)
	if string(after) != string(before) || !fi2.ModTime().Equal(fi.ModTime()) {
		t.Errorf("the plan file changed")
	}

	// Again: 404, back to the Overview.
	rec, a = e.postJSON("/app/plans/sharing/unregister", url.Values{"return": {"/app/plans/sharing"}})
	if rec.Code != http.StatusNotFound || a.Location != "/app/" || a.Error != `Not unregistered: no plan "sharing" is registered` {
		t.Errorf("second unregister = %d %+v", rec.Code, a)
	}
	page := e.follow(e.post("/app/plans/nope/unregister", url.Values{"return": {"/app/plans/nope"}}), "/app/?toast=")
	wantIn(t, "unknown plan, plain form", section(t, page, "toasts"), "toast error", `Not unregistered: no plan &#34;nope&#34; is registered`)

	// A plain form: a 303 to the Overview with the toast, which has no
	// Undo form; the menu has no plans left.
	page = e.follow(e.post("/app/plans/aim/unregister", url.Values{"return": {"/app/plans/aim?view=kanban"}}), "/app/?toast=")
	toast := section(t, page, "toasts")
	wantIn(t, "plain form", toast, "<span>Unregistered ai-manager — build plan. The plan file is unchanged; to register it again, run /plan-register "+e.aimPlan+"</span>")
	wantNotIn(t, "plain form", toast, "<form", "Undo")
	wantNotIn(t, "menu", section(t, page, "menu"), `href="/app/plans/`)
	checkNoInline(t, "Overview after", page)

	// Registering the file again starts a new board.
	e.api("POST", "/api/plans", api.RegisterRequest{Path: e.sharing, Repos: repos}, http.StatusCreated)
	if again, err := os.ReadFile(e.sharing); err != nil || string(again) != string(before) {
		t.Errorf("the plan file changed on registering again (%v)", err)
	}
}

// leads are the sessions monitored as a plan's lead.
func leads(t *testing.T, st *store.Store) map[string]bool {
	t.Helper()
	rows, err := st.ListMonitored(context.Background())
	must(t, err)
	out := map[string]bool{}
	for _, m := range rows {
		if m.Reason == store.MonitorPlan {
			out[m.SessionID] = true
		}
	}
	return out
}

// The toast keeps the whole command: a title too long for it gives way to
// the slug.
func TestUnregisteredText(t *testing.T) {
	res := api.Unregistered{Slug: "big", Title: strings.Repeat("Long title ", 30), Path: "/w/repo/docs/big-plan.md"}
	if got := unregisteredText(res); got != "Unregistered big. The plan file is unchanged; to register it again, run /plan-register /w/repo/docs/big-plan.md" {
		t.Errorf("long title: %q", got)
	}
	res.Title = ""
	if got := unregisteredText(res); !strings.HasPrefix(got, "Unregistered big. ") {
		t.Errorf("no title: %q", got)
	}
}

// Plan files and PRs are untrusted: nothing reaches the page as markup.
func TestAppPlanEscapes(t *testing.T) {
	e := newPlanEnv(t)
	plan := e.write(filepath.Join(e.root, "evil", "hostile-plan.md"), `# <script>alert("title")</script> plan

Plan slug: hostile

| Lane | Packages | Rule |
|---|---|---|
| <img src=x onerror=alert(1)> | X1, X2 | single-writer |

| Package | What | Repo | Size | Needs |
|---|---|---|---|---|
| **X1 <script>alert('pkg')</script>** | w | <svg onload=alert(3)> | S | — |
| **X2 Second "quoted" & <iframe src=javascript:alert(4)>** | w | r | S | X1, Z9; you: <img src=x onerror=alert(5)> |
`)
	must(t, e.st.UpsertPRs(context.Background(), store.PR{Repo: "evil/repo", Number: 2, HeadBranch: `evil"><script>alert(8)</script>`,
		Title: `<script>alert(9)</script>`, State: store.PROpen, URL: "https://github.com/evil/repo/pull/2", FirstSeenAt: t0.Add(-time.Hour)}))
	e.api("POST", "/api/plans", api.RegisterRequest{Path: plan, Repos: []string{"evil/repo"}}, http.StatusCreated)
	for _, target := range []string{"/app/plans/hostile", "/app/plans/hostile?view=kanban&pkg=X2", "/app/plans/hostile?view=timeline&pkg=X1",
		"/app/plans/hostile?pkg=X2&q=" + url.QueryEscape(`"><script>alert(10)</script>`)} {
		page := get(t, e.h, target)
		// (The titles say "javascript:" as text, so checkNoInline can't run.)
		wantNotIn(t, target, page, "<script>alert", "<img src=x", "<svg onload", "<iframe", `"><script>`, `href="javascript:`, `src="javascript:`)
	}
	wantIn(t, "escaped", get(t, e.h, "/app/plans/hostile?pkg=X1"), "&lt;script&gt;alert(&#39;pkg&#39;)&lt;/script&gt;")
}

// The graph stays readable for a big plan (the roadmap fixture and more) and the
// sharing plan: every package a box with its title cut to fit, in a drawing
// that scrolls sideways but not far down.
func TestAppPlanGraphReadable(t *testing.T) {
	e := newBoardEnv(t)
	src, err := os.ReadFile(filepath.Join("..", "..", "testdata", "plans", "ROADMAP.md"))
	must(t, err)
	// Make it a big plan (45 packages): a phase of 19 more, fanning out.
	var more strings.Builder
	more.WriteString("\n### Phase 5 — The app\n\n| Package | What | Size | Needs |\n|---|---|---|---|\n")
	for i := 1; i <= 19; i++ {
		needs := "D3, E2"
		if i > 1 {
			needs = "W" + strconv.Itoa((i-2)/3+1)
		}
		more.WriteString("| **W" + strconv.Itoa(i) + " A page whose title is longer than a box** | x | M | " + needs + " |\n")
	}
	live := e.write(filepath.Join(e.root, "live", "ROADMAP.md"), string(src)+more.String())
	e.api("POST", "/api/plans", api.RegisterRequest{Path: live, Repos: []string{aimRepo}}, http.StatusCreated)
	e.registerSharing()
	for slug, maxH := range map[string]int{"aim": 1200, "sharing": 600} {
		b, err := e.svc.Plan(context.Background(), slug)
		must(t, err)
		g := planGraphOf(b, planQuery{slug: slug, view: planGraph}, nil)
		t.Logf("%s: %d packages in a %d×%d drawing", slug, len(b.Packages), g.W, g.H)
		if len(g.Nodes) != len(b.Packages) || g.H > maxH {
			t.Errorf("%s: %d boxes for %d packages, %d high (at most %d)", slug, len(g.Nodes), len(b.Packages), g.H, maxH)
		}
		for _, n := range g.Nodes {
			if utf8.RuneCountInString(n.Title) > planGraphMetrics.titleRunes {
				t.Errorf("%s %s: title %q not cut to fit", slug, n.ID, n.Title)
			}
		}
		page := get(t, e.h, "/app/plans/"+slug)
		wantIn(t, slug, page, `class="pg-scroll" data-keep-scroll`, `width="`+strconv.Itoa(g.W)+`"`)
	}
	b, err := e.svc.Plan(context.Background(), "aim")
	must(t, err)
	if len(b.Packages) < 40 {
		t.Errorf("the big plan has %d packages; the test means to check a big one", len(b.Packages))
	}
	// A drawing wider than the pane scrolls in its own box.
	wantIn(t, "app-plan.css", string(staticFiles["app-plan.css"].data), ".pg-scroll { overflow: auto;", ".pg { display: block; max-width: none;")
}

// tlPlans is fakePlans with a timeline.
type tlPlans struct {
	fakePlans
	tl api.Timeline
}

func (f tlPlans) Timeline(context.Context, string, api.TimelineQuery) (api.Timeline, error) {
	return f.tl, nil
}

// The timeline's bars come from the history: working from the first sign
// of work to the review, then in review up to now; a done package from
// start to finish.
func TestAppPlanTimelineBars(t *testing.T) {
	day := 24 * time.Hour
	b := api.Plan{Slug: "p", Title: "P", Total: 4, Counts: map[string]int{"in_review": 1, "working": 1, "done": 1, "blocked": 1}, Packages: []api.Package{
		{ID: "R1", Title: "In review", Lifecycle: "in_review", Since: t0.Add(-2 * day)},
		{ID: "W1", Title: "Working", Lifecycle: "working", Since: t0.Add(-day)},
		{ID: "D1", Title: "Done", Lifecycle: "done", Since: t0.Add(-day)},
		{ID: "B1", Title: "Blocked", Lifecycle: "blocked"},
	}}
	tl := api.Timeline{Entries: []api.TimelineEntry{
		{At: t0.Add(-3 * day), Kind: api.TimelineLifecycle, Packages: []string{"R1"}, Lifecycle: "working", Prev: "ready"},
		{At: t0.Add(-2 * day), Kind: api.TimelinePR, Action: "opened", Packages: []string{"R1"}},
		{At: t0.Add(-2*day + time.Minute), Kind: api.TimelineLifecycle, Packages: []string{"R1"}, Lifecycle: "in_review", Prev: "working"},
		{At: t0.Add(-36 * time.Hour), Kind: api.TimelineEvent, Packages: []string{"W1"}, Event: "tool"},
		{At: t0.Add(-3 * day), Kind: api.TimelineLink, Packages: []string{"D1"}, Link: &api.LinkRef{Target: api.Target{Kind: store.LinkAgent}}},
		{At: t0.Add(-1 * day), Kind: api.TimelinePR, Action: "merged", Packages: []string{"D1"}},
		{At: t0.Add(-1*day + time.Minute), Kind: api.TimelineLifecycle, Packages: []string{"D1"}, Lifecycle: "done", Prev: "in_review"},
	}}
	srv, _ := newApp(t, func(o *Options) {
		o.Plans = tlPlans{fakePlans: fakePlans{plans: []api.Plan{b}, sums: map[string]api.PlanSummary{"p": {}}}, tl: tl}
	})
	list := inner(t, get(t, srv.Handler(), "/app/plans/p?view=timeline"), "list")
	// The axis runs from the first start (3 days ago) to now.
	for id, bars := range map[string][]string{
		"R1": {`class="tl-bar work" x="0.00%" y="1" width="33.33%" height="12"`, `class="tl-bar review" x="33.33%" y="4" width="66.67%" height="6"`},
		"W1": {`class="tl-bar work" x="50.00%" y="1" width="50.00%"`},
		"D1": {`class="tl-bar done" x="0.00%" y="1" width="66.67%"`},
	} {
		wantIn(t, id, itemOf(t, list, id), bars...)
	}
	wantNotIn(t, "B1", itemOf(t, list, "B1"), "tl-bar")
	wantIn(t, "R1 tip", itemOf(t, list, "R1"), "Worked from Nov 2 12:00; in review since Nov 3 12:00 (2d)")
	// Day ticks at midnight, labelled by date.
	wantIn(t, "axis", list, `>Nov 3</text>`, `>Nov 4</text>`, `>Nov 5</text>`)
}

func TestTimelineTicks(t *testing.T) {
	ps := func(f float64) string { return strconv.FormatFloat(f, 'f', 1, 64) }
	for _, c := range []struct {
		span time.Duration
		want []string
	}{
		{24 * time.Hour, []string{"Nov 4 12:00", "18:00", "Nov 5", "06:00", "12:00"}},
		{6 * 24 * time.Hour, []string{"Oct 31", "Nov 1", "Nov 2", "Nov 3", "Nov 4", "Nov 5"}},
		{50 * 24 * time.Hour, []string{"Sep 17", "Sep 24", "Oct 1", "Oct 8", "Oct 15", "Oct 22", "Oct 29", "Nov 5"}},
	} {
		lo := t0.Add(-c.span)
		pct := func(x time.Time) float64 { return float64(x.Sub(lo)) / float64(c.span) * 100 }
		var got []string
		for _, tk := range timelineTicks(lo, t0, pct, ps) {
			got = append(got, tk.Label)
		}
		if len(got) > maxTicks || !slices.Equal(got[:min(len(got), len(c.want))], c.want) {
			t.Errorf("ticks over %v = %v, want %v", c.span, got, c.want)
		}
	}
}

func TestAppPlanStatic(t *testing.T) {
	h := newDash(t, newTestStore(t), nil).Handler()
	for file, ctype := range map[string]string{"app-plan.js": "text/javascript; charset=utf-8", "app-plan.css": "text/css; charset=utf-8"} {
		rec := do(t, h, "GET", "/static/"+file, nil, nil)
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != ctype {
			t.Errorf("GET /static/%s = %d %q", file, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
	js := string(staticFiles["app-plan.js"].data)
	// The shell loads the graph's links and keeps the scroll: the page
	// only brings the selected box into view.
	wantIn(t, "app-plan.js", js, "aim:swap", "data-keep-scroll", "scrollIntoView")
	wantNotIn(t, "app-plan.js", js, "AIM.navigate", "data-plan-pane", "scrollLeft =")
	wantNotIn(t, "app-plan.js", js, "innerHTML", "outerHTML", "insertAdjacentHTML", "eval(", "new Function", "http://", "https://", "fetch(")
	if len(js) > 4<<10 {
		t.Errorf("app-plan.js is %d bytes; keep it small", len(js))
	}
	css := string(staticFiles["app-plan.css"].data)
	wantNotIn(t, "app-plan.css", css, "@import", "url(http", "fonts.googleapis")
	if node, err := exec.LookPath("node"); err == nil {
		f := filepath.Join(t.TempDir(), "app-plan.js")
		must(t, os.WriteFile(f, staticFiles["app-plan.js"].data, 0o644))
		if out, err := exec.Command(node, "--check", f).CombinedOutput(); err != nil {
			t.Errorf("node --check app-plan.js: %v\n%s", err, out)
		}
	}
}
