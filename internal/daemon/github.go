package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/github"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
	"github.com/gravexa-llc/ai-manager/internal/worktrees"
)

// githubLoop runs one GitHub tick every poll.github_active, starting once
// the first worktree scan has said which branches exist. A dashboard
// connecting asks for the open-PR search between ticks if it is due, and
// RefreshPRs for a read of some PRs by number, at most one every
// refreshMinGap. Ticks and reads run one at a time, and a tick that runs
// past the next one skips it rather than queuing it (nextTick).
func (d *Daemon) githubLoop(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-d.firstScan:
	}
	every := d.cfg.Poll.GitHubActive
	tick := func() time.Duration {
		start := time.Now()
		d.githubTick(ctx)
		return time.Until(nextTick(start, time.Now(), every))
	}
	t := time.NewTimer(tick())
	defer t.Stop()
	var refresh <-chan time.Time // armed while a read is waiting for its gap
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			t.Reset(tick())
		case <-d.searchKick:
			d.maybeSearch(ctx, time.Now(), true)
		case <-d.refreshKick:
			if refresh == nil {
				d.mu.Lock()
				wait := time.Until(d.lastRefresh.Add(refreshMinGap))
				d.mu.Unlock()
				refresh = time.After(max(wait, 0))
			}
		case <-refresh:
			refresh = nil
			d.readRefreshes(ctx)
		}
	}
}

// Reads on request (RefreshPRs).
const (
	// refreshMinGap is the least time between two reads of PRs the plugin
	// saw a tool call create, merge or close. Requests in between join the
	// next read.
	refreshMinGap = 10 * time.Second
	// refreshMax caps the PRs waiting for such a read; more are dropped,
	// and the polls read them later.
	refreshMax = 50
)

// RefreshPRs asks for these PRs to be read from GitHub by number soon,
// rather than at the next poll: the plugin saw a tool call create, merge
// or close them. GitHub stays the source of truth: what is stored is what
// it answers. Requests are joined into one read, at most one every
// refreshMinGap, and none while the budget says stop. It returns how many
// PRs it queued that weren't waiting already. It never blocks.
func (d *Daemon) RefreshPRs(prs []store.PRKey) int {
	d.mu.Lock()
	queued := 0
	for _, k := range prs {
		key := prKey(k.Repo, k.Number)
		if _, ok := d.refreshWant[key]; ok || k.Number < 1 || !strings.Contains(k.Repo, "/") {
			continue
		}
		if len(d.refreshWant) >= refreshMax {
			d.log.Warn("PR refresh: too many waiting; the polls read the rest", "waiting", len(d.refreshWant))
			break
		}
		d.refreshWant[key] = k
		queued++
	}
	d.mu.Unlock()
	if queued > 0 {
		kick(d.refreshKick)
	}
	return queued
}

// readRefreshes reads the PRs RefreshPRs queued, by number, and stores
// what GitHub answers. A repo whose read fails is listed as unreadable
// like a poll's; one that reads isn't cleared from the list, since this
// read is only part of what the polls ask.
func (d *Daemon) readRefreshes(ctx context.Context) {
	now := time.Now()
	d.mu.Lock()
	want := d.refreshWant
	d.refreshWant = map[string]store.PRKey{}
	d.lastRefresh = now
	d.mu.Unlock()
	if len(want) == 0 {
		return
	}
	if b := d.budget(); b.Verdict == github.BudgetStop {
		d.log.Info("PR refresh skipped: the GitHub budget says stop; the polls read them later", "prs", len(want))
		return
	}
	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	byRepo := map[string]*github.RepoNumbers{}
	var repos []*github.RepoNumbers
	for _, k := range keys {
		pk := want[k]
		owner, name, _ := strings.Cut(pk.Repo, "/")
		rn := byRepo[strings.ToLower(pk.Repo)]
		if rn == nil {
			rn = &github.RepoNumbers{Owner: owner, Name: name}
			byRepo[strings.ToLower(pk.Repo)] = rn
			repos = append(repos, rn)
		}
		rn.Numbers = append(rn.Numbers, pk.Number)
	}
	req := make([]github.RepoNumbers, len(repos))
	for i, rn := range repos {
		req[i] = *rn
	}
	res := d.gh.PRsByNumber(ctx, req)
	if ctx.Err() != nil {
		return
	}
	var prs []github.PR
	errsChanged := false
	d.mu.Lock()
	for _, r := range res.Repos {
		if r.Err != nil {
			errsChanged = d.noteRepoErrLocked(readNumbers, r.Owner+"/"+r.Name, r.Err, now) || errsChanged
		}
		prs = append(prs, numberPRs(r)...)
	}
	d.mu.Unlock()
	if errsChanged {
		d.notify(api.TopicStatus)
	}
	changed, err := d.writePRs(ctx, prs)
	if err != nil {
		d.log.Warn("PR refresh: store", "err", err)
		return
	}
	d.log.Info("read PRs on request", "asked", len(want), "read", len(prs), "queries", res.Queries, "cost", res.Cost)
	if changed {
		d.notify(api.TopicPRs)
	}
}

