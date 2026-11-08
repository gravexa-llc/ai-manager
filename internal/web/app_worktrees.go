package web

// The Worktrees & cleanup page (GET /app/worktrees).
//
// Each live worktree has `aim prune`'s verdict, as SafeToRemove last
// worked it out: Safe to remove, Scratch (detached, safe by rule), Not safe
// (with the reason), Locked, or Prunable record (folder gone). Main
// checkouts are never candidates and aren't listed. The list's sections
// fold (groups.go), grouped by ?group= (Group by, remembered as the page's
// default):
//
//   - repo (the default): one section per repo, by its GitHub name (else
//     its main checkout's folder name); the repos with a worktree on a plan
//     or a monitored session's first, then by name;
//   - plan: one section per registered plan, in the order they were
//     registered, then "No plan · watched" (a monitored session works in
//     it), then "Not on a plan". A worktree is on a plan when a board
//     links it, its branch, or its branch's PR (wtPlans);
//   - status: the verdicts' sections, in the order above.
//
// Under Repo and Plan grouping a section with no worktree on a plan or a
// monitored session's starts collapsed; every verdict section starts open;
// a search opens them all. The prunable records are their own section in
// every grouping, with their own form. Every row shows its verdict.
//
// Safe and scratch rows have checkboxes, in one selection form around
// every section; Remove selected opens a confirmation that lists each one
// with its branch and reason, and its POST (/app/worktrees/remove) hands
// them to Cleanup (cleanup.go), which checks each again at that moment and
// removes only those still safe. A separate POST (/app/worktrees/prune)
// runs `git worktree prune` for the records listed. The detail pane is the
// removal log.
//
// Selections are in the URL: ?pick=<id> for each checked row, ?all=1 for
// all the safe ones, and ?confirm=1 opens the confirmation (the way to it
// without the script); ?group= rides along. A request names worktrees by
// id (wtID) only, never by path: the server maps each id to a worktree
// aimd lists, and Cleanup matches that path against what git lists at that
// moment.

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/monitor"
	"github.com/gravexa-llc/ai-manager/internal/prune"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
	"github.com/gravexa-llc/ai-manager/internal/worktrees"
)

var appWorktrees = &appPage{
	pattern:     "/app/worktrees",
	name:        "worktrees",
	title:       "Worktrees & cleanup",
	panes:       2,
	listLabel:   "Worktrees",
	detailLabel: "Removal log",
	topics:      []string{"worktrees", "prs"},
	tmpl:        parseApp("worktrees.html"),
	fill:        fillWorktreesApp,
	routes:      worktreesRoutes,
}

// worktreesRoutes adds the page's form posts.
func worktreesRoutes(s *Server) {
	s.mux.HandleFunc("POST /app/worktrees/remove", s.worktreesRemove)
	s.mux.HandleFunc("POST /app/worktrees/prune", s.worktreesPrune)
}

// safeToRemoveCount is the Worktrees & cleanup badge in the menu: the live
// worktrees `aim prune` would remove (SafeToRemove's verdicts, from its
// cache). It is 0 while the verdicts are pending or not wired.
func (s *Server) safeToRemoveCount(ctx context.Context, sn *snapshot) int {
	v := s.verdicts(ctx)
	n := 0
	for _, w := range sn.worktrees {
		if vd, ok := v.get(w.Path); ok && vd.Removable {
			n++
		}
	}
	return n
}

// The verdicts' groups, in the order Cleanup status grouping shows them.
const (
	wtSafe     = "safe"
	wtScratch  = "scratch"
	wtNotSafe  = "not-safe"
	wtLocked   = "locked"
	wtPrunable = "prunable"
)

// wtVerdictOrder is the verdicts' order.
var wtVerdictOrder = []string{wtSafe, wtScratch, wtNotSafe, wtLocked, wtPrunable}

// wtGroupTitles are the verdicts' headers.
var wtGroupTitles = map[string]string{
	wtSafe:     "Safe to remove",
	wtScratch:  "Scratch: detached, safe by rule",
	wtNotSafe:  "Not safe",
	wtLocked:   "Locked: never removed",
	wtPrunable: "Prunable records: folder gone",
}

