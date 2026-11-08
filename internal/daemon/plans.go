package daemon

import (
	"context"
	"strings"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/github"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
	"github.com/gravexa-llc/ai-manager/internal/worktrees"
)

// What the plan API (internal/planapi) reads from the daemon: the last
// worktree scan, the canonical GitHub names of repos, and a PR backfill
// for newly registered plans.

// PRSearcher reads every PR of some repos (*github.Client). BackfillPRs
// does nothing when the daemon's GitHub client isn't one.
type PRSearcher interface {
	RepoPRs(ctx context.Context, repos []string) github.RepoPRsResult
}

var _ PRSearcher = (*github.Client)(nil)

// LastScan returns the last worktree scan (nil before the first). The
// result is replaced, never changed, so it can be read while the daemon
// scans again; don't change it.
func (d *Daemon) LastScan() *worktrees.Result {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.scan
}

// CanonicalRepo returns the nameWithOwner GitHub answered with for a repo
// a remote names as nameWithOwner (they differ after a transfer), or the
// name as given when GitHub hasn't answered under another.
func (d *Daemon) CanonicalRepo(nameWithOwner string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c, ok := d.canonical[strings.ToLower(nameWithOwner)]; ok {
		return c
	}
	return nameWithOwner
}

// noteCanonicalLocked records the name GitHub answered with for a repo
// asked for as asked. d.mu is held.
func (d *Daemon) noteCanonicalLocked(asked, answered string) {
	if answered == "" {
		return
	}
	if strings.EqualFold(asked, answered) {
		delete(d.canonical, strings.ToLower(asked))
		return
	}
	d.canonical[strings.ToLower(asked)] = answered
}

// BackfillPRs reads every PR of the given GitHub repos ("owner/name") and
// stores them, so a newly registered plan's board gets PRs whose branches
// the GitHub poller never asked about: merged before aimd ran, or with no
// worktree left. A PR stored for the first time counts as first seen when
// GitHub created it, so an old branch never makes a low-confidence match
// (internal/state). It skips the search when the GitHub budget says stop,
// and runs in the caller's goroutine.
func (d *Daemon) BackfillPRs(ctx context.Context, repos []string) {
	s, ok := d.gh.(PRSearcher)
	if !ok || len(repos) == 0 {
		return
	}
	if b := d.gh.Budget(); b.Verdict == github.BudgetStop {
		d.log.Warn("plan PR backfill skipped: GitHub budget says stop", "repos", repos)
		return
	}
	start := time.Now()
	res := s.RepoPRs(ctx, repos)
	if ctx.Err() != nil {
		return
	}
	d.mu.Lock()
	for _, r := range res.Repos {
		if r.Err == nil {
			d.noteCanonicalLocked(r.Repo, r.Name)
		}
	}
	d.mu.Unlock()
	for _, r := range res.Repos {
		if r.Err != nil {
			d.log.Warn("plan PR backfill: repo unreadable", "repo", r.Repo, "err", r.Err)
		}
	}
	changed, err := d.writePRsSeen(ctx, res.PRs, true)
	if err != nil {
		d.log.Error("plan PR backfill: store", "err", err)
		return
	}
	d.log.Info("plan PR backfill", "repos", repos, "prs", len(res.PRs), "queries", res.Queries, "cost", res.Cost,
		"truncated", res.Truncated, "took", time.Since(start).Round(time.Millisecond))
	if changed {
		d.notify(api.TopicPRs)
	}
}
