package state

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/config"
	"github.com/gravexa-llc/ai-manager/internal/plans"
	"github.com/gravexa-llc/ai-manager/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), store.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// registerMini registers the mini plan the way the plan service does.
func registerMini(t *testing.T, st *store.Store) int64 {
	t.Helper()
	ctx := context.Background()
	parsed := plans.Parse([]byte(miniPlan))
	id, err := st.InsertPlan(ctx, store.Plan{Slug: plans.Slug("x-plan.md", []byte(miniPlan)), Title: parsed.Title,
		SourceMD: miniPlan, Repos: []string{repo}, CreatedAt: registered})
	must(t, err)
	must(t, st.SyncPackages(ctx, id, StorePackages(parsed)))
	return id
}

func TestRun(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	plan := registerMini(t, st)
	must(t, st.UpsertPRs(ctx, pr(1, "aim/a1-x", "One"), pr(2, "feature/b1-y", "Store (B1)"), pr(3, "chore/deps", "Bump")))
	must(t, st.UpsertWorktrees(ctx, store.Worktree{Path: wtRoot + "c1", MainRepo: repoPath, Branch: "aim/c1-z", FirstSeen: registered.Add(time.Hour), LastSeen: now}))
	names := map[string]string{repoPath: repo}

	states, err := Run(ctx, st, now, names, Options{})
	must(t, err)
	ps := states[0]
	deepEq(t, "lifecycles", lifecycles(ps), map[Lifecycle][]string{
		InReview: {"A1", "B1"},
		Blocked:  {"A2", "R1"},
		Ready:    {"B2", "B3b-2", "C1", "C2", "Y1"},
	})
	deepEq(t, "unmatched", unmatchedNotes(ps), map[string]string{prRef(3): ""})

	// The matcher's links are stored, inferred, and the board carries their ids.
	links, err := st.ListLinks(ctx, plan)
	must(t, err)
	var got []string
	for _, l := range links {
		if !l.Inferred {
			t.Errorf("link not inferred: %+v", l)
		}
		got = append(got, l.PkgID+" "+l.Kind+" "+l.Ref+" "+l.MatchMethod)
	}
	deepEq(t, "stored links", got, []string{
		"A1 pr example-org/app#1 branch",
		"B1 pr example-org/app#2 title",
		"C1 branch example-org/app:aim/c1-z branch",
	})
	if a1 := pkgOf(t, ps, "A1"); a1.Links[0].ID != links[0].ID {
		t.Errorf("A1's link id = %d, want %d", a1.Links[0].ID, links[0].ID)
	}
	// So are the lifecycles.
	pkgs, err := st.ListPackages(ctx, plan)
	must(t, err)
	for _, p := range pkgs {
		if p.Lifecycle == "" || !p.LifecycleAt.Equal(now) {
			t.Errorf("%s lifecycle %q at %v", p.PkgID, p.Lifecycle, p.LifecycleAt)
		}
	}

	// A1 merges: A2 and R1 unlock. B1's link: you remove it.
	must(t, st.UpsertPRs(ctx, merged(pr(1, "aim/a1-x", "One"))))
	must(t, st.Unlink(ctx, store.Tombstone{PlanID: plan, PkgID: "B1", Kind: store.LinkPR, Ref: prRef(2)}))
	later := now.Add(time.Hour)
	states, err = Run(ctx, st, later, names, Options{UnlockedSince: now})
	must(t, err)
	ps = states[0]
	deepEq(t, "lifecycles after the merge", lifecycles(ps), map[Lifecycle][]string{
		Done:  {"A1"},
		Ready: {"A2", "B1", "B2", "B3b-2", "C1", "C2", "Y1", "R1"},
	})
	deepEq(t, "alerts", alertKinds(ps), []string{"unlocked"})
	deepEq(t, "unlocked", ps.Alerts[0].Packages, []string{"A2", "R1"})
	deepEq(t, "unmatched", unmatchedNotes(ps), map[string]string{prRef(2): "you unlinked it from aim · B1", prRef(3): ""})
	pkgs, err = st.ListPackages(ctx, plan)
	must(t, err)
	for _, p := range pkgs {
		if p.PkgID == "A2" && (p.Lifecycle != "ready" || p.LifecyclePrev != "blocked" || !p.LifecycleAt.Equal(later)) {
			t.Errorf("A2 stored as %+v", p)
		}
		if p.PkgID == "B2" && !p.LifecycleAt.Equal(now) {
			t.Errorf("B2's unchanged lifecycle moved: %+v", p)
		}
	}
	links, err = st.ListLinks(ctx, plan)
	must(t, err)
	if len(links) != 2 {
		t.Errorf("links after the unlink: %+v", links)
	}

	// Linking it by hand brings it back as yours.
	_, err = st.UpsertLink(ctx, store.Link{PlanID: plan, PkgID: "C2", Kind: store.LinkPR, Ref: prRef(2), MatchMethod: store.MatchManual, Confidence: store.ConfidenceExact})
	must(t, err)
	states, err = Run(ctx, st, later, names, Options{})
	must(t, err)
	if c2 := pkgOf(t, states[0], "C2"); c2.Lifecycle != InReview || c2.Links[0].Method != store.MatchManual {
		t.Errorf("C2 = %s, %+v", c2.Lifecycle, c2.Links)
	}
	if b1 := pkgOf(t, states[0], "B1"); len(b1.Links) != 0 {
		t.Errorf("B1 links = %+v", b1.Links)
	}
}

