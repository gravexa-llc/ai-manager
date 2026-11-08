-- 0001_init: the first tables.
--
-- Conventions, shared by every later migration (store.go says why):
--   * Times are INTEGER Unix milliseconds, UTC. NULL means "not set".
--   * Optional text is NOT NULL DEFAULT '', never NULL, so it can sit in a
--     unique index (SQLite treats NULLs as distinct there).
--   * Booleans are INTEGER 0 / 1.
--   * Lists of strings are JSON arrays in a TEXT column.
--   * Values from a fixed set (link kinds, states) are checked in Go, not with
--     CHECK, so a later change can add one without rebuilding the table.
--   * Paths, repo names, plan slugs and package IDs compare case-insensitively
--     (COLLATE NOCASE). Branch names and SHAs don't.

-- A registered plan file. Any session in one of its repos can dispatch for it.
CREATE TABLE plans (
    id                    INTEGER PRIMARY KEY,
    slug                  TEXT    NOT NULL COLLATE NOCASE UNIQUE,
    title                 TEXT    NOT NULL DEFAULT '',
    source_path           TEXT    NOT NULL DEFAULT '' COLLATE NOCASE,
    source_md             TEXT    NOT NULL DEFAULT '', -- snapshot of the plan file
    repos                 TEXT    NOT NULL DEFAULT '[]'
                          CHECK (json_valid(repos) AND json_type(repos) = 'array'),
    registered_in_session TEXT    NOT NULL DEFAULT '',
    status                TEXT    NOT NULL DEFAULT '',
    created_at            INTEGER NOT NULL,
    updated_at            INTEGER NOT NULL
);
CREATE INDEX plans_source_path ON plans (source_path);

-- One row of a plan's package table.
CREATE TABLE packages (
    plan_id      INTEGER NOT NULL REFERENCES plans (id) ON DELETE CASCADE,
    pkg_id       TEXT    NOT NULL COLLATE NOCASE,
    seq          INTEGER NOT NULL DEFAULT 0, -- position in the plan file
    title        TEXT    NOT NULL DEFAULT '',
    repo         TEXT    NOT NULL DEFAULT '',
    size         TEXT    NOT NULL DEFAULT '',
    needs        TEXT    NOT NULL DEFAULT '[]' -- package IDs, as written
                 CHECK (json_valid(needs) AND json_type(needs) = 'array'),
    needs_note   TEXT    NOT NULL DEFAULT '', -- the rest of the Needs cell
    owner        TEXT    NOT NULL DEFAULT '',
    lane         TEXT    NOT NULL DEFAULT '',
    done_in_plan INTEGER NOT NULL DEFAULT 0,
    done_manual  INTEGER NOT NULL DEFAULT 0,
    note         TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (plan_id, pkg_id)
);

-- How a package is tied to work: an agent, a branch, a PR, a worktree or a
-- cloud session. A row is unique per package and target.
CREATE TABLE links (
    id           INTEGER PRIMARY KEY,
    plan_id      INTEGER NOT NULL,
    pkg_id       TEXT    NOT NULL COLLATE NOCASE,
    kind         TEXT    NOT NULL, -- agent | branch | pr | worktree | cloud_session
    session_id   TEXT    NOT NULL DEFAULT '',
    job_id       TEXT    NOT NULL DEFAULT '',
    agent_id     TEXT    NOT NULL DEFAULT '',
    ref          TEXT    NOT NULL DEFAULT '', -- branch, "owner/repo#123", path, session URL
    match_method TEXT    NOT NULL, -- spawn | plan_line | title | branch | manual
    confidence   TEXT    NOT NULL, -- exact | high | low
    created_at   INTEGER NOT NULL,
    FOREIGN KEY (plan_id, pkg_id) REFERENCES packages (plan_id, pkg_id)
        ON DELETE CASCADE ON UPDATE CASCADE
);
CREATE UNIQUE INDEX links_target ON links (plan_id, pkg_id, kind, session_id, job_id, agent_id, ref);
CREATE INDEX links_agent ON links (session_id, agent_id) WHERE session_id <> '';
CREATE INDEX links_job ON links (job_id) WHERE job_id <> '';
CREATE INDEX links_ref ON links (kind, ref) WHERE ref <> '';

