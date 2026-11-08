package web

import (
	"context"
	"net/http"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/monitor"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// One need-you count everywhere: a plan's summary, its band, the board's
// band and the menu's per-plan badge all count the plan's Open items in
// the Needs you inbox, sessions and PRs alike (internal/prwait's PR rule,
// and review requests on any PR).
//
// The world: both plans, monitored opt-in, with
//   - sharing: its seven ready PRs (this plan merges "you");
//   - aim: its lead (registered with its session id) at a permission
//     prompt, C6's agent asking a question, and a review requested on D1's
//     red PR #21. The lead's own wait is on the plan and no package: it
//     isn't D1's, the package of the subagent it runs;
//   - no plan: a review requested on pivot#219, which no package links,
//     and a watched session at a prompt.
func TestSummaryCountsFollowTheInbox(t *testing.T) {
	e := newBoardEnv(t)
	e.monitored(monitor.ModeOptIn)
	e.registerSharing()
	e.registerAim()
	ctx := context.Background()
	e.api("POST", "/api/plans", api.RegisterRequest{Path: e.aimPlan, Repos: []string{aimRepo}, SessionID: "lead-1"}, http.StatusOK)
	must(t, e.st.UpsertSessions(ctx,
		store.Session{Key: "pid:43", SessionID: "lead-1", Kind: "interactive", Name: "aim lead", Cwd: "/w/aim", State: needsYou,
			StateReason: "permission prompt: Bash: git push", NeedsYouSince: t0.Add(-3 * time.Minute), StartedAt: t0.Add(-2 * time.Hour), LastSeenAt: t0},
		store.Session{Key: "pid:42", SessionID: "sess-c6", Kind: "interactive", Name: "c6 agent", Cwd: "/w/c6", State: needsYou,
			StateReason: "question: AskUserQuestion", NeedsYouSince: t0.Add(-2 * time.Minute), StartedAt: t0.Add(-time.Hour), LastSeenAt: t0},
		store.Session{Key: "pid:45", SessionID: "watch-1", Kind: "interactive", Name: "watched one", Cwd: "/w/x", State: needsYou,
			StateReason: "permission prompt", NeedsYouSince: t0.Add(-time.Minute), LastSeenAt: t0},
	))
	e.watchAPI("POST", "watch-1", http.StatusOK)
	_, err := e.st.SetReviewRequests(ctx, []string{"acme", "example-user"},
		[]store.PRKey{{Repo: aimRepo, Number: 21}, {Repo: "acme/pivot", Number: 219}}, t0)
	must(t, err)

	// inboxOpen counts the inbox's Open items by plan, from its API.
	inboxOpen := func() map[string]int {
		t.Helper()
		var in api.Inbox
		decode(t, do(t, e.h, "GET", "/api/inbox?tab=open", nil, nil), &in)
		out := map[string]int{}
		for _, it := range in.Items {
			out[strings.ToLower(it.Plan)]++
		}
		if n := in.Counts["open"]; n != len(in.Items) {
			t.Errorf("open count %d, items %d", n, len(in.Items))
		}
		return out
	}
	summary := func(slug string) api.PlanSummary {
		t.Helper()
		var sum api.PlanSummary
		decode(t, e.api("GET", "/api/plans/"+slug+"/summary", nil, http.StatusOK), &sum)
		return sum
	}
	menuRE := regexp.MustCompile(`href="/app/plans/([a-z]+)"[^>]*>\s*<span class="nav-label">[^<]*</span>(?:<span class="mini" title="(\d+) waiting on you">)?`)
	menu := func() map[string]int {
		t.Helper()
		out := map[string]int{}
		for _, m := range menuRE.FindAllStringSubmatch(get(t, e.h, "/app/?fragment=menu"), -1) {
			out[m[1]], _ = strconv.Atoi(m[2])
		}
		return out
	}
	check := func(when string, want map[string]int) {
		t.Helper()
		open, m := inboxOpen(), menu()
		for _, slug := range []string{"sharing", "aim"} {
			sum := summary(slug)
			var board api.Plan
			decode(t, e.api("GET", "/api/plans/"+slug, nil, http.StatusOK), &board)
			verb := map[bool]string{true: " needs you", false: " need you"}[want[slug] == 1]
			if sum.NeedsYou != want[slug] || open[slug] != want[slug] || m[slug] != want[slug] || len(sum.NeedsYouItems) != want[slug] ||
				!strings.HasSuffix(sum.Band, " · "+strconv.Itoa(want[slug])+verb) || board.Band != sum.Band {
				t.Errorf("%s, %s: summary %d (%d items, band %q, board band %q), inbox %d, menu %d; want %d",
					when, slug, sum.NeedsYou, len(sum.NeedsYouItems), sum.Band, board.Band, open[slug], m[slug], want[slug])
			}
		}
		if open[""] != want[""] {
			t.Errorf("%s: %d items on no plan, want %d", when, open[""], want[""])
		}
	}
	check("at first", map[string]int{"sharing": 7, "aim": 3, "": 2})

	// aim's reasons: each item's, on its package, oldest first. Red CI
	// alone (D1's #21 before its review request) never counted.
	sum := summary("aim")
	var reasons []string
	for _, it := range sum.NeedsYouItems {
		reasons = append(reasons, it.Pkg+": "+it.Reason)
	}
	if want := []string{": permission prompt: Bash: git push", "C6: question: AskUserQuestion", "D1: review requested on #21"}; !slices.Equal(reasons, want) {
		t.Errorf("aim's items %q, want %q", reasons, want)
	}
	var pkgs []string
	for _, p := range sum.NeedsYouPackages {
		pkgs = append(pkgs, p.ID+" "+strings.Join(p.Reasons, "; "))
	}
	if want := []string{"C6 question: AskUserQuestion", "D1 review requested on #21"}; !slices.Equal(pkgs, want) {
		t.Errorf("aim's needs-you packages %q, want %q", pkgs, want)
	}

	// sharing's lead, which runs no agent, waits on a plan approval: an
	// item on the plan and on no package, so in needs_you_items only.
	e.api("POST", "/api/plans", api.RegisterRequest{Path: e.sharing, Repos: []string{"acme/pivot", "acme/pivot-web"},
		SessionID: "lead-sp"}, http.StatusOK)
	must(t, e.st.UpsertSessions(ctx, store.Session{Key: "pid:90", SessionID: "lead-sp", Kind: "interactive", Name: "sharing lead",
		Cwd: "/w/sp", State: needsYou, StateReason: "plan approval: ExitPlanMode", NeedsYouSince: t0.Add(-4 * time.Minute), LastSeenAt: t0}))
	check("with sharing's lead waiting", map[string]int{"sharing": 8, "aim": 3, "": 2})
	sp := summary("sharing")
	if i := slices.IndexFunc(sp.NeedsYouItems, func(it api.SummaryItem) bool { return it.Title == "sharing lead" }); i < 0 ||
		sp.NeedsYouItems[i].Pkg != "" || sp.NeedsYouItems[i].Kind != "plan" || sp.NeedsYouItems[i].URL != InboxItemURL(sp.NeedsYouItems[i].ID) {
		t.Errorf("sharing's items = %+v, want the lead's plan approval on no package", sp.NeedsYouItems)
	}
	if len(sp.NeedsYouPackages) != 7 {
		t.Errorf("sharing's needs-you packages = %+v, want the 7 with a ready PR", sp.NeedsYouPackages)
	}

	// Snoozed, In progress and Done items leave the count, everywhere.
	lead := sum.NeedsYouItems[0].ID
	e.api("POST", "/api/inbox/"+strconv.FormatInt(lead, 10), api.InboxActRequest{Action: "snooze", Snooze: "1h"}, http.StatusOK)
	e.api("POST", "/api/inbox/"+strconv.FormatInt(summary("sharing").NeedsYouItems[0].ID, 10), api.InboxActRequest{Action: "done"}, http.StatusOK)
	check("after a snooze and a done", map[string]int{"sharing": 7, "aim": 2, "": 2})

	// A wait that ends leaves it at once: the c6 agent goes back to work.
	must(t, e.st.UpsertSessions(ctx, store.Session{Key: "pid:42", SessionID: "sess-c6", Kind: "interactive", Name: "c6 agent", Cwd: "/w/c6",
		State: "working", StartedAt: t0.Add(-time.Hour), LastSeenAt: t0}))
	check("after the question was answered", map[string]int{"sharing": 7, "aim": 1, "": 2})
}

// to_merge counts a lead-merged plan's PRs that are ready to merge and
// that no agent works on ("waiting on lead"); on a plan you merge a
// ready PR is a Needs you item instead, and to_merge is 0.
func TestSummaryToMerge(t *testing.T) {
	e := newBoardEnv(t)
	e.monitored(monitor.ModeOptIn)
	e.registerSharing()
	ctx := context.Background()
	pr := func(n int, pkg string) store.PR {
		return store.PR{Repo: "o/lead", Number: n, HeadBranch: "lead/" + strings.ToLower(pkg), BaseRef: "main", State: store.PROpen,
			Title: "Work (" + pkg + ")", PlanSlug: "lead", PlanPkg: pkg, URL: "https://github.com/o/lead/pull/" + strconv.Itoa(n),
			UpdatedAt: t0.Add(-time.Hour), FirstSeenAt: t0.Add(-2 * time.Hour)}
	}
	ready, red, draft := pr(1, "L1"), pr(2, "L2"), pr(3, "L3")
	ready.MergeStateStatus, ready.ChecksState = "CLEAN", "SUCCESS"
	red.MergeStateStatus, red.ChecksState = "UNSTABLE", "FAILURE"
	draft.MergeStateStatus, draft.Draft = "DRAFT", true
	must(t, e.st.UpsertPRs(ctx, ready, red, draft))
	plan := e.write(filepath.Join(e.root, "lead", "lead-plan.md"), "# Lead plan\n\nMerges: lead\n\n| Package | Repo | Needs |\n|---|---|---|\n"+
		"| **L1 One** | lead | — |\n| **L2 Two** | lead | — |\n| **L3 Three** | lead | — |\n")
	e.api("POST", "/api/plans", api.RegisterRequest{Path: plan, Repos: []string{"o/lead"}}, http.StatusCreated)

	rec := e.api("GET", "/api/plans/lead/summary", nil, http.StatusOK)
	var sum api.PlanSummary
	decode(t, rec, &sum)
	if sum.Merges != "lead" || sum.ToMerge != 1 || !slices.Equal(sum.ToMergePRs, []string{"o/lead#1"}) || sum.NeedsYou != 0 {
		t.Errorf("lead plan: merges %q, to merge %d %v, needs you %d; want lead, 1 [o/lead#1], 0", sum.Merges, sum.ToMerge, sum.ToMergePRs, sum.NeedsYou)
	}
	wantIn(t, "summary JSON", rec.Body.String(), `"to_merge":1,"to_merge_prs":["o/lead#1"]`, `"needs_you":0,"needs_you_items":[]`)
	// The PRs page agrees: #1 waits on the lead.
	wantIn(t, "PRs page", get(t, e.h, "/app/prs?pr=o/lead%231&fragment=detail"), "Waiting on lead")

	// On a plan you merge, a ready PR needs you and none is to merge.
	decode(t, e.api("GET", "/api/plans/sharing/summary", nil, http.StatusOK), &sum)
	if sum.Merges != "you" || sum.ToMerge != 0 || len(sum.ToMergePRs) != 0 || sum.NeedsYou != 7 {
		t.Errorf("sharing: merges %q, to merge %d %v, needs you %d; want you, 0, 7", sum.Merges, sum.ToMerge, sum.ToMergePRs, sum.NeedsYou)
	}

	// An agent still working on L1: its PR waits on the agent, not the lead.
	must(t, e.st.UpsertSessions(ctx, store.Session{Key: "pid:80", SessionID: "sess-l1", Kind: "interactive", Name: "l1 agent", Cwd: "/w/l1",
		State: "working", StartedAt: t0.Add(-time.Hour), LastSeenAt: t0}))
	e.api("POST", "/api/links/manual", api.LinkRequest{Plan: "lead", Pkg: "L1", Kind: "agent", Ref: "sess-l1"}, http.StatusCreated)
	decode(t, e.api("GET", "/api/plans/lead/summary", nil, http.StatusOK), &sum)
	if sum.ToMerge != 0 || len(sum.ToMergePRs) != 0 {
		t.Errorf("with L1's agent working: to merge %d %v, want 0", sum.ToMerge, sum.ToMergePRs)
	}
}

// An expired background job never needs you: GET /api/status,
// the inbox and the count without an inbox (needsYouTotal) leave it out.
func TestExpiredJobsNeverNeedYou(t *testing.T) {
	status := func(e *boardEnv) api.Counts {
		t.Helper()
		var st api.Status
		decode(t, do(t, e.h, "GET", "/api/status", nil, nil), &st)
		return st.Counts
	}
	total := func(e *boardEnv) int {
		t.Helper()
		sn, err := e.srv.load(context.Background())
		must(t, err)
		return needsYouTotal(sn)
	}

	// job:ex1 is watched, blocked and expired; pid:45 and pid:50 are
	// watched and waiting.
	e, _ := newSessionsEnv(t, monitor.ModeOptIn, nil)
	c := status(e)
	if c.SessionsNeedingYou != 2 || !c.OldestNeedsYouSince.Equal(t0.Add(-time.Hour)) {
		t.Errorf("status: %d sessions need you, oldest since %v; want 2, since pid:50's %v", c.SessionsNeedingYou, c.OldestNeedsYouSince, t0.Add(-time.Hour))
	}
	n := total(e)
	wantNotIn(t, "inbox", get(t, e.h, "/app/inbox?fragment=list"), "old watched job")

	// Without the expiry source the job counts: so the filter is what
	// leaves it out.
	without, _ := newSessionsEnv(t, monitor.ModeOptIn, func(o *Options) { o.Jobs = nil })
	if c := status(without); c.SessionsNeedingYou != 3 {
		t.Errorf("status without Jobs: %d sessions need you, want 3", c.SessionsNeedingYou)
	}
	if m := total(without); m != n+1 {
		t.Errorf("needsYouTotal without Jobs: %d, with Jobs %d", m, n)
	}
	wantIn(t, "inbox without Jobs", get(t, without.h, "/app/inbox?fragment=list"), "old watched job")
}
