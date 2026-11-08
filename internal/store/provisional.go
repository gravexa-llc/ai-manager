package store

// Provisional packages (migration 0011): packages work names
// that the plan file's tables don't have yet. internal/state finds them
// and decides when they expire; this file keeps them.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// What can name a provisional package (ProvisionalSource.Kind).
const (
	// ProvisionalSpawn is a spawn brief's "Package: <slug> · <ID>" line,
	// from the mod's spawn link. Its ref is the agent's (AgentRef), or a
	// remote agent's session URL.
	ProvisionalSpawn = "spawn"
	// ProvisionalBranch is a same-repo branch named "<slug>/<id>-…"; its
	// ref is BranchRef's.
	ProvisionalBranch = "branch"
	// ProvisionalPR is an open PR's "Plan: <slug> · <ID>" line; its ref
	// is PRRef's.
	ProvisionalPR = "pr"
)

// titleRank orders the kinds by how good a title each gives: a spawn's
// description is what the lead called the work, a PR's title what the
// agent called it, a branch's words a guess.
var titleRank = map[string]int{ProvisionalSpawn: 3, ProvisionalPR: 2, ProvisionalBranch: 1}

// provisionalSeqBase puts provisional packages after the file's rows in
// plan order, in the order they were first seen.
const provisionalSeqBase = 1 << 20

// ProvisionalSource is one thing that named a provisional package.
type ProvisionalSource struct {
	Kind  string // ProvisionalSpawn, ProvisionalBranch or ProvisionalPR
	Ref   string
	Title string // the title it gave, if any
	// FirstSeen and LastSeen are the first and the latest time aimd saw
	// it (a branch or a PR is seen again at every state pass while it
	// lasts; a spawn once).
	FirstSeen time.Time
	LastSeen  time.Time
}

// Sighting is work that names a package of a plan: one source, seen at At
// (zero means now).
type Sighting struct {
	PlanID int64
	PkgID  string
	Kind   string
	Ref    string
	Title  string
	At     time.Time
}

