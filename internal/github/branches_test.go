package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// findBranch returns the named repo's branch from a result.
func findBranch(t *testing.T, res BranchPRsResult, repo, branch string) BranchPRs {
	t.Helper()
	for _, r := range res.Repos {
		if !strings.EqualFold(r.Owner+"/"+r.Name, repo) && !strings.EqualFold(r.Repo, repo) {
			continue
		}
		for _, b := range r.Branches {
			if b.Branch == branch {
				return b
			}
		}
	}
	t.Fatalf("no branch %s in %s", branch, repo)
	return BranchPRs{}
}

// onlyPR returns the branch's single PR.
func onlyPR(t *testing.T, b BranchPRs) PR {
	t.Helper()
	if b.Err != nil || len(b.PRs) != 1 {
		t.Fatalf("branch %s: %d PRs, err %v; want one PR", b.Branch, len(b.PRs), b.Err)
	}
	return b.PRs[0]
}

// The whole recorded batch in one query must send exactly the recorded
// variables, and parse the recorded response (testdata/github).
func TestBranchPRsCombinedFixture(t *testing.T) {
	f := newFakeGitHub(t)
	var want struct {
		Combined map[string]any `json:"combined"`
	}
	if err := json.Unmarshal(readFixture(t, "branch_queries_variables.json"), &want); err != nil {
		t.Fatal(err)
	}
	f.handle = func(w http.ResponseWriter, req fakeRequest) bool {
		if !reflect.DeepEqual(req.Vars, want.Combined) {
			t.Errorf("variables differ from the S3 combined query:\n got %v\nwant %v", req.Vars, want.Combined)
		}
		if n := branchCount(req.Query); n != 59 {
			t.Errorf("query has %d branch connections, want 59", n)
		}
		rawJSON(w, http.StatusOK, readFixture(t, "branch_prs_combined_18_repos.json"))
		return true
	}
	clk := &clock{now: t0}
	c := f.client(nil, clk, Options{MaxBranchesPerQuery: 100})

	res := c.BranchPRs(context.Background(), trackedRepos(t))

	if res.Queries != 1 || res.Cost != 1 {
		t.Errorf("Queries = %d, Cost = %d; want 1, 1", res.Queries, res.Cost)
	}
	if len(res.Repos) != 18 {
		t.Fatalf("%d repos, want 18", len(res.Repos))
	}
	nBranches := 0
	for _, r := range res.Repos {
		if r.Err != nil || !r.StaleSince.IsZero() {
			t.Errorf("%s/%s: Err %v, StaleSince %v", r.Owner, r.Name, r.Err, r.StaleSince)
		}
		if r.DefaultBranch != "main" {
			t.Errorf("%s: DefaultBranch %q", r.Repo, r.DefaultBranch)
		}
		nBranches += len(r.Branches)
		for _, b := range r.Branches {
			for _, pr := range b.PRs {
				if pr.Repo != r.Repo {
					t.Errorf("PR %s#%d keyed on %q, want the answered name %q", pr.Repo, pr.Number, pr.Repo, r.Repo)
				}
				if pr.HeadRef != b.Branch {
					t.Errorf("PR #%d HeadRef %q under branch %q", pr.Number, pr.HeadRef, b.Branch)
				}
				// GitHub never computes mergeable for merged or closed PRs.
				if pr.State != StateOpen && pr.Mergeable != MergePending {
					t.Errorf("%s#%d %s: Mergeable %q, want pending", pr.Repo, pr.Number, pr.State, pr.Mergeable)
				}
			}
		}
	}
	if nBranches != 59 {
		t.Errorf("%d branches, want 59", nBranches)
	}

	// Transferred repos: asked under the old owner, keyed on the new one.
	renamed := map[string]string{}
	for _, r := range res.Repos {
		if r.Renamed() {
			renamed[r.Owner+"/"+r.Name] = r.Repo
		}
	}
	wantRenamed := map[string]string{
		"example-user/relay":        "acme/relay",
		"example-user/ops-tools":    "acme/ops-tools",
		"example-user/landing-page": "acme/landing-page",
	}
	if !reflect.DeepEqual(renamed, wantRenamed) {
		t.Errorf("renamed = %v, want %v", renamed, wantRenamed)
	}
	if got := c.Aliases(); !reflect.DeepEqual(got, wantRenamed) {
		t.Errorf("Aliases() = %v, want %v", got, wantRenamed)
	}

	// Open, mergeable, no CI.
	pr := onlyPR(t, findBranch(t, res, "acme/pivot", "share/v1-email-reports"))
	want214 := PR{
		Repo: "acme/pivot", Number: 214, State: StateOpen,
		Mergeable: Mergeable, MergeState: MergeStateClean, Checks: ChecksNone,
		HeadRef: "share/v1-email-reports", HeadOID: "64a260aca901aede716644da290221d81d0701e1", BaseRef: "release/2.4",
		Title: "PR title (V1)", URL: "https://github.com/acme/pivot/pull/214", Author: "example-user",
		CreatedAt: time.Date(2026, 11, 2, 4, 32, 7, 0, time.UTC),
		UpdatedAt: time.Date(2026, 11, 2, 4, 41, 47, 0, time.UTC),
	}
	if !reflect.DeepEqual(pr, want214) {
		t.Errorf("pivot #214 =\n%+v\nwant\n%+v", pr, want214)
	}

	// A branch with no PR.
	if b := findBranch(t, res, "acme/pivot", "worktree-agent-ab66668f3b601afd9"); b.Err != nil || len(b.PRs) != 0 || b.TotalCount != 0 {
		t.Errorf("branch without PR = %+v", b)
	}

	// A reused branch: 13 PRs, the newest 5 returned, newest first.
	full := findBranch(t, res, "example-org/meadow-web", "site-staging")
	var nums []int
	for _, p := range full.PRs {
		nums = append(nums, p.Number)
	}
	if full.TotalCount != 13 || !reflect.DeepEqual(nums, []int{156, 155, 153, 151, 150}) {
		t.Errorf("site-staging: TotalCount %d, PRs %v; want 13, [156 155 153 151 150]", full.TotalCount, nums)
	}

	// Merged with red CI.
	pr = onlyPR(t, findBranch(t, res, "example-user/ai-manager", "aim/f1-repo-scaffold"))
	if pr.Number != 2 || pr.State != StateMerged || pr.Checks != ChecksFailure || pr.MergedAt.IsZero() {
		t.Errorf("ai-manager #2 = %+v; want merged with FAILURE checks", pr)
	}

	// Merged with no CI: a null rollup is "no CI", not failing.
	pr = onlyPR(t, findBranch(t, res, "example-org/meadow-backend", "surge-b-3b-2"))
	if pr.Number != 478 || pr.Checks != ChecksNone || pr.BaseRef != "surge" {
		t.Errorf("meadow-backend #478 = %+v; want Checks NONE, base surge", pr)
	}

	// Closed without merging, draft.
	pr = onlyPR(t, findBranch(t, res, "example-org/meadow-backend", "surge-b-2a-spike"))
	if pr.Number != 447 || pr.State != StateClosed || !pr.Draft || !pr.MergedAt.IsZero() || pr.ClosedAt.IsZero() {
		t.Errorf("meadow-backend #447 = %+v; want closed, draft, not merged", pr)
	}

	b := c.Budget()
	if !b.Known || b.Remaining != 4983 || b.Verdict != BudgetNormal || b.LastCost != 1 {
		t.Errorf("Budget() = %+v; want remaining 4983, normal", b)
	}
	if h := c.Health(); h.Stale() || !h.LastOK.Equal(t0) {
		t.Errorf("Health() = %+v; want fresh at %v", h, t0)
	}
}

