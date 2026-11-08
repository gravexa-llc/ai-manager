package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestRefs(t *testing.T) {
	equal(t, "BranchRef", BranchRef("example-org/app", "aim/c2-state"), "example-org/app:aim/c2-state")
	equal(t, "AgentRef", AgentRef("s1", "a26d"), "s1/a26d")
	equal(t, "AgentRef main loop", AgentRef("s1", ""), "s1/")
	equal(t, "JobRef", JobRef("job-1"), "job:job-1")
}

// NeedsYou is a parser field: written by every write, kept by none. The
// lifecycle is written only by SetLifecycles and kept by everything else.
func TestPackageNeedsYouAndLifecycle(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	plan := newPlan(t, s, "aim")

	must(t, s.InsertPackage(ctx, Package{PlanID: plan, PkgID: "R1", NeedsYou: "run the check"}))
	must(t, s.SyncPackages(ctx, plan, []Package{
		{PkgID: "R1", NeedsYou: "run the check again"},
		{PkgID: "B4", NeedsYou: "OK to install the launchd agent"},
	}))
	must(t, s.SetLifecycles(ctx, plan, []PackageLifecycle{
		{PkgID: "r1", Lifecycle: "ready", Prev: "blocked", At: at(time.Minute)},
		{PkgID: "B4", Lifecycle: "blocked"}, // zero time means now
		{PkgID: "Z9", Lifecycle: "ready"},   // gone: skipped
	}))
	got, err := s.ListPackages(ctx, plan)
	must(t, err)
	equal(t, "after SetLifecycles", got, []Package{
		{PlanID: plan, PkgID: "R1", Seq: 0, NeedsYou: "run the check again", Lifecycle: "ready", LifecyclePrev: "blocked", LifecycleAt: at(time.Minute)},
		{PlanID: plan, PkgID: "B4", Seq: 1, NeedsYou: "OK to install the launchd agent", Lifecycle: "blocked", LifecycleAt: t0},
	})

	// A re-parse and an update keep the lifecycle.
	must(t, s.SyncPackages(ctx, plan, []Package{{PkgID: "R1"}, {PkgID: "B4", Title: "Install aimd"}}))
	must(t, s.UpdatePackage(ctx, Package{PlanID: plan, PkgID: "R1", NeedsYou: "by hand", DoneManual: true}))
	got, err = s.ListPackages(ctx, plan)
	must(t, err)
	equal(t, "after a re-parse and an update", got, []Package{
		{PlanID: plan, PkgID: "R1", Seq: 0, NeedsYou: "by hand", DoneManual: true, Lifecycle: "ready", LifecyclePrev: "blocked", LifecycleAt: at(time.Minute)},
		{PlanID: plan, PkgID: "B4", Seq: 1, Title: "Install aimd", Lifecycle: "blocked", LifecycleAt: t0},
	})
}

