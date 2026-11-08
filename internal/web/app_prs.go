package web

// The Pull requests page (GET /app/prs).
//
//   - The list: the open PRs in sections that fold (groups.go), grouped by
//     ?group= (Group by, remembered as the page's default):
//     – repo (the default): one section per repo, as GitHub names it; the
//     repos with a PR on a plan or a monitored session's first, then by
//     name;
//     – plan: one section per registered plan, in the order they were
//     registered, headed by its title; then "No plan · watched", the PRs
//     on a monitored session's branch on no plan; then "Not on a plan".
//     A PR is on a plan when its verdict's plan (a board's link, or the
//     plan of the monitored session on its branch) or its Plan: line names
//     a registered plan. A PR is involved when it is on a plan or, in
//     "opt-in" mode, counts under internal/monitor's rules (sn.mon.PR: a
//     same-repo PR on a monitored session's branch). A section with no
//     involved PR starts collapsed ("Not on a plan", a repo of strays),
//     and the one holding the selected PR, or every one under a search,
//     opens. In a section, the PRs that need you come first, longest wait
//     first as Needs you orders them, then the others, most recently
//     updated first.
//   - Each row: repo and number, plan and package, title, a who-chip and
//     an age; the section's own repo or plan is left out of the row. The
//     chip comes from the PR's verdict (sn.mon.PRWait,
//     internal/prwait): Waiting on you (ready to merge, and you merge it),
//     Review requested (on any PR, a plan or not), Waiting on lead, Agent
//     working, Stale draft (an open draft not updated for 30 days), Ready
//     (ready to merge, but nobody waits on it: not counted), or Snoozed
//     (its Needs you item is snoozed in the inbox; prInbox). A PR with
//     none of these shows what keeps it from being ready ("CI failing")
//     as a plain tag. A PR aimd has never read from GitHub (store.PR.Unread:
//     linked by URL before a poll read it) shows Not read yet instead, its
//     age counted from when it was linked, and its detail says so in
//     State, with why GitHub didn't answer when its last read failed.
//   - The detail pane (?pr=<owner/repo#n>): state, CI, package, who
//     merges, updated, the verdict's reason (why it is or isn't in Needs
//     you) with a link to its inbox item, and Open on GitHub, a plain
//     link to the PR (g).
//
// A ?pr= naming a merged or closed PR still shows its detail.

