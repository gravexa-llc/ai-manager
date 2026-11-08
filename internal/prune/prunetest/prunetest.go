// Package prunetest builds temp git worlds and fakes for testing aim ls and
// aim prune: a fake GitHub (prune.PRSource), session lister and process
// table, and World, a repo with one linked worktree per case the prune
// rules distinguish. Only tests import it.
package prunetest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/github"
	"github.com/gravexa-llc/ai-manager/internal/sessions"
	"github.com/gravexa-llc/ai-manager/internal/testenv"
)

// Main runs a test binary with git isolated from the machine: no system
// or global config (a test-only global config instead), fixed identities,
// HOME in a temp dir, and a ceiling at the temp dir so no git call can find
// a repo outside it. It runs testenv.Setup first (a small TMPDIR, and the
// fork watchdog). Use it as
// `func TestMain(m *testing.M) { os.Exit(prunetest.Main(m)) }`.
func Main(m *testing.M) int {
	done := testenv.Setup()
	return done(isolateGit(m))
}

// isolateGit is Main after testenv.Setup.
func isolateGit(m *testing.M) int {
	dir, err := os.MkdirTemp("", "aim-a3-git")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(dir)
	cfg := filepath.Join(dir, "gitconfig")
	const body = "[init]\n\tdefaultBranch = main\n[advice]\n\tdetachedHead = false\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	tmp, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	home := filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for k, v := range map[string]string{
		"GIT_CONFIG_GLOBAL":       cfg,
		"GIT_CONFIG_NOSYSTEM":     "1",
		"GIT_CEILING_DIRECTORIES": tmp,
		"GIT_AUTHOR_NAME":         "Test",
		"GIT_AUTHOR_EMAIL":        "test@example.com",
		"GIT_COMMITTER_NAME":      "Test",
		"GIT_COMMITTER_EMAIL":     "test@example.com",
		"HOME":                    home,
		"AIM_HOME":                filepath.Join(dir, "aim"),
	} {
		os.Setenv(k, v)
	}
	baseDir = dir
	return m.Run()
}

// baseDir is the temp dir Main made for this test binary, removed when the
// tests are done; "" without Main. BuildWorld keeps its template there.
var baseDir string

// RequireGit skips the test when git isn't installed.
func RequireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

// TempDir is t.TempDir with symlinks resolved, so it compares equal to the
// scanner's canonical paths.
func TempDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// Git runs git in dir and returns its trimmed output, failing the test on
// error.
func Git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

// Commit commits everything in dir (adding all files) and returns the
// commit.
func Commit(t *testing.T, dir, msg string) string {
	t.Helper()
	Git(t, dir, "add", "-A")
	Git(t, dir, "commit", "-q", "--allow-empty", "-m", msg)
	return Git(t, dir, "rev-parse", "HEAD")
}

// Write writes a file, creating its directory.
func Write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Exists reports whether p exists.
func Exists(p string) bool { _, err := os.Stat(p); return err == nil }

// FakeGitHub is a prune.PRSource with canned answers. Keys are
// "owner/name" (any case) and "owner/name#branch".
type FakeGitHub struct {
	mu        sync.Mutex
	PRs       map[string][]github.PR
	Total     map[string]int // TotalCount override; default len(PRs)
	BranchErr map[string]error
	RepoErr   map[string]error
	Default   map[string]string
	Canonical map[string]string // nameWithOwner GitHub answers with
	Verdict   github.Budget
	Calls     int
	Requests  []github.RepoBranches
}

// BranchPRs implements prune.PRSource.
func (f *FakeGitHub) BranchPRs(_ context.Context, repos []github.RepoBranches) github.BranchPRsResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls++
	f.Requests = append(f.Requests, repos...)
	res := github.BranchPRsResult{Queries: 1, Cost: 1}
	for _, rb := range repos {
		key := strings.ToLower(rb.Owner + "/" + rb.Name)
		r := github.RepoPRs{Owner: rb.Owner, Name: rb.Name, Repo: rb.Owner + "/" + rb.Name, DefaultBranch: f.Default[key]}
		if c := f.Canonical[key]; c != "" {
			r.Repo = c
		}
		if err := f.RepoErr[key]; err != nil {
			r.Repo, r.DefaultBranch, r.Err, r.StaleSince = "", "", err, time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
		}
		for _, b := range rb.Branches {
			bp := github.BranchPRs{Branch: b}
			bkey := key + "#" + b
			switch {
			case r.Err != nil:
				bp.Err = r.Err
			case f.BranchErr[bkey] != nil:
				bp.Err = f.BranchErr[bkey]
				if r.Err == nil {
					r.Err, r.StaleSince = bp.Err, time.Date(2026, 11, 5, 9, 0, 0, 0, time.UTC)
				}
			default:
				bp.PRs = append([]github.PR{}, f.PRs[bkey]...)
				bp.TotalCount = len(bp.PRs)
				if n, ok := f.Total[bkey]; ok {
					bp.TotalCount = n
				}
			}
			r.Branches = append(r.Branches, bp)
		}
		res.Repos = append(res.Repos, r)
	}
	return res
}

// Budget implements prune.PRSource.
func (f *FakeGitHub) Budget() github.Budget {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Verdict.Verdict == "" {
		return github.Budget{Verdict: github.BudgetNormal}
	}
	return f.Verdict
}

// CallCount is how many BranchPRs calls were made.
func (f *FakeGitHub) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Calls
}

// FakeSessions is a prune.SessionLister with a canned snapshot.
type FakeSessions struct {
	mu   sync.Mutex
	Snap sessions.Snapshot
	Err  error
}

// List implements prune.SessionLister.
func (f *FakeSessions) List(context.Context) (sessions.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Snap, f.Err
}

// Add adds a session.
func (f *FakeSessions) Add(s sessions.Session) {
	f.mu.Lock()
	f.Snap.Sessions = append(f.Snap.Sessions, s)
	f.mu.Unlock()
}

// FakeProcs is a prune.ProcessTable: pid → start time. A pid not in the
// map isn't running.
type FakeProcs map[int]time.Time

// StartTime implements prune.ProcessTable.
func (f FakeProcs) StartTime(_ context.Context, pid int) (time.Time, bool, error) {
	t, ok := f[pid]
	return t, ok, nil
}
