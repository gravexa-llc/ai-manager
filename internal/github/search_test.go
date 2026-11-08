package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

var threeOwners = []string{"example-user", "acme", "example-org"}

// fixtureSearch answers every search with a fixture file.
func fixtureSearch(t *testing.T, name string) func(fakeRequest) any {
	return func(fakeRequest) any {
		var v any
		if err := json.Unmarshal(readFixture(t, name), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
}

func prKeys(prs []PR) []string {
	var keys []string
	for _, p := range prs {
		keys = append(keys, fmt.Sprintf("%s#%d", p.Repo, p.Number))
	}
	slices.Sort(keys)
	return keys
}

// The first read after a change: GitHub hasn't computed mergeable for five
// PRs yet, and they must come back pending, not as conflicts.
func TestOpenPRsFirstReadPending(t *testing.T) {
	f := newFakeGitHub(t)
	f.search = fixtureSearch(t, "open_prs_mine_first_read.json")
	clk := &clock{now: t0}
	c := f.client(nil, clk, Options{})

	res := c.OpenPRs(context.Background(), threeOwners)

	reqs := f.reqs()
	if len(reqs) != 1 || res.Queries != 1 || res.Cost != 1 {
		t.Fatalf("%d requests, Queries %d, Cost %d; want 1, 1, 1", len(reqs), res.Queries, res.Cost)
	}
	q, _ := reqs[0].Vars["q"].(string)
	for _, part := range []string{"is:pr", "is:open", "archived:false", "author:@me", "user:example-user", "user:acme", "user:example-org"} {
		if !strings.Contains(" "+q+" ", " "+part+" ") {
			t.Errorf("search %q lacks %q", q, part)
		}
	}
	if _, ok := reqs[0].Vars["after"]; ok {
		t.Error("first page sent an after cursor")
	}
	if len(res.PRs) != 17 || res.Truncated {
		t.Fatalf("%d PRs (truncated %v), want 17", len(res.PRs), res.Truncated)
	}
	for _, st := range res.Owners {
		if st.Err != nil {
			t.Errorf("owner %s: %v", st.Owner, st.Err)
		}
	}

	var pending []string
	for _, p := range res.PRs {
		if p.State != StateOpen {
			t.Errorf("%s#%d is %s", p.Repo, p.Number, p.State)
		}
		if p.Mergeable == MergePending {
			if !p.Mergeable.Pending() || p.MergeState != MergeStateUnknown {
				t.Errorf("%s#%d: pending but %q / %q", p.Repo, p.Number, p.Mergeable, p.MergeState)
			}
			pending = append(pending, fmt.Sprintf("%s#%d", p.Repo, p.Number))
		}
	}
	slices.Sort(pending)
	want := []string{"example-org/meadow-backend#126", "example-org/meadow-backend#188", "example-org/meadow-backend#71", "example-org/meadow-ios#342", "example-user/weekly-digest#5"}
	if !reflect.DeepEqual(pending, want) {
		t.Errorf("pending = %v, want %v", pending, want)
	}
}

// A minute later the same PRs have resolved, four of them to conflicts.
func TestOpenPRsSecondReadConflicts(t *testing.T) {
	f := newFakeGitHub(t)
	f.search = fixtureSearch(t, "open_prs_mine_second_read.json")
	res := f.client(nil, nil, Options{}).OpenPRs(context.Background(), threeOwners)

	var conflicts, drafts, clean []string
	for _, p := range res.PRs {
		key := fmt.Sprintf("%s#%d", p.Repo, p.Number)
		if p.Mergeable.Pending() {
			t.Errorf("%s still pending", key)
		}
		switch {
		case p.Mergeable == Conflicting:
			if p.MergeState != MergeStateDirty {
				t.Errorf("%s conflicting with MergeState %q", key, p.MergeState)
			}
			conflicts = append(conflicts, key)
		case p.MergeState == MergeStateClean:
			clean = append(clean, key)
		}
		if p.Draft {
			drafts = append(drafts, key)
		}
	}
	slices.Sort(conflicts)
	wantConflicts := []string{"acme/scoreboard#518", "example-org/meadow-backend#126", "example-org/meadow-backend#188", "example-org/meadow-ios#342"}
	if !reflect.DeepEqual(conflicts, wantConflicts) {
		t.Errorf("conflicts = %v, want %v", conflicts, wantConflicts)
	}
	// The fifth first-read UNKNOWN resolved the other way.
	if !slices.Contains(clean, "example-org/meadow-backend#71") {
		t.Errorf("meadow-backend#71 not clean: %v", clean)
	}
	if len(drafts) != 6 || len(clean) != 13 {
		t.Errorf("%d drafts, %d clean; want 6, 13", len(drafts), len(clean))
	}
}

// Search pages are followed with the end cursor.
func TestOpenPRsPaginates(t *testing.T) {
	var full map[string]any
	f := newFakeGitHub(t)
	if err := json.Unmarshal(readFixture(t, "open_prs_mine_first_read.json"), &full); err != nil {
		t.Fatal(err)
	}
	search := full["data"].(map[string]any)["search"].(map[string]any)
	nodes := search["nodes"].([]any)
	page := func(from, to int, next bool, cursor string) any {
		return map[string]any{"data": map[string]any{
			"rateLimit": full["data"].(map[string]any)["rateLimit"],
			"search": map[string]any{
				"issueCount": len(nodes),
				"pageInfo":   map[string]any{"hasNextPage": next, "endCursor": cursor},
				"nodes":      nodes[from:to],
			},
		}}
	}
	f.search = func(req fakeRequest) any {
		switch req.Vars["after"] {
		case nil:
			return page(0, 6, true, "c1")
		case "c1":
			return page(6, 12, true, "c2")
		case "c2":
			return page(12, len(nodes), false, "c3")
		}
		t.Errorf("unexpected cursor %v", req.Vars["after"])
		return page(0, 0, false, "")
	}
	res := f.client(nil, nil, Options{}).OpenPRs(context.Background(), threeOwners)
	if res.Queries != 3 || len(res.PRs) != 17 || res.Truncated {
		t.Errorf("Queries %d, %d PRs, truncated %v; want 3, 17, false", res.Queries, len(res.PRs), res.Truncated)
	}
	reqs := f.reqs()
	for _, r := range reqs[1:] {
		if r.Vars["q"] != reqs[0].Vars["q"] {
			t.Errorf("page query changed: %v", r.Vars["q"])
		}
	}
}

// A search that never ends is cut off, and says so.
func TestOpenPRsTruncated(t *testing.T) {
	f := newFakeGitHub(t)
	n := 0
	f.search = func(fakeRequest) any {
		n++
		return map[string]any{"data": map[string]any{
			"rateLimit": map[string]any{"cost": 1, "remaining": 4000, "resetAt": t0.Add(30 * time.Minute)},
			"search": map[string]any{
				"issueCount": 5000,
				"pageInfo":   map[string]any{"hasNextPage": true, "endCursor": fmt.Sprint("c", n)},
				"nodes":      []any{map[string]any{"number": n, "state": "OPEN", "repository": map[string]any{"nameWithOwner": "o/r"}}},
			},
		}}
	}
	res := f.client(nil, nil, Options{}).OpenPRs(context.Background(), []string{"o"})
	if !res.Truncated || res.Queries != maxSearchPages || len(res.PRs) != maxSearchPages {
		t.Errorf("Truncated %v, Queries %d, %d PRs; want true, %d, %d", res.Truncated, res.Queries, len(res.PRs), maxSearchPages, maxSearchPages)
	}
	if res.Owners[0].Err != nil {
		t.Errorf("truncation is not an error: %v", res.Owners[0].Err)
	}
}

// Owners read by different accounts get a search each; one failing leaves
// the other's results and marks only its owners stale.
func TestOpenPRsPerToken(t *testing.T) {
	f := newFakeGitHub(t)
	f.search = func(req fakeRequest) any {
		q := req.Vars["q"].(string)
		var v map[string]any
		if err := json.Unmarshal(readFixture(t, "open_prs_mine_second_read.json"), &v); err != nil {
			t.Fatal(err)
		}
		// Keep only the PRs of the owners in this search.
		s := v["data"].(map[string]any)["search"].(map[string]any)
		var kept []any
		for _, n := range s["nodes"].([]any) {
			repo := n.(map[string]any)["repository"].(map[string]any)["nameWithOwner"].(string)
			owner, _, _ := strings.Cut(repo, "/")
			if strings.Contains(q, "user:"+owner) {
				kept = append(kept, n)
			}
		}
		s["nodes"] = kept
		return v
	}
	toks := &fakeTokens{tokens: map[string]string{"example-user": "tok-a", "acme": "tok-a", "example-org": "tok-b"}}
	clk := &clock{now: t0}
	c := f.client(toks, clk, Options{})

	res := c.OpenPRs(context.Background(), threeOwners)
	reqs := f.reqs()
	if len(reqs) != 2 {
		t.Fatalf("%d searches, want 2 (one per token)", len(reqs))
	}
	byToken := map[string]string{}
	for _, r := range reqs {
		byToken[r.Token] = r.Vars["q"].(string)
	}
	if q := byToken["tok-a"]; !strings.Contains(q, "user:example-user") || !strings.Contains(q, "user:acme") || strings.Contains(q, "example-org") {
		t.Errorf("tok-a search = %q", q)
	}
	if q := byToken["tok-b"]; !strings.Contains(q, "user:example-org") || strings.Contains(q, "acme") {
		t.Errorf("tok-b search = %q", q)
	}
	if len(res.PRs) != 17 {
		t.Errorf("%d PRs across both searches, want 17", len(res.PRs))
	}

	// example-org's account loses its token.
	clk.Advance(time.Minute)
	toks.setErr("example-org", fmt.Errorf("%w: gh: not logged in", ErrAuth))
	res = c.OpenPRs(context.Background(), threeOwners)
	for _, st := range res.Owners {
		switch st.Owner {
		case "example-org":
			if !errors.Is(st.Err, ErrAuth) || !st.StaleSince.Equal(t0) {
				t.Errorf("example-org: Err %v, StaleSince %v; want ErrAuth, stale since %v", st.Err, st.StaleSince, t0)
			}
		default:
			if st.Err != nil || !st.StaleSince.IsZero() {
				t.Errorf("%s: Err %v, StaleSince %v", st.Owner, st.Err, st.StaleSince)
			}
		}
	}
	for _, p := range res.PRs {
		if strings.HasPrefix(p.Repo, "example-org/") {
			t.Errorf("PR %s#%d from the failed owner", p.Repo, p.Number)
		}
	}
	if h := c.Health(); !h.Stale() || !errors.Is(h.Err, ErrAuth) {
		t.Errorf("Health() = %+v; want stale with ErrAuth", h)
	}
}

// nil owners means the configured ones; duplicates and bad names are
// handled without a query for them.
func TestOpenPRsOwners(t *testing.T) {
	f := newFakeGitHub(t)
	f.search = fixtureSearch(t, "open_prs_any_author.json")
	c := New(configWithOwners(map[string]string{"acme": "acct", "Example-Org": "acct"}), Options{
		Endpoint: f.srv.URL, Tokens: staticTokens("tok"),
	})
	res := c.OpenPRs(context.Background(), nil)
	var got []string
	for _, st := range res.Owners {
		got = append(got, st.Owner)
	}
	if want := []string{"acme", "Example-Org"}; !reflect.DeepEqual(got, want) {
		t.Errorf("owners = %v, want %v", got, want)
	}
	if len(res.PRs) != 19 {
		t.Errorf("%d PRs, want 19", len(res.PRs))
	}

	res = c.OpenPRs(context.Background(), []string{"a", "A", "not an owner"})
	if len(res.Owners) != 2 || res.Owners[0].Err != nil || !errors.Is(res.Owners[1].Err, ErrInvalid) {
		t.Errorf("owners = %+v", res.Owners)
	}
	if q := f.reqs()[1].Vars["q"].(string); strings.Count(q, "user:") != 1 {
		t.Errorf("search %q, want one user: qualifier", q)
	}
}

func TestOpenPRsSplitsManyOwners(t *testing.T) {
	f := newFakeGitHub(t)
	f.search = func(fakeRequest) any {
		return map[string]any{"data": map[string]any{
			"rateLimit": map[string]any{"cost": 1, "remaining": 4000, "resetAt": t0.Add(30 * time.Minute)},
			"search":    map[string]any{"issueCount": 0, "pageInfo": map[string]any{"hasNextPage": false}, "nodes": []any{}},
		}}
	}
	var owners []string
	for i := range 23 {
		owners = append(owners, fmt.Sprint("owner", i))
	}
	res := f.client(nil, nil, Options{}).OpenPRs(context.Background(), owners)
	if res.Queries != 3 {
		t.Errorf("%d searches for 23 owners, want 3", res.Queries)
	}
	for _, r := range f.reqs() {
		if n := strings.Count(r.Vars["q"].(string), "user:"); n > maxOwnersPerSearch {
			t.Errorf("search with %d owners", n)
		}
	}
}

// Every PR the search returns carries the same fields as the branch query.
func TestOpenPRsFields(t *testing.T) {
	f := newFakeGitHub(t)
	f.search = fixtureSearch(t, "open_prs_mine_second_read.json")
	f.bodies = nil
	res := f.client(nil, nil, Options{}).OpenPRs(context.Background(), threeOwners)
	if q := f.reqs()[0].Query; !strings.Contains(q, "...PRFields") || !strings.Contains(q, "repository { nameWithOwner }") || !strings.Contains(q, "createdAt") {
		t.Errorf("search query doesn't use the shared fragment:\n%s", f.reqs()[0].Query)
	}
	for _, p := range res.PRs {
		if p.Repo == "" || p.Number == 0 || p.URL == "" || p.HeadOID == "" || p.BaseRef == "" || p.Author == "" || p.UpdatedAt.IsZero() || p.CreatedAt.IsZero() {
			t.Errorf("incomplete PR %+v", p)
		}
	}
}
