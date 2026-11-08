package planapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/plans"
	"github.com/gravexa-llc/ai-manager/internal/state"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// Where a registered plan's repos came from (api.RegisterResponse).
const (
	ReposFromRequest = "request"
	ReposFromPlan    = "plan"
	ReposFromFile    = "file"
)

// Register registers a plan file, or registers it again (POST /api/plans):
// it reads and parses the file, takes its slug from plans.Slug and its
// repos from the request, the plan's Repos: line or the file's own repo,
// stores the plan and its packages, watches the file, runs a state pass,
// and starts the repos' PR backfill. A file registered already is
// updated (Created false): its packages are synced, so links and Mark done
// are kept. A request with a session id makes that session a monitored
// lead of the plan (store.MonitorPlanLead); each session that registers
// the plan is one.
//
// Refusals: the path checks (checkPath), a file with no package table
// (422), a slug another file's plan has (409), and repos that can't be
// told (422).
func (s *Service) Register(ctx context.Context, req api.RegisterRequest) (api.RegisterResponse, error) {
	f, err := s.readPlanFile(req.Path)
	if err != nil {
		return api.RegisterResponse{}, err
	}
	parsed := plans.Parse(f.src)
	if len(parsed.Packages) == 0 {
		return api.RegisterResponse{}, refuse(http.StatusUnprocessableEntity,
			"%s has no package table: a plan needs a table with a package column (Package, ID, …) and a Needs column, whose rows start with a package ID such as **A1 Title**", f.path)
	}
	slug := plans.Slug(f.path, f.src)

	repos, from, err := s.planRepos(ctx, f, req.Repos)
	if err != nil {
		return api.RegisterResponse{}, err
	}

	existing, err := s.st.GetPlanBySourcePath(ctx, f.path)
	switch {
	case errors.Is(err, store.ErrNotFound):
		existing = store.Plan{}
	case err != nil:
		return api.RegisterResponse{}, err
	}
	if other, err := s.st.GetPlanBySlug(ctx, slug); err == nil && other.ID != existing.ID {
		return api.RegisterResponse{}, refuse(http.StatusConflict,
			"the slug %q is taken by the plan registered from %s; give this plan its own slug with a line \"Plan slug: <name>\"", slug, other.SourcePath)
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return api.RegisterResponse{}, err
	}

	p := existing
	p.Slug, p.Title, p.SourcePath, p.SourceMD, p.Repos = slug, parsed.Title, f.path, string(f.src), repos
	if req.SessionID != "" {
		p.RegisteredInSession = req.SessionID
	}
	created := existing.ID == 0
	if created {
		if p.ID, err = s.st.InsertPlan(ctx, p); err != nil {
			return api.RegisterResponse{}, err
		}
	} else if err := s.st.UpdatePlan(ctx, p); err != nil {
		return api.RegisterResponse{}, err
	}
	if err := s.st.SyncPackages(ctx, p.ID, state.StorePackages(parsed)); err != nil {
		return api.RegisterResponse{}, err
	}
	if err := s.st.UndismissPlanFile(ctx, f.path); err != nil {
		return api.RegisterResponse{}, err
	}
	// The registering session is the plan's lead: monitored from now on
	// (internal/monitor), until the plan is unregistered.
	if req.SessionID != "" {
		if _, err := s.st.MonitorPlanLead(ctx, req.SessionID, p.ID, s.now()); err != nil {
			return api.RegisterResponse{}, err
		}
	}
	s.watch(f.path)
	s.log.Info("plan registered", "plan", slug, "path", f.path, "repos", repos, "created", created)

	if err := s.runState(ctx); err != nil {
		return api.RegisterResponse{}, err
	}
	b, err := s.board(ctx, slug)
	if err != nil {
		return api.RegisterResponse{}, err
	}
	sum, err := s.summaryNow(ctx, b, time.Time{})
	if err != nil {
		return api.RegisterResponse{}, err
	}
	resp := api.RegisterResponse{
		Created: created, Slug: slug, Title: p.Title, Path: f.path, Repos: repos, ReposFrom: from,
		Packages: len(parsed.Packages), Warnings: len(b.Warnings), Backfill: "off", Summary: sum,
	}
	if s.startBackfill(repos) {
		resp.Backfill = "started"
	}
	return resp, nil
}

// planRepos works out the repos to register a plan for, and where they
// came from: the request's, else the plan's Repos: line, else the repo
// the file is in.
func (s *Service) planRepos(ctx context.Context, f planFile, asked []string) ([]string, string, error) {
	repos, err := s.normRepos(ctx, asked)
	if err != nil {
		return nil, "", err
	}
	if len(repos) > 0 {
		return repos, ReposFromRequest, nil
	}
	if marked := plans.Repos(f.src); len(marked) > 0 {
		repos, err := s.normRepos(ctx, marked)
		if err != nil {
			return nil, "", err
		}
		return repos, ReposFromPlan, nil
	}
	if ri, ok := s.repoOf(ctx, f.path); ok {
		return []string{ri.key()}, ReposFromFile, nil
	}
	return nil, "", refuse(http.StatusUnprocessableEntity,
		"can't tell which repos %s's packages live in: the file isn't in a git repo; add a line \"Repos: owner/name, …\" to the plan or pass repos", f.path)
}

