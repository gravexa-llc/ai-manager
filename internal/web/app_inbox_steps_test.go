package web

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/inbox"
	"github.com/gravexa-llc/ai-manager/internal/monitor"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// searchV2 is the plan of these tests, a made-up one. At first:
//   - P1 is your step, done (✅);
//   - Q1 is your step (Owner you), ready: its need, P1, is done;
//   - Q2 is an agent's package with a "you:" step, ready;
//   - Q3 is your step, blocked on Q1; its Repo cell names the repo by
//     its name alone;
//   - Q4 is an agent's package, blocked on Q2.
const searchV2 = "# Search v2\n\nRepos: acme/app\n\n" +
	"| Package | What | Repo | Owner | Needs |\n|---|---|---|---|---|\n" +
	"| **P1 Pick the ranking** ✅ | Choose the ranking we ship. |  | you | — |\n" +
	"| **Q1 Try it in staging** | Deploy the new index to `staging` and try a few **real** searches. |  | you | P1 |\n" +
	"| **Q2 Ship the index** | The agent rolls the index out. | acme/app |  | P1; you: sign off the rollout |\n" +
	"| **Q3 Write the announcement** | A short post for the team. | app | you | Q1 |\n" +
	"| **Q4 Clean up the old index** | Remove the old index. | acme/app |  | Q2 |\n"

// stepsEnv is the dashboard with Search v2 registered and monitored as
// aimd monitors it, its clock two hours after the plan's first state pass.
type stepsEnv struct {
	*boardEnv
	plan string
}

func newStepsEnv(t *testing.T) *stepsEnv {
	t.Helper()
	e := &stepsEnv{boardEnv: newBoardEnv(t)}
	e.plan = e.write(filepath.Join(e.root, "app", "docs", "search-v2-plan.md"), searchV2)
	e.api("POST", "/api/plans", api.RegisterRequest{Path: e.plan, Repos: []string{"acme/app"}}, http.StatusCreated)
	e.at(t0.Add(2 * time.Hour))
	return e
}

// at rebuilds the server as aimd serves it, its clock at now.
func (e *stepsEnv) at(now time.Time) {
	e.t.Helper()
	mon := &monitor.Source{Mode: monitor.ModeAll, Store: e.st, Plans: func(ctx context.Context) ([]api.Plan, error) {
		list, err := e.svc.Plans(ctx)
		return list.Plans, err
	}}
	e.srv = New(Options{Port: testPort, Token: testToken, Store: e.st, Plans: e.svc, Monitor: mon, HomeDir: e.tmp,
		Now: func() time.Time { return now }})
	e.h = e.srv.routes()
}

// edit rewrites the plan file, replacing old with new, and registers it
// again, as the plan's lead does after an edit.
func (e *stepsEnv) edit(old, new string) {
	e.t.Helper()
	src, err := os.ReadFile(e.plan)
	must(e.t, err)
	if !strings.Contains(string(src), old) {
		e.t.Fatalf("the plan has no %q", old)
	}
	e.write(e.plan, strings.Replace(string(src), old, new, 1))
	e.api("POST", "/api/plans", api.RegisterRequest{Path: e.plan, Repos: []string{"acme/app"}}, http.StatusOK)
}

// steps runs a sync pass and returns every your-step item, waiting or
// not, by package; of two items of one package, the waiting one.
func (e *stepsEnv) steps() map[string]store.InboxItem {
	e.t.Helper()
	ctx := context.Background()
	_, err := e.srv.SyncInbox(ctx)
	must(e.t, err)
	list, err := e.st.ListInbox(ctx, store.InboxFilter{})
	must(e.t, err)
	out := map[string]store.InboxItem{}
	for _, it := range list {
		if it.Kind != inbox.KindYourStep {
			continue
		}
		if have, ok := out[it.Pkg]; !ok || !have.Waiting() {
			out[it.Pkg] = it
		}
	}
	return out
}