func TestUnlinkAndTombstones(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	plan := newPlan(t, s, "aim")
	must(t, s.SyncPackages(ctx, plan, []Package{{PkgID: "A1"}, {PkgID: "B1"}}))

	pr := Link{PlanID: plan, PkgID: "A1", Kind: LinkPR, Ref: PRRef("example-org/app", 3), MatchMethod: MatchTitle, Confidence: ConfidenceHigh}
	_, err := s.UpsertLink(ctx, pr)
	must(t, err)

	// Unlink removes the stored link and leaves a tombstone.
	must(t, s.Unlink(ctx, Tombstone{PlanID: plan, PkgID: "a1", Kind: LinkPR, Ref: pr.Ref, CreatedAt: at(time.Minute)}))
	links, err := s.ListLinks(ctx, plan)
	must(t, err)
	if len(links) != 0 {
		t.Errorf("links after Unlink = %+v", links)
	}
	// A link that was never stored (one reached through another) can be
	// unlinked too, and unlinking twice is fine. An agent's ref is filled in.
	must(t, s.Unlink(ctx, Tombstone{PlanID: plan, PkgID: "B1", Kind: LinkAgent, SessionID: "s1", AgentID: "a2"}))
	must(t, s.Unlink(ctx, Tombstone{PlanID: plan, PkgID: "B1", Kind: LinkAgent, SessionID: "s1", AgentID: "a2"}))
	ts, err := s.ListTombstones(ctx, plan)
	must(t, err)
	equal(t, "tombstones", ts, []Tombstone{
		{PlanID: plan, PkgID: "A1", Kind: LinkPR, Ref: pr.Ref, CreatedAt: at(time.Minute)},
		{PlanID: plan, PkgID: "B1", Kind: LinkAgent, SessionID: "s1", AgentID: "a2", Ref: "s1/a2", CreatedAt: t0},
	})

	// The matcher can't bring the PR link back.
	ch, err := s.ReplaceInferredLinks(ctx, plan, []Link{pr})
	must(t, err)
	equal(t, "changes", ch, LinkChanges{})

	// A manual link to the same target clears the tombstone.
	manual := pr
	manual.MatchMethod = MatchManual
	manual.Confidence = ConfidenceExact
	_, err = s.UpsertLink(ctx, manual)
	must(t, err)
	ts, err = s.ListTombstones(ctx, plan)
	must(t, err)
	if len(ts) != 1 || ts[0].PkgID != "B1" {
		t.Errorf("tombstones after a manual link = %+v", ts)
	}
	// A spawn link doesn't.
	_, err = s.UpsertLink(ctx, Link{PlanID: plan, PkgID: "B1", Kind: LinkAgent, SessionID: "s1", AgentID: "a2", MatchMethod: MatchSpawn, Confidence: ConfidenceExact})
	must(t, err)
	ts, err = s.ListTombstones(ctx, plan)
	must(t, err)
	if len(ts) != 1 {
		t.Errorf("a spawn link cleared a tombstone: %+v", ts)
	}

	for name, tomb := range map[string]Tombstone{
		"missing package": {PlanID: plan, PkgID: "Z9", Kind: LinkPR, Ref: "x#1"},
		"unknown kind":    {PlanID: plan, PkgID: "A1", Kind: "issue", Ref: "x"},
		"no ref":          {PlanID: plan, PkgID: "A1", Kind: LinkBranch},
	} {
		if err := s.Unlink(ctx, tomb); err == nil {
			t.Errorf("%s: Unlink succeeded", name)
		} else if name == "missing package" && !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v, want ErrNotFound", name, err)
		}
	}

	// Tombstones follow a package's respelling and go with it.
	must(t, s.SyncPackages(ctx, plan, []Package{{PkgID: "a1"}, {PkgID: "b1"}}))
	ts, err = s.ListTombstones(ctx, plan)
	must(t, err)
	if len(ts) != 1 || ts[0].PkgID != "b1" {
		t.Errorf("tombstones after a respelling = %+v", ts)
	}
	must(t, s.SyncPackages(ctx, plan, []Package{{PkgID: "a1"}}))
	ts, err = s.ListTombstones(ctx, plan)
	must(t, err)
	if len(ts) != 0 {
		t.Errorf("tombstones after the package went = %+v", ts)
	}
}

func TestReplaceInferredLinks(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	plan := newPlan(t, s, "aim")
	other := newPlan(t, s, "other")
	must(t, s.SyncPackages(ctx, plan, []Package{{PkgID: "A1"}, {PkgID: "B1"}, {PkgID: "C1"}}))
	must(t, s.SyncPackages(ctx, other, []Package{{PkgID: "A1"}}))

	title := Link{PkgID: "A1", Kind: LinkPR, Ref: PRRef("example-org/app", 3), MatchMethod: MatchTitle, Confidence: ConfidenceHigh}
	branch := Link{PkgID: "b1", Kind: LinkBranch, Ref: BranchRef("example-org/app", "share/b1-x"), MatchMethod: MatchBranch, Confidence: ConfidenceLow, CreatedAt: at(time.Second)}
	gone := Link{PkgID: "Z9", Kind: LinkBranch, Ref: "example-org/app:z9", MatchMethod: MatchBranch, Confidence: ConfidenceHigh}
	ch, err := s.ReplaceInferredLinks(ctx, plan, []Link{title, branch, gone, title})
	must(t, err)
	equal(t, "first run", ch, LinkChanges{Added: 2})
	otherID, err := s.UpsertLink(ctx, Link{PlanID: other, PkgID: "A1", Kind: LinkPR, Ref: title.Ref, MatchMethod: MatchManual, Confidence: ConfidenceExact})
	must(t, err)

	links, err := s.ListLinks(ctx, plan)
	must(t, err)
	if len(links) != 2 || !links[0].Inferred || links[0].PkgID != "A1" || links[1].PkgID != "B1" || !links[1].CreatedAt.Equal(at(time.Second)) {
		t.Fatalf("links after the first run = %+v", links)
	}
	branchID := links[1].ID

	// The same answer again changes nothing.
	ch, err = s.ReplaceInferredLinks(ctx, plan, []Link{title, branch})
	must(t, err)
	equal(t, "same answer", ch, LinkChanges{})

	// A spawn link to C1 is not the matcher's: the matcher can't change or
	// delete it. The branch link moves to high confidence; the title link
	// is no longer proposed and goes.
	spawn := Link{PlanID: plan, PkgID: "C1", Kind: LinkAgent, SessionID: "s1", AgentID: "a1", MatchMethod: MatchSpawn, Confidence: ConfidenceExact}
	spawnID, err := s.UpsertLink(ctx, spawn)
	must(t, err)
	branch.Confidence = ConfidenceHigh
	inferredSpawn := spawn
	inferredSpawn.MatchMethod, inferredSpawn.Confidence = MatchBranch, ConfidenceLow
	ch, err = s.ReplaceInferredLinks(ctx, plan, []Link{branch, inferredSpawn})
	must(t, err)
	equal(t, "second run", ch, LinkChanges{Updated: 1, Removed: 1})
	links, err = s.ListLinks(ctx, plan)
	must(t, err)
	equal(t, "links after the second run", links, []Link{
		{ID: branchID, PlanID: plan, PkgID: "B1", Kind: LinkBranch, Ref: branch.Ref, MatchMethod: MatchBranch, Confidence: ConfidenceHigh, CreatedAt: at(time.Second), Inferred: true},
		{ID: spawnID, PlanID: plan, PkgID: "C1", Kind: LinkAgent, SessionID: "s1", AgentID: "a1", Ref: "s1/a1", MatchMethod: MatchSpawn, Confidence: ConfidenceExact, CreatedAt: t0},
	})

	// Confirming an inferred link by hand makes it yours: an empty answer
	// then leaves it.
	_, err = s.UpsertLink(ctx, Link{PlanID: plan, PkgID: "B1", Kind: LinkBranch, Ref: branch.Ref, MatchMethod: MatchManual, Confidence: ConfidenceExact})
	must(t, err)
	ch, err = s.ReplaceInferredLinks(ctx, plan, nil)
	must(t, err)
	equal(t, "empty answer", ch, LinkChanges{})
	links, err = s.ListLinks(ctx, plan)
	must(t, err)
	if len(links) != 2 || links[0].Inferred || links[0].MatchMethod != MatchManual {
		t.Errorf("links after the empty answer = %+v", links)
	}

	// The other plan's links are untouched throughout.
	otherLinks, err := s.ListLinks(ctx, other)
	must(t, err)
	if len(otherLinks) != 1 || otherLinks[0].ID != otherID {
		t.Errorf("other plan's links = %+v", otherLinks)
	}

	// An invalid link fails the whole call.
	if _, err := s.ReplaceInferredLinks(ctx, plan, []Link{title, {PkgID: "A1", Kind: "issue", Ref: "x", MatchMethod: MatchTitle, Confidence: ConfidenceHigh}}); err == nil {
		t.Error("an invalid link was accepted")
	}
	links, err = s.ListLinks(ctx, plan)
	must(t, err)
	if len(links) != 2 {
		t.Errorf("a refused call changed the links: %+v", links)
	}
}

