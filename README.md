# ai-manager

A local dashboard for running Claude Code at scale. A lead session writes a plan of work packages and dispatches one agent per package, each in its own git worktree. Each agent opens a PR. ai-manager follows every package from plan to agent, worktree, PR, CI and merge. It shows what needs you, lets you jump to the right terminal tab, and tells you which worktrees are safe to delete.

It has three parts, all on your Mac:

- **`aimd`**: a small daemon under launchd. It watches your Claude Code sessions, git worktrees and GitHub PRs, and serves the dashboard at http://127.0.0.1:7777.
- **`aim`**: a CLI to start and stop the daemon, list and prune worktrees, and jump to a session.
- **The plugin** (`plugin/`): a Claude Code plugin that loads in every session. It links agents to their packages, reports what a session is waiting on, adds a progress line above the prompt, and gives you `/aim` and `/plan-register`.

## What you get

- **Overview:** progress for each plan, agents running now, the last 24 hours of activity, and cleanup and health.
- **Needs you:** an inbox of everything waiting on you, grouped by plan. That covers permission prompts, questions, plan approvals, background jobs waiting for a reply, and PRs that need your review or your merge.
  - **Go** jumps to the session's terminal tab; the item comes back if the session is still waiting 10 minutes later.
  - **Snooze** for 15 minutes, an hour, until tomorrow 9am, or until the item changes.
  - **Done** marks it handled; history is kept for 30 days, with Undo.
- **Plans:** one page per plan, with a dependency graph, kanban and timeline, and each package's PR, agent and branch.
- **Sessions:** the sessions you monitor, with their terminal app (iTerm2 or Terminal), why each is monitored, and Watch for the others.
- **Worktrees & cleanup:** which worktrees are safe to remove, and removal from the page after a confirmation.
- **Pull requests:** every open PR across your GitHub owners, and who it waits on.
- **Settings:** alerts (tab-title count, macOS notifications, chime, highlight), inbox timings, who merges each plan's PRs, and a light or dark theme.
- **In each Claude session:** a line above the prompt such as `search · 1/6 done · 2 ready · 1 needs you`. Clicking it opens the plan's page.

## Guides

- [The dashboard](docs/guides/dashboard.md): a tour of every page, what "needs you" means, and the keyboard shortcuts.
- [Writing and registering plans](docs/guides/plans.md): the plan format, registering a plan, and how work is matched to packages.
- [The plugin and its skills](docs/guides/plugin-and-skills.md): the mod's hooks and settings, the commands, the `plan_status` tool, the skills, and what the plugin sends to aimd.
- [Architecture](docs/guides/architecture.md): aimd, aim and the plugin; what aimd watches; where data lives; the security model.
- [The HTTP API](docs/api.md): every endpoint, for scripts and `curl`.
- [Contributing](CONTRIBUTING.md): building, testing, running a development copy, and sending a PR.

## Requirements

- macOS. aimd runs under launchd, and Go buttons and notifications use AppleScript.
- **Claude Code 2.1.319 or later**, with plugin mods. Check with `claude --version`.
- Go 1.21 or later, e.g. `brew install go`. `go.mod`'s `toolchain` line makes the go command download the exact version it needs (1.26.10) for this repo only, so your other projects keep their Go.
- `git`, `jq`, and the GitHub CLI `gh`, logged in (`gh auth status`) to an account that can read your repos.
- iTerm2 or Terminal.app, for the Go buttons.

## Install

Do this once per Mac.

**1. Get the code.** Clone it where you keep your projects. The plugin is read straight from this checkout, so keep it on `main`.

```sh
git clone https://github.com/gravexa-llc/ai-manager.git ~/Projects/ai-manager
cd ~/Projects/ai-manager
```

**2. Build and install `aim` and `aimd`** into any folder on your `PATH`:

```sh
make build && install bin/aim bin/aimd ~/.local/bin/
```

**3. Configure.** This is optional. Without a config file, aimd looks for repos under `~/Projects` and reads GitHub with `gh`'s active account. To change that, create `~/.ai-manager/config.toml`:

```toml
roots = ["~/Projects"]          # where your repos live

[owners]                        # GitHub owner → the gh account that reads it
my-org = "my-github-login"
```

Every setting, with its default, is listed at the top of `internal/config/config.go`.

**4. Start aimd.** This installs the launchd agent `com.gravexa.aimd`, so aimd starts at login and restarts if it stops:

```sh
aim up        # aimd is running at http://127.0.0.1:7777
aim status    # one line: what needs you, stale sources, GitHub budget, aimd's version
```

**5. Install the plugin** for every Claude Code session, and hand it aimd's token:

