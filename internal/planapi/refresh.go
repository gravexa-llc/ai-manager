package planapi

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// maxBranchRef bounds a branch name a refresh request may name.
const maxBranchRef = 255

// RefreshPRs asks for the PR a tool call just created, merged or closed to
// be read from GitHub now (POST /api/prs/refresh), by number: the plugin
// sees the call succeed (`gh pr merge 12`, the GitHub MCP server's
// merge_pull_request, …) and says which PR. aimd reads it soon, at most
// once every few seconds however many requests come, and stores what
// GitHub answers, so the board doesn't wait for the next poll. Nothing
// is taken from the command but which PR to read.
//
// The PR is named by URL or owner/repo#123, or by a number or head branch
// in the request's repo or the repo at its cwd; with no ref, it is the
// branch checked out at cwd. A branch names its stored open PRs, and none
// when aimd has none (its branch poll finds a new one).
func (s *Service) RefreshPRs(ctx context.Context, req api.PRRefreshRequest) (api.PRRefreshResult, error) {
	keys, err := s.refreshTargets(ctx, req)
	if err != nil {
		return api.PRRefreshResult{}, err
	}
	out := api.PRRefreshResult{PRs: []string{}}
	for _, k := range keys {
		out.PRs = append(out.PRs, store.PRRef(k.Repo, k.Number))
	}
	if s.refresh != nil && len(keys) > 0 {
		out.Queued = s.refresh(keys)
	}
	s.log.Info("PR refresh", "action", req.Action, "prs", out.PRs, "queued", out.Queued)
	return out, nil
}

// refreshTargets resolves a refresh request to the PRs to read.
func (s *Service) refreshTargets(ctx context.Context, req api.PRRefreshRequest) ([]store.PRKey, error) {
	ref := strings.TrimSpace(req.Ref)
	if repo, n, ok := parsePR(ref); ok {
		return []store.PRKey{s.storedKey(ctx, repo, n)}, nil
	}
	repo, branchAt, err := s.refreshRepo(ctx, req)
	if err != nil {
		return nil, err
	}
	if n, err := strconv.Atoi(strings.TrimPrefix(ref, "#")); err == nil {
		if n < 1 {
			return nil, badRequest("ref: %q isn't a PR number", ref)
		}
		return []store.PRKey{s.storedKey(ctx, repo, n)}, nil
	}
	branch := ref
	if branch == "" {
		if branchAt == "" {
			return nil, badRequest("ref is empty and cwd %q isn't on a branch aimd knows", req.Cwd)
		}
		branch = branchAt
	}
	if len(branch) > maxBranchRef || strings.ContainsAny(branch, " \t\n") {
		return nil, badRequest("ref: %q is neither a PR nor a branch", ref)
	}
	prs, err := s.st.ListPRs(ctx, store.PRFilter{HeadBranch: branch, State: store.PROpen})
	if err != nil {
		return nil, err
	}
	var keys []store.PRKey
	for _, p := range prs {
		if strings.EqualFold(p.Repo, repo) && !p.IsCrossRepository {
			keys = append(keys, store.PRKey{Repo: p.Repo, Number: p.Number})
		}
	}
	return keys, nil
}

// refreshRepo returns the GitHub repo a refresh request's number or branch
// is in: its repo, else the repo at its cwd, with the branch checked out
// there.
func (s *Service) refreshRepo(ctx context.Context, req api.PRRefreshRequest) (repo, branch string, err error) {
	if r := strings.TrimSpace(req.Repo); r != "" {
		name := strings.TrimSuffix(strings.TrimSuffix(r, "/"), ".git")
		for _, p := range []string{"https://github.com/", "http://github.com/", "github.com/"} {
			if len(name) > len(p) && strings.EqualFold(name[:len(p)], p) {
				name = name[len(p):]
			}
		}
		if !githubName.MatchString(name) {
			return "", "", badRequest("repo: %q isn't owner/repo", r)
		}
		return s.canonical(name), "", nil
	}
	cwd := strings.TrimSpace(req.Cwd)
	if cwd == "" || !filepath.IsAbs(cwd) {
		return "", "", badRequest("a PR named by number or branch needs repo, or cwd as an absolute path")
	}
	ri, ok := s.repoOf(ctx, filepath.Clean(cwd))
	if !ok || ri.name == "" {
		return "", "", badRequest("cwd %s isn't in a GitHub repo aimd knows", cwd)
	}
	if ri.wt != nil && !ri.wt.Detached {
		branch = ri.wt.Branch
	}
	return ri.name, branch, nil
}

// storedKey is a PR's key with its repo as GitHub spells it, when aimd has
// the PR.
func (s *Service) storedKey(ctx context.Context, repo string, n int) store.PRKey {
	if pr, err := s.st.GetPR(ctx, repo, n); err == nil {
		repo = pr.Repo
	} else if !errors.Is(err, store.ErrNotFound) {
		s.log.Warn("PR refresh: read the store", "pr", store.PRRef(repo, n), "err", err)
	}
	return store.PRKey{Repo: repo, Number: n}
}

// refreshNow asks for a PR the link API just stored without reading it
// to be read from GitHub soon.
func (s *Service) refreshNow(repo string, n int) {
	if s.refresh != nil {
		s.refresh([]store.PRKey{{Repo: repo, Number: n}})
	}
}
