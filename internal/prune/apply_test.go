package prune_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gravexa-llc/ai-manager/internal/prune"
	"github.com/gravexa-llc/ai-manager/internal/prune/prunetest"
	"github.com/gravexa-llc/ai-manager/internal/sessions"
	"github.com/gravexa-llc/ai-manager/internal/worktrees"
)

// branchExists reports whether the widgets repo still has the branch.
func branchExists(t *testing.T, w *prunetest.World, branch string) bool {
	t.Helper()
	out := prunetest.Git(t, w.Repo, "branch", "--list", branch)
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(out), "+")) != ""
}

func TestApply(t *testing.T) {
	parallel(t)
	w := prunetest.BuildWorld(t)
	c := checker(t, w)
	rep := evaluate(t, c, prune.Options{})
	plan := rep.Plan(false)
	res := c.Apply(context.Background(), plan, prune.Options{}, rep)
	if res.Failed() {
		t.Fatalf("apply failed: %+v", res)
	}

	removed := map[string]bool{}
	for _, r := range res.Removals {
		name := w.Name(r.Item.Worktree.Path)
		if r.Skipped != "" || !r.Removed || r.Err != nil {
			t.Errorf("%s: removed %v, skipped %q, err %v", name, r.Removed, r.Skipped, r.Err)
		}
		if r.BranchDeleted != (r.Item.Worktree.Branch != "") {
			t.Errorf("%s: branch deleted %v", name, r.BranchDeleted)
		}
		removed[name] = true
	}
	for name, cat := range w.Want {
		p := w.WT[name]
		switch {
		case removed[name]:
			if prunetest.Exists(p) {
				t.Errorf("%s: folder still there", name)
			}
			if !strings.HasPrefix(name, "detached") && !strings.HasPrefix(name, "scratch") && branchExists(t, w, name) {
				t.Errorf("%s: branch still there", name)
			}
		case cat == "prunable":
		default:
			if !prunetest.Exists(p) {
				t.Errorf("%s (%s): folder removed", name, cat)
			}
		}
	}
	// Kept branches.
	for _, name := range []string{"closed", "atrisk", "after", "locked-live", "nopr"} {
		if !branchExists(t, w, name) {
			t.Errorf("branch %s deleted", name)
		}
	}
	// The at-risk folder kept its local state.
	if !prunetest.Exists(filepath.Join(w.WT["atrisk"], "scripts", ".spike-state", "state.json")) {
		t.Error("atrisk lost its ignored state")
	}

	// Prune ran in widgets and cleared both prunable records, leaving the
	// nogit folder's files and every locked record alone.
	if len(res.Prunes) != 1 || res.Prunes[0].Skipped != "" || len(res.Prunes[0].Pruned) != 2 {
		t.Fatalf("prunes: %+v", res.Prunes)
	}
	if !prunetest.Exists(filepath.Join(w.WT["nogit"], "keep.txt")) {
		t.Error("prune deleted a file in the nogit folder")
	}
	after := evaluate(t, c, prune.Options{})
	got := items(t, w, after)
	for _, name := range []string{"gone", "nogit"} {
		if got[name] != nil {
			t.Errorf("%s: still listed after prune", name)
		}
	}
	for _, name := range []string{"locked-live", "locked-stale", "locked-reused", "locked-other"} {
		if it := got[name]; it == nil || it.Category != prune.Locked {
			t.Errorf("%s: no longer locked after apply", name)
		}
	}
	if p := after.Plan(false); !p.Empty() {
		t.Errorf("a second plan isn't empty: %d to remove, %d to prune", len(p.Remove), len(p.Prune))
	}
}

func TestApplyIncludeClosed(t *testing.T) {
	parallel(t)
	w := prunetest.BuildWorld(t)
	c := checker(t, w)
	rep := evaluate(t, c, prune.Options{Repo: "widgets"})
	res := c.Apply(context.Background(), rep.Plan(true), prune.Options{Repo: "widgets"}, rep)
	if res.Failed() {
		t.Fatalf("apply failed: %+v", res)
	}
	if prunetest.Exists(w.WT["closed"]) || branchExists(t, w, "closed") {
		t.Errorf("closed worktree or branch kept with --include-closed; %s", outcome(w, rep, res, "closed"))
	}
}

// outcome says what the dry run and Apply made of a case, for failure
// messages.
func outcome(w *prunetest.World, rep *prune.Report, res prune.Result, name string) string {
	s := "not in the report"
	for _, rr := range rep.Repos {
		for _, it := range rr.Items {
			if w.Name(it.Worktree.Path) == name {
				s = fmt.Sprintf("dry run: %s %q", it.Category, it.Reasons)
			}
		}
	}
	for _, r := range res.Removals {
		if w.Name(r.Item.Worktree.Path) == name {
			return s + fmt.Sprintf("; apply: removed %v, skipped %q, err %v", r.Removed, r.Skipped, r.Err)
		}
	}
	return s + "; not in the plan"
}

