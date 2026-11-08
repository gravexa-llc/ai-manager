// Package inbox is the model of the dashboard's Needs you inbox.
//
// # Items
//
// An item is one waiting episode: a monitored session that starts waiting
// on you (a permission prompt, a question, a plan approval, a background
// job waiting for a reply), or a PR that starts needing you, or a plan's
// provisional package whose row is still missing from the plan file, or a
// registered plan's package that is your step and ready: yours (its Owner
// cell says you, or its Needs cell has a "you:" step) and everything it
// needs is done. It starts when the thing starts waiting and ends when it
// stops; a new episode is a new item. Items are rows of the store's
// inbox_items (migration 0008), so their ids are stable across aimd
// restarts and a notification or a link can name one
// (/app/inbox?item=<id>).
//
// An episode is named by what waits (its ref: a session row,
// "session:<sessions.id>", a PR, "owner/repo#123", a provisional
// package, PlanRowRef's "planrow:<slug>/<ID>", or your step,
// YourStepRef's "step:<slug>/<ID>") and, for a session,
// by when the wait began (sessions.needs_you_since). A sync pass matches
// what waits now (a Snapshot) to the items still waiting:
//
//   - the same ref, the same kind and a session wait that didn't begin
//     later: the same episode. A poll's generic reason ("permission
//     prompt") and the mod's richer one ("permission prompt: Bash: …") are
//     the same episode, either way round, and an earlier start (the mod's
//     time for a wait the poll saw first) keeps the item.
//   - the same ref with another kind, or a session wait that began later:
//     the old episode ended and a new one began.
//   - a waiting item whose ref waits no more: its episode ended.
//
// # States
//
// Open (waiting, not handled), In progress (you clicked Go), Snoozed and
// Done. The moves:
//
//   - Go: Open or In progress → In progress, due back to Open after
//     Settings.BackToOpen. If the episode still waits then, a pass moves
//     it back to Open.
//   - Snooze for 15 minutes, 1 hour, until tomorrow 9am (local time), or
//     until it changes. A pass moves it back to Open when its time comes
//     or when the episode changes (its signature, Episode.Sig, changes),
//     whichever is first.
//   - Done, by you. Or by aimd when the episode ends: an Open, In progress
//     or Snoozed item goes to Done with the reason in its history.
//   - Back to Open, from In progress, Snoozed or Done; and Undo of your
//     last move, which restores the state, due time and snooze it left.
//
// The old dashboard's Dismiss (store.DismissNeedsYou) counts as Done once:
// a pass moves the item to Done when it sees a newer dismissal, and moving
// it back to Open sticks.
//
// Due times are stored, and every pass applies those that are due, so the
// timers hold across restarts. Done items whose episode ended are kept
// Settings.KeepDone (30 days) and then pruned.
package inbox

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/store"
)

// States, as stored.
const (
	Open       = store.InboxOpen
	InProgress = store.InboxInProgress
	Snoozed    = store.InboxSnoozed
	Done       = store.InboxDone
)

// States lists the states in tab order.
var States = []string{Open, InProgress, Snoozed, Done}

// StateLabel names a state as its tab does.
func StateLabel(state string) string {
	switch state {
	case Open:
		return "Open"
	case InProgress:
		return "In progress"
	case Snoozed:
		return "Snoozed"
	case Done:
		return "Done"
	}
	return state
}

// Sources: what an item's episode comes from.
const (
	SourceSession = "session"
	SourcePR      = "pr"
	// SourcePlan is a registered plan's package: a provisional one whose
	// row is still missing from the plan file (KindPlanRow), or your step,
	// ready (KindYourStep).
	SourcePlan = "plan"
)

// Kinds of item.
const (
	// From monitored sessions.
	KindPermission = "permission"
	KindQuestion   = "question"
	KindPlan       = "plan"
	KindReply      = "reply" // a background job ended its turn asking for a reply (`blocked`)
	KindPrompt     = "prompt"
	// From PRs (internal/prwait): your review is requested, or it is
	// ready to merge and the merge is yours.
	KindReview = "review"
	KindMerge  = "merge"
	// From plans: a provisional package's row is still
	// missing from the plan file a while after its work began.
	KindPlanRow = "plan_row"
	// From plans: a package that is yours (Owner "you", or a "you:" step
	// in its Needs cell) is ready, as the plan page's "needs you" badge
	// says.
	KindYourStep = "your_step"
)

