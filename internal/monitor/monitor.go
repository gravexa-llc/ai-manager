// Package monitor decides which sessions aimd monitors: the
// ones the Needs you inbox, the desktop notifications (internal/notify)
// and the dashboard's session lists follow. Every other session is "not
// monitored": the dashboard lists it only in a collapsed group, and it
// never shows in Needs you or raises a notification.
//
// # Rules
//
// With config sessions.monitor = "opt-in" (the default), a session is
// monitored when any of these holds:
//
//  1. Plan lead (ReasonPlan): it registered a plan, with POST /api/plans
//     and its session_id (the band's Register button, /plan-register).
//     Stored (store.MonitorPlanLead) until the plan is unregistered.
//  2. Watched (ReasonWatch): you asked to watch it, with /aim watch or the
//     dashboard's Watch button. Stored (store.WatchSession) until /aim
//     unwatch or Unwatch.
//  3. Plan agent (ReasonAgent), worked out on every call and never stored:
//     a. a registered plan's board links it to a package as an agent: a
//     spawn or manual link, or one the matcher reached through the
//     package's worktree, branch or PR (internal/state). A link to one of
//     its subagents counts for the session too, since a subagent runs
//     inside its lead and its needs-you shows on the lead's row; or
//     b. aimd's lineage ties it to a monitored lead: it works in a linked
//     worktree that Claude Code locked for an agent of that lead's
//     process (the lock reason names the lead's pid), or that the scanner
//     records as that session's.
//
// Stored rows name a session by its Claude Code session id. A session row
// matches by its id or by the one it had before its last /clear
// (store.Session.PrevSessionID); the store also moves the rows to the new
// id when it sees the change (store.MoveSessionID, UpsertSessions), so a
// watch survives any number of /clears.
//
// With sessions.monitor = "all" every session is monitored; one no rule
// covers gets ReasonAll.
//
// # PRs
//
// A PR counts for Needs you when a registered plan's board links it to a
// package, or when it is a same-repo PR whose head branch is the branch a
// monitored session works on (its worktree's) in that repo. In "all" mode
// every PR counts. Lists of PRs (the Pull requests page) still show them
// all.
//
// A PR that counts needs you only when it waits on you: your review is
// requested, or it is ready to merge and its plan says you merge
// (internal/prwait has the rule; Set.PRWait gives it the plan, the plan's
// Merges line and whether an agent still works on the PR). A review request
// is the exception: it needs you on any open PR, counted or not, since
// someone asked you by name.
package monitor

import (
	"cmp"
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/prwait"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
	"github.com/gravexa-llc/ai-manager/internal/worktrees"
)

// Modes: config sessions.monitor.
const (
	ModeOptIn = "opt-in"
	ModeAll   = "all"
)

// Why a session is monitored.
const (
	ReasonPlan  = "plan"
	ReasonWatch = "watch"
	ReasonAgent = "agent"
	ReasonAll   = "all"
)

// Why is one reason a session is monitored.
type Why struct {
	// Reason is ReasonPlan, ReasonWatch, ReasonAgent or ReasonAll.
	Reason string
	// Plan is a plan's slug: the plan it registered (ReasonPlan), the
	// package's plan, or the lead's plan (ReasonAgent).
	Plan string
	// Pkg is the package (ReasonAgent through a plan link).
	Pkg string
	// Subagent is set when the link names one of the session's subagents
	// rather than the session itself (ReasonAgent through a plan link);
	// Agent is that subagent's id, when the link says it.
	Subagent bool
	Agent    string
	// Lead and LeadTitle name the monitored lead whose agent it is
	// (ReasonAgent through lineage): its session key and title.
	Lead, LeadTitle string
	// Since is when it started (ReasonPlan, ReasonWatch).
	Since time.Time
}

