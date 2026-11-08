package web

// The Overview page (GET /app/, the landing page). Four cards, each its
// own live region (a fragment of the page), so a change refreshes only the
// cards that show it:
//
//   - ov-plans: each registered plan's progress, as a bar with one segment
//     per package, "N of M done" and its Open inbox items. A plan opens its
//     page.
//   - ov-agents: the live monitored sessions (internal/monitor) and their
//     live subagents: plan, what each is doing and for how long, its
//     terminal tag, and Go under Go's rules (golink.go; POST
//     /app/overview/go, answered with the toast protocol).
//   - ov-activity: the last 24 hours, newest first, from what aimd stores
//     (activity below). It shows the newest 30; ?feed=all shows them all.
//   - ov-health: the cleanup counts, each linking its page, and the
//     health chips.

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/events"
	"github.com/gravexa-llc/ai-manager/internal/inbox"
	"github.com/gravexa-llc/ai-manager/internal/monitor"
	"github.com/gravexa-llc/ai-manager/internal/prwait"
	"github.com/gravexa-llc/ai-manager/internal/sessions"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

var appOverview = &appPage{
	pattern: "/app/",
	name:    "overview",
	title:   "Overview",
	panes:   1,
	// No page-wide topics: each card is its own live region with its own
	// (overview.html), and the header's summary doesn't change.
	fragments: []string{ovPlansID, ovAgentsID, ovActivityID, ovHealthID},
	tmpl:      parseApp("overview.html"),
	fill:      fillOverview,
	routes:    overviewRoutes,
}

// The cards' region ids.
const (
	ovPlansID    = "ov-plans"
	ovAgentsID   = "ov-agents"
	ovActivityID = "ov-activity"
	ovHealthID   = "ov-health"
)

// ---- View ----

// overviewView is the page's data. A card is nil when the region asked
// for doesn't show it.
type overviewView struct {
	Plans    *ovPlans
	Agents   *ovAgents
	Activity *ovActivity
	Health   *ovHealth
}

type ovPlans struct {
	Plans []ovPlan
	// Empty says why there are none.
	Empty string
}

type ovPlan struct {
	Title, URL  string
	Done, Total int
	// Segs are the bar's segments, one per package; Counts its legend;
	// BarLabel the bar for screen readers.
	Segs     []segView
	Counts   []countView
	BarLabel string
	// NeedsYou counts the plan's Open inbox items.
	NeedsYou int
}

type ovAgents struct {
	Rows []ovAgent
	// Empty is said when there are none; NotMonitored counts the live
	// sessions left out because they aren't monitored.
	Empty        string
	NotMonitored int
}

// ovAgent is a session or one of its subagents (Sub, listed under it).
type ovAgent struct {
	Name string
	Sub  bool
	// Plan is its plan and role ("ai-manager · lead"); Why says why it is
	// monitored, in a tooltip.
	Plan, Why string
	// Doing is its state, with the wait reason when it waits on you; Dot
	// the state dot's color class.
	Doing, Dot    string
	For, ForTitle string
	Term          termTag
	// Go is its Go button when ShowGo (Go is wired): enabled (Key) or
	// disabled with the reason (Off).
	ShowGo bool
	Go     goButton
}

type ovActivity struct {
	Entries []ovEntry
	// More counts the entries the cap left out (MoreURL shows them); All
	// is ?feed=all, with LessURL back to the newest only.
	More             int
	All              bool
	MoreURL, LessURL string
	// Note says a source couldn't be read.
	Note string
}

// ovEntry is one line of the activity feed: When (an age) and Stamp,
// What happened (bold), and Where, linked to URL when it has one.
type ovEntry struct {
	When, Stamp, Clock string
	What, Where        string
	URL                string
	// External links open in a new tab (a PR on GitHub).
	External bool
}

type ovHealth struct {
	Stats []ovStat
	Chips []ovChip
}

// ovStat is a cleanup count: N (a number, or "—" when unknown), what it
// counts, and the page it links.
type ovStat struct {
	N, Label, URL, Title string
}

// ovChip is a health chip; Tone is "ok", "warn" or "" (quiet).
type ovChip struct {
	Text, Tone, Title string
}

// ---- Fill ----

// fillOverview builds the cards the region asked for shows: all four for
// the page or its content, one for a card's fragment, none for the menu
// or the summary.
func fillOverview(s *Server, r *http.Request, sn *snapshot, d *appData) error {
	d.Summary = "Your plans, agents and repos at a glance"
	v := &overviewView{}
	d.View = v
	want := func(id string) bool { return d.Fragment == "" || d.Fragment == "content" || d.Fragment == id }
	o := &ovData{s: s, ctx: r.Context(), sn: sn}
	if want(ovPlansID) {
		v.Plans = o.plansCard()
	}
	if want(ovAgentsID) {
		v.Agents = o.agentsCard()
	}
	if want(ovActivityID) {
		act, err := o.activityCard(r.URL.Query().Get("feed") == "all")
		if err != nil {
			return err
		}
		v.Activity = act
	}
	if want(ovHealthID) {
		v.Health = o.healthCard()
	}
	return nil
}

