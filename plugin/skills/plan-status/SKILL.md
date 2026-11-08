---
name: plan-status
description: Answers "what's ready?" from ai-manager's plan_status tool and turns it into the next wave of agents. Use when the user asks where a plan stands or what's next, and before spawning agents for plan packages.
---

# Plan status and the next wave

## Get the facts

Call the `plan_status` tool first, every time. It is listed as `mcp__ai-manager__plan_status`; in a repo with no plan registered at session start it waits behind ToolSearch, so load it there first. Its answer comes from aimd, which joins the plan file with GitHub PRs, CI and live agents: per plan, the summary (counts, `ready`, `needs_you_packages`, `unlocked`) and the board (every package with its lane, needs, lifecycle, `blocked_by`, badges and links; `unmatched`; `warnings`; `alerts`).

If the tool isn't available or it fails, say so: "ai-manager's plan_status tool isn't available. Is aimd running and the plugin loaded?" Then stop. Don't work the status out from the plan file, branches or memory, and don't dispatch without it: those miss merges, CI and who is waiting.

## Report it

Keep it short, in this order:

1. **Needs the user**, oldest first: agents or sessions waiting on a permission or a question; PRs that are green and mergeable, red or in conflict; `you:` steps; ready packages the user owns.
2. **Ready to dispatch**: the ready list.
3. **Blocked**: each package with the IDs it waits on.
4. **Working** and **in review**: ID, agent or PR, and badges such as quiet, stalled or collision.
5. **Done**: the count, and what merged since the last check.

Add parser warnings and unmatched branches or PRs in one line, if there are any. If the summary lists `provisional` packages (work names them, but the plan file has no row for them), say so and offer to add each one's `suggested_row` to the plan file.

## Pick the wave

- Start from the ready list. Leave out packages the user owns, and ask the user about a package's `you:` step before dispatching it.
- **single-writer lanes**: one agent at a time in such a lane. So at most one of its packages per wave (the first ready one in plan order), and none while another of its packages is working. If its rule also limits open PRs ("one open PR at a time"), a package in review counts too.
- Keep the wave small enough to review: 3 agents at most unless the user says otherwise, and fewer while PRs wait on the user.
- If the user only asked what's ready, propose the wave and wait. Dispatch when they ask.

## Brief each agent

**Every brief starts with `Package: <slug> · <ID>` on its own first line.** The ai-manager mod reads that line to link the agent to its package. Put the ID in the Agent `description` too (`T2 dark palette`). Spawn each agent in the background in its own worktree, unless the plan says otherwise. A new package gets its row in the plan file before its agent is spawned: without the row it shows only as provisional, and the mod reminds you to add it.

```text
Package: dark · T2

Build T2 (Dark palette) from docs/dark-plan.md: its row, the sections it names, and CLAUDE.md.
Branch dark/t2-<short-name>. PR title ends with (T2). PR body starts with: Plan: dark · T2
Don't merge.
```
