// Package planapi is what aimd does for the plan API (the contract is
// docs/api.md, the HTTP handlers are in internal/web): it registers plan
// files, keeps them parsed as they change, runs the state engine
// (internal/state) and answers with boards, summaries and the mod's
// context, and writes the links and Mark done that the mod and the
// dashboard send.
//
// # State runs
//
// state.Run is one pass over the store. The Service runs it after each
// poll that changed something (Kick, from the daemon), at most once every
// Options.MinGap (5s), and at once after each write it makes itself, so a
// write's answer shows its effect. Reads are served from the last pass.
// When a pass changes what the boards say, Notify is called with
// api.TopicPlans.
//
// # Files
//
// Registration and detection read files a client names, so a path must be
// absolute, name a .md file of at most MaxPlanBytes (after following
// symlinks too), and lie inside a configured root or a repo the worktree
// scanner knows. Everything else is refused with an Error that says why.
// Registered files are watched (fsnotify on their folders) and re-parsed
// when they change; a re-parse that finds no packages at all is ignored,
// so a file caught half-written doesn't lose its packages and links.
//
// # Repos
//
// A plan is registered for GitHub repos (nameWithOwner, canonical when
// GitHub has answered under a new name): the request's repos if given,
// else the plan's "Repos:" line (plans.Repos), else the repo the plan file
// is in. A repo with no GitHub remote is named by its main checkout's
// path. After registering, the repos' PRs are read from GitHub once in the
// background (Options.Backfill), so PRs merged before aimd tracked their
// branches reach the board.
package planapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/config"
	"github.com/gravexa-llc/ai-manager/internal/state"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
	"github.com/gravexa-llc/ai-manager/internal/worktrees"
)

// DefaultMinGap is the least time between two state passes started by
// Kick.
const DefaultMinGap = 5 * time.Second

// resyncDelay is how long a changed plan file is left alone before it is
// re-read, so an editor's save (truncate, write, rename) settles first.
const resyncDelay = 500 * time.Millisecond

// RepoScanner reads git repos for paths the last scan doesn't cover:
// *worktrees.Scanner.
type RepoScanner interface {
	CommonDir(ctx context.Context, path string) (string, error)
	ScanRepos(ctx context.Context, gitDirs []string) []worktrees.Repo
}

var _ RepoScanner = (*worktrees.Scanner)(nil)

// Options configure a Service. Store is required.
type Options struct {
	Store *store.Store
	// Config gives the roots plan files may be read from and the state
	// thresholds.
	Config config.Config
	// Scanner reads a repo the last scan doesn't have; nil means
	// worktrees.New for Config.
	Scanner RepoScanner
	// LastScan returns the daemon's last worktree scan (nil before the
	// first). Its repos are the ones plan files may be read from besides
	// the roots, and they name the GitHub repos of worktrees. Nil means
	// none.
	LastScan func() *worktrees.Result
	// Canonical returns the nameWithOwner GitHub answers with for a repo
	// a remote names, which differs after a transfer. Nil keeps names as
	// the remotes write them.
	Canonical func(nameWithOwner string) string
	// Backfill reads every PR of the given GitHub repos into the store.
	// It is called in its own goroutine after a registration. Nil turns
	// the backfill off.
	Backfill func(ctx context.Context, repos []string)
	// Refresh asks for PRs to be read from GitHub by number soon, rather
	// than at the next poll (the daemon's RefreshPRs, which joins and
	// paces the reads), and returns how many it queued. It must not
	// block. Nil turns it off: RefreshPRs still answers, and nothing is
	// read until the polls.
	Refresh func(prs []store.PRKey) int
	// Notify is told api.TopicPlans when a state pass changes a board. It
	// must not block. Nil tells nobody.
	Notify func(topic string)
	// Logger receives the service's log; nil discards it.
	Logger *slog.Logger
	// Now is the clock; time.Now if nil.
	Now func() time.Time
	// MinGap is the least time between passes started by Kick;
	// DefaultMinGap if zero.
	MinGap time.Duration
}