```sh
claude plugin install ai-manager --marketplace ~/Projects/ai-manager
jq -n --rawfile t ~/.ai-manager/token '{token: ($t | rtrimstr("\n"))}' \
  | claude plugin configure ai-manager@ai-manager --values-stdin
```

The token stays in `~/.ai-manager/token`, readable only by you. The command passes it to the plugin without printing it.

The plugin's other settings, and their defaults (`claude plugin configure ai-manager@ai-manager` lists them; `--values-stdin` changes the ones you give and keeps the rest):

| Setting | Default | What it does |
|---|---|---|
| `aimdUrl` | `http://127.0.0.1:7777` | Where aimd listens. |
| `band` | on | The plan line above the prompt, and the Register offer. |
| `appendConventions` | on | Adds the package, its branch name and the PR's `Plan:` line to each agent's brief. |
| `prCheck` | on | Asks a linked agent to add the `Plan:` line to its PR body. |
| `requirePlanRow` | off | Refuses to spawn an agent whose brief names a package its plan file has no row for yet, until you add the row. Off, the agent starts, the package shows on the board as provisional ("not in plan file"), and the lead is told to add the row. |

```sh
echo '{"requirePlanRow": "true"}' | claude plugin configure ai-manager@ai-manager --values-stdin
```

**6. Restart your Claude sessions.** New sessions load the plugin by themselves. In a session that's already running, type `/exit`, then `claude -c` to continue the same conversation. Background jobs pick it up the next time they start.

**7. Check it works.**
- Open http://127.0.0.1:7777/app/ and you should see the Overview.
- In a restarted session, `/aim` opens the dashboard.
- In a repo with a registered plan, the progress line appears above the prompt.

## Using it

### Choose what to monitor

ai-manager tracks only the sessions you opt into. Everything else sits under **Not monitored** on the Sessions page, and stays out of Needs you and notifications. A session is monitored when any of these is true:

- **It registered a plan** (see below). Its agents are monitored too.
- **It works on a package** of a registered plan.
- **You asked for it:** type `/aim watch` in the session, or click **Watch** on the Sessions page. `/aim unwatch` stops it.

PRs follow the same rule. A PR counts when it's on a registered plan or a monitored session's branch. A review request counts on any PR, because someone asked you by name.

### Plans

A plan is a markdown file in your repo with a table of work packages:

```markdown
Plan slug: search
Repos: example-org/recipes-api, example-org/recipes-web
Merges: you

| Package | What | Needs |
|---|---|---|
| **IDX1 Search index** | A full-text index on titles and ingredients. | — |
| **API1 Search endpoint** | `GET /search?q=` over the index. | IDX1 |
| **REL1 Release** | Ship search behind a flag. | API1; you: approve the rollout |
```

- **Package IDs** come from the bold prefix (`IDX1`, `API1`, …).
- **`Needs`** lists the IDs a package waits on. Other text there, such as "you: approve the rollout", is kept as a note.
- **A ✅ in a row** marks a package done.
- **`Plan slug:`** names the plan.
- **`Repos:`** lists the repos its packages live in.
- **`Merges: you | lead`** says who merges its PRs. Use `lead` when the lead session merges its agents' PRs, so ready PRs stay out of your Needs you. The default is `you`.

Ask Claude to "write a plan" and the plugin's `plan-write` skill follows this format. [Writing and registering plans](docs/guides/plans.md) has the whole format, lanes included.

**To register a plan:** save the plan file in a lead session and accept the plugin's offer above the prompt, or run `/plan-register [path]`. That session becomes the plan's lead and is monitored from then on.

**To unregister a plan:** open its page in the dashboard and, with no package selected, press **Unregister plan…**. ai-manager forgets the plan, its links and its history; the plan file stays in the repo, and registering it again starts afresh.

**Matching work to packages.** The plugin does this for you:
- each agent's brief starts with `Package: <slug> · <ID>`;
- each PR body carries a `Plan: <slug> · <ID>` line;
- branches are named `<slug>/<id>-<name>`.

ai-manager matches work to packages by these markers. It falls back to PR titles ending in `(ID)` and to branch names. Anything it can't match shows in the plan's **Unmatched** list, where you can link it by hand.

**Work for a package the plan doesn't have yet.** When a brief's `Package:` line, an open PR's `Plan:` line or a `<slug>/<id>-…` branch names an ID that isn't in a registered plan's tables, the work shows on the board anyway, as a **provisional** package marked "not in plan file". Its detail pane has a suggested row in the plan's own table format, with a Copy button. ai-manager never edits the plan file: add the row yourself (or let the lead), and the package becomes the plan's, with its links. The plugin tells the lead to add the row when it spawns such an agent, and if the row is still missing 10 minutes later, a "Plan row missing" item opens in Needs you. A provisional package whose work is all gone (no live agent, no worktree, no open or merged PR) disappears after a day.