// KindLabel names a kind as the inbox shows it.
func KindLabel(kind string) string {
	switch kind {
	case KindPermission:
		return "Permission"
	case KindQuestion:
		return "Question"
	case KindPlan:
		return "Plan approval"
	case KindReply:
		return "Waiting for your reply"
	case KindPrompt:
		return "Waiting on you"
	case KindReview:
		return "Review requested"
	case KindMerge:
		return "Ready to merge"
	case KindPlanRow:
		return "Plan row missing"
	case KindYourStep:
		return "Your step"
	}
	return kind
}

// sessionKind reports whether kind comes from a session.
func sessionKind(kind string) bool {
	switch kind {
	case KindPermission, KindQuestion, KindPlan, KindReply, KindPrompt:
		return true
	}
	return false
}

// SessionRef is the ref of a session row's episodes.
func SessionRef(rowID int64) string { return "session:" + strconv.FormatInt(rowID, 10) }

// PlanRowAfter is how long a provisional package may go
// without its row in the plan file before it is a Needs you item, if its
// plan has a monitored lead: long enough for the lead to add the row after
// the mod's note.
const PlanRowAfter = 10 * time.Minute

// PlanRowRef is the ref of a provisional package's missing-row episodes:
// "planrow:<slug>/<ID>", the slug in lower case and the ID in upper case.
func PlanRowRef(slug, id string) string {
	return "planrow:" + strings.ToLower(slug) + "/" + strings.ToUpper(id)
}

// YourStepRef is the ref of a package's your-step episodes:
// "step:<slug>/<ID>", cased as PlanRowRef cases it.
func YourStepRef(slug, id string) string {
	return "step:" + strings.ToLower(slug) + "/" + strings.ToUpper(id)
}

// ItemURL is an item's page on aimd's dashboard: where a notification or a
// plan summary about it links.
func ItemURL(id int64) string { return "/app/inbox?item=" + strconv.FormatInt(id, 10) }

// Core is a session's needs-you reason without the subagent that asks:
// "agent a04…: permission prompt: Bash: x" → "permission prompt: Bash: x".
func Core(reason string) string {
	_, rest := splitAgent(reason)
	return rest
}

// AgentOf is the subagent a session's needs-you reason names, or "".
func AgentOf(reason string) string {
	agent, _ := splitAgent(reason)
	return agent
}

func splitAgent(reason string) (agent, rest string) {
	if after, ok := strings.CutPrefix(reason, "agent "); ok {
		if id, more, ok := strings.Cut(after, ": "); ok && id != "" && !strings.Contains(id, " ") {
			return id, more
		}
	}
	return "", reason
}

// SessionKind is the kind of a session's wait, from its needs-you reason:
// the poller's `waitingFor` ("permission prompt", "input needed", "dialog
// open", …), the mod's words ("question: …", "plan approval: …"), or
// "reply" for a background job waiting for a reply. A reason it can't name
// is KindPrompt.
func SessionKind(reason string) string {
	r := strings.ToLower(Core(reason))
	has := func(p string) bool { return r == p || strings.HasPrefix(r, p+":") }
	switch {
	case r == "reply":
		return KindReply
	case has("permission prompt"), has("sandbox request"):
		return KindPermission
	case has("question"), has("input needed"):
		return KindQuestion
	case has("plan approval"), has("goal proposal"):
		return KindPlan
	}
	return KindPrompt
}

// Settings are the inbox's timings: Defaults, or what the Settings page
// set.
type Settings struct {
	// BackToOpen is how long after Go an item that still waits returns
	// to Open.
	BackToOpen time.Duration
	// KeepDone is how long a Done item whose episode ended is kept.
	KeepDone time.Duration
	// Morning is the hour "tomorrow" snoozes end at, in Location.
	Morning  int
	Location *time.Location
}

// Defaults are the spec's timings: back to Open after 10 minutes, Done
// kept 30 days, snoozes until 9am local time.
func Defaults() Settings {
	return Settings{BackToOpen: 10 * time.Minute, KeepDone: 30 * 24 * time.Hour, Morning: 9, Location: time.Local}
}

func (s Settings) loc() *time.Location {
	if s.Location == nil {
		return time.Local
	}
	return s.Location
}

// Snooze choices (store.InboxItem.Snooze).
const (
	Snooze15m      = "15m"
	Snooze1h       = "1h"
	SnoozeTomorrow = "tomorrow"
	SnoozeChange   = "change"
)

