package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// This file holds the Needs you inbox (migration 0008): its
// items and their history. internal/inbox owns the rules (the states, the
// transitions, the timers and the sync); the store only keeps the rows,
// and gives the rules a transaction (UpdateInbox) so a sync pass and your
// clicks never overwrite each other.

// Inbox item states (inbox_items.state).
const (
	InboxOpen       = "open"
	InboxInProgress = "progress"
	InboxSnoozed    = "snoozed"
	InboxDone       = "done"
)

// Who made an inbox move (inbox_moves.by).
const (
	InboxByYou  = "you"
	InboxByAimd = "aimd"
)

// InboxItem is a row of inbox_items: one waiting episode. See the
// migration for what each column means.
type InboxItem struct {
	ID int64
	// Source is "session" or "pr"; Ref names what waits:
	// "session:<sessions.id>" or "owner/repo#123".
	Source, Ref string
	Kind        string
	State       string
	// Since is when the episode began, StateAt when State was last set.
	Since, StateAt time.Time
	// DueAt is when the item moves by itself (In progress back to Open, a
	// timed snooze ending); zero for none. Snooze is the snooze choice
	// while Snoozed.
	DueAt  time.Time
	Snooze string
	// EndedAt is when the episode ended (zero while it waits), and
	// EndReason why.
	EndedAt   time.Time
	EndReason string
	// SeenAt is when you first opened the item: zero for a new one.
	SeenAt time.Time
	// DismissedAt is the old dashboard's Dismiss the item has followed.
	DismissedAt time.Time

	// What it is, as of the last sync pass while it waited.
	Title, Subtitle string
	Detail, Sig     string
	PlanSlug, Pkg   string
	SessionKey      string
	JobID           string
	Repo, Branch    string
	Folder          string
	PR, PRURL       string
	Why             string

	CreatedAt time.Time
}

// Waiting reports whether the item's episode still lasts.
func (it InboxItem) Waiting() bool { return it.EndedAt.IsZero() }

// InboxMove is a row of inbox_moves: one entry of an item's history. A
// move between states has From and To set; a note (its episode changed or
// ended while the state stayed) has them equal. FromDue and FromSnooze are
// what a move left, which Undo restores.
type InboxMove struct {
	ID         int64
	ItemID     int64
	At         time.Time
	By         string // InboxByYou or InboxByAimd
	Text       string
	From, To   string
	FromDue    time.Time
	FromSnooze string
}

// InboxFilter narrows ListInbox.
type InboxFilter struct {
	// State keeps the items in one state; "" keeps all.
	State string
	// Waiting keeps only the items whose episode still lasts.
	Waiting bool
	// IDs keeps these items; nil keeps all.
	IDs []int64
}

const inboxColumns = `id, source, ref, kind, state, since, state_at, due_at, snooze, ended_at, end_reason,
	seen_at, dismissed_at, title, subtitle, detail, sig, plan_slug, pkg, session_key, job_id, repo,
	branch, folder, pr, pr_url, why, created_at`

func scanInbox(sc scanner) (InboxItem, error) {
	var it InboxItem
	err := sc.Scan(&it.ID, &it.Source, &it.Ref, &it.Kind, &it.State, msTime{&it.Since}, msTime{&it.StateAt},
		msTime{&it.DueAt}, &it.Snooze, msTime{&it.EndedAt}, &it.EndReason, msTime{&it.SeenAt}, msTime{&it.DismissedAt},
		&it.Title, &it.Subtitle, &it.Detail, &it.Sig, &it.PlanSlug, &it.Pkg, &it.SessionKey, &it.JobID, &it.Repo,
		&it.Branch, &it.Folder, &it.PR, &it.PRURL, &it.Why, msTime{&it.CreatedAt})
	return it, err
}

