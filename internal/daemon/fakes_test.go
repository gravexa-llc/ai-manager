package daemon

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/config"
	"github.com/gravexa-llc/ai-manager/internal/github"
	"github.com/gravexa-llc/ai-manager/internal/sessions"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/term"
	"github.com/gravexa-llc/ai-manager/internal/worktrees"
)

// entry is one `claude agents --json` element for fakeRunner.
type entry struct {
	ID         string `json:"id,omitempty"`
	PID        int    `json:"pid,omitempty"`
	SessionID  string `json:"sessionId,omitempty"`
	Cwd        string `json:"cwd"`
	Kind       string `json:"kind"`
	StartedAt  int64  `json:"startedAt,omitempty"`
	Status     string `json:"status,omitempty"`
	WaitingFor string `json:"waitingFor,omitempty"`
	State      string `json:"state,omitempty"`
}

// fakeRunner stands in for `claude`: it answers `agents --json` with the
// entries set, and records when it was called.
type fakeRunner struct {
	mu      sync.Mutex
	entries []entry
	err     error
	// delay is how long each call takes. A call still running when its
	// context ends returns the context's error, as the real one killed at
	// its timeout does.
	delay time.Duration
	calls []time.Time
}

func (f *fakeRunner) Run(ctx context.Context, cmd sessions.Cmd) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, time.Now())
	delay, err := f.delay, f.err
	out, jsonErr := json.Marshal(append([]entry{}, f.entries...))
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	return out, jsonErr
}

func (f *fakeRunner) set(es ...entry) {
	f.mu.Lock()
	f.entries = es
	f.mu.Unlock()
}

func (f *fakeRunner) setDelay(d time.Duration) {
	f.mu.Lock()
	f.delay = d
	f.mu.Unlock()
}

func (f *fakeRunner) callTimes() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// fakeScanner answers Scan and ScanRepos from a result the test sets.
type fakeScanner struct {
	mu        sync.Mutex
	res       *worktrees.Result
	scans     []scanCall
	repoScans []scanCall
}

type scanCall struct {
	at   time.Time
	args []string
}

func (f *fakeScanner) Scan(ctx context.Context, extra ...string) *worktrees.Result {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scans = append(f.scans, scanCall{time.Now(), slices.Clone(extra)})
	if f.res == nil {
		return &worktrees.Result{}
	}
	cp := *f.res
	cp.Repos = slices.Clone(f.res.Repos)
	return &cp
}

func (f *fakeScanner) ScanRepos(ctx context.Context, gitDirs []string) []worktrees.Repo {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repoScans = append(f.repoScans, scanCall{time.Now(), slices.Clone(gitDirs)})
	out := make([]worktrees.Repo, len(gitDirs))
	for i, g := range gitDirs {
		out[i] = worktrees.Repo{GitDir: g, Path: g}
		if f.res != nil {
			for _, r := range f.res.Repos {
				if r.GitDir == g {
					out[i] = r
				}
			}
		}
	}
	return out
}

func (f *fakeScanner) set(res *worktrees.Result) {
	f.mu.Lock()
	f.res = res
	f.mu.Unlock()
}

func (f *fakeScanner) calls() (scans, repoScans []scanCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.scans), slices.Clone(f.repoScans)
}

// fakeGitHub records what it is asked and answers from the test's data.
type fakeGitHub struct {
	mu        sync.Mutex
	budget    func(now time.Time) github.Budget
	prs       map[string][]github.PR // "owner/name:branch" → PRs
	defaults  map[string]string      // "owner/name" → default branch
	answerAs  map[string]string      // "owner/name" → the nameWithOwner GitHub answers with (a transfer)
	open      []github.PR
	healthErr error
	branchReq []ghCall
	searchReq []ghCall

	// The review-request search: its answer, owners whose search fails,
	// whether it was cut off, and the calls.
	review          []github.PR
	reviewErr       map[string]error // lower-case owner → error
	reviewTruncated bool
	reviewReq       []ghCall

	// branchDelay is how long each BranchPRs call takes.
	branchDelay time.Duration

	// PRsByNumber: the PRs GitHub has by "owner/name#n" (lower-case
	// key), the repos it fails for (lower-case "owner/name"), and the
	// calls. A number it hasn't got fails with ErrNotFound.
	byNumber  map[string]github.PR
	numberErr map[string]error
	numberReq []ghCall
}

type ghCall struct {
	at    time.Time
	repos []github.RepoBranches
	owner []string
	nums  []github.RepoNumbers
}

