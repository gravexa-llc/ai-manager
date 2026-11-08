package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// Session is a row of the sessions table: one Claude Code session from
// `claude agents --json`, refined by mod events.
//
// Fields fall into three groups by who writes them:
//
//   - Observed (the session poller, through UpsertSessions): Key, SessionID,
//     JobID, Kind, Headless, Name, Host, Cwd, WorktreePath, Repo, Branch,
//     PID, State, StateReason, NeedsYouSince, StartedAt, LastSeenAt.
//   - Activity (the mod, through UpdateSessionLive): ItermSessionID,
//     LastAction, LastSaid, LastEventAt, PluginBuild, PluginRoot. DismissNeedsYou sets
//     NeedsYouDismissedAt and EndSession sets EndedAt.
//   - Kept by the store: ID, PrevSessionID, FirstSeenAt.
//
// UpsertSessions ignores the last two groups, so a poll never erases what
// the mod or the dashboard wrote.
type Session struct {
	// ID is the row's id. A key can have several rows over time (see Key),
	// so this is the only permanent identity of a row.
	ID int64

	// Key is the poller's stable key, internal/sessions Session.Key:
	// "job:<id>" for a background job, "pid:<pid>" otherwise. Leave it
	// empty to have it derived with SessionKey. Only one row per key is
	// live (EndedAt zero); a key upserted after its row ended starts a new
	// row, so a pid reused by a later process doesn't inherit the old
	// session's history.
	Key string

	SessionID string // `sessionId`; changes when a background job respawns
	JobID     string // `id`, a background job's stable id
	// PrevSessionID is the session id before the last change, kept when an
	// upsert brings a different non-empty SessionID.
	PrevSessionID string

	Kind     string // `kind`: interactive | background, or as listed
	Headless bool   // a `claude -p` run, hidden by default
	Name     string
	Host     string
	Cwd      string
	// WorktreePath, Repo and Branch are where Cwd is, as the caller
	// resolved it.
	WorktreePath   string
	Repo           string
	Branch         string
	ItermSessionID string
	PID            int

	// State is the session state (internal/sessions State)
	// and StateReason why, e.g. the `waitingFor` text.
	State       string
	StateReason string

	// NeedsYouSince is when the session was first seen needing you. An
	// upsert keeps the stored time while it is set and the new one is set
	// too, and clears it when the new one is zero: pass the poll time while
	// the session needs you and the zero time otherwise.
	NeedsYouSince       time.Time
	NeedsYouDismissedAt time.Time

	LastAction string // a short label, never a prompt or tool output
	LastSaid   string // a short summary, never the text itself

	// PluginBuild is the plugin build the session loaded at its start, as
	// the mod reported it on session_start (internal/events PluginBuild),
	// and PluginRoot the plugin's folder. Both are "" until reported.
	PluginBuild string
	PluginRoot  string

	StartedAt time.Time // `startedAt`
	// FirstSeenAt and LastSeenAt are the first and latest poll that listed
	// the session. Pass the poll time as LastSeenAt; zero means now.
	FirstSeenAt time.Time
	LastSeenAt  time.Time
	LastEventAt time.Time
	EndedAt     time.Time
}

// SessionKey derives a session's key the way internal/sessions does:
// "job:<id>" when there is a job id, else "pid:<pid>", else
// "session:<sessionId>". It returns "" when all three are empty.
func SessionKey(jobID string, pid int, sessionID string) string {
	switch {
	case jobID != "":
		return "job:" + jobID
	case pid > 0:
		return "pid:" + strconv.Itoa(pid)
	case sessionID != "":
		return "session:" + sessionID
	default:
		return ""
	}
}

const sessionColumns = `id, key, session_id, job_id, prev_session_id, kind, headless,
	name, host, cwd, worktree_path, repo, branch, iterm_session_id, pid, state,
	state_reason, needs_you_since, needs_you_dismissed_at, last_action, last_said,
	plugin_build, plugin_root,
	started_at, first_seen_at, last_seen_at, last_event_at, ended_at`

