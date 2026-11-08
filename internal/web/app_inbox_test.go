package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/inbox"
	"github.com/gravexa-llc/ai-manager/internal/monitor"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/term"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// The inbox's world is seedDash's (newApp), with nothing monitored-only:
//
//	pid:11 routes agent  permission prompt, 10 minutes      Open
//	job:j1 sharing copy  a reply, 3 hours                   Open
//	job:j2 dismissed job a reply, dismissed on the old page Done
//	o/app#7              ready to merge, on plan sharing    Open
//	o/app#8              your review is requested           Open
//
// pid:11 runs in Terminal, so Go is enabled for it; the jobs are
// background sessions, so it isn't for them.
type inboxEnv struct {
	t     *testing.T
	srv   *Server
	h     http.Handler
	st    *store.Store
	g     *stubGo
	clock time.Time
}

func newInboxEnv(t *testing.T) *inboxEnv {
	t.Helper()
	e := &inboxEnv{t: t, clock: t0, g: &stubGo{res: api.GoResult{Key: "pid:11", Action: api.GoFocused, App: "Terminal", Message: "Focused."}}}
	e.srv, e.st = newApp(t, func(o *Options) {
		o.Go = e.g
		o.Terminals = fakeTerms{"pid:11": {Kind: term.KindTerminal, Name: "Terminal"}}
		o.Now = func() time.Time { return e.clock }
	})
	e.h = e.srv.Handler()
	return e
}

// ref is a session's inbox ref, by its key.
func (e *inboxEnv) ref(key string) string {
	e.t.Helper()
	rows, err := e.st.ListSessions(context.Background(), store.SessionFilter{})
	must(e.t, err)
	for _, r := range rows {
		if r.Key == key {
			return inbox.SessionRef(r.ID)
		}
	}
	e.t.Fatalf("no session %s", key)
	return ""
}

// item is the latest item of a ref ("pid:11" or "o/app#7").
func (e *inboxEnv) item(ref string) store.InboxItem {
	e.t.Helper()
	if !strings.Contains(ref, "#") {
		ref = e.ref(ref)
	}
	items, err := e.st.ListInbox(context.Background(), store.InboxFilter{})
	must(e.t, err)
	var found *store.InboxItem
	for i := range items {
		if items[i].Ref == ref {
			found = &items[i]
		}
	}
	if found == nil {
		e.t.Fatalf("no item for %s in %+v", ref, items)
	}
	return *found
}

func (e *inboxEnv) id(ref string) string { return strconv.FormatInt(e.item(ref).ID, 10) }

// post sends an inbox form, as the page's script does (asJSON) or as a
// plain form.
func (e *inboxEnv) post(action string, vals url.Values, asJSON bool) *httptest.ResponseRecorder {
	e.t.Helper()
	vals.Set(CSRFField, e.srv.CSRF().Token())
	hdr := map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": "null", "Sec-Fetch-Site": "same-origin"}
	if asJSON {
		hdr["Accept"] = "application/json"
	}
	return do(e.t, e.h, "POST", "/app/inbox/"+action, strings.NewReader(vals.Encode()), hdr)
}

// answer decodes a JSON answer.
func (e *inboxEnv) answer(rec *httptest.ResponseRecorder, status int) appAnswer {
	e.t.Helper()
	var a appAnswer
	if rec.Code != status {
		e.t.Fatalf("answer %d, want %d: %s", rec.Code, status, rec.Body)
	}
	decode(e.t, rec, &a)
	return a
}

