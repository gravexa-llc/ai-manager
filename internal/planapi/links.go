package planapi

import (
	"context"
	"errors"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/state"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// LinkSpawn links an agent the mod saw spawn to its package (POST
// /api/links/spawn): by the lead's session id and the new agent's id, or,
// for a remote agent, by its cloud session URL. The link is exact, made
// by spawn, and the matcher never changes it.
func (s *Service) LinkSpawn(ctx context.Context, req api.SpawnLinkRequest) (api.LinkResult, error) {
	sessionID, agentID, cloud := strings.TrimSpace(req.SessionID), strings.TrimSpace(req.AgentID), strings.TrimSpace(req.CloudURL)
	l := store.Link{MatchMethod: store.MatchSpawn, Confidence: store.ConfidenceExact}
	switch {
	case agentID != "" && cloud != "":
		return api.LinkResult{}, badRequest("give agent_id or cloud_url, not both")
	case agentID != "":
		if sessionID == "" {
			return api.LinkResult{}, badRequest("session_id is required with agent_id")
		}
		l.Kind, l.SessionID, l.AgentID = store.LinkAgent, sessionID, agentID
	case cloud != "":
		u, err := webURL(cloud)
		if err != nil {
			return api.LinkResult{}, badRequest("cloud_url: %v", err)
		}
		l.Kind, l.Ref = store.LinkCloudSession, u
	default:
		return api.LinkResult{}, badRequest("agent_id (with session_id) or cloud_url is required")
	}
	if req.Provisional {
		if err := s.seeSpawn(ctx, req, l); err != nil {
			return api.LinkResult{}, err
		}
	}
	return s.writeLink(ctx, req.Plan, req.Pkg, l)
}

// seeSpawn records a spawn whose brief's Package: line names a package
// the plan file doesn't have: a new provisional package, or
// another source of one, titled from the spawn's description. A package
// the file has is left alone, and an ID that isn't one is a 400.
func (s *Service) seeSpawn(ctx context.Context, req api.SpawnLinkRequest, l store.Link) error {
	p, err := s.planBySlug(ctx, req.Plan)
	if err != nil {
		return err
	}
	id := state.ProvisionalID(req.Pkg)
	if id == "" {
		return badRequest("pkg must be a package ID like W12: %q", req.Pkg)
	}
	switch pkg, err := s.st.GetPackage(ctx, p.ID, id); {
	case err == nil && !pkg.Provisional:
		return nil // the plan file has it
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return err
	}
	made, err := s.st.SeeProvisional(ctx, []store.Sighting{{PlanID: p.ID, PkgID: id, Kind: store.ProvisionalSpawn,
		Ref: refOf(l), Title: state.ProvisionalTitle(id, req.Title), At: s.now()}})
	if err != nil {
		return err
	}
	if made > 0 {
		s.log.Info("provisional package", "plan", p.Slug, "pkg", id, "from", store.ProvisionalSpawn, "ref", refOf(l))
	}
	return nil
}

// LinkManual links any target to a package by hand (POST
// /api/links/manual). It also clears a tombstone the same link had.
func (s *Service) LinkManual(ctx context.Context, req api.LinkRequest) (api.LinkResult, error) {
	l, err := s.parseTarget(ctx, req.Kind, req.Ref)
	if err != nil {
		return api.LinkResult{}, err
	}
	l.MatchMethod, l.Confidence = store.MatchManual, store.ConfidenceExact
	return s.writeLink(ctx, req.Plan, req.Pkg, l)
}

// writeLink stores l for the plan's package, runs a state pass, and
// answers with the link and the package.
func (s *Service) writeLink(ctx context.Context, slug, pkgID string, l store.Link) (api.LinkResult, error) {
	p, err := s.planBySlug(ctx, slug)
	if err != nil {
		return api.LinkResult{}, err
	}
	pkg, err := s.packageOf(ctx, p, pkgID)
	if err != nil {
		return api.LinkResult{}, err
	}
	l.PlanID, l.PkgID = p.ID, pkg.PkgID
	id, err := s.st.UpsertLink(ctx, l)
	if err != nil {
		return api.LinkResult{}, orNotFound(err, "plan %s has no package %s", p.Slug, pkgID)
	}
	s.log.Info("link", "plan", p.Slug, "pkg", pkg.PkgID, "kind", l.Kind, "ref", refOf(l), "method", l.MatchMethod)
	return s.linkResult(ctx, []linkedTo{{plan: p.Slug, pkg: pkg.PkgID, link: l, id: id}})
}

// linkedTo is a link written or removed, for the answer.
type linkedTo struct {
	plan, pkg string
	link      store.Link
	id        int64
}

// linkResult runs a state pass and answers with the links and their
// packages as derived now.
func (s *Service) linkResult(ctx context.Context, ls []linkedTo) (api.LinkResult, error) {
	if err := s.runState(ctx); err != nil {
		return api.LinkResult{}, err
	}
	out := api.LinkResult{Links: []api.LinkRef{}, Packages: []api.Package{}}
	seen := map[string]bool{}
	for _, l := range ls {
		out.Links = append(out.Links, api.LinkRef{
			Plan: l.plan, Pkg: l.pkg, Target: api.Target{Kind: l.link.Kind, Ref: refOf(l.link)},
			Method: l.link.MatchMethod, Confidence: l.link.Confidence, ID: l.id,
		})
		k := strings.ToLower(l.plan + " " + l.pkg)
		if seen[k] {
			continue
		}
		seen[k] = true
		b, err := s.board(ctx, l.plan)
		if err != nil {
			return api.LinkResult{}, err
		}
		for _, p := range b.Packages {
			if strings.EqualFold(p.ID, l.pkg) {
				out.Packages = append(out.Packages, p)
			}
		}
	}
	return out, nil
}

// refOf is a link's ref as the board shows it.
func refOf(l store.Link) string {
	if l.Kind == store.LinkAgent && l.Ref == "" {
		if l.SessionID != "" {
			return store.AgentRef(l.SessionID, l.AgentID)
		}
		return store.JobRef(l.JobID)
	}
	return l.Ref
}

// prURL matches a GitHub pull request URL, capturing owner, repo and
// number. Anything after the number (/files, #…) is allowed.
var prURL = regexp.MustCompile(`^https?://(?:www\.)?github\.com/([A-Za-z0-9][A-Za-z0-9-]{0,38})/([A-Za-z0-9._-]{1,100})/pull/([0-9]+)(?:[/?#].*)?$`)

// prRefRE matches "owner/repo#123".
var prRefRE = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100})#([0-9]+)$`)

