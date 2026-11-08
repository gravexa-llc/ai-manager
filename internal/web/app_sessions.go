package web

// The Sessions page (GET /app/sessions).
//
//   - The list: the monitored sessions (sn.mon) grouped by the plan
//     their reasons put them under, in the order the plans were
//     registered, then "No plan · watched". In a group, the sessions that
//     need you come first (longest wait first), then the others in the
//     store's order (by start time), then expired background jobs. A
//     monitored session's live subagents (the mod's agent rows) follow it.
//     Each row has a state dot, its name, its role and state, and its
//     terminal tag (internal/term). Headless `claude -p` runs are left
//     out, as the menu's count leaves them out.
//   - "Not monitored", collapsed: the other sessions, each with Watch
//     (POST /app/sessions/watch), whose toast's Undo unwatches. In "all"
//     mode every session is monitored and the group isn't there.
//   - Every group folds (groups.go): the plans' groups start open, Not
//     monitored collapsed, and the one holding the selected session, or
//     every one under a search, opens.
//   - Expired background jobs: Claude Code deleted their conversation
//     (internal/sessions Jobs, Options.Jobs) but still lists the job. They
//     show "expired: …" and the `claude rm <id>` command instead of Watch
//     and Go; aimd never deletes them, and they never count as needing
//     you, even when monitored.
//   - Older plugin: a session that runs an older plugin build than the
//     folder it loaded it from holds now gets a tag in its row and its
//     detail, and the summary counts them (app_plugin.go).
//   - The detail pane (?s=<session key>, or <session key>/<agent id> for a
//     subagent): state, terminal tag, its agent line as a package's pane
//     draws it (agent_line.go: a subagent's own, a session's main loop's)
//     and, for a monitored session, a line per live subagent linking to
//     its detail and the finished ones folded (sessionAgents); why it's
//     monitored (monitor.Summary), plan, package, folder, branch, when it
//     started and plugin build; Go (Go's rules, the
//     reason shown when it can't), Stop monitoring for a watched session (POST
//     /app/sessions/unwatch, with Undo), and Watch for one that isn't
//     monitored.
//
// Go is POST /app/sessions/go (s.goTo), answered with a toast.

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/events"
	"github.com/gravexa-llc/ai-manager/internal/focus"
	"github.com/gravexa-llc/ai-manager/internal/monitor"
	"github.com/gravexa-llc/ai-manager/internal/sessions"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/term"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
	"github.com/gravexa-llc/ai-manager/internal/worktrees"
)

// appSessionsPath is the page's path.
const appSessionsPath = "/app/sessions"

var appSessions = &appPage{
	pattern:     appSessionsPath,
	name:        "sessions",
	title:       "Sessions",
	panes:       2,
	listLabel:   "Sessions",
	detailLabel: "Session",
	// Plans and worktrees decide which sessions are monitored (a plan's
	// lead and agents, lineage), and a session's plan group.
	topics: []string{"sessions", "plans", "worktrees"},
	tmpl:   parseApp("sessions.html"),
	fill:   fillAppSessions,
	routes: sessionsRoutes,
	keys:   []appKey{{"g", "Go: switch to its terminal tab"}},
}

// sessionCounts counts the live sessions, headless `claude -p` runs aside
// as the old Sessions page does: the monitored ones (internal/monitor's
// rules, sn.mon) and the rest. The Sessions menu badge is the monitored
// count. Expired
// background jobs count where the page lists them; subagents, which the
// page lists under their lead, aren't sessions and don't count.
func sessionCounts(sn *snapshot) (monitored, notMonitored int) {
	for _, row := range sn.sessions {
		switch {
		case row.Headless:
		case sn.mon.Monitored(row):
			monitored++
		default:
			notMonitored++
		}
	}
	return monitored, notMonitored
}

// ---- Expired background jobs ----

// ExpiredJobs tells which background jobs have expired: Claude Code
// deleted their conversation but still lists the job (*sessions.Jobs).
type ExpiredJobs interface {
	Expiry(jobID string, sessionIDs ...string) sessions.JobExpiry
}

// expiredJob asks Options.Jobs whether a session row is an expired
// background job. The snapshot (sn.expired), the Watch route and the
// Needs you inbox's sync pass (which opens no item for one) ask it.
func (s *Server) expiredJob(row store.Session) (sessions.JobExpiry, bool) {
	if s.opts.Jobs == nil || row.JobID == "" {
		return sessions.JobExpiry{}, false
	}
	e := s.opts.Jobs.Expiry(row.JobID, ids(row)...)
	return e, e.Expired
}

// isExpiredJob reports whether a session row is an expired background job.
func (s *Server) isExpiredJob(row store.Session) bool {
	_, ok := s.expiredJob(row)
	return ok
}