// Split into queries of at most MaxBranchesPerQuery branches, the answer
// must be the same as from one query.
func TestBranchPRsSplits(t *testing.T) {
	f := newFakeGitHub(t)
	one := f.client(nil, nil, Options{MaxBranchesPerQuery: 100}).BranchPRs(context.Background(), trackedRepos(t))
	if one.Queries != 1 {
		t.Fatalf("baseline used %d queries", one.Queries)
	}

	for _, tc := range []struct {
		max, wantQueries int
	}{
		{0, 2},  // default 50: 59 branches
		{50, 2}, //
		{17, 4}, // meadow-backend's 17 fill one query exactly
		{10, 7}, // meadow-backend split across two queries
		{1, 59},
	} {
		t.Run(fmt.Sprint(tc.max), func(t *testing.T) {
			f := newFakeGitHub(t)
			res := f.client(nil, nil, Options{MaxBranchesPerQuery: tc.max}).BranchPRs(context.Background(), trackedRepos(t))
			limit := tc.max
			if limit == 0 {
				limit = DefaultMaxBranchesPerQuery
			}
			reqs := f.reqs()
			if res.Queries != tc.wantQueries || len(reqs) != tc.wantQueries {
				t.Errorf("Queries = %d (server saw %d), want %d", res.Queries, len(reqs), tc.wantQueries)
			}
			if res.Cost != res.Queries {
				t.Errorf("Cost = %d, want 1 per query (%d)", res.Cost, res.Queries)
			}
			total := 0
			for _, r := range reqs {
				n := branchCount(r.Query)
				if n > limit {
					t.Errorf("a query has %d branches, over the limit %d", n, limit)
				}
				total += n
			}
			if total != 59 {
				t.Errorf("%d branch connections sent in total, want 59", total)
			}
			if !reflect.DeepEqual(res.Repos, one.Repos) {
				t.Errorf("split result differs from the single-query result")
			}
		})
	}
}

