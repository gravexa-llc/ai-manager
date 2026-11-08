package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// This file holds the sessions you chose to track (migration 0006): the
// plan leads and the watched sessions. internal/monitor reads
// them, adds the plan agents it works out, and decides what the needs-you
// bar and the notifications follow.

// Why a session is monitored explicitly (monitored_sessions.reason).
const (
	// MonitorPlan: the session registered a plan (POST /api/plans with its
	// session_id).
	MonitorPlan = "plan"
	// MonitorWatch: you asked to watch it (/aim watch, or Watch on the
	// dashboard).
	MonitorWatch = "watch"
)

// MonitoredSession is a row of monitored_sessions.
type MonitoredSession struct {
	ID int64
	// SessionID is Claude Code's session id. The store carries it over to
	// the new id after a /clear or a background job's respawn
	// (MoveSessionID, UpsertSessions).
	SessionID string
	// Reason is MonitorPlan or MonitorWatch.
	Reason string
	// PlanID is the plan a MonitorPlan row's session registered, and
	// PlanSlug its slug (read only). Both are zero for MonitorWatch.
	PlanID   int64
	PlanSlug string
	// Since is when it started: the first registration from the session,
	// or the watch.
	Since time.Time
}

const monitoredColumns = `m.id, m.session_id, m.reason, coalesce(m.plan_id, 0), coalesce(p.slug, ''), m.since`

func scanMonitored(sc scanner) (MonitoredSession, error) {
	var m MonitoredSession
	err := sc.Scan(&m.ID, &m.SessionID, &m.Reason, &m.PlanID, &m.PlanSlug, msTime{&m.Since})
	return m, err
}

// WatchSession marks a session watched since at (zero means now). Watching
// a session that is watched already keeps the first watch. It returns the
// watch, and whether this call made it.
func (s *Store) WatchSession(ctx context.Context, sessionID string, at time.Time) (MonitoredSession, bool, error) {
	if strings.TrimSpace(sessionID) == "" {
		return MonitoredSession{}, false, errors.New("store: watch session: empty session id")
	}
	var m MonitoredSession
	var created bool
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO monitored_sessions (session_id, reason, since) VALUES (?, 'watch', ?)
			ON CONFLICT (session_id) WHERE reason = 'watch' DO NOTHING`,
			sessionID, s.stamp(at).UnixMilli())
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		created = n > 0
		m, err = scanMonitored(tx.QueryRowContext(ctx, `SELECT `+monitoredColumns+`
			FROM monitored_sessions m LEFT JOIN plans p ON p.id = m.plan_id
			WHERE m.session_id = ? AND m.reason = 'watch'`, sessionID))
		return err
	})
	if err != nil {
		return MonitoredSession{}, false, fmt.Errorf("store: watch session %s: %w", sessionID, err)
	}
	return m, created, nil
}

// UnwatchSession removes the watch of every session id given (a session's
// id and the one before its last /clear, say). It returns how many watches
// it removed; none is not an error.
func (s *Store) UnwatchSession(ctx context.Context, sessionIDs ...string) (int, error) {
	var ids []any
	for _, id := range sessionIDs {
		if id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return 0, nil
	}
	q := `DELETE FROM monitored_sessions WHERE reason = 'watch' AND session_id IN (?` + strings.Repeat(", ?", len(ids)-1) + `)`
	res, err := s.w.ExecContext(ctx, q, ids...)
	if err != nil {
		return 0, fmt.Errorf("store: unwatch sessions: %w", err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// MonitorPlanLead records that a session registered a plan, since at (zero
// means now), so the session is monitored as that plan's lead until the
// plan is unregistered. Recording it again keeps the first time. It
// reports whether this call recorded it.
func (s *Store) MonitorPlanLead(ctx context.Context, sessionID string, planID int64, at time.Time) (bool, error) {
	if strings.TrimSpace(sessionID) == "" || planID == 0 {
		return false, errors.New("store: monitor plan lead: empty session id or plan")
	}
	res, err := s.w.ExecContext(ctx, `
		INSERT INTO monitored_sessions (session_id, reason, plan_id, since) VALUES (?, 'plan', ?, ?)
		ON CONFLICT (session_id, plan_id) WHERE reason = 'plan' DO NOTHING`,
		sessionID, planID, s.stamp(at).UnixMilli())
	if err != nil {
		return false, fmt.Errorf("store: monitor plan lead %s of plan %d: %w", sessionID, planID, err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ListMonitored returns every explicitly monitored session, oldest first.
func (s *Store) ListMonitored(ctx context.Context) ([]MonitoredSession, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT `+monitoredColumns+`
		FROM monitored_sessions m LEFT JOIN plans p ON p.id = m.plan_id
		ORDER BY m.since, m.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MonitoredSession
	for rows.Next() {
		m, err := scanMonitored(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// carryMonitored moves the monitoring of session oldID to newID, inside a
// write transaction: the session is the same, under a new id (/clear, or a
// background job's respawn). A row newID already has is kept, and the old
// one is left behind.
func carryMonitored(ctx context.Context, tx *sql.Tx, oldID, newID string) error {
	if oldID == "" || newID == "" || oldID == newID {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE OR IGNORE monitored_sessions SET session_id = ? WHERE session_id = ?`, newID, oldID)
	return err
}
