package worktrees

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/config"
)

var (
	d1 = time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	d2 = d1.Add(24 * time.Hour)
	d3 = d2.Add(24 * time.Hour)
	d4 = d3.Add(24 * time.Hour)
	d5 = d4.Add(24 * time.Hour)
)

const (
	agentLocked   = "a0123456789abcdef"
	agentUnlocked = "abbbbbbbbbbbbbbbb"
	scratchID     = "5213f886-247d-496b-a20e-50b0e6c1973f"
	claudeReason  = "claude agent agent-" + agentLocked + " (pid 4242 start Fri Nov  6 01:23:32 2026)"
)

// statesWorld is a repo with a worktree in every state the scanner reports,
// a bare remote with a worktree of its own, and a Claude-scratchpad-shaped
// worktree outside the root.
type statesWorld struct {
	base, root, app, remote string
	wt                      map[string]string // name → path
	c1, c5                  string            // commits
}

func buildStates(t *testing.T) *statesWorld {
	t.Helper()
	requireGit(t)
	base := tempDir(t)
	w := &statesWorld{base: base, root: filepath.Join(base, "root"), wt: map[string]string{}}
	w.app = filepath.Join(w.root, "app")
	w.remote = filepath.Join(w.root, ".srv", "app.git")
	path := func(name string, parts ...string) string {
		p := filepath.Join(parts...)
		w.wt[name] = p
		return p
	}

	// app with a remote; main at c5, pushed.
	newRepo(t, w.app, d1)
	write(t, filepath.Join(w.app, ".gitignore"), "*.log\n.claude/\n")
	write(t, filepath.Join(w.app, "old.txt"), "old\n")
	gitT(t, w.app, "add", ".")
	w.c1 = commitAt(t, w.app, "c1", d1)
	mkdir(t, filepath.Dir(w.remote))
	gitT(t, w.root, "init", "-q", "--bare", w.remote)
	gitT(t, w.app, "remote", "add", "origin", w.remote)
	// A second remote with a dot in its name and two URLs (the first wins).
	gitT(t, w.app, "remote", "add", "up.stream", "https://example.com/a.git")
	gitT(t, w.app, "config", "--add", "remote.up.stream.url", "https://example.com/b.git")
	gitT(t, w.app, "push", "-q", "-u", "origin", "main")
	w.c5 = commitAt(t, w.app, "c5", d5)
	gitT(t, w.app, "push", "-q", "origin", "main")

	add := func(args ...string) { gitT(t, w.app, append([]string{"worktree", "add", "-q"}, args...)...) }

	// feat: sibling, pushed once, then two more commits (2 unpushed).
	add("-b", "feat", path("feat", w.root, "app-feat"), w.c1)
	commitAt(t, w.wt["feat"], "c2", d2)
	gitT(t, w.wt["feat"], "push", "-q", "-u", "origin", "feat")
	commitAt(t, w.wt["feat"], "c3", d3)
	commitAt(t, w.wt["feat"], "c4", d4)

	// old: tracks origin/main from c1, so 1 behind.
	add("-b", "old", path("old", w.root, "app-old"), w.c1)
	gitT(t, w.wt["old"], "branch", "-q", "--set-upstream-to=origin/main")

	// gone: pushed, then its remote-tracking ref removed (what fetch --prune does).
	add("-b", "gone", path("gone", w.root, "app-gone"), w.c1)
	gitT(t, w.wt["gone"], "push", "-q", "-u", "origin", "gone")
	gitT(t, w.app, "update-ref", "-d", "refs/remotes/origin/gone")

	// stale: deleted on the remote but never fetched, so still "ok".
	add("-b", "stale", path("stale", w.root, "app-stale"), w.c1)
	gitT(t, w.wt["stale"], "push", "-q", "-u", "origin", "stale")
	gitT(t, w.root, "--git-dir="+w.remote, "branch", "-q", "-D", "stale")

	add("--detach", path("detached", w.root, "app-detached"), w.c1)

	// Agent worktrees: one running (locked by its lead), one finished.
	add("-b", "worktree-agent-"+agentLocked, path("agent", w.app, ".claude", "worktrees", "agent-"+agentLocked), w.c1)
	gitT(t, w.app, "worktree", "lock", "--reason", claudeReason, w.wt["agent"])
	write(t, filepath.Join(w.wt["agent"], "notes.txt"), "wip\n")
	add("-b", "share/v1", path("agent-done", w.app, ".claude", "worktrees", "agent-"+agentUnlocked), w.c1)

	add("-b", "plainlock", path("plainlock", w.root, "app-plainlock"), w.c1)
	gitT(t, w.app, "worktree", "lock", w.wt["plainlock"])

	// prunable: directory removed.
	add("-b", "pruneme", path("prunable", w.root, "app-prunable"), w.c1)
	os.RemoveAll(w.wt["prunable"])

	// vanished: locked, then removed. The lock hides "prunable".
	add("-b", "vanished", path("vanished", w.root, "app-vanished"), w.c1)
	gitT(t, w.app, "worktree", "lock", "--reason", claudeReason, w.wt["vanished"])
	os.RemoveAll(w.wt["vanished"])

	// Directory still there but its .git file gone: unlocked is prunable;
	// locked isn't, and git status inside it must not fall through to app.
	add("-b", "nogit", path("nogit", w.root, "app-nogit"), w.c1)
	os.Remove(filepath.Join(w.wt["nogit"], ".git"))
	add("-b", "nogit-locked", path("nogit-locked", w.app, ".claude", "worktrees", "nogit-locked"), w.c1)
	gitT(t, w.app, "worktree", "lock", w.wt["nogit-locked"])
	os.Remove(filepath.Join(w.wt["nogit-locked"], ".git"))

	// A review checkout in a Claude scratchpad, outside every root.
	add("--detach", path("scratch", base, "tmp", "claude-501", "-Users-u-Projects-app", scratchID, "scratchpad", "review-1"), w.c1)
	write(t, filepath.Join(w.wt["scratch"], "sub", "out.txt"), "x\n")

	// The bare remote's own worktree, under the root.
	gitT(t, w.root, "--git-dir="+w.remote, "worktree", "add", "-q", path("bare-wt", w.root, "bare-wt"), "main")

	// The main checkout: 3 tracked changes, 2 untracked entries, 1 ignored.
	write(t, filepath.Join(w.app, "README"), "changed\n")
	write(t, filepath.Join(w.app, "staged.txt"), "new\n")
	gitT(t, w.app, "add", "staged.txt")
	gitT(t, w.app, "mv", "old.txt", "new.txt")
	write(t, filepath.Join(w.app, "u1.txt"), "u\n")
	write(t, filepath.Join(w.app, "udir", "x"), "x\n")
	write(t, filepath.Join(w.app, "udir", "y"), "y\n")
	write(t, filepath.Join(w.app, "debug.log"), "ignored\n")
	return w
}

