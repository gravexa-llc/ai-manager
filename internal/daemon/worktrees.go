package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
	"github.com/gravexa-llc/ai-manager/internal/worktrees"
)

// worktreeLoop runs the full scans and the one-repo rescans, one at a
// time, so two scans of a repo never race.
func (d *Daemon) worktreeLoop(ctx context.Context) {
	// The first scan waits briefly for the first session poll, so the
	// sessions' cwds (repos outside the roots) are in it.
	select {
	case <-ctx.Done():
		return
	case <-d.firstSessions:
	case <-time.After(firstSessionsWait):
	}

	lastFull := time.Now()
	d.fullScan(ctx)
	full := time.NewTimer(d.cfg.Poll.Worktrees)
	defer full.Stop()

	var kickC <-chan time.Time // a kicked full scan, waiting out scanKickMinGap
	var kickTimer *time.Timer
	pending := map[string]time.Time{}  // gitDir → when to rescan it
	lastRepo := map[string]time.Time{} // gitDir → when it was last rescanned
	var repoTimer *time.Timer
	var repoC <-chan time.Time
	defer func() {
		if kickTimer != nil {
			kickTimer.Stop()
		}
		if repoTimer != nil {
			repoTimer.Stop()
		}
	}()

	var events <-chan string
	if d.repoWatcher != nil {
		events = d.repoWatcher.Events()
	}

	runFull := func() {
		lastFull = time.Now()
		d.fullScan(ctx)
		full.Reset(d.cfg.Poll.Worktrees)
		clear(pending) // the full scan covered them
		if kickTimer != nil {
			kickTimer.Stop()
			kickTimer, kickC = nil, nil
		}
	}
	armRepo := func() {
		if repoTimer != nil {
			repoTimer.Stop()
			repoTimer, repoC = nil, nil
		}
		if len(pending) == 0 {
			return
		}
		next := time.Time{}
		for _, due := range pending {
			if next.IsZero() || due.Before(next) {
				next = due
			}
		}
		repoTimer = time.NewTimer(max(0, time.Until(next)))
		repoC = repoTimer.C
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-full.C:
			runFull()
		case <-d.scanKick:
			d.mu.Lock()
			needed := d.needsScanLocked()
			d.mu.Unlock()
			if kickC != nil || !needed {
				continue // already scheduled, or a scan since covered it
			}
			wait := time.Until(lastFull.Add(scanKickMinGap))
			if wait <= 0 {
				runFull()
				continue
			}
			kickTimer = time.NewTimer(wait)
			kickC = kickTimer.C
		case <-kickC:
			kickTimer, kickC = nil, nil
			runFull()
		case path := <-events:
			gitDir := d.gitDirFor(path)
			if gitDir == "" {
				continue
			}
			if _, ok := pending[gitDir]; ok {
				continue // already due; later events join it
			}
			due := time.Now().Add(repoDebounce)
			if earliest := lastRepo[gitDir].Add(repoMinGap); earliest.After(due) {
				due = earliest
			}
			pending[gitDir] = due
			armRepo()
		case <-repoC:
			repoTimer, repoC = nil, nil
			now := time.Now()
			var due []string
			for g, t := range pending {
				if !t.After(now) {
					due = append(due, g)
				}
			}
			slices.Sort(due)
			for _, g := range due {
				delete(pending, g)
				lastRepo[g] = time.Now()
				d.rescanRepo(ctx, g)
			}
			armRepo()
		}
	}
}

// gitDirFor maps a watched path back to its repo's git dir.
func (d *Daemon) gitDirFor(path string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.watchMap[path]
}

