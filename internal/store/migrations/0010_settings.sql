-- 0010_settings: the dashboard's Settings page's settings. The
-- conventions are 0001's.
--
-- Settings live here, not in config.toml, so the page can change them.
-- A row is one setting the user changed: key names it and value holds it
-- in its stored form ("on" or "off" for a switch, a number of minutes or
-- days for a timing). A missing row means the setting's default, so a
-- database with no rows behaves as the spec's defaults, and a value this
-- build can't read (a key from a newer build, say) is ignored. The keys
-- and their values are checked in Go (store/settings.go).
CREATE TABLE settings (
    key        TEXT    PRIMARY KEY,
    value      TEXT    NOT NULL,
    updated_at INTEGER NOT NULL
);

-- Who merges a plan's PRs, when the user overrode the plan's Merges line
-- on the Settings page: merges is you or lead. Without a row the plan's
-- line decides (internal/plans.Merges). The plan file is never changed.
-- The row goes with the plan's registration: unregistering the plan
-- deletes it, so registering it again starts from its line.
CREATE TABLE plan_merges (
    plan_id    INTEGER PRIMARY KEY REFERENCES plans (id) ON DELETE CASCADE,
    merges     TEXT    NOT NULL, -- you | lead
    updated_at INTEGER NOT NULL
);