-- Every worktree the scanner has seen, including main checkouts. A worktree
-- that disappears keeps its row with removed_at set.
CREATE TABLE worktrees (
    path            TEXT    NOT NULL COLLATE NOCASE PRIMARY KEY,
    main_repo       TEXT    NOT NULL COLLATE NOCASE,
    branch          TEXT    NOT NULL DEFAULT '',
    detached        INTEGER NOT NULL DEFAULT 0,
    head_sha        TEXT    NOT NULL DEFAULT '',
    dirty_count     INTEGER NOT NULL DEFAULT 0,
    untracked_count INTEGER NOT NULL DEFAULT 0,
    ignored_at_risk TEXT    NOT NULL DEFAULT '[]' -- ignored paths outside the rebuildable list
                    CHECK (json_valid(ignored_at_risk) AND json_type(ignored_at_risk) = 'array'),
    upstream_state  TEXT    NOT NULL DEFAULT '', -- ok | gone | none
    unpushed_count  INTEGER NOT NULL DEFAULT 0,
    last_commit_at  INTEGER,
    locked          INTEGER NOT NULL DEFAULT 0, -- git allows a lock with no reason
    locked_reason   TEXT    NOT NULL DEFAULT '',
    lock_agent_id   TEXT    NOT NULL DEFAULT '',
    lock_pid        INTEGER NOT NULL DEFAULT 0,
    prunable_reason TEXT    NOT NULL DEFAULT '',
    session_id      TEXT    NOT NULL DEFAULT '',
    agent_id        TEXT    NOT NULL DEFAULT '',
    first_seen      INTEGER NOT NULL,
    last_seen       INTEGER NOT NULL,
    removed_at      INTEGER
);
CREATE INDEX worktrees_repo_branch ON worktrees (main_repo, branch);
CREATE INDEX worktrees_lock_agent ON worktrees (lock_agent_id) WHERE lock_agent_id <> '';

-- Pull requests, keyed on the nameWithOwner GitHub returns (a local remote
-- can still name an old owner) and the number. The body is never stored, only
-- the slug and package ID parsed from its Plan: line.
CREATE TABLE prs (
    repo                TEXT    NOT NULL COLLATE NOCASE,
    number              INTEGER NOT NULL,
    head_branch         TEXT    NOT NULL DEFAULT '',
    head_ref_oid        TEXT    NOT NULL DEFAULT '',
    base_ref            TEXT    NOT NULL DEFAULT '',
    is_cross_repository INTEGER NOT NULL DEFAULT 0,
    title               TEXT    NOT NULL DEFAULT '',
    author              TEXT    NOT NULL DEFAULT '',
    state               TEXT    NOT NULL DEFAULT '', -- OPEN | CLOSED | MERGED
    draft               INTEGER NOT NULL DEFAULT 0,
    mergeable           TEXT    NOT NULL DEFAULT '',
    merge_state_status  TEXT    NOT NULL DEFAULT '',
    review_decision     TEXT    NOT NULL DEFAULT '',
    checks_state        TEXT    NOT NULL DEFAULT '', -- '' = no CI (statusCheckRollup null)
    plan_slug           TEXT    NOT NULL DEFAULT '' COLLATE NOCASE,
    plan_pkg            TEXT    NOT NULL DEFAULT '' COLLATE NOCASE,
    url                 TEXT    NOT NULL DEFAULT '',
    updated_at          INTEGER, -- GitHub's updatedAt
    merged_at           INTEGER,
    closed_at           INTEGER,
    PRIMARY KEY (repo, number)
);
CREATE INDEX prs_head_branch ON prs (repo, head_branch);
CREATE INDEX prs_plan ON prs (plan_slug, plan_pkg) WHERE plan_slug <> '';
CREATE INDEX prs_state ON prs (state);

