package worktrees

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// This file holds the git calls `aim prune` makes beyond the scan:
// reads that decide whether a worktree is safe to remove, and the three
// writes that remove it. They all go through the scanner's hardened runner
// (see the package doc), under its concurrency limit and timeout.

// ErrNoCommit is returned (wrapped) when a revision a check needs isn't a
// commit in the local repo, such as a PR head that was never fetched. The
// answer is then unknown, not "no".
var ErrNoCommit = errors.New("not a commit in the local repo")

// Ignored lists the ignored files and directories in the worktree at w.Path,
// relative to it, with a trailing "/" on directories: what
// `git worktree remove` deletes without asking. It runs
// `git status --porcelain=v2 -z --ignored=matching`, which names each path
// that an ignore pattern matched; a matched directory is listed once, not
// its contents. Like the scan's status call, it runs with the worktree's
// filter drivers disabled and submodules left alone. For a worktree whose
// status the scan read, the drivers are the ones that scan listed;
// otherwise they are listed now.
func (s *Scanner) Ignored(ctx context.Context, w *Worktree) ([]string, error) {
	if w.Bare || w.Missing || w.Path == "" {
		return nil, errors.New("no working tree to read")
	}
	env := ceilingEnv(w.Path)
	filters := w.filters
	if !w.filtersRead {
		var err error
		if filters, err = s.filterOverrides(ctx, w.Path, env); err != nil {
			return nil, err
		}
	}
	args := append(slices.Clip(filters), "status", "--porcelain=v2", "-z", "--ignored=matching",
		"--untracked-files=normal", "--ignore-submodules=dirty")
	out, err := s.git(ctx, w.Path, env, args...)
	if err != nil {
		return nil, err
	}
	return parseIgnored(out)
}

// parseIgnored returns the paths of the ignored ("!") entries in
// `git status --porcelain=v2 -z` output.
func parseIgnored(data []byte) ([]string, error) {
	var out []string
	fields := strings.Split(string(data), "\x00")
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if f == "" {
			continue
		}
		switch f[0] {
		case '!':
			p, ok := strings.CutPrefix(f, "! ")
			if !ok || p == "" {
				return nil, fmt.Errorf("status: unexpected entry %q", f)
			}
			out = append(out, p)
		case '2':
			i++ // the original path of a rename
		case '1', 'u', '?', '#':
		default:
			return nil, fmt.Errorf("status: unexpected entry %q", f)
		}
	}
	return out, nil
}