// inboxDB is a *sql.DB or *sql.Tx.
type inboxDB interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func listInbox(ctx context.Context, db inboxDB, f InboxFilter) ([]InboxItem, error) {
	q := `SELECT ` + inboxColumns + ` FROM inbox_items WHERE 1 = 1`
	var args []any
	if f.State != "" {
		q += ` AND state = ?`
		args = append(args, f.State)
	}
	if f.Waiting {
		q += ` AND ended_at IS NULL`
	}
	if f.IDs != nil {
		if len(f.IDs) == 0 {
			return nil, nil
		}
		q += ` AND id IN (?` + strings.Repeat(", ?", len(f.IDs)-1) + `)`
		for _, id := range f.IDs {
			args = append(args, id)
		}
	}
	q += ` ORDER BY since, id`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list inbox: %w", err)
	}
	defer rows.Close()
	var out []InboxItem
	for rows.Next() {
		it, err := scanInbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func inboxItem(ctx context.Context, db inboxDB, id int64) (InboxItem, error) {
	it, err := scanInbox(db.QueryRowContext(ctx, `SELECT `+inboxColumns+` FROM inbox_items WHERE id = ?`, id))
	if err != nil {
		return InboxItem{}, notFound(err, "inbox item %d", id)
	}
	return it, nil
}

func inboxMoves(ctx context.Context, db inboxDB, itemID int64) ([]InboxMove, error) {
	rows, err := db.QueryContext(ctx, `SELECT id, item_id, at, by, text, from_state, to_state, from_due, from_snooze
		FROM inbox_moves WHERE item_id = ? ORDER BY at, id`, itemID)
	if err != nil {
		return nil, fmt.Errorf("store: inbox history of %d: %w", itemID, err)
	}
	defer rows.Close()
	var out []InboxMove
	for rows.Next() {
		var m InboxMove
		if err := rows.Scan(&m.ID, &m.ItemID, msTime{&m.At}, &m.By, &m.Text, &m.From, &m.To, msTime{&m.FromDue}, &m.FromSnooze); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListInbox returns the inbox items f keeps, oldest episode first.
func (s *Store) ListInbox(ctx context.Context, f InboxFilter) ([]InboxItem, error) {
	return listInbox(ctx, s.r, f)
}

// InboxItem returns one item, or an error wrapping ErrNotFound.
func (s *Store) InboxItem(ctx context.Context, id int64) (InboxItem, error) {
	return inboxItem(ctx, s.r, id)
}

// InboxMoves returns an item's history, oldest first.
func (s *Store) InboxMoves(ctx context.Context, itemID int64) ([]InboxMove, error) {
	return inboxMoves(ctx, s.r, itemID)
}

// InboxCounts returns how many items are in each state.
func (s *Store) InboxCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT state, count(*) FROM inbox_items GROUP BY state`)
	if err != nil {
		return nil, fmt.Errorf("store: count inbox: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return nil, err
		}
		out[state] = n
	}
	return out, rows.Err()
}

// pruneInboxWhere is what PruneInbox deletes: Done items whose episode
// ended, both before the cutoff. A Done item whose episode still lasts is
// kept, or the next sync would open it again as new.
const pruneInboxWhere = `state = 'done' AND ended_at IS NOT NULL AND ended_at < ?1 AND state_at < ?1`

// InboxPrunable reports whether PruneInbox(before) would delete anything.
func (s *Store) InboxPrunable(ctx context.Context, before time.Time) (bool, error) {
	var n int
	err := s.r.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM inbox_items WHERE `+pruneInboxWhere+`)`, before.UnixMilli()).Scan(&n)
	return n == 1, err
}

// UpdateInbox runs fn in one write transaction and commits it if fn
// returns nil. Everything fn reads through tx is as of that transaction,
// so a sync pass and an action never undo each other's writes.
func (s *Store) UpdateInbox(ctx context.Context, fn func(tx *InboxTx) error) error {
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		return fn(&InboxTx{ctx: ctx, tx: tx, now: s.now})
	})
}

// InboxTx is the inbox inside an UpdateInbox transaction.
type InboxTx struct {
	ctx context.Context
	tx  *sql.Tx
	now func() time.Time
}

// List returns the items f keeps, oldest episode first.
func (t *InboxTx) List(f InboxFilter) ([]InboxItem, error) { return listInbox(t.ctx, t.tx, f) }

// Item returns one item, or an error wrapping ErrNotFound.
func (t *InboxTx) Item(id int64) (InboxItem, error) { return inboxItem(t.ctx, t.tx, id) }

