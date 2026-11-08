package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPlanFileDismissals(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	const path = "/repos/pivot/docs/cache-plan.md"

	if _, ok, err := s.PlanFileDismissed(ctx, path); err != nil || ok {
		t.Fatalf("PlanFileDismissed before = %v, %v; want false, nil", ok, err)
	}
	got, err := s.DismissPlanFile(ctx, path, at(-time.Hour))
	must(t, err)
	equal(t, "dismissed at", got, at(-time.Hour))

	// Dismissing again keeps the first time; the path ignores case.
	got, err = s.DismissPlanFile(ctx, "/REPOS/pivot/docs/cache-plan.md", time.Time{})
	must(t, err)
	equal(t, "dismissed again at", got, at(-time.Hour))
	when, ok, err := s.PlanFileDismissed(ctx, "/repos/PIVOT/docs/cache-plan.md")
	must(t, err)
	if !ok || !when.Equal(at(-time.Hour)) {
		t.Errorf("PlanFileDismissed = %v, %v; want true at %v", when, ok, at(-time.Hour))
	}

	must(t, s.UndismissPlanFile(ctx, path))
	must(t, s.UndismissPlanFile(ctx, path)) // twice is fine
	if _, ok, err := s.PlanFileDismissed(ctx, path); err != nil || ok {
		t.Errorf("PlanFileDismissed after undismiss = %v, %v; want false, nil", ok, err)
	}
	if _, err := s.DismissPlanFile(ctx, "", time.Time{}); err == nil {
		t.Error("DismissPlanFile with an empty path: no error")
	}
}

func TestGetPlanBySourcePathAndPackage(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	id, err := s.InsertPlan(ctx, Plan{Slug: "sharing", SourcePath: "/repos/pivot/docs/sharing-plan.md"})
	must(t, err)
	must(t, s.SyncPackages(ctx, id, []Package{{PkgID: "A1", Title: "String Catalog"}, {PkgID: "A2", Needs: []string{"A1"}}}))

	p, err := s.GetPlanBySourcePath(ctx, "/Repos/PIVOT/docs/sharing-plan.md")
	must(t, err)
	if p.ID != id {
		t.Errorf("GetPlanBySourcePath = plan %d, want %d", p.ID, id)
	}
	if _, err := s.GetPlanBySourcePath(ctx, "/elsewhere.md"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetPlanBySourcePath(unknown) = %v, want ErrNotFound", err)
	}

	pkg, err := s.GetPackage(ctx, id, "a1")
	must(t, err)
	if pkg.PkgID != "A1" || pkg.Title != "String Catalog" || pkg.DoneManual {
		t.Errorf("GetPackage = %+v", pkg)
	}
	if _, err := s.GetPackage(ctx, id, "Z9"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetPackage(unknown) = %v, want ErrNotFound", err)
	}

	// SetDoneManual changes only done_manual, and SyncPackages keeps it.
	must(t, s.SetDoneManual(ctx, id, "a1", true))
	must(t, s.SyncPackages(ctx, id, []Package{{PkgID: "A1", Title: "String Catalogs"}, {PkgID: "A2", Needs: []string{"A1"}}}))
	pkg, err = s.GetPackage(ctx, id, "A1")
	must(t, err)
	if !pkg.DoneManual || pkg.Title != "String Catalogs" {
		t.Errorf("after SetDoneManual and a sync: %+v", pkg)
	}
	must(t, s.SetDoneManual(ctx, id, "A1", false))
	if pkg, _ = s.GetPackage(ctx, id, "A1"); pkg.DoneManual {
		t.Error("SetDoneManual(false) left it done")
	}
	if err := s.SetDoneManual(ctx, id, "Z9", true); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetDoneManual(unknown) = %v, want ErrNotFound", err)
	}
}
