package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestReposRoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	repos, err := s.ListRepos(ctx)
	must(t, err)
	equal(t, "repos of a new store", len(repos), 0)

	must(t, s.UpsertRepos(ctx,
		Repo{Name: "example-org/web", DefaultBranch: "trunk", UpdatedAt: at(time.Minute)},
		Repo{Name: "example-org/App", DefaultBranch: "main"}, // zero UpdatedAt means now
	))
	repos, err = s.ListRepos(ctx)
	must(t, err)
	equal(t, "repos", repos, []Repo{
		{Name: "example-org/App", DefaultBranch: "main", UpdatedAt: t0},
		{Name: "example-org/web", DefaultBranch: "trunk", UpdatedAt: at(time.Minute)},
	})

	// The name compares case-insensitively; a write takes GitHub's new
	// spelling and branch.
	must(t, s.UpsertRepos(ctx, Repo{Name: "Example-Org/app", DefaultBranch: "develop", UpdatedAt: at(time.Hour)}))
	repos, err = s.ListRepos(ctx)
	must(t, err)
	equal(t, "repos after an update", repos, []Repo{
		{Name: "Example-Org/app", DefaultBranch: "develop", UpdatedAt: at(time.Hour)},
		{Name: "example-org/web", DefaultBranch: "trunk", UpdatedAt: at(time.Minute)},
	})

	if err := s.UpsertRepos(ctx, Repo{DefaultBranch: "main"}); err == nil {
		t.Error("UpsertRepos without a name succeeded")
	}
}

// Migration 0005 applies over a database at 0004 and keeps its PRs, which
// have no creation time until they're written again; repos starts empty.
func TestMigrate0005(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), FileName)
	db := rawDBAt(t, path)
	ms, err := loadMigrations(migrationFS)
	must(t, err)
	if len(ms) < 5 || ms[4].name != "0005_github.sql" {
		t.Fatalf("migration 5 = %+v", ms[4])
	}
	must(t, migrate(ctx, db, ms[:4]))
	_, err = db.Exec(`INSERT INTO prs (repo, number, head_branch, state, first_seen_at) VALUES ('example-org/app', 7, 'aim/b1-store', 'OPEN', ?)`, at(time.Hour).UnixMilli())
	must(t, err)
	must(t, db.Close())

	s, err := Open(ctx, path)
	must(t, err)
	defer s.Close()
	s.now = func() time.Time { return t0 }
	if v, err := s.SchemaVersion(ctx); err != nil || v < 5 {
		t.Fatalf("schema version %d after open (%v)", v, err)
	}
	pr, err := s.GetPR(ctx, "example-org/app", 7)
	must(t, err)
	if !pr.CreatedAt.IsZero() || !pr.OpenedAt().Equal(at(time.Hour)) {
		t.Errorf("PR from before 0005: created %v, opened %v", pr.CreatedAt, pr.OpenedAt())
	}
	pr.CreatedAt = at(-time.Hour)
	must(t, s.UpsertPRs(ctx, pr))
	pr, err = s.GetPR(ctx, "example-org/app", 7)
	must(t, err)
	if !pr.CreatedAt.Equal(at(-time.Hour)) || !pr.FirstSeenAt.Equal(at(time.Hour)) {
		t.Errorf("PR rewritten after 0005: created %v, first seen %v", pr.CreatedAt, pr.FirstSeenAt)
	}
	repos, err := s.ListRepos(ctx)
	must(t, err)
	equal(t, "repos after 0005", len(repos), 0)
}
