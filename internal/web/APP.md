# The web app: conventions for its pages

The dashboard is the app under `/app/`: a shared shell, and one page per set of files in it. `GET /` redirects to `/app/` (the Overview), and the old pages' URLs redirect to their app pages (`redirects.go`).

## Who changes what

- **The shared shell** is `app.go`, `templates/app/shell.html`, `static/app-shell.css` and `static/app-shell.js`. Every page depends on it: keep page-specific code out of it.
- **A page owns its own files:**
  - `app_<page>.go`: its `appPage`, fill function, view types and form routes;
  - `templates/app/<page>.html`;
  - optionally `static/app-<page>.css` and `static/app-<page>.js`;
  - its tests, in `app_<page>_test.go`.
- If your page needs a shell change (a new component class, a key, a menu change), make it as a change of its own, separate from the page's.
- What the pages share besides the shell: the snapshot and formatting helpers (`dashboard.go`), a plan's board facts (`board.go`), the graph layout (`graph.go`), Go's rules and terminal tags (`golink.go`), the watch API (`monitor.go`), fresh card facts (`fresh.go`), an agent's line (`agent_line.go`, `templates/app/agent.html`; see *Shared partials*), sections that fold with Group by (`groups.go`, `static/app-groups.js`; see *Sections and Group by*), and the rule for your step that is ready (`yourstep.go`), which the plan page's "needs you" badge and the inbox's Your step items both follow.

| Page | Route | Files |
|---|---|---|
| Overview | `/app/` | `app_overview.go`, `overview.html` |
| Needs you | `/app/inbox` | `app_inbox.go`, `inbox.html` |
| A plan | `/app/plans/{slug}` | `app_plans.go`, `plan.html` (the What cell: `app_plans_what.go`; the Unmatched lane: `app_plans_unmatched.go`) |
| Sessions | `/app/sessions` | `app_sessions.go`, `sessions.html` (plugin builds: `app_plugin.go`) |
| Pull requests | `/app/prs` | `app_prs.go`, `prs.html` |
| Worktrees & cleanup | `/app/worktrees` | `app_worktrees.go`, `worktrees.html` |
| Settings | `/app/settings` | `app_settings.go`, `settings.html` (Versions: `app_plugin.go`) |

## A page

A page is an `appPage` in its own file. It is already listed in `appPages`, and `routeApp` registers its GET route.

```go
var appInbox = &appPage{
	pattern: "/app/inbox", name: "inbox", title: "Needs you",
	panes: 2, listLabel: "Needs you list", detailLabel: "Details",
	topics:    []string{"sessions", "prs"}, // what refreshes its panes
	fragments: []string{"inbox-tabs"},     // its own live regions, if any
	tmpl:      parseApp("inbox.html"),
	fill:      fillInbox,  // sets d.View, and d.Title / d.Summary if they vary
	routes:    inboxRoutes, // adds its POST routes, if any
	keys: []appKey{        // its own keys, as the ? sheet lists them
		{"g", "Go: switch to its terminal tab, or open the PR on GitHub"},
		{"s", "Snooze"},
	},
}
```

- **The `?` sheet** lists the shell's keys (`j`/`k`, `Enter` and `/` on a page with a list; `1`–`6`, `?`, `Esc` everywhere) and the page's `keys`, in the page's words: `g` is Go on the inbox and Open on GitHub on the PRs page. Declare every key your page binds with `data-key`. Without a `u` of its own, the sheet lists the toast's Undo.

- **The fill function** gets the request, the snapshot (`*snapshot`: sessions, worktrees, PRs, status) and `*appData`.
  - Set `d.View` to your own view type. Templates read it as `.View`. Give it display strings: templates have no functions of their own.
  - **Times.** `sn.now` is in the Time zone setting's zone (`sn.zone`; System is the Mac's, from `/etc/localtime`). Write a clock time with `clock(sn.now, t)`, a tooltip's full time with `stamp(sn.now, t)`, an age with `ago(sn.now, t)`, so every page shows the same zone and its days. A `datetime=` attribute stays UTC: `t.UTC().Format(time.RFC3339)`.
  - `d.Fragment` names the region asked for (`""` for the whole page), so you can skip work it doesn't show.
  - Return `errNoPage{"…"}` for a 404 (an unknown plan, say).
