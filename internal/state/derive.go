package state

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/plans"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/worktrees"
)

// Defaults for the zero Options fields.
const (
	DefaultStalledAfter = 24 * time.Hour
	DefaultQuietAfter   = 15 * time.Minute
)

// Options tune Derive.
type Options struct {
	// StalledAfter is how long a package can have commits with no live
	// agent and no PR before it's stalled (config state.stalled_after);
	// DefaultStalledAfter if zero.
	StalledAfter time.Duration
	// QuietAfter is how long a working session can go without a mod event
	// before it's quiet; DefaultQuietAfter if zero. A session whose state
	// is already "quiet" is quiet whatever this says.
	QuietAfter time.Duration
	// UnlockedSince is when you last looked: the dependencies-unlocked alert
	// lists packages that went from blocked to ready after it. Zero lists
	// every package whose last change was from blocked to ready.
	UnlockedSince time.Time
}

func (o Options) withDefaults() Options {
	if o.StalledAfter <= 0 {
		o.StalledAfter = DefaultStalledAfter
	}
	if o.QuietAfter <= 0 {
		o.QuietAfter = DefaultQuietAfter
	}
	return o
}

// pkgCtx is one package while Derive works on it.
type pkgCtx struct {
	plan  *planIdx
	pkg   *store.Package
	links []Link
	// What the links reach.
	prs        []*store.PR
	liveAgents []*Agent
	worktrees  []*store.Worktree // existing ones
}

// Derive works out every plan's board from a snapshot and the matcher's
// answer for it: each package's lifecycle, badges and links (with how each
// was made), and the plan's Unmatched lane, warnings and alerts. It is
// pure. The links it starts from are in.Links that aren't Inferred (spawn
// and manual links) plus m's links; stored inferred links are ignored but
// for their ids. See the package documentation for the rules.
func Derive(in Input, m Matches, opts Options) []PlanState {
	opts = opts.withDefaults()
	w := newWorld(&in)
	g := w.buildGraph()

	// Direct links, by plan and package.
	type pkgKey struct {
		plan int64
		pkg  string
	}
	direct := map[pkgKey][]Link{}
	owners := map[string][]owner{}
	storedID := map[string]int64{} // inferred links' ids, by plan, package and target
	addDirect := func(l store.Link, inferred bool) {
		t := Target{l.Kind, linkRef(l)}
		if w.isDead(l.PlanID, l.PkgID, t) {
			return
		}
		k := pkgKey{l.PlanID, canonID(l.PkgID)}
		id := l.ID
		if inferred && id == 0 {
			id = storedID[fmt.Sprint(l.PlanID, " ", k.pkg, " ", targetKey(t))]
		}
		direct[k] = append(direct[k], Link{Target: t, Method: l.MatchMethod, Confidence: l.Confidence, ID: id, Inferred: inferred})
		o := owner{l.PlanID, k.pkg}
		tk := targetKey(t)
		for _, have := range owners[tk] {
			if have == o {
				return
			}
		}
		owners[tk] = append(owners[tk], o)
	}
	for _, l := range in.Links {
		if l.Inferred {
			storedID[fmt.Sprint(l.PlanID, " ", canonID(l.PkgID), " ", targetKey(Target{l.Kind, linkRef(l)}))] = l.ID
		}
	}
	for _, l := range in.Links {
		if !l.Inferred {
			addDirect(l, false)
		}
	}
	for _, p := range w.plans {
		for _, l := range m.Links[p.ID] {
			addDirect(l, true)
		}
	}

	// Every package's links, and everything linked to any package.
	ctxs := map[pkgKey]*pkgCtx{}
	linked := map[string]bool{}
	for _, p := range w.plans {
		for _, pkg := range p.order {
			k := pkgKey{p.ID, canonID(pkg.PkgID)}
			c := &pkgCtx{plan: p, pkg: pkg, links: closure(g, w, p.ID, pkg.PkgID, direct[k], owners)}
			seenAgent := map[*Agent]bool{}
			for _, l := range c.links {
				linked[targetKey(l.Target)] = true
				if l.PR != nil {
					c.prs = append(c.prs, l.PR)
				}
				if l.Agent != nil && l.Agent.Live && !seenAgent[l.Agent] {
					seenAgent[l.Agent] = true
					c.liveAgents = append(c.liveAgents, l.Agent)
				}
				if l.Worktree != nil && l.Worktree.RemovedAt.IsZero() {
					c.worktrees = append(c.worktrees, l.Worktree)
				}
			}
			ctxs[k] = c
		}
	}

	out := make([]PlanState, 0, len(w.plans))
	for _, p := range w.plans {
		get := func(id string) *pkgCtx { return ctxs[pkgKey{p.ID, canonID(id)}] }
		out = append(out, derivePlan(w, g, p, get, linked, m, in.Now, opts))
	}
	return out
}

