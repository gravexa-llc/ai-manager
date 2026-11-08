package main

import (
	"strings"
	"testing"

	"github.com/gravexa-llc/ai-manager/internal/prune/prunetest"
)

// sectionOf returns the report section whose title starts with title.
func sectionOf(t *testing.T, out, title string) string {
	t.Helper()
	i := strings.Index(out, "\n"+title)
	if !strings.HasPrefix(out, title) && i < 0 {
		t.Errorf("no section %q in:\n%s", title, out)
		return ""
	}
	rest := out[i+1:]
	if strings.HasPrefix(out, title) {
		rest = out
	}
	end := strings.Index(rest, "\n\n")
	if end < 0 {
		return rest
	}
	return rest[:end]
}

func TestPruneDryRun(t *testing.T) {
	w := world(t)
	code, out, errOut := aim(t, "", "prune")
	if code != 0 || errOut != "" {
		t.Fatalf("aim prune = %d, stderr %q", code, errOut)
	}
	sections := map[string][]string{
		"Safe to remove, PR merged (5):":                                              {"merged", "squash", "ff", "donejob", "basemissing"},
		"Safe to remove, PR closed without merging (only with --include-closed) (1):": {"closed"},
		"Scratch: detached, HEAD on the remote (3):":                                  {"detached-main", "detached-surge", "scratch-dead"},
		"Locked: never removed (4):":                                                  {"locked-live", "locked-stale", "locked-reused", "locked-other"},
		"Prunable records: folder or its .git gone (2):":                              {"gone", "nogit"},
		"Not safe (14):": {"after", "headmissing", "atrisk", "open", "nopr", "fork", "dirty", "incwd", "stalegh",
			"many", "lfs", "detached-local", "scratch-live", "plain-feature"},
	}
	for title, names := range sections {
		sec := sectionOf(t, out, title)
		for _, n := range names {
			if !strings.Contains(sec, w.WT[n]+" ") {
				t.Errorf("%s not in section %q:\n%s", n, title, sec)
			}
		}
		if got := strings.Count(sec, "\n"); got != len(names) {
			t.Errorf("section %q has %d lines, want %d", title, got, len(names))
		}
	}
	if !strings.Contains(out, "Main checkouts (2) are never candidates.") {
		t.Error("no main checkout count")
	}
	want := "Dry run: nothing was removed. `aim prune --apply` would remove 8 worktrees and delete 5 branches, and run git worktree prune in 1 repo for 2 records."
	if !strings.Contains(out, want) {
		t.Errorf("no summary %q in:\n%s", want, out)
	}
	for name, p := range w.WT {
		if name != "gone" && !prunetest.Exists(p) {
			t.Errorf("dry run removed %s", name)
		}
	}
}

func TestPruneIncludeClosedDryRun(t *testing.T) {
	world(t)
	_, out, _ := aim(t, "", "prune", "--include-closed", "--repo", "widgets")
	if !strings.Contains(out, "Safe to remove, PR closed without merging (1):") ||
		!strings.Contains(out, "would remove 9 worktrees and delete 6 branches") {
		t.Errorf("aim prune --include-closed:\n%s", out)
	}
}

func TestPruneApplyYes(t *testing.T) {
	w := world(t)
	code, out, errOut := aim(t, "", "prune", "--apply", "--yes")
	if code != 0 || errOut != "" {
		t.Fatalf("aim prune --apply --yes = %d, stderr %q\n%s", code, errOut, out)
	}
	if strings.Contains(out, "Continue?") {
		t.Error("asked for confirmation despite --yes")
	}
	for _, n := range []string{"merged", "squash", "ff", "donejob", "basemissing"} {
		if l := actionLine(out, "removed", w.WT[n]); !strings.HasSuffix(l, "branch "+n+" deleted") {
			t.Errorf("%s: removal line %q", n, l)
		}
		if prunetest.Exists(w.WT[n]) {
			t.Errorf("%s: still there", n)
		}
	}
	for _, n := range []string{"detached-main", "detached-surge", "scratch-dead"} {
		if l := actionLine(out, "removed", w.WT[n]); !strings.HasSuffix(l, " detached") {
			t.Errorf("%s: removal line %q", n, l)
		}
	}
	if l := actionLine(out, "pruned", w.Repo); !strings.Contains(l, " 2 records: "+w.WT["gone"]+", "+w.WT["nogit"]) {
		t.Errorf("prune line %q in:\n%s", l, out)
	}
	if !strings.Contains(out, "Removed 8 worktrees, deleted 5 branches, pruned 2 records; 0 skipped, 0 failed.") {
		t.Errorf("no summary in:\n%s", out)
	}
	for _, n := range []string{"closed", "atrisk", "after", "locked-live", "scratch-live", "incwd", "plain-feature"} {
		if !prunetest.Exists(w.WT[n]) {
			t.Errorf("%s: removed", n)
		}
	}

	// Again: nothing left.
	code, out, _ = aim(t, "", "prune", "--apply", "--yes")
	if code != 0 || !strings.Contains(out, "Nothing to remove.") {
		t.Errorf("second apply = %d:\n%s", code, out)
	}
}

func TestPruneApplyConfirm(t *testing.T) {
	w := world(t)
	for _, input := range []string{"", "n\n", "no\n", "maybe\n"} {
		code, out, errOut := aim(t, input, "prune", "--apply", "--repo", "widgets")
		if code != 1 || !strings.Contains(out, "Continue? [y/N]") || !strings.Contains(errOut, "not confirmed") {
			t.Errorf("answer %q: %d, stderr %q", input, code, errOut)
		}
		if !prunetest.Exists(w.WT["merged"]) {
			t.Fatalf("answer %q removed a worktree", input)
		}
	}
	code, out, _ := aim(t, "Y\n", "prune", "--apply", "--repo", "widgets")
	if code != 0 || prunetest.Exists(w.WT["merged"]) || !strings.Contains(out, "Removed 8 worktrees") {
		t.Errorf("answer y: %d\n%s", code, out)
	}
}

func TestPruneFlags(t *testing.T) {
	world(t)
	if code, _, errOut := aim(t, "", "prune", "--yes"); code != 2 || !strings.Contains(errOut, "--yes only makes sense with --apply") {
		t.Errorf("aim prune --yes = %d, %q", code, errOut)
	}
	if code, _, _ := aim(t, "", "prune", "now"); code != 2 {
		t.Errorf("aim prune now = %d", code)
	}
	if code, _, errOut := aim(t, "", "prune", "--help"); code != 0 || !strings.Contains(errOut, "usage: aim prune") {
		t.Errorf("aim prune --help = %d, %q", code, errOut)
	}
}

func TestPruneNothingToDo(t *testing.T) {
	world(t)
	code, out, _ := aim(t, "", "prune", "--repo", "plain")
	if code != 0 || !strings.Contains(out, "Dry run: nothing to remove.") {
		t.Errorf("aim prune --repo plain = %d:\n%s", code, out)
	}
	code, out, _ = aim(t, "", "prune", "--repo", "plain", "--apply")
	if code != 0 || !strings.Contains(out, "Nothing to remove.") || strings.Contains(out, "Continue?") {
		t.Errorf("aim prune --repo plain --apply = %d:\n%s", code, out)
	}
}
