// Package hooks tests the pre-commit hook and its installer (the shell
// scripts beside this file) with the repo's .gitleaks.toml, in temp git
// repos. The tests need gitleaks and are skipped without it; scripts/ci.sh
// requires it, so CI always runs them.
package hooks

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gravexa-llc/ai-manager/internal/testenv"
)

func TestMain(m *testing.M) { os.Exit(testenv.Main(m)) }

// realShaped is a credential gitleaks' own rules find and .gitleaks.toml
// doesn't excuse. It is put together here so this file holds no match
// itself: `make secrets` scans it too.
var realShaped = "gh" + "p_" + "wJ4kLm9QzX2vB7nR" + "5tY8uE3aD6fG1hS0pCiO"

// madeUp is one of the test values .gitleaks.toml excuses in test files.
const madeUp = "ghp_0123456789abcdefABCDEF0123456789abcd"

// repo is a temp git repo with the hook's files copied in, as a clone has them.
type repo struct {
	t   *testing.T
	dir string
}

func newRepo(t *testing.T) *repo {
	t.Helper()
	if _, err := exec.LookPath("gitleaks"); err != nil {
		t.Skip("gitleaks is not on PATH")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := &repo{t: t, dir: dir}
	r.git("init", "-q", "-b", "main")
	for from, to := range map[string]string{
		"pre-commit":           "scripts/hooks/pre-commit",
		"install":              "scripts/hooks/install",
		"../../.gitleaks.toml": ".gitleaks.toml",
	} {
		data, err := os.ReadFile(from)
		if err != nil {
			t.Fatal(err)
		}
		r.write(to, string(data), 0o755)
	}
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "the hook's files")
	return r
}

// run runs a program in dir with a clean git environment: no global or
// system config, so a hook or hooksPath of the machine's can't take part.
func run(t *testing.T, dir, name string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (r *repo) git(args ...string) string {
	r.t.Helper()
	out, err := run(r.t, r.dir, "git", args...)
	if err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func (r *repo) write(name, content string, mode os.FileMode) {
	r.t.Helper()
	path := filepath.Join(r.dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		r.t.Fatal(err)
	}
}

func (r *repo) install() string {
	r.t.Helper()
	out, err := run(r.t, r.dir, "sh", "scripts/hooks/install")
	if err != nil {
		r.t.Fatalf("install: %v\n%s", err, out)
	}
	return out
}

// commit stages one file in dir and commits it; the output and whether git made the commit.
func commit(t *testing.T, dir, name, content string) (string, bool) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, dir, "git", "add", name); err != nil {
		t.Fatalf("git add %s: %v\n%s", name, err, out)
	}
	out, err := run(t, dir, "git", "commit", "-q", "-m", "add "+name)
	return out, err == nil
}

func TestPreCommitRefusesACredential(t *testing.T) {
	r := newRepo(t)
	r.install()

	if out, ok := commit(t, r.dir, "notes.md", "nothing secret here\n"); !ok {
		t.Fatalf("an ordinary commit was refused:\n%s", out)
	}

	out, ok := commit(t, r.dir, "deploy.sh", "export GITHUB_TOKEN="+realShaped+"\n")
	if ok {
		t.Fatal("a commit with a credential went through")
	}
	if !strings.Contains(out, "looks like a credential") || !strings.Contains(out, "deploy.sh") {
		t.Errorf("the refusal doesn't say what and where:\n%s", out)
	}
	if strings.Contains(out, realShaped) {
		t.Errorf("the refusal prints the credential:\n%s", out)
	}
	r.git("reset", "-q", "--hard")

	// Only what is staged counts: the same file left unstaged blocks nothing.
	r.write("scratch.txt", "token "+realShaped+"\n", 0o644)
	if out, ok := commit(t, r.dir, "more.md", "more notes\n"); !ok {
		t.Errorf("an unstaged credential blocked another file's commit:\n%s", out)
	}
}