// ovData is what one render reads, once: the plans' boards and what their
// links name.
type ovData struct {
	s   *Server
	ctx context.Context
	sn  *snapshot

	loaded bool
	boards []api.Plan
	// note says why there are no boards (no plan service, or it failed).
	note string
	// titles are the plans' titles and pkgs their packages' titles, by
	// lower-case slug (and package ID); refs the packages an agent link
	// ref names, prs the first package that links a PR (lower-case ref).
	titles map[string]string
	pkgs   map[string]string
	refs   map[string][]ovPkg
	prs    map[string]ovPkg
}

// ovPkg is a plan's package.
type ovPkg struct{ plan, pkg string }

func pkgKeyOf(plan, pkg string) string { return strings.ToLower(plan) + "\x00" + strings.ToUpper(pkg) }

// load reads the boards once.
func (o *ovData) load() {
	if o.loaded {
		return
	}
	o.loaded = true
	o.titles, o.pkgs, o.refs, o.prs = map[string]string{}, map[string]string{}, map[string][]ovPkg{}, map[string]ovPkg{}
	if o.s.opts.Plans == nil {
		o.note = "Plans aren't available: this aimd runs without its plan service."
		return
	}
	list, err := o.s.opts.Plans.Plans(o.ctx)
	if err != nil {
		o.s.log.Error("overview plans", "err", err)
		o.note = "aimd couldn't read the plans; see aimd.log."
		return
	}
	o.boards = list.Plans
	for _, b := range o.boards {
		o.titles[strings.ToLower(b.Slug)] = cmp.Or(b.Title, b.Slug)
		for _, p := range b.Packages {
			o.pkgs[pkgKeyOf(b.Slug, p.ID)] = p.Title
			for _, l := range p.Links {
				here := ovPkg{b.Slug, p.ID}
				switch l.Kind {
				case store.LinkAgent:
					for _, ref := range agentLinkRefs(l) {
						if !slices.Contains(o.refs[ref], here) {
							o.refs[ref] = append(o.refs[ref], here)
						}
					}
				case store.LinkPR:
					if _, ok := o.prs[strings.ToLower(l.Ref)]; !ok {
						o.prs[strings.ToLower(l.Ref)] = here
					}
				}
			}
		}
	}
}

// agentLinkRefs are the refs an agent link answers to: its own, and the
// one its agent's ids make.
func agentLinkRefs(l api.Link) []string {
	refs := []string{l.Ref}
	if a := l.Agent; a != nil {
		switch {
		case a.JobID != "":
			refs = append(refs, store.JobRef(a.JobID))
		case a.SessionID != "":
			refs = append(refs, store.AgentRef(a.SessionID, a.AgentID))
		}
	}
	return refs
}

// planTitle is a plan's title, else its slug.
func (o *ovData) planTitle(slug string) string {
	o.load()
	return cmp.Or(o.titles[strings.ToLower(slug)], slug)
}

// pkgLabel is "T1 Tile filters", or the ID alone when the board has no title.
func (o *ovData) pkgLabel(p ovPkg) string {
	o.load()
	if t := o.pkgs[pkgKeyOf(p.plan, p.pkg)]; t != "" {
		return p.pkg + " " + t
	}
	return p.pkg
}

// linked is the first package a board links to any of refs.
func (o *ovData) linked(refs ...string) (ovPkg, bool) {
	o.load()
	for _, ref := range refs {
		if ps := o.refs[ref]; len(ps) > 0 {
			return ps[0], true
		}
	}
	return ovPkg{}, false
}

// sessionRefs are the agent link refs a session row answers to: its
// background job, and its main loop under its id and the one before its
// last /clear. The first is the one a spawn link names it by.
func sessionRefs(row store.Session) []string {
	var refs []string
	if row.JobID != "" {
		refs = append(refs, store.JobRef(row.JobID))
	}
	for _, id := range []string{row.SessionID, row.PrevSessionID} {
		if id != "" {
			refs = append(refs, store.AgentRef(id, ""))
		}
	}
	return refs
}

// ---- Plan progress ----

func (o *ovData) plansCard() *ovPlans {
	o.load()
	v := &ovPlans{}
	switch {
	case o.note != "":
		v.Empty = o.note
		return v
	case len(o.boards) == 0:
		v.Empty = "No plans registered. Register one from the band when you save a plan in a lead session, or with /plan-register."
		return v
	}
	open, inboxOK := o.openByPlan()
	for _, b := range o.boards {
		p := ovPlan{Title: cmp.Or(b.Title, b.Slug), URL: appPlanURL(b.Slug), Total: cmp.Or(b.Total, len(b.Packages)), Done: b.Counts["done"]}
		p.Segs, p.Counts, p.BarLabel = progress(b)
		if p.BarLabel == "" {
			p.BarLabel = "no packages"
		}
		if inboxOK {
			p.NeedsYou = open[strings.ToLower(b.Slug)]
		} else if sum, err := o.s.opts.Plans.Summary(o.ctx, b.Slug, time.Time{}); err == nil {
			p.NeedsYou = sum.NeedsYou // no inbox: the menu's count
		} else {
			o.s.log.Error("overview plan summary", "plan", b.Slug, "err", err)
		}
		v.Plans = append(v.Plans, p)
	}
	return v
}

