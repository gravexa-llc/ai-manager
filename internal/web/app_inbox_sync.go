package web

// The Needs you inbox's sync: what waits now, worked out from the store's
// sessions and PRs with internal/monitor's monitoring, handed to
// internal/inbox, which keeps the items. See internal/inbox for the rules.
//
// SyncInbox runs a pass. It is called:
//   - before the inbox page, its fragments, GET /api/inbox and the menu's
//     Needs you badge are read, so they show what the store holds now;
//   - before plan summaries and bands are read by the plan API's reads
//     (syncForSummaries); the app's pages run one through the
//     menu's Needs you badge;
//   - by aimd on a ticker (RunInbox), so the timers fire and items open
//     while no page is open;
//   - by the notifier, before it decides what to notify about
//     (InboxOpenSessions).
//
// Passes take turns (Server.inbox), and each reads the store afresh after
// the one before it, so a slower pass never brings back an episode a
// later one saw end.

import (
	"cmp"
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/inbox"
	"github.com/gravexa-llc/ai-manager/internal/monitor"
	"github.com/gravexa-llc/ai-manager/internal/prwait"
	"github.com/gravexa-llc/ai-manager/internal/sessions"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
	"github.com/gravexa-llc/ai-manager/internal/worktrees"
)

// inboxState serializes the server's sync passes.
type inboxState struct {
	mu sync.Mutex
}

// InboxStore is the part of *store.Store the inbox needs. The server's
// Store has it when it is a *store.Store; another Store leaves the inbox
// out (the page says so and the badge falls back to the old bar's count).
type InboxStore interface {
	inbox.Store
	InboxItem(ctx context.Context, id int64) (store.InboxItem, error)
	InboxMoves(ctx context.Context, itemID int64) ([]store.InboxMove, error)
	InboxCounts(ctx context.Context) (map[string]int, error)
}

var _ InboxStore = (*store.Store)(nil)

func (s *Server) inboxStore() (InboxStore, bool) {
	st, ok := s.opts.Store.(InboxStore)
	return st, ok
}

// inboxSettings are the inbox's timings: the back-to-Open delay and how
// long Done items are kept, from the Settings page (app_settings.go),
// and the snooze choices.
func (s *Server) inboxSettings(ctx context.Context) inbox.Settings {
	return inboxSettingsOf(s.settings(ctx))
}

// inboxReady reports whether the session list is fresh enough to sync
// from: the session poller has succeeded since aimd started. Before that
// the store still holds the sessions of aimd's last run. Without a status
// source (tests), or one that doesn't list the poller, it is ready.
func (s *Server) inboxReady() bool {
	if s.opts.Status == nil {
		return true
	}
	for _, src := range s.opts.Status.Status().Sources {
		if src.Name == api.SourceSessions {
			return !src.LastOK.IsZero()
		}
	}
	return true
}

// SyncInbox runs one sync pass and, when it changed anything, publishes
// the inbox topic. It does nothing without an inbox store, or before the
// first session poll.
func (s *Server) SyncInbox(ctx context.Context) (inbox.Result, error) {
	st, ok := s.inboxStore()
	if !ok || !s.inboxReady() {
		return inbox.Result{}, nil
	}
	s.inbox.mu.Lock()
	defer s.inbox.mu.Unlock()
	in, err := s.inboxInput(ctx)
	if err != nil {
		return inbox.Result{}, err
	}
	res, err := inbox.Sync(ctx, st, s.inboxSnapshot(in), s.now(), s.inboxSettings(ctx))
	if err != nil {
		return res, err
	}
	if res.Changed() {
		s.opts.Hub.Publish(api.TopicInbox)
	}
	return res, nil
}

// syncForSummaries runs a sync pass before plan summaries or bands are
// read: their need-you counts are the plans' Open items in the
// inbox (internal/planapi reads them from the store), so they show what
// waits now, as the inbox does. A failure is logged, and the summaries
// count the items as they stand.
func (s *Server) syncForSummaries(ctx context.Context) {
	if _, err := s.SyncInbox(ctx); err != nil {
		s.log.Error("inbox sync", "err", err)
	}
}