// SeeProvisional records sightings, in one transaction. A sighting of a
// package the plan file has is ignored. One of a provisional package adds
// its source (or moves its LastSeen on), moves the package's ActiveAt on,
// and gives it a better title if the source has one (a spawn's, then a
// PR's, then a branch's). One of an ID the plan has no package for makes a
// new provisional package, its title the source's. A sighting of a plan
// that is gone is skipped. It returns how many packages it made.
func (s *Store) SeeProvisional(ctx context.Context, sightings []Sighting) (int, error) {
	created := 0
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		created = 0
		for _, sg := range sightings {
			made, err := s.see(ctx, tx, sg)
			if err != nil {
				return fmt.Errorf("store: see provisional %d/%s %s %s: %w", sg.PlanID, sg.PkgID, sg.Kind, sg.Ref, err)
			}
			if made {
				created++
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return created, nil
}

func (s *Store) see(ctx context.Context, tx *sql.Tx, sg Sighting) (bool, error) {
	id := strings.TrimSpace(sg.PkgID)
	if id == "" || sg.Ref == "" {
		return false, errors.New("a sighting needs a package id and a ref")
	}
	if _, ok := titleRank[sg.Kind]; !ok {
		return false, fmt.Errorf("unknown source kind %q", sg.Kind)
	}
	at := s.stamp(sg.At).UnixMilli()
	title := strings.TrimSpace(sg.Title)

	var have, titleFrom, oldTitle string
	var provisional bool
	err := tx.QueryRowContext(ctx, `
		SELECT p.pkg_id, p.title, pp.plan_id IS NOT NULL, coalesce(pp.title_from, '')
		FROM packages AS p
		LEFT JOIN provisional_packages AS pp ON pp.plan_id = p.plan_id AND pp.pkg_id = p.pkg_id
		WHERE p.plan_id = ? AND p.pkg_id = ?`, sg.PlanID, id).Scan(&have, &oldTitle, &provisional, &titleFrom)
	made := false
	switch {
	case errors.Is(err, sql.ErrNoRows):
		res, err := tx.ExecContext(ctx, `
			INSERT INTO packages (plan_id, pkg_id, seq, title)
			SELECT id, ?, (SELECT max(?, coalesce(max(seq), 0) + 1) FROM packages WHERE plan_id = ?), ?
			FROM plans WHERE id = ?`,
			id, provisionalSeqBase, sg.PlanID, title, sg.PlanID)
		if err != nil {
			return false, err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return false, err // the plan is gone
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO provisional_packages (plan_id, pkg_id, first_seen, active_at, title_from)
			VALUES (?, ?, ?, ?, ?)`, sg.PlanID, id, at, at, sg.Kind); err != nil {
			return false, err
		}
		have, made = id, true
	case err != nil:
		return false, err
	case !provisional:
		return false, nil // the plan file has it
	default:
		if _, err := tx.ExecContext(ctx, `
			UPDATE provisional_packages SET active_at = max(active_at, ?)
			WHERE plan_id = ? AND pkg_id = ?`, at, sg.PlanID, have); err != nil {
			return false, err
		}
		if title != "" && (titleRank[sg.Kind] > titleRank[titleFrom] || oldTitle == "") {
			if _, err := tx.ExecContext(ctx, `UPDATE packages SET title = ? WHERE plan_id = ? AND pkg_id = ?`, title, sg.PlanID, have); err != nil {
				return false, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE provisional_packages SET title_from = ? WHERE plan_id = ? AND pkg_id = ?`, sg.Kind, sg.PlanID, have); err != nil {
				return false, err
			}
		}
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO provisional_sources (plan_id, pkg_id, kind, ref, title, first_seen, last_seen)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (plan_id, pkg_id, kind, ref) DO UPDATE SET
			last_seen = max(last_seen, excluded.last_seen),
			title = CASE WHEN excluded.title <> '' THEN excluded.title ELSE title END`,
		sg.PlanID, have, sg.Kind, sg.Ref, title, at, at)
	return made, err
}

// provisionalSources returns a plan's provisional sources by upper-case
// package ID, oldest first.
func (s *Store) provisionalSources(ctx context.Context, planID int64) (map[string][]ProvisionalSource, error) {
	rows, err := s.r.QueryContext(ctx, `
		SELECT pkg_id, kind, ref, title, first_seen, last_seen
		FROM provisional_sources WHERE plan_id = ?
		ORDER BY first_seen, kind, ref`, planID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]ProvisionalSource{}
	for rows.Next() {
		var id string
		var src ProvisionalSource
		if err := rows.Scan(&id, &src.Kind, &src.Ref, &src.Title, msTime{&src.FirstSeen}, msTime{&src.LastSeen}); err != nil {
			return nil, err
		}
		k := strings.ToUpper(id)
		out[k] = append(out[k], src)
	}
	return out, rows.Err()
}

// SetProvisionalActive records that a state pass found work on some of a
// plan's provisional packages at at: their ActiveAt moves on to it. IDs
// that aren't provisional are skipped.
func (s *Store) SetProvisionalActive(ctx context.Context, planID int64, ids []string, at time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	list, err := listJSON(ids)
	if err != nil {
		return err
	}
	_, err = s.w.ExecContext(ctx, `
		UPDATE provisional_packages SET active_at = max(active_at, ?)
		WHERE plan_id = ? AND pkg_id IN (SELECT value FROM json_each(?))`,
		s.stamp(at).UnixMilli(), planID, list)
	if err != nil {
		return fmt.Errorf("store: set provisional active of plan %d: %w", planID, err)
	}
	return nil
}

// ExpireProvisional deletes those of a plan's provisional packages, by ID,
// whose ActiveAt is before before, and with each its links, tombstones,
// history and sources. A package that became the file's row, or was seen
// at work again, meanwhile stays. It returns how many it deleted.
func (s *Store) ExpireProvisional(ctx context.Context, planID int64, ids []string, before time.Time) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	list, err := listJSON(ids)
	if err != nil {
		return 0, err
	}
	res, err := s.w.ExecContext(ctx, `
		DELETE FROM packages
		WHERE plan_id = ? AND pkg_id IN (SELECT value FROM json_each(?))
		  AND EXISTS (SELECT 1 FROM provisional_packages AS pp
		              WHERE pp.plan_id = packages.plan_id AND pp.pkg_id = packages.pkg_id AND pp.active_at < ?)`,
		planID, list, before.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("store: expire provisional packages of plan %d: %w", planID, err)
	}
	n, err := res.RowsAffected()
	return int(n), err
}