// derivePlan builds one plan's board.
func derivePlan(w *world, g *graph, p *planIdx, get func(string) *pkgCtx, linked map[string]bool, m Matches, now time.Time, opts Options) PlanState {
	ps := PlanState{PlanID: p.ID, Slug: p.Slug, Title: p.Title, Counts: map[Lifecycle]int{}}
	parsed := plans.Parse([]byte(p.SourceMD))
	ps.Lanes = parsed.Lanes
	ps.Merges = parsed.Merges
	for _, wn := range parsed.Warnings {
		ps.Warnings = append(ps.Warnings, Warning{Kind: string(wn.Kind), Line: wn.Line, ID: wn.ID, Message: wn.Message})
	}
	inCycle, cycleList := cycles(p.Packages)
	for _, cyc := range cycleList {
		line := 0
		if pp, ok := parsed.Package(cyc[0]); ok {
			line = pp.Line
		}
		ps.Warnings = append(ps.Warnings, Warning{Kind: WarnCycle, Line: line, ID: cyc[0],
			Message: fmt.Sprintf("dependency cycle %s: these packages stay blocked until one of them is done or their Needs change", strings.Join(cyc, " → "))})
	}

	// Done for the purpose of Needs: the brief's "done" condition, whatever
	// the package's lifecycle shows.
	done := map[string]bool{}
	for _, pkg := range p.order {
		done[canonID(pkg.PkgID)] = isDone(get(pkg.PkgID))
	}

	for _, pkg := range p.order {
		c := get(pkg.PkgID)
		st := PackageState{
			ID: pkg.PkgID, Title: pkg.Title, What: pkg.What, Repo: pkg.Repo, Size: pkg.Size, Owner: pkg.Owner, Lane: pkg.Lane,
			Needs: pkg.Needs, NeedsNote: pkg.NeedsNote, NeedsYou: pkg.NeedsYou, Yours: yours(pkg),
			Provisional: pkg.Provisional, Links: c.links,
		}
		for _, n := range pkg.Needs {
			if _, known := p.pkgs[canonID(n)]; known && !done[canonID(n)] {
				st.BlockedBy = append(st.BlockedBy, n)
			}
		}
		st.Cycle = inCycle[canonID(pkg.PkgID)]
		st.Lifecycle = lifecycle(c, st.BlockedBy)
		st.PrevLifecycle, st.Since = Lifecycle(pkg.LifecyclePrev), pkg.LifecycleAt
		if string(st.Lifecycle) != pkg.Lifecycle {
			st.PrevLifecycle, st.Since, st.Changed = Lifecycle(pkg.Lifecycle), now, true
		}
		st.Badges = badges(c, st, now, opts)
		ps.Counts[st.Lifecycle]++
		// A provisional package isn't the plan's to dispatch until the
		// file has its row.
		if st.Lifecycle == Ready && !st.Yours && !st.Provisional {
			ps.ReadyToDispatch = append(ps.ReadyToDispatch, st.ID)
		}
		ps.Packages = append(ps.Packages, st)
	}

	lanes := laneConflicts(&ps, parsed, get)
	ps.Alerts = alerts(w, g, p, &ps, lanes, linked, opts, now)
	ps.Unmatched, ps.Hidden = unmatched(w, p, linked, m)
	return ps
}

