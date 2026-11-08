package web

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/focus"
	"github.com/gravexa-llc/ai-manager/internal/monitor"
	"github.com/gravexa-llc/ai-manager/internal/sessions"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// fakeJobs is an ExpiredJobs: the jobs listed are expired, after that
// many days.
type fakeJobs map[string]int

func (f fakeJobs) Expiry(jobID string, _ ...string) sessions.JobExpiry {
	days, ok := f[jobID]
	return sessions.JobExpiry{Expired: ok, Days: days}
}

// newSessionsEnv is newMonitorEnv's world (opt-in or all) with more
// sessions, and a server with Go, terminal apps and expired jobs, changed
// by opt if given:
//   - watched: pid:50 "older wait" (a question, waiting 1h), pid:51 "idle
//     watched", and job:ex1 "old watched job", expired but still blocked;
//   - not monitored: job:ex2 "old stray job" (expired) and job:live1
//     "fresh job";
//   - terminals: the aim lead (pid:43) in iTerm2, the watched one (pid:45)
//     in tmux, pid:50 in Terminal.
func newSessionsEnv(t *testing.T, mode string, opt func(*Options)) (*boardEnv, *stubGo) {
	t.Helper()
	e := newMonitorEnv(t, mode)
	ctx := context.Background()
	must(t, e.st.UpsertSessions(ctx,
		store.Session{Key: "pid:50", SessionID: "watch-2", PID: 50, Kind: "interactive", Name: "older wait", Cwd: "/w/two",
			State: needsYou, StateReason: "input needed", NeedsYouSince: t0.Add(-time.Hour), StartedAt: t0.Add(-3 * time.Hour), LastSeenAt: t0},
		store.Session{Key: "pid:51", SessionID: "watch-3", PID: 51, Kind: "interactive", Name: "idle watched", Cwd: "/w/three",
			State: "idle", StartedAt: t0.Add(-4 * time.Hour), LastSeenAt: t0},
		store.Session{Key: "job:ex1", JobID: "ex1", SessionID: "ex-1", Kind: "background", Name: "old watched job", Cwd: "/w/four",
			State: needsYou, StateReason: "reply", NeedsYouSince: t0.Add(-40 * 24 * time.Hour), LastSeenAt: t0},
		store.Session{Key: "job:ex2", JobID: "ex2", SessionID: "ex-2", Kind: "background", Name: "old stray job", Cwd: "/w/five",
			State: needsYou, StateReason: "reply", NeedsYouSince: t0.Add(-50 * 24 * time.Hour), LastSeenAt: t0},
		store.Session{Key: "job:live1", JobID: "live1", SessionID: "live-1", Kind: "background", Name: "fresh job", Cwd: "/w/six",
			State: "working", LastSeenAt: t0},
	))
	for _, id := range []string{"watch-2", "watch-3", "ex-1"} {
		e.watchAPI("POST", id, http.StatusOK)
	}
	g := &stubGo{res: api.GoResult{Key: "pid:43", Action: api.GoFocused, Message: "Focused aim lead in iTerm2."}}
	repos := fakeRepos{watchedRepo: aimRepo}
	o := Options{Port: testPort, Token: testToken, Store: e.st, Plans: e.svc, Repos: repos, HomeDir: e.tmp,
		Now: func() time.Time { return t0 },
		Monitor: &monitor.Source{Mode: mode, Store: e.st, RepoNames: repos.RepoNames,
			Plans: func(ctx context.Context) ([]api.Plan, error) {
				list, err := e.svc.Plans(ctx)
				return list.Plans, err
			}},
		Go:        g,
		Terminals: fakeTerms{"pid:43": appITerm2, "pid:45": appTmux, "pid:50": appTerminal},
		Jobs:      fakeJobs{"ex1": 30, "ex2": 30},
	}
	if opt != nil {
		opt(&o)
	}
	e.srv = New(o)
	e.h = e.srv.Handler()
	return e, g
}

// rowKeysRE finds the list's rows in order.
var rowKeysRE = regexp.MustCompile(`data-item="([^"]+)"`)