// parsePR reads a PR from a URL or an "owner/repo#123" ref.
func parsePR(s string) (repo string, number int, ok bool) {
	s = strings.TrimSpace(s)
	if m := prURL.FindStringSubmatch(s); m != nil {
		n, err := strconv.Atoi(m[3])
		return m[1] + "/" + m[2], n, err == nil && n > 0
	}
	if m := prRefRE.FindStringSubmatch(s); m != nil {
		n, err := strconv.Atoi(m[2])
		return m[1], n, err == nil && n > 0
	}
	return "", 0, false
}

// LinkPR records the PR a linked agent just created (POST /api/links/pr),
// so it lands on the agent's package before the next GitHub poll: the PR
// is linked to every package the agent is linked to, with the method and
// confidence of that link. Until GitHub is read, the PR is stored as open
// with only its URL known (store.PR.Unread), and it is asked for at once
// (Options.Refresh), so the read comes within seconds.
func (s *Service) LinkPR(ctx context.Context, req api.PRLinkRequest) (api.LinkResult, error) {
	sessionID, agentID := strings.TrimSpace(req.SessionID), strings.TrimSpace(req.AgentID)
	if sessionID == "" {
		return api.LinkResult{}, badRequest("session_id is required")
	}
	repo, number, ok := parsePR(req.URL)
	if !ok {
		return api.LinkResult{}, badRequest("url must be a pull request URL like https://github.com/owner/repo/pull/123: %q", req.URL)
	}
	owners, err := s.agentPackages(ctx, sessionID, agentID)
	if err != nil {
		return api.LinkResult{}, err
	}
	if len(owners) == 0 {
		return api.LinkResult{}, notFound("agent %s isn't linked to a package; link it first (POST /api/links/spawn or /api/links/manual)", store.AgentRef(sessionID, agentID))
	}
	if pr, err := s.st.GetPR(ctx, repo, number); err == nil {
		repo = pr.Repo // as GitHub spells it
		if pr.Unread() {
			s.refreshNow(repo, number)
		}
	} else if errors.Is(err, store.ErrNotFound) {
		placeholder := store.PR{Repo: repo, Number: number, State: store.PROpen,
			URL: "https://github.com/" + repo + "/pull/" + strconv.Itoa(number)}
		if err := s.st.UpsertPRs(ctx, placeholder); err != nil {
			return api.LinkResult{}, err
		}
		s.refreshNow(repo, number) // read it now, not at the next poll
	} else {
		return api.LinkResult{}, err
	}
	var written []linkedTo
	for _, o := range owners {
		l := store.Link{PlanID: o.planID, PkgID: o.pkg, Kind: store.LinkPR, Ref: store.PRRef(repo, number),
			MatchMethod: o.method, Confidence: o.confidence}
		id, err := s.st.UpsertLink(ctx, l)
		if err != nil {
			return api.LinkResult{}, err
		}
		s.log.Info("link", "plan", o.slug, "pkg", o.pkg, "kind", l.Kind, "ref", l.Ref, "method", l.MatchMethod)
		written = append(written, linkedTo{plan: o.slug, pkg: o.pkg, link: l, id: id})
	}
	return s.linkResult(ctx, written)
}

