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
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// The settings' world is the inbox's (newInboxEnv: seedDash's store and
// appPlans), with appPlans registered in the store too, so a plan can
// have an override of who merges.
func newSettingsEnv(t *testing.T) *inboxEnv {
	t.Helper()
	e := newInboxEnv(t)
	for _, p := range appPlans.plans {
		_, err := e.st.InsertPlan(context.Background(), store.Plan{Slug: p.Slug, Title: p.Title})
		must(t, err)
	}
	return e
}

// postSettings sends a settings form, as the page's script does (asJSON)
// or as a plain form.
func (e *inboxEnv) postSettings(path string, vals url.Values, asJSON bool) *httptest.ResponseRecorder {
	e.t.Helper()
	vals.Set(CSRFField, e.srv.CSRF().Token())
	hdr := map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": "null", "Sec-Fetch-Site": "same-origin"}
	if asJSON {
		hdr["Accept"] = "application/json"
	}
	return do(e.t, e.h, "POST", path, strings.NewReader(vals.Encode()), hdr)
}

// set changes one setting through the page's form, as the script does.
func (e *inboxEnv) set(key, value string) appAnswer {
	e.t.Helper()
	return e.answer(e.postSettings("/app/settings/set", url.Values{"key": {key}, "value": {value}, "return": {"/app/settings"}}, true), http.StatusOK)
}

func TestSettingsPage(t *testing.T) {
	e := newSettingsEnv(t)
	page := get(t, e.h, "/app/settings")
	wantIn(t, "page", page,
		"<h1>Settings</h1>", "<p>Alerts, inbox and plans</p>",
		`href="/static/app-settings.css?v=`+assetVersion+`"`,
		`<div id="content" class="content" data-live="plans">`,
	)
	// The menu carries the shell's flags; the page has no script.
	wantIn(t, "menu", section(t, page, "menu"), `<span hidden data-flags data-tab-count="on" data-chime="on" data-keys="on" data-theme="system"></span>`)
	wantNotIn(t, "page", page, "app-settings.js", "data-settings-flags")
	// The content, as the script fetches it, is the page's.
	content := get(t, e.h, "/app/settings?fragment=content")
	if !strings.Contains(noTokens(page), strings.TrimSpace(noTokens(content))) {
		t.Errorf("the content fragment isn't the page's:\n%s", content)
	}
	wantIn(t, "content", content,
		// The cards.
		`<h2 id="set-alerts">Alerts</h2>`, `<h2 id="set-inbox">Needs you inbox</h2>`, `<h2 id="set-merges">Who merges each plan's PRs</h2>`,
		`<h2 id="set-monitoring">Monitoring</h2>`, `<h2 id="set-shortcuts">Shortcuts</h2>`,
		// A switch is a form posting the other value.
		`<form method="post" action="/app/settings/set" data-pane="content">`,
		`<input type="hidden" name="key" value="tab_count"><input type="hidden" name="value" value="off">`,
		`<button type="submit" id="sw-tab_count" class="switch" role="switch" aria-checked="true" aria-labelledby="sw-tab_count-title" aria-describedby="sw-tab_count-desc"><span class="knob"></span></button>`,
		`<div class="trow-title" id="sw-tab_count-title">Count in the tab title</div>`,
		`id="sw-notifications" class="switch" role="switch" aria-checked="true"`,
		`<input type="hidden" name="key" value="chime"><input type="hidden" name="value" value="off">`,
		`id="sw-chime" class="switch" role="switch" aria-checked="true"`,
		`id="sw-highlight" class="switch" role="switch" aria-checked="true"`,
		`id="sw-shortcuts" class="switch" role="switch" aria-checked="true"`,
		// The timings are segmented controls, the current one pressed.
		`<form method="post" action="/app/settings/set" data-pane="content" class="seg" role="group" aria-labelledby="seg-back_to_open_minutes-title">`,
		`<button type="submit" id="seg-back_to_open_minutes-5" name="value" value="5" aria-pressed="false">5 min</button>`,
		`<button type="submit" id="seg-back_to_open_minutes-10" name="value" value="10" class="on" aria-pressed="true">10 min</button>`,
		`<button type="submit" id="seg-keep_done_days-30" name="value" value="30" class="on" aria-pressed="true">30 days</button>`,
		`<button type="submit" id="seg-keep_done_days-90" name="value" value="90" aria-pressed="false">90 days</button>`,
		"15 minutes · 1 hour · until tomorrow 9am · until it changes",
		// Who merges: each plan, from its line.
		`<div class="trow-title" id="mg-0-title">Pivot share links</div>`,
		`<div class="trow-title" id="mg-1-title">ai-manager — build plan</div>`,
		"From the plan&#39;s Merges line: you merge.",
		`<input type="hidden" name="plan" value="sharing">`,
		`<button type="submit" id="mg-0-you" name="merges" value="you" class="on" aria-pressed="true">You</button>`,
		`<button type="submit" id="mg-0-lead" name="merges" value="lead" aria-pressed="false">Lead session</button>`,
		// Monitoring, from the spec.
		"Monitored automatically: a lead that registers a plan, the agents it spawns, and agents on a registered plan's packages.",
		"<code>/aim watch</code>", "<code>/aim unwatch</code>",
		`data-open="help"`,
		// Each form carries the token and comes back here.
		`<input type="hidden" name="csrf_token" value="`, `<input type="hidden" name="return" value="/app/settings">`,
	)
	wantNotIn(t, "content", content, "Override", "Reset to the plan", "config.toml turns them off", `sessions.monitor = &#34;all&#34;`, "Built by W8")
}

