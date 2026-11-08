package planapi_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// searchPlan is a plan with a What column: a short cell, a long one with
// markup, and an empty one.
const searchPlan = `# Search v2

Plan slug: search-v2

| Package | What | Needs |
|---|---|---|
| **A1 Index builder** | Builds the ` + "`search`" + ` index **nightly** from the catalog. | — |
| **A2 Query API** | | A1 |
`

// registerSearch registers searchPlan in the pivot repo.
func (e *env) registerSearch() string {
	e.t.Helper()
	path := e.pivot + "/docs/search-plan.md"
	e.write(path, searchPlan)
	ok[api.RegisterResponse](e.t, e.req("POST", "/api/plans", api.RegisterRequest{Path: path, Repos: []string{"acme/pivot"}}), http.StatusCreated)
	return path
}

// The board carries the What cell as written.
func TestBoardWhat(t *testing.T) {
	e := newEnv(t)
	e.registerSearch()
	b := e.board("search-v2")
	if got := pkgOf(t, b, "A1").What; got != "Builds the `search` index **nightly** from the catalog." {
		t.Errorf("A1 what = %q", got)
	}
	if got := pkgOf(t, b, "A2").What; got != "" {
		t.Errorf("A2 what = %q", got)
	}
}

// A plan registered by an aimd that didn't keep the What column gets it
// when aimd starts, though its file hasn't changed: Run re-parses the
// stored copy. Links and Mark done stay.
func TestRunFillsWhatOfAnUnchangedPlan(t *testing.T) {
	e := newEnv(t)
	e.registerSearch()
	ok[api.DoneResult](t, e.req("POST", "/api/plans/search-v2/packages/A1/done", nil), http.StatusOK)
	ctx := context.Background()
	p, err := e.st.GetPlanBySlug(ctx, "search-v2")
	must(t, err)
	// As an older aimd stored it: no What.
	pkg, err := e.st.GetPackage(ctx, p.ID, "A1")
	must(t, err)
	pkg.What = ""
	must(t, e.st.UpdatePackage(ctx, pkg))

	e.run()
	eventually(t, "A1's What stored", func() bool {
		pkg, err := e.st.GetPackage(ctx, p.ID, "A1")
		return err == nil && strings.Contains(pkg.What, "nightly")
	})
	pkg, err = e.st.GetPackage(ctx, p.ID, "A1")
	must(t, err)
	if !pkg.DoneManual {
		t.Errorf("A1 lost its Mark done: %+v", pkg)
	}
}

// HideUnmatched takes a PR or branch off one plan's Unmatched lane and
// restores it; it links nothing, and says when nothing changed.
func TestHideUnmatched(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	stray := store.PR{Repo: "acme/pivot", Number: 45, HeadBranch: "fix/readme", BaseRef: "main", Title: "Fix a typo in the README",
		State: store.PROpen, CreatedAt: time.Now().Add(-90 * 24 * time.Hour)}
	must(t, e.st.UpsertPRs(ctx, stray))
	e.registerSearch()
	e.registerSharing()
	ref := store.PRRef("acme/pivot", 45)

	b := e.board("search-v2")
	u, found := unmatchedOf(b.Unmatched, ref)
	if !found || u.New || u.Since.IsZero() {
		t.Fatalf("#45 before hiding: %+v (found %v), want listed, not new, with since", u, found)
	}

	before := e.notes.count(api.TopicPlans)
	res, err := e.svc.HideUnmatched(ctx, api.HideRequest{Plan: "search-v2", Kind: "pr", Ref: "https://github.com/acme/pivot/pull/45", Hidden: true})
	must(t, err)
	if res.Ref != ref || !res.Hidden || !res.Changed || res.Plan != "search-v2" {
		t.Errorf("hide = %+v", res)
	}
	b = e.board("search-v2")
	if _, found := unmatchedOf(b.Unmatched, ref); found {
		t.Error("#45 still unmatched after hiding")
	}
	if h, found := unmatchedOf(b.HiddenUnmatched, ref); !found || h.HiddenAt.IsZero() {
		t.Errorf("#45 not hidden: %+v", b.HiddenUnmatched)
	}
	if links := pkgOf(t, b, "A1").Links; len(links) != 0 {
		t.Errorf("hiding linked something: %+v", links)
	}
	if e.notes.count(api.TopicPlans) <= before {
		t.Error("no plans change event after hiding")
	}
	// The other plan on the repo still lists it.
	if _, found := unmatchedOf(e.board("sharing").Unmatched, ref); !found {
		t.Error("hiding from search-v2 hid #45 from sharing too")
	}

	res, err = e.svc.HideUnmatched(ctx, api.HideRequest{Plan: "search-v2", Kind: "pr", Ref: ref, Hidden: true})
	must(t, err)
	if res.Changed {
		t.Error("hiding twice changed something")
	}
	res, err = e.svc.HideUnmatched(ctx, api.HideRequest{Plan: "search-v2", Kind: "pr", Ref: ref})
	must(t, err)
	if res.Hidden || !res.Changed {
		t.Errorf("restore = %+v", res)
	}
	b = e.board("search-v2")
	if _, found := unmatchedOf(b.Unmatched, ref); !found || len(b.HiddenUnmatched) != 0 {
		t.Errorf("after restore: unmatched %+v, hidden %+v", b.Unmatched, b.HiddenUnmatched)
	}
	res, err = e.svc.HideUnmatched(ctx, api.HideRequest{Plan: "search-v2", Kind: "pr", Ref: ref})
	must(t, err)
	if res.Changed {
		t.Error("restoring what isn't hidden changed something")
	}

	for _, c := range []struct {
		what   string
		req    api.HideRequest
		status int
		part   string
	}{
		{"an agent", api.HideRequest{Plan: "search-v2", Kind: "agent", Ref: "s1/a1", Hidden: true}, http.StatusBadRequest, "only a pr or a branch"},
		{"no kind", api.HideRequest{Plan: "search-v2", Ref: ref, Hidden: true}, http.StatusBadRequest, "kind is required"},
		{"a bad ref", api.HideRequest{Plan: "search-v2", Kind: "pr", Ref: "acme/pivot", Hidden: true}, http.StatusBadRequest, "owner/repo#123"},
		{"no plan", api.HideRequest{Plan: "nope", Kind: "pr", Ref: ref, Hidden: true}, http.StatusNotFound, "no plan"},
	} {
		_, err := e.svc.HideUnmatched(ctx, c.req)
		var refusal interface{ HTTPStatus() int }
		if !errors.As(err, &refusal) || refusal.HTTPStatus() != c.status || !strings.Contains(err.Error(), c.part) {
			t.Errorf("%s: %v, want a %d with %q", c.what, err, c.status, c.part)
		}
	}
}

func unmatchedOf(us []api.Unmatched, ref string) (api.Unmatched, bool) {
	for _, u := range us {
		if strings.EqualFold(u.Ref, ref) {
			return u, true
		}
	}
	return api.Unmatched{}, false
}