// SnoozeChoice is one entry of the Snooze menu.
type SnoozeChoice struct {
	Key   string
	Label string // the menu's text: "15 minutes"
	Text  string // after "Snoozed": "for 15 minutes"
}

// Snoozes lists the Snooze menu, in order.
func (s Settings) Snoozes() []SnoozeChoice {
	morning := hourText(s.Morning)
	return []SnoozeChoice{
		{Snooze15m, "15 minutes", "for 15 minutes"},
		{Snooze1h, "1 hour", "for 1 hour"},
		{SnoozeTomorrow, "Until tomorrow " + morning, "until tomorrow " + morning},
		{SnoozeChange, "Until it changes", "until it changes"},
	}
}

// Snooze returns the choice with the key.
func (s Settings) Snooze(key string) (SnoozeChoice, bool) {
	i := slices.IndexFunc(s.Snoozes(), func(c SnoozeChoice) bool { return c.Key == key })
	if i < 0 {
		return SnoozeChoice{}, false
	}
	return s.Snoozes()[i], true
}

// hourText writes an hour of the day as "9am", "12pm" or "6pm".
func hourText(h int) string {
	switch {
	case h == 0:
		return "12am"
	case h < 12:
		return strconv.Itoa(h) + "am"
	case h == 12:
		return "12pm"
	}
	return strconv.Itoa(h-12) + "pm"
}

// SnoozeUntil is when a snooze made at now ends: zero for "until it
// changes". "Tomorrow" is the next day at Settings.Morning, local time;
// before 4am it is still the night before, so it is the same day's
// morning.
func (s Settings) SnoozeUntil(key string, now time.Time) (time.Time, error) {
	switch key {
	case Snooze15m:
		return now.Add(15 * time.Minute), nil
	case Snooze1h:
		return now.Add(time.Hour), nil
	case SnoozeChange:
		return time.Time{}, nil
	case SnoozeTomorrow:
		l := now.In(s.loc())
		day := l
		if l.Hour() >= 4 {
			day = l.AddDate(0, 0, 1)
		}
		return time.Date(day.Year(), day.Month(), day.Day(), s.Morning, 0, 0, 0, s.loc()), nil
	}
	return time.Time{}, &Refusal{"There is no snooze " + strconv.Quote(key) + "."}
}

// ---- Episodes and sync ----

// Episode is something waiting on you now, as the caller (the web
// layer) works it out from the sessions, the PRs, the plans and the
// monitoring.
type Episode struct {
	Source string // SourceSession, SourcePR or SourcePlan
	Ref    string // SessionRef, store.PRRef, PlanRowRef or YourStepRef
	Kind   string
	// Since is when the wait began: the session's needs_you_since; for a
	// PR, what the caller counts from.
	Since time.Time
	// Detail is what it waits on, as shown; Sig is what "it changed"
	// compares (a session's Core reason; a PR's head, CI and review; your
	// step's "you:" note).
	Detail, Sig string
	// DismissedAt is the old dashboard's Dismiss of this wait, if any.
	DismissedAt time.Time

	Title, Subtitle string
	PlanSlug, Pkg   string
	SessionKey      string
	JobID           string
	Repo, Branch    string
	Folder          string
	PR, PRURL       string
	Why             string
}

// Snapshot is what waits now.
type Snapshot struct {
	Episodes []Episode
	// Stopped says, for refs that are known but don't wait, why: "it
	// stopped waiting", "merged on GitHub". A waiting item whose ref is in
	// neither list is gone: its session ended, or its PR is no longer
	// stored.
	Stopped map[string]string
}

// goneReason is why an item's episode ended when its ref is gone.
func goneReason(it store.InboxItem) string {
	switch {
	case it.Source == SourcePR:
		return "the PR is gone"
	case it.Kind == KindYourStep:
		return "the package is gone: its plan was unregistered, or its row left the plan file"
	case it.Source == SourcePlan:
		return "the provisional package is gone"
	}
	return "the session ended"
}

// Store is the part of *store.Store the inbox uses.
type Store interface {
	ListInbox(ctx context.Context, f store.InboxFilter) ([]store.InboxItem, error)
	InboxPrunable(ctx context.Context, before time.Time) (bool, error)
	UpdateInbox(ctx context.Context, fn func(tx *store.InboxTx) error) error
}

var _ Store = (*store.Store)(nil)

