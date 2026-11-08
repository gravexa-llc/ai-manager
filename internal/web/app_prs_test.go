package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/monitor"
	"github.com/gravexa-llc/ai-manager/internal/prwait"
	"github.com/gravexa-llc/ai-manager/internal/store"
)

// newPRsEnv is newMonitorEnv's world (aim's PRs: #21 D1 with red CI and
// its agent at work, #22 C4 a draft, #23 D3, #30 a ready stray, #31 on
// the watched session's branch) with PRs on no plan: example-org/brand#6,
// your review requested; o/web#3, a draft untouched for 40 days; o/web#4,
// red CI. The watched session has finished its turn, so #31, ready on no
// plan, waits on you.
func newPRsEnv(t *testing.T, mode string) *boardEnv {
	t.Helper()
	e := newMonitorEnv(t, mode)
	ctx := context.Background()
	must(t, e.st.UpsertPRs(ctx,
		store.PR{Repo: "example-org/brand", Number: 6, Title: "Brand refresh", State: store.PROpen, HeadBranch: "brand-v2",
			MergeStateStatus: "BLOCKED", ReviewDecision: "REVIEW_REQUIRED", URL: "https://github.com/example-org/brand/pull/6",
			Author: "someone", UpdatedAt: t0.Add(-5 * time.Hour)},
		store.PR{Repo: "o/web", Number: 3, Title: "Parked draft", State: store.PROpen, Draft: true, HeadBranch: "parked",
			MergeStateStatus: "DRAFT", URL: "https://github.com/o/web/pull/3", UpdatedAt: t0.Add(-40 * 24 * time.Hour)},
		store.PR{Repo: "o/web", Number: 4, Title: "Red build", State: store.PROpen, HeadBranch: "red",
			MergeStateStatus: "UNSTABLE", ChecksState: "FAILURE", URL: "https://github.com/o/web/pull/4", UpdatedAt: t0.Add(-3 * time.Hour)},
	))
	_, err := e.st.SetReviewRequests(ctx, []string{"example-org"}, []store.PRKey{{Repo: "example-org/brand", Number: 6}}, t0.Add(-2*time.Hour))
	must(t, err)
	rows, err := e.st.ListSessions(ctx, store.SessionFilter{})
	must(t, err)
	for _, row := range rows {
		if row.Key == "pid:45" {
			row.State, row.StateReason, row.NeedsYouSince = "idle", "idle", time.Time{}
			must(t, e.st.UpsertSessions(ctx, row))
		}
	}
	return e
}

func TestAppPRsGroups(t *testing.T) {
	e := newPRsEnv(t, monitor.ModeOptIn)
	list := get(t, e.h, "/app/prs?fragment=list")

	// On a plan or a monitored session: what needs you first, then the
	// rest by their last update (newest first; ties by repo, then newest
	// number).
	on := groupOf(t, list, `<span>On a plan or a monitored session</span><span>4</span>`)
	if got := strings.Join(rowKeys(on), " "); got != "example-user/ai-manager#31 example-user/ai-manager#23 example-user/ai-manager#22 example-user/ai-manager#21" {
		t.Errorf("on a plan = %s", got)
	}
	wantIn(t, "on a plan", on,
		`<span class="isub">example-user/ai-manager #31 · a monitored session&#39;s branch</span><span class="ititle">Watched work</span>`,
		`<span class="pill tone-warn" title="ready to merge · on no plan, so you merge it">Waiting on you</span>`,
		`<span class="isub">example-user/ai-manager #22 · aim · C4</span>`,
		`<span class="pill tone-lead" title="draft · no agent is working on it, so it&#39;s for this plan&#39;s lead">Waiting on lead</span>`,
		`<span class="pill tone-info" title="CI failing · an agent is still working on it">Agent working</span>`,
		`<span class="iwait" title="updated 2026-11-05T10:00:00Z">2h</span>`)

	// Not on a plan: the review request first (it needs you), then the
	// rest by their last update.
	off := groupOf(t, list, `<span>Not on a plan</span><span>4</span>`)
	if got := strings.Join(rowKeys(off), " "); got != "example-org/brand#6 example-user/ai-manager#30 o/web#4 o/web#3" {
		t.Errorf("not on a plan = %s", got)
	}
	wantIn(t, "not on a plan", off,
		`<span class="isub">example-org/brand #6 · not on a plan</span><span class="ititle">Brand refresh</span>`,
		`<span class="pill tone-warn" title="your review is requested">Review requested</span>`,
		`<span class="iwait" title="review requested since 2026-11-05T10:00:00Z">2h</span>`,
		`<span class="pill tone-quiet" title="ready to merge, but not on a plan or a monitored session&#39;s branch: nobody waits on it">Ready</span>`,
		`<span class="tag">CI failing</span>`,
		`<span class="pill tone-quiet" title="a draft with no update for 30 days or more">Stale draft</span>`)
	// Merged and closed PRs aren't listed.
	wantNotIn(t, "list", list, `data-item="example-user/ai-manager#7"`)

	wantIn(t, "summary", get(t, e.h, "/app/prs?fragment=summary"), "<p>8 open · 2 waiting on you</p>")
	wantIn(t, "menu", get(t, e.h, "/app/prs?fragment=menu"), `Pull requests<span class="mini" title="8 open">`)

	// "all" mode counts every PR: the first group is the ones on a plan.
	all := get(t, newPRsEnv(t, monitor.ModeAll).h, "/app/prs?fragment=list")
	on = groupOf(t, all, "On a plan or a monitored session")
	wantIn(t, "all mode", on, `data-item="example-user/ai-manager#21"`, `data-item="example-user/ai-manager#22"`)
	wantNotIn(t, "all mode", on, `data-item="o/web#4"`, `data-item="example-org/brand#6"`)
}

