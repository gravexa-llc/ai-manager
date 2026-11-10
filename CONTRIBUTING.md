# Contributing to ai-manager

Thanks for helping. This file covers:

- building and testing;
- running a development copy next to your real one;
- how pull requests get reviewed.

To learn the product first, start with the [README](README.md) and the [guides](README.md#guides). [`docs/guides/architecture.md`](docs/guides/architecture.md) is the best map of the code.

## What you need

- **macOS.** aimd runs under launchd, and the dashboard's Go button and notifications use AppleScript. Most of the code is portable, but nothing else is tested.
- **Go 1.21 or later.** `go.mod`'s `toolchain` line makes the go command fetch the exact version this repo uses (1.26.10) for this repo only.
- **Claude Code 2.1.319 or later**, on your `PATH`: `make check` runs its plugin checks.
- **`git`, and the GitHub CLI `gh`**, logged in, for running aimd against real repos.
- **[gitleaks](https://github.com/gitleaks/gitleaks)** (`brew install gitleaks`), for the credential scan in `make check` and the pre-commit hook.

## Build and check

```sh
make hooks        # once per clone: the pre-commit hook that keeps credentials out
make build        # bin/aim and bin/aimd
make check        # what CI runs: the credential scan, gofmt, go vet, go test -race, and the plugin checks
make go-check     # the Go part only, without the race detector: quicker while you work
```

`make check` runs:

1. `gitleaks` over the working tree, for credentials (`make secrets`);
2. a check that every Go file is `gofmt`-clean (it doesn't rewrite them: run `gofmt -w` yourself);
3. `go vet ./...`;
4. `go test -race ./...`;
5. `claude plugin validate plugin/` and `claude plugin validate .`, for the plugin and the marketplace;
6. `claude plugin test plugin/`, the mod's TypeScript tests.

Without `gitleaks` on your `PATH` it skips step 1, and without `claude` steps 5 and 6, and says so. CI doesn't skip them.

### Keeping credentials out

Three checks use the same rules, gitleaks' own plus `.gitleaks.toml`:

- **The pre-commit hook** (`scripts/hooks/pre-commit`) refuses a commit whose staged changes hold what looks like a credential. `make hooks` installs it, once per clone. It goes in the clone's shared hooks folder, so every worktree runs it, agents' worktrees included. Run `make hooks` again when the hook changes.
- **`make secrets`**, the first step of `make check`, scans the whole working tree. That covers a clone without the hook and a commit made with `--no-verify`.
- **`scripts/ci.sh`** runs the same scan itself on a PR's merge result, with `main`'s rules: a PR can change the `Makefile` and `.gitleaks.toml`, so its own `make check` isn't what vouches for it.

A test that needs something credential-shaped uses one of the made-up values `.gitleaks.toml` lists. They pass only in test files (`*_test.go`, `*.test.ts`). For a new one, add it there in the same PR, and say why: CI scans with `main`'s rules until the maintainer has read that change. Don't get past the hook with `--no-verify`.

gitleaks only knows credentials. It doesn't know names, paths or other projects: for those, see [Code conventions](#code-conventions).

Pass `make check` before you push.

## Repo layout

```text
cmd/aimd/            the daemon's main: flags, logging, wiring
cmd/aim/             the CLI: up, down, status, ls, prune, go
deploy/              the launchd plist template that `aim up` fills in
internal/            the Go packages (see the code map in docs/guides/architecture.md)
  store/migrations/  SQLite schema migrations, applied in order at start
  web/               the HTTP server, the API and the dashboard
    templates/app/   one template per dashboard page
    static/          the dashboard's CSS and JavaScript (no build step)
    APP.md           conventions for dashboard pages
plugin/              the Claude Code plugin: the mod (TypeScript) and the skills
.claude-plugin/      the marketplace manifest, so the plugin installs from a checkout
testdata/            fixtures: plans, scrubbed session listings and events, GitHub answers
scripts/ci.sh        the local CI script
docs/api.md          the HTTP API reference
docs/guides/         the user guides
```

## Running a development aimd

Your real aimd runs under launchd on port 7777, with its data in `~/.ai-manager`. Run a development copy beside it with its own home folder and port.

**1. Make a home folder and a config.** Use the resolved path (`pwd -P`). On macOS, temporary folders live under a symlink (`/var` is `/private/var`), and aimd compares real paths.

```sh
DEV=$(cd "$(mktemp -d -t aim-dev)" && pwd -P)
mkdir -p "$DEV/home" "$DEV/repos"
cat > "$DEV/home/config.toml" <<EOF
port = 7778
roots = ["$DEV/repos"]   # scan only your test repos, not all of ~/Projects

[notify]
enabled = false          # leave notifications to your real aimd
EOF
```

**2. Run it in the foreground.** `--debug` logs every poll.

```sh
make build
AIM_HOME="$DEV/home" bin/aimd --foreground --debug
```

**3. Use it.** It makes `$DEV/home/token` on its first start.

```sh
AIM_HOME="$DEV/home" bin/aim open     # opens http://127.0.0.1:7778/app/ and signs your browser in
AIM_HOME="$DEV/home" bin/aim status
curl -s -X POST -H "X-AIM-Token: $(cat "$DEV/home/token")" \
  http://127.0.0.1:7778/api/plans -d '{"path": "'"$DEV"'/repos/demo/docs/demo-plan.md"}'
```

Put test repos in `$DEV/repos`: `git init` one, commit, and add a plan file. [`docs/guides/plans.md`](docs/guides/plans.md) has an example plan to start from.

**What a development aimd still touches:**

- **Your sessions, read-only.** It lists your real Claude Code sessions with `claude agents --json`, and scans the repos they run in.
- **GitHub, read-only,** with your `gh` login, and from the same rate-limit budget as your real aimd.

**Never with a development build or a temporary `AIM_HOME`:**

- **`aim up` or `aim down`.** There's one launchd agent, `com.gravexa.aimd`, so either would replace or stop your real aimd. Run `bin/aimd --foreground` instead.
- **`aim prune --apply`, or Remove on the dashboard, against your real worktrees.** Try cleanup on throwaway repos in `$DEV/repos`.

## Testing the plugin

**Unit tests** need no aimd:

```sh
claude plugin test plugin/
```

They drive the mod's hooks with a fake engine (`plugin/mod/testkit.tsx`) and fake aimd answers (`plugin/mod/fixtures.ts`). Add a `*.test.ts` file next to the code you change.

**A live session** loads the plugin straight from your working copy. Start it in a test repo, not in a real project:

```sh
cd "$DEV/repos/demo"
claude --plugin-dir /path/to/your/ai-manager/plugin
```

- **It loads in place of your installed copy,** for that session only, as `ai-manager@inline`.
- **It has its own settings.** In the session, run `/plugin configure ai-manager@inline`:
  - set `aimdUrl` to `http://127.0.0.1:7778`;
  - set `token` to the contents of `$DEV/home/token`;
  - restart the session.

  Unconfigured, it talks to the aimd on port 7777, your real one, without a token: it can read, but its writes are refused.
- **The mod's API types** are laid into `plugin/.claude-plugin/types/` (git-ignored) in an interactive session, for your editor. A `claude -p` run doesn't lay them out, and draws no band.
- **Keep your installed checkout on `main`.** The installed plugin loads in place from the checkout you installed it from. Work in a separate clone or worktree, so your everyday sessions keep running the released plugin.

[`docs/guides/plugin-and-skills.md`](docs/guides/plugin-and-skills.md) explains the mod's hooks, its settings and how skills are built.

## Pull requests

Forks are welcome. To send a change:

1. **Fork the repo and branch from `main`.** Keep one change per PR.
2. **Run `make check`,** and push to your fork.
3. **Open a PR against `main`.** Its body has three sections:
   - **What changed:** what and why;
   - **How it was tested:** the commands you ran, and anything you tried by hand in a development aimd or a `--plugin-dir` session;
   - **Open questions:** anything you weren't sure of, or left out on purpose.
4. **Expect a review before checks run.** There's no hosted CI. The maintainer runs `scripts/ci.sh <PR>` on their own Mac, which:
   - merges the PR into the latest `main` in a temporary worktree;
   - scans the result for credentials itself, with `main`'s `.gitleaks.toml` rather than the PR's, before any of the PR's code runs. A PR that changes that file is called out, and the maintainer reads the change before scanning with it;
   - runs `make check` there, with a scrubbed environment and a throwaway `HOME`;
   - posts the result to the PR's head commit as the `local-ci` status.

   Because that runs your code on the maintainer's machine, it only happens after a review. The script runs only branches of this repo, so the maintainer brings a fork's commits in to check them.

Small fixes to docs and typos are just as welcome as code.

## Code conventions

The short version. [`CLAUDE.md`](CLAUDE.md) has the rest, and it's what Claude Code reads if you work on this repo with it.

- **Go 1.26,** module `github.com/gravexa-llc/ai-manager`. Prefer the standard library. SQLite is `modernc.org/sqlite`, pure Go with no cgo. Explain any new dependency in your PR.
- **Nothing personal in code.** Home paths, usernames, GitHub owners and repo roots come from `internal/config`. `AIM_HOME` overrides `~/.ai-manager`.
- **Tests never touch the real machine.** That means:
  - use temp dirs, temp git repos and a temp `AIM_HOME`;
  - never use the real `~/.ai-manager`, `~/.claude`, launchd, or your real repos and worktrees;
  - packages whose tests run git use `testenv.Main` in their `TestMain`.
- **Tests, fixtures and docs use made-up data,** like `acme/app` and `example-org/web`, never the names, ids, paths or output of your own sessions and repos.
- **Recorded fixtures in `testdata/` are scrubbed:**
  - the home path replaced with `~`;
  - no tokens;
  - prompts, Claude's answers and tool output replaced with short placeholders.
- **The plugin fails open.** Every hook ends in `.catch(($, e, next) => next(e))`, and nothing waits on aimd for more than its 1 s timeout. On any error the session carries on as if the plugin weren't there.
- **The plugin sends summaries only.** Any new detail it sends needs redaction in both `plugin/mod/wait.ts` and `internal/events/redact.go`.
- **Dashboard pages follow [`internal/web/APP.md`](internal/web/APP.md):**
  - no inline script or style, since the pages run under a strict Content Security Policy;
  - colors as theme tokens, so dark mode works;
  - every selection in the URL;
  - writes as forms that answer with a toast and an Undo.
- **Schema changes are new migrations** in `internal/store/migrations/`. Never edit one that has shipped.
- **Docs move with the code:**
  - an API change updates [`docs/api.md`](docs/api.md);
  - a plan-format change updates the parser's `doc.go`, the `plan-write` skill and its test, and [`docs/guides/plans.md`](docs/guides/plans.md);
  - anything you'd see on a page updates [`docs/guides/dashboard.md`](docs/guides/dashboard.md).
