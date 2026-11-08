package web

// The Needs you page (GET /app/inbox): one item per waiting episode of
// a monitored session or a PR that needs you, a plan's missing row, or
// your step that is ready (internal/inbox), with Open, In progress,
// Snoozed and Done tabs, in sections that fold (groups.go), oldest first
// in each.
//
//   - ?group= is the Group by (remembered as the page's default): plan
//     (the default; the plans in the order they were registered, then the
//     items on no plan), repo (by name; then "Plans" for your steps whose
//     row names no repo, then "No repo"), or session (one section per
//     session, named as the Sessions page names it, by name; then "Pull
//     requests" for the PRs, which come from no session, and "Plan files"
//     for the plans' missing rows and your steps). Every section starts open,
//     since everything here needs you; the one holding the selected item,
//     or every one under a search, opens whatever the browser remembers.
//   - ?tab=open|progress|snoozed|done picks the tab; without it, the
//     selected item's state's tab, else Open.
//   - ?item=<id> selects an item: the detail pane shows it, and opening it
//     clears its "new" highlight (the one write a GET makes, as a mail
//     client marks a message read). A notification's link names only the
//     item, so it lands on the item's tab.
//   - ?q= keeps the items whose labels contain it.
//
// The actions (Go, Snooze, Done, back to Open, Undo) are forms posting to
// /app/inbox/… (app_inbox_actions.go), answered with the toast protocol.
// Your step's Go is Open in plan, and its Mark done is the plan page's
// own form (POST /app/plans/{slug}/done), which comes back here. The sync
// that keeps the items is app_inbox_sync.go.

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/inbox"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// inboxPath is the Needs you page.
const inboxPath = "/app/inbox"

var appInbox = &appPage{
	pattern:     inboxPath,
	name:        "inbox",
	title:       "Needs you",
	panes:       2,
	listLabel:   "Needs you list",
	detailLabel: "Details",
	topics:      []string{api.TopicSessions, api.TopicPRs, api.TopicInbox},
	tmpl:        parseApp("inbox.html"),
	fill:        fillInbox,
	routes:      inboxRoutes,
	keys: []appKey{
		{"g", "Go: switch to its terminal tab, open the PR on GitHub, or open your step in its plan"},
		{"s", "Snooze"},
		{"d", "Done"},
		{"u", "Undo, or move a Done item back to Open"},
	},
}

// inboxCount is the Needs you badge in the menu and the count in the tab
// title: the inbox's Open items, after a sync pass. Without an inbox store
// it is needsYouTotal: what the store says waits on you.
func (s *Server) inboxCount(ctx context.Context, sn *snapshot) int {
	st, ok := s.inboxStore()
	if !ok {
		return needsYouTotal(sn)
	}
	if _, err := s.SyncInbox(ctx); err != nil {
		s.log.Error("inbox sync", "err", err)
	}
	counts, err := st.InboxCounts(ctx)
	if err != nil {
		s.log.Error("inbox counts", "err", err)
		return needsYouTotal(sn)
	}
	return counts[inbox.Open]
}

// ---- View ----

type inboxView struct {
	Tab  string
	Tabs []inboxTab
	// GroupBy is the list's Group by control.
	GroupBy groupControl
	// Groups are the tab's items by the Group by; Empty is said when there
	// are none, Footer under the list.
	Groups []inboxGroupView
	Empty  string
	Footer string
	// Item is the selected item; Missing is true when ?item= names none.
	Item    *inboxDetail
	Missing bool
	// Unavailable says why there is no inbox (no inbox store).
	Unavailable string
}

type inboxTab struct {
	Key, Label, URL string
	Count           int
	On              bool
}

type inboxGroupView struct {
	listGroup
	Label string
	Count int
	Items []inboxRow
}

// inboxGroupChoices are the page's Group by choices, the default first.
var inboxGroupChoices = []groupChoice{
	{Value: groupPlan, Label: "Plan"},
	{Value: groupRepo, Label: "Repo"},
	{Value: groupSession, Label: "Session"},
}

// inboxGroupNames say the Group by in the summary and the footer.
var inboxGroupNames = map[string]string{groupPlan: "plan", groupRepo: "repo", groupSession: "session"}

type inboxRow struct {
	ID, URL              string
	KindClass, KindLabel string
	Title, Sub           string
	Wait, WaitTitle      string
	Sel, New             bool
	// Chime is set on the rows that should ring: new Open permission
	// prompts and questions.
	Chime string
	// Search is more text the header's filter matches: the branch.
	Search string
}

