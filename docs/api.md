# aimd HTTP API

aimd's HTTP API is what its clients use: the plugin's mod, the dashboard, the `plan_status` tool, `aim`, and your own scripts with `curl`. This is its reference.

The Go types of the bodies are in `internal/web/api`. The plan logic is in `internal/planapi`, and the handlers are in `internal/web`.

The examples use a made-up plan, `search`, whose packages live in `example-org/recipes-api` and `example-org/recipes-web`. [Writing and registering plans](guides/plans.md) has the whole plan.

## The dashboard

The dashboard is the web app at `/app/`, and `GET /` answers `302` to it. Its pages are HTML only:

| Page | Path | Selections in the URL |
|---|---|---|
| Overview | `/app/` | `?feed=all` |
| Needs you | `/app/inbox` | `?tab=`, `?item=<id>` |
| A plan | `/app/plans/{slug}` | `?view=graph\|kanban\|timeline`, `?pkg=<ID>` |
| Sessions | `/app/sessions` | `?s=<key>` |
| Worktrees & cleanup | `/app/worktrees` | `?pick=` |
| Pull requests | `/app/prs` | `?pr=<owner/repo#n>` |
| Settings | `/app/settings` | |

The list pages also take `?q=`, their search.

The pages' forms post under `/app/<page>/…` with the page's CSRF token, and call the same logic as the endpoints below, so a form and the API always agree. [The dashboard guide](guides/dashboard.md) describes each page.

Older page URLs answer `308` to their app pages, and drop their other parameters:

| Old URL | Goes to |
|---|---|
| `/sessions` | `/app/sessions` |
| `/worktrees` | `/app/worktrees` |
| `/plans/{slug}` | `/app/plans/{slug}` |
| `/plans/{slug}/graph` | `/app/plans/{slug}?view=graph` |
| `/plans/{slug}/timeline` | `/app/plans/{slug}?view=timeline`, keeping `?pkg=` |

## Conventions

- **Where.** aimd listens on `http://127.0.0.1:<port>`, 7777 by default. The `Host` header must be `127.0.0.1:<port>` or `localhost:<port>`. No CORS headers are ever sent.
- **Reads** (`GET`) need no auth.
- **Writes** (`POST`, `DELETE`) need one of two things:
  - **aimd's token** in `X-AIM-Token`. The plugin, `aim` and `curl` use this. The token is the content of `$AIM_HOME/token`, by default `~/.ai-manager/token`.
  - **For a script on one of aimd's own dashboard pages:** the page's CSRF token in `X-CSRF-Token`. The request must also be same-origin: its `Origin`, if sent, is aimd's own, and `Sec-Fetch-Site`, if sent, is `same-origin` or `none`. A plain form post to `/api/` needs the token: the pages' forms post under `/app/`.

  A missing or wrong token gets `401`. A bad CSRF token or another origin gets `403`.
- **Request bodies** are JSON objects of at most 64 KB. aimd ignores the `Content-Type`, so `curl -d` works. Unknown fields are ignored.
- **Responses** are JSON:
  - field names are snake_case;
  - times are RFC 3339 in UTC, and are left out when unknown;
  - lists are `[]`, never `null`, unless a field is marked optional.
- **Errors** are `{"error": "<message>"}`. The message says what to do, so the plugin can show it as is. Statuses:
  - `400`: the request is malformed.
  - `401` / `403`: auth, Host or path rules.
  - `404`: no such plan, package, link or file.
  - `409`: the slug is taken.
  - `413`: the file or body is too large.
  - `422`: the file isn't a plan, or its repos can't be told.
  - `500`: aimd itself failed (see `aimd.log`).
- **The plugin's rule** applies to every call it makes: a 1 s timeout, and on any failure it carries on as if aimd weren't there.

### Values

| Field | Values |
|---|---|
| `lifecycle` | `blocked`, `ready`, `working`, `in_review`, `done`, `dropped` |
| link `kind` | `agent`, `branch`, `pr`, `worktree`, `cloud_session` |
| link `method` | `spawn`, `plan_line`, `title`, `branch`, `manual` (most reliable first) |
| link `confidence` | `exact`, `high`, `low` |
| badge `kind` | `needs_you`, `ci_failing`, `conflict`, `waiting_on_you`, `changes_requested`, `review_requested`, `draft`, `stalled`, `quiet`, `collision`, `lane_conflict`, `low_confidence`, `you` |
| alert `kind` | `unlocked`, `stalled`, `lane_collision`, `off_plan` |

[How a package's state is worked out](guides/plans.md#how-a-packages-state-is-worked-out) explains the lifecycles and badges.

A target's `ref` depends on its `kind`:

| `kind` | `ref` |
|---|---|
| `pr` | `owner/repo#123`. A request may also give the PR's URL. |
| `branch` | `owner/repo:branch`. For a repo with no GitHub remote, the main checkout's path stands in for `owner/repo`. |
| `worktree` | The worktree's absolute path. |
| `agent` | `session_id/agent_id`, or `session_id/` for a session's main loop, or `job:<id>` for a background session. |
| `cloud_session` | The session URL. |

### File rules

Registration, detection and dismissal name a file on this Mac, so aimd checks the path before reading anything:

1. It is absolute, and its name ends in `.md`.
2. After following symlinks, the target also ends in `.md`, exists, and is a regular file of at most 1 MB.
3. The resolved path is inside a configured root (`roots` in `config.toml`), or inside a repo the worktree scanner knows: a main checkout or any of its worktrees, wherever they are.

Anything else is refused with a message that says which rule failed:

- `400` for a malformed path;
- `404` for a missing file;
- `403` for a path outside the roots and known repos;
- `413` for a file that's too large.

Responses give the resolved `path`.

### Change events: `GET /api/stream`

A server-sent event stream. Its first event is `event: ready`, with `{"seq": …, "assets": "<version>"}`. A dashboard page from another aimd build reloads when the asset versions differ.

After that, each change is `event: change`, with `{"topic": "<topic>", "seq": …, "at": …}`, when that part of aimd's data changed and a client should fetch it again:

| Topic | Changes when |
|---|---|
| `sessions` | a session or agent appeared, changed state or went |
| `worktrees` | a worktree scan found a difference |
| `prs` | a GitHub poll stored a changed PR |
| `status` | a poller's health changed |
| `plans` | a state pass changed any board |
| `inbox` | the Needs you inbox changed, or a setting did |

A comment line keeps the connection alive. A state pass runs:

- after each poll that changed sessions, worktrees or PRs, at most once every 5 s;
- right after every write below, so the response already shows its effect;
- when a registered plan file changes on disk. A re-parse that finds no packages at all is ignored, because the file was probably caught half-written.

A plan's page refreshes on `plans`, `sessions`, `prs` and `inbox`. Between passes, a card shows its PRs and agents as aimd has them now, so a `sessions` or `prs` change shows on it at once. Its column and badges follow with the next pass.

---

## Plans

### `POST /api/plans`: register a plan

Auth: write. This is the band's **Register** button and `/plan-register`.

```json
{"path": "/Users/me/Projects/recipes-api/docs/search-plan.md",
 "repos": ["/Users/me/Projects/recipes-api", "example-org/recipes-web"],
 "session_id": "8a1f…"}
```

- `path` (required) is the plan file. The file rules apply.
- `repos` (optional) are the repos the plan's packages live in. Give each as `owner/name` (a GitHub URL also works), or as the absolute path of a local repo. A local repo is registered under its GitHub `nameWithOwner`, or under its main checkout's path if it has no GitHub remote.
- `session_id` (optional) records which session registered the plan, and makes that session the plan's **lead**: monitored, with reason `plan` (see [Monitored sessions](#monitored-sessions)), until the plan is unregistered. Each session that registers the plan becomes one of its leads. Registering again from the same session changes nothing. The band's **Register** button and `/plan-register` send it.

aimd then does the following:

- **It reads and parses the file.** A file without a package table gets `422`.
- **It takes the slug** from a `Plan slug: x` line, or else from the file name (`search-plan.md` gives `search`). Another file's plan with the same slug gets `409`.
- **It picks the repos,** first match wins (`repos_from` in the response says which):
  1. the request's `repos` (`"request"`);
  2. the plan's line `Repos: owner/name, owner/name` (`"plan"`);
  3. the repo the file is in (`"file"`). If the file isn't in a repo either, the answer is `422`.

  Names are GitHub's current `nameWithOwner` once GitHub has answered under a new name after a transfer.
- **It stores the plan and its packages,** clears any dismissal of the file, starts watching the file, and runs a state pass.
- **It starts a backfill:** one GitHub search per repo for all its PRs (open, merged and closed), in the background. This brings in PRs whose branches aimd never polled, such as ones merged before aimd ran or whose worktree is gone. A PR stored for the first time this way counts as first seen at its GitHub creation time, so old branches never make low-confidence matches. When the backfill finishes, a `plans` change event follows.

Registering a file that is already registered updates that plan, keeps its links and Mark done, and answers `200` with `"created": false`. A new plan gets `201`.

```json
{
  "created": true,
  "slug": "search",
  "title": "Recipe search: dispatch plan",
  "path": "/Users/me/Projects/recipes-api/docs/search-plan.md",
  "repos": ["example-org/recipes-api", "example-org/recipes-web"],
  "repos_from": "request",
  "packages": 6,
  "warnings": 0,
  "backfill": "started",
  "summary": { "…": "as GET /api/plans/{slug}/summary" }
}
```

### `DELETE /api/plans/{slug}`: unregister a plan

Auth: write. The slug matches in any case.

The dashboard's **Unregister plan…** button, on a plan's page with no package selected, calls the same logic:

1. it confirms in a dialog;
2. it posts `POST /app/plans/{slug}/unregister` with the page's CSRF token;
3. it answers with a toast that gives the `/plan-register <path>` command to register the file again, and sends the browser to the Overview.

It has no Undo.

**What aimd forgets:** the plan's board, its packages, its stored links (spawn, manual and the matcher's), its unlinks and its Mark done.

**What it keeps:** the plan file, git and GitHub are never touched, and the PRs it has read stay in its database.

It stops watching the file and runs a state pass, so the board is gone from every answer at once, and a `plans` change event follows. Registering the file again starts the plan afresh, without the manual links, unlinks or Mark done.

```json
{"slug": "search", "title": "Recipe search: dispatch plan",
 "path": "/Users/me/Projects/recipes-api/docs/search-plan.md",
 "packages": 6, "links": 9}
```

`packages` and `links` count what was removed. An unknown slug gets `404`, so a second unregister does too.

### `GET /api/plans`: every board

Auth: none. The response is `{"plans": [<board>, …]}`, in the order the plans were registered. Each board is the same as `GET /api/plans/{slug}`.