func scanSession(sc scanner) (Session, error) {
	var s Session
	err := sc.Scan(&s.ID, &s.Key, &s.SessionID, &s.JobID, &s.PrevSessionID, &s.Kind, &s.Headless,
		&s.Name, &s.Host, &s.Cwd, &s.WorktreePath, &s.Repo, &s.Branch, &s.ItermSessionID, &s.PID, &s.State,
		&s.StateReason, msTime{&s.NeedsYouSince}, msTime{&s.NeedsYouDismissedAt}, &s.LastAction, &s.LastSaid,
		&s.PluginBuild, &s.PluginRoot,
		msTime{&s.StartedAt}, msTime{&s.FirstSeenAt}, msTime{&s.LastSeenAt}, msTime{&s.LastEventAt}, msTime{&s.EndedAt})
	return s, err
}

// upsertSession writes the observed fields into the live row for the key,
// or starts a row. An empty SessionID or JobID never erases a stored one.
const upsertSession = `
INSERT INTO sessions (
	key, session_id, job_id, kind, headless, name, host, cwd, worktree_path,
	repo, branch, pid, state, state_reason, needs_you_since, started_at,
	first_seen_at, last_seen_at
) VALUES (
	:key, :session_id, :job_id, :kind, :headless, :name, :host, :cwd, :worktree_path,
	:repo, :branch, :pid, :state, :state_reason, :needs_you_since, :started_at,
	:seen_at, :seen_at
)
ON CONFLICT (key) WHERE ended_at IS NULL DO UPDATE SET
	prev_session_id = CASE
		WHEN excluded.session_id <> '' AND sessions.session_id <> ''
		     AND excluded.session_id <> sessions.session_id THEN sessions.session_id
		ELSE sessions.prev_session_id END,
	session_id      = CASE WHEN excluded.session_id = '' THEN sessions.session_id ELSE excluded.session_id END,
	job_id          = CASE WHEN excluded.job_id = '' THEN sessions.job_id ELSE excluded.job_id END,
	kind            = excluded.kind,
	headless        = excluded.headless,
	name            = excluded.name,
	host            = excluded.host,
	cwd             = excluded.cwd,
	worktree_path   = excluded.worktree_path,
	repo            = excluded.repo,
	branch          = excluded.branch,
	pid             = excluded.pid,
	state           = excluded.state,
	state_reason    = excluded.state_reason,
	needs_you_since = CASE WHEN excluded.needs_you_since IS NULL THEN NULL
	                       ELSE coalesce(sessions.needs_you_since, excluded.needs_you_since) END,
	started_at      = excluded.started_at,
	last_seen_at    = excluded.last_seen_at`

// UpsertSessions writes what a poll observed about each session, in one
// transaction. Each session goes into the live row for its key, or starts a
// new row when the key has none. Only the observed fields are written (see
// Session); a new row's FirstSeenAt is its LastSeenAt.
//
// When a live row gets a new session id (a /clear the mod didn't report,
// or a background job that respawned), the session's monitoring
// (monitored_sessions) moves to the new id with it.
func (s *Store) UpsertSessions(ctx context.Context, sessions ...Session) error {
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		for _, ss := range sessions {
			key := ss.Key
			if key == "" {
				key = SessionKey(ss.JobID, ss.PID, ss.SessionID)
			}
			if key == "" {
				return errors.New("store: upsert session: no key, job id, pid or session id")
			}
			var was string
			err := tx.QueryRowContext(ctx, `SELECT session_id FROM sessions WHERE key = ? AND ended_at IS NULL`, key).Scan(&was)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("store: upsert session %s: %w", key, err)
			}
			_, err = tx.ExecContext(ctx, upsertSession,
				sql.Named("key", key),
				sql.Named("session_id", ss.SessionID),
				sql.Named("job_id", ss.JobID),
				sql.Named("kind", ss.Kind),
				sql.Named("headless", ss.Headless),
				sql.Named("name", ss.Name),
				sql.Named("host", ss.Host),
				sql.Named("cwd", ss.Cwd),
				sql.Named("worktree_path", ss.WorktreePath),
				sql.Named("repo", ss.Repo),
				sql.Named("branch", ss.Branch),
				sql.Named("pid", ss.PID),
				sql.Named("state", ss.State),
				sql.Named("state_reason", ss.StateReason),
				sql.Named("needs_you_since", ms(ss.NeedsYouSince)),
				sql.Named("started_at", ms(ss.StartedAt)),
				sql.Named("seen_at", s.stamp(ss.LastSeenAt).UnixMilli()),
			)
			if err != nil {
				return fmt.Errorf("store: upsert session %s: %w", key, err)
			}
			if err := carryMonitored(ctx, tx, was, ss.SessionID); err != nil {
				return fmt.Errorf("store: upsert session %s: %w", key, err)
			}
		}
		return nil
	})
}