// Result says what a sync pass did.
type Result struct {
	// Created counts new items, Updated the waiting items it changed
	// (states, labels or history), Pruned the Done items it deleted.
	Created, Updated, Pruned int
}

// Changed reports whether the pass wrote anything.
func (r Result) Changed() bool { return r.Created+r.Updated+r.Pruned > 0 }

// Sync brings the items in step with snap at now: it opens items for new
// episodes, follows the waiting ones (their labels, a change, the old
// dashboard's Dismiss), ends the ones that stopped waiting, applies the
// due timers and prunes old Done items. It writes only when something
// changed: it plans from a read first, then plans again inside the write
// transaction, so a move you make meanwhile is never undone.
func Sync(ctx context.Context, st Store, snap Snapshot, now time.Time, set Settings) (Result, error) {
	cutoff := now.Add(-set.KeepDone)
	waiting, err := st.ListInbox(ctx, store.InboxFilter{Waiting: true})
	if err != nil {
		return Result{}, err
	}
	prunable, err := st.InboxPrunable(ctx, cutoff)
	if err != nil {
		return Result{}, err
	}
	if p := reconcile(waiting, snap, now); len(p.creates) == 0 && len(p.saves) == 0 && !prunable {
		return Result{}, nil
	}
	var res Result
	err = st.UpdateInbox(ctx, func(tx *store.InboxTx) error {
		res = Result{}
		waiting, err := tx.List(store.InboxFilter{Waiting: true})
		if err != nil {
			return err
		}
		p := reconcile(waiting, snap, now)
		// Ended items first: a new episode of the same ref needs the old
		// one's place in the index of waiting items.
		for _, u := range p.saves {
			if err := tx.Save(u.item); err != nil {
				return err
			}
			if err := addMoves(tx, u.item.ID, u.moves); err != nil {
				return err
			}
			res.Updated++
		}
		for _, c := range p.creates {
			id, err := tx.Insert(c.item)
			if err != nil {
				return err
			}
			if err := addMoves(tx, id, c.moves); err != nil {
				return err
			}
			res.Created++
		}
		res.Pruned, err = tx.Prune(cutoff)
		return err
	})
	if err != nil {
		return Result{}, fmt.Errorf("inbox: sync: %w", err)
	}
	return res, nil
}

func addMoves(tx *store.InboxTx, id int64, moves []store.InboxMove) error {
	for _, m := range moves {
		m.ItemID = id
		if _, err := tx.AddMove(m); err != nil {
			return err
		}
	}
	return nil
}

// change is an item to write with the history entries to add.
type change struct {
	item  store.InboxItem
	moves []store.InboxMove
}

type plan struct {
	creates []change
	saves   []change
}

// reconcile plans a pass: what to create and what to save.
func reconcile(waiting []store.InboxItem, snap Snapshot, now time.Time) plan {
	var p plan
	byRef := map[string]store.InboxItem{}
	for _, it := range waiting {
		byRef[it.Ref] = it
	}
	matched := map[string]bool{}
	for _, ep := range snap.Episodes {
		if ep.Ref == "" || matched[ep.Ref] {
			continue
		}
		matched[ep.Ref] = true
		if it, ok := byRef[ep.Ref]; ok {
			if same(it, ep) {
				if c, dirty := follow(it, ep, now); dirty {
					p.saves = append(p.saves, c)
				}
				continue
			}
			p.saves = append(p.saves, end(it, "a new wait began: "+strings.ToLower(KindLabel(ep.Kind)), now))
		}
		p.creates = append(p.creates, create(ep, now))
	}
	for _, it := range waiting {
		if matched[it.Ref] {
			continue
		}
		reason := snap.Stopped[it.Ref]
		if reason == "" {
			reason = goneReason(it)
		}
		p.saves = append(p.saves, end(it, reason, now))
	}
	return p
}

// same reports whether ep is the episode it is.
func same(it store.InboxItem, ep Episode) bool {
	if it.Source != ep.Source {
		return false
	}
	kinds := it.Kind == ep.Kind ||
		it.Kind == KindPrompt && sessionKind(ep.Kind) ||
		ep.Kind == KindPrompt && sessionKind(it.Kind)
	if !kinds {
		return false
	}
	return ep.Source != SourceSession || !ep.Since.After(it.Since)
}

// refines reports whether signature b says more than a about the same
// wait: "permission prompt" → "permission prompt: Bash: x".
func refines(a, b string) bool { return a != "" && strings.HasPrefix(b, a+": ") }