func TestMadeUpValuesPassOnlyInTests(t *testing.T) {
	r := newRepo(t)
	r.install()

	if out, ok := commit(t, r.dir, "redact_test.go", "package x\n\nconst sample = \""+madeUp+"\"\n"); !ok {
		t.Errorf("the tests' made-up value was refused in a test file:\n%s", out)
	}
	if out, ok := commit(t, r.dir, "wait.test.ts", "const sample = '"+madeUp+"'\n"); !ok {
		t.Errorf("the tests' made-up value was refused in a mod test:\n%s", out)
	}
	// The same value outside a test, and a real-shaped one inside a test, are both found.
	if _, ok := commit(t, r.dir, "config.go", "package x\n\nconst token = \""+madeUp+"\"\n"); ok {
		t.Error("the made-up value went through outside a test file")
	}
	r.git("reset", "-q", "--hard")
	if _, ok := commit(t, r.dir, "other_test.go", "package x\n\nconst token = \""+realShaped+"\"\n"); ok {
		t.Error("another credential went through because it was in a test file")
	}
}

func TestHookCoversEveryWorktree(t *testing.T) {
	r := newRepo(t)
	out := r.install()
	hook := filepath.Join(r.dir, ".git", "hooks", "pre-commit")
	if info, err := os.Stat(hook); err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("no executable %s after install (%v):\n%s", hook, err, out)
	}

	// A worktree on a branch from before the hook's files existed.
	r.git("checkout", "-q", "--orphan", "old")
	r.git("rm", "-q", "-rf", ".")
	r.write("README.md", "an old branch\n", 0o644)
	r.git("add", "-A")
	r.git("commit", "-q", "-m", "old")
	r.git("checkout", "-q", "main")
	wt := filepath.Join(filepath.Dir(r.dir), filepath.Base(r.dir)+"-wt")
	r.git("worktree", "add", "-q", wt, "old")
	t.Cleanup(func() { os.RemoveAll(wt) })

	if out, ok := commit(t, wt, "fine.md", "fine\n"); !ok {
		t.Fatalf("an ordinary commit in a worktree was refused:\n%s", out)
	}
	if _, ok := commit(t, wt, "leak.env", "GITHUB_TOKEN="+realShaped+"\n"); ok {
		t.Error("a credential went through in a worktree without the hook's files")
	}

	// Installing from a worktree lands in the same shared folder.
	r.git("worktree", "add", "-q", "--detach", wt+"2", "main")
	t.Cleanup(func() { os.RemoveAll(wt + "2") })
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, wt+"2", "sh", "scripts/hooks/install"); err != nil {
		t.Fatalf("install from a worktree: %v\n%s", err, out)
	}
	if _, err := os.Stat(hook); err != nil {
		t.Errorf("install from a worktree didn't write %s", hook)
	}
}

func TestInstallLeavesOtherHooksAlone(t *testing.T) {
	r := newRepo(t)
	hook := filepath.Join(r.dir, ".git", "hooks", "pre-commit")
	const own = "#!/bin/sh\n# someone's own hook\nexit 0\n"
	if err := os.WriteFile(hook, []byte(own), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, r.dir, "sh", "scripts/hooks/install")
	if err == nil || !strings.Contains(out, "a hook of your own") {
		t.Errorf("install over another hook: err %v\n%s", err, out)
	}
	if got, _ := os.ReadFile(hook); string(got) != own {
		t.Error("install changed a hook that isn't ours")
	}

	// Its own earlier copy is replaced.
	if err := os.WriteFile(hook, []byte("#!/bin/sh\n# ai-manager pre-commit hook: an older one\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r.install()
	want, _ := os.ReadFile("pre-commit")
	if got, _ := os.ReadFile(hook); string(got) != string(want) {
		t.Error("install didn't replace its own older hook")
	}

	// core.hooksPath set: git wouldn't read the shared folder, so say so.
	r.git("config", "core.hooksPath", "elsewhere")
	out, err = run(t, r.dir, "sh", "scripts/hooks/install")
	if err == nil || !strings.Contains(out, "core.hooksPath") {
		t.Errorf("install with core.hooksPath set: err %v\n%s", err, out)
	}
}
