// Package daemon is aimd's scheduler. It runs the session poller
// (internal/sessions), the worktree scanner (internal/worktrees) and the
// GitHub client (internal/github) on the schedules below, and writes what
// they find to the store (internal/store).
//
// # Schedules
//
//   - Sessions: `claude agents --json` every poll.sessions (10s) while at
//     least one dashboard is connected (SetWatchers), every
//     poll.sessions_unwatched (30s) otherwise, and every
//     poll.sessions_mod_live (60s) once the mod reports live
//     (SetModLive). File changes in Claude Code's session registry and
//     job store kick an immediate, debounced poll. Each poll also takes one process
//     snapshot, which says what terminal app each session runs in
//     (Terminals).
//   - Worktrees: a full scan every poll.worktrees (2m), with the live
//     sessions' cwds and every repo seen that way before passed in, plus a
//     rescan of just one repo when its WatchPaths change (fsnotify).
//   - GitHub: one tick every poll.github_active (60s). Each tick sends one
//     BranchPRs call for the active repos, and every Nth tick (N =
//     poll.github_idle / poll.github_active, 5 by default) for the idle ones
//     too. The open-PR search, and right after it the review-request
//     search, run every poll.github_active while a dashboard is connected and
//     every poll.github_idle otherwise. The
//     budget is obeyed: "idle-only" runs only the idle ticks, "stop" runs
//     nothing until it lifts.
//   - Retention: PurgeEvents once a day.
//
// # Load
//
// On a loaded machine the external commands slow down: `claude
// agents` gets 20s (sessions.DefaultTimeout) and ps 10s
// (term.DefaultPSTimeout). Each source runs one run at a time, and a run
// that takes longer than its interval skips the ticks that fell while it
// ran instead of queuing them. A single timeout doesn't mark a source
// failing (see ran), and a failed `claude agents` call or ps snapshot
// leaves the last good sessions and terminal apps in place.
//
// # Change events
//
// After a write, the daemon calls Options.Notify with a topic (see
// internal/web/api) when what it wrote differs from what it wrote last
// time, so the dashboard refreshes only when something changed.
package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/config"
	"github.com/gravexa-llc/ai-manager/internal/github"
	"github.com/gravexa-llc/ai-manager/internal/sessions"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/term"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
	"github.com/gravexa-llc/ai-manager/internal/worktrees"
)

// Store is the part of *store.Store the daemon writes and reads.
type Store interface {
	UpsertSessions(ctx context.Context, sessions ...store.Session) error
	ListSessions(ctx context.Context, f store.SessionFilter) ([]store.Session, error)
	EndSession(ctx context.Context, key string, at time.Time) error
	UpsertWorktrees(ctx context.Context, wts ...store.Worktree) error
	ListWorktrees(ctx context.Context, f store.WorktreeFilter) ([]store.Worktree, error)
	MarkMissingWorktreesRemoved(ctx context.Context, mainRepo string, present []string, at time.Time) (int64, error)
	UpsertPRs(ctx context.Context, prs ...store.PR) error
	ListPRs(ctx context.Context, f store.PRFilter) ([]store.PR, error)
	SetReviewRequests(ctx context.Context, owners []string, found []store.PRKey, at time.Time) (int64, error)
	UpsertRepos(ctx context.Context, repos ...store.Repo) error
	PurgeEvents(ctx context.Context, now time.Time, retentionDays int) (int64, error)
}

var _ Store = (*store.Store)(nil)

// Scanner is the part of *worktrees.Scanner the daemon uses.
type Scanner interface {
	Scan(ctx context.Context, extra ...string) *worktrees.Result
	ScanRepos(ctx context.Context, gitDirs []string) []worktrees.Repo
}

var _ Scanner = (*worktrees.Scanner)(nil)

// GitHub is the part of *github.Client the daemon uses.
type GitHub interface {
	BranchPRs(ctx context.Context, repos []github.RepoBranches) github.BranchPRsResult
	OpenPRs(ctx context.Context, owners []string) github.OpenPRsResult
	ReviewRequests(ctx context.Context, owners []string) github.OpenPRsResult
	Budget() github.Budget
	Health() github.Health
}

var _ GitHub = (*github.Client)(nil)

// RepoWatcher reports changes to the paths it watches. Set replaces the
// watched set; Events delivers a watched path (as given to Set) whose
// contents changed. NewFSWatcher is the real one.
type RepoWatcher interface {
	Set(paths []string)
	Events() <-chan string
}