### `GET /api/plans/{slug}`: one board

Auth: none. The slug matches in any case. This is the full derived state from the last state pass. The one exception is `band`, whose need-you count is read with the board (see the summary). An unknown slug gets `404`.

```json
{
  "slug": "search",
  "title": "Recipe search: dispatch plan",
  "path": "/Users/me/Projects/recipes-api/docs/search-plan.md",
  "repos": ["example-org/recipes-api", "example-org/recipes-web"],
  "registered_in_session": "8a1f…",
  "merges": "you",
  "created_at": "2026-11-05T21:10:04.113Z",
  "updated_at": "2026-11-05T21:10:04.113Z",
  "band": "search · 1/6 done · 1 ready · 1 needs you",
  "total": 6,
  "counts": {"blocked": 2, "ready": 2, "working": 0, "in_review": 1, "done": 1, "dropped": 0},
  "ready_to_dispatch": ["UI1"],
  "packages": [
    {
      "id": "API1",
      "title": "Search endpoint",
      "what": "`GET /search` over the index, **paged**.",
      "repo": "recipes-api",
      "size": "S",
      "lane": "Schema",
      "needs": ["IDX1"],
      "lifecycle": "in_review",
      "prev_lifecycle": "working",
      "since": "2026-11-05T21:10:04.113Z",
      "blocked_by": [],
      "badges": [
        {"kind": "waiting_on_you", "text": "waiting on you on #42",
         "ref": {"kind": "pr", "ref": "example-org/recipes-api#42"}}
      ],
      "links": [
        {"kind": "pr", "ref": "example-org/recipes-api#42", "method": "plan_line", "confidence": "exact",
         "id": 12, "inferred": true,
         "pr": {"repo": "example-org/recipes-api", "number": 42, "title": "Search endpoint (API1)",
                "url": "https://github.com/example-org/recipes-api/pull/42", "state": "OPEN",
                "head_branch": "search/api1-search-endpoint", "checks_state": "SUCCESS",
                "mergeable": "MERGEABLE", "merge_state_status": "CLEAN"}},
        {"kind": "branch", "ref": "example-org/recipes-api:search/api1-search-endpoint",
         "method": "plan_line", "confidence": "exact", "inferred": true,
         "via": {"kind": "pr", "ref": "example-org/recipes-api#42"}},
        {"kind": "worktree", "ref": "/Users/me/Projects/recipes-api/.claude/worktrees/agent-a73d…",
         "method": "plan_line", "confidence": "exact", "inferred": true,
         "via": {"kind": "branch", "ref": "example-org/recipes-api:search/api1-search-endpoint"},
         "worktree": {"path": "/Users/me/Projects/recipes-api/.claude/worktrees/agent-a73d…",
                      "main_repo": "/Users/me/Projects/recipes-api", "branch": "search/api1-search-endpoint",
                      "locked": true, "last_commit_at": "2026-11-04T18:02:11Z"}}
      ],
      "suggested_branch": "search/api1-search-endpoint",
      "plan_line": "Plan: search · API1"
    }
  ],
  "lanes": [
    {"name": "Schema", "packages": ["IDX1", "API1"],
     "rule": "**single-writer**: one agent at a time in `db/migrations/`.", "single_writer": true},
    {"name": "Web", "packages": ["UI1", "UI2"], "rule": "Parallel with the API."}
  ],
  "unmatched": [
    {"kind": "pr", "ref": "example-org/recipes-api#47", "repo": "example-org/recipes-api", "branch": "try/tokenizer",
     "number": 47, "title": "Try a new tokenizer", "state": "OPEN", "since": "2026-11-05T22:41:09Z", "new": true},
    {"kind": "pr", "ref": "example-org/recipes-api#45", "repo": "example-org/recipes-api", "branch": "fix/readme",
     "number": 45, "title": "Fix a typo in the README", "state": "OPEN", "since": "2026-08-12T09:03:55Z"}
  ],
  "hidden_unmatched": [
    {"kind": "branch", "ref": "example-org/recipes-api:old/experiment", "repo": "example-org/recipes-api",
     "branch": "old/experiment", "since": "2026-07-01T10:00:00Z", "hidden_at": "2026-11-06T08:15:30.200Z"}
  ],
  "warnings": [],
  "alerts": [],
  "derived_at": "2026-11-05T21:10:05.002Z"
}
```

Package fields:

| Field | Meaning |
|---|---|
| `what` | The package table's What cell (or its Description, Summary or Scope column), as written: markdown, left out when empty. The plan page shows it under the title. |
| `repo`, `size`, `owner`, `lane` | The plan's cells as written, each left out when empty. |
| `needs`, `needs_note`, `needs_you` | `needs` are the IDs in the Needs cell, `needs_note` the rest of the cell, and `needs_you` its `you:` step. |
| `yours` | A package owned by `you`. |
| `done_in_plan`, `done_manual` | ✅ in the plan, and Mark done. |
| `prev_lifecycle`, `since` | The lifecycle before this one, and when it changed. |
| `blocked_by` | The needed packages that aren't done. It is also set on a done or in-review package, so it isn't a lifecycle. |
| `cycle` | The dependency cycle the package is in, as a path like `["UI2","REL1","UI2"]`. A package in a cycle is blocked only by the members it needs that aren't done, so once one member is done, the rest proceed. |
| `badges` | Each has `ref` when it is about one target, and `since` when known. |
| `suggested_branch`, `plan_line` | The convention block's data: the branch is `<slug>/<id>-<short-name>`, and the line goes in the PR body. |
| `provisional` | `true` for a package the plan file's tables don't have yet ([Provisional packages](#provisional-packages)). Left out otherwise, along with the three fields below. A provisional package comes after the file's packages, has no lane or needs, and is never in `ready_to_dispatch`. |
| `suggested_row` | The row to paste into the plan file, in the format of its last package table: `\| **SORT1 Sort options** \|  \|  \|`. The package cell is filled in, with a `✅` after it once the work is done; the other cells are left blank. |
| `first_seen`, `sources` | When aimd first saw its work, and what named it. Each source has its `kind` (`spawn`, `branch` or `pr`), `ref` (the agent `session_id/agent_id` or a remote session's URL, `owner/repo:branch`, or `owner/repo#123`), the `title` it gave, `first_seen` and `last_seen`. |

Link fields:

| Field | Meaning |
|---|---|
| `id` | The stored link's id, for `DELETE /api/links/{id}`. It is `0` (left out) for a link reached through another. |
| `inferred` | The matcher made the link, or the link it was reached through. |
| `via` | Set when the link was reached through another target linked to the package: a PR's branch, the worktree on that branch, or the agent in that worktree. |
| `pr`, `worktree`, `agent` | What the link points at, when aimd has it. `agent` has `session_id`, `job_id`, `agent_id`, `kind`, `name`, `state`, `live`, `needs_you`, `needs_you_since`, `reason`, `last_action` (as the plugin reported it, e.g. `Bash: make check`), `last_event_at`, `worktree_path`, `started_at` (when the session started, or the subagent's first event) and `tokens`. `tokens` is the agent's token count as Claude Code's agent list shows it ("↓ 284.1k tokens"): its latest model response's input, cache read, cache write and output tokens added up, about the size of its context now; a session's is its main loop's. It isn't a running total: each response replaces it, so it can go down after a compaction. It is left out before the first response, and for a session whose plugin is older than the main loop's count. |

Board fields:

| Field | Meaning |
|---|---|
| `merges` | Who merges the plan's PRs, from its `Merges:` line: `you` (the default, also with no line) or `lead` (the plan's lead session merges its agents' PRs). A line with any other value reads as `you` and is an `unknown-merges` warning. It decides whether a ready PR of the plan needs you; see [`GET /api/prs`](#get-apiprs-pull-requests-and-what-they-wait-on). |
| `total`, `counts`, `provisional` | `total` counts the packages on the board, and `counts` counts them by lifecycle, provisional packages included. `provisional` counts those alone. |
| `lanes` | The plan's lanes table, in file order: `name`, `packages` (the IDs the row lists), `rule` (the row's other cells) and `single_writer` (the row says at most one agent at a time works in the lane). A package's `lane` names one. The board draws them as swimlanes. |
| `unmatched` | Same-repo PRs in the plan's repos that no package of any plan links: open ones, and ones closed after the plan was registered. Then branches with a worktree and no PR that nothing links. Each has the matcher's `note` when it has one (`ambiguous: …`, `you unlinked it from …`), `since`, when it appeared (a PR's GitHub `createdAt`, or when aimd first stored it while it hasn't read it from GitHub; a branch's first sighting in a worktree), and `new` when that is after the plan was registered (or isn't known). The plan page lists the new ones first and the others under "Older". |
| `hidden_unmatched` | What would be in `unmatched` but that you said isn't part of this plan (**Not part of this plan** on its page), with `hidden_at`, most recently hidden first. It is this plan's alone: other plans' `unmatched` still list it. |
| `warnings` | Parser warnings (`unknown-merges` among them) and dependency cycles: `kind`, `line`, `id`, `message`. |
| `alerts` | `unlocked` lists every package whose last change was blocked → ready. Also `stalled`, `lane_collision` (with `lane`), and `off_plan` (with the agent in `ref`). |

### `GET /api/plans/{slug}/summary[?since=<RFC 3339>]`: the band and `plan_status`

Auth: none. A `since` that isn't RFC 3339 gets `400`, and an unknown slug gets `404`.

```json
{
  "slug": "search",
  "title": "Recipe search: dispatch plan",
  "merges": "you",
  "band": "search · 1/6 done · 1 ready · 1 needs you",
  "total": 6,
  "done": 1,
  "counts": {"blocked": 2, "ready": 2, "working": 1, "in_review": 0, "done": 1, "dropped": 0},
  "ready": [
    {"id": "UI1", "title": "Search box", "lifecycle": "ready"}
  ],
  "needs_you": 1,
  "needs_you_items": [
    {"id": 7, "url": "/app/inbox?item=7", "kind": "permission", "kind_label": "Permission", "title": "Agent · API1",
     "pkg": "API1", "reason": "permission prompt: Bash: make check", "since": "2026-11-05T21:02:11Z"}
  ],
  "needs_you_packages": [
    {"id": "API1", "title": "Search endpoint", "lifecycle": "working", "reasons": ["permission prompt: Bash: make check"]}
  ],
  "to_merge": 0,
  "to_merge_prs": [],
  "since": "2026-11-05T09:00:00Z",
  "unlocked": [{"id": "UI1", "title": "Search box", "lifecycle": "ready", "blocked_by": []}],
  "packages": [
    {"id": "IDX1", "title": "Search index", "lifecycle": "done", "prs": ["example-org/recipes-api#40"]},
    {"id": "UI2", "title": "Results page", "lifecycle": "blocked", "blocked_by": ["API1", "P1"]}
  ],
  "provisional": [],
  "warnings": 0,
  "alerts": [],
  "derived_at": "2026-11-05T21:12:40.551Z"
}
```

| Field | Meaning |
|---|---|
| `merges` | Who merges the plan's PRs, `you` or `lead`, as on the board. |
| `band` | `<slug> · <done>/<total> done · <ready> ready · <n> need you`, with `1 needs you` for one. `<n>` is `needs_you`. |
| `ready` | The packages ready to dispatch: ready, and not yours. `ready` in the band counts them. |
| `needs_you` | What waits on you on this plan: its **Open** items in the [Needs you inbox](#the-needs-you-inbox). See below. In progress, Snoozed and Done items don't count. This is the number the dashboard's menu shows beside the plan, and the number the Overview and the band show. The read runs an inbox sync pass first, as `GET /api/inbox` does. |
| `needs_you_items` | Those items, oldest first: `id` and `url` (its page), `kind` and `kind_label` (as in the inbox), `title`, `pkg` and `pr` when it has them, `reason` and `since`. `reason` says what it waits on: `permission prompt: Bash: make check`, `waiting for your reply`, `review requested on #12`, `ready to merge on #7`. `needs_you` is the list's length. It never carries what a session waits on in detail (`waiting_on`), which needs the token. |
| `needs_you_packages` | The plan's packages that have one or more of those items, in plan order, each with its items' `reasons`. An item on no package of the plan, such as the lead's own prompt, is in `needs_you_items` only, so this list can be shorter than `needs_you`. |
| `to_merge`, `to_merge_prs` | The plan's open PRs ready for its **lead** to merge. That means all of these: the plan says `Merges: lead`; the PR is ready to merge (no draft, red CI, conflict or review blocking it, and `CLEAN`); and no agent still works on its package or branch. So the PR waits on the lead (`waiting_on: "lead"` in [`GET /api/prs`](#get-apiprs-pull-requests-and-what-they-wait-on)). `to_merge` counts them and `to_merge_prs` lists them (`owner/repo#123`). On a plan that merges `you` it is always `0`: a ready PR there is a Needs you item. It is worked out at each state pass, so a change shows with the `plans` topic. |
| `unlocked` | The packages that went from blocked to ready after `since`. Without `since`, every package whose last change was blocked → ready. |
| `packages` | Every package in plan order, compactly: `blocked_by`, and `prs` (linked PRs, open ones first). A provisional package also has `provisional: true` and its `suggested_row`. |
| `provisional` | The plan's [provisional packages](#provisional-packages), compactly as in `packages`, each with `provisional: true` and the `suggested_row` to add to the plan file. `[]` when the file has every package work names. The `plan_status` tool hands it to the lead. |

`needs_you` counts these, while they are Open inbox items:

- the monitored sessions on the plan that wait on you: a permission prompt, a question, a plan approval, or a background job's reply;
- the PRs on it that need you: your review is requested, or it is ready to merge and the plan merges `you`;
- a provisional package whose row is still missing;
- your steps that are ready: a package that is `yours` (`Owner` you) or has a `needs_you` step, and whose `lifecycle` is `ready`. Its item's `reason` is `your step is ready`, or `your step is ready: <its you: step>`.

Red CI, conflicts and changes requested wait on an agent or on the lead, so they don't count. Neither does your step before it's ready: until then it's on the board only, as a package's `yours` and `needs_you`, and its `you` badge. The count changes with the `inbox` topic on `GET /api/stream` as well as `plans`.

### `POST /api/plans/detect`: does this file look like a plan?

Auth: write. The plugin sends every `.md` file the lead writes. The file rules apply. Any answer but `200` means "show no offer".

```json
{"path": "/Users/me/Projects/recipes-api/docs/search-plan.md"}
```

```json
{"path": "/Users/me/Projects/recipes-api/docs/search-plan.md",
 "is_plan": true, "title": "Recipe search: dispatch plan",
 "packages": 6, "lanes": 2, "slug": "search",
 "registered": false, "dismissed": false, "offer": true}
```

- `is_plan`: the file has a package table and at least 2 package IDs.
- `slug`: the registered plan's slug, or the slug the file would get.
- `dismissed_at`: set when `dismissed` is true.
- `offer`: `is_plan`, not registered and not dismissed. That is when the band shows **Register** and **Not a plan**.

### `POST /api/plans/dismiss`: "Not a plan"

Auth: write. The file rules apply. aimd remembers the dismissal per resolved file path, and registering the file later clears it.

```json
{"path": "/Users/me/Projects/recipes-api/docs/notes.md"}
```

```json
{"path": "/Users/me/Projects/recipes-api/docs/notes.md", "dismissed": true,
 "dismissed_at": "2026-11-05T21:15:00.120Z"}
```

### `POST /api/plans/{slug}/packages/{id}/done`, `DELETE …/done`: Mark done and its undo

Auth: write. `POST` sets the package's Mark done and `DELETE` clears it. Neither changes the plan file: a ✅ in the plan stays a ✅. aimd runs a state pass and answers with the package and the plan's summary. An unknown plan or package gets `404`.

```json
{"package": { "…": "as in a board", "id": "P1", "lifecycle": "done", "done_manual": true },
 "summary": { "…": "as GET /api/plans/{slug}/summary", "ready": [{"id": "UI1", "…": "…"}] }}
```

---

## Links

Every link write answers with a `LinkResult`: the links written or removed, and the packages they touch, freshly derived.

```json
{"links": [{"plan": "search", "pkg": "API1", "kind": "agent", "ref": "8a1f…/a73d…",
            "method": "spawn", "confidence": "exact", "id": 31}],
 "packages": [{ "…": "the package, as in a board" }]}
```

### `POST /api/links/spawn`: the plugin saw an agent spawn for a package

Auth: write. It answers `201`.

```json
{"session_id": "8a1f…", "agent_id": "a73d…", "plan": "search", "pkg": "API1"}
```

```json
{"session_id": "8a1f…", "cloud_url": "https://claude.ai/code/session_…", "plan": "search", "pkg": "UI1"}
```

- `session_id` is the lead's session, and `agent_id` is the id `agent.spawn` returned.
- A remote agent (`remote_launched`) has no agent id, so give `cloud_url` (its `sessionUrl`) instead. The link is then a `cloud_session`.
- `provisional` (optional): the brief's `Package:` line names a package the plan file doesn't have. aimd then makes it a [provisional package](#provisional-packages), or adds this spawn to one, before linking. Its title comes from `title`, the spawn's description, without the ID. The plugin sends both. A package the file has is just linked.

```json
{"session_id": "8a1f…", "agent_id": "a9c2…", "plan": "search", "pkg": "SORT1", "provisional": true, "title": "SORT1 sort options"}
```

The link is exact, made by `spawn`, and the matcher never changes it.

These get `400`:

- `session_id` missing with `agent_id`;
- both or neither of `agent_id` and `cloud_url`;
- a `cloud_url` that isn't http(s);
- with `provisional`, a `pkg` that isn't a package ID.

An unknown plan gets `404`, and so does an unknown package without `provisional`.

### `POST /api/links/pr`: a linked agent created a PR

Auth: write. It answers `201`.

```json
{"session_id": "8a1f…", "agent_id": "a73d…", "url": "https://github.com/example-org/recipes-api/pull/42"}
```

aimd links the PR to every package the agent is linked to, with that link's method and confidence (`spawn`/`exact` after a spawn link). To find the agent's packages it looks for:

- a stored spawn or manual link for `session_id/agent_id`, where an empty `agent_id` means the main loop;
- failing that, the agent as the last board reached it, such as an agent working in a linked worktree.

If aimd has no row for the PR yet, it stores one as `OPEN` with only the URL known: a placeholder, which `GET /api/prs` and the boards mark `"unread": true`. So the card shows the PR in review before GitHub is read, and the dashboard shows it as not read from GitHub yet rather than as an open PR. aimd then reads it by number at once, as [`POST /api/prs/refresh`](#post-apiprsrefresh-read-a-pr-from-github-now) does, which fills in the rest within seconds. Its idle GitHub ticks (every `poll.github_idle`) read any placeholder still left, so a read that failed is tried again.

An agent linked to nothing gets `404`, with a message saying to link it first. A URL that isn't `https://github.com/<owner>/<repo>/pull/<n>` gets `400`.

### `POST /api/prs/refresh`: read a PR from GitHub now

Auth: write. It answers `202`.

The plugin sends it after a tool call that worked created, merged or closed a PR: `gh pr create` by an agent no package claims, `gh pr merge`, `gh pr close`, or the GitHub MCP server's `merge_pull_request` (or an `update_pull_request` that closes). aimd reads that PR from GitHub by number soon, instead of at its next poll, and stores what GitHub answers. The request only says which PR to read: nothing in it is taken as the PR's state.

```json
{"ref": "12", "repo": "example-org/recipes-api", "cwd": "/Users/me/Projects/recipes-api", "action": "merge"}
```

| Field | Meaning |
|---|---|
| `ref` | The PR as the command named it: a pull request URL, `owner/repo#123`, a number (`123` or `#123`), or a head branch. Empty means the PR of the branch checked out at `cwd`, as `gh pr merge` takes no argument. |
| `repo` | The repo a number or branch is in, when the command named one (`-R`/`--repo`): `owner/repo`, or its GitHub URL. Without it, the repo at `cwd`. |
| `cwd` | The absolute folder the command ran in. |
| `action` | `create`, `merge` or `close`. It is logged and changes nothing. |

A branch names its stored open PRs in that repo, and none when aimd has none: its branch poll finds a new one. The answer lists the PRs aimd will read (`prs`, as `owner/repo#123`) and how many weren't waiting already (`queued`):

```json
{"prs": ["example-org/recipes-api#12"], "queued": 1}
```

Requests are joined into one read, at most one every 10 seconds, so a burst of merges costs one GitHub query a repo chunk (1 point). None run while the budget says stop: the polls read those PRs later. A repo GitHub can't read is listed in `GET /api/status`'s `github.unreadable`.

A number or branch with neither `repo` nor an absolute `cwd`, a `cwd` outside every GitHub repo aimd knows, or a `ref` that is no PR or branch gets `400`.

### `POST /api/links/manual`: link by hand

Auth: write. It answers `201`.

```json
{"plan": "search", "pkg": "UI2", "kind": "pr", "ref": "example-org/recipes-web#7"}
```

`kind` and `ref` are as in [Values](#values). A PR may be given by its URL, and an agent by a bare session id (its main loop). The link is `manual`/`exact`, and it clears an earlier unlink of the same target. A malformed `kind` or `ref` gets `400`, and an unknown plan or package gets `404`.

### `DELETE /api/links`, `DELETE /api/links/{id}`: unlink

Auth: write. It answers `200`.

```json
{"plan": "search", "pkg": "UI2", "kind": "pr", "ref": "https://github.com/example-org/recipes-web/pull/7"}
```

The body is the same as a manual link. `DELETE /api/links/{id}` takes a stored link's `id` from a board instead.

aimd removes the link and leaves a tombstone. From then on:

- the matcher never makes that link again;
- nothing is reached through it;
- the target shows in the Unmatched lane with the note `you unlinked it from <ID>`, until something else claims it.

The link doesn't have to be stored: a link the board shows `via` another one is unlinked the same way. Unlinking twice is fine. The answer's `links` entry has no `id` or `method`.

---

## Provisional packages

A lead can dispatch an agent for a package before it has added the package's row to the plan file. aimd never edits the plan file. It shows the work on the board as a **provisional** package until the row is there.

For a registered plan, an ID that isn't in its tables counts as unknown when one of these names it:

- a spawn brief's `Package: <slug> · <ID>` line, from the plugin's spawn link (`POST /api/links/spawn` with `provisional`);
- a same-repo branch named `<slug>/<id>-…`, while there is work on it: a worktree has it checked out and it has no PR yet, or it has an open PR;
- an open same-repo PR's `Plan: <slug> · <ID>` line.

The branch and PR sources work without the plugin, so a lead session that started before the plugin was installed is covered too.

These are left alone:

- a slug no plan is registered under. For a branch or a PR, also a plan registered only for another repo;
- a brief with no `Package:` line;
- IDs the plan file has;
- merged and closed PRs, and the branches they leave behind;
- work already linked to one of the file's packages.

A branch whose PR has a `Plan:` line for the plan leaves the ID to that line.

**On the board** (`GET /api/plans/{slug}`), a provisional package has `provisional: true`, its `suggested_row`, `first_seen` and `sources` (see the package fields above).

- **Its links** attach the way they do for any package: the matcher links its PR by the `Plan:` line and its branch by name, and the plugin's spawn link names its agent.
- **It has a real lifecycle** (working, in review, done, …), comes after the file's packages, and is never ready to dispatch.
- **Its title** is the best one a source gave: a spawn's description, then a PR's title, then the branch's words.
- **The dashboard** draws it in a last column of the graph, dotted, with a "not in plan file" badge on its card, row and node. Its detail pane says "Not in *file* yet", with the suggested row and a Copy button.

**When the plan file gains the row,** the package becomes the file's. Its links, Mark done and history stay, and `provisional` goes. The file's cells (title, needs, size, lane) replace the provisional ones.

**When its work is gone.** A provisional package is deleted, with its links, after a day with none of these:

- a live agent linked;
- a worktree left on its branches;
- an open or merged PR.

So one named only by a branch whose worktree was removed goes a day later, and so does an agent that never left a branch or a PR. One whose PR merged stays until its row lands: done work belongs in the plan file.

**Nudges:**

- **When the lead spawns for an unknown ID,** the plugin tells it in its next turn, once per package: "Package SORT1 isn't in search-plan.md's tables. Add its row so ai-manager tracks it; it shows as provisional until then."
- **With the plugin setting `requirePlanRow` on** (off by default), the plugin refuses such a spawn instead, saying the same, unless aimd is down.
- **10 minutes after aimd first saw the work,** if the package still has no row and its plan has a monitored lead, a [Needs you item](#the-needs-you-inbox) of kind `plan_row` opens on the plan, with the suggested row. A monitored lead is a live session that registered the plan, or the monitored session that spawned the package's agent. The item closes by itself when the row lands, or when you Mark the package done.

---

## `GET /api/context?cwd=<path>`: what the plugin needs at session start

Auth: none. `cwd` must be absolute, or the answer is `400`. A directory outside every repo aimd may look at gets an empty context (`"repo": null`), not an error.

```json
{
  "cwd": "/Users/me/Projects/recipes-api/.claude/worktrees/agent-a73d…/src",
  "repo": {"path": "/Users/me/Projects/recipes-api", "name": "example-org/recipes-api"},
  "worktree": {"path": "/Users/me/Projects/recipes-api/.claude/worktrees/agent-a73d…", "branch": "search/api1-search-endpoint"},
  "plans": [
    {"slug": "search", "title": "Recipe search: dispatch plan",
     "path": "/Users/me/Projects/recipes-api/docs/search-plan.md",
     "band": "search · 1/6 done · 1 ready · 1 needs you",
     "packages": [
       {"id": "UI1", "title": "Search box", "repo": "recipes-web", "lifecycle": "ready",
        "suggested_branch": "search/ui1-search-box", "plan_line": "Plan: search · UI1"}
     ]}
  ],
  "package": {
    "plan": "search", "id": "API1", "title": "Search endpoint", "lifecycle": "in_review",
    "suggested_branch": "search/api1-search-endpoint", "plan_line": "Plan: search · API1",
    "link": {"kind": "branch", "ref": "example-org/recipes-api:search/api1-search-endpoint", "method": "plan_line",
             "confidence": "exact", "inferred": true, "via": {"kind": "pr", "ref": "example-org/recipes-api#42"}}
  }
}
```

| Field | Meaning |
|---|---|
| `repo` | The repo `cwd` is in: its main checkout, and its GitHub `name` when it has one. |
| `worktree` | The worktree `cwd` is in, with `"main": true` for the main checkout. |
| `plans` | The plans registered for the repo, by GitHub name or main checkout path, each with its file's `path` and every package's convention data. The plugin's `agent.spawn` hook needs nothing more to append the convention block. Each package's `repo` is the plan's Repo cell as written (`recipes-api`), for telling which packages belong to this repo. A provisional package has `provisional: true`: the plugin links to it, but still tells the lead the plan file lacks its row. |
| `package` | The package that `cwd`'s worktree or branch is linked to, directly or through its PR. The plugin uses it to add the convention block to a session that wasn't spawned through the Agent tool. The main checkout never gets one through the worktree, only through a linked branch. If more than one package matches, `candidates` lists them all and `package` is the first. |

---

## `GET /api/sessions[?all=1]`: live sessions

Auth: none, except for `waiting_on` (below). The types are `api.Sessions` and `api.Session`. It lists the live sessions as aimd has them. Headless `claude -p` runs are left out and counted in `hidden_headless`, unless `all=1` is given.

Each session also says whether it is monitored (`monitored`, `monitored_why`; see [Monitored sessions](#monitored-sessions)) and which terminal app it runs in (`terminal`, `terminal_app`; see [Terminal apps](#terminal-apps)).

### Which plugin build it runs: `plugin_build`

A session loads the plugin when it starts and keeps that build until it restarts, so after a `git pull` in the checkout the plugin is read from, the sessions already running still run the old one. The plugin reports which build it loaded on `session_start`, and aimd keeps it on the session's row.

| Field | Meaning |
|---|---|
| `plugin_build` | The build the session loaded: `git describe --tags --always` of the last commit that changed the plugin's folder, with `-dirty` when its tracked files differed from it, e.g. `v1.0.0-12-gabc1234`. Commits that don't touch the plugin don't change it. Left out when not reported: a plugin from before the field, or a folder that isn't a git checkout. |
| `plugin_root` | The folder it loaded the plugin from (the one holding `.claude-plugin/plugin.json`). |
| `plugin_on_disk` | The build in that folder now, worked out by aimd the same way and cached for a minute. aimd only looks in a folder whose `plugin.json` names this plugin. Left out when it can't tell. |
| `plugin_outdated` | `true` when `plugin_build` and `plugin_on_disk` are both known and differ: the session runs an older plugin, and a restart (quit, then `claude --resume`) loads the one on disk. The dashboard's Sessions page marks it **Older plugin**. |

### What a session waits on: `waiting_on`

`waiting_on` says what a session that needs you waits on, as the plugin reported it:

- It is there only while the session's state is `needs_you` and the plugin described the ask. It is the oldest pending ask's.
- aimd keeps it in memory, never in its database, so an aimd restart forgets it, as it forgets pending asks.
- A needs-you that only `claude agents --json` reports has none: a background job waiting for a reply, or a session without the plugin.

| Field | Meaning |
|---|---|
| `kind` | `permission`, `question` or `plan` |
| `agent_id` | The subagent whose dialog it is; left out for the main loop. Subagents can only raise permissions. |
| `tool`, `tool_use_id` | The tool asked about, and the call. |
| `summary` | `permission`: one line saying what the call does (the Bash command, the file path, the URL…), at most 300 characters, with the secrets the plugin and aimd spot replaced by `[redacted]`. |
| `questions` | `question`: each `{header, question, options, multi_select}`, at most 4 questions of at most 6 option labels. |
| `plan` | `plan`: the plan's first 12 lines and 1000 characters. |
| `since` | When the ask began (the session's `needs_you_since`, when it is the only one). |

```json
{"key": "pid:95591", "session_id": "55f811f4-…", "state": "needs_you", "state_reason": "question: AskUserQuestion", "needs_you": true,
 "waiting_on": {"kind": "question", "tool": "AskUserQuestion", "tool_use_id": "toolu_01Ae…", "since": "2026-11-06T18:42:29.172Z",
                "questions": [{"header": "Ranking", "question": "Rank results by relevance or by date?", "options": ["Relevance", "Date"]}]},
 "...": "…"}
```

### Who may read it

- **Reads are open to local processes, by design.** aimd listens only on 127.0.0.1. `GET` requests (the dashboard's pages and `/api/` JSON) need no credentials. The `Host` check stops DNS rebinding, and aimd sends no CORS headers, so other websites can't read the answers. Any process on this Mac can.
- **That is no new exposure on a one-account Mac.** Those processes run as you, and you can already read `~/.ai-manager/token` and Claude Code's transcripts, which hold far more than aimd shows.
- **Wait detail is redacted twice.** `waiting_on` holds the command a person is asked to allow, the question and its options, or the plan. The plugin redacts it before it leaves the session, and aimd redacts it again when it arrives. A command can still carry a secret neither layer spots.
- **The `/api/` field needs the token.** `GET /api/sessions` includes `waiting_on` only when the request carries aimd's token in `X-AIM-Token`, as the plugin and `aim` send it. A dashboard page's CSRF token (`X-CSRF-Token`) doesn't count, since every page embeds one and anything that can fetch a page has it. Without the token, each session is listed with its state, `state_reason` and `needs_you_since`, but no `waiting_on`. The only other `/api/` answers that carry the wait detail are the inbox's (`GET /api/inbox`, `GET /api/inbox/{id}` and the answer of `POST /api/inbox/{id}`), under the same rule.
- **Pages show the redacted detail.** aimd's own server-rendered pages are your UI and may show it. The app's script fetches HTML fragments, not `/api/` JSON, and uses the CSRF token only for writes.

### Terminal apps

`terminal` and `terminal_app` say what terminal app the session runs in. aimd reads it from the process tree, with no AppleScript and no Automation permission:

1. on each session poll it takes one snapshot with `/bin/ps -A -o pid=,ppid=,tty=,comm=`;
2. it walks up from each session's pid to the process that made its tty: the nearest ancestor on another tty.

The answer is kept in memory, as of the last poll. If `ps` fails, the last answers stand.

| `terminal` | When | `terminal_app` |
|---|---|---|
| `iterm2` | The tty's maker is iTerm2, or its `iTermServer-<version>` process, which keeps iTerm2's ttys and outlives an iTerm2 restart. | `iTerm2` |
| `terminal` | Terminal.app made the tty. | `Terminal` |
| `tmux` | A tmux server made it: claude runs in a tmux pane, even if tmux runs in an iTerm2 tab. | `tmux` |
| `other` | Another app made it: VS Code, Ghostty, Warp, sshd, `script`, … | The app's `.app` bundle name (`Visual Studio Code`, `Ghostty`), `SSH`, or the command's name; left out when unknown. |
| `none` | No pid, no such process, or no tty. Also a session the last poll didn't see (one the plugin reported first), and any background session: claude's job host gives each job a tty that no app shows. | Left out. |

```json
{"key": "pid:91470", "kind": "interactive", "pid": 91470, "terminal": "iterm2", "terminal_app": "iTerm2", "state": "working", "...": "…"}
```

The dashboard tags every session with this: iTerm2, Terminal, tmux, the other app's name (or "other app"), or "no terminal". It enables a Go button only for an interactive session that isn't headless and runs in `iterm2` or `terminal`, since those are the apps whose tabs Go can switch to. Every other session gets a disabled Go, with the reason as its title:

| Session | Reason |
|---|---|
| background | Background session: no terminal tab to switch to. Open it with `claude attach <id>`. |
| headless | Headless `claude -p` run; Go only switches to interactive sessions. |
| `tmux` | Runs inside tmux; Go can only switch to iTerm2 and Terminal tabs. |
| `other` | Runs in `<app>` (or "another app"); Go can only switch to iTerm2 and Terminal tabs. |
| `none` | No terminal. |

A plan card's agent link follows the rules of the session Go would go to: a subagent's lead, the linked session, or a background job.

## Monitored sessions

aimd tracks sessions by opt-in. Only the **monitored** sessions count for:

- the Needs you inbox;
- `aim status`'s needs-you counts;
- aimd's desktop notifications.

The dashboard's Sessions page lists the others in a collapsed group. The rules are in `internal/monitor`.

### Which sessions are monitored

With `[sessions] monitor = "opt-in"` in `config.toml` (the default), a session is monitored when any of these holds:

1. **Plan lead** (`plan`): it registered a plan, with `POST /api/plans` and its `session_id`. This lasts until the plan is unregistered.
2. **Watched** (`watch`): you ran `/aim watch` in it, or pressed **Watch** on the dashboard's Sessions page. `/aim unwatch` or **Stop monitoring** ends it.
3. **Plan agent** (`agent`), worked out on every read and never stored. Either of these:
   - **A registered plan's board links it to a package as an agent.** That's a spawn or manual link, or one the matcher reaches through the package's worktree, branch or PR (the board's agent links, `via` included). A link to one of its subagents counts for the session too, because a subagent runs inside its lead and its needs-you shows on the lead's row.
   - **Lineage ties it to a monitored lead.** It works in a worktree (not a main checkout) that Claude Code locked for an agent of that lead's process (the lock reason names the lead's pid), or that the scanner records as that session's. A background session has no other lineage to its lead yet.

With `monitor = "all"`, every session is monitored. A session that no rule covers then has the reason `all`.

The first two are stored. A row names the session by its Claude Code session id, the one the plugin reads with `$.session.id()`. That id changes on `/clear` and when a background job respawns, so aimd moves the rows to the new id wherever it sees the change:

- in the plugin's `session_start` after `/clear`, which carries the previous id;
- in a session poll that lists a new session id under the session's key (its pid, or its job id). That also sets the session's `prev_session_id`.

A session also matches a row under the id it had before its last `/clear`. So a watch follows the session through any number of `/clear`s, and `claude --resume` of a watched conversation brings its watch back.

### PRs

A PR **counts** for Needs you (and `prs_needing_you`) only when either:

- a registered plan's board links it to a package; or
- it is a same-repo PR whose head branch is the branch a monitored session works on in that repo.

With `monitor = "all"`, every PR counts.

The one exception is a review request. An open PR whose review is requested from you needs you whether or not it counts, since someone asked you by name.

A PR that counts needs you only when it waits on you: your review is requested, or it is ready to merge and its plan says you merge (see [`GET /api/prs`](#get-apiprs-pull-requests-and-what-they-wait-on)). `GET /api/prs` and the dashboard's Pull requests page still show every PR, with what it waits on.

### In `GET /api/sessions`

```json
{
  "sessions": [
    {"key": "pid:91470", "session_id": "8a1f…", "state": "needs_you", "…": "…",
     "monitored": true,
     "monitored_why": [
       {"reason": "plan", "plan": "search", "since": "2026-11-06T10:02:11Z", "text": "registered plan search"},
       {"reason": "agent", "plan": "search", "pkg": "API1", "subagent": true, "text": "runs the agent of API1"}
     ]},
    {"key": "pid:91533", "session_id": "c42e…", "state": "idle", "…": "…", "monitored": false, "monitored_why": []}
  ],
  "hidden_headless": 2,
  "monitor": "opt-in",
  "not_monitored": 1
}
```

| Field | Meaning |
|---|---|
| `monitored` | The session is monitored. |
| `monitored_why` | Why, most explicit first: plan leads (in the order they registered), the watch, then agent reasons. `[]` when it isn't monitored. |
| `monitored_why[].reason` | `plan`, `watch`, `agent` or `all`. |
| `monitored_why[].plan` | The plan's slug, where there is one: the plan it registered, the package's plan, or its lead's plan. |
| `monitored_why[].pkg`, `subagent` | An `agent`'s package, and whether the link names one of the session's subagents rather than the session itself. |
| `monitored_why[].lead` | An `agent` by lineage: its lead's session key. |
| `monitored_why[].since` | When a `plan` or `watch` began. |
| `monitored_why[].text` | The reason for people: `registered plan search`, `you asked to watch it`, `agent of UI1`, `runs the agent of UI1`, `agent of <lead>`, `sessions.monitor is "all"`. |
| `monitor` | `config.toml`'s `sessions.monitor`: `opt-in` or `all`. |
| `not_monitored` | How many of the listed sessions aren't monitored. |

`?all=1` still means only "with the headless `claude -p` runs".

`GET /api/status`'s `counts` follows the same rules:

- `not_monitored` counts the live sessions that aren't monitored, headless ones aside;
- `sessions_needing_you` and `oldest_needs_you_since` count only the monitored sessions waiting on you;
- `prs_needing_you` counts only the PRs that need you.

Neither endpoint counts an expired background job, even a watched one still listed as `needs_you`: Claude Code deleted its conversation, so there is nothing to answer. If aimd can't read what the rules need (its database, the plan boards), it logs that and counts every session as monitored, so nothing that needs you is hidden.

### `POST /api/sessions/watch`, `DELETE /api/sessions/watch`: watch a session, or stop

Auth: write (the token, or a dashboard page's CSRF token). The plugin's `/aim watch` and `/aim unwatch` call these with the session's own id.

```json
{"session_id": "8a1f0c2e-…"}
```

`session_id` is a Claude Code session id: letters, digits and `.`, `_`, `:` or `-`, at most 128 characters. Anything else gets `400`. aimd doesn't need to have listed the session yet: the watch is stored, and it counts once the session shows up.

- `POST` watches the session. Watching a watched session keeps the first watch.
- `DELETE` removes the watch. When aimd lists the session, it also removes a watch under the session's id before its last `/clear`. It never removes a `plan` reason: a lead stays monitored until its plan is unregistered, and the answer says so.

Both answer `200` with the session as aimd sees it now:

```json
{
  "session_id": "8a1f0c2e-…",
  "watched": true,
  "changed": true,
  "watched_since": "2026-11-06T14:02:00Z",
  "known": true,
  "key": "pid:91470",
  "title": "recipes-api",
  "monitored": true,
  "monitored_why": [{"reason": "watch", "since": "2026-11-06T14:02:00Z", "text": "you asked to watch it"}],
  "monitor": "opt-in",
  "message": "Watching this session: it shows under Needs you and in notifications when it waits on you."
}
```

| Field | Meaning |
|---|---|
| `watched` | The session is watched after this call. |
| `changed` | This call changed that: `false` when it already was (`POST`) or already wasn't (`DELETE`). |
| `known` | aimd lists a live session with this id (or with it before its last `/clear`); `key` and `title` then name it. |
| `monitored`, `monitored_why`, `monitor` | As in `GET /api/sessions`. |
| `message` | What happened, in one line, for the plugin to show as is: for example `This session was already watched.` or `Stopped watching this session; it stays monitored: registered plan search.` |

A change sends a `sessions` change event on `/api/stream`.

```sh
curl -s -X POST -H "X-AIM-Token: $(cat ~/.ai-manager/token)" http://127.0.0.1:7777/api/sessions/watch -d '{"session_id": "8a1f0c2e-…"}'
```

### `/aim`, `/aim watch` and `/aim unwatch`

These are the plugin's commands:

- **`/aim`** with no arguments opens the dashboard: the page of this session's plan (`/app/plans/{slug}`, the plan the band's line shows), or `/app/` when the repo has none or aimd is down.
- **`/aim watch` and `/aim unwatch`** send `$.session.id()` to the endpoints above with the token, and show aimd's `message` in one line, prefixed `ai-manager:`. A refusal shows aimd's error. When aimd is down, or doesn't answer within 1 s, they say so and change nothing.
- **Any other argument** shows how to use `/aim`.

Like every hook, they never fail the command.

### On the dashboard

- **Needs you** lists only monitored sessions and the PRs that count.
- **Sessions** (`/app/sessions`) lists the monitored sessions by plan: a group per plan, in registration order, headed by the plan's title and linked to its page, then **No plan · watched** for the rest. A session's detail says why it is monitored.
- **Every other session** is listed only in a collapsed **Not monitored** group. Like every group on the page it's a `<details>` that works without JavaScript, and the browser remembers it open or folded across live refreshes and visits. Each row there has a **Watch** button. A session aimd has no session id for yet gets a disabled one, with the reason as its title.
- **A watched session's detail** has **Stop monitoring**.
- **Watch and Stop monitoring** post to `/app/sessions/watch` and `/app/sessions/unwatch` with the page's CSRF token, and answer with a toast that has an Undo.
- **With `monitor = "all"`,** there is no Not monitored group and no Watch button.

## `GET /api/prs`: pull requests and what they wait on

Auth: none. Every PR aimd has stored, by repo then number.

- `?state=open|merged|closed|all` narrows it; any other `state` gets `400`.
- `?repo=<owner/name>` narrows it to one repo.

The types are `api.PRs` and `api.PR`. The rule for what each PR waits on is in `internal/prwait`. Needs you, the dashboard's Pull requests page, `GET /api/status`'s `prs_needing_you` and this endpoint all use it, so they agree.

```json
{
  "prs": [
    {"repo": "example-org/recipes-api", "number": 42, "title": "Search endpoint (API1)", "state": "OPEN",
     "head_branch": "search/api1-search-endpoint", "mergeable": "MERGEABLE", "merge_state_status": "CLEAN", "checks_state": "SUCCESS",
     "needs_you": "ready_to_merge", "waiting_on": "you", "merges": "you", "reason": "ready to merge · this plan merges: you",
     "monitored": true, "plan": "search", "pkg": "API1", "…": "…"},
    {"repo": "example-org/billing", "number": 12, "title": "Invoice export (EXP1)", "state": "OPEN",
     "needs_you": "", "waiting_on": "lead", "merges": "lead", "reason": "ready to merge · this plan's lead merges it",
     "monitored": true, "plan": "billing", "pkg": "EXP1", "…": "…"},
    {"repo": "other-org/tools", "number": 2, "title": "…", "state": "OPEN", "author": "other-user",
     "review_requested": true, "review_requested_since": "2026-11-06T18:40:02Z",
     "needs_you": "", "waiting_on": "none", "reason": "not on a registered plan or a monitored session's branch",
     "monitored": false, "…": "…"}
  ]
}
```

| Field | Meaning |
|---|---|
| `needs_you` | `review_requested`, `ready_to_merge` or `""`. The PR is in Needs you exactly when this is set. |
| `waiting_on` | Who has the next move: `you` (exactly when `needs_you` is set), `lead` (the plan's lead session), `agent` (a live agent still works on its package or branch) or `none`. |
| `merges` | Who merges it: its plan's `merges` (`you` or `lead`), or `you` for a monitored session's branch on no plan. Left out when the PR doesn't count. An override on the Settings page ([Settings](#settings)) replaces the plan's line here and in the rule below; the board's own `merges` stays what the line says. |
| `reason` | The same in one line, for people: `your review is requested`, `ready to merge · this plan merges: you`, `ready to merge · this plan's lead merges it`, `ready to merge · on no plan, so you merge it`, `CI failing · an agent is still working on it`, `merge conflict · no agent is working on it, so it's for this plan's lead`, `not on a registered plan or a monitored session's branch`, `merged`, … |
| `monitored` | The PR counts for Needs you (see [PRs](#prs) above): a registered plan's board links it, or it is a same-repo PR on the branch a monitored session works on. With `monitor = "all"`, every PR counts. |
| `plan`, `pkg` | The registered plan (and package) it is on: the first board that links it, else the plan of the monitored session on its branch. `plan_slug` and `plan_pkg` are only what the PR body's `Plan:` line says. |
| `review_requested`, `review_requested_since` | Your review is requested on it, as the last review-request search of its owner found, and when aimd first saw the request. This is the fact; `needs_you` says whether it counts. |
| `unread` | aimd has never read the PR from GitHub: an agent reported it by URL ([`POST /api/links/pr`](#post-apilinkspr-a-linked-agent-created-a-pr)) before a poll read it. Only `repo`, `number` and `url` are known, and `state`'s `OPEN` is a guess. It waits on `none` with the reason `not read from GitHub yet`, and never needs you. An open row counts as unread while it has neither a `head_branch` nor an `updated_at`, which every PR GitHub sends has. |

The rule, in order, for an open PR that has been read from GitHub:

1. **Review requested** (`review_requested`, waiting on you): your review is requested. This holds on any open PR, even one that doesn't count (another person's project), on a lead-merged plan, on a draft and while an agent works, because someone asked you by name. Steps 2–4 apply only to a PR that counts.
2. **Agent** (`agent`): a live agent still works on its package (a board's agent link that runs and hasn't finished its turn), or a live session works on its branch (working, quiet, or waiting on you mid-turn).
3. **Ready to merge:** not a draft, no red CI, no conflict, no changes requested or review required, and `merge_state_status` is `CLEAN` (checks passing, or none). Then:
   - on a plan that merges `you`, or on no plan: `ready_to_merge`, waiting on you;
   - on a plan that merges `lead`: waiting on `lead`.
4. **Not ready, no agent on it:** waiting on `lead` on a plan (the lead sends it back to an agent), `none` on no plan. The `reason` names what's in the way: `draft`, `CI failing`, `merge conflict`, `changes requested`, `review required`, `CI running`, `behind its base branch`, `blocked by a branch rule`, `mergeability not known yet`.

A merged or closed PR, or one that doesn't count and has no review request, waits on `none`. So red CI, a conflict or changes requested never put a PR in Needs you: they wait on an agent or the lead.

**Where review requests come from.** aimd runs one GitHub search per account: `is:pr is:open archived:false review-requested:@me user:<owner>…`.

- It runs right after the open-PR search, on the same schedule and under the same rate-limit budget: every `poll.github_active` while a dashboard is connected, every `poll.github_idle` otherwise. Its health is part of the `github_search` source in `GET /api/status`.
- It costs 1 point a page.
- It finds requests to you, and to teams you're in, on anyone's PR. aimd stores the PRs it returns like the open-PR search's.
- A PR the search found keeps its request until a successful search of its owner no longer finds it. A failed search leaves its owners' requests as they were, and so does a search cut off at 1000 results.

## `POST /api/go`: bring a session to the front

Auth: write. `aim go` calls this. The logic is in `internal/focus` and `internal/term`.

The endpoint takes any live session, background ones included: it attaches them. Only the dashboard holds back its Go buttons, as described in [Terminal apps](#terminal-apps). The dashboard's Go buttons post to their own pages' routes (`/app/inbox/go`, `/app/overview/go`, `/app/sessions/go`, `/app/plans/{slug}/go`), which call the same logic and answer with a toast. A plain form post here needs the token.

A plan's page posts `/app/plans/{slug}/go` with `to`, what its button said: `agent` or `lead`. With `pkg`, it is the package's Go: to a live agent linked to it, or else to the plan's lead. Without `pkg`, it is the plan's Go to lead. The plan's lead is the newest of the sessions that registered it (`POST /api/plans` with `session_id`) that Go can reach under the rules in [Terminal apps](#terminal-apps). aimd decides the target again from its own state and takes no session from the form. It answers `409` with the reason when Go is off now, or when it would go to the other of agent and lead than `to` says.

Request:

```json
{"key": "pid:91470"}
```

`key` can be any of these, tried in this order:

1. a session key (`pid:<pid>`, `job:<id>`, `session:<id>`). `session:<id>` also matches a session whose id, or whose id before a `/clear`, is `<id>`;
2. a background job id;
3. a pid;
4. a session id;
5. a unique, case-insensitive prefix of a session's name, of its cwd (`~/` is expanded), or of its cwd's last folder. An exact name wins over prefixes. Headless `claude -p` runs are matched only by 1–4.

A subagent can be named by `agent:<agentId>`, `agent-<agentId>`, the bare `agentId`, or an agent link's ref `<session id>/<agent id>`. Go can't select a subagent inside its lead, so it goes to the lead session and says so.

What it does:

- **An interactive session:**
  1. it finds the session's tty with `ps -o tty= -p <pid>`;
  2. it selects the iTerm2 session with that tty (or, failing that, with the session's `ITERM_SESSION_ID`, when the plugin reported one), or else the Terminal.app tab with that tty;
  3. it brings the tab's window to the front and activates the app.

  Neither app is launched to look.
- **A background session:** it types `'<claude>' attach <id>` into a new iTerm2 tab when iTerm2 is running, otherwise into a new Terminal.app window.

The AppleScripts are constants, and every value is passed to them as an argument after validation.

Response `200`:

```json
{
  "key": "pid:91470",
  "title": "recipes-api",
  "action": "focused",
  "app": "iTerm2",
  "via": "tty",
  "tty": "/dev/ttys021",
  "message": "Focused recipes-api in iTerm2 (/dev/ttys021)."
}
```

| Field | Values |
|---|---|
| `action` | `focused`, or `attached` (with `command`, the text typed into the new tab) |
| `app` | `iTerm2` or `Terminal` |
| `via` | `tty` or `iterm_session_id` (for `focused` only) |
| `agent_id` | Set when the key named a subagent; `key` is then its lead's. |

Refusals are `{"error": "…"}`, plus `"candidates": [{"key", "title", "kind", "cwd"}]` for an ambiguous key:

- `400`: no key.
- `404`: no live session matches, or an agent isn't known, or its lead isn't live.
- `409`: the key matches several sessions.
- `422`: the session can't be reached. It has no terminal, it's in no iTerm2 or Terminal tab, neither app is running, a background session has no job id, or a value failed validation.
- `502` / `504`: osascript failed or timed out. The first Go may wait on macOS asking whether aimd may control iTerm2 or Terminal.

```sh
curl -s -X POST -H "X-AIM-Token: $(cat ~/.ai-manager/token)" http://127.0.0.1:7777/api/go -d '{"key": "recipes-api"}'
```

## The Needs you inbox

The inbox at `/app/inbox` has one item per waiting episode. An episode begins when:

- a monitored session starts waiting on you: a permission prompt, a question, a plan approval, or a background job's reply;
- a PR starts needing you: your review is requested, on any PR, or it is ready to merge and the merge is yours (see [`GET /api/prs`](#get-apiprs-pull-requests-and-what-they-wait-on));
- a [provisional package](#provisional-packages)'s plan row is still missing;
- a registered plan's package is your step and ready: it is `yours` (`Owner` you) or has a `needs_you` step (`you:` in its Needs cell), and its `lifecycle` is `ready`. The dashboard's plan page badges the same packages "needs you".

The rules are in `internal/inbox`, and the types in `internal/web/api/inbox.go`.

### Items and their ids

An item's `id` is its row id. It is stable across aimd restarts, and it is what `/app/inbox?item=<id>` and the notifications name. An episode is named by what waits (`ref`) and, for a session, when it began waiting:

| `source` | `ref` | One episode |
|---|---|---|
| `session` | `session:<row id>`: the session's row, which outlives `/clear` and a background job's respawn | From `needs_you_since` until the session stops needing you. A wait that begins later, or a different kind of wait, is a new episode. The poll's generic reason ("permission prompt") and the plugin's richer one ("permission prompt: Bash: …") are the same episode. |
| `pr` | `owner/repo#123` | From the moment the PR rule says it needs you until it says it doesn't. Review requested and ready to merge are separate episodes. |
| `plan` | `planrow:<slug>/<ID>` | A provisional package whose row is still missing from its plan file. It runs from 10 minutes after aimd first saw the package's work, once its plan has a monitored lead, until one of these: the row lands (`its row is in search-plan.md now`); you Mark the package done; or the package goes (expired, or the plan unregistered). |
| `plan` | `step:<slug>/<ID>` (the slug in lower case, the ID in upper case) | Your step: from when the package became ready (its `since`) until it isn't your step and ready any more: it is done (`it was marked done`, `it is ✅ in search-plan.md now`, `it is done: its PR merged`), work on it began, it has a PR in review, it is blocked again (`it is blocked again: it needs UI1`), or the package goes (its plan unregistered, or its row taken out of the file). Undo of Mark done makes it ready again: a new episode. A provisional package is never one. |

`kind` is one of:

- `permission`, `question` or `plan`;
- `reply`: a background job waiting for a reply;
- `prompt`: a dialog the poller can't name, such as `dialog open` or `waiting`;
- `review` or `merge`;
- `plan_row`: "Plan row missing";
- `your_step`: "Your step".

### States and moves

`state` is `open`, `progress` (In progress), `snoozed` or `done`.

| Move | From | To | Notes |
|---|---|---|---|
| `go` | open, progress | progress | For a session, it switches to the terminal tab first, where Go is allowed ([Terminal apps](#terminal-apps)); otherwise it answers `422` with the reason. For a PR, the page opens it on GitHub. For your step, the page opens the package on its plan's page (**Open in plan**). `due_at` is 10 minutes later: if the item still waits then, it returns to Open by itself. |
| `snooze` (`15m`, `1h`, `tomorrow`, `change`) | open, progress | snoozed | `tomorrow` is 9:00 local time the next day (before 4:00, the same day). A timed snooze returns to Open at `due_at`, or earlier when the wait changes (new commits, CI, the question). `change` has no `due_at`. |
| `done` | open, progress, snoozed | done | |
| `open` (back to Open) | progress, snoozed, done | open | |
| `undo` (`move`) | — | the state, due time and snooze your move left | Only your latest move on the item. |
| `seen` | any | the same | Clears `new`, as opening the item on the page does. |

aimd moves items too, and says why in their history:

- **Back to Open** after Go's delay, or when a snooze ends.
- **To Done when the episode ends,** with `ended_at` and `end_reason` set: `it stopped waiting`, `the session ended`, `merged on GitHub`, `it is no longer monitored`, …
- **To Done for a wait hidden by the old dashboard's Dismiss,** once. Only dismissals stored before Dismiss was removed do this, and moving the item back to Open sticks.

A moved-back item whose episode ended stays Open until you mark it done; `go` and `snooze` refuse it.

Two Settings values apply:

- **Done history** (30 days by default; 7, 30 or 90): Done items whose episode ended are deleted after it.
- **Back-to-Open delay** (10 minutes by default; 5, 10 or 30): how long after Go an item that still waits returns to Open.

A sync pass runs:

- before each read below, and before each inbox page and badge is drawn;
- before each read of a plan's summary, board or band, whose need-you count is the plan's Open items;
- every 10 seconds in aimd;
- before each notifier check.

Due times are stored, so they hold across restarts. A pass that changes anything sends `{"topic": "inbox"}` on `GET /api/stream`, and so does every move.

Notifications go only for sessions whose wait is an Open item, never In progress, Snoozed or Done, and only while the Settings page's macOS notifications switch is on. Each carries its item's page, but `display notification` can't attach a link: a click opens Script Editor, not the item.

### `GET /api/inbox[?tab=open|progress|snoozed|done|all]`: one tab's items

`tab` defaults to `open`. Items are grouped by plan: the registered plans in order, other plan slugs, then no plan. In each group, the oldest episode comes first.

```json
{
  "tab": "open",
  "counts": {"open": 4, "progress": 0, "snoozed": 0, "done": 1},
  "items": [{
    "id": 7, "url": "/app/inbox?item=7", "source": "session", "ref": "session:31",
    "kind": "permission", "kind_label": "Permission", "state": "open", "new": true,
    "title": "search endpoint agent", "subtitle": "recipes-api · agent of API1",
    "detail": "permission prompt: Bash: Create directory x",
    "plan": "search", "pkg": "API1", "session_key": "pid:4242", "repo": "example-org/recipes-api",
    "branch": "search/api1-search-endpoint", "folder": "/Users/…/recipes-api-wt",
    "since": "2026-11-06T10:58:00Z", "state_at": "2026-11-06T10:58:03Z"
  }]
}
```

- **A PR item** has `pr`, `pr_url` and `why` (the PR rule's reason) instead of the session fields.
- **A `plan_row` item** has `plan` and `pkg`, the title `<ID> isn't in <plan file>`, the suggested row as its `detail`, `why`, and the branch or PR that named the package when one did. Go refuses it (`422`): add the row instead.
- **A `your_step` item** has `plan` and `pkg`, the title `<ID> <title>`, its `you:` step as its `detail` (empty for a package whose `Owner` is you), `why` (`your step is ready`, with the step after a colon), and `repo` when its row names one. Its `since` is when the package became ready. A change of its `you:` step is a change: it wakes a snooze "until it changes". Marking the package done (`POST /api/plans/{slug}/packages/{id}/done`, or the dashboard's **Mark done**) ends it. The dashboard offers Mark done on the item only for a package whose `Owner` is you: an item there by its `you:` step alone is an agent's package, finished by its PR or its ✅, and is dismissed with `done`.
- `due_at` and `snooze` are there while an item is In progress or Snoozed; `ended_at` and `end_reason` once its episode ended.

A bad `tab` gets `400`.

**What it waits on.** `detail` is the session's needs-you reason, as the poll and the plugin's state report give it. A request with aimd's token in `X-AIM-Token` also gets `waiting_on` on a waiting session item: what the plugin reported the session waits on, as [`GET /api/sessions`](#what-a-session-waits-on-waiting_on) gives it (`kind`, `tool`, the redacted `summary`, the `questions` and their `options`, or the `plan`). It is there only while the session still needs you and the report is of the item's kind, and only for the token, never for a page's CSRF token. The dashboard's detail pane shows the same detail (pages are aimd's own UI), and falls back to `detail` when there is no report.

```json
{"id": 7, "kind": "permission", "detail": "permission prompt: Bash: Build the app", "…": "…",
 "waiting_on": {"kind": "permission", "tool": "Bash", "summary": "go build ./...", "since": "2026-11-06T10:58:00Z"}}
```

### `GET /api/inbox/{id}`: one item with its history

The item as above, plus `history`, oldest first: `[{"id", "at", "by": "you" | "aimd", "text", "from", "to"}]`. A note that didn't move the item, such as "it changed" or "it stopped waiting while Done", has `from` equal to `to`. An unknown or pruned id gets `404`.

### `POST /api/inbox/{id}`: a move

Auth: write. The page's forms post the same moves to `/app/inbox/go`, `/snooze`, `/done`, `/open` and `/undo` with its CSRF token.

```json
{"action": "snooze", "snooze": "1h"}
{"action": "undo", "move": 42}
```

Response `200`: `{"item": {…, "history": […]}, "move": {"id": 43, …}, "message": "Snoozed for 1 hour"}`. Pass `move.id` to `undo`.

Refusals are `{"error": "…"}`:

- `400`: an unknown action or a bad id;
- `404`: no such item;
- `409`: the move doesn't fit its state ("It is Done already.", "It moved again since; there is nothing to undo.");
- for `go`, the refusals of `POST /api/go`, and `422` when Go isn't allowed, with the reason.

```sh
curl -s http://127.0.0.1:7777/api/inbox | jq '.items[] | {id, kind_label, title}'
curl -s -X POST -H "X-AIM-Token: $(cat ~/.ai-manager/token)" http://127.0.0.1:7777/api/inbox/7 -d '{"action": "done"}'
```

## The band's progress line

In a session whose repo has a registered plan, the plugin's band above the prompt shows one line for one plan:

```text
search · 1/6 done · 1 ready · 1 needs you · 2 to merge · +1 plan
```

The plugin counts nothing itself. Every number is a field of [`GET /api/plans/{slug}/summary`](#get-apiplansslugsummarysincerfc-3339-the-band-and-plan_status) as aimd answers it, so the line changes when the summary's rules do:

| Part | Field |
|---|---|
| `1/6 done` | `done` / `total` |
| `1 ready` | The length of `ready`: the packages ready to dispatch, yours left out. |
| `1 needs you` | `needs_you` (`2 need you` for more than one). |
| `2 to merge` | `to_merge`, shown only when `merges` is `lead` and it is above 0. |
| `+1 plan` | The other plans `GET /api/context` lists for the repo. |

An answer missing `done`, `total`, `ready` or `needs_you`, or with one of them of the wrong type, shows no line.

- **Which plan.** The first of these:
  1. the plan this session works on: its worktree or branch is linked to one of the plan's packages, and not by a low-confidence guess;
  2. the plan this session registered or last dispatched an agent for, as the plugin saw it (kept in the session's `$.state`, so a reload keeps it);
  3. the repo's first plan, in `GET /api/context`'s order.
- **Refresh.** The line is asked for 15 s after its last refresh. It's asked for sooner after a turn ends (the main loop's or an agent's), a PR is created (`gh pr create` or the GitHub MCP tool), an agent is linked to a package, or a plan is registered. Those refresh at once, but at most once every 2 s: a burst collapses into one more refresh. Drawing the band never calls aimd, and no hook waits on a refresh. The repo's plan list comes from the plugin's cached `GET /api/context`, fetched again at most once a minute.
- **aimd down or slow** (no answer in 1 s), or the plan unregistered since: no line, never an error. The line comes back with the next refresh that aimd answers.
- **Pressing the line** (a click, or Enter once the band has the focus) opens the plan's page, `/app/plans/{slug}`, with `open` (`xdg-open` elsewhere). If neither opens a browser, a toast says where the page is.
- **No line** when the plugin's `band` setting is off (and nothing is asked for), and in a session that draws nowhere (`claude -p`, the SDK) until a terminal or desktop client attaches.

## Settings

The Settings page at `/app/settings` and these endpoints read and change the same settings. They are stored in aimd's database (`internal/store/settings.go`), not in `config.toml`. A setting nobody changed has no row and is its default.

| Field | Default | Values | What it does |
|---|---|---|---|
| `tab_count` | `true` | bool | The needs-you count in the browser tab's title: "(3) aim". |
| `notifications` | `true` | bool | aimd's macOS notifications. While it is off, nothing is sent. Turning it back on starts afresh: waits that began meanwhile count as already notified. `config.toml`'s `notify.enabled = false` still turns them all off. |
| `chime` | `true` | bool | A chime for new Open permission prompts and questions, on the Needs you page. |
| `highlight` | `true` | bool | New inbox items (never opened) are highlighted until you open them. The API's `new` field is the fact either way. |
| `back_to_open_minutes` | `10` | `5`, `10`, `30` | How long after Go an item that still waits returns to Open. It applies to the next Go; an item already In progress keeps its time. |
| `keep_done_days` | `30` | `7`, `30`, `90` | How long Done items whose episode ended are kept. A shorter history deletes the older ones at the next sync pass. |
| `shortcuts` | `true` | bool | The dashboard's keyboard shortcuts. |
| `theme` | `"system"` | `"system"`, `"dark"`, `"light"` | The dashboard's colors. `system` follows the Mac's appearance (the browser's `prefers-color-scheme`), and changes with it at once. `dark` and `light` force one. |
| `time_zone` | `"system"` | `"system"`, or an IANA name such as `"America/New_York"` or `"UTC"` | The zone the dashboard writes clock times in, and whose midnight starts "today" and "tomorrow" (and ends "Until tomorrow 9am" snoozes). `system` is the zone of the machine aimd runs on, from `/etc/localtime` (not `TZ`). A name must load on that machine; a stored one that stops loading counts as `system`. The API's own times stay UTC. |
| `system_time_zone` | the machine's | read-only | The zone `system` stands for, as in `"Europe/Berlin"`. A change to it is ignored. |
| `merges` | each plan's `Merges:` line | `you`, `lead` | Who merges each registered plan's PRs. A change is stored as an override of that plan's line. It replaces the board's `merges` before the PR rule reads it, so `GET /api/prs`, Needs you and the notifications follow it. The plan file is never changed. Unregistering the plan drops its override. |

Open pages follow a change within seconds. Every change sends `{"topic": "inbox"}` on `GET /api/stream`. A `merges` change also sends `{"topic": "prs"}`, after a sync pass that moves the PRs it affects. A `time_zone` change sends every topic, so every open page redraws its times.

### `GET /api/settings`: every setting

```json
{
  "tab_count": true, "notifications": true, "chime": true, "highlight": true,
  "back_to_open_minutes": 10, "keep_done_days": 30, "shortcuts": true, "theme": "system",
  "time_zone": "system", "system_time_zone": "Europe/Berlin",
  "merges": [
    {"plan": "search", "title": "Recipe search: dispatch plan", "merges": "you", "line": "you", "override": false},
    {"plan": "billing", "title": "Billing exports", "merges": "you", "line": "lead", "override": true}
  ]
}
```

`merges` lists the registered plans in registration order:

- `merges`: who merges now;
- `line`: what the plan's `Merges:` line says;
- `override`: whether the Settings page overrode it.

### `POST /api/settings`: change settings

Auth: write. The body has any of the fields above. `merges` is an object from a plan's slug (any case) to `"you"` or `"lead"`. `""` or `null` drops the override, so the plan's line decides again.

```json
{"chime": true, "back_to_open_minutes": 5, "theme": "dark", "time_zone": "America/New_York", "merges": {"billing": "lead", "search": null}}
```

Response `200`: every setting, as `GET` gives them.

Everything is checked before anything is stored. If one value fails, nothing changes:

- a value a setting can't take gets `400`, as in `{"error": "Can't save that: back_to_open_minutes is 5, 10 or 30 minutes."}`;
- an unregistered plan gets `404`.

The page's forms post to `/app/settings/set` and `/app/settings/merges` with its CSRF token, and each answer's toast has an Undo that posts the old value back:

- **`/app/settings/set`** takes `key` and `value`. `value` is `on` or `off` for a switch, the number for a timing, `system`, `dark` or `light` for `theme`, or `system` or a zone's name for `time_zone`.
- **`/app/settings/merges`** takes `plan` and `merges`.

```sh
curl -s http://127.0.0.1:7777/api/settings | jq '{chime, back_to_open_minutes}'
curl -s -X POST -H "X-AIM-Token: $(cat ~/.ai-manager/token)" http://127.0.0.1:7777/api/settings -d '{"merges": {"search": "lead"}}'
```

## Status, worktrees and events

### `GET /api/status`: aimd's health

Auth: none. `aim status` reads it, and prints the running aimd's `version`, with a note when it isn't the `aim` binary's own (`aim --version`): an update replaces the binaries while the old aimd keeps running until `aim up` restarts it.

- `version`, `started_at`, `now`;
- `watchers`: how many dashboard pages are connected to the stream;
- `mod_live`: whether the plugin is reporting;
- `sources`: each poller (`sessions`, `worktrees`, `github`, `github_search`, `retention`), with its interval, last run, last success, last error and whether it is stale;
- `github`: the rate-limit budget, and `unreadable`: the repos GitHub didn't answer for on their last read (not found, no access for the token, a failed token, or a PR number GitHub has no PR for), each with `repo`, `error` and `since`, sorted by repo. Their PRs stay as last read. `aim status` names one ("GitHub can't read owner/name") or counts several;
- `counts`: sessions, PRs and worktrees, including what needs you (see [Monitored sessions](#in-get-apisessions));
- `events`: counters for what the plugin has sent: batches, events, events dropped as repeats or of unknown kinds, older plugins' `tool_check` asks ignored (`ignored`, below), and batches refused.

### `GET /api/worktrees[?all=1][&repo=<path>]`: worktrees

Auth: none. The worktrees the last scans found: path, main checkout, branch, HEAD, lock and prune state, uncommitted and untracked counts, upstream state and last commit time. `?all=1` adds the ones since removed, and `?repo=` narrows the list to one main checkout's path.

### `POST /events`: the plugin's reports

Auth: the token. The plugin POSTs `{"events": [ … ]}`: at most 200 events and 256 KB a batch, about once a second. aimd answers `200 {}` as soon as the batch is decoded, and applies batches in order.

- Events of a kind aimd doesn't know, or missing a required field, are dropped and counted in `GET /api/status`'s `events`.
- A `needs_you` whose `source` is `tool_check` is dropped too, before anything else looks at it: no state, no Needs you item, no chime, no timeline row. Plugins from before the "needs you" fix send one for every call Claude Code's `tool.check` answers "ask", and in auto mode the classifier then decides nearly all of them with no dialog. Those plugins report a dialog that does open as `permission_request`, which still counts. The dropped ones are counted in `events.ignored`.
- A malformed body gets `400`, a missing or wrong token `401`, and an oversized batch `413`. The plugin drops a batch refused that way.
- On a timeout or a `5xx`, the plugin keeps the events and tries again.

[The plugin guide](guides/plugin-and-skills.md#what-it-reports-to-aimd) lists the events and what each carries.

## Endpoints at a glance

| Method and path | Auth | What |
|---|---|---|
| `GET /api/plans` | — | Every board |
| `POST /api/plans` | write | Register a plan file |
| `DELETE /api/plans/{slug}` | write | Unregister a plan (never touches its file or GitHub) |
| `GET /api/plans/{slug}` | — | One board |
| `GET /api/plans/{slug}/summary[?since=]` | — | Counts, ready, needs you (the inbox's Open items), to merge, unlocked, band |
| `POST /api/plans/detect` | write | Does a file look like a plan; is it registered or dismissed? |
| `POST /api/plans/dismiss` | write | "Not a plan" |
| `POST /api/plans/{slug}/packages/{id}/done` | write | Mark done |
| `DELETE /api/plans/{slug}/packages/{id}/done` | write | Undo Mark done |
| `POST /api/links/spawn` | write | Link a spawned agent or remote session, making a provisional package for one the plan file lacks |
| `POST /api/links/pr` | write | Link a PR a linked agent created |
| `POST /api/prs/refresh` | write | Read a PR a tool call created, merged or closed from GitHub now |
| `POST /api/links/manual` | write | Link anything by hand |
| `DELETE /api/links` | write | Unlink by target (tombstone) |
| `DELETE /api/links/{id}` | write | Unlink a stored link by id |
| `GET /api/context?cwd=` | — | Repo, plans and linked package for a directory |
| `GET /api/sessions[?all=1]` | — | Live sessions, with their terminal apps and whether they are monitored; with the token, what each waits on |
| `POST /api/sessions/watch` | write | Watch a session: monitor it |
| `DELETE /api/sessions/watch` | write | Stop watching a session |
| `GET /api/prs[?state=][&repo=]` | — | Stored PRs, each with what it waits on and whether it needs you |
| `POST /api/go` | write | Bring a session to the front in iTerm2 or Terminal |
| `GET /api/inbox[?tab=]` | — | The Needs you inbox: one tab's items and every tab's count; with the token, what each waiting session waits on |
| `GET /api/inbox/{id}` | — | One inbox item with its history |
| `POST /api/inbox/{id}` | write | Go, snooze, done, back to Open, undo or seen |
| `GET /api/settings` | — | The Settings page's settings, and who merges each plan's PRs |
| `POST /api/settings` | write | Change settings, or who merges a plan's PRs |
| `GET /api/status` | — | aimd's health and counts |
| `GET /api/worktrees[?all=1][&repo=]` | — | Worktrees the scans found |
| `GET /api/stream` | — | Change events (server-sent events) |
| `POST /events` | token | The plugin's event batches |

Examples with `curl`:

```sh
TOKEN=$(cat ~/.ai-manager/token)
curl -s -X POST -H "X-AIM-Token: $TOKEN" http://127.0.0.1:7777/api/plans \
  -d '{"path": "'"$HOME"'/Projects/recipes-api/docs/search-plan.md"}'
curl -s http://127.0.0.1:7777/api/plans/search/summary | jq -r .band
curl -s -X POST -H "X-AIM-Token: $TOKEN" http://127.0.0.1:7777/api/plans/search/packages/P1/done
```