- **Monitoring (`internal/monitor`).** `sn.mon` says which sessions are monitored (`sn.mon.Monitored(row)`, `sn.mon.Why(row)`) and which PRs count for Needs you (`sn.mon.PR(p)`). A nil set monitors everything. Follow it wherever a page counts or lists what needs you. What a PR waits on is `sn.mon.PRWait(p)` (`internal/prwait`): `NeedsYou` is `review_requested`, `ready_to_merge` or empty, `WaitingOn` is you, lead, agent or none, and `Merges` and `Reason` go with them. Don't rebuild that rule in a page.
- **The menu badges** come from functions in the page files, so each page owns its own:
  - `inboxCount` (`app_inbox.go`) is the Needs you badge and the tab title's count: the inbox's Open items (`internal/inbox`), after a sync pass; without an inbox store, `needsYouTotal` (`dashboard.go`).
  - `planMenu` (`app_plans.go`) lists the plans and their needs-you counts: each plan summary's `NeedsYou`, which is the plan's Open inbox items, so the plan badges add up with the Needs you badge (items on no plan aside).
- **What needs you on a plan** is its summary (`s.opts.Plans.Summary`): `NeedsYou` and `NeedsYouItems` (its Open inbox items), `NeedsYouPackages` (their packages and reasons), and `ToMerge` / `ToMergePRs` (ready PRs waiting for a lead-merged plan's lead). Count from it rather than from badges, and expired background jobs never need you (`sn.expired`). A package that is your step and ready (`yourStep`) is one of those items: don't count it again from its badge.
  - `sessionCounts` (`app_sessions.go`): the monitored live sessions, headless runs aside.
  - `safeToRemoveCount` (`app_worktrees.go`).
  - `openPRCount` (`app_prs.go`): the open PRs.
  - `appSettingsFlags` (`app_settings.go`) holds the shell's settings: the tab title count, the chime, the keys and the theme, read from the Settings page's settings in the store. The shell draws them on `<html>` when a page loads, and the menu carries them (`data-flags`): a change in Settings publishes `inbox`, every open tab's menu refreshes, and the script copies the flags onto `<html>`. `s.settings(ctx)` gives every stored setting, defaults included.
- **Every selection is in the URL**: `?item=<id>`, `?tab=open`, `?view=graph&pkg=T1`, `?s=<key>`, `?group=plan`. Read them in fill, and render the selected row with `class="… sel" aria-current="true"`. A reload or a deep link must land on the same view.
- `q` (the header's filter, as a server-side search when the user presses Enter), `fragment` and `toast` are reserved query parameters.

## Template blocks

The shell (`templates/app/shell.html`) draws the menu, the header and the panes. Your file defines blocks:

| Block | When | What |
|---|---|---|
| `list` | `panes: 2` | The list pane (`<section id="list">`): toolbar, group headers, rows. |
| `detail` | `panes: 2` | The detail pane (`<aside id="detail">`). |
| `content` | `panes: 1` | The single content area (`<div id="content">`), e.g. cards (Overview, Settings). |
| `summary` | optional | The header's `<h1>` and summary line. The default shows `.Title` and `.Summary`. |
| `head` | optional | Extra `<head>` tags: your stylesheet or script, e.g. `<link rel="stylesheet" href="/static/app-inbox.css?v={{.AssetVersion}}">`. |

Any other `{{define}}` in your file is a partial, or one of the page's own fragments if it is listed in `fragments`.

**Shared partials.** A partial more than one page draws goes in a shared template file that `parseApp` gives every page (`appShared` in `app.go`), not in one page's file. `templates/app/agent.html` holds `agent-now`, an agent's line as Claude Code's agent list has it (who, what it did last or how it ended, how long it has run, its tokens). Build its data with `agentLine` or `endedLine` (`agent_line.go`); app-shell.css styles it (`.agent-now`). A package's pane and the Sessions page's detail both draw it, so the two can't drift.

## Sections and Group by

A grouped list (Needs you, Sessions, Worktrees & cleanup, Pull requests) draws each group as a section that folds:

```
{{template "group-open" .}}
<summary class="ghead"><span>{{.Label}}</span><span>{{len .Rows}}</span></summary>
<ul class="rows">…</ul>
</details>
```

- **The group** embeds a `listGroup` (`groups.go`): `Key`, unique on the page (`repo:acme/app`, `plan:search-v2`, `plan:-none`), and how it starts. `Open` is false when nothing in it is on a plan or a monitored session's ("Not on a plan", "Not monitored", a repo of strays). `Force` is true when it holds the selected row or `?q=` matched it. The shell's `group-open` partial draws `<details class="group" data-group="…">`, `open` when either holds, and `data-group-force` for Force. Without the script the browser folds it.
- **`static/app-groups.js`** (load it in your `head` block) remembers each section the user folds or opens, per browser: `localStorage`, under `aim-groups:<path>`, by key, the latest 200. It puts them back on load and after each swap. A forced section opens once for each URL; folded again, it stays folded until the URL changes. While the header's filter hides rows, the sections with rows left open.
- **Group by** is `?group=`. Call `groupBy(r, d, "<page>", choices)` in your fill, the default choice first. It returns the choice, the `?group=` your own links should carry on (`""` when the URL has none), and the control for the shell's `group-by` partial: links with `data-pane=""` and `data-group-by`. The choice is also the page's default next time, in the cookie `aim_group_<page>`. The script sets it when a Group by link is clicked; without the script, a whole page loaded with `?group=` sets it (`d.cookies`). A fragment never does, so one tab's live refresh doesn't change another's default.
- **The shell's `busy`** ignores an open `.group`: focus in a section doesn't hold the list's refresh, as it does in a menu.

## Fragments

`GET <page URL>&fragment=<id>` renders one region alone:
- `menu` or `summary`;
- `list` and `detail` (two panes), or `content` (one pane);
- one of your `fragments`.

The answer is the region's inner HTML, with a fresh CSRF token in the `X-AIM-CSRF` header. Any other id is a 404. The script uses this for pane links and for live refresh, so a block must render the same from the same URL, whether alone or in the page.

## Data attributes (`static/app-shell.js`)

| Attribute | On | Does |
|---|---|---|
| `data-pane="detail"` | `<a>`, an SVG `<a>` too | A click (or `Enter` on a focused SVG link) fetches the link's URL into the named regions (space-separated; empty means all panes) and pushes it to the history. Back and Forward work. It only applies to links to the same path; other links load normally. On an item row (`data-item`) it also marks the row selected (`sel`, `aria-current`), clears `new`, and keeps the scroll of the pane the row is in; the other regions start at the top. |
| `data-pane="ov-activity"` | `<a>` or `<form>` | A region name is any element id the page serves as a fragment: the panes (`list`, `detail`, `content`), `summary`, `menu`, or one of the page's own `fragments`. The Overview's cards are their own regions, so its "Show more" link loads only its card. |
| `data-pane="list detail"` | `<form>` | A GET form loads its URL into the panes. A POST form is sent with `fetch` and answered by the toast protocol. Without the script, both submit normally. |
| `data-live="sessions prs"` | an element with an `id` | When `/api/stream` reports one of these topics (`sessions`, `worktrees`, `prs`, `status`, `plans`, and `inbox` when the Needs you inbox changes), the region is re-fetched from the current URL, debounced (300 ms). It keeps scroll, focus, the j/k cursor and `<details id>` open state. It waits while the user types in it, has a menu open in it, or has a `<dialog>` open in it (a confirmation stays as it is until it closes). Every live region is also refreshed once a minute and after a reconnect, so ages ("4m") move on. The panes and summary already have the page's `topics`. Add your own regions only for parts that change on other topics. |
| `data-live=""` | an element with an `id` | No topics: the region refreshes only on the minute tick and after a reconnect. A live region inside a region that is due is refreshed with it, not on its own. The Overview's `#content` is one: page `topics` empty, each card its own `data-live` with its topics, so a change refreshes only its cards. |
| `data-item="<stable id>"` | an `<a>` or `<button>` row in the list, an SVG `<a>` too | `j`/`k` move a cursor over the visible items (not those the filter hides) and focus them. `Enter` opens one. The id lets the cursor and focus survive a refresh. |
| `data-key="g"` | a button or link | Pressing the key clicks it. The script looks in an open dialog only, else in the toast, `#detail`, `#content`, `#list`, then the rest of the page. If the element is `disabled` or `aria-disabled="true"`, the key shows a toast with its `data-off` or `title` instead (Go's reason). The shell binds `1`–`6` (menu), `?` (help sheet), `/` (the filter) and `u` (a toast's Undo, which wins while a toast shows). The spec's `g`, `s`, `d` and `u` are yours to bind; list them in your page's `keys`. |
| `data-open="<dialog id>"` / `data-close` | a button | Opens the `<dialog>` with `showModal()` / closes the dialog it is in. Esc and a click on the backdrop close it too. |
| `data-chime="<key>"` | anything | After a refresh, a key not seen since the page loaded rings the chime, if `<html data-chime="on">` (the Settings page's chime; on by default). Put it on the rows that should ring, such as new Open permission prompts and questions. |
| `data-search="<text>"` | a list row | More text the header's filter matches, such as a branch the row doesn't show. |
| `data-needs-count` | the menu (shell) | The tab title's count. The script keeps the title at "(N) aim". |
| `data-flags` | the menu (shell) | The shell's settings (`data-tab-count`, `data-chime`, `data-keys`, each `on` or `off`, and `data-theme`: `system`, `dark` or `light`). After each swap the script copies them onto `<html>` (`data-theme="dark"` or `"light"`, none for System), shows or hides the tab-title preview and the `?` sheet's hint, and redraws the title. |

**Scroll across swaps.** A swap keeps the scroll of every box inside the region that has an `id`, sideways too (the plan graph): give a scrolling box an id and it keeps its place through live refreshes and pane links.

**The header's filter.** On a page with a list, the header's box filters the list's rows as the user types: each row of a `ul.rows` or `ol.rows` (not in a dialog), and each `data-item` outside one (a plan's boxes, cards and timeline rows), by its text and its `data-search`, in any case. A group's `.ghead` goes with its last row, a graph's boxes dim instead of going, and "Nothing in this list matches" shows when nothing does. The filter holds across refreshes. `/` focuses it and `Esc` clears it. `Enter` submits it as `?q=`, the page's own server-side search, which may match more than the rows show; while the box holds the URL's `?q=`, the script hides nothing. A page with no list (Overview, Settings) has no box. Search across pages is not built.

A `<details class="dropdown">` with a `<summary>` and a `.dropdown-menu` is a menu (Snooze ▾). Esc or a click outside closes it.

Shortcuts are ignored while focus is in a text field, with a modifier key, or when `<html data-keys="off">`.

**Old anchors.** The old pages' `#anchors` come along with their redirects. The script sends Home's `#prs-needing-you` and `#home-prs` to `/app/prs`, `#home-sessions` to `/app/sessions`, `#home-cleanup` to `/app/worktrees`, and a board's `#pkg-<ID>` to the plan page's `?pkg=<ID>`.

### `window.AIM`, for page scripts

| Call | Does |
|---|---|
| `AIM.toast(text, {kind: "error", undo: {action, fields, label}, timeout})` | Shows a toast. |
| `AIM.navigate(href, "detail")` | What a `data-pane` link does. |
| `AIM.refresh("list detail")` | Re-fetches regions now. |
| `AIM.post(action, {field: value}, "list detail")` | What a `data-pane` POST form does. |
| `AIM.bind(key, fn)` | `fn(event)` runs on that key, before `data-key` and the built-ins. |
| `AIM.chime()` | Rings, if the chime is on. |
| `AIM.csrf()` | The current CSRF token. |

After every swap the region fires `aim:swap` (it bubbles; `event.detail.id` is the region). Listen on `document` to set up what you swapped in.

## Forms and the toast protocol

POST routes live under `/app/<page>/…`. Add them in your `routes` function:

```go
func inboxRoutes(s *Server) { s.mux.HandleFunc("POST /app/inbox/move", s.inboxMove) }
```

- **Sign-in.** Every page, fragment and form post needs the browser's session cookie (`requireSignIn`, `signin.go`); a handler never checks it itself. A fragment fetch that gets `401` reloads the page, which then shows the sign-in page.
- **CSRF.** `CSRF.Protect` checks every `/app/` write. Put `<input type="hidden" name="csrf_token" value="{{.CSRF}}">` in each form, and a `return` field when the answer should come back to the page (`appReturn(r)` reads it, limited to `/app/` paths). The script refreshes the token in every form it submits.
- **The answer.** Use one of the two helpers, never your own redirect:

```go
s.appReply(w, r, "/app/inbox?tab=open&item=7", &appToast{
	Text: "Moved to Done",
	Undo: &appUndo{Action: "/app/inbox/move", Fields: map[string]string{"item": "7", "to": "open"}},
})
s.appRefuse(w, r, http.StatusConflict, appReturn(r), "It stopped waiting meanwhile.")
```

- **From the script** (`Accept: application/json`), the answer is JSON:

```json
{"location": "/app/inbox?tab=open&item=7",
 "toast": {"text": "Moved to Done", "undo": {"action": "/app/inbox/move", "fields": {"item": "7", "to": "open"}}}}
```

  A refusal is `{"error": "…"}` with the status you gave. The script then:
  1. moves to `location` (replacing the history entry, or loading the page if it's another path);
  2. refreshes the form's `data-pane` panes, the menu and the summary;
  3. shows the toast. Undo posts `fields` to `action`, and that answer can carry its own toast ("Moved back to Open").
- **From a plain form**, the answer is a 303 to `location` with the toast in `?toast=`. The toast is sealed with the CSRF key, so a link can't forge one, and it expires after an hour. The page shows it with Undo as a form.
- **Bounds.** Toast text is cut at 300 bytes. An Undo must post to `/app/…`, with at most 8 fields of 200 bytes each, or it is dropped.
- **Go.** For Go, call `s.goTo(r, key)` from your own route (only when `s.opts.Go` is set) and answer with `appReply`. `POST /api/go` is for programs (`aim go`): JSON with the token.

## CSP: no inline code

Every response carries `Content-Security-Policy: default-src 'self'`. So:
- no `style=` attributes, `<style>`, inline `<script>`, `on…=` handlers or `javascript:` URLs (`TestAppTemplatesNeedNoInlineCode` reads every app template and page for them);
- colors and sizes are classes, and colors are tokens (below);
- for proportional widths, use one element per unit in a flex row (`.bar > span.lc-done`), SVG attributes, or set `el.style.width` from a `data-` attribute in your page script (CSSOM writes are allowed);
- fonts are the system stacks (`--sans`, `--mono`).

Templates never mark text safe: titles, branches and prompts are untrusted.

## Components (`static/app-shell.css`)

| Group | Classes |
|---|---|
| Layout | `.toolbar` (`.end` pushes right), `.content > .card` (cards wrap at 460px), `.actions` (a button row; forms inside lay out inline) |
| Buttons | `.btn`, `.btn.primary`, `.btn.sm`, `:disabled` / `aria-disabled`; `kbd` for hints (`Go <kbd>g</kbd>`) |
| Tabs and controls | `.tabs > .tab` (`.on`, `aria-current="page"` or `aria-pressed="true"`) with `.tcount`; `.seg` (segmented; `.on` or `aria-pressed`); `.gby` (Group by: a label and a small `.seg`); `.switch[role=switch][aria-checked] > .knob`; `.trow` with `.trow-title` and `.trow-desc` (a settings row) |
| Lists | `.ghead` (group header; also a `<button>` or `<summary>`); `details.group` (a section that folds, its `summary.ghead` with a chevron; it hides when the filter hid its rows); `.irow` (inbox row: `.kdot`, `.ibody > .ikind, .ititle, .isub`, `.iwait`); `.srow` (simple row); `.sel`, `.new`, `.cursor`, hover; `.rows` (a bare `<ul>`); `.empty` |
| Detail | `.pill`, `.tag`, `.badge` (`.needs`); `h2` and `.subtitle`; `.label` (section head); `dl.ctx`; `.code` (`.tool`); `.hint`; `.history`; `.agent-now` (an agent's line, the `agent-now` partial: `.agent-now-who`, `.agent-now-doing`, `.agent-now-quiet`, `.agent-now-stat`) |
| Overview | `.card`, `.bar`, `.legend > .sw`, `.dot`, `.feed > .when`, `.stats > .stat > strong + span` |
| Plans | `.node` (`.ntitle`, `.nid`, `.sel`), `.kcard`, `.khead` |
| Overlays | `<dialog class="dialog"><div class="dialog-body">` with `.dialog-head`, `.dialog-actions`; `.dropdown`; toasts are the shell's |
| Colors | Lifecycles `.lc-blocked`, `.lc-ready`, `.lc-working`, `.lc-in_review`, `.lc-done`, `.lc-dropped`. Item kinds `.kind-permission`, `.kind-question`, `.kind-plan`, `.kind-merge`, `.kind-review`, `.kind-reply`. Tones `.tone-ok`, `.tone-warn`, `.tone-info`, `.tone-lead`, `.tone-quiet`. Every color is a token (see *Colors and dark mode*). |

Each color class sets a palette (`--fill`, `--edge`, `--text`, `--bar`), and the components inside read it. For example:
- `<span class="pill lc-working">`;
- `<a class="irow kind-permission">`, where the `.kdot` and `.ikind` inside take the kind's colors;
- `<span class="tag tone-ok">` for a green health chip.

## Colors and dark mode

The app has a light and a dark theme. The Theme setting (Settings › Appearance) is System, Dark or Light. On System, `<html>` has no `data-theme`, and the stylesheet's `prefers-color-scheme` query follows macOS as it changes. Dark and Light are drawn by aimd as `<html data-theme="dark">` or `"light"`, so a page loads in its colors, and reach open tabs on the menu's `data-flags`.

- **Every color is a token.** A rule uses `var(--…)`, never a hex, `rgb()`, `hsl()` or a named color. The light values are on `:root`. The dark values are written twice, and the two copies must match:
  - under `@media (prefers-color-scheme: dark) { :root:not([data-theme="light"]) { … } }`, for System;
  - under `:root[data-theme="dark"] { … }`, for Dark.
- **The shell's tokens** are at the top of `app-shell.css`:
  - surfaces and lines: `--ground`, `--panel`, `--raised` (dialogs, menus), `--hover`, `--sel`, `--new`, `--line…`;
  - text: `--ink`, `--ink-2`, `--muted`;
  - accent: `--accent` is a fill under `--on-accent` text, `--accent-line` the focus ring, cursor, selection and highlighted graph edges, `--link` a link;
  - states: `--ok-…`, `--warn-…`, `--bad-ink`, `--need-ink`;
  - each lifecycle, kind and tone: `--lc-done-fill`, `-edge`, `-text`, `-bar`, which its class (`.lc-done`) maps to `--fill`, `--edge`, `--text` and `--bar`.
- **A page with colors of its own** adds the same three blocks to its own stylesheet, as `app-plan.css` does for the graph's edges and the badges.
- **SVG is colored by classes and CSS**, never by `fill=` or `stroke=` attributes; `none` and `currentColor` are fine.
- **The tests** (`app_theme_test.go`):
  - `TestAppColorsAreTokens` fails on a raw color outside the token blocks, and on an SVG color attribute in a template or a page;
  - `TestAppThemeBlocks` checks that the two dark blocks match and that every light color has a dark value;
  - `TestAppPaletteContrast` checks text at 4.5:1 against what it sits on, in both themes, and focus rings at 3:1. Add a pair there for a new token;
  - `TestAppPaletteLightness` checks that the lifecycle bars, and the item kinds' bars, are at least 8 L* apart, so they differ in lightness, not only in hue.

## Tests

- Test your page the way `app_test.go` does: `newApp(t, opt)` gives the seeded store, plans and verdicts; `get`, `section`, `wantIn` and `do` are shared helpers.
- Check that every selection renders from its URL, both whole and as each fragment.
- Check that your POST routes answer both JSON and plain forms.
- Don't touch the user's aimd or `~/.ai-manager`. Tests use temp stores.
