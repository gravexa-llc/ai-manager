// Package live is aimd's event receiver: it takes the batches the mod
// POSTs to /events (internal/events; the HTTP side is internal/web), keeps
// each session's live state, and writes it to the store: the session
// state and its needs-you, the agents of each session with
// their lineage and worktrees, the last action and last words, and
// a summary row per event for the timeline.
//
// # Processing
//
// [Receiver.Accept] only counts and queues a batch, so POST /events
// answers at once; one goroutine ([Receiver.Run]) applies batches in the
// order they came, each sorted by time. Events are deduplicated by
// session, agent, kind and time within a second (plus the tool call, label
// or origin for the kinds where two can come that close), in case a
// settings-hook fallback ever sends the same thing as the mod.
//
// # State
//
// A session's state is derived from its loops (its main loop and each
// subagent), in this order: needs_you (any live loop has a pending ask)
// → error (the main loop's last turn failed) → working (a loop is in a
// turn, or a subagent runs) → quiet (working, but no event for
// state.quiet_after) → turn_done (the main loop finished a turn) → idle →
// gone (session_end). A needs-you is cleared when its loop moves on: a
// turn starts or ends, the tool call it asked about finishes, or the
// person sends a prompt; or when a session poll taken 5 s or more after
// it finds the session not waiting (a granted permission's tool may
// run long after its dialog closed, and no hook marks the close). While a
// needs-you is pending, the mod's wait detail (what it asks) is kept with
// it in memory, for GET /api/sessions ([Receiver.WaitingOn]). Agent rows take their status from agent_list
// snapshots and turn events, with "waiting" while the agent waits on you.
//
// # The session poller
//
// `claude agents --json` stays the list of sessions on this host. A
// session the mod reports first gets a row keyed "session:<id>", which the
// poller's row for it adopts later ([Receiver.RefineSessions], called by
// the daemon before it writes a poll). For sessions it has heard from,
// the receiver's state wins over the poll's verdict, except that a poll
// that sees a needs-you the mod didn't report (and no event came after
// that poll) still shows it.
//
// # Timers
//
// [Receiver.Tick] runs every few seconds: it turns working sessions quiet
// after state.quiet_after without events, repairs a session row a poll
// overwrote, and switches the daemon's mod-live mode off five minutes
// after the last batch (SetModLive(true) happens on the first batch).
package live

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/events"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
	"github.com/gravexa-llc/ai-manager/internal/worktrees"
)

// Store is the part of *store.Store the receiver uses.
type Store interface {
	FindSession(ctx context.Context, sessionID string) (store.Session, error)
	UpsertSessions(ctx context.Context, sessions ...store.Session) error
	UpdateSessionLive(ctx context.Context, key string, u store.SessionLive) error
	AdoptSession(ctx context.Context, fromKey, toKey string) error
	MoveSessionID(ctx context.Context, oldID, newID string) (bool, error)
	ListSessions(ctx context.Context, f store.SessionFilter) ([]store.Session, error)
	EndSession(ctx context.Context, key string, at time.Time) error
	ListAgents(ctx context.Context, sessionID string) ([]store.Agent, error)
	UpsertAgents(ctx context.Context, agents ...store.Agent) error
	InsertEvents(ctx context.Context, events ...store.Event) error
	ListWorktrees(ctx context.Context, f store.WorktreeFilter) ([]store.Worktree, error)
}

var _ Store = (*store.Store)(nil)

// Defaults for the zero Options fields.
const (
	DefaultQuietAfter = 15 * time.Minute
	DefaultModLiveFor = 5 * time.Minute
	DefaultTickEvery  = 10 * time.Second
	DefaultQueueSize  = 256
)

// Retention of what the receiver keeps in memory.
const (
	dedupeWindow = time.Second
	dedupeKeep   = time.Minute      // how long a dedupe entry is kept
	aliasKeep    = 10 * time.Minute // how long an old session id leads to the new one after /clear
	endedKeep    = 10 * time.Minute // how long an ended session is remembered
	silentKeep   = 24 * time.Hour   // a session silent this long is forgotten
	worktreeTTL  = 10 * time.Second // how long the worktree list is cached
	futureSlack  = time.Minute      // event times further ahead than this are taken as now
	maxUnknown   = 32               // distinct unknown kinds counted by name
	// answeredSlack: a session poll taken this long after an ask began,
	// that finds the session not waiting, answers the ask.
	answeredSlack = 5 * time.Second
)

