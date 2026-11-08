# The dashboard

aimd serves the dashboard at http://127.0.0.1:7777/app/. In a Claude session, `/aim` opens it at that session's plan. It updates live: each page listens to aimd's change stream and redraws only the parts that changed. Pages also refresh once a minute, so ages move on. Every selection is in the URL, so a reload or a bookmark lands on the same view.

The menu on the left:

| Entry | Shows | Its badge |
|---|---|---|
| **Overview** | Plans, running agents, the last day's activity, cleanup and health | |
| **Needs you** | Everything waiting on you | Open items |
| **Plans** | One entry per registered plan | That plan's Open items |
| **Sessions** | The sessions ai-manager monitors, and the others | Monitored sessions running |
| **Worktrees & cleanup** | Which worktrees can go | Worktrees safe to remove |
| **Pull requests** | Open PRs and who each waits on | Open PRs |
| **Settings** | Alerts, inbox timings, who merges, theme | |

The browser tab's title shows the Needs you count, as in "(3) aim".

## What "needs you" means

Something needs you when nothing moves until you act. ai-manager counts three kinds of thing.

**A monitored session waiting on you:**

- a **permission** prompt: it wants to run a command or edit a file, and asks first;
- a **question**: Claude asked you to choose between options;
- a **plan approval**: Claude finished a plan and waits for your go-ahead;
- a background job **waiting for your reply**: it ended its turn with a question for you;
- any other dialog open in the session ("Waiting on you").

**A pull request waiting on you:**

- **review requested:** someone asked for your review, on any PR, by anyone;
- **ready to merge**, when the merge is yours. That means all of these:
  - it's on a plan that says `Merges: you`, or on a monitored session's branch outside any plan;
  - it isn't a draft, its CI passed (or it has none), it has no conflict, and no review blocks it;
  - no agent is still working on it.

**A plan row missing:** an agent works on a package the plan file has no row for, and the row is still missing after 10 minutes.

These **don't** need you, and stay out of the inbox:

- **Sessions you don't monitor.** See [Sessions](#sessions) for what's monitored.
- **A red, conflicted or not-yet-approved PR.** It waits on its agent or, if no agent is working on it, on the plan's lead.
- **A ready PR on a plan that says `Merges: lead`.** It waits on the lead session. The plugin's line in that session counts it as "to merge".
- **Expired background jobs.** Claude Code deleted their conversation, so there's nothing to answer.

A plan's page has its own "Needs you" list, which also includes your own steps (`Owner` `you`) once they're ready. Those are reminders on the board. They don't create inbox items.

## Overview

The landing page has four cards:

- **Plan progress.** Each registered plan as a bar with one segment per package, colored by state, with "N of M done" and how many items wait on you. Click a plan to open it.
- **Agents running now.** The monitored sessions and their running subagents: which plan, what each is doing, the terminal app it runs in, and **Go** to switch to its tab. A note says how many other sessions aren't monitored.
- **Activity, last 24 hours.** PRs opened and merged, plans registered, packages done, links made by hand, and agents started and finished, newest first. **Show more** shows them all.
- **Cleanup & health.** Counts that link to their pages: worktrees safe to remove, git records whose folder is gone, stale draft PRs (no update in 30 days), and expired background jobs. Health chips show:
  - whether aimd is running, and for how long;
  - when sessions were last polled;
  - GitHub's remaining API budget;
  - the plans and their parser warnings;
  - whether the plugin is reporting.

## Needs you

The inbox has one item per **episode**: a session that starts waiting, or a PR that starts needing you. When the episode ends, because the session moved on or the PR merged, aimd moves the item to Done by itself and says why in its history. A new wait is a new item.

### Tabs

- **Open:** waiting, and not yet handled. Items are grouped by plan, oldest first. New items are highlighted until you open them.
- **In progress:** you pressed Go. If the item still waits 10 minutes later, it goes back to Open.
- **Snoozed:** comes back at its time, or sooner when what it waits on changes.
- **Done:** handled, or no longer waiting. Kept for 30 days.

### Actions

- **Go** (`g`) switches to the session's terminal tab in iTerm2 or Terminal.app and moves the item to In progress. For a PR it reads **Open on GitHub**. Go is greyed out where there's no tab to switch to, with the reason on hover:
  - a background job: its detail shows the `claude attach <id>` command instead;
  - a session inside tmux, or in another terminal app;
  - a headless `claude -p` run.
