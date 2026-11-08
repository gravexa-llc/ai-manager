-- 0003_plan_api: what the plan API (docs/api.md) keeps. The conventions
-- are 0001's.

-- Markdown files you said aren't plans ("Not a plan" on the band, or POST
-- /api/plans/dismiss), so the band doesn't offer them again. path is the
-- file's resolved path (symlinks followed). Registering the file clears it.
CREATE TABLE plan_dismissals (
    path         TEXT    NOT NULL COLLATE NOCASE PRIMARY KEY,
    dismissed_at INTEGER NOT NULL
);
