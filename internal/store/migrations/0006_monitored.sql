-- 0006_monitored: the sessions you chose to track (internal/monitor). The
-- conventions are 0001's.
--
-- A row says a session is monitored, and why: it registered a plan (reason
-- plan: POST /api/plans with its session_id; plan_id names the plan, and the
-- row goes when the plan is unregistered) or you asked to watch it (reason
-- watch: /aim watch or the dashboard's Watch button; plan_id is NULL, and
-- /aim unwatch removes the row). The third rule, an agent of a registered
-- plan's package or of a monitored lead, is worked out from links and
-- lineage on every read (internal/monitor) and never stored.
--
-- session_id is Claude Code's session id: what the mod reads with
-- $.session.id() and sessions.session_id holds. It changes on /clear and
-- when a background job respawns, so the store carries the rows over to the
-- new id wherever it sees that happen: MoveSessionID (the mod's /clear
-- handover) and UpsertSessions (a poll that lists a new session id under a
-- session's key, which also sets prev_session_id). A watch so follows the
-- session (its process, or its background job), not the conversation it
-- started in, and coming back to a conversation with --resume brings its
-- watch back.
CREATE TABLE monitored_sessions (
    id         INTEGER PRIMARY KEY,
    session_id TEXT    NOT NULL CHECK (session_id <> ''),
    reason     TEXT    NOT NULL, -- plan | watch
    plan_id    INTEGER REFERENCES plans (id) ON DELETE CASCADE, -- reason plan only
    since      INTEGER NOT NULL
);
CREATE UNIQUE INDEX monitored_sessions_watch ON monitored_sessions (session_id) WHERE reason = 'watch';
CREATE UNIQUE INDEX monitored_sessions_plan ON monitored_sessions (session_id, plan_id) WHERE reason = 'plan';
CREATE INDEX monitored_sessions_plan_id ON monitored_sessions (plan_id) WHERE plan_id IS NOT NULL;