// agentOwner is a package an agent is linked to, and how.
type agentOwner struct {
	planID             int64
	slug, pkg          string
	method, confidence string
}

// agentPackages returns the packages an agent (session id and agent id;
// "" for the main loop) is linked to: by a stored spawn or manual link,
// or, failing that, on the last board (an agent working in a linked
// worktree).
func (s *Service) agentPackages(ctx context.Context, sessionID, agentID string) ([]agentOwner, error) {
	ref := store.AgentRef(sessionID, agentID)
	ps, err := s.st.ListPlans(ctx, store.PlanFilter{})
	if err != nil {
		return nil, err
	}
	var out []agentOwner
	for _, p := range ps {
		links, err := s.st.ListLinks(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		for _, l := range links {
			if l.Kind == store.LinkAgent && !l.Inferred && refOf(l) == ref {
				out = append(out, agentOwner{planID: p.ID, slug: p.Slug, pkg: l.PkgID, method: l.MatchMethod, confidence: l.Confidence})
			}
		}
	}
	if len(out) > 0 {
		return out, nil
	}
	boards, err := s.boardsNow(ctx)
	if err != nil {
		return nil, err
	}
	ids := map[string]int64{}
	for _, p := range ps {
		ids[strings.ToLower(p.Slug)] = p.ID
	}
	for _, b := range boards {
		for _, pkg := range b.Packages {
			for _, l := range pkg.Links {
				if l.Kind == store.LinkAgent && l.Ref == ref {
					out = append(out, agentOwner{planID: ids[strings.ToLower(b.Slug)], slug: b.Slug, pkg: pkg.ID, method: l.Method, confidence: l.Confidence})
					break
				}
			}
		}
	}
	return out, nil
}

// Unlink removes a link from a package and remembers that you did (POST
// /api/links/manual's opposite, DELETE /api/links): the matcher never
// makes it again, and nothing is reached through it. The link needn't be
// stored: a link the board shows through another one is unlinked the same
// way. Unlinking twice is fine.
func (s *Service) Unlink(ctx context.Context, req api.LinkRequest) (api.LinkResult, error) {
	l, err := s.parseTarget(ctx, req.Kind, req.Ref)
	if err != nil {
		return api.LinkResult{}, err
	}
	p, err := s.planBySlug(ctx, req.Plan)
	if err != nil {
		return api.LinkResult{}, err
	}
	pkg, err := s.packageOf(ctx, p, req.Pkg)
	if err != nil {
		return api.LinkResult{}, err
	}
	return s.unlink(ctx, p, pkg.PkgID, l)
}

// UnlinkID unlinks a stored link by its id (DELETE /api/links/{id}).
func (s *Service) UnlinkID(ctx context.Context, id int64) (api.LinkResult, error) {
	ps, err := s.st.ListPlans(ctx, store.PlanFilter{})
	if err != nil {
		return api.LinkResult{}, err
	}
	for _, p := range ps {
		links, err := s.st.ListLinks(ctx, p.ID)
		if err != nil {
			return api.LinkResult{}, err
		}
		for _, l := range links {
			if l.ID == id {
				return s.unlink(ctx, p, l.PkgID, l)
			}
		}
	}
	return api.LinkResult{}, notFound("no link %d", id)
}

func (s *Service) unlink(ctx context.Context, p store.Plan, pkgID string, l store.Link) (api.LinkResult, error) {
	t := store.Tombstone{PlanID: p.ID, PkgID: pkgID, Kind: l.Kind, SessionID: l.SessionID, JobID: l.JobID, AgentID: l.AgentID, Ref: l.Ref}
	if err := s.st.Unlink(ctx, t); err != nil {
		return api.LinkResult{}, orNotFound(err, "plan %s has no package %s", p.Slug, pkgID)
	}
	s.log.Info("unlink", "plan", p.Slug, "pkg", pkgID, "kind", l.Kind, "ref", refOf(l))
	l.MatchMethod, l.Confidence = "", ""
	return s.linkResult(ctx, []linkedTo{{plan: p.Slug, pkg: pkgID, link: l}})
}

// parseTarget reads a link target from a kind and a ref (api.Target's
// formats; a PR may be a URL, an agent a bare session id for its main
// loop). A PR's repo takes GitHub's spelling when aimd has the PR, and an
// existing worktree's path is resolved.
func (s *Service) parseTarget(ctx context.Context, kind, ref string) (store.Link, error) {
	kind, ref = strings.TrimSpace(kind), strings.TrimSpace(ref)
	if ref == "" {
		return store.Link{}, badRequest("ref is required")
	}
	l := store.Link{Kind: kind}
	switch kind {
	case store.LinkPR:
		repo, n, ok := parsePR(ref)
		if !ok {
			return store.Link{}, badRequest("a pr ref is owner/repo#123 or a pull request URL: %q", ref)
		}
		if pr, err := s.st.GetPR(ctx, repo, n); err == nil {
			repo = pr.Repo
		}
		l.Ref = store.PRRef(repo, n)
	case store.LinkBranch:
		repo, branch, ok := strings.Cut(ref, ":")
		if !ok || repo == "" || branch == "" {
			return store.Link{}, badRequest("a branch ref is owner/repo:branch: %q", ref)
		}
		if !filepath.IsAbs(repo) && !githubName.MatchString(repo) {
			return store.Link{}, badRequest("a branch ref's repo is owner/repo or a main checkout's path: %q", ref)
		}
		l.Ref = ref
	case store.LinkWorktree:
		if !filepath.IsAbs(ref) {
			return store.Link{}, badRequest("a worktree ref is an absolute path: %q", ref)
		}
		p := filepath.Clean(ref)
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		}
		l.Ref = p
	case store.LinkAgent:
		switch {
		case strings.HasPrefix(ref, "job:"):
			if l.JobID = strings.TrimPrefix(ref, "job:"); l.JobID == "" {
				return store.Link{}, badRequest("an agent ref is session_id/agent_id or job:<id>: %q", ref)
			}
		default:
			l.SessionID, l.AgentID, _ = strings.Cut(ref, "/")
			if l.SessionID == "" {
				return store.Link{}, badRequest("an agent ref is session_id/agent_id or job:<id>: %q", ref)
			}
		}
	case store.LinkCloudSession:
		u, err := webURL(ref)
		if err != nil {
			return store.Link{}, badRequest("a cloud_session ref is the session URL: %v", err)
		}
		l.Ref = u
	case "":
		return store.Link{}, badRequest("kind is required: agent, branch, pr, worktree or cloud_session")
	default:
		return store.Link{}, badRequest("unknown kind %q: want agent, branch, pr, worktree or cloud_session", kind)
	}
	return l, nil
}

