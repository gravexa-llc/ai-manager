package prune_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/config"
	"github.com/gravexa-llc/ai-manager/internal/github"
	"github.com/gravexa-llc/ai-manager/internal/prune"
	"github.com/gravexa-llc/ai-manager/internal/prune/prunetest"
	"github.com/gravexa-llc/ai-manager/internal/worktrees"
)

func TestMain(m *testing.M) { os.Exit(prunetest.Main(m)) }

// gitHeavy bounds how many tests scan a world at once. Every git call
// starts as a fork of this race-instrumented binary, and a fork inherits
// the pipes of any git call another goroutine is starting at that moment
// until it execs. With ten tests at once (four git calls each) on a loaded
// machine, those forks took long enough to exec that calls which had
// succeeded hit the scanner's one-second WaitDelay ("exec: WaitDelay
// expired before I/O complete").
var gitHeavy = make(chan struct{}, 4)

// parallel runs the test in parallel, at most cap(gitHeavy) at a time.
func parallel(t *testing.T) {
	t.Helper()
	t.Parallel()
	gitHeavy <- struct{}{}
	t.Cleanup(func() { <-gitHeavy })
}

// checker returns a Checker over the world, with the default rebuildable
// list.
func checker(t *testing.T, w *prunetest.World) *prune.Checker {
	t.Helper()
	m, err := prune.NewMatcher(config.DefaultRebuildable)
	if err != nil {
		t.Fatal(err)
	}
	return &prune.Checker{
		Scanner:     worktrees.New(worktrees.Options{Roots: []string{w.Root}}),
		GitHub:      w.GitHub,
		Sessions:    w.Sessions,
		Procs:       w.Procs,
		Rebuildable: m,
	}
}