// Service is the plan API's logic. Create it with New; call Run to start
// the state loop and the file watcher. Its methods are safe for
// concurrent use, and work before Run (without watching files).
type Service struct {
	st        *store.Store
	cfg       config.Config
	scanner   RepoScanner
	lastScan  func() *worktrees.Result
	canonical func(string) string
	backfill  func(ctx context.Context, repos []string)
	refresh   func(prs []store.PRKey) int
	notify    func(string)
	log       *slog.Logger
	now       func() time.Time
	minGap    time.Duration

	kick    chan struct{}
	resync  chan string // a registered file to re-read soon
	watcher *fileWatcher

	baseCtx context.Context // Run's context once it runs; background before
	bg      sync.WaitGroup  // backfills in flight

	runMu sync.Mutex // one state pass at a time

	mu      sync.Mutex
	lastRun time.Time
	boards  []api.Plan          // the last pass's boards, in registration order, without bands
	toMerge map[string][]string // the last pass's PRs for the leads to merge (needs.go)
	derived time.Time           // when the last pass ran
	fp      string              // fingerprint of boards and toMerge
}

// New returns a service for opts.
func New(opts Options) *Service {
	s := &Service{
		st:        opts.Store,
		cfg:       opts.Config,
		scanner:   opts.Scanner,
		lastScan:  opts.LastScan,
		canonical: opts.Canonical,
		backfill:  opts.Backfill,
		refresh:   opts.Refresh,
		notify:    opts.Notify,
		log:       opts.Logger,
		now:       opts.Now,
		minGap:    opts.MinGap,
		kick:      make(chan struct{}, 1),
		resync:    make(chan string, 16),
		baseCtx:   context.Background(),
	}
	if s.scanner == nil {
		s.scanner = worktrees.New(worktrees.OptionsFromConfig(s.cfg))
	}
	if s.lastScan == nil {
		s.lastScan = func() *worktrees.Result { return nil }
	}
	if s.canonical == nil {
		s.canonical = func(n string) string { return n }
	}
	if s.notify == nil {
		s.notify = func(string) {}
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.minGap <= 0 {
		s.minGap = DefaultMinGap
	}
	return s
}

// Kick asks for a state pass: at once if the last one was at least MinGap
// ago, otherwise when MinGap has passed. Kicks in between join it. It
// never blocks; it does nothing until Run runs.
func (s *Service) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Run re-reads every registered plan file (it may have changed while aimd
// was down), re-parses the stored copy of each one that hasn't (this aimd
// may read more of it than the one that stored it: reparseStored), watches
// them, runs a first state pass, and then runs passes on Kick and re-reads
// files as they change, until ctx is done. It waits for background
// backfills before it returns nil.
func (s *Service) Run(ctx context.Context) error {
	s.mu.Lock()
	s.baseCtx = ctx
	s.mu.Unlock()
	defer s.bg.Wait()

	w, err := newFileWatcher(ctx, s.log)
	if err != nil {
		s.log.Warn("plan files can't be watched; re-register a plan to pick up its changes", "err", err)
	} else {
		s.mu.Lock()
		s.watcher = w
		s.mu.Unlock()
	}
	// A plan whose file can't be read yet is tried again after each kick:
	// one outside the roots is readable only once the first worktree scan
	// knows its repo.
	var retry []int64
	if ps, err := s.st.ListPlans(ctx, store.PlanFilter{}); err != nil {
		s.log.Error("list plans", "err", err)
	} else {
		for _, p := range ps {
			s.watch(p.SourcePath)
			changed, err := s.resyncPlan(ctx, p)
			if err != nil && ctx.Err() == nil {
				s.log.Info("plan file unreadable for now; keeping the last copy", "plan", p.Slug, "path", p.SourcePath, "err", err)
				retry = append(retry, p.ID)
			}
			if !changed && ctx.Err() == nil {
				if err := s.reparseStored(ctx, p); err != nil {
					s.log.Warn("re-parse the stored plan", "plan", p.Slug, "err", err)
				}
			}
		}
	}
	retryResync := func() {
		var still []int64
		for _, id := range retry {
			p, err := s.st.GetPlan(ctx, id)
			if err != nil {
				continue // unregistered meanwhile
			}
			if _, err := s.resyncPlan(ctx, p); err != nil {
				still = append(still, id)
			} else {
				s.log.Info("plan file readable again", "plan", p.Slug, "path", p.SourcePath)
			}
		}
		retry = still
	}
	if err := s.runState(ctx); err != nil && ctx.Err() == nil {
		s.log.Error("state pass", "err", err)
	}

	var events <-chan string
	if w != nil {
		events = w.events
	}
	var gapTimer *time.Timer
	var gapC <-chan time.Time
	due := map[string]time.Time{} // PathKey → when to re-read
	var resyncTimer *time.Timer
	var resyncC <-chan time.Time
	defer func() {
		if gapTimer != nil {
			gapTimer.Stop()
		}
		if resyncTimer != nil {
			resyncTimer.Stop()
		}
	}()
	armResync := func() {
		if resyncTimer != nil {
			resyncTimer.Stop()
			resyncTimer, resyncC = nil, nil
		}
		var next time.Time
		for _, t := range due {
			if next.IsZero() || t.Before(next) {
				next = t
			}
		}
		if !next.IsZero() {
			resyncTimer = time.NewTimer(max(0, time.Until(next)))
			resyncC = resyncTimer.C
		}
	}
	queue := func(path string) {
		due[worktrees.PathKey(path)] = time.Now().Add(resyncDelay)
		armResync()
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-s.kick:
			if len(retry) > 0 {
				retryResync()
			}
			if gapC != nil {
				continue // a pass is already scheduled
			}
			s.mu.Lock()
			wait := time.Until(s.lastRun.Add(s.minGap))
			s.mu.Unlock()
			if wait <= 0 {
				s.pass(ctx)
				continue
			}
			gapTimer = time.NewTimer(wait)
			gapC = gapTimer.C
		case <-gapC:
			gapTimer, gapC = nil, nil
			s.pass(ctx)
		case p := <-events:
			queue(p)
		case p := <-s.resync:
			queue(p)
		case <-resyncC:
			resyncTimer, resyncC = nil, nil
			now := time.Now()
			changed := false
			for k, t := range due {
				if t.After(now) {
					continue
				}
				delete(due, k)
				if s.resyncPath(ctx, k) {
					changed = true
				}
			}
			armResync()
			if changed {
				s.pass(ctx)
			}
		}
	}
}

// pass runs a state pass and logs a failure.
func (s *Service) pass(ctx context.Context) {
	if err := s.runState(ctx); err != nil && ctx.Err() == nil {
		s.log.Error("state pass", "err", err)
	}
}

// ctx returns the context background work runs under: Run's, once it
// runs.
func (s *Service) ctx() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.baseCtx
}