// GetSession returns the live row for key.
func (s *Store) GetSession(ctx context.Context, key string) (Session, error) {
	row := s.r.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE key = ? AND ended_at IS NULL`, key)
	ss, err := scanSession(row)
	if err != nil {
		return Session{}, notFound(err, "live session %s", key)
	}
	return ss, nil
}

// SessionFilter narrows ListSessions.
type SessionFilter struct {
	// IncludeEnded lists ended rows too. By default only live rows are
	// listed.
	IncludeEnded bool
}

// ListSessions returns sessions, live ones first, then by start time
// (first seen when the start time is unknown).
func (s *Store) ListSessions(ctx context.Context, f SessionFilter) ([]Session, error) {
	q := `SELECT ` + sessionColumns + ` FROM sessions`
	if !f.IncludeEnded {
		q += ` WHERE ended_at IS NULL`
	}
	q += ` ORDER BY ended_at IS NOT NULL, coalesce(started_at, first_seen_at), id`
	rows, err := s.r.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		ss, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ss)
	}
	return out, rows.Err()
}

// EndSession marks the live row for key ended at the given time (zero means
// now): the session left `claude agents --json`, or the mod reported its
// end. The row stays, for history.
func (s *Store) EndSession(ctx context.Context, key string, at time.Time) error {
	res, err := s.w.ExecContext(ctx, `UPDATE sessions SET ended_at = ? WHERE key = ? AND ended_at IS NULL`,
		s.stamp(at).UnixMilli(), key)
	if err != nil {
		return fmt.Errorf("store: end session %s: %w", key, err)
	}
	return mustAffect(res, "live session %s", key)
}

// DismissNeedsYou records that you dismissed the live session's needs-you
// alert at the given time (zero means now). The alert counts as dismissed
// while NeedsYouDismissedAt is at or after NeedsYouSince, so the next
// needs-you spell shows again.
func (s *Store) DismissNeedsYou(ctx context.Context, key string, at time.Time) error {
	res, err := s.w.ExecContext(ctx, `UPDATE sessions SET needs_you_dismissed_at = ? WHERE key = ? AND ended_at IS NULL`,
		s.stamp(at).UnixMilli(), key)
	if err != nil {
		return fmt.Errorf("store: dismiss session %s: %w", key, err)
	}
	return mustAffect(res, "live session %s", key)
}

// SessionActivity is what the mod reports about a session between polls.
type SessionActivity struct {
	LastAction string // a short label, e.g. "Bash: make check"
	LastSaid   string // a short summary
	// ItermSessionID is written only when non-empty.
	ItermSessionID string
	// At is when it happened; zero means now.
	At time.Time
}

// UpdateSessionActivity writes the mod's latest activity into the live row
// for key.
func (s *Store) UpdateSessionActivity(ctx context.Context, key string, a SessionActivity) error {
	res, err := s.w.ExecContext(ctx, `
		UPDATE sessions SET
			last_action = ?,
			last_said = ?,
			iterm_session_id = CASE WHEN ? = '' THEN iterm_session_id ELSE ? END,
			last_event_at = ?
		WHERE key = ? AND ended_at IS NULL`,
		a.LastAction, a.LastSaid, a.ItermSessionID, a.ItermSessionID, s.stamp(a.At).UnixMilli(), key)
	if err != nil {
		return fmt.Errorf("store: update session %s: %w", key, err)
	}
	return mustAffect(res, "live session %s", key)
}
