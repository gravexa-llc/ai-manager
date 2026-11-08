package web

// The removing side of the Worktrees & cleanup page (app_worktrees.go is
// the page). Cleanup re-checks each worktree the page
// asks to remove against `aim prune`'s rules at that moment, from fresh git,
// GitHub and session sources, and removes only those that are still safe,
// with the code `aim prune --apply` uses (prune.Checker.RemovePicked and
// PruneListed). It runs one removal at a time per repo, refuses a
// confirmation sent twice, and keeps the removal log.

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/prune"
	"github.com/gravexa-llc/ai-manager/internal/sessions"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/worktrees"
)

const (
	// removalLogKeep is how long the removal log keeps an entry.
	removalLogKeep = 30 * 24 * time.Hour
	// removalLogShown is how many entries the page shows at most.
	removalLogShown = 200
	// cleanupTimeout bounds one removal or prune run, re-check included.
	cleanupTimeout = 3 * time.Minute
	// runMemory is how long a used confirmation is remembered.
	runMemory = time.Hour
	// purgeEvery is how often reading the log also purges it.
	purgeEvery = time.Hour
)

// ErrConfirmationUsed is what Remove and Prune return for a confirmation
// that was sent already (a double submit): nothing more is done.
var ErrConfirmationUsed = errors.New("this confirmation was sent already")

// RemovalLog keeps the removal log; *store.Store implements it.
type RemovalLog interface {
	AddRemovals(ctx context.Context, entries ...store.Removal) error
	ListRemovals(ctx context.Context, since time.Time, limit int) ([]store.Removal, error)
	PurgeRemovals(ctx context.Context, before time.Time) (int64, error)
}

var _ RemovalLog = (*store.Store)(nil)

// CleanupOptions configure a Cleanup.
type CleanupOptions struct {
	// Checker has what `aim prune --apply` checks with: the Scanner, a
	// GitHub client and a session lister that answer fresh (never the
	// store's copies), Procs and Rebuildable.
	Checker *prune.Checker
	// Store, if set, adds the live sessions aimd stored (with what the mod
	// reported) to the session lister's: a worktree either one finds in
	// use is in use. Status says whether the stored ones can be trusted;
	// while they can't, nothing is safe.
	Store  Store
	Status StatusSource
	// Log keeps the removal log.
	Log RemovalLog
	// OnChange is called after a run that removed or pruned something.
	OnChange func()
	// Now is the clock; time.Now if nil.
	Now    func() time.Time
	Logger *slog.Logger
}

// Cleanup removes worktrees and clears prunable records for the dashboard.
type Cleanup struct {
	checker  prune.Checker
	log      RemovalLog
	onChange func()
	now      func() time.Time
	logger   *slog.Logger

	mu     sync.Mutex
	repos  map[string]chan struct{} // worktrees.PathKey(main checkout) → its lock
	runs   map[string]time.Time     // confirmations used → when
	purged time.Time
}