// nextTick returns when the run after one that began at start and ended
// at end is due, running every interval: start+interval, or if the run
// took longer, the first start+n*interval at or after end. The ticks that
// fell while it ran are skipped, not queued. internal/sessions' poller
// does the same.
func nextTick(start, end time.Time, interval time.Duration) time.Time {
	next := start.Add(interval)
	if ran := end.Sub(start); interval > 0 && next.Before(end) {
		n := (ran + interval - 1) / interval // ran/interval, rounded up
		next = start.Add(n * interval)
	}
	return next
}

// githubTick is one GitHub tick: one BranchPRs call for the active repos,
// plus the idle ones on every idleEvery-th tick (tick 0 included), and on
// those idle ticks one PRsByNumber call for the open PRs aimd has never
// read (trackedRepos), then the open-PR search if it is due. The budget
// decides what runs: "idle-only" skips every tick that isn't an idle
// tick, and "stop" skips everything until its ResumeAt.
func (d *Daemon) githubTick(ctx context.Context) {
	now := time.Now()
	d.mu.Lock()
	n := d.ghTick
	d.ghTick++
	d.ghStatus.Tick = d.ghTick
	d.mu.Unlock()
	idleTick := n%d.idleEvery() == 0

	b := d.budget()
	switch b.Verdict {
	case github.BudgetStop:
		why := "budget: stop"
		if !b.ResumeAt.IsZero() {
			why += " until " + b.ResumeAt.Format(time.RFC3339)
		}
		d.skipped(api.SourceGitHub, now, why)
		d.skipped(api.SourceGitHubSearch, now, why)
		return
	case github.BudgetIdleOnly:
		if !idleTick {
			d.skipped(api.SourceGitHub, now, "budget: idle-only, waiting for the idle tick")
			return
		}
	}

	active, idle, unread := d.trackedRepos(ctx, now, idleTick)
	d.mu.Lock()
	d.ghStatus.ActiveRepos, d.ghStatus.IdleRepos = countRepos(active), countRepos(idle)
	d.mu.Unlock()
	req := active
	if idleTick {
		req = append(req, idle...)
	}
	if len(req) > 0 || len(unread) > 0 {
		var res github.BranchPRsResult
		if len(req) > 0 {
			res = d.gh.BranchPRs(ctx, req)
		}
		var nums github.NumberPRsResult
		if len(unread) > 0 && ctx.Err() == nil {
			nums = d.gh.PRsByNumber(ctx, unread)
		}
		if ctx.Err() != nil {
			return
		}
		d.applyBranchPRs(ctx, now, res, nums, idleTick)
	} else {
		d.mu.Lock()
		d.ghStatus.LastQueries, d.ghStatus.LastCost = 0, 0
		cleared := idleTick && len(d.repoErrs) > 0
		if idleTick {
			clear(d.repoErrs) // nothing left to read
		}
		d.mu.Unlock()
		if cleared {
			d.notify(api.TopicStatus)
		}
		d.ran(api.SourceGitHub, now, nil, 0)
	}
	d.maybeSearch(ctx, now, idleTick)
}