func TestInboxPage(t *testing.T) {
	e := newInboxEnv(t)
	page := get(t, e.h, "/app/inbox")
	wantIn(t, "page", page,
		`<title>(4) aim</title>`, "<h1>Needs you</h1>", "<p>4 open · grouped by plan</p>",
		`href="/static/app-inbox.css?v=`+assetVersion+`"`, `<script src="/static/app-inbox.js?v=`+assetVersion+`" defer></script>`,
		`data-live="sessions prs inbox"`,
	)
	list := section(t, page, "list")
	// The tabs and their counts; Open is on.
	wantIn(t, "tabs", list,
		`<a class="tab on" aria-current="page" href="/app/inbox?tab=open" data-pane="list detail">Open<span class="tcount"><span class="vh">: </span>4</span></a>`,
		`<a class="tab" href="/app/inbox?tab=progress" data-pane="list detail">In progress<span class="tcount"><span class="vh">: </span>0</span></a>`,
		`<a class="tab" href="/app/inbox?tab=snoozed" data-pane="list detail">Snoozed<span class="tcount"><span class="vh">: </span>0</span></a>`,
		`<a class="tab" href="/app/inbox?tab=done" data-pane="list detail">Done<span class="tcount"><span class="vh">: </span>1</span></a>`,
		"Grouped by plan, oldest first in each group.",
	)
	// Grouped: the plan first, then no plan; oldest first within.
	sharing := strings.Index(list, `<span>Pivot share links</span><span>1</span>`)
	noPlan := strings.Index(list, `<span>No plan</span><span>3</span>`)
	j1, pr8, pid11 := strings.Index(list, "sharing copy"), strings.Index(list, "app #8 · Red build"), strings.Index(list, "routes agent")
	if sharing < 0 || noPlan < sharing || !(noPlan < j1 && j1 < pr8 && pr8 < pid11) {
		t.Errorf("order: sharing %d, no plan %d, j1 %d, #8 %d, pid:11 %d\n%s", sharing, noPlan, j1, pr8, pid11, list)
	}
	id11 := e.id("pid:11")
	wantIn(t, "rows", list,
		// A row: kind color, data-item, new (never opened), the chime on a
		// new permission prompt, the wait.
		`<a class="irow kind-permission new" href="/app/inbox?item=`+id11+`&amp;tab=open" data-pane="detail" data-item="`+id11+`" data-chime="inbox-`+id11+`" data-search="feat">`,
		`<span class="ikind">Permission</span><span class="ititle">routes agent</span><span class="isub">app</span>`,
		`<span class="iwait" title="waiting since 2026-11-05T11:50:00Z">10m</span>`,
		`<span class="ikind">Waiting for your reply</span><span class="ititle">sharing copy</span><span class="isub">web · background job</span>`,
		`<span class="ikind">Ready to merge</span><span class="ititle">app #7 · Routes (V1)</span><span class="isub">Ready to merge · CI green</span>`,
		`<a class="irow kind-review new"`, `<span class="ikind">Review requested</span>`,
	)
	// The reply isn't chimed; the dismissed job is in Done.
	wantNotIn(t, "rows", list, `data-chime="inbox-`+e.id("job:j1")+`"`, "dismissed job")
	wantIn(t, "detail", section(t, page, "detail"), "Select an item to see what it's waiting on.")

	// The Done tab: the old dashboard's Dismiss.
	done := get(t, e.h, "/app/inbox?tab=done&fragment=list")
	wantIn(t, "done tab", done, "dismissed job", `<span class="iwait" title="done `)
	if e.item("job:j2").State != inbox.Done {
		t.Errorf("the dismissed job is %s", e.item("job:j2").State)
	}
}

// Group by: plan (the default), repo or session, within the tab showing,
// oldest first in each section. Nothing starts collapsed, since all of it
// needs you; the selected item's section opens whatever the browser
// remembers.
func TestInboxGroupBy(t *testing.T) {
	e := newInboxEnv(t)
	get(t, e.h, "/app/inbox") // a pass opens the items
	j1, pr7, pr8, pid11 := e.id("job:j1"), e.id("o/app#7"), e.id("o/app#8"), e.id("pid:11")
	j1Ref, pid11Ref := e.item("job:j1").Ref, e.item("pid:11").Ref
	rows := func(list, key string) string {
		t.Helper()
		_, body := sectionOf(t, list, key)
		return strings.Join(rowKeys(body), " ")
	}
	for _, c := range []struct {
		group, summary string
		sections       []string
		rows           map[string]string
		heads          []string
	}{
		{"plan", "grouped by plan", []string{"plan:sharing", "plan:-none"},
			map[string]string{"plan:sharing": pr7, "plan:-none": j1 + " " + pr8 + " " + pid11},
			[]string{`<span>Pivot share links</span><span>1</span>`, `<span>No plan</span><span>3</span>`}},
		{"repo", "grouped by repo", []string{"repo:o/app", "repo:o/web"},
			map[string]string{"repo:o/app": pr7 + " " + pr8 + " " + pid11, "repo:o/web": j1},
			[]string{`<span>o/app</span><span>3</span>`, `<span>o/web</span><span>1</span>`}},
		// One section per session, named as the Sessions page names it, by
		// name; then the PRs, which come from no session.
		{"session", "grouped by session", []string{"session:" + strings.TrimPrefix(pid11Ref, "session:"), "session:" + strings.TrimPrefix(j1Ref, "session:"), "session:-prs"},
			map[string]string{"session:-prs": pr7 + " " + pr8},
			[]string{`<span>routes agent</span><span>1</span>`, `<span>sharing copy</span><span>1</span>`, `<span>Pull requests</span><span>2</span>`}},
	} {
		list := get(t, e.h, "/app/inbox?group="+c.group+"&fragment=list")
		if got := sectionKeys(list); strings.Join(got, " ") != strings.Join(c.sections, " ") {
			t.Errorf("%s: sections %v, want %v", c.group, got, c.sections)
		}
		for _, k := range c.sections {
			wantSection(t, list, k, true, false)
		}
		for k, want := range c.rows {
			if got := rows(list, k); got != want {
				t.Errorf("%s: %s rows %s, want %s", c.group, k, got, want)
			}
		}
		wantIn(t, c.group, list, c.heads...)
		wantIn(t, c.group, list, `data-group-by="`+c.group+`" class="on" aria-current="true">`,
			"Grouped by "+c.group+", oldest first in each group.",
			// The tabs and the rows keep the Group by.
			`href="/app/inbox?group=`+c.group+`&amp;tab=done" data-pane="list detail">Done`,
			`href="/app/inbox?group=`+c.group+`&amp;item=`+pid11+`&amp;tab=open"`)
		wantIn(t, c.group, get(t, e.h, "/app/inbox?group="+c.group+"&fragment=summary"), "<p>4 open · "+c.summary+"</p>")
	}
	// The default is plan.
	wantIn(t, "default", get(t, e.h, "/app/inbox?fragment=list"), `data-group-by="plan" class="on" aria-current="true">`)

	// The selected item's section is forced open; the others aren't.
	sel := get(t, e.h, "/app/inbox?group=session&item="+pr8+"&fragment=list")
	wantSection(t, sel, "session:-prs", true, true)
	wantSection(t, sel, "session:"+strings.TrimPrefix(j1Ref, "session:"), true, false)
	// A search forces open every section it leaves.
	found := get(t, e.h, "/app/inbox?group=repo&q=routes&fragment=list")
	wantSection(t, found, "repo:o/app", true, true)
	wantNotIn(t, "search", found, `data-group="repo:o/web"`)

	// Another tab groups its own items: the dismissed job, its session
	// ended.
	done := get(t, e.h, "/app/inbox?group=session&tab=done&fragment=list")
	wantIn(t, "done", done, `<span>dismissed job</span><span>1</span>`)
	if got := len(sectionKeys(done)); got != 1 {
		t.Errorf("done: %d sections", got)
	}
}

