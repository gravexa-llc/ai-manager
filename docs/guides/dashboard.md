# The dashboard

aimd serves the dashboard at http://127.0.0.1:7777/app/, to a signed-in browser only. `aim open` opens it and signs your browser in for 30 days; after that a bookmark works. In a Claude session, `/aim` opens it at that session's plan, signing the browser in the same way. It updates live: each page listens to aimd's change stream and redraws only the parts that changed. Pages also refresh once a minute, so ages move on. Every selection is in the URL, so a reload or a bookmark lands on the same view.

The menu on the left:

| Entry | Shows | Its badge |
|---|---|---|
| **Overview** | Plans, running agents, the last day's activity, cleanup and health | |
| **Needs you** | Everything waiting on you | Open items |
| **Plans** | One entry per registered plan | That plan's Open items |
| **Sessions** | The sessions ai-manager monitors, and the others | Monitored sessions running |
| **Worktrees & cleanup** | Which worktrees can go | Worktrees safe to remove |
| **Pull requests** | Open PRs and who each waits on | Open PRs |
| **Settings** | Alerts, inbox timings, who merges, theme, time zone | |

The browser tab's title shows the Needs you count, as in "(3) aim".

### Sections and Group by

The lists on Needs you, Sessions, Worktrees & cleanup and Pull requests come in sections. Click a section's header to fold it or open it. The browser remembers what you folded on each page, and live updates leave it as you set it.

A section starts collapsed when nothing in it is on a plan or a monitored session: **Not on a plan**, **Not monitored**, and, grouped by repo, a repo with only stray work. Every other section starts open, and so does everything on Needs you, since all of it waits on you. The section holding the item you selected opens whatever you folded before, and so does every section with rows your search found.

**Group by**, above the list, picks the sections:

| Page | Choices |
|---|---|
| **Needs you** | **Plan** (the default), **Repo**, **Session** |
| **Worktrees & cleanup** | **Repo** (the default), **Plan**, **Cleanup status** |
| **Pull requests** | **Repo** (the default), **Plan** |

Your choice goes in the URL (`?group=plan`), so a link or a reload keeps it. The page also starts with it the next time you open it in this browser.

## What "needs you" means

Something needs you when nothing moves until you act. ai-manager counts four kinds of thing.

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

**Your step, ready:** a package of a registered plan is yours and everything it needs is done. It's yours when:

- its `Owner` is `you`: a step you do yourself, with no agent;
- or its `Needs` cell has a `you:` step, such as `you: sign off the rollout`: something you do before the package's agent can carry it out.

