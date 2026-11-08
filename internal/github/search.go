package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	// maxSearchPages bounds pagination (100 per page): GitHub search returns
	// at most 1000 results anyway.
	maxSearchPages = 10
	// maxOwnersPerSearch bounds the user: qualifiers in one search query.
	maxOwnersPerSearch = 10
)

// searchDoc is the cross-owner search for open PRs, one page at a time.
const searchDoc = `query($q: String!, $after: String) {
  rateLimit { cost remaining resetAt limit used }
  search(query: $q, type: ISSUE, first: 100, after: $after) {
    issueCount
    pageInfo { hasNextPage endCursor }
    nodes { ... on PullRequest { repository { nameWithOwner } ...PRFields } }
  }
}
` + prFields

// OpenPRsResult is the answer to one OpenPRs or ReviewRequests call.
type OpenPRsResult struct {
	// PRs are the open PRs found, from every owner whose search succeeded.
	// PRs from forks are kept (CrossRepository is set).
	PRs []PR
	// Owners has one entry per owner searched, in order. An owner with Err
	// set contributed no PRs: keep its previous ones.
	Owners []OwnerStatus
	// Truncated is true when a search had more results than were read
	// (over 1000).
	Truncated bool
	// Queries is how many GraphQL requests were sent; Cost the points
	// charged for them.
	Queries int
	Cost    int
}

// OwnerStatus is one owner's part of an OpenPRs or ReviewRequests call.
type OwnerStatus struct {
	Owner string
	// Err is nil when the owner's search completed.
	Err error
	// StaleSince is zero when Err is nil. Otherwise it is when this owner's
	// search last succeeded, or, if never, when it first failed.
	StaleSince time.Time
}

// OpenPRs searches for open PRs authored by the user across owners
// (is:pr is:open archived:false author:@me user:<owner>...). nil owners
// means the owners in the config's [owners] table.
//
// "The user" is the account whose token reads each owner, so owners whose
// tokens are the same share one search, and owners read by different
// accounts get one search each. Results are paginated, 100 per page; one
// page costs 1 point. Owners whose token or search fails come back with Err
// and StaleSince, and the others are unaffected. OpenPRs itself never fails.
func (c *Client) OpenPRs(ctx context.Context, owners []string) OpenPRsResult {
	return c.searchOwners(ctx, owners, "search:", searchQuery)
}

// ReviewRequests searches for open PRs that ask for the user's review
// across owners (is:pr is:open archived:false review-requested:@me
// user:<owner>...). nil owners means the owners in the config's
// [owners] table. It finds requests made to the user and to teams the user
// is in, on PRs anyone opened.
//
// It is OpenPRs with another search string, and works the same way: one
// search per account, 100 PRs a page, 1 point a page, failures per owner,
// never failing itself. So an owner with no Err has its complete answer:
// a PR of that owner it doesn't return asks for no review of the user's.
func (c *Client) ReviewRequests(ctx context.Context, owners []string) OpenPRsResult {
	return c.searchOwners(ctx, owners, "review:", reviewQuery)
}

