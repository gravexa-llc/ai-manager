package worktrees

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/testenv"
)

// TestMain isolates git from the machine: no system or global config (one
// test-only global config instead), fixed identities, and a ceiling at the
// temp dir so no git call in a test can find a repo outside it. An ssh
// that always fails comes first on PATH, so a scanner a test makes
// without a fake ssh (Options.SSH) never reads the real ssh config.
func TestMain(m *testing.M) {
	done := testenv.Setup() // a small TMPDIR, and the fork watchdog (see testenv)
	// The fake ssh is a shell script: under the race detector on a busy
	// machine it can take longer than the real cap to start (sshhost.go).
	sshTimeout = 30 * time.Second
	os.Exit(done(func() int {
		dir, err := os.MkdirTemp("", "aim-a1-git")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer os.RemoveAll(dir)
		cfg := filepath.Join(dir, "gitconfig")
		const body = "[init]\n\tdefaultBranch = main\n[protocol \"file\"]\n\tallow = always\n[advice]\n\tdetachedHead = false\n"
		if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		tmp, err := filepath.EvalSymlinks(os.TempDir())
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		bin := filepath.Join(dir, "bin")
		if err := os.Mkdir(bin, 0o755); err == nil {
			err = os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\nexit 255\n"), 0o755)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		for k, v := range map[string]string{
			"PATH":                    bin + string(os.PathListSeparator) + os.Getenv("PATH"),
			"GIT_CONFIG_GLOBAL":       cfg,
			"GIT_CONFIG_NOSYSTEM":     "1",
			"GIT_CEILING_DIRECTORIES": tmp,
			"GIT_AUTHOR_NAME":         "Test",
			"GIT_AUTHOR_EMAIL":        "test@example.com",
			"GIT_COMMITTER_NAME":      "Test",
			"GIT_COMMITTER_EMAIL":     "test@example.com",
			"HOME":                    dir,
		} {
			os.Setenv(k, v)
		}
		return m.Run()
	}()))
}

// gitHeavy bounds how many tests run git at once. Every git call starts
// as a fork of this race-instrumented binary, which on a loaded machine
// can take over a second to exec, and holds the pipes of any git call
// another goroutine is starting until it does; with many tests at once,
// single calls ran past the scanner's timeout.
var gitHeavy = make(chan struct{}, 4)

// parallel runs the test in parallel, at most cap(gitHeavy) at a time.
func parallel(t *testing.T) {
	t.Helper()
	t.Parallel()
	gitHeavy <- struct{}{}
	t.Cleanup(func() { <-gitHeavy })
}

// requireGit skips the test when git isn't installed.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

// tempDir is t.TempDir with symlinks resolved (/var is /private/var on
// macOS), so it compares equal to the scanner's canonical paths.
func tempDir(t testing.TB) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// gitT runs git in dir and returns its trimmed stdout, failing the test on
// error.
func gitT(t testing.TB, dir string, args ...string) string {
	t.Helper()
	return gitEnv(t, dir, nil, args...)
}

func gitEnv(t testing.TB, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// commitAt commits everything staged in dir with the given time.
func commitAt(t testing.TB, dir, msg string, at time.Time) string {
	t.Helper()
	date := at.Format(time.RFC3339)
	gitEnv(t, dir, []string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date},
		"commit", "-q", "--allow-empty", "-m", msg)
	return gitT(t, dir, "rev-parse", "HEAD")
}

// newRepo creates a repo at dir with one commit and returns the commit.
func newRepo(t testing.TB, dir string, at time.Time) string {
	t.Helper()
	mkdir(t, dir)
	gitT(t, dir, "init", "-q")
	write(t, filepath.Join(dir, "README"), "hello\n")
	gitT(t, dir, "add", "README")
	return commitAt(t, dir, "init", at)
}

func mkdir(t testing.TB, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func write(t testing.TB, path, body string) {
	t.Helper()
	mkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixture reads a file from testdata/worktrees.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "worktrees", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// worktreeByPath finds a worktree in a result by path.
func worktreeByPath(t *testing.T, res *Result, path string) *Worktree {
	t.Helper()
	for i := range res.Repos {
		for j := range res.Repos[i].Worktrees {
			if w := &res.Repos[i].Worktrees[j]; SamePath(w.Path, path) {
				return w
			}
		}
	}
	t.Fatalf("no worktree %s in the result", path)
	return nil
}

// repoByPath finds a repo in a result by path.
func repoByPath(t *testing.T, res *Result, path string) *Repo {
	t.Helper()
	for i := range res.Repos {
		if SamePath(res.Repos[i].Path, path) {
			return &res.Repos[i]
		}
	}
	t.Fatalf("no repo %s in the result; have %v", path, repoPaths(res))
	return nil
}

func repoPaths(res *Result) []string {
	var out []string
	for _, r := range res.Repos {
		out = append(out, r.Path)
	}
	return out
}
