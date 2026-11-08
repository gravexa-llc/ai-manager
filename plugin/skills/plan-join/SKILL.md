---
name: plan-join
description: Links this session's work to a package of an ai-manager plan when the lead didn't spawn it (a manual tab, an Agent View dispatch). Use when the user says this session works on a plan package, or before pushing or opening a PR for one.
---

# Joining a plan package

aimd links work to a package by three conventions. There is nothing to register and no API to call: follow all three.

1. **Find the slug and the ID.** The user names the package, or read the plan file. The slug is its `Plan slug:` line, otherwise the file name minus `-plan` (`search-plan.md` gives `search`). The ID is the first bold word of the package's row (`**API1 Search endpoint**` is `API1`). If it's unclear which package, ask. One package per session.
2. **Rename the branch** to `<slug>/<id>-<short-name>`, lowercase: `git branch -m search/api1-search-endpoint`. Do it before the first push. If the old name was already pushed, push the new one with `git push -u origin <new-name>` and leave the old remote branch for the user. If a PR is already open, keep the branch (the PR stays on the old name) and fix the PR with `gh pr edit` instead.
3. **PR body**: the first line is `Plan: <slug> · <ID>` (`Plan: search · API1`).
4. **PR title** ends with `(<ID>)`: `Search endpoint (API1)`.

The `Plan:` line is the exact link, and it outlives the branch and the worktree. The title and the branch name are the fallbacks.