type inboxDetail struct {
	ID                   int64
	KindClass, KindLabel string
	Term                 *termTag
	Status               string
	Title, Sub           string
	// What it waits on: a session's (Waits, Reply), a PR's (PR), a
	// provisional package's missing row (PlanRow), or your step (Step).
	Waits   *inboxWaits
	Reply   *inboxReply
	PR      *inboxPRView
	PlanRow *inboxPlanRow
	Step    *inboxStep
	// The actions.
	Go       inboxGo
	Snooze   bool
	Snoozes  []inbox.SnoozeChoice
	Done     bool
	BackOpen string // the back-to-Open button's label, "" for none
	Where    []inboxWhere
	History  []inboxHistory
}

// inboxGo is the detail pane's Go button: hidden (Show false), disabled
// with the reason Off (Go's rules, golink.go), or a form; OpenURL makes it
// open the PR on GitHub too.
type inboxGo struct {
	Show    bool
	Label   string
	Off     string
	OpenURL string
}

// inboxWaits is what a session's item waits on (waitsOf): from the mod's report
// when the mod made one (the tool and its redacted command, the question
// and its options, the plan's steps), else the tool and its label from
// the needs-you reason ("permission prompt: Bash: Create directory x").
type inboxWaits struct {
	// Label heads the section: "Wants to run", "Asks", "Plan to approve".
	Label     string
	Tool      string
	Summary   string
	Questions []inboxQuestion
	Steps     []string
	Hint      string
}

type inboxQuestion struct {
	Question string
	Options  []string
}

// inboxReply is a background job waiting for your reply.
type inboxReply struct {
	// Last is the job's last message, as the mod summarized it.
	Last string
	// Attach is the command that opens it: claude attach <id>.
	Attach string
}

type inboxPRView struct {
	State, Why string
}

// inboxPlanRow is a provisional package's missing row: the row to
// paste into the plan file (ID is its element's id, for Copy), the file's
// name, why it is missing, and the package on its plan's page.
type inboxPlanRow struct {
	ID, Row, File, Why, PkgURL string
}

// inboxStep is your step's package: its What cell (plain text, as a graph
// box's tooltip has it), its "you:" step (Note), how long it has been
// ready, what to do (Hint), and the plan page's Mark done for it.
type inboxStep struct {
	What, Note        string
	Ready, ReadyTitle string
	Hint              string
	// MarkDone is the plan page's Mark done form's action
	// (/app/plans/<slug>/done) and Pkg its package, while the item waits
	// and the plan offers it, on your own step only; MarkOff says why it
	// is off instead. DoneHint, on a "you:" step, says to dismiss it with
	// Done instead.
	MarkDone, Pkg string
	MarkOff       string
	DoneHint      string
}

type inboxWhere struct {
	Key, Value, URL string
	Code            bool
}

type inboxHistory struct {
	When, Stamp, Text string
}

// inboxFooter is a tab's footer, which says the Group by, the Settings
// page's highlight and the Done history.
func inboxFooter(tab, group string, set inbox.Settings, highlight bool) string {
	switch tab {
	case inbox.Open:
		by := "Grouped by " + inboxGroupNames[group] + ", oldest first in each group."
		if !highlight {
			return by
		}
		return by + " New items stay highlighted until you open them."
	case inbox.InProgress:
		return "You clicked Go on these. Each returns to Open if it is still waiting after the delay."
	case inbox.Snoozed:
		return "Snoozed items come back at their time, or when what they wait on changes."
	case inbox.Done:
		return "Handled or no longer waiting, kept for " + keptText(set) + ". Move one back to Open from its details."
	}
	return ""
}

// inboxURL is the page's URL for a tab, an item, a search and the Group
// by the URL has (group, or "").
func inboxURL(tab string, item int64, q, group string) string {
	v := url.Values{}
	if tab != "" {
		v.Set("tab", tab)
	}
	if item > 0 {
		v.Set("item", strconv.FormatInt(item, 10))
	}
	if q != "" {
		v.Set("q", q)
	}
	if group != "" {
		v.Set("group", group)
	}
	if len(v) == 0 {
		return "/app/inbox"
	}
	return "/app/inbox?" + v.Encode()
}

func validTab(tab string) bool {
	for _, t := range inbox.States {
		if t == tab {
			return true
		}
	}
	return false
}

