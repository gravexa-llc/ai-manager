package github

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// searchPage is one search answer holding the given PR nodes.
func searchPage(nodes ...map[string]any) any {
	ns := make([]any, len(nodes))
	for i, n := range nodes {
		ns[i] = n
	}
	return map[string]any{"data": map[string]any{
		"rateLimit": map[string]any{"cost": 1, "remaining": 4000, "resetAt": t0.Add(30 * time.Minute)},
		"search": map[string]any{
			"issueCount": len(nodes),
			"pageInfo":   map[string]any{"hasNextPage": false, "endCursor": ""},
			"nodes":      ns,
		},
	}}
}

func prNode(repo string, number int, state, branch, body string) map[string]any {
	n := map[string]any{
		"repository":  map[string]any{"nameWithOwner": repo},
		"number":      number,
		"state":       state,
		"headRefName": branch,
		"title":       "PR " + branch,
		"url":         fmt.Sprintf("https://github.com/%s/pull/%d", repo, number),
		"body":        body,
		"createdAt":   t0.Add(-48 * time.Hour),
		"updatedAt":   t0.Add(-time.Hour),
	}
	if state == "MERGED" {
		n["mergedAt"] = t0.Add(-2 * time.Hour)
		n["closedAt"] = t0.Add(-2 * time.Hour)
	}
	return n
}

// One search per repo, every state, with the Plan: line parsed; the
// canonical name comes from the answer.
func TestRepoPRs(t *testing.T) {
	f := newFakeGitHub(t)
	f.search = func(req fakeRequest) any {
		q := req.Vars["q"].(string)
		switch {
		case strings.Contains(q, "repo:old-owner/app "):
			return searchPage(
				prNode("new-owner/app", 2, "MERGED", "aim/c1-plan-parser", "Plan: aim · C1\n\nbody"),
				prNode("new-owner/app", 3, "OPEN", "aim/c3-plan-api", ""),
			)
		case strings.Contains(q, "repo:o/empty "):
			return searchPage()
		}
		t.Errorf("unexpected search %q", q)
		return searchPage()
	}
	res := f.client(nil, nil, Options{}).RepoPRs(context.Background(), []string{"old-owner/app", "o/empty", "OLD-OWNER/app", "not a repo"})

	reqs := f.reqs()
	if len(reqs) != 2 || res.Queries != 2 || res.Cost != 2 {
		t.Fatalf("%d requests, Queries %d, Cost %d; want 2 each (one per distinct valid repo)", len(reqs), res.Queries, res.Cost)
	}
	q := reqs[0].Vars["q"].(string)
	for _, part := range []string{"is:pr", "repo:old-owner/app", "sort:updated-desc"} {
		if !strings.Contains(" "+q+" ", " "+part+" ") {
			t.Errorf("search %q lacks %q", q, part)
		}
	}
	for _, not := range []string{"is:open", "author:", "user:"} {
		if strings.Contains(q, not) {
			t.Errorf("search %q has %q: a backfill wants every PR", q, not)
		}
	}
	if len(res.Repos) != 3 {
		t.Fatalf("Repos = %+v, want 3 (repeats dropped)", res.Repos)
	}
	if r := res.Repos[0]; r.Err != nil || r.Name != "new-owner/app" || r.PRs != 2 {
		t.Errorf("Repos[0] = %+v, want new-owner/app with 2 PRs", r)
	}
	if r := res.Repos[1]; r.Err != nil || r.Name != "" || r.PRs != 0 {
		t.Errorf("Repos[1] = %+v, want no PRs, no name", r)
	}
	if r := res.Repos[2]; !errors.Is(r.Err, ErrInvalid) {
		t.Errorf("Repos[2] = %+v, want ErrInvalid", r)
	}
	if len(res.PRs) != 2 {
		t.Fatalf("PRs = %+v", res.PRs)
	}
	merged := res.PRs[0]
	if merged.State != StateMerged || merged.Plan.Slug != "aim" || merged.Plan.ID != "C1" || merged.CreatedAt.IsZero() || merged.MergedAt.IsZero() {
		t.Errorf("merged PR = %+v", merged)
	}
}

// A repo whose token fails is reported, and the others are still read.
func TestRepoPRsTokenError(t *testing.T) {
	f := newFakeGitHub(t)
	f.search = func(req fakeRequest) any { return searchPage(prNode("ok/app", 1, "OPEN", "x", "")) }
	tokens := staticTokens("tok")
	tokens.setErr("bad", ErrAuth)
	c := f.client(tokens, nil, Options{})
	res := c.RepoPRs(context.Background(), []string{"bad/app", "ok/app"})
	if !errors.Is(res.Repos[0].Err, ErrAuth) || res.Repos[1].Err != nil || len(res.PRs) != 1 {
		t.Errorf("Repos = %+v, PRs = %d; want bad/app failing with ErrAuth and ok/app read", res.Repos, len(res.PRs))
	}
	if h := c.Health(); h.Err == nil {
		t.Error("Health has no error after a token failure")
	}
}