// Without a settings store (plainStore, app_overview_test.go) the page
// shows the defaults and can't change them.
func TestSettingsWithoutStore(t *testing.T) {
	st := newTestStore(t)
	seedDash(t, st)
	srv := newDash(t, plainStore{st}, func(o *Options) { o.Plans = appPlans })
	h := srv.Handler()
	wantIn(t, "page", get(t, h, "/app/settings"), "these are the defaults, and they can&#39;t be changed here", `aria-checked="true"`)
	rec := do(t, h, "POST", "/app/settings/set", strings.NewReader(url.Values{"key": {"chime"}, "value": {"on"}, CSRFField: {srv.CSRF().Token()}}.Encode()),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Accept": "application/json", "Origin": "null", "Sec-Fetch-Site": "same-origin"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("a change without a store = %d %s", rec.Code, rec.Body)
	}
}

// The form: JSON for the script, a 303 with the toast for a plain form,
// each with an Undo that puts the old value back.
func TestSettingsForms(t *testing.T) {
	e := newSettingsEnv(t)

	a := e.set(store.SettingChime, store.Off)
	if a.Location != "/app/settings" || a.Toast == nil || a.Toast.Text != "Chime: off" || a.Toast.Undo == nil {
		t.Fatalf("JSON answer %+v (toast %+v)", a, a.Toast)
	}
	want := map[string]string{"key": "chime", "value": "on", "return": "/app/settings", "undo": "1"}
	if u := a.Toast.Undo; u.Action != "/app/settings/set" || len(u.Fields) != len(want) {
		t.Errorf("undo %+v", u)
	} else {
		for k, v := range want {
			if u.Fields[k] != v {
				t.Errorf("undo field %s = %q, want %q", k, u.Fields[k], v)
			}
		}
	}
	if set := e.srv.settings(context.Background()); set.Chime {
		t.Error("the chime is still on")
	}
	// The Undo's answer carries no Undo of its own.
	b := e.answer(e.postSettings("/app/settings/set", url.Values{"key": {"chime"}, "value": {"on"}, "return": {"/app/settings"}, "undo": {"1"}}, true), http.StatusOK)
	if b.Toast == nil || b.Toast.Text != "Chime: on" || b.Toast.Undo != nil || !e.srv.settings(context.Background()).Chime {
		t.Errorf("undo answer %+v", b.Toast)
	}
	// The timings say what they are now.
	if c := e.set(store.SettingBackToOpen, "30"); c.Toast.Text != "After Go, back to Open after 30 minutes" || c.Toast.Undo.Fields["value"] != "10" {
		t.Errorf("back to open answer %+v", c.Toast)
	}
	if c := e.set(store.SettingKeepDone, "7"); c.Toast.Text != "Done items are kept 7 days" {
		t.Errorf("keep done answer %+v", c.Toast)
	}
	// Setting what it is already offers nothing to undo.
	if c := e.set(store.SettingKeepDone, "7"); c.Toast.Undo != nil {
		t.Errorf("an unchanged setting has an Undo: %+v", c.Toast.Undo)
	}

	// Without the script: a 303 back to the page, which shows the toast
	// with its Undo as a form, and the setting as it is now.
	rec := e.postSettings("/app/settings/set", url.Values{"key": {"highlight"}, "value": {"off"}, "return": {"/app/settings"}}, false)
	loc := rec.Header().Get("Location")
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "/app/settings?toast=") {
		t.Fatalf("form post = %d to %q", rec.Code, loc)
	}
	page := get(t, e.h, loc)
	wantIn(t, "toast", section(t, page, "toasts"), "<span>Highlight new items: off</span>",
		`<form method="post" action="/app/settings/set" data-pane>`, `<input type="hidden" name="value" value="on">`, `<input type="hidden" name="undo" value="1">`)
	wantIn(t, "page", page, `id="sw-highlight" class="switch" role="switch" aria-checked="false"`,
		`<input type="hidden" name="key" value="highlight"><input type="hidden" name="value" value="on">`)

	// Refusals: a bad value or key, as JSON and as an error toast.
	for _, vals := range []url.Values{{"key": {"back_to_open_minutes"}, "value": {"15"}}, {"key": {"volume"}, "value": {"11"}}, {}} {
		c := e.answer(e.postSettings("/app/settings/set", vals, true), http.StatusBadRequest)
		if c.Error == "" || c.Toast != nil {
			t.Errorf("refusal of %v: %+v", vals, c)
		}
	}
	rec = e.postSettings("/app/settings/set", url.Values{"key": {"back_to_open_minutes"}, "value": {"15"}, "return": {"/app/settings"}}, false)
	wantIn(t, "refused", section(t, get(t, e.h, rec.Header().Get("Location")), "toasts"),
		`<div class="toast error" role="alert" data-toast>`, "<span>Can&#39;t save that: back_to_open_minutes is 5, 10 or 30 minutes.</span>")
	if got := e.srv.settings(context.Background()).BackToOpenMinutes; got != 30 {
		t.Errorf("a refused change stored %d", got)
	}

	// Every write needs the page's CSRF token.
	rec = do(t, e.h, "POST", "/app/settings/set", strings.NewReader("key=chime&value=off"),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": "null", "Sec-Fetch-Site": "same-origin"})
	if rec.Code != http.StatusForbidden || !e.srv.settings(context.Background()).Chime {
		t.Errorf("a post without the token = %d", rec.Code)
	}
}