It's the same rule as the plan page's **needs you** badge, so the badge and the inbox always agree. The item closes by itself when the package is done (a ✅ in its row, **Mark done** on your own step, or, for an agent's package, its PR merging), when work on it begins, or when it's blocked again. A package [not in the plan file](#not-in-the-plan-file) yet is never your step: it has no row to say so.

These **don't** need you, and stay out of the inbox:

- **Sessions you don't monitor.** See [Sessions](#sessions) for what's monitored.
- **A red, conflicted or not-yet-approved PR.** It waits on its agent or, if no agent is working on it, on the plan's lead.
- **A ready PR on a plan that says `Merges: lead`.** It waits on the lead session. The plugin's line in that session counts it as "to merge".
- **Expired background jobs.** Claude Code deleted their conversation, so there's nothing to answer.
- **Your steps that aren't ready yet**, and your steps on a plan that isn't registered.

## Overview

The landing page has four cards:

- **Plan progress.** Each registered plan as a bar with one segment per package, colored by state, with "N of M done" and how many items wait on you. Click a plan to open it.
- **Agents running now.** The monitored sessions and their running subagents: which plan, what each is doing, the terminal app it runs in, and **Go** to switch to its tab. A note says how many other sessions aren't monitored.
- **Activity, last 24 hours.** PRs opened and merged, plans registered, packages done, links made by hand, and agents started and finished, newest first. **Show more** shows them all.
- **Cleanup & health.** Counts that link to their pages: worktrees safe to remove, git records whose folder is gone, stale draft PRs (no update in 30 days), and expired background jobs. Health chips show:
  - whether aimd is running, and for how long;
  - when sessions were last polled;
  - GitHub's remaining API budget, and how many repos GitHub couldn't read;
  - the plans and their parser warnings;
  - whether the plugin is reporting.

## Needs you

The inbox has one item per **episode**: a session that starts waiting, a PR that starts needing you, a plan row that stays missing, or your step that becomes ready. When the episode ends, because the session moved on, the PR merged or you marked the step done, aimd moves the item to Done by itself and says why in its history. A new wait is a new item.

### Tabs

- **Open:** waiting, and not yet handled. Items are grouped by plan, oldest first in each section. New items are highlighted until you open them.

**Group by** sorts the tab you're on into other sections. **Repo** gives one section per repo, then **Plans** for your steps whose plan row names no repo, then **No repo**. Your step goes under its row's `Repo`, named as the plan's `Repos:` line names it. **Session** gives one section per session, named as on the Sessions page, then **Pull requests** for the PRs (they come from no session) and **Plan files** for plan rows that are missing and your steps.
- **In progress:** you pressed Go. If the item still waits 10 minutes later, it goes back to Open.
- **Snoozed:** comes back at its time, or sooner when what it waits on changes.
- **Done:** handled, or no longer waiting. Kept for 30 days.

### Actions

- **Go** (`g`) switches to the session's terminal tab in iTerm2 or Terminal.app and moves the item to In progress. For a PR it reads **Open on GitHub**. For your step it reads **Open in plan**: it opens the package on its plan's page. Go is greyed out where there's no tab to switch to, with the reason on hover:
  - a background job: its detail shows the `claude attach <id>` command instead;
  - a session inside tmux, or in another terminal app;
  - a headless `claude -p` run.
- **Mark done**, on your own step (`Owner` you): the plan page's Mark done, for a step finished without a PR. It marks the package done on its plan (the plan file is unchanged), which closes the item, and its toast has **Undo**. It's greyed out where the plan page's is, such as on a package with a PR. A package that is your step only because of a `you:` step has no Mark done: it's an agent's package, finished when its PR merges or its row gets a ✅. When your part is done, dismiss its item with **Done**.
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
| Your step | A package that is yours is ready | The package's **What**, **Your part** (its `you:` step), and how long it has been ready, with **Open in plan**, and **Mark done** for your own steps (`Owner` you) |

Every kind counts in the Needs you badge, the tab title, the plan's badge in the menu, the Overview and the plan's progress line, and a new item is highlighted until you open it. Only permission prompts and questions chime, and only monitored sessions get a macOS notification (see [Settings](#settings)): a PR, a plan row or your step can wait for the next time you look.

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

A graph box's and a card's tooltip shows the package's **What** cell from the plan, as plain text.

### Timeline

One row per package:

- a solid blue bar while an agent worked on it;
- a thin purple bar while its PR waited in review, up to now;
- a green bar from start to finish once it's done.

Rows with no bar haven't started. Times are in the Time zone setting's zone, named under the rows ("Times in America/New_York").

### Not in the plan file

A **provisional** package is work that names an ID the plan file doesn't have yet. It's drawn dotted in the graph's last column, with a "not in plan file" badge on its card and row. Its detail pane says "Not in *file* yet" and gives a **suggested row** in the plan's own format, with a **Copy** button. aimd never edits the plan file: paste the row in, fill in the blank cells, and the package becomes the plan's. See [Writing and registering plans](plans.md#work-for-a-package-the-plan-doesnt-have-yet).

### A package's detail

- its lifecycle, title, size, lane and how long it has been in its state;
- right under the title, its **What** cell from the plan's package table (or its Description, Summary or Scope column), with the cell's `code`, **bold** and *italics*. A link shows as its text, and anything else, HTML included, as the text it is. A long cell shows four lines and **more**, which opens it whole (and **less** folds it again);
- its badges, each with the reason;
- **Needs** and **Unblocks**: the packages before and after it, with their states;
- **Work:** its PR (with CI and review state), agent, branch (or the suggested one), worktree, and who merges. A live agent gets a line like the one in Claude Code's agent list, such as `general-purpose  Bash: Run the tests  19m 30s · 284.1k tokens`: its type (or name), its last action, how long it has run, and its token count. Claude Code's own activity sentence isn't available to plugins, so the line shows the last action instead. The token count is the agent's latest model response: its input, cache and output tokens, about the size of its context now. It isn't a running total, and Claude Code counts it the same way. For an agent that is a whole session, it's the session's own loop's count. The Sessions page shows the same line;
- **Links**, folded: every link, how it was made (spawn, `Plan:` line, title, branch or by hand, and how sure), and **Unlink**.

Its actions:

- **Go to agent** (`g`) switches to the terminal tab of the agent working on it, or its lead's.
- **Go to lead** (`g`) takes its place when no agent on the package can be reached, for example when its agents have finished. It switches to the plan's lead session instead (see [Go to lead](#go-to-lead)), and its hint says why.
- **Mark done** (`d`) is for a step finished without a PR, such as your own step or an approval. It doesn't edit the plan file, and **Undo Mark done** (`u`) reverses it. A package whose PR will merge, or that has a ✅, can't be marked done.
- **Link a PR or agent…** links work by hand. Pick from the Unmatched list (the new ones, then the older ones; not the hidden ones) or a live session, or type a reference: `owner/repo#123`, a session id, `owner/repo:branch`, a worktree path or a cloud session URL. The picker starts on "Pick a PR, branch or session…", so nothing is picked until you pick it.

### The plan's own detail

With no package selected:

- progress;
- **Go to lead** (`g`): switches to the plan's lead session;
- **Ready to dispatch:** ready packages that aren't your own;
- **Needs you:** packages with a needs-you badge, and why;
- packages not in the plan file yet;
- **alerts:** dependencies unlocked, a stalled package, two agents in a single-writer lane, or an agent on a branch that matches no package;
- **parser warnings**, with line numbers;
- **Unmatched:** PRs and branches in the plan's repos that no package links (see [Unmatched](#unmatched) below);
- the plan file, its repos, and who merges.

### Unmatched

The plan's Unmatched list is about work since the plan began, so a busy repo's old PRs don't crowd it:

- **At the top:** PRs opened, and branches first seen in a worktree, after the plan was registered. A PR's time is GitHub's `createdAt` (or, for a PR aimd hasn't read yet, when it first stored it); a branch's is the first worktree scan that saw it.
- **Older (N),** folded: the ones from before the plan was registered.
- **Hidden (N),** folded: the ones you said aren't part of this plan, each with **Restore**.

Each entry says when it was opened or first seen, and has two controls:

- **Link** links it to the package you pick. The picker starts on "Pick a package…", so a stray click links nothing; a Link sent without a package is refused and says so.
- **Not part of this plan** hides it from this plan's list for good. It links and unlinks nothing, and other plans still list it. The toast has **Undo**, and **Restore** in Hidden puts it back any time.

The list's note above the views ("2 unmatched") counts only the new entries.

### Go to lead

A plan's lead is the session that registered it with `/plan-register`: the plan's main session. Go to lead switches to its terminal tab.

- A plan can have several leads. Go picks the one registered most recently, of those running in an iTerm2 or Terminal tab.
- A lead stays the lead after `/clear`.
- When there's no lead to go to, the button is greyed out and says why. On a package, the reason starts with why its agent can't be reached. The lead half is one of these:
  - no lead is registered: run `/plan-register <file>` in the plan's main session;
  - the lead isn't running;
  - the lead runs in tmux, in another app or as a background job, is a headless `claude -p` run, or has no terminal. Go only switches to iTerm2 and Terminal tabs.

aimd checks again when you press the button. If the agent or the lead can't be reached any more, or the button would now go to the other one, nothing happens and a message says why.

### Unregister

**Unregister plan…**, at the bottom of the plan's own detail, makes aimd forget the plan:

- its board, links, unlinks, Mark done, what you hid from Unmatched, and history;
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
- **Not monitored,** collapsed: every other session, each with **Watch**. These never reach Needs you or notifications.

Select a session or a subagent to see:

- **its agent line,** under its name: the same line a package's detail shows for its agent, such as `general-purpose  Bash: Run the tests  9m 38s · 335.9k tokens`. That's its type (`main` for a session's own loop), its last action, how long it has run, and its token count. See [A package's detail](#a-packages-detail) for what the count is. A session started with an older plugin has no count for its own loop; restarting it fixes that;
- for a monitored session, **Agents:** one line per live subagent, each opening that subagent's detail. Under it, **Finished (N)**, folded: the subagents that have ended, latest first (up to 20), with how each ended, how long it ran and its last token count;
- what it waits on, if anything;
- **why it's monitored**, such as "registered plan search" or "agent of API1";
- where it works: plan, package, folder and branch;
- when it started, as a clock time (the line says how long it has run);
- which plugin build it loaded, such as `v1.0.0-12-gabc1234`.

**Older plugin.** A session loads the plugin when it starts and keeps that build until it restarts, so a `git pull` in the plugin's checkout doesn't reach the sessions already running. aimd compares the build each session loaded with the build in its plugin folder now. A session whose build differs gets an **Older plugin** tag in its row and its detail, which also shows both builds; the list's summary counts them. Restart it (quit, then `claude --resume`) to load the new one. A session started before the plugin reported its build shows "Not reported" instead; restarting it fixes that too. See [the plugin guide](plugin-and-skills.md#what-it-reports-to-aimd) for how a build is named.

**Go** (`g`) brings its tab to the front. Go works for interactive sessions in iTerm2 and Terminal.app. For anything else it's greyed out, with the reason. The first Go may trigger macOS's prompt asking whether aimd may control the terminal app.

### Expired jobs

Claude Code deletes a conversation's transcript after `cleanupPeriodDays` (30 days by default), but `claude agents` keeps listing a background job whose conversation is gone, often as waiting for a reply. The page marks such a job **expired**. It never counts as needing you, and Go is off, since `claude attach` would open a blank tab. Its row shows the `claude rm <id>` command to remove it yourself. aimd never deletes a job.

To monitor every session without opting in, set `[sessions] monitor = "all"` in `~/.ai-manager/config.toml`.

## Worktrees & cleanup

Merged branches leave worktrees behind. This page lists every worktree aimd knows, with what can be done with it. Group by picks the sections:

- **Repo** (the default): one section per repo, by its GitHub name, or its folder's name for a repo with no GitHub remote. Repos with a worktree on a plan or a monitored session come first.
- **Plan:** one section per registered plan, then **No plan · watched** (a monitored session works in it), then **Not on a plan**. A worktree is on a plan when the plan links it, its branch or its branch's PR.
- **Cleanup status:** one section per verdict, as below.

Prunable records keep their own section in every grouping. Every row shows its verdict:

| Verdict | Meaning |
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

1. Tick the ones you want, or press **Select all safe**. That ticks the safe ones in folded sections too.
2. Press **Remove selected**.
3. Confirm in the dialog. It lists every worktree you ticked.

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

Group by picks the sections:

- **Repo** (the default): one section per repo. Repos with a PR on a plan or a monitored session's branch come first, then the rest by name.
- **Plan:** one section per registered plan, then **No plan · watched** (PRs on a monitored session's branch), then **Not on a plan**.

In each section, PRs that need you come first, then the most recently updated. A row leaves out what its section's header already says: its repo when grouped by repo, its plan when grouped by plan.

Each row has a chip saying who has the next move:

| Chip | Meaning |
|---|---|
| **Review requested** | Your review is requested. This needs you. |
| **Waiting on you** | Ready to merge, and the merge is yours. This needs you. |
| **Waiting on lead** | Ready to merge on a plan whose lead merges; or not ready, with no agent working on it, so the lead should send it back to one. |
| **Agent working** | An agent is still working on its package or branch. |
| **Snoozed** | It needs you, but you snoozed its inbox item. |
| **Ready** | Ready to merge, but not on a plan or a monitored session's branch, so nothing waits on it. |
| **Not read yet** | aimd knows only its URL: an agent linked it before a GitHub poll read it. It doesn't need you or wait on anyone until GitHub answers. If GitHub's last read failed, hovering over the chip says why. |
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

**Appearance.**

- **Theme:** **System** follows your Mac's light or dark appearance as it changes; **Dark** and **Light** force one.
- **Time zone:** the zone every clock time in the dashboard is written in, on every page: the Timeline's axis and bars, Needs you, the Overview, Sessions, Pull requests and the cleanup log. Its midnight also decides what "today", "tomorrow" and a date like "Nov 5" mean, and when "Until tomorrow 9am" ends. **System (Europe/Berlin)**, say, the default, is the zone of the Mac aimd runs on, as Date & Time sets it (aimd reads `/etc/localtime`, not `TZ`), and follows it if it changes. Or pick a zone from the menu (the common ones first, then every zone the Mac knows) and press **Set**. A zone the Mac can't load any more counts as System. Tooltips give the full time with its offset (`2026-11-05T09:00:00+01:00`); the Timeline says which zone it shows.

**Versions.** What this Mac runs: aimd's version and how long it has run, and the plugin build on disk in each folder live sessions loaded the plugin from, with how many of those sessions are on an older build. `aim status` prints the running aimd's version too, and says when it isn't the `aim` binary's own: after an update, `aim up` restarts aimd on the new one.

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
| `g` | Needs you | Go; Open on GitHub for a PR; Open in plan for your step |
| `s` | Needs you | Snooze |
| `d` | Needs you | Done |
| `u` | Needs you | Undo, or move a Done item back to Open |
| `g` | a plan | Go to the package's agent, or else to the plan's lead |
| `d` | a plan | Mark done |
| `u` | a plan | Undo Mark done, or the last change |
| `g` | Sessions | Go |
| `g` | Pull requests | Open on GitHub |
| `u` | while a toast shows | Its Undo |

When a key's button is greyed out, pressing the key shows the reason.