// Every selection is in the URL, and renders the same alone as in the page.
func TestInboxSelection(t *testing.T) {
	e := newInboxEnv(t)
	get(t, e.h, "/app/inbox") // a pass opens the items
	id11 := e.id("pid:11")
	target := "/app/inbox?tab=open&item=" + id11
	detail := get(t, e.h, target+"&fragment=detail")
	wantIn(t, "detail", detail,
		`<span class="pill kind-permission">Permission</span>`,
		`<span class="tag term-terminal" title="Runs in a Terminal tab">Terminal</span>`,
		`<span class="istatus">Waiting 10m</span>`, "<h2>routes agent</h2>", `<p class="subtitle">app</p>`,
		// Go (a Terminal tab), Snooze ▾ with four choices, Done.
		`<form method="post" action="/app/inbox/go" data-pane="list detail">`,
		`<input type="hidden" name="item" value="`+id11+`">`, `<input type="hidden" name="return" value="/app/inbox?item=`+id11+`&amp;tab=open">`,
		`<button type="submit" class="btn primary" data-key="g">Go <kbd>g</kbd></button>`,
		`<summary class="btn" data-key="s" aria-keyshortcuts="s">Snooze ▾ <kbd>s</kbd></summary>`,
		`<input type="hidden" name="snooze" value="15m">`, `<button type="submit" class="tab">1 hour</button>`,
		`<button type="submit" class="tab">Until tomorrow 9am</button>`, `<button type="submit" class="tab">Until it changes</button>`,
		`<button type="submit" class="btn" data-key="d" aria-keyshortcuts="d">Done <kbd>d</kbd></button>`,
		// Where and History.
		"<dt>Plan</dt><dd>—</dd>", "<dt>Branch</dt><dd><code>feat</code></dd>", "<dt>Folder</dt><dd><code>~/Projects/app-wt</code></dd>",
		`<dt>PR</dt><dd><a href="https://github.com/o/app/pull/7">o/app#7 (open)</a></dd>`,
		`<ol class="history">`, "· Started waiting: permission prompt</li>",
	)
	wantNotIn(t, "detail", detail, "Move back to Open", "Unsnooze", `data-key="u"`)

	// Opening it cleared its highlight; the list shows it selected.
	list := get(t, e.h, target+"&fragment=list")
	wantIn(t, "list", list, `<a class="irow kind-permission sel" href="/app/inbox?item=`+id11+`&amp;tab=open" data-pane="detail" data-item="`+id11+`" aria-current="true" data-search="feat">`)
	if e.item("pid:11").SeenAt.IsZero() {
		t.Error("opening the item didn't mark it seen")
	}
	// The whole page shows the same regions.
	page := get(t, e.h, target)
	wantIn(t, "page", section(t, page, "detail"), "<h2>routes agent</h2>", "Go <kbd>g</kbd>")
	wantIn(t, "page", section(t, page, "list"), `data-item="`+id11+`" aria-current="true"`)

	// A link that names only the item (a notification's) lands on its
	// tab: the dismissed job is Done, with Move back to Open.
	j2 := e.id("job:j2")
	page = get(t, e.h, "/app/inbox?item="+j2)
	wantIn(t, "deep link", section(t, page, "list"), `<a class="tab on" aria-current="page" href="/app/inbox?tab=done"`, "dismissed job")
	wantIn(t, "deep link", section(t, page, "detail"), "Done · still waiting", `data-key="u" aria-keyshortcuts="u">Move back to Open <kbd>u</kbd>`,
		"· Done: dismissed on the old dashboard</li>", "<p class=\"label\">Open it from a terminal</p>", "claude attach j2")
	wantNotIn(t, "deep link", section(t, page, "detail"), "Snooze ▾", "Done <kbd>d</kbd>")

	// The background job: Go is disabled, with Go's reason shown.
	j1 := get(t, e.h, "/app/inbox?item="+e.id("job:j1")+"&fragment=detail")
	off := "Background session: no terminal tab to switch to. Open it with `claude attach j1`."
	wantIn(t, "job", j1, `<button type="button" class="btn primary" disabled aria-disabled="true" data-key="g" data-off="`+off+`" title="`+off+`">Go <kbd>g</kbd></button>`,
		`<p class="hint">`+off+`</p>`, `<span class="tag term-none"`)

	// A PR: Open on GitHub, its state and why.
	pr7 := get(t, e.h, "/app/inbox?item="+e.id("o/app#7")+"&fragment=detail")
	wantIn(t, "PR", pr7, `<span class="tag term-web" title="A pull request on GitHub">GitHub</span>`,
		`<form method="post" action="/app/inbox/go" data-pane="list detail" target="_blank" rel="noopener noreferrer" data-open-url="https://github.com/o/app/pull/7">`,
		">Open on GitHub <kbd>g</kbd></button>", `<p class="ipr">Open · CI green · mergeable</p>`,
		"<dt>Plan</dt><dd><a href=\"/app/plans/sharing\">Pivot share links</a></dd>", "<dt>Package</dt><dd>V1</dd>")

	// The search box keeps what matches.
	wantIn(t, "search", get(t, e.h, "/app/inbox?q=routes&fragment=list"), "routes agent", `href="/app/inbox?q=routes&amp;tab=open"`)
	wantNotIn(t, "search", get(t, e.h, "/app/inbox?q=routes&fragment=list"), "sharing copy")
	wantIn(t, "search", get(t, e.h, "/app/inbox?q=nothing-like-it&fragment=list"), "Nothing here matches")

	// An item that is gone, and a bad tab.
	wantIn(t, "gone", get(t, e.h, "/app/inbox?item=999&fragment=detail"), "That item is gone.")
	wantIn(t, "bad tab", get(t, e.h, "/app/inbox?tab=nope&fragment=list"), `<a class="tab on" aria-current="page" href="/app/inbox?tab=open"`)
}

