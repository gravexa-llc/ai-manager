package planapi

import (
	"cmp"
	"context"
	"slices"
	"strings"

	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// timelineSkip are the event kinds the timeline leaves out: a turn's start
// says nothing its tool calls and answer don't.
var timelineSkip = []string{"turn_start"}

// agentLoops is an agent linked to a plan's packages, with the loops whose
// events are its own.
type agentLoops struct {
	api.TimelineAgent
	loops []store.EventLoop
}

// Timeline returns a plan's timeline, newest first: its packages'
// lifecycle changes, the links stored for them, their PRs opened, merged
// and closed, the events of the agents linked to them, and the plan's
// stalled and lane-collision alerts. q narrows it to a package or an
// agent, and caps it. An unknown plan, package or agent is a 404.
//
// The agents are the ones the last state pass linked to a package
// (directly or through a worktree). A session's main loop (or a background
// job) brings its own events, not its subagents'; a subagent brings its
// events. Events recorded under a session's id before a /clear count too.
func (s *Service) Timeline(ctx context.Context, slug string, q api.TimelineQuery) (api.Timeline, error) {
	b, err := s.board(ctx, slug)
	if err != nil {
		return api.Timeline{}, err
	}
	p, err := s.planBySlug(ctx, b.Slug)
	if err != nil {
		return api.Timeline{}, err
	}
	q.Pkg, q.Agent = strings.TrimSpace(q.Pkg), strings.TrimSpace(q.Agent)
	switch {
	case q.Limit <= 0:
		q.Limit = api.DefaultTimelineLimit
	case q.Limit > api.MaxTimelineLimit:
		q.Limit = api.MaxTimelineLimit
	}
	if q.Pkg != "" {
		found := false
		for _, pkg := range b.Packages {
			if strings.EqualFold(pkg.ID, q.Pkg) {
				q.Pkg, found = pkg.ID, true
			}
		}
		if !found {
			return api.Timeline{}, notFound("plan %s has no package %s", b.Slug, q.Pkg)
		}
	}

	// What the board links: agents (with the loops whose events are
	// theirs) and PRs, each with its packages.
	sessions, err := s.st.ListSessions(ctx, store.SessionFilter{IncludeEnded: true})
	if err != nil {
		return api.Timeline{}, err
	}
	var agents []*agentLoops
	byRef := map[string]*agentLoops{}
	prPkgs := map[string][]string{}
	var prOrder []string
	for _, pkg := range b.Packages {
		for _, l := range pkg.Links {
			switch l.Kind {
			case store.LinkAgent:
				a := byRef[l.Ref]
				if a == nil {
					a = &agentLoops{TimelineAgent: api.TimelineAgent{Ref: l.Ref, Agent: l.Agent}, loops: loopsOf(l.Ref, sessions)}
					byRef[l.Ref] = a
					agents = append(agents, a)
				}
				a.Packages = addID(a.Packages, pkg.ID)
			case store.LinkPR:
				if _, seen := prPkgs[l.Ref]; !seen {
					prOrder = append(prOrder, l.Ref)
				}
				prPkgs[l.Ref] = addID(prPkgs[l.Ref], pkg.ID)
			}
		}
	}
	if q.Agent != "" && byRef[q.Agent] == nil {
		return api.Timeline{}, notFound("no agent %s is linked to plan %s", q.Agent, b.Slug)
	}

	// Each source gives at most limit+1 entries: the newest limit of all
	// of them are among those, and an extra one means there are more.
	n := q.Limit + 1
	var out []api.TimelineEntry
	add := func(e api.TimelineEntry) {
		if q.Pkg == "" || slices.ContainsFunc(e.Packages, func(id string) bool { return strings.EqualFold(id, q.Pkg) }) {
			out = append(out, e)
		}
	}

	if q.Agent == "" {
		hist, err := s.st.ListLifecycleHistory(ctx, p.ID, store.HistoryFilter{PkgID: q.Pkg, Limit: n})
		if err != nil {
			return api.Timeline{}, err
		}
		for _, h := range hist {
			add(api.TimelineEntry{At: h.At, Kind: api.TimelineLifecycle, ID: h.ID, Packages: []string{h.PkgID},
				Lifecycle: h.Lifecycle, Prev: h.Prev})
		}

		alerts, err := s.st.ListPlanAlerts(ctx, p.ID, store.HistoryFilter{PkgID: q.Pkg, Limit: n})
		if err != nil {
			return api.Timeline{}, err
		}
		for _, a := range alerts {
			e := api.TimelineEntry{At: a.RaisedAt, Kind: api.TimelineAlert, ID: a.ID, Packages: nonNil(a.Packages),
				Action: "raised", AlertKind: a.Kind, Message: a.Message}
			add(e)
			if !a.ClearedAt.IsZero() {
				e.At, e.Action = a.ClearedAt, "cleared"
				add(e)
			}
		}

		var prEntries []api.TimelineEntry
		for _, ref := range prOrder {
			repo, number, ok := parsePR(ref)
			if !ok {
				continue
			}
			pr, err := s.st.GetPR(ctx, repo, number)
			if err != nil {
				continue // linked by hand, not read from GitHub yet
			}
			lp := &api.LinkPR{Repo: pr.Repo, Number: pr.Number, Title: pr.Title, URL: pr.URL, State: pr.State,
				Draft: pr.Draft, HeadBranch: pr.HeadBranch, MergedAt: pr.MergedAt, ClosedAt: pr.ClosedAt}
			e := api.TimelineEntry{Kind: api.TimelinePR, ID: int64(pr.Number), Packages: prPkgs[ref], PR: lp}
			if at := pr.OpenedAt(); !at.IsZero() {
				e.At, e.Action = at, "opened"
				prEntries = append(prEntries, e)
			}
			switch {
			case pr.State == store.PRMerged && !pr.MergedAt.IsZero():
				e.At, e.Action = pr.MergedAt, "merged"
				prEntries = append(prEntries, e)
			case pr.State == store.PRClosed && !pr.ClosedAt.IsZero():
				e.At, e.Action = pr.ClosedAt, "closed"
				prEntries = append(prEntries, e)
			}
		}
		for _, e := range newest(prEntries, n) {
			add(e)
		}
	}

	links, err := s.st.ListLinks(ctx, p.ID)
	if err != nil {
		return api.Timeline{}, err
	}
	var linkEntries []api.TimelineEntry
	for _, l := range links {
		ref := refOf(l)
		if q.Agent != "" && (l.Kind != store.LinkAgent || ref != q.Agent) {
			continue
		}
		e := api.TimelineEntry{At: l.CreatedAt, Kind: api.TimelineLink, ID: l.ID, Packages: []string{l.PkgID},
			Link: &api.LinkRef{Plan: b.Slug, Pkg: l.PkgID, Target: api.Target{Kind: l.Kind, Ref: ref},
				Method: l.MatchMethod, Confidence: l.Confidence, ID: l.ID}}
		if l.Kind == store.LinkAgent {
			e.Agent = ref
		}
		linkEntries = append(linkEntries, e)
	}
	for _, e := range newest(linkEntries, n) {
		add(e)
	}

	// Events of the chosen agents: every linked agent, or the one asked
	// for; with a package, the agents linked to it.
	owner := map[store.EventLoop]*agentLoops{}
	var loops []store.EventLoop
	for _, a := range agents {
		if q.Agent != "" && a.Ref != q.Agent {
			continue
		}
		if q.Pkg != "" && !slices.Contains(a.Packages, q.Pkg) {
			continue
		}
		for _, l := range a.loops {
			if owner[l] == nil {
				owner[l] = a
				loops = append(loops, l)
			}
		}
	}
	events, err := s.st.ListLoopEvents(ctx, store.EventFilter{Loops: loops, Exclude: timelineSkip, Limit: n})
	if err != nil {
		return api.Timeline{}, err
	}
	for _, ev := range events {
		a := owner[store.EventLoop{SessionID: ev.SessionID, AgentID: ev.AgentID}]
		if a == nil {
			continue
		}
		add(api.TimelineEntry{At: ev.At, Kind: api.TimelineEvent, ID: ev.ID, Packages: a.Packages, Agent: a.Ref,
			Event: ev.Event, Tool: ev.ToolName, Summary: ev.Summary})
	}

	out = newest(out, len(out))
	t := api.Timeline{Slug: b.Slug, Title: b.Title, Query: q, Entries: out, Agents: []api.TimelineAgent{}}
	if len(out) > q.Limit {
		t.Entries, t.More = out[:q.Limit], true
	}
	if t.Entries == nil {
		t.Entries = []api.TimelineEntry{}
	}
	for _, a := range agents {
		t.Agents = append(t.Agents, a.TimelineAgent)
	}
	return t, nil
}

// kindOrder breaks ties between entries at the same time: what caused a
// change comes before (below, newest first) the change itself.
var kindOrder = map[string]int{
	api.TimelineEvent:     0,
	api.TimelinePR:        1,
	api.TimelineLink:      2,
	api.TimelineAlert:     3,
	api.TimelineLifecycle: 4,
}

// newest sorts entries newest first and keeps the first n.
func newest(es []api.TimelineEntry, n int) []api.TimelineEntry {
	slices.SortStableFunc(es, func(a, b api.TimelineEntry) int {
		return cmp.Or(b.At.Compare(a.At), cmp.Compare(kindOrder[b.Kind], kindOrder[a.Kind]), cmp.Compare(b.ID, a.ID))
	})
	if len(es) > n {
		es = es[:n]
	}
	return es
}

// addID adds a package ID to a list once.
func addID(ids []string, id string) []string {
	if slices.Contains(ids, id) {
		return ids
	}
	return append(ids, id)
}

// loopsOf returns the loops whose events belong to the agent an agent
// link's ref names: a subagent ("session/agent"), a session's main loop
// ("session/"), or every session of a background job ("job:<id>"), each
// under its session id and the ids it had before a /clear.
func loopsOf(ref string, sessions []store.Session) []store.EventLoop {
	var ids []string
	agentID := ""
	if job, ok := strings.CutPrefix(ref, "job:"); ok {
		for _, s := range sessions {
			if s.JobID == job && s.SessionID != "" {
				ids = append(ids, s.SessionID)
			}
		}
	} else if sid, aid, ok := strings.Cut(ref, "/"); ok && sid != "" {
		ids, agentID = []string{sid}, aid
	}
	prev := map[string][]string{}
	for _, s := range sessions {
		if s.SessionID != "" && s.PrevSessionID != "" {
			prev[s.SessionID] = append(prev[s.SessionID], s.PrevSessionID)
		}
	}
	seen := map[string]bool{}
	var out []store.EventLoop
	for len(ids) > 0 {
		id := ids[0]
		ids = ids[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, store.EventLoop{SessionID: id, AgentID: agentID})
		ids = append(ids, prev[id]...)
	}
	return out
}
