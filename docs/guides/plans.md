# Writing and registering plans

ai-manager is built around one way of working. A **lead** Claude Code session writes a plan: a markdown file in the repo with a table of work packages and what each one needs. The lead then dispatches one **agent** per package, usually in its own git worktree, and each agent opens a pull request. ai-manager reads the plan file, follows each package through its agent, branch, worktree, PR, CI and merge, and shows the result as a board.

This guide covers:

- the plan format;
- registering a plan;
- how ai-manager ties work to packages;
- how a package's state is worked out.

The plugin's `plan-write` skill teaches the same format to Claude, so "write a plan for …" in a lead session gives you a file in this shape.

## A short example

````markdown
# Recipe search: dispatch plan

Plan slug: search
Repos: example-org/recipes-api, example-org/recipes-web
Merges: you

## Lanes

| Lane | Packages | Rule |
|---|---|---|
| Schema | IDX1, API1 | **single-writer**: one agent at a time in `db/migrations/`. |
| Web | UI1, UI2 | Parallel with the API. |

## Phase 1

| Package | What | Repo | Size | Owner | Needs |
|---|---|---|---|---|---|
| **IDX1 Search index** ✅ | A full-text index on titles and ingredients. | recipes-api | M | | — |
| **API1 Search endpoint** | `GET /search?q=` over the index. | recipes-api | S | | IDX1 |
| **UI1 Search box** | A search box in the site header. | recipes-web | S | | — |
| **P1 Pick the ranking** | Decide how results are ordered. | | S | you | — |

## Phase 2

| Package | What | Needs |
|---|---|---|
| **UI2 Results page** | Results with paging, wired to the endpoint. | API1, UI1, P1 |
| **REL1 Release** | Ship search behind a flag, then to everyone. | UI2; you: approve the rollout |
````

Registered, this plan's board shows:

- **IDX1** done, from its ✅;
- **API1** and **UI1** ready to dispatch;
- **P1** ready, as your own step;
- **UI2** and **REL1** blocked.

The plugin's progress line above the prompt reads `search · 1/6 done · 2 ready · 0 need you`. **P1** isn't counted as ready, because no agent will pick it up.

## The format

### Package tables

A package table is a markdown pipe table whose header has two columns:

- **a package column:** `Package`, `Work package`, `Pkg`, `ID`, `Name`, `Task` or `Step`;
- **a dependency column:** `Needs`, `Depends on`, `Depends`, `Dependencies`, `Deps`, `Requires`, `Blocked by` or `Prerequisites`.

Header names are matched in any case, may be bold, and may have a remark in brackets (`Needs (IDs)`). A table without a dependency column is never a package table.

These columns are optional, and any other column is ignored:

| Column | Also accepted | Meaning |
|---|---|---|
| `What` | `Description`, `Summary`, `Scope` | What the package does. The plan page shows it under the package's title, with its `code`, bold and italics. |
| `Repo` | `Repository` | Which repo it lives in, when the plan spans several. Use a short name (`recipes-api`). |
| `Size` | `Estimate`, `Effort` | Shown on its card. |
| `Owner` | `Who`, `Assignee` | `you` marks your own step (below). |
| `Status` | `State` | `done`, `merged`, `complete` or `completed` marks it done. |

A plan can have many package tables, such as one per phase. They form one plan.

Tables inside fenced code blocks or HTML comments are ignored. So is a row commented out on its own line. The example above is in a code block for that reason.

### Package IDs and titles

The package cell starts with the ID and the title in bold: `**API1 Search endpoint**` is package `API1`, titled "Search endpoint". These forms also work:

- `**API1** Search endpoint`;
- `**API1:** Search endpoint`;
- `__API1 Search endpoint__`;
- a link around any of these;
- a cell without bold that starts with the ID.

An ID has the following parts, 10 characters at most:

1. one to four uppercase letters;
2. an optional hyphen;
3. a digit;
4. then letters and digits, optionally in hyphen-separated groups.

`P1`, `API1`, `UI2`, `B3b-2` and `SEC-1` are IDs. `v1`, `ID` and `search` are not. Each ID is used once in the whole file. A repeated ID is a warning, and its first row wins.

A file counts as a plan once it has a package table and at least two package IDs.

### Needs

The `Needs` cell lists the IDs a package waits on, separated by commas: `API1, UI1, P1`. Use `—` (or `-`, `none`, `n/a`, or nothing) for none.

