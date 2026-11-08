package worktrees

import (
	"context"
	"errors"
	"slices"
	"strings"
)

// ErrNoCommitHere is returned (wrapped) by DescribeDir for a folder no
// commit has changed: untracked, or in no repository git found.
var ErrNoCommitHere = errors.New("no commit changed this folder")

// DescribeDir names the build of the files under dir: `git describe
// --tags --always` of the last commit that changed them, with "-dirty" when
// their tracked files differ from that commit (staged or not; untracked
// files don't count), e.g. "v1.0.0-12-gabc1234" or "abc1234-dirty".
//
// It is how a plugin build is identified (internal/events PluginBuild):
// the plugin's mod runs the same three commands in its own folder at
// session start, and aimd runs them in that folder later to see whether
// the build on disk has moved on. Commits that change nothing under dir
// leave its name alone, so a pull that only touches the daemon doesn't
// make every session's plugin look old.
//
// dir may be anywhere in a worktree: git searches upward from it for the
// repository. The calls go through the scanner's hardened runner, with
// the worktree's filter drivers disabled for the status call.
func (s *Scanner) DescribeDir(ctx context.Context, dir string) (string, error) {
	out, err := s.git(ctx, dir, nil, "log", "-1", "--format=%H", "--", ".")
	if err != nil {
		if isNotRepo(err) {
			return "", errors.Join(ErrNoCommitHere, err)
		}
		return "", err
	}
	commit := strings.TrimSpace(string(out))
	if !isSHA(commit) {
		return "", ErrNoCommitHere
	}
	out, err = s.git(ctx, dir, nil, "describe", "--tags", "--always", commit)
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(string(out))
	if name == "" {
		return "", errors.New("git describe: no output")
	}
	filters, err := s.filterOverrides(ctx, dir, nil)
	if err != nil {
		return "", err
	}
	args := append(slices.Clip(filters), "status", "--porcelain", "--untracked-files=no", "--ignore-submodules=dirty", "--", ".")
	out, err = s.git(ctx, dir, nil, args...)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(string(out)) != "" {
		name += "-dirty"
	}
	return name, nil
}