func TestScanStates(t *testing.T) {
	parallel(t)
	w := buildStates(t)
	ctx := context.Background()

	// Make feat's index stale, so a status that refreshed the index would
	// rewrite it; the snapshot below proves nothing was written.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(w.wt["feat"], "README"), old, old); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, w.base)

	// The bare remote is passed in explicitly: the walk alone would skip it.
	s := New(Options{Roots: []string{w.root}})
	res := s.Scan(ctx, filepath.Join(w.wt["scratch"], "sub"), w.remote)

	if after := snapshot(t, w.base); !reflect.DeepEqual(before, after) {
		t.Errorf("the scan changed files:\n%s", diffSnapshots(before, after))
	}
	if len(res.Errors) != 0 {
		t.Errorf("errors: %v", res.Errors)
	}
	if got := repoPaths(res); len(got) != 2 {
		t.Fatalf("repos %v, want app and the bare remote", got)
	}

	app := repoByPath(t, res, w.app)
	if app.Err != nil || app.Bare || !SamePath(app.GitDir, filepath.Join(w.app, ".git")) {
		t.Errorf("app repo: %+v", app)
	}
	wantRemotes := map[string]string{"origin": w.remote, "up.stream": "https://example.com/a.git"}
	if !reflect.DeepEqual(app.Remotes, wantRemotes) {
		t.Errorf("app remotes %v, want %v", app.Remotes, wantRemotes)
	}
	if len(app.Worktrees) != len(w.wt) { // the main one, plus all but bare-wt
		t.Errorf("app has %d worktrees, want %d", len(app.Worktrees), len(w.wt))
	}
	if m := app.Main(); m == nil || !SamePath(m.Path, w.app) || !m.Main {
		t.Errorf("app main: %+v", m)
	}
	for _, wt := range app.Worktrees[1:] {
		if wt.Main {
			t.Errorf("%s flagged main", wt.Path)
		}
	}

	type want struct {
		branch                 string
		detached, locked       bool
		lockReason             string
		prunable, missing      bool
		statusRead             bool
		uncommitted, untracked int
		upstream               UpstreamState
		upstreamRef            string
		unpushed, behind       int
		last                   time.Time
		agentID, scratch       string
		hasErr                 bool
	}
	cases := map[string]want{
		"feat":         {branch: "feat", statusRead: true, upstream: UpstreamOK, upstreamRef: "origin/feat", unpushed: 2, last: d4},
		"old":          {branch: "old", statusRead: true, upstream: UpstreamOK, upstreamRef: "origin/main", behind: 1, last: d1},
		"gone":         {branch: "gone", statusRead: true, upstream: UpstreamGone, upstreamRef: "origin/gone", last: d1},
		"stale":        {branch: "stale", statusRead: true, upstream: UpstreamOK, upstreamRef: "origin/stale", last: d1},
		"detached":     {detached: true, statusRead: true, upstream: UpstreamNone, last: d1},
		"agent":        {branch: "worktree-agent-" + agentLocked, locked: true, lockReason: claudeReason, statusRead: true, untracked: 1, upstream: UpstreamNone, last: d1, agentID: agentLocked},
		"agent-done":   {branch: "share/v1", statusRead: true, upstream: UpstreamNone, last: d1, agentID: agentUnlocked},
		"plainlock":    {branch: "plainlock", locked: true, statusRead: true, upstream: UpstreamNone, last: d1},
		"prunable":     {branch: "pruneme", prunable: true, missing: true, upstream: UpstreamNone, last: d1},
		"vanished":     {branch: "vanished", locked: true, lockReason: claudeReason, missing: true, upstream: UpstreamNone, last: d1, agentID: agentLocked},
		"nogit":        {branch: "nogit", prunable: true, upstream: UpstreamNone, last: d1},
		"nogit-locked": {branch: "nogit-locked", locked: true, upstream: UpstreamNone, last: d1, hasErr: true},
		"scratch":      {detached: true, statusRead: true, untracked: 1, upstream: UpstreamNone, last: d1, scratch: scratchID},
	}
	for name, c := range cases {
		wt := worktreeByPath(t, res, w.wt[name])
		got := want{
			branch: wt.Branch, detached: wt.Detached, locked: wt.Locked, lockReason: wt.LockReason,
			prunable: wt.Prunable, missing: wt.Missing, statusRead: wt.StatusRead,
			uncommitted: wt.Uncommitted, untracked: wt.Untracked,
			upstream: wt.Upstream, upstreamRef: wt.UpstreamRef, unpushed: wt.Unpushed, behind: wt.Behind,
			last: wt.LastCommitAt, agentID: wt.AgentID, scratch: wt.ScratchpadSession, hasErr: wt.Err != nil,
		}
		if !got.last.Equal(c.last) {
			t.Errorf("%s: last commit %v, want %v", name, got.last, c.last)
		}
		got.last = c.last
		if got != c {
			t.Errorf("%s:\n got %+v\nwant %+v (err %v)", name, got, c, wt.Err)
		}
		if (c.prunable || c.missing) != (wt.PrunableReason != "" || c.missing) {
			t.Errorf("%s: prunable reason %q", name, wt.PrunableReason)
		}
	}

	main := worktreeByPath(t, res, w.app)
	if !main.StatusRead || main.Uncommitted != 3 || main.Untracked != 2 || main.Dirty() != 5 {
		t.Errorf("main dirty counts: %+v", main)
	}
	if main.Branch != "main" || main.Head != w.c5 || main.Upstream != UpstreamOK || main.Unpushed != 0 || main.Behind != 0 || !main.LastCommitAt.Equal(d5) {
		t.Errorf("main: %+v", main)
	}

	agent := worktreeByPath(t, res, w.wt["agent"])
	if agent.Lock == nil || agent.Lock.PID != 4242 || agent.Lock.AgentID != agentLocked ||
		!agent.Lock.Start.Equal(time.Date(2026, 11, 6, 1, 23, 32, 0, time.UTC)) {
		t.Errorf("agent lock: %+v", agent.Lock)
	}
	if done := worktreeByPath(t, res, w.wt["agent-done"]); done.Lock != nil {
		t.Errorf("finished agent has a lock: %+v", done.Lock)
	}
	if nl := worktreeByPath(t, res, w.wt["nogit-locked"]); !isNotRepo(nl.Err) {
		t.Errorf("locked worktree without .git: err %v, want git's not-a-repository", nl.Err)
	}
	if p := worktreeByPath(t, res, w.wt["prunable"]); p.Path != filepath.Clean(p.RecordedPath) {
		t.Errorf("missing worktree path %q, recorded %q", p.Path, p.RecordedPath)
	}

	remote := repoByPath(t, res, w.remote)
	if remote.Err != nil || !remote.Bare || len(remote.Worktrees) != 2 || !SamePath(remote.GitDir, w.remote) || len(remote.Remotes) != 0 {
		t.Fatalf("bare repo: %+v", remote)
	}
	if b := remote.Worktrees[0]; !b.Main || !b.Bare || b.Head != "" || b.StatusRead || b.Upstream != UpstreamUnknown {
		t.Errorf("bare entry: %+v", b)
	}
	if bw := remote.Worktrees[1]; bw.Main || bw.Branch != "main" || bw.Upstream != UpstreamNone || !bw.StatusRead || !bw.LastCommitAt.Equal(d5) {
		t.Errorf("bare repo's worktree: %+v", bw)
	}

	// Lookup: deepest worktree wins; nothing outside.
	if r, wt := res.Lookup(filepath.Join(w.wt["agent"], "src", "x.go")); r != app || wt == nil || !SamePath(wt.Path, w.wt["agent"]) {
		t.Errorf("Lookup in agent worktree: %v", wt)
	}
	if _, wt := res.Lookup(filepath.Join(w.app, "udir")); wt == nil || !wt.Main {
		t.Errorf("Lookup in main checkout: %v", wt)
	}
	if r, wt := res.Lookup(filepath.Join(w.base, "elsewhere")); r != nil || wt != nil {
		t.Errorf("Lookup outside: %v %v", r, wt)
	}

	// Repos sort by path: root/.srv/app.git before root/app.
	wantWatch := []string{filepath.Join(remote.GitDir, "worktrees"), filepath.Join(app.GitDir, "worktrees")}
	if got := res.WatchPaths(); !reflect.DeepEqual(got, wantWatch) {
		t.Errorf("WatchPaths %v, want %v", got, wantWatch)
	}

	// Worktrees outside every root are listed without being passed in; the
	// bare remote, found only by the walk this time, is skipped.
	res2 := s.Scan(ctx)
	worktreeByPath(t, res2, w.wt["scratch"])
	if len(res2.Repos) != 1 || len(res2.Errors) != 1 || !errors.Is(res2.Errors[0], ErrBareSkipped) {
		t.Errorf("walk only: repos %v errors %v", repoPaths(res2), res2.Errors)
	}
}

