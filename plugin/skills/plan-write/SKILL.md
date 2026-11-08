---
name: plan-write
description: The format ai-manager parses for a phased dispatch plan (package table, Needs, lanes, slug, who merges). Use when drafting or editing a markdown plan of work packages to hand to agents.
---

# Writing a dispatch plan

ai-manager reads the plan's markdown file and turns it into a board. Only what follows these rules is tracked. Anything else is ignored or shows as a warning.

## Package table

- The header needs a `Package` column and a `Needs` column. Optional columns: `What`, `Repo`, `Size`, `Owner`, `Status`. Use `| Package | What | Repo | Size | Owner | Needs |` and drop the optional columns you don't use. Several tables (one per phase) are fine.
- The package cell starts with the ID and title in bold: `**API1 Search endpoint**`.
- An ID is 1 to 4 uppercase letters, an optional hyphen, a digit, then letters or digits (hyphen groups allowed), 10 characters at most: `A2`, `R0`, `B3b-2`, `SEC-1`. `v1`, `x86` and `ID` are not IDs. Each ID is used once in the whole file.
- **Every step that should be tracked is a table row**: release steps, approvals and the user's own steps too. Steps in a numbered list or in prose are invisible to the board.
- Never put the real table inside a code block or an HTML comment: those are ignored.
- The file counts as a plan once it has a package table with at least 2 IDs.

## Needs

- List IDs only, separated by commas: `A1, G1`. Use `—` for none.
- Free text never makes a dependency. Write "everything above" as the list of IDs. "A1 and A2" is a warning (missing comma), and so is an ID that isn't a package of this plan.
- Put human notes after the IDs, after a semicolon: `T1; after the mockups`.
- `you:` marks a step the user must take for a package that an agent carries out: `B3; you: approve the install`. It doesn't make the package the user's.

## Owner and done

- Owner `you` marks the user's own step: no agent, no PR. It completes when its row gets a ✅ (or **Mark done** on the dashboard).
- A ✅ anywhere in a row marks the package done (a `Status` of `done` or `merged` also works).

## Lanes

- A table with `| Lane | Packages | Rule |`. Packages lists IDs of this plan, separated by commas. A package belongs to one lane at most.
- When only one agent at a time may work in a lane (shared files, migrations, string catalogs), say so in the rule with the word `single-writer`: `**single-writer**: one agent at a time in Theme/`.

## Slug and repos

- The slug names the plan in PR lines (`Plan: <slug> · <ID>`) and branches (`<slug>/<id>-<short-name>`). It defaults to the file name minus `-plan` (`search-plan.md` gives `search`). Set it with a line `Plan slug: <slug>`.
- When packages land in more than one repo, add a line `Repos: owner/name, owner/name`, and use those repos' names in the Repo column.

## Who merges

- Add a line `Merges: you` or `Merges: lead`. Without it the plan merges `you`: a PR that is ready to merge shows in the user's Needs you. Write `Merges: lead` when the lead session reviews and merges its agents' PRs itself. A ready PR then waits on the lead, and only a review requested from the user reaches their Needs you. Any other value is a warning and reads as `you`.

## Example

```markdown
# Dark mode: dispatch plan

Plan slug: dark
Repos: example-org/app, example-org/web
Merges: you

## Lanes

| Lane | Packages | Rule |
|---|---|---|
| Theme | T1, T2 | **single-writer**: one agent at a time in `Theme/`. |
| Site | W1 | Parallel with the app. |

## Phase 1

| Package | What | Repo | Size | Owner | Needs |
|---|---|---|---|---|---|
| **T1 Color tokens** ✅ | Every hard-coded color moves into `Theme/Colors.swift`. | app | M | | — |
| **T2 Dark palette** | A dark value for each token. | app | S | | T1 |
| **W1 Site theme toggle** | A light / dark switch in the site header. | web | S | | — |
| **D1 Pick the accent** | Choose the dark-mode accent color. | | S | you | — |

## Phase 2

| Package | What | Needs |
|---|---|---|
| **Q1 Screenshot pass** | Every screen in dark mode, on the smallest phone. | T2, W1, D1; you: check on a real device |
| **R1 Release** | Ship 2.0 with dark mode, then the site toggle. | Q1; once the beta has run a week |
```

Once the plan is saved, the ai-manager band offers **Register**. `/plan-register <path>` does the same by hand.