// repoNames maps every scanned repo's main checkout to its GitHub name
// (state.Input.RepoNames).
func (s *Service) repoNames() map[string]string {
	out := map[string]string{}
	scan := s.lastScan()
	if scan == nil {
		return out
	}
	for i := range scan.Repos {
		repo := &scan.Repos[i]
		if owner, name, ok := repo.GitHubRepo(); ok && repo.Path != "" {
			out[repo.Path] = s.canonical(owner + "/" + name)
		}
	}
	return out
}

// runState runs one state pass and keeps its boards. It tells Notify when
// they changed.
func (s *Service) runState(ctx context.Context) error {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	// As the store keeps times (whole milliseconds, UTC), so a lifecycle
	// that changed in this pass reads the same in the next.
	now := s.now().Truncate(time.Millisecond).UTC()
	s.mu.Lock()
	s.lastRun = time.Now()
	s.mu.Unlock()

	states, err := state.Run(ctx, s.st, now, s.repoNames(), state.OptionsFromConfig(s.cfg))
	if err != nil {
		return err
	}
	ps, err := s.st.ListPlans(ctx, store.PlanFilter{})
	if err != nil {
		return fmt.Errorf("list plans: %w", err)
	}
	byID := map[int64]state.PlanState{}
	for _, st := range states {
		byID[st.PlanID] = st
	}
	boards := make([]api.Plan, 0, len(ps))
	for _, p := range ps {
		st, ok := byID[p.ID]
		if !ok {
			continue // registered after the pass loaded the store
		}
		pkgs, err := s.st.ListPackages(ctx, p.ID)
		if err != nil {
			return fmt.Errorf("list packages of %s: %w", p.Slug, err)
		}
		boards = append(boards, planJSON(p, st, pkgs, now))
	}
	toMerge, err := s.leadMerges(ctx, boards)
	if err != nil {
		return fmt.Errorf("PRs for the leads to merge: %w", err)
	}
	fp := fingerprint(boards, toMerge)
	s.mu.Lock()
	changed := fp != s.fp
	s.boards, s.toMerge, s.derived, s.fp = boards, toMerge, now, fp
	s.mu.Unlock()
	if changed {
		s.notify(api.TopicPlans)
	}
	return nil
}

