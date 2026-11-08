package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Plan is a row of the plans table: one registered plan file.
type Plan struct {
	ID         int64  // set by the store
	Slug       string // unique, compared case-insensitively
	Title      string
	SourcePath string
	SourceMD   string // snapshot of the plan file
	// Repos are the repos the plan's packages live in. Package IDs are only
	// looked up in plans registered for the repo in question.
	Repos               []string
	RegisteredInSession string
	Status              string
	CreatedAt           time.Time // zero means now
	UpdatedAt           time.Time // set by the store on every write
}

const planColumns = `id, slug, title, source_path, source_md, repos,
	registered_in_session, status, created_at, updated_at`

func scanPlan(sc scanner) (Plan, error) {
	var p Plan
	err := sc.Scan(&p.ID, &p.Slug, &p.Title, &p.SourcePath, &p.SourceMD, jsonList{&p.Repos},
		&p.RegisteredInSession, &p.Status, msTime{&p.CreatedAt}, msTime{&p.UpdatedAt})
	return p, err
}

// InsertPlan stores a new plan and returns its id. The slug must not be in
// use.
func (s *Store) InsertPlan(ctx context.Context, p Plan) (int64, error) {
	if p.Slug == "" {
		return 0, errors.New("store: insert plan: empty slug")
	}
	repos, err := listJSON(p.Repos)
	if err != nil {
		return 0, err
	}
	created := s.stamp(p.CreatedAt).UnixMilli()
	var id int64
	err = s.w.QueryRowContext(ctx, `
		INSERT INTO plans (slug, title, source_path, source_md, repos, registered_in_session, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING id`,
		p.Slug, p.Title, p.SourcePath, p.SourceMD, repos, p.RegisteredInSession, p.Status, created, created).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: insert plan %s: %w", p.Slug, err)
	}
	return id, nil
}

// UpdatePlan rewrites the plan with p.ID: every field but CreatedAt, and
// UpdatedAt becomes now.
func (s *Store) UpdatePlan(ctx context.Context, p Plan) error {
	if p.Slug == "" {
		return errors.New("store: update plan: empty slug")
	}
	repos, err := listJSON(p.Repos)
	if err != nil {
		return err
	}
	res, err := s.w.ExecContext(ctx, `
		UPDATE plans SET slug = ?, title = ?, source_path = ?, source_md = ?, repos = ?,
			registered_in_session = ?, status = ?, updated_at = ?
		WHERE id = ?`,
		p.Slug, p.Title, p.SourcePath, p.SourceMD, repos, p.RegisteredInSession, p.Status, s.now().UnixMilli(), p.ID)
	if err != nil {
		return fmt.Errorf("store: update plan %d: %w", p.ID, err)
	}
	return mustAffect(res, "plan %d", p.ID)
}