func TestChunkKeepsReposWhole(t *testing.T) {
	c := New(configWithOwners(nil), Options{MaxBranchesPerQuery: 10, Tokens: staticTokens("x")})
	mk := func(n int) RepoPRs {
		r := RepoPRs{}
		for i := range n {
			r.Branches = append(r.Branches, BranchPRs{Branch: fmt.Sprint(i)})
		}
		return r
	}
	all := []RepoPRs{mk(6), mk(6), mk(0), mk(23), mk(3)}
	chunks := c.chunk(all, []int{0, 1, 2, 3, 4})
	var shape [][]string
	for _, ch := range chunks {
		var s []string
		for _, cr := range ch {
			s = append(s, fmt.Sprintf("%d:%d", cr.repo, len(cr.branches)))
		}
		shape = append(shape, s)
	}
	// Repo 1 (6) doesn't fit after repo 0 (6), so it starts a new query;
	// repo 2 (no branches) takes one slot; repo 3 (23) is too big for any
	// query, so it fills the rest of one and spills over.
	want := [][]string{{"0:6"}, {"1:6", "2:0", "3:3"}, {"3:10"}, {"3:10"}, {"4:3"}}
	if !reflect.DeepEqual(shape, want) {
		t.Errorf("chunks = %v, want %v", shape, want)
	}
}

// One missing repo in a batch: the others still come back (a recorded
// partial response).
func TestBranchPRsPartialNotFound(t *testing.T) {
	f := newFakeGitHub(t)
	f.handle = func(w http.ResponseWriter, req fakeRequest) bool {
		want := map[string]any{
			"o0": "acme", "n0": "pivot-web", "r0b0": "seo/pricing-page",
			"o1": "example-user", "n1": "does-not-exist-probe", "r1b0": "main",
			"o2": "example-user", "n2": "relay", "r2b0": "claude/eager-blackwell-b5eea4",
		}
		if !reflect.DeepEqual(req.Vars, want) {
			t.Errorf("variables = %v, want %v", req.Vars, want)
		}
		rawJSON(w, http.StatusOK, readFixture(t, "branch_prs_partial_not_found.json"))
		return true
	}
	clk := &clock{now: t0}
	c := f.client(nil, clk, Options{})
	res := c.BranchPRs(context.Background(), []RepoBranches{
		{Owner: "acme", Name: "pivot-web", Branches: []string{"seo/pricing-page"}},
		{Owner: "example-user", Name: "does-not-exist-probe", Branches: []string{"main"}},
		{Owner: "example-user", Name: "relay", Branches: []string{"claude/eager-blackwell-b5eea4"}},
	})

	if len(res.Repos) != 3 || res.Queries != 1 {
		t.Fatalf("%d repos in %d queries", len(res.Repos), res.Queries)
	}
	ok, missing, relay := res.Repos[0], res.Repos[1], res.Repos[2]

	if ok.Err != nil || ok.Repo != "acme/pivot-web" {
		t.Errorf("pivot-web: %+v", ok)
	}
	if pr := onlyPR(t, ok.Branches[0]); pr.Number != 33 || pr.State != StateMerged || pr.BaseRef != "site-staging" {
		t.Errorf("pivot-web PR = %+v", pr)
	}

	if !errors.Is(missing.Err, ErrNotFound) {
		t.Errorf("missing repo Err = %v, want ErrNotFound", missing.Err)
	}
	if !strings.Contains(missing.Err.Error(), "does-not-exist-probe") {
		t.Errorf("missing repo Err = %q, want it to name the repo", missing.Err)
	}
	if !missing.StaleSince.Equal(t0) {
		t.Errorf("missing repo StaleSince = %v, want %v (never read)", missing.StaleSince, t0)
	}
	if missing.Repo != "" || !errors.Is(missing.Branches[0].Err, ErrNotFound) || missing.Branches[0].PRs != nil {
		t.Errorf("missing repo = %+v", missing)
	}

	if relay.Err != nil || !relay.Renamed() || relay.Repo != "acme/relay" {
		t.Errorf("relay = %+v; want renamed to acme/relay", relay)
	}
	if b := relay.Branches[0]; b.Err != nil || len(b.PRs) != 0 {
		t.Errorf("relay branch = %+v; want no PRs, no error", b)
	}

	// A missing repo is GitHub answering, not GitHub being down.
	if h := c.Health(); h.Stale() {
		t.Errorf("Health() = %+v; want fresh", h)
	}
	if b := c.Budget(); b.Remaining != 4938 {
		t.Errorf("Budget().Remaining = %d, want 4938", b.Remaining)
	}
}