// fillInbox fills the page: a sync pass, the tabs and their counts, the
// tab's items by plan, and the selected item.
func fillInbox(s *Server, r *http.Request, sn *snapshot, d *appData) error {
	ctx := r.Context()
	v := &inboxView{}
	d.View = v
	st, ok := s.inboxStore()
	if !ok {
		v.Unavailable = "The inbox needs aimd's own store."
		d.Summary = strconv.Itoa(needsYouTotal(sn)) + " waiting on you"
		return nil
	}
	if _, err := s.SyncInbox(ctx); err != nil {
		s.log.Error("inbox sync", "err", err) // show what the store has
	}
	counts, err := st.InboxCounts(ctx)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	search := strings.TrimSpace(q.Get("q"))
	prefs := s.settings(ctx) // the highlight and the chime
	set := s.inboxSettingsOf(prefs)

	var sel *store.InboxItem
	if id, _ := strconv.ParseInt(q.Get("item"), 10, 64); id > 0 {
		it, err := st.InboxItem(ctx, id)
		switch {
		case errors.Is(err, store.ErrNotFound):
			v.Missing = true
		case err != nil:
			return err
		default:
			sel = &it
		}
	}
	// Opening an item clears its highlight.
	if sel != nil && sel.SeenAt.IsZero() && (d.Fragment == "" || d.Fragment == "detail") {
		if it, _, err := inbox.Act(ctx, st, sel.ID, inbox.Action{Do: inbox.ActSeen}, s.now(), set); err == nil {
			*sel = it
		}
	}
	v.Tab = q.Get("tab")
	if !validTab(v.Tab) {
		v.Tab = inbox.Open
		if sel != nil {
			v.Tab = sel.State
		}
	}
	group, kept, ctl := groupBy(r, d, "inbox", inboxGroupChoices)
	v.GroupBy = ctl
	d.Summary = strconv.Itoa(counts[inbox.Open]) + " open · grouped by " + inboxGroupNames[group]
	plans := s.inboxPlans(ctx)

	selID := int64(0)
	if sel != nil {
		selID = sel.ID
	}
	for _, t := range inbox.States {
		v.Tabs = append(v.Tabs, inboxTab{Key: t, Label: inbox.StateLabel(t), URL: inboxURL(t, 0, search, kept), Count: counts[t], On: t == v.Tab})
	}
	if d.Fragment == "" || d.Fragment == "list" {
		items, err := st.ListInbox(ctx, store.InboxFilter{State: v.Tab})
		if err != nil {
			return err
		}
		items = filterInbox(items, search)
		for _, g := range s.inboxSections(ctx, sn, items, group, plans) {
			gv := inboxGroupView{listGroup: listGroup{Key: g.key, Open: true, Force: search != ""}, Label: g.label, Count: len(g.items)}
			for _, it := range g.items {
				gv.Items = append(gv.Items, inboxRowOf(sn.now, it, v.Tab, search, kept, it.ID == selID, prefs))
				gv.Force = gv.Force || it.ID == selID
			}
			v.Groups = append(v.Groups, gv)
		}
		v.Footer = inboxFooter(v.Tab, group, set, prefs.Highlight)
		v.Empty = "Nothing here."
		if search != "" {
			v.Empty = "Nothing here matches “" + search + "”."
		}
	}
	if sel != nil && (d.Fragment == "" || d.Fragment == "detail") {
		moves, err := st.InboxMoves(ctx, sel.ID)
		if err != nil {
			return err
		}
		v.Item = s.inboxDetailOf(sn, *sel, moves, plans, set)
	}
	return nil
}

// filterInbox keeps the items whose labels contain q (any case).
func filterInbox(items []store.InboxItem, q string) []store.InboxItem {
	if q == "" {
		return items
	}
	q = strings.ToLower(q)
	var out []store.InboxItem
	for _, it := range items {
		hay := strings.ToLower(strings.Join([]string{it.Title, it.Subtitle, it.Detail, it.PlanSlug, it.Pkg, it.Branch, it.PR, it.Repo}, "\x00"))
		if strings.Contains(hay, q) {
			out = append(out, it)
		}
	}
	return out
}

// inboxKindClass is a kind's color class (the shell's .kind-*).
func inboxKindClass(kind string) string {
	switch kind {
	case inbox.KindPrompt:
		return "kind-permission"
	case inbox.KindPlanRow:
		return "kind-reply" // the shell's neutral palette: a chore, not a wait
	case inbox.KindYourStep:
		return "kind-plan" // a plan's step, in the plan approval's palette
	}
	return "kind-" + kind
}

// inboxRowOf is an item's row. An unseen Open item is new: highlighted
// while the Settings page's highlight is on, and, for a permission prompt
// or a question, marked to chime while the chime is on.
func inboxRowOf(now time.Time, it store.InboxItem, tab, q, group string, sel bool, prefs store.Settings) inboxRow {
	unseen := it.State == inbox.Open && it.SeenAt.IsZero() && !sel
	r := inboxRow{
		ID:        strconv.FormatInt(it.ID, 10),
		URL:       inboxURL(tab, it.ID, q, group),
		KindClass: inboxKindClass(it.Kind),
		KindLabel: inbox.KindLabel(it.Kind),
		Title:     it.Title,
		Sub:       it.Subtitle,
		Sel:       sel,
		New:       unseen && prefs.Highlight,
		Search:    it.Branch,
	}
	if r.Title == "" {
		r.Title = it.Ref
	}
	r.Wait, r.WaitTitle = waitText(now, it)
	if unseen && prefs.Chime && (it.Kind == inbox.KindPermission || it.Kind == inbox.KindQuestion) {
		r.Chime = "inbox-" + r.ID
	}
	return r
}