// ResolveCommit returns the full SHA of rev in the repo with common git
// directory gitDir, or an error wrapping ErrNoCommit when rev doesn't name a
// commit there. Nothing is fetched: a commit that only exists on GitHub is
// ErrNoCommit.
func (s *Scanner) ResolveCommit(ctx context.Context, gitDir, rev string) (string, error) {
	out, err := s.git(ctx, gitDir, nil, gitDirArg(gitDir), "rev-parse", "--verify", "--quiet",
		"--end-of-options", rev+"^{commit}")
	if err != nil {
		if exitCode(err) == 1 { // --quiet: not a valid object name, or not a commit
			return "", fmt.Errorf("%s: %w", rev, ErrNoCommit)
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// CountNotIn counts the commits reachable from tip and from none of not:
// `git rev-list --count <tip> ^<not>...`, the same as
// `rev-list --count <tip> --not <not>...`. Prune rule 4′ passes the PR head
// and the base's remote-tracking ref, so 0 means nothing on the branch is
// missing from both. Every revision must exist locally (check with
// ResolveCommit first); a missing one is an error, never a count.
func (s *Scanner) CountNotIn(ctx context.Context, gitDir, tip string, not ...string) (int, error) {
	// ^rev rather than --not, which git refuses after --end-of-options.
	args := []string{gitDirArg(gitDir), "rev-list", "--count", "--end-of-options", tip}
	for _, n := range not {
		args = append(args, "^"+n)
	}
	out, err := s.git(ctx, gitDir, nil, args...)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, fmt.Errorf("rev-list --count: unexpected output %q", strings.TrimSpace(string(out)))
	}
	return n, nil
}

// IsAncestor reports whether commit is reachable from of:
// `git merge-base --is-ancestor <commit> <of>`. Exit 1 is false; anything
// else but 0 (a missing object is 128) is an error.
func (s *Scanner) IsAncestor(ctx context.Context, gitDir, commit, of string) (bool, error) {
	_, err := s.git(ctx, gitDir, nil, gitDirArg(gitDir), "merge-base", "--is-ancestor", "--end-of-options", commit, of)
	switch {
	case err == nil:
		return true, nil
	case exitCode(err) == 1:
		return false, nil
	default:
		return false, err
	}
}

// RemoteHead returns the branch that refs/remotes/<remote>/HEAD points at
// (e.g. "main"), which clones set to the remote's default branch. It
// returns "" with no error when that ref isn't set.
func (s *Scanner) RemoteHead(ctx context.Context, gitDir, remote string) (string, error) {
	ref := "refs/remotes/" + remote + "/HEAD"
	out, err := s.git(ctx, gitDir, nil, gitDirArg(gitDir), "symbolic-ref", "--quiet", ref)
	if err != nil {
		if exitCode(err) == 1 || exitCode(err) == 128 {
			return "", nil
		}
		return "", err
	}
	target := strings.TrimSpace(string(out))
	return strings.TrimPrefix(target, "refs/remotes/"+remote+"/"), nil
}

// RemoveWorktree runs `git worktree remove <path>` for w, never with
// --force: git refuses a worktree that is locked, has modified or
// untracked files, or holds submodules, and deletes ignored files without
// asking (check them with Ignored first). A worktree whose directory is
// already gone just has its admin data cleared.
//
// git checks the worktree by running `git status` in it, which inherits
// this call's -c options, so the worktree's filter drivers are disabled
// for that status too.
func (s *Scanner) RemoveWorktree(ctx context.Context, repo *Repo, w *Worktree) error {
	if w.Main || w.Bare {
		return errors.New("refusing to remove a main worktree")
	}
	var filters []string
	if !w.Missing {
		var err error
		if filters, err = s.filterOverrides(ctx, w.Path, ceilingEnv(w.Path)); err != nil {
			return err
		}
	}
	args := append(filters, gitDirArg(repo.GitDir), "worktree", "remove", "--", w.RecordedPath)
	_, err := s.git(ctx, repo.GitDir, nil, args...)
	return err
}

// ErrBranchMoved is returned by DeleteBranch when the branch no longer
// points at the expected commit.
var ErrBranchMoved = errors.New("branch moved since it was checked")

// DeleteBranch runs `git branch -D <branch>` in the repo, but only while
// refs/heads/<branch> still points at want (a full SHA); otherwise it
// returns an error wrapping ErrBranchMoved and deletes nothing. git itself
// refuses to delete a branch checked out in any worktree.
func (s *Scanner) DeleteBranch(ctx context.Context, gitDir, branch, want string) error {
	if branch == "" || !isSHA(want) {
		return fmt.Errorf("delete branch %q: invalid request", branch)
	}
	got, err := s.ResolveCommit(ctx, gitDir, "refs/heads/"+branch)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%s is at %s, not %s: %w", branch, short(got), short(want), ErrBranchMoved)
	}
	_, err = s.git(ctx, gitDir, nil, gitDirArg(gitDir), "branch", "-D", "--", branch)
	return err
}

// PruneEntry is one worktree record `git worktree prune` would clear.
type PruneEntry struct {
	// ID is the admin directory's name under <GitDir>/worktrees.
	ID string
	// Reason is git's reason, e.g. "gitdir file points to non-existent location".
	Reason string
	// Path is the worktree path the record names (from its gitdir file,
	// without the trailing /.git), or "" if that file can't be read: such a
	// record never shows in `git worktree list`.
	Path string
}

// PrunePreview returns what `git worktree prune` would clear in the repo,
// from `git worktree prune --dry-run --verbose`, which changes nothing.
// Locked worktrees are never in it.
func (s *Scanner) PrunePreview(ctx context.Context, gitDir string) ([]PruneEntry, error) {
	// --verbose reports on stderr.
	out, err := s.git(withStderr(ctx), gitDir, nil, gitDirArg(gitDir), "worktree", "prune", "--dry-run", "--verbose")
	if err != nil {
		return nil, err
	}
	var entries []PruneEntry
	for _, line := range strings.Split(string(out), "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "Removing worktrees/")
		if !ok {
			continue
		}
		id, reason, _ := strings.Cut(rest, ": ")
		e := PruneEntry{ID: id, Reason: reason}
		if data, err := os.ReadFile(filepath.Join(gitDir, "worktrees", id, "gitdir")); err == nil {
			if p := strings.TrimSpace(string(data)); p != "" {
				e.Path = strings.TrimSuffix(filepath.Clean(p), string(filepath.Separator)+".git")
			}
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// Prune runs `git worktree prune` in the repo: it clears the admin data of
// every worktree whose directory or .git file is gone, except locked ones,
// and never touches files in a worktree directory. It is repo-wide; check
// PrunePreview first.
func (s *Scanner) Prune(ctx context.Context, gitDir string) error {
	_, err := s.git(ctx, gitDir, nil, gitDirArg(gitDir), "worktree", "prune")
	return err
}

// short abbreviates a SHA for messages.
func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
