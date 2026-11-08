package web

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/github"
	"github.com/gravexa-llc/ai-manager/internal/prune"
	"github.com/gravexa-llc/ai-manager/internal/sessions"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// ErrVerdictsPending is what a SafeToRemove returns before its first
// answer. The pages say the verdict is on its way rather than that it
// failed.
var ErrVerdictsPending = errors.New("the safe-to-remove verdict is being worked out")

// PruneVerdicts is the SafeToRemove aimd wires: internal/prune's Checker, the
// rules `aim ls` and `aim prune` apply, fed from aimd's store instead of
// fresh GitHub and `claude agents` calls, so it costs no GitHub points and
// agrees with what the pages show.
//
// An evaluation rescans every repo and runs git in each candidate worktree,
// which takes seconds, so it runs in the background: Verdicts starts one
// when the last is older than MinAge and returns the last finished answer
// meanwhile. Nothing runs while nobody looks at a page that shows verdicts.
type PruneVerdicts struct {
	ctx     context.Context
	checker prune.Checker
	status  StatusSource
	minAge  time.Duration
	update  func()
	log     *slog.Logger

	mu      sync.Mutex
	running bool
	stale   bool      // Refresh was called: evaluate again on the next call
	done    time.Time // when the last evaluation finished; zero before
	last    map[string]Verdict
	err     error
}

// Refresh makes the next Verdicts call start an evaluation even when the
// last is younger than MinAge (after the dashboard removed worktrees, say).
// Verdicts still returns the last answer meanwhile.
func (p *PruneVerdicts) Refresh() {
	p.mu.Lock()
	p.stale = true
	p.mu.Unlock()
}

// PruneOptions configure PruneVerdicts.
type PruneOptions struct {
	// Checker has the Scanner, Procs and Rebuildable `aim prune` uses. Its
	// GitHub and Sessions are replaced by the store-backed sources.
	Checker *prune.Checker
	Store   Store
	// Status is the daemon's: a sessions or GitHub source that is failing,
	// or hasn't run yet, makes what it would answer unknown (never safe).
	Status StatusSource
	// Canonical maps a remote's owner/name to the name PRs are stored
	// under (the daemon's GitHubName); nil keeps it.
	Canonical func(ownerName string) string
	// MinAge is the least time between evaluations; 2 minutes if zero.
	MinAge time.Duration
	// OnUpdate is called after each evaluation, so open pages refresh.
	OnUpdate func()
	Logger   *slog.Logger
}

// evalTimeout bounds one evaluation.
const evalTimeout = 2 * time.Minute

// NewPruneVerdicts returns verdicts that evaluate in the background until
// ctx ends.
func NewPruneVerdicts(ctx context.Context, o PruneOptions) *PruneVerdicts {
	p := &PruneVerdicts{
		ctx:     ctx,
		checker: *o.Checker,
		status:  o.Status,
		minAge:  cmp.Or(o.MinAge, 2*time.Minute),
		update:  o.OnUpdate,
		log:     o.Logger,
	}
	if p.log == nil {
		p.log = slog.New(slog.DiscardHandler)
	}
	p.checker.GitHub = storePRs{st: o.Store, status: o.Status, canonical: o.Canonical}
	p.checker.Sessions = storeSessions{st: o.Store, status: o.Status}
	return p
}

// Verdicts implements SafeToRemove: the last evaluation's answer, or
// ErrVerdictsPending before the first. It starts an evaluation in the
// background when the last is older than MinAge.
func (p *PruneVerdicts) Verdicts(context.Context) (map[string]Verdict, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.running && p.ctx.Err() == nil && (p.stale || p.done.IsZero() || time.Since(p.done) >= p.minAge) {
		p.running, p.stale = true, false
		go p.run()
	}
	if p.done.IsZero() {
		return nil, ErrVerdictsPending
	}
	return p.last, p.err
}

// run is one background evaluation. Until the daemon has polled sessions
// and GitHub once, every candidate would come out "unknown", so it waits
// for that and leaves the answer pending; the next page refresh (the first
// PR write publishes one) asks again.
func (p *PruneVerdicts) run() {
	if !p.sourcesReady() {
		p.mu.Lock()
		p.running = false
		p.mu.Unlock()
		return
	}
	v, err := p.Evaluate(p.ctx)
	if err != nil {
		p.log.Warn("safe-to-remove verdicts", "err", err)
	}
	p.mu.Lock()
	p.running = false
	p.done, p.last, p.err = time.Now(), v, err
	p.mu.Unlock()
	if p.update != nil && p.ctx.Err() == nil {
		p.update()
	}
}

// sourcesReady reports whether the sessions and GitHub sources have each
// run (or been skipped) once; true without a Status. A source that ran and
// failed is ready: its answers come out unknown.
func (p *PruneVerdicts) sourcesReady() bool {
	if p.status == nil {
		return true
	}
	st := p.status.Status()
	for _, name := range []string{api.SourceSessions, api.SourceGitHub} {
		if src, ok := source(st, name); ok && src.LastRun.IsZero() {
			return false
		}
	}
	return true
}

// Evaluate runs the prune checker once and returns each worktree's verdict,
// keyed by worktree path.
func (p *PruneVerdicts) Evaluate(ctx context.Context) (map[string]Verdict, error) {
	ctx, cancel := context.WithTimeout(ctx, evalTimeout)
	defer cancel()
	rep, err := p.checker.Evaluate(ctx, prune.Options{})
	if err != nil {
		return nil, err
	}
	out := map[string]Verdict{}
	for _, rr := range rep.Repos {
		for _, it := range rr.Items {
			out[it.Worktree.Path] = Verdict{
				Category:  string(it.Category),
				Removable: it.Category.Removable(false),
				Reasons:   slices.Clone(it.Reasons),
			}
		}
	}
	return out, nil
}