-- Sessions from `claude agents --json`, refined by the mod. key is the
-- poller's stable key (internal/sessions Session.Key): "job:<id>" for a
-- background job, "pid:<pid>" otherwise. Only one live row (ended_at NULL)
-- per key; a pid reused after its session ended starts a new row.
CREATE TABLE sessions (
    id                     INTEGER PRIMARY KEY,
    key                    TEXT    NOT NULL,
    session_id             TEXT    NOT NULL DEFAULT '',
    job_id                 TEXT    NOT NULL DEFAULT '', -- a background job's stable id
    prev_session_id        TEXT    NOT NULL DEFAULT '',
    kind                   TEXT    NOT NULL DEFAULT '', -- interactive | background
    headless               INTEGER NOT NULL DEFAULT 0,
    name                   TEXT    NOT NULL DEFAULT '',
    host                   TEXT    NOT NULL DEFAULT '',
    cwd                    TEXT    NOT NULL DEFAULT '' COLLATE NOCASE,
    worktree_path          TEXT    NOT NULL DEFAULT '' COLLATE NOCASE,
    repo                   TEXT    NOT NULL DEFAULT '' COLLATE NOCASE,
    branch                 TEXT    NOT NULL DEFAULT '',
    iterm_session_id       TEXT    NOT NULL DEFAULT '',
    pid                    INTEGER NOT NULL DEFAULT 0,
    state                  TEXT    NOT NULL DEFAULT '',
    state_reason           TEXT    NOT NULL DEFAULT '', -- e.g. waitingFor
    needs_you_since        INTEGER,
    needs_you_dismissed_at INTEGER,
    last_action            TEXT    NOT NULL DEFAULT '',
    last_said              TEXT    NOT NULL DEFAULT '',
    started_at             INTEGER,
    first_seen_at          INTEGER NOT NULL,
    last_seen_at           INTEGER NOT NULL,
    last_event_at          INTEGER,
    ended_at               INTEGER
);
CREATE UNIQUE INDEX sessions_live_key ON sessions (key) WHERE ended_at IS NULL;
CREATE INDEX sessions_session_id ON sessions (session_id) WHERE session_id <> '';
CREATE INDEX sessions_job_id ON sessions (job_id) WHERE job_id <> '';

-- The loops inside a session: the main loop (kind lead, agent_id '') and its
-- subagents and teammates.
CREATE TABLE agents (
    session_id      TEXT    NOT NULL,
    agent_id        TEXT    NOT NULL DEFAULT '',
    kind            TEXT    NOT NULL DEFAULT '', -- lead | subagent | teammate
    agent_type      TEXT    NOT NULL DEFAULT '',
    background      INTEGER NOT NULL DEFAULT 0,
    model           TEXT    NOT NULL DEFAULT '',
    parent_agent_id TEXT    NOT NULL DEFAULT '',
    worktree_path   TEXT    NOT NULL DEFAULT '' COLLATE NOCASE,
    state           TEXT    NOT NULL DEFAULT '',
    last_action     TEXT    NOT NULL DEFAULT '',
    started_at      INTEGER,
    ended_at        INTEGER,
    PRIMARY KEY (session_id, agent_id)
);
CREATE INDEX agents_agent_id ON agents (agent_id) WHERE agent_id <> '';
CREATE INDEX agents_worktree ON agents (worktree_path) WHERE worktree_path <> '';

-- What sessions did, as short summaries. There is deliberately no column for
-- a payload: never store prompts, file contents or tool output here.
-- The length limits are a backstop for that rule and match
-- the Max* constants in events.go.
CREATE TABLE events (
    id         INTEGER PRIMARY KEY,
    session_id TEXT    NOT NULL CHECK (session_id <> ''),
    agent_id   TEXT    NOT NULL DEFAULT '',
    event      TEXT    NOT NULL CHECK (length(event) BETWEEN 1 AND 64),
    tool_name  TEXT    NOT NULL DEFAULT '' CHECK (length(tool_name) <= 128),
    summary    TEXT    NOT NULL DEFAULT '' CHECK (length(summary) <= 300),
    at         INTEGER NOT NULL
);
CREATE INDEX events_session_at ON events (session_id, at);
CREATE INDEX events_at ON events (at);
