package state

import (
	"sort"
	"strings"

	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/worktrees"
)

// node is a target in the join graph: PR ↔ head branch ↔ worktree ↔ agent.
// Links reach from a linked target to its neighbours.
type node struct {
	t      Target
	pr     *store.PR
	branch *branchInfo
	wt     *store.Worktree
	agent  *Agent
	// mainCheckout is true for a repo's main worktree: agents there are the
	// lead and its non-isolated subagents, which aren't on any one package,
	// so no link crosses between it and its agents.
	mainCheckout bool
	edges        []*node
}

// graph is the join graph, keyed by targetKey.
type graph struct {
	nodes map[string]*node
	order []*node // in creation order, for stable output
}

func (g *graph) get(t Target) *node { return g.nodes[targetKey(t)] }

func (g *graph) ensure(t Target) *node {
	k := targetKey(t)
	if n := g.nodes[k]; n != nil {
		return n
	}
	n := &node{t: t}
	g.nodes[k] = n
	g.order = append(g.order, n)
	return n
}

func connect(a, b *node) {
	if a == nil || b == nil || a == b {
		return
	}
	for _, e := range a.edges {
		if e == b {
			return
		}
	}
	a.edges = append(a.edges, b)
	b.edges = append(b.edges, a)
}

// Terminal states of a subagent ($.agent.list()) and of a background job.
var agentEnded = map[string]bool{"completed": true, "failed": true, "killed": true, "done": true, "stopped": true}

// Session states (internal/sessions.State) used here.
const (
	sessionNeedsYou = "needs_you"
	sessionWorking  = "working"
	sessionTurnDone = "turn_done"
	sessionError    = "error"
	sessionGone     = "gone"
	sessionQuiet    = "quiet"
	agentWaiting    = "waiting"
)

// sessionLive reports whether a session is still running.
func sessionLive(s *store.Session) bool {
	if !s.EndedAt.IsZero() || s.State == sessionGone {
		return false
	}
	if s.Kind == "background" && (s.State == sessionTurnDone || s.State == sessionError) {
		return false // the job finished its turn or failed
	}
	return true
}

// The rules below are the board's, exported for the dashboard, which
// shows a card's agents as the store has them now, between passes.

// SessionLive reports whether a session row is still running, as the
// board decides it: not ended or gone, and for a background job, its turn
// not finished or failed.
func SessionLive(s *store.Session) bool { return sessionLive(s) }

// SessionNeedsYou reports whether a session row, live, waits on you and
// you haven't dismissed this wait.
func SessionNeedsYou(s *store.Session) bool {
	return sessionLive(s) && s.State == sessionNeedsYou &&
		(s.NeedsYouDismissedAt.IsZero() || s.NeedsYouDismissedAt.Before(s.NeedsYouSince))
}

// AgentEnded reports whether a subagent's state ($.agent.list()'s words)
// is a final one.
func AgentEnded(state string) bool { return agentEnded[state] }

// AgentWaiting is the subagent state that waits on you.
const AgentWaiting = agentWaiting