// waitText is a row's right-hand text: how long an Open item has waited,
// when an In progress item returns, until when one is snoozed.
func waitText(now time.Time, it store.InboxItem) (text, title string) {
	switch it.State {
	case inbox.Open:
		if !it.Waiting() {
			return "stopped", "stopped waiting " + stamp(now, it.EndedAt)
		}
		if it.Kind == inbox.KindYourStep {
			return ago(now, it.Since), "ready since " + stamp(now, it.Since)
		}
		return ago(now, it.Since), "waiting since " + stamp(now, it.Since)
	case inbox.InProgress:
		return "back at " + dueText(now, it.DueAt), "back to Open at " + stamp(now, it.DueAt) + " if still waiting"
	case inbox.Snoozed:
		if it.DueAt.IsZero() {
			return "until it changes", "snoozed until it changes"
		}
		return "until " + dueText(now, it.DueAt), "snoozed until " + stamp(now, it.DueAt)
	}
	return "done", "done " + stamp(now, it.StateAt)
}

// dueText writes a coming time: "11:15", "tomorrow 09:00", "Nov 9 09:00".
func dueText(now, t time.Time) string {
	t = t.In(now.Location())
	y, m, d := t.Date()
	if ny, nm, nd := now.Date(); y == ny && m == nm && d == nd {
		return t.Format("15:04")
	}
	if ty, tm, td := now.AddDate(0, 0, 1).Date(); y == ty && m == tm && d == td {
		return "tomorrow " + t.Format("15:04")
	}
	return t.Format("Jan 2 15:04")
}

// statusLine is the detail pane's line beside the kind.
func statusLine(now time.Time, it store.InboxItem) string {
	if !it.Waiting() && it.State != inbox.Done {
		return inbox.StateLabel(it.State) + " · stopped waiting at " + clock(now, it.EndedAt) + " (" + it.EndReason + ")"
	}
	switch it.State {
	case inbox.Open:
		if it.Kind == inbox.KindYourStep {
			return "Ready for " + ago(now, it.Since)
		}
		return "Waiting " + ago(now, it.Since)
	case inbox.InProgress:
		return "In progress · back to Open at " + dueText(now, it.DueAt) + " if still waiting"
	case inbox.Snoozed:
		if it.DueAt.IsZero() {
			return "Snoozed until it changes"
		}
		return "Snoozed until " + dueText(now, it.DueAt)
	}
	if !it.Waiting() {
		return "Done · " + it.EndReason
	}
	return "Done · still waiting"
}

// inboxPlans are the registered plans, for the groups' order and names
// and the packages' titles (and, for your steps, the packages).
type inboxPlans struct {
	order  []string
	titles map[string]string      // lower-case slug → title
	files  map[string]string      // lower-case slug → the plan file's name
	pkgs   map[string]api.Package // lower-case slug + "\x00" + package ID → the package
}

func (s *Server) inboxPlans(ctx context.Context) inboxPlans {
	p := inboxPlans{titles: map[string]string{}, files: map[string]string{}, pkgs: map[string]api.Package{}}
	if s.opts.Plans == nil {
		return p
	}
	list, err := s.opts.Plans.Plans(ctx)
	if err != nil {
		return p
	}
	for _, b := range list.Plans {
		p.order = append(p.order, b.Slug)
		p.titles[strings.ToLower(b.Slug)] = b.Title
		if b.Path != "" {
			p.files[strings.ToLower(b.Slug)] = filepath.Base(b.Path)
		}
		for _, k := range b.Packages {
			key := strings.ToLower(b.Slug) + "\x00" + k.ID
			if _, dup := p.pkgs[key]; !dup {
				p.pkgs[key] = k // the first of two rows with one ID is the package
			}
		}
	}
	return p
}

// pkgOf is package id of plan slug, if the plan has it.
func (p inboxPlans) pkgOf(slug, id string) (api.Package, bool) {
	k, ok := p.pkgs[strings.ToLower(slug)+"\x00"+id]
	return k, ok
}

// title is a plan's title, else its slug, or "—" for none.
func (p inboxPlans) title(slug string) string {
	if slug == "" {
		return "—"
	}
	return cmp.Or(p.titles[strings.ToLower(slug)], slug)
}