func TestAppPRsSelection(t *testing.T) {
	e := newPRsEnv(t, monitor.ModeOptIn)
	ref := url.QueryEscape("example-user/ai-manager#21")
	sel := `<a class="srow sel" href="/app/prs?pr=` + ref + `" data-pane="detail" data-item="example-user/ai-manager#21" aria-current="true" data-search="aim/d1-events">`
	detail := []string{
		`<span class="pill tone-info" title="CI failing · an agent is still working on it">Agent working</span>`,
		"<h2>Event receiver and live state (D1)</h2>",
		`<p class="subtitle">example-user/ai-manager #21 · <code>aim/d1-events</code></p>`,
		"<dt>State</dt><dd>Open · CI failing</dd>", "<dt>CI</dt><dd>Failing</dd>",
		`<dt>Package</dt><dd><a href="/app/plans/aim">aim · D1</a></dd>`, "<dt>Merges</dt><dd>You (plan setting)</dd>",
		`<dt>Updated</dt><dd title="2026-11-05T11:00:00Z">1h ago</dd>`,
		`<p class="dtext">Not in Needs you: CI failing · an agent is still working on it.</p>`,
		`<a class="btn primary" href="https://github.com/example-user/ai-manager/pull/21" rel="noreferrer noopener" target="_blank" data-key="g" aria-keyshortcuts="g">Open on GitHub <kbd>g</kbd></a>`,
	}
	page := get(t, e.h, "/app/prs?pr="+ref)
	wantIn(t, "page list", section(t, page, "list"), sel)
	wantIn(t, "page detail", section(t, page, "detail"), detail...)
	wantIn(t, "list fragment", get(t, e.h, "/app/prs?pr="+ref+"&fragment=list"), sel)
	wantIn(t, "detail fragment", get(t, e.h, "/app/prs?pr="+ref+"&fragment=detail"), detail...)
	// The repo's case doesn't matter.
	wantIn(t, "case", get(t, e.h, "/app/prs?pr="+url.QueryEscape("Example-User/AI-Manager#21")+"&fragment=detail"), "<h2>Event receiver and live state (D1)</h2>")

	// The review request on no plan: in Needs you, nobody merges it.
	brand := get(t, e.h, "/app/prs?pr="+url.QueryEscape("example-org/brand#6")+"&fragment=detail")
	wantIn(t, "brand#6", brand, `<span class="pill tone-warn" title="your review is requested">Review requested</span>`,
		"<dt>Package</dt><dd>—</dd>", "<dt>Merges</dt><dd>—</dd>", "<dt>State</dt><dd>Open · review required</dd>", "<dt>CI</dt><dd>No checks</dd>",
		`<p class="subtitle">example-org/brand #6 · <code>brand-v2</code> · by someone</p>`,
		`<p class="dtext">In Needs you: your review is requested. That counts on any PR, even one on no plan.</p>`)
	// Ready on a monitored session's branch, on no plan: you merge it.
	wantIn(t, "#31", get(t, e.h, "/app/prs?pr="+url.QueryEscape("example-user/ai-manager#31")+"&fragment=detail"),
		"<dt>Merges</dt><dd>You (on no plan, so nobody else would)</dd>", "<dt>State</dt><dd>Open · ready to merge</dd>",
		`<p class="dtext">In Needs you: ready to merge · on no plan, so you merge it.</p>`)
	// A ready PR nobody waits on.
	wantIn(t, "#30", get(t, e.h, "/app/prs?pr="+url.QueryEscape("example-user/ai-manager#30")+"&fragment=detail"),
		`<p class="dtext">Not in Needs you: not on a registered plan or a monitored session&#39;s branch.</p>`)
	// A merged PR still has its detail, from a deep link.
	wantIn(t, "merged", get(t, e.h, "/app/prs?pr="+url.QueryEscape("example-user/ai-manager#7")+"&fragment=detail"),
		`<span class="tag">Merged</span>`, "<dt>State</dt><dd>Merged</dd>", `<p class="dtext">Not in Needs you: merged.</p>`)

	// Nothing selected, a PR aimd doesn't know, and a bad ref.
	wantIn(t, "none", get(t, e.h, "/app/prs?fragment=detail"), "Select a pull request to see its state, CI and who merges it.")
	wantIn(t, "unknown", get(t, e.h, "/app/prs?pr="+url.QueryEscape("o/nope#9")+"&fragment=detail"), "aimd doesn&#39;t know o/nope#9")
	for _, bad := range []string{"nope", "o#1", "o/x#0", "o/x#abc"} {
		wantIn(t, bad, get(t, e.h, "/app/prs?pr="+url.QueryEscape(bad)+"&fragment=detail"), "isn&#39;t a pull request")
	}

	// The search box, kept in the rows' links.
	found := get(t, e.h, "/app/prs?q=brand&fragment=list")
	if got := strings.Join(rowKeys(found), " "); got != "example-org/brand#6" {
		t.Errorf("search = %s", got)
	}
	wantIn(t, "search", found, `href="/app/prs?pr=example-org%2Fbrand%236&amp;q=brand"`)
	wantIn(t, "search none", get(t, e.h, "/app/prs?q=zzz&fragment=list"), "No open pull request matches “zzz”.")
	wantIn(t, "no PRs", get(t, newDash(t, newTestStore(t), nil).Handler(), "/app/prs?fragment=list"), "No open pull requests.")
}