// buildGraph puts the snapshot's PRs, branches, worktrees, sessions and
// agents into the join graph.
func (w *world) buildGraph() *graph {
	g := &graph{nodes: map[string]*node{}}

	// Branches and their PRs. Cross-repo PRs aren't in it.
	for _, b := range w.branchList {
		bn := g.ensure(Target{store.LinkBranch, b.ref})
		bn.branch = b
		for _, pr := range b.prs {
			pn := g.ensure(Target{store.LinkPR, store.PRRef(pr.Repo, pr.Number)})
			pn.pr = pr
			connect(pn, bn)
		}
	}
	for _, pr := range w.prs { // PRs from a base branch have no branch node
		if n := g.ensure(Target{store.LinkPR, store.PRRef(pr.Repo, pr.Number)}); n.pr == nil {
			n.pr = pr
		}
	}

	// Worktrees, and the branch each has checked out.
	wtNodes := map[string]*node{} // by PathKey
	for i := range w.in.Worktrees {
		wt := &w.in.Worktrees[i]
		n := g.ensure(Target{store.LinkWorktree, wt.Path})
		n.wt = wt
		n.mainCheckout = worktrees.SamePath(wt.Path, wt.MainRepo)
		wtNodes[worktrees.PathKey(wt.Path)] = n
		if !wt.Detached && wt.Branch != "" {
			name := w.repoNames[worktrees.PathKey(wt.MainRepo)]
			if b := w.branches[branchKey(repoKey(name, wt.MainRepo), wt.Branch)]; b != nil {
				connect(n, g.get(Target{store.LinkBranch, b.ref}))
			}
		}
	}
	// wtFor returns the worktree node a path is in: the deepest worktree
	// whose path is it or contains it.
	wtFor := func(path string) *node {
		if path == "" {
			return nil
		}
		p := worktrees.PathKey(path)
		for {
			if n := wtNodes[p]; n != nil {
				return n
			}
			parent := parentDir(p)
			if parent == p {
				return nil
			}
			p = parent
		}
	}
	link := func(wt, ag *node) {
		if wt != nil && ag != nil && !wt.mainCheckout {
			connect(wt, ag)
		}
	}

	// Sessions: one agent node each, keyed on the job id for a background
	// job, else on the session id's main loop.
	bySessionID := map[string]*node{}
	for i := range w.in.Sessions {
		s := &w.in.Sessions[i]
		if s.Headless {
			continue
		}
		var ref string
		switch {
		case s.JobID != "":
			ref = store.JobRef(s.JobID)
		case s.SessionID != "":
			ref = store.AgentRef(s.SessionID, "")
		default:
			continue
		}
		n := g.ensure(Target{store.LinkAgent, ref})
		if n.agent == nil {
			n.agent = &Agent{Ref: n.t, JobID: s.JobID}
		}
		mergeSession(n.agent, s)
		if s.SessionID != "" {
			bySessionID[s.SessionID] = n
		}
		wt := wtFor(s.WorktreePath)
		if wt == nil {
			wt = wtFor(s.Cwd)
		}
		link(wt, n)
	}

	// Agent rows: a main loop joins its session's node; a subagent is a
	// node of its own, alive only while its session is.
	byAgentID := map[string][]*node{}
	for i := range w.in.Agents {
		a := &w.in.Agents[i]
		var n *node
		if a.AgentID == "" {
			n = bySessionID[a.SessionID]
		}
		if n == nil {
			n = g.ensure(Target{store.LinkAgent, store.AgentRef(a.SessionID, a.AgentID)})
		}
		if n.agent == nil {
			n.agent = &Agent{Ref: n.t, SessionID: a.SessionID, AgentID: a.AgentID, Kind: a.Kind, Name: a.AgentType, State: a.State, LastAction: a.LastAction,
				StartedAt: a.StartedAt, Tokens: a.Tokens}
			n.agent.Live = a.EndedAt.IsZero() && !agentEnded[a.State]
			if lead := bySessionID[a.SessionID]; lead != nil && lead.agent != nil && !lead.agent.Live {
				n.agent.Live = false
			}
		}
		if a.State == agentWaiting && n.agent.Live {
			n.agent.NeedsYou = true
			if n.agent.Reason == "" {
				n.agent.Reason = "waiting"
			}
		}
		if n.agent.WorktreePath == "" {
			n.agent.WorktreePath = a.WorktreePath
		}
		if n.agent.LastAction == "" {
			n.agent.LastAction = a.LastAction
		}
		if a.AgentID == "" && a.Tokens > 0 {
			n.agent.Tokens = a.Tokens // the main loop's count, on its session's node
		}
		if a.AgentID != "" {
			byAgentID[a.AgentID] = append(byAgentID[a.AgentID], n)
		}
		link(wtFor(a.WorktreePath), n)
	}

	// Worktrees name their agent (the agent-<id> folder, Claude Code's lock)
	// and sometimes their session. Without the mod, a locked worktree's
	// agent is found through the lead's pid in the lock.
	livePID := map[int]*store.Session{}
	for i := range w.in.Sessions {
		s := &w.in.Sessions[i]
		if s.PID > 0 && s.EndedAt.IsZero() {
			livePID[s.PID] = s
		}
	}
	for _, n := range g.order {
		wt := n.wt
		if wt == nil || n.mainCheckout {
			continue
		}
		if wt.SessionID != "" {
			link(n, bySessionID[wt.SessionID])
		}
		for _, id := range uniq(wt.AgentID, wt.LockAgentID) {
			if len(byAgentID[id]) > 0 {
				for _, an := range byAgentID[id] {
					link(n, an)
				}
				continue
			}
			lead := livePID[wt.LockPID]
			if id != wt.LockAgentID || lead == nil || lead.SessionID == "" {
				continue
			}
			an := g.ensure(Target{store.LinkAgent, store.AgentRef(lead.SessionID, id)})
			if an.agent == nil {
				an.agent = &Agent{Ref: an.t, SessionID: lead.SessionID, AgentID: id, Kind: "subagent", WorktreePath: wt.Path,
					Live: wt.Locked && wt.RemovedAt.IsZero() && sessionLive(lead)}
			}
			link(n, an)
		}
	}
	return g
}