// Options configure a Receiver. Only Store is required.
type Options struct {
	Store Store
	// QuietAfter is state.quiet_after: how long a working session can go
	// without events before it is quiet. DefaultQuietAfter if zero.
	QuietAfter time.Duration
	// ModLiveFor is how long after the last batch the mod counts as live.
	// DefaultModLiveFor if zero.
	ModLiveFor time.Duration
	// TickEvery is how often Run calls Tick. DefaultTickEvery if zero.
	TickEvery time.Duration
	// QueueSize is how many batches can wait; a batch that finds the queue
	// full is dropped and counted. DefaultQueueSize if zero.
	QueueSize int
	// SetModLive is the daemon's SetModLive: called with true on the first
	// batch and with false ModLiveFor after the last one. It must not
	// block or call the receiver. May be nil.
	SetModLive func(live bool)
	// Notify is called with api.TopicSessions after a batch or a tick
	// changed sessions or agents. It is the same function the daemon
	// notifies through (so the state pass runs after it too). It must not
	// block. May be nil.
	Notify func(topic string)
	// Now is the clock; time.Now if nil.
	Now func() time.Time
	// Logger receives the receiver's log; nil discards it.
	Logger *slog.Logger
}

// Receiver applies the mod's events. Create it with New, start Run, and
// give it to internal/web as the /events sink.
type Receiver struct {
	opts  Options
	st    Store
	now   func() time.Time
	log   *slog.Logger
	queue chan job

	// smu guards what the HTTP handler touches: counters and the mod-live
	// switch. It is never held while waiting on the store.
	smu      sync.Mutex
	stats    api.EventStats
	modLive  bool
	lastRecv time.Time
	// waits is what each session waits on, by session id, as of the
	// last batch, tick or poll: a copy GET /api/sessions reads without
	// waiting for the worker.
	waits map[string]api.WaitingOn

	// mu guards the state below. The worker holds it while it applies a
	// batch, RefineSessions and Tick while they run.
	mu       sync.Mutex
	sessions map[string]*session
	aliases  map[string]alias
	seen     map[dupKey]dupVal
	wts      []store.Worktree
	wtsAt    time.Time
}

type job struct {
	batch *events.Batch
	done  chan struct{}
}

type alias struct {
	to    string
	until time.Time
}

type dupKey struct {
	session, agent string
	kind           events.Kind
	disc           string
}

type dupVal struct{ at, seen time.Time }

// New returns a receiver for opts. Nothing is applied until Run.
func New(opts Options) *Receiver {
	if opts.QuietAfter <= 0 {
		opts.QuietAfter = DefaultQuietAfter
	}
	if opts.ModLiveFor <= 0 {
		opts.ModLiveFor = DefaultModLiveFor
	}
	if opts.TickEvery <= 0 {
		opts.TickEvery = DefaultTickEvery
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = DefaultQueueSize
	}
	r := &Receiver{
		opts:     opts,
		st:       opts.Store,
		now:      opts.Now,
		log:      opts.Logger,
		queue:    make(chan job, opts.QueueSize),
		sessions: map[string]*session{},
		aliases:  map[string]alias{},
		seen:     map[dupKey]dupVal{},
	}
	if r.now == nil {
		r.now = time.Now
	}
	if r.log == nil {
		r.log = slog.New(slog.DiscardHandler)
	}
	return r
}

// Run applies queued batches and ticks every TickEvery until ctx is done.
// It returns nil.
func (r *Receiver) Run(ctx context.Context) error {
	t := time.NewTicker(r.opts.TickEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case j := <-r.queue:
			if j.batch != nil {
				r.process(ctx, j.batch)
			}
			if j.done != nil {
				close(j.done)
			}
		case <-t.C:
			r.Tick(ctx)
		}
	}
}

