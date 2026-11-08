package worktrees

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// opsWorld is a repo with a feature worktree and a fake origin, set up with
// update-ref so nothing is fetched.
type opsWorld struct {
	base, repo, gitDir string
	feat               string // feature worktree path
	c0, c1, c2         string // main's root commit; feature commits c1 then c2
}

func buildOps(t *testing.T) *opsWorld {
	t.Helper()
	requireGit(t)
	w := &opsWorld{base: tempDir(t)}
	w.repo = filepath.Join(w.base, "repo")
	w.gitDir = filepath.Join(w.repo, ".git")
	w.c0 = newRepo(t, w.repo, d1)
	gitT(t, w.repo, "remote", "add", "origin", "git@github.com:acme/widgets.git")
	gitT(t, w.repo, "update-ref", "refs/remotes/origin/main", w.c0)
	w.feat = filepath.Join(w.base, "feat")
	gitT(t, w.repo, "worktree", "add", "-q", "-b", "feat", w.feat)
	w.c1 = commitAt(t, w.feat, "c1", d2)
	w.c2 = commitAt(t, w.feat, "c2", d3)
	return w
}

func TestIgnored(t *testing.T) {
	parallel(t)
	w := buildOps(t)
	write(t, filepath.Join(w.feat, ".gitignore"), "node_modules/\n*.log\n.env\nbuild\n/lambdas/bin/\n")
	gitT(t, w.feat, "add", ".gitignore")
	commitAt(t, w.feat, "ignore", d3)
	for _, f := range []string{"web/node_modules/x/a.js", ".env", "src/a.log", "lambdas/bin/main",
		"onlyignored/q.log", "untracked/u.txt", "untracked/z.log", "build/out/x", "sp ace/a b.log"} {
		write(t, filepath.Join(w.feat, f), "x\n")
	}
	s := New(Options{})
	res := s.Scan(context.Background(), w.repo)
	wt := worktreeByPath(t, res, w.feat)
	got, err := s.Ignored(context.Background(), wt)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	want := []string{".env", "build/", "lambdas/bin/", "onlyignored/q.log", "sp ace/a b.log", "src/a.log", "untracked/z.log", "web/node_modules/"}
	if !slices.Equal(got, want) {
		t.Errorf("Ignored = %q\nwant      %q", got, want)
	}

	// A missing worktree has nothing to read.
	if _, err := s.Ignored(context.Background(), &Worktree{Path: filepath.Join(w.base, "nope"), Missing: true}); err == nil {
		t.Error("Ignored on a missing worktree: no error")
	}
}

func TestParseIgnored(t *testing.T) {
	data := "# branch.oid abc\x001 .M N... 100644 100644 100644 a b f.txt\x002 R. N... 100644 100644 100644 a b R100 new\x00old\x00? u\x00! i/\x00! j k\x00"
	got, err := parseIgnored([]byte(data))
	if err != nil || !slices.Equal(got, []string{"i/", "j k"}) {
		t.Errorf("parseIgnored = %q, %v", got, err)
	}
	if _, err := parseIgnored([]byte("x weird\x00")); err == nil {
		t.Error("parseIgnored accepted an unknown entry")
	}
}

