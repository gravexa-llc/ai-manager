package web

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/config"
	"github.com/gravexa-llc/ai-manager/internal/github"
	"github.com/gravexa-llc/ai-manager/internal/prune"
	"github.com/gravexa-llc/ai-manager/internal/prune/prunetest"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
	"github.com/gravexa-llc/ai-manager/internal/worktrees"
)

// git in these tests is isolated from the machine's config (A3's helper).
func TestMain(m *testing.M) { os.Exit(prunetest.Main(m)) }

// healthy is a status whose sessions and GitHub sources have run and work.
func healthy() fakeStatus {
	return fakeStatus{api.Status{Sources: []api.Source{
		{Name: api.SourceSessions, Runs: 1, LastRun: t0, LastOK: t0},
		{Name: api.SourceGitHub, Runs: 1, LastRun: t0, LastOK: t0},
	}}}
}

// storeWorld copies an A3 test world's GitHub answers and live sessions
// into a store, the way the daemon would have written them, with every PR
// stored under repo.
func storeWorld(t *testing.T, w *prunetest.World, repo string) *store.Store {
	t.Helper()
	st := newTestStore(t)
	ctx := context.Background()
	for _, prs := range w.GitHub.PRs {
		for _, p := range prs {
			must(t, st.UpsertPRs(ctx, store.PR{
				Repo: repo, Number: p.Number, State: string(p.State), HeadBranch: p.HeadRef, HeadRefOID: p.HeadOID,
				BaseRef: p.BaseRef, IsCrossRepository: p.CrossRepository, URL: p.URL, Title: "t",
			}))
		}
	}
	snap, _ := w.Sessions.List(ctx)
	for _, s := range snap.Sessions {
		if !s.Active() {
			continue // the daemon ends sessions that are no longer active
		}
		must(t, st.UpsertSessions(ctx, store.Session{
			Key: string(s.Key()), JobID: s.JobID, PID: s.PID, SessionID: s.SessionID, Kind: string(s.Kind), Cwd: s.Cwd,
			State: string(s.Verdict().State), LastSeenAt: t0,
		}))
	}
	return st
}