// Options configure a Daemon. Only Config and Store are required.
type Options struct {
	Config config.Config
	Store  Store

	// Lister lists sessions; its zero value runs the real claude.
	Lister sessions.Lister
	// Scanner scans worktrees; nil means worktrees.New for Config's roots.
	Scanner Scanner
	// GitHub reads PRs; nil means github.New(Config, ...).
	GitHub GitHub
	// WatchSessions calls kick whenever Claude Code's session registry or
	// job store changes, until ctx is done. Nil means sessions.Watch on
	// sessions.WatchDirs().
	WatchSessions func(ctx context.Context, kick func()) error
	// RepoWatcher watches repos' WatchPaths. Nil means NewFSWatcher, started
	// by Run; if that fails, repos are rescanned on the interval only.
	RepoWatcher RepoWatcher

	// Notify is called with a topic (api.Topic*) after a write that changed
	// something. It must not block. Nil means nobody is told.
	Notify func(topic string)
	// RefineSessions, if set, sees each poll's session rows before they
	// are written and returns the rows to write: the event receiver
	// (internal/live) merges what the mod reported.
	RefineSessions func(ctx context.Context, rows []store.Session, at time.Time) []store.Session
	// Procs takes the process snapshot each session poll reads the
	// sessions' terminal apps from (Terminals; internal/term). Nil means
	// term.PS{}.Procs, the real ps.
	Procs func(ctx context.Context) (term.Procs, error)
	// Logger receives the daemon's log. Nil discards it.
	Logger *slog.Logger

	// TempDirs are the directories a session's cwd marks it headless in.
	// Nil means $TMPDIR, as given and with symlinks resolved.
	TempDirs []string
	// Host is stored on every session; empty means os.Hostname().
	Host string
	// Version is reported by Status.
	Version string
}

// Timings that aren't in config.toml. Tests in this package change them.
const (
	// activeWindow: a repo whose worktrees changed (or got a commit) this
	// recently is active for the GitHub poller.
	activeWindow = 30 * time.Minute
	// repoDebounce and repoMinGap pace the rescan of one repo after its
	// WatchPaths change: wait repoDebounce for more events, and never scan
	// the same repo more often than repoMinGap.
	repoDebounce = time.Second
	repoMinGap   = 5 * time.Second
	// scanKickMinGap is the least time between full scans when a new
	// session cwd asks for one before the interval.
	scanKickMinGap = 15 * time.Second
	// firstSessionsWait is how long the first worktree scan waits for the
	// first session poll, so it can include the sessions' cwds.
	firstSessionsWait = 5 * time.Second
	// retentionFirst and retentionEvery schedule PurgeEvents.
	retentionFirst = time.Minute
	retentionEvery = 24 * time.Hour
	// searchSlack lets a search run up to this fraction of its cadence
	// early, so a 60s search on a 60s tick isn't pushed to every other tick.
	searchSlack = 0.05
	// failAfterTimeouts and failAfterIntervals say when a timeout marks a
	// source failing (see ran): once it is the failAfterTimeouts-th failed
	// run in a row, or once the source's last success is more than
	// failAfterIntervals of its intervals old.
	failAfterTimeouts  = 2
	failAfterIntervals = 3
)

// Daemon runs the pollers. Create it with New and start it with Run.
type Daemon struct {
	cfg           config.Config
	st            Store
	scanner       Scanner
	gh            GitHub
	watchSessions func(ctx context.Context, kick func()) error
	repoWatcher   RepoWatcher
	notify        func(string)
	refine        func(context.Context, []store.Session, time.Time) []store.Session
	procs         func(context.Context) (term.Procs, error)
	log           *slog.Logger
	tempDirs      []string
	host          string
	version       string
	poller        *sessions.Poller
	startedAt     time.Time

	runCtx context.Context // set by Run before any goroutine starts

	scanKick      chan struct{} // a full worktree scan, soon
	searchKick    chan struct{} // the open-PR search, if it is due
	firstSessions chan struct{} // closed after the first session poll
	firstScan     chan struct{} // closed after the first worktree scan
	firstSessOnce sync.Once
	firstScanOnce sync.Once

	mu       sync.Mutex
	watchers int
	modLive  bool
	sources  map[string]*api.Source
	streaks  map[string]*streak // source name → its current run of failures

	// Sessions.
	live      []liveSession // the latest poll's live sessions
	firstSeen map[sessions.Key]time.Time
	sessFP    string
	terms     map[string]term.App // session key → terminal app, from the latest poll; replaced, never changed
	procsErr  string              // the last process snapshot's error, if it failed

	// Worktrees.
	scan          *worktrees.Result    // every repo as last scanned
	scanExtras    map[string]bool      // PathKey of the cwds the last full scan was given
	extraRepos    map[string]string    // PathKey → main path of repos found outside the roots
	repoFP        map[string]string    // PathKey(gitDir) → fingerprint of its rows
	repoChangedAt map[string]time.Time // PathKey(gitDir) → last time its rows changed
	watchMap      map[string]string    // watched path → gitDir

	// GitHub.
	ghTick        int
	ghStatus      api.GitHubStatus
	lastSearch    time.Time
	searchOpen    map[string]bool   // PR keys the last good search found open
	defaultBranch map[string]string // lower-case owner/name → default branch
	storedDefault map[string]string // lower-case nameWithOwner → name + "\x00" + default branch, as last stored
	prFP          map[string]string // PR key → fingerprint
	repoErrs      map[string]string // GitHub repo → last error logged
	canonical     map[string]string // lower-case owner/name a remote names → the name GitHub answered with (plans.go)
}