// searchOwners runs one search per account for owners (the configured ones
// if nil), with the search string query builds for a group of owners.
// freshKey prefixes the owners' freshness keys.
func (c *Client) searchOwners(ctx context.Context, owners []string, freshKey string, query func(owners []string) string) OpenPRsResult {
	if owners == nil {
		owners = c.owners
	}
	var res OpenPRsResult
	seen := map[string]bool{}
	for _, o := range owners {
		o = strings.TrimSpace(o)
		if seen[strings.ToLower(o)] {
			continue
		}
		seen[strings.ToLower(o)] = true
		st := OwnerStatus{Owner: o}
		if !loginRE.MatchString(o) {
			st.Err = fmt.Errorf("%w: owner %q", ErrInvalid, o)
		}
		res.Owners = append(res.Owners, st)
	}

	var callErr error
	attempted := false
	type group struct {
		owner  string
		token  string
		owners []int
	}
	var groups []*group
	byToken := map[string]*group{}
	for i, st := range res.Owners {
		if st.Err != nil {
			continue
		}
		attempted = true
		tok, err := c.tokens.Token(ctx, st.Owner)
		if err != nil {
			res.Owners[i].Err = err
			callErr = firstErr(callErr, err)
			continue
		}
		g := byToken[tok]
		if g == nil {
			g = &group{owner: st.Owner, token: tok}
			byToken[tok] = g
			groups = append(groups, g)
		}
		g.owners = append(g.owners, i)
	}

	keys := map[string]bool{}
	for _, g := range groups {
		for start := 0; start < len(g.owners); start += maxOwnersPerSearch {
			idx := g.owners[start:min(start+maxOwnersPerSearch, len(g.owners))]
			names := make([]string, len(idx))
			for k, i := range idx {
				names[k] = res.Owners[i].Owner
			}
			prs, truncated, err := c.search(ctx, g.owner, g.token, query(names), &res)
			if err != nil {
				for _, i := range idx {
					res.Owners[i].Err = err
				}
				callErr = firstErr(callErr, err)
				continue
			}
			res.Truncated = res.Truncated || truncated
			for _, pr := range prs {
				k := strings.ToLower(pr.Repo) + "#" + fmt.Sprint(pr.Number)
				if !keys[k] {
					keys[k] = true
					res.PRs = append(res.PRs, pr)
				}
			}
		}
	}

	now := c.now()
	for i := range res.Owners {
		st := &res.Owners[i]
		key := freshKey + strings.ToLower(st.Owner)
		if st.Err == nil {
			c.markFresh(key, now)
		} else {
			st.StaleSince = c.markStale(key, now)
		}
	}
	if attempted {
		c.endCall(now, callErr)
	}
	return res
}

// searchQuery builds the search string for owners. author:@me is the
// token's own login.
func searchQuery(owners []string) string {
	q := "is:pr is:open archived:false author:@me"
	for _, o := range owners {
		q += " user:" + o
	}
	return q
}

// reviewQuery builds the review-request search string for owners.
// review-requested:@me matches requests to the token's own login and to
// teams it is in.
func reviewQuery(owners []string) string {
	q := "is:pr is:open archived:false review-requested:@me"
	for _, o := range owners {
		q += " user:" + o
	}
	return q
}

// search runs one search query to the end (or maxSearchPages). Any failed
// page fails the whole search, so a partial list is never returned.
func (c *Client) search(ctx context.Context, owner, token, q string, res *OpenPRsResult) ([]PR, bool, error) {
	var prs []PR
	vars := map[string]any{"q": q}
	for page := 0; page < maxSearchPages; page++ {
		resp, err := c.do(ctx, owner, token, searchDoc, vars)
		res.Queries++
		if err != nil {
			return nil, false, err
		}
		res.Cost += resp.cost()
		raw := resp.Data["search"]
		if isNull(raw) {
			return nil, false, firstErr(resp.errorAt("search"), resp.generalError(),
				fmt.Errorf("%w: no search results in response", ErrUnavailable))
		}
		var s struct {
			IssueCount int `json:"issueCount"`
			PageInfo   struct {
				HasNextPage bool   `json:"hasNextPage"`
				EndCursor   string `json:"endCursor"`
			} `json:"pageInfo"`
			Nodes []rawPR `json:"nodes"`
		}
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, false, fmt.Errorf("%w: decode search: %v", ErrUnavailable, err)
		}
		for _, n := range s.Nodes {
			if n.Number == 0 { // not a PullRequest
				continue
			}
			prs = append(prs, n.toPR(""))
		}
		if !s.PageInfo.HasNextPage || s.PageInfo.EndCursor == "" {
			return prs, false, nil
		}
		vars = map[string]any{"q": q, "after": s.PageInfo.EndCursor}
	}
	return prs, true, nil
}