// Text says it in a few words: "registered plan sharing", "you asked to
// watch it" (with /aim watch or the dashboard's Watch button), "agent of
// T1".
func (w Why) Text() string {
	switch w.Reason {
	case ReasonPlan:
		return "registered plan " + w.Plan
	case ReasonWatch:
		return "you asked to watch it"
	case ReasonAgent:
		switch {
		case w.Lead != "":
			return "agent of " + cmp.Or(w.LeadTitle, w.Lead)
		case w.Subagent:
			return "runs the agent of " + w.Pkg
		}
		return "agent of " + w.Pkg
	case ReasonAll:
		return `sessions.monitor is "all"`
	}
	return w.Reason
}

// Summary says why a session is monitored in one short line, its reasons
// joined with " · ". Agent reasons of one plan are put together ("agent of
// T1, T2"), and plan slugs are added to them only when the reasons name
// more than one plan.
func Summary(ws []Why) string {
	plans := map[string]bool{}
	for _, w := range ws {
		if w.Plan != "" {
			plans[strings.ToLower(w.Plan)] = true
		}
	}
	many := len(plans) > 1
	// Agent reasons through a plan link go together by plan and by whether
	// the link names the session or one of its subagents.
	type groupKey struct {
		plan     string
		subagent bool
	}
	type group struct {
		at   int // its place in parts
		sub  bool
		pkgs []string
	}
	var parts []string
	var groups []*group
	byKey := map[groupKey]*group{}
	for _, w := range ws {
		if w.Reason != ReasonAgent || w.Lead != "" || w.Pkg == "" {
			parts = append(parts, w.Text())
			continue
		}
		k := groupKey{strings.ToLower(w.Plan), w.Subagent}
		g := byKey[k]
		if g == nil {
			g = &group{at: len(parts), sub: w.Subagent}
			byKey[k] = g
			groups = append(groups, g)
			parts = append(parts, "") // filled in below
		}
		pkg := w.Pkg
		if many && w.Plan != "" {
			pkg = w.Plan + " · " + w.Pkg
		}
		if !slices.Contains(g.pkgs, pkg) {
			g.pkgs = append(g.pkgs, pkg)
		}
	}
	for _, g := range groups {
		list := strings.Join(g.pkgs, ", ")
		switch {
		case g.sub && len(g.pkgs) > 1:
			parts[g.at] = "runs the agents of " + list
		case g.sub:
			parts[g.at] = "runs the agent of " + list
		default:
			parts[g.at] = "agent of " + list
		}
	}
	return strings.Join(parts, " · ")
}

// PlanOf is the plan a session's reasons put it under (the dashboard
// groups monitored sessions by it): the first reason that names a plan,
// or "" for none.
func PlanOf(ws []Why) string {
	for _, w := range ws {
		if w.Plan != "" {
			return w.Plan
		}
	}
	return ""
}

// Has reports whether ws has a reason of the kind.
func Has(ws []Why, reason string) bool {
	return slices.ContainsFunc(ws, func(w Why) bool { return w.Reason == reason })
}

// Set is what Compute decided for a snapshot of sessions. A nil *Set
// monitors everything: it is what callers use when no monitoring is wired
// or its inputs couldn't be read, so nothing that needs you is hidden.
type Set struct {
	mode string
	why  map[string][]Why // by session key
	// prs are the PRs a board links, as lower-case "owner/repo#123";
	// branches the monitored sessions' branches, lower-case
	// "owner/repo" + "\x00" + branch.
	prs      map[string]bool
	branches map[string]bool

	// What PRWait needs (internal/prwait). on maps a PR a board links to the
	// first plan and package that link it; branchPlan maps a monitored
	// session's branch (as in branches) to its plan; merges maps a plan's
	// lower-case slug to its Merges value; pkgWorking holds the packages
	// (pkgKey) a live agent works on, and branchWorking the branches (as
	// in branches) a live session works on, monitored or not.
	on            map[string]planPkg
	branchPlan    map[string]string
	merges        map[string]string
	pkgWorking    map[string]bool
	branchWorking map[string]bool
}

// planPkg is a plan's slug and one of its packages.
type planPkg struct{ plan, pkg string }

// pkgKey keys a package: lower-case slug, NUL, upper-case ID.
func pkgKey(plan, pkg string) string { return strings.ToLower(plan) + "\x00" + strings.ToUpper(pkg) }

