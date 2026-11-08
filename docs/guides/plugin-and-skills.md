# The plugin and its skills

The `ai-manager` Claude Code plugin is what connects your sessions to aimd. It has two halves:

- **the mod**, TypeScript hooks that run inside every session;
- **three skills**, markdown instructions Claude loads when a task calls for them.

This guide covers how both are built, how the plugin is installed and updated, and what it sends to aimd.

## Layout

```text
.claude-plugin/marketplace.json     the repo is a one-plugin marketplace named ai-manager
plugin/
  .claude-plugin/plugin.json        name, version and the settings (userConfig)
  hooks/hooks.json                  {"modules": ["../mod/register.tsx"]}: loads the mod
  mod/
    register.tsx                    every hook: linking, the band, commands, plan_status
    aimd.ts                         the client for aimd: settings, token, 1 s timeout
    report.ts                       event reporting: the queue, batches and retries
    wait.ts                         last-action labels, what a dialog waits on, redaction
    text.ts                         Package: and Plan: lines, the convention block, PR bodies
    progress.ts                     the progress line's words
    *.test.ts, *.test.tsx           tests, run with `claude plugin test plugin/`
    testkit.tsx, fixtures.ts        test helpers
  skills/
    plan-write/SKILL.md
    plan-status/SKILL.md
    plan-join/SKILL.md
  types/index.d.ts                  the shapes the mod keeps in $.state
```

## The mod

The mod is one hooks module, `plugin/mod/register.tsx`, that Claude Code loads in every session. Its `register` function subscribes to the session's events with `on(event, handler)`. Each handler gets the engine (`$`), the event (`e`) and `next`, which passes the event on.

### What each hook does

**Linking agents to packages**

