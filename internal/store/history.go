package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// What a plan's timeline reads besides links, PRs and events (migration
// 0004): every lifecycle change of its packages, and its alerts over time.

// LifecycleChange is a row of lifecycle_history: a package's derived
// lifecycle changed from Prev to Lifecycle at At. SetLifecycles writes
// them; Prev is "" for a package's first derivation.
type LifecycleChange struct {
	ID        int64
	PlanID    int64
	PkgID     string
	Lifecycle string
	Prev      string
	At        time.Time
}

// HistoryFilter narrows ListLifecycleHistory and ListPlanAlerts.
type HistoryFilter struct {
	// PkgID keeps one package's rows (any case). "" keeps all.
	PkgID string
	// Limit caps the rows, newest first; <= 0 means 200.
	Limit int
}

func (f HistoryFilter) limit() int {
	if f.Limit <= 0 {
		return 200
	}
	return f.Limit
}

// ListLifecycleHistory returns a plan's lifecycle changes, newest first.
func (s *Store) ListLifecycleHistory(ctx context.Context, planID int64, f HistoryFilter) ([]LifecycleChange, error) {
	q := `SELECT id, plan_id, pkg_id, lifecycle, prev, at FROM lifecycle_history WHERE plan_id = ?`
	args := []any{planID}
	if f.PkgID != "" {
		q += ` AND pkg_id = ?`
		args = append(args, f.PkgID)
	}
	q += ` ORDER BY at DESC, id DESC LIMIT ?`
	args = append(args, f.limit())
	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LifecycleChange
	for rows.Next() {
		var c LifecycleChange
		if err := rows.Scan(&c.ID, &c.PlanID, &c.PkgID, &c.Lifecycle, &c.Prev, msTime{&c.At}); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// PlanAlert is a row of plan_alerts: one alert of a plan, from the state
// pass that raised it until the one that no longer found it.
type PlanAlert struct {
	ID     int64
	PlanID int64
	// Kind is the alert's kind (internal/state AlertKind); Key what it is
	// about within the kind (a package ID, a lane). Kind and Key (any case)
	// name the alert.
	Kind     string
	Key      string
	Packages []string
	// Message is the alert's text when it was raised.
	Message   string
	RaisedAt  time.Time // zero means now, in SyncAlerts
	ClearedAt time.Time // zero while it is raised
}

// AlertChanges counts what SyncAlerts did.
type AlertChanges struct {
	Raised, Cleared int
}

// SyncAlerts makes a plan's open alerts match one state pass's alerts, in
// one transaction: an alert not open yet is raised (at its RaisedAt, or
// now), one already open is left as it was raised, and an open alert
// missing from alerts is cleared at now. now is the pass's time; zero
// means the store's clock. Alerts with an empty Kind or Key are skipped.
func (s *Store) SyncAlerts(ctx context.Context, planID int64, alerts []PlanAlert, now time.Time) (AlertChanges, error) {
	var ch AlertChanges
	// It runs after every state pass, and alerts seldom change: a read
	// that finds nothing to do takes no write lock.
	open, err := openAlerts(ctx, s.r, planID)
	if err != nil {
		return ch, fmt.Errorf("store: sync alerts of plan %d: %w", planID, err)
	}
	want := map[alertKey]bool{}
	for _, a := range alerts {
		if a.Kind != "" && a.Key != "" {
			want[alertKeyOf(a.Kind, a.Key)] = true
		}
	}
	unchanged := len(want) == len(open)
	for k := range want {
		if _, ok := open[k]; !ok {
			unchanged = false
		}
	}
	if unchanged {
		return ch, nil
	}
	err = s.writeTx(ctx, func(tx *sql.Tx) error {
		open, err := openAlerts(ctx, tx, planID)
		if err != nil {
			return err
		}
		keep := map[int64]bool{}
		for _, a := range alerts {
			if a.Kind == "" || a.Key == "" {
				continue
			}
			k := alertKeyOf(a.Kind, a.Key)
			if id, ok := open[k]; ok {
				keep[id] = true
				continue
			}
			pkgs, err := listJSON(a.Packages)
			if err != nil {
				return err
			}
			raised := a.RaisedAt
			if raised.IsZero() {
				raised = now
			}
			var id int64
			err = tx.QueryRowContext(ctx, `
				INSERT INTO plan_alerts (plan_id, kind, key, packages, message, raised_at)
				VALUES (?, ?, ?, ?, ?, ?) RETURNING id`,
				planID, a.Kind, a.Key, pkgs, a.Message, s.stamp(raised).UnixMilli()).Scan(&id)
			if err != nil {
				return err
			}
			open[k], keep[id] = id, true
			ch.Raised++
		}
		for _, id := range open {
			if keep[id] {
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE plan_alerts SET cleared_at = ? WHERE id = ?`, s.stamp(now).UnixMilli(), id); err != nil {
				return err
			}
			ch.Cleared++
		}
		return nil
	})
	if err != nil {
		return AlertChanges{}, fmt.Errorf("store: sync alerts of plan %d: %w", planID, err)
	}
	return ch, nil
}

// alertKey names an alert within its plan: its kind, and its key in upper
// case.
type alertKey struct{ kind, key string }

func alertKeyOf(kind, key string) alertKey { return alertKey{kind, strings.ToUpper(key)} }

// queryer is a *sql.DB or a *sql.Tx.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// openAlerts returns a plan's open alerts' ids by alertKey.
func openAlerts(ctx context.Context, q queryer, planID int64) (map[alertKey]int64, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, kind, key FROM plan_alerts WHERE plan_id = ? AND cleared_at IS NULL`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	open := map[alertKey]int64{}
	for rows.Next() {
		var id int64
		var kind, k string
		if err := rows.Scan(&id, &kind, &k); err != nil {
			return nil, err
		}
		open[alertKeyOf(kind, k)] = id
	}
	return open, rows.Err()
}

// ListPlanAlerts returns a plan's alerts, open and cleared, the most
// recently raised or cleared first. With PkgID, only alerts naming that
// package.
func (s *Store) ListPlanAlerts(ctx context.Context, planID int64, f HistoryFilter) ([]PlanAlert, error) {
	q := `SELECT id, plan_id, kind, key, packages, message, raised_at, cleared_at FROM plan_alerts WHERE plan_id = ?`
	args := []any{planID}
	if f.PkgID != "" {
		q += ` AND EXISTS (SELECT 1 FROM json_each(plan_alerts.packages) WHERE json_each.value = ? COLLATE NOCASE)`
		args = append(args, f.PkgID)
	}
	q += ` ORDER BY coalesce(cleared_at, raised_at) DESC, id DESC LIMIT ?`
	args = append(args, f.limit())
	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlanAlert
	for rows.Next() {
		var a PlanAlert
		if err := rows.Scan(&a.ID, &a.PlanID, &a.Kind, &a.Key, jsonList{&a.Packages}, &a.Message,
			msTime{&a.RaisedAt}, msTime{&a.ClearedAt}); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