// The count in the tab title: on by default, off in the title, the
// preview and <html>.
func TestSettingTabCount(t *testing.T) {
	e := newSettingsEnv(t)
	page := get(t, e.h, "/app/inbox")
	wantIn(t, "default", page, "<title>(4) aim</title>", "Tab title: <span data-tab-title>(4) aim</span>")
	wantNotIn(t, "default", page, `data-tab-count="off"`)

	e.set(store.SettingTabCount, store.Off)
	for _, p := range []string{"/app/inbox", "/app/settings", "/app/"} {
		page := get(t, e.h, p)
		wantIn(t, p, page, "<title>aim</title>", `<html lang="en" data-assets="`)
		wantIn(t, p, page[:strings.Index(page, "<head>")], `data-tab-count="off"`)
		// The preview is there, hidden, for the script to show again.
		wantIn(t, p, page, `title="What the browser tab shows while this page is open" hidden>`)
	}
	// Every open tab's menu carries it.
	wantIn(t, "flags", get(t, e.h, "/app/inbox?fragment=menu"), `<span hidden data-flags data-tab-count="off" data-chime="on" data-keys="on" data-theme="system">`)
}

// The chime: on by default, so <html> lets the shell ring and the inbox's
// new permission prompts and questions carry data-chime; off, neither.
func TestSettingChime(t *testing.T) {
	e := newSettingsEnv(t)
	// A question too.
	must(t, e.st.UpsertSessions(context.Background(), store.Session{Key: "pid:14", Kind: "interactive", Name: "asking", Cwd: webRepo, Repo: webRepo,
		State: "needs_you", StateReason: "question: Which route?", NeedsYouSince: t0.Add(-time.Minute), LastSeenAt: t0}))
	page := get(t, e.h, "/app/inbox")
	wantIn(t, "default", page[:strings.Index(page, "<head>")], `data-chime="on"`)
	list := section(t, page, "list")
	wantIn(t, "default", list, `data-chime="inbox-`+e.id("pid:11")+`"`, `data-chime="inbox-`+e.id("pid:14")+`"`)
	// Not for a reply or a PR, and not once the item is opened.
	wantNotIn(t, "default", list, `data-chime="inbox-`+e.id("job:j1")+`"`, `data-chime="inbox-`+e.id("o/app#7")+`"`)
	get(t, e.h, "/app/inbox?item="+e.id("pid:14"))
	wantNotIn(t, "opened", get(t, e.h, "/app/inbox?fragment=list"), `data-chime="inbox-`+e.id("pid:14")+`"`)

	// Off, <html> says nothing and the rows lose it at their next refresh.
	e.set(store.SettingChime, store.Off)
	page = get(t, e.h, "/app/inbox")
	wantNotIn(t, "off", page[:strings.Index(page, "<head>")], `data-chime="on"`)
	wantNotIn(t, "off", get(t, e.h, "/app/inbox?fragment=list"), "data-chime=")
	wantIn(t, "flags", get(t, e.h, "/app/sessions?fragment=menu"), `<span hidden data-flags data-tab-count="on" data-chime="off" data-keys="on" data-theme="system">`)

	// On again, it comes back.
	e.set(store.SettingChime, store.On)
	wantIn(t, "on again", get(t, e.h, "/app/inbox?fragment=list"), `data-chime="inbox-`+e.id("pid:11")+`"`)
}