// jsonPost posts a form as app-shell.js does, and decodes its answer.
func (e *stepsEnv) jsonPost(target string, vals url.Values) (int, appAnswer) {
	e.t.Helper()
	vals.Set(CSRFField, e.srv.CSRF().Token())
	rec := do(e.t, e.h, "POST", target, strings.NewReader(vals.Encode()), map[string]string{
		"Content-Type": "application/x-www-form-urlencoded", "Accept": "application/json", CSRFHeader: e.srv.CSRF().Token()})
	var a appAnswer
	decode(e.t, rec, &a)
	return rec.Code, a
}

func itemID(it store.InboxItem) string { return strconv.FormatInt(it.ID, 10) }

// The plan page's "needs you" badge and the inbox's Your step items follow
// one rule (yourStep): yours, by Owner or a "you:" step, and ready. A
// package that doesn't meet it has no badge and no episode, and the
// episode's end says why.
func TestYourStepRule(t *testing.T) {
	sn := &snapshot{now: t0}
	for _, c := range []struct {
		name    string
		p       api.Package
		why     string // the badge's why, "" for no badge and no item
		stopped string // why its episode ends
	}{
		{"yours, ready", api.Package{ID: "Q1", Title: "Try it in staging", Owner: "you", Yours: true, Lifecycle: "ready"},
			"your step is ready", ""},
		{"a you: step, ready", api.Package{ID: "Q2", Title: "Ship the index", NeedsYou: "sign off the rollout", Lifecycle: "ready"},
			"your step is ready: sign off the rollout", ""},
		{"yours, blocked again", api.Package{ID: "Q3", Yours: true, Lifecycle: "blocked", BlockedBy: []string{"Q1"}},
			"", "it is blocked again: it needs Q1"},
		{"a you: step, blocked", api.Package{ID: "Q4", NeedsYou: "sign off", Lifecycle: "blocked"}, "", "it is blocked again"},
		{"yours, marked done", api.Package{ID: "Q1", Yours: true, Lifecycle: "done", DoneManual: true}, "", "it was marked done"},
		{"yours, ✅ in the plan", api.Package{ID: "Q1", Yours: true, Lifecycle: "done", DoneInPlan: true}, "", "it is ✅ in search-v2-plan.md now"},
		{"a you: step, its PR merged", api.Package{ID: "Q2", NeedsYou: "sign off", Lifecycle: "done"}, "", "it is done: its PR merged"},
		{"a you: step, its agent at work", api.Package{ID: "Q2", NeedsYou: "sign off", Lifecycle: "working"}, "", "work on it began"},
		{"yours, a PR in review", api.Package{ID: "Q1", Yours: true, Lifecycle: "in_review"}, "", "it has a PR in review"},
		{"an agent's, ready", api.Package{ID: "Q4", Lifecycle: "ready"}, "", "it isn't your step any more"},
		// A provisional package has no row to make it yours; its missing
		// row is a plan_row item.
		{"provisional", api.Package{ID: "Z9", Yours: true, Lifecycle: "ready", Provisional: true}, "", "its row isn't in search-v2-plan.md"},
	} {
		why, ok := yourStep(c.p)
		if ok != (c.why != "") || why != c.why {
			t.Errorf("%s: yourStep = %q, %v; want %q", c.name, why, ok, c.why)
		}
		var badge string
		for _, b := range planBadgesOf(sn, nil, c.p, false) {
			if b.Kind == "needs" {
				badge = b.Why
			}
		}
		if badge != c.why {
			t.Errorf("%s: needs-you badge %q, want %q", c.name, badge, c.why)
		}
		b := api.Plan{Slug: "search-v2", Title: "Search v2", Path: "/p/search-v2-plan.md", Packages: []api.Package{c.p}}
		snap := inbox.Snapshot{Stopped: map[string]string{}}
		yourStepEpisodes(inboxInput{boards: []api.Plan{b}}, &snap)
		ref := inbox.YourStepRef("search-v2", c.p.ID)
		switch {
		case c.why != "" && (len(snap.Episodes) != 1 || snap.Episodes[0].Ref != ref || snap.Episodes[0].Why != c.why):
			t.Errorf("%s: episodes %+v, want one of %s", c.name, snap.Episodes, ref)
		case c.why == "" && len(snap.Episodes) != 0:
			t.Errorf("%s: episodes %+v, want none", c.name, snap.Episodes)
		case c.why == "" && snap.Stopped[ref] != c.stopped:
			t.Errorf("%s: stopped %q, want %q", c.name, snap.Stopped[ref], c.stopped)
		}
	}
	if got := inbox.YourStepRef("Search-V2", "q1"); got != "step:search-v2/Q1" {
		t.Errorf("YourStepRef = %q", got)
	}
}