// isDone reports whether a package is done for the packages that need it:
// a linked PR merged and none open (not for your own steps), ✅ in the
// plan, or Mark done.
func isDone(c *pkgCtx) bool {
	if c.pkg.DoneInPlan || c.pkg.DoneManual {
		return true
	}
	if yours(c.pkg) {
		return false
	}
	merged := false
	for _, pr := range c.prs {
		switch pr.State {
		case store.PROpen:
			return false
		case store.PRMerged:
			merged = true
		}
	}
	return merged
}

// lifecycle decides a package's lifecycle: the first of in review, done,
// working, dropped and blocked that applies, else ready. A package in a
// dependency cycle is blocked only by the members it needs that aren't
// done (they are in blockedBy like any other need), so once a member is
// done the rest proceed.
func lifecycle(c *pkgCtx, blockedBy []string) Lifecycle {
	open, closed := false, 0
	for _, pr := range c.prs {
		switch pr.State {
		case store.PROpen:
			open = true
		case store.PRClosed:
			closed++
		}
	}
	switch {
	case open:
		return InReview
	case isDone(c):
		return Done
	case len(c.liveAgents) > 0 || len(commitWorktrees(c)) > 0:
		return Working
	case len(c.prs) > 0 && closed == len(c.prs):
		return Dropped
	case len(blockedBy) > 0:
		return Blocked
	}
	return Ready
}

// hasCommits reports whether a worktree's branch has commits of its own: it
// is ahead of its upstream, or its last commit is newer than the worktree.
// A new worktree's HEAD is the commit it was made from, older than the
// worktree itself.
func hasCommits(wt *store.Worktree) bool {
	if wt.UnpushedCount > 0 {
		return true
	}
	return !wt.LastCommitAt.IsZero() && !wt.FirstSeen.IsZero() && wt.LastCommitAt.After(wt.FirstSeen)
}

// commitWorktrees returns the package's existing worktrees with commits.
func commitWorktrees(c *pkgCtx) []*store.Worktree {
	var out []*store.Worktree
	for _, wt := range c.worktrees {
		if hasCommits(wt) {
			out = append(out, wt)
		}
	}
	return out
}

// ciFailing reports whether a PR's checks failed. No checks at all ("" as
// the store keeps it, NONE as the GitHub client reports it) counts as
// green, like SUCCESS.
func ciFailing(checks string) bool { return checks == "FAILURE" || checks == "ERROR" }