// openByPlan counts the inbox's Open items by plan (lower-case slug),
// after a sync pass, as the menu's Needs you badge does. ok is false
// without an inbox store, or when it can't be read.
func (o *ovData) openByPlan() (map[string]int, bool) {
	st, ok := o.s.inboxStore()
	if !ok {
		return nil, false
	}
	if _, err := o.s.SyncInbox(o.ctx); err != nil {
		o.s.log.Error("inbox sync", "err", err) // count what the store has
	}
	items, err := st.ListInbox(o.ctx, store.InboxFilter{State: inbox.Open})
	if err != nil {
		o.s.log.Error("overview inbox", "err", err)
		return nil, false
	}
	out := map[string]int{}
	for _, it := range items {
		if it.PlanSlug != "" {
			out[strings.ToLower(it.PlanSlug)]++
		}
	}
	return out, true
}

// ---- Agents running now ----

// agentsCard lists the live monitored sessions, headless runs and expired
// background jobs aside (internal/monitor's rules, sn.mon; sn.expired: their
// conversation is gone, and Cleanup & health counts them), each followed
// by its live subagents. The ones waiting on you come first, longest wait
// first, then the working ones, then the rest, each in the store's order
// (by start).
func (o *ovData) agentsCard() *ovAgents {
	sn := o.sn
	v := &ovAgents{}
	var rows []store.Session
	for _, row := range sn.sessions {
		switch {
		case row.Headless, sn.jobExpired(row):
		case sn.mon.Monitored(row):
			rows = append(rows, row)
		default:
			v.NotMonitored++
		}
	}
	slices.SortStableFunc(rows, func(a, b store.Session) int {
		if c := cmp.Compare(agentRank(a.State), agentRank(b.State)); c != 0 {
			return c
		}
		if a.State == needsYou {
			return byWait(a.NeedsYouSince, b.NeedsYouSince)
		}
		return 0
	})
	for _, row := range rows {
		v.Rows = append(v.Rows, o.sessionAgent(row))
		for _, a := range o.liveSubagents(row) {
			v.Rows = append(v.Rows, o.subagent(row, a))
		}
	}
	if len(v.Rows) == 0 {
		v.Empty = "No monitored session is running."
		if sn.mon.All() {
			v.Empty = "No session is running."
		}
	}
	return v
}

// agentRank orders sessions: waiting on you, working, the rest.
func agentRank(state string) int {
	switch state {
	case needsYou:
		return 0
	case string(sessions.StateWorking), "quiet":
		return 1
	}
	return 2
}

func (o *ovData) sessionAgent(row store.Session) ovAgent {
	sn := o.sn
	app := appOf(sn.terms, row)
	ws := sn.mon.Why(row)
	a := ovAgent{Name: sessionTitle(row), Term: termTagOf(row, app)}
	if !monitor.Has(ws, monitor.ReasonAll) {
		a.Why = monitor.Summary(ws)
	}
	plan := monitor.PlanOf(ws)
	if plan == "" {
		if p, ok := o.linked(sessionRefs(row)...); ok {
			plan = p.plan
		}
	}
	a.Plan = joinNonEmpty(" · ", o.planOrNone(plan), roleOf(ws))
	a.Doing, a.Dot = sessionDoing(row)
	if row.State == needsYou {
		a.For, a.ForTitle = ago(sn.now, row.NeedsYouSince), "waiting since "+stamp(row.NeedsYouSince)
	} else if start := cmp.Or(row.StartedAt, row.FirstSeenAt); !start.IsZero() {
		a.For, a.ForTitle = ago(sn.now, start), "running since "+stamp(start)
	}
	if o.s.opts.Go != nil {
		a.ShowGo, a.Go = true, o.s.goButtonOf(row, app)
	}
	return a
}

// planOrNone is a plan's title, or "No plan".
func (o *ovData) planOrNone(slug string) string {
	if slug == "" {
		return "No plan"
	}
	return o.planTitle(slug)
}

// roleOf says in a word or two why a session is monitored: lead, watched,
// agent of T1. "" in "all" mode, or for none.
func roleOf(ws []monitor.Why) string {
	if len(ws) == 0 {
		return ""
	}
	switch w := ws[0]; w.Reason {
	case monitor.ReasonPlan:
		return "lead"
	case monitor.ReasonWatch:
		return "watched"
	case monitor.ReasonAgent:
		return w.Text()
	}
	return ""
}

// sessionDoing is what a session is doing, and its dot's color class.
func sessionDoing(row store.Session) (text, dot string) {
	switch row.State {
	case needsYou:
		if row.StateReason == sessions.ReasonReply {
			return "waiting for your reply", inboxKindClass(inbox.KindReply)
		}
		text = "waiting on you: " + needReason(inbox.Core(row.StateReason))
		if inbox.AgentOf(row.StateReason) != "" {
			text += " (from its agent)"
		}
		return text, inboxKindClass(inbox.SessionKind(row.StateReason))
	case string(sessions.StateWorking), "quiet":
		return joinNonEmpty(": ", stateLabel(row.State), row.LastAction), "lc-working"
	case string(sessions.StateError), string(sessions.StateUnknown):
		return joinNonEmpty(": ", stateLabel(row.State), row.StateReason), "tone-warn"
	}
	return stateLabel(row.State), "tone-quiet"
}

// liveSubagents are a session's subagents that still run, by start.
func (o *ovData) liveSubagents(row store.Session) []store.Agent {
	if row.SessionID == "" {
		return nil
	}
	all, err := o.s.opts.Store.ListAgents(o.ctx, row.SessionID)
	if err != nil {
		o.s.log.Error("overview agents", "err", err)
		return nil
	}
	var out []store.Agent
	for _, a := range all {
		if a.AgentID != "" && a.EndedAt.IsZero() && !events.AgentEnded(a.State) {
			out = append(out, a)
		}
	}
	return out
}

