-- 0004_history: what a plan's timeline (GET /plans/{slug}/timeline)
-- reads besides links, PRs and events. The conventions are 0001's.

-- Every change of a package's derived lifecycle, as internal/state saved
-- it (Store.SetLifecycles writes a row in the same transaction). prev is
-- '' for the first derivation. Rows go with their package.
CREATE TABLE lifecycle_history (
    id        INTEGER PRIMARY KEY,
    plan_id   INTEGER NOT NULL,
    pkg_id    TEXT    NOT NULL COLLATE NOCASE,
    lifecycle TEXT    NOT NULL,
    prev      TEXT    NOT NULL DEFAULT '',
    at        INTEGER NOT NULL,
    FOREIGN KEY (plan_id, pkg_id) REFERENCES packages (plan_id, pkg_id)
        ON DELETE CASCADE ON UPDATE CASCADE
);
CREATE INDEX lifecycle_history_plan_at ON lifecycle_history (plan_id, at);

-- A plan's alerts over time (Store.SyncAlerts, after each state pass): one
-- row from the pass that first raised an alert until the pass that no
-- longer finds it (cleared_at). key says what the alert is about within
-- its kind: a package ID for stalled, a lane for lane_collision. At most
-- one open row per alert.
CREATE TABLE plan_alerts (
    id         INTEGER PRIMARY KEY,
    plan_id    INTEGER NOT NULL REFERENCES plans (id) ON DELETE CASCADE,
    kind       TEXT    NOT NULL,
    key        TEXT    NOT NULL COLLATE NOCASE,
    packages   TEXT    NOT NULL DEFAULT '[]'
               CHECK (json_valid(packages) AND json_type(packages) = 'array'),
    message    TEXT    NOT NULL DEFAULT '',
    raised_at  INTEGER NOT NULL,
    cleared_at INTEGER
);
CREATE UNIQUE INDEX plan_alerts_open ON plan_alerts (plan_id, kind, key) WHERE cleared_at IS NULL;
CREATE INDEX plan_alerts_plan ON plan_alerts (plan_id, raised_at);
