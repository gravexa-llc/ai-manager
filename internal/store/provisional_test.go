package store

import (
	"context"
	"testing"
	"time"
)

// provisionalPlan stores a plan with the packages A1 and A2 from its file.
func provisionalPlan(t *testing.T, s *Store) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := s.InsertPlan(ctx, Plan{Slug: "aim", Repos: []string{"example-org/ai-manager"}})
	must(t, err)
	must(t, s.SyncPackages(ctx, id, []Package{{PkgID: "A1", Title: "One"}, {PkgID: "A2", Title: "Two", Needs: []string{"A1"}}}))
	return id
}

func packageIDs(t *testing.T, s *Store, planID int64) (ids []string, provisional []string) {
	t.Helper()
	pkgs, err := s.ListPackages(context.Background(), planID)
	must(t, err)
	for _, p := range pkgs {
		ids = append(ids, p.PkgID)
		if p.Provisional {
			provisional = append(provisional, p.PkgID)
		}
	}
	return ids, provisional
}

func TestSeeProvisional(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	plan := provisionalPlan(t, s)

	// A branch names W12 first: a new provisional package, after the
	// file's rows, titled from the branch.
	n, err := s.SeeProvisional(ctx, []Sighting{
		{PlanID: plan, PkgID: "W12", Kind: ProvisionalBranch, Ref: "example-org/ai-manager:aim/w12-unregister", Title: "Unregister", At: at(time.Minute)},
		// The file's own package is left alone.
		{PlanID: plan, PkgID: "a1", Kind: ProvisionalBranch, Ref: "example-org/ai-manager:aim/a1-x", Title: "X", At: at(time.Minute)},
		// A plan that is gone is skipped.
		{PlanID: plan + 99, PkgID: "Z1", Kind: ProvisionalPR, Ref: "example-org/ai-manager#9", At: at(time.Minute)},
	})
	must(t, err)
	if n != 1 {
		t.Errorf("SeeProvisional made %d packages, want 1", n)
	}
	ids, prov := packageIDs(t, s, plan)
	equal(t, "packages", ids, []string{"A1", "A2", "W12"})
	equal(t, "provisional", prov, []string{"W12"})

	// A PR names it too: a better title, a second source, active later.
	// Then the spawn: the best title.
	_, err = s.SeeProvisional(ctx, []Sighting{{PlanID: plan, PkgID: "w12", Kind: ProvisionalPR, Ref: "example-org/ai-manager#66", Title: "Unregister from the plan page", At: at(5 * time.Minute)}})
	must(t, err)
	got, err := s.GetPackage(ctx, plan, "W12")
	must(t, err)
	if got.Title != "Unregister from the plan page" || !got.Provisional || !got.FirstSeen.Equal(at(time.Minute)) || !got.ActiveAt.Equal(at(5*time.Minute)) {
		t.Errorf("after the PR: %+v", got)
	}
	_, err = s.SeeProvisional(ctx, []Sighting{
		{PlanID: plan, PkgID: "W12", Kind: ProvisionalSpawn, Ref: "s1/a1", Title: "W12 unregister button", At: at(3 * time.Minute)},
		// A branch seen again keeps the better title.
		{PlanID: plan, PkgID: "W12", Kind: ProvisionalBranch, Ref: "example-org/ai-manager:aim/w12-unregister", Title: "Unregister", At: at(6 * time.Minute)},
	})
	must(t, err)
	pkgs, err := s.ListPackages(ctx, plan)
	must(t, err)
	w12 := pkgs[2]
	if w12.Title != "W12 unregister button" || !w12.ActiveAt.Equal(at(6*time.Minute)) {
		t.Errorf("after the spawn: %+v", w12)
	}
	equal(t, "sources", w12.Sources, []ProvisionalSource{
		{Kind: ProvisionalBranch, Ref: "example-org/ai-manager:aim/w12-unregister", Title: "Unregister", FirstSeen: at(time.Minute), LastSeen: at(6 * time.Minute)},
		{Kind: ProvisionalSpawn, Ref: "s1/a1", Title: "W12 unregister button", FirstSeen: at(3 * time.Minute), LastSeen: at(3 * time.Minute)},
		{Kind: ProvisionalPR, Ref: "example-org/ai-manager#66", Title: "Unregister from the plan page", FirstSeen: at(5 * time.Minute), LastSeen: at(5 * time.Minute)},
	})

	// A second provisional package goes after the first.
	_, err = s.SeeProvisional(ctx, []Sighting{{PlanID: plan, PkgID: "C8", Kind: ProvisionalPR, Ref: "example-org/ai-manager#67", At: at(7 * time.Minute)}})
	must(t, err)
	ids, _ = packageIDs(t, s, plan)
	equal(t, "packages", ids, []string{"A1", "A2", "W12", "C8"})

	// Bad sightings fail the call.
	if _, err := s.SeeProvisional(ctx, []Sighting{{PlanID: plan, PkgID: "X1", Kind: "guess", Ref: "r"}}); err == nil {
		t.Error("an unknown source kind was accepted")
	}
	if _, err := s.SeeProvisional(ctx, []Sighting{{PlanID: plan, PkgID: "X1", Kind: ProvisionalPR}}); err == nil {
		t.Error("a sighting without a ref was accepted")
	}
}