// subagent is a lead's live subagent. It runs inside the lead, so its
// terminal and Go are the lead's: Go goes to the lead's tab.
func (o *ovData) subagent(lead store.Session, a store.Agent) ovAgent {
	sn := o.sn
	app := appOf(sn.terms, lead)
	ref := store.AgentRef(a.SessionID, a.AgentID)
	v := ovAgent{Sub: true, Term: termTagOf(lead, app)}
	v.Term.Label += " · lead's tab"
	v.Term.Title = "Runs inside its lead session, " + sessionTitle(lead) + ". " + v.Term.Title
	if p, ok := o.linked(ref); ok {
		v.Name = "Agent · " + o.pkgLabel(p)
		v.Plan = o.planTitle(p.plan) + " · agent of " + p.pkg
	} else {
		v.Name = "Agent · " + cmp.Or(a.AgentType, shortID(a.AgentID))
		lp := monitor.PlanOf(sn.mon.Why(lead))
		if lp == "" {
			if p, ok := o.linked(sessionRefs(lead)...); ok {
				lp = p.plan
			}
		}
		v.Plan = o.planOrNone(lp) + " · in " + sessionTitle(lead)
	}
	switch {
	case lead.State == needsYou && inbox.AgentOf(lead.StateReason) == a.AgentID:
		v.Doing = "waiting on you: " + needReason(inbox.Core(lead.StateReason))
		v.Dot = inboxKindClass(inbox.SessionKind(lead.StateReason))
		v.For, v.ForTitle = ago(sn.now, lead.NeedsYouSince), "waiting since "+stamp(lead.NeedsYouSince)
	default:
		v.Doing, v.Dot = agentDoing(a)
		if !a.StartedAt.IsZero() {
			v.For, v.ForTitle = ago(sn.now, a.StartedAt), "running since "+stamp(a.StartedAt)
		}
	}
	if o.s.opts.Go != nil {
		v.ShowGo = true
		if off := goOff(lead, app); off != "" {
			v.Go = goButton{Off: off}
		} else {
			v.Go = goButton{Key: ref}
		}
	}
	return v
}

// agentDoing is what a subagent is doing ($.agent.list() states).
func agentDoing(a store.Agent) (text, dot string) {
	switch a.State {
	case events.AgentRunning, "":
		return joinNonEmpty(": ", "working", a.LastAction), "lc-working"
	case events.AgentPending:
		return "starting", "lc-working"
	}
	return stateLabel(a.State), "tone-quiet"
}

// joinNonEmpty joins the parts that aren't empty.
func joinNonEmpty(sep string, parts ...string) string {
	return strings.Join(slices.DeleteFunc(parts, func(p string) bool { return p == "" }), sep)
}

// ---- Go ----

// overviewRoutes adds the page's Go form route.
func overviewRoutes(s *Server) {
	s.mux.HandleFunc("POST /app/overview/go", s.overviewGo)
}

// overviewGo is a Go button's form (fields key, csrf_token, return): it
// switches to the session's terminal tab, or a subagent's lead's, only
// where Go's rules enable Go, and answers with the toast protocol.
func (s *Server) overviewGo(w http.ResponseWriter, r *http.Request) {
	ret := appReturn(r)
	if s.opts.Go == nil {
		s.appRefuse(w, r, http.StatusNotFound, ret, "Go isn't available on this aimd.")
		return
	}
	key := formValue(r, "key")
	if key == "" {
		s.appRefuse(w, r, http.StatusBadRequest, ret, "Go where? The form names no session.")
		return
	}
	rows, err := s.opts.Store.ListSessions(r.Context(), store.SessionFilter{})
	if err != nil {
		s.log.Error("overview go", "err", err)
		s.appRefuse(w, r, http.StatusInternalServerError, ret, "aimd couldn't read its sessions; see aimd.log.")
		return
	}
	row, ok := goTargetRow(rows, key)
	if !ok {
		s.appRefuse(w, r, http.StatusConflict, ret, "That session isn't running any more.")
		return
	}
	if off := goOff(row, appOf(s.terminals(), row)); off != "" {
		s.appRefuse(w, r, http.StatusUnprocessableEntity, ret, off)
		return
	}
	res, err := s.goTo(r, key)
	if err != nil {
		var refusal interface{ HTTPStatus() int }
		if errors.As(err, &refusal) {
			s.appRefuse(w, r, refusal.HTTPStatus(), ret, "Go: "+err.Error())
			return
		}
		s.appRefuse(w, r, http.StatusInternalServerError, ret, "Go failed; see aimd.log.")
		return
	}
	s.appReply(w, r, ret, &appToast{Text: cmp.Or(res.Message, "Switched to "+cmp.Or(res.App, "its terminal"))})
}

// goTargetRow is the live session a Go key from this page names: a row's
// key, or a subagent's ref (<session id>/<agent id>), whose lead it is.
func goTargetRow(rows []store.Session, key string) (store.Session, bool) {
	for _, row := range rows {
		if row.Key == key {
			return row, true
		}
	}
	if sid, aid, ok := strings.Cut(key, "/"); ok && sid != "" && aid != "" {
		return liveSessionByID(rows, sid)
	}
	return store.Session{}, false
}