// Highlight new items: on by default; off, no row is "new", while the
// chime (on by default) still marks them.
func TestSettingHighlight(t *testing.T) {
	e := newSettingsEnv(t)
	list := get(t, e.h, "/app/inbox?fragment=list")
	id := e.id("pid:11")
	wantIn(t, "default", list, `<a class="irow kind-permission new"`, "New items stay highlighted until you open them.")

	e.set(store.SettingHighlight, store.Off)
	list = get(t, e.h, "/app/inbox?fragment=list")
	wantIn(t, "off", list, `<a class="irow kind-permission" href="/app/inbox?item=`+id+`&amp;tab=open" data-pane="detail" data-item="`+id+`" data-chime="inbox-`+id+`" data-search="feat">`)
	wantNotIn(t, "off", list, " new\"", "New items stay highlighted")
	// The API still says which items you haven't opened.
	var got api.Inbox
	decode(t, do(t, e.h, "GET", "/api/inbox", nil, nil), &got)
	for _, it := range got.Items {
		if it.ID == e.item("pid:11").ID && !it.New {
			t.Error("GET /api/inbox: new follows the highlight")
		}
	}
}

// Keyboard shortcuts: on by default; off, <html data-keys="off">.
func TestSettingShortcuts(t *testing.T) {
	e := newSettingsEnv(t)
	page := get(t, e.h, "/app/inbox")
	wantNotIn(t, "default", page[:strings.Index(page, "<head>")], `data-keys="off"`)
	// Both hints are there for the script; the one that applies shows.
	wantIn(t, "help", section(t, page, "help"), `<p class="hint" data-keys-hint="on">They work anywhere outside a text field.</p>`,
		`<p class="hint" data-keys-hint="off" hidden>They are off; turn them on in Settings.</p>`)
	e.set(store.SettingShortcuts, store.Off)
	page = get(t, e.h, "/app/inbox")
	wantIn(t, "off", page[:strings.Index(page, "<head>")], `data-keys="off"`)
	wantIn(t, "help", section(t, page, "help"), `<p class="hint" data-keys-hint="on" hidden>They work anywhere outside a text field.</p>`,
		`<p class="hint" data-keys-hint="off">They are off; turn them on in Settings.</p>`)
	wantIn(t, "flags", get(t, e.h, "/app/inbox?fragment=menu"), `data-keys="off" data-theme="system"></span>`)
}

