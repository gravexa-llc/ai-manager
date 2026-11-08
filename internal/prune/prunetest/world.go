package prunetest

import (
	"bytes"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/github"
	"github.com/gravexa-llc/ai-manager/internal/sessions"
)

// LockStart is the lead start time written in the world's Claude locks.
var LockStart = time.Date(2026, 11, 6, 1, 23, 32, 0, time.UTC)

// Session ids of the world's two scratchpads.
const (
	LiveScratchSession = "11111111-2222-4333-8444-555555555555"
	DeadScratchSession = "99999999-8888-4777-8666-555555555555"
)

// Lead pids in the world's locks: LivePID runs with the lock's start time,
// StalePID isn't running, ReusedPID runs but started at another time.
const (
	LivePID   = 111
	StalePID  = 222
	ReusedPID = 333
)

// World is a temp root holding:
//
//   - widgets: a repo whose origin is git@github.com:acme/widgets.git
//     (never contacted; its remote-tracking refs are set with update-ref),
//     with one linked worktree per case under wt/, and two detached
//     worktrees in Claude scratchpads under claude-501/;
//   - plain: a repo with no GitHub remote and one linked worktree.
//
// GitHub, Sessions and Procs answer for it. Want maps each worktree's
// name to the category the rules should give it without --include-closed.
type World struct {
	Root, Repo, Plain string
	// WT maps a case name to its worktree path.
	WT map[string]string
	// Head maps a case name to its HEAD commit.
	Head map[string]string
	// Want maps a case name to the expected category, and Reason to a
	// substring of the expected reasons.
	Want   map[string]string
	Reason map[string]string

	C0 string // widgets' root commit, on origin/main
	M  string // the merge commit on origin/surge

	GitHub   *FakeGitHub
	Sessions *FakeSessions
	Procs    FakeProcs
}