// DeletePlan removes the plan with the given id, and with it its packages,
// links and unlink tombstones (the foreign keys cascade). PR rows and plan
// file dismissals are left alone. It returns ErrNotFound when there is no
// such plan.
func (s *Store) DeletePlan(ctx context.Context, id int64) error {
	res, err := s.w.ExecContext(ctx, `DELETE FROM plans WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete plan %d: %w", id, err)
	}
	return mustAffect(res, "plan %d", id)
}

// GetPlan returns the plan with the given id.
func (s *Store) GetPlan(ctx context.Context, id int64) (Plan, error) {
	p, err := scanPlan(s.r.QueryRowContext(ctx, `SELECT `+planColumns+` FROM plans WHERE id = ?`, id))
	if err != nil {
		return Plan{}, notFound(err, "plan %d", id)
	}
	return p, nil
}

// GetPlanBySlug returns the plan with the given slug, compared
// case-insensitively.
func (s *Store) GetPlanBySlug(ctx context.Context, slug string) (Plan, error) {
	p, err := scanPlan(s.r.QueryRowContext(ctx, `SELECT `+planColumns+` FROM plans WHERE slug = ?`, slug))
	if err != nil {
		return Plan{}, notFound(err, "plan %q", slug)
	}
	return p, nil
}

// PlanFilter narrows ListPlans.
type PlanFilter struct {
	// Repo lists only plans whose Repos include it, compared
	// case-insensitively. "" lists every plan.
	Repo string
}

// ListPlans returns plans in the order they were registered.
func (s *Store) ListPlans(ctx context.Context, f PlanFilter) ([]Plan, error) {
	q := `SELECT ` + planColumns + ` FROM plans`
	var args []any
	if f.Repo != "" {
		q += ` WHERE EXISTS (SELECT 1 FROM json_each(plans.repos) WHERE json_each.value = ? COLLATE NOCASE)`
		args = append(args, f.Repo)
	}
	q += ` ORDER BY created_at, id`
	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Plan
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Package is a row of the packages table: one row of a plan's package
// table, keyed on (PlanID, PkgID).
type Package struct {
	PlanID int64
	PkgID  string // e.g. "B1", compared case-insensitively
	Seq    int    // position in the plan file
	Title  string
	Repo   string
	Size   string
	// Needs are the package IDs in the Needs cell, as written; NeedsNote is
	// the rest of the cell ("Sam + reviewer approve").
	Needs     []string
	NeedsNote string
	// NeedsYou is the Needs cell's `you:` clause ("run the check"): a step
	// you take for a package someone else carries out.
	NeedsYou string
	Owner    string
	Lane     string
	// DoneInPlan is a done marker in the plan file; DoneManual is "Mark
	// done" on the dashboard.
	DoneInPlan bool
	DoneManual bool
	Note       string

	// Lifecycle is the package's derived lifecycle as internal/state last
	// saved it, LifecyclePrev the one before, and LifecycleAt when it
	// changed. Only SetLifecycles writes them; every other write keeps
	// them.
	Lifecycle     string
	LifecyclePrev string
	LifecycleAt   time.Time

	// Provisional is true for a package the plan file's tables don't have
	// yet (migration 0011): work names it, and aimd shows it until
	// the file gains its row. FirstSeen is when aimd first saw that work,
	// ActiveAt the last state pass that found any of it, and Sources what
	// named it (ListPackages fills them in; GetPackage leaves Sources out).
	// Only SeeProvisional, SetProvisionalActive and ExpireProvisional write
	// them, and SyncPackages ends them when the file gains the row.
	Provisional bool
	FirstSeen   time.Time
	ActiveAt    time.Time
	Sources     []ProvisionalSource
}

// packageSelect reads packages with their provisional rows, if any.
const packageSelect = `SELECT p.plan_id, p.pkg_id, p.seq, p.title, p.repo, p.size, p.needs, p.needs_note,
	p.needs_you, p.owner, p.lane, p.done_in_plan, p.done_manual, p.note,
	p.lifecycle, p.lifecycle_prev, p.lifecycle_at,
	pp.plan_id IS NOT NULL, pp.first_seen, pp.active_at
	FROM packages AS p
	LEFT JOIN provisional_packages AS pp ON pp.plan_id = p.plan_id AND pp.pkg_id = p.pkg_id`

func scanPackage(sc scanner) (Package, error) {
	var p Package
	err := sc.Scan(&p.PlanID, &p.PkgID, &p.Seq, &p.Title, &p.Repo, &p.Size, jsonList{&p.Needs}, &p.NeedsNote,
		&p.NeedsYou, &p.Owner, &p.Lane, &p.DoneInPlan, &p.DoneManual, &p.Note,
		&p.Lifecycle, &p.LifecyclePrev, msTime{&p.LifecycleAt},
		&p.Provisional, msTime{&p.FirstSeen}, msTime{&p.ActiveAt})
	return p, err
}

// InsertPackage stores a new package. Its plan must exist and its ID must
// be new in that plan. Its lifecycle starts empty.
func (s *Store) InsertPackage(ctx context.Context, p Package) error {
	if p.PkgID == "" {
		return errors.New("store: insert package: empty package id")
	}
	needs, err := listJSON(p.Needs)
	if err != nil {
		return err
	}
	_, err = s.w.ExecContext(ctx, `
		INSERT INTO packages (plan_id, pkg_id, seq, title, repo, size, needs, needs_note,
			needs_you, owner, lane, done_in_plan, done_manual, note)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.PlanID, p.PkgID, p.Seq, p.Title, p.Repo, p.Size, needs, p.NeedsNote,
		p.NeedsYou, p.Owner, p.Lane, p.DoneInPlan, p.DoneManual, p.Note)
	if err != nil {
		return fmt.Errorf("store: insert package %d/%s: %w", p.PlanID, p.PkgID, err)
	}
	return nil
}