// The back-to-Open delay is the one Go uses.
func TestSettingBackToOpen(t *testing.T) {
	e := newSettingsEnv(t)
	if got := e.srv.inboxSettings(context.Background()).BackToOpen; got != 10*time.Minute {
		t.Errorf("default delay %v", got)
	}
	e.set(store.SettingBackToOpen, "5")
	if got := e.srv.inboxSettings(context.Background()).BackToOpen; got != 5*time.Minute {
		t.Errorf("delay %v after choosing 5", got)
	}
	get(t, e.h, "/app/inbox")
	id := e.id("pid:11")
	e.answer(e.post("go", url.Values{"item": {id}}, true), http.StatusOK)
	if it := e.item("pid:11"); it.State != inbox.InProgress || !it.DueAt.Equal(t0.Add(5*time.Minute)) {
		t.Errorf("after Go: %s due %v", it.State, it.DueAt)
	}
	e.clock = t0.Add(5 * time.Minute)
	get(t, e.h, "/app/inbox?fragment=summary")
	if it := e.item("pid:11"); it.State != inbox.Open {
		t.Errorf("after 5 minutes: %s", it.State)
	}
}

// The Done history is how long the inbox keeps Done items.
func TestSettingKeepDone(t *testing.T) {
	e := newSettingsEnv(t)
	ctx := context.Background()
	get(t, e.h, "/app/inbox")
	id := e.item("pid:11").ID
	// It stops waiting: Done, its episode ended now.
	must(t, e.st.UpsertSessions(ctx, store.Session{Key: "pid:11", Kind: "interactive", Name: "routes agent",
		Cwd: appRepo + "-wt/src", Repo: appRepo, WorktreePath: appRepo + "-wt", Branch: "feat", State: "working", LastSeenAt: t0}))
	get(t, e.h, "/app/inbox?fragment=summary")
	if it := e.item("pid:11"); it.State != inbox.Done {
		t.Fatalf("after it stopped waiting: %s", it.State)
	}
	gone := func() bool {
		t.Helper()
		get(t, e.h, "/app/inbox?fragment=summary") // a sync pass
		_, err := e.st.InboxItem(ctx, id)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			t.Fatal(err)
		}
		return err != nil
	}
	// Eight days on: kept with the default 30 days, and with 7 days gone.
	e.clock = t0.Add(8 * 24 * time.Hour)
	if gone() {
		t.Fatal("pruned after 8 days with a 30-day history")
	}
	wantIn(t, "footer", get(t, e.h, "/app/inbox?tab=done&fragment=list"), "kept for 30 days")
	e.set(store.SettingKeepDone, "7")
	if got := e.srv.inboxSettings(ctx).KeepDone; got != 7*24*time.Hour {
		t.Errorf("KeepDone %v", got)
	}
	if !gone() {
		t.Error("kept after 8 days with a 7-day history")
	}
	wantIn(t, "footer", get(t, e.h, "/app/inbox?tab=done&fragment=list"), "kept for 7 days")
	rec := do(t, e.h, "GET", "/api/inbox/"+strconv.FormatInt(id, 10), nil, nil)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "kept for 7 days") {
		t.Errorf("GET a pruned item = %d %s", rec.Code, rec.Body)
	}
}