// ---- Activity, last 24 hours ----

// The feed's window and its caps: the newest activityCap entries, or with
// ?feed=all at most activityMax.
const (
	activityWindow = 24 * time.Hour
	activityCap    = 30
	activityMax    = 500
)

// activityStore is the part of *store.Store the feed reads besides the
// Store interface: the registered plans, their packages' stored lifecycle
// changes (lifecycle_history, migration 0004) and their links.
// Another Store leaves those sources out.
type activityStore interface {
	ListPlans(ctx context.Context, f store.PlanFilter) ([]store.Plan, error)
	ListLifecycleHistory(ctx context.Context, planID int64, f store.HistoryFilter) ([]store.LifecycleChange, error)
	ListLinks(ctx context.Context, planID int64) ([]store.Link, error)
}

var _ activityStore = (*store.Store)(nil)

// What kind of entry, for ties at the same time (lower first).
const (
	actPlan = iota
	actStarted
	actLink
	actOpened
	actDone
	actFinished
	actMerged
)

// activity is an entry being collected, with what sorts it.
type activity struct {
	at   time.Time
	kind int
	key  string
	ovEntry
}

// activityCard is the feed: what happened in the last 24 hours, newest
// first, from what aimd stores:
//
//   - PRs opened and merged (GitHub's createdAt and mergedAt): the ones on
//     a registered plan's board or a monitored session's branch, or with
//     your review requested (feedPR), named by the package that links it
//     or its Plan: line;
//   - plans registered (plans.created_at);
//   - packages done: a package's stored lifecycle turning done after the
//     plan's first pass (lifecycle_history), so registering a plan with
//     done packages isn't a burst;
//   - links made by hand (links with match method manual);
//   - agents started and finished: the mod's spawn links, and the
//     sessions and subagents aimd stores (sessions.started_at and
//     ended_at; agents.started_at and ended_at, which internal/live keeps
//     from the mod's events), the monitored ones only. Headless runs are
//     left out.
func (o *ovData) activityCard(all bool) (*ovActivity, error) {
	now := o.sn.now
	since := now.Add(-activityWindow)
	in := func(t time.Time) bool { return t.After(since) && !t.After(now) }
	var acts []activity
	byKey := map[string]int{} // key → index in acts, for the starts seen twice
	add := func(at time.Time, kind int, key string, e ovEntry) {
		if i, ok := byKey[key]; ok {
			if at.Before(acts[i].at) {
				acts[i].at = at
			}
			return
		}
		byKey[key] = len(acts)
		acts = append(acts, activity{at: at, kind: kind, key: key, ovEntry: e})
	}
	v := &ovActivity{All: all, MoreURL: "/app/?feed=all", LessURL: "/app/"}

	// PRs, as opt-in monitoring counts them.
	for _, p := range o.sn.prs {
		if !feedPR(o.sn.mon, p) {
			continue
		}
		ref := store.PRRef(p.Repo, p.Number)
		pkg, onPlan := o.prPkg(p)
		e := ovEntry{Where: ref + " · " + p.Title}
		if onPlan {
			e.Where = o.planTitle(pkg.plan) + " · " + e.Where
		}
		if githubURL(p.URL) {
			e.URL, e.External = p.URL, true
		}
		if at := p.OpenedAt(); in(at) {
			e.What = "PR opened"
			if onPlan {
				e.What = pkg.pkg + " PR opened"
			}
			add(at, actOpened, "opened\x00"+ref, e)
		}
		if p.State == store.PRMerged && in(p.MergedAt) {
			e.What = "PR merged"
			if onPlan {
				e.What = pkg.pkg + " merged"
			}
			add(p.MergedAt, actMerged, "merged\x00"+ref, e)
		}
	}

	// Plans, packages done, links by hand and spawn links.
	if hs, ok := o.s.opts.Store.(activityStore); ok {
		if err := o.planActivity(hs, in, add); err != nil {
			o.s.log.Error("overview activity", "err", err)
			v.Note = "aimd couldn't read the plans' history; see aimd.log."
		}
	}

	// Sessions and subagents.
	if err := o.agentActivity(in, add); err != nil {
		return nil, err
	}

	slices.SortStableFunc(acts, func(a, b activity) int {
		return cmp.Or(b.at.Compare(a.at), cmp.Compare(b.kind, a.kind), strings.Compare(a.key, b.key))
	})
	limit := activityCap
	if all {
		limit = activityMax
	}
	if len(acts) > limit {
		if !all {
			v.More = len(acts) - limit
		}
		acts = acts[:limit]
	}
	for _, a := range acts {
		e := a.ovEntry
		e.When, e.Stamp, e.Clock = when(now, a.at), a.at.UTC().Format(time.RFC3339), clock(now, a.at)
		v.Entries = append(v.Entries, e)
	}
	return v, nil
}

// when is how long ago an entry was: "now" within a minute, else ago.
func when(now, t time.Time) string {
	if now.Sub(t) < time.Minute {
		return "now"
	}
	return ago(now, t)
}

// feedPR reports whether a PR's activity is in the feed: monitoring counts
// it (a registered plan's board links it, or a monitored session works on
// its branch; every PR in "all" mode), or your review is requested on it
// (someone asked you by name, on any PR).
func feedPR(mon *monitor.Set, p store.PR) bool {
	return mon.PR(p) || mon.PRWait(p).NeedsYou == prwait.ReviewRequested
}

