package github

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// numberOf returns a repo's answer for one number.
func numberOf(t *testing.T, res NumberPRsResult, repo string, number int) (RepoNumberPRs, NumberPR) {
	t.Helper()
	for _, r := range res.Repos {
		if !strings.EqualFold(r.Owner+"/"+r.Name, repo) {
			continue
		}
		for _, np := range r.Numbers {
			if np.Number == number {
				return r, np
			}
		}
	}
	t.Fatalf("no #%d in %s", number, repo)
	return RepoNumberPRs{}, NumberPR{}
}

// PRs are read by number in one 1-point query: open and merged alike, a
// repo answered under its new name, and a missing PR or repo failing
// only itself.
func TestPRsByNumber(t *testing.T) {
	f := newFakeGitHub(t)
	clk := &clock{now: t0}
	c := f.client(nil, clk, Options{})
	res := c.PRsByNumber(context.Background(), []RepoNumbers{
		{Owner: "acme", Name: "pivot", Numbers: []int{214, 219, 214}},
		{Owner: "example-org", Name: "meadow-web", Numbers: []int{156}},
		{Owner: "ACME", Name: "Pivot", Numbers: []int{999}}, // the same repo, another case
		{Owner: "example-user", Name: "relay", Numbers: []int{7}},
		{Owner: "example-org", Name: "nope", Numbers: []int{1}},
		{Owner: "acme", Name: "scoreboard", Numbers: []int{0}},
	})
	if res.Queries != 1 || res.Cost != 1 {
		t.Errorf("Queries %d, Cost %d; want one 1-point query", res.Queries, res.Cost)
	}
	if len(res.Repos) != 5 {
		t.Fatalf("%d repos, want 5 (acme/pivot merged)", len(res.Repos))
	}
	reqs := f.reqs()
	if len(reqs) != 1 || !strings.Contains(reqs[0].Query, "p214: pullRequest(number: 214) { ...PRFields }") ||
		strings.Count(reqs[0].Query, "pullRequest(number: 214)") != 1 || strings.Contains(reqs[0].Query, "number: 0") {
		t.Fatalf("query:\n%s", reqs[0].Query)
	}

	pivot, np := numberOf(t, res, "acme/pivot", 214)
	if np.Err != nil || np.PR == nil || np.PR.State != StateOpen || np.PR.HeadRef != "share/v1-email-reports" || np.PR.Repo != "acme/pivot" {
		t.Errorf("#214 = %+v, %+v; want the open PR on share/v1-email-reports", np, np.PR)
	}
	if pivot.Repo != "acme/pivot" || len(pivot.Numbers) != 3 {
		t.Errorf("acme/pivot: Repo %q, %d numbers; want 214, 219, 999", pivot.Repo, len(pivot.Numbers))
	}
	if _, np := numberOf(t, res, "acme/pivot", 999); !errors.Is(np.Err, ErrNotFound) || np.PR != nil {
		t.Errorf("#999 = %+v; want ErrNotFound", np)
	}
	if !errors.Is(pivot.Err, ErrNotFound) || !pivot.StaleSince.Equal(t0) {
		t.Errorf("acme/pivot Err %v, StaleSince %v; want #999's ErrNotFound", pivot.Err, pivot.StaleSince)
	}
	web, np := numberOf(t, res, "example-org/meadow-web", 156)
	if web.Err != nil || np.PR == nil || np.PR.State != StateMerged || np.PR.MergedAt.IsZero() {
		t.Errorf("meadow-web #156 = %+v, repo Err %v; want merged", np.PR, web.Err)
	}
	if relay, _ := numberOf(t, res, "example-user/relay", 7); relay.Repo != "acme/relay" {
		t.Errorf("relay answered as %q, want acme/relay", relay.Repo)
	}
	if nope, np := numberOf(t, res, "example-org/nope", 1); !errors.Is(nope.Err, ErrNotFound) || !errors.Is(np.Err, ErrNotFound) {
		t.Errorf("a missing repo: Err %v, #1 Err %v; want ErrNotFound", nope.Err, np.Err)
	}
	if sb, np := numberOf(t, res, "acme/scoreboard", 0); !errors.Is(np.Err, ErrInvalid) || !errors.Is(sb.Err, ErrInvalid) {
		t.Errorf("#0: %v, repo %v; want ErrInvalid", np.Err, sb.Err)
	}
	if h := c.Health(); h.Stale() {
		t.Errorf("Health() = %+v; a missing repo isn't a failed call", h)
	}

	// Read again, the repo is fresh once every number reads.
	res = c.PRsByNumber(context.Background(), []RepoNumbers{{Owner: "acme", Name: "pivot", Numbers: []int{219}}})
	if r := res.Repos[0]; r.Err != nil || !r.StaleSince.IsZero() || r.Numbers[0].PR == nil {
		t.Errorf("acme/pivot #219 = %+v", r)
	}
}