// newMergesEnv is the settings' world with monitoring: sharing's
// board links o/app#7 (ready to merge) to V1, and says you merge; the
// overrides come from the store, as aimd wires them.
func newMergesEnv(t *testing.T) *inboxEnv {
	t.Helper()
	e := &inboxEnv{t: t, clock: t0, g: &stubGo{}}
	boards := fakePlans{
		plans: []api.Plan{
			{Slug: "sharing", Title: "Pivot share links", Merges: "you", Total: 15, Counts: map[string]int{"done": 4},
				Packages: []api.Package{{ID: "V1", Links: []api.Link{{Target: api.Target{Kind: store.LinkPR, Ref: "o/app#7"}}}}}},
			{Slug: "aim", Title: "ai-manager — build plan", Merges: "lead", Total: 32, Counts: map[string]int{"done": 25}},
		},
		sums: appPlans.sums,
	}
	e.srv, e.st = newApp(t, func(o *Options) {
		o.Plans = boards
		o.Now = func() time.Time { return e.clock }
	})
	e.srv.opts.Monitor = &monitor.Source{Mode: monitor.ModeOptIn, Store: e.st,
		Plans: func(ctx context.Context) ([]api.Plan, error) {
			list, err := boards.Plans(ctx)
			return list.Plans, err
		},
		RepoNames: fakeRepos{appRepo: "o/app", webRepo: "o/web"}.RepoNames,
		Merges:    e.st.PlanMerges,
	}
	e.h = e.srv.Handler()
	// #7 is ready to merge on a branch no session works on.
	must(t, e.st.UpsertPRs(context.Background(), store.PR{Repo: "o/app", Number: 7, Title: "Routes (V1)", URL: "https://github.com/o/app/pull/7",
		State: "OPEN", HeadBranch: "share/v1", MergeStateStatus: "CLEAN", ChecksState: "SUCCESS", UpdatedAt: t0.Add(-2 * time.Hour)}))
	for _, p := range boards.plans {
		_, err := e.st.InsertPlan(context.Background(), store.Plan{Slug: p.Slug, Title: p.Title})
		must(t, err)
	}
	return e
}

// prJSON is one PR of GET /api/prs.
func (e *inboxEnv) prJSON(repo string, n int) api.PR {
	e.t.Helper()
	var out api.PRs
	decode(e.t, do(e.t, e.h, "GET", "/api/prs", nil, nil), &out)
	for _, p := range out.PRs {
		if p.Repo == repo && p.Number == n {
			return p
		}
	}
	e.t.Fatalf("no PR %s#%d", repo, n)
	return api.PR{}
}

// Who merges: an override replaces the plan's line before the PR rule
// reads it, so a ready PR goes from "needs you" to "waiting on lead", and
// back with the reset.
func TestSettingMergesOverride(t *testing.T) {
	e := newMergesEnv(t)
	if p := e.prJSON("o/app", 7); p.NeedsYou != "ready_to_merge" || p.WaitingOn != "you" || p.Merges != "you" {
		t.Fatalf("the plan's line: %+v", p)
	}
	get(t, e.h, "/app/inbox")
	first := e.item("o/app#7")
	if first.State != inbox.Open || first.Kind != inbox.KindMerge {
		t.Fatalf("its item: %+v", first)
	}
	content := get(t, e.h, "/app/settings?fragment=content")
	wantIn(t, "lines", content, "From the plan&#39;s Merges line: you merge.", "From the plan&#39;s Merges line: its lead session merges.",
		`id="mg-1-lead" name="merges" value="lead" class="on" aria-pressed="true"`)

	// The lead merges it now.
	a := e.answer(e.postSettings("/app/settings/merges", url.Values{"plan": {"sharing"}, "merges": {"lead"}, "return": {"/app/settings"}}, true), http.StatusOK)
	if a.Toast == nil || a.Toast.Text != "Pivot share links: its lead session merges its PRs (an override)" ||
		a.Toast.Undo == nil || a.Toast.Undo.Action != "/app/settings/merges" || a.Toast.Undo.Fields["merges"] != "" || a.Toast.Undo.Fields["plan"] != "sharing" {
		t.Fatalf("override answer %+v", a.Toast)
	}
	if p := e.prJSON("o/app", 7); p.NeedsYou != "" || p.WaitingOn != "lead" || p.Merges != "lead" {
		t.Errorf("overridden: %+v", p)
	}
	if it := e.item("o/app#7"); it.ID != first.ID || it.State != inbox.Done {
		t.Errorf("its item after the override: %+v", it)
	}
	content = get(t, e.h, "/app/settings?fragment=content")
	wantIn(t, "override", content,
		`Pivot share links <span class="tag set-override">Override</span>`, "Overridden here. The plan&#39;s Merges line says you merge.",
		`id="mg-0-lead" name="merges" value="lead" class="on" aria-pressed="true"`,
		`<input type="hidden" name="plan" value="sharing"><input type="hidden" name="merges" value="">`,
		`<button type="submit" id="mg-0-reset" class="btn sm">Reset to the plan's line</button>`)
	var got api.Settings
	decode(t, do(t, e.h, "GET", "/api/settings", nil, nil), &got)
	if len(got.Merges) != 2 || got.Merges[0] != (api.PlanMerges{Plan: "sharing", Title: "Pivot share links", Merges: "lead", Line: "you", Override: true}) {
		t.Errorf("GET /api/settings merges %+v", got.Merges)
	}

	// Reset to the plan's line, without the script: you merge it again,
	// a new episode.
	rec := e.postSettings("/app/settings/merges", url.Values{"plan": {"sharing"}, "merges": {""}, "return": {"/app/settings"}}, false)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("reset = %d", rec.Code)
	}
	wantIn(t, "reset toast", section(t, get(t, e.h, rec.Header().Get("Location")), "toasts"),
		"<span>Pivot share links: back to the plan&#39;s line, you merge its PRs</span>", `<input type="hidden" name="merges" value="lead">`)
	if p := e.prJSON("o/app", 7); p.NeedsYou != "ready_to_merge" || p.WaitingOn != "you" {
		t.Errorf("after the reset: %+v", p)
	}
	if it := e.item("o/app#7"); it.ID == first.ID || it.State != inbox.Open {
		t.Errorf("its item after the reset: %+v", it)
	}

	// A lead-merged plan overridden to you; an unknown plan or value is
	// refused.
	e.answer(e.postSettings("/app/settings/merges", url.Values{"plan": {"AIM"}, "merges": {"you"}}, true), http.StatusOK)
	if over, _ := e.st.PlanMerges(context.Background()); over["aim"] != "you" {
		t.Errorf("overrides %v", over)
	}
	e.answer(e.postSettings("/app/settings/merges", url.Values{"plan": {"nope"}, "merges": {"you"}}, true), http.StatusNotFound)
	e.answer(e.postSettings("/app/settings/merges", url.Values{"plan": {"aim"}, "merges": {"agent"}}, true), http.StatusBadRequest)
}

