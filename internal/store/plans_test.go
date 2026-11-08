package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPlansRoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	in := Plan{
		Slug:                "sharing",
		Title:               "Family sharing",
		SourcePath:          "/repos/pivot/docs/plans/sharing-plan.md",
		SourceMD:            "# Sharing plan\n",
		Repos:               []string{"example-org/pivot", "example-org/web"},
		RegisteredInSession: "s1",
		Status:              "active",
		CreatedAt:           at(-time.Hour),
	}
	id, err := s.InsertPlan(ctx, in)
	must(t, err)
	want := in
	want.ID = id
	want.UpdatedAt = in.CreatedAt

	got, err := s.GetPlan(ctx, id)
	must(t, err)
	equal(t, "GetPlan", got, want)
	got, err = s.GetPlanBySlug(ctx, "SHARING")
	must(t, err)
	equal(t, "GetPlanBySlug", got, want)

	aim, err := s.InsertPlan(ctx, Plan{Slug: "aim", Repos: []string{"example-org/ai-manager"}}) // created now
	must(t, err)

	list, err := s.ListPlans(ctx, PlanFilter{})
	must(t, err)
	if len(list) != 2 || list[0].ID != id || list[1].ID != aim {
		t.Errorf("ListPlans = %+v, want sharing then aim", list)
	}
	byRepo, err := s.ListPlans(ctx, PlanFilter{Repo: "Example-Org/Web"})
	must(t, err)
	equal(t, "plans for example-org/web", byRepo, []Plan{want})
	none, err := s.ListPlans(ctx, PlanFilter{Repo: "example-org/nothing"})
	must(t, err)
	equal(t, "plans for an unknown repo", none, []Plan(nil))

	// Update rewrites everything but CreatedAt and stamps UpdatedAt.
	s.now = func() time.Time { return at(time.Hour) }
	want.Title = "Sharing (v2)"
	want.Repos = nil
	want.Status = "archived"
	want.CreatedAt = at(-24 * time.Hour) // ignored
	must(t, s.UpdatePlan(ctx, want))
	got, err = s.GetPlan(ctx, id)
	must(t, err)
	want.CreatedAt = in.CreatedAt
	want.UpdatedAt = at(time.Hour)
	equal(t, "plan after update", got, want)
}