func (f *fakeGitHub) PRsByNumber(ctx context.Context, repos []github.RepoNumbers) github.NumberPRsResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]github.RepoNumbers, len(repos))
	for i, r := range repos {
		cp[i] = github.RepoNumbers{Owner: r.Owner, Name: r.Name, Numbers: slices.Clone(r.Numbers)}
	}
	f.numberReq = append(f.numberReq, ghCall{at: time.Now(), nums: cp})
	res := github.NumberPRsResult{Queries: 1, Cost: 1}
	for _, r := range repos {
		key := strings.ToLower(r.Owner + "/" + r.Name)
		rp := github.RepoNumberPRs{Owner: r.Owner, Name: r.Name}
		if err := f.numberErr[key]; err != nil {
			rp.Err = err
			for _, n := range r.Numbers {
				rp.Numbers = append(rp.Numbers, github.NumberPR{Number: n, Err: err})
			}
			res.Repos = append(res.Repos, rp)
			continue
		}
		rp.Repo = r.Owner + "/" + r.Name
		for _, n := range r.Numbers {
			np := github.NumberPR{Number: n}
			if pr, ok := f.byNumber[prKey(r.Owner+"/"+r.Name, n)]; ok {
				np.PR = &pr
			} else {
				np.Err = github.ErrNotFound
				rp.Err = cmpOrErr(rp.Err, np.Err)
			}
			rp.Numbers = append(rp.Numbers, np)
		}
		res.Repos = append(res.Repos, rp)
	}
	return res
}

// cmpOrErr returns the first non-nil error.
func cmpOrErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

func (f *fakeGitHub) numberCalls() []ghCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.numberReq)
}

func (f *fakeGitHub) BranchPRs(ctx context.Context, repos []github.RepoBranches) github.BranchPRsResult {
	f.mu.Lock()
	cp := make([]github.RepoBranches, len(repos))
	for i, r := range repos {
		cp[i] = github.RepoBranches{Owner: r.Owner, Name: r.Name, Branches: slices.Clone(r.Branches)}
	}
	f.branchReq = append(f.branchReq, ghCall{at: time.Now(), repos: cp})
	delay := f.branchDelay
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return github.BranchPRsResult{}
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	res := github.BranchPRsResult{Queries: 1, Cost: 1}
	for _, r := range repos {
		key := r.Owner + "/" + r.Name
		rp := github.RepoPRs{Owner: r.Owner, Name: r.Name, Repo: key, DefaultBranch: f.defaults[key]}
		if as := f.answerAs[key]; as != "" {
			rp.Repo = as
		}
		for _, b := range r.Branches {
			rp.Branches = append(rp.Branches, github.BranchPRs{Branch: b, PRs: f.prs[key+":"+b]})
		}
		res.Repos = append(res.Repos, rp)
	}
	return res
}

func (f *fakeGitHub) OpenPRs(ctx context.Context, owners []string) github.OpenPRsResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searchReq = append(f.searchReq, ghCall{at: time.Now(), owner: slices.Clone(owners)})
	res := github.OpenPRsResult{PRs: slices.Clone(f.open), Queries: 1, Cost: 1}
	for _, o := range owners {
		res.Owners = append(res.Owners, github.OwnerStatus{Owner: o})
	}
	return res
}

// ReviewRequests answers with f.review; an owner in f.reviewErr fails.
func (f *fakeGitHub) ReviewRequests(ctx context.Context, owners []string) github.OpenPRsResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reviewReq = append(f.reviewReq, ghCall{at: time.Now(), owner: slices.Clone(owners)})
	res := github.OpenPRsResult{Queries: 1, Cost: 1, Truncated: f.reviewTruncated}
	failed := map[string]bool{}
	for _, o := range owners {
		st := github.OwnerStatus{Owner: o}
		if err := f.reviewErr[strings.ToLower(o)]; err != nil {
			st.Err = err
			failed[strings.ToLower(o)] = true
		}
		res.Owners = append(res.Owners, st)
	}
	for _, p := range f.review {
		if o, _, _ := strings.Cut(p.Repo, "/"); !failed[strings.ToLower(o)] {
			res.PRs = append(res.PRs, p)
		}
	}
	return res
}

func (f *fakeGitHub) Budget() github.Budget {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.budget == nil {
		return github.Budget{Verdict: github.BudgetNormal}
	}
	return f.budget(time.Now())
}

func (f *fakeGitHub) Health() github.Health {
	f.mu.Lock()
	defer f.mu.Unlock()
	return github.Health{Err: f.healthErr}
}

func (f *fakeGitHub) calls() (branch, search []ghCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.branchReq), slices.Clone(f.searchReq)
}

// fakeStore keeps rows in maps, with the store's live/ended semantics.
type fakeStore struct {
	mu        sync.Mutex
	sessions  map[string]store.Session // key → live row
	ended     map[string]time.Time
	worktrees map[string]store.Worktree
	prs       map[string]store.PR
	repos     []store.Repo // every UpsertRepos row, in order
	purges    []time.Time
	upserts   int

	reviewCalls []reviewCall
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		sessions:  map[string]store.Session{},
		ended:     map[string]time.Time{},
		worktrees: map[string]store.Worktree{},
		prs:       map[string]store.PR{},
	}
}

