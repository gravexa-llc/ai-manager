package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// HiddenUnmatched is a PR or branch you said isn't part of a plan ("Not
// part of this plan" on its page; migration 0015): the plan's Unmatched
// lane leaves it out until you restore it. Kind and Ref are a link
// target's: LinkPR with PRRef, LinkBranch with BranchRef.
type HiddenUnmatched struct {
	PlanID   int64
	Kind     string
	Ref      string
	HiddenAt time.Time // zero means now
}

// HideUnmatched hides a target from a plan's Unmatched lane. Hiding it
// again keeps the first time. It returns ErrNotFound when there is no such
// plan.
func (s *Store) HideUnmatched(ctx context.Context, h HiddenUnmatched) error {
	if h.Kind == "" || strings.TrimSpace(h.Ref) == "" {
		return errors.New("store: hide unmatched: empty kind or ref")
	}
	res, err := s.w.ExecContext(ctx, `
		INSERT INTO unmatched_hidden (plan_id, kind, ref, hidden_at)
		SELECT id, ?, ?, ? FROM plans WHERE id = ?
		ON CONFLICT (plan_id, kind, ref) DO NOTHING`,
		h.Kind, h.Ref, s.stamp(h.HiddenAt).UnixMilli(), h.PlanID)
	if err != nil {
		return fmt.Errorf("store: hide unmatched %s %s from plan %d: %w", h.Kind, h.Ref, h.PlanID, err)
	}
	if n, err := res.RowsAffected(); err != nil || n > 0 {
		return err
	}
	// Nothing inserted: hidden already, or no such plan.
	var one int
	err = s.w.QueryRowContext(ctx, `SELECT 1 FROM plans WHERE id = ?`, h.PlanID).Scan(&one)
	return notFound(err, "plan %d", h.PlanID)
}

// UnhideUnmatched restores a hidden target to a plan's Unmatched lane, and
// reports whether it was hidden.
func (s *Store) UnhideUnmatched(ctx context.Context, planID int64, kind, ref string) (bool, error) {
	res, err := s.w.ExecContext(ctx, `DELETE FROM unmatched_hidden WHERE plan_id = ? AND kind = ? AND ref = ?`, planID, kind, ref)
	if err != nil {
		return false, fmt.Errorf("store: unhide unmatched %s %s from plan %d: %w", kind, ref, planID, err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ListHiddenUnmatched returns what is hidden from a plan's Unmatched lane,
// most recently hidden first.
func (s *Store) ListHiddenUnmatched(ctx context.Context, planID int64) ([]HiddenUnmatched, error) {
	rows, err := s.r.QueryContext(ctx, `
		SELECT plan_id, kind, ref, hidden_at FROM unmatched_hidden
		WHERE plan_id = ? ORDER BY hidden_at DESC, kind, ref`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HiddenUnmatched
	for rows.Next() {
		var h HiddenUnmatched
		if err := rows.Scan(&h.PlanID, &h.Kind, &h.Ref, msTime{&h.HiddenAt}); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
