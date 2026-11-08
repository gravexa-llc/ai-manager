-- 0005_github: two things GitHub answers that aimd didn't keep. The
-- conventions are 0001's.

-- GitHub's createdAt: when the PR was opened. internal/state counts it as
-- the PR's head branch's first sighting (the low-confidence branch rule),
-- and the timeline shows it as "opened". Both fall back to first_seen_at
-- while it is NULL, as it is for rows stored before this migration until a
-- poll or backfill rewrites them.
ALTER TABLE prs ADD COLUMN created_at INTEGER;

-- One row per GitHub repo aimd has read PRs of: its default branch, as
-- GitHub last answered. name is the nameWithOwner GitHub answered with, the
-- key prs.repo uses. internal/state counts only this branch and the
-- branches PRs target as a repo's base branches; for a repo with no row it
-- falls back to main, master, trunk and develop.
CREATE TABLE repos (
    name           TEXT    NOT NULL COLLATE NOCASE PRIMARY KEY,
    default_branch TEXT    NOT NULL DEFAULT '',
    updated_at     INTEGER NOT NULL
);
