package live

import (
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/events"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// Session states, the values internal/sessions and
// internal/state use, plus quiet, which only events can give.
const (
	StateNeedsYou = "needs_you"
	StateError    = "error"
	StateWorking  = "working"
	StateQuiet    = "quiet"
	StateTurnDone = "turn_done"
	StateIdle     = "idle"
	StateGone     = "gone"
)

// loop is one loop of a session: its main loop (id "") or a subagent.
type loop struct {
	id         string
	kind       string // store.AgentLead, AgentSubagent
	agentType  string
	model      string // kept from the row; events don't carry it
	background bool
	parent     string

	// named: something that names the session's agents named this one, so
	// it is an agent: a turn_start or agent_start in its loop, an
	// agent_list that lists it, a worktree_create of agent-<id>, or its
	// row in the store. The main loop always is. A loop known only from
	// its tool calls (or asks, errors, a turn's end) isn't: Claude Code's
	// own forks of an agent (compaction, memory) carry an agentId no
	// agent list names, in the agent's cwd, and a fork's tool attempt
	// fires tool.call. Such a loop is never live, working or waiting, and
	// gets no agent row; its events still go to the timeline.
	named bool

	// status is the agent status ($.agent.list() vocabulary) as the events
	// last set it, and statusAt when. A pending needs-you shows as
	// "waiting" on top of it (rowState).
	status   string
	statusAt time.Time

	// The turn: inTurn between turn_start and turn_complete; turnAt is the
	// time of the latest of the two, lastReason the latest
	// turn_complete's reason ("" before the first).
	inTurn     bool
	turnAt     time.Time
	lastReason string

	// errAt / errReason: the loop's turn failed (error event, or
	// turn_complete with reason error), until a later turn_start.
	errAt     time.Time
	errReason string

	// The pending needs-you, if needSince is set.
	needSince  time.Time
	needSource string
	needReason string
	needToolID string
	// needWait is what the ask waits on, when the mod said. Memory
	// only: it is never written to the store.
	needWait *events.Wait
	// progressAt is the latest time the loop showed it had moved on (a
	// tool finished, a turn started or ended). A needs-you older than it
	// is already answered.
	progressAt time.Time

	cwd string
	// worktree is the worktree the loop works in. fromCreate marks one
	// taken from worktree_create's name (agent-<id>), which the cwd
	// doesn't override.
	worktree   string
	fromCreate bool

	lastAction string
	// tokens is the loop's token count: a subagent's as agent_list last
	// reported it (events.Agent.Tokens), the main loop's as its last tool
	// or turn_complete did (events.Event.Tokens); 0 until one does, and
	// the main loop's again after /clear. tokensAt is when the main loop's
	// was sent (memory only).
	tokens    int64
	tokensAt  time.Time
	startedAt time.Time
	endedAt   time.Time

	dirty bool // the agent row needs writing
}

func (l *loop) live() bool { return l.named && l.endedAt.IsZero() && !events.AgentEnded(l.status) }

// mainTokens takes the main loop's token count from one of its tool or
// turn_complete events. The latest count stands, up or down (a compacted
// context shrinks), and one older than the count it has is ignored; an
// event without one keeps the last. A subagent's count comes on
// agent_list only.
func (l *loop) mainTokens(e events.Event) {
	if l.id != "" || e.Tokens <= 0 || e.At.Before(l.tokensAt) {
		return
	}
	l.tokensAt = e.At
	if l.tokens != e.Tokens {
		l.tokens, l.dirty = e.Tokens, true
	}
}

// name marks the loop as one of the session's agents (named).
func (l *loop) name() {
	if !l.named {
		l.named, l.dirty = true, true
	}
}

// begin records that a turn of the loop (for a subagent, a run: each run
// is one turn) started at a time.
func (l *loop) begin(at time.Time) {
	if !at.Before(l.turnAt) {
		l.inTurn, l.turnAt = true, at
	}
	if at.After(l.errAt) {
		l.errAt, l.errReason = time.Time{}, ""
	}
	l.progress(at, "")
	l.setStatus(events.AgentRunning, at)
}

// setStatus records an agent status at a time, unless a later one is
// already recorded.
func (l *loop) setStatus(status string, at time.Time) {
	if status == "" || at.Before(l.statusAt) {
		return
	}
	if l.status != status {
		l.dirty = true
	}
	l.status, l.statusAt = status, at
	switch {
	case events.AgentEnded(status) && l.endedAt.IsZero():
		l.endedAt = at
		l.dirty = true
	case !events.AgentEnded(status) && !l.endedAt.IsZero():
		l.endedAt = time.Time{}
		l.dirty = true
	}
	if events.AgentEnded(status) {
		l.clearNeed()
	}
}

// progress records that the loop moved on at a time: a needs-you raised
// at or before it is answered. A tool event (toolID set) only answers the
// needs-you for its own call when both name one.
func (l *loop) progress(at time.Time, toolID string) {
	if at.After(l.progressAt) {
		l.progressAt = at
	}
	if l.needSince.IsZero() || at.Before(l.needSince) {
		return
	}
	if toolID != "" && l.needToolID != "" && toolID != l.needToolID {
		return
	}
	l.clearNeed()
}

func (l *loop) clearNeed() {
	if l.needSince.IsZero() {
		return
	}
	l.needSince, l.needSource, l.needReason, l.needToolID = time.Time{}, "", "", ""
	l.needWait = nil
	l.dirty = true
}

// rowState is the agent row's state: "waiting" while a needs-you is
// pending, else the status, filled in from the turn when no status was
// reported.
func (l *loop) rowState() string {
	if !l.needSince.IsZero() && l.live() {
		return events.AgentWaiting
	}
	if l.status != "" {
		return l.status
	}
	if l.inTurn {
		return events.AgentRunning
	}
	return events.AgentIdle
}

// working reports whether the loop is busy: in a turn, or a live subagent
// whose status says it runs.
func (l *loop) working() bool {
	if !l.live() {
		return false
	}
	if l.inTurn {
		return true
	}
	if l.id == "" {
		return false
	}
	switch l.status {
	case events.AgentRunning, events.AgentPending, events.AgentWaiting:
		return true
	}
	return false
}

// pollNeed is a needs-you the session poller saw (`status: waiting` or a
// background job's `state: blocked`) that the mod didn't report.
type pollNeed struct {
	reason string
	since  time.Time
	at     time.Time // the poll's snapshot time
}

// session is what the receiver knows about one Claude Code session.
type session struct {
	id     string
	key    string // the store row's key; "" until the row exists
	prevID string // the session id before a /clear

	host        string
	repo        string // main checkout
	root        string // worktree root at session_start
	startBranch string // branch at session_start, in root
	iterm       string
	interactive *bool
	background  bool   // session_start said it is a background session
	kind        string // the row's kind, if known
	// pluginBuild and pluginRoot: the plugin build the session loaded, and
	// its folder, as session_start said.
	pluginBuild string
	pluginRoot  string

	// Where the main loop is now.
	cwd      string
	worktree string
	branch   string

	startedAt   time.Time
	lastEventAt time.Time
	lastSaid    string
	lastAction  string
	idleAt      time.Time // an idle event, until a later turn_start or prompt

	ended     bool
	endedAt   time.Time
	endReason string
	// endedKey is the row key the session ended under, so a poll that
	// still lists its process doesn't bring the row back.
	endedKey string

	poll *pollNeed

	loops map[string]*loop
	order []string // loop ids in the order first seen

	dirty bool // the session row needs writing
}

func newSession(id string) *session {
	s := &session{id: id, loops: map[string]*loop{}}
	s.loop("")
	return s
}

// loop returns the loop with the id, adding it.
func (s *session) loop(id string) *loop {
	if l := s.loops[id]; l != nil {
		return l
	}
	l := &loop{id: id, kind: store.AgentSubagent, dirty: true}
	if id == "" {
		l.kind, l.named = store.AgentLead, true
	}
	s.loops[id] = l
	s.order = append(s.order, id)
	return l
}

func (s *session) main() *loop { return s.loop("") }

// derived is a session's state as written to its row.
type derived struct {
	State  string
	Reason string
	Since  time.Time
}

// derive works out the session state at now, in priority order:
// needs_you → error → working → quiet → turn_done → idle → gone.
func (s *session) derive(now time.Time, quietAfter time.Duration) derived {
	if s.ended {
		return derived{State: StateGone, Reason: s.endReason}
	}
	if need := s.oldestNeed(false); need != nil {
		return derived{State: StateNeedsYou, Reason: needText(need), Since: need.needSince}
	}
	if p := s.poll; p != nil && !s.lastEventAt.After(p.at) {
		return derived{State: StateNeedsYou, Reason: p.reason, Since: p.since}
	}
	m := s.main()
	if !m.errAt.IsZero() {
		return derived{State: StateError, Reason: m.errReason}
	}
	working := false
	for _, l := range s.loops {
		if l.working() {
			working = true
			break
		}
	}
	if working {
		if quietAfter > 0 && !s.lastEventAt.IsZero() && now.Sub(s.lastEventAt) > quietAfter {
			return derived{State: StateQuiet}
		}
		return derived{State: StateWorking}
	}
	if m.lastReason != "" && !s.idleAt.After(m.turnAt) {
		return derived{State: StateTurnDone, Reason: m.lastReason}
	}
	return derived{State: StateIdle}
}

// oldestNeed returns the live loop with the oldest pending needs-you (with
// a wait detail, if withWait), or nil.
func (s *session) oldestNeed(withWait bool) *loop {
	var need *loop
	for _, id := range s.order {
		l := s.loops[id]
		if l.needSince.IsZero() || !l.live() || (withWait && l.needWait == nil) {
			continue
		}
		if need == nil || l.needSince.Before(need.needSince) {
			need = l
		}
	}
	return need
}

// waitingOn is what the session waits on: the wait detail of its
// oldest pending ask that has one.
func (s *session) waitingOn() (api.WaitingOn, bool) {
	if s.ended {
		return api.WaitingOn{}, false
	}
	l := s.oldestNeed(true)
	if l == nil {
		return api.WaitingOn{}, false
	}
	w := l.needWait
	out := api.WaitingOn{
		Kind: w.Kind, AgentID: l.id, Tool: w.Tool, ToolUseID: l.needToolID,
		Summary: w.Summary, Plan: w.Plan, Since: l.needSince,
	}
	for _, q := range w.Questions {
		out.Questions = append(out.Questions, api.WaitQuestion{
			Header: q.Header, Question: q.Question, Options: slices.Clone(q.Options), MultiSelect: q.MultiSelect,
		})
	}
	return out, true
}

// answeredBy clears every pending needs-you raised at or before cutoff: a
// session poll taken after them says the session isn't waiting any more.
// That is the only sign a permission was granted before the tool it
// allowed has finished: no hook fires when the dialog closes.
func (s *session) answeredBy(cutoff time.Time) bool {
	cleared := false
	for _, l := range s.loops {
		if !l.needSince.IsZero() && !l.needSince.After(cutoff) {
			l.clearNeed()
			cleared = true
		}
	}
	if cleared {
		s.dirty = true
	}
	return cleared
}

// needText is a needs-you's reason as the session row shows it, in the
// poller's words where they exist ("permission prompt", like
// `waitingFor`), then what is asked, then which subagent asks.
func needText(l *loop) string {
	var what string
	switch l.needSource {
	case events.SourcePermissionRequest:
		what = "permission prompt"
	case events.SourceAskUserQuestion:
		what = "question"
	case events.SourceExitPlanMode:
		what = "plan approval"
	case events.SourceNotification:
		what = "notification"
	default:
		what = "waiting"
	}
	// The wait detail knows a question or a plan approval even when another
	// signal (a permission dialog for the call, a notification) came first.
	if l.needWait != nil {
		switch l.needWait.Kind {
		case events.WaitQuestion:
			what = "question"
		case events.WaitPlan:
			what = "plan approval"
		}
	}
	if l.needReason != "" {
		what += ": " + l.needReason
	}
	if l.id != "" {
		what = "agent " + l.id + ": " + what
	}
	return events.Truncate(what, store.MaxSummaryLen)
}

// apply folds one event into the session. resolve maps a cwd to its
// worktree and branch.
func (s *session) apply(e events.Event, resolve func(s *session, cwd string) (string, string)) {
	if e.Kind == events.AgentStart && e.AgentID == "" {
		return // SubagentStart always names its agent: there is nothing to apply
	}
	if e.At.After(s.lastEventAt) {
		s.lastEventAt = e.At
	}
	s.dirty = true
	l := s.loop(e.AgentID)
	if l.startedAt.IsZero() || e.At.Before(l.startedAt) {
		l.startedAt = e.At
		l.dirty = true
	}
	if s.startedAt.IsZero() {
		s.startedAt = e.At
	}
	// turn_complete's cwd isn't reliable; session_start sets its
	// roots before its cwd is resolved.
	if e.Cwd != "" && e.Kind != events.TurnComplete && e.Kind != events.SessionStart {
		s.setCwd(l, e.Cwd, resolve)
	}

	switch e.Kind {
	case events.SessionStart:
		set(&s.host, e.Host)
		set(&s.repo, e.Repo)
		set(&s.root, e.Worktree)
		set(&s.startBranch, e.Branch)
		set(&s.iterm, e.ItermSessionID)
		set(&s.pluginBuild, e.PluginBuild)
		set(&s.pluginRoot, e.PluginRoot)
		if e.Interactive != nil {
			v := *e.Interactive
			s.interactive = &v
		}
		if e.Background != nil {
			s.background = *e.Background
		}
		if e.At.Before(s.startedAt) {
			s.startedAt = e.At
		}
		if e.Cwd != "" {
			s.setCwd(l, e.Cwd, resolve)
		} else if s.root != "" && s.worktree == "" {
			s.worktree, s.branch = s.root, s.startBranch
		}

	case events.Prompt:
		if events.FromPerson(e.OriginKind) {
			for _, ll := range s.loops {
				if !ll.needSince.IsZero() && !e.At.Before(ll.needSince) {
					ll.clearNeed()
				}
			}
			s.poll = nil
		}
		if e.At.After(s.idleAt) {
			s.idleAt = time.Time{}
		}

	case events.TurnStart:
		// A subagent's turn is a run of it: turn.start names its loop as
		// agent.spawn answered it, a resumed run's (SendMessage to a
		// finished agent) included, which goes on under the same id.
		l.name()
		l.begin(e.At)
		if l.id == "" && e.At.After(s.idleAt) {
			s.idleAt = time.Time{}
		}

	case events.AgentStart:
		// classic.SubagentStart: a run of the agent begins (its first, or a
		// resume), as turn_start says, with its type. It comes just after
		// that run's turn_start, or in its place.
		if e.AgentType != "" && l.agentType != e.AgentType {
			l.agentType, l.dirty = e.AgentType, true
		}
		l.name()
		l.begin(e.At)

	case events.TurnComplete:
		if !e.At.Before(l.turnAt) {
			l.inTurn, l.turnAt = false, e.At
			l.lastReason = e.Reason
			if l.lastReason == "" {
				l.lastReason = events.ReasonAnswer
			}
		}
		l.progress(e.At, "")
		l.mainTokens(e)
		if e.Reason == events.ReasonError && !e.At.Before(l.errAt) {
			l.errAt = e.At
			if l.errReason == "" {
				l.errReason = events.ReasonError
			}
		}
		if l.id == "" {
			if e.LastSaid != "" {
				s.lastSaid = e.LastSaid
			}
			if e.Reason == events.ReasonError {
				l.setStatus(events.AgentFailed, e.At)
				// The main loop goes on: it is failed only until the next turn.
				l.endedAt = time.Time{}
			} else {
				l.setStatus(events.AgentIdle, e.At)
			}
		} else {
			switch e.Reason {
			case events.ReasonError:
				l.setStatus(events.AgentFailed, e.At)
			case events.ReasonAborted:
				l.setStatus(events.AgentKilled, e.At)
			default:
				l.setStatus(events.AgentCompleted, e.At)
			}
		}

	case events.Tool:
		l.progress(e.At, e.ToolUseID)
		l.mainTokens(e)
		label := e.Label
		if label == "" {
			label = e.Tool
		}
		if label != "" {
			l.lastAction, l.dirty = label, true
			if l.named {
				s.lastAction = label // a fork's attempt isn't the session's
			}
		}
		if l.status == "" || l.status == events.AgentPending {
			l.setStatus(events.AgentRunning, e.At)
		}

	case events.NeedsYou:
		if e.At.Before(l.progressAt) || !l.live() {
			break // already answered, or the loop is over
		}
		if l.needSince.IsZero() {
			l.needSince, l.needSource, l.needReason, l.needToolID = e.At, e.Source, e.Reason, e.ToolUseID
			l.needWait = e.Wait
			l.dirty = true
		} else {
			if e.At.Before(l.needSince) {
				l.needSince = e.At
			}
			// A notification only says "a dialog is up": it doesn't replace
			// what an earlier signal said is asked.
			if e.Source != events.SourceNotification || l.needReason == "" {
				set(&l.needReason, e.Reason)
			}
			// The wait detail follows the call the ask now names.
			sameCall := e.ToolUseID == "" || l.needToolID == "" || e.ToolUseID == l.needToolID
			set(&l.needToolID, e.ToolUseID)
			switch {
			case e.Wait != nil:
				l.needWait = e.Wait
			case !sameCall:
				l.needWait = nil
			}
		}
		if l.id != "" && (l.status == "" || l.status == events.AgentPending) {
			l.setStatus(events.AgentRunning, e.At) // it asks, so it runs
		}

	case events.Idle:
		if l.id == "" && e.At.After(l.turnAt) {
			s.idleAt = e.At
		}

	case events.Error:
		if !e.At.Before(l.errAt) {
			l.errAt = e.At
			l.errReason = e.Reason
			if l.errReason == "" {
				l.errReason = events.ReasonError
			}
		}
		if l.id != "" {
			l.setStatus(events.AgentFailed, e.At)
		}

	case events.WorktreeCreate:
		id, ok := strings.CutPrefix(e.Name, "agent-")
		if !ok || id == "" {
			break // EnterWorktree in the main loop: its cwd says the rest
		}
		a := s.loop(id)
		a.name()
		if a.startedAt.IsZero() {
			a.startedAt = e.At
		}
		if s.repo != "" {
			a.worktree = filepath.Join(s.repo, ".claude", "worktrees", e.Name)
			a.fromCreate = true
			a.dirty = true
		}

	case events.AgentList:
		for _, it := range e.Agents {
			a := s.loop(it.ID)
			if a.id == "" {
				continue
			}
			a.name()
			if a.startedAt.IsZero() {
				a.startedAt = e.At
				a.dirty = true
			}
			if it.Type != "" && a.agentType != it.Type {
				a.agentType, a.dirty = it.Type, true
			}
			if it.Background != nil && a.background != *it.Background {
				a.background, a.dirty = *it.Background, true
			}
			if it.ParentID != "" && a.parent != it.ParentID {
				a.parent, a.dirty = it.ParentID, true
			}
			// The latest count stands, up or down (a compacted context
			// shrinks); a snapshot without one keeps the last.
			if it.Tokens > 0 && a.tokens != it.Tokens {
				a.tokens, a.dirty = it.Tokens, true
			}
			// A snapshot taken just after a loop's turn ended can still say
			// running; foreground agents drop out of the list once done.
			// So after a turn_complete only idle and final statuses count,
			// until the next turn_start.
			if !a.inTurn && a.lastReason != "" {
				if it.Status != events.AgentIdle && !events.AgentEnded(it.Status) {
					continue
				}
			}
			a.setStatus(it.Status, e.At)
		}

	case events.SessionEnd:
		s.end(e)
	}
}

// end marks the session over at a session_end that isn't a /clear.
func (s *session) end(e events.Event) {
	s.ended, s.endedAt, s.endReason = true, e.At, e.Reason
	s.poll = nil
	for _, l := range s.loops {
		l.clearNeed()
		l.inTurn = false
		switch {
		case l.id == "":
			l.setStatus(events.AgentCompleted, e.At)
		case !l.live():
		case l.status == events.AgentIdle:
			l.setStatus(events.AgentCompleted, e.At)
		default:
			l.setStatus(events.AgentKilled, e.At)
		}
		if l.endedAt.IsZero() {
			l.endedAt = e.At
		}
		l.dirty = true
	}
}

// clearConversation resets the main loop after /clear: the process goes
// on with an empty conversation, at the prompt.
func (s *session) clearConversation(at time.Time) {
	m := s.main()
	m.inTurn, m.lastReason, m.errAt, m.errReason = false, "", time.Time{}, ""
	// A new conversation: its own first response counts, not a late event
	// of the old one.
	m.tokensAt = at
	if m.tokens != 0 {
		m.tokens, m.dirty = 0, true
	}
	m.clearNeed()
	m.setStatus(events.AgentIdle, at)
	s.idleAt = time.Time{}
	s.poll = nil
	s.dirty = true
}

// setCwd records a loop's cwd and, when it changed, the worktree it is in.
func (s *session) setCwd(l *loop, cwd string, resolve func(*session, string) (string, string)) {
	if sameDir(l.cwd, cwd) {
		return
	}
	l.cwd = cwd
	wt, branch := resolve(s, cwd)
	if l.id == "" {
		s.cwd = cwd
		if wt != "" {
			s.worktree, s.branch = wt, branch
		}
		if wt != "" && l.worktree != wt {
			l.worktree = wt
			l.dirty = true
		}
		return
	}
	if wt != "" && !l.fromCreate && l.worktree != wt {
		l.worktree = wt
		l.dirty = true
	}
}

// set replaces *dst with v when v is non-empty.
func set(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}
