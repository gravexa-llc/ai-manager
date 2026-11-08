package worktrees

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// ErrBareSkipped is the error (inside an *fs.PathError naming the repo's git
// dir) for a bare repo that only the root walk found. Such repos are not
// scanned; pass one to Scan or Discover to scan it.
var ErrBareSkipped = errors.New("skipped: bare (found by walk)")

// ErrEmbeddedBare is the error (inside an *fs.PathError) for a path passed
// in that git resolves to a bare repo sitting inside another repo's working
// tree: a repo can commit such a directory to make git treat it as a
// repository of its own, with config the scanner shouldn't trust.
var ErrEmbeddedBare = errors.New("refused: bare repository inside another repository's working tree")

// candidate is a directory that may be in a repo.
type candidate struct {
	dir     string
	bare    bool // walked: looks like a bare repo (HEAD file, objects and refs dirs)
	gitFile bool // walked: its .git is a file (a linked worktree or a submodule)
	walked  bool // found by the walk, not passed in
}

// resolution is what resolve learns about a candidate.
type resolution struct {
	gitDir string // canonical common git dir; "" for a skipped submodule
	bare   bool   // the repo is bare (only checked for walked candidates)
}

// Discover returns the common git directories of the repos found under the
// roots and of the repos that contain each extra path, without duplicates,
// sorted by PathKey. Problems with single paths come back as errors (each an
// *fs.PathError) and don't stop discovery. See the package doc for the
// walk's rules.
func (s *Scanner) Discover(ctx context.Context, extra ...string) ([]string, []error) {
	return s.discover(ctx, s.newCanon(), extra)
}

// discover is Discover with the scan's canon.
func (s *Scanner) discover(ctx context.Context, c *canon, extra []string) ([]string, []error) {
	var (
		cands []candidate
		errs  []error
	)
	for _, root := range s.opts.Roots {
		found, err := s.walk(root)
		if err != nil {
			errs = append(errs, err)
		}
		cands = append(cands, found...)
	}
	for _, p := range extra {
		cands = append(cands, candidate{dir: absPath(p)})
	}

	type resolved struct {
		resolution
		err error
	}
	results := make([]resolved, len(cands))
	var wg sync.WaitGroup
	for i, cand := range cands {
		wg.Go(func() {
			results[i].resolution, results[i].err = s.resolve(ctx, c, cand)
		})
	}
	wg.Wait()

	// A repo reached from a path passed in is explicit, even if the walk
	// also found it.
	explicit := map[string]bool{}
	for i, r := range results {
		if !cands[i].walked && r.err == nil && r.gitDir != "" {
			explicit[PathKey(r.gitDir)] = true
		}
	}
	seen := map[string]bool{}
	var dirs []string
	for _, r := range results {
		key := PathKey(r.gitDir)
		switch {
		case r.err != nil:
			errs = append(errs, r.err)
		case r.gitDir == "": // skipped (a submodule)
		case seen[key]:
		case r.bare && !explicit[key]:
			seen[key] = true
			errs = append(errs, &fs.PathError{Op: "discover", Path: r.gitDir, Err: ErrBareSkipped})
		default:
			seen[key] = true
			dirs = append(dirs, r.gitDir)
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return PathKey(dirs[i]) < PathKey(dirs[j]) })
	return dirs, errs
}

// CommonDir returns the canonical common git directory of the repo that
// contains path, which can be any directory in any worktree: git's
// `rev-parse --git-common-dir`. It fails with ErrNotRepo (in an
// *fs.PathError) when path is not in a repo.
func (s *Scanner) CommonDir(ctx context.Context, path string) (string, error) {
	r, err := s.resolve(ctx, s.newCanon(), candidate{dir: absPath(path)})
	return r.gitDir, err
}

// absPath makes p absolute, falling back to cleaning it.
func absPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return filepath.Clean(p)
}