// moveTo records a move of it to state, by who, and makes it. The move
// keeps what it left, for Undo. Leaving In progress or Snoozed clears the
// due time and the snooze; the caller sets them for the new state.
func moveTo(it *store.InboxItem, state, by, text string, now time.Time) store.InboxMove {
	m := store.InboxMove{ItemID: it.ID, At: now, By: by, Text: text, From: it.State, To: state, FromDue: it.DueAt, FromSnooze: it.Snooze}
	it.State, it.StateAt = state, now
	it.DueAt, it.Snooze = time.Time{}, ""
	return m
}

// note records what happened to an item's episode without a move.
func note(it store.InboxItem, text string, now time.Time) store.InboxMove {
	return store.InboxMove{ItemID: it.ID, At: now, By: store.InboxByAimd, Text: text, From: it.State, To: it.State}
}

// labels copies ep's labels onto it, reporting whether any changed.
func labels(it *store.InboxItem, ep Episode) bool {
	dirty := false
	set := func(dst *string, v string) {
		if *dst != v {
			*dst, dirty = v, true
		}
	}
	set(&it.Title, ep.Title)
	set(&it.Subtitle, ep.Subtitle)
	set(&it.PlanSlug, ep.PlanSlug)
	set(&it.Pkg, ep.Pkg)
	set(&it.SessionKey, ep.SessionKey)
	set(&it.JobID, ep.JobID)
	set(&it.Repo, ep.Repo)
	set(&it.Branch, ep.Branch)
	set(&it.Folder, ep.Folder)
	set(&it.PR, ep.PR)
	set(&it.PRURL, ep.PRURL)
	set(&it.Why, ep.Why)
	return dirty
}

// create is a new Open item for ep, with its first history entry (at the
// time the wait began), and Done at once if the old dashboard dismissed
// it.
func create(ep Episode, now time.Time) change {
	since := ep.Since
	if since.IsZero() || since.After(now) {
		since = now
	}
	it := store.InboxItem{Source: ep.Source, Ref: ep.Ref, Kind: ep.Kind, State: Open, Since: since, StateAt: now,
		Detail: ep.Detail, Sig: ep.Sig, CreatedAt: now}
	labels(&it, ep)
	first := store.InboxMove{At: since, By: store.InboxByAimd, Text: startText(ep), To: Open}
	c := change{item: it, moves: []store.InboxMove{first}}
	if !ep.DismissedAt.IsZero() {
		c.item.DismissedAt = ep.DismissedAt
		c.moves = append(c.moves, moveTo(&c.item, Done, store.InboxByYou, "Done: dismissed on the old dashboard", now))
	}
	return c
}

// startText is an item's first history entry.
func startText(ep Episode) string {
	what := ep.Detail
	switch ep.Source {
	case SourcePR:
		what = cmp.Or(ep.Why, KindLabel(ep.Kind))
		return "Started needing you: " + strings.ToLower(what)
	case SourcePlan:
		// Its why names the package: only its first letter goes down.
		return "Started needing you: " + lowerFirst(cmp.Or(ep.Why, KindLabel(ep.Kind)))
	}
	return "Started waiting: " + cmp.Or(what, strings.ToLower(KindLabel(ep.Kind)))
}

