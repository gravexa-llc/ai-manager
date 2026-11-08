package prune

import (
	"context"
	"fmt"
	"strings"

	"github.com/gravexa-llc/ai-manager/internal/worktrees"
)

// Plan is what `aim prune --apply` would do.
type Plan struct {
	// Remove are the worktrees to remove (Safe and Scratch, plus
	// SafeClosed with --include-closed), each with its branch if it has
	// one.
	Remove []*Item
	// Prune are the repos to run `git worktree prune` in: those with
	// prunable records. Each record shown is in the repo's Items.
	Prune []*RepoReport
}

// Empty reports whether the plan does nothing.
func (p Plan) Empty() bool { return len(p.Remove) == 0 && len(p.Prune) == 0 }

// Plan returns what --apply would do with this report.
func (r *Report) Plan(includeClosed bool) Plan {
	var p Plan
	for _, rr := range r.Repos {
		prunable := false
		for _, it := range rr.Items {
			switch {
			case it.Category.Removable(includeClosed):
				p.Remove = append(p.Remove, it)
			case it.Category == Prunable:
				prunable = true
			}
		}
		if prunable {
			p.Prune = append(p.Prune, rr)
		}
	}
	return p
}

// Removal is what happened to one planned worktree.
type Removal struct {
	Item *Item
	// Removed is true when `git worktree remove` succeeded.
	Removed bool
	// BranchDeleted is true when its branch was deleted too.
	BranchDeleted bool
	// Skipped says why it was left alone after the re-check ("" if it
	// wasn't).
	Skipped string
	// Err is the failure of the removal, or of the branch deletion when
	// Removed is true.
	Err error
}

// PruneRun is what happened in one repo planned for `git worktree prune`.
type PruneRun struct {
	Repo *RepoReport
	// Pruned are the records cleared.
	Pruned []worktrees.PruneEntry
	// Skipped says why prune wasn't run ("" if it was).
	Skipped string
	Err     error
}

// Result is what Apply did.
type Result struct {
	Removals []Removal
	Prunes   []PruneRun
	// RecheckErr is set when the re-check couldn't run; then nothing was
	// done.
	RecheckErr error
}

// Failed reports whether anything failed (skips are not failures).
func (r Result) Failed() bool {
	if r.RecheckErr != nil {
		return true
	}
	for _, x := range r.Removals {
		if x.Err != nil {
			return true
		}
	}
	for _, x := range r.Prunes {
		if x.Err != nil {
			return true
		}
	}
	return false
}

// Apply carries out plan, which came from prior (evaluated with opts).
// First it evaluates again, reusing prior's GitHub answers, so that a
// worktree that changed since (a new file, a session that moved into it, a
// new commit, a lock) is skipped. Then, for each planned worktree that is
// still in the same category at the same commit, it runs
// `git worktree remove` (never --force) and, for a branch worktree,
// `git branch -D` while the branch still points at that commit. Last, in
// each planned repo, it runs `git worktree prune` when
// `git worktree prune --dry-run` names only records the plan listed.
func (c *Checker) Apply(ctx context.Context, plan Plan, opts Options, prior *Report) Result {
	var res Result
	opts.Prior = prior
	fresh, err := c.Evaluate(ctx, opts)
	if err != nil {
		res.RecheckErr = err
		return res
	}
	freshItems := map[string]*Item{}
	freshRepos := map[string]*RepoReport{}
	for _, rr := range fresh.Repos {
		freshRepos[worktrees.PathKey(rr.Repo.GitDir)] = rr
		for _, it := range rr.Items {
			freshItems[itemKey(it)] = it
		}
	}

	for _, it := range plan.Remove {
		out := Removal{Item: it}
		now := freshItems[itemKey(it)]
		switch {
		case now == nil:
			out.Skipped = "no longer listed by git"
		case now.Category != it.Category || now.Worktree.Head != it.Worktree.Head || now.Worktree.Branch != it.Worktree.Branch:
			out.Skipped = "changed since the dry run: " + verdictLine(now)
		default:
			out.Removed, out.BranchDeleted, out.Err = c.remove(ctx, now)
		}
		res.Removals = append(res.Removals, out)
	}

	for _, rr := range plan.Prune {
		run := PruneRun{Repo: rr}
		now := freshRepos[worktrees.PathKey(rr.Repo.GitDir)]
		if now == nil {
			run.Skipped = "repo no longer found"
		} else {
			run.Pruned, run.Skipped, run.Err = c.prune(ctx, rr, now)
		}
		res.Prunes = append(res.Prunes, run)
	}
	return res
}

// remove removes one worktree and then its branch.
func (c *Checker) remove(ctx context.Context, it *Item) (removed, branchDeleted bool, err error) {
	w := it.Worktree
	if err := c.Scanner.RemoveWorktree(ctx, it.Repo.Repo, w); err != nil {
		return false, false, err
	}
	if w.Branch == "" {
		return true, false, nil
	}
	if err := c.Scanner.DeleteBranch(ctx, it.Repo.Repo.GitDir, w.Branch, w.Head); err != nil {
		return true, false, fmt.Errorf("worktree removed, branch %s kept: %w", w.Branch, err)
	}
	return true, true, nil
}

// prune runs `git worktree prune` in a repo when every record it would
// clear is one the plan listed (planned) and none is locked in the fresh
// evaluation (now).
func (c *Checker) prune(ctx context.Context, planned, now *RepoReport) ([]worktrees.PruneEntry, string, error) {
	listed := map[string]bool{}
	for _, it := range planned.Items {
		if it.Category == Prunable {
			listed[worktrees.PathKey(it.Worktree.RecordedPath)] = true
			listed[worktrees.PathKey(it.Worktree.Path)] = true
		}
	}
	locked := map[string]bool{}
	for _, it := range now.Items {
		if it.Worktree.Locked {
			locked[worktrees.PathKey(it.Worktree.RecordedPath)] = true
			locked[worktrees.PathKey(it.Worktree.Path)] = true
		}
	}
	preview, err := c.Scanner.PrunePreview(ctx, now.Repo.GitDir)
	if err != nil {
		return nil, "", err
	}
	if len(preview) == 0 {
		return nil, "nothing left to prune", nil
	}
	var unlisted []string
	for _, e := range preview {
		key := worktrees.PathKey(e.Path)
		switch {
		case e.Path == "":
			unlisted = append(unlisted, fmt.Sprintf("worktrees/%s (%s)", e.ID, e.Reason))
		case locked[key]:
			unlisted = append(unlisted, fmt.Sprintf("%s (locked)", e.Path))
		case !listed[key]:
			unlisted = append(unlisted, fmt.Sprintf("%s (%s)", e.Path, e.Reason))
		}
	}
	if len(unlisted) > 0 {
		return nil, "prune would also clear records that weren't listed: " + strings.Join(unlisted, "; "), nil
	}
	if err := c.Scanner.Prune(ctx, now.Repo.GitDir); err != nil {
		return nil, "", err
	}
	return preview, "", nil
}

// itemKey identifies a worktree across evaluations.
func itemKey(it *Item) string {
	return worktrees.PathKey(it.Repo.Repo.GitDir) + "\x00" + worktrees.PathKey(it.Worktree.RecordedPath)
}

// verdictLine is the category and reasons on one line.
func verdictLine(it *Item) string {
	return string(it.Category) + ": " + strings.Join(it.Reasons, "; ")
}
