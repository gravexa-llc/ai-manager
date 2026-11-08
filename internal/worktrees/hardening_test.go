package worktrees

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// markerScript writes an executable script that creates marker when run,
// then exits 1, and returns the script's path.
func markerScript(t *testing.T, dir, name, marker string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	body := "#!/bin/sh\ntouch '" + marker + "'\nexit 1\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// plainStatus runs an ordinary `git status` in dir, the control showing that
// a repo's config would run a program. Errors are ignored: a failing hook or
// filter can make it fail.
func plainStatus(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	t.Logf("control git status: err=%v\n%s", err, out)
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// requireMarker fails the test when the control didn't create marker: then
// the scan's result would prove nothing.
func requireMarker(t *testing.T, marker string) {
	t.Helper()
	if !exists(marker) {
		t.Fatalf("control: plain git status didn't create %s", filepath.Base(marker))
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
}

// sameSize rewrites a file with different content of the same length, so
// git can't tell from size alone and must read (and clean) the content.
func sameSize(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, strings.Repeat("z", len(data)))
}

func TestHardeningFSMonitorNotRun(t *testing.T) {
	parallel(t)
	requireGit(t)
	base := tempDir(t)
	repo := filepath.Join(base, "r")
	newRepo(t, repo, d1)
	marker := filepath.Join(base, "fsmonitor-ran")
	gitT(t, repo, "config", "core.fsmonitor", markerScript(t, base, "fsmonitor.sh", marker))
	write(t, filepath.Join(repo, "README"), "changed\n")

	plainStatus(t, repo)
	requireMarker(t, marker)

	res := New(Options{}).Scan(context.Background(), repo)
	if exists(marker) {
		t.Error("the scan ran the repo's core.fsmonitor hook")
	}
	if wt := worktreeByPath(t, res, repo); !wt.StatusRead || wt.Uncommitted != 1 {
		t.Errorf("worktree: %+v", wt)
	}
}

func TestHardeningFilterDriversNotRun(t *testing.T) {
	parallel(t)
	requireGit(t)
	base := tempDir(t)
	cleanMarker := filepath.Join(base, "clean-ran")
	smudgeMarker := filepath.Join(base, "smudge-ran")
	procMarker := filepath.Join(base, "process-ran")
	reqMarker := filepath.Join(base, "required-ran")
	touchThenCat := func(marker string) string { return "sh -c 'touch " + marker + "; cat'" }

	// Files are committed before the drivers exist, so setup runs no filter.
	repo := filepath.Join(base, "r")
	newRepo(t, repo, d1)
	write(t, filepath.Join(repo, ".gitattributes"), "*.txt filter=evil\n*.dat filter=evilproc\n")
	write(t, filepath.Join(repo, "a.txt"), "aaaa\n")
	write(t, filepath.Join(repo, "b.dat"), "bbbb\n")
	gitT(t, repo, "add", ".")
	commitAt(t, repo, "files", d2)
	gitT(t, repo, "config", "filter.evil.clean", touchThenCat(cleanMarker))
	gitT(t, repo, "config", "filter.evil.smudge", touchThenCat(smudgeMarker))
	gitT(t, repo, "config", "filter.evilproc.process", markerScript(t, base, "process.sh", procMarker))
	sameSize(t, filepath.Join(repo, "a.txt"))
	sameSize(t, filepath.Join(repo, "b.dat"))

	// A required driver: with its commands emptied, status fails.
	req := filepath.Join(base, "req")
	newRepo(t, req, d1)
	write(t, filepath.Join(req, ".gitattributes"), "*.req filter=req\n")
	write(t, filepath.Join(req, "c.req"), "cccc\n")
	gitT(t, req, "add", ".")
	commitAt(t, req, "files", d2)
	gitT(t, req, "config", "filter.req.clean", touchThenCat(reqMarker))
	gitT(t, req, "config", "filter.req.required", "true")
	sameSize(t, filepath.Join(req, "c.req"))

	plainStatus(t, repo)
	requireMarker(t, cleanMarker)
	requireMarker(t, procMarker)
	plainStatus(t, req)
	requireMarker(t, reqMarker)

	res := New(Options{}).Scan(context.Background(), repo, req)
	for _, m := range []string{cleanMarker, smudgeMarker, procMarker, reqMarker} {
		if exists(m) {
			t.Errorf("the scan ran a filter driver: %s", filepath.Base(m))
		}
	}
	if wt := worktreeByPath(t, res, repo); !wt.StatusRead || wt.Uncommitted != 2 || wt.Err != nil {
		t.Errorf("filtered worktree: %+v", wt)
	}
	wt := worktreeByPath(t, res, req)
	if wt.StatusRead || wt.Err == nil || !strings.Contains(wt.Err.Error(), "filter") {
		t.Errorf("required filter: want unknown state with a reason, got StatusRead %v err %v", wt.StatusRead, wt.Err)
	}
	if r := repoByPath(t, res, req); r.Err != nil {
		t.Errorf("a worktree's status failure should not fail the repo: %v", r.Err)
	}
}

func TestHardeningSubmoduleFiltersNotRun(t *testing.T) {
	parallel(t)
	requireGit(t)
	base := tempDir(t)
	marker := filepath.Join(base, "submodule-filter-ran")

	lib := filepath.Join(base, "libsrc")
	newRepo(t, lib, d1)
	write(t, filepath.Join(lib, ".gitattributes"), "*.txt filter=subevil\n")
	write(t, filepath.Join(lib, "a.txt"), "aaaa\n")
	gitT(t, lib, "add", ".")
	commitAt(t, lib, "files", d2)

	super := filepath.Join(base, "super")
	newRepo(t, super, d1)
	gitT(t, super, "submodule", "add", "-q", lib, "lib")
	commitAt(t, super, "add lib", d2)
	// The driver lives only in the submodule's own config, so overriding
	// the superproject's drivers wouldn't cover it.
	gitT(t, filepath.Join(super, "lib"), "config", "filter.subevil.clean", "sh -c 'touch "+marker+"; cat'")
	sameSize(t, filepath.Join(super, "lib", "a.txt"))

	plainStatus(t, super)
	requireMarker(t, marker)

	res := New(Options{}).Scan(context.Background(), super)
	if exists(marker) {
		t.Error("the scan ran a submodule's filter driver")
	}
	if wt := worktreeByPath(t, res, super); !wt.StatusRead || wt.Err != nil {
		t.Errorf("superproject: %+v", wt)
	}
}

// recordCalls wraps s.run to record the directory and arguments of every git
// call.
func recordCalls(s *Scanner) func() [][]string {
	var (
		mu    sync.Mutex
		calls [][]string
	)
	real := s.run
	s.run = func(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
		mu.Lock()
		calls = append(calls, append([]string{dir}, args...))
		mu.Unlock()
		return real(ctx, dir, env, args...)
	}
	return func() [][]string {
		mu.Lock()
		defer mu.Unlock()
		return append([][]string(nil), calls...)
	}
}

// explicitBarePolicy points git at a global config with
// safe.bareRepository=explicit for the rest of the test, as a careful user
// might have. Scanners must be created after it.
func explicitBarePolicy(t *testing.T) {
	t.Helper()
	cfg := filepath.Join(tempDir(t), "gitconfig")
	write(t, cfg, "[init]\n\tdefaultBranch = main\n[safe]\n\tbareRepository = explicit\n")
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
}

// gitDirCalls returns the recorded calls that named dir with --git-dir.
func gitDirCalls(calls [][]string, dir string) [][]string {
	var out [][]string
	for _, c := range calls {
		if slicesContain(c[1:], "--git-dir="+dir) {
			out = append(out, c)
		}
	}
	return out
}

func TestPassedPathsLetGitDecide(t *testing.T) {
	for _, policy := range []string{"all", "explicit"} {
		t.Run("safe.bareRepository="+policy, func(t *testing.T) {
			requireGit(t)
			if policy == "explicit" {
				explicitBarePolicy(t)
			}
			base := tempDir(t)
			ctx := context.Background()

			// A working tree whose top also holds HEAD, objects and refs: a
			// layout guess would take it for a bare repo; git finds .git first.
			top := filepath.Join(base, "wt-top")
			newRepo(t, top, d1)
			write(t, filepath.Join(top, "HEAD"), "ref: refs/heads/main\n")
			mkdir(t, filepath.Join(top, "objects"))
			mkdir(t, filepath.Join(top, "refs", "heads"))

			// A real bare repo, and a directory inside it.
			src := filepath.Join(base, "src")
			newRepo(t, src, d1)
			bare := filepath.Join(base, "srv", "proj.git")
			mkdir(t, filepath.Dir(bare))
			gitT(t, base, "init", "-q", "--bare", bare)
			gitT(t, src, "push", "-q", bare, "main")
			inside := filepath.Join(bare, "refs")

			// A bare repo inside another repo's working tree.
			host := filepath.Join(base, "host")
			newRepo(t, host, d1)
			embedded := filepath.Join(host, "sub", "emb.git")
			mkdir(t, filepath.Dir(embedded))
			gitT(t, base, "init", "-q", "--bare", embedded)

			discover := func(path string) ([]string, []error, [][]string) {
				s := New(Options{})
				calls := recordCalls(s)
				dirs, errs := s.Discover(ctx, path)
				return dirs, errs, calls()
			}

			if dirs, errs, _ := discover(top); len(errs) != 0 || !samePaths(dirs, []string{filepath.Join(top, ".git")}) {
				t.Errorf("working tree with HEAD/objects/refs: %v %v, want its .git", dirs, errs)
			}

			if dirs, errs, _ := discover(bare); len(errs) != 0 || !samePaths(dirs, []string{bare}) {
				t.Errorf("bare repo passed in: %v %v", dirs, errs)
			}

			dirs, errs, calls := discover(inside)
			if policy == "all" {
				// git's own discovery finds the repo above.
				if len(errs) != 0 || !samePaths(dirs, []string{bare}) {
					t.Errorf("inside a bare repo: %v %v", dirs, errs)
				}
			} else {
				if len(dirs) != 0 || len(errs) != 1 || !isBareRefused(errs[0]) {
					t.Errorf("inside a bare repo: %v %v, want git's refusal", dirs, errs)
				}
				if c := append(gitDirCalls(calls, inside), gitDirCalls(calls, bare)...); len(c) != 0 {
					t.Errorf("named with --git-dir: %v", c)
				}
			}

			dirs, errs, calls = discover(embedded)
			if len(dirs) != 0 || len(errs) != 1 || !errors.Is(errs[0], ErrEmbeddedBare) {
				t.Errorf("embedded bare repo: %v %v, want ErrEmbeddedBare", dirs, errs)
			}
			if c := gitDirCalls(calls, embedded); len(c) != 0 {
				t.Errorf("embedded bare repo named with --git-dir: %v", c)
			}

			// CommonDir resolves the same way.
			if _, err := New(Options{}).CommonDir(ctx, embedded); !errors.Is(err, ErrEmbeddedBare) {
				t.Errorf("CommonDir(embedded) err %v", err)
			}
		})
	}
}

func TestNestedEmbeddedBareRefused(t *testing.T) {
	for _, policy := range []string{"all", "explicit"} {
		t.Run("safe.bareRepository="+policy, func(t *testing.T) {
			requireGit(t)
			if policy == "explicit" {
				explicitBarePolicy(t)
			}
			base := tempDir(t)
			if a := dotGitAncestor(base); a != "" {
				t.Skipf("the temp dir is under %s, which has a .git", a)
			}
			ctx := context.Background()

			// host/sub/wrap.git is a bare repo inside host's working tree, and
			// inner.git a bare repo inside it. From inner.git's parent git
			// answers "not a work tree", so only the ancestor walk sees host.
			host := filepath.Join(base, "host")
			newRepo(t, host, d1)
			wrap := filepath.Join(host, "sub", "wrap.git")
			inner := filepath.Join(wrap, "inner.git")
			mkdir(t, filepath.Dir(wrap))
			gitT(t, base, "init", "-q", "--bare", wrap)
			gitT(t, base, "init", "-q", "--bare", inner)

			// A bare repo with no .git anywhere above it.
			src := filepath.Join(base, "src")
			newRepo(t, src, d1)
			clean := filepath.Join(base, "srv", "clean.git")
			mkdir(t, filepath.Dir(clean))
			gitT(t, base, "init", "-q", "--bare", clean)
			gitT(t, src, "push", "-q", clean, "main")

			s := New(Options{})
			calls := recordCalls(s)
			res := s.Scan(ctx, inner, clean)
			var refused bool
			for _, err := range res.Errors {
				var pe *fs.PathError
				if errors.As(err, &pe) && SamePath(pe.Path, inner) && errors.Is(err, ErrEmbeddedBare) {
					refused = true
				} else {
					t.Errorf("unexpected error %v", err)
				}
			}
			if !refused {
				t.Errorf("nested embedded bare repo not refused: repos %v errors %v", repoPaths(res), res.Errors)
			}
			if c := gitDirCalls(calls(), inner); len(c) != 0 {
				t.Errorf("nested embedded bare repo named with --git-dir: %v", c)
			}
			if r := repoByPath(t, res, clean); r.Err != nil || !r.Bare || len(r.Worktrees) != 1 {
				t.Errorf("clean bare repo: %+v", r)
			}
			if len(res.Repos) != 1 {
				t.Errorf("repos %v, want only clean.git", repoPaths(res))
			}

			if _, err := New(Options{}).CommonDir(ctx, inner); !errors.Is(err, ErrEmbeddedBare) {
				t.Errorf("CommonDir(inner) err %v", err)
			}
			if got, err := New(Options{}).CommonDir(ctx, clean); err != nil || !SamePath(got, clean) {
				t.Errorf("CommonDir(clean) = %q, %v", got, err)
			}
		})
	}
}

func TestDotGitAncestor(t *testing.T) {
	parallel(t)
	base := tempDir(t)
	if a := dotGitAncestor(base); a != "" {
		t.Skipf("the temp dir is under %s, which has a .git", a)
	}
	deep := filepath.Join(base, "a", "b", "c")
	mkdir(t, deep)
	if got := dotGitAncestor(deep); got != "" {
		t.Errorf("no .git above: got %q", got)
	}
	write(t, filepath.Join(base, "a", ".git"), "gitdir: elsewhere\n") // a .git file counts too
	if got := dotGitAncestor(deep); got != filepath.Join(base, "a") {
		t.Errorf("got %q, want %s", got, filepath.Join(base, "a"))
	}
	// dir's own .git doesn't count: only ancestors.
	if got := dotGitAncestor(filepath.Join(base, "a")); got != "" {
		t.Errorf("own .git counted: %q", got)
	}
	if got := dotGitAncestor("/"); got != "" && got != "/" {
		t.Errorf("root: %q", got)
	}
}

// without returns env minus the entries equal to drop.
func without(env []string, drop ...string) []string {
	var out []string
	for _, kv := range env {
		if !slicesContain(drop, kv) {
			out = append(out, kv)
		}
	}
	return out
}

func TestLazyFetchCannotReachTransport(t *testing.T) {
	parallel(t)
	requireGit(t)
	base := tempDir(t)
	marker := filepath.Join(base, "transport-ran")

	// A repo set up like a partial clone whose promisor remote is an ext::
	// URL: reaching that transport runs `touch <marker>`. The repo's own
	// config allows ext, which outranks -c protocol.allow=never.
	repo := filepath.Join(base, "r")
	newRepo(t, repo, d1)
	gitT(t, repo, "config", "core.repositoryformatversion", "1")
	gitT(t, repo, "config", "extensions.partialClone", "origin")
	gitT(t, repo, "remote", "add", "origin", "ext::sh -c touch% "+marker)
	gitT(t, repo, "config", "remote.origin.promisor", "true")
	gitT(t, repo, "config", "remote.origin.partialclonefilter", "blob:none")
	gitT(t, repo, "config", "protocol.ext.allow", "always")
	// A worktree whose HEAD names a commit the repo doesn't have, so reading
	// it asks the promisor remote for it.
	wt := filepath.Join(base, "wt")
	gitT(t, repo, "worktree", "add", "-q", "--detach", wt)
	missing := strings.Repeat("1234567890", 4)
	write(t, filepath.Join(repo, ".git", "worktrees", "wt", "HEAD"), missing+"\n")

	cmd := exec.Command("git", "rev-list", "--no-walk", "--ignore-missing", missing)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	t.Logf("control git rev-list: err=%v\n%s", err, out)
	requireMarker(t, marker)

	check := func(name string, s *Scanner) {
		t.Helper()
		res := s.Scan(context.Background(), repo)
		if exists(marker) {
			t.Errorf("%s: the scan reached the promisor remote's transport", name)
			os.Remove(marker)
		}
		w := worktreeByPath(t, res, wt)
		if w.Head != missing || !w.LastCommitAt.IsZero() {
			t.Errorf("%s: worktree %+v", name, w)
		}
	}
	check("default", New(Options{}))

	// git before 2.45 ignores GIT_NO_LAZY_FETCH; the rest must still hold.
	old := New(Options{})
	old.run = execGitEnv("git", without(baseEnv(os.Environ()), "GIT_NO_LAZY_FETCH=1"))
	check("without GIT_NO_LAZY_FETCH", old)
}

func TestRefusedBarePath(t *testing.T) {
	parallel(t)
	err := &GitError{Stderr: "fatal: cannot use bare repository '/x/y.git' (safe.bareRepository is 'explicit')"}
	if got := refusedBarePath(err); got != "/x/y.git" {
		t.Errorf("refusedBarePath = %q", got)
	}
	if got := refusedBarePath(&GitError{Stderr: "fatal: something else"}); got != "" {
		t.Errorf("other message: %q", got)
	}
	if got := refusedBarePath(errors.New("x")); got != "" {
		t.Errorf("not a GitError: %q", got)
	}
}

func TestBareReposOnlyWhenPassed(t *testing.T) {
	requireGit(t)
	base := tempDir(t)
	explicitBarePolicy(t)

	root := filepath.Join(base, "root")
	src := filepath.Join(base, "src")
	newRepo(t, src, d1)
	proj := filepath.Join(root, "srv", "proj.git") // has a worktree under the root
	other := filepath.Join(root, "srv", "other.git")
	mkdir(t, filepath.Join(root, "srv"))
	gitT(t, root, "init", "-q", "--bare", proj)
	gitT(t, root, "init", "-q", "--bare", other)
	gitT(t, src, "push", "-q", proj, "main")
	projWt := filepath.Join(root, "proj-wt")
	gitT(t, root, "--git-dir="+proj, "worktree", "add", "-q", projWt, "main")
	newRepo(t, filepath.Join(root, "plain"), d1)

	ctx := context.Background()

	// Walk only: both bare repos are skipped and reported, once each, and no
	// git command runs in them.
	s := New(Options{Roots: []string{root}})
	calls := recordCalls(s)
	res := s.Scan(ctx)
	if got := repoPaths(res); len(got) != 1 || !SamePath(got[0], filepath.Join(root, "plain")) {
		t.Errorf("walk only: repos %v, want just plain", got)
	}
	skipped := map[string]bool{}
	for _, err := range res.Errors {
		var pe *fs.PathError
		if !errors.Is(err, ErrBareSkipped) || !errors.As(err, &pe) {
			t.Errorf("unexpected error %v", err)
			continue
		}
		if !strings.HasSuffix(err.Error(), "skipped: bare (found by walk)") {
			t.Errorf("error text %q", err)
		}
		for _, p := range []string{proj, other} {
			if SamePath(pe.Path, p) {
				skipped[p] = true
			}
		}
	}
	if len(res.Errors) != 2 || !skipped[proj] || !skipped[other] {
		t.Errorf("walk only: errors %v, want proj.git and other.git skipped", res.Errors)
	}
	for _, c := range calls() {
		for _, p := range []string{proj, other} {
			if SamePath(c[0], p) || within(c[0], p) || slicesContain(c[1:], "--git-dir="+p) {
				t.Errorf("git ran in a walk-found bare repo: %v", c)
			}
		}
	}

	// Passed explicitly (its directory, or a path in its worktree): scanned.
	for _, explicit := range []string{proj, filepath.Join(projWt, ".")} {
		res := New(Options{Roots: []string{root}}).Scan(ctx, explicit)
		r := repoByPath(t, res, proj)
		if r.Err != nil || !r.Bare || len(r.Worktrees) != 2 || !r.Worktrees[1].StatusRead {
			t.Errorf("passed %s: %+v", explicit, r)
		}
		if len(res.Errors) != 1 || !strings.Contains(res.Errors[0].Error(), other) {
			t.Errorf("passed %s: errors %v, want only other.git skipped", explicit, res.Errors)
		}
	}
}

func slicesContain(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestScanLeavesIndexAlone(t *testing.T) {
	parallel(t)
	requireGit(t)
	base := tempDir(t)
	repo := filepath.Join(base, "r")
	newRepo(t, repo, d1)
	wt := filepath.Join(base, "wt")
	gitT(t, repo, "worktree", "add", "-q", "-b", "wt", wt)
	// Stale stat data in both indexes: a status allowed to refresh them
	// would rewrite them.
	old := time.Now().Add(-time.Hour)
	for _, f := range []string{filepath.Join(repo, "README"), filepath.Join(wt, "README")} {
		if err := os.Chtimes(f, old, old); err != nil {
			t.Fatal(err)
		}
	}
	indexes := []string{filepath.Join(repo, ".git", "index"), filepath.Join(repo, ".git", "worktrees", "wt", "index")}
	type state struct {
		mtime time.Time
		data  []byte
	}
	read := func() []state {
		var out []state
		for _, p := range indexes {
			fi, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, state{fi.ModTime(), data})
		}
		return out
	}
	before := read()
	res := New(Options{}).Scan(context.Background(), repo)
	after := read()
	for i := range indexes {
		if !after[i].mtime.Equal(before[i].mtime) || !bytes.Equal(after[i].data, before[i].data) {
			t.Errorf("%s changed: mtime %v → %v", indexes[i], before[i].mtime, after[i].mtime)
		}
	}
	for _, p := range []string{repo, wt} {
		if w := worktreeByPath(t, res, p); !w.StatusRead || w.Dirty() != 0 {
			t.Errorf("%s: %+v", p, w)
		}
	}
}