// badges works out a package's badges, in BadgeKind order.
func badges(c *pkgCtx, st PackageState, now time.Time, opts Options) []Badge {
	var bs []Badge
	add := func(b Badge) { bs = append(bs, b) }

	for _, a := range c.liveAgents {
		if a.NeedsYou {
			text := "needs you"
			if a.Reason != "" {
				text += ": " + a.Reason
			}
			add(Badge{Kind: BadgeNeedsYou, Text: text, Ref: a.Ref, Since: a.NeedsYouSince})
		}
	}
	var open []*store.PR
	for _, pr := range c.prs {
		if pr.State == store.PROpen {
			open = append(open, pr)
		}
	}
	prBadge := func(kind BadgeKind, text string, ok func(*store.PR) bool) {
		for _, pr := range open {
			if ok(pr) {
				add(Badge{Kind: kind, Text: fmt.Sprintf("%s on #%d", text, pr.Number), Ref: Target{store.LinkPR, store.PRRef(pr.Repo, pr.Number)}})
			}
		}
	}
	prBadge(BadgeCIFailing, "CI failing", func(pr *store.PR) bool { return ciFailing(pr.ChecksState) })
	prBadge(BadgeConflict, "conflict", func(pr *store.PR) bool {
		return pr.MergeStateStatus == "DIRTY" || pr.Mergeable == "CONFLICTING"
	})
	prBadge(BadgeWaitingOnYou, "waiting on you", func(pr *store.PR) bool { return !pr.Draft && pr.MergeStateStatus == "CLEAN" })
	prBadge(BadgeChangesRequested, "changes requested", func(pr *store.PR) bool { return pr.ReviewDecision == "CHANGES_REQUESTED" })
	prBadge(BadgeReviewRequested, "review requested", func(pr *store.PR) bool { return pr.ReviewDecision == "REVIEW_REQUIRED" })
	prBadge(BadgeDraft, "draft", func(pr *store.PR) bool { return pr.Draft })

	if since, ok := stalledSince(c, st, now, opts); ok {
		add(Badge{Kind: BadgeStalled, Text: "stalled: commits, no live agent and no PR for " + roundAge(now.Sub(since)), Since: since})
	}
	for _, a := range c.liveAgents {
		if quiet(a, now, opts) {
			add(Badge{Kind: BadgeQuiet, Text: "quiet", Ref: a.Ref, Since: a.LastEventAt})
		}
	}
	if n := workers(c.liveAgents); n > 1 {
		add(Badge{Kind: BadgeCollision, Text: fmt.Sprintf("collision: %d live agents", n)})
	}
	for _, l := range st.Links {
		if l.Confidence == store.ConfidenceLow {
			add(Badge{Kind: BadgeLowConfidence, Text: "low-confidence link: " + l.String(), Ref: l.Target})
			break
		}
	}
	if st.NeedsYou != "" && st.Lifecycle != Done {
		add(Badge{Kind: BadgeYou, Text: "you: " + st.NeedsYou})
	}
	return bs
}

// stalledSince reports whether a working package is stalled, and since
// when: it has existing worktrees with commits, no live agent and no open
// PR, and its newest commit is older than StalledAfter.
func stalledSince(c *pkgCtx, st PackageState, now time.Time, opts Options) (time.Time, bool) {
	if st.Lifecycle != Working || len(c.liveAgents) > 0 {
		return time.Time{}, false
	}
	var last time.Time
	for _, wt := range commitWorktrees(c) {
		if wt.LastCommitAt.After(last) {
			last = wt.LastCommitAt
		}
	}
	if last.IsZero() || now.Sub(last) <= opts.StalledAfter {
		return time.Time{}, false
	}
	return last, true
}

// quiet reports whether a live agent is quiet: its session says so, or it
// is working and the mod hasn't reported for QuietAfter.
func quiet(a *Agent, now time.Time, opts Options) bool {
	if a.State == sessionQuiet {
		return true
	}
	return a.State == sessionWorking && !a.LastEventAt.IsZero() && now.Sub(a.LastEventAt) > opts.QuietAfter
}

// laneConflicts badges the packages of single-writer lanes with more than
// one live agent, and returns an alert for each such lane.
func laneConflicts(ps *PlanState, parsed plans.Plan, get func(string) *pkgCtx) []Alert {
	var out []Alert
	for _, lane := range parsed.Lanes {
		if !lane.SingleWriter {
			continue
		}
		var agents []*Agent
		var busy []string
		for i := range ps.Packages {
			st := &ps.Packages[i]
			if !strings.EqualFold(st.Lane, lane.Name) {
				continue
			}
			c := get(st.ID)
			if len(c.liveAgents) > 0 {
				busy = append(busy, st.ID)
			}
			agents = append(agents, c.liveAgents...)
		}
		n := workers(agents)
		if n < 2 {
			continue
		}
		text := fmt.Sprintf("lane %s is single-writer and has %d live agents", lane.Name, n)
		for i := range ps.Packages {
			st := &ps.Packages[i]
			if strings.EqualFold(st.Lane, lane.Name) && len(get(st.ID).liveAgents) > 0 {
				st.Badges = insertBadge(st.Badges, Badge{Kind: BadgeLaneConflict, Text: text})
			}
		}
		out = append(out, Alert{Kind: AlertLaneCollision, Lane: lane.Name, Packages: busy,
			Message: fmt.Sprintf("Lane %s is single-writer, and %d live agents are in it (%s)", lane.Name, n, strings.Join(busy, ", "))})
	}
	return out
}

