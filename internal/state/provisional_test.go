package state

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/plans"
	"github.com/gravexa-llc/ai-manager/internal/store"
)

// planLine gives a PR a Plan: line.
func planLine(p store.PR, slug, pkg string) store.PR {
	p.PlanSlug, p.PlanPkg = slug, pkg
	return p
}

func sightings(ss []store.Sighting) []string {
	var out []string
	for _, s := range ss {
		out = append(out, s.PkgID+" "+s.Kind+" "+s.Ref+" · "+s.Title)
	}
	return out
}

// Unknown finds each source of a provisional package, and nothing else.
func TestUnknown(t *testing.T) {
	in := baseInput(t)
	gone := worktree("old", "aim/w20-gone")
	gone.RemovedAt = registered.Add(3 * time.Hour)
	other := mdPlan(t, 2, "# Other\n\nPlan slug: other\n\n| Package | Needs |\n|---|---|\n| **O1 One** | — |\n| **O2 Two** | — |\n", registered, "example-org/elsewhere")
	in.Plans = append(in.Plans, other)
	in.PRs = []store.PR{
		// Sources.
		planLine(pr(1, "aim/w12-unregister", "Unregister from the plan page (W12)"), "aim", "W12"),
		planLine(pr(2, "feature/c8", "Unknown packages (C8)"), "AIM", "c8"),
		// Not: an ID the file has; another plan's slug; an unregistered
		// slug; done work; a PR in a repo the plan isn't for; no Plan:
		// line and a branch not under the slug.
		planLine(pr(3, "aim/w13-x", "x"), "aim", "A1"),
		planLine(pr(4, "feature/z", "z"), "other", "O9"),
		planLine(pr(5, "feature/q", "q"), "nope", "Q1"),
		planLine(merged(pr(6, "aim/w14-merged", "merged")), "aim", "W14"),
		planLine(closed(pr(7, "aim/w15-closed", "closed")), "aim", "W15"),
		{Repo: "example-org/elsewhere", Number: 8, HeadBranch: "aim/w16-x", BaseRef: "main", State: store.PROpen, PlanSlug: "aim", PlanPkg: "W16"},
		pr(9, "fix/w17-typo", "Typo"),
		// Source: the head branch of an open PR without a Plan: line.
		pr(10, "aim/x4-2-fix", "Fix it"),
	}
	in.Worktrees = append(in.Worktrees,
		// Source: a branch at work with no PR.
		worktree("a", "aim/d9-new-dashboard-bit"),
		// Not: a branch whose worktree is gone; the branch of a merged
		// PR; one of a PR whose Plan: line has the say (W12's own branch is
		// left to it, A1's names a file's package); branches the file has
		// (matched); not under the slug; not an ID; another repo's.
		worktree("b", "aim/b3b-2-fix"),
		gone,
		worktree("c", "aim/w14-merged"),
		worktree("d", "aim/w12-unregister"),
		worktree("e", "aim/w13-x"),
		worktree("f", "aim/c1-other-one"),
		worktree("g", "chore/x2-thing"),
		worktree("h", "aim/notes"),
	)
	in.Worktrees = append(in.Worktrees, store.Worktree{Path: "/work/else/.claude/worktrees/x", MainRepo: "/work/else", Branch: "aim/w18-x", FirstSeen: registered, LastSeen: now})
	in.RepoNames["/work/else"] = "example-org/elsewhere"
	got := sightings(Unknown(in, Match(in)))
	deepEq(t, "sightings", got, []string{
		"W12 pr example-org/app#1 · Unregister from the plan page",
		"C8 pr example-org/app#2 · Unknown packages",
		"D9 branch example-org/app:aim/d9-new-dashboard-bit · New dashboard bit",
		"X4-2 branch example-org/app:aim/x4-2-fix · Fix",
	})

	// A branch you linked by hand to a file's package is that package's.
	in.Links = []store.Link{link("C2", store.LinkBranch, "example-org/app:aim/d9-new-dashboard-bit", store.MatchManual, store.ConfidenceExact)}
	got = sightings(Unknown(in, Match(in)))
	if slices.ContainsFunc(got, func(s string) bool { return s[:2] == "D9" }) {
		t.Errorf("a branch linked to C2 by hand is still a sighting: %v", got)
	}

	// Once provisional, a package is still sighted, so its sources stay
	// fresh; its sightings carry the snapshot's time.
	in = baseInput(t)
	in.Plans[0].Packages = append(in.Plans[0].Packages, store.Package{PlanID: 1, PkgID: "W12", Title: "Unregister", Provisional: true})
	in.PRs = []store.PR{planLine(pr(1, "aim/w12-unregister", "Unregister (W12)"), "aim", "W12")}
	ss := Unknown(in, Match(in))
	if len(ss) != 1 || ss[0].PkgID != "W12" || !ss[0].At.Equal(now) {
		t.Errorf("sightings of a provisional package: %+v", ss)
	}
}