// Done, then Undo: with the script (JSON) and without (303 and ?toast=).
func TestInboxDoneAndUndo(t *testing.T) {
	e := newInboxEnv(t)
	get(t, e.h, "/app/inbox")
	id := e.id("job:j1")
	ret := "/app/inbox?tab=open&item=" + id

	a := e.answer(e.post("done", url.Values{"item": {id}, "return": {ret}}, true), http.StatusOK)
	if a.Location != "/app/inbox?item="+id+"&tab=open" || a.Toast == nil || a.Toast.Text != "Marked done" || a.Toast.Undo == nil ||
		a.Toast.Undo.Action != "/app/inbox/undo" || a.Toast.Undo.Fields["item"] != id || a.Toast.Undo.Fields["move"] == "" {
		t.Fatalf("done answer %+v (toast %+v)", a, a.Toast)
	}
	if it := e.item("job:j1"); it.State != inbox.Done || it.Waiting() == false {
		t.Errorf("after Done: %+v", it)
	}
	// The detail now offers Move back to Open; the Open tab lost it.
	wantIn(t, "after done", get(t, e.h, ret+"&fragment=detail"), "Move back to Open <kbd>u</kbd>", "· Done</li>")
	wantNotIn(t, "after done", get(t, e.h, ret+"&fragment=list"), "sharing copy")
	wantIn(t, "menu", get(t, e.h, ret+"&fragment=menu"), `data-needs-count="3"`)

	// Undo, as the toast's button posts it.
	undo := url.Values{}
	for k, v := range a.Toast.Undo.Fields {
		undo.Set(k, v)
	}
	b := e.answer(e.post("undo", undo, true), http.StatusOK)
	if b.Toast == nil || b.Toast.Text != "Undone: back to Open" || b.Toast.Undo != nil {
		t.Errorf("undo answer %+v", b.Toast)
	}
	if it := e.item("job:j1"); it.State != inbox.Open {
		t.Errorf("after Undo: %s", it.State)
	}
	// Twice is refused, with the reason.
	c := e.answer(e.post("undo", undo, true), http.StatusConflict)
	if !strings.Contains(c.Error, "nothing to undo") {
		t.Errorf("second undo: %+v", c)
	}

	// Without the script: a 303 back with the toast and its Undo form.
	rec := e.post("done", url.Values{"item": {id}, "return": {ret}}, false)
	loc := rec.Header().Get("Location")
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "/app/inbox?item="+id+"&tab=open&toast=") {
		t.Fatalf("plain done = %d to %q", rec.Code, loc)
	}
	toasts := section(t, get(t, e.h, loc), "toasts")
	wantIn(t, "toast", toasts, "<span>Marked done</span>", `<form method="post" action="/app/inbox/undo" data-pane>`,
		`<input type="hidden" name="item" value="`+id+`">`, `<input type="hidden" name="move" value="`, `data-key="u">Undo</button>`)
	rec = e.post("undo", url.Values{"item": {id}, "move": {strconv.FormatInt(lastMove(t, e.st, e.item("job:j1").ID), 10)}, "return": {ret}}, false)
	if rec.Code != http.StatusSeeOther || e.item("job:j1").State != inbox.Open {
		t.Errorf("plain undo = %d, state %s", rec.Code, e.item("job:j1").State)
	}
	// A refusal without the script is an error toast.
	rec = e.post("open", url.Values{"item": {id}, "return": {ret}}, false)
	wantIn(t, "refusal", section(t, get(t, e.h, rec.Header().Get("Location")), "toasts"),
		`<div class="toast error" role="alert" data-toast>`, "It is Open already.")

	// No item, or an unknown one.
	if a := e.answer(e.post("done", url.Values{"return": {ret}}, true), http.StatusBadRequest); a.Error == "" {
		t.Error("a form without an item wasn't refused")
	}
	e.answer(e.post("done", url.Values{"item": {"999"}}, true), http.StatusNotFound)
}