// workers counts the distinct agents working: a session counts once,
// with its subagents unless they work apart from it (a subagent whose
// session is in the list too is the same worker, like an interactive
// session and the helper it spawned in the same worktree).
func workers(agents []*Agent) int {
	sessions := map[string]bool{}
	seen := map[*Agent]bool{}
	for _, a := range agents {
		if a.AgentID == "" && a.SessionID != "" {
			sessions[a.SessionID] = true
		}
	}
	n := 0
	for _, a := range agents {
		if seen[a] {
			continue
		}
		seen[a] = true
		if a.AgentID != "" && sessions[a.SessionID] {
			continue
		}
		n++
	}
	return n
}

// badgeOrder is BadgeKind's display order.
var badgeOrder = map[BadgeKind]int{
	BadgeNeedsYou: 0, BadgeCIFailing: 1, BadgeConflict: 2, BadgeWaitingOnYou: 3, BadgeChangesRequested: 4,
	BadgeReviewRequested: 5, BadgeDraft: 6, BadgeStalled: 7, BadgeQuiet: 8, BadgeCollision: 9,
	BadgeLaneConflict: 10, BadgeLowConfidence: 11, BadgeYou: 12,
}

func insertBadge(bs []Badge, b Badge) []Badge {
	i := sort.Search(len(bs), func(i int) bool { return badgeOrder[bs[i].Kind] > badgeOrder[b.Kind] })
	bs = append(bs, Badge{})
	copy(bs[i+1:], bs[i:])
	bs[i] = b
	return bs
}

// alerts works out a plan's alerts, in this order: dependencies unlocked,
// stalled packages, lane collisions (from laneConflicts), agents off the
// plan.
func alerts(w *world, g *graph, p *planIdx, ps *PlanState, lanes []Alert, linked map[string]bool, opts Options, now time.Time) []Alert {
	var out []Alert
	var unlocked []string
	for _, st := range ps.Packages {
		if st.Lifecycle == Ready && st.PrevLifecycle == Blocked && (opts.UnlockedSince.IsZero() || st.Since.After(opts.UnlockedSince)) {
			unlocked = append(unlocked, st.ID)
		}
	}
	if len(unlocked) > 0 {
		out = append(out, Alert{Kind: AlertUnlocked, Packages: unlocked, Since: opts.UnlockedSince,
			Message: fmt.Sprintf("Dependencies unlocked: %s ready to dispatch", strings.Join(unlocked, ", "))})
	}
	for _, st := range ps.Packages {
		for _, b := range st.Badges {
			if b.Kind == BadgeStalled {
				out = append(out, Alert{Kind: AlertStalled, Packages: []string{st.ID}, Since: b.Since,
					Message: fmt.Sprintf("%s is stalled: commits, no live agent and no PR for %s", st.ID, roundAge(now.Sub(b.Since)))})
			}
		}
	}
	out = append(out, lanes...)
	// Off-plan: a live agent in one of the plan's repos, on a branch that
	// matches no package, and not linked to one itself.
	for _, n := range g.order {
		a := n.agent
		if a == nil || !a.Live || linked[targetKey(n.t)] {
			continue
		}
		keys, branch, where := agentBranch(w, n)
		if branch == "" || len(keys) == 0 || w.isBase(keys[0], branch) {
			continue
		}
		inRepo, branchLinked := false, false
		for _, k := range keys {
			inRepo = inRepo || p.repos[k]
			if b := w.branches[branchKey(k, branch)]; b != nil && linked[targetKey(Target{store.LinkBranch, b.ref})] {
				branchLinked = true
			}
		}
		if !inRepo || branchLinked {
			continue
		}
		name := a.Name
		if name == "" {
			name = a.Ref.Ref
		}
		out = append(out, Alert{Kind: AlertOffPlan, Ref: a.Ref,
			Message: fmt.Sprintf("%s is working in %s on %s, which matches no package of %s", name, where, branch, p.slug)})
	}
	return out
}

