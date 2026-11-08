package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestWorktreesRoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	main := Worktree{Path: "/repos/app", MainRepo: "/repos/app", Branch: "main", HeadSHA: "aaa", UpstreamState: UpstreamOK, LastSeen: at(0)}
	wt := Worktree{
		Path:           "/repos/app/.claude/worktrees/agent-1",
		MainRepo:       "/repos/app",
		Branch:         "worktree-agent-1",
		HeadSHA:        "bbb",
		DirtyCount:     2,
		UntrackedCount: 3,
		IgnoredAtRisk:  []string{".spike-state/", "local.env"},
		UpstreamState:  UpstreamNone,
		UnpushedCount:  4,
		LastCommitAt:   at(-time.Hour),
		Locked:         true,
		LockedReason:   "claude agent agent-1 (pid 123 started 2026-11-05)",
		LockAgentID:    "1",
		LockPID:        123,
		SessionID:      "s1",
		AgentID:        "1",
		LastSeen:       at(0),
	}
	scratch := Worktree{Path: "/tmp/scratch/review", MainRepo: "/repos/app", Detached: true, HeadSHA: "ccc", PrunableReason: "gitdir file points to non-existent location", LastSeen: at(0)}
	must(t, s.UpsertWorktrees(ctx, wt, main, scratch))

	got, err := s.GetWorktree(ctx, wt.Path)
	must(t, err)
	want := wt
	want.FirstSeen = at(0)
	equal(t, "worktree", got, want)

	list, err := s.ListWorktrees(ctx, WorktreeFilter{MainRepo: "/repos/app"})
	must(t, err)
	if len(list) != 3 || list[0].Path != main.Path || list[1].Path != wt.Path || list[2].Path != scratch.Path {
		t.Errorf("ListWorktrees = %+v, want main, agent-1, scratch", list)
	}

	// A rescan rewrites the scanner's fields and keeps FirstSeen. A path
	// that differs only in case is the same worktree.
	wt2 := wt
	wt2.Path = "/Repos/App/.claude/worktrees/agent-1"
	wt2.DirtyCount = 0
	wt2.IgnoredAtRisk = nil
	wt2.Locked = false
	wt2.LockedReason = ""
	wt2.LockAgentID = ""
	wt2.LockPID = 0
	wt2.LastSeen = at(time.Minute)
	must(t, s.UpsertWorktrees(ctx, wt2))
	got, err = s.GetWorktree(ctx, wt.Path)
	must(t, err)
	want = wt2
	want.FirstSeen = at(0)
	equal(t, "worktree after rescan", got, want)

	other, err := s.ListWorktrees(ctx, WorktreeFilter{MainRepo: "/repos/other"})
	must(t, err)
	if len(other) != 0 {
		t.Errorf("another repo's worktrees = %+v", other)
	}
}

func TestMarkWorktreeRemoved(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	wt := Worktree{Path: "/repos/app-b1", MainRepo: "/repos/app", LastSeen: at(0)}
	must(t, s.UpsertWorktrees(ctx, wt))

	must(t, s.MarkWorktreeRemoved(ctx, "/REPOS/app-b1", at(time.Minute)))
	must(t, s.MarkWorktreeRemoved(ctx, wt.Path, at(time.Hour))) // keeps the first time
	got, err := s.GetWorktree(ctx, wt.Path)
	must(t, err)
	equal(t, "RemovedAt", got.RemovedAt, at(time.Minute))

	live, err := s.ListWorktrees(ctx, WorktreeFilter{})
	must(t, err)
	if len(live) != 0 {
		t.Errorf("ListWorktrees lists a removed worktree: %+v", live)
	}
	all, err := s.ListWorktrees(ctx, WorktreeFilter{IncludeRemoved: true})
	must(t, err)
	if len(all) != 1 {
		t.Errorf("ListWorktrees with removed = %+v, want one", all)
	}

	// Seen again: no longer removed.
	must(t, s.UpsertWorktrees(ctx, wt))
	got, err = s.GetWorktree(ctx, wt.Path)
	must(t, err)
	equal(t, "RemovedAt after being seen again", got.RemovedAt, time.Time{})

	if err := s.MarkWorktreeRemoved(ctx, "/nowhere", time.Time{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("MarkWorktreeRemoved on a missing path: %v, want ErrNotFound", err)
	}
	if _, err := s.GetWorktree(ctx, "/nowhere"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetWorktree on a missing path: %v, want ErrNotFound", err)
	}
}

func TestMarkMissingWorktreesRemoved(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(t, s.UpsertWorktrees(ctx,
		Worktree{Path: "/repos/app", MainRepo: "/repos/app"},
		Worktree{Path: "/repos/app-a", MainRepo: "/repos/app"},
		Worktree{Path: "/repos/app-b", MainRepo: "/repos/app"},
		Worktree{Path: "/repos/app-c", MainRepo: "/repos/app"},
		Worktree{Path: "/repos/other-a", MainRepo: "/repos/other"},
	))
	must(t, s.MarkWorktreeRemoved(ctx, "/repos/app-c", at(-time.Hour)))

	// The scan found app and app-a (in other case); app-b is gone, app-c
	// was already gone, other-a belongs to another repo.
	n, err := s.MarkMissingWorktreesRemoved(ctx, "/Repos/App", []string{"/repos/app", "/REPOS/APP-A"}, at(time.Minute))
	must(t, err)
	if n != 1 {
		t.Errorf("marked %d, want 1", n)
	}
	removed := map[string]time.Time{}
	all, err := s.ListWorktrees(ctx, WorktreeFilter{IncludeRemoved: true})
	must(t, err)
	for _, w := range all {
		removed[w.Path] = w.RemovedAt
	}
	equal(t, "removed times", removed, map[string]time.Time{
		"/repos/app":     {},
		"/repos/app-a":   {},
		"/repos/app-b":   at(time.Minute),
		"/repos/app-c":   at(-time.Hour),
		"/repos/other-a": {},
	})

	// No paths at all: everything of the repo goes.
	n, err = s.MarkMissingWorktreesRemoved(ctx, "/repos/other", nil, time.Time{})
	must(t, err)
	if n != 1 {
		t.Errorf("marked %d of other, want 1", n)
	}
	if _, err := s.MarkMissingWorktreesRemoved(ctx, "", nil, time.Time{}); err == nil {
		t.Error("MarkMissingWorktreesRemoved with no repo succeeded")
	}
}

func TestUpsertWorktreesValidates(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	for _, w := range []Worktree{{MainRepo: "/r"}, {Path: "/r"}} {
		if err := s.UpsertWorktrees(ctx, w); err == nil {
			t.Errorf("UpsertWorktrees(%+v) succeeded", w)
		}
	}
}