// streak is a source's current run of failed runs, which a success ends.
type streak struct {
	fails int       // failed runs in a row
	since time.Time // when the first of them ran
}

// liveSession is one active session from the latest poll, with what the
// daemon derived for it.
type liveSession struct {
	s        sessions.Session
	headless bool
	repo     string // main checkout path, if the cwd is in a scanned repo
}

// New returns a daemon for opts. Nothing runs until Run.
func New(opts Options) *Daemon {
	d := &Daemon{
		cfg:           opts.Config,
		st:            opts.Store,
		scanner:       opts.Scanner,
		gh:            opts.GitHub,
		watchSessions: opts.WatchSessions,
		repoWatcher:   opts.RepoWatcher,
		notify:        opts.Notify,
		refine:        opts.RefineSessions,
		procs:         opts.Procs,
		log:           opts.Logger,
		tempDirs:      opts.TempDirs,
		host:          opts.Host,
		version:       opts.Version,
		startedAt:     time.Now(),

		scanKick:      make(chan struct{}, 1),
		searchKick:    make(chan struct{}, 1),
		firstSessions: make(chan struct{}),
		firstScan:     make(chan struct{}),

		sources:       map[string]*api.Source{},
		streaks:       map[string]*streak{},
		firstSeen:     map[sessions.Key]time.Time{},
		scanExtras:    map[string]bool{},
		extraRepos:    map[string]string{},
		repoFP:        map[string]string{},
		repoChangedAt: map[string]time.Time{},
		watchMap:      map[string]string{},
		searchOpen:    map[string]bool{},
		defaultBranch: map[string]string{},
		storedDefault: map[string]string{},
		prFP:          map[string]string{},
		repoErrs:      map[string]string{},
		canonical:     map[string]string{},
	}
	if d.scanner == nil {
		d.scanner = worktrees.New(worktrees.OptionsFromConfig(d.cfg))
	}
	if d.gh == nil {
		d.gh = github.New(d.cfg, github.Options{})
	}
	if d.watchSessions == nil {
		d.watchSessions = watchClaudeDirs
	}
	if d.notify == nil {
		d.notify = func(string) {}
	}
	if d.procs == nil {
		d.procs = term.PS{}.Procs
	}
	if d.log == nil {
		d.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if d.tempDirs == nil {
		d.tempDirs = defaultTempDirs()
	}
	if d.host == "" {
		d.host, _ = os.Hostname()
	}
	for _, name := range []string{api.SourceSessions, api.SourceWorktrees, api.SourceGitHub, api.SourceGitHubSearch, api.SourceRetention} {
		d.sources[name] = &api.Source{Name: name}
		d.streaks[name] = &streak{}
	}
	d.ghStatus.IdleEvery = d.idleEvery()
	d.poller = sessions.NewPoller(sessions.Options{
		Lister:   opts.Lister,
		Interval: d.sessionInterval(),
		OnResult: d.onSessions,
	})
	return d
}

// watchClaudeDirs is the default WatchSessions: internal/sessions' watcher
// on Claude Code's session registry and job store.
func watchClaudeDirs(ctx context.Context, kick func()) error {
	dirs, err := sessions.WatchDirs()
	if err != nil {
		return err
	}
	return sessions.Watch(ctx, dirs, kick)
}

// defaultTempDirs returns $TMPDIR as given and with symlinks resolved
// (/var is /private/var on macOS; session cwds are resolved).
func defaultTempDirs() []string {
	t := filepath.Clean(os.TempDir())
	dirs := []string{t}
	if r, err := filepath.EvalSymlinks(t); err == nil && r != t {
		dirs = append(dirs, r)
	}
	return dirs
}

// Run starts every poller and blocks until ctx is done, then waits for
// them to stop. It returns nil.
func (d *Daemon) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	d.runCtx = ctx

	if d.repoWatcher == nil {
		w, err := NewFSWatcher(ctx, d.log)
		if err != nil {
			d.log.Warn("repo watcher unavailable; repos rescan on the interval only", "err", err)
		} else {
			d.repoWatcher = w
		}
	}

	d.log.Info("aimd scheduler starting",
		"sessions", d.sessionInterval(), "worktrees", d.cfg.Poll.Worktrees,
		"github", d.cfg.Poll.GitHubActive, "idle_every", d.idleEvery())

	var wg sync.WaitGroup
	wg.Go(func() {
		if err := d.poller.Run(ctx); err != nil {
			d.log.Error("session poller", "err", err)
		}
	})
	wg.Go(func() {
		if err := d.watchSessions(ctx, d.poller.Kick); err != nil {
			d.log.Warn("session watcher stopped; sessions poll on the interval only", "err", err)
		}
	})
	wg.Go(func() { d.worktreeLoop(ctx) })
	wg.Go(func() { d.githubLoop(ctx) })
	wg.Go(func() { d.retentionLoop(ctx) })
	wg.Wait()
	return nil
}