func TestPlanErrors(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_, err := s.InsertPlan(ctx, Plan{Slug: "aim"})
	must(t, err)
	if _, err := s.InsertPlan(ctx, Plan{Slug: "AIM"}); err == nil {
		t.Error("a second plan with the same slug (other case) was accepted")
	}
	if _, err := s.InsertPlan(ctx, Plan{}); err == nil {
		t.Error("a plan with no slug was accepted")
	}
	if err := s.UpdatePlan(ctx, Plan{ID: 999, Slug: "x"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdatePlan on a missing plan: %v, want ErrNotFound", err)
	}
	if err := s.UpdatePlan(ctx, Plan{ID: 1}); err == nil {
		t.Error("UpdatePlan with no slug succeeded")
	}
	if _, err := s.GetPlan(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetPlan on a missing plan: %v, want ErrNotFound", err)
	}
	if _, err := s.GetPlanBySlug(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetPlanBySlug on a missing plan: %v, want ErrNotFound", err)
	}
}

// newPlan inserts a plan and returns its id.
func newPlan(t *testing.T, s *Store, slug string) int64 {
	t.Helper()
	id, err := s.InsertPlan(context.Background(), Plan{Slug: slug})
	must(t, err)
	return id
}

func TestPackagesRoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	plan := newPlan(t, s, "aim")

	b1 := Package{PlanID: plan, PkgID: "B1", Seq: 2, Title: "Store", Repo: "ai-manager", Size: "M", Needs: []string{"F1"}, Lane: "Store"}
	a1 := Package{PlanID: plan, PkgID: "A1", Seq: 1, Title: "Worktree scanner", Size: "M", Needs: []string{"F1", "S1"}, NeedsNote: "after review", Owner: "you", DoneInPlan: true, DoneManual: true, Note: "n"}
	must(t, s.InsertPackage(ctx, b1))
	must(t, s.InsertPackage(ctx, a1))

	got, err := s.ListPackages(ctx, plan)
	must(t, err)
	equal(t, "packages in plan order", got, []Package{a1, b1})

	b1.Title = "Store v2"
	b1.Needs = nil
	b1.DoneManual = true
	b1.Note = "marked done"
	must(t, s.UpdatePackage(ctx, Package{PlanID: plan, PkgID: "b1", Seq: b1.Seq, Title: b1.Title, Repo: b1.Repo, Size: b1.Size, Lane: b1.Lane, DoneManual: true, Note: b1.Note}))
	got, err = s.ListPackages(ctx, plan)
	must(t, err)
	equal(t, "packages after update", got, []Package{a1, b1})

	if err := s.InsertPackage(ctx, Package{PlanID: plan, PkgID: "b1"}); err == nil {
		t.Error("a duplicate package id (other case) was accepted")
	}
	if err := s.InsertPackage(ctx, Package{PlanID: 999, PkgID: "X1"}); err == nil {
		t.Error("a package of a missing plan was accepted")
	}
	if err := s.InsertPackage(ctx, Package{PlanID: plan}); err == nil {
		t.Error("a package with no id was accepted")
	}
	if err := s.UpdatePackage(ctx, Package{PlanID: plan, PkgID: "Z9"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdatePackage on a missing package: %v, want ErrNotFound", err)
	}
}

func TestSyncPackages(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	plan := newPlan(t, s, "aim")
	other := newPlan(t, s, "other")

	must(t, s.SyncPackages(ctx, plan, []Package{
		{PkgID: "A1", Title: "Scanner"},
		{PkgID: "b1", Title: "Store"},
		{PkgID: "C1", Title: "Parser"},
	}))
	must(t, s.SyncPackages(ctx, other, []Package{{PkgID: "A1", Title: "Other plan's A1"}}))

	// The dashboard marks A1 done and notes it; the matcher links B1 and C1.
	must(t, s.UpdatePackage(ctx, Package{PlanID: plan, PkgID: "A1", Title: "Scanner", DoneManual: true, Note: "by hand"}))
	b1Link, err := s.UpsertLink(ctx, Link{PlanID: plan, PkgID: "b1", Kind: LinkPR, Ref: PRRef("example-org/app", 7), MatchMethod: MatchPlanLine, Confidence: ConfidenceExact})
	must(t, err)
	_, err = s.UpsertLink(ctx, Link{PlanID: plan, PkgID: "C1", Kind: LinkBranch, Ref: "aim/c1-parser", MatchMethod: MatchBranch, Confidence: ConfidenceHigh})
	must(t, err)

	// A re-parse: new order, B1's case fixed, C1 gone, D1 new, A1 now
	// done in the file.
	must(t, s.SyncPackages(ctx, plan, []Package{
		{PkgID: "D1", Title: "New", Needs: []string{"B1"}},
		{PkgID: "B1", Title: "Store (renamed)", Size: "M"},
		{PkgID: "a1", Title: "Scanner", DoneInPlan: true},
	}))
	got, err := s.ListPackages(ctx, plan)
	must(t, err)
	equal(t, "packages after re-parse", got, []Package{
		{PlanID: plan, PkgID: "D1", Seq: 0, Title: "New", Needs: []string{"B1"}},
		{PlanID: plan, PkgID: "B1", Seq: 1, Title: "Store (renamed)", Size: "M"},
		{PlanID: plan, PkgID: "a1", Seq: 2, Title: "Scanner", DoneInPlan: true, DoneManual: true, Note: "by hand"},
	})

	// C1's link went with it; B1's stayed and follows the new spelling.
	links, err := s.ListLinks(ctx, plan)
	must(t, err)
	if len(links) != 1 || links[0].ID != b1Link || links[0].PkgID != "B1" {
		t.Errorf("links after re-parse = %+v, want only B1's PR link", links)
	}

	// The other plan is untouched.
	otherPkgs, err := s.ListPackages(ctx, other)
	must(t, err)
	if len(otherPkgs) != 1 || otherPkgs[0].Title != "Other plan's A1" {
		t.Errorf("other plan's packages = %+v", otherPkgs)
	}

	// An empty parse empties the plan.
	must(t, s.SyncPackages(ctx, plan, nil))
	got, err = s.ListPackages(ctx, plan)
	must(t, err)
	if len(got) != 0 {
		t.Errorf("packages after an empty sync = %+v", got)
	}
}

func TestSyncPackagesErrors(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	plan := newPlan(t, s, "aim")
	must(t, s.SyncPackages(ctx, plan, []Package{{PkgID: "A1"}}))

	if err := s.SyncPackages(ctx, 999, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("SyncPackages on a missing plan: %v, want ErrNotFound", err)
	}
	for name, pkgs := range map[string][]Package{
		"duplicate": {{PkgID: "B1"}, {PkgID: "b1"}},
		"empty id":  {{PkgID: "B1"}, {}},
	} {
		if err := s.SyncPackages(ctx, plan, pkgs); err == nil {
			t.Errorf("%s: SyncPackages succeeded", name)
		}
	}
	got, err := s.ListPackages(ctx, plan)
	must(t, err)
	if len(got) != 1 || got[0].PkgID != "A1" {
		t.Errorf("a refused sync changed the packages: %+v", got)
	}
}

func TestLinksRoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	plan := newPlan(t, s, "aim")
	must(t, s.SyncPackages(ctx, plan, []Package{{PkgID: "A1"}, {PkgID: "B1"}}))

	agent := Link{PlanID: plan, PkgID: "B1", Kind: LinkAgent, SessionID: "s1", AgentID: "a26d", MatchMethod: MatchSpawn, Confidence: ConfidenceExact, CreatedAt: at(0)}
	job := Link{PlanID: plan, PkgID: "B1", Kind: LinkAgent, JobID: "job-1", MatchMethod: MatchPlanLine, Confidence: ConfidenceExact, CreatedAt: at(time.Second)}
	branch := Link{PlanID: plan, PkgID: "A1", Kind: LinkBranch, Ref: "aim/a1-scanner", MatchMethod: MatchBranch, Confidence: ConfidenceHigh} // created now

	var err error
	agent.ID, err = s.UpsertLink(ctx, agent)
	must(t, err)
	job.ID, err = s.UpsertLink(ctx, job)
	must(t, err)
	branch.ID, err = s.UpsertLink(ctx, branch)
	must(t, err)
	branch.CreatedAt = t0
	// An agent link's ref is filled in from its ids.
	agent.Ref = "s1/a26d"
	job.Ref = "job:job-1"

	got, err := s.ListLinks(ctx, plan)
	must(t, err)
	equal(t, "links", got, []Link{branch, agent, job})

	// The same target again is the same link: method and confidence move,
	// id and CreatedAt stay. The package ID compares case-insensitively.
	again := agent
	again.PkgID = "b1"
	again.MatchMethod = MatchManual
	again.Confidence = ConfidenceLow
	again.CreatedAt = at(time.Hour)
	id, err := s.UpsertLink(ctx, again)
	must(t, err)
	if id != agent.ID {
		t.Errorf("re-upsert gave id %d, want %d", id, agent.ID)
	}
	agent.MatchMethod, agent.Confidence = MatchManual, ConfidenceLow

	// UpdateLink moves a link to another package and target.
	branch.PkgID = "B1"
	branch.Ref = "aim/b1-store"
	branch.Confidence = ConfidenceExact
	must(t, s.UpdateLink(ctx, branch))

	got, err = s.ListLinks(ctx, plan)
	must(t, err)
	equal(t, "links after updates", got, []Link{agent, job, branch})

	must(t, s.DeleteLink(ctx, job.ID))
	got, err = s.ListLinks(ctx, plan)
	must(t, err)
	equal(t, "links after delete", got, []Link{agent, branch})
	if err := s.DeleteLink(ctx, job.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second DeleteLink: %v, want ErrNotFound", err)
	}
	if err := s.UpdateLink(ctx, job); !errors.Is(err, ErrNotFound) {
		t.Errorf("UpdateLink on a deleted link: %v, want ErrNotFound", err)
	}
}