// wtGroupChoices are the page's Group by choices, the default first.
var wtGroupChoices = []groupChoice{
	{Value: groupRepo, Label: "Repo"},
	{Value: groupPlan, Label: "Plan"},
	{Value: groupStatus, Label: "Cleanup status"},
}

// worktreesAppView is the page's data.
type worktreesAppView struct {
	// GroupBy is the list's Group by control, and Group the ?group= the
	// page's links and its selection form carry on.
	GroupBy groupControl
	Group   string
	// Groups are the worktrees' sections, in the selection form, and
	// Records the prunable records; each only when it has rows.
	Groups  []wtGroup
	Records *wtGroup
	// Removable are the rows with a checkbox, for the confirmation.
	Removable []wtRow
	// Picked counts the checked rows; AllPicked is true when every one is.
	Picked    int
	AllPicked bool
	// Confirm opens the confirmation without the script (?confirm=1).
	Confirm bool
	// CanRemove is false when aimd has no Cleanup: the page is read-only.
	CanRemove bool
	// Run and PruneRun name this rendering's confirmation and prune form,
	// so a second submit of either is refused.
	Run, PruneRun string
	// AllURL selects every safe row, ClearURL none, and CancelURL closes
	// the confirmation (links, for the page without the script).
	AllURL, ClearURL, CancelURL string
	// Mains counts the main checkouts left out; Shown the rows listed.
	Mains, Shown int
	// Note says why verdicts are missing (pending, failed, not wired).
	Note string
	// Log is the removal log, newest first; LogError says why it's
	// missing.
	Log      []wtLogEntry
	LogError string
}

// wtGroup is one section of rows.
type wtGroup struct {
	listGroup
	Title string
	Rows  []wtRow
	// involved is true when a row is on a plan or a monitored session's;
	// rank orders Plan grouping's sections.
	involved bool
	rank     int
}

// wtRow is one worktree.
type wtRow struct {
	// ID names it in URLs and forms (wtID).
	ID string
	// Path is shown (~, elided), PathFull in its tooltip.
	Path, PathFull string
	// Branch is the branch, or "detached at <sha>".
	Branch string
	Reason string
	// Pill and Tone are the verdict's label and color class.
	Pill, Tone         string
	Checkable, Checked bool
	// Pick is the confirmation's value for it: id, HEAD and branch as
	// shown, which must still hold when it is removed.
	Pick string
}

// wtLogEntry is one entry of the removal log.
type wtLogEntry struct {
	When, Stamp string
	// Verb is what happened: removed, skipped, couldn't remove, cleared.
	Verb           string
	Path, PathFull string
	// Deleted is the branch deleted with it; Kept a branch left in place.
	Deleted, Kept string
	Note          string
	Tone          string
}

// wtID names a worktree in URLs and forms: a hash of its path, so a
// request never carries a path.
func wtID(path string) string {
	sum := sha256.Sum256([]byte("aim-worktree\x00" + worktrees.PathKey(path)))
	return hex.EncodeToString(sum[:8])
}