func TestWhoChips(t *testing.T) {
	open := store.PR{State: store.PROpen, UpdatedAt: t0.Add(-time.Hour)}
	ready := open
	ready.MergeStateStatus = "CLEAN"
	red := open
	red.ChecksState, red.MergeStateStatus = "FAILURE", "UNSTABLE"
	stale := open
	stale.Draft, stale.MergeStateStatus, stale.UpdatedAt = true, "DRAFT", t0.Add(-31*24*time.Hour)
	merged := store.PR{State: store.PRMerged}
	for _, c := range []struct {
		name    string
		p       store.PR
		v       prwait.Verdict
		snoozed string
		want    string
	}{
		{"ready, you merge", ready, prwait.Verdict{NeedsYou: prwait.ReadyToMerge, WaitingOn: prwait.WaitingYou}, "", "Waiting on you tone-warn"},
		{"review requested", open, prwait.Verdict{NeedsYou: prwait.ReviewRequested, WaitingOn: prwait.WaitingYou}, "", "Review requested tone-warn"},
		{"snoozed in the inbox", ready, prwait.Verdict{NeedsYou: prwait.ReadyToMerge, WaitingOn: prwait.WaitingYou}, "until 9am", "Snoozed tone-quiet"},
		{"a lead-merged plan", ready, prwait.Verdict{WaitingOn: prwait.WaitingLead, Monitored: true}, "", "Waiting on lead tone-lead"},
		{"an agent at work", red, prwait.Verdict{WaitingOn: prwait.WaitingAgent, Monitored: true}, "", "Agent working tone-info"},
		{"a stale draft", stale, prwait.Verdict{WaitingOn: prwait.WaitingNone}, "", "Stale draft tone-quiet"},
		{"ready, not counted", ready, prwait.Verdict{WaitingOn: prwait.WaitingNone}, "", "Ready tone-quiet"},
		{"red, not counted", red, prwait.Verdict{WaitingOn: prwait.WaitingNone}, "", "CI failing  plain"},
		{"merged", merged, prwait.Verdict{WaitingOn: prwait.WaitingNone}, "", "Merged tone-quiet plain"},
		// Snoozing only applies to what needs you.
		{"snoozed, but not needing you", ready, prwait.Verdict{WaitingOn: prwait.WaitingNone}, "until 9am", "Ready tone-quiet"},
	} {
		w := whoChipOf(t0, c.p, c.v, c.snoozed)
		got := w.Label + " " + w.Tone
		if w.Plain {
			got += " plain"
		}
		if got != c.want {
			t.Errorf("%s: chip %q, want %q", c.name, got, c.want)
		}
	}
	if got := needsYouWhy(prwait.Verdict{NeedsYou: prwait.ReadyToMerge, Reason: "ready to merge · this plan merges: you"}, "until 9am"); got !=
		"In Needs you, snoozed until 9am: ready to merge · this plan merges: you." {
		t.Errorf("snoozed why = %q", got)
	}
}