// Accept counts a decoded batch and queues it. It never blocks: when the
// queue is full the batch is dropped and counted. The first batch turns
// the daemon's mod-live mode on.
func (r *Receiver) Accept(b events.Batch) {
	now := r.now()
	r.smu.Lock()
	r.stats.Batches++
	r.stats.Events += int64(len(b.Events))
	r.stats.Invalid += int64(b.Invalid)
	for kind, n := range b.Unknown {
		r.stats.UnknownKinds += int64(n)
		if r.stats.UnknownByKind == nil {
			r.stats.UnknownByKind = map[string]int64{}
		}
		if _, ok := r.stats.UnknownByKind[kind]; !ok && len(r.stats.UnknownByKind) >= maxUnknown {
			kind = "(other)"
		}
		r.stats.UnknownByKind[kind] += int64(n)
	}
	r.stats.LastBatchAt = now
	r.lastRecv = now
	if !r.modLive {
		// Called under smu, so a Tick switching it off can't land after
		// this. SetModLive never blocks and never calls back.
		r.modLive = true
		r.log.Info("mod reports live")
		if r.opts.SetModLive != nil {
			r.opts.SetModLive(true)
		}
	}
	r.smu.Unlock()
	if b.Dropped() > 0 {
		r.log.Debug("events dropped", "unknown", b.Unknown, "invalid", b.Invalid)
	}
	if len(b.Events) == 0 {
		return
	}
	select {
	case r.queue <- job{batch: &b}:
	default:
		r.smu.Lock()
		r.stats.DroppedBatches++
		r.smu.Unlock()
		r.log.Warn("event queue full; batch dropped", "events", len(b.Events))
	}
}

// Refuse counts a POST refused as a whole (too large, or not a batch).
func (r *Receiver) Refuse(reason string) {
	r.smu.Lock()
	r.stats.RefusedBatches++
	r.smu.Unlock()
	r.log.Warn("event batch refused", "reason", reason)
}

// Stats returns the receiver's counters, for GET /api/status.
func (r *Receiver) Stats() api.EventStats {
	r.smu.Lock()
	defer r.smu.Unlock()
	st := r.stats
	if st.UnknownByKind != nil {
		st.UnknownByKind = make(map[string]int64, len(r.stats.UnknownByKind))
		for k, v := range r.stats.UnknownByKind {
			st.UnknownByKind[k] = v
		}
	}
	st.ModLive = r.modLive
	return st
}

// WaitingOn returns what a session waits on: the permission,
// question or plan of its oldest pending ask that the mod described, and
// which loop asks. ok is false when it waits on nothing the mod described.
// It is what the receiver knew after its last batch, tick or poll, and is
// never read from the store. GET /api/sessions calls it for each session.
func (r *Receiver) WaitingOn(sessionID string) (w api.WaitingOn, ok bool) {
	r.smu.Lock()
	defer r.smu.Unlock()
	w, ok = r.waits[sessionID]
	if ok {
		w.Questions = slices.Clone(w.Questions)
	}
	return w, ok
}

// publishWaitsLocked refreshes the copy WaitingOn reads. r.mu is held.
func (r *Receiver) publishWaitsLocked() {
	waits := map[string]api.WaitingOn{}
	for id, s := range r.sessions {
		if w, ok := s.waitingOn(); ok {
			waits[id] = w
		}
	}
	r.smu.Lock()
	r.waits = waits
	r.smu.Unlock()
}

