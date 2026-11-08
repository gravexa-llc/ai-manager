// Package state ties work to plan packages and works out each package's
// state: the matcher, lifecycles, badges, the Unmatched lane and alerts.
//
// It is mostly pure. [Match] proposes links from a snapshot ([Input]), and
// [Derive] turns the snapshot and the matcher's answer into one
// [PlanState] per plan. [Load] reads the snapshot from the store and [Run]
// does a whole pass: load, match, store the matcher's links, derive, store
// the lifecycles that changed. aimd calls Run after each poll.
//
// # Repos
//
// A plan is registered for repos (store.Plan.Repos), each as GitHub's
// nameWithOwner or a main checkout's absolute path. Package IDs are only
// looked up in the plans registered for the repo of the PR or branch in
// question. A PR's repo is its nameWithOwner; a worktree's is its main
// checkout, named through Input.RepoNames when its GitHub name is known.
//
// # Matching (Match)
//
// Only same-repo PRs count (store.PR.IsCrossRepository false). Each one is
// matched by the first rule that applies:
//
//  1. Its Plan: line (exact, plan_line). The line names the plan, so it
//     settles the match: when it names a plan that isn't registered for
//     the PR's repo, or a package that plan doesn't have, the PR goes to
//     the Unmatched lane with a note, and no later rule is tried.
//  2. A trailing "(ID)" in its title (high, title).
//  3. Its head branch's name, as for a branch below.
//
// A branch with no PR (one checked out in a worktree) is matched by its
// name. A branch with a PR is matched through the PR. The name rules:
//
//   - "<slug>/<id>-…" (or "<slug>/<id>") is high confidence, for a plan
//     with that slug registered for the repo. A branch under a plan's
//     slug that names none of its packages matches nothing.
//   - Otherwise a package ID that appears as a token is low confidence: the
//     name is split into words at every non-alphanumeric character, and an
//     ID matches as its hyphen-separated groups, with or without its
//     leading letters split off (share/v1-email-reports names V1, surge-b-3b-2
//     names B3b-2). A low-confidence match only counts for a branch first
//     seen after the plan was registered (store.Plan.CreatedAt): tokens
//     like v1 are everywhere. A branch is first seen at the earliest of
//     its PRs' openings (GitHub's createdAt, store.PR.OpenedAt, which falls
//     back to when aimd first stored the PR) and its worktrees' first
//     sightings (store.Worktree.FirstSeen).
//
// A title ID or token that more than one package could mean (an ID two of
// the repo's plans share, or a branch naming two packages) matches
// nothing and goes to the Unmatched lane as "ambiguous": the Plan: line or
// a slug-prefixed branch names the plan. IDs compare ignoring case and the
// optional hyphen after their letters (SEC-1 is SEC1). A repo's base
// branches are never matched: its default branch as GitHub names it
// (Input.DefaultBranches; main, master, trunk and develop while that isn't
// known) and every branch some PR targets (unless it's a PR's head too, as
// in a stack).
//
// The matcher never proposes a link you removed (store.Unlink leaves a
// tombstone); such a target goes to the Unmatched lane with a note. A
// target with a spawn or manual link in a plan is settled there: the
// matcher proposes nothing else for it in that plan. Its links are stored
// with store.ReplaceInferredLinks.
//
// # Links reached through others (Derive)
//
// A package's direct links are its spawn and manual links plus the
// matcher's. From each, links reach further through the join graph: a PR
// and its head branch, a branch and the worktree that
// has it checked out, a worktree and the sessions and agents working in it
// (by the session's worktree or cwd, the agent's worktree, and the
// worktree's own agent id, session id and Claude Code lock: a locked
// worktree's agent is found through the lead's pid even without the mod).
// A reached link has the method and confidence of the direct link it came
// from, and Link.Via says what it came through. Nothing is reached through
// a main checkout or a base branch, since the lead and its
// non-isolated subagents work there; nor through a target you unlinked
// from the package, or one that is directly linked to another package.
//
// # Lifecycle
//
// The first that applies:
//
//  1. in_review: a linked PR is open.
//  2. done: a linked PR merged and none is open, ✅ in the plan, or Mark
//     done. A package owned by "you" is done only by ✅ or Mark done.
//  3. working: a linked agent is alive, or a linked worktree that still
//     exists has commits of its own (it is ahead of its upstream, or its
//     last commit is newer than the worktree).
//  4. dropped: every linked PR was closed without merging.
//  5. blocked: a package it needs isn't done.
//  6. ready: every package it needs is done.
//
// A dependency cycle longer than one is named on each member's card
// (PackageState.Cycle) and gets a plan warning, but needs no rule of its
// own: a member is blocked by the members it needs that aren't done, like
// any other package. Until one member is done (✅, Mark done or a merged
// PR) every member is blocked; once one is, the members that only needed
// it proceed normally.
//
// "Done" for the packages that need it is rule 2's condition, whatever the
// lifecycle shows: a ✅ package with a follow-up PR open is in_review but
// still unblocks what needs it.
//
// The lifecycle is stored with the time it last changed and the one
// before, so the dependencies-unlocked alert can list what went from
// blocked to ready since a given time.
//
// # Badges
//
// needs you (a linked live agent or session waits on you, with since
// when) · CI failing · conflict (DIRTY or CONFLICTING) · waiting on you
// (CLEAN and not a draft; no CI counts as green) · changes requested and
// review requested (only when reviewDecision is set) · draft · stalled
// (working with commits, no live agent and no open PR, the last commit
// older than Options.StalledAfter) · quiet (a working session the mod
// hasn't heard from for Options.QuietAfter, or one marked quiet) ·
// collision (two live agents) · lane conflict (two live agents in a
// single-writer lane) · low-confidence link · you: … (the Needs cell's
// `you:` step, until the package is done). PR badges are for open PRs. A
// session and a subagent of its own count as one agent.
//
// # Unmatched lane and alerts
//
// The Unmatched lane lists the same-repo PRs in the plan's repos that no
// package of any plan links (open ones, and ones closed after the plan
// was registered), then the branches without a PR that are checked out in
// a worktree and linked to nothing, each with the matcher's note, when it
// appeared (a PR's createdAt, a branch's first worktree sighting) and
// whether that was after the plan was registered (New). Targets you hid
// from the plan (Input.Hidden: "Not part of this plan") go to
// PlanState.Hidden instead; other plans still list them.
//
// Alerts: dependencies unlocked (since Options.UnlockedSince), each
// stalled package, each single-writer lane with two live agents, and each
// live agent in one of the plan's repos on a branch that matches no
// package (off-plan).
//
// # Provisional packages
//
// Work can name a package before the lead has added its row to the plan
// file. aimd never edits the file; it shows the work as a provisional
// package (store.Package.Provisional) until the row lands. Its sources
// are a spawn brief's "Package: <slug> · <ID>" line (the mod's spawn
// link, through the plan API), an open same-repo PR's Plan: line naming
// the plan and the ID, and a same-repo branch "<slug>/<id>-…" at work (a
// worktree and no PR yet, or an open PR); [Unknown] has the rules and
// what is left alone. [Run] stores what it finds (store.SeeProvisional)
// and passes again, so the matcher links the work to the new package by
// the rules above, exactly as it would a package of the file.
//
// A provisional package is on the board like any other (lifecycle,
// badges, links), with Provisional set, and never in ReadyToDispatch.
// When a re-parse of the file finds its ID, store.SyncPackages makes it
// the file's package: its links, Mark done and history stay. A provisional
// package with no work left (no live agent, no existing worktree, no open
// or merged PR) for ProvisionalQuiet, a day, is deleted with its links:
// one named only by a branch whose worktree was removed goes a day after,
// and one whose PR merged stays until the row lands.
package state