- **Snooze** (`s`): for 15 minutes, for an hour, until tomorrow 9am, or until it changes. "Until it changes" waits for something new: new commits, CI, a different question.
- **Done** (`d`): you've handled it.
- **Undo** (`u`): the toast after each move has Undo. In a Done item's detail, **Move back to Open** brings it back, and a Snoozed item has **Unsnooze**.

The Settings page sets how long Go waits before an item returns to Open (5, 10 or 30 minutes) and how long Done items are kept (7, 30 or 90 days).

### Item kinds

| Kind | What it waits on | Its detail shows |
|---|---|---|
| Permission | A session asks before running a tool | **Wants to run**: the tool and its command or path |
| Question | Claude asked you a question | **Asks**: each question and its options |
| Plan approval | Claude wants its plan approved | **Plan to approve**: the plan's first lines |
| Waiting for your reply | A background job ended its turn asking you something | Its last message, and `claude attach <id>` to open it |
| Waiting on you | Another dialog is open in the session | What aimd knows of it |
| Review requested | Someone asked for your review | The PR and why it needs you |
| Ready to merge | A PR is ready, and the merge is yours | The PR and why it needs you |
| Plan row missing | A package isn't in its plan file yet | The suggested row, with **Copy** |

### The detail pane

Select an item to see:

- **what it waits on**, as in the table above. You answer the session in its own terminal; the dashboard doesn't answer for you;
- **where it is:** plan, package, repo, branch, folder and PR, each linking to its page;
- **its history:** when it started waiting, each move you made, and what aimd did, such as "returned to Open" or "merged on GitHub".

What a session waits on comes from the plugin, with secrets such as tokens and passwords replaced by `[redacted]`. Without the plugin, the item shows the short reason `claude agents` gives, such as "permission prompt".

## Plans

Each registered plan has a page. The list pane shows the plan in one of three views. The detail pane shows the selected package, or the plan itself when none is selected.

### Graph

A dependency graph: each column is one step deeper in dependencies, and each arrow runs from a package to one that needs it. Boxes are colored by lifecycle and carry their most important badge. Select a box to see the package and highlight its links. Packages in a dependency cycle are marked. The graph opens scrolled to the right, where the current work usually is.

### Kanban

One column per lifecycle (blocked, ready, working, in review, done, dropped), each card with its lane and badges.

### Timeline

One row per package:

- a solid blue bar while an agent worked on it;
- a thin purple bar while its PR waited in review, up to now;
- a green bar from start to finish once it's done.

Rows with no bar haven't started.

### Not in the plan file

A **provisional** package is work that names an ID the plan file doesn't have yet. It's drawn dotted in the graph's last column, with a "not in plan file" badge on its card and row. Its detail pane says "Not in *file* yet" and gives a **suggested row** in the plan's own format, with a **Copy** button. aimd never edits the plan file: paste the row in, fill in the blank cells, and the package becomes the plan's. See [Writing and registering plans](plans.md#work-for-a-package-the-plan-doesnt-have-yet).

### A package's detail

- its lifecycle, title, size, lane and how long it has been in its state;
- its badges, each with the reason;
- **Needs** and **Unblocks**: the packages before and after it, with their states;
- **Work:** its PR (with CI and review state), agent, branch (or the suggested one), worktree, and who merges;
- **Links**, folded: every link, how it was made (spawn, `Plan:` line, title, branch or by hand, and how sure), and **Unlink**.

Its actions:

- **Go to agent** (`g`) switches to the terminal tab of the agent working on it, or its lead's.
- **Mark done** (`d`) is for a step finished without a PR, such as your own step or an approval. It doesn't edit the plan file, and **Undo Mark done** (`u`) reverses it. A package whose PR will merge, or that has a ✅, can't be marked done.
- **Link a PR or agent…** links work by hand. Pick from the Unmatched list or a live session, or type a reference: `owner/repo#123`, a session id, `owner/repo:branch`, a worktree path or a cloud session URL.

### The plan's own detail

With no package selected:

- progress;
- **Ready to dispatch:** ready packages that aren't your own;
- **Needs you:** packages with a needs-you badge, and why;
- packages not in the plan file yet;
- **alerts:** dependencies unlocked, a stalled package, two agents in a single-writer lane, or an agent on a branch that matches no package;
- **parser warnings**, with line numbers;
- **Unmatched:** PRs and branches in the plan's repos that no package links, each with a **Link** control;
- the plan file, its repos, and who merges.

### Unregister

**Unregister plan…**, at the bottom of the plan's own detail, makes aimd forget the plan:

- its board, links, unlinks, Mark done and history;
- its "who merges" override.

Its lead stops being monitored as a lead, unless you watch it. The plan file, branches, worktrees and PRs are untouched. There's no Undo. The dialog gives the `/plan-register <path>` command to register the file again, which starts afresh.

## Sessions

ai-manager only tracks the sessions you opt into. A session is **monitored** when:

- **it registered a plan.** It is that plan's lead until the plan is unregistered;
- **it works on a package** of a registered plan: an agent the lead spawned for it, or a session in the package's worktree or on its branch;
- **it's an agent of a monitored lead,** in a worktree Claude Code made for that lead;
- **you asked:** `/aim watch` in the session, or **Watch** on this page. `/aim unwatch`, or **Stop monitoring** in its detail, ends it.

A watch follows the session through `/clear` and `claude --resume`.

The page lists:

- **The monitored sessions, by plan:** a group per plan, then "No plan · watched". Each row shows the session's state (working, quiet, needs you, turn done, idle, error) and its terminal app: iTerm2, Terminal, tmux, another app, or none.
- **Not monitored (N),** collapsed: every other session, each with **Watch**. These never reach Needs you or notifications.

Select a session to see:

- what it waits on, if anything;
- **why it's monitored**, such as "registered plan search" or "agent of API1";
- where it works: plan, package, folder and branch;
- when it started, and its last action, such as `Bash: make check`.

**Go** (`g`) brings its tab to the front. Go works for interactive sessions in iTerm2 and Terminal.app. For anything else it's greyed out, with the reason. The first Go may trigger macOS's prompt asking whether aimd may control the terminal app.

### Expired jobs

Claude Code deletes a conversation's transcript after `cleanupPeriodDays` (30 days by default), but `claude agents` keeps listing a background job whose conversation is gone, often as waiting for a reply. The page marks such a job **expired**. It never counts as needing you, and Go is off, since `claude attach` would open a blank tab. Its row shows the `claude rm <id>` command to remove it yourself. aimd never deletes a job.

To monitor every session without opting in, set `[sessions] monitor = "all"` in `~/.ai-manager/config.toml`.

## Worktrees & cleanup

Merged branches leave worktrees behind. This page groups every worktree aimd knows by what can be done with it:

| Group | Meaning |
|---|---|
| **Safe to remove** | All four checks below pass. |
| **Scratch** | A detached worktree whose commit is already on the remote. Safe by rule. |
| **Not safe** | One check fails. The row says which. |
| **Locked** | Locked by git or by Claude Code for a running agent. Never removed. |
| **Prunable records** | git still lists the worktree, but its folder is gone. |

A branch worktree is **safe to remove** when:

1. it has no uncommitted or untracked files, and no ignored files except rebuildable ones such as `node_modules/`, `dist/` or `.venv/`;
2. no live session is working in it;
3. its PR is merged;
4. it has no commit that isn't in that PR or its base branch.

Anything aimd can't check, such as GitHub being unreachable, counts as not safe. Main checkouts are never candidates. A worktree whose PR was closed without merging is "not safe" here; only `aim prune --include-closed` removes it.

**To remove worktrees:**

1. Tick the ones you want, or press **Select all safe**.
2. Press **Remove selected**.
3. Confirm in the dialog.

aimd checks each one again first and skips any that is no longer safe. It removes each folder with `git worktree remove`, never forced, then deletes the branch with `git branch -D`. There's no Undo.

**Clear these records** runs `git worktree prune` for the prunable records. That clears only git's records; existing folders and branches stay.

The detail pane is the **removal log:** what was removed and which branch was deleted, and what was skipped and why. It's kept for 30 days.

The same rules are on the command line:

```sh
aim ls                       # every worktree, with its verdict and why
aim prune                    # what would be removed (a dry run)
aim prune --apply            # remove them, after asking
aim prune --repo recipes-api --apply --yes
```