// budget reads the client's budget and remembers it for Status, telling
// watchers when the verdict changes.
func (d *Daemon) budget() github.Budget {
	b := d.gh.Budget()
	nb := api.Budget{
		Verdict:   string(b.Verdict),
		Known:     b.Known,
		Remaining: b.Remaining,
		Limit:     b.Limit,
		ResetAt:   b.ResetAt,
		ResumeAt:  b.ResumeAt,
		LastCost:  b.LastCost,
	}
	d.mu.Lock()
	old := d.ghStatus.Budget
	changed := old.Verdict != nb.Verdict || old.Known != nb.Known
	d.ghStatus.Budget = nb
	d.mu.Unlock()
	if changed {
		if b.Known && (old.Known || b.Verdict != github.BudgetNormal) {
			d.log.Info("GitHub budget", "verdict", b.Verdict, "remaining", b.Remaining, "reset_at", b.ResetAt)
		}
		d.notify(api.TopicStatus)
	}
	return b
}

// searchCadenceLocked is how often the open-PR search runs now. d.mu is
// held.
func (d *Daemon) searchCadenceLocked() time.Duration {
	if d.watchers > 0 && d.ghStatus.Budget.Verdict != string(github.BudgetIdleOnly) {
		return d.cfg.Poll.GitHubActive
	}
	return d.cfg.Poll.GitHubIdle
}

// maybeSearch runs the open-PR search if it is due: every
// poll.github_active while a dashboard is connected, every
// poll.github_idle otherwise or while the budget is idle-only. allowed is
// false on a non-idle tick under an idle-only budget. Nothing runs while
// the budget says stop.
func (d *Daemon) maybeSearch(ctx context.Context, now time.Time, allowed bool) {
	b := d.budget()
	if b.Verdict == github.BudgetStop || (b.Verdict == github.BudgetIdleOnly && !allowed) {
		return
	}
	d.mu.Lock()
	cadence := d.searchCadenceLocked()
	due := d.lastSearch.IsZero() || now.Sub(d.lastSearch) >= cadence-time.Duration(float64(cadence)*searchSlack)
	if due {
		d.lastSearch = now
	}
	d.mu.Unlock()
	if !due {
		return
	}
	owners := d.searchOwners()
	if len(owners) == 0 {
		d.skipped(api.SourceGitHubSearch, now, "no owners: set [owners] in config.toml")
		return
	}
	res := d.gh.OpenPRs(ctx, owners)
	if ctx.Err() != nil {
		return
	}
	open := map[string]bool{}
	changed, partial, err := d.writeSearch(ctx, res, open)
	searchOK := partial == 0 && err == nil

	// The review-request search shares the cadence, the source and
	// the budget: 1 point a page per account.
	rev := d.gh.ReviewRequests(ctx, owners)
	if ctx.Err() != nil {
		return
	}
	revOpen := map[string]bool{}
	revChanged, revPartial, revErr := d.writeSearch(ctx, rev, revOpen)
	if revErr == nil {
		var n int64
		n, revErr = d.storeReviewRequests(ctx, now, rev)
		revChanged = revChanged || n > 0
	}
	if searchOK && revPartial == 0 && revErr == nil {
		for k := range revOpen {
			open[k] = true
		}
	}
	changed = changed || revChanged
	d.ran(api.SourceGitHubSearch, now, errors.Join(err, revErr), partial+revPartial)
	d.mu.Lock()
	if searchOK {
		d.searchOpen = open
	}
	d.mu.Unlock()
	d.log.Debug("open-PR search", "owners", len(owners), "prs", len(res.PRs), "review_requests", len(rev.PRs),
		"queries", res.Queries+rev.Queries, "cost", res.Cost+rev.Cost)
	if changed {
		d.notify(api.TopicPRs)
	}
}