func lastMove(t *testing.T, st *store.Store, id int64) int64 {
	t.Helper()
	moves, err := st.InboxMoves(context.Background(), id)
	must(t, err)
	return moves[len(moves)-1].ID
}

// Snooze, its timer, and Unsnooze.
func TestInboxSnooze(t *testing.T) {
	e := newInboxEnv(t)
	get(t, e.h, "/app/inbox")
	id := e.id("o/app#8")
	a := e.answer(e.post("snooze", url.Values{"item": {id}, "snooze": {"15m"}, "return": {"/app/inbox?tab=open"}}, true), http.StatusOK)
	if a.Toast == nil || a.Toast.Text != "Snoozed for 15 minutes" || a.Toast.Undo == nil {
		t.Fatalf("snooze answer %+v", a.Toast)
	}
	wantIn(t, "snoozed tab", get(t, e.h, "/app/inbox?tab=snoozed&fragment=list"), "app #8 · Red build", `<span class="iwait" title="snoozed until 2026-11-05T12:15:00Z">until 12:15</span>`)
	// As in the mockup, a snoozed item offers Unsnooze and Done, not Go or
	// Snooze.
	snoozed := get(t, e.h, "/app/inbox?item="+id+"&fragment=detail")
	wantIn(t, "detail", snoozed, "Snoozed until 12:15", "Unsnooze <kbd>u</kbd>", "Done <kbd>d</kbd>")
	wantNotIn(t, "detail", snoozed, "Snooze ▾", "Open on GitHub")
	// Its time comes: a pass (any read) brings it back to Open.
	e.clock = t0.Add(15 * time.Minute)
	get(t, e.h, "/app/inbox?fragment=summary")
	if it := e.item("o/app#8"); it.State != inbox.Open {
		t.Errorf("after the snooze: %s", it.State)
	}
	// Until it changes, then Unsnooze.
	e.answer(e.post("snooze", url.Values{"item": {id}, "snooze": {"change"}}, true), http.StatusOK)
	e.clock = t0.Add(48 * time.Hour)
	get(t, e.h, "/app/inbox?fragment=summary")
	if it := e.item("o/app#8"); it.State != inbox.Snoozed {
		t.Errorf("snoozed until it changes, after 2 days: %s", it.State)
	}
	b := e.answer(e.post("open", url.Values{"item": {id}}, true), http.StatusOK)
	if b.Toast == nil || b.Toast.Text != "Unsnoozed" || e.item("o/app#8").State != inbox.Open {
		t.Errorf("unsnooze: %+v, %s", b.Toast, e.item("o/app#8").State)
	}
	e.answer(e.post("snooze", url.Values{"item": {id}, "snooze": {"2d"}}, true), http.StatusConflict)
}

