package prune_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravexa-llc/ai-manager/internal/prune"
	"github.com/gravexa-llc/ai-manager/internal/prune/prunetest"
)

// pick is the Pick for a world case, as a page would have shown it.
func pick(w *prunetest.World, name string) prune.Pick {
	branch := name
	if strings.HasPrefix(name, "detached") || strings.HasPrefix(name, "scratch") || name == "gone" || name == "nogit" {
		branch = ""
	}
	if name == "main" {
		branch = "main"
	}
	return prune.Pick{Path: w.WT[name], Head: w.Head[name], Branch: branch}
}

func picked(t *testing.T, w *prunetest.World, res []prune.Picked) map[string]prune.Picked {
	t.Helper()
	out := map[string]prune.Picked{}
	for _, r := range res {
		name := w.Name(r.Pick.Path)
		if name == "" {
			name = r.Pick.Path
		}
		if _, dup := out[name]; dup {
			name += " (again)"
		}
		out[name] = r
	}
	return out
}

func TestRemovePicked(t *testing.T) {
	parallel(t)
	w := prunetest.BuildWorld(t)
	c := checker(t, w)
	calls := w.GitHub.CallCount()
	notRepo := filepath.Join(w.Root, "not-a-repo")
	picks := []prune.Pick{
		pick(w, "merged"), pick(w, "detached-main"),
		pick(w, "main"), pick(w, "locked-live"), pick(w, "locked-stale"), pick(w, "dirty"), pick(w, "gone"),
		pick(w, "closed"), pick(w, "incwd"),
		{Path: notRepo},
		pick(w, "merged"),
	}
	res, err := c.RemovePicked(context.Background(), prune.Options{Repo: w.Repo}, picks)
	if err != nil {
		t.Fatal(err)
	}
	if w.GitHub.CallCount() != calls+1 {
		t.Errorf("GitHub asked %d times, want once: the re-check reads it fresh", w.GitHub.CallCount()-calls)
	}
	if len(res) != len(picks) {
		t.Fatalf("%d results for %d picks", len(res), len(picks))
	}
	got := picked(t, w, res)
	for _, name := range []string{"merged", "detached-main"} {
		r := got[name]
		if !r.Removed || r.Skipped != "" || r.Err != nil || r.Item == nil {
			t.Errorf("%s: %+v", name, r)
		}
		if prunetest.Exists(w.WT[name]) {
			t.Errorf("%s: folder still there", name)
		}
	}
	if !got["merged"].BranchDeleted || branchExists(t, w, "merged") {
		t.Error("merged: branch not deleted")
	}
	if got["detached-main"].BranchDeleted {
		t.Error("detached-main: deleted a branch")
	}
	for name, want := range map[string]string{
		"main":           "a main checkout is never removed",
		"locked-live":    "locked, never removed: locked by agent",
		"locked-stale":   "locked, never removed: stale lock",
		"dirty":          "no longer safe: 1 uncommitted file; 1 untracked file",
		"gone":           "its folder is gone",
		"closed":         "only aim prune --include-closed removes it",
		"incwd":          "no longer safe: in use: cwd of interactive session",
		notRepo:          "git doesn't list a worktree there now",
		"merged (again)": "picked twice",
	} {
		r := got[name]
		if r.Removed || r.Err != nil || !strings.Contains(r.Skipped, want) {
			t.Errorf("%s: removed %v, err %v, skipped %q; want skipped with %q", name, r.Removed, r.Err, r.Skipped, want)
		}
	}
	for _, name := range []string{"main", "locked-live", "locked-stale", "dirty", "closed", "incwd"} {
		if !prunetest.Exists(w.WT[name]) {
			t.Errorf("%s: folder removed", name)
		}
	}
	for _, name := range []string{"locked-live", "locked-stale", "dirty", "closed", "incwd"} {
		if !branchExists(t, w, name) {
			t.Errorf("%s: branch deleted", name)
		}
	}
	if !prunetest.Exists(notRepo) {
		t.Error("a folder git doesn't list was removed")
	}
	// The gone record is still git's: prune is a separate action.
	if !prunetest.Exists(filepath.Join(w.Repo, ".git", "worktrees", "gone")) {
		t.Error("the prunable record was cleared")
	}
}