// A plan's packages that are your step and ready are Your step items: one
// each, while the badge shows. Each ends when the package stops meeting
// the rule, and a change of its "you:" step changes its signature.
func TestInboxYourSteps(t *testing.T) {
	e := newStepsEnv(t)
	got := e.steps()
	q1, q2 := got["Q1"], got["Q2"]
	if len(got) != 2 {
		t.Fatalf("your steps = %+v, want Q1 and Q2", got)
	}
	if q1.Source != inbox.SourcePlan || q1.Ref != "step:search-v2/Q1" || q1.State != inbox.Open || q1.Title != "Q1 Try it in staging" ||
		q1.Subtitle != "Ready in Search v2" || q1.PlanSlug != "search-v2" || q1.Pkg != "Q1" || q1.Detail != "" || q1.Repo != "" ||
		q1.Why != "your step is ready" || !q1.Since.Equal(t0) {
		t.Errorf("Q1's item = %+v", q1)
	}
	if q2.Detail != "sign off the rollout" || q2.Subtitle != "Ready in Search v2 · you: sign off the rollout" || q2.Repo != "acme/app" ||
		q2.Why != "your step is ready: sign off the rollout" {
		t.Errorf("Q2's item = %+v", q2)
	}
	moves, err := e.st.InboxMoves(context.Background(), q1.ID)
	must(t, err)
	if len(moves) != 1 || moves[0].Text != "Started needing you: your step is ready" {
		t.Errorf("Q1's history = %+v", moves)
	}
	// The plan page badges the same two, and its pane lists them.
	graph := inner(t, get(t, e.h, "/app/plans/search-v2"), "list")
	for _, id := range []string{"Q1", "Q2"} {
		wantIn(t, id+" node", itemOf(t, graph, id), "pg-badge b-needs", ">needs you</text>")
	}
	for _, id := range []string{"P1", "Q3", "Q4"} {
		wantNotIn(t, id+" node", itemOf(t, graph, id), "b-needs")
	}

	// The note changes while Q2 is snoozed until it changes: the same
	// item, back to Open, with the new step as its detail.
	e.api("POST", "/api/inbox/"+itemID(q2), api.InboxActRequest{Action: "snooze", Snooze: inbox.SnoozeChange}, http.StatusOK)
	e.edit("you: sign off the rollout", "you: sign off the rollout with the team")
	if it := e.steps()["Q2"]; it.ID != q2.ID || it.State != inbox.Open || it.Detail != "sign off the rollout with the team" || it.Sig == q2.Sig {
		t.Errorf("Q2 after its step changed = %+v (was sig %q)", it, q2.Sig)
	}
	moves, err = e.st.InboxMoves(context.Background(), q2.ID)
	must(t, err)
	if last := moves[len(moves)-1]; last.Text != "Back to Open: it changed: sign off the rollout with the team" {
		t.Errorf("Q2's last move = %+v", last)
	}

	// Mark done ends Q1's episode, and unblocks Q3: a new item, in the
	// plan's repo, though its row names the repo by its name alone.
	e.api("POST", "/api/plans/search-v2/packages/Q1/done", nil, http.StatusOK)
	got = e.steps()
	if it := got["Q1"]; it.ID != q1.ID || it.Waiting() || it.State != inbox.Done || it.EndReason != "it was marked done" {
		t.Errorf("Q1 after Mark done = %+v", it)
	}
	q3 := got["Q3"]
	if !q3.Waiting() || q3.State != inbox.Open || q3.Repo != "acme/app" {
		t.Errorf("Q3 once Q1 is done = %+v", q3)
	}
	// Undone: Q1 is ready again, a new episode; Q3 is blocked again.
	e.api("DELETE", "/api/plans/search-v2/packages/Q1/done", nil, http.StatusOK)
	got = e.steps()
	if it := got["Q1"]; !it.Waiting() || it.ID == q1.ID || it.State != inbox.Open {
		t.Errorf("Q1 after Undo Mark done = %+v", it)
	}
	if it := got["Q3"]; it.ID != q3.ID || it.Waiting() || it.EndReason != "it is blocked again: it needs Q1" {
		t.Errorf("Q3 once Q1 is ready again = %+v", it)
	}

	// Q2's agent starts on it: its step is taken, the item ends.
	must(t, e.st.UpsertSessions(context.Background(), store.Session{Key: "pid:61", SessionID: "agent-q2", Kind: "interactive",
		Name: "q2 agent", Cwd: "/w/q2", State: "working", StartedAt: t0, LastSeenAt: t0.Add(2 * time.Hour)}))
	e.api("POST", "/api/links/manual", api.LinkRequest{Plan: "search-v2", Pkg: "Q2", Kind: "agent", Ref: "agent-q2"}, http.StatusCreated)
	if it := e.steps()["Q2"]; it.Waiting() || it.EndReason != "work on it began" {
		t.Errorf("Q2 once its agent works = %+v", it)
	}

	// A ✅ in the plan file ends Q1's new episode.
	e.edit("| **Q1 Try it in staging** |", "| **Q1 Try it in staging** ✅ |")
	if it := e.steps()["Q1"]; it.Waiting() || it.EndReason != "it is ✅ in search-v2-plan.md now" {
		t.Errorf("Q1 after its ✅ = %+v", it)
	}

	// Unregistered, the plan opens no items, and Q3's (ready again) ends.
	if it := e.steps()["Q3"]; !it.Waiting() {
		t.Fatalf("Q3 once Q1 has its ✅ = %+v", it)
	}
	e.api("DELETE", "/api/plans/search-v2", nil, http.StatusOK)
	for id, it := range e.steps() {
		if it.Waiting() {
			t.Errorf("%s still waits after the plan was unregistered: %+v", id, it)
		}
	}
	if it := e.steps()["Q3"]; it.EndReason != "the package is gone: its plan was unregistered, or its row left the plan file" {
		t.Errorf("Q3 after unregister = %+v", it)
	}
}