// Run reads what the store keeps from GitHub: each repo's default branch,
// and when each PR was opened.
func TestRunReadsGitHubFacts(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	registerMini(t, st)
	names := map[string]string{repoPath: repo}
	// Opened before the plan was registered, though aimd stored it after.
	must(t, st.UpsertPRs(ctx, withBase(opened(pr(1, "share/c2-thing", "No ID"), registered.Add(-time.Hour)), "trunk")))
	must(t, st.UpsertWorktrees(ctx,
		store.Worktree{Path: repoPath, MainRepo: repoPath, Branch: "main", FirstSeen: registered, LastSeen: now},
		store.Worktree{Path: wtRoot + "dev", MainRepo: repoPath, Branch: "develop", FirstSeen: registered, LastSeen: now},
	))
	unmatched := func() map[string]string {
		t.Helper()
		states, err := Run(ctx, st, now, names, Options{})
		must(t, err)
		return unmatchedNotes(states[0])
	}
	tooOld := "its branch names C2 only as a token, and the branch was first seen before plan aim was registered"
	deepEq(t, "unmatched without a default branch", unmatched(), map[string]string{prRef(1): tooOld})

	must(t, st.UpsertRepos(ctx, store.Repo{Name: repo, DefaultBranch: "trunk"}))
	deepEq(t, "unmatched with trunk the default", unmatched(), map[string]string{
		prRef(1):                         tooOld,
		store.BranchRef(repo, "main"):    "",
		store.BranchRef(repo, "develop"): "",
	})
}

