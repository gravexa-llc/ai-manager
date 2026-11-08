package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// What is a parser field like the title: every write sets it, and a
// re-parse rewrites it.
func TestPackageWhat(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	plan := newPlan(t, s, "search-v2")

	must(t, s.InsertPackage(ctx, Package{PlanID: plan, PkgID: "A1", Title: "Index builder", What: "Builds the `search` index."}))
	got, err := s.GetPackage(ctx, plan, "a1")
	must(t, err)
	equal(t, "inserted", got.What, "Builds the `search` index.")

	must(t, s.SyncPackages(ctx, plan, []Package{
		{PkgID: "A1", Title: "Index builder", What: "Builds the index **nightly**."},
		{PkgID: "A2", Title: "Query API"},
	}))
	pkgs, err := s.ListPackages(ctx, plan)
	must(t, err)
	equal(t, "after a re-parse", []string{pkgs[0].What, pkgs[1].What}, []string{"Builds the index **nightly**.", ""})

	must(t, s.UpdatePackage(ctx, Package{PlanID: plan, PkgID: "A2", Title: "Query API", What: "Answers queries."}))
	got, err = s.GetPackage(ctx, plan, "A2")
	must(t, err)
	equal(t, "updated", got.What, "Answers queries.")
}

// Hiding a target from a plan's Unmatched lane is per plan, keeps its
// first time, and goes with the plan.
func TestHiddenUnmatched(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	plan := newPlan(t, s, "search-v2")
	other := newPlan(t, s, "web")

	pr := PRRef("acme/app", 45)
	br := BranchRef("acme/app", "fix/readme")
	must(t, s.HideUnmatched(ctx, HiddenUnmatched{PlanID: plan, Kind: LinkPR, Ref: pr, HiddenAt: at(time.Minute)}))
	must(t, s.HideUnmatched(ctx, HiddenUnmatched{PlanID: plan, Kind: LinkPR, Ref: pr, HiddenAt: at(time.Hour)})) // again: kept
	must(t, s.HideUnmatched(ctx, HiddenUnmatched{PlanID: plan, Kind: LinkBranch, Ref: br, HiddenAt: at(2 * time.Minute)}))
	got, err := s.ListHiddenUnmatched(ctx, plan)
	must(t, err)
	equal(t, "hidden", got, []HiddenUnmatched{
		{PlanID: plan, Kind: LinkBranch, Ref: br, HiddenAt: at(2 * time.Minute)},
		{PlanID: plan, Kind: LinkPR, Ref: pr, HiddenAt: at(time.Minute)},
	})
	if got, err := s.ListHiddenUnmatched(ctx, other); err != nil || len(got) != 0 {
		t.Errorf("the other plan's hidden = %+v, %v", got, err)
	}

	if err := s.HideUnmatched(ctx, HiddenUnmatched{PlanID: 999, Kind: LinkPR, Ref: pr}); !errors.Is(err, ErrNotFound) {
		t.Errorf("hide on no plan = %v, want ErrNotFound", err)
	}
	if err := s.HideUnmatched(ctx, HiddenUnmatched{PlanID: plan, Kind: LinkPR}); err == nil {
		t.Error("hide with no ref: no error")
	}

	was, err := s.UnhideUnmatched(ctx, plan, LinkPR, pr)
	must(t, err)
	again, err := s.UnhideUnmatched(ctx, plan, LinkPR, pr)
	must(t, err)
	if !was || again {
		t.Errorf("unhide = %v then %v, want true then false", was, again)
	}

	must(t, s.DeletePlan(ctx, plan))
	got, err = s.ListHiddenUnmatched(ctx, plan)
	must(t, err)
	if len(got) != 0 {
		t.Errorf("hidden after the plan went = %+v", got)
	}
}