// Your steps count wherever what needs you is counted: the Needs you badge
// and the tab title, the plan's menu badge, its summary (the band and
// plan_status's needs_you, needs_you_items and needs_you_packages) and the
// Overview. They never chime, and the notifier never sees them.
func TestYourStepCounts(t *testing.T) {
	e := newStepsEnv(t)
	menu := get(t, e.h, "/app/inbox?fragment=menu")
	wantIn(t, "menu", menu, `data-needs-count="2"`, `Needs you<span class="count"><span class="vh">: </span>2</span>`,
		`<span class="nav-label">Search v2</span><span class="mini" title="2 waiting on you">`)
	wantIn(t, "title", get(t, e.h, "/app/sessions"), "<title>(2) aim</title>")
	wantIn(t, "overview", get(t, e.h, "/app/"), `<span class="legend ovneeds">2 need you</span>`)

	var sum api.PlanSummary
	decode(t, e.api("GET", "/api/plans/search-v2/summary", nil, http.StatusOK), &sum)
	if sum.NeedsYou != 2 || len(sum.NeedsYouItems) != 2 || !strings.HasSuffix(sum.Band, " · 2 need you") {
		t.Fatalf("summary = %+v", sum)
	}
	for i, want := range []string{"your step is ready", "your step is ready: sign off the rollout"} {
		if it := sum.NeedsYouItems[i]; it.Kind != inbox.KindYourStep || it.KindLabel != "Your step" || it.Reason != want {
			t.Errorf("summary item %d = %+v, want reason %q", i, it, want)
		}
	}
	if len(sum.NeedsYouPackages) != 2 || sum.NeedsYouPackages[0].ID != "Q1" || sum.NeedsYouPackages[1].ID != "Q2" ||
		sum.NeedsYouPackages[1].Reasons[0] != "your step is ready: sign off the rollout" {
		t.Errorf("summary packages = %+v", sum.NeedsYouPackages)
	}
	// The inbox API shows the kind.
	var box api.Inbox
	decode(t, do(t, e.h, "GET", "/api/inbox", nil, nil), &box)
	if len(box.Items) != 2 || box.Items[0].Kind != "your_step" || box.Items[0].KindLabel != "Your step" || box.Items[0].Source != "plan" ||
		box.Items[1].Detail != "sign off the rollout" {
		t.Errorf("GET /api/inbox = %+v", box.Items)
	}

	// New and highlighted, but no chime: that is for permission prompts
	// and questions.
	list := get(t, e.h, "/app/inbox?fragment=list")
	wantIn(t, "list", list, `class="irow kind-plan new"`, `<span class="ikind">Your step</span>`,
		`<span class="ititle">Q1 Try it in staging</span>`, `title="ready since 2026-11-05T12:00:00Z">2h</span>`)
	wantNotIn(t, "list", list, "data-chime")
	// The notifier asks for sessions' Open items only.
	open, err := e.srv.InboxOpenSessions(context.Background())
	must(t, err)
	if len(open) != 0 {
		t.Errorf("InboxOpenSessions = %v, want none", open)
	}

	// Snoozed, it leaves the counts, and the badge says so.
	e.api("POST", "/api/inbox/"+itemID(e.steps()["Q1"]), api.InboxActRequest{Action: "snooze", Snooze: "1h"}, http.StatusOK)
	wantIn(t, "menu after a snooze", get(t, e.h, "/app/inbox?fragment=menu"), `data-needs-count="1"`, `title="1 waiting on you"`)
	wantIn(t, "Q1 node after a snooze", itemOf(t, inner(t, get(t, e.h, "/app/plans/search-v2"), "list"), "Q1"), "b-snoozed")
}