func TestBranchID(t *testing.T) {
	for _, tc := range []struct{ rest, id, title string }{
		{"w12-unregister", "W12", "Unregister"},
		{"c8-unknown-packages", "C8", "Unknown packages"},
		{"b3b-2-fix", "B3B-2", "Fix"},
		{"sec-1-auth", "SEC-1", "Auth"},
		{"m6-flaky-gh-test", "M6", "Flaky gh test"},
		{"w12", "W12", ""},
		{"w12-x/more", "W12", "X"},
		{"notes", "", ""},
		{"", "", ""},
		{"12-x", "", ""},
	} {
		id, title := branchID(tc.rest)
		if id != tc.id || title != tc.title {
			t.Errorf("branchID(%q) = %q, %q; want %q, %q", tc.rest, id, title, tc.id, tc.title)
		}
	}
	for in, want := range map[string]string{
		"W12 unregister button":   "Unregister button",
		"W12: unregister":         "Unregister",
		"unregister button (W12)": "Unregister button",
		"mod acting":              "Mod acting",
		"W12":                     "",
		"W120 is another":         "W120 is another",
	} {
		if got := ProvisionalTitle("W12", in); got != want {
			t.Errorf("ProvisionalTitle(W12, %q) = %q, want %q", in, got, want)
		}
	}
}

// A pass makes the provisional package, the matcher links its work, and
// the board shows it, never ready to dispatch. When the file gains the
// row, the package becomes the file's with its links.
func TestRunProvisional(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	plan := registerMini(t, st)
	names := map[string]string{repoPath: repo}
	must(t, st.UpsertWorktrees(ctx, worktree("w12", "aim/w12-unregister"), worktree("d9", "aim/d9-later")))
	must(t, st.UpsertPRs(ctx, planLine(pr(66, "aim/w12-unregister", "Unregister from the plan page (W12)"), "aim", "W12")))

	states, err := Run(ctx, st, now, names, Options{})
	must(t, err)
	ps := states[0]
	w12, d9 := pkgOf(t, ps, "W12"), pkgOf(t, ps, "D9")
	if !w12.Provisional || w12.Lifecycle != InReview || w12.Title != "Unregister from the plan page" {
		t.Errorf("W12 = %+v", w12)
	}
	deepEq(t, "W12's links", refs(w12), []string{"pr example-org/app#66", "branch example-org/app:aim/w12-unregister < example-org/app#66",
		"worktree " + wtRoot + "w12 < example-org/app:aim/w12-unregister"})
	if !d9.Provisional || d9.Lifecycle != Ready || d9.Title != "Later" {
		t.Errorf("D9 = %+v", d9)
	}
	if slices.Contains(ps.ReadyToDispatch, "D9") {
		t.Errorf("a provisional package is ready to dispatch: %v", ps.ReadyToDispatch)
	}
	if len(ps.Unmatched) != 0 {
		t.Errorf("unmatched: %+v", ps.Unmatched)
	}
	pkgs, err := st.ListPackages(ctx, plan)
	must(t, err)
	if last := pkgs[len(pkgs)-1]; last.PkgID != "D9" || len(last.Sources) != 1 || last.Sources[0].Kind != store.ProvisionalBranch || !last.FirstSeen.Equal(now) {
		t.Errorf("D9 stored as %+v", last)
	}

	// The plan file gains W12's row: it is the file's package now, with
	// its links; D9 stays provisional.
	md := miniPlan + "| **W12 Unregister** | | A1 |\n"
	must(t, st.SyncPackages(ctx, plan, StorePackages(plans.Parse([]byte(md)))))
	p, err := st.GetPlan(ctx, plan)
	must(t, err)
	p.SourceMD = md
	must(t, st.UpdatePlan(ctx, p))
	states, err = Run(ctx, st, now.Add(time.Minute), names, Options{})
	must(t, err)
	w12 = pkgOf(t, states[0], "W12")
	if w12.Provisional || w12.Lifecycle != InReview || w12.Title != "Unregister" || len(w12.Links) != 3 || !slices.Equal(w12.Needs, []string{"A1"}) {
		t.Errorf("W12 after its row landed = %+v", w12)
	}
	if !pkgOf(t, states[0], "D9").Provisional {
		t.Error("D9 stopped being provisional")
	}
}