// groupLabel is a group's header: its plan's title, or for the items on
// no plan "No plan · watched" (the sessions you watch), or just "No plan"
// once it holds a PR (a review request reaches you on any PR).
func (p inboxPlans) groupLabel(g inbox.Group) string {
	if g.Plan != "" {
		return p.title(g.Plan)
	}
	for _, it := range g.Items {
		if it.Source == inbox.SourcePR {
			return "No plan"
		}
	}
	return "No plan · watched"
}

// inboxSection is one section of a tab's items.
type inboxSection struct {
	key, label string
	items      []store.InboxItem
}

// The Needs you sections that hold no session's or no repo's items, under
// Session and Repo grouping.
const (
	inboxPRsLabel   = "Pull requests"
	inboxPlansLabel = "Plan files"
	inboxNoRepo     = "No repo"
	// inboxStepsNoRepo holds, by repo, your steps whose plan row names no
	// repo.
	inboxStepsNoRepo = "Plans"
)

// inboxSections groups a tab's items under the Group by (see the file
// comment), oldest first in each section.
func (s *Server) inboxSections(ctx context.Context, sn *snapshot, items []store.InboxItem, group string, plans inboxPlans) []inboxSection {
	if group != groupRepo && group != groupSession {
		var out []inboxSection
		for _, g := range inbox.Groups(items, plans.order) {
			key := notOnPlanKey
			if g.Plan != "" {
				key = planKey(g.Plan)
			}
			out = append(out, inboxSection{key: key, label: plans.groupLabel(g), items: g.Items})
		}
		return out
	}
	// rank puts the sections of no repo or no session last, in this order.
	type section struct {
		inboxSection
		rank int
	}
	by := map[string]*section{}
	var order []*section
	names := inboxSessionNames{s: s, ctx: ctx, sn: sn}
	for _, it := range items {
		sec := section{}
		switch {
		case group == groupRepo && it.Repo == "" && it.Kind == inbox.KindYourStep:
			sec = section{inboxSection{key: "repo:-plans", label: inboxStepsNoRepo}, 1}
		case group == groupRepo && it.Repo == "":
			sec = section{inboxSection{key: "repo:-none", label: inboxNoRepo}, 2}
		case group == groupRepo:
			// A repo with no GitHub name is stored by its main checkout's path.
			label := it.Repo
			if strings.HasPrefix(label, "/") {
				label = filepath.Base(label)
			}
			sec = section{inboxSection{key: repoKey(it.Repo), label: label}, 0}
		case it.Source == inbox.SourceSession:
			sec = section{inboxSection{key: "session:" + strings.TrimPrefix(it.Ref, "session:"), label: names.of(it)}, 0}
		case it.Source == inbox.SourcePR:
			sec = section{inboxSection{key: "session:-prs", label: inboxPRsLabel}, 1}
		default:
			sec = section{inboxSection{key: "session:-plans", label: inboxPlansLabel}, 2}
		}
		have := by[sec.key]
		if have == nil {
			have = &sec
			by[sec.key] = have
			order = append(order, have)
		}
		have.items = append(have.items, it)
	}
	slices.SortStableFunc(order, func(a, c *section) int {
		return cmp.Or(cmp.Compare(a.rank, c.rank), cmp.Compare(strings.ToLower(a.label), strings.ToLower(c.label)), cmp.Compare(a.key, c.key))
	})
	out := make([]inboxSection, 0, len(order))
	for _, sec := range order {
		slices.SortStableFunc(sec.items, func(a, c store.InboxItem) int {
			return cmp.Or(a.Since.Compare(c.Since), cmp.Compare(a.ID, c.ID))
		})
		out = append(out, sec.inboxSection)
	}
	return out
}

// inboxSessionNames names the sessions of items as the Sessions page does
// (sessionTitle): from the live sessions, else, once, from the store's
// ended ones too.
type inboxSessionNames struct {
	s     *Server
	ctx   context.Context
	sn    *snapshot
	names map[string]string // inbox.SessionRef → title
	ended bool
}

func (n *inboxSessionNames) of(it store.InboxItem) string {
	if n.names == nil {
		n.names = map[string]string{}
		for _, row := range n.sn.sessions {
			n.names[inbox.SessionRef(row.ID)] = sessionTitle(row)
		}
	}
	if name, ok := n.names[it.Ref]; ok {
		return name
	}
	if !n.ended {
		n.ended = true
		rows, err := n.s.opts.Store.ListSessions(n.ctx, store.SessionFilter{IncludeEnded: true})
		if err != nil {
			n.s.log.Error("needs you: list ended sessions", "err", err)
		}
		for _, row := range rows {
			if ref := inbox.SessionRef(row.ID); n.names[ref] == "" {
				n.names[ref] = sessionTitle(row)
			}
		}
		if name, ok := n.names[it.Ref]; ok {
			return name
		}
	}
	return cmp.Or(it.SessionKey, "A session that has ended")
}

