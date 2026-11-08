package worktrees

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestDescribeDir(t *testing.T) {
	parallel(t)
	requireGit(t)
	base := tempDir(t)
	repo := filepath.Join(base, "app")
	newRepo(t, repo, d1)
	plugin := filepath.Join(repo, "plugin")
	write(t, filepath.Join(plugin, "mod", "register.tsx"), "v1\n")
	gitT(t, repo, "add", ".")
	commitAt(t, repo, "plugin", d2)
	gitT(t, repo, "tag", "v1.0.0")
	s := New(Options{})
	ctx := context.Background()

	describe := func(dir string) string {
		t.Helper()
		got, err := s.DescribeDir(ctx, dir)
		if err != nil {
			t.Fatalf("DescribeDir(%s): %v", dir, err)
		}
		return got
	}
	if got := describe(plugin); got != "v1.0.0" {
		t.Errorf("at the tag: %q, want v1.0.0", got)
	}

	// A change to the plugin moves its name; one elsewhere doesn't.
	write(t, filepath.Join(plugin, "mod", "register.tsx"), "v2\n")
	gitT(t, repo, "add", ".")
	c := commitAt(t, repo, "plugin v2", d3)
	want := gitT(t, repo, "describe", "--tags", "--always", c)
	if !strings.HasPrefix(want, "v1.0.0-1-g") {
		t.Fatalf("git describe = %q", want)
	}
	if got := describe(plugin); got != want {
		t.Errorf("after a plugin commit: %q, want %q", got, want)
	}
	write(t, filepath.Join(repo, "README"), "changed\n")
	gitT(t, repo, "commit", "-q", "-am", "readme")
	if got := describe(plugin); got != want {
		t.Errorf("after a commit outside the plugin: %q, want %q still", got, want)
	}
	// A deeper folder of the plugin is described the same way.
	if got := describe(filepath.Join(plugin, "mod")); got != want {
		t.Errorf("plugin/mod: %q, want %q", got, want)
	}

	// Untracked files don't count; a changed tracked file, staged or not, does.
	write(t, filepath.Join(plugin, "notes.txt"), "scratch\n")
	if got := describe(plugin); got != want {
		t.Errorf("with an untracked file: %q, want %q", got, want)
	}
	write(t, filepath.Join(plugin, "mod", "register.tsx"), "v3\n")
	if got := describe(plugin); got != want+"-dirty" {
		t.Errorf("with a change: %q, want %q", got, want+"-dirty")
	}
	gitT(t, repo, "add", "plugin/mod/register.tsx")
	if got := describe(plugin); got != want+"-dirty" {
		t.Errorf("with a staged change: %q, want %q", got, want+"-dirty")
	}
	// A change outside the folder doesn't make it dirty.
	gitT(t, repo, "reset", "-q", "--hard")
	write(t, filepath.Join(repo, "README"), "again\n")
	if got := describe(plugin); got != want {
		t.Errorf("with a change outside the plugin: %q, want %q", got, want)
	}

	// No tag: the short hash.
	other := filepath.Join(base, "other")
	c0 := newRepo(t, other, d1)
	if got, short := describe(other), gitT(t, other, "rev-parse", "--short", c0); got != short {
		t.Errorf("untagged: %q, want %q", got, short)
	}

	// A folder no commit changed, and one in no repo.
	untracked := filepath.Join(repo, "scratch")
	mkdir(t, untracked)
	if _, err := s.DescribeDir(ctx, untracked); !errors.Is(err, ErrNoCommitHere) {
		t.Errorf("untracked folder: %v, want ErrNoCommitHere", err)
	}
	outside := filepath.Join(base, "loose")
	mkdir(t, outside)
	if _, err := s.DescribeDir(ctx, outside); !errors.Is(err, ErrNoCommitHere) {
		t.Errorf("no repo: %v, want ErrNoCommitHere", err)
	}
}