// An error on one branch alias leaves the rest of the repo intact.
func TestBranchPRsBranchError(t *testing.T) {
	f := newFakeGitHub(t)
	f.handle = func(w http.ResponseWriter, req fakeRequest) bool {
		out := f.branchAnswer(req)
		out["data"].(map[string]any)["r0"].(map[string]any)["b1"] = nil
		out["errors"] = []map[string]any{{"type": "FORBIDDEN", "path": []string{"r0", "b1"}, "message": "Resource not accessible"}}
		body, _ := json.Marshal(out)
		rawJSON(w, http.StatusOK, body)
		return true
	}
	res := f.client(nil, nil, Options{}).BranchPRs(context.Background(), []RepoBranches{
		{Owner: "acme", Name: "pivot", Branches: []string{"share/v1-email-reports", "share/a3-embed-endpoint", "share/s1-scheduled-snapshots"}},
	})
	r := res.Repos[0]
	if !errors.Is(r.Err, ErrForbidden) || r.StaleSince.IsZero() {
		t.Errorf("repo Err = %v, StaleSince %v; want ErrForbidden and stale", r.Err, r.StaleSince)
	}
	if !errors.Is(r.Branches[1].Err, ErrForbidden) || r.Branches[1].PRs != nil {
		t.Errorf("branch 1 = %+v; want ErrForbidden", r.Branches[1])
	}
	for _, i := range []int{0, 2} {
		if b := r.Branches[i]; b.Err != nil || len(b.PRs) != 1 {
			t.Errorf("branch %d = %+v; want one PR", i, b)
		}
	}
}

// A query that fails as a whole marks every repo in it stale.
func TestBranchPRsWholeQueryError(t *testing.T) {
	f := newFakeGitHub(t)
	f.handle = func(w http.ResponseWriter, req fakeRequest) bool {
		rawJSON(w, http.StatusOK, []byte(`{"data":null,"errors":[{"message":"Something went wrong while executing your query."}]}`))
		return true
	}
	clk := &clock{now: t0}
	c := f.client(nil, clk, Options{})
	res := c.BranchPRs(context.Background(), trackedRepos(t)[:3])
	for _, r := range res.Repos {
		if r.Err == nil || !strings.Contains(r.Err.Error(), "Something went wrong") || !r.StaleSince.Equal(t0) {
			t.Errorf("%s: Err %v, StaleSince %v", r.Name, r.Err, r.StaleSince)
		}
		for _, b := range r.Branches {
			if b.Err == nil {
				t.Errorf("%s %s: no Err", r.Name, b.Branch)
			}
		}
	}
	if h := c.Health(); !h.Stale() {
		t.Error("Health() not stale after a failed query")
	}
}