// wtNewRun returns a fresh confirmation name.
func wtNewRun() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func fillWorktreesApp(s *Server, r *http.Request, sn *snapshot, d *appData) error {
	ctx := r.Context()
	q := r.URL.Query()
	v := &worktreesAppView{CanRemove: s.opts.Cleanup != nil, Run: wtNewRun(), PruneRun: wtNewRun()}

	var log []store.Removal
	if s.opts.Cleanup != nil {
		var err error
		if log, err = s.opts.Cleanup.Log(ctx); err != nil {
			s.log.Error("removal log", "err", err)
			v.LogError = "aimd couldn't read the removal log; see aimd.log."
		}
	}
	verdicts := s.verdicts(ctx)
	switch {
	case !verdicts.Wired:
		v.Note = "This aimd doesn't work out which worktrees are safe to remove, so nothing can be removed here."
	case verdicts.Pending:
		v.Note = "aimd is working out which worktrees are safe to remove; this list fills in shortly."
	case verdicts.Error != "":
		v.Note = verdicts.Error
	}

	// A worktree removed from here stays in the store until the next scan
	// (seconds): leave it out meanwhile.
	removed := map[string]time.Time{}
	for _, e := range log {
		k := worktrees.PathKey(e.Path)
		if e.Action == store.RemovalRemoved && e.At.After(removed[k]) {
			removed[k] = e.At
		}
	}
	picks := map[string]bool{}
	for _, id := range q["pick"] {
		picks[id] = true
	}
	all := truthy(q.Get("all"))
	group, kept, ctl := groupBy(r, d, "worktrees", wtGroupChoices)
	v.GroupBy, v.Group = ctl, kept
	var plans wtPlans
	if group == groupPlan || group == groupRepo {
		plans = s.wtPlansOf(ctx, sn)
	}
	sections := map[string]*wtGroup{}
	var order []*wtGroup
	records := 0 // prunable records, whatever the search
	for _, w := range sn.worktrees {
		if at, ok := removed[worktrees.PathKey(w.Path)]; ok && !w.LastSeen.After(at) {
			continue
		}
		row, verdict := s.wtRowOf(w, verdicts)
		if verdict == "" {
			v.Mains++
			continue
		}
		if verdict == wtPrunable {
			records++
		}
		if !wtMatches(row, d.Query) {
			continue
		}
		row.Checkable = row.Checkable && v.CanRemove
		row.Checked = row.Checkable && (all || picks[row.ID])
		v.Shown++
		if verdict == wtPrunable {
			if v.Records == nil {
				v.Records = &wtGroup{listGroup: listGroup{Key: "records", Open: true, Force: d.Query != ""}, Title: wtGroupTitles[wtPrunable]}
			}
			v.Records.Rows = append(v.Records.Rows, row)
			continue
		}
		g := plans.section(group, w, verdict)
		if have := sections[g.Key]; have != nil {
			g = have
		} else {
			g.Force = d.Query != ""
			sections[g.Key] = g
			order = append(order, g)
		}
		g.Rows = append(g.Rows, row)
		if group != groupStatus {
			// Repo and Plan grouping fold what is on no plan and no
			// monitored session's.
			g.involved = g.involved || plans.involved(w)
			g.Open = g.involved
		}
	}
	slices.SortStableFunc(order, func(a, c *wtGroup) int {
		if group == groupRepo {
			return byInvolvedThenName(a.involved, a.Title, c.involved, c.Title)
		}
		return cmp.Compare(a.rank, c.rank)
	})
	byPath := func(a, b wtRow) int {
		return strings.Compare(worktrees.PathKey(a.PathFull), worktrees.PathKey(b.PathFull))
	}
	for _, g := range order {
		slices.SortStableFunc(g.Rows, byPath)
		v.Groups = append(v.Groups, *g)
		for _, row := range g.Rows {
			if row.Checkable {
				v.Removable = append(v.Removable, row)
				if row.Checked {
					v.Picked++
				}
			}
		}
	}
	if v.Records != nil {
		slices.SortStableFunc(v.Records.Rows, byPath)
	}
	v.AllPicked = len(v.Removable) > 0 && v.Picked == len(v.Removable)
	v.Confirm = truthy(q.Get("confirm")) && v.Picked > 0
	v.AllURL = worktreesURL(d.Query, kept, url.Values{"all": {"1"}})
	v.ClearURL = worktreesURL(d.Query, kept, nil)
	cancel := url.Values{}
	for _, row := range v.Removable {
		if row.Checked {
			cancel.Add("pick", row.ID)
		}
	}
	v.CancelURL = worktreesURL(d.Query, kept, cancel)
	for _, e := range log {
		v.Log = append(v.Log, wtLogEntryOf(s, sn.now, e))
	}

	parts := []string{
		strconv.Itoa(len(sn.worktrees)) + " worktrees",
		strconv.Itoa(s.safeToRemoveCount(ctx, sn)) + " safe to remove",
	}
	if records > 0 {
		parts = append(parts, wtNouns(records, "record")+" whose folder is gone")
	}
	d.Summary = strings.Join(parts, " · ")
	d.View = v
	return nil
}

