package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Upstream states.
const (
	UpstreamOK   = "ok"   // the local tracking ref exists
	UpstreamGone = "gone" // it is configured but gone
	UpstreamNone = "none" // no upstream configured
)

// Worktree is a row of the worktrees table: one worktree the scanner has
// seen, main checkouts included (Path equals MainRepo).
type Worktree struct {
	Path     string // compared case-insensitively
	MainRepo string // the main checkout's path
	Branch   string // "" when detached
	Detached bool
	HeadSHA  string

	DirtyCount     int
	UntrackedCount int
	// IgnoredAtRisk lists ignored paths outside the rebuildable list, which
	// `git worktree remove` would delete.
	IgnoredAtRisk []string
	UpstreamState string // UpstreamOK, UpstreamGone, UpstreamNone
	UnpushedCount int
	LastCommitAt  time.Time

	Locked         bool
	LockedReason   string // as git reports it
	LockAgentID    string // parsed from Claude Code's lock reason
	LockPID        int
	PrunableReason string // set when git reports the path gone

	// SessionID and AgentID are the session or agent working in it, if known.
	SessionID string
	AgentID   string

	// FirstSeen and LastSeen are the first and latest scan that saw it.
	// Pass the scan time as LastSeen; zero means now.
	FirstSeen time.Time
	LastSeen  time.Time
	// RemovedAt is set once a scan no longer finds it.
	RemovedAt time.Time
}

const worktreeColumns = `path, main_repo, branch, detached, head_sha, dirty_count,
	untracked_count, ignored_at_risk, upstream_state, unpushed_count, last_commit_at,
	locked, locked_reason, lock_agent_id, lock_pid, prunable_reason, session_id,
	agent_id, first_seen, last_seen, removed_at`

func scanWorktree(sc scanner) (Worktree, error) {
	var w Worktree
	err := sc.Scan(&w.Path, &w.MainRepo, &w.Branch, &w.Detached, &w.HeadSHA, &w.DirtyCount,
		&w.UntrackedCount, jsonList{&w.IgnoredAtRisk}, &w.UpstreamState, &w.UnpushedCount, msTime{&w.LastCommitAt},
		&w.Locked, &w.LockedReason, &w.LockAgentID, &w.LockPID, &w.PrunableReason, &w.SessionID,
		&w.AgentID, msTime{&w.FirstSeen}, msTime{&w.LastSeen}, msTime{&w.RemovedAt})
	return w, err
}

// UpsertWorktrees writes each worktree, keyed on its path, in one
// transaction. Every field the scanner reports is written as given; a new
// row's FirstSeen is its LastSeen, an existing row keeps its FirstSeen, and
// RemovedAt is cleared, since the worktree was just seen.
func (s *Store) UpsertWorktrees(ctx context.Context, wts ...Worktree) error {
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		for _, w := range wts {
			if w.Path == "" || w.MainRepo == "" {
				return fmt.Errorf("store: upsert worktree %q: empty path or main repo", w.Path)
			}
			ignored, err := listJSON(w.IgnoredAtRisk)
			if err != nil {
				return err
			}
			seen := s.stamp(w.LastSeen).UnixMilli()
			_, err = tx.ExecContext(ctx, `
				INSERT INTO worktrees (`+worktreeColumns+`)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)
				ON CONFLICT (path) DO UPDATE SET
					path = excluded.path,
					main_repo = excluded.main_repo,
					branch = excluded.branch,
					detached = excluded.detached,
					head_sha = excluded.head_sha,
					dirty_count = excluded.dirty_count,
					untracked_count = excluded.untracked_count,
					ignored_at_risk = excluded.ignored_at_risk,
					upstream_state = excluded.upstream_state,
					unpushed_count = excluded.unpushed_count,
					last_commit_at = excluded.last_commit_at,
					locked = excluded.locked,
					locked_reason = excluded.locked_reason,
					lock_agent_id = excluded.lock_agent_id,
					lock_pid = excluded.lock_pid,
					prunable_reason = excluded.prunable_reason,
					session_id = excluded.session_id,
					agent_id = excluded.agent_id,
					last_seen = excluded.last_seen,
					removed_at = NULL`,
				w.Path, w.MainRepo, w.Branch, w.Detached, w.HeadSHA, w.DirtyCount,
				w.UntrackedCount, ignored, w.UpstreamState, w.UnpushedCount, ms(w.LastCommitAt),
				w.Locked, w.LockedReason, w.LockAgentID, w.LockPID, w.PrunableReason, w.SessionID,
				w.AgentID, seen, seen)
			if err != nil {
				return fmt.Errorf("store: upsert worktree %s: %w", w.Path, err)
			}
		}
		return nil
	})
}

// GetWorktree returns the worktree at path, removed or not.
func (s *Store) GetWorktree(ctx context.Context, path string) (Worktree, error) {
	w, err := scanWorktree(s.r.QueryRowContext(ctx, `SELECT `+worktreeColumns+` FROM worktrees WHERE path = ?`, path))
	if err != nil {
		return Worktree{}, notFound(err, "worktree %s", path)
	}
	return w, nil
}

// WorktreeFilter narrows ListWorktrees.
type WorktreeFilter struct {
	MainRepo       string // only this repo's worktrees; "" for all
	IncludeRemoved bool   // list removed worktrees too
}

// ListWorktrees returns worktrees by main repo, then path.
func (s *Store) ListWorktrees(ctx context.Context, f WorktreeFilter) ([]Worktree, error) {
	q := `SELECT ` + worktreeColumns + ` FROM worktrees WHERE 1`
	var args []any
	if f.MainRepo != "" {
		q += ` AND main_repo = ?`
		args = append(args, f.MainRepo)
	}
	if !f.IncludeRemoved {
		q += ` AND removed_at IS NULL`
	}
	q += ` ORDER BY main_repo, path`
	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Worktree
	for rows.Next() {
		w, err := scanWorktree(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// MarkWorktreeRemoved records that the worktree at path is gone, at the
// given time (zero means now). A worktree already marked keeps its first
// RemovedAt. It returns ErrNotFound when there is no row for path.
func (s *Store) MarkWorktreeRemoved(ctx context.Context, path string, at time.Time) error {
	res, err := s.w.ExecContext(ctx, `UPDATE worktrees SET removed_at = coalesce(removed_at, ?) WHERE path = ?`,
		s.stamp(at).UnixMilli(), path)
	if err != nil {
		return fmt.Errorf("store: mark worktree %s removed: %w", path, err)
	}
	return mustAffect(res, "worktree %s", path)
}

// MarkMissingWorktreesRemoved marks removed, at the given time (zero means
// now), every worktree of mainRepo that isn't removed yet and whose path
// isn't in present: the paths one scan of that repo found. It returns how
// many it marked. Paths compare case-insensitively.
func (s *Store) MarkMissingWorktreesRemoved(ctx context.Context, mainRepo string, present []string, at time.Time) (int64, error) {
	if mainRepo == "" {
		return 0, errors.New("store: mark missing worktrees: empty main repo")
	}
	paths, err := listJSON(present)
	if err != nil {
		return 0, err
	}
	res, err := s.w.ExecContext(ctx, `
		UPDATE worktrees SET removed_at = ?
		WHERE main_repo = ? AND removed_at IS NULL
		  AND path NOT IN (SELECT value FROM json_each(?))`,
		s.stamp(at).UnixMilli(), mainRepo, paths)
	if err != nil {
		return 0, fmt.Errorf("store: mark missing worktrees of %s: %w", mainRepo, err)
	}
	return res.RowsAffected()
}