// follow updates a waiting item from its episode: its labels, a richer
// reason, a change (which wakes a snooze), the old dashboard's Dismiss,
// and the due timers. dirty is false when nothing changed.
func follow(it store.InboxItem, ep Episode, now time.Time) (change, bool) {
	var moves []store.InboxMove
	dirty := labels(&it, ep)
	if ep.Since.Before(it.Since) {
		it.Since, dirty = ep.Since, true
	}
	switch {
	case it.Kind == KindPrompt && ep.Kind != KindPrompt:
		// The mod named what the poll couldn't: the same wait, told better.
		it.Kind, it.Sig, it.Detail, dirty = ep.Kind, ep.Sig, ep.Detail, true
	case ep.Kind == KindPrompt && it.Kind != KindPrompt, ep.Sig == it.Sig, refines(ep.Sig, it.Sig):
		// The same wait, told less well: keep what we know.
		if ep.Sig == it.Sig && ep.Detail != it.Detail {
			it.Detail, dirty = ep.Detail, true
		}
	case refines(it.Sig, ep.Sig):
		it.Sig, it.Detail, dirty = ep.Sig, ep.Detail, true
	default:
		it.Sig, it.Detail, dirty = ep.Sig, ep.Detail, true
		what := "It changed"
		if ep.Detail != "" {
			what += ": " + ep.Detail
		}
		if it.State == Snoozed {
			moves = append(moves, moveTo(&it, Open, store.InboxByAimd, "Back to Open: "+lowerFirst(what), now))
		} else {
			moves = append(moves, note(it, what, now))
		}
	}
	if !ep.DismissedAt.IsZero() && ep.DismissedAt.After(it.DismissedAt) {
		it.DismissedAt, dirty = ep.DismissedAt, true
		if it.State != Done {
			moves = append(moves, moveTo(&it, Done, store.InboxByYou, "Done: dismissed on the old dashboard", now))
		}
	}
	if !it.DueAt.IsZero() && !now.Before(it.DueAt) {
		switch it.State {
		case InProgress:
			moves = append(moves, moveTo(&it, Open, store.InboxByAimd, "Back to Open: still waiting", now))
		case Snoozed:
			moves = append(moves, moveTo(&it, Open, store.InboxByAimd, "Back to Open: the snooze ended", now))
		}
	}
	return change{item: it, moves: moves}, dirty || len(moves) > 0
}