// Fork PRs with the same branch name are dropped; TotalCount stays GitHub's.
func TestBranchPRsDropsForks(t *testing.T) {
	f := newFakeGitHub(t)
	f.extra["acme/pivot|share/v1-email-reports"] = []map[string]any{{
		"number": 999, "state": "OPEN", "isCrossRepository": true, "headRefName": "share/v1-email-reports",
		"mergeable": "MERGEABLE", "mergeStateStatus": "CLEAN", "title": "PR title", "author": map[string]any{"login": "other-user"},
	}}
	res := f.client(nil, nil, Options{}).BranchPRs(context.Background(), []RepoBranches{
		{Owner: "acme", Name: "pivot", Branches: []string{"share/v1-email-reports"}},
	})
	b := res.Repos[0].Branches[0]
	if pr := onlyPR(t, b); pr.Number != 214 {
		t.Errorf("kept PR #%d, want #214 only", pr.Number)
	}
	if b.TotalCount != 2 {
		t.Errorf("TotalCount = %d, want 2", b.TotalCount)
	}
}

// After GitHub answers with a new name, later calls ask for that name, with
// the token for the new owner, and still report the requested one.
func TestBranchPRsUsesAnsweredName(t *testing.T) {
	f := newFakeGitHub(t)
	toks := &fakeTokens{tokens: map[string]string{"example-user": "tok-old", "acme": "tok-new"}}
	c := f.client(toks, nil, Options{})
	ask := []RepoBranches{{Owner: "example-user", Name: "relay", Branches: []string{"claude/eager-blackwell-b5eea4"}}}

	first := c.BranchPRs(context.Background(), ask)
	second := c.BranchPRs(context.Background(), ask)

	reqs := f.reqs()
	if len(reqs) != 2 {
		t.Fatalf("%d requests, want 2", len(reqs))
	}
	if reqs[0].Vars["o0"] != "example-user" || reqs[0].Token != "tok-old" {
		t.Errorf("first request asked %v with %q", reqs[0].Vars["o0"], reqs[0].Token)
	}
	if reqs[1].Vars["o0"] != "acme" || reqs[1].Token != "tok-new" {
		t.Errorf("second request asked %v with %q; want the answered owner and its token", reqs[1].Vars["o0"], reqs[1].Token)
	}
	for _, res := range []BranchPRsResult{first, second} {
		r := res.Repos[0]
		if r.Owner != "example-user" || r.Name != "relay" || r.Repo != "acme/relay" || r.Err != nil {
			t.Errorf("result = %+v", r)
		}
	}
}