// source finds a source in a status.
func source(st api.Status, name string) (api.Source, bool) {
	for _, s := range st.Sources {
		if s.Name == name {
			return s, true
		}
	}
	return api.Source{}, false
}

// sourceErr says why a source's data can't be trusted: it is failing, or
// hasn't succeeded yet. nil without a Status.
func sourceErr(status StatusSource, name, what string) error {
	if status == nil {
		return nil
	}
	src, ok := source(status.Status(), name)
	switch {
	case !ok:
		return nil
	case !src.FailingSince.IsZero():
		return errors.New(what + " stale since " + src.FailingSince.Format(time.RFC3339) + ": " + src.Error)
	case src.LastOK.IsZero():
		return errors.New(what + " not read yet")
	}
	return nil
}

// storePRs is a prune.PRSource that answers from the store: the PRs the
// daemon's GitHub poller wrote. A branch with no stored PR has no PR, which
// is never safe. While the GitHub source is failing, every answer is an
// error, so nothing is safe on stale data.
type storePRs struct {
	st        Store
	status    StatusSource
	canonical func(string) string
}

// Budget implements prune.PRSource: reading the store costs nothing.
func (storePRs) Budget() github.Budget { return github.Budget{Verdict: github.BudgetNormal} }

// BranchPRs implements prune.PRSource. Like github.Client, it leaves out
// PRs from forks and lists a branch's PRs newest first.
func (s storePRs) BranchPRs(ctx context.Context, repos []github.RepoBranches) github.BranchPRsResult {
	var res github.BranchPRsResult
	fail := sourceErr(s.status, api.SourceGitHub, "GitHub data")
	byBranch := map[string][]store.PR{} // lower-case nameWithOwner + "\x00" + branch
	if fail == nil {
		all, err := s.st.ListPRs(ctx, store.PRFilter{})
		if err != nil {
			fail = err
		}
		for _, p := range all {
			if !p.IsCrossRepository && p.HeadBranch != "" {
				k := strings.ToLower(p.Repo) + "\x00" + p.HeadBranch
				byBranch[k] = append(byBranch[k], p)
			}
		}
	}
	for _, rb := range repos {
		name := rb.Owner + "/" + rb.Name
		if s.canonical != nil {
			name = s.canonical(name)
		}
		r := github.RepoPRs{Owner: rb.Owner, Name: rb.Name, Repo: name, Err: fail}
		if fail != nil {
			r.Repo = ""
		}
		var seen []string
		for _, b := range rb.Branches {
			b = strings.TrimPrefix(b, "refs/heads/")
			if b == "" || slices.Contains(seen, b) {
				continue
			}
			seen = append(seen, b)
			bp := github.BranchPRs{Branch: b, Err: fail}
			if fail == nil {
				prs := byBranch[strings.ToLower(name)+"\x00"+b]
				slices.SortFunc(prs, func(a, b store.PR) int { return cmp.Compare(b.Number, a.Number) })
				bp.PRs = make([]github.PR, 0, len(prs))
				for _, p := range prs {
					bp.PRs = append(bp.PRs, githubPR(p))
				}
				bp.TotalCount = len(bp.PRs)
			}
			r.Branches = append(r.Branches, bp)
		}
		res.Repos = append(res.Repos, r)
	}
	return res
}

// githubPR converts a stored PR back to the client's shape, as far as the
// prune rules read it.
func githubPR(p store.PR) github.PR {
	return github.PR{
		Repo:            p.Repo,
		Number:          p.Number,
		State:           github.State(p.State),
		Draft:           p.Draft,
		HeadRef:         p.HeadBranch,
		HeadOID:         p.HeadRefOID,
		BaseRef:         p.BaseRef,
		CrossRepository: p.IsCrossRepository,
		Title:           p.Title,
		URL:             p.URL,
		Author:          p.Author,
		UpdatedAt:       p.UpdatedAt,
		MergedAt:        p.MergedAt,
		ClosedAt:        p.ClosedAt,
	}
}

// storeSessions is a prune.SessionLister that answers from the store: the
// live sessions the daemon's poller wrote, headless runs included. While
// the sessions source is failing, it fails, so rule 2 is unknown.
type storeSessions struct {
	st     Store
	status StatusSource
}

// List implements prune.SessionLister.
func (s storeSessions) List(ctx context.Context) (sessions.Snapshot, error) {
	if err := sourceErr(s.status, api.SourceSessions, "the session list"); err != nil {
		return sessions.Snapshot{}, err
	}
	rows, err := s.st.ListSessions(ctx, store.SessionFilter{})
	if err != nil {
		return sessions.Snapshot{}, err
	}
	snap := sessions.Snapshot{At: time.Now()}
	for _, r := range rows {
		ss := sessions.Session{
			JobID:     r.JobID,
			PID:       r.PID,
			SessionID: r.SessionID,
			Name:      r.Name,
			Cwd:       r.Cwd,
			Kind:      sessions.Kind(r.Kind),
			StartedAt: r.StartedAt,
		}
		// Only active sessions are stored live; a job without a process
		// is still one (Session.Active).
		if ss.PID == 0 {
			ss.State = sessions.JobWorking
		}
		snap.Sessions = append(snap.Sessions, ss)
	}
	return snap, nil
}
