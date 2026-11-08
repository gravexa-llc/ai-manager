package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// reviewState maps "repo#number" to its ReviewRequestedAt, zero for none.
func reviewState(t *testing.T, s *Store) map[string]time.Time {
	t.Helper()
	prs, err := s.ListPRs(context.Background(), PRFilter{})
	must(t, err)
	out := map[string]time.Time{}
	for _, p := range prs {
		out[PRRef(p.Repo, p.Number)] = p.ReviewRequestedAt
		if p.ReviewRequested() != !p.ReviewRequestedAt.IsZero() {
			t.Errorf("%s: ReviewRequested() = %v with ReviewRequestedAt %v", PRRef(p.Repo, p.Number), p.ReviewRequested(), p.ReviewRequestedAt)
		}
	}
	return out
}

// A search sets the request on the PRs it found, keeps the time it was
// first seen, clears the PRs of its owners it no longer finds, and leaves
// other owners' PRs alone. UpsertPRs never touches it.
func TestSetReviewRequests(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(t, s.UpsertPRs(ctx,
		PR{Repo: "example-org/brand", Number: 6, State: PROpen},
		PR{Repo: "example-org/brand", Number: 7, State: PROpen},
		PR{Repo: "acme/pivot", Number: 214, State: PROpen},
		PR{Repo: "other-org/lib", Number: 9, State: PROpen},
	))

	// First search of both configured owners: two requests.
	n, err := s.SetReviewRequests(ctx, []string{"example-org", "Acme"},
		[]PRKey{{"example-org/brand", 6}, {"ACME/pivot", 214}, {"example-org/brand", 404}}, at(time.Minute))
	must(t, err)
	equal(t, "rows changed", n, int64(2))
	equal(t, "after the first search", reviewState(t, s), map[string]time.Time{
		"example-org/brand#6": at(time.Minute), "example-org/brand#7": {}, "acme/pivot#214": at(time.Minute), "other-org/lib#9": {},
	})

	// A poll rewrites the PR: the request stays.
	must(t, s.UpsertPRs(ctx, PR{Repo: "example-org/brand", Number: 6, State: PROpen, Title: "PR title"}))
	equal(t, "after an upsert", reviewState(t, s)["example-org/brand#6"], at(time.Minute))

	// Second search: #6 still requested (its first-seen time stays), #7
	// newly requested, pivot#214 reviewed. A PR of an owner that wasn't
	// searched is skipped.
	n, err = s.SetReviewRequests(ctx, []string{"example-org", "acme"},
		[]PRKey{{"example-org/brand", 6}, {"example-org/brand", 7}, {"other-org/lib", 9}}, at(2*time.Minute))
	must(t, err)
	equal(t, "rows changed", n, int64(2))
	equal(t, "after the second search", reviewState(t, s), map[string]time.Time{
		"example-org/brand#6": at(time.Minute), "example-org/brand#7": at(2 * time.Minute), "acme/pivot#214": {}, "other-org/lib#9": {},
	})

	// acme's search failed: only example-org's result counts, and
	// a acme request stays as it was.
	_, err = s.SetReviewRequests(ctx, []string{"acme"}, []PRKey{{"acme/pivot", 214}}, at(3*time.Minute))
	must(t, err)
	n, err = s.SetReviewRequests(ctx, []string{"example-org"}, nil, at(4*time.Minute))
	must(t, err)
	equal(t, "rows changed", n, int64(2))
	equal(t, "after a search finding none", reviewState(t, s), map[string]time.Time{
		"example-org/brand#6": {}, "example-org/brand#7": {}, "acme/pivot#214": at(3 * time.Minute), "other-org/lib#9": {},
	})

	// No owners: nothing searched, nothing changes.
	n, err = s.SetReviewRequests(ctx, nil, []PRKey{{"example-org/brand", 6}}, at(5*time.Minute))
	must(t, err)
	equal(t, "rows changed with no owners", n, int64(0))
	if got, err := s.GetPR(ctx, "acme/pivot", 214); err != nil || !got.ReviewRequestedAt.Equal(at(3*time.Minute)) {
		t.Errorf("GetPR = %+v, %v; want the request from 3m", got, err)
	}
}

// Migration 0007 applies over a database at 0006 and keeps its PRs, with
// no review requested until a search says so.
func TestMigrate0007(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), FileName)
	db := rawDBAt(t, path)
	ms, err := loadMigrations(migrationFS)
	must(t, err)
	if len(ms) < 7 || ms[6].name != "0007_review_requests.sql" {
		t.Fatalf("migration 7 = %+v", ms[6])
	}
	must(t, migrate(ctx, db, ms[:6]))
	_, err = db.Exec(`INSERT INTO prs (repo, number, head_branch, state, first_seen_at) VALUES ('example-org/app', 7, 'aim/w3-pr-triggers', 'OPEN', ?)`, at(time.Hour).UnixMilli())
	must(t, err)
	must(t, db.Close())

	s, err := Open(ctx, path)
	must(t, err)
	defer s.Close()
	if v, err := s.SchemaVersion(ctx); err != nil || v < 7 {
		t.Fatalf("schema version %d after open (%v)", v, err)
	}
	pr, err := s.GetPR(ctx, "example-org/app", 7)
	must(t, err)
	if pr.ReviewRequested() || pr.HeadBranch != "aim/w3-pr-triggers" || !pr.FirstSeenAt.Equal(at(time.Hour)) {
		t.Errorf("PR from before 0007 = %+v", pr)
	}
	_, err = s.SetReviewRequests(ctx, []string{"example-org"}, []PRKey{{"example-org/app", 7}}, at(2*time.Hour))
	must(t, err)
	if pr, err = s.GetPR(ctx, "example-org/app", 7); err != nil || !pr.ReviewRequestedAt.Equal(at(2*time.Hour)) {
		t.Errorf("after SetReviewRequests: %+v, %v", pr, err)
	}
}
