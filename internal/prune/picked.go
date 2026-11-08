package prune

import (
	"context"
	"errors"
	"strings"

	"github.com/gravexa-llc/ai-manager/internal/worktrees"
)

// This file holds removal for a caller that shows the verdicts and lets you
// pick worktrees, such as the dashboard's Worktrees & cleanup page. It
// re-checks the rules at the moment of removal and removes with Apply's own
// code (remove, prune), so `aim prune --apply` and the dashboard remove
// worktrees the same way.

// Pick is a worktree chosen for removal from an earlier look at it: its
// path, and the HEAD and branch ("" when detached) it had when it was shown.
type Pick struct {
	Path   string
	Head   string
	Branch string
}

// Picked is what RemovePicked did with one Pick.
type Picked struct {
	Pick Pick
	// Item is the worktree as the re-check found it; nil when git no
	// longer lists a worktree at the pick's path.
	Item *Item
	// Removed is true when `git worktree remove` succeeded, and
	// BranchDeleted when its branch was deleted too.
	Removed, BranchDeleted bool
	// Skipped says why it was left alone ("" if it wasn't).
	Skipped string
	// Err is the failure of the removal, or of the branch deletion when
	// Removed is true.
	Err error
}

// RemovePicked evaluates the worktrees again with opts, at this moment
// (GitHub is asked again unless opts.Prior is set, and the sessions are
// listed again), and removes each pick that is still safe: git lists a
// worktree at its path, the rules put it in a category `aim prune --apply`
// removes without --include-closed (Safe or Scratch), and it is still at
// the pick's HEAD on the pick's branch. It removes them as Apply does, with
// the same code: `git worktree remove` (never --force), then for a branch
// worktree `git branch -D` while the branch still points at that HEAD.
//
// Every other pick is skipped, with the reason: a main checkout, a locked
// worktree, a prunable record, one that is no longer safe, one that moved
// to another commit or branch, a path git doesn't list, and a path picked
// twice. A pick's path is only ever matched against what git lists; nothing
// is done to a path git doesn't list as a worktree.
//
// The error is the re-check's; then nothing was done.
func (c *Checker) RemovePicked(ctx context.Context, opts Options, picks []Pick) ([]Picked, error) {
	fresh, err := c.Evaluate(ctx, opts)
	if err != nil {
		return nil, err
	}
	byPath := map[string]*Item{}
	for _, rr := range fresh.Repos {
		for _, it := range rr.Items {
			byPath[worktrees.PathKey(it.Worktree.Path)] = it
		}
	}
	seen := map[string]bool{}
	out := make([]Picked, 0, len(picks))
	for _, p := range picks {
		r := Picked{Pick: p}
		key := worktrees.PathKey(p.Path)
		now := byPath[key]
		switch {
		case p.Path == "":
			r.Skipped = "no path given"
		case seen[key]:
			r.Skipped = "picked twice; handled once"
		case now == nil:
			r.Skipped = "git doesn't list a worktree there now"
		case !now.Category.Removable(false):
			r.Item, r.Skipped = now, notRemovable(now)
		case now.Worktree.Head != p.Head || now.Worktree.Branch != p.Branch:
			r.Item = now
			r.Skipped = "changed since it was shown: it is " + headLine(now.Worktree.Head, now.Worktree.Branch) +
				" now, not " + headLine(p.Head, p.Branch)
		default:
			r.Item = now
			r.Removed, r.BranchDeleted, r.Err = c.remove(ctx, now)
		}
		seen[key] = true
		out = append(out, r)
	}
	return out, nil
}

// notRemovable says why a re-checked worktree isn't removed.
func notRemovable(it *Item) string {
	why := strings.Join(it.Reasons, "; ")
	switch it.Category {
	case Main:
		return "a main checkout is never removed"
	case Locked:
		return "locked, never removed: " + why
	case Prunable:
		return "its folder is gone; git worktree prune clears the record"
	case SafeClosed:
		return why + "; only aim prune --include-closed removes it"
	}
	return "no longer safe: " + why
}

// headLine names a commit and branch: "on <branch> at <sha>", or "detached
// at <sha>".
func headLine(head, branch string) string {
	at := short(head)
	if at == "" {
		at = "an unknown commit"
	}
	if branch == "" {
		return "detached at " + at
	}
	return "on " + branch + " at " + at
}

// PruneListed runs `git worktree prune` in the one repo opts.Repo names, as
// Apply does: only when every record prune would clear is one of records
// (the paths of the prunable records shown earlier) that git still calls
// prunable, and none of them is locked. GitHub and the session list aren't
// read: they don't change what prune clears. The error is the re-check's;
// then nothing was done.
func (c *Checker) PruneListed(ctx context.Context, opts Options, records []string) (PruneRun, error) {
	if opts.Repo == "" {
		return PruneRun{}, errors.New("prune records: no repo given")
	}
	cc := *c
	cc.GitHub, cc.Sessions = nil, nil
	fresh, err := cc.Evaluate(ctx, opts)
	if err != nil {
		return PruneRun{}, err
	}
	if len(fresh.Repos) != 1 {
		return PruneRun{}, errors.New("prune records: " + opts.Repo + " doesn't name exactly one repo")
	}
	rr := fresh.Repos[0]
	want := map[string]bool{}
	for _, p := range records {
		want[worktrees.PathKey(p)] = true
	}
	listed := &RepoReport{Repo: rr.Repo}
	for _, it := range rr.Items {
		w := it.Worktree
		if it.Category == Prunable && (want[worktrees.PathKey(w.Path)] || want[worktrees.PathKey(w.RecordedPath)]) {
			listed.Items = append(listed.Items, it)
		}
	}
	run := PruneRun{Repo: rr}
	if len(listed.Items) == 0 {
		run.Skipped = "none of the records shown is prunable now"
		return run, nil
	}
	run.Pruned, run.Skipped, run.Err = cc.prune(ctx, listed, rr)
	return run, nil
}