// When the plan file gains the row, the provisional package becomes the
// file's: its links, Mark done and history stay, and it is no longer
// provisional. The other provisional packages stay as they are.
func TestSyncPackagesMergesProvisional(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	plan := provisionalPlan(t, s)
	_, err := s.SeeProvisional(ctx, []Sighting{
		{PlanID: plan, PkgID: "W12", Kind: ProvisionalSpawn, Ref: "s1/a1", Title: "Unregister", At: at(time.Minute)},
		{PlanID: plan, PkgID: "C8", Kind: ProvisionalPR, Ref: "example-org/ai-manager#67", At: at(time.Minute)},
	})
	must(t, err)
	_, err = s.UpsertLink(ctx, Link{PlanID: plan, PkgID: "W12", Kind: LinkAgent, SessionID: "s1", AgentID: "a1", MatchMethod: MatchSpawn, Confidence: ConfidenceExact})
	must(t, err)
	_, err = s.ReplaceInferredLinks(ctx, plan, []Link{{PkgID: "W12", Kind: LinkPR, Ref: "example-org/ai-manager#66", MatchMethod: MatchPlanLine, Confidence: ConfidenceExact}})
	must(t, err)
	must(t, s.SetDoneManual(ctx, plan, "W12", true))
	must(t, s.SetLifecycles(ctx, plan, []PackageLifecycle{{PkgID: "W12", Lifecycle: "in_review", At: at(2 * time.Minute)}}))

	// A re-parse without the row keeps both provisional packages.
	must(t, s.SyncPackages(ctx, plan, []Package{{PkgID: "A1", Title: "One"}, {PkgID: "A2", Title: "Two"}}))
	_, prov := packageIDs(t, s, plan)
	equal(t, "provisional after a re-parse without the row", prov, []string{"W12", "C8"})

	// The row lands: the file's fields, the links, Mark done and the
	// lifecycle stay.
	must(t, s.SyncPackages(ctx, plan, []Package{{PkgID: "A1", Title: "One"}, {PkgID: "A2", Title: "Two"}, {PkgID: "W12", Title: "Unregister from the plan page", Size: "S", Needs: []string{"A1"}}}))
	ids, prov := packageIDs(t, s, plan)
	equal(t, "packages", ids, []string{"A1", "A2", "W12", "C8"})
	equal(t, "provisional", prov, []string{"C8"})
	got, err := s.GetPackage(ctx, plan, "w12")
	must(t, err)
	if got.Provisional || got.Title != "Unregister from the plan page" || got.Size != "S" || got.Seq != 2 || !got.DoneManual || got.Lifecycle != "in_review" {
		t.Errorf("merged W12: %+v", got)
	}
	links, err := s.ListLinks(ctx, plan)
	must(t, err)
	if len(links) != 2 || links[0].PkgID != "W12" || links[1].PkgID != "W12" {
		t.Errorf("links after the merge: %+v", links)
	}
	pkgs, err := s.ListPackages(ctx, plan)
	must(t, err)
	if len(pkgs[2].Sources) != 0 {
		t.Errorf("the merged package kept its sources: %+v", pkgs[2].Sources)
	}

	// The row goes again: an ordinary package of the file, so it goes
	// with its links.
	must(t, s.SyncPackages(ctx, plan, []Package{{PkgID: "A1", Title: "One"}, {PkgID: "A2", Title: "Two"}}))
	ids, _ = packageIDs(t, s, plan)
	equal(t, "packages after the row went", ids, []string{"A1", "A2", "C8"})
}

