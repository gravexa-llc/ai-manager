package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// repoNameRE matches a GitHub repo name.
var repoNameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

// RepoBranches asks for the PRs of some head branches in one repo.
type RepoBranches struct {
	// Owner and Name are the repo as the local remote names it. After a
	// transfer the old name still resolves (GitHub redirects), and the
	// answer carries the new one.
	Owner, Name string
	// Branches are head branch names, with or without "refs/heads/". Empty
	// names and repeats are dropped.
	Branches []string
}

// BranchPRsResult is the answer to one BranchPRs call.
type BranchPRsResult struct {
	// Repos has one entry per distinct requested repo, in request order.
	Repos []RepoPRs
	// Queries is how many GraphQL requests were sent.
	Queries int
	// Cost is the rate-limit points GitHub charged for them.
	Cost int
}

// RepoPRs is one repo's answer.
type RepoPRs struct {
	// Owner and Name are as requested.
	Owner, Name string
	// Repo is the nameWithOwner GitHub answered with: the key to store PRs
	// under. It differs from Owner/Name after a transfer (see Renamed).
	// Empty if GitHub never answered for this repo.
	Repo string
	// DefaultBranch is the repo's default branch, if GitHub answered.
	DefaultBranch string
	// Branches has one entry per distinct requested branch, in request
	// order.
	Branches []BranchPRs
	// Err is nil when every branch was read. Otherwise it is the first
	// failure (ErrNotFound for a missing repo, ErrAuth, ErrUnavailable, a
	// *QueryError, ...) and the branches it hit have their own Err.
	Err error
	// StaleSince is zero when Err is nil. Otherwise it is when this repo was
	// last read successfully, or, if never, when the first attempt failed.
	StaleSince time.Time
}

// Renamed reports whether GitHub answered under a different name than the
// one requested, such as after a transfer.
func (r RepoPRs) Renamed() bool {
	return r.Repo != "" && !strings.EqualFold(r.Repo, r.Owner+"/"+r.Name)
}

// BranchPRs is one head branch's PRs.
type BranchPRs struct {
	Branch string
	// TotalCount is how many PRs GitHub has with this head branch, including
	// any beyond the newest five and any from forks.
	TotalCount int
	// PRs are the newest PRs (up to five) whose head is this branch in this
	// repo, newest first. PRs from forks with the same branch name are
	// dropped. Empty with Err nil means the branch has no PR.
	PRs []PR
	// Err is set when this branch couldn't be read; PRs is then nil and the
	// caller should keep what it had.
	Err error
}

