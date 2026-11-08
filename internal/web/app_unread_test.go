package web

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/monitor"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// unreadEnv is newPRsEnv's world with #40 linked to aim's D2 the way an
// agent's PR is when it reports it by URL before aimd has read it: the
// link API's placeholder, with only its URL known. With readErr set, the
// status says GitHub's last read of the repo failed.
func unreadEnv(t *testing.T, readErr string) *boardEnv {
	t.Helper()
	e := newPRsEnv(t, monitor.ModeOptIn)
	ctx := context.Background()
	must(t, e.st.UpsertPRs(ctx, store.PR{Repo: aimRepo, Number: 40, State: store.PROpen,
		URL: "https://github.com/" + aimRepo + "/pull/40", FirstSeenAt: t0.Add(-10 * time.Minute)}))
	e.api("POST", "/api/links/manual", api.LinkRequest{Plan: "aim", Pkg: "D2", Kind: "pr", Ref: aimRepo + "#40"}, http.StatusCreated)
	st := api.Status{Now: t0}
	if readErr != "" {
		st.GitHub.Unreadable = []api.RepoError{{Repo: aimRepo, Error: readErr, Since: t0.Add(-5 * time.Minute)}}
	}
	repos := fakeRepos{watchedRepo: aimRepo}
	mon := &monitor.Source{Mode: monitor.ModeOptIn, Store: e.st, RepoNames: repos.RepoNames,
		Plans: func(ctx context.Context) ([]api.Plan, error) {
			list, err := e.svc.Plans(ctx)
			return list.Plans, err
		}}
	e.srv = New(Options{Port: testPort, Token: testToken, Store: e.st, Plans: e.svc, Monitor: mon, Repos: repos,
		Status: fakeStatus{st}, HomeDir: e.tmp, Now: func() time.Time { return t0 }})
	e.h = e.srv.Handler()
	return e
}

// A PR aimd has never read from GitHub reads as not read yet everywhere
// it is shown, never as an open PR waiting on the lead or an agent.
func TestUnreadPRShownAsNotRead(t *testing.T) {
	e := unreadEnv(t, "")
	ref := aimRepo + "#40"

	// The PRs page: on a plan, chip "Not read yet", its age since it was
	// linked; not "Waiting on lead", which an open PR on a plan with no
	// agent at work would say.
	row := itemOf(t, get(t, e.h, "/app/prs?fragment=list"), ref)
	wantIn(t, "row", row,
		`<span class="isub">#40 · aim · D2</span>`,
		`<span class="pill tone-quiet" title="not read from GitHub yet">Not read yet</span>`,
		`<span class="iwait" title="linked 2026-11-05T11:50:00Z">10m</span>`)
	wantNotIn(t, "row", row, "Waiting on lead", "Agent working", "mergeability")

	detail := get(t, e.h, "/app/prs?pr="+url.QueryEscape(ref)+"&fragment=detail")
	wantIn(t, "detail", detail,
		"<dt>State</dt><dd>Not read from GitHub yet</dd>", "<dt>CI</dt><dd>—</dd>",
		`<dt>Package</dt><dd><a href="/app/plans/aim">aim · D2</a></dd>`,
		`<p class="dtext">Not in Needs you: not read from GitHub yet.</p>`,
		`href="https://github.com/example-user/ai-manager/pull/40"`)
	wantNotIn(t, "detail", detail, "Open ·", "No checks")

	// The package pane's PR line, and the plan's badges.
	pane := get(t, e.h, "/app/plans/aim?pkg=D2&fragment=detail")
	wantIn(t, "pane", pane, `<span class="fact fact-quiet" title="aimd knows only its URL so far: it was linked before a GitHub poll read it">not read from GitHub yet</span>`)
	wantNotIn(t, "pane", pane, `>open</span>`, "no CI", "waiting on lead", "last read failed")

	// GET /api/prs says so too.
	var prs api.PRs
	decode(t, do(t, e.h, "GET", "/api/prs", nil, nil), &prs)
	found := false
	for _, p := range prs.PRs {
		if p.Repo == aimRepo && p.Number == 40 {
			found = true
			if !p.Unread || p.Reason != "not read from GitHub yet" || p.WaitingOn != "none" || p.NeedsYou != "" {
				t.Errorf("/api/prs #40 = %+v", p)
			}
		} else if p.Unread {
			t.Errorf("/api/prs: #%d (read from GitHub) is unread", p.Number)
		}
	}
	if !found {
		t.Error("/api/prs has no #40")
	}
}

// When GitHub's last read of the repo failed, the PR says why.
func TestUnreadPRShowsReadError(t *testing.T) {
	const why = "github: NOT_FOUND: Could not resolve to a PullRequest with the number of 40."
	e := unreadEnv(t, why)
	ref := aimRepo + "#40"
	row := itemOf(t, get(t, e.h, "/app/prs?fragment=list"), ref)
	wantIn(t, "row", row, `title="not read from GitHub yet: the last read failed (`+why+`)">Not read yet</span>`)
	detail := get(t, e.h, "/app/prs?pr="+url.QueryEscape(ref)+"&fragment=detail")
	wantIn(t, "detail", detail, "<dt>State</dt><dd>Not read from GitHub yet: the last read failed ("+why+")</dd>")
	pane := get(t, e.h, "/app/plans/aim?pkg=D2&fragment=detail")
	wantIn(t, "pane", pane, `<span class="fact fact-bad" title="`+why+`">last read failed</span>`)
}