// branchKey keys a same-repo PR's or a session's branch: lower-case
// nameWithOwner, NUL, branch. "" when there is no branch.
func branchKey(repo, branch string) string {
	if repo == "" || branch == "" {
		return ""
	}
	return strings.ToLower(repo) + "\x00" + branch
}

// Mode is the set's mode, ModeOptIn or ModeAll (ModeAll for nil).
func (s *Set) Mode() string {
	if s == nil {
		return ModeAll
	}
	return s.mode
}

// All reports whether every session counts as monitored: "all" mode, or
// a nil set.
func (s *Set) All() bool { return s == nil || s.mode == ModeAll }

// Why lists why a session row is monitored, most explicit first: plan
// leads (in the order they registered), the watch, then agent reasons. It
// is nil for a session that isn't monitored, and for every session of a
// nil set.
func (s *Set) Why(row store.Session) []Why {
	if s == nil {
		return nil
	}
	return s.why[row.Key]
}

// Monitored reports whether a session row is monitored.
func (s *Set) Monitored(row store.Session) bool {
	return s.All() || len(s.why[row.Key]) > 0
}

// PR reports whether a PR counts for Needs you (see the package comment).
func (s *Set) PR(p store.PR) bool {
	if s.All() {
		return true
	}
	if s.prs[strings.ToLower(store.PRRef(p.Repo, p.Number))] {
		return true
	}
	return !p.IsCrossRepository && p.HeadBranch != "" && s.branches[branchKey(p.Repo, p.HeadBranch)]
}

// PRWait decides what a PR waits on (internal/prwait): whether it needs
// you, and if not, who has the next move. The set supplies the rule's
// inputs: whether the PR counts (PR), the plan it is on (the first board
// that links it, else the plan of the monitored session on its branch),
// that plan's Merges line, and whether a live agent works on its package
// or a live session on its branch. A nil set counts every PR, on no plan,
// so a ready PR needs you: nothing that might need you is hidden.
func (s *Set) PRWait(p store.PR) prwait.Verdict {
	in := prwait.Input{PR: p, Monitored: s.PR(p)}
	if s != nil {
		bk := ""
		if !p.IsCrossRepository {
			bk = branchKey(p.Repo, p.HeadBranch)
		}
		if on, ok := s.on[strings.ToLower(store.PRRef(p.Repo, p.Number))]; ok {
			in.Plan, in.Pkg = on.plan, on.pkg
		} else if bk != "" {
			in.Plan = s.branchPlan[bk]
		}
		if in.Plan != "" {
			in.Merges = s.merges[strings.ToLower(in.Plan)]
		}
		in.AgentWorking = (in.Pkg != "" && s.pkgWorking[pkgKey(in.Plan, in.Pkg)]) || (bk != "" && s.branchWorking[bk])
	}
	return prwait.Decide(in)
}

// Session and agent states (internal/sessions, and the subagent states of
// $.agent.list()) that mean it has stopped working for now: its turn is
// done, it is idle, it failed or it is gone.
var notWorking = map[string]bool{"turn_done": true, "idle": true, "error": true, "gone": true}

// sessionWorking reports whether a session row still works: it hasn't
// ended, isn't a headless run, and is working, quiet or waiting on you
// mid-turn (a background job waiting for a reply included).
func sessionWorking(row store.Session) bool {
	if !row.EndedAt.IsZero() || row.Headless {
		return false
	}
	switch row.State {
	case "working", "quiet", "needs_you":
		return true
	}
	return false
}

// agentWorking reports whether a board's agent still works on its
// package: it is live and its turn isn't done.
func agentWorking(a *api.LinkAgent) bool {
	return a != nil && a.Live && !notWorking[a.State]
}

// AgentWorking is agentWorking, for pages that show who works on a
// package by the same rule PRWait uses (the plan page's "working" badge).
func AgentWorking(a *api.LinkAgent) bool { return agentWorking(a) }