// walk returns the repo candidates under root, to MaxDepth levels below it.
// Only an unreadable root is an error; unreadable directories below it are
// skipped.
func (s *Scanner) walk(root string) ([]candidate, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, &fs.PathError{Op: "walk", Path: root, Err: unwrapPathErr(err)}
	}
	skip := map[string]bool{".git": true}
	for _, name := range s.opts.SkipDirs {
		skip[name] = true
	}
	var out []candidate
	var visit func(dir string, entries []os.DirEntry, depth int)
	visit = func(dir string, entries []os.DirEntry, depth int) {
		var dotGit, gitFile, head, objects, refs bool
		for _, e := range entries {
			switch e.Name() {
			case ".git":
				dotGit, gitFile = true, !e.IsDir()
			case "HEAD":
				head = e.Type().IsRegular()
			case "objects":
				objects = e.IsDir()
			case "refs":
				refs = e.IsDir()
			}
		}
		switch {
		case dotGit:
			out = append(out, candidate{dir: dir, gitFile: gitFile, walked: true})
		case head && objects && refs:
			out = append(out, candidate{dir: dir, bare: true, walked: true})
			return // nothing to find inside a bare repo
		}
		if depth >= s.opts.MaxDepth {
			return
		}
		for _, e := range entries {
			// IsDir is false for a symlink, so links aren't followed.
			if !e.IsDir() || skip[e.Name()] {
				continue
			}
			sub := filepath.Join(dir, e.Name())
			if subEntries, err := os.ReadDir(sub); err == nil {
				visit(sub, subEntries, depth+1)
			}
		}
	}
	visit(filepath.Clean(root), entries, 0)
	return out, nil
}

// resolve finds a candidate's common git directory; see resolveWalked and
// resolvePassed.
func (s *Scanner) resolve(ctx context.Context, c *canon, cand candidate) (resolution, error) {
	var (
		r   resolution
		err error
	)
	if cand.walked {
		r, err = s.resolveWalked(ctx, c, cand)
	} else {
		r, err = s.resolvePassed(ctx, c, cand.dir)
	}
	if err != nil {
		if isNotRepo(err) {
			err = ErrNotRepo
		}
		return resolution{}, &fs.PathError{Op: "resolve", Path: cand.dir, Err: err}
	}
	return r, nil
}

// resolveWalked resolves a directory the walk found. A bare-looking one is
// not given to git at all (it will be skipped). Otherwise git's normal
// discovery runs with a ceiling, so a broken .git can't resolve to an
// enclosing repo, and a linked worktree is checked for a bare main repo via
// core.bare (reading config runs nothing). A submodule resolves to an empty
// gitDir.
func (s *Scanner) resolveWalked(ctx context.Context, c *canon, cand candidate) (resolution, error) {
	if cand.bare {
		gitDir, err := c.path(cand.dir)
		return resolution{gitDir: gitDir, bare: true}, err
	}
	env := ceilingEnv(cand.dir)
	out, err := s.git(ctx, cand.dir, env, "rev-parse", "--path-format=absolute", "--git-common-dir", "--show-superproject-working-tree")
	if err != nil {
		return resolution{}, err
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) > 1 && lines[1] != "" {
		return resolution{}, nil // a submodule: part of its superproject
	}
	gitDir, err := c.path(lines[0])
	if err != nil {
		return resolution{}, err
	}
	r := resolution{gitDir: gitDir}
	if cand.gitFile {
		out, err := s.git(ctx, cand.dir, env, "config", "--type=bool", "--get", "core.bare")
		switch {
		case err == nil:
			r.bare = strings.TrimSpace(string(out)) == "true"
		case exitCode(err) == 1: // core.bare unset
		default:
			return resolution{}, err
		}
	}
	return r, nil
}