// SetWatchers records how many dashboards (SSE clients) are connected. It
// switches the session poll interval, and when the first one connects it
// asks for the open-PR search if that is due at the watched cadence. Safe
// to call at any time.
func (d *Daemon) SetWatchers(n int) {
	d.mu.Lock()
	prev := d.watchers
	d.watchers = n
	d.mu.Unlock()
	d.poller.SetInterval(d.sessionInterval())
	if prev == 0 && n > 0 {
		kick(d.searchKick)
	}
}

// SetModLive records whether the mod reports live (internal/live). While it
// does, sessions are polled every poll.sessions_mod_live, since the mod's
// events carry the detail and the poll is only a backstop.
func (d *Daemon) SetModLive(live bool) {
	d.mu.Lock()
	d.modLive = live
	d.mu.Unlock()
	d.poller.SetInterval(d.sessionInterval())
}

// SessionInterval returns the session poll interval in effect.
func (d *Daemon) SessionInterval() time.Duration { return d.poller.Interval() }

// sessionInterval picks the interval for the current watchers and mod
// state.
func (d *Daemon) sessionInterval() time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case d.modLive:
		return d.cfg.Poll.SessionsModLive
	case d.watchers > 0:
		return d.cfg.Poll.Sessions
	default:
		return d.cfg.Poll.SessionsUnwatched
	}
}

// idleEvery is how many GitHub ticks make one idle interval.
func (d *Daemon) idleEvery() int {
	if d.cfg.Poll.GitHubActive <= 0 {
		return 5
	}
	n := int((d.cfg.Poll.GitHubIdle + d.cfg.Poll.GitHubActive/2) / d.cfg.Poll.GitHubActive)
	return max(1, n)
}

// Status returns the pollers' health and the GitHub budget. Counts are
// left for the caller (internal/web reads them from the store).
func (d *Daemon) Status() api.Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	st := api.Status{
		Version:   d.version,
		StartedAt: d.startedAt,
		Now:       time.Now(),
		Watchers:  d.watchers,
		ModLive:   d.modLive,
		GitHub:    d.ghStatus,
	}
	for _, name := range []string{api.SourceSessions, api.SourceWorktrees, api.SourceGitHub, api.SourceGitHubSearch, api.SourceRetention} {
		s := *d.sources[name]
		s.IntervalSeconds = int64(d.intervalLocked(name) / time.Second)
		s.Stale = !s.FailingSince.IsZero()
		st.Sources = append(st.Sources, s)
	}
	return st
}

// RepoNames maps the main checkout path of each repo in the last worktree
// scan (as stored in worktrees.main_repo and sessions.repo) to the GitHub
// nameWithOwner its PRs are stored under. That is the name GitHub answered
// with once it has (it differs from the remote after a transfer), else the
// name the repo's remote gives. Repos without a GitHub remote are left out.
// The dashboard joins worktrees to their PRs with it.
func (d *Daemon) RepoNames() map[string]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[string]string{}
	if d.scan == nil {
		return out
	}
	for i := range d.scan.Repos {
		repo := &d.scan.Repos[i]
		owner, name, ok := repo.GitHubRepo()
		if !ok {
			continue
		}
		full := owner + "/" + name
		if c, ok := d.canonical[strings.ToLower(full)]; ok {
			full = c // see CanonicalRepo
		}
		out[repo.Path] = full
	}
	return out
}

