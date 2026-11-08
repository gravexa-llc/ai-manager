package web

// A plan's page (GET /app/plans/{slug}).
//
// The list pane shows the plan in one of three views, chosen by ?view=:
//
//   - graph (the default): the dependency graph, drawn as SVG from
//     graph.go's layout. Columns are dependency depth, an edge runs
//     from each package a package needs to it, and boxes take their
//     lifecycle's colors. The selected package's edges are highlighted.
//   - kanban: a column per lifecycle, each card with its package's lane.
//   - timeline: per package, a bar while an agent worked on it, then a
//     thinner one while it waited in review, up to now. It is read from the
//     plan's stored history, the plan service's Timeline.
//
// Nodes, cards and rows carry badges (planBadgesOf): needs you, waiting on
// you, review requested, waiting on lead, snoozed and working.
//
// The detail pane shows the package ?pkg= names: its lifecycle and ID, its
// What cell (app_plans_what.go), what it needs and unblocks, its PR, agent (a live one with its line as Claude
// Code's agent list has it: pkgAgentOf) and branch, and Go to agent (Go to
// lead when no linked agent can be reached), Mark done and Link. With
// nothing selected it shows the plan: progress, Go to lead, ready to
// dispatch, what needs you, alerts, the parser warnings, the Unmatched
// lane (app_plans_unmatched.go: new, older and hidden), its file and
// Unregister. ?q= (the search box) dims the packages it
// doesn't match.
//
// Everything comes from the package's shared parts: the board (the plan
// service, internal/planapi, with its links refreshed as fresh.go does),
// the graph layout (graph.go), the plan's history (Timeline), the card
// facts and the Unmatched lane (board.go), Go's rules (golink.go) and what
// a PR waits on (sn.mon.PRWait, internal/prwait). Agents linked to a
// registered plan's packages and the PRs its board links are monitored by
// definition (internal/monitor), so the page needs no monitoring filter of
// its own.
//
// # Forms
//
// They answer with the toast protocol (app.go):
//
//   - POST /app/plans/{slug}/done    pkg, done=1|0: Mark done, with an Undo,
//     or its undo (the plan service's SetDone);
//   - POST /app/plans/{slug}/link    pkg, and target ("<kind> <ref>", a
//     pick from the Unmatched lane or the sessions) or kind and ref: link
//     by hand (the plan service's manual link), with an Undo that unlinks.
//     Both pickers start on a disabled "Pick …" option, and a form sent
//     without a pick is refused;
//   - POST /app/plans/{slug}/unlink  pkg, kind, ref: unlink for good, with
//     an Undo that links again;
//   - POST /app/plans/{slug}/go      pkg, to=agent|lead: Go to the
//     package's live agent or to the plan's lead (planGoOf); with no pkg,
//     to the plan's lead (planLeadGo). The server decides again which
//     session that is, under Go's rules, and refuses when Go is off now or
//     would go to the other of the two than the button said;
//   - POST /app/plans/{slug}/hide    kind, ref, hidden=1|0: Not part of
//     this plan, or Restore (app_plans_unmatched.go);
//   - POST /app/plans/{slug}/unregister: unregister the plan, with the
//     plan service's Unregister, as DELETE /api/plans/{slug} does. It
//     answers with the Overview, since the page is gone, and a toast that
//     says how to register the file again. No Undo: what went (links, Mark
//     done, history) doesn't come back by registering again. Its
//     confirmation is a dialog on the plan's pane; without the script, the
//     button loads the page with ?unregister=1, which renders it open.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gravexa-llc/ai-manager/internal/monitor"
	"github.com/gravexa-llc/ai-manager/internal/prwait"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

var appPlan = &appPage{
	pattern:     "/app/plans/{slug}",
	name:        "plans",
	title:       "Plan",
	panes:       2,
	listLabel:   "Plan",
	detailLabel: "Package",
	topics:      []string{api.TopicPlans, api.TopicPRs, api.TopicSessions, api.TopicInbox}, // inbox: snoozed badges
	tmpl:        parseApp("plan.html"),
	fill:        fillAppPlan,
	routes:      appPlanRoutes,
	keys: []appKey{
		{"g", "Go to the package's agent, or else the plan's lead"},
		{"d", "Mark done"},
		{"u", "Undo Mark done, or the last change"},
	},
}

// planMenu lists the registered plans for the menu, each with what waits
// on you there: the plan summary's count, its Open items in the Needs you
// inbox, as of the sync pass inboxCount ran for the same menu. note
// says why the list is empty.
func (s *Server) planMenu(ctx context.Context) (plans []appMenuPlan, note string) {
	if s.opts.Plans == nil {
		return nil, "Plans aren't available."
	}
	list, err := s.opts.Plans.Plans(ctx)
	if err != nil {
		s.log.Error("app menu plans", "err", err)
		return nil, "aimd couldn't read the plans."
	}
	for _, b := range list.Plans {
		p := appMenuPlan{Slug: b.Slug, Title: cmp.Or(b.Title, b.Slug), URL: appPlanURL(b.Slug)}
		if sum, err := s.opts.Plans.Summary(ctx, b.Slug, time.Time{}); err == nil {
			p.NeedsYou = sum.NeedsYou
		} else {
			s.log.Error("app menu plan summary", "plan", b.Slug, "err", err)
		}
		plans = append(plans, p)
	}
	if len(plans) == 0 {
		note = "No plans registered."
	}
	return plans, note
}

// ---- The page's state, in its URL ----

// The plan views (?view=).
const (
	planGraph    = "graph"
	planKanban   = "kanban"
	planTimeline = "timeline"
)

var planViews = []struct{ key, label string }{
	{planGraph, "Dependency graph"},
	{planKanban, "Kanban"},
	{planTimeline, "Timeline"},
}

// planViewKey reads ?view=: one of the views, the graph by default.
func planViewKey(v string) string {
	for _, pv := range planViews {
		if strings.EqualFold(v, pv.key) {
			return pv.key
		}
	}
	return planGraph
}

// planQuery is a plan page's state: the plan, the view, the selected
// package (its ID as the plan writes it) and the search.
type planQuery struct{ slug, view, pkg, q string }

// href is the page with view and pkg ("" selects nothing), keeping the
// search.
func (pq planQuery) href(view, pkg string) string {
	h := appPlanURL(pq.slug) + "?view=" + url.QueryEscape(view)
	if pkg != "" {
		h += "&pkg=" + url.QueryEscape(pkg)
	}
	if pq.q != "" {
		h += "&q=" + url.QueryEscape(pq.q)
	}
	return h
}

// pick is the page in its view with pkg selected.
func (pq planQuery) pick(pkg string) string { return pq.href(pq.view, pkg) }

// matches reports whether p matches the search: its ID, title or lane
// holds it, in any case.
func (pq planQuery) matches(p api.Package) bool {
	if pq.q == "" {
		return true
	}
	q := strings.ToLower(pq.q)
	return strings.Contains(strings.ToLower(p.ID), q) || strings.Contains(strings.ToLower(p.Title), q) ||
		strings.Contains(strings.ToLower(p.Lane), q)
}

// ---- The view ----

// planPage is the page's .View.
type planPage struct {
	Slug, Title string
	// View is graph, kanban or timeline; Tabs switch it, keeping the
	// selection.
	View string
	Tabs []planTab
	// Legend counts the packages in each lifecycle.
	Legend []planLegend
	// Warnings, Unmatched and Provisional say how many parser warnings,
	// new Unmatched items (the older ones aside) and provisional packages
	// the plan has ("" for none); PlanHref shows them: the plan with
	// nothing selected.
	Warnings, Unmatched, Provisional string
	PlanHref                         string
	// Search is ?q=.
	Search string
	// Empty is true for a plan without packages.
	Empty    bool
	Graph    *planGraphView
	Kanban   []kanbanCol
	Timeline *planTimelineView
	// Pkg is the selected package's pane; Plan the plan's, when nothing
	// (or a package the plan doesn't have) is selected.
	Pkg  *pkgDetail
	Plan *planOverview
	// Action is the forms' base path, /app/plans/<slug>.
	Action string
}

type planTab struct {
	Label, Href string
	On          bool
}