func lockReason(pid int) string {
	return "claude agent agent-a" + strings.Repeat("1", 16) + " (pid " + itoa(pid) + " start " + LockStart.Format(time.ANSIC) + ")"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

// BuildWorld creates the world in a fresh temp dir.
//
// Building it takes over a hundred git calls, so in a test binary that uses
// Main the world is built once, as a template in Main's temp dir, and each
// call gets a copy of it (see World.copyTo): the same files, refs and
// objects, with git's worktree links moved to the copy's root, and fresh
// fakes. Tests can change their copy freely.
func BuildWorld(t *testing.T) *World {
	t.Helper()
	RequireGit(t)
	if baseDir == "" {
		return buildWorld(t, TempDir(t))
	}
	return template(t).copyTo(t, TempDir(t))
}

// worldTemplate is the world BuildWorld copies, built on first use.
var worldTemplate struct {
	mu sync.Mutex
	w  *World
}

// template returns the template world, building it if this is the first
// call. A build that fails ends the calling test (t.Fatal unwinds through
// the deferred Unlock) and leaves no template, so the next call tries again.
func template(t *testing.T) *World {
	t.Helper()
	worldTemplate.mu.Lock()
	defer worldTemplate.mu.Unlock()
	if worldTemplate.w == nil {
		dir, err := os.MkdirTemp(baseDir, "world")
		if err != nil {
			t.Fatal(err)
		}
		if dir, err = filepath.EvalSymlinks(dir); err != nil {
			t.Fatal(err)
		}
		worldTemplate.w = buildWorld(t, dir)
	}
	return worldTemplate.w
}

// copyTo copies the world's directory tree to root, an empty directory, and
// returns the copy's World. Git records each linked worktree by absolute
// path in two files, <repo>/.git/worktrees/<name>/gitdir and the
// worktree's .git file; those are rewritten to point inside root, which is
// what `git worktree add` would have written there. Any other file holding
// the old root fails the test, since it would point into the template.
// Index files keep the template's stat data, so git status re-reads file
// contents to see that nothing changed.
func (w *World) copyTo(t *testing.T, root string) *World {
	t.Helper()
	oldRoot, newRoot := []byte(w.Root), []byte(root)
	err := filepath.WalkDir(w.Root, func(src string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(w.Root, src)
		if err != nil {
			return err
		}
		dst := filepath.Join(root, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(dst, info.Mode().Perm())
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(src)
			if err != nil {
				return err
			}
			if strings.HasPrefix(target, w.Root) {
				return fmt.Errorf("symlink %s points into the template: %s", src, target)
			}
			return os.Symlink(target, dst)
		case d.Type().IsRegular():
			data, err := os.ReadFile(src)
			if err != nil {
				return err
			}
			if bytes.Contains(data, oldRoot) {
				if name := d.Name(); name != "gitdir" && name != ".git" {
					return fmt.Errorf("%s holds the template's root %s; teach copyTo to rewrite it", src, w.Root)
				}
				data = bytes.ReplaceAll(data, oldRoot, newRoot)
			}
			return os.WriteFile(dst, data, info.Mode().Perm())
		default:
			return fmt.Errorf("can't copy %s: mode %v", src, d.Type())
		}
	})
	if err != nil {
		t.Fatalf("copy the template world: %v", err)
	}

	move := func(p string) string {
		if rest, ok := strings.CutPrefix(p, w.Root); ok {
			return root + rest
		}
		return p
	}
	c := &World{
		Root: root, Repo: move(w.Repo), Plain: move(w.Plain),
		WT: map[string]string{}, Head: maps.Clone(w.Head), Want: maps.Clone(w.Want), Reason: maps.Clone(w.Reason),
		C0: w.C0, M: w.M,
		GitHub: &FakeGitHub{
			PRs: map[string][]github.PR{}, Total: maps.Clone(w.GitHub.Total), BranchErr: maps.Clone(w.GitHub.BranchErr),
			RepoErr: maps.Clone(w.GitHub.RepoErr), Default: maps.Clone(w.GitHub.Default),
			Canonical: maps.Clone(w.GitHub.Canonical), Verdict: w.GitHub.Verdict,
		},
		Sessions: &FakeSessions{Snap: w.Sessions.Snap, Err: w.Sessions.Err},
		Procs:    maps.Clone(w.Procs),
	}
	for name, p := range w.WT {
		c.WT[name] = move(p)
	}
	for k, prs := range w.GitHub.PRs {
		c.GitHub.PRs[k] = slices.Clone(prs)
	}
	c.Sessions.Snap.Sessions = slices.Clone(w.Sessions.Snap.Sessions)
	for i := range c.Sessions.Snap.Sessions {
		c.Sessions.Snap.Sessions[i].Cwd = move(c.Sessions.Snap.Sessions[i].Cwd)
	}
	c.Sessions.Snap.Warnings = slices.Clone(w.Sessions.Snap.Warnings)
	return c
}

// buildWorld creates the world in root, an empty directory, with git.
func buildWorld(t *testing.T, root string) *World {
	t.Helper()
	w := &World{
		Root: root, Repo: filepath.Join(root, "widgets"), Plain: filepath.Join(root, "plain"),
		WT: map[string]string{}, Head: map[string]string{}, Want: map[string]string{}, Reason: map[string]string{},
		GitHub: &FakeGitHub{
			PRs: map[string][]github.PR{}, Total: map[string]int{}, BranchErr: map[string]error{},
			RepoErr: map[string]error{}, Default: map[string]string{"acme/widgets": "main"},
		},
		Sessions: &FakeSessions{},
		Procs:    FakeProcs{LivePID: LockStart, ReusedPID: LockStart.Add(time.Hour)},
	}
	repo := w.Repo
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	Git(t, repo, "init", "-q")
	Write(t, filepath.Join(repo, "README"), "widgets\n")
	Write(t, filepath.Join(repo, ".gitignore"), "node_modules\n.spike-state/\n*.log\n")
	w.C0 = Commit(t, repo, "init")
	Git(t, repo, "remote", "add", "origin", "git@github.com:acme/widgets.git")
	Git(t, repo, "update-ref", "refs/remotes/origin/main", w.C0)
	Git(t, repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")

	// add makes a branch worktree from C0 with one commit and returns it.
	add := func(name string, files map[string]string) string {
		p := filepath.Join(root, "wt", name)
		Git(t, repo, "worktree", "add", "-q", "-b", name, p, w.C0)
		for f, body := range files {
			Write(t, filepath.Join(p, f), body)
		}
		w.WT[name] = p
		w.Head[name] = Commit(t, p, name)
		return w.Head[name]
	}
	pr := func(branch string, n int, state github.State, head, base string) github.PR {
		return github.PR{Repo: "acme/widgets", Number: n, State: state, HeadRef: branch, HeadOID: head, BaseRef: base,
			URL: "https://github.com/acme/widgets/pull/" + itoa(n)}
	}
	setPRs := func(branch string, prs ...github.PR) { w.GitHub.PRs["acme/widgets#"+branch] = prs }
	want := func(name, cat, reason string) { w.Want[name], w.Reason[name] = cat, reason }

	// Merged, tip = PR head, with rebuildable ignored files: two
	// node_modules folders and a node_modules symlink to one of them.
	h := add("merged", map[string]string{"merged.txt": "m\n", "api/keep.txt": "k\n"})
	Write(t, filepath.Join(w.WT["merged"], "web", "node_modules", "x", "a.js"), "x\n")
	Write(t, filepath.Join(w.WT["merged"], "node_modules", "y.js"), "x\n")
	if err := os.Symlink("../web/node_modules", filepath.Join(w.WT["merged"], "api", "node_modules")); err != nil {
		t.Fatal(err)
	}
	setPRs("merged", pr("merged", 1, github.StateMerged, h, "main"))
	want("merged", "safe", "PR #1 merged into main")

	// Squash-merged: two commits; origin/main never gets them.
	add("squash", map[string]string{"s1.txt": "1\n"})
	Write(t, filepath.Join(w.WT["squash"], "s2.txt"), "2\n")
	h = Commit(t, w.WT["squash"], "squash 2")
	w.Head["squash"] = h
	setPRs("squash", pr("squash", 2, github.StateMerged, h, "main"))
	want("squash", "safe", "PR #2 merged into main")

	// Fast-forwarded onto the base after the merge: the PR head is an
	// ancestor of the tip, which is the merge commit on origin/surge.
	h = add("ff", map[string]string{"ff.txt": "f\n"})
	tree := Git(t, repo, "rev-parse", h+"^{tree}")
	w.M = Git(t, repo, "commit-tree", tree, "-p", w.C0, "-p", h, "-m", "Merge ff")
	Git(t, repo, "update-ref", "refs/remotes/origin/surge", w.M)
	Git(t, w.WT["ff"], "merge", "-q", "--ff-only", w.M)
	w.Head["ff"] = w.M
	setPRs("ff", pr("ff", 3, github.StateMerged, h, "surge"))
	want("ff", "safe", "PR #3 merged into surge")

	// A commit after the PR.
	h = add("after", map[string]string{"a.txt": "a\n"})
	Write(t, filepath.Join(w.WT["after"], "a2.txt"), "later\n")
	w.Head["after"] = Commit(t, w.WT["after"], "after the PR")
	setPRs("after", pr("after", 4, github.StateMerged, h, "main"))
	want("after", "not-safe", "1 commit not in PR #4 or origin/main")

	// The PR head was never fetched.
	add("headmissing", nil)
	setPRs("headmissing", pr("headmissing", 5, github.StateMerged, strings.Repeat("de", 20), "main"))
	want("headmissing", "not-safe", "isn't in the local repo")

	// Merged, but with local state in an ignored folder.
	h = add("atrisk", nil)
	Write(t, filepath.Join(w.WT["atrisk"], "scripts", ".spike-state", "state.json"), "{}\n")
	Write(t, filepath.Join(w.WT["atrisk"], "node_modules", "z.js"), "x\n")
	setPRs("atrisk", pr("atrisk", 6, github.StateMerged, h, "main"))
	want("atrisk", "not-safe", "1 ignored path at risk: scripts/.spike-state/")

	// An open PR, with an older merged one.
	h = add("open", nil)
	setPRs("open", pr("open", 71, github.StateOpen, h, "main"), pr("open", 70, github.StateMerged, h, "main"))
	want("open", "not-safe", "PR #71 is open")

	// Closed without merging.
	h = add("closed", nil)
	setPRs("closed", pr("closed", 8, github.StateClosed, h, "main"))
	want("closed", "safe-closed", "PR #8 closed without merging")

	// No PR, and only a fork's PR.
	add("nopr", nil)
	want("nopr", "not-safe", "no PR for this branch")
	h = add("fork", nil)
	forkPR := pr("fork", 9, github.StateMerged, h, "main")
	forkPR.CrossRepository = true
	setPRs("fork", forkPR)
	want("fork", "not-safe", "no PR for this branch")

	// Merged, but with an untracked file and a modified one.
	h = add("dirty", nil)
	Write(t, filepath.Join(w.WT["dirty"], "new.txt"), "n\n")
	Write(t, filepath.Join(w.WT["dirty"], "README"), "changed\n")
	setPRs("dirty", pr("dirty", 10, github.StateMerged, h, "main"))
	want("dirty", "not-safe", "1 uncommitted file; 1 untracked file")

	// Merged, but a live session works in a subfolder.
	h = add("incwd", map[string]string{"sub/keep.txt": "k\n"})
	setPRs("incwd", pr("incwd", 11, github.StateMerged, h, "main"))
	want("incwd", "not-safe", "in use: cwd of interactive session")
	w.Sessions.Add(sessions.Session{PID: 500, SessionID: "aaaaaaaa-0000-4000-8000-000000000001", Kind: sessions.Interactive,
		Cwd: filepath.Join(w.WT["incwd"], "sub"), Status: sessions.StatusIdle})

	// Merged, and a finished background job's cwd: not live, so safe.
	h = add("donejob", nil)
	setPRs("donejob", pr("donejob", 12, github.StateMerged, h, "main"))
	want("donejob", "safe", "PR #12 merged into main")
	w.Sessions.Add(sessions.Session{JobID: "0badf00d", SessionID: "bbbbbbbb-0000-4000-8000-000000000002", Kind: sessions.Background,
		Cwd: w.WT["donejob"], State: sessions.JobDone})

	// GitHub couldn't answer for this branch.
	add("stalegh", nil)
	w.GitHub.BranchErr["acme/widgets#stalegh"] = github.ErrUnavailable
	want("stalegh", "not-safe", "PR unknown: github: unavailable")

	// The base was never fetched: tip = head is still safe.
	h = add("basemissing", nil)
	setPRs("basemissing", pr("basemissing", 13, github.StateMerged, h, "release"))
	want("basemissing", "safe", "PR #13 merged into release")

	// More PRs than were read.
	h = add("many", nil)
	setPRs("many", pr("many", 14, github.StateMerged, h, "main"))
	w.GitHub.Total["acme/widgets#many"] = 7
	want("many", "not-safe", "7 PRs on this branch")

	// Status unknown: a required filter (as git-lfs sets) that the scan
	// disables makes status fail.
	p := filepath.Join(root, "wt", "lfs")
	Git(t, repo, "worktree", "add", "-q", "-b", "lfs", p, w.C0)
	Write(t, filepath.Join(p, ".gitattributes"), "*.req filter=req\n")
	Write(t, filepath.Join(p, "c.req"), "cccc\n")
	w.WT["lfs"], w.Head["lfs"] = p, Commit(t, p, "lfs")
	Git(t, repo, "config", "filter.req.clean", "cat")
	Git(t, repo, "config", "filter.req.required", "true")
	Write(t, filepath.Join(p, "c.req"), "zzzz\n")
	setPRs("lfs", pr("lfs", 15, github.StateMerged, w.Head["lfs"], "main"))
	want("lfs", "not-safe", "status unknown")

	// Locks: a live lead, a dead one, a reused pid, and a non-Claude lock.
	for name, pid := range map[string]int{"locked-live": LivePID, "locked-stale": StalePID, "locked-reused": ReusedPID} {
		add(name, nil)
		Git(t, repo, "worktree", "lock", "--reason", lockReason(pid), w.WT[name])
	}
	want("locked-live", "locked", "locked by agent a1111111111111111 of lead pid 111")
	want("locked-stale", "locked", "stale lock: agent a1111111111111111 of lead pid 222; pid 222 isn't running")
	want("locked-reused", "locked", "pid 333 is now another process")
	add("locked-other", nil)
	Git(t, repo, "worktree", "lock", "--reason", "moving disks", w.WT["locked-other"])
	want("locked-other", "locked", "locked: moving disks")

	// Detached worktrees: on origin/main, on origin/surge (a PR's base),
	// and on nothing the remote has.
	detach := func(name, dir, at string) {
		Git(t, repo, "worktree", "add", "-q", "--detach", dir, at)
		w.WT[name], w.Head[name] = dir, at
	}
	detach("detached-main", filepath.Join(root, "wt", "detached-main"), w.C0)
	want("detached-main", "scratch", "on origin/main")
	detach("detached-surge", filepath.Join(root, "wt", "detached-surge"), w.Head["ff"])
	want("detached-surge", "scratch", "on origin/surge")
	detach("detached-local", filepath.Join(root, "wt", "detached-local"), w.Head["after"])
	want("detached-local", "not-safe", "isn't on origin/main or origin/surge")

	// Scratchpads: one of a live session, one of a gone session.
	scratch := func(session string) string {
		return filepath.Join(root, "claude-501", "-Users-test-proj", session, "scratchpad")
	}
	detach("scratch-live", filepath.Join(scratch(LiveScratchSession), "review-1"), w.C0)
	want("scratch-live", "not-safe", "in the scratchpad of live interactive session 11111111 (pid 501)")
	w.Sessions.Add(sessions.Session{PID: 501, SessionID: LiveScratchSession, Kind: sessions.Interactive, Cwd: repo, Status: sessions.StatusBusy})
	detach("scratch-dead", filepath.Join(scratch(DeadScratchSession), "review-2"), w.C0)
	want("scratch-dead", "scratch", "on origin/main")

	// Prunable: a folder that's gone, and one whose .git file is gone.
	detach("gone", filepath.Join(root, "wt", "gone"), w.C0)
	if err := os.RemoveAll(w.WT["gone"]); err != nil {
		t.Fatal(err)
	}
	want("gone", "prunable", "gitdir file points to non-existent location")
	detach("nogit", filepath.Join(root, "wt", "nogit"), w.C0)
	if err := os.Remove(filepath.Join(w.WT["nogit"], ".git")); err != nil {
		t.Fatal(err)
	}
	Write(t, filepath.Join(w.WT["nogit"], "keep.txt"), "user file\n")
	want("nogit", "prunable", "the folder stays")

	// The main checkout.
	w.WT["main"], w.Head["main"] = repo, w.C0
	want("main", "main", "main checkout")

	// A session whose cwd isn't a repo at all.
	notRepo := filepath.Join(root, "not-a-repo")
	if err := os.MkdirAll(notRepo, 0o755); err != nil {
		t.Fatal(err)
	}
	w.Sessions.Add(sessions.Session{PID: 502, SessionID: "cccccccc-0000-4000-8000-000000000003", Kind: sessions.Interactive, Cwd: notRepo, Status: sessions.StatusIdle})

	// plain: no GitHub remote.
	if err := os.MkdirAll(w.Plain, 0o755); err != nil {
		t.Fatal(err)
	}
	Git(t, w.Plain, "init", "-q")
	Write(t, filepath.Join(w.Plain, "README"), "plain\n")
	c := Commit(t, w.Plain, "init")
	Git(t, w.Plain, "remote", "add", "origin", "/srv/git/plain.git")
	pf := filepath.Join(root, "plain-wt", "feature")
	Git(t, w.Plain, "worktree", "add", "-q", "-b", "feature", pf, c)
	w.WT["plain-feature"], w.Head["plain-feature"] = pf, c
	want("plain-feature", "not-safe", "no GitHub remote")
	w.WT["plain-main"], w.Head["plain-main"] = w.Plain, c
	want("plain-main", "main", "main checkout")
	return w
}

// Name returns the case name of a worktree path, or "".
func (w *World) Name(path string) string {
	for name, p := range w.WT {
		if strings.EqualFold(p, path) {
			return name
		}
	}
	return ""
}

// ConfigTOML is a config.toml whose only root is the world.
func (w *World) ConfigTOML() string {
	return "roots = [\"" + w.Root + "\"]\n"
}