// agentBranch returns the keys of the repo an agent works in (the first
// one preferred), its branch, and a name for the repo: from the worktree
// it's in, or else from its session's repo and branch.
func agentBranch(w *world, n *node) (keys []string, branch, where string) {
	for _, e := range n.edges {
		if wt := e.wt; wt != nil && !wt.Detached && wt.Branch != "" {
			name := w.repoNames[worktrees.PathKey(wt.MainRepo)]
			where = name
			if where == "" {
				where = wt.MainRepo
			}
			keys = []string{repoKey(name, wt.MainRepo)}
			if name != "" {
				keys = append(keys, repoKey("", wt.MainRepo))
			}
			return keys, wt.Branch, where
		}
	}
	if n.agent.Repo != "" && n.agent.Branch != "" {
		return []string{normRepo(n.agent.Repo)}, n.agent.Branch, n.agent.Repo
	}
	return nil, "", ""
}

// unmatched lists the plan's Unmatched lane: same-repo PRs in its repos
// that no package of any plan links (open ones, and closed ones that
// closed after the plan was registered), then branches without a PR that
// have a worktree and no link. Each says when it appeared and whether that
// was after the plan was registered. The ones you hid from the plan go in
// hidden instead, most recently hidden first.
func unmatched(w *world, p *planIdx, linked map[string]bool, m Matches) (out, hidden []Unmatched) {
	add := func(u Unmatched) {
		u.New = u.Since.IsZero() || u.Since.After(p.CreatedAt)
		if at, ok := w.hidden[p.ID][targetKey(u.Target)]; ok {
			u.HiddenAt = at
			hidden = append(hidden, u)
			return
		}
		out = append(out, u)
	}
	for _, pr := range w.prs {
		if !p.repos[repoKey(pr.Repo, "")] {
			continue
		}
		t := Target{store.LinkPR, store.PRRef(pr.Repo, pr.Number)}
		if linked[targetKey(t)] {
			continue
		}
		if pr.State != store.PROpen {
			end := pr.ClosedAt
			if end.IsZero() {
				end = pr.MergedAt
			}
			if !end.IsZero() && end.Before(p.CreatedAt) {
				continue // history from before the plan
			}
		}
		add(Unmatched{Target: t, Repo: pr.Repo, Branch: pr.HeadBranch, Number: pr.Number,
			Title: pr.Title, State: pr.State, Note: m.Notes[t], Since: pr.OpenedAt()})
	}
	for _, b := range w.branchList {
		if len(b.prs) > 0 || !inPlan(p, b) {
			continue
		}
		t := Target{store.LinkBranch, b.ref}
		if linked[targetKey(t)] {
			continue
		}
		exists := false
		for _, wt := range b.worktrees {
			if wt.RemovedAt.IsZero() {
				exists = true
			}
		}
		if !exists {
			continue
		}
		repo := b.repoName
		if repo == "" {
			repo = b.repoPath
		}
		add(Unmatched{Target: t, Repo: repo, Branch: b.name, Note: m.Notes[t], Since: b.firstSeen})
	}
	sort.SliceStable(hidden, func(i, j int) bool { return hidden[i].HiddenAt.After(hidden[j].HiddenAt) })
	return out, hidden
}

func inPlan(p *planIdx, b *branchInfo) bool {
	for _, k := range b.repoKeys() {
		if p.repos[k] {
			return true
		}
	}
	return false
}

// roundAge shows a duration the way a card does: "3h", "2d", "45m".
func roundAge(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
}