func worldVerdicts(t *testing.T, w *prunetest.World, st Store, status StatusSource, canonical func(string) string) map[string]Verdict {
	t.Helper()
	m, err := prune.NewMatcher(config.DefaultRebuildable)
	if err != nil {
		t.Fatal(err)
	}
	p := NewPruneVerdicts(context.Background(), PruneOptions{
		Checker:   &prune.Checker{Scanner: worktrees.New(worktrees.Options{Roots: []string{w.Root}}), Procs: w.Procs, Rebuildable: m},
		Store:     st,
		Status:    status,
		Canonical: canonical,
	})
	v, err := p.Evaluate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestPruneVerdicts runs A3's rules on A3's test world (built once; it
// takes a while), with GitHub and sessions answered from the store.
func TestPruneVerdicts(t *testing.T) {
	w := prunetest.BuildWorld(t)
	st := storeWorld(t, w, "acme/widgets")
	t.Run("from store", func(t *testing.T) { testFromStore(t, w, st) })
	t.Run("stale sources", func(t *testing.T) { testStaleSources(t, w, st) })
	t.Run("background", func(t *testing.T) { testBackground(t, w, st) })
}

// testFromStore expects A3's verdicts for its world, though GitHub and
// sessions come from the store instead of A3's fakes.
func testFromStore(t *testing.T, w *prunetest.World, st *store.Store) {
	v := worldVerdicts(t, w, st, healthy(), nil)

	for name, want := range w.Want {
		path := w.WT[name]
		got, ok := v[path]
		if name == "gone" || name == "nogit" {
			// Prunable records are keyed by the path git recorded.
			for p, vd := range v {
				if worktrees.SamePath(p, path) {
					got, ok = vd, true
				}
			}
		}
		if !ok {
			t.Errorf("%s: no verdict for %s", name, path)
			continue
		}
		switch name {
		case "many":
			// The store keeps no PR count, so it can't know a branch has
			// more PRs than the poller read (open question in the PR).
			continue
		case "stalegh":
			// A3's fake failed this branch; the store just has no PR for it.
			if got.Category != "not-safe" || !strings.Contains(strings.Join(got.Reasons, "; "), "no PR for this branch") {
				t.Errorf("%s = %+v, want not-safe: no PR", name, got)
			}
			continue
		}
		if got.Category != want {
			t.Errorf("%s = %s (%v), want %s", name, got.Category, got.Reasons, want)
		}
		if r := strings.Join(got.Reasons, "; "); !strings.Contains(r, w.Reason[name]) {
			t.Errorf("%s reasons %q, want %q in them", name, r, w.Reason[name])
		}
		if got.Removable != prune.Category(want).Removable(false) {
			t.Errorf("%s: Removable = %v for %s", name, got.Removable, want)
		}
	}

	// Stored under the name GitHub gave after a transfer: found through
	// Canonical, and not without it.
	moved := storeWorld(t, w, "neworg/widgets")
	if v := worldVerdicts(t, w, moved, healthy(), nil); v[w.WT["merged"]].Category != "not-safe" {
		t.Errorf("transferred repo without Canonical: merged = %+v, want not-safe", v[w.WT["merged"]])
	}
	canon := func(n string) string {
		if strings.EqualFold(n, "acme/widgets") {
			return "neworg/widgets"
		}
		return n
	}
	if v := worldVerdicts(t, w, moved, healthy(), canon); v[w.WT["merged"]].Category != "safe" {
		t.Errorf("transferred repo with Canonical: merged = %+v, want safe", v[w.WT["merged"]])
	}
}

// testStaleSources: stale GitHub or session data is never grounds for
// "safe".
func testStaleSources(t *testing.T, w *prunetest.World, st *store.Store) {
	ghDown := healthy()
	ghDown.st.Sources[1].FailingSince, ghDown.st.Sources[1].Error = t0.Add(-time.Minute), "auth expired"
	v := worldVerdicts(t, w, st, ghDown, nil)
	if got := v[w.WT["merged"]]; got.Category != "not-safe" || !strings.Contains(strings.Join(got.Reasons, "; "), "GitHub data stale since") {
		t.Errorf("GitHub failing: merged = %+v, want not-safe, GitHub stale", got)
	}
	if got := v[w.WT["detached-main"]]; got.Category != "scratch" {
		t.Errorf("GitHub failing: detached-main = %+v; detached worktrees don't need PRs", got)
	}

	ghNever := healthy()
	ghNever.st.Sources[1].LastOK = time.Time{}
	if got := worldVerdicts(t, w, st, ghNever, nil)[w.WT["merged"]]; got.Category != "not-safe" || !strings.Contains(strings.Join(got.Reasons, "; "), "not read yet") {
		t.Errorf("GitHub never read: merged = %+v, want not-safe", got)
	}

	sessDown := healthy()
	sessDown.st.Sources[0].FailingSince = t0.Add(-time.Minute)
	v = worldVerdicts(t, w, st, sessDown, nil)
	for _, name := range []string{"merged", "detached-main"} {
		if got := v[w.WT[name]]; got.Category != "not-safe" || !strings.Contains(strings.Join(got.Reasons, "; "), "live sessions unknown") {
			t.Errorf("sessions failing: %s = %+v, want not-safe, sessions unknown", name, got)
		}
	}
}

// slowStatus is a StatusSource whose answer can change.
type slowStatus struct{ st atomic.Pointer[api.Status] }

func (s *slowStatus) Status() api.Status { return *s.st.Load() }

// testBackground: Verdicts answers pending, evaluates in the background
// once the sources have run, and not again within MinAge.
func testBackground(t *testing.T, w *prunetest.World, st *store.Store) {
	m, _ := prune.NewMatcher(config.DefaultRebuildable)
	status := &slowStatus{}
	notReady := healthy().st
	notReady.Sources[1].LastRun, notReady.Sources[1].LastOK = time.Time{}, time.Time{}
	status.st.Store(&notReady)
	updates := make(chan struct{}, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := NewPruneVerdicts(ctx, PruneOptions{
		Checker:  &prune.Checker{Scanner: worktrees.New(worktrees.Options{Roots: []string{w.Root}}), Procs: w.Procs, Rebuildable: m},
		Store:    st,
		Status:   status,
		MinAge:   time.Hour,
		OnUpdate: func() { updates <- struct{}{} },
	})

	// Before GitHub was polled once: pending, and no evaluation.
	if _, err := p.Verdicts(ctx); !errors.Is(err, ErrVerdictsPending) {
		t.Fatalf("first Verdicts err = %v, want pending", err)
	}
	waitIdle(t, p)
	select {
	case <-updates:
		t.Fatal("evaluated before the sources were ready")
	default:
	}
	if _, err := p.Verdicts(ctx); !errors.Is(err, ErrVerdictsPending) {
		t.Fatalf("Verdicts before the sources ran = %v, want still pending", err)
	}
	waitIdle(t, p)

	// Once they have: one evaluation in the background, then its answer.
	ready := healthy().st
	status.st.Store(&ready)
	if _, err := p.Verdicts(ctx); !errors.Is(err, ErrVerdictsPending) {
		t.Fatalf("Verdicts while the first runs = %v, want pending", err)
	}
	select {
	case <-updates:
	case <-time.After(30 * time.Second):
		t.Fatal("no OnUpdate within 30s")
	}
	v, err := p.Verdicts(ctx)
	if err != nil || v[w.WT["merged"]].Category != "safe" {
		t.Fatalf("Verdicts after the evaluation = %v, %v", v[w.WT["merged"]], err)
	}
	// Within MinAge, no new evaluation.
	for range 3 {
		_, _ = p.Verdicts(ctx)
	}
	waitIdle(t, p)
	select {
	case <-updates:
		t.Error("evaluated again within MinAge")
	default:
	}
	// Refresh (after a removal) starts one at the next call, within
	// MinAge, and the last answer stands meanwhile.
	p.Refresh()
	if v, err := p.Verdicts(ctx); err != nil || v[w.WT["merged"]].Category != "safe" {
		t.Fatalf("Verdicts after Refresh = %v, %v", v[w.WT["merged"]], err)
	}
	select {
	case <-updates:
	case <-time.After(30 * time.Second):
		t.Fatal("no evaluation within 30s of a Refresh")
	}
	waitIdle(t, p)
	_, _ = p.Verdicts(ctx)
	waitIdle(t, p)
	select {
	case <-updates:
		t.Error("a Refresh evaluated more than once")
	default:
	}
}

// waitIdle waits until no background evaluation runs.
func waitIdle(t *testing.T, p *PruneVerdicts) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		running := p.running
		p.mu.Unlock()
		if !running {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("evaluation still running after 30s")
}

func TestStorePRsAnswers(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	must(t, st.UpsertPRs(ctx,
		store.PR{Repo: "O/App", Number: 3, State: "MERGED", HeadBranch: "feat", HeadRefOID: "aaa", BaseRef: "main"},
		store.PR{Repo: "o/app", Number: 9, State: "OPEN", HeadBranch: "feat", HeadRefOID: "bbb", BaseRef: "main"},
		store.PR{Repo: "o/app", Number: 10, State: "OPEN", HeadBranch: "feat", IsCrossRepository: true},
		store.PR{Repo: "o/app", Number: 4, State: "CLOSED", HeadBranch: "other"},
	))
	s := storePRs{st: st, status: healthy()}
	res := s.BranchPRs(ctx, []github.RepoBranches{{Owner: "o", Name: "app", Branches: []string{"refs/heads/feat", "feat", "none", ""}}})
	if len(res.Repos) != 1 || res.Repos[0].Err != nil || res.Repos[0].Repo != "o/app" {
		t.Fatalf("repos = %+v", res.Repos)
	}
	br := res.Repos[0].Branches
	if len(br) != 2 || br[0].Branch != "feat" || br[1].Branch != "none" {
		t.Fatalf("branches = %+v, want feat and none once each", br)
	}
	// Newest first, the fork's PR left out, case-insensitive repo.
	if got := br[0].PRs; len(got) != 2 || got[0].Number != 9 || got[1].Number != 3 || br[0].TotalCount != 2 ||
		got[1].HeadOID != "aaa" || got[1].BaseRef != "main" || got[1].State != github.StateMerged {
		t.Errorf("feat = %+v", br[0])
	}
	if len(br[1].PRs) != 0 || br[1].Err != nil {
		t.Errorf("a branch with no PR = %+v, want no PRs and no error", br[1])
	}
	if b := s.Budget(); b.Verdict != github.BudgetNormal {
		t.Errorf("Budget = %+v", b)
	}
}
