-- 0011_provisional: provisional packages. The conventions are 0001's.
--
-- Work can name a package its plan's tables don't have yet: a spawn
-- brief's "Package: <slug> · <ID>" line, a same-repo branch named
-- "<slug>/<id>-…", an open PR's "Plan: <slug> · <ID>" line. aimd never
-- edits the plan file. It shows the work on the board as a provisional
-- package instead, until the plan file gains the row.
--
-- A provisional package is a row of packages (so links, tombstones, Mark
-- done and the lifecycle history attach to it as to any package) plus a
-- row here. When a re-parse of the plan file finds its ID, the file's row
-- takes the packages row over (store.SyncPackages) and the row here is
-- deleted: the links stay where they are, on what is now the real
-- package. When nothing of its work is left for a while
-- (internal/state's ProvisionalQuiet) the packages row is deleted, and
-- everything that hangs off it with it.
CREATE TABLE provisional_packages (
    plan_id    INTEGER NOT NULL,
    pkg_id     TEXT    NOT NULL COLLATE NOCASE,
    first_seen INTEGER NOT NULL,
    -- The last state pass that found work on it: a live agent, a worktree,
    -- an open or merged PR. It expires from this.
    active_at  INTEGER NOT NULL,
    -- Which kind of source gave packages.title: spawn | pr | branch. A
    -- better one replaces the title (spawn, then pr, then branch).
    title_from TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (plan_id, pkg_id),
    FOREIGN KEY (plan_id, pkg_id) REFERENCES packages (plan_id, pkg_id)
        ON DELETE CASCADE ON UPDATE CASCADE
);

-- What named a provisional package: one row per source. kind is spawn (ref
-- the agent, "session_id/agent_id", or a remote agent's session URL),
-- branch (ref "owner/repo:branch") or pr (ref "owner/repo#123"). title is
-- the title that source gave: the spawn's description, the PR's title, the
-- branch's words.
CREATE TABLE provisional_sources (
    plan_id    INTEGER NOT NULL,
    pkg_id     TEXT    NOT NULL COLLATE NOCASE,
    kind       TEXT    NOT NULL,
    ref        TEXT    NOT NULL,
    title      TEXT    NOT NULL DEFAULT '',
    first_seen INTEGER NOT NULL,
    last_seen  INTEGER NOT NULL,
    PRIMARY KEY (plan_id, pkg_id, kind, ref),
    FOREIGN KEY (plan_id, pkg_id) REFERENCES provisional_packages (plan_id, pkg_id)
        ON DELETE CASCADE ON UPDATE CASCADE
);