// wtPlans says which registered plan each worktree is on and whether a
// monitored session works in it, for Repo and Plan grouping. A worktree is
// on a plan when a board links it (the matcher links a package's worktrees
// through its branch and PR too), links its branch or a PR on its branch,
// or its branch's PR is on one (sn.mon.PRWait, else its Plan: line); else
// the plan of the monitored session that works in it, if any. In "all"
// mode, where every session is monitored, only plans count.
type wtPlans struct {
	all   bool
	plans []api.Plan
	mon   *monitor.Set
	prs   prIndex
	// names maps a main checkout (worktrees.PathKey) to its GitHub name.
	names map[string]string
	// byPath and byBranch are the plans boards link worktrees to, by path
	// (worktrees.PathKey) and by branch (branchRef), as indexes in plans.
	byPath, byBranch map[string]int
	// byFolder and bySession are the monitored sessions' plans (an index,
	// or -1 for none) by the worktree they work in (worktrees.PathKey) and
	// by session id.
	byFolder, bySession map[string]int
}

// branchRef keys a repo's branch: lower-case nameWithOwner, NUL, branch.
func branchRef(repo, branch string) string { return strings.ToLower(repo) + "\x00" + branch }

// wtPlansOf reads what wtPlans needs from the snapshot and the plans.
func (s *Server) wtPlansOf(ctx context.Context, sn *snapshot) wtPlans {
	x := wtPlans{all: sn.mon.All(), plans: s.registeredPlans(ctx, sn), mon: sn.mon, prs: indexPRs(sn.prs),
		names: map[string]string{}, byPath: map[string]int{}, byBranch: map[string]int{},
		byFolder: map[string]int{}, bySession: map[string]int{}}
	if s.opts.Repos != nil {
		for path, name := range s.opts.Repos.RepoNames() {
			if name != "" {
				x.names[worktrees.PathKey(path)] = name
			}
		}
	}
	first := func(m map[string]int, k string, i int) {
		if _, ok := m[k]; !ok {
			m[k] = i
		}
	}
	for i, p := range x.plans {
		for _, pk := range p.Packages {
			for _, l := range pk.Links {
				switch l.Kind {
				case store.LinkWorktree:
					path := l.Ref
					if l.Worktree != nil {
						path = cmp.Or(l.Worktree.Path, path)
					}
					if path != "" {
						first(x.byPath, worktrees.PathKey(path), i)
					}
				case store.LinkBranch:
					if repo, br, ok := strings.Cut(l.Ref, ":"); ok && br != "" {
						first(x.byBranch, branchRef(repo, br), i)
					}
				case store.LinkPR:
					if l.PR != nil && l.PR.HeadBranch != "" {
						first(x.byBranch, branchRef(l.PR.Repo, l.PR.HeadBranch), i)
					}
				}
			}
		}
	}
	if x.all {
		return x
	}
	for _, row := range sn.sessions {
		if row.Headless || !sn.mon.Monitored(row) {
			continue
		}
		i := planIndex(x.plans, monitor.PlanOf(sn.mon.Why(row)))
		if path := cmp.Or(row.WorktreePath, row.Cwd); path != "" {
			if have, ok := x.byFolder[worktrees.PathKey(path)]; !ok || have < 0 {
				x.byFolder[worktrees.PathKey(path)] = i
			}
		}
		if row.SessionID != "" {
			first(x.bySession, row.SessionID, i)
		}
	}
	return x
}

