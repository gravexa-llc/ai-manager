-- 0008_inbox: the Needs you inbox (internal/inbox). The
-- conventions are 0001's.
--
-- An item is one waiting episode: a monitored session that started waiting
-- on you (a permission prompt, a question, a plan approval, a background
-- job's reply), or a PR that started needing you. ref names what waits: a
-- session row ("session:<sessions.id>", which outlives /clear and a
-- background job's respawn) or a PR ("owner/repo#123"). While the episode
-- lasts ended_at is NULL, and at most one item per ref is in that state
-- (inbox_items_waiting): a new episode of the same session or PR is a new
-- item. The ids are the items' stable ids (/app/inbox?item=<id>).
--
-- state is open, progress (you clicked Go), snoozed or done. due_at is
-- when an item moves by itself: an In progress item back to Open if it
-- still waits, a timed snooze back to Open. A snooze until it changes has
-- no due_at. aimd applies due times on every sync pass, so they hold
-- across restarts.
--
-- The text columns (title … why) are what the item showed at the last pass
-- while it waited, so a Done item still reads after its session or PR is
-- gone. They are labels and short summaries, never prompts or tool output.
CREATE TABLE inbox_items (
    id           INTEGER PRIMARY KEY,
    source       TEXT    NOT NULL,            -- session | pr
    ref          TEXT    NOT NULL CHECK (ref <> ''),
    kind         TEXT    NOT NULL,            -- permission | question | plan | reply | prompt | review | merge | pr
    state        TEXT    NOT NULL,            -- open | progress | snoozed | done
    since        INTEGER NOT NULL,            -- the episode began
    state_at     INTEGER NOT NULL,            -- state last set
    due_at       INTEGER,
    snooze       TEXT    NOT NULL DEFAULT '', -- 15m | 1h | tomorrow | change, while snoozed
    ended_at     INTEGER,                     -- the episode ended
    end_reason   TEXT    NOT NULL DEFAULT '',
    seen_at      INTEGER,                     -- first opened; NULL: a new item
    dismissed_at INTEGER,                     -- the old dashboard's Dismiss this item followed
    title        TEXT    NOT NULL DEFAULT '',
    subtitle     TEXT    NOT NULL DEFAULT '',
    detail       TEXT    NOT NULL DEFAULT '', -- what it waits on, as shown
    sig          TEXT    NOT NULL DEFAULT '', -- what "it changed" compares
    plan_slug    TEXT    NOT NULL DEFAULT '',
    pkg          TEXT    NOT NULL DEFAULT '',
    session_key  TEXT    NOT NULL DEFAULT '',
    job_id       TEXT    NOT NULL DEFAULT '',
    repo         TEXT    NOT NULL DEFAULT '',
    branch       TEXT    NOT NULL DEFAULT '',
    folder       TEXT    NOT NULL DEFAULT '',
    pr           TEXT    NOT NULL DEFAULT '', -- owner/repo#123
    pr_url       TEXT    NOT NULL DEFAULT '',
    why          TEXT    NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL
);
CREATE UNIQUE INDEX inbox_items_waiting ON inbox_items (ref) WHERE ended_at IS NULL;
CREATE INDEX inbox_items_state ON inbox_items (state, since);

-- An item's history: each move between states, by you or by aimd, and
-- what else happened to its episode (it changed, it stopped waiting). A
-- move keeps the state it left (from_state, from_due, from_snooze), which
-- is what Undo restores.
CREATE TABLE inbox_moves (
    id          INTEGER PRIMARY KEY,
    item_id     INTEGER NOT NULL REFERENCES inbox_items (id) ON DELETE CASCADE,
    at          INTEGER NOT NULL,
    by          TEXT    NOT NULL,             -- you | aimd
    text        TEXT    NOT NULL,
    from_state  TEXT    NOT NULL DEFAULT '',
    to_state    TEXT    NOT NULL DEFAULT '',
    from_due    INTEGER,
    from_snooze TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX inbox_moves_item ON inbox_moves (item_id, id);