// The plan page and a PR's detail say who merges with Settings' override
// where one is set, and label it so: "Lead session (override)", not
// "(plan setting)".
func TestMergesOverrideShown(t *testing.T) {
	e := newMergesEnv(t)
	merges := func(page string) string {
		t.Helper()
		_, after, ok := strings.Cut(page, "<dt>Merges</dt><dd>")
		if !ok {
			t.Fatalf("no Merges in %s", page)
		}
		v, _, _ := strings.Cut(after, "</dd>")
		return v
	}
	sharing := func(q string) string { return merges(get(t, e.h, "/app/plans/sharing?"+q+"fragment=detail")) }
	pr := func() string { return merges(get(t, e.h, "/app/prs?pr=o/app%237&fragment=detail")) }
	const youLine = "you (Merges: you): a ready PR waits on you"
	for name, got := range map[string]string{"plan": sharing(""), "package": sharing("pkg=V1&")} {
		if got != youLine {
			t.Errorf("%s, the plan's line: %q", name, got)
		}
	}
	if got := pr(); got != "You (plan setting)" {
		t.Errorf("PR, the plan's line: %q", got)
	}

	e.answer(e.postSettings("/app/settings/merges", url.Values{"plan": {"sharing"}, "merges": {"lead"}, "return": {"/app/settings"}}, true), http.StatusOK)
	const leadOver = "the lead session (override in Settings; the plan says Merges: you): a ready PR waits on the lead, not on you"
	for name, got := range map[string]string{"plan": sharing(""), "package": sharing("pkg=V1&")} {
		if got != leadOver {
			t.Errorf("%s, overridden: %q", name, got)
		}
	}
	if got := pr(); got != "Lead session (override)" {
		t.Errorf("PR, overridden: %q", got)
	}

	// aim's line says its lead merges; overridden to you.
	if got := merges(get(t, e.h, "/app/plans/aim?fragment=detail")); got != "the lead session (Merges: lead): a ready PR waits on the lead, not on you" {
		t.Errorf("aim, its line: %q", got)
	}
	e.answer(e.postSettings("/app/settings/merges", url.Values{"plan": {"aim"}, "merges": {"you"}}, true), http.StatusOK)
	if got := merges(get(t, e.h, "/app/plans/aim?fragment=detail")); got != "you (override in Settings; the plan says Merges: lead): a ready PR waits on you" {
		t.Errorf("aim, overridden: %q", got)
	}

	// Reset: the plan's line again.
	e.answer(e.postSettings("/app/settings/merges", url.Values{"plan": {"sharing"}, "merges": {""}}, true), http.StatusOK)
	if got := sharing(""); got != youLine {
		t.Errorf("plan, reset: %q", got)
	}
	if got := pr(); got != "You (plan setting)" {
		t.Errorf("PR, reset: %q", got)
	}
}