// of is the plan a worktree is on (an index in x.plans, or -1) and
// whether a monitored session's work is in it (see wtPlans).
func (x wtPlans) of(w store.Worktree) (plan int, monitored bool) {
	plan = -1
	if i, ok := x.byPath[worktrees.PathKey(w.Path)]; ok {
		plan = i
	}
	if repo := x.names[worktrees.PathKey(w.MainRepo)]; plan < 0 && repo != "" && w.Branch != "" {
		if i, ok := x.byBranch[branchRef(repo, w.Branch)]; ok {
			plan = i
		} else if p := x.prs.best(repo, w.Branch); p != nil {
			v := x.mon.PRWait(*p)
			if plan = planIndex(x.plans, v.Plan); plan < 0 {
				plan = planIndex(x.plans, p.PlanSlug)
			}
			monitored = !x.all && v.Monitored
		}
	}
	if x.all {
		return plan, false
	}
	i, ok := x.byFolder[worktrees.PathKey(w.Path)]
	if !ok && w.SessionID != "" {
		i, ok = x.bySession[w.SessionID]
	}
	if ok {
		monitored = true
		if plan < 0 {
			plan = i
		}
	}
	return plan, monitored
}

// involved reports whether a worktree is on a plan or a monitored
// session's: its section starts open.
func (x wtPlans) involved(w store.Worktree) bool {
	plan, monitored := x.of(w)
	return plan >= 0 || monitored
}

// repo names a worktree's repo: its GitHub name, else its main checkout's
// folder; key tells two local repos of one folder name apart.
func (x wtPlans) repo(w store.Worktree) (key, name string) {
	if n := x.names[worktrees.PathKey(w.MainRepo)]; n != "" {
		return repoKey(n), n
	}
	return repoKey(worktrees.PathKey(w.MainRepo)), filepath.Base(w.MainRepo)
}

// section is the section a worktree of the verdict goes in under the
// Group by (a new one; the caller merges it with one it has by Key).
func (x wtPlans) section(group string, w store.Worktree, verdict string) *wtGroup {
	switch group {
	case groupStatus:
		return &wtGroup{listGroup: listGroup{Key: "status:" + verdict, Open: true}, Title: wtGroupTitles[verdict],
			rank: slices.Index(wtVerdictOrder, verdict)}
	case groupPlan:
		plan, monitored := x.of(w)
		switch {
		case plan >= 0:
			p := x.plans[plan]
			return &wtGroup{listGroup: listGroup{Key: planKey(p.Slug)}, Title: cmp.Or(p.Title, p.Slug), rank: plan}
		case monitored:
			return &wtGroup{listGroup: listGroup{Key: watchedKey}, Title: noPlanLabel, rank: len(x.plans)}
		}
		return &wtGroup{listGroup: listGroup{Key: notOnPlanKey}, Title: notOnPlanLabel, rank: len(x.plans) + 1}
	}
	key, name := x.repo(w)
	return &wtGroup{listGroup: listGroup{Key: key}, Title: name}
}

// wtRowOf is a worktree's row and the group it goes in ("" for a main
// checkout, which isn't listed).
func (s *Server) wtRowOf(w store.Worktree, v verdictState) (wtRow, string) {
	row := wtRow{
		ID:       wtID(w.Path),
		Path:     s.displayPath(w.Path),
		PathFull: s.shortPath(w.Path),
		Branch:   w.Branch,
		Pick:     wtID(w.Path) + ":" + w.HeadSHA + ":" + w.Branch,
	}
	if w.Branch == "" {
		row.Branch = "detached at " + shortSHA(w.HeadSHA)
	}
	if worktrees.SamePath(w.Path, w.MainRepo) {
		return row, ""
	}
	vd, ok := v.get(w.Path)
	if ok {
		row.Reason = strings.Join(vd.Reasons, "; ")
	}
	switch {
	case ok && vd.Category == string(prune.Main):
		return row, ""
	case ok && vd.Removable && vd.Category == string(prune.Safe):
		row.Pill, row.Tone, row.Checkable = "Safe to remove", "tone-ok", true
		return row, wtSafe
	case ok && vd.Removable && vd.Category == string(prune.Scratch):
		row.Pill, row.Tone, row.Checkable = "Scratch", "tone-info", true
		return row, wtScratch
	case ok && vd.Category == string(prune.Locked), !ok && w.Locked:
		row.Pill, row.Tone = "Locked", "tone-quiet"
		if !ok {
			row.Reason = "locked: " + cmp.Or(w.LockedReason, "no reason given")
		}
		return row, wtLocked
	case ok && vd.Category == string(prune.Prunable), !ok && w.PrunableReason != "":
		row.Pill, row.Tone = "Folder gone", "tone-quiet"
		if !ok {
			row.Reason = w.PrunableReason
		}
		return row, wtPrunable
	}
	row.Pill, row.Tone = "Not safe", "tone-warn"
	switch {
	case !ok && v.Pending:
		row.Reason = "not checked yet"
	case !ok:
		row.Reason = "no verdict"
	case vd.Category == string(prune.SafeClosed):
		row.Reason += "; only aim prune --include-closed removes it"
	case row.Reason == "":
		row.Reason = vd.Category
	}
	return row, wtNotSafe
}