func (p inboxPlans) pkg(slug, id string) string {
	if k, ok := p.pkgOf(slug, id); ok && k.Title != "" {
		return id + " " + k.Title
	}
	return id
}

// liveRowOf is the live session an item's episode is of, if it still
// runs.
func liveRowOf(sn *snapshot, it store.InboxItem) (store.Session, bool) {
	if it.Source != inbox.SourceSession {
		return store.Session{}, false
	}
	for _, row := range sn.sessions {
		if inbox.SessionRef(row.ID) == it.Ref {
			return row, true
		}
	}
	return store.Session{}, false
}

// inboxDetailOf is the detail pane for an item.
func (s *Server) inboxDetailOf(sn *snapshot, it store.InboxItem, moves []store.InboxMove, plans inboxPlans, set inbox.Settings) *inboxDetail {
	now := sn.now
	d := &inboxDetail{
		ID:        it.ID,
		KindClass: inboxKindClass(it.Kind),
		KindLabel: inbox.KindLabel(it.Kind),
		Status:    statusLine(now, it),
		Title:     cmp.Or(it.Title, it.Ref),
		Sub:       it.Subtitle,
	}
	active := it.State == inbox.Open || it.State == inbox.InProgress
	row, live := liveRowOf(sn, it)

	switch it.Source {
	case inbox.SourceSession:
		if live {
			tag := termTagOf(row, appOf(sn.terms, row))
			d.Term = &tag
		}
		if it.Waiting() {
			d.Waits = waitsOf(it, row, live, s.sessionWait(row, live))
		}
		if it.Kind == inbox.KindReply {
			d.Reply = &inboxReply{}
			if live {
				d.Reply.Last = row.LastSaid
			}
			if it.JobID != "" {
				d.Reply.Attach = "claude attach " + it.JobID
			}
		}
		if active && it.Waiting() && s.opts.Go != nil {
			d.Go = inboxGo{Show: true, Label: "Go"}
			switch {
			case !live:
				d.Go.Off = "Its session isn't running any more."
			default:
				d.Go.Off = goOff(row, appOf(sn.terms, row))
			}
		}
	case inbox.SourcePR:
		d.Term = &termTag{Class: "term-web", Label: "GitHub", Title: "A pull request on GitHub"}
		d.PR = &inboxPRView{State: prStateLine(sn, it), Why: prWhy(it)}
		if active && it.Waiting() && githubURL(it.PRURL) {
			d.Go = inboxGo{Show: true, Label: "Open on GitHub", OpenURL: it.PRURL}
		}
	case inbox.SourcePlan:
		if it.Kind == inbox.KindYourStep {
			// Your step: Go is Open in plan, which shows the package on its
			// plan's page (and moves the item to In progress, as Go does).
			d.Step = yourStepOf(now, it, plans)
			if active && it.Waiting() {
				d.Go = inboxGo{Show: true, Label: "Open in plan"}
			}
			break
		}
		// A missing plan row: no Go, the row to paste instead.
		d.PlanRow = &inboxPlanRow{ID: "planrow-" + strconv.FormatInt(it.ID, 10), Row: it.Detail,
			File: cmp.Or(plans.files[strings.ToLower(it.PlanSlug)], "the plan file"), Why: upperFirst(it.Why) + ".",
			PkgURL: itemPkgURL(it)}
	}
	if it.Waiting() && active {
		d.Snooze, d.Snoozes = true, set.Snoozes()
	}
	d.Done = it.State != inbox.Done
	d.BackOpen = map[string]string{inbox.Done: "Move back to Open", inbox.Snoozed: "Unsnooze"}[it.State]

	plan := "—"
	planURL := ""
	if it.PlanSlug != "" {
		plan, planURL = plans.title(it.PlanSlug), appPlanURL(it.PlanSlug)
	}
	d.Where = append(d.Where, inboxWhere{Key: "Plan", Value: plan, URL: planURL})
	if it.Pkg != "" {
		w := inboxWhere{Key: "Package", Value: plans.pkg(it.PlanSlug, it.Pkg)}
		if it.Source == inbox.SourcePlan {
			w.URL = itemPkgURL(it)
		}
		d.Where = append(d.Where, w)
	}
	if it.Branch != "" {
		d.Where = append(d.Where, inboxWhere{Key: "Branch", Value: it.Branch, Code: true})
	}
	switch {
	case it.Folder != "":
		d.Where = append(d.Where, inboxWhere{Key: "Folder", Value: s.displayPath(it.Folder), Code: true})
	case it.Repo != "":
		d.Where = append(d.Where, inboxWhere{Key: "Repo", Value: it.Repo})
	}
	if pr, prURL := s.inboxPRLink(sn, it); pr != "" {
		d.Where = append(d.Where, inboxWhere{Key: "PR", Value: pr, URL: prURL})
	}
	for _, m := range moves {
		d.History = append(d.History, inboxHistory{When: clock(now, m.At), Stamp: m.At.UTC().Format(time.RFC3339), Text: m.Text})
	}
	return d
}