func (f *fakeStore) UpsertRepos(ctx context.Context, repos ...store.Repo) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repos = append(f.repos, repos...)
	return nil
}

func (f *fakeStore) repoWrites() []store.Repo {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.repos)
}

func (f *fakeStore) UpsertSessions(ctx context.Context, ss ...store.Session) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upserts++
	for _, s := range ss {
		if old, ok := f.sessions[s.Key]; ok && !old.NeedsYouSince.IsZero() && !s.NeedsYouSince.IsZero() {
			s.NeedsYouSince = old.NeedsYouSince
		}
		f.sessions[s.Key] = s
		delete(f.ended, s.Key)
	}
	return nil
}

func (f *fakeStore) ListSessions(ctx context.Context, _ store.SessionFilter) ([]store.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Session
	for _, s := range f.sessions {
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b store.Session) int { return strings.Compare(a.Key, b.Key) })
	return out, nil
}

func (f *fakeStore) EndSession(ctx context.Context, key string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.sessions[key]; !ok {
		return store.ErrNotFound
	}
	delete(f.sessions, key)
	f.ended[key] = at
	return nil
}

func (f *fakeStore) UpsertWorktrees(ctx context.Context, wts ...store.Worktree) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, w := range wts {
		w.RemovedAt = time.Time{}
		f.worktrees[w.Path] = w
	}
	return nil
}

func (f *fakeStore) ListWorktrees(ctx context.Context, flt store.WorktreeFilter) ([]store.Worktree, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.Worktree
	for _, w := range f.worktrees {
		if (flt.IncludeRemoved || w.RemovedAt.IsZero()) && (flt.MainRepo == "" || flt.MainRepo == w.MainRepo) {
			out = append(out, w)
		}
	}
	slices.SortFunc(out, func(a, b store.Worktree) int { return strings.Compare(a.Path, b.Path) })
	return out, nil
}

func (f *fakeStore) MarkMissingWorktreesRemoved(ctx context.Context, mainRepo string, present []string, at time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for p, w := range f.worktrees {
		if w.MainRepo == mainRepo && w.RemovedAt.IsZero() && !slices.Contains(present, p) {
			w.RemovedAt = at
			f.worktrees[p] = w
			n++
		}
	}
	return n, nil
}

func (f *fakeStore) UpsertPRs(ctx context.Context, prs ...store.PR) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range prs {
		k := prKey(p.Repo, p.Number)
		p.ReviewRequestedAt = f.prs[k].ReviewRequestedAt // only SetReviewRequests writes it
		f.prs[k] = p
	}
	return nil
}

// reviewCall is one SetReviewRequests call.
type reviewCall struct {
	owners []string
	found  []store.PRKey
}

// SetReviewRequests has the store's semantics: found PRs of the searched
// owners get the request (first time kept), their others lose it.
func (f *fakeStore) SetReviewRequests(ctx context.Context, owners []string, found []store.PRKey, at time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reviewCalls = append(f.reviewCalls, reviewCall{slices.Clone(owners), slices.Clone(found)})
	searched := func(repo string) bool {
		o, _, _ := strings.Cut(repo, "/")
		return slices.ContainsFunc(owners, func(s string) bool { return strings.EqualFold(s, o) })
	}
	want := map[string]bool{}
	for _, k := range found {
		want[prKey(k.Repo, k.Number)] = true
	}
	var n int64
	for k, p := range f.prs {
		if !searched(p.Repo) {
			continue
		}
		switch {
		case want[k] && p.ReviewRequestedAt.IsZero():
			p.ReviewRequestedAt = at
		case !want[k] && !p.ReviewRequestedAt.IsZero():
			p.ReviewRequestedAt = time.Time{}
		default:
			continue
		}
		f.prs[k] = p
		n++
	}
	return n, nil
}

func (f *fakeStore) reviews() []reviewCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reviewCalls)
}

func (f *fakeStore) ListPRs(ctx context.Context, flt store.PRFilter) ([]store.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []store.PR
	for _, p := range f.prs {
		if flt.State == "" || p.State == flt.State {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b store.PR) int { return strings.Compare(prKey(a.Repo, a.Number), prKey(b.Repo, b.Number)) })
	return out, nil
}

func (f *fakeStore) PurgeEvents(ctx context.Context, now time.Time, days int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.purges = append(f.purges, now)
	return 0, nil
}

func (f *fakeStore) pr(repo string, n int) store.PR {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prs[prKey(repo, n)]
}