// The item's detail: the package, its What and its step, how long it has
// been ready, and Open in plan, Mark done, Snooze and Done.
func TestYourStepDetail(t *testing.T) {
	e := newStepsEnv(t)
	steps := e.steps()
	q1, q2 := steps["Q1"], steps["Q2"]
	page := get(t, e.h, "/app/inbox?item="+itemID(q1))
	checkNoInline(t, "Q1's item", page)
	detail := section(t, page, "detail")
	wantIn(t, "Q1's detail", detail,
		`<span class="pill kind-plan">Your step</span>`, `<span class="istatus">Ready for 2h</span>`,
		"<h2>Q1 Try it in staging</h2>", `<p class="subtitle">Ready in Search v2</p>`,
		`<p class="istep-what">Deploy the new index to staging and try a few real searches.</p>`,
		`title="ready since 2026-11-05T12:00:00Z">Ready since 12:00 (2h)</p>`,
		"It&#39;s yours (Owner: you), and everything it needs is done.", "put a ✅ in its row in search-v2-plan.md",
		// Open in plan is Go.
		`<form method="post" action="/app/inbox/go" data-pane="list detail">`, `Open in plan <kbd>g</kbd>`,
		// Mark done is the plan page's own form.
		`<form method="post" action="/app/plans/search-v2/done" data-pane="list detail">`,
		`<input type="hidden" name="pkg" value="Q1"><input type="hidden" name="done" value="1"><input type="hidden" name="return" value="/app/inbox?item=`+itemID(q1)+`">`,
		">Mark done</button>", "Snooze ▾", "Done <kbd>d</kbd>",
		`<dd><a href="/app/plans/search-v2">Search v2</a></dd>`, `<dd><a href="/app/plans/search-v2?pkg=Q1">Q1 Try it in staging</a></dd>`,
		"· Started needing you: your step is ready</li>")
	wantNotIn(t, "Q1's detail", detail, "Your part", `target="_blank"`)
	// Q2's: its step, its repo, and what ends it. It is an agent's
	// package, done by its PR or its ✅: no Mark done at all, not even
	// greyed out, and Done to dismiss it.
	q2Detail := get(t, e.h, "/app/inbox?item="+itemID(q2)+"&fragment=detail")
	wantIn(t, "Q2's detail", q2Detail,
		`<p class="label">Your part</p>`+"\n"+`<p class="iask">sign off the rollout</p>`,
		"this item closes by itself when work on it begins", "<dt>Repo</dt><dd>acme/app</dd>",
		`<p class="hint">Your part is done? Dismiss this with Done; the package finishes when its PR merges.</p>`,
		"Open in plan <kbd>g</kbd>", "Snooze ▾", "Done <kbd>d</kbd>")
	wantNotIn(t, "Q2's detail", q2Detail, "Mark done", "/app/plans/search-v2/done")
	wantNotIn(t, "Q1's detail", detail, "Dismiss this with Done")

	// Open in plan, from the script: In progress, and the answer goes to
	// the package on its plan, with an Undo that comes back there.
	code, a := e.jsonPost("/app/inbox/go", url.Values{"item": {itemID(q1)}, "return": {"/app/inbox?item=" + itemID(q1)}})
	if code != http.StatusOK || a.Location != "/app/plans/search-v2?pkg=Q1" || a.Toast == nil ||
		a.Toast.Text != "Opened Q1 in its plan · moved to In progress" || a.Toast.Undo == nil ||
		a.Toast.Undo.Action != "/app/inbox/undo" || a.Toast.Undo.Fields["return"] != a.Location {
		t.Fatalf("Open in plan = %d %+v (toast %+v)", code, a, a.Toast)
	}
	if it := e.steps()["Q1"]; it.State != inbox.InProgress {
		t.Errorf("Q1 after Open in plan = %+v", it)
	}
	moves, err := e.st.InboxMoves(context.Background(), q1.ID)
	must(t, err)
	if last := moves[len(moves)-1]; last.Text != "Go: opened in its plan" {
		t.Errorf("Q1's last move = %+v", last)
	}
	// From a plain form: a 303 to the package.
	e.api("POST", "/api/inbox/"+itemID(q2), api.InboxActRequest{Action: "seen"}, http.StatusOK)
	rec := e.post("/app/inbox/go", url.Values{"item": {itemID(q2)}, "return": {"/app/inbox"}})
	if loc := rec.Header().Get("Location"); rec.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "/app/plans/search-v2?pkg=Q2&toast=") {
		t.Errorf("plain Open in plan = %d to %q", rec.Code, loc)
	}

	// Mark done needs the page's form token, as on the plan page.
	rec = do(t, e.h, "POST", "/app/plans/search-v2/done", strings.NewReader(url.Values{"pkg": {"Q1"}, "done": {"1"}}.Encode()),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": "null", "Sec-Fetch-Site": "same-origin"})
	if rec.Code != http.StatusForbidden || e.lifecycle("Q1") != "ready" {
		t.Fatalf("Mark done without the token = %d, Q1 %s", rec.Code, e.lifecycle("Q1"))
	}
	// With it, from the inbox: back to the inbox, the item done.
	ret := "/app/inbox?item=" + itemID(q1)
	code, a = e.jsonPost("/app/plans/search-v2/done", url.Values{"pkg": {"Q1"}, "done": {"1"}, "return": {ret}})
	if code != http.StatusOK || a.Location != ret || a.Toast == nil || !strings.HasPrefix(a.Toast.Text, "Marked Q1 done (the plan file is unchanged).") ||
		a.Toast.Undo == nil || a.Toast.Undo.Action != "/app/plans/search-v2/done" || a.Toast.Undo.Fields["done"] != "0" || a.Toast.Undo.Fields["return"] != ret {
		t.Fatalf("Mark done = %d %+v (toast %+v)", code, a, a.Toast)
	}
	wantIn(t, "Q1's detail after Mark done", get(t, e.h, ret+"&fragment=detail"),
		"Done · it was marked done", "· Done: it was marked done</li>")
	wantNotIn(t, "Q1's detail after Mark done", get(t, e.h, ret+"&fragment=detail"), "Open in plan", "/app/plans/search-v2/done")
	// Q3, unblocked, is Open; Q2 is In progress since its Open in plan.
	wantIn(t, "menu after Mark done", get(t, e.h, "/app/inbox?fragment=menu"), `data-needs-count="1"`)
	if it := e.steps()["Q3"]; !it.Waiting() || it.State != inbox.Open {
		t.Errorf("Q3 after Q1's Mark done = %+v", it)
	}
	// A plain form goes back to the inbox too.
	rec = e.post("/app/plans/search-v2/done", url.Values{"pkg": {"Q1"}, "done": {"0"}, "return": {ret}})
	if loc := rec.Header().Get("Location"); rec.Code != http.StatusSeeOther || !strings.HasPrefix(loc, ret+"&toast=") {
		t.Errorf("plain Undo Mark done = %d to %q", rec.Code, loc)
	}

	// A package with a PR linked: Mark done is off, with the plan page's
	// reason.
	must(t, e.st.UpsertPRs(context.Background(), store.PR{Repo: "acme/app", Number: 5, HeadBranch: "search-v2/q1-notes", Title: "Notes (Q1)",
		State: store.PRMerged, URL: "https://github.com/acme/app/pull/5", MergedAt: t0, UpdatedAt: t0, FirstSeenAt: t0}))
	e.api("POST", "/api/links/manual", api.LinkRequest{Plan: "search-v2", Pkg: "Q1", Kind: "pr", Ref: "acme/app#5"}, http.StatusCreated)
	q1 = e.steps()["Q1"]
	if !q1.Waiting() {
		t.Fatalf("Q1 with a merged PR = %+v: your own step isn't done by a merge", q1)
	}
	wantIn(t, "Q1 with a PR", get(t, e.h, "/app/inbox?item="+itemID(q1)+"&fragment=detail"),
		`<button type="button" class="btn" disabled aria-disabled="true" title="Mark done is for steps without a PR: this one is done when its PR merges.">Mark done</button>`)
}