func TestScanUnbornBranch(t *testing.T) {
	parallel(t)
	requireGit(t)
	base := tempDir(t)
	repo := filepath.Join(base, "r")
	newRepo(t, repo, d1)
	fresh := filepath.Join(base, "fresh")
	cmd := exec.Command("git", "worktree", "add", "-q", "--orphan", "-b", "fresh", fresh)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("git worktree add --orphan (git 2.42+): %v %s", err, out)
	}
	res := New(Options{}).Scan(context.Background(), repo)
	r := repoByPath(t, res, repo)
	wt := worktreeByPath(t, res, fresh)
	if r.Err != nil || wt.Branch != "fresh" || wt.Head != strings.Repeat("0", 40) ||
		!wt.LastCommitAt.IsZero() || wt.Upstream != UpstreamNone || !wt.StatusRead {
		t.Errorf("unborn: repo err %v, %+v", r.Err, wt)
	}
}

func TestDiscover(t *testing.T) {
	parallel(t)
	requireGit(t)
	base := tempDir(t)
	root := filepath.Join(base, "root")
	out := filepath.Join(base, "outside")
	ctx := context.Background()

	newRepo(t, filepath.Join(root, "tools"), d1)
	newRepo(t, filepath.Join(root, "tools", "nested"), d1) // a repo inside a repo
	newRepo(t, filepath.Join(root, "org", "group", "deep3"), d1)
	newRepo(t, filepath.Join(root, "org", "group", "sub", "deep4"), d1) // one level too deep
	newRepo(t, filepath.Join(root, "web", "node_modules", "pkg"), d1)   // skipped dir

	// A worktree under the root whose main repo is outside it.
	mainOut := filepath.Join(out, "main-out")
	newRepo(t, mainOut, d1)
	gitT(t, mainOut, "worktree", "add", "-q", "-b", "linked", filepath.Join(root, "linked"))

	// A submodule: skipped, as part of its superproject.
	libsrc := filepath.Join(out, "libsrc")
	newRepo(t, libsrc, d1)
	super := filepath.Join(root, "super")
	newRepo(t, super, d1)
	gitT(t, super, "submodule", "add", "-q", libsrc, "lib")

	// A .git that isn't a repo.
	mkdir(t, filepath.Join(root, "stray", ".git"))

	// A symlink to a repo: not followed.
	newRepo(t, filepath.Join(out, "symrepo"), d1)
	if err := os.Symlink(filepath.Join(out, "symrepo"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	// Paths passed in: inside a repo outside the root, not a repo, missing.
	other := filepath.Join(out, "other")
	newRepo(t, other, d1)
	mkdir(t, filepath.Join(other, "deeper", "x"))
	plain := filepath.Join(out, "plain")
	mkdir(t, plain)
	missing := filepath.Join(out, "missing")
	noRoot := filepath.Join(base, "no-such-root")

	s := New(Options{Roots: []string{root, noRoot}})
	dirs, errs := s.Discover(ctx, filepath.Join(other, "deeper", "x"), plain, missing, filepath.Join(root, "tools"))

	gd := func(p string) string { return filepath.Join(p, ".git") }
	want := []string{ // sorted by path
		gd(mainOut),
		gd(other),
		gd(filepath.Join(root, "org", "group", "deep3")),
		gd(filepath.Join(root, "super")),
		gd(filepath.Join(root, "tools")),
		gd(filepath.Join(root, "tools", "nested")),
	}
	if !samePaths(dirs, want) {
		t.Errorf("Discover:\n got %v\nwant %v", dirs, want)
	}

	errFor := map[string]error{}
	for _, err := range errs {
		var pe *fs.PathError
		if !errors.As(err, &pe) {
			t.Fatalf("error %v is not an *fs.PathError", err)
		}
		errFor[pe.Path] = pe.Err
	}
	for path, target := range map[string]error{
		plain:                        ErrNotRepo,
		filepath.Join(root, "stray"): ErrNotRepo,
		missing:                      fs.ErrNotExist,
		noRoot:                       fs.ErrNotExist,
	} {
		if err := errFor[path]; !errors.Is(err, target) {
			t.Errorf("error for %s: %v, want %v", path, err, target)
		}
	}
	if len(errs) != 4 {
		t.Errorf("errors: %v", errs)
	}

	// One more level finds deep4.
	dirs, _ = New(Options{Roots: []string{root}, MaxDepth: 4}).Discover(ctx)
	if !containsPath(dirs, gd(filepath.Join(root, "org", "group", "sub", "deep4"))) {
		t.Errorf("MaxDepth 4: %v", dirs)
	}

	// CommonDir maps any path in a worktree to the main repo.
	if got, err := s.CommonDir(ctx, filepath.Join(root, "linked")); err != nil || !SamePath(got, gd(mainOut)) {
		t.Errorf("CommonDir(linked) = %q, %v", got, err)
	}
	if _, err := s.CommonDir(ctx, plain); !errors.Is(err, ErrNotRepo) {
		t.Errorf("CommonDir(plain) err %v", err)
	}
}

func TestScanCaseInsensitivePaths(t *testing.T) {
	parallel(t)
	requireGit(t)
	base := tempDir(t)
	repo := filepath.Join(base, "Proj", "relay")
	newRepo(t, repo, d1)
	lower := filepath.Join(base, "proj", "relay")
	if _, err := os.Stat(lower); err != nil {
		t.Skip("case-sensitive file system")
	}
	// Like the relay capture (relay.porcelain): git records the case it was given.
	recorded := filepath.Join(lower, ".claude", "worktrees", "eager")
	gitT(t, repo, "worktree", "add", "-q", "-b", "eager", recorded)
	onDisk := filepath.Join(repo, ".claude", "worktrees", "eager")

	ctx := context.Background()
	res := New(Options{}).Scan(ctx, filepath.Join(base, "PROJ", "RELAY"))
	if len(res.Repos) != 1 || len(res.Errors) != 0 {
		t.Fatalf("repos %v errors %v", repoPaths(res), res.Errors)
	}
	r := res.Repos[0]
	if r.Path != repo || r.GitDir != filepath.Join(repo, ".git") {
		t.Errorf("repo path %q git dir %q, want on-disk case %q", r.Path, r.GitDir, repo)
	}
	wt := r.Worktrees[1]
	if wt.Path != onDisk || wt.RecordedPath != recorded {
		t.Errorf("worktree path %q recorded %q; want %q and %q", wt.Path, wt.RecordedPath, onDisk, recorded)
	}
	if !wt.StatusRead || wt.Missing {
		t.Errorf("worktree: %+v", wt)
	}
	for _, p := range []string{onDisk, recorded, strings.ToUpper(onDisk)} {
		if _, got := res.Lookup(p); got == nil || got.Path != onDisk {
			t.Errorf("Lookup(%s) = %v", p, got)
		}
	}
}

// fakeGit writes a git wrapper script that runs the real git after the
// given shell snippet, and returns its path.
func fakeGit(t *testing.T, snippet string) string {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	path := filepath.Join(tempDir(t), "git")
	body := "#!/bin/sh\n" + snippet + "\nexec '" + real + "' \"$@\"\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestScanTimeoutIsPerRepo(t *testing.T) {
	parallel(t)
	requireGit(t)
	root := filepath.Join(tempDir(t), "root")
	newRepo(t, filepath.Join(root, "fast"), d1)
	newRepo(t, filepath.Join(root, "slowrepo"), d1)
	// Listing slowrepo hangs.
	git := fakeGit(t, `case "$*" in *slowrepo*worktree*) exec sleep 60;; esac`)

	// The timeout bounds the fast repo's real git calls too (through the
	// fake's shell), so it is the default one, as production scans use:
	// a short one fails the fast repo on a loaded machine. The test runs
	// in parallel, so its wait overlaps the others.
	start := time.Now()
	res := New(Options{Roots: []string{root}, Git: git}).Scan(context.Background())
	if d := time.Since(start); d > 40*time.Second {
		t.Errorf("scan took %v: it waited for the hung call", d)
	}
	slow := repoByPath(t, res, filepath.Join(root, "slowrepo"))
	if !errors.Is(slow.Err, context.DeadlineExceeded) || len(slow.Worktrees) != 0 {
		t.Errorf("slow repo: err %v, %d worktrees", slow.Err, len(slow.Worktrees))
	}
	fast := repoByPath(t, res, filepath.Join(root, "fast"))
	if fast.Err != nil || len(fast.Worktrees) != 1 || !fast.Worktrees[0].StatusRead {
		t.Errorf("fast repo: %+v", fast)
	}
}

func TestScanToleratesChurn(t *testing.T) {
	parallel(t)
	requireGit(t)
	base := tempDir(t)
	repo := filepath.Join(base, "r")
	newRepo(t, repo, d1)
	churn := filepath.Join(base, "churn-wt")
	gitT(t, repo, "worktree", "add", "-q", "-b", "churn", churn)
	// The worktree is removed between the listing and its status.
	git := fakeGit(t, `d=$(pwd -P)
case "$d" in '`+churn+`') case "$*" in *status*) rm -rf "$d";; esac;; esac`)

	res := New(Options{Git: git}).Scan(context.Background(), repo)
	wt := worktreeByPath(t, res, churn)
	if !wt.Missing || wt.Err != nil || wt.StatusRead {
		t.Errorf("churned worktree: %+v", wt)
	}
	if r := repoByPath(t, res, repo); r.Err != nil || !r.Worktrees[0].StatusRead {
		t.Errorf("repo: %+v", r)
	}
}

func TestConcurrencyIsBounded(t *testing.T) {
	parallel(t)
	requireGit(t)
	root := filepath.Join(tempDir(t), "root")
	for _, name := range []string{"a", "b", "c", "d", "e", "f"} {
		repo := filepath.Join(root, name)
		newRepo(t, repo, d1)
		gitT(t, repo, "worktree", "add", "-q", "-b", "x", filepath.Join(root, name+"-x"))
	}

	const limit = 2
	s := New(Options{Roots: []string{root}, Concurrency: limit})
	var (
		inFlight, peak atomic.Int32
		mu             sync.Mutex
	)
	real := s.run
	s.run = func(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		mu.Lock()
		if n > peak.Load() {
			peak.Store(n)
		}
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		return real(ctx, dir, env, args...)
	}
	res := s.Scan(context.Background())
	if len(res.Repos) != 6 || len(res.Errors) != 0 {
		t.Fatalf("repos %v errors %v", repoPaths(res), res.Errors)
	}
	for _, r := range res.Repos {
		if r.Err != nil || len(r.Worktrees) != 2 {
			t.Errorf("%s: %+v", r.Path, r)
		}
	}
	if p := peak.Load(); p > limit {
		t.Errorf("%d git processes at once, limit %d", p, limit)
	}
	t.Logf("peak %d git processes", peak.Load())
}

func TestCancelledScan(t *testing.T) {
	parallel(t)
	requireGit(t)
	root := filepath.Join(tempDir(t), "root")
	newRepo(t, filepath.Join(root, "a"), d1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := New(Options{Roots: []string{root}}).Scan(ctx)
	// Discovery's git calls fail, so nothing is scanned; nothing panics.
	if len(res.Repos) != 0 || len(res.Errors) != 1 || !errors.Is(res.Errors[0], context.Canceled) {
		t.Errorf("repos %v errors %v", repoPaths(res), res.Errors)
	}
}

func TestOptionsFromConfig(t *testing.T) {
	cfg := config.Default()
	cfg.Roots = []string{"/x/Projects", "/y"}
	opts := OptionsFromConfig(cfg)
	if !reflect.DeepEqual(opts.Roots, cfg.Roots) {
		t.Errorf("roots %v", opts.Roots)
	}
	opts.Roots[0] = "changed"
	if cfg.Roots[0] != "/x/Projects" {
		t.Error("OptionsFromConfig shares the roots slice")
	}
	s := New(opts)
	if s.opts.MaxDepth != DefaultMaxDepth || s.opts.Concurrency != DefaultConcurrency ||
		s.opts.Timeout != DefaultTimeout || s.opts.Git != "git" || len(s.opts.SkipDirs) == 0 {
		t.Errorf("defaults: %+v", s.opts)
	}
}

func TestBaseEnv(t *testing.T) {
	// Repo-locating variables go; config variables (system, global) stay, and
	// nothing sets GIT_CONFIG_NOSYSTEM. Later duplicates win in os/exec.
	env := baseEnv([]string{"PATH=/bin", "GIT_DIR=/elsewhere", "GIT_WORK_TREE=/w", "GIT_CONFIG_GLOBAL=/c", "GIT_OPTIONAL_LOCKS=1"})
	want := []string{"PATH=/bin", "GIT_CONFIG_GLOBAL=/c", "GIT_OPTIONAL_LOCKS=1",
		"GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1", "GIT_ALLOW_PROTOCOL=", "LC_ALL=C"}
	if !reflect.DeepEqual(env, want) {
		t.Errorf("baseEnv = %v, want %v", env, want)
	}
}

func samePaths(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if !SamePath(got[i], want[i]) {
			return false
		}
	}
	return true
}

func containsPath(list []string, p string) bool {
	for _, q := range list {
		if SamePath(q, p) {
			return true
		}
	}
	return false
}

// fileState is what snapshot records for each path.
type fileState struct {
	mode  fs.FileMode
	size  int64
	mtime time.Time
}

// snapshot records every file and directory under dir.
func snapshot(t *testing.T, dir string) map[string]fileState {
	t.Helper()
	out := map[string]fileState{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		st := fileState{mode: fi.Mode(), size: fi.Size(), mtime: fi.ModTime()}
		if fi.IsDir() {
			st.size = 0 // directory sizes vary with no change on some file systems
		}
		out[p] = st
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func diffSnapshots(a, b map[string]fileState) string {
	var lines []string
	for p, s := range a {
		if t, ok := b[p]; !ok {
			lines = append(lines, "removed "+p)
		} else if s != t {
			lines = append(lines, "changed "+p)
		}
	}
	for p := range b {
		if _, ok := a[p]; !ok {
			lines = append(lines, "added "+p)
		}
	}
	return strings.Join(lines, "\n")
}