func TestProvisionalActiveAndExpire(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	plan := provisionalPlan(t, s)
	_, err := s.SeeProvisional(ctx, []Sighting{
		{PlanID: plan, PkgID: "W12", Kind: ProvisionalBranch, Ref: "example-org/ai-manager:aim/w12-x", At: at(time.Minute)},
		{PlanID: plan, PkgID: "C8", Kind: ProvisionalBranch, Ref: "example-org/ai-manager:aim/c8-y", At: at(time.Minute)},
	})
	must(t, err)
	_, err = s.UpsertLink(ctx, Link{PlanID: plan, PkgID: "W12", Kind: LinkBranch, Ref: "example-org/ai-manager:aim/w12-x", MatchMethod: MatchManual, Confidence: ConfidenceExact})
	must(t, err)

	must(t, s.SetProvisionalActive(ctx, plan, []string{"c8", "A1"}, at(time.Hour)))
	got, err := s.GetPackage(ctx, plan, "C8")
	must(t, err)
	if !got.ActiveAt.Equal(at(time.Hour)) {
		t.Errorf("C8 active at %v, want %v", got.ActiveAt, at(time.Hour))
	}
	// An earlier time never moves it back.
	must(t, s.SetProvisionalActive(ctx, plan, []string{"C8"}, at(time.Minute)))
	got, _ = s.GetPackage(ctx, plan, "C8")
	if !got.ActiveAt.Equal(at(time.Hour)) {
		t.Errorf("C8 moved back to %v", got.ActiveAt)
	}

	// Only what was quiet since before the cutoff goes, never a file's row.
	n, err := s.ExpireProvisional(ctx, plan, []string{"W12", "C8", "A1"}, at(30*time.Minute))
	must(t, err)
	if n != 1 {
		t.Errorf("expired %d, want 1 (W12)", n)
	}
	ids, prov := packageIDs(t, s, plan)
	equal(t, "packages", ids, []string{"A1", "A2", "C8"})
	equal(t, "provisional", prov, []string{"C8"})
	links, err := s.ListLinks(ctx, plan)
	must(t, err)
	if len(links) != 0 {
		t.Errorf("W12's links outlived it: %+v", links)
	}
	var sources int
	must(t, s.r.QueryRow(`SELECT count(*) FROM provisional_sources WHERE pkg_id = 'W12'`).Scan(&sources))
	if sources != 0 {
		t.Errorf("W12 kept %d sources", sources)
	}

	// Unregistering the plan takes its provisional packages along.
	must(t, s.DeletePlan(ctx, plan))
	var left int
	must(t, s.r.QueryRow(`SELECT (SELECT count(*) FROM provisional_packages) + (SELECT count(*) FROM provisional_sources)`).Scan(&left))
	if left != 0 {
		t.Errorf("%d provisional rows outlived the plan", left)
	}
}