func (f *fakeStore) liveSessions() map[string]store.Session {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]store.Session{}
	for k, v := range f.sessions {
		out[k] = v
	}
	return out
}

func (f *fakeStore) endedKeys() map[string]time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]time.Time{}
	for k, v := range f.ended {
		out[k] = v
	}
	return out
}

// fakeWatcher is a RepoWatcher the test drives.
type fakeWatcher struct {
	mu     sync.Mutex
	paths  []string
	events chan string
}

func newFakeWatcher() *fakeWatcher { return &fakeWatcher{events: make(chan string, 16)} }

func (f *fakeWatcher) Set(paths []string) {
	f.mu.Lock()
	f.paths = slices.Clone(paths)
	f.mu.Unlock()
}

func (f *fakeWatcher) Events() <-chan string { return f.events }

func (f *fakeWatcher) watched() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.paths)
}

// notes records Notify calls.
type notes struct {
	mu     sync.Mutex
	topics []string
}

func (n *notes) notify(topic string) {
	n.mu.Lock()
	n.topics = append(n.topics, topic)
	n.mu.Unlock()
}

func (n *notes) count(topic string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	c := 0
	for _, t := range n.topics {
		if t == topic {
			c++
		}
	}
	return c
}

// fakeProcs is the process snapshot: the one the test set (empty at
// first), or an error.
type fakeProcs struct {
	mu    sync.Mutex
	procs term.Procs
	err   error
	calls int
}

func (f *fakeProcs) snapshot(context.Context) (term.Procs, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.procs, nil
}

func (f *fakeProcs) set(procs term.Procs, err error) {
	f.mu.Lock()
	f.procs, f.err = procs, err
	f.mu.Unlock()
}

func (f *fakeProcs) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// rig is a daemon wired to fakes.
type rig struct {
	d       *Daemon
	runner  *fakeRunner
	scanner *fakeScanner
	gh      *fakeGitHub
	st      *fakeStore
	watcher *fakeWatcher
	procs   *fakeProcs
	notes   *notes
	kick    func() // the session watcher's kick, once Run started it
	kickMu  sync.Mutex
}

// newRig builds a daemon on fakes with the default config, roots at /r.
func newRig(t *testing.T, mod func(*config.Config)) *rig {
	t.Helper()
	cfg := config.Default()
	cfg.Roots = []string{"/r"}
	cfg.Owners = map[string]string{"o": "acct"}
	if mod != nil {
		mod(&cfg)
	}
	r := &rig{
		runner:  &fakeRunner{},
		scanner: &fakeScanner{},
		gh:      &fakeGitHub{prs: map[string][]github.PR{}, defaults: map[string]string{}},
		st:      newFakeStore(),
		watcher: newFakeWatcher(),
		procs:   &fakeProcs{procs: term.Procs{}},
		notes:   &notes{},
	}
	r.d = New(Options{
		Config:      cfg,
		Store:       r.st,
		Lister:      sessions.Lister{Runner: r.runner},
		Scanner:     r.scanner,
		GitHub:      r.gh,
		RepoWatcher: r.watcher,
		Procs:       r.procs.snapshot,
		Notify:      r.notes.notify,
		TempDirs:    []string{"/tmpdir"},
		Host:        "testhost",
		WatchSessions: func(ctx context.Context, kick func()) error {
			r.kickMu.Lock()
			r.kick = kick
			r.kickMu.Unlock()
			<-ctx.Done()
			return nil
		},
	})
	return r
}

// start runs the daemon until the test ends.
func (r *rig) start(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.d.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func (r *rig) kickSessions() {
	r.kickMu.Lock()
	k := r.kick
	r.kickMu.Unlock()
	k()
}

// repo builds a scanned repo at /r/<name> with a GitHub origin o/<name>
// and worktrees on the given branches (the first is the main checkout).
func repo(name string, lastCommit time.Time, branches ...string) worktrees.Repo {
	path := "/r/" + name
	r := worktrees.Repo{
		Path:    path,
		GitDir:  path + "/.git",
		Remotes: map[string]string{"origin": "https://github.com/o/" + name + ".git"},
	}
	for i, b := range branches {
		p := path
		if i > 0 {
			p = path + "-" + b
		}
		r.Worktrees = append(r.Worktrees, worktrees.Worktree{
			Path: p, RecordedPath: p, Main: i == 0, Branch: b, Head: strings.Repeat("a", 40),
			StatusRead: true, Upstream: worktrees.UpstreamOK, LastCommitAt: lastCommit,
		})
	}
	return r
}

// offsets returns each time as an offset from base, rounded to 100ms.
func offsets(base time.Time, ts []time.Time) []time.Duration {
	out := make([]time.Duration, len(ts))
	for i, t := range ts {
		out[i] = t.Sub(base).Round(100 * time.Millisecond)
	}
	return out
}