// UpdatePackage rewrites every field of the package (PlanID, PkgID) but
// the lifecycle, which only SetLifecycles writes.
func (s *Store) UpdatePackage(ctx context.Context, p Package) error {
	needs, err := listJSON(p.Needs)
	if err != nil {
		return err
	}
	res, err := s.w.ExecContext(ctx, `
		UPDATE packages SET seq = ?, title = ?, repo = ?, size = ?, needs = ?, needs_note = ?,
			needs_you = ?, owner = ?, lane = ?, done_in_plan = ?, done_manual = ?, note = ?
		WHERE plan_id = ? AND pkg_id = ?`,
		p.Seq, p.Title, p.Repo, p.Size, needs, p.NeedsNote,
		p.NeedsYou, p.Owner, p.Lane, p.DoneInPlan, p.DoneManual, p.Note, p.PlanID, p.PkgID)
	if err != nil {
		return fmt.Errorf("store: update package %d/%s: %w", p.PlanID, p.PkgID, err)
	}
	return mustAffect(res, "package %d/%s", p.PlanID, p.PkgID)
}

// SyncPackages makes a plan's packages match a fresh parse of its file, in
// one transaction. pkgs are in plan order; each one's Seq is set to its
// index and its PlanID to planID. Packages in pkgs are inserted or updated:
// the parser's fields are written, and DoneManual and Note, which come from
// the dashboard, and the derived lifecycle are kept. Packages missing from
// pkgs are deleted, and their links and tombstones with them, except the
// provisional ones: those aren't the file's. A provisional
// package that pkgs has becomes the file's row, keeping its links, Mark
// done and history, and is no longer provisional.
//
// Use it rather than deleting and re-inserting a plan's packages, which
// would delete every link too.
func (s *Store) SyncPackages(ctx context.Context, planID int64, pkgs []Package) error {
	ids := make([]string, len(pkgs))
	seen := make(map[string]bool, len(pkgs))
	for i, p := range pkgs {
		if p.PkgID == "" {
			return fmt.Errorf("store: sync packages of plan %d: package %d has an empty id", planID, i)
		}
		k := strings.ToUpper(p.PkgID)
		if seen[k] {
			return fmt.Errorf("store: sync packages of plan %d: duplicate package id %s", planID, p.PkgID)
		}
		seen[k] = true
		ids[i] = p.PkgID
	}
	idList, err := listJSON(ids)
	if err != nil {
		return err
	}
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		var one int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM plans WHERE id = ?`, planID).Scan(&one); err != nil {
			return notFound(err, "plan %d", planID)
		}
		for i, p := range pkgs {
			needs, err := listJSON(p.Needs)
			if err != nil {
				return err
			}
			_, err = tx.ExecContext(ctx, `
				INSERT INTO packages (plan_id, pkg_id, seq, title, repo, size, needs, needs_note,
					needs_you, owner, lane, done_in_plan, done_manual, note)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, '')
				ON CONFLICT (plan_id, pkg_id) DO UPDATE SET
					pkg_id = excluded.pkg_id,
					seq = excluded.seq,
					title = excluded.title,
					repo = excluded.repo,
					size = excluded.size,
					needs = excluded.needs,
					needs_note = excluded.needs_note,
					needs_you = excluded.needs_you,
					owner = excluded.owner,
					lane = excluded.lane,
					done_in_plan = excluded.done_in_plan`,
				planID, p.PkgID, i, p.Title, p.Repo, p.Size, needs, p.NeedsNote, p.NeedsYou, p.Owner, p.Lane, p.DoneInPlan)
			if err != nil {
				return fmt.Errorf("store: sync package %d/%s: %w", planID, p.PkgID, err)
			}
		}
		// A provisional package the file now has is the file's row from
		// here on: the upsert above took its packages row
		// over, links and all, and it stops being provisional.
		_, err := tx.ExecContext(ctx, `
			DELETE FROM provisional_packages
			WHERE plan_id = ? AND pkg_id IN (SELECT value FROM json_each(?))`,
			planID, idList)
		if err != nil {
			return fmt.Errorf("store: sync packages of plan %d: merge provisional: %w", planID, err)
		}
		// The other provisional packages aren't the file's to delete.
		_, err = tx.ExecContext(ctx, `
			DELETE FROM packages
			WHERE plan_id = ? AND pkg_id NOT IN (SELECT value FROM json_each(?))
			  AND NOT EXISTS (SELECT 1 FROM provisional_packages AS pp
			                  WHERE pp.plan_id = packages.plan_id AND pp.pkg_id = packages.pkg_id)`,
			planID, idList)
		if err != nil {
			return fmt.Errorf("store: sync packages of plan %d: %w", planID, err)
		}
		// A change of case only ("b1" to "B1") is no change to the
		// foreign key, so ON UPDATE CASCADE doesn't fire; respell the
		// links here.
		_, err = tx.ExecContext(ctx, `
			UPDATE links SET pkg_id = p.pkg_id
			FROM packages AS p
			WHERE links.plan_id = ? AND p.plan_id = links.plan_id
			  AND p.pkg_id = links.pkg_id AND p.pkg_id <> links.pkg_id COLLATE BINARY`,
			planID)
		if err != nil {
			return fmt.Errorf("store: sync packages of plan %d: respell links: %w", planID, err)
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE link_tombstones SET pkg_id = p.pkg_id
			FROM packages AS p
			WHERE link_tombstones.plan_id = ? AND p.plan_id = link_tombstones.plan_id
			  AND p.pkg_id = link_tombstones.pkg_id AND p.pkg_id <> link_tombstones.pkg_id COLLATE BINARY`,
			planID)
		if err != nil {
			return fmt.Errorf("store: sync packages of plan %d: respell tombstones: %w", planID, err)
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE lifecycle_history SET pkg_id = p.pkg_id
			FROM packages AS p
			WHERE lifecycle_history.plan_id = ? AND p.plan_id = lifecycle_history.plan_id
			  AND p.pkg_id = lifecycle_history.pkg_id AND p.pkg_id <> lifecycle_history.pkg_id COLLATE BINARY`,
			planID)
		if err != nil {
			return fmt.Errorf("store: sync packages of plan %d: respell history: %w", planID, err)
		}
		return nil
	})
}

// PackageLifecycle is one package's derived lifecycle, as SetLifecycles
// writes it.
type PackageLifecycle struct {
	PkgID     string
	Lifecycle string
	Prev      string    // the lifecycle before this one
	At        time.Time // when it changed; zero means now
}

// SetLifecycles writes the derived lifecycle of some of a plan's packages,
// in one transaction. Packages that no longer exist are skipped. A package
// whose stored lifecycle differs from the new one also gets a
// lifecycle_history row (ListLifecycleHistory) in the same transaction, so
// the timeline has every change the board made.
func (s *Store) SetLifecycles(ctx context.Context, planID int64, ls []PackageLifecycle) error {
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		for _, l := range ls {
			at := s.stamp(l.At).UnixMilli()
			_, err := tx.ExecContext(ctx, `
				INSERT INTO lifecycle_history (plan_id, pkg_id, lifecycle, prev, at)
				SELECT plan_id, pkg_id, ?, ?, ? FROM packages
				WHERE plan_id = ? AND pkg_id = ? AND lifecycle <> ?`,
				l.Lifecycle, l.Prev, at, planID, l.PkgID, l.Lifecycle)
			if err != nil {
				return fmt.Errorf("store: record lifecycle of %d/%s: %w", planID, l.PkgID, err)
			}
			_, err = tx.ExecContext(ctx, `
				UPDATE packages SET lifecycle = ?, lifecycle_prev = ?, lifecycle_at = ?
				WHERE plan_id = ? AND pkg_id = ?`,
				l.Lifecycle, l.Prev, at, planID, l.PkgID)
			if err != nil {
				return fmt.Errorf("store: set lifecycle of %d/%s: %w", planID, l.PkgID, err)
			}
		}
		return nil
	})
}

// ListPackages returns a plan's packages in plan order: the file's rows,
// then its provisional packages in the order aimd first saw them, each
// with its sources.
func (s *Store) ListPackages(ctx context.Context, planID int64) ([]Package, error) {
	rows, err := s.r.QueryContext(ctx, packageSelect+` WHERE p.plan_id = ? ORDER BY p.seq, p.pkg_id`, planID)
	if err != nil {
		return nil, err
	}
	var out []Package
	provisional := false
	for rows.Next() {
		p, err := scanPackage(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		provisional = provisional || p.Provisional
		out = append(out, p)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if !provisional {
		return out, nil
	}
	sources, err := s.provisionalSources(ctx, planID)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Sources = sources[strings.ToUpper(out[i].PkgID)]
	}
	return out, nil
}