// Go: Go's rules (internal/term) first, then In progress; back to Open
// after the delay.
func TestInboxGo(t *testing.T) {
	e := newInboxEnv(t)
	get(t, e.h, "/app/inbox")
	id := e.id("pid:11")
	a := e.answer(e.post("go", url.Values{"item": {id}, "return": {"/app/inbox?tab=open&item=" + id}}, true), http.StatusOK)
	if a.Toast == nil || a.Toast.Text != "Switched to Terminal · moved to In progress" || a.Toast.Undo == nil {
		t.Fatalf("go answer %+v", a.Toast)
	}
	if len(e.g.keys) != 1 || e.g.keys[0] != "pid:11" {
		t.Errorf("Go asked for %v", e.g.keys)
	}
	it := e.item("pid:11")
	if it.State != inbox.InProgress || !it.DueAt.Equal(t0.Add(10*time.Minute)) {
		t.Errorf("after Go: %+v", it)
	}
	wantIn(t, "progress", get(t, e.h, "/app/inbox?tab=progress&item="+id), "In progress · back to Open at 12:10 if still waiting",
		"· Go: switched to Terminal</li>", `<span class="iwait" title="back to Open at 2026-11-05T12:10:00Z if still waiting">back at 12:10</span>`)
	e.clock = t0.Add(10 * time.Minute)
	get(t, e.h, "/app/inbox?fragment=summary")
	if it := e.item("pid:11"); it.State != inbox.Open {
		t.Errorf("after the delay: %s", it.State)
	}

	// Go refused by its rules (a background job) leaves the item alone.
	j1 := e.id("job:j1")
	b := e.answer(e.post("go", url.Values{"item": {j1}}, true), http.StatusUnprocessableEntity)
	if !strings.Contains(b.Error, "Background session") || e.item("job:j1").State != inbox.Open || len(e.g.keys) != 1 {
		t.Errorf("refused Go: %+v, %s, keys %v", b, e.item("job:j1").State, e.g.keys)
	}
	// Go's own refusal is passed on.
	e.g.err = refusal{status: http.StatusUnprocessableEntity}
	c := e.answer(e.post("go", url.Values{"item": {id}}, true), http.StatusUnprocessableEntity)
	if c.Error != "Go: refused for a reason" || e.item("pid:11").State != inbox.Open {
		t.Errorf("Go failing: %+v", c)
	}
	e.g.err = errors.New("osascript blew up")
	e.answer(e.post("go", url.Values{"item": {id}}, true), http.StatusInternalServerError)

	// A PR: no Go service needed; JSON answers with the toast (the page's
	// script opens the PR), a plain form goes to the PR.
	pr := e.id("o/app#7")
	d := e.answer(e.post("go", url.Values{"item": {pr}}, true), http.StatusOK)
	if d.Toast == nil || d.Toast.Text != "Opened on GitHub · moved to In progress" || e.item("o/app#7").State != inbox.InProgress {
		t.Errorf("PR go: %+v, %s", d.Toast, e.item("o/app#7").State)
	}
	rec := e.post("go", url.Values{"item": {pr}}, false)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "https://github.com/o/app/pull/7" {
		t.Errorf("plain PR go = %d to %q", rec.Code, rec.Header().Get("Location"))
	}
	// Go on a snoozed item is refused before anything switches.
	e.answer(e.post("snooze", url.Values{"item": {pr}, "snooze": {"1h"}}, true), http.StatusOK)
	e.answer(e.post("go", url.Values{"item": {pr}}, true), http.StatusConflict)
}

// The inbox follows the store: a wait that ends goes Done with why, and
// the badge follows.
func TestInboxFollowsStore(t *testing.T) {
	e := newInboxEnv(t)
	get(t, e.h, "/app/inbox")
	id := e.item("pid:11").ID
	must(t, e.st.UpsertSessions(context.Background(), store.Session{Key: "pid:11", Kind: "interactive", Name: "routes agent",
		Cwd: appRepo + "-wt/src", Repo: appRepo, WorktreePath: appRepo + "-wt", Branch: "feat", State: "working", LastSeenAt: t0}))
	wantIn(t, "menu", get(t, e.h, "/app/inbox?fragment=menu"), `data-needs-count="3"`)
	it := e.item("pid:11")
	if it.ID != id || it.State != inbox.Done || it.EndReason != "it stopped waiting" {
		t.Errorf("after the session worked again: %+v", it)
	}
	wantIn(t, "detail", get(t, e.h, "/app/inbox?item="+strconv.FormatInt(id, 10)+"&fragment=detail"), "Done · it stopped waiting", "· Done: it stopped waiting</li>")

	// The PR merges.
	must(t, e.st.UpsertPRs(context.Background(), store.PR{Repo: "o/app", Number: 7, Title: "Routes (V1)", URL: "https://github.com/o/app/pull/7",
		State: store.PRMerged, HeadBranch: "feat", MergedAt: t0, UpdatedAt: t0}))
	get(t, e.h, "/app/inbox?fragment=summary")
	if it := e.item("o/app#7"); it.State != inbox.Done || it.EndReason != "merged on GitHub" {
		t.Errorf("after the merge: %+v", it)
	}
}

// Monitoring (internal/monitor) decides which sessions open items; a
// review request opens one on any PR.
func TestInboxFollowsMonitoring(t *testing.T) {
	e := newMonitorEnv(t, monitor.ModeOptIn)
	_, err := e.st.SetReviewRequests(context.Background(), []string{"example-user"},
		[]store.PRKey{{Repo: aimRepo, Number: 30}}, t0.Add(-time.Hour))
	must(t, err)
	list := get(t, e.h, "/app/inbox?fragment=list")
	wantIn(t, "list", list, "watched one", "#30 ·", `<span>No plan</span>`)
	wantNotIn(t, "list", list, "waiting stranger", "headless run")
	wantIn(t, "menu", get(t, e.h, "/app/inbox?fragment=menu"), `data-needs-count="`+strconv.Itoa(2+aimSteps)+`"`)

	// Unwatched, its item ends.
	e.watchAPI("DELETE", "watch-1", http.StatusOK)
	wantNotIn(t, "after unwatch", get(t, e.h, "/app/inbox?fragment=list"), "watched one")
	wantIn(t, "after unwatch", get(t, e.h, "/app/inbox?tab=done&fragment=list"), "watched one")
}