// TestRemovePickedRechecks changes worktrees between the look and the
// removal: each one that turned unsafe, or moved, is skipped.
func TestRemovePickedRechecks(t *testing.T) {
	parallel(t)
	w := prunetest.BuildWorld(t)
	c := checker(t, w)
	picks := []prune.Pick{pick(w, "merged"), pick(w, "squash"), pick(w, "ff"), pick(w, "donejob"), pick(w, "basemissing"), pick(w, "scratch-dead")}
	// Shown on another commit than it is on: safe, but not what was shown.
	picks[4].Head = w.C0

	prunetest.Write(t, filepath.Join(w.WT["merged"], "late.txt"), "x\n")
	prunetest.Write(t, filepath.Join(w.WT["squash"], "late.txt"), "x\n")
	prunetest.Commit(t, w.WT["squash"], "late commit")
	w.Sessions.Add(sessionIn(w.WT["ff"]))
	prunetest.Git(t, w.Repo, "worktree", "lock", w.WT["donejob"])

	res, err := c.RemovePicked(context.Background(), prune.Options{Repo: w.Repo}, picks)
	if err != nil {
		t.Fatal(err)
	}
	got := picked(t, w, res)
	for name, want := range map[string]string{
		"merged":      "no longer safe: 1 untracked file",
		"squash":      "no longer safe: 1 commit not in PR #2",
		"ff":          "no longer safe: in use: cwd of interactive session",
		"donejob":     "locked, never removed",
		"basemissing": "changed since it was shown: it is on basemissing at " + w.Head["basemissing"][:7] + " now, not on basemissing at " + w.C0[:7],
	} {
		r := got[name]
		if r.Removed || !strings.Contains(r.Skipped, want) {
			t.Errorf("%s: removed %v, skipped %q; want skipped with %q", name, r.Removed, r.Skipped, want)
		}
		if !prunetest.Exists(w.WT[name]) || !branchExists(t, w, name) {
			t.Errorf("%s: folder or branch removed", name)
		}
	}
	if r := got["scratch-dead"]; !r.Removed || prunetest.Exists(w.WT["scratch-dead"]) {
		t.Errorf("scratch-dead: %+v", r)
	}
}

func TestRemovePickedRecheckError(t *testing.T) {
	parallel(t)
	w := prunetest.BuildWorld(t)
	c := checker(t, w)
	if err := os.RemoveAll(w.Repo); err != nil {
		t.Fatal(err)
	}
	res, err := c.RemovePicked(context.Background(), prune.Options{Repo: w.Repo}, []prune.Pick{pick(w, "merged")})
	if err == nil || len(res) != 0 {
		t.Errorf("RemovePicked after the repo vanished = %+v, %v", res, err)
	}
	if !prunetest.Exists(w.WT["merged"]) {
		t.Error("merged removed")
	}
}

func TestPruneListed(t *testing.T) {
	parallel(t)
	w := prunetest.BuildWorld(t)
	c := checker(t, w)
	ctx := context.Background()
	calls := w.GitHub.CallCount()
	admin := func(name string) bool { return prunetest.Exists(filepath.Join(w.Repo, ".git", "worktrees", name)) }

	// Only one of the two records listed: prune would clear the other too,
	// so it doesn't run.
	run, err := c.PruneListed(ctx, prune.Options{Repo: w.Repo}, []string{w.WT["gone"]})
	if err != nil || !strings.Contains(run.Skipped, "weren't listed") || len(run.Pruned) != 0 || !admin("gone") {
		t.Fatalf("one of two listed: %+v, %v", run, err)
	}
	// Paths that aren't prunable records count for nothing.
	run, err = c.PruneListed(ctx, prune.Options{Repo: w.Repo}, []string{w.WT["merged"], w.Repo})
	if err != nil || run.Skipped != "none of the records shown is prunable now" || !admin("gone") {
		t.Fatalf("no record listed: %+v, %v", run, err)
	}
	// Both: they go, and the nogit folder's own files stay.
	run, err = c.PruneListed(ctx, prune.Options{Repo: w.Repo}, []string{w.WT["gone"], w.WT["nogit"]})
	if err != nil || run.Skipped != "" || run.Err != nil || len(run.Pruned) != 2 || admin("gone") || admin("nogit") {
		t.Fatalf("both listed: %+v, %v", run, err)
	}
	if !prunetest.Exists(filepath.Join(w.WT["nogit"], "keep.txt")) {
		t.Error("prune deleted the nogit folder's files")
	}
	// Locked records stay git's.
	for _, name := range []string{"locked-live", "locked-stale"} {
		if !prunetest.Exists(w.WT[name]) {
			t.Errorf("%s removed", name)
		}
	}
	if w.GitHub.CallCount() != calls {
		t.Error("pruning records asked GitHub")
	}
	if _, err := c.PruneListed(ctx, prune.Options{}, nil); err == nil {
		t.Error("PruneListed without a repo succeeded")
	}
}