// prPkg is the package a PR is on: the first a board links it to, else
// its Plan: line's.
func (o *ovData) prPkg(p store.PR) (ovPkg, bool) {
	o.load()
	if pk, ok := o.prs[strings.ToLower(store.PRRef(p.Repo, p.Number))]; ok {
		return pk, true
	}
	if p.PlanSlug != "" && p.PlanPkg != "" {
		return ovPkg{p.PlanSlug, p.PlanPkg}, true
	}
	return ovPkg{}, false
}

// activityHistory is how many lifecycle changes a plan's feed reads, newest
// first: far more than a day has.
const activityHistory = 500

// planActivity adds what the plans' stored history says: plans registered,
// packages done, links made by hand, and agents spawned for a package.
func (o *ovData) planActivity(hs activityStore, in func(time.Time) bool, add func(time.Time, int, string, ovEntry)) error {
	plans, err := hs.ListPlans(o.ctx, store.PlanFilter{})
	if err != nil {
		return err
	}
	for _, p := range plans {
		title := o.storedPlanTitle(p)
		page := appPlanURL(p.Slug)
		if in(p.CreatedAt) {
			add(p.CreatedAt, actPlan, "plan\x00"+p.Slug, ovEntry{What: "Plan registered", Where: title, URL: page})
		}
		hist, err := hs.ListLifecycleHistory(o.ctx, p.ID, store.HistoryFilter{Limit: activityHistory})
		if err != nil {
			return err
		}
		for _, h := range hist {
			if h.Lifecycle != "done" || h.Prev == "" || h.Prev == "done" || !in(h.At) {
				continue
			}
			add(h.At, actDone, "done\x00"+strconv.FormatInt(h.ID, 10), ovEntry{
				What: h.PkgID + " done", Where: title + " · " + o.pkgLabel(ovPkg{p.Slug, h.PkgID}), URL: pkgURL(p.Slug, h.PkgID),
			})
		}
		links, err := hs.ListLinks(o.ctx, p.ID)
		if err != nil {
			return err
		}
		for _, l := range links {
			if !in(l.CreatedAt) {
				continue
			}
			switch {
			case l.MatchMethod == store.MatchManual:
				add(l.CreatedAt, actLink, "link\x00"+strconv.FormatInt(l.ID, 10), ovEntry{
					What: linkTargetLabel(l) + " linked to " + l.PkgID + " by hand", Where: title, URL: pkgURL(p.Slug, l.PkgID),
				})
			case l.MatchMethod == store.MatchSpawn && l.Kind == store.LinkAgent:
				add(l.CreatedAt, actStarted, "started\x00"+l.Ref, ovEntry{
					What: l.PkgID + " agent started", Where: title + " · " + o.pkgLabel(ovPkg{p.Slug, l.PkgID}), URL: pkgURL(p.Slug, l.PkgID),
				})
			}
		}
	}
	return nil
}

// storedPlanTitle is a stored plan's title: its board's, else the store's,
// else its slug.
func (o *ovData) storedPlanTitle(p store.Plan) string {
	o.load()
	return cmp.Or(o.titles[strings.ToLower(p.Slug)], p.Title, p.Slug)
}

// pkgURL is a package on its plan's page.
func pkgURL(slug, pkg string) string {
	return appPlanURL(slug) + "?" + url.Values{"pkg": {pkg}}.Encode()
}

// linkTargetLabel names what a manual link links: "o/app#7", "Branch
// feat", "Agent", "Worktree app-wt".
func linkTargetLabel(l store.Link) string {
	switch l.Kind {
	case store.LinkPR:
		return l.Ref
	case store.LinkBranch:
		if _, b, ok := strings.Cut(l.Ref, ":"); ok {
			return "Branch " + b
		}
		return "Branch"
	case store.LinkWorktree:
		return "Worktree " + filepath.Base(l.Ref)
	case store.LinkAgent:
		return "Agent"
	case store.LinkCloudSession:
		return "Cloud session"
	}
	return upperFirst(l.Kind)
}