type planLegend struct {
	Key, Label string
	N          int
}

// planBadge is a badge on a node, a card, a row or in the detail pane.
type planBadge struct {
	// Kind is needs, waiting, review, lead, snoozed, working or prov (a
	// provisional package): its color (b-<kind> in app-plan.css).
	Kind string
	Text string
	// Why says more, for a title and the detail pane.
	Why string
}

func fillAppPlan(s *Server, r *http.Request, sn *snapshot, d *appData) error {
	ctx := r.Context()
	b, err := s.appPlanBoard(ctx, r.PathValue("slug"))
	if err != nil {
		return err
	}
	d.PlanSlug = b.Slug
	d.Title = cmp.Or(b.Title, b.Slug)
	d.Summary = strconv.Itoa(b.Total) + " packages · " + strconv.Itoa(b.Counts["done"]) + " done"
	if b.Provisional > 0 {
		d.Summary += " · " + strconv.Itoa(b.Provisional) + " not in the plan file"
	}
	if d.Fragment != "" && d.Fragment != "list" && d.Fragment != "detail" {
		return nil // the menu or the summary: nothing more to build
	}
	fresh, err := s.freshFacts(ctx, sn)
	if err != nil {
		return err
	}
	b = fresh.apply(b)

	q := r.URL.Query()
	pq := planQuery{slug: b.Slug, view: planViewKey(q.Get("view")), q: strings.TrimSpace(q.Get("q"))}
	var sel *api.Package
	missing := ""
	if id := strings.TrimSpace(q.Get("pkg")); id != "" {
		if i := slices.IndexFunc(b.Packages, func(p api.Package) bool { return strings.EqualFold(p.ID, id) }); i >= 0 {
			sel = &b.Packages[i]
			pq.pkg = sel.ID
		} else {
			missing = id
		}
	}
	snoozed := s.planSnoozed(ctx, b.Slug)
	badges := map[string][]planBadge{}
	for _, p := range b.Packages {
		badges[strings.ToUpper(p.ID)] = planBadgesOf(sn, fresh.prs, p, snoozed[strings.ToUpper(p.ID)])
	}

	v := &planPage{
		Slug: b.Slug, Title: cmp.Or(b.Title, b.Slug), View: pq.view, Search: pq.q,
		PlanHref: pq.href(pq.view, ""), Empty: len(b.Packages) == 0, Action: appPlanURL(b.Slug),
		Warnings: plural(len(b.Warnings), "parser warning", "parser warnings"), Unmatched: plural(unmatchedNew(b), "unmatched", "unmatched"),
		Provisional: plural(b.Provisional, "not in plan file", "not in plan file"),
	}
	for _, pv := range planViews {
		v.Tabs = append(v.Tabs, planTab{Label: pv.label, Href: pq.href(pv.key, pq.pkg), On: pv.key == pq.view})
	}
	for _, c := range boardColumns {
		if n := b.Counts[c.key]; c.key != "dropped" || n > 0 {
			v.Legend = append(v.Legend, planLegend{Key: c.key, Label: c.label, N: n})
		}
	}
	if d.Fragment != "detail" && !v.Empty {
		switch pq.view {
		case planKanban:
			v.Kanban = kanbanOf(b, pq, badges)
		case planTimeline:
			v.Timeline = s.planTimelineOf(ctx, sn, b, pq, badges)
		default:
			v.Graph = planGraphOf(b, pq, badges)
		}
	}
	if d.Fragment != "list" {
		// Who merges: the plan's Merges line, or Settings' override.
		merges := mergesLabel(mergesOf(b.Slug, b.Merges, s.mergesOverrides(ctx)))
		if sel != nil {
			v.Pkg = s.pkgDetailOf(sn, b, *sel, pq, badges[strings.ToUpper(sel.ID)])
			v.Pkg.Merges = merges
		} else {
			v.Plan = s.planOverviewOf(sn, b, pq, badges, d)
			v.Plan.Merges = merges
			v.Plan.Unregister = truthy(q.Get("unregister"))
			v.Plan.UnregisterHref = pq.href(pq.view, "") + "&unregister=1"
			if missing != "" {
				v.Plan.Missing = "This plan has no package " + strconv.Quote(missing) + "."
			}
		}
	}
	d.View = v
	return nil
}

// appPlanBoard reads a plan's board for the page, turning an unknown plan
// into a 404.
func (s *Server) appPlanBoard(ctx context.Context, slug string) (api.Plan, error) {
	if s.opts.Plans == nil {
		return api.Plan{}, errNoPage{"plans aren't available: this aimd runs without its plan service"}
	}
	b, err := s.opts.Plans.Plan(ctx, slug)
	if err != nil {
		var refusal interface{ HTTPStatus() int }
		if errors.As(err, &refusal) && refusal.HTTPStatus() == http.StatusNotFound {
			return api.Plan{}, errNoPage{"no plan " + strconv.Quote(slug) + " is registered"}
		}
		return api.Plan{}, err
	}
	return b, nil
}

// packagesByID are a board's packages by upper-case ID; the first of two
// with one ID (a parser warning) wins, as in the graph's layout.
func packagesByID(b api.Plan) map[string]api.Package {
	out := make(map[string]api.Package, len(b.Packages))
	for _, p := range b.Packages {
		if _, dup := out[strings.ToUpper(p.ID)]; !dup {
			out[strings.ToUpper(p.ID)] = p
		}
	}
	return out
}