func TestApplyRechecks(t *testing.T) {
	parallel(t)
	w := prunetest.BuildWorld(t)
	c := checker(t, w)
	rep := evaluate(t, c, prune.Options{})
	plan := rep.Plan(false)
	calls := w.GitHub.CallCount()

	// After the dry run: a new untracked file, a new commit, a new live
	// session, a new lock, and a folder removed by hand.
	prunetest.Write(t, filepath.Join(w.WT["merged"], "late.txt"), "x\n")
	prunetest.Write(t, filepath.Join(w.WT["squash"], "late.txt"), "x\n")
	prunetest.Commit(t, w.WT["squash"], "late commit")
	w.Sessions.Add(sessionIn(w.WT["ff"]))
	prunetest.Git(t, w.Repo, "worktree", "lock", w.WT["donejob"])
	if err := os.RemoveAll(w.WT["detached-main"]); err != nil {
		t.Fatal(err)
	}

	res := c.Apply(context.Background(), plan, prune.Options{}, rep)
	if w.GitHub.CallCount() != calls {
		t.Error("the re-check asked GitHub again")
	}
	outcome := map[string]prune.Removal{}
	for _, r := range res.Removals {
		outcome[w.Name(r.Item.Worktree.Path)] = r
	}
	for name, want := range map[string]string{
		"merged":        "1 untracked file",
		"squash":        "changed since the dry run",
		"ff":            "in use: cwd of",
		"donejob":       "locked",
		"detached-main": "prunable",
	} {
		r := outcome[name]
		if r.Removed || !strings.Contains(r.Skipped, want) {
			t.Errorf("%s: removed %v, skipped %q; want skipped with %q", name, r.Removed, r.Skipped, want)
		}
		if name != "detached-main" && !prunetest.Exists(w.WT[name]) {
			t.Errorf("%s: folder removed", name)
		}
	}
	for _, name := range []string{"merged", "squash", "ff", "donejob"} {
		if !branchExists(t, w, name) {
			t.Errorf("%s: branch deleted", name)
		}
	}
	// detached-main became a prunable record after the dry run, so prune
	// would clear something the plan didn't list: it's skipped.
	if len(res.Prunes) != 1 || !strings.Contains(res.Prunes[0].Skipped, "weren't listed") || len(res.Prunes[0].Pruned) != 0 {
		t.Errorf("prunes: %+v", res.Prunes)
	}
	if !prunetest.Exists(filepath.Join(w.Repo, ".git", "worktrees", "gone")) {
		t.Error("prune ran anyway")
	}
	// The others still went.
	for _, name := range []string{"basemissing", "detached-surge", "scratch-dead"} {
		if r := outcome[name]; !r.Removed {
			t.Errorf("%s: not removed: %q %v", name, r.Skipped, r.Err)
		}
	}
}

func TestApplySkipsPruneWithUnlistedRecord(t *testing.T) {
	parallel(t)
	w := prunetest.BuildWorld(t)
	c := checker(t, w)
	// An admin dir with no gitdir file: `git worktree list` never shows it,
	// but `git worktree prune` would clear it.
	prunetest.Write(t, filepath.Join(w.Repo, ".git", "worktrees", "broken", "HEAD"), "ref: refs/heads/main\n")
	rep := evaluate(t, c, prune.Options{})
	res := c.Apply(context.Background(), prune.Plan{Prune: rep.Plan(false).Prune}, prune.Options{}, rep)
	if len(res.Prunes) != 1 || !strings.Contains(res.Prunes[0].Skipped, "worktrees/broken") {
		t.Fatalf("prunes: %+v", res.Prunes)
	}
	if !prunetest.Exists(filepath.Join(w.Repo, ".git", "worktrees", "gone")) {
		t.Error("prune ran anyway")
	}
}

func TestApplyBranchMovedAfterRecheck(t *testing.T) {
	parallel(t)
	w := prunetest.BuildWorld(t)
	c := checker(t, w)
	rep := evaluate(t, c, prune.Options{Repo: "widgets"})
	var plan prune.Plan
	for _, it := range rep.Plan(false).Remove {
		if w.Name(it.Worktree.Path) == "merged" {
			plan.Remove = append(plan.Remove, it)
		}
	}
	// Remove the worktree by hand and move the branch: the re-check sees a
	// prunable record, so nothing is touched.
	prunetest.Git(t, w.Repo, "worktree", "remove", w.WT["merged"])
	prunetest.Git(t, w.Repo, "branch", "-f", "merged", w.C0)
	res := c.Apply(context.Background(), plan, prune.Options{Repo: "widgets"}, rep)
	if len(res.Removals) != 1 || res.Removals[0].Removed || res.Removals[0].Skipped == "" {
		t.Errorf("removals: %+v", res.Removals)
	}
	if !branchExists(t, w, "merged") {
		t.Error("moved branch deleted")
	}
}

func TestApplyRecheckError(t *testing.T) {
	parallel(t)
	w := prunetest.BuildWorld(t)
	c := checker(t, w)
	rep := evaluate(t, c, prune.Options{Repo: "widgets"})
	if err := os.RemoveAll(w.Repo); err != nil {
		t.Fatal(err)
	}
	res := c.Apply(context.Background(), rep.Plan(false), prune.Options{Repo: "widgets"}, rep)
	if res.RecheckErr == nil || !res.Failed() || len(res.Removals) != 0 {
		t.Errorf("apply after the repo vanished: %+v", res)
	}
}

func TestDeleteBranchRefusesMovedBranch(t *testing.T) {
	parallel(t)
	w := prunetest.BuildWorld(t)
	s := worktrees.New(worktrees.Options{})
	err := s.DeleteBranch(context.Background(), filepath.Join(w.Repo, ".git"), "nopr", w.C0)
	if !errors.Is(err, worktrees.ErrBranchMoved) {
		t.Errorf("DeleteBranch = %v", err)
	}
	if !slices.Contains(strings.Fields(prunetest.Git(t, w.Repo, "branch", "--list", "nopr")), "nopr") {
		t.Error("branch deleted")
	}
}

// sessionIn is a live interactive session working in dir.
func sessionIn(dir string) sessions.Session {
	return sessions.Session{PID: 777, SessionID: "dddddddd-0000-4000-8000-000000000004", Kind: sessions.Interactive,
		Cwd: dir, Status: sessions.StatusBusy}
}