// Input is what Compute decides from.
type Input struct {
	// Mode is ModeOptIn or ModeAll; anything else is ModeOptIn.
	Mode string
	// Sessions are the session rows to decide about (the live ones).
	Sessions []store.Session
	// Explicit is store.ListMonitored: the plan leads and the watches.
	Explicit []store.MonitoredSession
	// Plans are the registered plans' boards, with their links.
	Plans []api.Plan
	// Worktrees are the live worktrees, for the lineage rule.
	Worktrees []store.Worktree
	// RepoNames maps a repo's main checkout path (store.Session.Repo) to
	// its GitHub nameWithOwner, for the PR rule.
	RepoNames map[string]string
}

// Compute decides which of in.Sessions are monitored, and which PRs.
func Compute(in Input) *Set {
	s := &Set{mode: ModeOptIn, why: map[string][]Why{}, prs: map[string]bool{}, branches: map[string]bool{},
		on: map[string]planPkg{}, branchPlan: map[string]string{}, merges: map[string]string{},
		pkgWorking: map[string]bool{}, branchWorking: map[string]bool{}}
	if in.Mode == ModeAll {
		s.mode = ModeAll
	}

	// Rules 1 and 2: the stored rows, by session id.
	bySID := map[string][]Why{}
	for _, m := range in.Explicit {
		switch m.Reason {
		case store.MonitorPlan:
			bySID[m.SessionID] = append(bySID[m.SessionID], Why{Reason: ReasonPlan, Plan: m.PlanSlug, Since: m.Since})
		case store.MonitorWatch:
			bySID[m.SessionID] = append(bySID[m.SessionID], Why{Reason: ReasonWatch, Since: m.Since})
		}
	}
	// Rule 3a: the boards' agent links, by session id or background job;
	// and the PRs they link.
	agentsBySID := map[string][]Why{}
	agentsByJob := map[string][]Why{}
	for _, b := range in.Plans {
		if k := strings.ToLower(b.Slug); s.merges[k] == "" {
			s.merges[k] = b.Merges
		}
		for _, p := range b.Packages {
			for _, l := range p.Links {
				if agentWorking(l.Agent) {
					s.pkgWorking[pkgKey(b.Slug, p.ID)] = true
				}
				switch l.Kind {
				case store.LinkPR:
					ref := strings.ToLower(l.Ref)
					s.prs[ref] = true
					if _, ok := s.on[ref]; !ok {
						s.on[ref] = planPkg{b.Slug, p.ID}
					}
				case store.LinkAgent:
					sid, job, agent := agentOf(l)
					w := Why{Reason: ReasonAgent, Plan: b.Slug, Pkg: p.ID, Subagent: agent != "", Agent: agent}
					switch {
					case job != "":
						agentsByJob[job] = addWhy(agentsByJob[job], w)
					case sid != "":
						agentsBySID[sid] = addWhy(agentsBySID[sid], w)
					}
				}
			}
		}
	}
	for _, row := range in.Sessions {
		var ws []Why
		for _, id := range ids(row) {
			for _, w := range bySID[id] {
				ws = addWhy(ws, w)
			}
		}
		// Plan leads first (in the order they registered), then the watch.
		slices.SortStableFunc(ws, func(a, b Why) int { return boolCmp(a.Reason == ReasonWatch, b.Reason == ReasonWatch) })
		var agents []Why
		for _, id := range ids(row) {
			for _, w := range agentsBySID[id] {
				agents = addWhy(agents, w)
			}
		}
		if row.JobID != "" {
			for _, w := range agentsByJob[row.JobID] {
				agents = addWhy(agents, w)
			}
		}
		slices.SortStableFunc(agents, func(a, b Why) int {
			return cmp.Or(cmp.Compare(strings.ToLower(a.Plan), strings.ToLower(b.Plan)), boolCmp(a.Subagent, b.Subagent))
		})
		ws = append(ws, agents...)
		if len(ws) > 0 {
			s.why[row.Key] = ws
		}
	}
	// Rule 3b: lineage from a monitored lead.
	s.lineage(in)

	// The monitored sessions' branches, for the PR rule.
	names := map[string]string{}
	for path, name := range in.RepoNames {
		if name != "" {
			names[worktrees.PathKey(path)] = name
		}
	}
	for _, row := range in.Sessions {
		if row.Repo == "" || row.Branch == "" {
			continue
		}
		bk := branchKey(names[worktrees.PathKey(row.Repo)], row.Branch)
		if bk == "" {
			continue
		}
		if sessionWorking(row) {
			s.branchWorking[bk] = true
		}
		if ws := s.why[row.Key]; len(ws) > 0 {
			s.branches[bk] = true
			if s.branchPlan[bk] == "" {
				s.branchPlan[bk] = PlanOf(ws)
			}
		}
	}

	if s.mode == ModeAll {
		for _, row := range in.Sessions {
			if len(s.why[row.Key]) == 0 {
				s.why[row.Key] = []Why{{Reason: ReasonAll}}
			}
		}
	}
	return s
}