// rowKeys lists the rows' keys in html, in order.
func rowKeys(html string) []string {
	var out []string
	for _, m := range rowKeysRE.FindAllStringSubmatch(html, -1) {
		out = append(out, m[1])
	}
	return out
}

// groupOf returns the list's HTML from the group header with label to the
// next group header (or the end).
func groupOf(t *testing.T, list, label string) string {
	t.Helper()
	i := strings.Index(list, label)
	if i < 0 {
		t.Fatalf("no group %q in:\n%s", label, list)
	}
	rest := list[i+len(label):]
	if j := strings.Index(rest, `class="ghead"`); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

func TestAppSessionsGroups(t *testing.T) {
	e, _ := newSessionsEnv(t, monitor.ModeOptIn, nil)
	list := get(t, e.h, "/app/sessions?fragment=list")

	// The plan's group links to its page and lists its lead, the lead's
	// live subagent right after it, and the package's agent.
	aim := groupOf(t, list, `<a href="/app/plans/aim">ai-manager — build plan</a><span>3</span>`)
	if got := strings.Join(rowKeys(aim), " "); got != "pid:43 pid:43/agent-d1 pid:42" {
		t.Errorf("aim group rows = %s", got)
	}
	wantIn(t, "aim group", aim,
		`<span class="ititle">aim lead</span><span class="isub">Lead · interactive · Working</span>`,
		`<a class="srow subrow" href="/app/sessions?s=pid%3A43%2Fagent-d1" data-pane="detail" data-item="pid:43/agent-d1">`,
		`<span class="ititle">Agent · D1 Event receiver and live state</span><span class="isub">Subagent of aim lead · Working</span>`,
		`<span class="tag term-iterm2" title="Runs in an iTerm2 tab (its lead&#39;s)">iTerm2 · lead&#39;s tab</span>`,
		`<span class="isub">Agent of C6 · Working</span>`)

	// No plan · watched: who needs you first, longest wait first, then the
	// rest, then the expired job, which never needs you.
	watched := groupOf(t, list, `<span>No plan · watched</span><span>4</span>`)
	if got := strings.Join(rowKeys(watched), " "); got != "pid:50 pid:45 pid:51 job:ex1" {
		t.Errorf("watched group rows = %s", got)
	}
	wantIn(t, "watched group", watched,
		`<span class="dot kind-question" aria-hidden="true"></span>`, `<span class="isub">Interactive · Needs you · question</span>`,
		`<span class="dot kind-permission" aria-hidden="true"></span>`, `<span class="isub">Interactive · Needs you · permission</span>`,
		`<span class="tag term-tmux" title="Runs inside tmux">tmux</span>`,
		`data-chime="s:pid:45:`, `data-chime="s:pid:50:`,
		`<span class="dot lc-dropped" aria-hidden="true"></span>`,
		`<span class="isub">Background job · expired: its conversation was deleted after 30 days</span>`)
	wantNotIn(t, "watched group", watched, `data-chime="s:job:ex1`, "headless run")

	// Not monitored, collapsed: Watch on each, the disabled one with its
	// reason, and the expired job's `claude rm` instead.
	not := section(t, list, "sessions-not-monitored")
	wantIn(t, "not monitored", not,
		`<details id="sessions-not-monitored" class="notmon">`,
		`<summary class="ghead"><span>Not monitored (5)</span>`,
		`<input type="hidden" name="key" value="pid:46">`, `<input type="hidden" name="key" value="job:live1">`,
		`<button type="button" class="btn sm" disabled title="aimd doesn&#39;t know this session&#39;s Claude Code id yet`,
		`<span class="isub">Background job · expired: its conversation was deleted after 30 days</span>`,
		`<code class="rmcmd" title="Remove the job yourself in a terminal; aimd never deletes it">claude rm ex2</code>`)
	wantNotIn(t, "not monitored", not, `value="job:ex2"`, "headless run", `data-chime`)
	if n := strings.Count(not, `action="/app/sessions/watch"`); n != 3 {
		t.Errorf("not monitored has %d Watch forms, want 3 (pid:44, pid:46, job:live1)", n)
	}

	// The summary counts the menu's way, and says how many are expired;
	// the expired watched job doesn't count as needing you.
	wantIn(t, "summary", get(t, e.h, "/app/sessions?fragment=summary"), "<p>6 monitored · 5 not monitored · 2 expired</p>")
	menu := get(t, e.h, "/app/sessions?fragment=menu")
	wantIn(t, "menu", menu, `data-needs-count="2"`, `Sessions<span class="mini" title="6 monitored">`)
	// The menu's count ran a sync pass of the inbox: it opened items
	// for the two waiting sessions, and none for the expired job.
	items, err := e.st.ListInbox(context.Background(), store.InboxFilter{Waiting: true})
	must(t, err)
	var keys []string
	for _, it := range items {
		keys = append(keys, it.SessionKey)
	}
	if got := strings.Join(keys, " "); got != "pid:50 pid:45" {
		t.Errorf("inbox items for sessions %q, want pid:50 pid:45", got)
	}
}

func TestAppSessionsAllMode(t *testing.T) {
	e, _ := newSessionsEnv(t, monitor.ModeAll, nil)
	list := get(t, e.h, "/app/sessions?fragment=list")
	// Every session is monitored: no Not monitored group, and the lead's
	// plan by its slug (the boards are read in opt-in mode only).
	wantIn(t, "all mode", list, `<span>aim</span>`, `<span>No plan</span>`, `data-item="pid:46"`, `data-item="job:ex2"`)
	wantNotIn(t, "all mode", list, "sessions-not-monitored", "No plan · watched", `action="/app/sessions/watch"`)
	detail := get(t, e.h, "/app/sessions?s=pid%3A46&fragment=detail")
	wantIn(t, "all mode detail", detail, `sessions.monitor is &#34;all&#34;: aimd monitors every session.`)
	wantNotIn(t, "all mode detail", detail, "Stop monitoring", `action="/app/sessions/watch"`)
}

func TestAppSessionsSelection(t *testing.T) {
	e, _ := newSessionsEnv(t, monitor.ModeOptIn, nil)
	sel := `<a class="srow sel" href="/app/sessions?s=pid%3A45" data-pane="detail" data-item="pid:45" aria-current="true"`
	detail := []string{
		`<span class="pill kind-permission">Needs you · permission</span>`, "<h2>watched one</h2>",
		`<p class="subtitle">Interactive</p>`, "<p class=\"dtext\">Permission prompt · for 20m</p>",
		`<p class="dtext">You asked to watch it.</p>`, `<dt>Branch</dt><dd><code>feat/watched</code></dd>`,
		`<dt>Folder</dt><dd><code title="/w/ai">/w/ai</code></dd>`,
		`action="/app/sessions/unwatch"`, ">Stop monitoring</button>",
	}
	// The whole page, the list fragment and the detail fragment render the
	// same selection from the URL.
	page := get(t, e.h, "/app/sessions?s=pid%3A45")
	wantIn(t, "page list", section(t, page, "list"), sel)
	wantIn(t, "page detail", section(t, page, "detail"), detail...)
	wantIn(t, "list fragment", get(t, e.h, "/app/sessions?s=pid%3A45&fragment=list"), sel)
	frag := get(t, e.h, "/app/sessions?s=pid%3A45&fragment=detail")
	wantIn(t, "detail fragment", frag, detail...)
	wantNotIn(t, "detail fragment", frag, `class="srow`, "<html")
	if n := strings.Count(get(t, e.h, "/app/sessions?s=pid%3A45&fragment=list"), `aria-current="true"`); n != 1 {
		t.Errorf("%d rows selected, want 1", n)
	}

	// Nothing selected; a session that ended; a not-monitored one opens
	// its group and offers Watch.
	wantIn(t, "none", get(t, e.h, "/app/sessions?fragment=detail"), "Select a session to see why it&#39;s monitored")
	wantIn(t, "ended", get(t, e.h, "/app/sessions?s=pid%3A999&fragment=detail"), "That session has ended")
	wantIn(t, "ended agent", get(t, e.h, "/app/sessions?s=pid%3A43%2Fagent-zz&fragment=detail"), "That subagent has finished.")
	wantIn(t, "not monitored", get(t, e.h, "/app/sessions?s=pid%3A46&fragment=list"),
		`<details id="sessions-not-monitored" class="notmon" open>`)
	wantIn(t, "not monitored detail", get(t, e.h, "/app/sessions?s=pid%3A46&fragment=detail"),
		"Not monitored: it is left out of Needs you and notifications.", `action="/app/sessions/watch"`, `<input type="hidden" name="key" value="pid:46">`)

	// A lead: its plan, its package and why.
	lead := get(t, e.h, "/app/sessions?s=pid%3A43&fragment=detail")
	wantIn(t, "lead", lead, `<p class="subtitle">Lead · interactive</p>`, `<p class="dtext">Registered plan aim · runs the agent of D1.</p>`,
		`<dt>Plan</dt><dd><a href="/app/plans/aim">ai-manager — build plan</a></dd>`, `<dt>Package</dt><dd>D1 Event receiver and live state</dd>`)
	wantNotIn(t, "lead", lead, "Stop monitoring")

	// A subagent: under its lead, Go to the lead's tab.
	agent := get(t, e.h, "/app/sessions?s=pid%3A43%2Fagent-d1&fragment=detail")
	wantIn(t, "subagent", agent, "<h2>Agent · D1 Event receiver and live state</h2>", `<p class="subtitle">Subagent of aim lead</p>`,
		"Agent of D1 on a registered plan; it runs inside aim lead.", `<dt>Last action</dt><dd>Edit: live.go</dd>`,
		`<input type="hidden" name="key" value="lead-1/agent-d1">`)

	// The search box: matching rows only, with Not monitored open, and the
	// rows' links keep it.
	found := get(t, e.h, "/app/sessions?q=stranger&fragment=list")
	if got := strings.Join(rowKeys(found), " "); got != "pid:46" {
		t.Errorf("search rows = %s", got)
	}
	wantIn(t, "search", found, `class="notmon" open`, "No monitored session matches “stranger”.", `href="/app/sessions?q=stranger&amp;s=pid%3A46"`)
	wantIn(t, "search none", get(t, e.h, "/app/sessions?q=zzz&fragment=list"), "No session matches “zzz”.")
}

func TestAppSessionsGo(t *testing.T) {
	e, g := newSessionsEnv(t, monitor.ModeOptIn, nil)
	// Enabled: a form posting the key, on g.
	wantIn(t, "go on", get(t, e.h, "/app/sessions?s=pid%3A43&fragment=detail"),
		`<form method="post" action="/app/sessions/go" data-pane="detail">`, `<input type="hidden" name="key" value="pid:43">`,
		`<input type="hidden" name="return" value="/app/sessions?s=pid%3A43">`,
		`<button type="submit" class="btn primary" data-key="g" aria-keyshortcuts="g"`)
	// Disabled, with the reason on the button (the key's toast) and shown.
	for target, off := range map[string]string{
		"pid%3A45":    "Runs inside tmux; Go can only switch to iTerm2 and Terminal tabs.",
		"job%3Alive1": "Background session: no terminal tab to switch to. Open it with `claude attach live1`.",
		"job%3Aex1":   "Its conversation was deleted, so there&#39;s nothing to go to: `claude attach` would open a blank tab.",
	} {
		d := get(t, e.h, "/app/sessions?s="+target+"&fragment=detail")
		wantIn(t, target, d, `<button type="button" class="btn primary" disabled data-key="g" aria-keyshortcuts="g" data-off="`+off+`" title="`+off+`">`,
			`<p class="hint">`+off+`</p>`)
		wantNotIn(t, target, d, `action="/app/sessions/go"`)
	}

	// POST /app/sessions/go: JSON for the script, a 303 for a form.
	jsonHdr := map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Accept": "application/json", CSRFHeader: e.srv.CSRF().Token()}
	body := url.Values{"key": {"pid:43"}, "return": {"/app/sessions?s=pid:43"}, CSRFField: {e.srv.CSRF().Token()}}.Encode()
	rec := do(t, e.h, "POST", "/app/sessions/go", strings.NewReader(body), jsonHdr)
	var a appAnswer
	decode(t, rec, &a)
	if rec.Code != http.StatusOK || a.Toast == nil || a.Toast.Text != "Focused aim lead in iTerm2." || a.Toast.Undo != nil ||
		a.Location != "/app/sessions?s=pid%3A43" || len(g.keys) != 1 || g.keys[0] != "pid:43" {
		t.Errorf("go = %d %+v (toast %+v), keys %v", rec.Code, a, a.Toast, g.keys)
	}
	rec = e.post("/app/sessions/go", url.Values{"key": {"pid:43"}, "return": {"/app/sessions?s=pid:43"}})
	wantIn(t, "go form", section(t, e.follow(rec, "/app/sessions?s=pid%3A43&toast="), "toasts"), "<span>Focused aim lead in iTerm2.</span>")

	// A refusal keeps its status and says why.
	g.err = &focus.Error{Status: http.StatusUnprocessableEntity, Msg: "aim lead isn't in an iTerm2 or Terminal tab"}
	rec = do(t, e.h, "POST", "/app/sessions/go", strings.NewReader(body), jsonHdr)
	a = appAnswer{}
	decode(t, rec, &a)
	if rec.Code != http.StatusUnprocessableEntity || a.Error != "Go: aim lead isn't in an iTerm2 or Terminal tab" {
		t.Errorf("refused go = %d %+v", rec.Code, a)
	}

	// Without Go, no button, and the route says so.
	off, _ := newSessionsEnv(t, monitor.ModeOptIn, func(o *Options) { o.Go = nil })
	wantNotIn(t, "no go", get(t, off.h, "/app/sessions?s=pid%3A43&fragment=detail"), `data-key="g"`)
	rec = do(t, off.h, "POST", "/app/sessions/go", strings.NewReader(url.Values{"key": {"pid:43"}, CSRFField: {off.srv.CSRF().Token()}}.Encode()),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Accept": "application/json", CSRFHeader: off.srv.CSRF().Token()})
	if rec.Code != http.StatusNotFound {
		t.Errorf("go without Go = %d %s", rec.Code, rec.Body)
	}
}

func TestAppSessionsWatch(t *testing.T) {
	e, _ := newSessionsEnv(t, monitor.ModeOptIn, nil)
	tok := e.srv.CSRF().Token()
	jsonPost := func(target string, vals url.Values) (*appAnswer, int) {
		t.Helper()
		vals.Set(CSRFField, tok)
		rec := do(t, e.h, "POST", target, strings.NewReader(vals.Encode()),
			map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Accept": "application/json", CSRFHeader: tok})
		var a appAnswer
		decode(t, rec, &a)
		return &a, rec.Code
	}
	notMonitored := func() string { return section(t, get(t, e.h, "/app/sessions"), "sessions-not-monitored") }

	// Watch, from the script: the session moves to No plan · watched and
	// is selected; the toast's Undo unwatches.
	a, code := jsonPost("/app/sessions/watch", url.Values{"key": {"pid:46"}, "return": {"/app/sessions?q=wait"}})
	if code != http.StatusOK || a.Location != "/app/sessions?q=wait&s=pid%3A46" || a.Toast == nil ||
		!strings.HasPrefix(a.Toast.Text, "Watching waiting stranger: it shows under Needs you") || a.Toast.Undo == nil {
		t.Fatalf("watch = %d %+v (toast %+v)", code, a, a.Toast)
	}
	if u := a.Toast.Undo; u.Action != "/app/sessions/unwatch" || u.Fields["key"] != "pid:46" || u.Fields["undo"] != "1" || u.Fields["return"] != a.Location {
		t.Errorf("watch's undo = %+v", u)
	}
	wantIn(t, "after watch", groupOf(t, get(t, e.h, "/app/sessions?fragment=list"), "No plan · watched"), `data-item="pid:46"`)
	wantNotIn(t, "after watch", notMonitored(), `data-item="pid:46"`)
	wantIn(t, "after watch", get(t, e.h, "/app/sessions?fragment=menu"), `data-needs-count="3"`)

	// Undo: back under Not monitored, and no Undo of the Undo.
	fields := url.Values{}
	for k, v := range a.Toast.Undo.Fields {
		fields.Set(k, v)
	}
	b, code := jsonPost(a.Toast.Undo.Action, fields)
	if code != http.StatusOK || b.Toast == nil || !strings.HasPrefix(b.Toast.Text, "Stopped watching waiting stranger") || b.Toast.Undo != nil {
		t.Errorf("undo = %d %+v (toast %+v)", code, b, b.Toast)
	}
	wantIn(t, "after undo", notMonitored(), `data-item="pid:46"`)

	// Stop monitoring, from a plain form: a 303 back, the toast with its
	// Undo as a form.
	rec := e.post("/app/sessions/unwatch", url.Values{"key": {"pid:45"}, "return": {"/app/sessions?s=pid:45"}})
	page := e.follow(rec, "/app/sessions?s=pid%3A45&toast=")
	toast := section(t, page, "toasts")
	wantIn(t, "stop toast", toast, "<span>Stopped watching watched one: it is no longer monitored",
		`<form method="post" action="/app/sessions/watch" data-pane>`, `<input type="hidden" name="key" value="pid:45">`,
		`<input type="hidden" name="undo" value="1">`)
	wantIn(t, "stopped", section(t, page, "sessions-not-monitored"), `data-item="pid:45"`, `class="notmon" open`)
	// Its Undo, as a form, watches it again.
	rec = e.post("/app/sessions/watch", url.Values{"key": {"pid:45"}, "undo": {"1"}, "return": {"/app/sessions?s=pid:45"}})
	page = e.follow(rec, "/app/sessions?s=pid%3A45&toast=")
	wantIn(t, "watched again", section(t, page, "toasts"), "<span>Watching watched one")
	wantNotIn(t, "watched again", section(t, page, "toasts"), "<form")
	wantIn(t, "watched again", groupOf(t, section(t, page, "list"), "No plan · watched"), `data-item="pid:45"`)

	// Refusals: no key, an ended session, one aimd has no id for, an
	// expired job; and an unwatch that changes nothing has no Undo.
	for _, c := range []struct {
		vals   url.Values
		status int
		msg    string
	}{
		{url.Values{}, http.StatusBadRequest, "Which session?"},
		{url.Values{"key": {"pid:999"}}, http.StatusConflict, "That session has ended."},
		{url.Values{"key": {"pid:47"}}, http.StatusConflict, "aimd doesn't know this session's Claude Code id yet"},
		{url.Values{"key": {"job:ex2"}}, http.StatusConflict, "Not watched: this background job is expired: its conversation was deleted after 30 days. Remove it with `claude rm ex2`."},
	} {
		a, code := jsonPost("/app/sessions/watch", c.vals)
		if code != c.status || !strings.HasPrefix(a.Error, c.msg) {
			t.Errorf("watch %v = %d %q, want %d %q", c.vals, code, a.Error, c.status, c.msg)
		}
	}
	a, code = jsonPost("/app/sessions/unwatch", url.Values{"key": {"pid:46"}})
	if code != http.StatusOK || a.Toast == nil || a.Toast.Undo != nil || !strings.Contains(a.Toast.Text, "wasn't watched") {
		t.Errorf("unwatch of an unwatched session = %d %+v", code, a.Toast)
	}

	// CSRF.Protect guards the routes.
	if rec := do(t, e.h, "POST", "/app/sessions/watch", strings.NewReader("key=pid%3A46"),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"}); rec.Code != http.StatusForbidden {
		t.Errorf("watch without a token = %d, want 403", rec.Code)
	}
}

func TestAppSessionsExpired(t *testing.T) {
	e, _ := newSessionsEnv(t, monitor.ModeOptIn, nil)
	// A watched expired job: read-only but for Stop monitoring, with the
	// command to remove it.
	d := get(t, e.h, "/app/sessions?s=job%3Aex1&fragment=detail")
	wantIn(t, "expired detail", d, `<span class="pill lc-dropped">Expired</span>`, `<p class="subtitle">Background job</p>`,
		"This background job is expired: its conversation was deleted after 30 days.", `<p class="code">claude rm ex1</p>`,
		"aimd never deletes a job, and an expired one never counts as needing you.", ">Stop monitoring</button>")
	wantNotIn(t, "expired detail", d, "Waiting on you", `action="/app/sessions/go"`, `action="/app/sessions/watch"`)
	// One not monitored: no Watch.
	d = get(t, e.h, "/app/sessions?s=job%3Aex2&fragment=detail")
	wantIn(t, "expired stray", d, `<p class="code">claude rm ex2</p>`, "Not monitored")
	wantNotIn(t, "expired stray", d, `action="/app/sessions/watch"`, "Stop monitoring")

	// With aimd's real reader, on a Claude config dir in a temp folder:
	// ex1's state.json is 40 days old and no transcript is left; live1's
	// transcript is.
	dir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(dir, rel)
		must(t, os.MkdirAll(filepath.Dir(p), 0o755))
		must(t, os.WriteFile(p, []byte(content), 0o644))
	}
	old := strconv.FormatInt(t0.Add(-40*24*time.Hour).UnixMilli(), 10)
	write("jobs/ex1/state.json", `{"sessionId":"ex-1","updatedAt":`+old+`,"state":"blocked"}`)
	write("jobs/live1/state.json", `{"sessionId":"live-1","updatedAt":`+old+`}`)
	write("projects/-w-six/live-1.jsonl", "{}\n")
	real, _ := newSessionsEnv(t, monitor.ModeOptIn, func(o *Options) { o.Jobs = sessions.NewJobs(dir, func() time.Time { return t0 }) })
	list := get(t, real.h, "/app/sessions?fragment=list")
	wantIn(t, "real reader", groupOf(t, list, "No plan · watched"),
		`<span class="isub">Background job · expired: its conversation was deleted after 30 days</span>`)
	wantNotIn(t, "real reader", section(t, list, "sessions-not-monitored"), "claude rm")
	wantIn(t, "real reader summary", get(t, real.h, "/app/sessions?fragment=summary"), "· 1 expired</p>")
}

func TestAppSessionsEscapes(t *testing.T) {
	e, _ := newSessionsEnv(t, monitor.ModeOptIn, nil)
	evil := `<img src=x onerror=alert(1)>`
	must(t, e.st.UpsertSessions(context.Background(), store.Session{Key: "pid:66", SessionID: "evil-1", PID: 66, Kind: "interactive",
		Name: evil, Cwd: "/w/" + evil, Branch: evil, State: "idle", LastSeenAt: t0}))
	for _, target := range []string{"/app/sessions", "/app/sessions?s=pid%3A66", "/app/sessions?q=" + url.QueryEscape(evil)} {
		wantNotIn(t, target, get(t, e.h, target), "<img src=x")
	}
}

func TestNeedKind(t *testing.T) {
	for reason, want := range map[string]string{
		"permission prompt":                    "kind-permission permission",
		"permission prompt: Bash rm -rf build": "kind-permission permission",
		"agent a1: permission prompt: Edit":    "kind-permission permission",
		"sandbox request":                      "kind-permission permission",
		"input needed":                         "kind-question question",
		"question: Which plan? (permission?)":  "kind-question question",
		"agent a2: question: x":                "kind-question question",
		"dialog open":                          "kind-question question",
		"plan approval":                        "kind-plan plan approval",
		"goal proposal":                        "kind-plan plan approval",
		"reply":                                "kind-reply reply",
		"waiting":                              "kind-reply waiting",
		"":                                     "kind-reply waiting",
	} {
		tone, what := needKind(reason)
		if got := tone + " " + what; got != want {
			t.Errorf("needKind(%q) = %q, want %q", reason, got, want)
		}
	}
}