func TestIgnoredHardening(t *testing.T) {
	parallel(t)
	w := buildOps(t)
	fsmon := filepath.Join(w.base, "fsmonitor-ran")
	clean := filepath.Join(w.base, "clean-ran")
	write(t, filepath.Join(w.feat, ".gitattributes"), "*.txt filter=evil\n")
	write(t, filepath.Join(w.feat, "a.txt"), "aaaa\n")
	gitT(t, w.feat, "add", ".")
	commitAt(t, w.feat, "attrs", d3)
	gitT(t, w.repo, "config", "core.fsmonitor", markerScript(t, w.base, "fsmonitor.sh", fsmon))
	gitT(t, w.repo, "config", "filter.evil.clean", "sh -c 'touch "+clean+"; cat'")
	sameSize(t, filepath.Join(w.feat, "a.txt"))

	plainStatus(t, w.feat)
	requireMarker(t, fsmon)
	requireMarker(t, clean)

	s := New(Options{})
	calls := recordCalls(s)
	wt := worktreeByPath(t, s.Scan(context.Background(), w.repo), w.feat)
	os.Remove(fsmon) // the scan itself is A1's to test
	os.Remove(clean)
	scanned := len(calls())
	if _, err := s.Ignored(context.Background(), wt); err != nil {
		t.Fatal(err)
	}
	if exists(fsmon) || exists(clean) {
		t.Errorf("Ignored ran the repo's programs: fsmonitor %v, clean filter %v", exists(fsmon), exists(clean))
	}
	// It reuses the filter drivers the scan listed: one git call, the
	// status, with the evil driver disabled.
	after := calls()[scanned:]
	if len(after) != 1 || !slices.Contains(after[0], "status") || !slices.Contains(after[0], "filter.evil.clean=") {
		t.Errorf("Ignored after a scan ran %q, want one status with filter.evil.clean=", after)
	}

	// A worktree that didn't come from a scan gets its drivers listed.
	scanned = len(calls())
	if _, err := s.Ignored(context.Background(), &Worktree{Path: w.feat}); err != nil {
		t.Fatal(err)
	}
	if exists(clean) {
		t.Error("Ignored without a scan ran the clean filter")
	}
	after = calls()[scanned:]
	if len(after) != 2 || !slices.Contains(after[0], "config") || !slices.Contains(after[1], "filter.evil.clean=") {
		t.Errorf("Ignored without a scan ran %q, want config then status", after)
	}
}

func TestResolveCountAncestor(t *testing.T) {
	parallel(t)
	w := buildOps(t)
	s := New(Options{})
	ctx := context.Background()

	if got, err := s.ResolveCommit(ctx, w.gitDir, w.c1); err != nil || got != w.c1 {
		t.Errorf("ResolveCommit(c1) = %q, %v", got, err)
	}
	if got, err := s.ResolveCommit(ctx, w.gitDir, "refs/remotes/origin/main"); err != nil || got != w.c0 {
		t.Errorf("ResolveCommit(origin/main) = %q, %v", got, err)
	}
	for _, rev := range []string{strings.Repeat("ab", 20), "refs/remotes/origin/nope", "--all"} {
		if _, err := s.ResolveCommit(ctx, w.gitDir, rev); !errors.Is(err, ErrNoCommit) {
			t.Errorf("ResolveCommit(%q) = %v, want ErrNoCommit", rev, err)
		}
	}
	// A tree is not a commit.
	tree := gitT(t, w.repo, "rev-parse", "HEAD^{tree}")
	if _, err := s.ResolveCommit(ctx, w.gitDir, tree); !errors.Is(err, ErrNoCommit) {
		t.Errorf("ResolveCommit(tree) = %v, want ErrNoCommit", err)
	}

	for _, tc := range []struct {
		tip  string
		not  []string
		want int
	}{
		{w.c2, []string{w.c2}, 0},
		{w.c1, []string{w.c2}, 0},
		{w.c2, []string{w.c1}, 1},
		{w.c2, []string{w.c1, "refs/remotes/origin/main"}, 1},
		{w.c2, []string{"refs/remotes/origin/main"}, 2},
	} {
		if got, err := s.CountNotIn(ctx, w.gitDir, tc.tip, tc.not...); err != nil || got != tc.want {
			t.Errorf("CountNotIn(%.7s, %v) = %d, %v; want %d", tc.tip, tc.not, got, err, tc.want)
		}
	}
	if _, err := s.CountNotIn(ctx, w.gitDir, w.c2, strings.Repeat("ab", 20)); err == nil {
		t.Error("CountNotIn with a missing commit: no error")
	}

	if ok, err := s.IsAncestor(ctx, w.gitDir, w.c0, w.c2); err != nil || !ok {
		t.Errorf("IsAncestor(c0, c2) = %v, %v", ok, err)
	}
	if ok, err := s.IsAncestor(ctx, w.gitDir, w.c2, "refs/remotes/origin/main"); err != nil || ok {
		t.Errorf("IsAncestor(c2, origin/main) = %v, %v", ok, err)
	}
	if _, err := s.IsAncestor(ctx, w.gitDir, strings.Repeat("ab", 20), w.c2); err == nil {
		t.Error("IsAncestor with a missing commit: no error")
	}
}

