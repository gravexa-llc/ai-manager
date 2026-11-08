package planapi

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
	"github.com/gravexa-llc/ai-manager/internal/worktrees"
)

// Context answers GET /api/context?cwd=<path> for the mod, at session
// start: the repo cwd is in, the plans registered for it (with each
// package's convention data), and the package cwd's worktree or branch is
// linked to, if any. A cwd outside every repo aimd may look at gives an
// empty context, not an error.
func (s *Service) Context(ctx context.Context, cwd string) (api.Context, error) {
	cwd = strings.TrimSpace(cwd)
	if cwd == "" {
		return api.Context{}, badRequest("cwd is required")
	}
	if !filepath.IsAbs(cwd) {
		return api.Context{}, badRequest("cwd must be absolute: %q", cwd)
	}
	cwd = filepath.Clean(cwd)
	out := api.Context{Cwd: cwd, Plans: []api.ContextPlan{}}
	look := cwd
	if r, err := filepath.EvalSymlinks(cwd); err == nil {
		look = r
	}
	ri, ok := s.repoOf(ctx, look)
	if !ok {
		return out, nil
	}
	out.Repo = &api.ContextRepo{Path: ri.path, Name: ri.name}
	if wt := ri.wt; wt != nil {
		out.Worktree = &api.ContextWorktree{Path: wt.Path, Branch: wt.Branch, Detached: wt.Detached, Main: wt.Main}
	}
	boards, err := s.boardsNow(ctx)
	if err != nil {
		return api.Context{}, err
	}
	if boards, err = s.withBands(ctx, boards); err != nil {
		return api.Context{}, err
	}
	for _, b := range boards {
		if planFor(b, ri) {
			out.Plans = append(out.Plans, contextPlan(b))
		}
	}
	if ri.wt == nil {
		return out, nil
	}
	for _, b := range boards {
		for _, p := range b.Packages {
			for _, l := range p.Links {
				if linksHere(l, ri) {
					out.Candidates = append(out.Candidates, api.ContextPackage{
						Plan: b.Slug, ID: p.ID, Title: p.Title, Lifecycle: p.Lifecycle, Convention: p.Convention, Link: l,
					})
					break
				}
			}
		}
	}
	if len(out.Candidates) > 0 {
		first := out.Candidates[0]
		out.Package = &first
	}
	if len(out.Candidates) < 2 {
		out.Candidates = nil
	}
	return out, nil
}

// planFor reports whether a plan is registered for the repo.
func planFor(b api.Plan, ri repoInfo) bool {
	for _, r := range b.Repos {
		if ri.name != "" && strings.EqualFold(r, ri.name) {
			return true
		}
		if filepath.IsAbs(r) && worktrees.SamePath(r, ri.path) {
			return true
		}
	}
	return false
}

func contextPlan(b api.Plan) api.ContextPlan {
	cp := api.ContextPlan{Slug: b.Slug, Title: b.Title, Path: b.Path, Band: b.Band, Packages: make([]api.ContextPlanPackage, 0, len(b.Packages))}
	for _, p := range b.Packages {
		cp.Packages = append(cp.Packages, api.ContextPlanPackage{ID: p.ID, Title: p.Title, Repo: p.Repo, Lifecycle: p.Lifecycle,
			Convention: p.Convention, Provisional: p.Provisional})
	}
	return cp
}

// linksHere reports whether a package's link points at the worktree a
// context is in (not the main checkout, where the lead works), or at its
// branch.
func linksHere(l api.Link, ri repoInfo) bool {
	wt := ri.wt
	switch l.Kind {
	case store.LinkWorktree:
		return !wt.Main && worktrees.SamePath(l.Ref, wt.Path)
	case store.LinkBranch:
		if wt.Detached || wt.Branch == "" {
			return false
		}
		repo, branch, ok := strings.Cut(l.Ref, ":")
		if !ok || branch != wt.Branch {
			return false
		}
		return (ri.name != "" && strings.EqualFold(repo, ri.name)) || (filepath.IsAbs(repo) && worktrees.SamePath(repo, ri.path))
	}
	return false
}