Ask Claude "what's ready?" in a lead session; the plugin's `plan_status` tool answers from the dashboard.

### Needs you

- **Tabs:** Open, In progress, Snoozed and Done.
- **Detail pane:** select an item to see what it's waiting on (the question and its options, the permission's command, or the plan to approve), and where it is.
- **Go** switches to the session's existing tab in iTerm2 or Terminal. It's greyed out, with the reason on hover, where there's no tab to switch to. A background job has no tab, so its detail shows the `claude attach <id>` command instead.
- **Shortcuts:** press `?` in the dashboard. `j`/`k` move, `g` is Go, `s` snooze, `d` done, `u` undo, and `/` filters the list.

[The dashboard guide](docs/guides/dashboard.md) covers every page, and exactly what counts as needing you.

### Cleanup

Merged branches leave worktrees behind. A worktree is **safe to remove** when all of these hold:
- it has no uncommitted or untracked files, and no ignored files beyond rebuildable ones like `node_modules/`;
- no live session is using it;
- its PR is merged;
- it has no commits that aren't in that PR or its base.

There are two ways to remove them:
- **On the page:** **Worktrees & cleanup** → select → **Remove selected** → confirm. aimd checks each one again at that moment, and never forces.
- **From the terminal:**

  ```sh
  aim ls                # every worktree with its state and verdict
  aim prune             # what would be removed (dry run)
  aim prune --apply     # remove them
  ```

### Commands

| In a Claude session | Does |
|---|---|
| `/aim` | Opens this session's plan in the dashboard, or the dashboard itself |
| `/aim watch`, `/aim unwatch` | Monitors this session, or stops |
| `/plan-register [path]` | Registers a plan file |

| In a terminal | Does |
|---|---|
| `aim up`, `aim down` | Start aimd (and reload it after an update), or stop it |
| `aim status` | One line: what needs you, stale sources, GitHub budget, and the running aimd's version (with a note when it isn't this `aim`'s: run `aim up` to restart it) |
| `aim ls`, `aim prune [--apply]` | Worktrees and cleanup |
| `aim go <session>` | Brings a session's tab to the front |

## Updating

```sh
cd ~/Projects/ai-manager && git pull
make build && install bin/aim bin/aimd ~/.local/bin/
aim up                      # reloads aimd with the new build
```

Plugin changes reach each Claude session when it restarts.

## Uninstalling

```sh
aim down                                        # stops aimd and removes its launchd agent
claude plugin uninstall ai-manager@ai-manager
claude plugin marketplace remove ai-manager
rm -rf ~/.ai-manager                            # deletes aimd's history, settings and token
```

## Troubleshooting

- **"aimd not running".** Run `aim up`. Logs are in `~/.ai-manager/aimd.log`; startup crashes go to `~/.ai-manager/aimd.launchd.log`.
- **A session doesn't show up, or shows no detail.**
  - Make sure it was started or restarted after the plugin install, and that it's monitored (`/aim watch`).
  - `claude plugin configure ai-manager@ai-manager` should list the token as set.
- **Go is greyed out.** Hover over it for the reason: a background job with no tab, a session inside tmux, or an app other than iTerm2 or Terminal.
- **Clicking a macOS notification opens Script Editor.** That's a known limit of macOS's built-in notifications. Open the dashboard instead.
- **A background job is listed as expired.** Claude Code deleted its conversation after 30 days (`cleanupPeriodDays`), so it can't be reopened. Clear it with the `claude rm <id>` command shown on its row.

## Privacy and security

- aimd listens only on 127.0.0.1. Nothing is sent anywhere except read-only GitHub queries made with your own `gh` login.
- Writes to aimd need its token or the dashboard's per-page CSRF token.
- What a session is waiting on, such as a command, is stored only in memory, with secrets like tokens, passwords and auth headers redacted.
- Dashboard pages can be read by any program on your Mac, so don't expose port 7777 beyond your machine.

## Development

```sh
make build            # bin/aim and bin/aimd
make check            # gofmt, go vet, go test -race, and the plugin's validate and tests
```

There is no hosted CI: the maintainer runs `scripts/ci.sh <PR>` on their Mac, which runs `make check` on the PR merged into the latest `main` and posts the result as a commit status.

- [CONTRIBUTING.md](CONTRIBUTING.md): building, running a development aimd beside your real one, testing the plugin, and the PR process.
- [CLAUDE.md](CLAUDE.md): the code conventions, which Claude Code also reads when you work on this repo with it.
- [docs/api.md](docs/api.md): the HTTP API.

## License

MIT. See [LICENSE](LICENSE).