// itemPkgURL is a plan item's package on its plan's page: where Open in
// plan goes.
func itemPkgURL(it store.InboxItem) string {
	return appPlanURL(it.PlanSlug) + "?pkg=" + url.QueryEscape(it.Pkg)
}

// yourStepOf is your step's part of the detail pane: its package's What
// and "you:" step, how long it has been ready, what to do, and, for your
// own step (Owner you), Mark done, the plan page's (planDoneOf), from its
// package on the board now. A package that is your step only by its
// "you:" step is an agent's: its PR merging or its ✅ finishes it, so it
// gets no Mark done at all, and a click meant as "I did my part" can't
// close the agent's work. Done dismisses the item instead.
func yourStepOf(now time.Time, it store.InboxItem, plans inboxPlans) *inboxStep {
	v := &inboxStep{Note: it.Detail}
	p, ok := plans.pkgOf(it.PlanSlug, it.Pkg)
	if ok {
		v.What = whatTip(p.What)
	}
	if !it.Waiting() {
		return v
	}
	if !it.Since.IsZero() {
		v.Ready, v.ReadyTitle = "Ready since "+clock(now, it.Since)+" ("+ago(now, it.Since)+")", "ready since "+stamp(now, it.Since)
	}
	file := cmp.Or(plans.files[strings.ToLower(it.PlanSlug)], "the plan file")
	// Off the board (a race with the next sync pass), a step with no
	// "you:" note can only be yours by its Owner.
	yours := p.Yours || (!ok && it.Detail == "")
	if !yours {
		v.Hint = "Everything it needs is done, and its Needs cell has a step for you. Take it, then dispatch the package: " +
			"this item closes by itself when work on it begins."
		v.DoneHint = "Your part is done? Dismiss this with Done; the package finishes when its PR merges."
		return v
	}
	v.Hint = "It's yours (Owner: you), and everything it needs is done. When you've done it, press Mark done, " +
		"or put a ✅ in its row in " + file + ": this item closes by itself. Done only takes it out of Needs you."
	if !ok {
		return v
	}
	switch pd := planDoneOf(p); {
	case pd.Can:
		v.MarkDone, v.Pkg = appPlanURL(it.PlanSlug)+"/done", p.ID
	case !pd.Undo:
		v.MarkOff = pd.Off
	}
	return v
}

// githubURL reports whether u is a GitHub page: the only kind of link Go
// opens.
func githubURL(u string) bool { return strings.HasPrefix(u, "https://github.com/") }

// inboxPRLink is the PR the Where list names: a PR item's own, or the
// open (else latest) PR of a session's branch.
func (s *Server) inboxPRLink(sn *snapshot, it store.InboxItem) (label, link string) {
	if it.PR != "" {
		return it.PR + prSuffix(sn, it.PR), it.PRURL
	}
	// A repo with no GitHub name is stored by its path: no PR to find.
	if it.Repo == "" || it.Branch == "" || strings.HasPrefix(it.Repo, "/") {
		return "", ""
	}
	p := indexPRs(sn.prs).best(it.Repo, it.Branch)
	if p == nil {
		return "", ""
	}
	return store.PRRef(p.Repo, p.Number) + " (" + strings.ToLower(p.State) + ")", p.URL
}

// prSuffix is " (merged)" or " (closed)" for a PR that is no longer open.
func prSuffix(sn *snapshot, ref string) string {
	for _, p := range sn.prs {
		if strings.EqualFold(store.PRRef(p.Repo, p.Number), ref) && p.State != store.PROpen {
			return " (" + strings.ToLower(p.State) + ")"
		}
	}
	return ""
}

// prStateLine says a PR item's PR state now: "Open · CI green ·
// mergeable".
func prStateLine(sn *snapshot, it store.InboxItem) string {
	for _, p := range sn.prs {
		if !strings.EqualFold(store.PRRef(p.Repo, p.Number), it.PR) {
			continue
		}
		parts := []string{upperFirst(prStateLabel(p)), ciText(p)}
		switch {
		case p.Mergeable == "CONFLICTING" || p.MergeStateStatus == "DIRTY":
			parts = append(parts, "conflict")
		case p.MergeStateStatus == "CLEAN":
			parts = append(parts, "mergeable")
		}
		return strings.Join(parts, " · ")
	}
	return "Not stored any more"
}

