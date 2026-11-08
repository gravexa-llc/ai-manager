package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// RepoNumbers asks for some PRs of one repo by number.
type RepoNumbers struct {
	// Owner and Name are the repo, as stored (GitHub's nameWithOwner) or
	// as a PR URL names it. After a transfer the old name still resolves.
	Owner, Name string
	// Numbers are PR numbers. Repeats are dropped; a number below 1 comes
	// back with ErrInvalid.
	Numbers []int
}

// NumberPRsResult is the answer to one PRsByNumber call.
type NumberPRsResult struct {
	// Repos has one entry per distinct requested repo, in request order.
	Repos []RepoNumberPRs
	// Queries is how many GraphQL requests were sent, Cost the points
	// GitHub charged for them.
	Queries int
	Cost    int
}

// RepoNumberPRs is one repo's answer.
type RepoNumberPRs struct {
	// Owner and Name are as requested.
	Owner, Name string
	// Repo is the nameWithOwner GitHub answered with: the key to store the
	// PRs under. Empty if GitHub never answered for this repo.
	Repo string
	// Numbers has one entry per distinct requested number, in request
	// order.
	Numbers []NumberPR
	// Err is nil when every number was read. Otherwise it is the first
	// failure (ErrNotFound for a missing repo or PR, ErrAuth,
	// ErrUnavailable, a *QueryError, ...), and the numbers it hit have
	// their own Err.
	Err error
	// StaleSince is zero when Err is nil. Otherwise it is when this
	// repo's numbers were last all read, or, if never, when the first
	// attempt failed.
	StaleSince time.Time
}

// NumberPR is one PR's answer.
type NumberPR struct {
	Number int
	// PR is the pull request, in whatever state it is; nil when Err is
	// set.
	PR *PR
	// Err is set when the PR couldn't be read: the caller keeps what it
	// had.
	Err error
}

// PRsByNumber reads PRs by number, OPEN, MERGED and CLOSED alike, with
// one aliased pullRequest(number:) field per PR. It is how aimd reads a
// PR it knows only by its URL (a link made before any poll read it), and
// one an agent just merged or closed.
//
// It batches like BranchPRs: as few queries as possible, one per token,
// split so that no query has more than MaxBranchesPerQuery PRs, sent one
// at a time; a query costs 1 point. Repos with the same owner/name (any
// case) are merged and numbers de-duplicated. A repo or PR GitHub can't
// find, or a failed query or token, only affects the repos and PRs
// involved: they come back with Err and StaleSince set, and the rest of
// the batch is read normally. PRsByNumber itself never fails.
func (c *Client) PRsByNumber(ctx context.Context, repos []RepoNumbers) NumberPRsResult {
	var res NumberPRsResult
	type target struct{ owner, name string }
	var targets []target
	repoErr := map[int]error{}
	index := map[string]int{}

	for _, rn := range repos {
		owner, name := strings.TrimSpace(rn.Owner), strings.TrimSpace(rn.Name)
		key := repoKey(owner, name)
		i, ok := index[key]
		if !ok {
			i = len(res.Repos)
			index[key] = i
			res.Repos = append(res.Repos, RepoNumberPRs{Owner: owner, Name: name})
			to, tn := c.queryName(owner, name)
			targets = append(targets, target{to, tn})
			if !loginRE.MatchString(owner) || !repoNameRE.MatchString(name) {
				repoErr[i] = fmt.Errorf("%w: repo %q/%q", ErrInvalid, owner, name)
			}
		}
		r := &res.Repos[i]
		for _, n := range rn.Numbers {
			if slices.ContainsFunc(r.Numbers, func(have NumberPR) bool { return have.Number == n }) {
				continue
			}
			np := NumberPR{Number: n}
			if n < 1 {
				np.Err = fmt.Errorf("%w: PR number %d", ErrInvalid, n)
			}
			r.Numbers = append(r.Numbers, np)
		}
	}

	// One group of repos per distinct token, in request order.
	var callErr error
	attempted := false
	type group struct {
		owner string
		token string
		repos []int
	}
	var groups []*group
	byToken := map[string]*group{}
	for i := range res.Repos {
		if repoErr[i] != nil || !slices.ContainsFunc(res.Repos[i].Numbers, func(np NumberPR) bool { return np.Err == nil }) {
			continue
		}
		attempted = true
		tok, err := c.tokens.Token(ctx, targets[i].owner)
		if err != nil {
			repoErr[i] = err
			callErr = firstErr(callErr, err)
			continue
		}
		g := byToken[tok]
		if g == nil {
			g = &group{owner: targets[i].owner, token: tok}
			byToken[tok] = g
			groups = append(groups, g)
		}
		g.repos = append(g.repos, i)
	}

	for _, g := range groups {
		chunks := c.pack(g.repos, func(i int) []int {
			var ns []int
			for k, np := range res.Repos[i].Numbers {
				if np.Err == nil {
					ns = append(ns, k)
				}
			}
			return ns
		})
		for _, ch := range chunks {
			for k := range ch {
				ch[k].owner, ch[k].name = targets[ch[k].repo].owner, targets[ch[k].repo].name
			}
			if err := c.runNumberChunk(ctx, g.owner, g.token, ch, &res, repoErr); err != nil {
				callErr = firstErr(callErr, err)
			}
		}
	}

	now := c.now()
	for i := range res.Repos {
		r := &res.Repos[i]
		r.Err = repoErr[i]
		for k := range r.Numbers {
			np := &r.Numbers[k]
			if r.Err != nil && np.Err == nil && np.PR == nil {
				np.Err = r.Err
			}
			r.Err = firstErr(r.Err, np.Err)
		}
		key := "numbers:" + repoKey(r.Owner, r.Name)
		if r.Err == nil {
			c.markFresh(key, now)
		} else {
			r.StaleSince = c.markStale(key, now)
		}
	}
	if attempted {
		c.endCall(now, callErr)
	}
	return res
}