// resolvePassed resolves a path the caller passed in, letting git decide what
// it is rather than guessing from its layout:
//
//  1. git's normal discovery from dir, without --git-dir. It looks for a .git
//     entry before treating a directory as a bare git dir, and it applies
//     safe.bareRepository.
//  2. If discovery found a bare repo but safe.bareRepository=explicit refused
//     it, dir is named with --git-dir only when dir itself is that git dir:
//     the repo git's refusal names is dir, dir has no .git entry (git looks
//     for one first), and with --git-dir git reports dir as a bare repo that
//     is its own common dir. A path inside a bare repo is refused, and no
//     git command names it with --git-dir.
//
// Either way a bare result is refused (ErrEmbeddedBare) when any ancestor of
// its directory holds a .git entry, or git says its parent is in a working
// tree (see checkNotEmbedded).
func (s *Scanner) resolvePassed(ctx context.Context, c *canon, dir string) (resolution, error) {
	fi, err := os.Stat(dir)
	if err != nil {
		return resolution{}, unwrapPathErr(err)
	}
	if !fi.IsDir() {
		return resolution{}, fs.ErrInvalid
	}

	bareAndDir := []string{"rev-parse", "--path-format=absolute", "--is-bare-repository", "--git-common-dir"}
	out, err := s.git(ctx, dir, nil, bareAndDir...)
	if err == nil {
		bare, gitDir, err := parseBareAndDir(c, out)
		if err != nil {
			return resolution{}, err
		}
		if bare {
			if err := s.checkNotEmbedded(ctx, gitDir); err != nil {
				return resolution{}, err
			}
		}
		return resolution{gitDir: gitDir}, nil
	}
	if !isBareRefused(err) {
		return resolution{}, err
	}

	refused := err
	self, err := c.path(dir)
	if err != nil {
		return resolution{}, err
	}
	found, err := c.path(refusedBarePath(refused))
	if err != nil || !SamePath(found, self) {
		return resolution{}, refused // dir is inside the bare repo, not the repo itself
	}
	if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
		return resolution{}, refused // git would have used dir/.git; don't second-guess it
	}
	if err := s.checkNotEmbedded(ctx, self); err != nil {
		return resolution{}, err
	}
	out, err = s.git(ctx, dir, nil, append([]string{gitDirArg(dir)}, bareAndDir...)...)
	if err != nil {
		return resolution{}, err
	}
	bare, gitDir, err := parseBareAndDir(c, out)
	if err != nil {
		return resolution{}, err
	}
	if !bare || !SamePath(gitDir, self) {
		return resolution{}, refused
	}
	return resolution{gitDir: gitDir}, nil
}

// dotGitAncestor returns the nearest ancestor of dir (not dir itself), up to
// the filesystem root, that holds a .git entry of any kind, or "" if none
// does.
func dotGitAncestor(dir string) string {
	for p := filepath.Dir(filepath.Clean(dir)); ; p = filepath.Dir(p) {
		if _, err := os.Lstat(filepath.Join(p, ".git")); err == nil {
			return p
		}
		if filepath.Dir(p) == p {
			return ""
		}
	}
}

// refusedBarePath returns the repo named in git's safe.bareRepository
// refusal, "cannot use bare repository '<path>' (safe.bareRepository is
// 'explicit')", or "" if the message doesn't have that form (LC_ALL=C keeps
// it in English).
func refusedBarePath(err error) string {
	var ge *GitError
	if !errors.As(err, &ge) {
		return ""
	}
	_, rest, ok := strings.Cut(ge.Stderr, "cannot use bare repository '")
	if !ok {
		return ""
	}
	path, _, ok := strings.Cut(rest, "' (safe.bareRepository")
	if !ok {
		return ""
	}
	return path
}

// parseBareAndDir parses `rev-parse --is-bare-repository --git-common-dir`
// output and canonicalizes the git dir.
func parseBareAndDir(c *canon, out []byte) (bare bool, gitDir string, err error) {
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) != 2 {
		return false, "", fmt.Errorf("rev-parse: unexpected output %q", out)
	}
	gitDir, err = c.path(lines[1])
	return lines[0] == "true", gitDir, err
}

// checkNotEmbedded refuses a bare repo at dir that sits inside another
// repo's working tree. First, with no git involved, every ancestor of dir
// up to the filesystem root is checked for a .git entry (file or
// directory): asking git from the parent alone misses a bare repo nested
// in a bare-looking wrapper, where git answers "not a work tree". Then git
// is asked from dir's parent. Not being in a work tree (no repo, or inside
// a bare repo or a .git dir) is fine; any other failure refuses too, since
// it couldn't be ruled out.
func (s *Scanner) checkNotEmbedded(ctx context.Context, dir string) error {
	if a := dotGitAncestor(dir); a != "" {
		return fmt.Errorf("%w (%s has a .git)", ErrEmbeddedBare, a)
	}
	out, err := s.git(ctx, filepath.Dir(dir), nil, "rev-parse", "--show-toplevel")
	switch {
	case err == nil:
		return fmt.Errorf("%w (%s)", ErrEmbeddedBare, strings.TrimSpace(string(out)))
	case isNotRepo(err), isBareRefused(err), stderrHas(err, "must be run in a work tree"):
		return nil
	default:
		return fmt.Errorf("%w: can't rule it out: %w", ErrEmbeddedBare, err)
	}
}

// unwrapPathErr drops an *fs.PathError wrapper so the caller can wrap the
// cause with its own op and path.
func unwrapPathErr(err error) error {
	if pe, ok := err.(*fs.PathError); ok {
		return pe.Err
	}
	return err
}