// A batch is split into queries of at most MaxBranchesPerQuery PRs.
func TestPRsByNumberChunks(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.client(nil, nil, Options{MaxBranchesPerQuery: 2})
	res := c.PRsByNumber(context.Background(), []RepoNumbers{
		{Owner: "acme", Name: "pivot", Numbers: []int{214, 217, 218}},
		{Owner: "acme", Name: "pivot-web", Numbers: []int{32, 33}},
	})
	if res.Queries != 3 {
		t.Errorf("%d queries, want 3", res.Queries)
	}
	for _, r := range res.Repos {
		if r.Err != nil {
			t.Errorf("%s/%s: %v", r.Owner, r.Name, r.Err)
		}
		for _, np := range r.Numbers {
			if np.PR == nil || np.PR.Number != np.Number {
				t.Errorf("%s/%s #%d: %+v", r.Owner, r.Name, np.Number, np)
			}
		}
	}
}

// A token that fails, or a query that fails as a whole, fails only its
// repos, with StaleSince set and Health stale; nothing panics.
func TestPRsByNumberFailures(t *testing.T) {
	f := newFakeGitHub(t)
	tokens := staticTokens("tok")
	tokens.setErr("example-org", errors.New("github: auth: no gh account for example-org"))
	clk := &clock{now: t0}
	c := f.client(tokens, clk, Options{})
	res := c.PRsByNumber(context.Background(), []RepoNumbers{
		{Owner: "example-org", Name: "meadow-web", Numbers: []int{156}},
		{Owner: "acme", Name: "pivot", Numbers: []int{214}},
	})
	if web := res.Repos[0]; web.Err == nil || web.Numbers[0].Err == nil || !web.StaleSince.Equal(t0) {
		t.Errorf("meadow-web = %+v; want the token's error", web)
	}
	if pivot := res.Repos[1]; pivot.Err != nil || pivot.Numbers[0].PR == nil {
		t.Errorf("acme/pivot = %+v; want read", pivot)
	}
	if h := c.Health(); !h.Stale() {
		t.Error("Health() not stale after a token failure")
	}

	f.handle = func(w http.ResponseWriter, req fakeRequest) bool {
		w.WriteHeader(http.StatusBadGateway)
		return true
	}
	res = c.PRsByNumber(context.Background(), []RepoNumbers{{Owner: "acme", Name: "pivot", Numbers: []int{214, 217}}})
	r := res.Repos[0]
	if !errors.Is(r.Err, ErrUnavailable) || r.StaleSince.IsZero() {
		t.Errorf("after a 502: Err %v, StaleSince %v", r.Err, r.StaleSince)
	}
	for _, np := range r.Numbers {
		if !errors.Is(np.Err, ErrUnavailable) || np.PR != nil {
			t.Errorf("#%d = %+v; want ErrUnavailable", np.Number, np)
		}
	}

	// Nothing to ask: no request, and Health is left alone.
	before := len(f.reqs())
	if res := c.PRsByNumber(context.Background(), nil); len(res.Repos) != 0 || res.Queries != 0 || len(f.reqs()) != before {
		t.Errorf("an empty call = %+v", res)
	}
}