// Group by: under Plan, its plan's section; under Repo, the package's
// repo when its row names one, else Plans; under Session, Plan files.
func TestYourStepGroups(t *testing.T) {
	e := newStepsEnv(t)
	q1 := e.steps()["Q1"]
	byPlan := get(t, e.h, "/app/inbox?group=plan&fragment=list")
	if keys := sectionKeys(byPlan); len(keys) != 1 || keys[0] != "plan:search-v2" {
		t.Errorf("by plan: sections %v", keys)
	}
	_, plan := sectionOf(t, byPlan, "plan:search-v2")
	wantIn(t, "by plan", plan, "<span>Search v2</span><span>2</span>", "Q1 Try it in staging", "Q2 Ship the index")

	byRepo := get(t, e.h, "/app/inbox?group=repo&item="+itemID(q1)+"&fragment=list")
	if keys := sectionKeys(byRepo); strings.Join(keys, " ") != "repo:acme/app repo:-plans" {
		t.Errorf("by repo: sections %v", keys)
	}
	_, repo := sectionOf(t, byRepo, "repo:acme/app")
	wantIn(t, "by repo", repo, "<span>acme/app</span><span>1</span>", "Q2 Ship the index")
	wantSection(t, byRepo, "repo:-plans", true, true)
	_, plans := sectionOf(t, byRepo, "repo:-plans")
	wantIn(t, "by repo", plans, "<span>Plans</span><span>1</span>", "Q1 Try it in staging")

	bySession := get(t, e.h, "/app/inbox?group=session&fragment=list")
	if keys := sectionKeys(bySession); len(keys) != 1 || keys[0] != "session:-plans" {
		t.Errorf("by session: sections %v", keys)
	}
	_, files := sectionOf(t, bySession, "session:-plans")
	wantIn(t, "by session", files, "<span>Plan files</span><span>2</span>")
}

// lifecycle is a Search v2 package's lifecycle on the board now.
func (e *stepsEnv) lifecycle(id string) string {
	e.t.Helper()
	b, err := e.svc.Plan(context.Background(), "search-v2")
	must(e.t, err)
	for _, p := range b.Packages {
		if p.ID == id {
			return p.Lifecycle
		}
	}
	e.t.Fatalf("Search v2 has no package %s", id)
	return ""
}