- **Only IDs make dependencies.** Free text never does. Write "everything above" as the list of IDs.
- **Notes go after the IDs, after a semicolon:** `UI2; once the beta has run a week`. The note shows on the package's card and detail.
- **`you:` marks a step you take** for a package that an agent carries out, such as an approval or a check: `UI2; you: approve the rollout`. The package shows a "you: approve the rollout" badge until it is done. It stays an agent's package. Once it's ready, it's a **Your step** item in the dashboard's [Needs you](dashboard.md#needs-you) until work on it begins. That item has no **Mark done**, since the package is the agent's: when your part is done, dismiss the item with **Done**.

These are warnings, shown on the plan's page:

- an ID that isn't a package of this plan;
- `API1 and UI1` (a missing comma);
- a package that needs itself.

A dependency cycle is a warning too. Each package in it is blocked only by the members it needs that aren't done, so once one member is done, the rest can go ahead.

### Your own steps

`you` in the `Owner` column marks a step you do yourself, with no agent and no PR, such as **P1 Pick the ranking**. It is never in the "ready to dispatch" list. Once everything it needs is done, it's a **Your step** item in the dashboard's [Needs you](dashboard.md#needs-you). It's done when its row gets a ✅, or when you press **Mark done** on its card or on that item.

### Done

A ✅ anywhere in a row marks the package done. So does a `Status` of `done`, `merged`, `complete` or `completed`.

You don't have to keep the file up to date: a package whose PR has merged is done whether or not its row has a ✅.

### Lanes

A lanes table is any table with a `Lane` column that isn't a package table.

- **One row per lane.** The `Lane` cell names it.
- **Its packages** are the IDs in the column whose header mentions "package" (`Packages`, `Packages, in order`), separated by commas.
- **Its rule** is the rest of the row, as written.

A lane whose rule says `single-writer`, `single writer` or `one agent at a time` is a **single-writer** lane: at most one agent at a time should work in it. Use one for work that touches shared files, such as migrations or string catalogs.

- The board draws lanes as swimlanes.
- Two live agents in a single-writer lane show a "lane conflict" badge and a "lane collision" alert.
- The `plan-status` skill dispatches at most one package of a single-writer lane per wave.

A package belongs to one lane at most. A package listed in a second lane, or an ID that isn't a package of the plan, is a warning.

### Title, slug, repos and who merges

- **Title:** the file's first level-1 heading (or its first heading).
- **`Plan slug: search`** names the plan in branch names and PR lines. Without it, the slug is the file name minus a trailing `-plan`, so `search-plan.md` gives `search`. Keep it short and lowercase.
- **`Repos: example-org/recipes-api, example-org/recipes-web`** lists the GitHub repos the packages live in, the plan's own repo included if packages live there. Without it, the plan covers the repo the file is in.
- **`Merges: you`** or **`Merges: lead`** says who merges the plan's PRs:
  - `you` (the default, also with no line): a PR that's ready to merge waits on you, and shows in Needs you.
  - `lead`: the lead session reviews and merges its agents' PRs itself. A ready PR waits on the lead instead, and only a review requested from you reaches your Needs you. The plugin's line shows `· 2 to merge` for the lead.
  - Any other value is a warning and reads as `you`. The Settings page can override a plan's line without editing the file.

## Registering a plan

ai-manager only follows plans you register. There are two ways.

**The offer above the prompt.** When the lead session writes or edits a markdown file that looks like a plan, the plugin asks aimd about it. If the file isn't registered yet, a line appears above the prompt:

```text
search-plan.md looks like a dispatch plan: 6 packages, 2 lanes.  [Register]  [Not a plan]
```

- **Register** registers it.
- **Not a plan** dismisses the offer for that file. aimd remembers the dismissal until you register the file by hand.

The offer only comes from the session's main loop, not from its agents.

**The command.** In any session:

```text
/plan-register docs/search-plan.md
```

The path is relative to the session's folder. With no path, it registers the last file the plugin detected as a plan in this session.

**What registering does:**

- **It reads the plan.** A file with no package table is refused.
- **It takes the slug.** Another file already registered under the same slug is refused.
- **It picks the repos,** first match wins:
  1. the repos given with the request;
  2. the plan's `Repos:` line;
  3. the repo the file is in.

  Local repos are named by their GitHub `owner/name`, or by their folder when they have no GitHub remote.
- **It makes the session the plan's lead.** The lead is monitored from then on, and so are the agents it dispatches for the plan.
- **It starts a backfill.** In the background, one GitHub search per repo brings in the repos' PRs, merged and closed ones included. So packages already done or in review show that way at once.
- **It watches the file.** Edits show on the board within seconds. A save caught half-written, with no packages at all, is ignored. When aimd starts, it reads every registered plan again, so edits made while it was down show too, and so does what a newer aimd reads of an unchanged file (such as the What column).

Registering a file that is already registered updates it and keeps its links.

**The file has to be on this Mac and in a repo aimd knows:**

- a path ending in `.md`, at most 1 MB;
- inside a folder in `roots` (from `~/.ai-manager/config.toml`), or in a repo whose worktree aimd has seen.

**To unregister a plan,** open its page in the dashboard, select no package, and press **Unregister plan…**. aimd forgets the board, its links, Mark done, what you hid from Unmatched, and history. The file, branches, worktrees and PRs are untouched. Registering the file again starts afresh.

## How work is matched to packages

ai-manager links work to a package from markers the work carries. The plugin adds the markers for you when the lead dispatches agents. In order of trust:

| Marker | Where | Confidence |
|---|---|---|
| `Package: search · API1` | the first lines of the agent's brief | exact: linked when the agent spawns |
| `Plan: search · API1` | a line in the PR body | exact |
| A link you make | the plan's page | exact |
| `… (API1)` | the end of the PR title | high |
| `search/api1-search-endpoint` | the branch name: `<slug>/<id>-<short-name>` | high |
| `api1` as a word in another branch name | the branch name | low |

### The brief's `Package:` line

When the lead spawns an agent with the Agent tool, the plugin reads the brief. Its first `Package: <slug> · <ID>` line names the package:

```text
Package: search · API1

Build API1 (Search endpoint) from docs/search-plan.md: its row and the sections it names.
Branch search/api1-search-endpoint. PR title ends with (API1). PR body starts with: Plan: search · API1
Don't merge.
```

The agent is linked to the package as it starts, and the plugin adds a short block to the brief with the branch to use and the PR line to write:

```text
[ai-manager] This agent works on package search · API1 (Search endpoint) of a plan registered with ai-manager.
- Suggested branch: search/api1-search-endpoint
- Put this line in the pull request body: Plan: search · API1
```

Without a `Package:` line, the plugin looks for a package ID in the Agent call's description, and then in the brief. One match links the agent. When several of the repo's packages match, the plugin asks above the prompt:

```text
Link search endpoint work to: [API1] [UI2] [Skip]
```

A brief with no package ID at all is a general helper, and is left alone.

Write `Package:` lines and you won't need the fallbacks. The `plan-status` skill writes them for you when it dispatches.

### The PR's `Plan:` line

`Plan: search · API1`, on its own line anywhere in the PR body, ties the PR to the package for good: it outlives the branch and the worktree. The plugin checks it. When a linked agent runs `gh pr create`, or GitHub's MCP `create_pull_request` tool, without the line, the plugin refuses the call with a message saying which line to add. The agent adds it and tries again. When aimd isn't running, the check is skipped.

When the line names a plan registered for that repo, it settles the match. If it names a package that plan doesn't have, the PR shows as a provisional package (below).

### Branch names and PR titles

These are fallbacks for work that has no `Plan:` line yet:

- **A PR title** ending in `(API1)`.
- **A branch** named `search/api1-…` (the slug, then the ID).
- **A package ID as a word in any other branch name,** such as `feature/api1-endpoint`. This is a low-confidence guess. It only counts for branches first seen after the plan was registered, since short IDs turn up everywhere, and its card shows a "low-confidence link" badge.

An ID that two of the repo's plans share, or a branch that names two packages, matches nothing.

### Links you make

Each package's detail pane on the plan page has **Link a PR or agent…**. Pick something from the plan's Unmatched list or a live session, or type a reference. The picker starts on "Pick a PR, branch or session…", and an Unmatched entry's own **Link** starts on "Pick a package…", so nothing is linked until you choose:

| Kind | Reference |
|---|---|
| PR | `example-org/recipes-api#42` or its URL |
| agent | `session_id/agent_id`, a session id, or `job:<id>` |
| branch | `example-org/recipes-api:search/api1-search-endpoint` |
| worktree | the worktree's absolute path |
| cloud session | its URL |

The detail pane also lists the package's links and how each was made. **Unlink** removes one, and ai-manager never makes that link again. The target moves to the plan's Unmatched list with the note "you unlinked it from API1".

### What a link reaches

A link carries further along what ai-manager can see:

- a PR leads to its branch;
- a branch leads to the worktree that has it checked out;
- a worktree leads to the sessions and agents working in it.

So a PR matched by its `Plan:` line also brings in the agent working on it. Nothing is reached through a main checkout or a base branch, since the lead works there.

### Limits

- **Only PRs from the same repo count.** A PR from a fork is never matched.
- **IDs are looked up only in plans registered for that repo.**
- **Base branches are never matched:** the repo's default branch, and any branch a PR targets.

### Unmatched

The plan's page lists, under **Unmatched**:

- PRs in the plan's repos that no package of any plan links: open ones, and ones closed after the plan was registered;
- branches checked out in a worktree with no PR and no link.

Each has a **Link** control and, where it helps, the matcher's note, such as `ambiguous: …`.

The list is split by when each one appeared, so a plan on a busy repo isn't buried in its old PRs:

- **New** ones, at the top: PRs opened after the plan was registered, by GitHub's `createdAt` (for a PR aimd hasn't read from GitHub yet, when aimd first stored it), and branches whose worktree aimd first saw after it, by the first worktree scan that saw it. One whose time isn't known counts as new.
- **Older (N)**, folded: the rest. A PR opened before the plan but closed after it is there too.

**Not part of this plan** takes an entry off this plan's list for good. It links and unlinks nothing, so no package's state changes, and other plans on the repo still list it. It goes to **Hidden (N)**, folded, where **Restore** puts it back; the toast has **Undo** too. aimd keeps what you hid until you restore it or unregister the plan. If a hidden PR is linked later, it's on its package as usual, and if it's unlinked again, it goes back to Hidden.

### Sessions the lead didn't spawn

A session you start yourself (a new terminal tab, say) has no brief for the plugin to read. Tell it which package it works on, and the plugin's `plan-join` skill applies the markers:

- it renames the branch to `<slug>/<id>-<short-name>` before the first push;
- it starts the PR body with the `Plan:` line;
- it ends the PR title with `(<ID>)`.

## Work for a package the plan doesn't have yet

A lead sometimes dispatches an agent before adding the package's row to the plan. ai-manager never edits the plan file. It shows the work on the board anyway, as a **provisional** package marked "not in plan file". This happens when one of these names an ID the plan's tables don't have:

- a brief's `Package:` line;
- an open PR's `Plan:` line;
- a branch `<slug>/<id>-…` that has a worktree or an open PR.

What follows:

- **On the board.** The package comes after the file's packages, in the graph's last column, drawn dotted. Its detail pane has a **suggested row** in the plan's own table format, with a **Copy** button. Paste it in and fill in the blank cells.
- **A nudge.** The plugin tells the lead, in its next turn, to add the row.
- **In Needs you.** If the row is still missing 10 minutes later, and the plan's lead is monitored, a "Plan row missing" item opens. It closes by itself when the row lands, or when you mark the package done.
- **When the row lands,** the package becomes the plan's own, with its links and history.
- **When its work is gone.** A provisional package whose work is gone for a day (no live agent, no worktree, and no open or merged PR) disappears.

To refuse such spawns instead, turn on the plugin's `requirePlanRow` setting (see [The plugin and its skills](plugin-and-skills.md#settings)).

## How a package's state is worked out

Each package has one lifecycle, the first of these that applies:

| Lifecycle | When |
|---|---|
| **In review** | A linked PR is open. |
| **Done** | A linked PR merged and none is open; or the row has a ✅; or you pressed **Mark done**. Your own steps are done only by ✅ or Mark done. |
| **Working** | A linked agent is alive, or a linked worktree has commits of its own. |
| **Dropped** | Every linked PR was closed without merging. |
| **Blocked** | A package it needs isn't done. |
| **Ready** | Everything it needs is done. |

A package with a ✅ that gets a follow-up PR shows as in review, but still counts as done for what needs it.

**A live agent** is a session that hasn't ended, or a subagent while one of its runs goes on. A subagent's run starts when it's spawned, or when the lead sends a finished agent a message (it goes on under the same id), and ends with the run's last turn. Claude Code's own forks of an agent, such as compaction, carry ids no agent list names and work in the agent's folder. They never count as agents, so they can't keep a finished package working or make a collision. See [What it reports to aimd](plugin-and-skills.md#what-it-reports-to-aimd).

Badges add detail on a card:

| Badge | Meaning |
|---|---|
| needs you | A linked agent is waiting on you, or it's your step (`Owner` you, or a `you:` step) and it's ready. Your step is then an item in Needs you too, by the same rule. |
| CI failing, conflict, draft | About its open PR. |
| changes requested, review requested | Its open PR's review state. |
| waiting on you | Its PR is green, mergeable and not a draft. |
| stalled | It has commits, but no live agent and no open PR, for over a day. |
| quiet | Its agent hasn't been heard from for 15 minutes. |
| collision | Two live agents work on it. |
| lane conflict | Two live agents in its single-writer lane. |
| low-confidence link | One of its links is a guess from a branch name. |
| you: … | Its `Needs` cell's `you:` step, until it's done. |
| not in plan file | It's provisional. |

Badges are facts about the package. The **Needs you** inbox is narrower: it lists only what waits on you to act now. See [What "needs you" means](dashboard.md#what-needs-you-means).

## Asking where a plan stands

In a lead session, ask "what's ready?" or "where are we?". The plugin's `plan-status` skill calls its `plan_status` tool, which answers from aimd with each plan's summary and board. Claude then reports what needs you, what's ready to dispatch, what's blocked, what's in progress and what's done, and proposes the next wave of agents.

The dashboard's plan page shows the same board as a graph, a kanban and a timeline. See [The dashboard](dashboard.md#plans).
