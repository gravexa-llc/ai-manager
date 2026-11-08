# ai-manager: contributing

Notes for anyone changing this repo, human or Claude Code. `CONTRIBUTING.md` covers setup and running it locally; `docs/guides/` explains how it works.

## Layout
- `cmd/aimd`: the daemon. `cmd/aim`: the CLI (`aim ls`, `aim prune`, `aim up`, `aim down`, `aim status`, `aim go`).
- `internal/`: one package per job: `sessions`, `worktrees`, `github` (the sources), `store` (SQLite, migrations in `store/migrations/`), `plans` (the plan parser), `state` (matching work to packages), `planapi` and `web` (the HTTP API and the dashboard), `live` and `events` (what the mod reports), `config`.
- `plugin/`: the Claude Code plugin. `plugin/mod/` is the mod (TypeScript), `plugin/skills/` the skills.
- `deploy/`: the launchd plist template that `aim up` installs.
- `docs/api.md`: the HTTP API. `docs/guides/`: how it works. `testdata/`: recorded and hand-written fixtures.

## Code
- Go 1.26, module `github.com/gravexa-llc/ai-manager`. `go.mod`'s `toolchain` line picks a Go without the race detector's fork bug on the Mac; keep it, and don't set `GOTOOLCHAIN=local`.
- Prefer the standard library. SQLite is `modernc.org/sqlite` (no cgo). Explain any new dependency in the PR.
- Nothing personal in code: home paths, usernames, GitHub owners and repo roots come from `internal/config`. `AIM_HOME` overrides `~/.ai-manager`.
- The mod's API types are in the `plugin-authoring` skill's `types/claude-code.d.ts` (load the skill to find its path), and are laid into `plugin/.claude-plugin/types/` when the plugin is loaded with `--plugin-dir` in an interactive session.
- **Every mod hook fails open:** on any error it calls `next(e)` with the event unchanged, so a broken mod never blocks a session.

## Checks
- `make check` runs gofmt, `go vet`, the tests under the race detector, and `claude plugin validate` and `claude plugin test` when `claude` is on PATH. It must pass before you push. There is no hosted CI: `scripts/ci.sh <PR>` runs `make check` on a PR's merge result and posts a `local-ci` status.

## Tests
- Tests use temp dirs, temp git repos and `AIM_HOME`. They never touch the real `~/.ai-manager`, `~/.claude`, launchd, aimd or your repos and worktrees. `aim up` and `aim down` are tested with a fake `launchctl` (`cmd/aim/up_test.go`).
- Never run `aim prune --apply` against real worktrees while testing.
- Load the plugin with `--plugin-dir`, never at user scope, while you work on it.

## Fixtures (`testdata/`)
Scrub before committing: replace the home path with `~`, remove tokens, replace prompt, assistant and tool-output text with short placeholders, and use made-up owners, repos, branch names and session names (`acme/app`, `example-org/web`).

## Pull requests
- One change per PR, on its own branch. `make check` passes first.
- The PR body says what changed, how it was tested, and any open questions.
