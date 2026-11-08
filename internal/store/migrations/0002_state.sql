-- 0002_state: what the matcher and package state (internal/state)
-- keep between runs. The conventions are 0001's.

-- The Needs cell's `you:` clause (plans.Package.NeedsYou): a step you take
-- for a package someone else carries out. The board shows it as "you: …".
ALTER TABLE packages ADD COLUMN needs_you TEXT NOT NULL DEFAULT '';

-- The package's derived lifecycle (in_review | done | working | dropped |
-- ready | blocked), the one before it, and when it last changed, so an
-- alert can say what became ready since a given time. '' until the first
-- derivation. Only internal/state writes these (Store.SetLifecycles).
ALTER TABLE packages ADD COLUMN lifecycle      TEXT NOT NULL DEFAULT '';
ALTER TABLE packages ADD COLUMN lifecycle_prev TEXT NOT NULL DEFAULT '';
ALTER TABLE packages ADD COLUMN lifecycle_at   INTEGER;

-- 1 for a link the matcher made: it rewrites those on every run
-- (Store.ReplaceInferredLinks). 0 for a link the mod or the user made
-- (spawn, manual, …), which the matcher never changes.
ALTER TABLE links ADD COLUMN inferred INTEGER NOT NULL DEFAULT 0;

-- When aimd first stored the PR. A low-confidence branch-name match only
-- counts for a branch first seen after its plan was registered, and a
-- PR's head branch is first seen no later than this.
ALTER TABLE prs ADD COLUMN first_seen_at INTEGER;

-- Links you removed by hand. The matcher never re-creates a link with a
-- tombstone, and a manual link to the same target clears it. The columns
-- are a link's identity (links_target).
CREATE TABLE link_tombstones (
    plan_id    INTEGER NOT NULL,
    pkg_id     TEXT    NOT NULL COLLATE NOCASE,
    kind       TEXT    NOT NULL,
    session_id TEXT    NOT NULL DEFAULT '',
    job_id     TEXT    NOT NULL DEFAULT '',
    agent_id   TEXT    NOT NULL DEFAULT '',
    ref        TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    FOREIGN KEY (plan_id, pkg_id) REFERENCES packages (plan_id, pkg_id)
        ON DELETE CASCADE ON UPDATE CASCADE
);
CREATE UNIQUE INDEX link_tombstones_target
    ON link_tombstones (plan_id, pkg_id, kind, session_id, job_id, agent_id, ref);
