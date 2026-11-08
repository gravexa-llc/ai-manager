package worktrees

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGitPruneBehaviour pins down, on a temp repo, the git behaviour
// `aim prune` relies on. It tests git, not this package.
func TestGitPruneBehaviour(t *testing.T) {
	parallel(t)
	requireGit(t)
	base := tempDir(t)
	repo := filepath.Join(base, "r")
	newRepo(t, repo, d1)
	admin := func(name string) string { return filepath.Join(repo, ".git", "worktrees", name) }
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	try := func(args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		out, err := cmd.CombinedOutput()
		t.Logf("git %s: err=%v\n%s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		return string(out), err
	}

	gone1 := filepath.Join(base, "gone1")   // directory removed
	gone2 := filepath.Join(base, "gone2")   // directory removed
	locked := filepath.Join(base, "locked") // locked, directory removed
	nogit := filepath.Join(base, "nogit")   // directory kept, .git file removed
	for _, p := range []string{gone1, gone2, locked, nogit} {
		gitT(t, repo, "worktree", "add", "-q", "--detach", p)
	}
	gitT(t, repo, "worktree", "lock", locked)
	for _, p := range []string{gone1, gone2, locked} {
		os.RemoveAll(p)
	}
	os.Remove(filepath.Join(nogit, ".git"))
	write(t, filepath.Join(nogit, "keep.txt"), "user file\n")

	// `git worktree remove` (no --force) works on a missing directory: it
	// just clears the admin data; it doesn't need prune.
	if _, err := try("worktree", "remove", gone1); err != nil || exists(admin("gone1")) {
		t.Errorf("worktree remove on a missing directory: err %v, admin dir left: %v", err, exists(admin("gone1")))
	}
	// It refuses a directory whose .git file is gone, and a locked worktree.
	if _, err := try("worktree", "remove", nogit); err == nil || !exists(admin("nogit")) {
		t.Errorf("worktree remove without a .git file: err %v", err)
	}
	if _, err := try("worktree", "remove", locked); err == nil || !exists(admin("locked")) {
		t.Errorf("worktree remove on a locked worktree: err %v", err)
	}

	// prune -n -v names what prune would clear, and clears nothing.
	dry, err := try("worktree", "prune", "-n", "-v")
	if err != nil || !strings.Contains(dry, "worktrees/gone2:") || !strings.Contains(dry, "worktrees/nogit:") ||
		strings.Contains(dry, "worktrees/locked") {
		t.Errorf("prune -n -v should name gone2 and nogit, not locked: %v\n%s", err, dry)
	}
	if !exists(admin("gone2")) || !exists(admin("nogit")) {
		t.Error("prune -n removed something")
	}

	// prune clears both, keeps the locked one, and never touches a
	// worktree directory's files.
	if _, err := try("worktree", "prune"); err != nil {
		t.Fatal(err)
	}
	if exists(admin("gone2")) || exists(admin("nogit")) {
		t.Error("prune left a stale worktree")
	}
	if !exists(admin("locked")) {
		t.Error("prune removed a locked worktree")
	}
	if !exists(filepath.Join(nogit, "keep.txt")) {
		t.Error("prune deleted files in the worktree directory")
	}
}
