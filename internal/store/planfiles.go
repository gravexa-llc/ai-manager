package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// DismissPlanFile records that the markdown file at path isn't a plan
// ("Not a plan"), and returns when it was dismissed: at (or now), or the
// earlier time if it was already dismissed. path should be the file's
// resolved path; it compares case-insensitively.
func (s *Store) DismissPlanFile(ctx context.Context, path string, at time.Time) (time.Time, error) {
	if path == "" {
		return time.Time{}, errors.New("store: dismiss plan file: empty path")
	}
	var got time.Time
	err := s.w.QueryRowContext(ctx, `
		INSERT INTO plan_dismissals (path, dismissed_at) VALUES (?, ?)
		ON CONFLICT (path) DO UPDATE SET path = plan_dismissals.path
		RETURNING dismissed_at`,
		path, s.stamp(at).UnixMilli()).Scan(msTime{&got})
	if err != nil {
		return time.Time{}, fmt.Errorf("store: dismiss plan file %s: %w", path, err)
	}
	return got, nil
}

// PlanFileDismissed reports whether the file at path was dismissed, and
// when.
func (s *Store) PlanFileDismissed(ctx context.Context, path string) (time.Time, bool, error) {
	var at time.Time
	err := s.r.QueryRowContext(ctx, `SELECT dismissed_at FROM plan_dismissals WHERE path = ?`, path).Scan(msTime{&at})
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: plan file dismissed %s: %w", path, err)
	}
	return at, true, nil
}

// UndismissPlanFile forgets a dismissal. A file that wasn't dismissed is
// not an error.
func (s *Store) UndismissPlanFile(ctx context.Context, path string) error {
	if _, err := s.w.ExecContext(ctx, `DELETE FROM plan_dismissals WHERE path = ?`, path); err != nil {
		return fmt.Errorf("store: undismiss plan file %s: %w", path, err)
	}
	return nil
}

// GetPlanBySourcePath returns the plan registered from the file at path,
// compared case-insensitively. When two plans name the same file, the
// first registered is returned.
func (s *Store) GetPlanBySourcePath(ctx context.Context, path string) (Plan, error) {
	p, err := scanPlan(s.r.QueryRowContext(ctx,
		`SELECT `+planColumns+` FROM plans WHERE source_path = ? ORDER BY created_at, id LIMIT 1`, path))
	if err != nil {
		return Plan{}, notFound(err, "plan from %s", path)
	}
	return p, nil
}

// GetPackage returns one package of a plan; the ID compares
// case-insensitively.
func (s *Store) GetPackage(ctx context.Context, planID int64, pkgID string) (Package, error) {
	p, err := scanPackage(s.r.QueryRowContext(ctx,
		packageSelect+` WHERE p.plan_id = ? AND p.pkg_id = ?`, planID, pkgID))
	if err != nil {
		return Package{}, notFound(err, "package %d/%s", planID, pkgID)
	}
	return p, nil
}

// SetDoneManual sets or clears a package's Mark done, leaving every other
// field alone.
func (s *Store) SetDoneManual(ctx context.Context, planID int64, pkgID string, done bool) error {
	res, err := s.w.ExecContext(ctx, `UPDATE packages SET done_manual = ? WHERE plan_id = ? AND pkg_id = ?`, done, planID, pkgID)
	if err != nil {
		return fmt.Errorf("store: set done of %d/%s: %w", planID, pkgID, err)
	}
	return mustAffect(res, "package %d/%s", planID, pkgID)
}
