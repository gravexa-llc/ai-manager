-- 0009_removal_log: what the dashboard's Worktrees & cleanup page
-- removed. The conventions are 0001's.
--
-- One row per worktree the page was asked to remove, whatever came of it,
-- and one per record `git worktree prune` cleared (or per repo where it
-- didn't run). action is removed, skipped, failed or pruned; detail says
-- why a row was skipped or failed. The page shows the last 30 days, and
-- rows older than that are deleted (web.Cleanup).
CREATE TABLE removal_log (
    id             INTEGER PRIMARY KEY,
    at             INTEGER NOT NULL,
    repo           TEXT    NOT NULL DEFAULT '' COLLATE NOCASE, -- the main checkout
    path           TEXT    NOT NULL DEFAULT '' COLLATE NOCASE,
    branch         TEXT    NOT NULL DEFAULT '',
    head_sha       TEXT    NOT NULL DEFAULT '',
    action         TEXT    NOT NULL,
    branch_deleted INTEGER NOT NULL DEFAULT 0,
    detail         TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX removal_log_at ON removal_log (at);
