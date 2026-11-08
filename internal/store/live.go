package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// This file holds what the event receiver (internal/live) writes: the
// session fields the mod's events decide, a session row the mod reports
// before the poller lists it, and /clear continuity. Its columns were in
// 0001, except the plugin build (0014); MoveSessionID also moves 0006's
// monitored_sessions rows.

// ModSessionKeyPrefix starts the key of a session row the event receiver
// created before the poller listed the session: "session:<sessionId>"
// (SessionKey with no job id and no pid). The poller's own row for the same
// session adopts it (AdoptSession).
const ModSessionKeyPrefix = "session:"

// FindSession returns the row for a Claude Code session id: the live row,
// preferring the poller's over one the event receiver created, else the
// most recently ended one. ErrNotFound if there is none.
func (s *Store) FindSession(ctx context.Context, sessionID string) (Session, error) {
	row := s.r.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions
		WHERE session_id = ?
		ORDER BY ended_at IS NOT NULL, key LIKE 'session:%', coalesce(ended_at, 0) DESC, id DESC
		LIMIT 1`, sessionID)
	ss, err := scanSession(row)
	if err != nil {
		return Session{}, notFound(err, "session %s", sessionID)
	}
	return ss, nil
}

// SessionLive is what the event receiver decides about a live session row.
// The state fields and the activity fields are always written; Cwd,
// WorktreePath, Repo, Host, ItermSessionID, PluginBuild, PluginRoot and
// Kind only when non-empty,
// Branch whenever WorktreePath is (an empty branch then means unknown: it
// belongs to the worktree), and Headless only when set.
type SessionLive struct {
	State       string
	StateReason string
	// NeedsYouSince is written as given: zero clears it.
	NeedsYouSince time.Time
	LastAction    string
	LastSaid      string
	LastEventAt   time.Time

	Cwd            string
	WorktreePath   string
	Repo           string
	Branch         string
	Host           string
	ItermSessionID string
	// PluginBuild and PluginRoot are what session_start said about the
	// plugin the session loaded (Session.PluginBuild).
	PluginBuild string
	PluginRoot  string
	// Kind is written only when the row has none yet (the poller's kind
	// wins).
	Kind     string
	Headless *bool
}

// UpdateSessionLive writes what the event receiver decided into the live
// row for key. ErrNotFound if the key has no live row.
func (s *Store) UpdateSessionLive(ctx context.Context, key string, u SessionLive) error {
	var headless any
	if u.Headless != nil {
		headless = *u.Headless
	}
	res, err := s.w.ExecContext(ctx, `
		UPDATE sessions SET
			state            = :state,
			state_reason     = :state_reason,
			needs_you_since  = :needs_you_since,
			last_action      = :last_action,
			last_said        = :last_said,
			last_event_at    = :last_event_at,
			cwd              = CASE WHEN :cwd = '' THEN cwd ELSE :cwd END,
			worktree_path    = CASE WHEN :worktree_path = '' THEN worktree_path ELSE :worktree_path END,
			repo             = CASE WHEN :repo = '' THEN repo ELSE :repo END,
			branch           = CASE WHEN :worktree_path = '' THEN branch ELSE :branch END,
			host             = CASE WHEN :host = '' THEN host ELSE :host END,
			iterm_session_id = CASE WHEN :iterm = '' THEN iterm_session_id ELSE :iterm END,
			plugin_build     = CASE WHEN :plugin_build = '' THEN plugin_build ELSE :plugin_build END,
			plugin_root      = CASE WHEN :plugin_root = '' THEN plugin_root ELSE :plugin_root END,
			kind             = CASE WHEN kind = '' THEN :kind ELSE kind END,
			headless         = coalesce(:headless, headless)
		WHERE key = :key AND ended_at IS NULL`,
		sql.Named("state", u.State),
		sql.Named("state_reason", u.StateReason),
		sql.Named("needs_you_since", ms(u.NeedsYouSince)),
		sql.Named("last_action", u.LastAction),
		sql.Named("last_said", u.LastSaid),
		sql.Named("last_event_at", ms(u.LastEventAt)),
		sql.Named("cwd", u.Cwd),
		sql.Named("worktree_path", u.WorktreePath),
		sql.Named("repo", u.Repo),
		sql.Named("branch", u.Branch),
		sql.Named("host", u.Host),
		sql.Named("iterm", u.ItermSessionID),
		sql.Named("plugin_build", u.PluginBuild),
		sql.Named("plugin_root", u.PluginRoot),
		sql.Named("kind", u.Kind),
		sql.Named("headless", headless),
		sql.Named("key", key),
	)
	if err != nil {
		return fmt.Errorf("store: update live session %s: %w", key, err)
	}
	return mustAffect(res, "live session %s", key)
}

// AdoptSession folds the live row for fromKey (one the event receiver
// created) into the live row for toKey (the poller's, for the same
// session). When toKey has no live row, fromKey's row simply takes that
// key. Otherwise toKey's row takes the mod's fields from it (the state,
// the activity, iTerm session id, the plugin build, prev_session_id) and
// fromKey's row is
// deleted, since both described the same session. A fromKey with no live
// row is not an error: there is nothing to adopt.
func (s *Store) AdoptSession(ctx context.Context, fromKey, toKey string) error {
	if fromKey == toKey {
		return nil
	}
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		var fromID int64
		err := tx.QueryRowContext(ctx, `SELECT id FROM sessions WHERE key = ? AND ended_at IS NULL`, fromKey).Scan(&fromID)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return fmt.Errorf("store: adopt session %s: %w", fromKey, err)
		}
		var toID int64
		err = tx.QueryRowContext(ctx, `SELECT id FROM sessions WHERE key = ? AND ended_at IS NULL`, toKey).Scan(&toID)
		if err == sql.ErrNoRows {
			_, err = tx.ExecContext(ctx, `UPDATE sessions SET key = ? WHERE id = ?`, toKey, fromID)
			if err != nil {
				return fmt.Errorf("store: adopt session %s as %s: %w", fromKey, toKey, err)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("store: adopt session %s: %w", toKey, err)
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE sessions SET
				state            = f.state,
				state_reason     = f.state_reason,
				needs_you_since  = f.needs_you_since,
				last_action      = CASE WHEN f.last_action = '' THEN sessions.last_action ELSE f.last_action END,
				last_said        = CASE WHEN f.last_said = '' THEN sessions.last_said ELSE f.last_said END,
				last_event_at    = coalesce(f.last_event_at, sessions.last_event_at),
				iterm_session_id = CASE WHEN f.iterm_session_id = '' THEN sessions.iterm_session_id ELSE f.iterm_session_id END,
				plugin_build     = CASE WHEN f.plugin_build = '' THEN sessions.plugin_build ELSE f.plugin_build END,
				plugin_root      = CASE WHEN f.plugin_root = '' THEN sessions.plugin_root ELSE f.plugin_root END,
				prev_session_id  =CASE WHEN sessions.prev_session_id = '' THEN f.prev_session_id ELSE sessions.prev_session_id END
			FROM (SELECT * FROM sessions WHERE id = ?) AS f
			WHERE sessions.id = ?`, fromID, toID)
		if err != nil {
			return fmt.Errorf("store: adopt session %s into %s: %w", fromKey, toKey, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, fromID); err != nil {
			return fmt.Errorf("store: adopt session %s: %w", fromKey, err)
		}
		return nil
	})
}