func TestInboxAPI(t *testing.T) {
	e := newInboxEnv(t)
	var box api.Inbox
	decode(t, do(t, e.h, "GET", "/api/inbox", nil, nil), &box)
	if box.Tab != "open" || len(box.Items) != 4 || box.Counts["open"] != 4 || box.Counts["done"] != 1 || box.Counts["progress"] != 0 {
		t.Fatalf("GET /api/inbox = %+v", box)
	}
	// Grouped as the page: the plan's first.
	if box.Items[0].PR != "o/app#7" || box.Items[0].Kind != "merge" || box.Items[0].URL != "/app/inbox?item="+strconv.FormatInt(box.Items[0].ID, 10) || !box.Items[0].New {
		t.Errorf("first item %+v", box.Items[0])
	}
	decode(t, do(t, e.h, "GET", "/api/inbox?tab=all", nil, nil), &box)
	if len(box.Items) != 5 {
		t.Errorf("tab=all has %d items", len(box.Items))
	}
	if rec := do(t, e.h, "GET", "/api/inbox?tab=nope", nil, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("tab=nope = %d", rec.Code)
	}

	id := e.id("job:j1")
	token := map[string]string{"X-AIM-Token": testToken}
	rec := do(t, e.h, "POST", "/api/inbox/"+id, strings.NewReader(`{"action": "snooze", "snooze": "1h"}`), token)
	var res api.InboxActResult
	decode(t, rec, &res)
	if rec.Code != http.StatusOK || res.Item.State != "snoozed" || res.Move == nil || res.Message != "Snoozed for 1 hour" ||
		!res.Item.DueAt.Equal(t0.Add(time.Hour)) || len(res.Item.History) != 2 {
		t.Fatalf("POST snooze = %d %+v", rec.Code, res)
	}
	rec = do(t, e.h, "POST", "/api/inbox/"+id, strings.NewReader(`{"action": "undo", "move": `+strconv.FormatInt(res.Move.ID, 10)+`}`), token)
	decode(t, rec, &res)
	if rec.Code != http.StatusOK || res.Item.State != "open" {
		t.Errorf("POST undo = %d %+v", rec.Code, res)
	}
	var one api.InboxItem
	decode(t, do(t, e.h, "GET", "/api/inbox/"+id, nil, nil), &one)
	if len(one.History) != 3 || one.History[1].Text != "Snoozed for 1 hour" || one.History[1].By != "you" {
		t.Errorf("GET item history %+v", one.History)
	}
	for _, c := range []struct {
		target, body string
		hdr          map[string]string
		status       int
	}{
		{"/api/inbox/" + id, `{"action": "done"}`, nil, http.StatusUnauthorized},
		{"/api/inbox/" + id, `{"action": "fly"}`, token, http.StatusBadRequest},
		{"/api/inbox/" + id, `{"action": "open"}`, token, http.StatusConflict},
		{"/api/inbox/999", `{"action": "done"}`, token, http.StatusNotFound},
		{"/api/inbox/x", `{"action": "done"}`, token, http.StatusBadRequest},
	} {
		if rec := do(t, e.h, "POST", c.target, strings.NewReader(c.body), c.hdr); rec.Code != c.status {
			t.Errorf("POST %s %s = %d, want %d: %s", c.target, c.body, rec.Code, c.status, rec.Body)
		}
	}
	if rec := do(t, e.h, "GET", "/api/inbox/999", nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("GET /api/inbox/999 = %d", rec.Code)
	}
}

// The notifier's view: the Open items of sessions, by session row.
func TestInboxOpenSessions(t *testing.T) {
	e := newInboxEnv(t)
	open, err := e.srv.InboxOpenSessions(context.Background())
	must(t, err)
	if len(open) != 2 {
		t.Fatalf("open sessions %v, want pid:11 and job:j1", open)
	}
	e.answer(e.post("snooze", url.Values{"item": {e.id("job:j1")}, "snooze": {"15m"}}, true), http.StatusOK)
	open, err = e.srv.InboxOpenSessions(context.Background())
	must(t, err)
	if len(open) != 1 {
		t.Errorf("after a snooze: %v", open)
	}
	for _, item := range open {
		if item != e.item("pid:11").ID {
			t.Errorf("open %v, want pid:11's item", open)
		}
	}
}