import (
	"cmp"
	"context"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/inbox"
	"github.com/gravexa-llc/ai-manager/internal/prwait"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// appPRsPath is the page's path.
const appPRsPath = "/app/prs"

var appPRs = &appPage{
	pattern:     appPRsPath,
	name:        "prs",
	title:       "Pull requests",
	panes:       2,
	listLabel:   "Pull requests",
	detailLabel: "Pull request",
	// Plans link PRs to packages; sessions decide which branches are
	// monitored and whether an agent still works on a PR; the inbox says
	// which are snoozed.
	topics: []string{api.TopicPRs, api.TopicPlans, api.TopicSessions, api.TopicInbox},
	tmpl:   parseApp("prs.html"),
	fill:   fillAppPRs,
	keys:   []appKey{{"g", "Open the PR on GitHub"}},
}

// openPRCount is the Pull requests menu badge: the open PRs, drafts
// included, across every owner, which is what the page lists.
func openPRCount(sn *snapshot) int {
	n := 0
	for _, p := range sn.prs {
		if p.State == store.PROpen {
			n++
		}
	}
	return n
}

// prInbox returns the Needs you inbox's items for PRs that still wait,
// by lower-case "owner/repo#n": the page shows Snoozed for a snoozed one,
// and links each from the PR's detail. Without an inbox store, or when it
// can't be read (logged), there are none.
func (s *Server) prInbox(ctx context.Context) map[string]store.InboxItem {
	st, ok := s.inboxStore()
	if !ok {
		return nil
	}
	items, err := st.ListInbox(ctx, store.InboxFilter{Waiting: true})
	if err != nil {
		s.log.Error("pull requests page: list the inbox", "err", err)
		return nil
	}
	out := map[string]store.InboxItem{}
	for _, it := range items {
		if it.Source == inbox.SourcePR {
			out[strings.ToLower(it.Ref)] = it
		}
	}
	return out
}

// snoozedUntil says until when an inbox item is snoozed: "until 11:15",
// "until tomorrow 09:00" or "until it changes".
func snoozedUntil(now time.Time, it store.InboxItem) string {
	if it.DueAt.IsZero() {
		return "until it changes"
	}
	return "until " + dueText(now, it.DueAt)
}

// appPRsView is the page's data.
type appPRsView struct {
	// GroupBy is the list's Group by control.
	GroupBy groupControl
	Groups  []prItemGroup
	// Empty says why there are no groups.
	Empty string
	// Detail is the selected PR, or nil with DetailNote saying why.
	Detail     *prDetail
	DetailNote string
}

// prGroupChoices are the page's Group by choices, the default first.
var prGroupChoices = []groupChoice{{Value: groupRepo, Label: "Repo"}, {Value: groupPlan, Label: "Plan"}}

// prItemGroup is one section of the list.
type prItemGroup struct {
	listGroup
	Label string
	Rows  []prItem
}

// whoChip is a PR's who-chip: who it waits on.
type whoChip struct {
	// Label is the chip's text, Tone its color class, Title a tooltip.
	Label, Tone, Title string
	// Plain shows it as a tag, not a who-chip: what keeps a PR nobody
	// waits on from being ready.
	Plain bool
}

// prItem is one row.
type prItem struct {
	// Ref is "owner/repo#n": ?pr= and the row's data-item.
	Ref, Href string
	Repo      string
	Number    int
	// PlanPkg is "sharing · V1", or "not on a plan".
	PlanPkg string
	// Sub is the row's line over its title: repo, number, plan and
	// package, less what its section already says.
	Sub   string
	Title string
	Who   whoChip
	// Age is how long it has waited on you, or since its last update.
	Age, AgeTitle string
	Sel           bool
	// Search is more text the header's filter matches: the branch.
	Search string
}

// prDetail is the detail pane.
type prDetail struct {
	prItem
	URL, Branch, Author string
	State, CI           string
	// Pkg is the plan and package, PlanURL the plan's page in the app.
	Pkg, PlanURL string
	Merges       string
	// Updated is how long ago, UpdatedAt the exact time.
	Updated, UpdatedAt string
	// Why says why it is or isn't in Needs you, and InboxURL is its item
	// there, while it waits.
	Why, InboxURL string
}

func fillAppPRs(s *Server, r *http.Request, sn *snapshot, d *appData) error {
	ctx := r.Context()
	open, need := 0, 0
	for _, p := range sn.prs {
		if p.State != store.PROpen {
			continue
		}
		open++
		if sn.mon.PRWait(p).NeedsYou != "" {
			need++
		}
	}
	d.Summary = strconv.Itoa(open) + " open"
	if need > 0 {
		d.Summary += " · " + strconv.Itoa(need) + " waiting on you"
	}
	if d.Fragment == "summary" || d.Fragment == "menu" {
		return nil
	}
	b := prBuilder{s: s, sn: sn, query: strings.TrimSpace(d.Query), inbox: s.prInbox(ctx)}
	b.sel, b.selOK = parsePRRef(r.URL.Query().Get("pr"))
	v := &appPRsView{}
	b.group, b.kept, v.GroupBy = groupBy(r, d, "prs", prGroupChoices)
	if d.Fragment != "detail" {
		b.plans = s.registeredPlans(ctx, sn)
		b.list(v)
	}
	if d.Fragment != "list" {
		b.overrides = s.mergesOverrides(ctx)
		v.Detail, v.DetailNote = b.detail(r.URL.Query().Get("pr"))
	}
	d.View = v
	return nil
}

// prBuilder builds the page's rows and detail.
type prBuilder struct {
	s     *Server
	sn    *snapshot
	query string
	sel   store.PRKey
	selOK bool
	// inbox are the PRs' waiting inbox items (prInbox).
	inbox map[string]store.InboxItem
	// overrides are Settings' who-merges overrides (mergesOverrides), for
	// the detail's Merges.
	overrides map[string]string
	// group is the list's Group by, and kept the ?group= the rows' links
	// carry on ("" when the URL has none).
	group, kept string
	// plans are the registered plans, for Plan grouping.
	plans []api.Plan
}

// inboxItem is a PR's waiting inbox item, if it has one.
func (b prBuilder) inboxItem(p store.PR) (store.InboxItem, bool) {
	it, ok := b.inbox[strings.ToLower(store.PRRef(p.Repo, p.Number))]
	return it, ok
}

// parsePRRef reads ?pr=: "owner/repo#123".
func parsePRRef(v string) (store.PRKey, bool) {
	i := strings.LastIndex(v, "#")
	if i <= 0 || !strings.Contains(v[:i], "/") {
		return store.PRKey{}, false
	}
	n, err := strconv.Atoi(v[i+1:])
	if err != nil || n <= 0 {
		return store.PRKey{}, false
	}
	return store.PRKey{Repo: v[:i], Number: n}, true
}

// samePR reports whether p is the PR k names, the repo in any case.
func samePR(k store.PRKey, p store.PR) bool {
	return k.Number == p.Number && strings.EqualFold(k.Repo, p.Repo)
}

// planOf is the registered plan a PR is on (see the file comment): its
// index in b.plans, or -1.
func (b prBuilder) planOf(p store.PR, v prwait.Verdict) int {
	if i := planIndex(b.plans, v.Plan); i >= 0 {
		return i
	}
	return planIndex(b.plans, p.PlanSlug)
}

// prSection is the section a PR goes in under the page's Group by.
type prSection struct {
	key, label string
	// rank orders Plan grouping's sections: the plan's place, then the
	// watched and the rest.
	rank int
}

// sectionOf is a PR's section (see the file comment).
func (b prBuilder) sectionOf(p store.PR, v prwait.Verdict) prSection {
	if b.group != groupPlan {
		return prSection{key: repoKey(p.Repo), label: p.Repo}
	}
	if i := b.planOf(p, v); i >= 0 {
		return prSection{key: planKey(b.plans[i].Slug), label: cmp.Or(b.plans[i].Title, b.plans[i].Slug), rank: i}
	}
	if !b.sn.mon.All() && v.Monitored {
		return prSection{key: watchedKey, label: noPlanLabel, rank: len(b.plans)}
	}
	return prSection{key: notOnPlanKey, label: notOnPlanLabel, rank: len(b.plans) + 1}
}

// involved reports whether a PR is on a plan or a monitored session's
// (see the file comment): its section starts open.
func (b prBuilder) involved(p store.PR, v prwait.Verdict) bool {
	return b.planOf(p, v) >= 0 || (!b.sn.mon.All() && v.Monitored)
}

// list fills the sections.
func (b prBuilder) list(v *appPRsView) {
	type entry struct {
		item  prItem
		needs bool
		since time.Time
	}
	type section struct {
		prSection
		involved, sel bool
		entries       []entry
	}
	bySection := map[string]*section{}
	var order []*section
	any := false
	for _, p := range b.sn.prs {
		if p.State != store.PROpen {
			continue
		}
		any = true
		vd := b.sn.mon.PRWait(p)
		it := b.item(p, vd)
		if !b.matches(p, it) {
			continue
		}
		ps := b.sectionOf(p, vd)
		sec := bySection[ps.key]
		if sec == nil {
			sec = &section{prSection: ps}
			bySection[ps.key] = sec
			order = append(order, sec)
		}
		it.Sub = b.sub(it, ps)
		sec.involved = sec.involved || b.involved(p, vd)
		sec.sel = sec.sel || it.Sel
		sec.entries = append(sec.entries, entry{item: it, needs: vd.NeedsYou != "", since: waitSince(p, vd)})
	}
	slices.SortStableFunc(order, func(a, c *section) int {
		if b.group == groupPlan {
			return cmp.Compare(a.rank, c.rank)
		}
		return byInvolvedThenName(a.involved, a.label, c.involved, c.label)
	})
	sortEntries := func(es []entry) []prItem {
		slices.SortStableFunc(es, func(a, c entry) int {
			switch {
			case a.needs != c.needs:
				if a.needs {
					return -1
				}
				return 1
			case a.needs:
				if n := byWait(a.since, c.since); n != 0 {
					return n
				}
			default:
				if n := c.since.Compare(a.since); n != 0 {
					return n
				}
			}
			return cmp.Or(cmp.Compare(strings.ToLower(a.item.Repo), strings.ToLower(c.item.Repo)), cmp.Compare(c.item.Number, a.item.Number))
		})
		out := make([]prItem, len(es))
		for i, e := range es {
			out[i] = e.item
		}
		return out
	}
	for _, sec := range order {
		v.Groups = append(v.Groups, prItemGroup{
			listGroup: listGroup{Key: sec.key, Open: sec.involved, Force: sec.sel || b.query != ""},
			Label:     sec.label,
			Rows:      sortEntries(sec.entries),
		})
	}
	switch {
	case len(v.Groups) > 0:
	case any && b.query != "":
		v.Empty = "No open pull request matches “" + b.query + "”."
	default:
		v.Empty = "No open pull requests."
	}
}

// sub is a row's line over its title, less what its section says: under
// Repo grouping the repo, under Plan grouping the plan ("#7 · sharing ·
// V1", "o/app #7 · V1"). In Plan grouping's other sections the plan part
// is said by the header, unless it names a plan that isn't registered.
func (b prBuilder) sub(it prItem, ps prSection) string {
	num := "#" + strconv.Itoa(it.Number)
	if b.group != groupPlan {
		return num + " · " + it.PlanPkg
	}
	head := it.Repo + " " + num
	switch {
	case ps.key == watchedKey, it.PlanPkg == "not on a plan":
		return head
	case ps.key == notOnPlanKey:
		return head + " · " + it.PlanPkg
	}
	if _, pkg, ok := strings.Cut(it.PlanPkg, " · "); ok {
		return head + " · " + pkg
	}
	return head
}

// waitSince is when a PR started waiting on you: when aimd first saw your
// review request, else its last update (as Needs you counts it).
func waitSince(p store.PR, v prwait.Verdict) time.Time {
	if v.NeedsYou == prwait.ReviewRequested && !p.ReviewRequestedAt.IsZero() {
		return p.ReviewRequestedAt
	}
	return p.UpdatedAt
}

// matches reports whether a PR is shown for the search box.
func (b prBuilder) matches(p store.PR, it prItem) bool {
	if b.query == "" {
		return true
	}
	q := strings.ToLower(b.query)
	return slices.ContainsFunc([]string{it.Ref, it.Title, it.PlanPkg, it.Who.Label, p.HeadBranch, p.Author},
		func(t string) bool { return strings.Contains(strings.ToLower(t), q) })
}

// item is a PR's row.
func (b prBuilder) item(p store.PR, v prwait.Verdict) prItem {
	ref := store.PRRef(p.Repo, p.Number)
	q := url.Values{"pr": {ref}}
	if b.query != "" {
		q.Set("q", b.query)
	}
	if b.kept != "" {
		q.Set("group", b.kept)
	}
	it := prItem{
		Ref:     ref,
		Href:    appPRsPath + "?" + q.Encode(),
		Repo:    p.Repo,
		Number:  p.Number,
		PlanPkg: "not on a plan",
		Title:   p.Title,
		Who:     b.who(p, v),
		Sel:     b.selOK && samePR(b.sel, p),
		Search:  p.HeadBranch,
	}
	// The verdict's plan and package (a board's link), else the body's
	// Plan: line, which may name a plan that isn't registered.
	plan, pkg := v.Plan, v.Pkg
	if plan == "" || strings.EqualFold(plan, p.PlanSlug) {
		plan, pkg = cmp.Or(plan, p.PlanSlug), cmp.Or(pkg, p.PlanPkg)
	}
	switch {
	case plan != "" && pkg != "":
		it.PlanPkg = plan + " · " + pkg
	case plan != "":
		it.PlanPkg = plan
	case v.Monitored && !b.sn.mon.All():
		it.PlanPkg = "a monitored session's branch"
	}
	since := waitSince(p, v)
	it.Age = ago(b.sn.now, since)
	switch {
	case p.Unread():
		it.Age, it.AgeTitle = ago(b.sn.now, p.FirstSeenAt), "linked "+stamp(b.sn.now, p.FirstSeenAt)
	case v.NeedsYou == prwait.ReviewRequested && !p.ReviewRequestedAt.IsZero():
		it.AgeTitle = "review requested since " + stamp(b.sn.now, since)
	default:
		it.AgeTitle = "updated " + stamp(b.sn.now, since)
	}
	return it
}

// who is a PR's who-chip (see the file comment). A PR aimd has never read
// from GitHub gets "Not read yet", with why its repo couldn't be read when
// GitHub didn't answer.
func (b prBuilder) who(p store.PR, v prwait.Verdict) whoChip {
	c := whoChipOf(b.sn.now, p, v, b.snoozed(p))
	if p.Unread() {
		c.Title = notReadText(b.sn.status, p)
	}
	return c
}

// repoReadError is why GitHub's last read of a repo failed (the status's
// unreadable repos), or "" when it didn't.
func repoReadError(st api.Status, repo string) string {
	for _, e := range st.GitHub.Unreadable {
		if strings.EqualFold(e.Repo, repo) {
			return e.Error
		}
	}
	return ""
}

// notReadText says a PR hasn't been read from GitHub yet, with why when
// GitHub's last read of its repo failed.
func notReadText(st api.Status, p store.PR) string {
	if why := repoReadError(st, p.Repo); why != "" {
		return prwait.NotReadYet + ": the last read failed (" + why + ")"
	}
	return prwait.NotReadYet
}

// snoozed says until when a PR's inbox item is snoozed ("until 11:15"),
// or "" when it isn't.
func (b prBuilder) snoozed(p store.PR) string {
	if it, ok := b.inboxItem(p); ok && it.State == store.InboxSnoozed {
		return snoozedUntil(b.sn.now, it)
	}
	return ""
}

// whoChipOf is a PR's who-chip from its verdict, and, when its Needs you
// item is snoozed, until when ("" when it isn't).
func whoChipOf(now time.Time, p store.PR, v prwait.Verdict, snoozed string) whoChip {
	switch {
	case p.Unread():
		// Its OPEN is a guess: it never needs you or waits on anyone.
		return whoChip{Label: "Not read yet", Tone: "tone-quiet", Title: v.Reason}
	case v.NeedsYou != "" && snoozed != "":
		return whoChip{Label: "Snoozed", Tone: "tone-quiet", Title: "snoozed " + snoozed}
	case v.NeedsYou == prwait.ReviewRequested:
		return whoChip{Label: "Review requested", Tone: "tone-warn", Title: v.Reason}
	case v.NeedsYou == prwait.ReadyToMerge:
		return whoChip{Label: "Waiting on you", Tone: "tone-warn", Title: v.Reason}
	case v.WaitingOn == prwait.WaitingLead:
		return whoChip{Label: "Waiting on lead", Tone: "tone-lead", Title: v.Reason}
	case v.WaitingOn == prwait.WaitingAgent:
		return whoChip{Label: "Agent working", Tone: "tone-info", Title: v.Reason}
	case p.State != store.PROpen:
		return whoChip{Label: upperFirst(prStateLabel(p)), Tone: "tone-quiet", Plain: true}
	case staleDraft(now, p):
		return whoChip{Label: "Stale draft", Tone: "tone-quiet", Title: "a draft with no update for 30 days or more"}
	case prwait.IsReadyToMerge(p):
		return whoChip{Label: "Ready", Tone: "tone-quiet", Title: "ready to merge, but not on a plan or a monitored session's branch: nobody waits on it"}
	}
	return whoChip{Label: upperFirst(prwait.Blocker(p)), Plain: true}
}

// detail is the selected PR's detail, or nil and why.
func (b prBuilder) detail(raw string) (*prDetail, string) {
	if raw == "" {
		return nil, ""
	}
	if !b.selOK {
		return nil, "“" + raw + "” isn't a pull request: links name one as owner/repo#123."
	}
	i := slices.IndexFunc(b.sn.prs, func(p store.PR) bool { return samePR(b.sel, p) })
	if i < 0 {
		return nil, "aimd doesn't know " + raw + ": it isn't in a repo aimd reads, or GitHub hasn't been polled yet."
	}
	p := b.sn.prs[i]
	v := b.sn.mon.PRWait(p)
	d := &prDetail{
		prItem:    b.item(p, v),
		URL:       p.URL,
		Branch:    p.HeadBranch,
		Author:    p.Author,
		State:     prStateText(p),
		CI:        checksText(p.ChecksState),
		Pkg:       "—",
		Merges:    "—",
		UpdatedAt: stamp(b.sn.now, p.UpdatedAt),
	}
	d.Sel = true
	if !p.UpdatedAt.IsZero() {
		d.Updated = ago(b.sn.now, p.UpdatedAt) + " ago"
	}
	if p.Unread() {
		d.State, d.CI = upperFirst(notReadText(b.sn.status, p)), "—"
	}
	if d.PlanPkg != "not on a plan" {
		d.Pkg = d.PlanPkg
	}
	if v.Plan != "" {
		if pl := b.sn.board(v.Plan); pl != nil {
			d.PlanURL = appPlanURL(pl.Slug)
		}
	}
	// Settings' override is labelled as one; PRWait already follows it.
	from := "plan setting"
	if v.Plan != "" && b.overrides[strings.ToLower(v.Plan)] != "" {
		from = "override"
	}
	switch {
	case v.Merges == prwait.MergesLead:
		d.Merges = "Lead session (" + from + ")"
	case v.Merges == prwait.MergesYou && v.Plan != "":
		d.Merges = "You (" + from + ")"
	case v.Merges == prwait.MergesYou:
		d.Merges = "You (on no plan, so nobody else would)"
	}
	d.Why = needsYouWhy(v, b.snoozed(p))
	if it, ok := b.inboxItem(p); ok && v.NeedsYou != "" {
		d.InboxURL = InboxItemURL(it.ID)
	}
	return d, ""
}

// needsYouWhy says why a PR is or isn't in Needs you: the verdict's
// reason, in a sentence; snoozed is until when its item is snoozed, if it
// is.
func needsYouWhy(v prwait.Verdict, snoozed string) string {
	switch {
	case v.NeedsYou != "" && snoozed != "":
		return "In Needs you, snoozed " + snoozed + ": " + v.Reason + "."
	case v.NeedsYou == prwait.ReviewRequested && !v.Monitored:
		return "In Needs you: your review is requested. That counts on any PR, even one on no plan."
	case v.NeedsYou != "":
		return "In Needs you: " + v.Reason + "."
	}
	return "Not in Needs you: " + v.Reason + "."
}

// prStateText is a PR's state, with what keeps an open one from being
// ready to merge.
func prStateText(p store.PR) string {
	switch p.State {
	case store.PRMerged:
		return "Merged"
	case store.PRClosed:
		return "Closed without merging"
	}
	state := "Open"
	if p.Draft {
		state = "Draft"
	}
	if bl := prwait.Blocker(p); bl == "" {
		state += " · ready to merge"
	} else if bl != "draft" {
		state += " · " + bl
	}
	return state
}

// checksText is a PR's checks in a word or two.
func checksText(checks string) string {
	switch checks {
	case "":
		return "No checks"
	case "SUCCESS":
		return "Passing"
	case "FAILURE", "ERROR":
		return "Failing"
	case "PENDING", "EXPECTED":
		return "Running"
	}
	return upperFirst(strings.ToLower(checks))
}