func evaluate(t *testing.T, c *prune.Checker, opts prune.Options) *prune.Report {
	t.Helper()
	rep, err := c.Evaluate(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// items maps each world case name to its item.
func items(t *testing.T, w *prunetest.World, rep *prune.Report) map[string]*prune.Item {
	t.Helper()
	out := map[string]*prune.Item{}
	for _, rr := range rep.Repos {
		for _, it := range rr.Items {
			name := w.Name(it.Worktree.Path)
			if name == "" {
				t.Errorf("unexpected worktree %s", it.Worktree.Path)
				continue
			}
			out[name] = it
		}
	}
	return out
}

// item returns the case's item, failing the test now if the report lacks
// it (a broken scan would otherwise panic and stop every other test).
func item(t *testing.T, got map[string]*prune.Item, name string) *prune.Item {
	t.Helper()
	it := got[name]
	if it == nil {
		t.Fatalf("%s: not in the report", name)
	}
	return it
}

// firstReason is the item's first reason, or "".
func firstReason(it *prune.Item) string {
	if len(it.Reasons) == 0 {
		return ""
	}
	return it.Reasons[0]
}

func TestEvaluateEveryCase(t *testing.T) {
	parallel(t)
	w := prunetest.BuildWorld(t)
	rep := evaluate(t, checker(t, w), prune.Options{})
	got := items(t, w, rep)
	for name, cat := range w.Want {
		it := got[name]
		if it == nil {
			t.Errorf("%s: not in the report", name)
			continue
		}
		reasons := strings.Join(it.Reasons, "; ")
		if string(it.Category) != cat || !strings.Contains(reasons, w.Reason[name]) {
			t.Errorf("%s: %s: %s\n  want %s with %q", name, it.Category, reasons, cat, w.Reason[name])
		}
	}
	if len(got) != len(w.Want) {
		t.Errorf("report has %d worktrees, the world %d", len(got), len(w.Want))
	}
	if len(rep.Warnings) != 0 {
		t.Errorf("warnings: %q (a session cwd that isn't a repo must not warn)", rep.Warnings)
	}
	if rep.SessionsErr != nil || rep.GitHubSkipped != "" || rep.LiveSessions != 3 {
		t.Errorf("sessions err %v, GitHub skipped %q, live %d", rep.SessionsErr, rep.GitHubSkipped, rep.LiveSessions)
	}
	if n := w.GitHub.CallCount(); n != 1 {
		t.Errorf("BranchPRs called %d times, want 1", n)
	}

	// Details shown by aim ls.
	if ig := item(t, got, "merged").Ignored; ig == nil || ig.Rebuildable != 3 || len(ig.AtRisk) != 0 {
		t.Errorf("merged ignored = %+v, want 3 rebuildable (two folders, one symlink), none at risk", ig)
	}
	if ig := item(t, got, "atrisk").Ignored; ig == nil || !slices.Equal(ig.AtRisk, []string{"scripts/.spike-state/"}) || ig.Rebuildable != 1 {
		t.Errorf("atrisk ignored = %+v", ig)
	}
	if pr := item(t, got, "open").PR; pr == nil || pr.Number != 71 {
		t.Errorf("open: PR shown = %+v, want the open #71", pr)
	}
	if pr := item(t, got, "closed").PR; pr == nil || pr.State != github.StateClosed {
		t.Errorf("closed: PR shown = %+v", pr)
	}
	if !item(t, got, "stalegh").PRUnknown || item(t, got, "nopr").PRUnknown || item(t, got, "nopr").PR != nil {
		t.Errorf("PRUnknown: stalegh %v, nopr %v", item(t, got, "stalegh").PRUnknown, item(t, got, "nopr").PRUnknown)
	}
	for name, want := range map[string]prune.LockState{
		"locked-live": prune.LockLive, "locked-stale": prune.LockStale,
		"locked-reused": prune.LockStale, "locked-other": prune.LockUnknown,
	} {
		if l := item(t, got, name).Lock; l == nil || l.State != want {
			t.Errorf("%s: lock %+v, want %s", name, l, want)
		}
	}
	if item(t, got, "detached-surge").ReachableFrom != "origin/surge" || item(t, got, "detached-main").ReachableFrom != "origin/main" {
		t.Errorf("reachable from: %q, %q", item(t, got, "detached-surge").ReachableFrom, item(t, got, "detached-main").ReachableFrom)
	}
	for _, rr := range rep.Repos {
		switch {
		case worktrees.SamePath(rr.Repo.Path, w.Repo):
			if rr.GitHub != "acme/widgets" || rr.Remote != "origin" || rr.DefaultBranch != "main" {
				t.Errorf("widgets: %q %q %q", rr.GitHub, rr.Remote, rr.DefaultBranch)
			}
		case worktrees.SamePath(rr.Repo.Path, w.Plain):
			if rr.GitHub != "" || rr.Remote != "" {
				t.Errorf("plain: %q %q", rr.GitHub, rr.Remote)
			}
		}
	}
	// plain has no GitHub remote, so GitHub was asked only about widgets.
	for _, r := range w.GitHub.Requests {
		if r.Owner+"/"+r.Name != "acme/widgets" {
			t.Errorf("GitHub asked about %s/%s", r.Owner, r.Name)
		}
	}
}

func TestPlan(t *testing.T) {
	parallel(t)
	w := prunetest.BuildWorld(t)
	rep := evaluate(t, checker(t, w), prune.Options{})
	names := func(p prune.Plan) []string {
		var out []string
		for _, it := range p.Remove {
			out = append(out, w.Name(it.Worktree.Path))
		}
		slices.Sort(out)
		return out
	}
	want := []string{"basemissing", "detached-main", "detached-surge", "donejob", "ff", "merged", "scratch-dead", "squash"}
	p := rep.Plan(false)
	if got := names(p); !slices.Equal(got, want) {
		t.Errorf("plan removes %q\nwant          %q", got, want)
	}
	if len(p.Prune) != 1 || !worktrees.SamePath(p.Prune[0].Repo.Path, w.Repo) {
		t.Errorf("plan prunes %d repos, want widgets", len(p.Prune))
	}
	withClosed := append(slices.Clone(want), "closed")
	slices.Sort(withClosed)
	if got := names(rep.Plan(true)); !slices.Equal(got, withClosed) {
		t.Errorf("plan with closed removes %q", got)
	}
}

func TestSessionsUnknownIsNotSafe(t *testing.T) {
	parallel(t)
	w := prunetest.BuildWorld(t)
	w.Sessions.Err = errors.New("claude: not found")
	rep := evaluate(t, checker(t, w), prune.Options{})
	if rep.SessionsErr == nil {
		t.Fatal("no SessionsErr")
	}
	got := items(t, w, rep)
	for _, name := range []string{"merged", "detached-main", "scratch-dead"} {
		if it := item(t, got, name); it.Category != prune.NotSafe || !slices.Contains(it.Reasons, "live sessions unknown") {
			t.Errorf("%s: %s %q", name, it.Category, it.Reasons)
		}
	}
	if len(rep.Plan(true).Remove) != 0 {
		t.Error("plan removes something while sessions are unknown")
	}
}

func TestGitHubBudgetStopSkipsGitHub(t *testing.T) {
	parallel(t)
	w := prunetest.BuildWorld(t)
	w.GitHub.Verdict = github.Budget{Verdict: github.BudgetStop, Known: true, Remaining: 150}
	rep := evaluate(t, checker(t, w), prune.Options{})
	if w.GitHub.CallCount() != 0 {
		t.Error("GitHub was read despite the budget")
	}
	if !strings.Contains(rep.GitHubSkipped, "150 left") {
		t.Errorf("GitHubSkipped = %q", rep.GitHubSkipped)
	}
	got := items(t, w, rep)
	if it := item(t, got, "merged"); it.Category != prune.NotSafe || !strings.Contains(strings.Join(it.Reasons, ";"), "GitHub not read") || !it.PRUnknown {
		t.Errorf("merged: %s %q", it.Category, it.Reasons)
	}
	// Detached worktrees don't need GitHub: origin/HEAD names the default.
	if it := item(t, got, "detached-main"); it.Category != prune.Scratch {
		t.Errorf("detached-main: %s %q", it.Category, it.Reasons)
	}
}

func TestNoGitHubClient(t *testing.T) {
	parallel(t)
	w := prunetest.BuildWorld(t)
	c := checker(t, w)
	c.GitHub = nil
	rep := evaluate(t, c, prune.Options{})
	if rep.GitHubSkipped == "" {
		t.Error("GitHubSkipped empty without a client")
	}
	if it := item(t, items(t, w, rep), "merged"); it.Category != prune.NotSafe {
		t.Errorf("merged without GitHub: %s", it.Category)
	}
}

func TestGitHubRepoError(t *testing.T) {
	parallel(t)
	w := prunetest.BuildWorld(t)
	w.GitHub.RepoErr["acme/widgets"] = github.ErrNotFound
	rep := evaluate(t, checker(t, w), prune.Options{})
	got := items(t, w, rep)
	if it := item(t, got, "merged"); it.Category != prune.NotSafe || !strings.Contains(strings.Join(it.Reasons, ";"), "github: not found") {
		t.Errorf("merged: %s %q", it.Category, it.Reasons)
	}
	// No default branch from GitHub: origin/HEAD still answers.
	if it := item(t, got, "detached-main"); it.Category != prune.Scratch {
		t.Errorf("detached-main: %s %q", it.Category, it.Reasons)
	}
}

func TestNoDefaultBranchKnown(t *testing.T) {
	parallel(t)
	w := prunetest.BuildWorld(t)
	delete(w.GitHub.Default, "acme/widgets")
	prunetest.Git(t, w.Repo, "symbolic-ref", "--delete", "refs/remotes/origin/HEAD")
	// Without the PR bases either, nothing says which ref to check.
	w.GitHub.PRs = nil
	rep := evaluate(t, checker(t, w), prune.Options{})
	if it := item(t, items(t, w, rep), "detached-main"); it.Category != prune.NotSafe || !strings.Contains(firstReason(it), "no origin/<default branch> ref") {
		t.Errorf("detached-main: %s %q", it.Category, it.Reasons)
	}
}

func TestRepoFilter(t *testing.T) {
	parallel(t)
	w := prunetest.BuildWorld(t)
	c := checker(t, w)
	for _, sel := range []string{"widgets", "ACME/Widgets", w.Repo, w.WT["merged"]} {
		rep := evaluate(t, c, prune.Options{Repo: sel})
		if len(rep.Repos) != 1 || !worktrees.SamePath(rep.Repos[0].Repo.Path, w.Repo) {
			t.Errorf("--repo %s: %d repos", sel, len(rep.Repos))
		}
	}
	if rep := evaluate(t, c, prune.Options{Repo: "plain"}); len(rep.Repos) != 1 || !worktrees.SamePath(rep.Repos[0].Repo.Path, w.Plain) {
		t.Errorf("--repo plain: %d repos", len(rep.Repos))
	}
	if _, err := c.Evaluate(context.Background(), prune.Options{Repo: "nope"}); err == nil {
		t.Error("--repo nope: no error")
	}
	if _, err := c.Evaluate(context.Background(), prune.Options{Repo: filepath.Join(w.Root, "not-a-repo")}); err == nil {
		t.Error("--repo not-a-repo: no error")
	}

	// A repo outside the roots is scanned when named by path.
	outside := prunetest.TempDir(t)
	if err := os.MkdirAll(filepath.Join(outside, "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	prunetest.Git(t, filepath.Join(outside, "x"), "init", "-q")
	rep := evaluate(t, c, prune.Options{Repo: filepath.Join(outside, "x")})
	if len(rep.Repos) != 1 || !worktrees.SamePath(rep.Repos[0].Repo.Path, filepath.Join(outside, "x")) {
		t.Errorf("--repo outside the roots: %d repos", len(rep.Repos))
	}
}

func TestCustomRebuildable(t *testing.T) {
	parallel(t)
	w := prunetest.BuildWorld(t)
	c := checker(t, w)
	m, err := prune.NewMatcher([]string{".spike-state/", "node_modules/"})
	if err != nil {
		t.Fatal(err)
	}
	c.Rebuildable = m
	rep := evaluate(t, c, prune.Options{})
	if it := item(t, items(t, w, rep), "atrisk"); it.Category != prune.Safe {
		t.Errorf("atrisk with .spike-state/ rebuildable: %s %q", it.Category, it.Reasons)
	}

	// An empty list makes node_modules at risk too.
	c.Rebuildable, _ = prune.NewMatcher(nil)
	rep = evaluate(t, c, prune.Options{})
	if it := item(t, items(t, w, rep), "merged"); it.Category != prune.NotSafe || !strings.Contains(firstReason(it), "node_modules/") {
		t.Errorf("merged with nothing rebuildable: %s %q", it.Category, it.Reasons)
	}
}

// TestGeneratedFilesRebuildable: a merged worktree whose only ignored
// files are Finder's .DS_Store files and Next.js's next-env.d.ts is safe
// with the default list, and holds them at risk with an empty one.
func TestGeneratedFilesRebuildable(t *testing.T) {
	parallel(t)
	w := prunetest.BuildWorld(t)
	p := filepath.Join(w.Root, "wt", "nextapp")
	prunetest.Git(t, w.Repo, "worktree", "add", "-q", "-b", "nextapp", p, w.C0)
	// create-next-app's .gitignore ignores both.
	prunetest.Write(t, filepath.Join(p, ".gitignore"), ".DS_Store\nnext-env.d.ts\n")
	prunetest.Write(t, filepath.Join(p, "app", "page.tsx"), "page\n")
	h := prunetest.Commit(t, p, "nextapp")
	w.WT["nextapp"] = p
	w.GitHub.PRs["acme/widgets#nextapp"] = []github.PR{{Repo: "acme/widgets", Number: 16, State: github.StateMerged,
		HeadRef: "nextapp", HeadOID: h, BaseRef: "main"}}
	// Finder writes a .DS_Store in every folder it shows, including one
	// that holds nothing else.
	generated := []string{".DS_Store", "app/.DS_Store", "empty/.DS_Store", "next-env.d.ts"}
	for _, f := range generated {
		prunetest.Write(t, filepath.Join(p, f), "generated\n")
	}

	c := checker(t, w)
	it := item(t, items(t, w, evaluate(t, c, prune.Options{})), "nextapp")
	if it.Category != prune.Safe || firstReason(it) != "PR #16 merged into main" {
		t.Errorf("nextapp: %s %q, want safe", it.Category, it.Reasons)
	}
	if ig := it.Ignored; ig == nil || ig.Rebuildable != len(generated) || len(ig.AtRisk) != 0 {
		t.Errorf("nextapp ignored = %+v, want %d rebuildable, none at risk", ig, len(generated))
	}

	// With nothing rebuildable, git lists each file as ignored, and each
	// is at risk.
	c.Rebuildable, _ = prune.NewMatcher(nil)
	it = item(t, items(t, w, evaluate(t, c, prune.Options{})), "nextapp")
	if it.Category != prune.NotSafe || it.Ignored == nil || !slices.Equal(it.Ignored.AtRisk, generated) {
		t.Errorf("nextapp with nothing rebuildable: %s %q, ignored %+v", it.Category, it.Reasons, it.Ignored)
	}
}

// TestMatcherGeneratedDefaults covers the default entries for generated
// files and Apple build folders, and their near misses.
func TestMatcherGeneratedDefaults(t *testing.T) {
	m, err := prune.NewMatcher(config.DefaultRebuildable)
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]bool{
		// next-env.d.ts: a file name, at any depth.
		"next-env.d.ts":             true,
		"apps/web/next-env.d.ts":    true,
		"next-env.d.ts.bak":         false,
		"apps/web/my-next-env.d.ts": false,
		"next-env.ts":               false,
		// *.tsbuildinfo: at any depth.
		"tsconfig.tsbuildinfo":          true,
		"packages/ui/foo.tsbuildinfo":   true,
		"apps/web/tsconfig.tsbuildinfo": true,
		"tsconfig.tsbuildinfo.json":     false,
		"packages/ui/tsbuildinfo":       false,
		// .DS_Store: at any depth.
		".DS_Store":            true,
		"app/assets/.DS_Store": true,
		".DS_Store.bak":        false,
		"app/DS_Store":         false,
		// .build/: SwiftPM's folder, directories only.
		".build/":               true,
		".build/x":              true,
		"Packages/Core/.build/": true,
		".build":                false, // a file
		"Sources/.build":        false,
		"build.swift":           false,
		"MyApp.build/":          false,
		// DerivedData/: Xcode's folder, directories only.
		"DerivedData/":           true,
		"ios/DerivedData/x":      true,
		"ios/DerivedData/":       true,
		"DerivedData":            false, // a file
		"ios/DerivedDataBackup/": false,
	} {
		if got := m.Match(p); got != want {
			t.Errorf("Match(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestMatcher(t *testing.T) {
	m, err := prune.NewMatcher(append(slices.Clone(config.DefaultRebuildable), "*.pyc", "/out/", "**/gen/*.go"))
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]bool{
		"node_modules/":            true,
		"web/node_modules/":        true,
		"web/node_modules/x/a.js":  true,
		"node_modules":             false, // a file, and the entry wants a directory
		".next/":                   true,
		"apps/site/.next/":         true,
		"lambdas/bin/":             true,
		"lambdas/bin/main":         true,
		"svc/lambdas/bin/":         false, // anchored: it has a / in the middle
		"lambdas/":                 false,
		"cdk.out/":                 true,
		"a/__pycache__/":           true,
		"x.pyc":                    true,
		"deep/x.pyc":               true,
		"out/":                     true,
		"sub/out/":                 false,
		"pkg/gen/a.go":             true,
		"gen/a.go":                 true,
		"gen/a.txt":                false,
		".env":                     false,
		"scripts/.spike-state/":    false,
		"build.log":                false,
		"build/":                   true,
		"node_modules_backup/":     false,
		"dist":                     false,
		"coverage/lcov-report/x.h": true,
	} {
		if got := m.Match(p); got != want {
			t.Errorf("Match(%q) = %v, want %v", p, got, want)
		}
	}
	if _, err := prune.NewMatcher([]string{"[x"}); err == nil {
		t.Error("NewMatcher accepted a bad pattern")
	}
	if _, err := prune.NewMatcher([]string{"/"}); err == nil {
		t.Error("NewMatcher accepted an empty entry")
	}
}

func TestPS(t *testing.T) {
	start, running, err := prune.PS{}.StartTime(context.Background(), os.Getpid())
	if err != nil || !running {
		t.Fatalf("PS on this process: %v, running %v", err, running)
	}
	if d := time.Since(start); d < 0 || d > 24*time.Hour {
		t.Errorf("PS start time %v is not shortly before now", start)
	}
	if _, running, err := (prune.PS{}).StartTime(context.Background(), 0); running || err != nil {
		t.Errorf("PS pid 0: running %v, err %v", running, err)
	}
	// A pid far above any real one.
	if _, running, err := (prune.PS{}).StartTime(context.Background(), 99999999); running || err != nil {
		t.Errorf("PS missing pid: running %v, err %v", running, err)
	}
}
