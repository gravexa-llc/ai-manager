package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The review-request search against its recorded answer: one open PR
// on example-org/brand, opened by someone else, asks for the user's
// review. One request for all three owners, 1 point.
func TestReviewRequestsFixture(t *testing.T) {
	f := newFakeGitHub(t)
	f.search = fixtureSearch(t, "review_requested_mine.json")
	clk := &clock{now: t0}
	c := f.client(nil, clk, Options{})

	res := c.ReviewRequests(context.Background(), threeOwners)

	reqs := f.reqs()
	if len(reqs) != 1 || res.Queries != 1 || res.Cost != 1 {
		t.Fatalf("%d requests, Queries %d, Cost %d; want 1, 1, 1", len(reqs), res.Queries, res.Cost)
	}
	q, _ := reqs[0].Vars["q"].(string)
	for _, part := range []string{"is:pr", "is:open", "archived:false", "review-requested:@me", "user:example-user", "user:acme", "user:example-org"} {
		if !strings.Contains(" "+q+" ", " "+part+" ") {
			t.Errorf("search %q lacks %q", q, part)
		}
	}
	if strings.Contains(q, "author:") {
		t.Errorf("search %q is limited to an author; a review request can be on anyone's PR", q)
	}
	if !strings.Contains(reqs[0].Query, "...PRFields") || !strings.Contains(reqs[0].Query, "repository { nameWithOwner }") {
		t.Errorf("review search doesn't use the shared fragment:\n%s", reqs[0].Query)
	}
	for _, st := range res.Owners {
		if st.Err != nil || !st.StaleSince.IsZero() {
			t.Errorf("owner %s: Err %v, StaleSince %v", st.Owner, st.Err, st.StaleSince)
		}
	}
	if len(res.PRs) != 1 || res.Truncated {
		t.Fatalf("%d PRs (truncated %v), want 1", len(res.PRs), res.Truncated)
	}
	p := res.PRs[0]
	if p.Repo != "example-org/brand" || p.Number != 6 || p.State != StateOpen || p.Draft ||
		p.MergeState != MergeStateClean || p.Mergeable != Mergeable || p.Checks != ChecksNone ||
		p.Author != "other-user" || p.HeadRef != "feat/footer-tagline" || p.BaseRef != "main" ||
		p.CreatedAt.IsZero() || p.Plan != (PlanRef{}) {
		t.Errorf("PR = %+v", p)
	}
	if b := c.Budget(); !b.Known || b.Remaining != 4983 {
		t.Errorf("budget = %+v, want the fixture's remaining 4983", b)
	}
}

// Like OpenPRs, the review search runs once per account, pages through its
// results, and marks only the owners of a failed search stale, under keys
// of its own: a failed review search doesn't make the open-PR search stale.
func TestReviewRequestsPerTokenAndStale(t *testing.T) {
	f := newFakeGitHub(t)
	page := func(nodes []any, next bool, cursor string) any {
		return map[string]any{"data": map[string]any{
			"rateLimit": map[string]any{"cost": 1, "remaining": 4000, "resetAt": t0.Add(30 * time.Minute)},
			"search": map[string]any{
				"issueCount": len(nodes),
				"pageInfo":   map[string]any{"hasNextPage": next, "endCursor": cursor},
				"nodes":      nodes,
			},
		}}
	}
	node := func(repo string, n int) any {
		return map[string]any{"repository": map[string]any{"nameWithOwner": repo}, "number": n, "state": "OPEN",
			"mergeable": "MERGEABLE", "mergeStateStatus": "CLEAN", "headRefName": fmt.Sprint("b", n)}
	}
	f.search = func(req fakeRequest) any {
		q := req.Vars["q"].(string)
		switch {
		case strings.Contains(q, "author:@me"):
			return page(nil, false, "")
		case strings.Contains(q, "user:example-org"):
			return page([]any{node("example-org/brand", 6)}, false, "")
		case req.Vars["after"] == nil:
			return page([]any{node("example-user/ai-manager", 40)}, true, "c1")
		default:
			return page([]any{node("acme/pivot", 214)}, false, "c2")
		}
	}
	toks := &fakeTokens{tokens: map[string]string{"example-user": "tok-a", "acme": "tok-a", "example-org": "tok-b"}}
	clk := &clock{now: t0}
	c := f.client(toks, clk, Options{})

	res := c.ReviewRequests(context.Background(), threeOwners)
	if res.Queries != 3 || len(res.PRs) != 3 {
		t.Fatalf("Queries %d, PRs %v; want 3 requests (two pages for tok-a, one for tok-b), 3 PRs", res.Queries, prKeys(res.PRs))
	}
	for _, r := range f.reqs() {
		if q := r.Vars["q"].(string); !strings.Contains(q, "review-requested:@me") {
			t.Errorf("request %q isn't the review search", q)
		}
	}

	// example-org's account loses its token: only its owner goes stale, and
	// the open-PR search of the same owner is still fresh.
	if open := c.OpenPRs(context.Background(), threeOwners); open.Owners[2].Err != nil {
		t.Fatalf("open-PR search: %+v", open.Owners)
	}
	clk.Advance(time.Minute)
	toks.setErr("example-org", fmt.Errorf("%w: gh: not logged in", ErrAuth))
	res = c.ReviewRequests(context.Background(), threeOwners)
	for _, st := range res.Owners {
		switch st.Owner {
		case "example-org":
			if !errors.Is(st.Err, ErrAuth) || !st.StaleSince.Equal(t0) {
				t.Errorf("example-org: Err %v, StaleSince %v; want ErrAuth, stale since %v", st.Err, st.StaleSince, t0)
			}
		default:
			if st.Err != nil {
				t.Errorf("%s: %v", st.Owner, st.Err)
			}
		}
	}
	for _, p := range res.PRs {
		if strings.HasPrefix(p.Repo, "example-org/") {
			t.Errorf("PR %s#%d from the failed owner", p.Repo, p.Number)
		}
	}
	// The open-PR search's freshness is its own.
	clk.Advance(time.Minute)
	toks.setErr("example-org", nil)
	open := c.OpenPRs(context.Background(), threeOwners)
	for _, st := range open.Owners {
		if st.Err != nil || !st.StaleSince.IsZero() {
			t.Errorf("open-PR search %s: Err %v, StaleSince %v", st.Owner, st.Err, st.StaleSince)
		}
	}
}

// The fixture's search part parses as the other searches do: a node that
// isn't a PR (an issue) is skipped.
func TestReviewRequestsSkipsIssues(t *testing.T) {
	f := newFakeGitHub(t)
	f.search = func(fakeRequest) any {
		var v map[string]any
		if err := json.Unmarshal(readFixture(t, "review_requested_mine.json"), &v); err != nil {
			t.Fatal(err)
		}
		s := v["data"].(map[string]any)["search"].(map[string]any)
		s["nodes"] = append([]any{map[string]any{}}, s["nodes"].([]any)...)
		return v
	}
	res := f.client(nil, nil, Options{}).ReviewRequests(context.Background(), []string{"example-org"})
	if got := prKeys(res.PRs); len(got) != 1 || got[0] != "example-org/brand#6" {
		t.Errorf("PRs = %v, want example-org/brand#6", got)
	}
}