// Moves returns an item's history, oldest first.
func (t *InboxTx) Moves(itemID int64) ([]InboxMove, error) { return inboxMoves(t.ctx, t.tx, itemID) }

// itemArgs are an item's columns after id, in inboxColumns order.
func (t *InboxTx) itemArgs(it InboxItem) []any {
	created := it.CreatedAt
	if created.IsZero() {
		created = t.now()
	}
	return []any{it.Source, it.Ref, it.Kind, it.State, ms(it.Since), ms(it.StateAt), ms(it.DueAt), it.Snooze,
		ms(it.EndedAt), it.EndReason, ms(it.SeenAt), ms(it.DismissedAt), it.Title, it.Subtitle, it.Detail, it.Sig,
		it.PlanSlug, it.Pkg, it.SessionKey, it.JobID, it.Repo, it.Branch, it.Folder, it.PR, it.PRURL, it.Why,
		created.UnixMilli()}
}

// checkInbox refuses an item the table can't hold.
func checkInbox(it InboxItem) error {
	switch {
	case it.Ref == "":
		return errors.New("empty ref")
	case it.Since.IsZero() || it.StateAt.IsZero():
		return errors.New("no since or state time")
	}
	switch it.State {
	case InboxOpen, InboxInProgress, InboxSnoozed, InboxDone:
		return nil
	}
	return fmt.Errorf("unknown state %q", it.State)
}

// Insert adds an item and returns its id. It.ID is ignored; a zero
// CreatedAt means now.
func (t *InboxTx) Insert(it InboxItem) (int64, error) {
	if err := checkInbox(it); err != nil {
		return 0, fmt.Errorf("store: insert inbox item %s: %w", it.Ref, err)
	}
	res, err := t.tx.ExecContext(t.ctx, `INSERT INTO inbox_items (`+strings.TrimPrefix(inboxColumns, "id, ")+`)
		VALUES (?`+strings.Repeat(", ?", 26)+`)`, t.itemArgs(it)...)
	if err != nil {
		return 0, fmt.Errorf("store: insert inbox item %s: %w", it.Ref, err)
	}
	return res.LastInsertId()
}

// Save writes every column of an existing item.
func (t *InboxTx) Save(it InboxItem) error {
	if err := checkInbox(it); err != nil {
		return fmt.Errorf("store: save inbox item %d: %w", it.ID, err)
	}
	res, err := t.tx.ExecContext(t.ctx, `UPDATE inbox_items SET source = ?, ref = ?, kind = ?, state = ?, since = ?,
		state_at = ?, due_at = ?, snooze = ?, ended_at = ?, end_reason = ?, seen_at = ?, dismissed_at = ?, title = ?,
		subtitle = ?, detail = ?, sig = ?, plan_slug = ?, pkg = ?, session_key = ?, job_id = ?, repo = ?, branch = ?,
		folder = ?, pr = ?, pr_url = ?, why = ?, created_at = ? WHERE id = ?`, append(t.itemArgs(it), it.ID)...)
	if err != nil {
		return fmt.Errorf("store: save inbox item %d: %w", it.ID, err)
	}
	return mustAffect(res, "inbox item %d", it.ID)
}

// AddMove appends an entry to an item's history and returns its id. A
// zero At means now.
func (t *InboxTx) AddMove(m InboxMove) (int64, error) {
	if m.At.IsZero() {
		m.At = t.now()
	}
	res, err := t.tx.ExecContext(t.ctx, `INSERT INTO inbox_moves (item_id, at, by, text, from_state, to_state, from_due, from_snooze)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, m.ItemID, m.At.UnixMilli(), m.By, m.Text, m.From, m.To, ms(m.FromDue), m.FromSnooze)
	if err != nil {
		return 0, fmt.Errorf("store: add inbox move to %d: %w", m.ItemID, err)
	}
	return res.LastInsertId()
}

// Prune deletes the Done items whose episode ended before the cutoff and
// that went Done before it too, with their history. It returns how many
// items it deleted.
func (t *InboxTx) Prune(before time.Time) (int, error) {
	res, err := t.tx.ExecContext(t.ctx, `DELETE FROM inbox_items WHERE `+pruneInboxWhere, before.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("store: prune inbox: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}