// writeSearch stores one search's PRs and adds their keys to found. It
// returns whether a stored PR changed, how many owners' searches failed,
// and an error when the store failed or every owner's search did (then
// partial is 0).
func (d *Daemon) writeSearch(ctx context.Context, res github.OpenPRsResult, found map[string]bool) (changed bool, partial int, err error) {
	var failed []error
	for _, o := range res.Owners {
		if o.Err != nil {
			partial++
			failed = append(failed, fmt.Errorf("%s: %w", o.Owner, o.Err))
		}
	}
	for _, pr := range res.PRs {
		found[prKey(pr.Repo, pr.Number)] = true
	}
	changed, err = d.writePRs(ctx, res.PRs)
	if err == nil && partial == len(res.Owners) && partial > 0 {
		err = errors.Join(failed...) // every owner failed
		partial = 0
	}
	return changed, partial, err
}

// storeReviewRequests records which stored PRs ask for your review, from
// a review-request search whose PRs are already stored: for each owner
// whose search succeeded, the PRs it found ask for it and its other PRs
// don't. Owners whose search failed keep what they had, and so does
// everyone when the search was cut off (over 1000 results), since a PR
// past the cut can't be told from one with no request.
func (d *Daemon) storeReviewRequests(ctx context.Context, now time.Time, res github.OpenPRsResult) (int64, error) {
	if res.Truncated {
		d.log.Warn("review-request search cut off; keeping the review requests aimd had", "prs", len(res.PRs))
		return 0, nil
	}
	var owners []string
	for _, o := range res.Owners {
		if o.Err == nil {
			owners = append(owners, o.Owner)
		}
	}
	found := make([]store.PRKey, 0, len(res.PRs))
	for _, pr := range res.PRs {
		found = append(found, store.PRKey{Repo: pr.Repo, Number: pr.Number})
	}
	n, err := d.st.SetReviewRequests(ctx, owners, found, now)
	if err != nil {
		return 0, fmt.Errorf("store review requests: %w", err)
	}
	return n, nil
}

// searchOwners returns the owners to search: those in config's [owners]
// table, or, when it is empty, every owner a scanned repo's GitHub remote
// names.
func (d *Daemon) searchOwners() []string {
	var owners []string
	for o := range d.cfg.Owners {
		owners = append(owners, o)
	}
	if len(owners) == 0 {
		d.mu.Lock()
		if d.scan != nil {
			for i := range d.scan.Repos {
				if o, _, ok := d.scan.Repos[i].GitHubRepo(); ok && !slices.ContainsFunc(owners, func(have string) bool { return strings.EqualFold(have, o) }) {
					owners = append(owners, o)
				}
			}
		}
		d.mu.Unlock()
	}
	slices.SortFunc(owners, func(a, b string) int { return strings.Compare(strings.ToLower(a), strings.ToLower(b)) })
	return owners
}