// A database migrated to 0001 with rows in it upgrades to 0002: the new
// columns take their defaults.
func TestMigrate0002KeepsRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), FileName)
	db := rawDBAt(t, path)
	ms, err := loadMigrations(migrationFS)
	must(t, err)
	must(t, migrate(ctx, db, ms[:1]))
	_, err = db.Exec(`INSERT INTO plans (slug, created_at, updated_at) VALUES ('aim', 1, 1)`)
	must(t, err)
	_, err = db.Exec(`INSERT INTO packages (plan_id, pkg_id) VALUES (1, 'A1')`)
	must(t, err)
	_, err = db.Exec(`INSERT INTO links (plan_id, pkg_id, kind, ref, match_method, confidence, created_at) VALUES (1, 'A1', 'branch', 'x', 'manual', 'exact', 1)`)
	must(t, err)
	_, err = db.Exec(`INSERT INTO prs (repo, number) VALUES ('example-org/app', 1)`)
	must(t, err)
	must(t, db.Close())

	s, err := Open(ctx, path)
	must(t, err)
	defer s.Close()
	s.now = func() time.Time { return t0 }
	pkgs, err := s.ListPackages(ctx, 1)
	must(t, err)
	equal(t, "package", pkgs, []Package{{PlanID: 1, PkgID: "A1"}})
	links, err := s.ListLinks(ctx, 1)
	must(t, err)
	if len(links) != 1 || links[0].Inferred {
		t.Errorf("links = %+v", links)
	}
	pr, err := s.GetPR(ctx, "example-org/app", 1)
	must(t, err)
	if !pr.FirstSeenAt.IsZero() {
		t.Errorf("FirstSeenAt = %v, want zero for a PR stored before 0002", pr.FirstSeenAt)
	}
	// Its next upsert counts as its first sighting.
	pr.Title = "x"
	must(t, s.UpsertPRs(ctx, pr))
	pr, err = s.GetPR(ctx, "example-org/app", 1)
	must(t, err)
	equal(t, "FirstSeenAt after an upsert", pr.FirstSeenAt, t0)
}

// rawDBAt opens a bare writer pool on the file at path.
func rawDBAt(t *testing.T, path string) *sql.DB {
	t.Helper()
	must(t, createFile(path))
	db, err := sql.Open("sqlite", dsn(path, false))
	must(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}