// wtMatches reports whether a row matches the search box.
func wtMatches(row wtRow, q string) bool {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return true
	}
	for _, f := range []string{row.PathFull, row.Branch, row.Reason, row.Pill} {
		if strings.Contains(strings.ToLower(f), q) {
			return true
		}
	}
	return false
}

// worktreesURL is the page with the search box's q, the Group by the URL
// has (group, or ""), and more.
func worktreesURL(q, group string, more url.Values) string {
	vals := url.Values{}
	for k, v := range more {
		vals[k] = v
	}
	if q != "" {
		vals.Set("q", q)
	}
	if group != "" {
		vals.Set("group", group)
	}
	if len(vals) == 0 {
		return "/app/worktrees"
	}
	return "/app/worktrees?" + vals.Encode()
}

// wtLogEntryOf is a log entry as the page shows it.
func wtLogEntryOf(s *Server, now time.Time, e store.Removal) wtLogEntry {
	l := wtLogEntry{
		When: clock(now, e.At), Stamp: stamp(now, e.At),
		Path: s.displayPath(e.Path), PathFull: s.shortPath(e.Path),
		Note: e.Detail,
	}
	switch e.Action {
	case store.RemovalRemoved:
		l.Verb, l.Tone = "removed", "tone-ok"
		switch {
		case e.BranchDeleted:
			l.Deleted = e.Branch
		case e.Branch != "":
			l.Kept = e.Branch
		case e.Head != "":
			l.Note = strings.TrimPrefix(l.Note+"; ", "; ") + "detached at " + shortSHA(e.Head)
		}
	case store.RemovalPruned:
		l.Verb, l.Tone = "cleared the record of", "tone-quiet"
		l.Note = "git worktree prune: " + e.Detail
	case store.RemovalSkipped:
		l.Verb, l.Tone = "skipped", "tone-warn"
	default:
		l.Verb, l.Tone = "couldn't remove", "tone-warn"
	}
	return l
}

// ---- Form posts ----

// Bounds on a post.
const (
	maxPicks  = 500
	maxBranch = 255
)

// worktreesReturn is where a post's answer goes: the page, with its search
// and Group by but without the selection or the confirmation.
func worktreesReturn(r *http.Request) string {
	u, err := url.Parse(appReturn(r))
	q, group := "", ""
	if err == nil {
		q, group = u.Query().Get("q"), u.Query().Get("group")
	}
	if !slices.ContainsFunc(wtGroupChoices, func(c groupChoice) bool { return c.Value == group }) {
		group = ""
	}
	return worktreesURL(q, group, nil)
}