// fullScan discovers and scans every repo: the roots, the live sessions'
// cwds, and every repo found outside the roots before (so a repo seen once
// through a session stays tracked while it exists). It writes every
// worktree, marks the ones that are gone, and updates the watches.
func (d *Daemon) fullScan(ctx context.Context) {
	defer d.firstScanOnce.Do(func() { close(d.firstScan) })
	cwds := d.liveCwds()
	d.mu.Lock()
	extra := slices.Clone(cwds)
	for _, p := range d.extraRepos {
		extra = append(extra, p)
	}
	d.mu.Unlock()

	start := time.Now()
	res := d.scanner.Scan(ctx, extra...)
	if ctx.Err() != nil {
		return
	}
	at := time.Now()

	// Discovery problems: a cwd that isn't in a repo is expected, and a bare
	// repo found by the walk is skipped on purpose. Anything else counts.
	partial := 0
	var discoveryErr []error
	for _, err := range res.Errors {
		if errors.Is(err, worktrees.ErrNotRepo) || errors.Is(err, worktrees.ErrBareSkipped) || errors.Is(err, fs.ErrNotExist) {
			continue
		}
		partial++
		discoveryErr = append(discoveryErr, err)
	}

	var rows []store.Worktree
	for i := range res.Repos {
		repo := &res.Repos[i]
		if repo.Err != nil {
			partial++
		}
		rows = append(rows, worktreeRows(repo, at)...)
	}
	var errs []error
	if len(rows) > 0 {
		errs = append(errs, d.st.UpsertWorktrees(ctx, rows...))
	}
	present := map[string]bool{}
	for i := range res.Repos {
		repo := &res.Repos[i]
		present[worktrees.PathKey(repo.Path)] = true
		if len(repo.Worktrees) == 0 {
			continue // listing failed: don't mark its worktrees gone
		}
		if _, err := d.st.MarkMissingWorktreesRemoved(ctx, repo.Path, worktreePaths(repo), at); err != nil {
			errs = append(errs, err)
		}
	}
	// Repos the scan no longer found at all (deleted, or moved out of the
	// roots): their worktrees are gone too.
	stored, err := d.st.ListWorktrees(ctx, store.WorktreeFilter{})
	errs = append(errs, err)
	gone := map[string]string{}
	for _, w := range stored {
		if k := worktrees.PathKey(w.MainRepo); !present[k] {
			gone[k] = w.MainRepo
		}
	}
	for _, mainRepo := range gone {
		n, err := d.st.MarkMissingWorktreesRemoved(ctx, mainRepo, nil, at)
		errs = append(errs, err)
		if n > 0 {
			d.log.Info("repo no longer found; its worktrees are marked removed", "repo", mainRepo, "worktrees", n)
		}
	}
	if ctx.Err() != nil {
		return
	}
	err = errors.Join(errs...)
	if err == nil && len(res.Repos) == 0 && len(discoveryErr) > 0 {
		err = fmt.Errorf("no repos found: %w", errors.Join(discoveryErr...))
	}
	d.ran(api.SourceWorktrees, at, err, partial)

	d.mu.Lock()
	first := d.scan == nil
	d.scan = res
	clear(d.scanExtras)
	for _, c := range cwds {
		d.scanExtras[worktrees.PathKey(c)] = true
	}
	clear(d.extraRepos)
	for i := range res.Repos {
		repo := &res.Repos[i]
		if len(repo.Worktrees) > 0 && !d.underRoots(repo.Path) {
			d.extraRepos[worktrees.PathKey(repo.Path)] = repo.Path
		}
	}
	seen := map[string]bool{}
	changed := len(gone) > 0
	for i := range res.Repos {
		repo := &res.Repos[i]
		k := worktrees.PathKey(repo.GitDir)
		seen[k] = true
		if d.noteRepoLocked(repo, at, first) {
			changed = true
		}
	}
	for k := range d.repoFP {
		if !seen[k] {
			delete(d.repoFP, k)
			delete(d.repoChangedAt, k)
			changed = true
		}
	}
	d.mu.Unlock()

	d.updateWatches()
	d.log.Debug("worktree scan", "repos", len(res.Repos), "worktrees", len(rows), "partial_errors", partial, "took", at.Sub(start).Round(time.Millisecond))
	if changed {
		d.notify(api.TopicWorktrees)
	}
}

// rescanRepo rescans one repo after its WatchPaths changed.
func (d *Daemon) rescanRepo(ctx context.Context, gitDir string) {
	repos := d.scanner.ScanRepos(ctx, []string{gitDir})
	if ctx.Err() != nil || len(repos) != 1 {
		return
	}
	repo := repos[0]
	at := time.Now()
	if len(repo.Worktrees) == 0 {
		// The listing failed: keep what we had. A repo that is gone shows
		// up as such at the next full scan.
		d.log.Debug("repo rescan failed", "git_dir", gitDir, "err", repo.Err)
		return
	}
	rows := worktreeRows(&repo, at)
	err := d.st.UpsertWorktrees(ctx, rows...)
	if err == nil {
		_, err = d.st.MarkMissingWorktreesRemoved(ctx, repo.Path, worktreePaths(&repo), at)
	}
	if err != nil {
		if ctx.Err() == nil {
			d.ran(api.SourceWorktrees, at, err, 0)
		}
		return
	}

	d.mu.Lock()
	// Copy on write: Lookup may be reading the old result.
	next := &worktrees.Result{}
	replaced := false
	if d.scan != nil {
		next.Errors = d.scan.Errors
		next.Repos = slices.Clone(d.scan.Repos)
		for i := range next.Repos {
			if worktrees.SamePath(next.Repos[i].GitDir, repo.GitDir) {
				next.Repos[i] = repo
				replaced = true
			}
		}
	}
	if !replaced {
		next.Repos = append(next.Repos, repo)
	}
	d.scan = next
	changed := d.noteRepoLocked(&repo, at, false)
	d.mu.Unlock()

	d.updateWatches()
	d.log.Debug("repo rescan", "repo", repo.Path, "worktrees", len(rows), "changed", changed)
	if changed {
		d.notify(api.TopicWorktrees)
	}
}

