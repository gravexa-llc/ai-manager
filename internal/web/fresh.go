package web

// Fresh card facts. A board is the last state pass's, and a pass runs
// at most every 5 s after a poll or an event batch. The cards' PRs and
// agents are shown as the store has them now, so a "sessions" or "prs"
// change shows on its card as soon as the region refreshes. Which column
// a card is in, its badges and the header still come from the pass, which
// follows within seconds and sends "plans".

import (
	"cmp"
	"context"
	"slices"
	"strings"

	"github.com/gravexa-llc/ai-manager/internal/state"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// freshFacts are the store's rows a board's cards are refreshed from.
type freshFacts struct {
	prs     map[string]store.PR       // by lower-case PR ref
	byID    map[string]*store.Session // live rows by session id
	byJob   map[string]*store.Session // live rows by job id
	agents  map[[2]string]store.Agent // by session and agent id
	enabled bool
}

// freshFacts reads them: the snapshot's PRs and live sessions, and the
// agent rows.
func (s *Server) freshFacts(ctx context.Context, sn *snapshot) (freshFacts, error) {
	f := freshFacts{prs: map[string]store.PR{}, byID: map[string]*store.Session{}, byJob: map[string]*store.Session{},
		agents: map[[2]string]store.Agent{}, enabled: true}
	for _, p := range sn.prs {
		f.prs[strings.ToLower(store.PRRef(p.Repo, p.Number))] = p
	}
	for i := range sn.sessions {
		row := &sn.sessions[i]
		if row.SessionID != "" {
			f.byID[row.SessionID] = row
		}
		if row.JobID != "" {
			f.byJob[row.JobID] = row
		}
	}
	agents, err := s.opts.Store.ListAgents(ctx, "")
	if err != nil {
		return f, err
	}
	for _, a := range agents {
		f.agents[[2]string{a.SessionID, a.AgentID}] = a
	}
	return f, nil
}

// apply returns the board with its links' PRs and agents refreshed. The
// board itself (the plan service's) is left as it is.
func (f freshFacts) apply(b api.Plan) api.Plan {
	if !f.enabled {
		return b
	}
	pkgs := make([]api.Package, len(b.Packages))
	for i, p := range b.Packages {
		p.Links = slices.Clone(p.Links)
		for j := range p.Links {
			p.Links[j] = f.link(p.Links[j])
		}
		pkgs[i] = p
	}
	b.Packages = pkgs
	return b
}

func (f freshFacts) link(l api.Link) api.Link {
	switch l.Kind {
	case store.LinkPR:
		if p, ok := f.prs[strings.ToLower(l.Ref)]; ok {
			l.PR = &api.LinkPR{
				Repo: p.Repo, Number: p.Number, Title: p.Title, URL: p.URL, State: p.State, Unread: p.Unread(), Draft: p.Draft,
				HeadBranch: p.HeadBranch, ChecksState: p.ChecksState, Mergeable: p.Mergeable,
				MergeStateStatus: p.MergeStateStatus, ReviewDecision: p.ReviewDecision,
				MergedAt: p.MergedAt, ClosedAt: p.ClosedAt,
			}
		}
	case store.LinkAgent:
		if l.Agent != nil {
			a := f.agent(*l.Agent)
			l.Agent = &a
		}
	}
	return l
}

// agent refreshes a linked agent with the board's own rules
// (internal/state): a session or job from its live row, a subagent from
// its agent row and its session's.
func (f freshFacts) agent(a api.LinkAgent) api.LinkAgent {
	if a.AgentID == "" {
		row := f.byID[a.SessionID]
		if a.JobID != "" {
			row = f.byJob[a.JobID]
		}
		if row == nil {
			// No live row any more: the session ended since the pass.
			a.Live, a.NeedsYou = false, false
			return a
		}
		if row.SessionID != "" {
			a.SessionID = row.SessionID
		}
		a.Kind, a.Name, a.State = row.Kind, row.Name, row.State
		a.Live = state.SessionLive(row)
		a.NeedsYou = state.SessionNeedsYou(row)
		a.NeedsYouSince, a.Reason = row.NeedsYouSince, row.StateReason
		a.LastAction, a.LastEventAt, a.WorktreePath = row.LastAction, row.LastEventAt, row.WorktreePath
		a.StartedAt = row.StartedAt
		// The main loop's agent row fills in what the session row lacks.
		if lead, ok := f.agents[[2]string{row.SessionID, ""}]; ok {
			a.LastAction = cmp.Or(a.LastAction, lead.LastAction)
			a.WorktreePath = cmp.Or(a.WorktreePath, lead.WorktreePath)
			if lead.State == state.AgentWaiting && a.Live {
				a.NeedsYou, a.Reason = true, cmp.Or(a.Reason, "waiting")
			}
		}
		return a
	}
	row, ok := f.agents[[2]string{a.SessionID, a.AgentID}]
	if !ok {
		return a // known from its worktree's lock only
	}
	live := row.EndedAt.IsZero() && !state.AgentEnded(row.State)
	if lead := f.byID[a.SessionID]; lead != nil {
		live = live && state.SessionLive(lead)
	} else {
		live = live && a.Live
	}
	a.State, a.Live = row.State, live
	if row.LastAction != "" {
		a.LastAction = row.LastAction
	}
	// A subagent's name is its type (internal/state), which the first
	// agent_list brings.
	if row.AgentType != "" {
		a.Name = row.AgentType
	}
	if row.Tokens > 0 {
		a.Tokens = row.Tokens
	}
	if !row.StartedAt.IsZero() {
		a.StartedAt = row.StartedAt
	}
	a.NeedsYou = live && row.State == state.AgentWaiting
	if a.NeedsYou && a.Reason == "" {
		a.Reason = "waiting"
	}
	return a
}
