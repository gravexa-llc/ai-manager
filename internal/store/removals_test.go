package store

import (
	"context"
	"testing"
	"time"
)

func TestRemovalLog(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	old := Removal{At: at(-31 * 24 * time.Hour), Repo: "/r/widgets", Path: "/r/wt/old", Branch: "old", Head: "aaa",
		Action: RemovalRemoved, BranchDeleted: true}
	must(t, s.AddRemovals(ctx,
		old,
		Removal{At: at(-time.Hour), Repo: "/r/widgets", Path: "/r/wt/a", Branch: "a", Head: "bbb", Action: RemovalRemoved, BranchDeleted: true},
		Removal{At: at(-time.Hour), Repo: "/r/widgets", Path: "/r/wt/b", Branch: "b", Action: RemovalSkipped, Detail: "no longer safe: 1 untracked file"},
	))
	// A zero time is now.
	must(t, s.AddRemovals(ctx, Removal{Repo: "/r/widgets", Path: "/r/wt/gone", Action: RemovalPruned}))
	must(t, s.AddRemovals(ctx)) // nothing to add

	got, err := s.ListRemovals(ctx, time.Time{}, 0)
	must(t, err)
	var paths []string
	for _, e := range got {
		paths = append(paths, e.Path)
	}
	// Newest first; in the same millisecond, the later entry first.
	equal(t, "all", paths, []string{"/r/wt/gone", "/r/wt/b", "/r/wt/a", "/r/wt/old"})
	equal(t, "now", got[0].At, t0)
	a := got[2]
	a.ID = 0
	equal(t, "entry", a, Removal{At: at(-time.Hour), Repo: "/r/widgets", Path: "/r/wt/a", Branch: "a", Head: "bbb",
		Action: RemovalRemoved, BranchDeleted: true})
	equal(t, "detail", got[1].Detail, "no longer safe: 1 untracked file")

	// since and limit.
	recent, err := s.ListRemovals(ctx, at(-30*24*time.Hour), 2)
	must(t, err)
	if len(recent) != 2 || recent[0].Path != "/r/wt/gone" || recent[1].Path != "/r/wt/b" {
		t.Errorf("recent, limit 2: %+v", recent)
	}

	// Purge deletes what is older than the cutoff only.
	n, err := s.PurgeRemovals(ctx, at(-30*24*time.Hour))
	must(t, err)
	if n != 1 {
		t.Errorf("purged %d, want 1", n)
	}
	left, err := s.ListRemovals(ctx, time.Time{}, 0)
	must(t, err)
	if len(left) != 3 {
		t.Errorf("%d left after the purge, want 3", len(left))
	}

	// An entry without an action is refused, and nothing of its batch is kept.
	if err := s.AddRemovals(ctx, Removal{Path: "/r/wt/c", Action: RemovalRemoved}, Removal{Path: "/r/wt/d"}); err == nil {
		t.Error("an entry without an action was accepted")
	}
	if after, _ := s.ListRemovals(ctx, time.Time{}, 0); len(after) != 3 {
		t.Errorf("%d entries after a refused batch, want 3", len(after))
	}
}
