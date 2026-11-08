package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// This file holds the removal log (migration 0009): what the
// dashboard's Worktrees & cleanup page removed, skipped or pruned.

// What happened to one worktree or record (removal_log.action).
const (
	// RemovalRemoved: `git worktree remove` removed it (BranchDeleted says
	// whether its branch went too).
	RemovalRemoved = "removed"
	// RemovalSkipped: the re-check found it no longer safe, or it couldn't
	// be checked; nothing was done. For a repo where `git worktree prune`
	// didn't run, Path is the repo.
	RemovalSkipped = "skipped"
	// RemovalFailed: git refused or failed.
	RemovalFailed = "failed"
	// RemovalPruned: `git worktree prune` cleared its record.
	RemovalPruned = "pruned"
)

// Removal is one entry of the removal log.
type Removal struct {
	ID int64
	// At is when it happened; zero means now.
	At time.Time
	// Repo is the repo's main checkout, Path the worktree's folder.
	Repo string
	Path string
	// Branch and Head are what the worktree had ("" branch: detached).
	Branch string
	Head   string
	// Action is RemovalRemoved, RemovalSkipped, RemovalFailed or
	// RemovalPruned.
	Action        string
	BranchDeleted bool
	// Detail says why it was skipped or failed.
	Detail string
}

const removalColumns = `id, at, repo, path, branch, head_sha, action, branch_deleted, detail`

// AddRemovals appends entries to the removal log, in one transaction.
func (s *Store) AddRemovals(ctx context.Context, entries ...Removal) error {
	if len(entries) == 0 {
		return nil
	}
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		st, err := tx.PrepareContext(ctx, `INSERT INTO removal_log
			(at, repo, path, branch, head_sha, action, branch_deleted, detail) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer st.Close()
		for _, e := range entries {
			if e.Action == "" {
				return fmt.Errorf("entry for %s: no action", e.Path)
			}
			if _, err := st.ExecContext(ctx, s.stamp(e.At).UnixMilli(), e.Repo, e.Path, e.Branch, e.Head,
				e.Action, e.BranchDeleted, e.Detail); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("store: add removals: %w", err)
	}
	return nil
}

// ListRemovals returns the log entries at or after since, newest first, at
// most limit of them (all when limit is 0 or less).
func (s *Store) ListRemovals(ctx context.Context, since time.Time, limit int) ([]Removal, error) {
	if limit <= 0 {
		limit = -1
	}
	rows, err := s.r.QueryContext(ctx, `SELECT `+removalColumns+` FROM removal_log
		WHERE at >= ? ORDER BY at DESC, id DESC LIMIT ?`, since.UnixMilli(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: list removals: %w", err)
	}
	defer rows.Close()
	var out []Removal
	for rows.Next() {
		var e Removal
		if err := rows.Scan(&e.ID, msTime{&e.At}, &e.Repo, &e.Path, &e.Branch, &e.Head, &e.Action,
			&e.BranchDeleted, &e.Detail); err != nil {
			return nil, fmt.Errorf("store: list removals: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// PurgeRemovals deletes the log entries from before before and returns how
// many it deleted.
func (s *Store) PurgeRemovals(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.w.ExecContext(ctx, `DELETE FROM removal_log WHERE at < ?`, before.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("store: purge removals: %w", err)
	}
	return res.RowsAffected()
}