// Sync waits until every batch accepted before the call has been applied,
// or ctx is done. Run must be running.
func (r *Receiver) Sync(ctx context.Context) error {
	done := make(chan struct{})
	select {
	case r.queue <- job{done: done}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// process applies one batch.
func (r *Receiver) process(ctx context.Context, b *events.Batch) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	evs := slices.Clone(b.Events)
	for i := range evs {
		at := evs[i].At.UTC().Truncate(time.Millisecond)
		if at.After(now.Add(futureSlack)) {
			at = now.Truncate(time.Millisecond)
		}
		evs[i].At = at
	}
	slices.SortStableFunc(evs, func(a, b events.Event) int { return a.At.Compare(b.At) })

	var rows []store.Event
	var touched []*session
	touch := func(s *session) {
		if s != nil && !slices.Contains(touched, s) {
			touched = append(touched, s)
		}
	}
	var applied, deduped int64
	resolve := r.resolver(ctx)
	for _, e := range evs {
		e.SessionID = r.aliasOf(e.SessionID)
		if r.dup(e, now) {
			deduped++
			continue
		}
		if e.Kind != events.AgentList {
			rows = append(rows, eventRow(e))
		}
		if e.Kind == events.SessionStart && e.PrevSessionID != "" && e.PrevSessionID != e.SessionID {
			r.move(ctx, e.PrevSessionID, e.SessionID, e.At, now)
		}
		s := r.session(ctx, e)
		if s == nil {
			continue // an ended session: the event row is still kept
		}
		applied++
		if e.Kind == events.SessionEnd && e.Reason == events.ReasonClear {
			if e.At.After(s.lastEventAt) {
				s.lastEventAt = e.At
			}
			if e.NewSessionID != "" && e.NewSessionID != s.id {
				r.move(ctx, s.id, e.NewSessionID, e.At, now)
				touch(r.sessions[e.NewSessionID])
			} else {
				s.clearConversation(e.At)
				touch(s)
			}
			continue
		}
		s.apply(e, resolve)
		touch(s)
	}
	if len(rows) > 0 {
		if err := r.st.InsertEvents(ctx, rows...); err != nil {
			r.storeError("insert events", err)
		}
	}
	changed := false
	for _, s := range touched {
		if r.persist(ctx, s, now) {
			changed = true
		}
	}
	r.publishWaitsLocked()
	r.smu.Lock()
	r.stats.Applied += applied
	r.stats.Deduped += deduped
	r.stats.LastEventAt = maxTime(r.stats.LastEventAt, lastAt(evs))
	r.stats.LiveSessions = r.liveCountLocked()
	r.smu.Unlock()
	if changed {
		r.notify(api.TopicSessions)
	}
}

func lastAt(evs []events.Event) time.Time {
	if len(evs) == 0 {
		return time.Time{}
	}
	return evs[len(evs)-1].At
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func (r *Receiver) notify(topic string) {
	if r.opts.Notify != nil {
		r.opts.Notify(topic)
	}
}

func (r *Receiver) storeError(what string, err error) {
	r.smu.Lock()
	r.stats.StoreErrors++
	r.smu.Unlock()
	r.log.Warn("event receiver: store", "op", what, "err", err)
}

// liveCountLocked counts the sessions not ended. r.mu is held.
func (r *Receiver) liveCountLocked() int {
	n := 0
	for _, s := range r.sessions {
		if !s.ended {
			n++
		}
	}
	return n
}

// aliasOf follows /clear moves from an old session id to the current one.
func (r *Receiver) aliasOf(id string) string {
	for range 4 {
		a, ok := r.aliases[id]
		if !ok {
			return id
		}
		id = a.to
	}
	return id
}

// dup reports whether e repeats an event already applied within a second,
// and remembers e otherwise. agent_list snapshots are never duplicates:
// applying one twice is harmless, and dropping a later one would lose
// news.
func (r *Receiver) dup(e events.Event, now time.Time) bool {
	var disc string
	switch e.Kind {
	case events.AgentList:
		return false
	case events.Tool:
		disc = e.Tool + "\x00" + e.Label + "\x00" + e.ToolUseID
	case events.Prompt:
		disc = e.OriginKind
	case events.WorktreeCreate:
		disc = e.Name
	case events.NeedsYou:
		disc = e.ToolUseID
	}
	k := dupKey{e.SessionID, e.AgentID, e.Kind, disc}
	if v, ok := r.seen[k]; ok {
		d := e.At.Sub(v.at)
		if d < 0 {
			d = -d
		}
		if d <= dedupeWindow {
			return true
		}
	}
	r.seen[k] = dupVal{at: e.At, seen: now}
	return false
}

// session returns the session an event belongs to, loading what the store
// knows about it the first time. It returns nil when the session has
// ended and the event isn't a session_start after that end (claude
// --resume starts a new row).
func (r *Receiver) session(ctx context.Context, e events.Event) *session {
	id := e.SessionID
	if s := r.sessions[id]; s != nil {
		if !s.ended {
			return s
		}
		if e.Kind != events.SessionStart || !e.At.After(s.endedAt) {
			return nil
		}
		// A new life for the same id: a new row.
		s = newSession(id)
		r.sessions[id] = s
		return s
	}
	s := newSession(id)
	row, err := r.st.FindSession(ctx, id)
	switch {
	case err == nil && row.EndedAt.IsZero():
		r.seed(ctx, s, row)
	case err == nil:
		if e.Kind != events.SessionStart || !e.At.After(row.EndedAt) {
			s.ended, s.endedAt, s.endedKey = true, row.EndedAt, row.Key
			r.sessions[id] = s
			return nil
		}
	case errors.Is(err, store.ErrNotFound):
	default:
		r.storeError("find session", err)
	}
	r.sessions[id] = s
	return s
}

// seed fills a session from its live row and its agent rows (after aimd
// restarts, or for a session the poller listed first). Turns and pending
// needs-you aren't stored, so they start empty.
func (r *Receiver) seed(ctx context.Context, s *session, row store.Session) {
	s.key = row.Key
	s.prevID = row.PrevSessionID
	s.host, s.repo, s.iterm, s.kind = row.Host, row.Repo, row.ItermSessionID, row.Kind
	s.cwd, s.worktree, s.branch = row.Cwd, row.WorktreePath, row.Branch
	s.lastAction, s.lastSaid = row.LastAction, row.LastSaid
	s.startedAt, s.lastEventAt = row.StartedAt, row.LastEventAt
	agents, err := r.st.ListAgents(ctx, s.id)
	if err != nil {
		r.storeError("list agents", err)
		return
	}
	for _, a := range agents {
		l := s.loop(a.AgentID)
		if a.Kind != "" {
			l.kind = a.Kind
		}
		l.agentType, l.background, l.parent, l.model = a.AgentType, a.Background, a.ParentAgentID, a.Model
		l.worktree = a.WorktreePath
		l.fromCreate = a.AgentID != "" && strings.EqualFold(lastElem(a.WorktreePath), "agent-"+a.AgentID)
		l.status = a.State
		if l.status == events.AgentWaiting {
			l.status = events.AgentRunning // the ask itself isn't stored
		}
		l.lastAction, l.startedAt, l.endedAt = a.LastAction, a.StartedAt, a.EndedAt
		l.dirty = false
	}
}

// move carries a session over to its new id after /clear, in the store
// and in memory, and makes the old id an alias of the new one for a while.
func (r *Receiver) move(ctx context.Context, oldID, newID string, at, now time.Time) {
	if _, err := r.st.MoveSessionID(ctx, oldID, newID); err != nil {
		r.storeError("move session", err)
	}
	r.aliases[oldID] = alias{to: newID, until: now.Add(aliasKeep)}
	old := r.sessions[oldID]
	if old == nil {
		return
	}
	delete(r.sessions, oldID)
	if cur := r.sessions[newID]; cur != nil {
		set(&cur.host, old.host)
		set(&cur.repo, old.repo)
		set(&cur.iterm, old.iterm)
		cur.prevID = oldID
		if cur.key == "" {
			cur.key = old.key
		}
		cur.dirty = true
		return
	}
	old.id, old.prevID = newID, oldID
	for _, l := range old.loops {
		l.dirty = true // the rows are under the new id now
	}
	old.clearConversation(at)
	r.sessions[newID] = old
}

// persist writes a session's row and its changed agent rows, creating the
// row when the poller hasn't listed the session yet. It reports whether
// anything was written.
func (r *Receiver) persist(ctx context.Context, s *session, now time.Time) bool {
	d := s.derive(now, r.opts.QuietAfter)
	changed := false
	if s.key == "" {
		row := store.Session{
			Key: store.ModSessionKeyPrefix + s.id, SessionID: s.id, Kind: s.kindGuess(), Headless: s.headlessGuess(),
			Host: s.host, Cwd: s.cwd, WorktreePath: s.worktree, Repo: s.repo, Branch: s.branch,
			State: d.State, StateReason: d.Reason, NeedsYouSince: d.Since, StartedAt: s.startedAt, LastSeenAt: now,
		}
		if err := r.st.UpsertSessions(ctx, row); err != nil {
			r.storeError("create session", err)
			return false
		}
		s.key = row.Key
		changed = true
	}
	if s.dirty {
		u := store.SessionLive{
			State: d.State, StateReason: d.Reason, NeedsYouSince: d.Since,
			LastAction: s.lastAction, LastSaid: s.lastSaid, LastEventAt: s.lastEventAt,
			Cwd: s.cwd, WorktreePath: s.worktree, Repo: s.repo, Branch: s.branch,
			Host: s.host, ItermSessionID: s.iterm, Kind: s.kindGuess(),
		}
		if s.interactive != nil && *s.interactive {
			f := false
			u.Headless = &f
		}
		err := r.st.UpdateSessionLive(ctx, s.key, u)
		if errors.Is(err, store.ErrNotFound) {
			// The row's key changed (the poller adopted it) or the poller
			// ended it.
			row, ferr := r.st.FindSession(ctx, s.id)
			switch {
			case ferr == nil && row.EndedAt.IsZero():
				s.key = row.Key
				err = r.st.UpdateSessionLive(ctx, s.key, u)
			case ferr == nil:
				s.ended, s.endedAt, s.endedKey = true, row.EndedAt, row.Key
				s.endReason = "gone from claude agents"
				err = nil
			default:
				err = ferr
			}
		}
		if err != nil {
			r.storeError("update session", err)
		} else {
			s.dirty = false
			changed = true
		}
	}
	if r.persistAgents(ctx, s) {
		changed = true
	}
	if s.ended && s.endedKey == "" {
		err := r.st.EndSession(ctx, s.key, s.endedAt)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			r.storeError("end session", err)
		} else {
			s.endedKey = s.key
			changed = true
		}
	}
	return changed
}

// persistAgents writes a session's changed agent rows. It reports whether
// it wrote any.
func (r *Receiver) persistAgents(ctx context.Context, s *session) bool {
	var agents []store.Agent
	for _, id := range s.order {
		l := s.loops[id]
		if !l.dirty {
			continue
		}
		if l.id == "" && s.worktree != "" {
			l.worktree = s.worktree
		}
		agents = append(agents, store.Agent{
			SessionID: s.id, AgentID: l.id, Kind: l.kind, AgentType: l.agentType, Background: l.background,
			Model: l.model, ParentAgentID: l.parent, WorktreePath: l.worktree, State: l.rowState(),
			LastAction: l.lastAction, StartedAt: l.startedAt, EndedAt: l.endedAt,
		})
	}
	if len(agents) == 0 {
		return false
	}
	if err := r.st.UpsertAgents(ctx, agents...); err != nil {
		r.storeError("upsert agents", err)
		return false
	}
	for _, id := range s.order {
		s.loops[id].dirty = false
	}
	return true
}

// kindGuess is the session's kind when the poller hasn't said: an
// interactive session says so at session_start, and a background session
// says it is one (it says interactive too).
func (s *session) kindGuess() string {
	if s.kind != "" {
		return s.kind
	}
	if s.background {
		return "background"
	}
	if s.interactive != nil && *s.interactive {
		return "interactive"
	}
	return ""
}

// headlessGuess marks a session that said it isn't interactive (a `claude
// -p` run) headless until the poller says otherwise; background sessions
// aren't headless, and the poller knows which those are.
func (s *session) headlessGuess() bool {
	return s.interactive != nil && !*s.interactive && !s.background
}

// Tick runs the timers: it switches mod-live off ModLiveFor after the
// last batch, turns working sessions quiet, rewrites session rows whose
// state differs from what the receiver derives (a poll can overwrite one
// between a refine and its write), and forgets old sessions, aliases and
// dedupe entries. Run calls it every TickEvery; tests call it with a fake
// clock.
func (r *Receiver) Tick(ctx context.Context) {
	now := r.now()
	r.smu.Lock()
	if r.modLive && now.Sub(r.lastRecv) >= r.opts.ModLiveFor {
		r.modLive = false
		r.log.Info("mod quiet; session polling back to normal", "for", r.opts.ModLiveFor)
		if r.opts.SetModLive != nil {
			r.opts.SetModLive(false)
		}
	}
	r.smu.Unlock()

	r.mu.Lock()
	defer r.mu.Unlock()
	for k, v := range r.seen {
		if now.Sub(v.seen) > dedupeKeep {
			delete(r.seen, k)
		}
	}
	for k, a := range r.aliases {
		if now.After(a.until) {
			delete(r.aliases, k)
		}
	}
	for id, s := range r.sessions {
		if (s.ended && now.Sub(s.endedAt) > endedKeep) || (!s.ended && now.Sub(s.lastEventAt) > silentKeep) {
			delete(r.sessions, id)
		}
	}
	if len(r.sessions) == 0 {
		return
	}
	rows, err := r.st.ListSessions(ctx, store.SessionFilter{})
	if err != nil {
		r.storeError("list sessions", err)
		return
	}
	byKey := make(map[string]*store.Session, len(rows))
	for i := range rows {
		byKey[rows[i].Key] = &rows[i]
	}
	ids := make([]string, 0, len(r.sessions))
	for id := range r.sessions {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	changed := false
	for _, id := range ids {
		s := r.sessions[id]
		if s.ended || s.key == "" {
			continue
		}
		d := s.derive(now, r.opts.QuietAfter)
		row := byKey[s.key]
		if row != nil && row.State == d.State && row.StateReason == d.Reason && row.NeedsYouSince.Equal(d.Since) {
			continue
		}
		s.dirty = true
		if r.persist(ctx, s, now) {
			changed = true
		}
	}
	r.publishWaitsLocked()
	r.smu.Lock()
	r.stats.LiveSessions = r.liveCountLocked()
	r.smu.Unlock()
	if changed {
		r.notify(api.TopicSessions)
	}
}

// RefineSessions is the daemon's hook into a session poll, called with the
// rows the poll is about to write. It lets the poller's row for a session
// adopt the row the receiver made for it, and gives each session the
// receiver has heard from the receiver's state and whereabouts: its
// derived state (a needs-you only the poll sees still counts, until an
// event comes after the poll), the cwd and worktree it is in now, and not
// headless when the mod said it is interactive. Rows for a session the mod
// said ended are dropped, so a poll that still lists its process doesn't
// bring the row back. at is the poll's snapshot time.
func (r *Receiver) RefineSessions(ctx context.Context, rows []store.Session, at time.Time) []store.Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	modRows := map[string]store.Session{}
	if stored, err := r.st.ListSessions(ctx, store.SessionFilter{}); err != nil {
		r.storeError("list sessions", err)
	} else {
		for _, s := range stored {
			if store.IsModSessionKey(s.Key) && s.SessionID != "" {
				modRows[s.SessionID] = s
			}
		}
	}
	out := make([]store.Session, 0, len(rows))
	for _, row := range rows {
		if row.SessionID == "" {
			out = append(out, row)
			continue
		}
		row.SessionID = r.aliasOf(row.SessionID)
		if m, ok := modRows[row.SessionID]; ok && m.Key != row.Key {
			if err := r.st.AdoptSession(ctx, m.Key, row.Key); err != nil {
				r.storeError("adopt session", err)
			} else if s := r.sessions[row.SessionID]; s != nil && s.key == m.Key {
				s.key = row.Key
			}
			delete(modRows, row.SessionID)
		}
		s := r.sessions[row.SessionID]
		switch {
		case s == nil:
		case s.ended:
			if s.endedKey == row.Key {
				continue
			}
		default:
			if s.key == "" {
				s.key = row.Key
			}
			if row.State == StateNeedsYou {
				since := at
				if s.poll != nil {
					since = s.poll.since
				}
				s.poll = &pollNeed{reason: row.StateReason, since: since, at: at}
			} else {
				s.poll = nil
				// The poll says the session isn't waiting: the asks raised
				// well before it were answered, even if the tool a person
				// allowed still runs (no hook fires when a dialog closes).
				if pollNotWaiting(row.State) && s.answeredBy(at.Add(-answeredSlack)) {
					r.persistAgents(ctx, s)
				}
			}
			d := s.derive(now, r.opts.QuietAfter)
			row.State, row.StateReason, row.NeedsYouSince = d.State, d.Reason, d.Since
			if s.interactive != nil && *s.interactive {
				row.Headless = false
			}
			if s.cwd != "" && !sameDir(row.Cwd, s.cwd) {
				row.Cwd = s.cwd
				if s.worktree != "" {
					row.WorktreePath, row.Branch = s.worktree, s.branch
				}
				if s.repo != "" {
					row.Repo = s.repo
				}
			}
		}
		out = append(out, row)
	}
	r.publishWaitsLocked()
	return out
}

// pollNotWaiting reports whether a poll's verdict for a session says it
// waits on nothing: busy, idle, or done with its turn. An unknown, failed
// or gone verdict says nothing about its dialogs.
func pollNotWaiting(state string) bool {
	return state == StateWorking || state == StateIdle || state == StateTurnDone
}

// resolver returns the function that maps a cwd to the worktree it is in
// and that worktree's branch: the deepest of the worktrees the scanner
// knows, Claude Code's <repo>/.claude/worktrees/<name> layout (branch
// worktree-<name>, which the scanner's answer overrides once it has one),
// the session's worktree root and its main checkout.
func (r *Receiver) resolver(ctx context.Context) func(*session, string) (string, string) {
	return func(s *session, cwd string) (string, string) {
		best, branch := "", ""
		consider := func(path, br string) {
			if path != "" && within(cwd, path) && len(worktrees.PathKey(path)) > len(worktrees.PathKey(best)) {
				best, branch = path, br
			}
		}
		// On a tie the first one considered wins: the scanner, then what
		// the mod said at session_start, then Claude Code's layout.
		for _, wt := range r.worktrees(ctx) {
			consider(wt.Path, wt.Branch)
		}
		if s.startBranch != "" {
			consider(s.root, s.startBranch)
		}
		if p, br := claudeWorktree(cwd); p != "" {
			consider(p, br)
		}
		consider(s.root, "")
		if sameDir(s.repo, s.root) {
			consider(s.repo, s.startBranch)
		} else {
			consider(s.repo, "")
		}
		return best, branch
	}
}

// worktrees returns the worktrees the scanner found, cached briefly.
// r.mu is held.
func (r *Receiver) worktrees(ctx context.Context) []store.Worktree {
	now := r.now()
	if !r.wtsAt.IsZero() && now.Sub(r.wtsAt) < worktreeTTL && !now.Before(r.wtsAt) {
		return r.wts
	}
	wts, err := r.st.ListWorktrees(ctx, store.WorktreeFilter{})
	if err != nil {
		r.storeError("list worktrees", err)
		return r.wts
	}
	r.wts, r.wtsAt = wts, now
	return wts
}

// claudeWorktree returns the worktree a path is in under Claude Code's
// layout, <repo>/.claude/worktrees/<name>, and the branch Claude Code
// gives it (worktree-<name>), or "" when the path isn't in one.
func claudeWorktree(p string) (string, string) {
	const marker = "/.claude/worktrees/"
	i := strings.LastIndex(p, marker)
	if i < 0 {
		return "", ""
	}
	rest := p[i+len(marker):]
	name, _, _ := strings.Cut(rest, "/")
	if name == "" {
		return "", ""
	}
	return p[:i+len(marker)] + name, "worktree-" + name
}

// within reports whether p is dir or inside it, comparing paths as the
// worktree scanner does.
func within(p, dir string) bool {
	if p == "" || dir == "" {
		return false
	}
	p, dir = worktrees.PathKey(p), worktrees.PathKey(dir)
	if p == dir {
		return true
	}
	if !strings.HasSuffix(dir, "/") {
		dir += "/"
	}
	return strings.HasPrefix(p, dir)
}

// sameDir reports whether two paths are the same (both empty counts).
func sameDir(a, b string) bool {
	if a == "" || b == "" {
		return a == b
	}
	return worktrees.SamePath(a, b)
}

func lastElem(p string) string {
	p = strings.TrimRight(p, "/")
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// eventRow is an event's timeline row: its kind, the tool for a tool
// event, and a summary. agent_list snapshots get none.
func eventRow(e events.Event) store.Event {
	row := store.Event{SessionID: e.SessionID, AgentID: e.AgentID, Event: string(e.Kind), At: e.At}
	switch e.Kind {
	case events.SessionStart:
		row.Summary = e.Source
	case events.Prompt:
		row.Summary = e.OriginKind
	case events.TurnComplete:
		row.Summary = e.LastSaid
		if e.Reason != "" && e.Reason != events.ReasonAnswer {
			row.Summary = strings.TrimSuffix(e.Reason+": "+e.LastSaid, ": ")
		}
	case events.Tool:
		row.ToolName = e.Tool
		row.Summary = cmp.Or(e.Label, e.Tool)
	case events.NeedsYou:
		row.Summary = strings.TrimSuffix(e.Source+": "+e.Reason, ": ")
	case events.Error:
		row.Summary = e.Reason
	case events.WorktreeCreate:
		row.Summary = e.Name
	case events.SessionEnd:
		row.Summary = e.Reason
		if e.NewSessionID != "" {
			row.Summary += " → " + e.NewSessionID
		}
	}
	row.Summary = events.Truncate(row.Summary, store.MaxSummaryLen)
	row.ToolName = events.Truncate(row.ToolName, store.MaxToolNameLen)
	return row
}