// agentActivity adds the sessions and subagents that started or finished
// in the window. internal/monitor's rules decide which count, applied to
// the sessions still running and those that ended in the window, so an
// agent that has finished still shows.
func (o *ovData) agentActivity(in func(time.Time) bool, add func(time.Time, int, string, ovEntry)) error {
	rows, err := o.s.opts.Store.ListSessions(o.ctx, store.SessionFilter{IncludeEnded: true})
	if err != nil {
		return err
	}
	var window []store.Session
	for _, row := range rows {
		if !row.Headless && (row.EndedAt.IsZero() || in(row.EndedAt)) {
			window = append(window, row)
		}
	}
	set := o.s.monitorSet(o.ctx, window, o.sn.worktrees)
	bySID := map[string]store.Session{}
	for _, row := range window {
		if !set.Monitored(row) {
			continue
		}
		for _, id := range []string{row.SessionID, row.PrevSessionID} {
			if _, seen := bySID[id]; id != "" && !seen {
				bySID[id] = row
			}
		}
		who, where, page := o.sessionWho(row, set.Why(row))
		key := cmp.Or(firstOf(sessionRefs(row)), "row:"+strconv.FormatInt(row.ID, 10))
		if in(row.StartedAt) {
			add(row.StartedAt, actStarted, "started\x00"+key, ovEntry{What: who + " started", Where: where, URL: page})
		}
		if in(row.EndedAt) {
			add(row.EndedAt, actFinished, "finished\x00"+key, ovEntry{What: who + " finished", Where: where, URL: page})
		}
	}
	agents, err := o.s.opts.Store.ListAgents(o.ctx, "")
	if err != nil {
		return err
	}
	for _, a := range agents {
		lead, ok := bySID[a.SessionID]
		if a.AgentID == "" || !ok || !(in(a.StartedAt) || in(a.EndedAt)) {
			continue
		}
		ref := store.AgentRef(a.SessionID, a.AgentID)
		who, where, page := "Agent", "in "+sessionTitle(lead), ""
		if a.AgentType != "" {
			who = "Agent (" + a.AgentType + ")"
		}
		if p, ok := o.linked(ref); ok {
			who, where, page = p.pkg+" agent", o.planTitle(p.plan)+" · "+o.pkgLabel(p), pkgURL(p.plan, p.pkg)
		}
		if in(a.StartedAt) {
			add(a.StartedAt, actStarted, "started\x00"+ref, ovEntry{What: who + " started", Where: where, URL: page})
		}
		if in(a.EndedAt) {
			verb := " finished"
			switch a.State {
			case events.AgentFailed:
				verb = " failed"
			case events.AgentKilled:
				verb = " was stopped"
			}
			add(a.EndedAt, actFinished, "finished\x00"+ref, ovEntry{What: who + verb, Where: where, URL: page})
		}
	}
	return nil
}

// sessionWho names a session in the feed: the package agent a board links
// it to ("D1 agent"), else "Session <title>" or "Background job <title>";
// where is its plan and branch, and page its package or plan.
func (o *ovData) sessionWho(row store.Session, ws []monitor.Why) (who, where, page string) {
	if p, ok := o.linked(sessionRefs(row)...); ok {
		return p.pkg + " agent", o.planTitle(p.plan) + " · " + o.pkgLabel(p), pkgURL(p.plan, p.pkg)
	}
	who = "Session " + sessionTitle(row)
	if row.JobID != "" || row.Kind == "background" {
		who = "Background job " + sessionTitle(row)
	}
	plan := monitor.PlanOf(ws)
	if plan != "" {
		page = appPlanURL(plan)
	}
	return who, joinNonEmpty(" · ", o.planOrNone(plan), row.Branch), page
}