// intervalLocked is how often a source runs now. d.mu is held.
func (d *Daemon) intervalLocked(name string) time.Duration {
	switch name {
	case api.SourceSessions:
		return d.poller.Interval()
	case api.SourceWorktrees:
		return d.cfg.Poll.Worktrees
	case api.SourceGitHub:
		return d.cfg.Poll.GitHubActive
	case api.SourceGitHubSearch:
		return d.searchCadenceLocked()
	case api.SourceRetention:
		return retentionEvery
	}
	return 0
}

// ran records a run of a source. err nil means it worked; partial counts
// the parts that failed while the run as a whole worked. A change in
// health is logged and notified as a status change.
//
// A failed run marks the source failing (FailingSince, from the first
// failed run in a row), except a timeout (an error that wraps
// context.DeadlineExceeded), which a loaded machine causes with nothing
// wrong. A timeout marks it failing only as the second failed run
// in a row, or when the last success (before any, aimd's start) is more
// than failAfterIntervals of the source's intervals old. Until then the
// source isn't stale; Error has the timeout, and the data stays as last
// read, as after any failed run. Each run of failures is logged once
// when it starts, once more if it marks the source failing, and once
// when it ends.
func (d *Daemon) ran(name string, at time.Time, err error, partial int) {
	d.mu.Lock()
	s, st := d.sources[name], d.streaks[name]
	wasFailing := !s.FailingSince.IsZero()
	wasStreak := st.fails > 0
	s.Runs++
	s.LastRun = at
	s.Skipped = ""
	s.PartialErrors = partial
	if err == nil {
		s.LastOK = at
		s.FailingSince = time.Time{}
		s.Error = ""
		*st = streak{}
	} else {
		st.fails++
		if st.since.IsZero() {
			st.since = at
		}
		s.Error = err.Error()
		if s.FailingSince.IsZero() && d.failsLocked(name, s, st, at, err) {
			s.FailingSince = st.since
		}
	}
	failing := !s.FailingSince.IsZero()
	d.mu.Unlock()

	switch {
	case failing && !wasFailing:
		d.log.Warn("source failing", "source", name, "err", err)
		d.notify(api.TopicStatus)
	case err != nil && !wasStreak:
		d.log.Info("source timed out; its data stays as last read", "source", name, "err", err)
	case err == nil && wasStreak:
		d.log.Info("source recovered", "source", name)
		if wasFailing {
			d.notify(api.TopicStatus)
		}
	}
}

// failsLocked reports whether a failed run marks a source failing that
// isn't yet (see ran). d.mu is held.
func (d *Daemon) failsLocked(name string, s *api.Source, st *streak, at time.Time, err error) bool {
	if !errors.Is(err, context.DeadlineExceeded) || st.fails >= failAfterTimeouts {
		return true
	}
	lastOK := s.LastOK
	if lastOK.IsZero() {
		lastOK = d.startedAt
	}
	return at.Sub(lastOK) > failAfterIntervals*d.intervalLocked(name)
}

// skipped records that a source's tick didn't run, and why.
func (d *Daemon) skipped(name string, at time.Time, why string) {
	d.mu.Lock()
	s := d.sources[name]
	changed := s.Skipped != why
	s.LastRun = at
	s.Skipped = why
	d.mu.Unlock()
	if changed {
		d.log.Info("source skipped", "source", name, "why", why)
		d.notify(api.TopicStatus)
	}
}

// kick sends on a capacity-1 channel without blocking.
func kick(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// retentionLoop purges old events once a day, starting a minute after
// start.
func (d *Daemon) retentionLoop(ctx context.Context) {
	t := time.NewTimer(retentionFirst)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			now := time.Now()
			n, err := d.st.PurgeEvents(ctx, now, d.cfg.EventRetentionDays)
			if ctx.Err() != nil {
				return
			}
			d.ran(api.SourceRetention, now, err, 0)
			if err == nil && n > 0 {
				d.log.Info("purged events", "deleted", n, "retention_days", d.cfg.EventRetentionDays)
			}
			t.Reset(retentionEvery)
		}
	}
}