// fingerprint hashes the boards without their derivation times, and the
// PRs to merge, so a pass that changes nothing gives the same value.
func fingerprint(boards []api.Plan, toMerge map[string][]string) string {
	h := sha256.New()
	enc := json.NewEncoder(h)
	for _, b := range boards {
		b.DerivedAt = time.Time{}
		_ = enc.Encode(b)
	}
	_ = enc.Encode(toMerge) // a map encodes in key order
	return fmt.Sprintf("%x", h.Sum(nil))
}

// boardsNow returns the last pass's boards, running a pass first if none
// has run. Their bands are empty: withBands fills them in.
func (s *Service) boardsNow(ctx context.Context) ([]api.Plan, error) {
	s.mu.Lock()
	ran := !s.derived.IsZero()
	boards := s.boards
	s.mu.Unlock()
	if ran {
		return boards, nil
	}
	if err := s.runState(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.boards, nil
}

// board returns one plan's board from the last pass, by slug (any case).
func (s *Service) board(ctx context.Context, slug string) (api.Plan, error) {
	boards, err := s.boardsNow(ctx)
	if err != nil {
		return api.Plan{}, err
	}
	for _, b := range boards {
		if strings.EqualFold(b.Slug, slug) {
			return b, nil
		}
	}
	return api.Plan{}, notFound("no plan %q is registered", slug)
}

// Plans returns every plan's board (GET /api/plans).
func (s *Service) Plans(ctx context.Context) (api.PlanList, error) {
	boards, err := s.boardsNow(ctx)
	if err != nil {
		return api.PlanList{}, err
	}
	boards, err = s.withBands(ctx, boards)
	if err != nil {
		return api.PlanList{}, err
	}
	return api.PlanList{Plans: boards}, nil
}

// Plan returns one plan's board (GET /api/plans/{slug}).
func (s *Service) Plan(ctx context.Context, slug string) (api.Plan, error) {
	b, err := s.board(ctx, slug)
	if err != nil {
		return api.Plan{}, err
	}
	sum, err := s.summaryNow(ctx, b, time.Time{})
	if err != nil {
		return api.Plan{}, err
	}
	b.Band = sum.Band
	return b, nil
}

// Summary returns one plan's summary (GET /api/plans/{slug}/summary).
// since, if not zero, limits Unlocked to packages unlocked after it.
func (s *Service) Summary(ctx context.Context, slug string, since time.Time) (api.PlanSummary, error) {
	b, err := s.board(ctx, slug)
	if err != nil {
		return api.PlanSummary{}, err
	}
	return s.summaryNow(ctx, b, since)
}