func firstOf(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

// ---- Cleanup & health ----

func (o *ovData) healthCard() *ovHealth {
	return &ovHealth{Stats: o.cleanupStats(), Chips: o.healthChips()}
}

// cleanupStats are the cleanup counts, each linking the page that shows
// them.
func (o *ovData) cleanupStats() []ovStat {
	s, sn := o.s, o.sn
	safe := ovStat{Label: "worktrees safe to remove", URL: "/app/worktrees", N: "—"}
	switch v := s.verdicts(o.ctx); {
	case !v.Wired:
		safe.Title = "This aimd runs without the safe-to-remove verdicts."
	case v.Pending:
		safe.Title = "aimd is still working out which worktrees are safe to remove."
	case v.Error != "":
		safe.Title = upperFirst(v.Error) + "."
	default:
		safe.N = strconv.Itoa(s.safeToRemoveCount(o.ctx, sn))
		safe.Title = "aim prune's rules: merged, clean, nothing local only"
	}
	gone, drafts := 0, 0
	for _, w := range sn.worktrees {
		if w.PrunableReason != "" {
			gone++
		}
	}
	for _, p := range sn.prs {
		if staleDraft(sn.now, p) {
			drafts++
		}
	}
	expired := ovStat{N: "—", Label: "expired background jobs", URL: "/app/sessions",
		Title: "This aimd doesn't check background jobs for a deleted conversation."}
	if n, known := s.expiredJobCount(sn); known {
		expired.N, expired.Title = strconv.Itoa(n), "Background jobs whose conversation Claude Code deleted: Sessions shows each one's claude rm"
	}
	return []ovStat{
		safe,
		{N: strconv.Itoa(gone), Label: "records whose folder is gone", URL: "/app/worktrees", Title: "git still lists them; git worktree prune clears them"},
		{N: strconv.Itoa(drafts), Label: "stale draft PRs", URL: "/app/prs", Title: "Open drafts with no update for 30 days"},
		expired,
	}
}

// expiredJobCount counts the live background jobs whose conversation
// Claude Code deleted (internal/sessions/expired.go through
// Options.Jobs, in sn.expired), headless runs aside, as the Sessions page
// counts them. known is false when this aimd doesn't check (no
// Options.Jobs), and the Overview shows "—".
func (s *Server) expiredJobCount(sn *snapshot) (n int, known bool) {
	if s.opts.Jobs == nil {
		return 0, false
	}
	for _, row := range sn.sessions {
		if !row.Headless && sn.jobExpired(row) {
			n++
		}
	}
	return n, true
}

// sessionPollLate is how old the last good session poll may be before the
// chip warns (the poller runs every 10 to 60 s).
const sessionPollLate = 5 * time.Minute

// healthChips: aimd's uptime, the last session poll, the GitHub budget,
// the plans and their parser warnings, and the plugin's reporting.
func (o *ovData) healthChips() []ovChip {
	st, now := o.sn.status, o.sn.now
	return []ovChip{uptimeChip(st, now), sessionPollChip(st, now), githubChip(st, now), o.plansChip(), pluginChip(st, now)}
}

// uptimeChip is how long aimd has run, with its version.
func uptimeChip(st api.Status, now time.Time) ovChip {
	c := ovChip{Text: "aimd running", Tone: "ok", Title: "version " + cmp.Or(st.Version, "unknown")}
	if !st.StartedAt.IsZero() {
		c.Text += " · " + ago(now, st.StartedAt)
		c.Title = "started " + stamp(st.StartedAt) + " · " + c.Title
	}
	return c
}

// sessionPollChip is when `claude agents` was last read, and whether that
// is failing or late.
func sessionPollChip(st api.Status, now time.Time) ovChip {
	src, ok := source(st, api.SourceSessions)
	switch {
	case !ok:
		return ovChip{Text: "Sessions: no poller", Title: "This aimd doesn't report its session poller."}
	case !src.FailingSince.IsZero():
		return ovChip{Text: "Session poll failing since " + clock(now, src.FailingSince), Tone: "warn", Title: src.Error}
	case src.LastOK.IsZero():
		return ovChip{Text: "Sessions not polled yet", Title: "aimd's first poll of claude agents hasn't finished."}
	case now.Sub(src.LastOK) > sessionPollLate:
		return ovChip{Text: "Sessions last polled " + ago(now, src.LastOK) + " ago", Tone: "warn", Title: "last good poll " + stamp(src.LastOK)}
	}
	return ovChip{Text: "Sessions polled " + ago(now, src.LastOK) + " ago", Tone: "ok", Title: "last good poll " + stamp(src.LastOK)}
}

// plansChip counts the plans and their parser warnings.
func (o *ovData) plansChip() ovChip {
	o.load()
	if o.note != "" {
		return ovChip{Text: "Plans unavailable", Title: o.note}
	}
	warnings := 0
	var with []string
	for _, b := range o.boards {
		if n := len(b.Warnings); n > 0 {
			warnings += n
			with = append(with, cmp.Or(b.Title, b.Slug)+": "+strconv.Itoa(n))
		}
	}
	c := ovChip{Text: counted(len(o.boards), "plan", "plans") + " · " + counted(warnings, "parser warning", "parser warnings"), Tone: "ok"}
	if warnings > 0 {
		c.Tone, c.Title = "warn", strings.Join(with, "; ")
	}
	return c
}

// githubChip is the GitHub budget: points left of the hour's, and whether
// the poller is holding back or failing.
func githubChip(st api.Status, now time.Time) ovChip {
	b := st.GitHub.Budget
	c := ovChip{Text: "GitHub budget unknown", Title: "GitHub hasn't reported a rate limit yet."}
	if b.Known {
		c = ovChip{Text: "GitHub " + thousands(b.Remaining) + " / " + thousands(b.Limit) + " points", Tone: "ok"}
		if !b.ResetAt.IsZero() {
			c.Title = "resets at " + clock(now, b.ResetAt)
		}
		switch b.Verdict {
		case "stop":
			c.Tone, c.Text = "warn", c.Text+" · stopped"
			if !b.ResumeAt.IsZero() {
				c.Text += " until " + clock(now, b.ResumeAt)
			}
		case "idle-only":
			c.Tone, c.Text = "warn", c.Text+" · idle repos only"
		}
	}
	if src, ok := source(st, api.SourceGitHub); ok && !src.FailingSince.IsZero() {
		c.Tone = "warn"
		c.Text += " · failing since " + clock(now, src.FailingSince)
		c.Title = src.Error
	}
	return c
}

// pluginChip says whether the plugin's mod reports to aimd (its
// reporting, POST /events), from the event receiver's counters: live while
// events arrive (the receiver's ModLive: a batch in the last 5 minutes),
// else waiting for them.
func pluginChip(st api.Status, now time.Time) ovChip {
	ev := st.Events
	if ev == nil {
		return ovChip{Text: "Plugin reporting: no receiver", Title: "This aimd doesn't take the mod's events."}
	}
	last := ev.LastBatchAt
	if ev.LastEventAt.After(last) {
		last = ev.LastEventAt
	}
	switch {
	case (ev.ModLive || st.ModLive) && !last.IsZero():
		return ovChip{Text: "Plugin reporting · live · last event " + ago(now, last) + " ago", Tone: "ok",
			Title: strconv.FormatInt(ev.Events, 10) + " events since aimd started · last " + stamp(last)}
	case !last.IsZero():
		return ovChip{Text: "Plugin reporting · waiting for events",
			Title: "Last event " + ago(now, last) + " ago (" + stamp(last) + "): no session has done anything for 5 minutes."}
	}
	return ovChip{Text: "Plugin reporting · waiting for events", Title: "No event has reached aimd since it started."}
}

// counted is "0 plans", "1 plan" or "3 plans" (plural, with zero).
func counted(n int, one, many string) string {
	return cmp.Or(plural(n, one, many), "0 "+many)
}

// thousands writes n with commas: 4,961.
func thousands(n int) string {
	s := strconv.Itoa(n)
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign, s = "-", s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return sign + b.String()
}