// trackedRepos returns what to ask GitHub about, split into active and
// idle repos. Each repo's branches are every non-default branch checked
// out in one of its worktrees. A repo is active when a live session works
// in it, or when its worktrees changed or got a commit in the last
// activeWindow. On idle ticks, the idle set also carries the branches of
// stored open PRs that nothing else tracks and the last search didn't
// find, so a PR whose worktree was removed right after it merged still
// gets its final state.
//
// unread, on idle ticks only, are the stored open PRs with no head branch
// (not from a fork) that the last good search didn't find: the
// placeholders the link API stores for a PR it knows only by URL
// (store.PR.Unread). No branch query can find them, so they are read by
// number.
func (d *Daemon) trackedRepos(ctx context.Context, now time.Time, idleTick bool) (active, idle []github.RepoBranches, unread []github.RepoNumbers) {
	d.mu.Lock()
	sessionRepos := map[string]bool{}
	for _, l := range d.live {
		if !l.headless && l.repo != "" {
			sessionRepos[worktrees.PathKey(l.repo)] = true
		}
	}
	tracked := map[string]bool{} // lower-case "owner/name:branch"
	if d.scan != nil {
		for i := range d.scan.Repos {
			repo := &d.scan.Repos[i]
			owner, name, ok := repo.GitHubRepo()
			if !ok {
				continue
			}
			def := d.defaultBranch[strings.ToLower(owner+"/"+name)]
			var branches []string
			recent := false
			for j := range repo.Worktrees {
				w := &repo.Worktrees[j]
				if now.Sub(w.LastCommitAt) < activeWindow {
					recent = true
				}
				if w.Branch == "" || w.Branch == def || slices.Contains(branches, w.Branch) {
					continue
				}
				branches = append(branches, w.Branch)
			}
			if len(branches) == 0 {
				continue
			}
			for _, b := range branches {
				tracked[strings.ToLower(owner+"/"+name)+":"+b] = true
			}
			rb := github.RepoBranches{Owner: owner, Name: name, Branches: branches}
			changedAt := d.repoChangedAt[worktrees.PathKey(repo.GitDir)]
			if sessionRepos[worktrees.PathKey(repo.Path)] || recent || (!changedAt.IsZero() && now.Sub(changedAt) < activeWindow) {
				active = append(active, rb)
			} else {
				idle = append(idle, rb)
			}
		}
	}
	searchOpen := d.searchOpen
	d.mu.Unlock()

	if !idleTick {
		return active, idle, nil
	}
	open, err := d.st.ListPRs(ctx, store.PRFilter{State: store.PROpen})
	if err != nil {
		d.log.Warn("list open PRs", "err", err)
		return active, idle, nil
	}
	orphans := map[string]*github.RepoBranches{}
	var order []string
	numbers := map[string]*github.RepoNumbers{}
	var numOrder []string
	for _, p := range open {
		owner, name, ok := strings.Cut(p.Repo, "/")
		if !ok || p.IsCrossRepository || searchOpen[prKey(p.Repo, p.Number)] {
			continue
		}
		if p.HeadBranch == "" {
			k := strings.ToLower(p.Repo)
			rn := numbers[k]
			if rn == nil {
				rn = &github.RepoNumbers{Owner: owner, Name: name}
				numbers[k] = rn
				numOrder = append(numOrder, k)
			}
			rn.Numbers = append(rn.Numbers, p.Number)
			continue
		}
		if tracked[strings.ToLower(p.Repo)+":"+p.HeadBranch] {
			continue
		}
		k := strings.ToLower(p.Repo)
		rb := orphans[k]
		if rb == nil {
			rb = &github.RepoBranches{Owner: owner, Name: name}
			orphans[k] = rb
			order = append(order, k)
		}
		if !slices.Contains(rb.Branches, p.HeadBranch) {
			rb.Branches = append(rb.Branches, p.HeadBranch)
		}
	}
	for _, k := range order {
		idle = append(idle, *orphans[k])
	}
	for _, k := range numOrder {
		unread = append(unread, *numbers[k])
	}
	return active, idle, unread
}

// The kinds of read a repo can fail, each tracked on its own in repoErrs:
// a branch read that works doesn't hide a PR number GitHub can't find.
const (
	readBranches = "branches" // BranchPRs
	readNumbers  = "numbers"  // PRsByNumber
)

// repoErr is why a repo's last read of one kind failed.
type repoErr struct {
	repo  string    // owner/name as asked
	err   string    // the error
	since time.Time // when this run of failures began
}

// noteRepoErrLocked records the outcome of one read of a repo: err nil
// clears what a failed read of the same kind left. A new failure, or a
// different error, is logged. It reports whether the unreadable set
// changed. d.mu is held.
func (d *Daemon) noteRepoErrLocked(kind, repo string, err error, now time.Time) bool {
	key := kind + ":" + strings.ToLower(repo)
	old, had := d.repoErrs[key]
	if err == nil {
		delete(d.repoErrs, key)
		return had
	}
	msg := err.Error()
	if had && old.err == msg {
		return false
	}
	d.log.Warn("GitHub repo unreadable", "repo", repo, "read", kind, "err", err)
	since := now
	if had {
		since = old.since
	}
	d.repoErrs[key] = repoErr{repo: repo, err: msg, since: since}
	return true
}

// unreadableLocked lists the repos whose last read failed, one entry per
// repo (its branch read's error first), sorted by repo. d.mu is held.
func (d *Daemon) unreadableLocked() []api.RepoError {
	byRepo := map[string]api.RepoError{}
	for _, kind := range []string{readBranches, readNumbers} {
		for k, e := range d.repoErrs {
			if !strings.HasPrefix(k, kind+":") {
				continue
			}
			r := strings.ToLower(e.repo)
			if _, have := byRepo[r]; !have {
				byRepo[r] = api.RepoError{Repo: e.repo, Error: e.err, Since: e.since}
			}
		}
	}
	if len(byRepo) == 0 {
		return nil
	}
	out := make([]api.RepoError, 0, len(byRepo))
	for _, e := range byRepo {
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b api.RepoError) int { return strings.Compare(strings.ToLower(a.Repo), strings.ToLower(b.Repo)) })
	return out
}

