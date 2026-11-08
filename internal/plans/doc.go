// Package plans parses dispatch plans: markdown files whose tables list work
// packages and their dependencies. Parse reads a
// plan into packages, lanes and warnings; Detect cheaply tells whether a
// file looks like a plan at all.
//
// The rules below are the plan format. The plan-write skill teaches them.
//
// # Tables
//
// Plans use GFM pipe tables. Leading and trailing pipes are optional, and
// the delimiter row may carry alignment colons (|:---|:--:|). A literal pipe
// in a cell is written \|; a pipe inside an inline code span (`a | b`)
// doesn't split the cell either. A row with fewer cells than its header is
// padded with empty cells and extra cells are ignored; in package and lanes
// tables both are warnings. Tables inside fenced code blocks or HTML
// comments are ignored, and a row commented out on its own line is skipped
// without splitting its table.
//
// # Package tables
//
// A package table is found by its header row, which needs two columns
// (matched case-insensitively, bold allowed, optionally followed by a
// parenthesized remark such as "Needs (IDs)"):
//
//   - a package column: Package, Work package, Pkg, ID, Name, Task or Step;
//   - a dependency column: Needs, Depends on, Depends, Dependencies, Deps,
//     Requires, Blocked by or Prerequisites.
//
// A table without a dependency column is never a package table, whatever
// else it holds. Optional columns: What (or Description, Summary, Scope),
// Repo (Repository), Size (Estimate, Effort), Owner (Who, Assignee) and
// Status (State). Other columns are ignored.
//
// # Package IDs
//
// The package cell starts with a bold prefix whose first word is the ID and
// whose other words are the title: **A2 Conflict handling** is package A2,
// "Conflict handling". Also accepted: **A2** Conflict handling,
// **A2:** Conflict handling, __A2 Conflict handling__, a link around
// any of these, and a cell without bold that starts with the ID.
//
// An ID is one to four uppercase ASCII letters, an optional hyphen, then a
// digit, followed by letters and digits, optionally in hyphen-separated
// groups, at most 10 characters in all (IsID): G1, A2, R0, W3, B3b-2. A row
// whose package cell doesn't start with an ID is a warning. So is a second
// row with an ID already used (compared ignoring case); the first row wins.
//
// # Needs
//
// The Needs cell is split on commas and semicolons. Each part that is
// exactly an ID of this plan (ignoring emphasis, code markers and ✅) is a
// dependency. Everything else is kept, with its separators, as the note:
// "Sam + reviewer approve", "you: run the check". IDs inside prose are not
// dependencies: in "— (its interactive part is R1)" the R1 is a remark. A
// cell that is empty, a dash (-, – or —), "none" or "n/a" means no
// dependencies, and a dash before a remark is dropped from the note.
// Warnings: an ID-shaped part that isn't a package of this plan (kept in the
// note), a part that starts with a known ID but has more words, such as
// "A1 and A2" (a missing comma; kept in the note), and a package that needs
// itself. Write "everything above" as a list of IDs: free text never becomes
// a dependency.
//
// A clause that starts with "you:" ("B3; you: OK to install the launchd
// agent") is a step you take for a package that an agent or the lead carries
// out: an approval, a check, a token to paste. Its text up to the next
// semicolon is Package.NeedsYou, and it stays in the note. It does not make
// the package yours.
//
// # Owner
//
// An Owner column whose value is "you" marks your own step (Package.Yours):
// a package you do yourself, such as "Sam listens", which no agent or PR
// completes. It's done when its row gets a ✅ or you mark it done on the
// dashboard.
//
// # Done
//
// A package is done in the plan when its row has a ✅ anywhere, or its
// Status cell's first word is done, merged, complete or completed (any case),
// or the cell holds a ✅.
//
// # Lanes
//
// The lanes table is a table with a Lane column that isn't a package table.
// Each row is a lane: its name is the Lane cell; its packages are the IDs in
// the first column whose header mentions "package" ("Packages", "Packages,
// in order"), or in every other cell if there is no such column; its rule
// is the remaining cells, as written. A lane is single-writer when its row
// says "single-writer", "single writer" or "one agent at a time" (any case).
// A package belongs to the first lane that lists it; a second lane listing
// it is a warning, and so is a lane that lists an ID that isn't a package of
// this plan.
//
// # Title and detection
//
// The plan's title is its first level-1 heading (or its first heading, if it
// has no level-1 heading). Detect counts a file as a plan when it has at
// least one package table and at least MinPackages package IDs in all.
//
// # Slug
//
// A plan's slug names it in `Plan: <slug> · <ID>` lines and `<slug>/<id>-…`
// branch names. A line "Plan slug: x", or "plan slug `x`" anywhere in a
// line, sets it; otherwise it is the file name's stem minus a trailing
// "-plan" (sharing-plan.md is "sharing"). See Slug.
//
// # Repos
//
// A line "Repos: owner/name, owner/name" lists the GitHub repos the plan's
// packages live in, the plan's own repo included when packages live there.
// aimd registers the plan for those repos; without the line, for the repo
// the plan file is in. See Repos.
//
// # Merges
//
// A line "Merges: you" or "Merges: lead" says who merges the plan's PRs.
// With "you" (the default, also when there is no line) a PR that is ready
// to merge needs you. With "lead" the plan's lead session merges its
// agents' PRs, so a ready PR waits on the lead instead. Any other value is
// a warning and reads as "you". See Merges.
package plans