func TestLinkValidation(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	plan := newPlan(t, s, "aim")
	must(t, s.SyncPackages(ctx, plan, []Package{{PkgID: "A1"}}))

	ok := Link{PlanID: plan, PkgID: "A1", Kind: LinkPR, Ref: "example-org/app#1", MatchMethod: MatchTitle, Confidence: ConfidenceHigh}
	for name, mutate := range map[string]func(*Link){
		"unknown kind":       func(l *Link) { l.Kind = "issue" },
		"unknown method":     func(l *Link) { l.MatchMethod = "guess" },
		"unknown confidence": func(l *Link) { l.Confidence = "medium" },
		"no package":         func(l *Link) { l.PkgID = "" },
		"pr without ref":     func(l *Link) { l.Ref = "" },
		"agent without ids":  func(l *Link) { l.Kind = LinkAgent; l.Ref = "" },
		"missing package":    func(l *Link) { l.PkgID = "Z9" },
		"missing plan":       func(l *Link) { l.PlanID = 999 },
	} {
		l := ok
		mutate(&l)
		if _, err := s.UpsertLink(ctx, l); err == nil {
			t.Errorf("%s: UpsertLink succeeded", name)
		}
	}
	id, err := s.UpsertLink(ctx, ok)
	must(t, err)
	bad := ok
	bad.ID = id
	bad.Kind = "issue"
	if err := s.UpdateLink(ctx, bad); err == nil {
		t.Error("UpdateLink to an unknown kind succeeded")
	}
	bad = ok
	bad.ID = id
	bad.PkgID = "Z9"
	if err := s.UpdateLink(ctx, bad); err == nil {
		t.Error("UpdateLink to a missing package succeeded")
	}
}

