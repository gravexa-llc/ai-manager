# ai-manager — build plan

Hand-written fixture for the plan parser, the board and the plan API: a
made-up build plan for a local daemon, a CLI and a Claude Code plugin, in
the shape of a real one. Line numbers matter to the tests; append, don't
insert.

Status: v1 · 2026-11-05 · plan slug `aim` · phase 0 done except the manual check (R1)

## 1. What it is

A daemon that watches sessions, worktrees and pull requests, a CLI that
lists and cleans them up, and a plugin that reports from inside each
session. None of the tables in this section are package tables.

| Piece | Talks to | Notes |
|---|---|---|
| Daemon | the store, GitHub | one process \| one port |
| CLI | the daemon | `aim ls \| less` works |
| Plugin | the daemon | fails open |

The layout, which the parser must skip because it is in a code block:

```text
| Package | Needs |
|---|---|
| **Z1 In a fence** | — |
cmd/       the daemon and the CLI
internal/  everything else
plugin/    the plugin
```

## 2. Decisions

| # | Decision | Decided |
|---|---|---|
| D1 | Where state lives | **SQLite**, one file. |
| D2 | How the plugin reports | **Batched POSTs** to the daemon. |

| # | Question | Why it matters | Answered by |
|---|---|---|---|
| Q1 | Does a background session say who started it? | Lineage | R1 |

## 3. Phases and packages

Package tables use the same format as any plan, so this file is also a parser fixture.

**Lanes**

| Lane | Packages | Rule |
|---|---|---|
| Spike | S1, S2, S3 | Parallel. Each writes only its own `testdata/` folder. |
| Mod | C4, D2, E1 | **single-writer**: one agent at a time in `plugin/mod/`. |
| Store | B1, C2, C3, D1 | **single-writer** for `internal/store/migrations/`: one open PR at a time adds a migration. |
| Web | B3, C6, D3 | **single-writer** for `internal/web/` templates. |
| Plan | R0 | Lead only. |

### Phase 0 — Verify assumptions

| Package | What | Size | Needs |
|---|---|---|---|
| **F1 Repo scaffold** ✅ | The module, two binaries that build, the config package and `make check`. | S | — |
| **S1 Session and worktree probes** ✅ | Record what the session list and `git worktree list` print on a busy machine. | S | — |
| **S2 Mod event probe** ✅ | A logging plugin that records every event it sees. | M | — (its interactive part is R1) |
| **S3 GitHub query budget** ✅ | Measure what one batched query costs. | S | — |
| **R0 Fold findings into the plan** ✅ | The lead updates this plan from the probes. | S | S1, S2, S3 |
| **R1 Interactive-check results** | You run a five-minute check and paste its output to the lead. | S | S2; you: run the check |

### Phase 1a — Inventory CLI, no daemon, no plugin

| Package | What | Size | Needs |
|---|---|---|---|
| **A1 Worktree scanner** | `internal/worktrees`: find repos and their worktrees. | M | F1, S1 |
| **A2 GitHub client** | `internal/github`: one batched query per call. | M | F1, S3 |
| **A3 aim ls and aim prune** | The two commands, with a dry run by default. | M | A1, A2 |
| **A4 Session poller** | `internal/sessions`: poll the session list and diff snapshots. | S | F1, S1 |

### Phase 1b — Daemon and first dashboard

| Package | What | Size | Needs |
|---|---|---|---|
| **B1 Store** | `internal/store`: SQLite and numbered migrations. | M | F1 |
| **B2 aimd core** | The daemon: runs A1, A2 and A4 on a schedule. | M | A1, A2, A4, B1 |
| **B3 Dashboard v1** | Sessions, worktrees and open PRs on one page. | M | B2 |
| **B4 Install aimd** | The lead installs the daemon and checks the dashboard against reality. | S | B3; you: OK to install the launchd agent |

### Phase 2 — Plans and the mod

| Package | What | Size | Needs |
|---|---|---|---|
| **C1 Plan parser and detector** | `internal/plans`, with this file as a fixture. | M | F1 |
| **C2 Matcher and package state** | `internal/state`: links, lifecycles and badges. | L | C1, B1, A2 |
| **C3 Plan API** | Register, list, get and link plans. | M | B2, C2 |
| **C4 Mod v1: acting** | The plugin's hooks on spawns and tool calls. | L | C3, S2 |
| **C5 Skills** | Write a plan, check its status, join it. | S | C1 |
| **C6 Plan board** | Each plan's packages, lanes and links. | M | C3, B3 |
| **C7 Install the plugin** | The lead installs the plugin; you paste the token. | S | C4, C5; you: OK, since it runs in every session |

### Phase 3 — Live detail

| Package | What | Size | Needs |
|---|---|---|---|
| **D1 Event receiver and live state** | `POST /events` and the session state machine. | M | B2, S2 |
| **D2 Mod reporting** | The plugin reports what each session is doing. | M | D1, C4, R1 |
| **D3 Live UI** | Live updates on the board and a timeline. | M | D1, C6 |

### Phase 4 — In-session + iTerm2

| Package | What | Size | Needs |
|---|---|---|---|
| **E1 Band progress and /aim** | The progress line above the prompt and the `/aim` command. | S | D2 |
| **E2 Notifications and Go** | A notification when a session needs you, and a button that opens it. | M | D1; you: try Go on a few sessions |