// numberPRs returns the PRs a PRsByNumber answer read for one repo.
func numberPRs(r github.RepoNumberPRs) []github.PR {
	var out []github.PR
	for _, np := range r.Numbers {
		if np.Err == nil && np.PR != nil {
			out = append(out, *np.PR)
		}
	}
	return out
}

// countRepos counts distinct owner/name pairs.
func countRepos(rbs []github.RepoBranches) int {
	seen := map[string]bool{}
	for _, rb := range rbs {
		seen[strings.ToLower(rb.Owner+"/"+rb.Name)] = true
	}
	return len(seen)
}

// applyBranchPRs writes one tick's BranchPRs and PRsByNumber answers and
// records the tick's health. A repo either couldn't read counts as a
// partial error and is listed in Status as unreadable until a read of the
// same kind succeeds; full is true on an idle tick, which asks about every
// repo, so a repo it didn't ask about is no longer listed.
func (d *Daemon) applyBranchPRs(ctx context.Context, now time.Time, res github.BranchPRsResult, nums github.NumberPRsResult, full bool) {
	var prs []github.PR
	var repos []store.Repo // default branches not stored yet, or changed
	partial := 0
	d.mu.Lock()
	errsChanged := false
	asked := map[string]bool{}
	for _, r := range res.Repos {
		if r.DefaultBranch != "" {
			d.defaultBranch[strings.ToLower(r.Owner+"/"+r.Name)] = r.DefaultBranch
			if r.Repo != "" {
				d.defaultBranch[strings.ToLower(r.Repo)] = r.DefaultBranch
				if d.storedDefault[strings.ToLower(r.Repo)] != r.Repo+"\x00"+r.DefaultBranch {
					repos = append(repos, store.Repo{Name: r.Repo, DefaultBranch: r.DefaultBranch, UpdatedAt: now})
				}
			}
		}
		d.noteCanonicalLocked(r.Owner+"/"+r.Name, r.Repo)
		if r.Err != nil {
			partial++
		}
		asked[readBranches+":"+strings.ToLower(r.Owner+"/"+r.Name)] = true
		errsChanged = d.noteRepoErrLocked(readBranches, r.Owner+"/"+r.Name, r.Err, now) || errsChanged
		for _, b := range r.Branches {
			if b.Err == nil {
				prs = append(prs, b.PRs...)
			}
		}
	}
	var read []github.PR
	for _, r := range nums.Repos {
		if r.Err != nil {
			partial++
		}
		asked[readNumbers+":"+strings.ToLower(r.Owner+"/"+r.Name)] = true
		errsChanged = d.noteRepoErrLocked(readNumbers, r.Owner+"/"+r.Name, r.Err, now) || errsChanged
		read = append(read, numberPRs(r)...)
	}
	if full {
		for k := range d.repoErrs {
			if !asked[k] {
				delete(d.repoErrs, k)
				errsChanged = true
			}
		}
	}
	prs = append(prs, read...)
	d.ghStatus.LastQueries, d.ghStatus.LastCost = res.Queries+nums.Queries, res.Cost+nums.Cost
	d.mu.Unlock()
	if errsChanged {
		d.notify(api.TopicStatus)
	}
	if len(read) > 0 {
		d.log.Info("read PRs by number", "prs", len(read), "repos", len(nums.Repos))
	}

	// The state engine (internal/state) reads default branches from the
	// store, so it never matches a repo's default branch to a package. A
	// write that fails is tried again on the next tick.
	reposChanged := false
	if len(repos) > 0 {
		if err := d.st.UpsertRepos(ctx, repos...); err != nil {
			d.log.Warn("store default branches", "err", err)
		} else {
			reposChanged = true
			d.mu.Lock()
			for _, r := range repos {
				d.storedDefault[strings.ToLower(r.Name)] = r.Name + "\x00" + r.DefaultBranch
			}
			d.mu.Unlock()
		}
	}

	changed, err := d.writePRs(ctx, prs)
	changed = changed || reposChanged
	if err == nil {
		if h := d.gh.Health(); h.Err != nil {
			err = h.Err
		}
	}
	d.ran(api.SourceGitHub, now, err, partial)
	d.log.Debug("GitHub tick", "repos", len(res.Repos), "prs", len(prs), "queries", res.Queries, "cost", res.Cost)
	if changed {
		d.notify(api.TopicPRs)
	}
}