// prWhy says why a PR item needs you: internal/prwait's reason.
func prWhy(it store.InboxItem) string {
	switch {
	case it.Why != "":
		return upperFirst(it.Why) + "."
	case it.Kind == inbox.KindReview:
		return "Your review is requested."
	}
	return "It is ready to merge, and the merge is yours."
}

// Hints under what a session item waits on.
const (
	hintPermission = "Allow or deny it in its terminal. Answering from here is a later feature."
	hintQuestion   = "Answer it in its terminal."
	hintPlan       = "Approve it or ask for changes in its terminal."
)

// sessionWait is the mod's report of what a live session that needs you waits
// on (Options.Waits: the mod's needs_you events, redacted, in aimd's
// memory), or nil when there is none.
func (s *Server) sessionWait(row store.Session, live bool) *api.WaitingOn {
	if !live || s.opts.Waits == nil || row.State != needsYou || row.SessionID == "" {
		return nil
	}
	if w, ok := s.opts.Waits.WaitingOn(row.SessionID); ok {
		return &w
	}
	return nil
}

// waitFits reports whether a report of kind wk tells what an item of kind
// waits on: the same kind, or a wait the poll couldn't name.
func waitFits(kind, wk string) bool { return kind == wk || kind == inbox.KindPrompt }

// waitsOf is what a session item waits on, when aimd knows. The mod's report w
// says it best: a permission's tool and the redacted summary of its
// command or input, a question with its options, the plan's steps. Without
// one (no mod, an aimd restart, a background job's reply), it is the tool
// and its label as the needs-you reason has them.
func waitsOf(it store.InboxItem, row store.Session, live bool, w *api.WaitingOn) *inboxWaits {
	if w != nil && waitFits(it.Kind, w.Kind) {
		if v := reportedWaits(*w); v != nil {
			return v
		}
	}
	reason := it.Sig
	if live {
		reason = inbox.Core(row.StateReason)
	}
	_, what, ok := strings.Cut(reason, ": ")
	if !ok || what == "" {
		return nil
	}
	switch it.Kind {
	case inbox.KindPermission:
		w := &inboxWaits{Label: "Wants to run", Hint: hintPermission}
		if tool, rest, ok := strings.Cut(what, ": "); ok && !strings.Contains(tool, " ") {
			w.Tool, w.Summary = tool, rest
		} else if tool, rest, ok := strings.Cut(what, " "); ok {
			w.Tool, w.Summary = tool, rest
		} else {
			w.Tool = what
		}
		return w
	case inbox.KindQuestion:
		return &inboxWaits{Label: "Asks", Questions: []inboxQuestion{{Question: what}}, Hint: hintQuestion}
	case inbox.KindPlan:
		return &inboxWaits{Label: "Plan to approve", Summary: what, Hint: hintPlan}
	}
	return nil
}

// reportedWaits lays out the mod's report for the detail pane, or nil when it
// says nothing to show.
func reportedWaits(w api.WaitingOn) *inboxWaits {
	switch w.Kind {
	case inbox.KindPermission:
		if w.Tool == "" {
			return nil
		}
		return &inboxWaits{Label: "Wants to run", Tool: w.Tool, Summary: w.Summary, Hint: hintPermission}
	case inbox.KindQuestion:
		v := &inboxWaits{Label: "Asks", Hint: hintQuestion}
		for _, q := range w.Questions {
			if text := cmp.Or(q.Question, q.Header); text != "" {
				v.Questions = append(v.Questions, inboxQuestion{Question: text, Options: q.Options})
			}
		}
		if len(v.Questions) == 0 {
			return nil
		}
		return v
	case inbox.KindPlan:
		title, steps := planSteps(w.Plan)
		if title == "" && len(steps) == 0 {
			return nil
		}
		return &inboxWaits{Label: "Plan to approve", Summary: title, Steps: steps, Hint: hintPlan}
	}
	return nil
}

// stepMarkRE is a Markdown list item's or heading's mark: "- ", "* ",
// "1. ", "2) ", "## ", or one alone on its line.
var stepMarkRE = regexp.MustCompile(`^(?:[-*+]|\d+[.)]|#+)(?:\s+|$)`)

// planSteps reads a plan's first lines (the mod keeps 12) as the detail pane
// lists them: its first heading as the title, then each other line that
// says something as a step, without its list mark.
func planSteps(plan string) (title string, steps []string) {
	for _, line := range strings.Split(plan, "\n") {
		line = strings.TrimSpace(line)
		heading := strings.HasPrefix(line, "#")
		line = strings.TrimSpace(stepMarkRE.ReplaceAllString(line, ""))
		switch {
		case line == "":
		case heading && title == "" && len(steps) == 0:
			title = line
		default:
			steps = append(steps, line)
		}
	}
	return title, steps
}