// buildNumberQuery writes the aliased query for one chunk: rN per repo,
// p<number> per PR, with variables oN and nN. The numbers are integers,
// written into the query as they are.
func buildNumberQuery(ch []chunkRepo, all []RepoNumberPRs) (string, map[string]any) {
	var decl []string
	var body strings.Builder
	vars := map[string]any{}
	for ri, cr := range ch {
		o, n := fmt.Sprintf("o%d", ri), fmt.Sprintf("n%d", ri)
		decl = append(decl, "$"+o+": String!", "$"+n+": String!")
		vars[o], vars[n] = cr.owner, cr.name
		fmt.Fprintf(&body, "  r%d: repository(owner: $%s, name: $%s) {\n    nameWithOwner\n", ri, o, n)
		for _, k := range cr.branches {
			num := all[cr.repo].Numbers[k].Number
			fmt.Fprintf(&body, "    p%d: pullRequest(number: %d) { ...PRFields }\n", num, num)
		}
		body.WriteString("  }\n")
	}
	q := "query(" + strings.Join(decl, ", ") + ") {\n" +
		"  rateLimit { cost remaining resetAt limit used }\n" +
		body.String() + "}\n" + prFields
	return q, vars
}

// runNumberChunk sends one chunk and fills in its repos and PRs. It
// returns the request's error, if the whole request failed.
func (c *Client) runNumberChunk(ctx context.Context, owner, token string, ch []chunkRepo, res *NumberPRsResult, repoErr map[int]error) error {
	q, vars := buildNumberQuery(ch, res.Repos)
	resp, err := c.do(ctx, owner, token, q, vars)
	res.Queries++
	if err != nil {
		for _, cr := range ch {
			for _, k := range cr.branches {
				res.Repos[cr.repo].Numbers[k].Err = err
			}
		}
		return err
	}
	res.Cost += resp.cost()
	general := resp.generalError()

	for ri, cr := range ch {
		alias := fmt.Sprintf("r%d", ri)
		r := &res.Repos[cr.repo]
		raw := resp.Data[alias]
		if isNull(raw) {
			e := firstErr(resp.errorAt(alias), general, fmt.Errorf("%w: no data for %s/%s", ErrUnavailable, cr.owner, cr.name))
			repoErr[cr.repo] = firstErr(repoErr[cr.repo], e)
			for _, k := range cr.branches {
				r.Numbers[k].Err = e
			}
			if errors.Is(e, ErrNotFound) {
				c.forgetCanonical(r.Owner, r.Name)
			}
			continue
		}
		var repo map[string]json.RawMessage
		if err := json.Unmarshal(raw, &repo); err != nil {
			e := fmt.Errorf("%w: decode %s: %v", ErrUnavailable, alias, err)
			repoErr[cr.repo] = firstErr(repoErr[cr.repo], e)
			for _, k := range cr.branches {
				r.Numbers[k].Err = e
			}
			continue
		}
		var nwo string
		_ = json.Unmarshal(repo["nameWithOwner"], &nwo)
		if nwo != "" {
			r.Repo = nwo
			c.setCanonical(r.Owner, r.Name, nwo)
		} else {
			nwo = r.Owner + "/" + r.Name
		}
		for _, k := range cr.branches {
			np := &r.Numbers[k]
			palias := fmt.Sprintf("p%d", np.Number)
			praw := repo[palias]
			if isNull(praw) {
				np.Err = firstErr(resp.errorAt(alias, palias), general,
					fmt.Errorf("%w: no data for PR #%d", ErrUnavailable, np.Number))
				continue
			}
			var node rawPR
			if err := json.Unmarshal(praw, &node); err != nil || node.Number != np.Number {
				np.Err = fmt.Errorf("%w: decode PR #%d: %v", ErrUnavailable, np.Number, err)
				continue
			}
			pr := node.toPR(nwo)
			np.PR = &pr
		}
	}
	return nil
}