// writePRs stores PRs and reports whether any differs from what was
// written last. For an open PR whose mergeability GitHub hasn't computed
// yet (UNKNOWN), the stored value is kept as long as the head commit is
// the same: UNKNOWN means "ask again", not "not mergeable".
func (d *Daemon) writePRs(ctx context.Context, prs []github.PR) (bool, error) {
	return d.writePRsSeen(ctx, prs, false)
}

// writePRsSeen is writePRs. With createdSeen, a PR stored for the first
// time counts as first seen when GitHub created it rather than now (the
// plan backfill, plans.go).
func (d *Daemon) writePRsSeen(ctx context.Context, prs []github.PR, createdSeen bool) (bool, error) {
	if len(prs) == 0 {
		return false, nil
	}
	old := map[string]store.PR{}
	if stored, err := d.st.ListPRs(ctx, store.PRFilter{State: store.PROpen}); err == nil {
		for _, p := range stored {
			old[prKey(p.Repo, p.Number)] = p
		}
	}
	rows := make([]store.PR, 0, len(prs))
	seen := map[string]bool{}
	for _, pr := range prs {
		row := prRow(pr)
		if createdSeen {
			row.FirstSeenAt = pr.CreatedAt
		}
		k := prKey(row.Repo, row.Number)
		if seen[k] {
			continue
		}
		seen[k] = true
		if o, ok := old[k]; ok && row.State == store.PROpen && row.Mergeable == string(github.MergePending) &&
			o.Mergeable != string(github.MergePending) && o.Mergeable != "" && o.HeadRefOID == row.HeadRefOID {
			row.Mergeable = o.Mergeable
			if row.MergeStateStatus == string(github.MergeStateUnknown) {
				row.MergeStateStatus = o.MergeStateStatus
			}
		}
		rows = append(rows, row)
	}
	if err := d.st.UpsertPRs(ctx, rows...); err != nil {
		return false, err
	}
	changed := false
	d.mu.Lock()
	for _, row := range rows {
		k := prKey(row.Repo, row.Number)
		fp := fingerprint([]store.PR{row}, nil)
		if d.prFP[k] != fp {
			d.prFP[k] = fp
			changed = true
		}
	}
	d.mu.Unlock()
	return changed, nil
}

// prRow converts a PR from GitHub to its store row.
func prRow(pr github.PR) store.PR {
	return store.PR{
		Repo:              pr.Repo,
		Number:            pr.Number,
		HeadBranch:        pr.HeadRef,
		HeadRefOID:        pr.HeadOID,
		BaseRef:           pr.BaseRef,
		IsCrossRepository: pr.CrossRepository,
		Title:             pr.Title,
		Author:            pr.Author,
		State:             string(pr.State),
		Draft:             pr.Draft,
		Mergeable:         string(pr.Mergeable),
		MergeStateStatus:  string(pr.MergeState),
		ReviewDecision:    string(pr.ReviewDecision),
		ChecksState:       string(pr.Checks),
		PlanSlug:          pr.Plan.Slug,
		PlanPkg:           pr.Plan.ID,
		URL:               pr.URL,
		CreatedAt:         pr.CreatedAt,
		UpdatedAt:         pr.UpdatedAt,
		MergedAt:          pr.MergedAt,
		ClosedAt:          pr.ClosedAt,
	}
}

// prKey identifies a PR: lower-case nameWithOwner and number.
func prKey(repo string, number int) string {
	return strings.ToLower(repo) + "#" + fmt.Sprint(number)
}