// Repos that need different tokens go in different queries.
func TestBranchPRsGroupsByToken(t *testing.T) {
	f := newFakeGitHub(t)
	toks := &fakeTokens{tokens: map[string]string{
		"example-org": "tok-a", "acme": "tok-b", "example-user": "tok-a",
	}}
	res := f.client(toks, nil, Options{}).BranchPRs(context.Background(), []RepoBranches{
		{Owner: "example-org", Name: "meadow-ios", Branches: []string{"watch-app"}},
		{Owner: "acme", Name: "pivot", Branches: []string{"share/v1-email-reports"}},
		{Owner: "example-user", Name: "ai-manager", Branches: []string{"aim/f1-repo-scaffold"}},
	})
	reqs := f.reqs()
	if len(reqs) != 2 || res.Queries != 2 {
		t.Fatalf("%d requests, want 2 (one per token)", len(reqs))
	}
	got := map[string][]any{}
	for _, r := range reqs {
		for _, k := range []string{"o0", "o1"} {
			if v, ok := r.Vars[k]; ok {
				got[r.Token] = append(got[r.Token], v)
			}
		}
	}
	want := map[string][]any{"tok-a": {"example-org", "example-user"}, "tok-b": {"acme"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("owners per token = %v, want %v", got, want)
	}
	for _, r := range res.Repos {
		if r.Err != nil {
			t.Errorf("%s: %v", r.Name, r.Err)
		}
	}
}

func TestBranchPRsMergesDuplicates(t *testing.T) {
	f := newFakeGitHub(t)
	res := f.client(nil, nil, Options{}).BranchPRs(context.Background(), []RepoBranches{
		{Owner: "acme", Name: "pivot", Branches: []string{"share/v1-email-reports", "", "refs/heads/share/a3-embed-endpoint"}},
		{Owner: "Acme", Name: "PIVOT", Branches: []string{"share/v1-email-reports", "share/s1-scheduled-snapshots"}},
	})
	if len(res.Repos) != 1 {
		t.Fatalf("%d repos, want 1", len(res.Repos))
	}
	var names []string
	for _, b := range res.Repos[0].Branches {
		names = append(names, b.Branch)
	}
	if want := []string{"share/v1-email-reports", "share/a3-embed-endpoint", "share/s1-scheduled-snapshots"}; !reflect.DeepEqual(names, want) {
		t.Errorf("branches = %v, want %v", names, want)
	}
	if n := branchCount(f.reqs()[0].Query); n != 3 {
		t.Errorf("query has %d branches, want 3", n)
	}
}

func TestBranchPRsInvalidRepo(t *testing.T) {
	f := newFakeGitHub(t)
	res := f.client(nil, nil, Options{}).BranchPRs(context.Background(), []RepoBranches{
		{Owner: "", Name: "x", Branches: []string{"main"}},
		{Owner: "--help", Name: "x", Branches: []string{"main"}},
		{Owner: "acme", Name: "pivot", Branches: []string{"share/v1-email-reports"}},
	})
	for _, r := range res.Repos[:2] {
		if !errors.Is(r.Err, ErrInvalid) || !errors.Is(r.Branches[0].Err, ErrInvalid) {
			t.Errorf("%q/%q: Err %v; want ErrInvalid", r.Owner, r.Name, r.Err)
		}
	}
	if r := res.Repos[2]; r.Err != nil {
		t.Errorf("valid repo: %v", r.Err)
	}
	if n := len(f.reqs()); n != 1 {
		t.Errorf("%d requests, want 1", n)
	}
}

func TestBranchPRsEmpty(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.client(nil, nil, Options{})
	res := c.BranchPRs(context.Background(), nil)
	if len(res.Repos) != 0 || res.Queries != 0 || len(f.reqs()) != 0 {
		t.Errorf("empty request sent %d queries: %+v", len(f.reqs()), res)
	}
	// Nothing was sent, so nothing is known about GitHub's health.
	if h := c.Health(); !h.LastOK.IsZero() || h.Stale() {
		t.Errorf("Health() after an empty call = %+v", h)
	}
}

// The Plan: line is parsed from the body, and the body goes no further.
func TestBranchPRsPlanLine(t *testing.T) {
	f := newFakeGitHub(t)
	secret := "private notes that must not be kept"
	f.bodies["acme/pivot#214"] = "Plan: pivot · V1\n\n## What changed\n" + secret
	f.bodies["acme/pivot#217"] = "**Plan:** pivot - A3\r\n" + secret
	f.bodies["acme/pivot#218"] = secret
	res := f.client(nil, nil, Options{}).BranchPRs(context.Background(), []RepoBranches{
		{Owner: "acme", Name: "pivot", Branches: []string{"share/v1-email-reports", "share/a3-embed-endpoint", "share/s1-scheduled-snapshots", "share/t1-tile-filters"}},
	})
	if q := f.reqs()[0].Query; !strings.Contains(q, " body") || !strings.Contains(q, "createdAt") {
		t.Error("query doesn't fetch body and createdAt")
	}
	for _, b := range res.Repos[0].Branches {
		if pr := onlyPR(t, b); pr.CreatedAt.IsZero() {
			t.Errorf("#%d has no CreatedAt", pr.Number)
		}
	}
	want := map[int]PlanRef{214: {"pivot", "V1"}, 217: {"pivot", "A3"}, 218: {}, 219: {}}
	for _, b := range res.Repos[0].Branches {
		pr := onlyPR(t, b)
		if pr.Plan != want[pr.Number] {
			t.Errorf("#%d Plan = %+v, want %+v", pr.Number, pr.Plan, want[pr.Number])
		}
		if s := fmt.Sprintf("%#v", pr); strings.Contains(s, secret) {
			t.Errorf("#%d keeps the body: %s", pr.Number, s)
		}
	}
}