// webURL checks that s is an absolute http(s) URL.
func webURL(s string) (string, error) {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", errors.New("want an http(s) URL, got " + strconv.Quote(s))
	}
	return u.String(), nil
}

// SetDone sets or clears a package's Mark done (POST and DELETE
// /api/plans/{slug}/packages/{id}/done), runs a state pass, and answers
// with the package and the plan's summary.
func (s *Service) SetDone(ctx context.Context, slug, id string, done bool) (api.DoneResult, error) {
	p, err := s.planBySlug(ctx, slug)
	if err != nil {
		return api.DoneResult{}, err
	}
	pkg, err := s.packageOf(ctx, p, id)
	if err != nil {
		return api.DoneResult{}, err
	}
	if err := s.st.SetDoneManual(ctx, p.ID, pkg.PkgID, done); err != nil {
		return api.DoneResult{}, orNotFound(err, "plan %s has no package %s", p.Slug, id)
	}
	s.log.Info("mark done", "plan", p.Slug, "pkg", pkg.PkgID, "done", done)
	if err := s.runState(ctx); err != nil {
		return api.DoneResult{}, err
	}
	b, err := s.board(ctx, p.Slug)
	if err != nil {
		return api.DoneResult{}, err
	}
	sum, err := s.summaryNow(ctx, b, time.Time{})
	if err != nil {
		return api.DoneResult{}, err
	}
	out := api.DoneResult{Summary: sum}
	for _, jp := range b.Packages {
		if strings.EqualFold(jp.ID, pkg.PkgID) {
			out.Package = jp
		}
	}
	return out, nil
}
