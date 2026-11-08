# Pivot share links: delivery plan

Plan slug: sharing

Today a Pivot dashboard can only be opened by people signed in to the workspace that owns it.
Customers keep asking for a way to show one to someone outside: a client, a board member, an
analyst at a partner. This plan adds read-only share links, embeds for other sites, and PDF
snapshots sent on a schedule. The API work is in **pivot**, the pages people see are in **web**,
and the epics that hold it all are **pivot#58** and **pivot-web#21**.

Priya owns the product side and Theo the security side. Each package below is sized for one agent
and one pull request.

## What a shared dashboard shows

A share link never shows more than its sharer can see, and usually less:

| Audience | Sees | Gets there by |
|---|---|---|
| Anyone with the link | The tiles the sharer picked, with redacted fields blanked | A link to `/s/<token>` |
| A site that embeds it | The same tiles, sized to the frame | An `<iframe>` from the embed endpoint |
| A snapshot's recipients | A PDF of the tiles as they were at send time | A scheduled email |

Filters a viewer could change in the workspace are fixed in a shared view. Shared data refreshes
every five minutes at most, whatever the dashboard's own setting.

## Settled questions

| Question | Answer | Settled by |
|---|---|---|
| Can a link be taken back? | Yes, at once. Links also expire after 90 days unless renewed. | Theo |
| Who may share? | Editors and admins. A viewer can ask an editor. | Priya |
| Is the token in the URL enough? | For links, yes. Embeds also check the parent site against an allow list. | Theo |
| Which tiles can be shared? | All but raw-table tiles, which may hold personal data. | Priya and Theo |
| What do snapshots cost? | Nothing extra on Team; Starter gets 20 a month. | Priya |

## Work packages

### Stage 1: Tokens and the viewer

Nothing can be shared until this stage is done. It ships dark, behind the `share_links` flag.

| Package | What | Repo | Size | Needs |
|---|---|---|---|---|
| **G1 Redaction rules** | `docs/share/redaction.md`: which fields a shared view blanks (emails, names, free-text notes), how a blanked value looks, and what an admin may override. Every other package follows it. | pivot | S | Priya + Theo sign off |
| **A1 Share tokens** | `internal/share/token.go`: one signed token per link, scoped to a dashboard and a list of tiles, stored hashed. The dashboard handlers (about 40) go through one access check that takes a session or a token. Audit lines share one format, `"share\t%s · %s"` (token, dashboard), so the security log can be searched. | pivot | L | — |
| **A2 Token revocation** | Revoke now, expire after 90 days, renew; a revoked token answers 410. | pivot | S | A1 |
| **A3 Embed endpoint** | `/embed/<token>` serves the viewer in a frame, with `frame-ancestors` taken from the allow list. | pivot | S | W1 |
| **A4 Snapshot renderer** | A headless renderer that turns a dashboard into a PDF, run as a job with a 60-second limit. | pivot | M | — |
| **W1 Public viewer** | The `/s/<token>` page: the tiles, no navigation, a "Shared by" footer, and a plain page for a revoked link. | web | M | — |
| **W2 Share dialog** | The dialog on a dashboard: pick tiles, copy the link, see and revoke the links already made. | web | S | W1 |

Stage 1 is done when a link made on staging opens in a private window, shows only the tiles picked,
and stops working within a minute of being revoked.

### Stage 2: Embeds and snapshots

| Package | What | Repo | Size | Needs |
|---|---|---|---|---|
| **T1 Tile filters** | Each tile's query is filtered on the server for a shared view, so a redacted field never leaves the API. | pivot | M | A1, G1 |
| **V1 Email reports** | A weekly email to a dashboard's owner: how often each of its links was opened, and from where. | pivot | M | G1 |
| **W3 Embed guide** | `/docs/embeds/`: how to embed a dashboard, the allow list, sizing, and what a viewer sees. | web | M | W1, G1 |
| **S1 Scheduled snapshots** | Pick recipients and a schedule (daily, weekly or monthly); the renderer's PDF goes out by email. | pivot | M | A4, G1 |

### Stage 3: Checks before launch

| Package | What | Needs |
|---|---|---|
| **Q1 Load test** | 500 viewers on one link and 50 embeds on one page, against staging, with the five-minute refresh. | T1, V1 |
| **Q2 Leak check** | A script that opens every shared view and fails on any field the redaction rules blank. | T1 |
| **Q3 Pilot with two customers** | Two customers use links and embeds for two weeks; what they report goes into the backlog. | Q2, V1 |
| **Q4 Security review** | Theo reviews tokens, embeds, snapshots and the docs together. | everything above |

## Rollout

1. **R1 Flag on for the pilot**: `share_links` is turned on for the two pilot workspaces once the
   leak check passes.
2. **R2 Flag on for everyone**: after the pilot and the security review, with a changelog entry and
   a step in the in-app tour.
3. **R3 Close the epics**: pivot#58 and pivot-web#21 close after a week of traffic with no leak
   alerts.

## Agents and lanes

Each package becomes an issue under its epic. A lead session gives each package to one agent, in
its own worktree and branch off `release/2.4`, and each agent ends by opening a pull request with
the package's `Plan:` line. You review and merge; agents never merge, deploy or turn a flag on.

A plan that wants its lead to merge says so on lines of their own, as in the block below. This one
keeps the default:

```text
Merges: lead
Repos: acme/pivot, acme/pivot-web
```

| Lane | Packages, in order | Runs alongside |
|---|---|---|
| Access | A1 → A2, T1 → Q2 | everything; only one agent at a time edits the access schema |
| Email | V1 | everything, once G1 is approved |
| Viewer | W1 → W2, W3 → A3 | everything (separate repo) |
| Snapshots | A4 → S1 | everything |

G1 comes first. Once it is signed off the four lanes run side by side, one agent each. Q1, Q3, Q4
and the rollout wait until the lanes are done.

Shared files: `internal/share/schema.sql`, `internal/auth/check.go` and the web app's
`src/routes.ts`. Only the lane that owns a file edits it.