// Run records every lifecycle change in the store's history, and keeps the
// stalled and lane-collision alerts raised and cleared over time (the
// plan's timeline, D3).
func TestRunRecordsHistoryAndAlerts(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	plan := registerMini(t, st)
	names := map[string]string{repoPath: repo}
	// C1's branch has commits, no agent and no PR: stalled after an hour.
	wt := store.Worktree{Path: wtRoot + "c1", MainRepo: repoPath, Branch: "aim/c1-z", UnpushedCount: 2,
		LastCommitAt: registered.Add(2 * time.Hour), LastSeen: now}
	must(t, st.UpsertWorktrees(ctx, wt))
	opts := Options{StalledAfter: time.Hour}

	_, err := Run(ctx, st, now, names, opts)
	must(t, err)
	h, err := st.ListLifecycleHistory(ctx, plan, store.HistoryFilter{})
	must(t, err)
	if len(h) != 9 {
		t.Fatalf("history after the first pass has %d rows, want one per package: %+v", len(h), h)
	}
	for _, c := range h {
		if c.Prev != "" || !c.At.Equal(now) {
			t.Errorf("first derivation recorded as %+v", c)
		}
	}
	alerts, err := st.ListPlanAlerts(ctx, plan, store.HistoryFilter{})
	must(t, err)
	if len(alerts) != 1 || alerts[0].Kind != string(AlertStalled) || alerts[0].Key != "C1" || !alerts[0].RaisedAt.Equal(now) || !alerts[0].ClearedAt.IsZero() {
		t.Fatalf("alerts after the first pass = %+v", alerts)
	}

	// A pass that changes nothing records nothing.
	_, err = Run(ctx, st, now.Add(time.Minute), names, opts)
	must(t, err)
	h2, err := st.ListLifecycleHistory(ctx, plan, store.HistoryFilter{})
	must(t, err)
	if len(h2) != len(h) {
		t.Errorf("an idle pass added history: %+v", h2[:len(h2)-len(h)])
	}

	// A1's PR merges and C1 gets a PR: A2 and R1 unlock, A1 is done, C1 in
	// review, and the stall clears.
	later := now.Add(time.Hour)
	must(t, st.UpsertPRs(ctx, merged(pr(1, "aim/a1-x", "One")), pr(4, "aim/c1-z", "Other one (C1)")))
	_, err = Run(ctx, st, later, names, opts)
	must(t, err)
	h, err = st.ListLifecycleHistory(ctx, plan, store.HistoryFilter{})
	must(t, err)
	var got []string
	for _, c := range h {
		if c.At.Equal(later) {
			got = append(got, c.PkgID+" "+c.Prev+" → "+c.Lifecycle)
		}
	}
	slices.Sort(got)
	deepEq(t, "changes recorded", got, []string{"A1 ready → done", "A2 blocked → ready", "C1 working → in_review", "R1 blocked → ready"})
	alerts, err = st.ListPlanAlerts(ctx, plan, store.HistoryFilter{})
	must(t, err)
	if len(alerts) != 1 || !alerts[0].ClearedAt.Equal(later) {
		t.Errorf("alerts after the stall ended = %+v", alerts)
	}
}

func TestRunEmptyStore(t *testing.T) {
	states, err := Run(context.Background(), openStore(t), now, nil, Options{})
	must(t, err)
	if len(states) != 0 {
		t.Errorf("states = %+v", states)
	}
}

func TestRunClosedStore(t *testing.T) {
	st := openStore(t)
	registerMini(t, st)
	st.Close()
	if _, err := Run(context.Background(), st, now, nil, Options{}); err == nil {
		t.Error("Run on a closed store succeeded")
	}
}

func TestOptionsFromConfig(t *testing.T) {
	cfg := config.Default()
	if got := OptionsFromConfig(cfg).StalledAfter; got != 24*time.Hour {
		t.Errorf("StalledAfter = %s", got)
	}
	cfg.State.QuietAfter = 20 * time.Minute
	if got := OptionsFromConfig(cfg).QuietAfter; got != 20*time.Minute {
		t.Errorf("QuietAfter = %s, want config's state.quiet_after", got)
	}
	if o := (Options{}).withDefaults(); o.StalledAfter != DefaultStalledAfter || o.QuietAfter != DefaultQuietAfter {
		t.Errorf("defaults = %+v", o)
	}
}

func TestStorePackages(t *testing.T) {
	got := StorePackages(plans.Parse([]byte(miniPlan)))
	r1 := got[8]
	if r1.PkgID != "R1" || r1.Seq != 8 || r1.NeedsYou != "run the check" || r1.NeedsNote != "you: run the check" || len(r1.Needs) != 1 {
		t.Errorf("R1 = %+v", r1)
	}
	if y1 := got[7]; y1.Owner != "you" {
		t.Errorf("Y1 = %+v", y1)
	}
	if b1 := got[2]; b1.Lane != "Store" {
		t.Errorf("B1 = %+v", b1)
	}
}