// end ends an item's episode: Done, unless it is Done already, with the
// reason in its history.
func end(it store.InboxItem, reason string, now time.Time) change {
	it.EndedAt, it.EndReason = now, reason
	var m store.InboxMove
	if it.State == Done {
		m = note(it, "Stopped waiting: "+reason, now)
	} else {
		m = moveTo(&it, Done, store.InboxByAimd, "Done: "+reason, now)
	}
	return change{item: it, moves: []store.InboxMove{m}}
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// ---- Your moves ----

// Actions you can take on an item.
const (
	ActGo     = "go"
	ActSnooze = "snooze"
	ActDone   = "done"
	ActOpen   = "open" // back to Open: Unsnooze, Move back to Open
	ActUndo   = "undo"
	ActSeen   = "seen" // you opened it: it is no longer new
)

// Action is one move you make.
type Action struct {
	Do string
	// Snooze is the snooze choice (ActSnooze).
	Snooze string
	// Move is the history entry to undo (ActUndo): the move a toast's
	// Undo names.
	Move int64
	// What says what Go did, for the history: "switched to Terminal",
	// "opened on GitHub".
	What string
}

// Refusal is an action the item's state doesn't allow. Its text says why,
// for a toast.
type Refusal struct{ Reason string }

func (r *Refusal) Error() string { return r.Reason }

// ErrNotFound is returned for an item that doesn't exist (or was pruned).
var ErrNotFound = store.ErrNotFound

// Act makes your move on item id at now, in one transaction, and returns
// the item as it is now and the history entry it added (zero for
// ActSeen, and for ActSeen on an item opened before). A move the state
// doesn't allow is a *Refusal.
func Act(ctx context.Context, st Store, id int64, a Action, now time.Time, set Settings) (store.InboxItem, store.InboxMove, error) {
	var it store.InboxItem
	var mv store.InboxMove
	err := st.UpdateInbox(ctx, func(tx *store.InboxTx) error {
		var err error
		if it, err = tx.Item(id); err != nil {
			return err
		}
		var last *store.InboxMove
		if a.Do == ActUndo {
			moves, err := tx.Moves(id)
			if err != nil {
				return err
			}
			for i := len(moves) - 1; i >= 0; i-- {
				if moves[i].From != moves[i].To {
					last = &moves[i]
					break
				}
			}
		}
		write, m, err := apply(&it, a, last, now, set)
		if err != nil || !write {
			return err
		}
		if err := tx.Save(it); err != nil {
			return err
		}
		if m.Text == "" {
			return nil
		}
		m.ItemID = id
		m.ID, err = tx.AddMove(m)
		mv = m
		return err
	})
	return it, mv, err
}

// apply makes a move on it. last is its latest move (ActUndo only). write
// is false when nothing changed.
func apply(it *store.InboxItem, a Action, last *store.InboxMove, now time.Time, set Settings) (write bool, m store.InboxMove, err error) {
	refuse := func(why string) (bool, store.InboxMove, error) { return false, store.InboxMove{}, &Refusal{why} }
	stopped := func() (bool, store.InboxMove, error) {
		return refuse("It stopped waiting (" + it.EndReason + "). Mark it done, or move it back to Open.")
	}
	in := func(states ...string) bool { return slices.Contains(states, it.State) }
	switch a.Do {
	case ActSeen:
		if !it.SeenAt.IsZero() {
			return false, m, nil
		}
		it.SeenAt = now
		return true, m, nil

	case ActGo:
		if !it.Waiting() {
			return stopped()
		}
		if !in(Open, InProgress) {
			return refuse("Go moves an Open item to In progress; this one is " + StateLabel(it.State) + ".")
		}
		text := "Go"
		if a.What != "" {
			text += ": " + a.What
		}
		m = moveTo(it, InProgress, store.InboxByYou, text, now)
		it.DueAt = now.Add(set.BackToOpen)

	case ActSnooze:
		if !it.Waiting() {
			return stopped()
		}
		if !in(Open, InProgress, Snoozed) {
			return refuse("A Done item can't be snoozed; move it back to Open first.")
		}
		c, ok := set.Snooze(a.Snooze)
		if !ok {
			return refuse("There is no snooze " + strconv.Quote(a.Snooze) + ".")
		}
		until, err := set.SnoozeUntil(c.Key, now)
		if err != nil {
			return false, m, err
		}
		m = moveTo(it, Snoozed, store.InboxByYou, "Snoozed "+c.Text, now)
		it.DueAt, it.Snooze = until, c.Key

	case ActDone:
		if it.State == Done {
			return refuse("It is Done already.")
		}
		m = moveTo(it, Done, store.InboxByYou, "Done", now)

	case ActOpen:
		text := map[string]string{InProgress: "Back to Open", Snoozed: "Unsnoozed", Done: "Moved back to Open"}[it.State]
		if text == "" {
			return refuse("It is Open already.")
		}
		m = moveTo(it, Open, store.InboxByYou, text, now)

	case ActUndo:
		if last == nil || last.ID != a.Move || last.By != store.InboxByYou || last.To != it.State || last.From == "" {
			return refuse("It moved again since; there is nothing to undo.")
		}
		if !it.Waiting() && (last.From == InProgress || last.From == Snoozed) {
			return stopped()
		}
		m = moveTo(it, last.From, store.InboxByYou, "Undone: back to "+StateLabel(last.From), now)
		it.DueAt, it.Snooze = last.FromDue, last.FromSnooze

	default:
		return refuse("There is no action " + strconv.Quote(a.Do) + ".")
	}
	if it.SeenAt.IsZero() {
		it.SeenAt = now // you handled it: it is no longer new
	}
	return true, m, nil
}

// IsRefusal reports whether err is a *Refusal, and returns it.
func IsRefusal(err error) (*Refusal, bool) {
	var r *Refusal
	ok := errors.As(err, &r)
	return r, ok
}

// ---- Grouping ----

// Group is the items of one plan, oldest episode first. Plan is "" for
// the items on no plan ("No plan · watched").
type Group struct {
	Plan  string
	Items []store.InboxItem
}

// Groups groups items by plan: the plans in the order given (the
// registered plans), then other plan slugs by name, then the items on no
// plan. Within a group the oldest episode comes first.
func Groups(items []store.InboxItem, plans []string) []Group {
	rank := map[string]int{}
	for i, p := range plans {
		if _, ok := rank[strings.ToLower(p)]; !ok {
			rank[strings.ToLower(p)] = i
		}
	}
	by := map[string]*Group{}
	var order []string
	for _, it := range items {
		k := strings.ToLower(it.PlanSlug)
		g := by[k]
		if g == nil {
			g = &Group{Plan: it.PlanSlug}
			by[k] = g
			order = append(order, k)
		}
		g.Items = append(g.Items, it)
	}
	slices.SortFunc(order, func(a, b string) int {
		if (a == "") != (b == "") {
			if a == "" {
				return 1
			}
			return -1
		}
		ra, oka := rank[a]
		rb, okb := rank[b]
		switch {
		case oka && okb:
			return cmp.Compare(ra, rb)
		case oka != okb:
			if oka {
				return -1
			}
			return 1
		}
		return cmp.Compare(a, b)
	})
	out := make([]Group, 0, len(order))
	for _, k := range order {
		g := by[k]
		slices.SortStableFunc(g.Items, func(a, b store.InboxItem) int {
			return cmp.Or(a.Since.Compare(b.Since), cmp.Compare(a.ID, b.ID))
		})
		out = append(out, *g)
	}
	return out
}