// No pass before the first session poll: the store still holds the last
// run's sessions.
func TestInboxWaitsForTheFirstPoll(t *testing.T) {
	status := &fakeStatus{st: api.Status{Sources: []api.Source{{Name: api.SourceSessions}}}}
	srv, st := newApp(t, func(o *Options) { o.Status = status })
	if res, err := srv.SyncInbox(context.Background()); err != nil || res.Changed() {
		t.Errorf("a pass before the first poll: %+v, %v", res, err)
	}
	if items, _ := st.ListInbox(context.Background(), store.InboxFilter{}); len(items) != 0 {
		t.Errorf("items before the first poll: %d", len(items))
	}
	status.st.Sources[0].LastOK = t0
	if res, err := srv.SyncInbox(context.Background()); err != nil || res.Created != 5 {
		t.Errorf("the first pass: %+v, %v", res, err)
	}
}

// RunInbox runs a pass at once and stops with its context.
func TestRunInbox(t *testing.T) {
	srv, st := newApp(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- srv.RunInbox(ctx, time.Hour) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if items, _ := st.ListInbox(context.Background(), store.InboxFilter{}); len(items) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("RunInbox ran no pass")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("RunInbox = %v", err)
	}
}

// Untrusted names are escaped, and the page needs no inline code.
func TestInboxEscapesAndNoInlineCode(t *testing.T) {
	e := newInboxEnv(t)
	evil := `<script>alert(1)</script><img src=x onerror=alert(2)>`
	must(t, e.st.UpsertSessions(context.Background(), store.Session{Key: "pid:66", PID: 66, Kind: "interactive", Name: evil,
		Cwd: "/w/" + evil, State: "needs_you", StateReason: "permission prompt: Bash: " + evil, NeedsYouSince: t0, LastSeenAt: t0}))
	get(t, e.h, "/app/inbox")
	id := e.id("pid:66")
	rec := e.post("done", url.Values{"item": {id}, "return": {"/app/inbox?item=" + id}}, false)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("done = %d", rec.Code)
	}
	for _, p := range []string{"/app/inbox", "/app/inbox?item=" + id, "/app/inbox?tab=done&item=" + id, rec.Header().Get("Location")} {
		wantNotIn(t, p, get(t, e.h, p), "<script>alert", "<img src=x")
	}

	// Every state's detail pane, and a toast, without inline code (in a
	// world without the evil names, whose escaped text the check matches).
	f := newInboxEnv(t)
	get(t, f.h, "/app/inbox")
	rec = f.post("done", url.Values{"item": {f.id("job:j1")}, "return": {"/app/inbox?tab=open"}}, false)
	pages := []string{rec.Header().Get("Location")}
	for _, ref := range []string{"pid:11", "job:j1", "job:j2", "o/app#7", "o/app#8"} {
		pages = append(pages, "/app/inbox?item="+f.id(ref))
	}
	f.answer(f.post("go", url.Values{"item": {f.id("pid:11")}}, true), http.StatusOK)
	f.answer(f.post("snooze", url.Values{"item": {f.id("o/app#8")}, "snooze": {"change"}}, true), http.StatusOK)
	for _, p := range pages {
		if m := inlineRE.FindString(get(t, f.h, p)); m != "" {
			t.Errorf("%s: inline code %q", p, m)
		}
	}
}

// A session's wait is a package's only when it is that package's agent's:
// a lead's own prompt is on its plan with no package, though the
// lead runs D1's subagent; a wait carrying D1's agent id (the mod's agentId
// on tool.check) is D1's; a subagent no package links is on the plan; a
// session a package links itself (C6's) is that package's.
func TestInboxWaitPackage(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name, key, reason, pkg, title string
	}{
		{"the lead's own prompt", "pid:43", "permission prompt: Bash: git push", "", "aim lead"},
		{"D1's subagent", "pid:43", "agent agent-d1: permission prompt: Bash: rm -r build", "D1", "Agent · D1"},
		{"a subagent on no package", "pid:43", "agent agent-zz: question: Which one?", "", "Agent · agent-zz"},
		{"C6's own session", "pid:42", "question: AskUserQuestion", "C6", "c6 agent"},
	} {
		e := newBoardEnv(t)
		e.monitored(monitor.ModeOptIn)
		e.registerAim()
		e.api("POST", "/api/plans", api.RegisterRequest{Path: e.aimPlan, Repos: []string{aimRepo}, SessionID: "lead-1"}, http.StatusOK)
		row, err := e.st.GetSession(ctx, c.key)
		must(t, err)
		row.State, row.StateReason, row.NeedsYouSince = needsYou, c.reason, t0.Add(-time.Minute)
		must(t, e.st.UpsertSessions(ctx, row))

		var in api.Inbox
		decode(t, do(t, e.h, "GET", "/api/inbox?tab=open", nil, nil), &in)
		var got *api.InboxItem
		for i := range in.Items {
			if in.Items[i].SessionKey == c.key {
				got = &in.Items[i]
			}
		}
		if got == nil {
			t.Errorf("%s: no Open item for %s in %+v", c.name, c.key, in.Items)
			continue
		}
		if got.Plan != "aim" || got.Pkg != c.pkg || got.Title != c.title {
			t.Errorf("%s: plan %q, package %q, title %q; want aim, %q, %q", c.name, got.Plan, got.Pkg, got.Title, c.pkg, c.title)
		}
	}
}