// worktreesRemove is POST /app/worktrees/remove: the confirmation's
// worktrees, each a field wt=<id>:<head>:<branch>, plus run. Each id must
// name a worktree aimd lists, not a main checkout; Cleanup then checks each
// one again and removes only those still safe.
func (s *Server) worktreesRemove(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.appRefuse(w, r, http.StatusBadRequest, "/app/worktrees", "That request couldn't be read; nothing was removed.")
		return
	}
	ret := worktreesReturn(r)
	if s.opts.Cleanup == nil {
		s.appRefuse(w, r, http.StatusNotImplemented, ret, "This aimd can't remove worktrees.")
		return
	}
	vals := r.PostForm["wt"]
	switch {
	case r.PostFormValue("run") == "":
		s.appRefuse(w, r, http.StatusBadRequest, ret, "This form has no confirmation; reload the page. Nothing was removed.")
		return
	case len(vals) == 0:
		s.appRefuse(w, r, http.StatusBadRequest, ret, "Nothing was selected, so nothing was removed.")
		return
	case len(vals) > maxPicks:
		s.appRefuse(w, r, http.StatusBadRequest, ret, "Too many worktrees at once; nothing was removed.")
		return
	}
	rows, err := s.opts.Store.ListWorktrees(r.Context(), store.WorktreeFilter{})
	if err != nil {
		s.log.Error("worktrees remove", "err", err)
		s.appRefuse(w, r, http.StatusInternalServerError, ret, "aimd couldn't read its database; nothing was removed.")
		return
	}
	byID := map[string]store.Worktree{}
	for _, row := range rows {
		byID[wtID(row.Path)] = row
	}
	var picks []CleanupPick
	refused := 0
	for _, v := range vals {
		id, head, branch, ok := parsePick(v)
		if !ok {
			s.appRefuse(w, r, http.StatusBadRequest, ret, "That request doesn't name worktrees the way this page does; nothing was removed.")
			return
		}
		row, found := byID[id]
		if !found || worktrees.SamePath(row.Path, row.MainRepo) {
			refused++
			continue
		}
		picks = append(picks, CleanupPick{Repo: row.MainRepo, Pick: prune.Pick{Path: row.Path, Head: head, Branch: branch}})
	}
	if len(picks) == 0 {
		s.appRefuse(w, r, http.StatusConflict, ret, "None of those is a worktree aimd lists; nothing was removed.")
		return
	}
	entries, err := s.opts.Cleanup.Remove(r.Context(), r.PostFormValue("run"), picks)
	if errors.Is(err, ErrConfirmationUsed) {
		s.appRefuse(w, r, http.StatusConflict, ret, "That confirmation was sent already, so nothing more was done. The removal log has what happened.")
		return
	}
	if err != nil {
		s.log.Error("worktrees remove", "err", err)
		s.appRefuse(w, r, http.StatusInternalServerError, ret, "The removal failed: "+err.Error())
		return
	}
	s.appReply(w, r, ret, removalToast(entries, refused))
}

// worktreesPrune is POST /app/worktrees/prune: the records the page listed,
// each a field rec=<id>, plus run. In each of their repos `git worktree
// prune` runs only when every record it would clear is one of them.
func (s *Server) worktreesPrune(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.appRefuse(w, r, http.StatusBadRequest, "/app/worktrees", "That request couldn't be read; nothing was pruned.")
		return
	}
	ret := worktreesReturn(r)
	if s.opts.Cleanup == nil {
		s.appRefuse(w, r, http.StatusNotImplemented, ret, "This aimd can't prune worktree records.")
		return
	}
	ids := r.PostForm["rec"]
	if r.PostFormValue("run") == "" {
		s.appRefuse(w, r, http.StatusBadRequest, ret, "This form has no confirmation; reload the page. Nothing was pruned.")
		return
	}
	if len(ids) == 0 || len(ids) > maxPicks {
		s.appRefuse(w, r, http.StatusBadRequest, ret, "No records were listed, so nothing was pruned.")
		return
	}
	rows, err := s.opts.Store.ListWorktrees(r.Context(), store.WorktreeFilter{})
	if err != nil {
		s.log.Error("worktrees prune", "err", err)
		s.appRefuse(w, r, http.StatusInternalServerError, ret, "aimd couldn't read its database; nothing was pruned.")
		return
	}
	byID := map[string]store.Worktree{}
	for _, row := range rows {
		byID[wtID(row.Path)] = row
	}
	var recs []CleanupPick
	for _, id := range ids {
		if row, ok := byID[id]; ok && !worktrees.SamePath(row.Path, row.MainRepo) {
			recs = append(recs, CleanupPick{Repo: row.MainRepo, Pick: prune.Pick{Path: row.Path}})
		}
	}
	if len(recs) == 0 {
		s.appRefuse(w, r, http.StatusConflict, ret, "None of those is a record aimd lists; nothing was pruned.")
		return
	}
	entries, err := s.opts.Cleanup.Prune(r.Context(), r.PostFormValue("run"), recs)
	if errors.Is(err, ErrConfirmationUsed) {
		s.appRefuse(w, r, http.StatusConflict, ret, "That was sent already, so nothing more was done. The removal log has what happened.")
		return
	}
	if err != nil {
		s.log.Error("worktrees prune", "err", err)
		s.appRefuse(w, r, http.StatusInternalServerError, ret, "Pruning failed: "+err.Error())
		return
	}
	s.appReply(w, r, ret, pruneToast(entries))
}

