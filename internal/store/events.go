package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// Length limits on event fields, in characters. They match the CHECK
// constraints on the events table and are a backstop for the rule that
// events hold summaries only: a caller that has more to say must shorten it
// first.
const (
	MaxEventLen    = 64
	MaxToolNameLen = 128
	MaxSummaryLen  = 300
)

// Event is a row of the events table: a short summary of one thing a
// session did, for the timeline and retros. It never holds a prompt, file
// contents or tool output; the table has no column that
// could.
type Event struct {
	ID        int64  // set by the store
	SessionID string // Claude Code's session id
	AgentID   string // "" for the main loop
	Event     string // e.g. "tool.call", "classic.Notification"
	ToolName  string
	Summary   string    // e.g. "Bash: make check"
	At        time.Time // zero means now
}

func (e Event) validate() error {
	switch {
	case e.SessionID == "":
		return errors.New("empty session id")
	case e.Event == "":
		return errors.New("empty event name")
	case utf8.RuneCountInString(e.Event) > MaxEventLen:
		return fmt.Errorf("event name longer than %d characters", MaxEventLen)
	case utf8.RuneCountInString(e.ToolName) > MaxToolNameLen:
		return fmt.Errorf("tool name longer than %d characters", MaxToolNameLen)
	case utf8.RuneCountInString(e.Summary) > MaxSummaryLen:
		return fmt.Errorf("summary longer than %d characters; events hold summaries only", MaxSummaryLen)
	}
	return nil
}

// InsertEvents stores events in one transaction: all of them or, if one is
// invalid, none. Each is checked against the Max* limits first.
func (s *Store) InsertEvents(ctx context.Context, events ...Event) error {
	for i, e := range events {
		if err := e.validate(); err != nil {
			return fmt.Errorf("store: event %d (%s): %w", i, e.Event, err)
		}
	}
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `
			INSERT INTO events (session_id, agent_id, event, tool_name, summary, at)
			VALUES (?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for i, e := range events {
			if _, err := stmt.ExecContext(ctx, e.SessionID, e.AgentID, e.Event, e.ToolName, e.Summary, s.stamp(e.At).UnixMilli()); err != nil {
				return fmt.Errorf("store: insert event %d (%s): %w", i, e.Event, err)
			}
		}
		return nil
	})
}

// ListSessionEvents returns a session's most recent events, newest first,
// at most limit of them (limit <= 0 means 100).
func (s *Store) ListSessionEvents(ctx context.Context, sessionID string, limit int) ([]Event, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.r.QueryContext(ctx, `
		SELECT id, session_id, agent_id, event, tool_name, summary, at
		FROM events WHERE session_id = ?
		ORDER BY at DESC, id DESC
		LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.SessionID, &e.AgentID, &e.Event, &e.ToolName, &e.Summary, msTime{&e.At}); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// EventLoop names one loop whose events to read: a session's main loop
// (AgentID "") or one of its subagents.
type EventLoop struct {
	SessionID string
	AgentID   string
}

// EventFilter narrows ListLoopEvents.
type EventFilter struct {
	// Loops are the loops to read; none reads nothing.
	Loops []EventLoop
	// Exclude leaves out events of these kinds (Event.Event).
	Exclude []string
	// Limit caps the rows, newest first; <= 0 means 200.
	Limit int
}

// maxEventLoops bounds the loops one ListLoopEvents call reads, which
// keeps its query's parameters well under SQLite's limit.
const maxEventLoops = 1000

// ListLoopEvents returns the events of the given loops, newest first, at
// most f.Limit of them: a plan's timeline reads the loops linked to its
// packages. Loops past the first 1000 are ignored.
func (s *Store) ListLoopEvents(ctx context.Context, f EventFilter) ([]Event, error) {
	if len(f.Loops) == 0 {
		return nil, nil
	}
	loops := f.Loops
	if len(loops) > maxEventLoops {
		loops = loops[:maxEventLoops]
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 200
	}
	var q strings.Builder
	q.WriteString(`SELECT id, session_id, agent_id, event, tool_name, summary, at FROM events WHERE (`)
	args := make([]any, 0, 2*len(loops)+len(f.Exclude)+1)
	for i, l := range loops {
		if i > 0 {
			q.WriteString(` OR `)
		}
		q.WriteString(`(session_id = ? AND agent_id = ?)`)
		args = append(args, l.SessionID, l.AgentID)
	}
	q.WriteString(`)`)
	if len(f.Exclude) > 0 {
		q.WriteString(` AND event NOT IN (?` + strings.Repeat(`, ?`, len(f.Exclude)-1) + `)`)
		for _, e := range f.Exclude {
			args = append(args, e)
		}
	}
	q.WriteString(` ORDER BY at DESC, id DESC LIMIT ?`)
	args = append(args, limit)
	rows, err := s.r.QueryContext(ctx, q.String(), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.SessionID, &e.AgentID, &e.Event, &e.ToolName, &e.Summary, msTime{&e.At}); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PurgeEvents deletes events older than retentionDays days before now (the
// event_retention_days setting in config.toml) and returns how many it
// deleted. It deletes in batches, each its own short write, so a large
// purge never holds the write lock for long.
func (s *Store) PurgeEvents(ctx context.Context, now time.Time, retentionDays int) (int64, error) {
	if retentionDays < 1 {
		return 0, fmt.Errorf("store: purge events: retention must be at least 1 day, got %d", retentionDays)
	}
	cutoff := s.stamp(now).AddDate(0, 0, -retentionDays).UnixMilli()
	var total int64
	for {
		res, err := s.w.ExecContext(ctx, `
			DELETE FROM events WHERE id IN (
				SELECT id FROM events WHERE at < ? ORDER BY at LIMIT ?)`,
			cutoff, s.purgeBatch)
		if err != nil {
			return total, fmt.Errorf("store: purge events: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return total, err
		}
		total += n
		if n < int64(s.purgeBatch) {
			return total, nil
		}
	}
}