// BranchPRs reads the PRs of every given branch, OPEN, MERGED and CLOSED,
// with one aliased pullRequests(headRefName:) connection per branch.
//
// Every repo goes into as few queries as possible: one per token, split so
// that no query has more than MaxBranchesPerQuery branches. A query costs 1
// point however many branches it holds. Queries are sent one at a
// time, never in parallel. Repos with the same owner/name (any case) are
// merged and branches de-duplicated.
//
// A repo GitHub can't find, or a failed query or token, only affects the
// repos and branches involved: they come back with Err and StaleSince set,
// and the rest of the batch is read normally. BranchPRs itself never fails.
func (c *Client) BranchPRs(ctx context.Context, repos []RepoBranches) BranchPRsResult {
	var res BranchPRsResult
	type target struct{ owner, name string }
	var targets []target // per res.Repos index: what to ask GitHub for
	repoErr := map[int]error{}
	index := map[string]int{}

	for _, rb := range repos {
		owner, name := strings.TrimSpace(rb.Owner), strings.TrimSpace(rb.Name)
		key := repoKey(owner, name)
		i, ok := index[key]
		if !ok {
			i = len(res.Repos)
			index[key] = i
			res.Repos = append(res.Repos, RepoPRs{Owner: owner, Name: name})
			to, tn := c.queryName(owner, name)
			targets = append(targets, target{to, tn})
			if !loginRE.MatchString(owner) || !repoNameRE.MatchString(name) {
				repoErr[i] = fmt.Errorf("%w: repo %q/%q", ErrInvalid, owner, name)
			}
		}
		r := &res.Repos[i]
		for _, b := range rb.Branches {
			b = strings.TrimPrefix(strings.TrimSpace(b), "refs/heads/")
			if b != "" && !slices.ContainsFunc(r.Branches, func(have BranchPRs) bool { return have.Branch == b }) {
				r.Branches = append(r.Branches, BranchPRs{Branch: b})
			}
		}
	}

	// One group of repos per distinct token, in request order.
	var callErr error
	attempted := false
	type group struct {
		owner string // an owner the token was fetched for
		token string
		repos []int
	}
	var groups []*group
	byToken := map[string]*group{}
	for i := range res.Repos {
		if repoErr[i] != nil {
			continue
		}
		attempted = true
		tok, err := c.tokens.Token(ctx, targets[i].owner)
		if err != nil {
			repoErr[i] = err
			if callErr == nil {
				callErr = err
			}
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
		for _, ch := range c.chunk(res.Repos, g.repos) {
			for k := range ch {
				ch[k].owner, ch[k].name = targets[ch[k].repo].owner, targets[ch[k].repo].name
			}
			if err := c.runBranchChunk(ctx, g.owner, g.token, ch, &res, repoErr); err != nil && callErr == nil {
				callErr = err
			}
		}
	}

	now := c.now()
	for i := range res.Repos {
		r := &res.Repos[i]
		r.Err = repoErr[i]
		for b := range r.Branches {
			if r.Err != nil && r.Branches[b].Err == nil && r.Branches[b].PRs == nil {
				r.Branches[b].Err = r.Err
			}
			if r.Err == nil {
				r.Err = r.Branches[b].Err
			}
		}
		key := "repo:" + repoKey(r.Owner, r.Name)
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

// chunkRepo is one repo's share of a query: some of its branches (or, for
// PRsByNumber, some of its numbers).
type chunkRepo struct {
	repo        int   // index in BranchPRsResult.Repos (NumberPRsResult.Repos)
	branches    []int // indices in that repo's Branches (Numbers)
	owner, name string
}

// chunk packs repos into queries of at most c.maxBranches branches. A repo
// is kept whole when it fits in a query of its own, and split across
// queries only when it has more branches than that. A repo with no
// branches to read still takes a slot (it resolves the repo's name).
func (c *Client) chunk(all []RepoPRs, repos []int) [][]chunkRepo {
	return c.pack(repos, func(i int) []int {
		var bs []int
		for b, br := range all[i].Branches {
			if br.Err == nil {
				bs = append(bs, b)
			}
		}
		return bs
	})
}

// pack is chunk for any items: items(i) are the indices of repo i's items
// to read.
func (c *Client) pack(repos []int, items func(i int) []int) [][]chunkRepo {
	var chunks [][]chunkRepo
	var cur []chunkRepo
	n := 0
	flush := func() {
		if len(cur) > 0 {
			chunks = append(chunks, cur)
		}
		cur, n = nil, 0
	}
	for _, i := range repos {
		bs := items(i)
		need := max(len(bs), 1)
		if need <= c.maxBranches && n+need > c.maxBranches {
			flush()
		}
		if len(bs) == 0 {
			cur = append(cur, chunkRepo{repo: i})
			n++
			continue
		}
		for len(bs) > 0 {
			if n == c.maxBranches {
				flush()
			}
			take := min(c.maxBranches-n, len(bs))
			cur = append(cur, chunkRepo{repo: i, branches: bs[:take:take]})
			n += take
			bs = bs[take:]
		}
	}
	flush()
	return chunks
}

// buildBranchQuery writes the aliased query for one chunk: rN per repo, bM
// per branch, with variables oN, nN and rNbM.
func buildBranchQuery(ch []chunkRepo, all []RepoPRs) (string, map[string]any) {
	var decl []string
	var body strings.Builder
	vars := map[string]any{}
	for ri, cr := range ch {
		o, n := fmt.Sprintf("o%d", ri), fmt.Sprintf("n%d", ri)
		decl = append(decl, "$"+o+": String!", "$"+n+": String!")
		vars[o], vars[n] = cr.owner, cr.name
		fmt.Fprintf(&body, "  r%d: repository(owner: $%s, name: $%s) {\n    nameWithOwner defaultBranchRef { name }\n", ri, o, n)
		for bi, b := range cr.branches {
			v := fmt.Sprintf("r%db%d", ri, bi)
			decl = append(decl, "$"+v+": String!")
			vars[v] = all[cr.repo].Branches[b].Branch
			fmt.Fprintf(&body, "    b%d: pullRequests(headRefName: $%s, first: %d, orderBy: {field: CREATED_AT, direction: DESC}) { totalCount nodes { ...PRFields } }\n", bi, v, prsPerBranch)
		}
		body.WriteString("  }\n")
	}
	q := "query(" + strings.Join(decl, ", ") + ") {\n" +
		"  rateLimit { cost remaining resetAt limit used }\n" +
		body.String() + "}\n" + prFields
	return q, vars
}

// runBranchChunk sends one chunk and fills in its repos and branches. It
// returns the request's error, if the whole request failed.
func (c *Client) runBranchChunk(ctx context.Context, owner, token string, ch []chunkRepo, res *BranchPRsResult, repoErr map[int]error) error {
	q, vars := buildBranchQuery(ch, res.Repos)
	resp, err := c.do(ctx, owner, token, q, vars)
	res.Queries++
	if err != nil {
		for _, cr := range ch {
			for _, b := range cr.branches {
				res.Repos[cr.repo].Branches[b].Err = err
			}
			if len(cr.branches) == 0 && repoErr[cr.repo] == nil {
				repoErr[cr.repo] = err
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
			if repoErr[cr.repo] == nil {
				repoErr[cr.repo] = e
			}
			for _, b := range cr.branches {
				r.Branches[b].Err = e
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
			for _, b := range cr.branches {
				r.Branches[b].Err = e
			}
			continue
		}
		var nwo string
		_ = json.Unmarshal(repo["nameWithOwner"], &nwo)
		var def struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(repo["defaultBranchRef"], &def)
		if nwo != "" {
			r.Repo = nwo
			c.setCanonical(r.Owner, r.Name, nwo)
		}
		if def.Name != "" {
			r.DefaultBranch = def.Name
		}

		for bi, b := range cr.branches {
			balias := fmt.Sprintf("b%d", bi)
			br := &r.Branches[b]
			braw := repo[balias]
			if isNull(braw) {
				br.Err = firstErr(resp.errorAt(alias, balias), resp.errorAt(alias), general,
					fmt.Errorf("%w: no data for branch %s", ErrUnavailable, br.Branch))
				continue
			}
			var conn struct {
				TotalCount int     `json:"totalCount"`
				Nodes      []rawPR `json:"nodes"`
			}
			if err := json.Unmarshal(braw, &conn); err != nil {
				br.Err = fmt.Errorf("%w: decode branch %s: %v", ErrUnavailable, br.Branch, err)
				continue
			}
			br.TotalCount = conn.TotalCount
			br.PRs = make([]PR, 0, len(conn.Nodes))
			for _, n := range conn.Nodes {
				if n.IsCrossRepository || n.Number == 0 {
					continue
				}
				br.PRs = append(br.PRs, n.toPR(nwo))
			}
		}
	}
	return nil
}

// queryName returns the owner and name to ask GitHub for: the name it
// answered with last time, if it differed, else the requested one.
func (c *Client) queryName(owner, name string) (string, string) {
	c.mu.Lock()
	nwo, ok := c.canonical[repoKey(owner, name)]
	c.mu.Unlock()
	if ok {
		if o, n, found := strings.Cut(nwo, "/"); found {
			return o, n
		}
	}
	return owner, name
}

func (c *Client) setCanonical(owner, name, nwo string) {
	key := repoKey(owner, name)
	c.mu.Lock()
	if strings.EqualFold(nwo, owner+"/"+name) {
		delete(c.canonical, key)
	} else {
		c.canonical[key] = nwo
	}
	c.mu.Unlock()
}

// forgetCanonical drops a cached name that GitHub no longer resolves, so
// the next call asks for the requested name again.
func (c *Client) forgetCanonical(owner, name string) {
	c.mu.Lock()
	delete(c.canonical, repoKey(owner, name))
	c.mu.Unlock()
}

// repoKey is the case-insensitive key for a repo.
func repoKey(owner, name string) string {
	return strings.ToLower(owner + "/" + name)
}

// firstErr returns the first non-nil error.
func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