// RunInbox runs a sync pass every interval until ctx is done: aimd's
// ticker, so items open, time out of In progress and wake from a snooze
// while no page is open. Passes wait for the first session poll
// (inboxReady).
func (s *Server) RunInbox(ctx context.Context, every time.Duration) error {
	if every <= 0 {
		every = 10 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if _, err := s.SyncInbox(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("inbox sync", "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// InboxOpenSessions runs a sync pass, then returns the Open items of
// sessions, by session row id: what the notifier may notify about.
func (s *Server) InboxOpenSessions(ctx context.Context) (map[int64]int64, error) {
	st, ok := s.inboxStore()
	if !ok {
		return nil, fmt.Errorf("web: the store has no inbox")
	}
	if _, err := s.SyncInbox(ctx); err != nil {
		return nil, err
	}
	items, err := st.ListInbox(ctx, store.InboxFilter{State: inbox.Open, Waiting: true})
	if err != nil {
		return nil, err
	}
	out := map[int64]int64{}
	for _, it := range items {
		if id, ok := strings.CutPrefix(it.Ref, "session:"); ok && it.Source == inbox.SourceSession {
			if row, err := strconv.ParseInt(id, 10, 64); err == nil {
				out[row] = it.ID
			}
		}
	}
	return out, nil
}

// InboxItemURL is an item's page: where a notification about it should
// land.
func InboxItemURL(id int64) string { return inbox.ItemURL(id) }

// inboxInput is what a sync pass reads.
type inboxInput struct {
	now       time.Time
	sessions  []store.Session
	prs       []store.PR
	mon       *monitor.Set
	repoNames map[string]string // worktrees.PathKey(main checkout) → nameWithOwner
	// boards are the registered plans' boards, for their provisional
	// packages; rowWaiting the refs of the missing-row items still
	// waiting.
	boards     []api.Plan
	rowWaiting map[string]bool
}

// inboxInput reads the store afresh. Unlike the pages, it doesn't go on
// when the monitoring can't be read: a nil set would monitor every
// session and open items for all of them.
func (s *Server) inboxInput(ctx context.Context) (inboxInput, error) {
	var in inboxInput
	var err error
	if in.sessions, err = s.opts.Store.ListSessions(ctx, store.SessionFilter{}); err != nil {
		return in, err
	}
	if in.prs, err = s.opts.Store.ListPRs(ctx, store.PRFilter{}); err != nil {
		return in, err
	}
	if in.mon, err = s.opts.Monitor.Load(ctx, in.sessions, nil); err != nil {
		return in, err
	}
	in.repoNames = map[string]string{}
	if s.opts.Repos != nil {
		for path, name := range s.opts.Repos.RepoNames() {
			in.repoNames[worktrees.PathKey(path)] = name
		}
	}
	in.now = s.now()
	// The boards as the monitoring reads them (the plan service's, in
	// aimd); without monitoring wired there are no plan-row items.
	if src := s.opts.Monitor; src != nil && src.Plans != nil {
		if in.boards, err = src.Plans(ctx); err != nil {
			return in, err
		}
	}
	in.rowWaiting = map[string]bool{}
	if st, ok := s.inboxStore(); ok {
		items, err := st.ListInbox(ctx, store.InboxFilter{Waiting: true})
		if err != nil {
			return in, err
		}
		for _, it := range items {
			if it.Source == inbox.SourcePlan {
				in.rowWaiting[it.Ref] = true
			}
		}
	}
	return in, nil
}

// inboxSnapshot works out what waits now: the monitored sessions that need
// you (headless runs aside), and the PRs the PR rule says need you; and
// for every other session and PR, why it doesn't.
func (s *Server) inboxSnapshot(in inboxInput) inbox.Snapshot {
	snap := inbox.Snapshot{Stopped: map[string]string{}}
	for _, row := range in.sessions {
		if row.Headless {
			continue
		}
		ref := inbox.SessionRef(row.ID)
		switch {
		case row.State != needsYou:
			snap.Stopped[ref] = "it stopped waiting"
		case s.isExpiredJob(row):
			// An expired background job never needs you: Claude Code
			// deleted its conversation, so there is nothing to answer.
			snap.Stopped[ref] = "its conversation was deleted: the job expired"
		case !in.mon.Monitored(row):
			snap.Stopped[ref] = "it is no longer monitored"
		default:
			snap.Episodes = append(snap.Episodes, sessionEpisode(in, row))
		}
	}
	for _, p := range in.prs {
		ref := store.PRRef(p.Repo, p.Number)
		kind, v := inboxPRNeed(in.mon, p)
		if kind == "" {
			snap.Stopped[ref] = prStopped(p, v)
			continue
		}
		snap.Episodes = append(snap.Episodes, prEpisode(p, kind, v))
	}
	planRowEpisodes(in, &snap)
	return snap
}

// planRowEpisodes adds the provisional packages whose row is still
// missing from the plan file: one item each, once the package has been
// provisional for inbox.PlanRowAfter and its plan has a monitored lead.
// An item that waits stays until the row lands (the package is the file's
// then), you Mark the package done, or the package goes (it expired, or
// the plan was unregistered).
func planRowEpisodes(in inboxInput, snap *inbox.Snapshot) {
	for _, b := range in.boards {
		file := cmp.Or(filepath.Base(b.Path), "the plan file")
		lead := planHasLead(in, b)
		for _, p := range b.Packages {
			ref := inbox.PlanRowRef(b.Slug, p.ID)
			waiting := in.rowWaiting[ref]
			switch {
			case !p.Provisional:
				snap.Stopped[ref] = "its row is in " + file + " now"
			case p.DoneManual:
				snap.Stopped[ref] = "it was marked done"
			case !waiting && in.now.Sub(p.FirstSeen) < inbox.PlanRowAfter:
				snap.Stopped[ref] = "not missing for long yet"
			case !waiting && !lead && !spawnedByMonitored(in, p):
				snap.Stopped[ref] = "its plan has no monitored lead"
			default:
				snap.Episodes = append(snap.Episodes, planRowEpisode(b, p, file))
			}
		}
	}
}

// planHasLead reports whether a plan has a monitored lead running: a live
// session monitored because it registered the plan, or the session the
// board names as the one that registered it, if that is monitored.
func planHasLead(in inboxInput, b api.Plan) bool {
	for _, row := range in.sessions {
		if row.Headless || !row.EndedAt.IsZero() {
			continue
		}
		for _, w := range in.mon.Why(row) {
			if w.Reason == monitor.ReasonPlan && strings.EqualFold(w.Plan, b.Slug) {
				return true
			}
		}
		if b.RegisteredInSession != "" && (row.SessionID == b.RegisteredInSession || row.PrevSessionID == b.RegisteredInSession) && in.mon.Monitored(row) {
			return true
		}
	}
	return false
}

// spawnedByMonitored reports whether a monitored live session spawned an
// agent for the package: the lead that dispatched it, which may not be the
// one that registered the plan.
func spawnedByMonitored(in inboxInput, p api.Package) bool {
	for _, src := range p.Sources {
		if src.Kind != store.ProvisionalSpawn {
			continue
		}
		sid, _, ok := strings.Cut(src.Ref, "/")
		if !ok || sid == "" {
			continue
		}
		for _, row := range in.sessions {
			if !row.Headless && row.EndedAt.IsZero() && (row.SessionID == sid || row.PrevSessionID == sid) && in.mon.Monitored(row) {
				return true
			}
		}
	}
	return false
}

// planRowEpisode is a provisional package whose row is missing. Its
// detail is the suggested row, to paste into the plan file.
func planRowEpisode(b api.Plan, p api.Package, file string) inbox.Episode {
	var kinds []string
	ep := inbox.Episode{
		Source:   inbox.SourcePlan,
		Ref:      inbox.PlanRowRef(b.Slug, p.ID),
		Kind:     inbox.KindPlanRow,
		Since:    p.FirstSeen.Add(inbox.PlanRowAfter),
		Detail:   p.SuggestedRow,
		Sig:      inbox.KindPlanRow + "|" + strings.ToUpper(p.ID),
		Title:    p.ID + " isn't in " + file,
		PlanSlug: b.Slug,
		Pkg:      p.ID,
		Why:      "work names " + p.ID + ", but " + file + "'s tables have no row for it: add the row and it becomes the plan's package",
	}
	for _, src := range p.Sources {
		k := map[string]string{store.ProvisionalSpawn: "a spawn brief", store.ProvisionalBranch: "a branch", store.ProvisionalPR: "a PR"}[src.Kind]
		if k != "" && !slices.Contains(kinds, k) {
			kinds = append(kinds, k)
		}
		switch src.Kind {
		case store.ProvisionalBranch:
			if repo, branch, ok := strings.Cut(src.Ref, ":"); ok && ep.Branch == "" {
				ep.Repo, ep.Branch = repo, branch
			}
		case store.ProvisionalPR:
			if ep.PR == "" {
				ep.PR = src.Ref
			}
		}
	}
	sub := "Provisional package of " + cmp.Or(b.Title, b.Slug)
	if len(kinds) > 0 {
		sub += " · named by " + strings.Join(kinds, ", ")
	}
	ep.Subtitle = sub
	return ep
}

// inboxPRNeed is the PR rule the inbox follows: internal/prwait's (through
// monitor.Set.PRWait). A PR needs you when your review is
// requested on it, or when it is ready to merge and the merge is yours: it
// is then an item of kind review or merge. v is the verdict either way.
func inboxPRNeed(mon *monitor.Set, p store.PR) (kind string, v prwait.Verdict) {
	v = mon.PRWait(p)
	switch v.NeedsYou {
	case prwait.ReviewRequested:
		return inbox.KindReview, v
	case prwait.ReadyToMerge:
		return inbox.KindMerge, v
	}
	return "", v
}

// prStopped says why a PR doesn't need you.
func prStopped(p store.PR, v prwait.Verdict) string {
	switch {
	case p.State == store.PRMerged:
		return "merged on GitHub"
	case p.State != store.PROpen:
		return "closed on GitHub"
	case v.Reason != "":
		return "it no longer needs you: " + v.Reason
	}
	return "it no longer needs you"
}

// sessionEpisode is a monitored session's wait.
func sessionEpisode(in inboxInput, row store.Session) inbox.Episode {
	whys := in.mon.Why(row)
	reason := row.StateReason
	agent := inbox.AgentOf(reason)
	plan, pkg := waitPackage(whys, agent)
	if plan == "" {
		plan = monitor.PlanOf(whys)
	}
	title := monitor.Title(row)
	var sub []string
	if r := shortRepo(in.repoName(row.Repo), row); r != "" {
		sub = append(sub, r)
	}
	if agent != "" {
		sub = append(sub, "subagent of "+title)
		title = "Agent · " + cmp.Or(pkg, agent)
	} else {
		if row.Kind == string(sessions.Background) {
			sub = append(sub, "background job")
		}
		if !monitor.Has(whys, monitor.ReasonAll) {
			if why := monitor.Summary(whys); why != "" {
				sub = append(sub, why)
			}
		}
	}
	since := cmp.Or(row.NeedsYouSince, row.FirstSeenAt)
	ep := inbox.Episode{
		Source:     inbox.SourceSession,
		Ref:        inbox.SessionRef(row.ID),
		Kind:       inbox.SessionKind(reason),
		Since:      since,
		Detail:     needReason(inbox.Core(reason)),
		Sig:        inbox.Core(reason),
		Title:      title,
		Subtitle:   strings.Join(sub, " · "),
		PlanSlug:   plan,
		Pkg:        pkg,
		SessionKey: row.Key,
		JobID:      row.JobID,
		Repo:       cmp.Or(in.repoName(row.Repo), row.Repo),
		Branch:     row.Branch,
		Folder:     cmp.Or(row.WorktreePath, row.Cwd),
	}
	if !row.NeedsYouDismissedAt.IsZero() && !row.NeedsYouDismissedAt.Before(row.NeedsYouSince) {
		ep.DismissedAt = row.NeedsYouDismissedAt
	}
	return ep
}

// waitPackage is the plan package a session's wait belongs to: the
// package a board links to the waiting loop itself. A wait that carries a
// subagent's id (the mod's agentId on tool.check, as "agent <id>: …" in
// the reason) is that subagent's package, when a link names that agent; a
// wait without one is the session's own package, when a link names the
// session itself. A lead's own prompt is never filed under one of its
// subagents' packages: with no such link the wait has no package, and the
// session's plan (monitor.PlanOf) holds it.
func waitPackage(whys []monitor.Why, agent string) (plan, pkg string) {
	for _, w := range whys {
		if w.Reason != monitor.ReasonAgent || w.Pkg == "" || w.Lead != "" {
			continue
		}
		if (agent == "" && !w.Subagent) || (agent != "" && w.Subagent && w.Agent == agent) {
			return w.Plan, w.Pkg
		}
	}
	return "", ""
}

// prEpisode is a PR that needs you. A review request's wait counts from
// when aimd first saw the request; a merge's from the PR's last update
// when it became ready, as the old bar's did: GitHub gives no "since" for
// being mergeable.
func prEpisode(p store.PR, kind string, v prwait.Verdict) inbox.Episode {
	since := cmp.Or(p.UpdatedAt, p.FirstSeenAt)
	if kind == inbox.KindReview && p.ReviewRequested() {
		since = p.ReviewRequestedAt
	}
	ref := store.PRRef(p.Repo, p.Number)
	return inbox.Episode{
		Source:   inbox.SourcePR,
		Ref:      ref,
		Kind:     kind,
		Since:    since,
		Detail:   v.Reason,
		Sig:      strings.Join([]string{kind, p.HeadRefOID, p.ChecksState, p.ReviewDecision}, "|"),
		Title:    repoTail(p.Repo) + " #" + strconv.Itoa(p.Number) + " · " + p.Title,
		Subtitle: upperFirst(v.Label()) + " · " + ciText(p),
		PlanSlug: cmp.Or(v.Plan, p.PlanSlug),
		Pkg:      cmp.Or(v.Pkg, p.PlanPkg),
		Repo:     p.Repo,
		Branch:   p.HeadBranch,
		PR:       ref,
		PRURL:    p.URL,
		Why:      v.Reason,
	}
}

func (in inboxInput) repoName(path string) string {
	if path == "" {
		return ""
	}
	return in.repoNames[worktrees.PathKey(path)]
}

// shortRepo names where a session works in a word: its GitHub repo's
// name, else its repo's or cwd's folder.
func shortRepo(name string, row store.Session) string {
	switch {
	case name != "":
		return repoTail(name)
	case row.Repo != "":
		return filepath.Base(row.Repo)
	case row.Cwd != "":
		return filepath.Base(row.Cwd)
	}
	return ""
}

// repoTail is "pivot" for "acme/pivot".
func repoTail(name string) string {
	if _, tail, ok := strings.Cut(name, "/"); ok {
		return tail
	}
	return name
}

// ciText says a PR's CI state in a few words.
func ciText(p store.PR) string {
	switch p.ChecksState {
	case "":
		return "no CI checks"
	case "SUCCESS":
		return "CI green"
	case "FAILURE", "ERROR":
		return "CI failing"
	}
	return "CI " + strings.ToLower(p.ChecksState)
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