// A provisional package with no work left goes after ProvisionalQuiet;
// one with work, or a merged PR, stays.
func TestRunExpiresProvisional(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	plan := registerMini(t, st)
	names := map[string]string{repoPath: repo}
	d9, w12 := worktree("d9", "aim/d9-later"), worktree("w12", "aim/w12-unregister")
	must(t, st.UpsertWorktrees(ctx, d9, w12))
	must(t, st.UpsertPRs(ctx, planLine(pr(66, "aim/w12-unregister", "Unregister (W12)"), "aim", "W12")))
	_, err := Run(ctx, st, now, names, Options{})
	must(t, err)
	// A spawn-only package: an agent that never left a trace.
	_, err = st.SeeProvisional(ctx, []store.Sighting{{PlanID: plan, PkgID: "E7", Kind: store.ProvisionalSpawn, Ref: "s1/a1", Title: "Gone agent", At: now}})
	must(t, err)

	// Both branches' worktrees go; W12's PR merges.
	must(t, st.MarkWorktreeRemoved(ctx, d9.Path, now.Add(time.Hour)))
	must(t, st.MarkWorktreeRemoved(ctx, w12.Path, now.Add(time.Hour)))
	must(t, st.UpsertPRs(ctx, planLine(merged(pr(66, "aim/w12-unregister", "Unregister (W12)")), "aim", "W12")))
	for _, step := range []struct {
		at   time.Time
		want []string
	}{
		{now.Add(2 * time.Hour), []string{"D9", "E7", "W12"}},
		{now.Add(ProvisionalQuiet - time.Minute), []string{"D9", "E7", "W12"}},
		{now.Add(ProvisionalQuiet + 2*time.Hour), []string{"W12"}},
		{now.Add(10 * ProvisionalQuiet), []string{"W12"}},
	} {
		states, err := Run(ctx, st, step.at, names, Options{})
		must(t, err)
		var prov []string
		for _, p := range states[0].Packages {
			if p.Provisional {
				prov = append(prov, p.ID)
			}
		}
		slices.Sort(prov)
		deepEq(t, "provisional at "+step.at.Sub(now).String(), prov, step.want)
	}
	pkgs, err := st.ListPackages(ctx, plan)
	must(t, err)
	if len(pkgs) != len(plans.Parse([]byte(miniPlan)).Packages)+1 {
		t.Errorf("the store kept %d packages", len(pkgs))
	}
}