The rebuildable list can be changed in `config.toml`:

```toml
[prune]
rebuildable = ["node_modules/", "dist/", ".venv/"]   # replaces the defaults
```

## Pull requests

Every open PR aimd knows about:

- your own open PRs across your GitHub owners;
- the PRs of branches checked out in your worktrees;
- the PRs of registered plans' repos;
- any PR that asks for your review.

They come in two groups: **On a plan or a monitored session**, then **Not on a plan**. In each group, PRs that need you come first, then the most recently updated.

Each row has a chip saying who has the next move:

| Chip | Meaning |
|---|---|
| **Review requested** | Your review is requested. This needs you. |
| **Waiting on you** | Ready to merge, and the merge is yours. This needs you. |
| **Waiting on lead** | Ready to merge on a plan whose lead merges; or not ready, with no agent working on it, so the lead should send it back to one. |
| **Agent working** | An agent is still working on its package or branch. |
| **Snoozed** | It needs you, but you snoozed its inbox item. |
| **Ready** | Ready to merge, but not on a plan or a monitored session's branch, so nothing waits on it. |
| **Stale draft** | A draft with no update in 30 days. |
| *a plain tag* | What keeps it from being ready, such as "CI failing" or "merge conflict". |

Select a PR to see:

- its state, CI, package and who merges it;
- when it was last updated;
- one line saying why it does or doesn't need you, such as "ready to merge · this plan's lead merges it" or "CI failing · an agent is still working on it".

**Open on GitHub** (`g`) opens it, and **Open in Needs you** goes to its inbox item.

## Settings

Settings are stored in aimd, not in a file, and apply to every open tab within seconds.

**Alerts**

| Setting | Default | What it does |
|---|---|---|
| Count in the tab title | on | The tab reads "(3) aim" while things wait on you. |
| macOS notifications | on | aimd's notifications. See below. |
| Chime | on | A short chime for new permission prompts and questions, while the Needs you page is open. |
| Highlight new items | on | New inbox items stay highlighted until you open them. |

aimd notifies only where Claude Code doesn't already:

- **at once,** when a background job starts waiting on you;
- **as a reminder,** when an interactive session has waited more than 2 minutes.

It notifies only for monitored sessions, and only for Open items. Clicking a notification opens Script Editor rather than the item, which is a limit of macOS's built-in notifications.

To turn notifications off for good, whatever this switch says, set `[notify] enabled = false` in `config.toml`.

**Needs you inbox**

- **After Go, back to Open if still waiting:** 5, 10 or 30 minutes.
- **Done history:** 7, 30 or 90 days.

**Who merges each plan's PRs.** Each plan's `Merges:` line decides whether a ready PR waits on you or on the lead session. Choose **You** or **Lead session** to override it without editing the file. **Reset to the plan's line** removes the override.

**Monitoring.** A reminder of what's monitored and how to watch a session.

**Shortcuts.** Turn the keyboard shortcuts on or off.

**Appearance.** Theme: **System** follows your Mac's light or dark appearance as it changes; **Dark** and **Light** force one.

## Keyboard shortcuts

Press `?` on any page for the list. Shortcuts work anywhere outside a text field, and can be turned off in Settings.

| Keys | Where | Does |
|---|---|---|
| `1` – `6` | everywhere | Overview, Needs you, the first plan, Sessions, Worktrees, Pull requests |
| `?` | everywhere | The shortcuts sheet |
| `Esc` | everywhere | Close a menu, dialog or the sheet; clear the filter |
| `j` / `k` | lists | Next / previous item |
| `Enter` | lists | Open the item |
| `/` | lists | Filter the list as you type. `Enter` runs it as a search. |
| `g` | Needs you | Go, or Open on GitHub for a PR |
| `s` | Needs you | Snooze |
| `d` | Needs you | Done |
| `u` | Needs you | Undo, or move a Done item back to Open |
| `g` | a plan | Go to the package's agent |
| `d` | a plan | Mark done |
| `u` | a plan | Undo Mark done, or the last change |
| `g` | Sessions | Go |
| `g` | Pull requests | Open on GitHub |
| `u` | while a toast shows | Its Undo |

When a key's button is greyed out, pressing the key shows the reason.