// TestAppPRsSnoozed snoozes a PR's item in the Needs you inbox: the
// page shows it Snoozed, and its detail links the item.
func TestAppPRsSnoozed(t *testing.T) {
	e := newPRsEnv(t, monitor.ModeOptIn)
	get(t, e.h, "/app/prs") // the menu's count runs a sync pass: the items open
	items, err := e.st.ListInbox(context.Background(), store.InboxFilter{Waiting: true})
	must(t, err)
	var id int64
	for _, it := range items {
		if it.Ref == "example-org/brand#6" {
			id = it.ID
		}
	}
	if id == 0 {
		t.Fatalf("no inbox item for brand#6 in %+v", items)
	}
	ref := url.QueryEscape("example-org/brand#6")
	wantIn(t, "open item", get(t, e.h, "/app/prs?pr="+ref+"&fragment=detail"),
		`<a class="btn" href="/app/inbox?item=`+strconv.FormatInt(id, 10)+`">Open in Needs you</a>`)

	rec := e.post("/app/inbox/snooze", url.Values{"item": {strconv.FormatInt(id, 10)}, "snooze": {"1h"}, "return": {"/app/inbox"}})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("snooze = %d %s", rec.Code, rec.Body)
	}
	wantIn(t, "snoozed row", groupOf(t, get(t, e.h, "/app/prs?fragment=list"), "Not on a plan"),
		`<span class="pill tone-quiet" title="snoozed until 13:00">Snoozed</span>`)
	wantIn(t, "snoozed detail", get(t, e.h, "/app/prs?pr="+ref+"&fragment=detail"),
		`<p class="dtext">In Needs you, snoozed until 13:00: your review is requested.</p>`, "Open in Needs you")
	// Its neighbour isn't snoozed.
	wantIn(t, "#31", groupOf(t, get(t, e.h, "/app/prs?fragment=list"), "On a plan or a monitored session"), ">Waiting on you</span>")
	// A PR that doesn't need you has no item to link.
	wantNotIn(t, "#21", get(t, e.h, "/app/prs?pr="+url.QueryEscape("example-user/ai-manager#21")+"&fragment=detail"), "Open in Needs you")
}

func TestAppPRsEscapes(t *testing.T) {
	e := newPRsEnv(t, monitor.ModeOptIn)
	evil := `<img src=x onerror=alert(1)>`
	must(t, e.st.UpsertPRs(context.Background(), store.PR{Repo: "o/evil", Number: 1, Title: evil, HeadBranch: evil, Author: evil,
		State: store.PROpen, URL: "javascript:alert(1)", UpdatedAt: t0}))
	for _, target := range []string{"/app/prs", "/app/prs?pr=" + url.QueryEscape("o/evil#1"), "/app/prs?q=" + url.QueryEscape(evil)} {
		body := get(t, e.h, target)
		wantNotIn(t, target, body, "<img src=x", "javascript:alert")
	}
}