// mergeSession folds one sessions-table row into an agent: a live row wins
// over an ended one with the same key.
func mergeSession(a *Agent, s *store.Session) {
	live := sessionLive(s)
	if a.SessionID != "" && a.Live && !live {
		return // keep the live row's view
	}
	a.SessionID = s.SessionID
	a.Kind = s.Kind
	a.Name = s.Name
	a.State = s.State
	a.Live = live
	a.Repo = s.Repo
	a.Branch = s.Branch
	a.WorktreePath = s.WorktreePath
	a.LastAction = s.LastAction
	a.LastEventAt = s.LastEventAt
	a.StartedAt = s.StartedAt
	a.NeedsYou = SessionNeedsYou(s)
	a.NeedsYouSince = s.NeedsYouSince
	a.Reason = s.StateReason
}

func uniq(ss ...string) []string {
	var out []string
	for _, s := range ss {
		if s != "" && (len(out) == 0 || out[len(out)-1] != s) {
			out = append(out, s)
		}
	}
	return out
}

// parentDir returns the directory above a path key ("/" stays "/").
func parentDir(p string) string {
	i := strings.LastIndexByte(p, '/')
	switch {
	case i < 0:
		return p
	case i == 0:
		return "/"
	}
	return p[:i]
}

// owner is a package some target is directly linked to.
type owner struct {
	plan int64
	pkg  string // canonID
}

// closure works out every link of one package: its direct links, and the
// targets reached from them through the join graph. A target that you
// unlinked from the package, or that is directly linked to another
// package, is not reached, and nothing is reached through it.
func closure(g *graph, w *world, planID int64, pkgID string, direct []Link, owners map[string][]owner) []Link {
	me := owner{planID, canonID(pkgID)}
	out := make([]Link, 0, len(direct))
	seen := map[string]bool{}
	type item struct {
		n    *node
		seed Link
	}
	var queue []item
	// Stronger links claim a target first.
	sorted := append([]Link(nil), direct...)
	sort.SliceStable(sorted, func(i, j int) bool { return linkRank(sorted[i]) < linkRank(sorted[j]) })
	for _, l := range sorted {
		k := targetKey(l.Target)
		if seen[k] {
			continue
		}
		seen[k] = true
		n := g.get(l.Target)
		fill(&l, n, w)
		out = append(out, l)
		if n != nil {
			queue = append(queue, item{n, l})
		}
	}
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		for _, nb := range it.n.edges {
			k := targetKey(nb.t)
			if seen[k] || w.isDead(planID, pkgID, nb.t) || ownedElsewhere(owners[k], me) {
				continue
			}
			seen[k] = true
			l := Link{Target: nb.t, Method: it.seed.Method, Confidence: it.seed.Confidence, Inferred: it.seed.Inferred, Via: it.n.t}
			fill(&l, nb, w)
			out = append(out, l)
			queue = append(queue, item{nb, it.seed})
		}
	}
	return out
}

func ownedElsewhere(os []owner, me owner) bool {
	for _, o := range os {
		if o != me {
			return true
		}
	}
	return false
}

// fill attaches what a link points at.
func fill(l *Link, n *node, w *world) {
	if n != nil {
		l.PR, l.Worktree, l.Agent = n.pr, n.wt, n.agent
	}
	if l.Kind == store.LinkPR && l.PR == nil {
		l.PR = w.prByRef[targetKey(l.Target)]
	}
}

// linkRank orders links strongest first: by confidence, then by method,
// most reliable first.
func linkRank(l Link) int {
	c := map[string]int{store.ConfidenceExact: 0, store.ConfidenceHigh: 1, store.ConfidenceLow: 2}[l.Confidence]
	m := map[string]int{store.MatchManual: 0, store.MatchSpawn: 1, store.MatchPlanLine: 2, store.MatchTitle: 3, store.MatchBranch: 4}[l.Method]
	return c*10 + m
}