// parsePick reads a wt field: <id>:<head>:<branch>, with a 16-digit hex id,
// a full hex SHA (or nothing) and a branch name ("" when detached).
func parsePick(v string) (id, head, branch string, ok bool) {
	id, rest, ok1 := strings.Cut(v, ":")
	head, branch, ok2 := strings.Cut(rest, ":")
	switch {
	case !ok1 || !ok2, len(id) != 16 || !wtIsHex(id),
		head != "" && (len(head) != 40 && len(head) != 64 || !wtIsHex(head)),
		len(branch) > maxBranch, strings.ContainsFunc(branch, func(r rune) bool { return r < ' ' || r == 0x7f }):
		return "", "", "", false
	}
	return id, head, branch, true
}

func wtIsHex(s string) bool {
	return s != "" && strings.Trim(s, "0123456789abcdef") == ""
}

// removalToast says what a removal did: how many were removed and
// branches deleted, and each one skipped or failed, with why. It has no
// Undo: a removal can't be undone, and the toast says so.
func removalToast(entries []store.Removal, refused int) *appToast {
	removed, branches := 0, 0
	var notes []string
	for _, e := range entries {
		name := filepath.Base(e.Path)
		switch e.Action {
		case store.RemovalRemoved:
			removed++
			if e.BranchDeleted {
				branches++
			} else if e.Detail != "" {
				notes = append(notes, name+": "+e.Detail)
			}
		case store.RemovalSkipped:
			notes = append(notes, "skipped "+name+": "+e.Detail)
		default:
			notes = append(notes, "couldn't remove "+name+": "+e.Detail)
		}
	}
	if refused > 0 {
		notes = append(notes, wtNouns(refused, "pick")+" not a worktree aimd lists, left alone")
	}
	var text string
	if removed > 0 {
		text = "Removed " + wtNouns(removed, "worktree")
		if branches > 0 {
			text += " and deleted " + wtNouns(branches, "branch")
		}
		text += ". There's no Undo: a removal can't be undone."
	} else {
		text = "Nothing was removed."
	}
	t := &appToast{Text: text}
	if len(notes) > 0 {
		t.Text += " " + wtUpperFirst(strings.Join(notes, "; ")) + "."
		t.Kind = "error"
	}
	return t
}

// pruneToast says what `git worktree prune` cleared, or why it didn't run.
func pruneToast(entries []store.Removal) *appToast {
	pruned := 0
	var notes []string
	for _, e := range entries {
		if e.Action == store.RemovalPruned {
			pruned++
			continue
		}
		notes = append(notes, filepath.Base(e.Path)+": "+e.Detail)
	}
	t := &appToast{Text: "Cleared " + wtNouns(pruned, "record") + " with git worktree prune."}
	if len(notes) > 0 {
		t.Text += " " + wtUpperFirst(strings.Join(notes, "; ")) + "."
		t.Kind = "error"
	}
	return t
}

// wtNouns is "1 worktree", "2 worktrees", "2 branches".
func wtNouns(n int, noun string) string {
	if n != 1 {
		if strings.HasSuffix(noun, "ch") || strings.HasSuffix(noun, "s") {
			noun += "es"
		} else {
			noun += "s"
		}
	}
	return strconv.Itoa(n) + " " + noun
}

func wtUpperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