func TestSettingsAPI(t *testing.T) {
	e := newSettingsEnv(t)
	var got api.Settings
	decode(t, do(t, e.h, "GET", "/api/settings", nil, nil), &got)
	want := api.Settings{TabCount: true, Notifications: true, Chime: true, Highlight: true, Shortcuts: true,
		BackToOpenMinutes: 10, KeepDoneDays: 30, Merges: []api.PlanMerges{
			{Plan: "sharing", Title: "Pivot share links", Merges: "you", Line: "you"},
			{Plan: "aim", Title: "ai-manager — build plan", Merges: "you", Line: "you"},
		}}
	if got.TabCount != want.TabCount || !got.Chime || got.BackToOpenMinutes != 10 || got.KeepDoneDays != 30 || len(got.Merges) != 2 || got.Merges[1] != want.Merges[1] {
		t.Errorf("defaults %+v", got)
	}

	post := func(body string, hdr map[string]string) *httptest.ResponseRecorder {
		return do(t, e.h, "POST", "/api/settings", strings.NewReader(body), hdr)
	}
	token := map[string]string{"X-AIM-Token": testToken}
	// The token is required.
	if rec := post(`{"chime": false}`, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token = %d", rec.Code)
	}
	rec := post(`{"chime": false, "notifications": false, "back_to_open_minutes": 30, "keep_done_days": 90, "merges": {"AIM": "lead"}}`, token)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST = %d %s", rec.Code, rec.Body)
	}
	got = api.Settings{}
	decode(t, rec, &got)
	if got.Chime || got.Notifications || got.BackToOpenMinutes != 30 || got.KeepDoneDays != 90 || !got.TabCount ||
		got.Merges[1] != (api.PlanMerges{Plan: "aim", Title: "ai-manager — build plan", Merges: "lead", Line: "you", Override: true}) {
		t.Errorf("after POST %+v", got)
	}
	// All or none: a bad value or an unknown plan changes nothing.
	for body, status := range map[string]int{
		`{"chime": true, "back_to_open_minutes": 15}`: http.StatusBadRequest,
		`{"chime": true, "keep_done_days": 1}`:        http.StatusBadRequest,
		`{"chime": true, "merges": {"aim": "agent"}}`: http.StatusBadRequest,
		`{"chime": true, "merges": {"nope": "lead"}}`: http.StatusNotFound,
		`{"chime": true`: http.StatusBadRequest,
	} {
		if rec := post(body, token); rec.Code != status {
			t.Errorf("POST %s = %d %s, want %d", body, rec.Code, rec.Body, status)
		}
	}
	if e.srv.settings(context.Background()).Chime {
		t.Error("a refused POST changed the chime")
	}
	// null or "" drops an override.
	rec = post(`{"merges": {"aim": null}}`, token)
	got = api.Settings{}
	decode(t, rec, &got)
	if rec.Code != http.StatusOK || got.Merges[1].Override || got.Merges[1].Merges != "you" {
		t.Errorf("reset by API = %d %+v", rec.Code, got.Merges)
	}
}

// The notifications switch says when config.toml turns them off.
func TestSettingNotificationsConfigOff(t *testing.T) {
	srv, _ := newApp(t, func(o *Options) { o.NotifyDisabled = true })
	wantIn(t, "page", get(t, srv.Handler(), "/app/settings"), "config.toml turns them off (notify.enabled = false), whatever this says.")
}