// ids are the session ids a row answers to: its own, and the one before
// its last /clear.
func ids(row store.Session) []string {
	var out []string
	for _, id := range []string{row.SessionID, row.PrevSessionID} {
		if id != "" && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// agentOf reads an agent link's session id, job id and agent id: from the
// agent aimd knows, or from its ref ("<session id>/<agent id>",
// "job:<id>").
func agentOf(l api.Link) (sessionID, jobID, agentID string) {
	if a := l.Agent; a != nil && (a.SessionID != "" || a.JobID != "") {
		return a.SessionID, a.JobID, a.AgentID
	}
	if job, ok := strings.CutPrefix(l.Ref, "job:"); ok {
		return "", job, ""
	}
	sid, agent, _ := strings.Cut(l.Ref, "/")
	return sid, "", agent
}

// addWhy appends w unless ws has it already.
func addWhy(ws []Why, w Why) []Why {
	if slices.ContainsFunc(ws, func(o Why) bool {
		return o.Reason == w.Reason && strings.EqualFold(o.Plan, w.Plan) && strings.EqualFold(o.Pkg, w.Pkg) &&
			o.Subagent == w.Subagent && o.Agent == w.Agent && o.Lead == w.Lead
	}) {
		return ws
	}
	return append(ws, w)
}

func boolCmp(a, b bool) int {
	switch {
	case a == b:
		return 0
	case !a:
		return -1
	}
	return 1
}

// lineage applies rule 3b until nothing changes: a session that isn't
// monitored, in a worktree (not a main checkout) that Claude Code locked
// for an agent of a monitored session's process, or that the scanner
// records as a monitored session's, is that session's agent.
func (s *Set) lineage(in Input) {
	wts := map[string]*store.Worktree{}
	for i := range in.Worktrees {
		wt := &in.Worktrees[i]
		if wt.RemovedAt.IsZero() && !worktrees.SamePath(wt.Path, wt.MainRepo) {
			wts[worktrees.PathKey(wt.Path)] = wt
		}
	}
	if len(wts) == 0 {
		return
	}
	// worktreeOf is the worktree a path is in: the deepest one that is it
	// or holds it.
	worktreeOf := func(path string) *store.Worktree {
		if path == "" {
			return nil
		}
		p := worktrees.PathKey(path)
		for {
			if wt := wts[p]; wt != nil {
				return wt
			}
			parent := filepath.Dir(p)
			if parent == p {
				return nil
			}
			p = parent
		}
	}
	for range in.Sessions { // each pass adds at least one session, or stops
		byPID := map[int]store.Session{}
		bySID := map[string]store.Session{}
		for _, r := range in.Sessions {
			if len(s.why[r.Key]) == 0 {
				continue
			}
			if r.PID > 0 {
				byPID[r.PID] = r
			}
			for _, id := range ids(r) {
				bySID[id] = r
			}
		}
		added := false
		for _, r := range in.Sessions {
			if len(s.why[r.Key]) > 0 {
				continue
			}
			wt := worktreeOf(r.WorktreePath)
			if wt == nil {
				wt = worktreeOf(r.Cwd)
			}
			if wt == nil {
				continue
			}
			var lead store.Session
			ok := false
			if wt.LockPID > 0 {
				lead, ok = byPID[wt.LockPID]
			}
			if !ok && wt.SessionID != "" {
				lead, ok = bySID[wt.SessionID]
			}
			if !ok || lead.Key == r.Key {
				continue
			}
			s.why[r.Key] = []Why{{Reason: ReasonAgent, Plan: PlanOf(s.why[lead.Key]), Lead: lead.Key, LeadTitle: Title(lead)}}
			added = true
		}
		if !added {
			return
		}
	}
}

// Title names a session as the dashboard does: its name, else its cwd's
// last folder, else its key.
func Title(row store.Session) string {
	switch {
	case row.Name != "":
		return row.Name
	case row.Cwd != "":
		return filepath.Base(row.Cwd)
	}
	return row.Key
}

// Store is the part of *store.Store a Source reads.
type Store interface {
	ListMonitored(ctx context.Context) ([]store.MonitoredSession, error)
	ListWorktrees(ctx context.Context, f store.WorktreeFilter) ([]store.Worktree, error)
}

var _ Store = (*store.Store)(nil)

// Source reads what Compute needs from aimd: the stored monitoring, the
// plan boards and the repo names. aimd makes one and shares it between the
// dashboard and the notifier.
type Source struct {
	// Mode is config sessions.monitor.
	Mode  string
	Store Store
	// Plans returns the registered plans' boards (the plan service's last
	// state pass). Nil means no plans.
	Plans func(ctx context.Context) ([]api.Plan, error)
	// RepoNames maps main checkout paths to GitHub names (the daemon's).
	// Nil means none, and the PR rule then goes by plan links only.
	RepoNames func() map[string]string
	// Merges returns the Settings page's overrides of who merges a plan's
	// PRs (store.PlanMerges), by lower-case slug: "you" or
	// "lead". Each replaces its board's Merges line before Compute reads
	// it, so a lead-merged plan's ready PR waits on the lead, not on you.
	// Nil means none.
	Merges func(ctx context.Context) (map[string]string, error)
}

// withMerges returns boards with the overrides of who merges in place of
// their Merges lines. The boards may be the plan service's own, so it
// changes a copy.
func withMerges(boards []api.Plan, over map[string]string) []api.Plan {
	if len(over) == 0 {
		return boards
	}
	out := slices.Clone(boards)
	for i := range out {
		if m := over[strings.ToLower(out[i].Slug)]; m != "" {
			out[i].Merges = m
		}
	}
	return out
}

// Load decides about the session rows. wts are the live worktrees, or nil
// to read them from the store. A nil Source returns a nil Set (everything
// monitored). If an input can't be read, Load returns a nil Set and the
// error, so a caller that goes on anyway hides nothing.
func (src *Source) Load(ctx context.Context, sessions []store.Session, wts []store.Worktree) (*Set, error) {
	if src == nil {
		return nil, nil
	}
	explicit, err := src.Store.ListMonitored(ctx)
	if err != nil {
		return nil, fmt.Errorf("monitor: list monitored sessions: %w", err)
	}
	if wts == nil {
		if wts, err = src.Store.ListWorktrees(ctx, store.WorktreeFilter{}); err != nil {
			return nil, fmt.Errorf("monitor: list worktrees: %w", err)
		}
	}
	var boards []api.Plan
	if src.Plans != nil {
		if boards, err = src.Plans(ctx); err != nil {
			return nil, fmt.Errorf("monitor: read plans: %w", err)
		}
	}
	if src.Merges != nil {
		over, err := src.Merges(ctx)
		if err != nil {
			return nil, fmt.Errorf("monitor: read who merges: %w", err)
		}
		boards = withMerges(boards, over)
	}
	var names map[string]string
	if src.RepoNames != nil {
		names = src.RepoNames()
	}
	return Compute(Input{Mode: src.Mode, Sessions: sessions, Explicit: explicit, Plans: boards, Worktrees: wts, RepoNames: names}), nil
}