| Hook | Does |
|---|---|
| `session.start` | Asks aimd (`GET /api/context`) which repo, plans and package the session's folder belongs to. Registers `/aim`, `/plan-register` and the `plan_status` tool, and starts the progress line. |
| `agent.spawn` | When the lead spawns an agent: finds its package from the brief's `Package:` line (or a package ID in the description or brief), appends the convention block to the brief, and links the agent (`POST /api/links/spawn`). For a package the plan file lacks, it tells the lead to add the row, or refuses the spawn when `requirePlanRow` is on. For a brief that could mean several packages, it offers a choice above the prompt. |
| `tool.call` on `Agent` | Links a remote agent once its cloud session URL comes back. |
| `prompt.compose` | Adds the convention block to the system prompt of a session whose worktree or branch is already linked to a package. |
| `tool.call` on `Bash` | Before `gh pr create` by a linked agent: refuses it if the PR body lacks the package's `Plan:` line (when `prCheck` is on). After it: tells aimd the PR exists (`POST /api/links/pr`), so the card moves to "in review" and aimd reads the PR from GitHub within seconds. A PR from an agent no package claims is only read (`POST /api/prs/refresh`). |
| `tool.call` on `mcp__…create_pull_request` | The same, for GitHub's MCP tool. |
| `tool.call` on `Bash`, after `gh pr merge` or `gh pr close` | When the command worked: tells aimd which PR (`POST /api/prs/refresh`: the number, URL or branch the command named, its `-R` repo, and the session's folder), and aimd reads that PR from GitHub within seconds instead of at its next poll. The command's own output is never taken as the PR's state. With no PR named, only the main loop reports it, as the current branch's. |
| `tool.call` on `mcp__…merge_pull_request`, `mcp__…update_pull_request` | The same, for GitHub's MCP tools: a merge, or an update that closes the PR. |
| `tool.call` on `Write`, `Edit` | When the main loop saves a `.md` file: asks aimd whether it looks like a plan (`POST /api/plans/detect`), for the Register offer. |

**The band above the prompt**

| Hook | Does |
|---|---|
| `ui.render` on `AbovePrompt` | Draws the band from the mod's saved state, never by calling aimd: the progress line, the Register offer and the link offer. |
| `session.attach` | Starts the progress line when a terminal or desktop client attaches to a session that started without one. |
| `turn.complete` | Refreshes the progress line soon after a turn ends. |

**Commands and the tool**

| Hook | Does |
|---|---|
| `command.run` on `aim` and `plan-register` | The commands below. |
| `tool.call` on `mcp__ai-manager__plan_status` | The `plan_status` tool. |

**Reporting.** Every other hook queues an event for aimd and passes the event on unchanged:

- `classic.SessionStart`, `session.end`;
- `prompt.submit`, `turn.start`, `turn.complete`;
- `tool.call` (every call), `tool.check`;
- `classic.PermissionRequest`, `classic.Notification`, `classic.StopFailure`, `classic.WorktreeCreate`.

See [What it reports to aimd](#what-it-reports-to-aimd).

### The band

The band is the area above the prompt. It has up to three lines:

```text
search · 1/6 done · 2 ready · 1 needs you · +1 plan
search-plan.md looks like a dispatch plan: 6 packages, 2 lanes.  [Register]  [Not a plan]
Link search endpoint work to: [API1] [UI2] [Skip]
```

- **The progress line** shows one of the repo's plans: the one this session works on, else the one it registered or last dispatched for, else the first. Every number comes from aimd's plan summary:
  - `1/6 done`: the packages done, out of all of them;
  - `2 ready`: packages ready to dispatch, your own steps left out;
  - `1 needs you`: the plan's Open items in Needs you;
  - `· 3 to merge`: shown only on a plan with `Merges: lead`, for PRs ready for the lead to merge;
  - `+1 plan`: other plans registered for the repo.

  The line refreshes every 15 seconds, and within 2 seconds of a turn ending, a PR being created, an agent being linked or a plan being registered. Clicking it, or pressing Enter when the band has focus, opens the plan's page in the dashboard.
- **The Register offer** appears after the main loop saves a markdown file that looks like a plan and isn't registered or dismissed.
- **The link offer** appears when the main loop spawns an agent whose brief could mean several packages. Pick one, or **Skip**.

There's no band in `claude -p` runs, or when the `band` setting is off.

### Failing open

The mod must never get in a session's way, so every hook fails open:

- **Every handler ends in `.catch(($, e, next) => next(e))`.** On any error, the event goes on unchanged.
- **Reporting never waits.** It only queues the event and returns. The POST runs later, unawaited, and anything it throws is swallowed.
- **Every call to aimd has a 1 second timeout and never throws.** A failure comes back as an answer with `ok: false`. The plugin then carries on as if aimd weren't there:
  - no progress line;
  - no offers;
  - `/aim watch` says aimd isn't answering;
  - `plan_status` says it can't reach aimd.
- **Refusals only happen when aimd is up.** The two places the mod refuses a call, the PR check and `requirePlanRow`, do so only on aimd's answer. The PR check needs aimd to have answered within the last 2 minutes. With aimd down, both let the call through.
- **Drawing the band never calls aimd.** It reads only the mod's saved state.

### Settings

The plugin's settings are the `userConfig` fields in `plugin/.claude-plugin/plugin.json`:

| Setting | Default | What it does |
|---|---|---|
| `token` | none | aimd's write token: the contents of `~/.ai-manager/token`. Stored as a secret. Without it, the plugin can still read the progress line and `plan_status`, but can't link, register, report or watch. |
| `aimdUrl` | `http://127.0.0.1:7777` | Where aimd listens. |
| `band` | on | The band above the prompt: the progress line, and the Register and link offers. |
| `appendConventions` | on | Appends the convention block to each linked agent's brief, and to a linked session's system prompt. |
| `prCheck` | on | Refuses a linked agent's `gh pr create` when the PR body lacks its `Plan:` line. |
| `requirePlanRow` | off | Refuses to spawn an agent whose brief names a package its plan file has no row for yet. Off, the agent starts, the package shows as provisional, and the lead is told to add the row. |

The convention block that `appendConventions` adds looks like this:

```text
[ai-manager] This agent works on package search · API1 (Search endpoint) of a plan registered with ai-manager.
- Suggested branch: search/api1-search-endpoint
- Put this line in the pull request body: Plan: search · API1
```

To see the settings, and which are unset:

```sh
claude plugin configure ai-manager@ai-manager
```

To change some, pass a JSON object of strings. The settings you leave out keep their values:

```sh
echo '{"requirePlanRow": "true", "band": "false"}' | claude plugin configure ai-manager@ai-manager --values-stdin
```

To hand it aimd's token without printing it:

```sh
jq -n --rawfile t ~/.ai-manager/token '{token: ($t | rtrimstr("\n"))}' \
  | claude plugin configure ai-manager@ai-manager --values-stdin
```

Sessions pick up a change when they restart.

## Commands

| Command | Does |
|---|---|
| `/aim` | Opens this session's plan in the dashboard (the plan the progress line shows), or the dashboard's Overview when the repo has no plan or aimd is down. |
| `/aim watch` | Monitors this session: it shows in Needs you and notifications when it waits on you. |
| `/aim unwatch` | Stops watching it. A plan's lead stays monitored until its plan is unregistered. |
| `/plan-register [path]` | Registers a plan file, and makes this session its lead. Without a path, it registers the last file the plugin detected as a plan in this session. |

All three answer in one line, and none of them fails the session when aimd is down.

## The `plan_status` tool

The mod registers a tool Claude sees as `mcp__ai-manager__plan_status`. It answers "where does this plan stand?" from aimd, which has joined the plan file with GitHub, CI and live agents. That's more than Claude could work out from the plan file and `git`.

**Input** (both optional):

| Field | Meaning |
|---|---|
| `plan` | A plan's slug. Without it: every plan registered for this session's repo. |
| `since` | An RFC 3339 time. The summary then lists the packages unlocked (blocked → ready) since then. |

**Output:** JSON with the repo and, per plan:

- **a summary:** counts by lifecycle, the packages ready to dispatch, what needs you and why, PRs waiting for the lead to merge, packages unlocked, and provisional packages with their suggested rows;
- **a compact board:** every package with its lane, needs, lifecycle, blocked-by, badges and links (PR state and CI, agent state), plus unmatched PRs and branches, parser warnings and alerts.

When aimd can't be reached, the tool refuses with "ai-manager's plan_status couldn't reach aimd at … Is aimd running?". It never guesses.

In a repo with a plan registered when the session starts, the tool is in Claude's tool list. Elsewhere it waits behind ToolSearch, so it costs nothing in sessions that don't need it.

## The skills

Each skill is a folder under `plugin/skills/` with a `SKILL.md`. The file starts with front matter:

```markdown
---
name: plan-status
description: Answers "what's ready?" from ai-manager's plan_status tool and turns it into the next wave of agents. Use when the user asks where a plan stands or what's next, and before spawning agents for plan packages.
---

# Plan status and the next wave
…
```

- **`name`** must match the folder's name.
- **`description`** is what Claude reads to decide when to load the skill. Say what it does and when to use it.

Claude Code lists every skill's name and description in the session. It loads a skill's body only when a request matches its description, or when you ask for the skill by name.

| Skill | Purpose | Triggers on |
|---|---|---|
| `plan-write` | The plan format: package tables, IDs, `Needs`, `you:` steps, lanes, `Plan slug:`, `Repos:` and `Merges:`, with a full example. | Drafting or editing a markdown plan of work packages. |
| `plan-status` | Calls `plan_status` and reports, in order: what needs you, what's ready, blocked, working, done. Then picks the next wave: respecting single-writer lanes, at most 3 agents unless you say otherwise, and proposing before dispatching. Shows the brief format, starting with `Package: <slug> · <ID>`. | "What's ready?", "where are we?", "what's next?", and before spawning agents for plan packages. |
| `plan-join` | Applies the markers to a session the lead didn't spawn: renames the branch to `<slug>/<id>-<short-name>`, starts the PR body with `Plan: <slug> · <ID>`, and ends the PR title with `(<ID>)`. | The user says this session works on a plan package; before pushing or opening a PR for one. |

The plan format itself is defined by the parser in `internal/plans`, whose `doc.go` holds the rules. The [plans guide](plans.md) explains them for people.

### Adding or changing a skill

1. Create `plugin/skills/<name>/SKILL.md`, with `name: <name>` and a `description` in its front matter. Keep the body short and specific: what to do, in what order, and what not to do.
2. Keep example plan tables inside fenced code blocks, so a skill file never reads as a plan itself.
3. Load the plugin from your checkout and try it (see [Developing the plugin](#developing-the-plugin)).
4. Run `make check`. It runs `claude plugin validate plugin/`, and Go tests in `internal/plans/skills_test.go` that check every skill:
   - it has front matter, its `name` matches its folder, and its `description` isn't empty;
   - it has no package table outside a code block.

   They also check that the example plan in `plan-write` parses exactly as the skill describes, with no warnings.

When you change the plan format, change the parser, the `plan-write` skill and its example, and that test together, and update the [plans guide](plans.md).

## Installing and updating

The repo is its own plugin marketplace: `.claude-plugin/marketplace.json` at the root names the marketplace `ai-manager` and points its one plugin at `./plugin`. Install it once, at user scope, so it loads in every session:

```sh
claude plugin install ai-manager --marketplace ~/Projects/ai-manager
```

This adds your checkout as the `ai-manager` marketplace and installs the plugin as `ai-manager@ai-manager`. Then give it aimd's token (above).

**The plugin loads in place.** Because the marketplace is a local folder, Claude Code reads the plugin straight from your checkout's `plugin/` folder rather than copying it. So:

- **Keep the checkout on `main`.** Switching it to another branch changes the plugin in every new session.
- **`git pull` is the update.** `claude plugin update` isn't needed, and the `version` in `plugin.json` doesn't gate it.
- **New sessions load the new files.** For a running session, `/exit` and then `claude -c` to continue the same conversation. Background jobs pick it up the next time they start.

To remove it:

```sh
claude plugin uninstall ai-manager@ai-manager
claude plugin marketplace remove ai-manager
```

## Developing the plugin

Test changes in a session that loads the plugin straight from your working copy:

```sh
claude --plugin-dir plugin
```

The plugin loads for that session only, as `ai-manager@inline`, in place of an installed copy. Its settings are its own, so point it at a development aimd rather than your real one. See [CONTRIBUTING](../../CONTRIBUTING.md#testing-the-plugin) for running one.

In an interactive `--plugin-dir` session, Claude Code also lays the mod's API types into `plugin/.claude-plugin/types/`, which is git-ignored. Your editor can use them.

Before you push:

```sh
claude plugin validate plugin/   # the manifest, hooks, skills
claude plugin validate .         # the marketplace
claude plugin test plugin/       # the mod's *.test.ts files
```

`make check` runs all three. The tests drive the hooks with a fake engine (`testkit.tsx`) and fake aimd answers (`fixtures.ts`), so they need no aimd and no network.

Rules for the mod:

- **Every hook fails open.** End each `on(…)` with `.catch(($, e, next) => next(e))`, and keep any work that can fail away from the path that calls `next`. `turn.step` streams, so its hook and its handler are generators: the handler is `.catch(async function* ($, e, next) { return yield* next(e) })`.
- **Never block a session on aimd.** Calls go through `call()` or `request()` in `aimd.ts`, which time out after 1 second and never throw. Reporting is unawaited.
- **Keep the hot paths cheap.** `tool.call` runs on every tool call: test with a local regex or an in-memory map before doing anything else.
- **Send summaries only** (below). A new kind of detail needs redaction on both sides: `plugin/mod/wait.ts` and `internal/events/redact.go` share the same patterns, tested on the same examples.

## What it reports to aimd

The mod POSTs events to aimd's `/events`, with the token:

- in batches about once a second;
- at once when a session starts waiting on you, hits an error, or ends;
- up to 200 events queued while aimd is down, retried after 2, 4, 8 and then every 15 seconds.

| Event | Carries |
|---|---|
| `session_start`, `session_end` | The session's folder, repo, worktree and branch; how it started or ended; at the start, the plugin build it loaded (below) |
| `prompt` | Where the prompt came from (you, the SDK, …), never its text |
| `turn_start`, `turn_complete` | The loop (main or which subagent); at the end of a turn, the first line of Claude's answer, at most 300 characters, and in the main loop its token count (below) |
| `tool` | A last-action label, such as `Bash: Run the tests`, `Edit src/search.ts` or `WebFetch example.com`, never the tool's input or output; in the main loop, its token count |
| `needs_you` | That the session waits on you, why, and what it waits on (below) |
| `idle`, `error` | The loop and a short reason |
| `worktree_create` | The new worktree's name |
| `agent_start` | A subagent's run began (Claude Code's `SubagentStart`): the agent, its type and its folder |
| `agent_list` | The session's agents and their state, every 30 seconds while it has any, with each subagent's token count: its latest model response's input, cache and output tokens, as Claude Code's agent list counts them |

**Which loops are agents.** Every run of a subagent starts with `turn_start` and `agent_start` under the agent's one id and ends with `turn_complete`. That includes a run that resumes a finished agent when the lead sends it a message. aimd counts a loop as an agent once one of these names it: `turn_start`, `agent_start`, `agent_list`, or a `worktree_create` of `agent-<id>`. Claude Code also runs forks of an agent for its own work, such as compaction or memory. A fork has an id no agent list names, works in the agent's folder, and its tools are refused. Its tool attempts still reach the mod as `tool` events. Such a loop is never an agent: it isn't live or working, it gets no row of its own, and its calls only go in the timeline.

**Token counts.** After each model request the plugin keeps the loop's count: the response's input, cache and output tokens, as Claude Code's agent list counts them. It isn't a running total, so it goes down after a compaction. A subagent's goes out on `agent_list`. The main loop's goes out on the main loop's next `tool` or `turn_complete`, since every request either calls tools or ends the turn. After `/clear` the main loop has no count until its next response. The dashboard shows both in the agent's line.

**When it says a session needs you.** Only when you're actually asked something:

- a permission dialog opens (`classic.PermissionRequest`);
- a question or a plan to approve (`AskUserQuestion`, `ExitPlanMode`);
- Claude Code's own notification that a dialog is waiting.

A call the mode decides by itself never counts. Claude Code's `tool.check` answers "ask" for any call no rule allows, but the mode's decider takes that ask first: in auto mode, its classifier approves or refuses most calls with no dialog at all. So `tool.check` only notes which call it is, for the dialog that follows if one does open. The need ends when the call finishes, the loop's turn moves on, or you send a prompt.

Older builds of the plugin did send a `needs_you` for every `tool.check` ask, with source `tool_check`, and a session keeps the build it started with until it restarts. aimd ignores those: it drops them before they reach the session's state, the Needs you inbox, the chime or the timeline, and counts them in `GET /api/status` (`events.ignored`). The same builds report a dialog that really opens as `permission_request`, so real prompts from old sessions still need you.

**Which build a session runs.** The mod is loaded once, when a session starts, from the plugin's folder (a checkout's `plugin/`). A `git pull` there doesn't reach sessions already running. So `session_start` carries `plugin_build`, the build the session loaded, and `plugin_root`, the folder. The build is `git describe --tags --always` of the last commit that changed the plugin's folder, with `-dirty` when its tracked files differ from that commit, such as `v1.0.0-12-gabc1234`. Nobody bumps a number for it: `plugin.json`'s version stays the same between releases, and commits that only touch aimd leave it alone. The mod works it out with three `git` calls in its own folder, each allowed a second, off the hook's path. If any of them fails, as in a folder that isn't a git checkout, `plugin_build` is left out. aimd works out the build in that folder now the same way, and the dashboard marks a session whose build differs **Older plugin**, as in [Using the dashboard](dashboard.md#sessions).

**Summaries only.** The mod never sends a prompt, a file's contents, a whole command or a tool's output, with one exception: what a dialog waits on, so the dashboard can show it. That is:

- the command or path a permission prompt is about, on one line, at most 300 characters;
- the questions and their options (at most 4 questions of 6 options);
- the first 12 lines of a plan to approve.

**Redaction.** Before it leaves the session, the mod replaces these with `[redacted]`:

- assignments whose name says secret, such as `GITHUB_TOKEN=…` or `api_key=…`;
- `Authorization` and `Cookie` headers, and other secret-named `-H` headers;
- `--password` and `--token` style flags;
- Bearer and Basic credentials;
- `user:password@` in URLs;
- GitHub, OpenAI-style, Slack and AWS key shapes;
- long runs of base64 or hex.

aimd applies the same rules again when the event arrives. It keeps the detail in memory only while the ask is pending, never writes it to its database, and gives it out over `/api/` only to callers with the token.

aimd answers each batch at once and never sends the mod anything to act on beyond "accepted" or "drop it".