// DeletePlan takes the plan's packages, links and tombstones with it, and
// leaves other plans alone.
func TestPlanDeleteCascades(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	plan := newPlan(t, s, "aim")
	other := newPlan(t, s, "other")
	for _, id := range []int64{plan, other} {
		must(t, s.SyncPackages(ctx, id, []Package{{PkgID: "A1"}, {PkgID: "A2"}}))
		_, err := s.UpsertLink(ctx, Link{PlanID: id, PkgID: "A1", Kind: LinkBranch, Ref: "aim/a1", MatchMethod: MatchBranch, Confidence: ConfidenceHigh})
		must(t, err)
		must(t, s.Unlink(ctx, Tombstone{PlanID: id, PkgID: "A2", Kind: LinkBranch, Ref: "aim/a2"}))
	}

	must(t, s.DeletePlan(ctx, plan))
	for _, table := range []string{"packages", "links", "link_tombstones"} {
		var n, left int
		must(t, s.r.QueryRow(`SELECT count(*) FROM `+table+` WHERE plan_id = ?`, plan).Scan(&n))
		must(t, s.r.QueryRow(`SELECT count(*) FROM `+table+` WHERE plan_id = ?`, other).Scan(&left))
		if n != 0 {
			t.Errorf("%s has %d rows of the deleted plan", table, n)
		}
		if left == 0 {
			t.Errorf("%s lost the other plan's rows", table)
		}
	}
	if _, err := s.GetPlan(ctx, plan); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetPlan after DeletePlan: %v, want ErrNotFound", err)
	}
	if err := s.DeletePlan(ctx, plan); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeletePlan twice: %v, want ErrNotFound", err)
	}
}
