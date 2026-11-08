package planapi

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/worktrees"
)

// repoInfo is a git repo as the plan API names it.
type repoInfo struct {
	path string // the main checkout
	name string // GitHub nameWithOwner, canonical; "" without a GitHub remote
	repo *worktrees.Repo
	wt   *worktrees.Worktree // the worktree the looked-up path is in, if known
}

// key is how the plan is registered for the repo: its GitHub name, or its
// main checkout's path.
func (r repoInfo) key() string {
	if r.name != "" {
		return r.name
	}
	return r.path
}

// repoLookupTimeout bounds reading a repo the last scan doesn't have.
const repoLookupTimeout = 5 * time.Second

// repoOf returns the repo a path is in: from the daemon's last scan when
// it has the path, otherwise by asking git (only for a path inside a root
// or a known repo). ok is false when the path isn't in a git repo aimd
// may look at.
func (s *Service) repoOf(ctx context.Context, path string) (repoInfo, bool) {
	if scan := s.lastScan(); scan != nil {
		if repo, wt := scan.Lookup(path); repo != nil {
			return s.info(repo, wt), true
		}
	}
	if !s.allowed(path) {
		return repoInfo{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, repoLookupTimeout)
	defer cancel()
	dir := path
	if isMarkdown(path) {
		dir = filepath.Dir(path)
	}
	gitDir, err := s.scanner.CommonDir(ctx, dir)
	if err != nil {
		return repoInfo{}, false
	}
	repos := s.scanner.ScanRepos(ctx, []string{gitDir})
	if len(repos) != 1 || repos[0].Path == "" {
		return repoInfo{}, false
	}
	repo := &repos[0]
	res := worktrees.Result{Repos: repos}
	_, wt := res.Lookup(path)
	return s.info(repo, wt), true
}

func (s *Service) info(repo *worktrees.Repo, wt *worktrees.Worktree) repoInfo {
	ri := repoInfo{path: repo.Path, repo: repo, wt: wt}
	if owner, name, ok := repo.GitHubRepo(); ok {
		ri.name = s.canonical(owner + "/" + name)
	}
	return ri
}

// githubName matches owner/name.
var githubName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`)

// normRepos turns the repos a request names into the keys a plan is
// registered for: "owner/name" (a GitHub URL is accepted too) is kept
// (canonical when known); an absolute path names a local repo, which is
// registered under its GitHub name, or its main checkout's path without
// one. Repeats are dropped, ignoring case.
func (s *Service) normRepos(ctx context.Context, in []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	add := func(k string) {
		if !seen[strings.ToLower(k)] {
			seen[strings.ToLower(k)] = true
			out = append(out, k)
		}
	}
	for _, r := range in {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if filepath.IsAbs(r) {
			ri, ok := s.repoOf(ctx, filepath.Clean(r))
			if !ok {
				return nil, badRequest("repos: %s isn't in a git repo inside the configured roots or a repo aimd knows", r)
			}
			add(ri.key())
			continue
		}
		name := strings.TrimSuffix(strings.TrimSuffix(r, "/"), ".git")
		for _, p := range []string{"https://github.com/", "http://github.com/", "github.com/"} {
			if len(name) > len(p) && strings.EqualFold(name[:len(p)], p) {
				name = name[len(p):]
			}
		}
		if !githubName.MatchString(name) {
			return nil, badRequest("repos: %q is neither owner/name nor an absolute path", r)
		}
		add(s.canonical(name))
	}
	return out, nil
}
