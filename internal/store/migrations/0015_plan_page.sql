-- 0015_plan_page: what the plan page shows of a package, and what you
-- told it isn't part of a plan. The conventions are 0001's.

-- The package table's What cell (or Description, Summary, Scope), as
-- written: markdown. The parser always read it (plans.Package.What); it
-- is stored from here on, and the plan page shows it under the package's
-- title. Rows stored before this migration get it when aimd next re-reads
-- their plan, which it does as it starts.
ALTER TABLE packages ADD COLUMN what TEXT NOT NULL DEFAULT '';

-- PRs and branches you said aren't part of a plan ("Not part of this
-- plan" on its page): the plan's Unmatched lane leaves them out, for good,
-- until you restore one. Hiding links and unlinks nothing; another plan's
-- lane still lists the target. kind and ref are a link target's
-- (store.Link): pr with "owner/repo#123", branch with "owner/repo:branch".
CREATE TABLE unmatched_hidden (
    plan_id   INTEGER NOT NULL REFERENCES plans (id) ON DELETE CASCADE,
    kind      TEXT    NOT NULL,
    ref       TEXT    NOT NULL,
    hidden_at INTEGER NOT NULL,
    PRIMARY KEY (plan_id, kind, ref)
);
