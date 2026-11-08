package web

import (
	"context"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/inbox"
	"github.com/gravexa-llc/ai-manager/internal/monitor"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// The provisional packages of these tests: Z9 named by a spawn brief
// of session lead-z (not running yet), Z8 by an open PR's Plan: line.
const (
	z9Row = "| **Z9 Add the thing** |  |  |  |"
	z8Row = "| **Z8 Later thing** |  |  |  |"
)

// provisional makes Z9 and Z8 on the aim plan, at t0.
func (e *planEnv) provisional() {
	e.t.Helper()
	must(e.t, e.st.UpsertPRs(context.Background(), store.PR{Repo: aimRepo, Number: 901, HeadBranch: "aim/z8-later", BaseRef: "main",
		Title: "Later thing (Z8)", State: store.PROpen, PlanSlug: "aim", PlanPkg: "Z8", URL: "https://github.com/" + aimRepo + "/pull/901",
		UpdatedAt: t0, FirstSeenAt: t0}))
	e.api("POST", "/api/links/spawn", api.SpawnLinkRequest{SessionID: "lead-z", AgentID: "agent-z9", Plan: "aim", Pkg: "Z9",
		Provisional: true, Title: "Z9: add the thing"}, http.StatusCreated)
}

// at rebuilds the server as serve does, with its clock at now.
func (e *planEnv) at(now time.Time) {
	e.t.Helper()
	mon := &monitor.Source{Mode: monitor.ModeAll, Store: e.st, Plans: func(ctx context.Context) ([]api.Plan, error) {
		list, err := e.svc.Plans(ctx)
		return list.Plans, err
	}}
	e.srv = New(Options{Port: testPort, Token: testToken, Store: e.st, Plans: e.svc, Terminals: e.terms, Monitor: mon, Go: e.g,
		HomeDir: e.tmp, Now: func() time.Time { return now }})
	e.h = e.srv.Handler()
}

var rectXRE = regexp.MustCompile(`<rect class="pg-box" x="(\d+)"`)

// boxX is the x of a package's box in the graph.
func boxX(t *testing.T, graph, id string) int {
	t.Helper()
	m := rectXRE.FindStringSubmatch(itemOf(t, graph, id))
	if m == nil {
		t.Fatalf("no box for %s", id)
	}
	x, err := strconv.Atoi(m[1])
	must(t, err)
	return x
}

// The plan page shows the provisional packages in every view and in their
// detail pane, in both themes, with no inline code and no raw colors.
func TestPlanPageProvisional(t *testing.T) {
	e := newPlanEnv(t)
	e.provisional()
	ctx := context.Background()

	for _, theme := range []string{store.ThemeLight, store.ThemeDark} {
		_, err := e.st.SetSettings(ctx, map[string]string{store.SettingTheme: theme})
		must(t, err)
		where := "theme " + theme

		page := get(t, e.h, "/app/plans/aim")
		wantIn(t, where, htmlTag(t, page), `data-theme="`+theme+`"`)
		checkNoInline(t, where, page)
		wantIn(t, where, page, "· 2 not in the plan file", `#plan-provisional" data-pane="list detail">2 not in plan file</a>`)

		// The graph: both in a last column of their own, dotted, Z9 with its badge.
		graph := inner(t, page, "list")
		z9, z8 := itemOf(t, graph, "Z9"), itemOf(t, graph, "Z8")
		wantIn(t, where+" Z9 node", z9, `class="pg-node lc-ready prov"`, `pg-badge b-prov`, ">not in plan file</text>")
		wantIn(t, where+" Z8 node", z8, ` prov"`)
		last := boxX(t, graph, "Z9")
		if boxX(t, graph, "Z8") != last {
			t.Errorf("%s: Z8 and Z9 aren't in one column", where)
		}
		for _, m := range dataItemRE.FindAllStringSubmatch(graph, -1) {
			if id := m[1]; id != "Z9" && id != "Z8" && boxX(t, graph, id) >= last {
				t.Errorf("%s: %s is in the provisional column or right of it", where, id)
			}
		}

		// Kanban: their real lifecycle, a dashed card and the badge.
		kanban := inner(t, get(t, e.h, "/app/plans/aim?view=kanban"), "list")
		wantIn(t, where+" Z9 card", itemOf(t, kanban, "Z9"), `class="kcard lc-ready prov"`, `<span class="badge b-prov"`, "not in plan file")
		wantIn(t, where+" Z8 card", itemOf(t, kanban, "Z8"), `class="kcard lc-in_review prov"`)

		// Timeline: a row with the badge.
		tl := inner(t, get(t, e.h, "/app/plans/aim?view=timeline"), "list")
		wantIn(t, where+" Z9 row", itemOf(t, tl, "Z9"), `class="tl-row lc-ready prov"`, `<span class="badge b-prov">not in plan file</span>`)

		// The detail pane: what's missing, the row with Copy, what named
		// it, its links and Go.
		detail := get(t, e.h, "/app/plans/aim?pkg=Z9&fragment=detail")
		checkNoInline(t, where+" detail", detail)
		wantIn(t, where+" Z9 detail", detail,
			"<h2>Add the thing</h2>", "Not in ROADMAP.md yet",
			`<pre class="code" id="pd-row-Z9">`+z9Row+`</pre>`,
			`<button type="button" class="btn sm" data-copy="pd-row-Z9"`,
			"a spawn brief&#39;s Package: line, agent lead-z/agent-z9 (“Add the thing”)",
			"Go to agent", "Links (1) and how they were made",
		)
		wantIn(t, where+" Z8 detail", get(t, e.h, "/app/plans/aim?pkg=Z8&fragment=detail"),
			`<pre class="code" id="pd-row-Z8">`+z8Row+`</pre>`, "the Plan: line of PR example-user/ai-manager#901 (“Later thing”)",
			"example-user/ai-manager#901")
		// A package of the file says nothing of it.
		wantNotIn(t, where+" D1 detail", get(t, e.h, "/app/plans/aim?pkg=D1&fragment=detail"), "Not in ROADMAP.md", "data-copy")

		// The plan's pane lists them.
		overview := get(t, e.h, "/app/plans/aim?fragment=detail")
		wantIn(t, where+" plan pane", section(t, overview, "plan-provisional"), "Not in ROADMAP.md yet")
		wantIn(t, where+" plan pane", overview, `href="/app/plans/aim?view=graph&amp;pkg=Z9"`, `href="/app/plans/aim?view=graph&amp;pkg=Z8"`)
	}
}

// A provisional package still without its row after inbox.PlanRowAfter is
// a Needs you item, once its plan has a monitored lead (the plan's, or the
// session that spawned for it); the item closes by itself when the row
// lands, or when you Mark the package done.
func TestInboxPlanRow(t *testing.T) {
	e := newPlanEnv(t)
	e.provisional()
	ctx := context.Background()
	items := func(now time.Time) map[string]store.InboxItem {
		t.Helper()
		e.at(now)
		_, err := e.srv.SyncInbox(ctx)
		must(t, err)
		list, err := e.st.ListInbox(ctx, store.InboxFilter{})
		must(t, err)
		out := map[string]store.InboxItem{}
		for _, it := range list {
			if it.Kind == inbox.KindPlanRow { // not the plan's your steps (aimSteps)
				out[it.Pkg] = it
			}
		}
		return out
	}

	if got := items(t0.Add(5 * time.Minute)); len(got) != 0 {
		t.Errorf("items before PlanRowAfter: %+v", got)
	}
	later := t0.Add(inbox.PlanRowAfter + 5*time.Minute)
	if got := items(later); len(got) != 0 {
		t.Errorf("items with no monitored lead: %+v", got)
	}

	// The session that spawned Z9's agent runs (and, in "all" mode, is
	// monitored): Z9's item opens.
	must(t, e.st.UpsertSessions(ctx, store.Session{Key: "pid:77", SessionID: "lead-z", Kind: "interactive", Name: "z lead", Cwd: "/w/z",
		State: "working", StartedAt: t0, LastSeenAt: later}))
	got := items(later)
	z9, ok := got["Z9"]
	if len(got) != 1 || !ok {
		t.Fatalf("items once lead-z runs: %+v", got)
	}
	if z9.Kind != inbox.KindPlanRow || z9.State != inbox.Open || z9.Ref != "planrow:aim/Z9" || z9.PlanSlug != "aim" ||
		z9.Title != "Z9 isn't in ROADMAP.md" || z9.Detail != z9Row || !z9.Since.Equal(t0.Add(inbox.PlanRowAfter)) {
		t.Errorf("Z9's item = %+v", z9)
	}
	page := get(t, e.h, "/app/inbox?item="+strconv.FormatInt(z9.ID, 10))
	checkNoInline(t, "inbox", page)
	wantIn(t, "Z9's item", page, "Plan row missing", "Suggested row for ROADMAP.md",
		`<pre class="code" id="planrow-`+strconv.FormatInt(z9.ID, 10)+`">`+z9Row+`</pre>`,
		`data-copy="planrow-`+strconv.FormatInt(z9.ID, 10)+`"`, `href="/app/plans/aim?pkg=Z9"`)
	wantNotIn(t, "Z9's item", section(t, page, "detail"), `action="/app/inbox/go"`)
	// Grouped by session, it comes from no session: Plan files (with the
	// plan's your steps), after the sessions, open as it holds the
	// selected item.
	bySession := get(t, e.h, "/app/inbox?group=session&item="+strconv.FormatInt(z9.ID, 10)+"&fragment=list")
	if keys := sectionKeys(bySession); len(keys) == 0 || keys[len(keys)-1] != "session:-plans" {
		t.Errorf("grouped by session: sections %v", keys)
	}
	wantSection(t, bySession, "session:-plans", true, true)
	_, planFiles := sectionOf(t, bySession, "session:-plans")
	wantIn(t, "Plan files", planFiles, `<span>Plan files</span><span>`+strconv.Itoa(1+aimSteps)+`</span>`, "Z9 isn&#39;t in ROADMAP.md")
	// It counts on the plan's summary.
	sum, err := e.svc.Summary(ctx, "aim", time.Time{})
	must(t, err)
	if len(sum.NeedsYouItems) == 0 || sum.NeedsYouItems[len(sum.NeedsYouItems)-1].Reason != "plan row missing: Z9 isn't in ROADMAP.md" {
		t.Errorf("summary items = %+v", sum.NeedsYouItems)
	}

	// The plan gets a monitored lead: Z8's item opens too.
	p, err := e.st.GetPlanBySlug(ctx, "aim")
	must(t, err)
	_, err = e.st.MonitorPlanLead(ctx, "lead-1", p.ID, t0)
	must(t, err)
	if got = items(later); len(got) != 2 || got["Z8"].State != inbox.Open || got["Z8"].Detail != z8Row {
		t.Fatalf("items once the plan has a lead: %+v", got)
	}

	// Z9's row lands: its item closes by itself; Z8 still waits.
	src, err := os.ReadFile(e.aimPlan)
	must(t, err)
	e.write(e.aimPlan, string(src)+"\n| Package | What | Size | Needs |\n|---|---|---|---|\n| **Z9 Add the thing** | It. | S | — |\n")
	e.api("POST", "/api/plans", api.RegisterRequest{Path: e.aimPlan, Repos: []string{aimRepo}}, http.StatusOK)
	got = items(later.Add(time.Minute))
	if it := got["Z9"]; it.Waiting() || it.State != inbox.Done || it.EndReason != "its row is in ROADMAP.md now" {
		t.Errorf("Z9's item after its row landed = %+v", it)
	}
	if !got["Z8"].Waiting() {
		t.Errorf("Z8's item stopped: %+v", got["Z8"])
	}

	// Marking Z8 done closes its item.
	e.api("POST", "/api/plans/aim/packages/Z8/done", nil, http.StatusOK)
	if it := items(later.Add(2 * time.Minute))["Z8"]; it.Waiting() || it.EndReason != "it was marked done" {
		t.Errorf("Z8's item after Mark done = %+v", it)
	}
}