func TestRemoteHead(t *testing.T) {
	parallel(t)
	w := buildOps(t)
	s := New(Options{})
	ctx := context.Background()
	if got, err := s.RemoteHead(ctx, w.gitDir, "origin"); err != nil || got != "" {
		t.Errorf("RemoteHead unset = %q, %v", got, err)
	}
	gitT(t, w.repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	if got, err := s.RemoteHead(ctx, w.gitDir, "origin"); err != nil || got != "main" {
		t.Errorf("RemoteHead = %q, %v", got, err)
	}
}

func TestRemoveWorktreeAndBranch(t *testing.T) {
	parallel(t)
	w := buildOps(t)
	s := New(Options{})
	ctx := context.Background()
	scan := func() (*Repo, *Worktree) {
		res := s.Scan(ctx, w.repo)
		return repoByPath(t, res, w.repo), worktreeByPath(t, res, w.feat)
	}

	// Refused while it has untracked files, and while locked.
	write(t, filepath.Join(w.feat, "new.txt"), "x\n")
	repo, wt := scan()
	if err := s.RemoveWorktree(ctx, repo, wt); err == nil || !exists(w.feat) {
		t.Fatalf("removed a worktree with an untracked file: %v", err)
	}
	os.Remove(filepath.Join(w.feat, "new.txt"))
	gitT(t, w.repo, "worktree", "lock", w.feat)
	repo, wt = scan()
	if err := s.RemoveWorktree(ctx, repo, wt); err == nil || !exists(w.feat) {
		t.Fatalf("removed a locked worktree: %v", err)
	}
	gitT(t, w.repo, "worktree", "unlock", w.feat)

	// The main worktree is refused before git runs.
	if err := s.RemoveWorktree(ctx, repo, repo.Main()); err == nil {
		t.Error("RemoveWorktree accepted the main worktree")
	}

	// An ignored file doesn't stop git (that is what Ignored is for).
	write(t, filepath.Join(w.gitDir, "info", "exclude"), "*.log\n")
	write(t, filepath.Join(w.feat, "local.log"), "x\n")
	repo, wt = scan()
	if err := s.RemoveWorktree(ctx, repo, wt); err != nil {
		t.Fatal(err)
	}
	if exists(w.feat) {
		t.Error("worktree directory still there")
	}

	// The branch is deleted only while it is where it was checked.
	if err := s.DeleteBranch(ctx, w.gitDir, "feat", w.c1); !errors.Is(err, ErrBranchMoved) {
		t.Errorf("DeleteBranch at the wrong commit = %v, want ErrBranchMoved", err)
	}
	if err := s.DeleteBranch(ctx, w.gitDir, "feat", "nonsense"); err == nil {
		t.Error("DeleteBranch accepted a non-SHA")
	}
	if err := s.DeleteBranch(ctx, w.gitDir, "feat", w.c2); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveCommit(ctx, w.gitDir, "refs/heads/feat"); !errors.Is(err, ErrNoCommit) {
		t.Errorf("branch still there: %v", err)
	}
	if err := s.DeleteBranch(ctx, w.gitDir, "feat", w.c2); !errors.Is(err, ErrNoCommit) {
		t.Errorf("DeleteBranch of a deleted branch = %v", err)
	}
}

func TestDeleteBranchCheckedOut(t *testing.T) {
	parallel(t)
	w := buildOps(t)
	s := New(Options{})
	if err := s.DeleteBranch(context.Background(), w.gitDir, "feat", w.c2); err == nil {
		t.Error("deleted a branch checked out in a worktree")
	}
}

func TestRemoveWorktreeHardening(t *testing.T) {
	parallel(t)
	w := buildOps(t)
	fsmon := filepath.Join(w.base, "fsmonitor-ran")
	clean := filepath.Join(w.base, "clean-ran")
	write(t, filepath.Join(w.feat, ".gitattributes"), "*.txt filter=evil\n")
	write(t, filepath.Join(w.feat, "a.txt"), "aaaa\n")
	gitT(t, w.feat, "add", ".")
	commitAt(t, w.feat, "attrs", d3)
	gitT(t, w.repo, "config", "core.fsmonitor", markerScript(t, w.base, "fsmonitor.sh", fsmon))
	gitT(t, w.repo, "config", "filter.evil.clean", "sh -c 'touch "+clean+"; cat'")
	// Same size and mtime-dirty, so git's status in remove must read it.
	sameSize(t, filepath.Join(w.feat, "a.txt"))

	plainStatus(t, w.feat)
	requireMarker(t, fsmon)
	requireMarker(t, clean)

	s := New(Options{})
	res := s.Scan(context.Background(), w.repo)
	repo, wt := repoByPath(t, res, w.repo), worktreeByPath(t, res, w.feat)
	os.Remove(fsmon)
	os.Remove(clean)
	// With the filter disabled the edit shows as a change, so git refuses.
	err := s.RemoveWorktree(context.Background(), repo, wt)
	if exists(fsmon) || exists(clean) {
		t.Errorf("worktree remove ran the repo's programs: fsmonitor %v, clean filter %v", exists(fsmon), exists(clean))
	}
	if err == nil || !exists(w.feat) {
		t.Errorf("worktree remove with a changed file: err %v, dir kept %v", err, exists(w.feat))
	}
}

func TestPrunePreviewAndPrune(t *testing.T) {
	parallel(t)
	w := buildOps(t)
	s := New(Options{})
	ctx := context.Background()

	gone := filepath.Join(w.base, "gone")
	locked := filepath.Join(w.base, "locked")
	for _, p := range []string{gone, locked} {
		gitT(t, w.repo, "worktree", "add", "-q", "--detach", p)
	}
	gitT(t, w.repo, "worktree", "lock", locked)
	os.RemoveAll(gone)
	os.RemoveAll(locked)

	got, err := s.PrunePreview(ctx, w.gitDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "gone" || got[0].Path != gone || !strings.Contains(got[0].Reason, "non-existent") {
		t.Fatalf("PrunePreview = %+v", got)
	}

	// An admin dir with no gitdir file never shows in `worktree list`, but
	// prune would clear it.
	broken := filepath.Join(w.gitDir, "worktrees", "broken")
	write(t, filepath.Join(broken, "HEAD"), "ref: refs/heads/main\n")
	got, err = s.PrunePreview(ctx, w.gitDir)
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(got, func(a, b PruneEntry) int { return strings.Compare(a.ID, b.ID) })
	if len(got) != 2 || got[0].ID != "broken" || got[0].Path != "" || got[1].ID != "gone" {
		t.Fatalf("PrunePreview with a broken record = %+v", got)
	}
	if !exists(filepath.Join(w.gitDir, "worktrees", "gone")) {
		t.Fatal("the preview pruned something")
	}

	if err := s.Prune(ctx, w.gitDir); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(w.gitDir, "worktrees", "gone")) || exists(broken) {
		t.Error("prune left a stale record")
	}
	if !exists(filepath.Join(w.gitDir, "worktrees", "locked")) {
		t.Error("prune removed a locked record")
	}
	if got, err := s.PrunePreview(ctx, w.gitDir); err != nil || len(got) != 0 {
		t.Errorf("PrunePreview after prune = %+v, %v", got, err)
	}
}