// startBackfill reads the GitHub repos' PRs in the background, then runs a
// state pass. It reports whether it started.
func (s *Service) startBackfill(repos []string) bool {
	if s.backfill == nil {
		return false
	}
	var gh []string
	for _, r := range repos {
		if githubName.MatchString(r) {
			gh = append(gh, r)
		}
	}
	if len(gh) == 0 {
		return false
	}
	ctx := s.ctx()
	s.bg.Go(func() {
		s.backfill(ctx, gh)
		if ctx.Err() == nil {
			s.pass(ctx)
		}
	})
	return true
}

// Wait waits for the backfills in flight. Tests use it; Run does it
// before it returns.
func (s *Service) Wait() { s.bg.Wait() }

// Detect says whether a markdown file looks like a dispatch plan, and
// whether it is registered or dismissed already (POST /api/plans/detect).
// The path checks apply.
func (s *Service) Detect(ctx context.Context, path string) (api.Detection, error) {
	f, err := s.readPlanFile(path)
	if err != nil {
		return api.Detection{}, err
	}
	d := plans.Detect(f.src)
	out := api.Detection{
		Path: f.path, IsPlan: d.IsPlan, Title: d.Title, Packages: d.Packages, Lanes: d.Lanes,
		Slug: plans.Slug(f.path, f.src),
	}
	p, err := s.st.GetPlanBySourcePath(ctx, f.path)
	switch {
	case err == nil:
		out.Registered, out.Slug = true, p.Slug
	case !errors.Is(err, store.ErrNotFound):
		return api.Detection{}, err
	}
	at, dismissed, err := s.st.PlanFileDismissed(ctx, f.path)
	if err != nil {
		return api.Detection{}, err
	}
	out.Dismissed, out.DismissedAt = dismissed, at
	out.Offer = out.IsPlan && !out.Registered && !out.Dismissed
	return out, nil
}

// Dismiss remembers that a markdown file isn't a plan, so the band stops
// offering it (POST /api/plans/dismiss). The path checks apply.
// Registering the file later clears it.
func (s *Service) Dismiss(ctx context.Context, path string) (api.Dismissal, error) {
	resolved, err := s.checkPath(path)
	if err != nil {
		return api.Dismissal{}, err
	}
	at, err := s.st.DismissPlanFile(ctx, resolved, s.now())
	if err != nil {
		return api.Dismissal{}, err
	}
	s.log.Info("plan file dismissed", "path", resolved)
	return api.Dismissal{Path: resolved, Dismissed: true, DismissedAt: at}, nil
}

// Unregister removes a registered plan (DELETE /api/plans/{slug}): the
// plan, its packages, its stored links (spawn, manual and the matcher's),
// its unlink tombstones and Mark done all go from aimd's database. The
// plan file, git and GitHub are never touched, and PR rows stay. The file
// stops being watched, and a state pass runs, so the board is gone from
// every answer at once. Registering the file again starts it afresh.
func (s *Service) Unregister(ctx context.Context, slug string) (api.Unregistered, error) {
	p, err := s.planBySlug(ctx, slug)
	if err != nil {
		return api.Unregistered{}, err
	}
	pkgs, err := s.st.ListPackages(ctx, p.ID)
	if err != nil {
		return api.Unregistered{}, err
	}
	links, err := s.st.ListLinks(ctx, p.ID)
	if err != nil {
		return api.Unregistered{}, err
	}
	if err := s.st.DeletePlan(ctx, p.ID); err != nil {
		return api.Unregistered{}, orNotFound(err, "no plan %q is registered", slug)
	}
	s.unwatch(p.SourcePath)
	s.log.Info("plan unregistered", "plan", p.Slug, "path", p.SourcePath, "packages", len(pkgs), "links", len(links))
	if err := s.runState(ctx); err != nil {
		return api.Unregistered{}, err
	}
	return api.Unregistered{Slug: p.Slug, Title: p.Title, Path: p.SourcePath, Packages: len(pkgs), Links: len(links)}, nil
}

// planBySlug returns a registered plan by slug (any case), or a 404.
func (s *Service) planBySlug(ctx context.Context, slug string) (store.Plan, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return store.Plan{}, badRequest("plan (the plan's slug) is required")
	}
	p, err := s.st.GetPlanBySlug(ctx, slug)
	if err != nil {
		return store.Plan{}, orNotFound(err, "no plan %q is registered", slug)
	}
	return p, nil
}

// packageOf returns a plan's package by ID (any case), or a 404.
func (s *Service) packageOf(ctx context.Context, p store.Plan, id string) (store.Package, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return store.Package{}, badRequest("pkg (the package ID) is required")
	}
	pkg, err := s.st.GetPackage(ctx, p.ID, id)
	if err != nil {
		return store.Package{}, orNotFound(err, "plan %s has no package %s", p.Slug, id)
	}
	return pkg, nil
}