// MoveSessionID carries a session over to a new session id after /clear:
// the live row for oldID gets newID as its session id and oldID as its
// prev_session_id; its agents, its agent links (and their tombstones), the
// plans it registered and its monitoring (monitored_sessions: plan lead,
// watch) move to newID. Rows that already exist under newID are kept as
// they are and the old ones left behind. It returns whether a live row
// moved.
func (s *Store) MoveSessionID(ctx context.Context, oldID, newID string) (bool, error) {
	if oldID == "" || newID == "" || oldID == newID {
		return false, nil
	}
	var moved bool
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		// A live row already under newID (the poller saw the new id first)
		// only needs prev_session_id.
		if _, err := tx.ExecContext(ctx, `
			UPDATE sessions SET prev_session_id = ?
			WHERE session_id = ? AND ended_at IS NULL AND prev_session_id = ''`, oldID, newID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE sessions SET session_id = ?, prev_session_id = ?
			WHERE session_id = ? AND ended_at IS NULL
			  AND NOT EXISTS (SELECT 1 FROM sessions WHERE session_id = ? AND ended_at IS NULL)`,
			newID, oldID, oldID, newID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		moved = n > 0
		if !moved {
			// The poller's row already has the new id; a row the event
			// receiver made for the old id would now be a duplicate.
			if _, err := tx.ExecContext(ctx, `
				DELETE FROM sessions WHERE session_id = ? AND ended_at IS NULL AND key LIKE 'session:%'
				  AND EXISTS (SELECT 1 FROM sessions WHERE session_id = ? AND ended_at IS NULL)`, oldID, newID); err != nil {
				return err
			}
		}
		stmts := []string{
			`UPDATE OR IGNORE agents SET session_id = ? WHERE session_id = ?`,
			`UPDATE OR IGNORE links SET session_id = ?1, ref = ?1 || '/' || agent_id
				WHERE kind = 'agent' AND session_id = ?2 AND ref = ?2 || '/' || agent_id`,
			`UPDATE OR IGNORE link_tombstones SET session_id = ?1, ref = ?1 || '/' || agent_id
				WHERE kind = 'agent' AND session_id = ?2 AND ref = ?2 || '/' || agent_id`,
			`UPDATE plans SET registered_in_session = ? WHERE registered_in_session = ?`,
			`UPDATE OR IGNORE monitored_sessions SET session_id = ? WHERE session_id = ?`,
		}
		for _, q := range stmts {
			if _, err := tx.ExecContext(ctx, q, newID, oldID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("store: move session %s to %s: %w", oldID, newID, err)
	}
	return moved, nil
}

// IsModSessionKey reports whether a session key is one the event receiver
// made (ModSessionKeyPrefix).
func IsModSessionKey(key string) bool { return strings.HasPrefix(key, ModSessionKeyPrefix) }