// noteRepoLocked records a repo's fingerprint and reports whether it
// changed. A change marks the repo active for the GitHub poller, except on
// the first scan after start, which has nothing to compare with. d.mu is
// held.
func (d *Daemon) noteRepoLocked(repo *worktrees.Repo, at time.Time, first bool) bool {
	k := worktrees.PathKey(repo.GitDir)
	fp := fingerprint(worktreeRows(repo, time.Time{}), nil)
	old, had := d.repoFP[k]
	d.repoFP[k] = fp
	if had && old == fp {
		return false
	}
	if !first {
		d.repoChangedAt[k] = at
	}
	return true
}

// updateWatches points the repo watcher at every scanned repo's
// WatchPaths.
func (d *Daemon) updateWatches() {
	if d.repoWatcher == nil {
		return
	}
	d.mu.Lock()
	m := map[string]string{}
	var paths []string
	if d.scan != nil {
		for i := range d.scan.Repos {
			repo := &d.scan.Repos[i]
			if len(repo.Worktrees) == 0 {
				continue
			}
			for _, p := range repo.WatchPaths() {
				if _, dup := m[p]; !dup {
					m[p] = repo.GitDir
					paths = append(paths, p)
				}
			}
		}
	}
	d.watchMap = m
	d.mu.Unlock()
	d.repoWatcher.Set(paths)
}

// underRoots reports whether p is inside one of the configured roots.
func (d *Daemon) underRoots(p string) bool {
	for _, r := range d.cfg.Roots {
		if under(p, r) {
			return true
		}
	}
	return false
}

// worktreePaths returns the paths of a repo's worktrees.
func worktreePaths(repo *worktrees.Repo) []string {
	out := make([]string, 0, len(repo.Worktrees))
	for i := range repo.Worktrees {
		out = append(out, repo.Worktrees[i].Path)
	}
	return out
}

// unknownCount is stored in the dirty and untracked counts when git status
// couldn't be read, so unknown never reads as clean. The API shows it as
// null.
const unknownCount = -1

// worktreeRows converts a scanned repo to store rows. A repo whose listing
// failed has none.
func worktreeRows(repo *worktrees.Repo, at time.Time) []store.Worktree {
	rows := make([]store.Worktree, 0, len(repo.Worktrees))
	for i := range repo.Worktrees {
		w := &repo.Worktrees[i]
		row := store.Worktree{
			Path:           w.Path,
			MainRepo:       repo.Path,
			Branch:         w.Branch,
			Detached:       w.Detached,
			HeadSHA:        w.Head,
			DirtyCount:     w.Uncommitted,
			UntrackedCount: w.Untracked,
			UpstreamState:  string(w.Upstream),
			UnpushedCount:  w.Unpushed,
			LastCommitAt:   w.LastCommitAt,
			Locked:         w.Locked,
			LockedReason:   w.LockReason,
			PrunableReason: w.PrunableReason,
			AgentID:        w.AgentID,
			LastSeen:       at,
		}
		if !w.StatusRead {
			row.DirtyCount, row.UntrackedCount = unknownCount, unknownCount
		}
		if w.Lock != nil {
			row.LockAgentID = w.Lock.AgentID
			row.LockPID = w.Lock.PID
		}
		rows = append(rows, row)
	}
	return rows
}

// fingerprint hashes v's JSON after scrub has blanked the fields that
// change on every write (seen times), so equal content gives equal
// fingerprints.
func fingerprint[T any](rows []T, scrub func(*T)) string {
	h := sha256.New()
	enc := json.NewEncoder(h)
	for _, r := range rows {
		if scrub != nil {
			scrub(&r)
		}
		_ = enc.Encode(r)
	}
	return hex.EncodeToString(h.Sum(nil))
}
