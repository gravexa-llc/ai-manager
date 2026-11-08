package github

import (
	"context"
	"fmt"
	"strings"
)

// RepoPRsResult is the answer to one RepoPRs call.
type RepoPRsResult struct {
	// PRs are the PRs found in every repo whose search succeeded, in any
	// state. PRs from forks are kept (CrossRepository is set).
	PRs []PR
	// Repos has one entry per distinct repo asked for, in order.
	Repos []RepoSearch
	// Truncated is true when a repo had more PRs than a search returns
	// (1000); the ones updated longest ago are missing.
	Truncated bool
	// Queries is how many GraphQL requests were sent; Cost the points
	// charged for them.
	Queries int
	Cost    int
}

// RepoSearch is one repo's part of a RepoPRs call.
type RepoSearch struct {
	// Repo is "owner/name" as asked.
	Repo string
	// Name is the nameWithOwner GitHub answered with, from the PRs found:
	// the canonical name, which differs from Repo after a transfer. Empty
	// when the repo has no PRs or the search failed.
	Name string
	// PRs counts the PRs found.
	PRs int
	// Err is nil when the search completed.
	Err error
}

// RepoPRs reads every pull request of each repo ("owner/name"), open,
// merged and closed, with one search per repo (is:pr repo:<owner/name>
// sort:updated-desc, 100 a page, 1 point a page, at most 1000 PRs). aimd
// uses it once per repo when a plan is registered, so the plan's board
// gets the PRs whose branches the per-branch query never asked about:
// those merged before aimd ran, or whose worktree is gone.
//
// A repo that isn't owner/name, or whose token or search fails, comes back
// with Err; the others are unaffected. RepoPRs itself never fails.
func (c *Client) RepoPRs(ctx context.Context, repos []string) RepoPRsResult {
	var res RepoPRsResult
	seen := map[string]bool{}
	var callErr error
	attempted := false
	keys := map[string]bool{}
	for _, repo := range repos {
		repo = strings.TrimSpace(repo)
		if seen[strings.ToLower(repo)] {
			continue
		}
		seen[strings.ToLower(repo)] = true
		st := RepoSearch{Repo: repo}
		owner, name, ok := strings.Cut(repo, "/")
		if !ok || !loginRE.MatchString(owner) || !repoNameRE.MatchString(name) {
			st.Err = fmt.Errorf("%w: repo %q", ErrInvalid, repo)
			res.Repos = append(res.Repos, st)
			continue
		}
		attempted = true
		owner, name = c.queryName(owner, name)
		tok, err := c.tokens.Token(ctx, owner)
		if err != nil {
			st.Err = err
			callErr = firstErr(callErr, err)
			res.Repos = append(res.Repos, st)
			continue
		}
		var counts OpenPRsResult
		prs, truncated, err := c.search(ctx, owner, tok, "is:pr repo:"+owner+"/"+name+" sort:updated-desc", &counts)
		res.Queries += counts.Queries
		res.Cost += counts.Cost
		if err != nil {
			st.Err = err
			callErr = firstErr(callErr, err)
			res.Repos = append(res.Repos, st)
			continue
		}
		res.Truncated = res.Truncated || truncated
		st.PRs = len(prs)
		for _, pr := range prs {
			if st.Name == "" {
				st.Name = pr.Repo
			}
			k := strings.ToLower(pr.Repo) + "#" + fmt.Sprint(pr.Number)
			if !keys[k] {
				keys[k] = true
				res.PRs = append(res.PRs, pr)
			}
		}
		res.Repos = append(res.Repos, st)
	}
	if attempted {
		c.endCall(c.now(), callErr)
	}
	return res
}