// expiredJobs returns the expired background jobs among rows, by session
// key.
func (s *Server) expiredJobs(rows []store.Session) map[string]sessions.JobExpiry {
	var out map[string]sessions.JobExpiry
	for _, row := range rows {
		if e, ok := s.expiredJob(row); ok {
			if out == nil {
				out = map[string]sessions.JobExpiry{}
			}
			out[row.Key] = e
		}
	}
	return out
}

// ids are the session ids a row answers to: its own and the one before its
// last /clear.
func ids(row store.Session) []string {
	var out []string
	for _, id := range []string{row.SessionID, row.PrevSessionID} {
		if id != "" && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// jobExpired reports whether a session row is an expired background job.
func (sn *snapshot) jobExpired(row store.Session) bool {
	_, ok := sn.expired[row.Key]
	return ok
}

// claudeRm is the command that removes a background job; aimd shows it and
// never runs it.
func claudeRm(jobID string) string { return "claude rm " + jobID }

// ---- The page ----

// appSessionsView is the Sessions page's data.
type appSessionsView struct {
	// Groups are the monitored sessions, by plan.
	Groups []sessItemGroup
	// Empty says why Groups is empty.
	Empty string
	// OptIn is true in "opt-in" mode, where Unmonitored lists the other
	// sessions; NotMon is their section, collapsed unless it holds the
	// selected row or a search.
	OptIn       bool
	Unmonitored []sessItem
	NotMon      listGroup
	// Detail is the selected session, or nil with DetailNote saying why.
	Detail     *sessDetail
	DetailNote string
}

// sessItemGroup is one plan's sessions (or "No plan · watched").
type sessItemGroup struct {
	listGroup
	Label string
	// URL is the plan's page in the app.
	URL  string
	Rows []sessItem
}

// sessItem is one row.
type sessItem struct {
	// Key is ?s= and the row's data-item: the session key, or for a
	// subagent the lead's key, "/", and the agent id.
	Key, Href string
	Title     string
	// Role is lead, agent, subagent, background job or interactive; State
	// the state in a few words, and Tone its color class (the dot).
	Role, State, Tone string
	Term              termTag
	// Sub marks a subagent's row, under its lead.
	Sub bool
	Sel bool
	// Chime is the row's data-chime key: a new permission prompt or
	// question in a monitored session rings.
	Chime string
	// Expired says why an expired background job is, and RmCmd is the
	// command that removes it.
	Expired, RmCmd string
	// OldPlugin, when set, is the Older plugin tag's hover text: the
	// session runs an older plugin build than its folder holds now
	// (app_plugin.go).
	OldPlugin string
	// Watch is a not-monitored row's Watch button.
	Watch watchButton
	// Search is more text the header's filter matches: the branch.
	Search string
}

// sessDetail is the detail pane.
type sessDetail struct {
	sessItem
	// Why says why it's monitored, or that it isn't.
	Why string
	// Waiting is what a session that needs you waits on, and WaitAge for
	// how long.
	Waiting, WaitAge string
	// Now is its agent line, as a package's pane draws it (agentLine): a
	// subagent's own, or a session's main loop's, with its last action as
	// the mod reported it. Nil for an expired job.
	Now *agentNow
	// Agents are a monitored session's live subagents, each line linking
	// to its detail, and Finished the latest of the FinishedN that have
	// ended (maxFinished), latest first, FinishedMore the ones left out
	// (sessionAgents).
	Agents                  []sessAgent
	Finished                []*agentNow
	FinishedN, FinishedMore int
	Plan, PlanURL           string
	Pkg                     string
	Folder, FolderFull      string
	Branch                  string
	// Started is when it started: its clock time, as the line says how
	// long it has run; with how long ago (startedText) when there's no
	// line.
	Started string
	// Plugin is the plugin build it loaded when it started, PluginFolder
	// where from, and PluginNote what to say when it reported none.
	// PluginOnDisk is the build in that folder now, set when it differs.
	Plugin, PluginFolder, PluginNote string
	PluginOnDisk                     string
	// Go is its Go button: Key to post, or Off with the reason.
	Go goButton
	// Unwatch offers Stop monitoring (a session you watch).
	Unwatch bool
}

// sessAgent is a live subagent on its lead's detail: its line, a link to
// its own detail.
type sessAgent struct {
	Href string
	Now  *agentNow
}

// startedText is when a session or agent started, at its clock time in
// the Time zone setting's zone and how long ago: "09:00 (3h ago)".
func startedText(now, t time.Time) string {
	return clock(now, t) + " (" + ago(now, t) + " ago)"
}

// noPlanAll heads every session with no plan in "all" mode, where nobody
// has to watch them.
const noPlanAll = "No plan"

func fillAppSessions(s *Server, r *http.Request, sn *snapshot, d *appData) error {
	mon, not := sessionCounts(sn)
	d.Summary = strconv.Itoa(mon) + " monitored · " + strconv.Itoa(not) + " not monitored"
	expired := 0
	for _, row := range sn.sessions {
		if !row.Headless && sn.jobExpired(row) {
			expired++
		}
	}
	if expired > 0 {
		d.Summary += " · " + strconv.Itoa(expired) + " expired"
	}
	if n := s.olderPlugins(r.Context(), sn); n > 0 {
		d.Summary += " · " + strconv.Itoa(n) + " on an older plugin"
	}
	if d.Fragment == "summary" || d.Fragment == "menu" {
		return nil
	}
	q := r.URL.Query()
	b := sessBuilder{ctx: r.Context(), s: s, sn: sn, query: strings.TrimSpace(d.Query), sel: q.Get("s")}
	b.agents = s.liveAgents(r.Context(), sn)
	v := &appSessionsView{OptIn: !sn.mon.All()}
	if d.Fragment != "detail" {
		b.list(v)
	}
	if d.Fragment != "list" {
		v.Detail, v.DetailNote = b.detail()
	}
	d.View = v
	return nil
}

// sessBuilder builds the page's rows and detail.
type sessBuilder struct {
	ctx    context.Context
	s      *Server
	sn     *snapshot
	query  string
	sel    string
	agents map[string][]store.Agent
}

// list fills the groups and the Not monitored rows.
func (b sessBuilder) list(v *appSessionsView) {
	sn := b.sn
	type entry struct {
		row   store.Session
		item  sessItem
		rank  int // 0 needs you, 1 the rest, 2 expired
		since time.Time
	}
	groups := map[string][]entry{}
	var order []string
	for _, row := range sn.sessions {
		if row.Headless {
			continue
		}
		it := b.item(row)
		agents := b.agentItems(row)
		if !b.matches(row, it) && !slices.ContainsFunc(agents, func(a sessItem) bool { return b.matchesText(a.Title, a.Role) }) {
			continue
		}
		if !sn.mon.Monitored(row) {
			if it.Sel {
				v.NotMon.Force = true
			}
			v.Unmonitored = append(v.Unmonitored, it)
			continue
		}
		k := strings.ToLower(monitor.PlanOf(sn.mon.Why(row)))
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		e := entry{row: row, item: it, rank: 1}
		switch {
		case it.Expired != "":
			e.rank = 2
		case row.State == needsYou:
			e.rank, e.since = 0, row.NeedsYouSince
		}
		groups[k] = append(groups[k], e)
		// A lead's subagents ride along under it.
		for _, a := range agents {
			groups[k] = append(groups[k], entry{item: a, rank: -1})
		}
	}
	rank := func(k string) int {
		if k == "" {
			return len(sn.boards) + 1
		}
		if i := slices.IndexFunc(sn.boards, func(p api.Plan) bool { return strings.EqualFold(p.Slug, k) }); i >= 0 {
			return i
		}
		return len(sn.boards)
	}
	slices.SortStableFunc(order, func(a, c string) int { return cmp.Or(cmp.Compare(rank(a), rank(c)), cmp.Compare(a, c)) })
	for _, k := range order {
		g := sessItemGroup{listGroup: listGroup{Key: watchedKey, Open: true, Force: b.query != ""}, Label: noPlanLabel}
		if sn.mon.All() {
			g.Label = noPlanAll
		}
		if k != "" {
			g.Key = planKey(k)
			g.Label = k
			if p := sn.board(k); p != nil {
				g.Label, g.URL = cmp.Or(p.Title, p.Slug), appPlanURL(p.Slug)
			} else {
				g.Label = monitor.PlanOf(sn.mon.Why(groups[k][0].row))
			}
		}
		// Sort the sessions, keeping each one's subagents after it.
		var blocks [][]entry
		for _, e := range groups[k] {
			if e.rank < 0 && len(blocks) > 0 {
				blocks[len(blocks)-1] = append(blocks[len(blocks)-1], e)
				continue
			}
			blocks = append(blocks, []entry{e})
		}
		slices.SortStableFunc(blocks, func(a, c []entry) int {
			if a[0].rank != c[0].rank {
				return cmp.Compare(a[0].rank, c[0].rank)
			}
			if a[0].rank == 0 {
				return byWait(a[0].since, c[0].since)
			}
			return 0
		})
		for _, bl := range blocks {
			for _, e := range bl {
				g.Rows = append(g.Rows, e.item)
				g.Force = g.Force || e.item.Sel
			}
		}
		v.Groups = append(v.Groups, g)
	}
	// A search shows the matching sessions that aren't monitored too.
	v.NotMon.Key = "not-monitored"
	v.NotMon.Force = v.NotMon.Force || b.query != ""
	switch {
	case len(v.Groups) > 0:
	case b.query != "" && len(v.Unmonitored) > 0:
		v.Empty = "No monitored session matches “" + b.query + "”."
	case b.query != "":
		v.Empty = "No session matches “" + b.query + "”."
	case len(v.Unmonitored) > 0:
		v.Empty = "No monitored sessions. Run /aim watch in a session, or Watch one under Not monitored."
	default:
		v.Empty = "No live sessions."
	}
}

// matches reports whether a session row is shown for the search box.
func (b sessBuilder) matches(row store.Session, it sessItem) bool {
	return b.matchesText(it.Title, it.Role, it.State, row.Key, row.JobID, row.Cwd, row.Branch,
		monitor.Summary(b.sn.mon.Why(row)), monitor.PlanOf(b.sn.mon.Why(row)))
}

// matchesText reports whether any of texts holds the search, ignoring
// case; anything matches an empty search.
func (b sessBuilder) matchesText(texts ...string) bool {
	if b.query == "" {
		return true
	}
	q := strings.ToLower(b.query)
	return slices.ContainsFunc(texts, func(t string) bool { return strings.Contains(strings.ToLower(t), q) })
}

// href is a row's link: this page with ?s=key, keeping the search.
func (b sessBuilder) href(key string) string {
	v := url.Values{"s": {key}}
	if b.query != "" {
		v.Set("q", b.query)
	}
	return appSessionsPath + "?" + v.Encode()
}

// item is a session's row.
func (b sessBuilder) item(row store.Session) sessItem {
	sn := b.sn
	it := sessItem{
		Key:    row.Key,
		Href:   b.href(row.Key),
		Title:  sessionTitle(row),
		Role:   sessionRole(row, sn.mon.Why(row)),
		Term:   termTagOf(row, appOf(sn.terms, row)),
		Sel:    b.sel == row.Key,
		Search: row.Branch,
	}
	it.State, it.Tone = sessionState(row)
	if e, ok := sn.expired[row.Key]; ok {
		it.State, it.Tone = "Expired", "lc-dropped"
		it.Expired, it.RmCmd = e.Text(), claudeRm(row.JobID)
		return it
	}
	if onDisk, old := b.s.pluginOf(b.ctx, row); old {
		it.OldPlugin = oldPluginTitle(row.PluginBuild, onDisk)
	}
	if !sn.mon.Monitored(row) {
		it.Watch = watchButtonOf(sn.mon, row)
		return it
	}
	if row.State == needsYou {
		if tone, _ := needKind(row.StateReason); tone == "kind-permission" || tone == "kind-question" {
			it.Chime = "s:" + row.Key + ":" + strconv.FormatInt(row.NeedsYouSince.Unix(), 10)
		}
	}
	return it
}

// sessionRole names what a session is: a plan's lead (it registered the
// plan, or runs a package's agent as a subagent), the agent of a package
// or of a lead, a background job, or an interactive session.
func sessionRole(row store.Session, ws []monitor.Why) string {
	if row.Kind == focus.KindBackground {
		return "Background job"
	}
	kind := "Interactive"
	switch {
	case row.Headless:
		kind = "Headless run"
	case row.Kind != "" && row.Kind != focus.KindInteractive:
		kind = upperFirst(row.Kind)
	}
	if slices.ContainsFunc(ws, func(w monitor.Why) bool {
		return w.Reason == monitor.ReasonPlan || w.Reason == monitor.ReasonAgent && w.Subagent
	}) {
		return "Lead · " + strings.ToLower(kind)
	}
	for _, w := range ws {
		switch {
		case w.Reason != monitor.ReasonAgent:
		case w.Lead != "":
			return "Agent of " + cmp.Or(w.LeadTitle, w.Lead)
		case w.Pkg != "":
			return "Agent of " + w.Pkg
		}
	}
	return kind
}

// sessionState is a session's state in a few words, and its color class.
func sessionState(row store.Session) (label, tone string) {
	switch row.State {
	case needsYou:
		tone, what := needKind(row.StateReason)
		return "Needs you · " + what, tone
	case string(sessions.StateWorking):
		return "Working", "lc-working"
	case "quiet":
		return "Quiet", "tone-quiet"
	case string(sessions.StateTurnDone):
		return "Turn done", "tone-quiet"
	case string(sessions.StateIdle):
		return "Idle", "tone-quiet"
	case string(sessions.StateError):
		return "Error", "tone-warn"
	case string(sessions.StateGone):
		return "Gone", "lc-dropped"
	}
	return upperFirst(stateLabel(row.State)), "tone-quiet"
}

// needKind reads what a session that needs you waits on from its state
// reason (the poller's waitingFor, or the mod's "permission prompt: …",
// "agent a1: question: …"): its item-kind color and a word or two.
func needKind(reason string) (tone, what string) {
	r := strings.ToLower(reason)
	if rest, ok := strings.CutPrefix(r, "agent "); ok {
		if _, after, ok := strings.Cut(rest, ": "); ok {
			r = after
		}
	}
	switch {
	case reason == sessions.ReasonReply:
		return "kind-reply", "reply"
	case strings.HasPrefix(r, sessions.WaitingPermission), strings.HasPrefix(r, sessions.WaitingSandbox):
		return "kind-permission", "permission"
	case strings.HasPrefix(r, "question"), strings.HasPrefix(r, sessions.WaitingInput), strings.HasPrefix(r, sessions.WaitingDialog):
		return "kind-question", "question"
	case strings.HasPrefix(r, "plan approval"), strings.HasPrefix(r, sessions.WaitingGoal):
		return "kind-plan", "plan approval"
	}
	return "kind-reply", "waiting"
}

// ---- Subagents ----

// liveAgents are the live subagents the mod reported, by session id. They
// show under their lead when it is monitored. An error is logged: the
// page shows the sessions without them.
func (s *Server) liveAgents(ctx context.Context, sn *snapshot) map[string][]store.Agent {
	if len(sn.sessions) == 0 {
		return nil
	}
	agents, err := s.opts.Store.ListAgents(ctx, "")
	if err != nil {
		s.log.Error("sessions page: list agents", "err", err)
		return nil
	}
	var out map[string][]store.Agent
	for _, a := range agents {
		if !liveSubagent(a) {
			continue
		}
		if out == nil {
			out = map[string][]store.Agent{}
		}
		out[a.SessionID] = append(out[a.SessionID], a)
	}
	return out
}

// subagent reports whether an agent row is a subagent's (or a teammate's),
// not a session's main loop.
func subagent(a store.Agent) bool { return a.AgentID != "" && a.Kind != store.AgentLead }

// liveSubagent reports whether an agent row is a subagent's that hasn't
// ended.
func liveSubagent(a store.Agent) bool {
	return subagent(a) && a.EndedAt.IsZero() && !events.AgentEnded(a.State)
}

// subagentWho is who a subagent is in its line: its type, as the package
// pane's agent link names it (agentLabel), or its id.
func subagentWho(a store.Agent) string {
	if a.AgentType != "" {
		return a.AgentType
	}
	return "agent " + shortID(a.AgentID)
}

// mainLoopWho is who a session's main loop is in its line: the session's
// own loop, as against its subagents.
const mainLoopWho = "main"

// agentsOf are a live session's live subagents.
func (b sessBuilder) agentsOf(lead store.Session) []store.Agent {
	var out []store.Agent
	for _, id := range ids(lead) {
		out = append(out, b.agents[id]...)
	}
	return out
}

// agentItems are a monitored lead's subagent rows.
func (b sessBuilder) agentItems(lead store.Session) []sessItem {
	if !b.sn.mon.Monitored(lead) || b.sn.jobExpired(lead) {
		return nil
	}
	var out []sessItem
	for _, a := range b.agentsOf(lead) {
		out = append(out, b.agentItem(lead, a))
	}
	return out
}

// agentItem is a subagent's row: in its lead's terminal tab, which Go
// goes to.
func (b sessBuilder) agentItem(lead store.Session, a store.Agent) sessItem {
	key := lead.Key + "/" + a.AgentID
	it := sessItem{
		Key:  key,
		Href: b.href(key),
		Role: "Subagent of " + sessionTitle(lead),
		Term: termTagOf(lead, appOf(b.sn.terms, lead)),
		Sub:  true,
		Sel:  b.sel == key,
	}
	if app := appOf(b.sn.terms, lead); app.Switchable() {
		it.Term.Label += " · lead's tab"
		it.Term.Title += " (its lead's)"
	}
	_, pkg, title := b.sn.agentPackage(a.SessionID, a.AgentID)
	switch {
	case pkg != "":
		it.Title = strings.TrimSpace("Agent · " + pkg + " " + title)
	case a.AgentType != "":
		it.Title = "Agent · " + a.AgentType
	default:
		it.Title = "Agent " + a.AgentID
	}
	switch a.State {
	case events.AgentRunning:
		it.State, it.Tone = "Working", "lc-working"
	case events.AgentPending:
		it.State, it.Tone = "Starting", "lc-working"
	case events.AgentWaiting:
		it.State, it.Tone = "Needs you", "kind-reply"
		// The lead's row says what the agent asks: "agent a1: question: …".
		if lead.State == needsYou && strings.HasPrefix(lead.StateReason, "agent "+a.AgentID+": ") {
			tone, what := needKind(lead.StateReason)
			it.State, it.Tone = "Needs you · "+what, tone
		}
	case events.AgentIdle:
		it.State, it.Tone = "Idle", "tone-quiet"
	default:
		it.State, it.Tone = upperFirst(stateLabel(a.State)), "tone-quiet"
	}
	return it
}

// agentPackage finds the package a board links a subagent to: the plan's
// slug, the package's ID and title. Boards are loaded in "opt-in" mode
// only.
func (sn *snapshot) agentPackage(sessionID, agentID string) (plan, pkg, title string) {
	ref := sessionID + "/" + agentID
	for _, p := range sn.boards {
		for _, pk := range p.Packages {
			for _, l := range pk.Links {
				if l.Kind != store.LinkAgent {
					continue
				}
				if a := l.Agent; (a != nil && a.SessionID == sessionID && a.AgentID == agentID) || l.Ref == ref {
					return p.Slug, pk.ID, pk.Title
				}
			}
		}
	}
	return "", "", ""
}

// ---- The detail pane ----

// detail is the selected session's or subagent's detail, or nil and why.
func (b sessBuilder) detail() (*sessDetail, string) {
	if b.sel == "" {
		return nil, ""
	}
	sn := b.sn
	if row, ok := liveSessionByKey(sn.sessions, b.sel); ok {
		return b.sessionDetail(row), ""
	}
	// A subagent: <lead key>/<agent id>.
	if i := strings.LastIndex(b.sel, "/"); i > 0 {
		if lead, ok := liveSessionByKey(sn.sessions, b.sel[:i]); ok {
			for _, a := range b.agentsOf(lead) {
				if a.AgentID == b.sel[i+1:] {
					return b.agentDetail(lead, a), ""
				}
			}
			return nil, "That subagent has finished."
		}
	}
	return nil, "That session has ended: aimd lists live sessions only."
}

// liveSessionByKey is the live session row with that key.
func liveSessionByKey(rows []store.Session, key string) (store.Session, bool) {
	i := slices.IndexFunc(rows, func(r store.Session) bool { return r.Key == key })
	if i < 0 {
		return store.Session{}, false
	}
	return rows[i], true
}

func (b sessBuilder) sessionDetail(row store.Session) *sessDetail {
	sn := b.sn
	ws := sn.mon.Why(row)
	app := appOf(sn.terms, row)
	d := &sessDetail{
		sessItem:   b.item(row),
		Folder:     b.s.displayPath(cmp.Or(row.Cwd, row.WorktreePath, row.Repo)),
		FolderFull: cmp.Or(row.Cwd, row.WorktreePath, row.Repo),
		Branch:     row.Branch,
		Go:         b.s.goButtonOf(row, app),
		Plugin:     row.PluginBuild,
		PluginNote: pluginNotReported(row),
	}
	d.Sel = true
	if row.PluginRoot != "" {
		d.PluginFolder = b.s.displayPath(row.PluginRoot)
	}
	if onDisk, old := b.s.pluginOf(b.ctx, row); old && d.Expired == "" {
		d.PluginOnDisk = onDisk
	}
	if d.Expired == "" {
		b.sessionAgents(row, d)
	}
	switch {
	case row.StartedAt.IsZero():
	case d.Now != nil:
		d.Started = clock(sn.now, row.StartedAt)
	default:
		d.Started = startedText(sn.now, row.StartedAt)
	}
	if row.State == needsYou && d.Expired == "" {
		d.Waiting = upperFirst(needReason(row.StateReason))
		if a := ago(sn.now, row.NeedsYouSince); a != "" {
			d.WaitAge = a
		}
	}

	switch {
	case !sn.mon.Monitored(row):
		d.Why = "Not monitored: it is left out of Needs you and notifications. Watch it, or run /aim watch in it."
	case monitor.Has(ws, monitor.ReasonAll) || sn.mon.Mode() == monitor.ModeAll && len(ws) == 0:
		d.Why = `sessions.monitor is "all": aimd monitors every session.`
	default:
		d.Why = upperFirst(monitor.Summary(ws)) + "."
	}
	if slug := monitor.PlanOf(ws); slug != "" {
		d.Plan = slug
		if p := sn.board(slug); p != nil {
			d.Plan, d.PlanURL = cmp.Or(p.Title, p.Slug), appPlanURL(p.Slug)
		}
	}
	var pkgs []string
	for _, w := range ws {
		if w.Pkg == "" {
			continue
		}
		label := w.Pkg
		if p := sn.board(w.Plan); p != nil {
			if i := slices.IndexFunc(p.Packages, func(pk api.Package) bool { return strings.EqualFold(pk.ID, w.Pkg) }); i >= 0 && p.Packages[i].Title != "" {
				label += " " + p.Packages[i].Title
			}
		}
		if !slices.Contains(pkgs, label) {
			pkgs = append(pkgs, label)
		}
	}
	d.Pkg = strings.Join(pkgs, ", ")

	wb := watchButtonOf(sn.mon, row)
	d.Unwatch = wb.Unwatch
	if d.Expired != "" {
		d.Go = goButton{}
		if b.s.opts.Go != nil {
			d.Go.Off = "Its conversation was deleted, so there's nothing to go to: `claude attach` would open a blank tab."
		}
	}
	return d
}

func (b sessBuilder) agentDetail(lead store.Session, a store.Agent) *sessDetail {
	sn := b.sn
	it := b.agentItem(lead, a)
	it.Sel = true
	d := &sessDetail{
		sessItem: it,
		Go:       b.s.goButtonOfSubagent(lead, a, appOf(sn.terms, lead)),
		Now:      agentLine(sn.now, subagentWho(a), a.LastAction, a.StartedAt, a.Tokens),
	}
	if !a.StartedAt.IsZero() {
		d.Started = clock(sn.now, a.StartedAt)
	}
	plan, pkg, title := sn.agentPackage(a.SessionID, a.AgentID)
	if pkg != "" {
		d.Why = "Agent of " + pkg + " on a registered plan; it runs inside " + sessionTitle(lead) + "."
		d.Pkg = strings.TrimSpace(pkg + " " + title)
		d.Plan = plan
		if p := sn.board(plan); p != nil {
			d.Plan, d.PlanURL = cmp.Or(p.Title, p.Slug), appPlanURL(p.Slug)
		}
	} else {
		d.Why = "It runs inside " + sessionTitle(lead) + ", a monitored session."
		if slug := monitor.PlanOf(sn.mon.Why(lead)); slug != "" {
			d.Plan = slug
			if p := sn.board(slug); p != nil {
				d.Plan, d.PlanURL = cmp.Or(p.Title, p.Slug), appPlanURL(p.Slug)
			}
		}
	}
	d.FolderFull = cmp.Or(a.WorktreePath, lead.Cwd)
	d.Folder = b.s.displayPath(d.FolderFull)
	d.Branch = lead.Branch
	if a.WorktreePath != "" {
		d.Branch = ""
		for _, w := range sn.worktrees {
			if worktrees.SamePath(w.Path, a.WorktreePath) {
				d.Branch = w.Branch
				break
			}
		}
	}
	return d
}

// maxFinished is how many finished subagents a lead's detail lists.
const maxFinished = 20

// sessionAgents fills a live session's agent lines: its main loop's (with
// the main loop's token count, from its agent row), and for a monitored
// session, whose subagents the list shows under it, one per live subagent
// (as the list has them) and the latest finished ones. The rows come from
// the store, for the selected session only; an error is logged and the
// lines go without tokens or the finished ones.
func (b sessBuilder) sessionAgents(row store.Session, d *sessDetail) {
	sn := b.sn
	var mainLoop store.Agent
	var ended []store.Agent
	for _, id := range ids(row) {
		rows, err := b.s.opts.Store.ListAgents(b.ctx, id)
		if err != nil {
			b.s.log.Error("sessions page: list a session's agents", "err", err)
			break
		}
		for _, a := range rows {
			switch {
			case a.AgentID == "" && id == row.SessionID:
				mainLoop = a
			case subagent(a) && !liveSubagent(a):
				ended = append(ended, a)
			}
		}
	}
	d.Now = agentLine(sn.now, mainLoopWho, row.LastAction, row.StartedAt, mainLoop.Tokens)
	if !sn.mon.Monitored(row) {
		return
	}
	for _, a := range b.agentsOf(row) {
		d.Agents = append(d.Agents, sessAgent{Href: b.href(row.Key + "/" + a.AgentID),
			Now: agentLine(sn.now, subagentWho(a), a.LastAction, a.StartedAt, a.Tokens)})
	}
	// Latest first: by when it ended, else when it started.
	slices.SortStableFunc(ended, func(x, y store.Agent) int {
		return cmp.Or(y.EndedAt, y.StartedAt).Compare(cmp.Or(x.EndedAt, x.StartedAt))
	})
	d.FinishedN = len(ended)
	for _, a := range ended[:min(len(ended), maxFinished)] {
		d.Finished = append(d.Finished, endedLine(sn.now, subagentWho(a), a.State, a.StartedAt, a.EndedAt, a.Tokens))
	}
	d.FinishedMore = d.FinishedN - len(d.Finished)
}

// goButtonOfSubagent is a subagent's Go button: Go takes its key
// (<session id>/<agent id>) to its lead's tab, so the lead's rules apply.
func (s *Server) goButtonOfSubagent(lead store.Session, a store.Agent, app term.App) goButton {
	if s.opts.Go == nil {
		return goButton{}
	}
	if off := goOff(lead, app); off != "" {
		return goButton{Off: off}
	}
	return goButton{Key: a.SessionID + "/" + a.AgentID}
}

// ---- Forms: Watch, Stop monitoring, Go ----

func sessionsRoutes(s *Server) {
	s.mux.HandleFunc("POST /app/sessions/watch", func(w http.ResponseWriter, r *http.Request) { s.appWatch(w, r, true) })
	s.mux.HandleFunc("POST /app/sessions/unwatch", func(w http.ResponseWriter, r *http.Request) { s.appWatch(w, r, false) })
	s.mux.HandleFunc("POST /app/sessions/go", s.appSessionGo)
}

// sessionsLocation is where a Sessions form sends the browser: the page it
// came from (its return field, with its search), showing the session key.
func sessionsLocation(r *http.Request, key string) string {
	u, err := url.Parse(appReturn(r))
	if err != nil || u.Path != appSessionsPath {
		u = &url.URL{Path: appSessionsPath}
	}
	q := u.Query()
	if key != "" {
		q.Set("s", key)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// appWatch is POST /app/sessions/watch (on) and /app/sessions/unwatch:
// fields key (a live session's key), return, and undo (set on a toast's
// Undo, whose answer has no Undo of its own). It answers with a toast
// whose Undo does the opposite.
func (s *Server) appWatch(w http.ResponseWriter, r *http.Request, on bool) {
	key := formValue(r, "key")
	loc := sessionsLocation(r, key)
	if key == "" {
		s.appRefuse(w, r, http.StatusBadRequest, loc, "Which session? The form had no key.")
		return
	}
	rows, err := s.opts.Store.ListSessions(r.Context(), store.SessionFilter{})
	if err != nil {
		s.log.Error("app watch: list sessions", "err", err)
		s.appRefuse(w, r, http.StatusInternalServerError, loc, "aimd couldn't read the sessions; see aimd.log.")
		return
	}
	row, ok := liveSessionByKey(rows, key)
	switch {
	case !ok:
		s.appRefuse(w, r, http.StatusConflict, loc, "That session has ended.")
		return
	case row.SessionID == "":
		s.appRefuse(w, r, http.StatusConflict, loc, noSessionID)
		return
	}
	if e, expired := s.expiredJob(row); on && expired {
		s.appRefuse(w, r, http.StatusConflict, loc, "Not watched: this background job is "+e.Text()+". Remove it with `"+claudeRm(row.JobID)+"`.")
		return
	}
	res, err := s.setWatch(r.Context(), row.SessionID, on, sessionTitle(row))
	if err != nil {
		s.log.Error("app watch", "key", key, "on", on, "err", err)
		s.appRefuse(w, r, http.StatusInternalServerError, loc, "aimd couldn't save the watch; see aimd.log.")
		return
	}
	t := &appToast{Text: res.Message}
	if res.Changed && formValue(r, "undo") == "" {
		action := "/app/sessions/unwatch"
		if !on {
			action = "/app/sessions/watch"
		}
		t.Undo = &appUndo{Action: action, Fields: map[string]string{"key": key, "undo": "1", "return": loc}}
	}
	s.appReply(w, r, loc, t)
}

// appSessionGo is POST /app/sessions/go: fields key (a Go key: a session
// key, or <session id>/<agent id> for a subagent) and return. It brings
// the session's tab to the front and says so in a toast.
func (s *Server) appSessionGo(w http.ResponseWriter, r *http.Request) {
	loc := appSessionsPath
	if r.PostFormValue("return") != "" {
		loc = appReturn(r)
	}
	if s.opts.Go == nil {
		s.appRefuse(w, r, http.StatusNotFound, loc, "Go isn't available in this aimd.")
		return
	}
	res, err := s.goTo(r, formValue(r, "key"))
	if err != nil {
		status := http.StatusInternalServerError
		var refusal interface{ HTTPStatus() int }
		if errors.As(err, &refusal) {
			status = refusal.HTTPStatus()
		}
		s.appRefuse(w, r, status, loc, s.refusedText(r, "Go: ", err))
		return
	}
	s.appReply(w, r, loc, &appToast{Text: res.Message})
}