// NewCleanup returns a Cleanup.
func NewCleanup(o CleanupOptions) *Cleanup {
	c := &Cleanup{
		checker:  *o.Checker,
		log:      o.Log,
		onChange: o.OnChange,
		now:      o.Now,
		logger:   o.Logger,
		repos:    map[string]chan struct{}{},
		runs:     map[string]time.Time{},
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.logger == nil {
		c.logger = slog.New(slog.DiscardHandler)
	}
	if o.Store != nil {
		stored := storeSessions{st: o.Store, status: o.Status}
		if c.checker.Sessions == nil {
			c.checker.Sessions = stored
		} else {
			c.checker.Sessions = bothSessions{c.checker.Sessions, stored}
		}
	}
	return c
}

// CleanupPick is a worktree to remove or a record to prune. Repo (its main
// checkout) and Path are as aimd lists them, never taken from a request;
// Head and Branch are what the page showed, and must still hold.
type CleanupPick struct {
	Repo string
	prune.Pick
}

// Remove re-checks the picks and removes those that are still safe, one
// repo at a time under that repo's lock, and logs what became of each. run
// names the confirmation they came from: one sent already is refused with
// ErrConfirmationUsed. A removal isn't cut short when the request ends:
// git is never stopped halfway through one.
func (c *Cleanup) Remove(ctx context.Context, run string, picks []CleanupPick) ([]store.Removal, error) {
	if err := c.claim(run); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	var out []store.Removal
	changed := false
	for _, g := range groupByRepo(picks) {
		var res []prune.Picked
		unlock, err := c.lock(ctx, g.repo)
		if err == nil {
			var ps []prune.Pick
			for _, p := range g.picks {
				ps = append(ps, p.Pick)
			}
			res, err = c.checker.RemovePicked(ctx, prune.Options{Repo: g.repo}, ps)
			unlock()
		}
		at := c.now()
		for i, p := range g.picks {
			e := store.Removal{At: at, Repo: g.repo, Path: p.Path, Branch: p.Branch, Head: p.Head}
			switch {
			case err != nil:
				e.Action, e.Detail = store.RemovalSkipped, "the re-check failed, so nothing was done: "+err.Error()
			case res[i].Skipped != "":
				e.Action, e.Detail = store.RemovalSkipped, res[i].Skipped
			case res[i].Removed:
				e.Action, e.BranchDeleted = store.RemovalRemoved, res[i].BranchDeleted
				if res[i].Err != nil {
					e.Detail = res[i].Err.Error()
				}
				changed = true
			default:
				e.Action, e.Detail = store.RemovalFailed, errText(res[i].Err)
			}
			out = append(out, e)
		}
	}
	c.record(ctx, out, changed)
	return out, nil
}

// Prune clears prunable records with `git worktree prune`, repo by repo
// under the repo's lock, as `aim prune --apply` does: in a repo only when
// every record prune would clear is one of records (the ones the page
// listed) and none is locked. run is as for Remove.
func (c *Cleanup) Prune(ctx context.Context, run string, records []CleanupPick) ([]store.Removal, error) {
	if err := c.claim(run); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	var out []store.Removal
	changed := false
	for _, g := range groupByRepo(records) {
		var res prune.PruneRun
		unlock, err := c.lock(ctx, g.repo)
		if err == nil {
			var paths []string
			for _, p := range g.picks {
				paths = append(paths, p.Path)
			}
			res, err = c.checker.PruneListed(ctx, prune.Options{Repo: g.repo}, paths)
			unlock()
		}
		at := c.now()
		repo := store.Removal{At: at, Repo: g.repo, Path: g.repo}
		switch {
		case err != nil:
			repo.Action, repo.Detail = store.RemovalSkipped, "git worktree prune not run; the re-check failed: "+err.Error()
			out = append(out, repo)
		case res.Err != nil:
			repo.Action, repo.Detail = store.RemovalFailed, "git worktree prune: "+res.Err.Error()
			out = append(out, repo)
		case res.Skipped != "":
			repo.Action, repo.Detail = store.RemovalSkipped, "git worktree prune not run: "+res.Skipped
			out = append(out, repo)
		default:
			for _, e := range res.Pruned {
				out = append(out, store.Removal{At: at, Repo: g.repo, Path: e.Path, Action: store.RemovalPruned, Detail: e.Reason})
				changed = true
			}
		}
	}
	c.record(ctx, out, changed)
	return out, nil
}

// Log returns the removal log of the last 30 days, newest first. It purges
// older entries, at most once an hour.
func (c *Cleanup) Log(ctx context.Context) ([]store.Removal, error) {
	now := c.now()
	c.mu.Lock()
	due := now.Sub(c.purged) >= purgeEvery
	if due {
		c.purged = now
	}
	c.mu.Unlock()
	if due {
		c.purge(ctx, now)
	}
	return c.log.ListRemovals(ctx, now.Add(-removalLogKeep), removalLogShown)
}

// record logs a run's entries, purges old ones, and tells the pages.
func (c *Cleanup) record(ctx context.Context, entries []store.Removal, changed bool) {
	for _, e := range entries {
		c.logger.Info("cleanup", "action", e.Action, "path", e.Path, "branch", e.Branch, "branch_deleted", e.BranchDeleted, "detail", e.Detail)
	}
	if err := c.log.AddRemovals(ctx, entries...); err != nil {
		c.logger.Error("removal log", "err", err)
	}
	c.purge(ctx, c.now())
	if changed && c.onChange != nil {
		c.onChange()
	}
}

func (c *Cleanup) purge(ctx context.Context, now time.Time) {
	if _, err := c.log.PurgeRemovals(ctx, now.Add(-removalLogKeep)); err != nil {
		c.logger.Error("removal log purge", "err", err)
	}
}

// claim marks a confirmation used, or returns ErrConfirmationUsed when it
// was already (or is empty).
func (c *Cleanup) claim(run string) error {
	if run == "" || len(run) > 64 {
		return ErrConfirmationUsed
	}
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, at := range c.runs {
		if now.Sub(at) > runMemory {
			delete(c.runs, k)
		}
	}
	if _, used := c.runs[run]; used {
		return ErrConfirmationUsed
	}
	c.runs[run] = now
	return nil
}

// lock takes a repo's lock, waiting for a run in the same repo to finish.
// It returns the unlock.
func (c *Cleanup) lock(ctx context.Context, repo string) (func(), error) {
	key := worktrees.PathKey(repo)
	c.mu.Lock()
	ch := c.repos[key]
	if ch == nil {
		ch = make(chan struct{}, 1)
		c.repos[key] = ch
	}
	c.mu.Unlock()
	select {
	case ch <- struct{}{}:
		return func() { <-ch }, nil
	case <-ctx.Done():
		return nil, errors.New("another removal in this repo took too long: " + ctx.Err().Error())
	}
}

// repoPicks are the picks of one repo.
type repoPicks struct {
	repo  string
	picks []CleanupPick
}

// groupByRepo groups picks by repo, in the order the repos first appear.
func groupByRepo(picks []CleanupPick) []repoPicks {
	var out []repoPicks
	at := map[string]int{}
	for _, p := range picks {
		k := worktrees.PathKey(p.Repo)
		i, ok := at[k]
		if !ok {
			i = len(out)
			at[k] = i
			out = append(out, repoPicks{repo: p.Repo})
		}
		out[i].picks = append(out[i].picks, p)
	}
	return out
}

func errText(err error) string {
	if err == nil {
		return "git didn't say why"
	}
	return err.Error()
}

// bothSessions lists the sessions of two listers together. Either one
// failing fails it: rule 2 is then unknown, and nothing is safe.
type bothSessions struct{ a, b prune.SessionLister }

// List implements prune.SessionLister.
func (s bothSessions) List(ctx context.Context) (sessions.Snapshot, error) {
	a, err := s.a.List(ctx)
	if err != nil {
		return sessions.Snapshot{}, err
	}
	b, err := s.b.List(ctx)
	if err != nil {
		return sessions.Snapshot{}, err
	}
	a.Sessions = append(a.Sessions, b.Sessions...)
	a.Warnings = append(a.Warnings, b.Warnings...)
	return a, nil
}