// plural says "1 thing" or "3 things"; "" for none.
func plural(n int, one, many string) string {
	switch n {
	case 0:
		return ""
	case 1:
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

// ---- Badges ----

// planBadgesOf works out a package's badges, the most urgent first: needs
// you (a linked agent waits on you, or your step is ready: yourStep),
// waiting on you (a PR ready to merge that you merge), review requested,
// snoozed (instead of those three), waiting on lead, working. prs are the
// snapshot's PRs by lower-case ref (freshFacts).
func planBadgesOf(sn *snapshot, prs map[string]store.PR, p api.Package, snoozed bool) []planBadge {
	var needs, waiting, review, lead, working []string
	for _, l := range p.Links {
		a := l.Agent
		if l.Kind != store.LinkAgent || a == nil || !a.Live {
			continue
		}
		name := agentLabel(a, l.Ref)
		switch {
		case a.NeedsYou:
			why := name + " needs you: " + agentWaitsOn(a)
			if !a.NeedsYouSince.IsZero() {
				why += " · " + ago(sn.now, a.NeedsYouSince)
			}
			needs = append(needs, why)
		case monitor.AgentWorking(a):
			why := name + " is working"
			if a.LastAction != "" {
				why += " · last: " + a.LastAction
			}
			working = append(working, why)
		}
	}
	// Your step's rule is the inbox's too (yourstep.go): a Your step item
	// waits while this badge shows.
	if why, ok := yourStep(p); ok {
		needs = append(needs, why)
	}
	for _, w := range prWaits(sn, prs, p) {
		why := w.label + " " + w.v.Reason
		switch {
		case w.v.NeedsYou == prwait.ReadyToMerge:
			waiting = append(waiting, why)
		case w.v.NeedsYou == prwait.ReviewRequested:
			review = append(review, why)
		case w.v.WaitingOn == prwait.WaitingLead:
			lead = append(lead, why)
		}
	}
	var out []planBadge
	add := func(kind, text string, whys []string) {
		if len(whys) > 0 {
			out = append(out, planBadge{Kind: kind, Text: text, Why: strings.Join(whys, "; ")})
		}
	}
	if snoozed {
		add("snoozed", "snoozed", []string{"what waits on you here is snoozed in Needs you"})
	} else {
		add("needs", "needs you", needs)
		add("waiting", "waiting on you", waiting)
		add("review", "review requested", review)
	}
	add("lead", "waiting on lead", lead)
	add("working", "working", working)
	if p.Provisional {
		add("prov", "not in plan file", []string{"the plan file's tables don't have " + p.ID + " yet; it shows as provisional until its row is added"})
	}
	return out
}

// prWait is what one of a package's open PRs waits on.
type prWait struct {
	label string // "#172"
	v     prwait.Verdict
}

// prWaits are the verdicts of internal/prwait's rule (sn.mon.PRWait:
// review requested, ready to merge on a plan you merge, waiting on the
// lead or an agent) for the package's open PRs. A PR aimd hasn't read
// from GitHub yet has none.
func prWaits(sn *snapshot, prs map[string]store.PR, p api.Package) []prWait {
	var out []prWait
	for _, l := range p.Links {
		if l.Kind != store.LinkPR {
			continue
		}
		pr, ok := prs[strings.ToLower(l.Ref)]
		if !ok || pr.State != store.PROpen {
			continue
		}
		out = append(out, prWait{label: "#" + strconv.Itoa(pr.Number), v: sn.mon.PRWait(pr)})
	}
	return out
}

// planSnoozed returns the packages of plan slug (by upper-case ID) whose
// waiting Needs you items (the inbox) are all snoozed: their
// badge says "snoozed" instead of what waits on you. It reads the items as
// the last sync pass left them (the menu's badge and aimd's ticker run
// one); without an inbox store nothing is snoozed.
func (s *Server) planSnoozed(ctx context.Context, slug string) map[string]bool {
	st, ok := s.inboxStore()
	if !ok {
		return nil
	}
	items, err := st.ListInbox(ctx, store.InboxFilter{Waiting: true})
	if err != nil {
		s.log.Error("app plan snoozed items", "plan", slug, "err", err)
		return nil
	}
	snoozed, awake := map[string]bool{}, map[string]bool{}
	for _, it := range items {
		if it.Pkg == "" || !strings.EqualFold(it.PlanSlug, slug) {
			continue
		}
		k := strings.ToUpper(it.Pkg)
		if it.State == store.InboxSnoozed {
			snoozed[k] = true
		} else {
			awake[k] = true
		}
	}
	for k := range awake {
		delete(snoozed, k)
	}
	return snoozed
}

// agentWaitsOn says what a linked agent that needs you waits on: its
// reason today ("permission prompt", "waiting for your reply"). The mod's
// reports add what it waits on in detail: a permission's tool
// and command, a question and its options, the plan to approve.
func agentWaitsOn(a *api.LinkAgent) string { return needReason(a.Reason) }

// ---- The graph ----

// planGraphMetrics size the page's graph: room for a title in each box and
// for a badge above it.
var planGraphMetrics = graphMetrics{nodeW: 164, nodeH: 44, gapX: 44, gapY: 20, pad: 14, titles: true, titleRunes: 22}

// planGraphView is the dependency graph's drawing.
type planGraphView struct {
	W, H  int
	Label string
	Nodes []planNode
	// Edges are drawn in order: the selected package's last, on top.
	Edges []planEdge
}

// planNode is a package's box.
type planNode struct {
	ID, Title, Lifecycle, Href string
	// Label is its accessible name, Tip its tooltip.
	Label, Tip              string
	X, Y, W, H, TX, TY, TY2 int
	Sel, Dim, Cycle         bool
	// Prov is a provisional package: drawn with a dotted edge.
	Prov bool
	// Focus is a box the graph opens scrolled to show (data-focus).
	Focus bool
	// Badge is its most urgent badge, drawn on its top edge.
	Badge *nodeBadge
}

type nodeBadge struct {
	Kind, Text         string
	X, Y, W, H, TX, TY int
}

// planEdge is a dependency's arrow. Class is pe, with met (the need is
// done), cycle, on (the selected package's) or off (another's while one is
// selected); Marker its arrowhead's id.
type planEdge struct {
	D, Class, Marker string
}

func planGraphOf(b api.Plan, pq planQuery, badges map[string][]planBadge) *planGraphView {
	g := layoutGraph(b.Packages)
	byID := packagesByID(b)
	label := fmt.Sprintf("Dependency graph of %s: %d packages, %d dependencies", cmp.Or(b.Title, b.Slug), len(g.Nodes), len(g.Edges))
	svg := drawGraph(g, planGraphMetrics, "pg", label, pq.pick)
	v := &planGraphView{W: svg.W, H: svg.H, Label: svg.Label}
	sel := strings.ToUpper(pq.pkg)
	for _, n := range svg.Nodes {
		p := byID[strings.ToUpper(n.ID)]
		pn := planNode{ID: n.ID, Title: n.Title, Lifecycle: n.Lifecycle, Href: n.Href, Tip: n.Tip,
			X: n.X, Y: n.Y, W: n.W, H: n.H, TX: n.TX, TY: n.TY, TY2: n.TY2, Cycle: n.Cycle,
			Sel: sel != "" && strings.EqualFold(n.ID, sel), Dim: !pq.matches(p), Prov: p.Provisional}
		pn.Label = n.ID + " " + p.Title + ": " + strings.ToLower(lifecycleLabel(n.Lifecycle))
		if w := whatTip(p.What); w != "" {
			pn.Tip += "\n" + w
		}
		for _, bd := range badges[strings.ToUpper(n.ID)] {
			pn.Label += ", " + bd.Text
			pn.Tip += "\n" + bd.Text + ": " + bd.Why
		}
		if bs := badges[strings.ToUpper(n.ID)]; len(bs) > 0 {
			w := 14 + 6*utf8.RuneCountInString(bs[0].Text)
			x := n.X + n.W - w - 8
			pn.Badge = &nodeBadge{Kind: bs[0].Kind, Text: bs[0].Text, X: x, Y: n.Y - 10, W: w, H: 18, TX: x + w/2, TY: n.Y + 3}
		}
		v.Nodes = append(v.Nodes, pn)
	}
	var on []planEdge
	for _, e := range svg.Edges {
		pe := planEdge{D: e.D, Class: "pe", Marker: "pg-a"}
		if byID[strings.ToUpper(e.From)].Lifecycle == "done" {
			pe.Class += " met"
		}
		if e.Marker == "c" {
			pe.Class += " cycle"
			pe.Marker = "pg-c"
		}
		if sel != "" {
			if strings.EqualFold(e.From, sel) || strings.EqualFold(e.To, sel) {
				pe.Class += " on"
				pe.Marker = "pg-h"
				on = append(on, pe)
				continue
			}
			pe.Class += " off"
		}
		v.Edges = append(v.Edges, pe)
	}
	v.Edges = append(v.Edges, on...)
	markFocus(v.Nodes)
	return v
}

// markFocus marks the boxes the graph opens scrolled to show (data-focus):
// the packages being worked on or in review or, with none, the ones ready to
// start. app-plan.js pans the graph from its left end just far enough to
// show them. With none of those either, it stays at its left end.
func markFocus(nodes []planNode) {
	for _, tier := range [][]string{{"working", "in_review"}, {"ready"}} {
		n := 0
		for i := range nodes {
			if slices.Contains(tier, nodes[i].Lifecycle) {
				nodes[i].Focus = true
				n++
			}
		}
		if n > 0 {
			return
		}
	}
}

// ---- Kanban ----

type kanbanCol struct {
	Key, Label string
	Cards      []kanbanCard
}

type kanbanCard struct {
	ID, Title, Lifecycle, Href string
	// Tip is its title attribute: the package's What, as plain text.
	Tip string
	// Lane is the package's lane, "Yours" for your own steps, or "".
	Lane     string
	Sel, Dim bool
	// Prov is a provisional package: drawn with a dashed edge, and
	// its "not in plan file" badge.
	Prov   bool
	Badges []planBadge
}

func kanbanOf(b api.Plan, pq planQuery, badges map[string][]planBadge) []kanbanCol {
	var cols []kanbanCol
	for _, c := range boardColumns {
		if c.key != "dropped" || b.Counts["dropped"] > 0 {
			cols = append(cols, kanbanCol{Key: c.key, Label: c.label})
		}
	}
	for _, p := range b.Packages {
		card := kanbanCard{ID: p.ID, Title: p.Title, Lifecycle: p.Lifecycle, Href: pq.pick(p.ID), Lane: laneOf(p), Tip: whatTip(p.What),
			Sel: strings.EqualFold(p.ID, pq.pkg), Dim: !pq.matches(p), Prov: p.Provisional, Badges: badges[strings.ToUpper(p.ID)]}
		for i := range cols {
			if cols[i].Key == p.Lifecycle {
				cols[i].Cards = append(cols[i].Cards, card)
				break
			}
		}
	}
	return cols
}

// laneOf is a package's lane as the board shows it.
func laneOf(p api.Package) string {
	switch {
	case p.Lane != "":
		return p.Lane
	case p.Yours:
		return "Yours"
	}
	return ""
}

// ---- Timeline ----

// planTimelineView is the timeline: an axis of ticks and a row per package.
type planTimelineView struct {
	Ticks []timelineTick
	Rows  []timelineRow
	// Note says what the bars leave out; Error why there are none.
	Note, Error string
	// Zone names the time zone of the axis and the tips (the Time zone
	// setting's).
	Zone string
}

// timelineTick is a time on the axis, at X (a percentage).
type timelineTick struct {
	X, Label, Anchor string
}

type timelineRow struct {
	ID, Title, Lifecycle, Href string
	Sel, Dim                   bool
	// Prov is a provisional package, tagged "not in plan file".
	Prov bool
	Bars []timelineBar
	// Tip says the bars in words.
	Tip string
}

// timelineBar is a span: work (an agent working), review (waiting in
// review, thinner) or done (a done package's whole span). X and W are
// percentages of the axis; Y and H its height in the row.
type timelineBar struct {
	Kind, X, W string
	Y, H       int
}

// pkgSpan is when things happened to a package, from its history.
type pkgSpan struct {
	// work is when an agent first worked on it (it became working, an
	// agent was linked, or a linked agent reported an event); review when
	// it first waited in review (it became in review, or a PR opened).
	work, review time.Time
	// done is its last move to done, merged its PR's merge, seenDone when
	// it was first seen already done (registered after it was done).
	done, merged, seenDone time.Time
}

// planHistoryLimit is how much of the plan's history the timeline reads:
// all the service gives.
const planHistoryLimit = api.MaxTimelineLimit

// timelineWindow is the longest span the axis shows; older bars are cut
// at its left edge.
const timelineWindow = 60 * 24 * time.Hour

func (s *Server) planTimelineOf(ctx context.Context, sn *snapshot, b api.Plan, pq planQuery, badges map[string][]planBadge) *planTimelineView {
	v := &planTimelineView{Zone: sn.zone.Name}
	tl, err := s.opts.Plans.Timeline(ctx, b.Slug, api.TimelineQuery{Limit: planHistoryLimit})
	if err != nil {
		s.log.Error("app plan timeline", "plan", b.Slug, "err", err)
		v.Error = "aimd couldn't read this plan's history."
		return v
	}
	spans := planSpans(tl.Entries)
	now := sn.now
	type bar struct {
		kind     string
		from, to time.Time
	}
	rows := make([][]bar, len(b.Packages))
	tips := make([]string, len(b.Packages))
	var lo time.Time
	for i, p := range b.Packages {
		sp := spans[strings.ToUpper(p.ID)]
		if sp == nil {
			sp = &pkgSpan{}
		}
		add := func(kind string, from, to time.Time) {
			if from.IsZero() || from.After(to) {
				return
			}
			rows[i] = append(rows[i], bar{kind, from, to})
			if lo.IsZero() || from.Before(lo) {
				lo = from
			}
		}
		since := func(what string, t time.Time) string {
			if t.IsZero() {
				return what
			}
			return what + " since " + clock(now, t) + " (" + ago(now, t) + ")"
		}
		switch p.Lifecycle {
		case "working":
			start := cmp.Or(sp.work, p.Since)
			add("work", start, now)
			tips[i] = since("Working", start)
		case "in_review":
			rev := cmp.Or(sp.review, p.Since)
			tips[i] = since("In review", rev)
			if !sp.work.IsZero() && sp.work.Before(rev) {
				add("work", sp.work, rev)
				tips[i] = "Worked from " + clock(now, sp.work) + "; " + since("in review", rev)
			}
			add("review", rev, now)
		case "done":
			end := cmp.Or(sp.merged, sp.done, sp.seenDone, p.Since)
			start := sp.work
			if start.IsZero() || (!sp.review.IsZero() && sp.review.Before(start)) {
				start = sp.review
			}
			add("done", start, end)
			tips[i] = "Done"
			if !end.IsZero() {
				tips[i] += " " + clock(now, end)
			}
			if !start.IsZero() && start.Before(end) {
				tips[i] = "From " + clock(now, start) + " to " + strings.ToLower(tips[i][:1]) + tips[i][1:]
			}
		default:
			tips[i] = "Not started: " + strings.ToLower(lifecycleLabel(p.Lifecycle))
		}
	}

	switch {
	case lo.IsZero():
		lo = now.Add(-7 * 24 * time.Hour)
		v.Note = "No package has started yet."
	case now.Sub(lo) > timelineWindow:
		lo = now.Add(-timelineWindow)
		v.Note = "Bars that began before " + lo.In(now.Location()).Format("Jan 2") + " are cut at the left edge."
	case now.Sub(lo) < 24*time.Hour:
		lo = now.Add(-24 * time.Hour)
	}
	span := now.Sub(lo)
	pct := func(t time.Time) float64 {
		f := float64(t.Sub(lo)) / float64(span) * 100
		return min(100, max(0, f))
	}
	ps := func(f float64) string { return strconv.FormatFloat(f, 'f', 2, 64) + "%" }
	v.Ticks = timelineTicks(lo, now, pct, ps)
	for i, p := range b.Packages {
		row := timelineRow{ID: p.ID, Title: p.Title, Lifecycle: p.Lifecycle, Href: pq.pick(p.ID),
			Sel: strings.EqualFold(p.ID, pq.pkg), Dim: !pq.matches(p), Prov: p.Provisional, Tip: p.ID + " " + p.Title + ": " + tips[i]}
		for _, bd := range badges[strings.ToUpper(p.ID)] {
			row.Tip += "; " + bd.Text
		}
		for _, br := range rows[i] {
			x, end := pct(br.from), pct(br.to)
			w := max(end-x, 0.4) // a short span still shows, inside the axis
			x = min(x, 100-w)
			tb := timelineBar{Kind: br.kind, X: ps(x), W: ps(w), Y: 1, H: 12}
			if br.kind == "review" {
				tb.Y, tb.H = 4, 6
			}
			row.Bars = append(row.Bars, tb)
		}
		v.Rows = append(v.Rows, row)
	}
	return v
}

// planSpans reads when things happened to each package (by upper-case ID)
// from the plan's history, newest first or not.
func planSpans(entries []api.TimelineEntry) map[string]*pkgSpan {
	out := map[string]*pkgSpan{}
	first := func(t *time.Time, at time.Time) {
		if !at.IsZero() && (t.IsZero() || at.Before(*t)) {
			*t = at
		}
	}
	last := func(t *time.Time, at time.Time) {
		if at.After(*t) {
			*t = at
		}
	}
	for _, e := range entries {
		for _, id := range e.Packages {
			k := strings.ToUpper(id)
			sp := out[k]
			if sp == nil {
				sp = &pkgSpan{}
				out[k] = sp
			}
			switch e.Kind {
			case api.TimelineLifecycle:
				switch e.Lifecycle {
				case "working":
					first(&sp.work, e.At)
				case "in_review":
					first(&sp.review, e.At)
				case "done":
					if e.Prev == "" {
						first(&sp.seenDone, e.At)
					} else {
						last(&sp.done, e.At)
					}
				}
			case api.TimelineEvent:
				first(&sp.work, e.At)
			case api.TimelineLink:
				if e.Link != nil && e.Link.Kind == store.LinkAgent {
					first(&sp.work, e.At)
				}
			case api.TimelinePR:
				switch e.Action {
				case "opened":
					first(&sp.review, e.At)
				case "merged":
					last(&sp.merged, e.At)
				}
			}
		}
	}
	return out
}

// tickSteps are the axis' steps; it uses the smallest that gives at most
// maxTicks ticks.
var tickSteps = []time.Duration{time.Hour, 3 * time.Hour, 6 * time.Hour, 12 * time.Hour,
	24 * time.Hour, 48 * time.Hour, 7 * 24 * time.Hour, 14 * 24 * time.Hour}

const maxTicks = 8

// timelineTicks are the axis' ticks between lo and hi, on whole hours or
// days in hi's time zone.
func timelineTicks(lo, hi time.Time, pct func(time.Time) float64, ps func(float64) string) []timelineTick {
	loc := hi.Location()
	lo = lo.In(loc)
	step := tickSteps[len(tickSteps)-1]
	for _, st := range tickSteps {
		if hi.Sub(lo)/st < maxTicks { // n steps make up to n+1 ticks
			step = st
			break
		}
	}
	var t time.Time
	if step >= 24*time.Hour {
		t = time.Date(lo.Year(), lo.Month(), lo.Day(), 0, 0, 0, 0, loc)
		for t.Before(lo) {
			t = t.AddDate(0, 0, 1)
		}
	} else {
		t = time.Date(lo.Year(), lo.Month(), lo.Day(), lo.Hour(), 0, 0, 0, loc)
		for t.Before(lo) || t.Hour()%int(step/time.Hour) != 0 {
			t = t.Add(time.Hour)
		}
	}
	var out []timelineTick
	for ; !t.After(hi); t = nextTick(t, step) {
		x := pct(t)
		// Days by their date; hours by the time, but midnight by its date
		// and the first tick by both.
		tk := timelineTick{X: ps(x), Label: t.Format("Jan 2"), Anchor: "middle"}
		if step < 24*time.Hour && t.Hour() != 0 {
			tk.Label = t.Format("15:04")
			if len(out) == 0 {
				tk.Label = t.Format("Jan 2 15:04")
			}
		}
		switch {
		case x < 4:
			tk.Anchor = "start"
		case x > 96:
			tk.Anchor = "end"
		}
		out = append(out, tk)
	}
	return out
}

// nextTick steps a tick: by calendar days for a step of days, so a daylight
// saving change keeps ticks on midnight.
func nextTick(t time.Time, step time.Duration) time.Time {
	if step >= 24*time.Hour {
		return t.AddDate(0, 0, int(step/(24*time.Hour)))
	}
	return t.Add(step)
}

// ---- The package's detail ----

// pkgDetail is the selected package's pane.
type pkgDetail struct {
	ID, Title, Lifecycle, LifecycleLabel string
	// What is the plan's What cell for it, nil when empty.
	What *pkgWhat
	// Sub is the plan, repo, size, lane and owner.
	Sub string
	// Since says how long it has been in its lifecycle.
	Since  string
	Badges []planBadge
	// Signals are the state engine's other badges: CI failing, a draft, a
	// conflict, stalled, quiet, a low-confidence link.
	Signals          []badgeView
	Needs, Unblocks  []pkgDep
	NeedsNote, Cycle string
	// PRs and Worktrees are its links of those kinds, and Agents its agent
	// links, each live one with its line (pkgAgent); Branches its
	// branches, from its branch links, its PRs or its worktrees; Suggested
	// the branch the conventions suggest, when it has none.
	PRs, Worktrees []linkView
	Agents         []pkgAgent
	Branches       []string
	Suggested      string
	// Merges says who merges this plan's PRs: its Merges line, or
	// Settings' override (fillAppPlan sets it).
	Merges string
	// Links are all its links, for Unlink.
	Links     []linkView
	Go        planGo
	Done      planDone
	Choices   []linkGroup
	CloseHref string
	// Provisional is set for a package the plan file doesn't have yet
	// (a provisional package): the pane says so and shows the row to paste.
	Provisional *pkgProvisional
}

// pkgAgent is an agent link on the package's pane. A live agent has Now:
// its line as Claude Code's agent list shows it ("general-purpose  Bash:
// Run the tests  19m 30s · ↓ 284.1k tokens"), with the link's other facts
// under it (Link.Facts, less the last action the line shows). An ended
// one, or one aimd hasn't seen, is just its link.
type pkgAgent struct {
	Link linkView
	Now  *agentNow
}

// pkgAgentOf is an agent link on the pane: a live agent with its line
// (agentLine, shared with the Sessions page).
func pkgAgentOf(now time.Time, lv linkView, a *api.LinkAgent) pkgAgent {
	if a == nil || !a.Live {
		return pkgAgent{Link: lv}
	}
	n := agentLine(now, lv.Label, a.LastAction, a.StartedAt, a.Tokens)
	// The line shows the last action: the facts under it don't repeat it.
	lv.Facts = slices.DeleteFunc(slices.Clone(lv.Facts), func(f factView) bool { return f.lastAction })
	return pkgAgent{Link: lv, Now: n}
}

// pkgProvisional is what the pane says of a provisional package: the plan
// file's name, the suggested row (RowID its element's id, for Copy), when
// aimd first saw its work, and what named it.
type pkgProvisional struct {
	File, Row, RowID, Since string
	Sources                 []string
}

// provisionalOf is a provisional package's part of the pane.
func provisionalOf(now time.Time, b api.Plan, p api.Package) *pkgProvisional {
	pp := &pkgProvisional{File: "the plan file", Row: p.SuggestedRow, RowID: "pd-row-" + p.ID}
	if b.Path != "" {
		pp.File = filepath.Base(b.Path)
	}
	if !p.FirstSeen.IsZero() {
		pp.Since = "aimd first saw its work at " + clock(now, p.FirstSeen) + " (" + ago(now, p.FirstSeen) + " ago)"
	}
	for _, src := range p.Sources {
		what := map[string]string{store.ProvisionalSpawn: "a spawn brief's Package: line, agent", store.ProvisionalBranch: "the branch",
			store.ProvisionalPR: "the Plan: line of PR"}[src.Kind]
		ref := src.Ref
		if src.Kind == store.ProvisionalBranch {
			if _, br, ok := strings.Cut(ref, ":"); ok {
				ref = br
			}
		}
		text := cmp.Or(what, src.Kind) + " " + ref
		if src.Title != "" {
			text += " (“" + src.Title + "”)"
		}
		pp.Sources = append(pp.Sources, text)
	}
	return pp
}

// pkgDep is a package a package needs or unblocks.
type pkgDep struct {
	ID, Title, Lifecycle, State, Href string
}

// planDone is the pane's Mark done: offered (Can), undoable (Undo: marked
// done here), or off, with why.
type planDone struct {
	Can, Undo bool
	Off       string
}

// planDoneOf is package p's Mark done, as the pane offers it; a Your step
// item in Needs you offers the same.
func planDoneOf(p api.Package) planDone {
	switch {
	case p.DoneManual:
		return planDone{Undo: true}
	case canMarkDone(p):
		return planDone{Can: true}
	case p.DoneInPlan:
		return planDone{Off: "It is marked ✅ in the plan file, so it is done."}
	case p.Lifecycle == "done":
		return planDone{Off: "It is done already."}
	case hasPRLink(p):
		return planDone{Off: "Mark done is for steps without a PR: this one is done when its PR merges."}
	}
	return planDone{Off: "It can't be marked done."}
}

// linkGroup is a group of the Link picker's choices.
type linkGroup struct {
	Label   string
	Choices []linkChoice
}

// linkChoice is one thing to link: Value is "<kind> <ref>".
type linkChoice struct{ Value, Label string }

// signalKinds are the state badges the pane lists under Signals: the ones
// the plan badges don't already say.
var signalKinds = map[string]bool{
	"ci_failing": true, "conflict": true, "changes_requested": true, "draft": true,
	"stalled": true, "quiet": true, "collision": true, "lane_conflict": true, "low_confidence": true,
}

func (s *Server) pkgDetailOf(sn *snapshot, b api.Plan, p api.Package, pq planQuery, badges []planBadge) *pkgDetail {
	byID := packagesByID(b)
	c := s.cardOf(sn, p) // the board's facts (board.go)
	d := &pkgDetail{ID: p.ID, Title: p.Title, Lifecycle: p.Lifecycle, LifecycleLabel: lifecycleLabel(p.Lifecycle),
		What: pkgWhatOf(p.ID, p.What), Badges: badges, NeedsNote: p.NeedsNote, Cycle: c.Cycle, CloseHref: pq.href(pq.view, ""), Links: c.Links}
	sub := []string{cmp.Or(b.Title, b.Slug)}
	for _, part := range []string{p.Repo, sizeLabel(p.Size), laneLabel(p), ownerLabel(p.Owner)} {
		if part != "" {
			sub = append(sub, part)
		}
	}
	d.Sub = strings.Join(sub, " · ")
	if c.Since != "" {
		d.Since = c.LifecycleLabel + " for " + c.Since
		if c.Was != "" {
			d.Since += "; it was " + c.Was
		}
	}
	for _, bd := range p.Badges {
		if signalKinds[bd.Kind] {
			d.Signals = append(d.Signals, badgeView{Text: bd.Text, Class: badgeClasses[bd.Kind]})
		}
	}
	for _, id := range p.Needs {
		d.Needs = append(d.Needs, pq.dep(byID, id))
	}
	for _, q := range b.Packages {
		if slices.ContainsFunc(q.Needs, func(n string) bool { return strings.EqualFold(strings.TrimSpace(n), p.ID) }) {
			d.Unblocks = append(d.Unblocks, pq.dep(byID, q.ID))
		}
	}

	branches := map[string]bool{}
	addBranch := func(br string) {
		if br != "" && !branches[br] {
			branches[br] = true
			d.Branches = append(d.Branches, br)
		}
	}
	for _, l := range p.Links {
		switch l.Kind {
		case store.LinkBranch:
			_, br, ok := strings.Cut(l.Ref, ":")
			if !ok {
				br = l.Ref
			}
			addBranch(br)
		case store.LinkPR:
			if l.PR != nil {
				addBranch(l.PR.HeadBranch)
			}
		case store.LinkWorktree:
			if l.Worktree != nil {
				addBranch(l.Worktree.Branch)
			}
		}
	}
	agents := map[string]*api.LinkAgent{}
	for _, l := range p.Links {
		if l.Kind == store.LinkAgent {
			agents[l.Ref] = l.Agent
		}
	}
	for _, lv := range c.Links {
		switch lv.Kind {
		case store.LinkPR:
			d.PRs = append(d.PRs, lv)
		case store.LinkAgent:
			d.Agents = append(d.Agents, pkgAgentOf(sn.now, lv, agents[lv.Ref]))
		case store.LinkWorktree:
			d.Worktrees = append(d.Worktrees, lv)
		}
	}
	if len(d.Branches) == 0 {
		d.Suggested = p.SuggestedBranch
	}

	if p.Provisional {
		d.Provisional = provisionalOf(sn.now, b, p)
	}
	d.Go = s.planGoOf(sn, b, p)
	d.Done = planDoneOf(p)
	d.Choices = linkChoicesOf(sn, b)
	return d
}

func sizeLabel(size string) string {
	if size == "" {
		return ""
	}
	return "size " + size
}

func laneLabel(p api.Package) string {
	if l := laneOf(p); l != "" && l != "Yours" {
		return "lane " + l
	}
	if p.Yours {
		return "your step"
	}
	return ""
}

func ownerLabel(owner string) string {
	if owner == "" {
		return ""
	}
	return "owner " + owner
}

// mergesLabel says who merges a plan's PRs: its Merges line, or the
// Settings page's override of it, labelled as one.
func mergesLabel(m api.PlanMerges) string {
	who, wait := "you", "a ready PR waits on you"
	if m.Merges == prwait.MergesLead {
		who, wait = "the lead session", "a ready PR waits on the lead, not on you"
	}
	if m.Override {
		return who + " (override in Settings; the plan says Merges: " + m.Line + "): " + wait
	}
	return who + " (Merges: " + m.Line + "): " + wait
}

// dep is a needed or unblocked package, linking to it; a need the plan has
// no package for has no link.
func (pq planQuery) dep(byID map[string]api.Package, id string) pkgDep {
	id = strings.TrimSpace(id)
	p, ok := byID[strings.ToUpper(id)]
	if !ok {
		return pkgDep{ID: id, State: "not in this plan"}
	}
	return pkgDep{ID: p.ID, Title: p.Title, Lifecycle: p.Lifecycle, State: lifecycleLabel(p.Lifecycle), Href: pq.pick(p.ID)}
}

// planGo is a plan page's Go button: Go to agent, to a live agent linked
// to the package; Go to lead, to the plan's lead session; or off, with the
// reason (goButton.Off) naming what was tried.
type planGo struct {
	goButton
	// Lead is set when it goes to the plan's lead. Why then says where,
	// and on a package's pane why not to its agent, for the button's title
	// and the pane's hint.
	Lead bool
	Why  string
	// agent is why the package's agent can't be reached ("" when it goes
	// there), and lead the lead's name: for appPlanGo's refusal when the
	// page's button no longer matches.
	agent, lead string
}

// Why Go is off on a plan's page.
const (
	goOffNoService    = "Go isn't available: this aimd runs without it."
	goOffNoAgent      = "No live agent on this package."
	goOffLeadsUnknown = "aimd couldn't read which session leads this plan."
)

// planGoOf is a package pane's Go. It is Go to agent, to the first of the
// package's live agents Go can reach (goButtonOfAgent, Go's rules), as
// before; else Go to lead, to the plan's lead (planLeadGo); else off, with
// the agent's reason and then the lead's.
func (s *Server) planGoOf(sn *snapshot, b api.Plan, p api.Package) planGo {
	if s.opts.Go == nil {
		return planGo{goButton: goButton{Off: goOffNoService}}
	}
	agent := ""
	for _, l := range p.Links {
		if l.Kind != store.LinkAgent || l.Agent == nil || !l.Agent.Live {
			continue
		}
		btn := s.goButtonOfAgent(sn, l.Agent)
		if btn.Key != "" {
			return planGo{goButton: btn}
		}
		if agent == "" {
			agent = btn.Off
		}
	}
	agent = cmp.Or(agent, goOffNoAgent)
	g := s.planLeadGo(sn, b)
	g.agent = agent
	if g.Key == "" {
		g.Off = agent + " " + g.Off
		return g
	}
	g.Why = agent + " Goes to the plan's lead instead: " + g.lead + "."
	return g
}

// planLeadGo is Go to lead: to the plan's lead, or off with why.
//
// A plan's leads are the sessions that registered it (monitor's
// ReasonPlan rows, stored by store.MonitorPlanLead), as the snapshot's
// monitor set has them. Several sessions can lead one plan, and a lead's
// session id moves on /clear, so liveSessionByID matches each by the
// session's id or the one it had before. Go goes to the lead registered
// most recently of those that run and that Go can reach (goOff:
// interactive, not headless, in iTerm2 or Terminal), the later store row
// winning a tie: the newest lead is the one you most likely work in. When
// none can be reached, the reason is the newest running lead's, else that
// none runs, else that none is registered.
func (s *Server) planLeadGo(sn *snapshot, b api.Plan) planGo {
	off := func(why string) planGo { return planGo{goButton: goButton{Off: why}} }
	switch {
	case s.opts.Go == nil:
		return off(goOffNoService)
	case sn.mon == nil: // no monitor, or it couldn't read its inputs (logged)
		return off(goOffLeadsUnknown)
	}
	leads := slices.Clone(sn.mon.Leads(b.Slug))
	if len(leads) == 0 {
		return off("No lead registered for this plan: run /plan-register " + b.Path + " in its main session.")
	}
	slices.SortStableFunc(leads, func(x, y store.MonitoredSession) int {
		return cmp.Or(y.Since.Compare(x.Since), cmp.Compare(y.ID, x.ID))
	})
	why := ""
	for _, m := range leads {
		row, ok := liveSessionByID(sn.sessions, m.SessionID)
		if !ok {
			continue
		}
		name := sessionTitle(row)
		if o := goOff(row, appOf(sn.terms, row)); o != "" {
			if why == "" {
				why = "The plan's lead, " + name + ", can't be reached: " + lowerFirst(o)
			}
			continue
		}
		return planGo{goButton: goButton{Key: row.Key}, Lead: true, Why: "Goes to the plan's lead, " + name + ".", lead: name}
	}
	switch {
	case why != "":
		return off(why)
	case len(leads) > 1:
		return off("None of the plan's " + strconv.Itoa(len(leads)) + " leads is running.")
	}
	return off("The plan's lead isn't running.")
}

// lowerFirst puts s's first letter in lower case, to go on with a
// sentence.
func lowerFirst(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError {
		return s
	}
	return string(unicode.ToLower(r)) + s[n:]
}

// linkChoicesOf are the Link picker's choices: the plan's Unmatched lane
// (unmatchedChoices: new, then older; not the hidden ones), and the live
// sessions no package of the plan links (monitored ones, as
// internal/monitor says; headless runs aside).
func linkChoicesOf(sn *snapshot, b api.Plan) []linkGroup {
	out := unmatchedChoices(b)
	linked := map[string]bool{}
	for _, p := range b.Packages {
		for _, l := range p.Links {
			if l.Kind != store.LinkAgent {
				continue
			}
			linked[l.Ref] = true
			if a := l.Agent; a != nil && a.AgentID == "" {
				linked[a.SessionID] = true
				if a.JobID != "" {
					linked["job:"+a.JobID] = true
				}
			}
		}
	}
	var ss []linkChoice
	for _, row := range sn.sessions {
		if row.Headless || !sn.mon.Monitored(row) {
			continue
		}
		ref := row.SessionID
		if row.JobID != "" {
			ref = "job:" + row.JobID
		}
		if ref == "" || linked[ref] || linked[row.SessionID] {
			continue
		}
		name := cmp.Or(row.Name, filepath.Base(row.Cwd), ref)
		label := name + " (" + cmp.Or(row.Kind, "session")
		if row.Branch != "" {
			label += ", " + row.Branch
		}
		ss = append(ss, linkChoice{Value: store.LinkAgent + " " + ref, Label: label + ")"})
	}
	if len(ss) > 0 {
		out = append(out, linkGroup{Label: "Sessions", Choices: ss})
	}
	return out
}

// ---- The plan's pane ----

// planOverview is the detail pane with nothing selected.
type planOverview struct {
	Title, Sub string
	Segs       []segView
	BarLabel   string
	Merges     string
	// Go is Go to lead (planLeadGo).
	Go       planGo
	Ready    []pkgDep
	NeedsYou []planNeed
	// Provisional lists the packages the plan file doesn't have yet,
	// File is the file's name.
	Provisional []pkgDep
	File        string
	Alerts      []alertView
	Warnings    []warningView
	// Unmatched is the Unmatched lane: new, older and hidden.
	Unmatched      planUnmatched
	Repos          []string
	Path, PathFull string
	// Missing says the ?pkg= asked for isn't in the plan.
	Missing string
	// Unregister renders the Unregister confirmation open: ?unregister=1,
	// where UnregisterHref (the button without the script) goes.
	Unregister     bool
	UnregisterHref string
}

// planNeed is a package that waits on you, with the badges that say why.
type planNeed struct {
	pkgDep
	Badges []planBadge
}

func (s *Server) planOverviewOf(sn *snapshot, b api.Plan, pq planQuery, badges map[string][]planBadge, d *appData) *planOverview {
	// The board's own facts (board.go); the page's badges say what waits
	// on you, by internal/prwait's rule.
	pv := s.planViewOf(sn, b)
	o := &planOverview{Title: pv.Title, Segs: pv.Segs, BarLabel: pv.BarLabel, Go: s.planLeadGo(sn, b),
		Alerts: pv.Alerts, Warnings: pv.Warnings, Unmatched: planUnmatchedOf(sn, b, d, pv.Options),
		Repos: pv.Repos, Path: pv.Path, PathFull: pv.PathFull}
	sub := []string{strconv.Itoa(b.Total) + " packages", strconv.Itoa(b.Counts["done"]) + " done"}
	if pv.Registered != "" {
		sub = append(sub, "registered "+pv.Registered+" ago")
	}
	if pv.Derived != "" {
		sub = append(sub, "updated "+pv.Derived+" ago")
	}
	o.Sub = strings.Join(sub, " · ")
	byID := packagesByID(b)
	for _, id := range b.ReadyToDispatch {
		o.Ready = append(o.Ready, pq.dep(byID, id))
	}
	for _, p := range b.Packages {
		var why []planBadge
		for _, bd := range badges[strings.ToUpper(p.ID)] {
			switch bd.Kind {
			case "needs", "waiting", "review":
				why = append(why, bd)
			}
		}
		if len(why) > 0 {
			o.NeedsYou = append(o.NeedsYou, planNeed{pkgDep: pq.dep(byID, p.ID), Badges: why})
		}
		if p.Provisional {
			o.Provisional = append(o.Provisional, pq.dep(byID, p.ID))
		}
	}
	o.File = "the plan file"
	if b.Path != "" {
		o.File = filepath.Base(b.Path)
	}
	return o
}

// ---- Forms ----

func appPlanRoutes(s *Server) {
	s.mux.HandleFunc("POST /app/plans/{slug}/done", s.appPlanDone)
	s.mux.HandleFunc("POST /app/plans/{slug}/link", s.appPlanLink)
	s.mux.HandleFunc("POST /app/plans/{slug}/unlink", s.appPlanUnlink)
	s.mux.HandleFunc("POST /app/plans/{slug}/hide", s.appPlanHide)
	s.mux.HandleFunc("POST /app/plans/{slug}/go", s.appPlanGo)
	s.mux.HandleFunc("POST /app/plans/{slug}/unregister", s.appPlanUnregister)
}

// appPlansOn refuses a form when there is no plan service.
func (s *Server) appPlansOn(w http.ResponseWriter, r *http.Request) bool {
	if s.opts.Plans == nil {
		s.appRefuse(w, r, http.StatusNotFound, "/app/", "Plans aren't available: this aimd runs without its plan service.")
		return false
	}
	return true
}

// appPlanRefuse answers a form the plan service refused, with the
// refusal's status and message, or aimd's own failure (logged) as a 500.
// It goes back to the page, or to the Overview if the plan is gone.
func (s *Server) appPlanRefuse(w http.ResponseWriter, r *http.Request, slug, prefix string, err error) {
	status := http.StatusInternalServerError
	var refusal interface{ HTTPStatus() int }
	if errors.As(err, &refusal) {
		status = refusal.HTTPStatus()
	}
	loc := appReturn(r)
	if _, perr := s.opts.Plans.Plan(r.Context(), slug); perr != nil {
		loc = "/app/"
	}
	s.appRefuse(w, r, status, loc, s.refusedText(r, prefix, err))
}

// planReturn is where a form on plan slug's page goes back to: its return
// field, with pkg selected when it is set. An Undo's return must fit a
// toast's field, so a long one is cut down to the plan and the package.
// Mark done on a Your step item (app_inbox.go) posts here from the Needs
// you page: it goes back there, as it came.
func planReturn(r *http.Request, slug, pkg string) string {
	page := appPlanURL(slug)
	u, err := url.Parse(appReturn(r))
	if err == nil && u.EscapedPath() == inboxPath {
		if loc := u.String(); len(loc) <= maxToastField {
			return loc
		}
		return inboxPath
	}
	if err != nil || u.EscapedPath() != page {
		u = &url.URL{Path: page}
	}
	q := u.Query()
	if pkg != "" {
		q.Set("pkg", pkg)
	}
	u.RawQuery = q.Encode()
	if loc := u.String(); len(loc) <= maxToastField {
		return loc
	}
	return page + "?pkg=" + url.QueryEscape(pkg)
}

// appPlanDone is POST /app/plans/{slug}/done (pkg, done=1|0).
func (s *Server) appPlanDone(w http.ResponseWriter, r *http.Request) {
	if !s.appPlansOn(w, r) {
		return
	}
	slug, pkg := r.PathValue("slug"), formValue(r, "pkg")
	done := formValue(r, "done") != "0"
	var before []string // ready to dispatch before, to say what it unblocked
	if b, err := s.opts.Plans.Plan(r.Context(), slug); err == nil {
		before = b.ReadyToDispatch
	}
	res, err := s.opts.Plans.SetDone(r.Context(), slug, pkg, done)
	if err != nil {
		s.appPlanRefuse(w, r, slug, "Not changed: ", err)
		return
	}
	id := cmp.Or(res.Package.ID, pkg)
	loc := planReturn(r, slug, id)
	t := &appToast{}
	if done {
		t.Text = "Marked " + id + " done (the plan file is unchanged)."
		t.Undo = &appUndo{Action: appPlanURL(slug) + "/done", Fields: map[string]string{"pkg": id, "done": "0", "return": loc}}
		var ready []string
		for _, p := range res.Summary.Ready {
			if !slices.ContainsFunc(before, func(b string) bool { return strings.EqualFold(b, p.ID) }) {
				ready = append(ready, p.ID)
			}
		}
		if len(ready) > 0 {
			t.Text += " Now ready to dispatch: " + strings.Join(ready, ", ") + "."
		}
	} else {
		t.Text = "Undid Mark done on " + id + "; it is " + strings.ToLower(lifecycleLabel(res.Package.Lifecycle)) + " now."
	}
	s.appReply(w, r, loc, t)
}

// linkTarget reads a link form's target: the picker's "<kind> <ref>", or
// the kind and ref fields.
func linkTarget(r *http.Request) (kind, ref string) {
	if t := formValue(r, "target"); t != "" {
		kind, ref, _ = strings.Cut(t, " ")
		return kind, strings.TrimSpace(ref)
	}
	return formValue(r, "kind"), formValue(r, "ref")
}

// appPlanLink is POST /app/plans/{slug}/link: a manual link, whose
// Undo unlinks it.
func (s *Server) appPlanLink(w http.ResponseWriter, r *http.Request) {
	if !s.appPlansOn(w, r) {
		return
	}
	slug := r.PathValue("slug")
	req := api.LinkRequest{Plan: slug, Pkg: formValue(r, "pkg")}
	req.Kind, req.Ref = linkTarget(r)
	// The pickers start on "Pick …", which sends nothing.
	switch {
	case req.Pkg == "":
		s.appRefuse(w, r, http.StatusBadRequest, appReturn(r), "Not linked: pick the package to link it to.")
		return
	case req.Kind == "" || req.Ref == "":
		s.appRefuse(w, r, http.StatusBadRequest, appReturn(r), "Not linked: pick a PR, branch or session to link, or type its ref.")
		return
	}
	res, err := s.opts.Plans.LinkManual(r.Context(), req)
	if err != nil {
		s.appPlanRefuse(w, r, slug, "Not linked: ", err)
		return
	}
	// What was stored: the package as the plan writes it, the ref as the
	// plan service normalized it (a PR's URL becomes owner/repo#123).
	pkg, kind, ref := strings.ToUpper(req.Pkg), req.Kind, req.Ref
	if len(res.Links) > 0 {
		pkg, kind, ref = res.Links[0].Pkg, res.Links[0].Kind, res.Links[0].Ref
	}
	loc := planReturn(r, slug, pkg)
	what := cmp.Or(linkKindLabels[kind], kind) + " " + ref
	s.appReply(w, r, loc, &appToast{Text: "Linked " + what + " to " + pkg + ".", Undo: &appUndo{
		Action: appPlanURL(slug) + "/unlink",
		Fields: map[string]string{"pkg": pkg, "kind": kind, "ref": ref, "return": loc},
	}})
}

// appPlanUnlink is POST /app/plans/{slug}/unlink: unlink for good, with
// an Undo that links it again.
func (s *Server) appPlanUnlink(w http.ResponseWriter, r *http.Request) {
	if !s.appPlansOn(w, r) {
		return
	}
	slug := r.PathValue("slug")
	req := api.LinkRequest{Plan: slug, Pkg: formValue(r, "pkg"), Kind: formValue(r, "kind"), Ref: formValue(r, "ref")}
	if _, err := s.opts.Plans.Unlink(r.Context(), req); err != nil {
		s.appPlanRefuse(w, r, slug, "Not unlinked: ", err)
		return
	}
	pkg := strings.ToUpper(req.Pkg)
	loc := planReturn(r, slug, pkg)
	what := cmp.Or(linkKindLabels[req.Kind], req.Kind) + " " + req.Ref
	s.appReply(w, r, loc, &appToast{
		Text: "Unlinked " + what + " from " + pkg + ". It stays unlinked until you link it again.",
		Undo: &appUndo{Action: appPlanURL(slug) + "/link", Label: "Link again",
			Fields: map[string]string{"pkg": pkg, "kind": req.Kind, "ref": req.Ref, "return": loc}},
	})
}

// appPlanGo is POST /app/plans/{slug}/go (pkg, to): a package pane's Go
// (planGoOf), or with no pkg the plan pane's Go to lead (planLeadGo).
// Which session, and whether Go may, is decided here again from the board
// and the store, not taken from the page. to is what the page's button
// said, agent or lead: if Go would now go to the other, it refuses and
// says so, rather than switch to a session the button didn't name.
func (s *Server) appPlanGo(w http.ResponseWriter, r *http.Request) {
	if !s.appPlansOn(w, r) {
		return
	}
	loc := appReturn(r)
	if s.opts.Go == nil {
		s.appRefuse(w, r, http.StatusNotFound, loc, "Go isn't available: this aimd runs without it.")
		return
	}
	ctx := r.Context()
	slug, id := r.PathValue("slug"), formValue(r, "pkg")
	b, err := s.opts.Plans.Plan(ctx, slug)
	if err != nil {
		s.appPlanRefuse(w, r, slug, "Go: ", err)
		return
	}
	sn, err := s.load(ctx)
	if err == nil {
		var fresh freshFacts
		if fresh, err = s.freshFacts(ctx, sn); err == nil {
			b = fresh.apply(b)
		}
	}
	if err != nil {
		s.appPlanRefuse(w, r, slug, "Go: ", err)
		return
	}
	var btn planGo
	if id == "" {
		btn = s.planLeadGo(sn, b)
	} else {
		i := slices.IndexFunc(b.Packages, func(p api.Package) bool { return strings.EqualFold(p.ID, id) })
		if i < 0 {
			s.appRefuse(w, r, http.StatusNotFound, loc, "Go: plan "+b.Slug+" has no package "+strconv.Quote(id)+".")
			return
		}
		btn = s.planGoOf(sn, b, b.Packages[i])
	}
	refuse := ""
	switch to := formValue(r, "to"); {
	case btn.Key == "":
		refuse = btn.Off
	case to == "agent" && btn.Lead && id != "": // the plan's pane has no Go to agent
		refuse = btn.agent + " The button now goes to the plan's lead, " + btn.lead + "."
	case to == "lead" && !btn.Lead:
		refuse = "A live agent on this package can be reached now; the button now goes to it."
	}
	if refuse != "" {
		s.appRefuse(w, r, http.StatusConflict, loc, "Go: "+refuse)
		return
	}
	res, err := s.goTo(r, btn.Key)
	if err != nil {
		s.appPlanRefuse(w, r, slug, "Go: ", err)
		return
	}
	s.appReply(w, r, loc, &appToast{Text: res.Message})
}

// appPlanUnregister is POST /app/plans/{slug}/unregister: the plan
// service's Unregister, the call DELETE /api/plans/{slug} makes. aimd
// forgets the plan (its packages, links, Mark done and history) and never
// touches its file. The page is gone, so the answer goes to the Overview,
// whose menu no longer lists the plan. There's no Undo: registering again
// starts afresh.
func (s *Server) appPlanUnregister(w http.ResponseWriter, r *http.Request) {
	if !s.appPlansOn(w, r) {
		return
	}
	slug := r.PathValue("slug")
	res, err := s.opts.Plans.Unregister(r.Context(), slug)
	if err != nil {
		s.appPlanRefuse(w, r, slug, "Not unregistered: ", err)
		return
	}
	s.appReply(w, r, "/app/", &appToast{Text: unregisteredText(res)})
}

// unregisteredText is the toast after unregistering: which plan, and the
// command that registers its file again, with the file's full path (the
// mod reads a path relative to the session's folder, never "~"). A title
// too long for the toast gives way to the slug, so the command fits whole.
func unregisteredText(res api.Unregistered) string {
	again := " The plan file is unchanged; to register it again, run /plan-register " + res.Path
	text := "Unregistered " + cmp.Or(res.Title, res.Slug) + "." + again
	if len(text) > maxToastText {
		text = "Unregistered " + res.Slug + "." + again
	}
	return text
}
