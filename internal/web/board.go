package web

// A plan's board facts, as the app's plan page (app_plans.go) shows them:
// the progress bar and its legend, the alerts and parser warnings, the
// Unmatched lane, and each package's card facts and links. They read the
// plan service (Options.Plans): the board is the last state pass's
// api.Plan, with each card's PRs and agents as the store has them now
// (fresh.go), so a sessions or prs change shows before the next pass.
//
// Plan files are untrusted text like everything else (titles, notes,
// warnings, lane rules, branch names): the templates never mark anything
// safe.

import (
	"cmp"
	"errors"
	"net/http"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/gravexa-llc/ai-manager/internal/sessions"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// errNoPage is what a page's fill returns for a page that doesn't exist
// (an unknown plan): the page answers 404 with its message.
type errNoPage struct{ msg string }

func (e errNoPage) Error() string { return e.msg }

// anchorRE keeps the characters an element id or URL fragment may hold.
var anchorRE = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// pkgAnchor is a package's element id.
func pkgAnchor(id string) string { return "pkg-" + anchorRE.ReplaceAllString(id, "_") }

// ---- Lifecycles ----

// boardColumns are the lifecycles in order; Dropped only shows when a
// package is dropped.
var boardColumns = []struct{ key, label string }{
	{"blocked", "Blocked"},
	{"ready", "Ready"},
	{"working", "Working"},
	{"in_review", "In review"},
	{"done", "Done"},
	{"dropped", "Dropped"},
}

// progressOrder is the order of a progress bar's segments.
var progressOrder = []string{"done", "in_review", "working", "ready", "blocked", "dropped"}

func lifecycleLabel(lc string) string {
	for _, c := range boardColumns {
		if c.key == lc {
			return c.label
		}
	}
	return stateLabel(lc)
}

type countView struct {
	Key, Label string
	N          int
}

type segView struct {
	Class, Title string
}

// progress returns a plan's progress bar (one segment per package, done
// first) and its legend (the lifecycles that have packages).
func progress(b api.Plan) (segs []segView, counts []countView, label string) {
	byLC := map[string][]api.Package{}
	for _, p := range b.Packages {
		byLC[p.Lifecycle] = append(byLC[p.Lifecycle], p)
	}
	var parts []string
	for _, lc := range progressOrder {
		for _, p := range byLC[lc] {
			segs = append(segs, segView{Class: "lc-" + lc, Title: p.ID + " " + p.Title + ": " + strings.ToLower(lifecycleLabel(lc))})
		}
		if n := len(byLC[lc]); n > 0 {
			counts = append(counts, countView{Key: lc, Label: strings.ToLower(lifecycleLabel(lc)), N: n})
			parts = append(parts, strconv.Itoa(n)+" "+strings.ToLower(lifecycleLabel(lc)))
		}
	}
	return segs, counts, strings.Join(parts, ", ")
}

// ---- The plan ----

type pkgRef struct {
	ID, Title, Anchor string
}

type warningView struct {
	Kind    string
	Line    int
	ID      string
	Message string
}

type alertView struct {
	Kind, Label, Class string
	Message            string
	Packages           []pkgRef
	Since              string
}

// planView is a plan's own facts: its header, progress, alerts, parser
// warnings and Unmatched lane.
type planView struct {
	Slug, Title    string
	Path, PathFull string
	Repos          []string
	Registered     string
	RegisteredAt   string
	Derived        string
	Total          int

	Segs     []segView
	Counts   []countView
	BarLabel string

	Alerts   []alertView
	Warnings []warningView

	Unmatched []unmatchedView
	// Options are the packages, for the Unmatched lane's link forms.
	Options []pkgOption
}

type pkgOption struct{ ID, Label string }

type badgeView struct {
	Text, Class, Title string
}

type factView struct {
	Text, Class, Title string
}

type linkView struct {
	Kind, KindLabel string
	// Ref is the link's target, for its Unlink form.
	Ref string
	// Label is the main text; URL makes it a link, Code shows it as code.
	Label, URL, Title string
	Code              bool
	// CI is a PR's CI state, shown as a dot of class CIClass.
	CI, CIClass string
	Facts       []factView
	// How says how the link was made; Low marks a low-confidence link
	// (drawn dashed).
	How, HowTitle string
	Low           bool
	// Go is a live agent link's Go button (golink.go).
	Go goButton
}

// cardView is a package's facts beyond the plan file's: how long it has
// been in its lifecycle, its links, and whether Mark done applies.
type cardView struct {
	ID, Title      string
	Lifecycle      string
	LifecycleLabel string
	// Since is how long it has been in its lifecycle, and Was the
	// lifecycle before, when aimd saw it change.
	Since, SinceAt, Was string
	Cycle               string
	Links               []linkView
	DoneInPlan          bool
	DoneManual          bool
	// CanMarkDone is true for a package with no PR that isn't done.
	CanMarkDone bool
}

type unmatchedView struct {
	Kind, KindLabel, Ref string
	Repo, Branch         string
	Label, URL           string
	Title                string
	State, StateClass    string
	Note                 string
}

func (s *Server) planViewOf(sn *snapshot, b api.Plan) *planView {
	v := &planView{
		Slug: b.Slug, Title: cmp.Or(b.Title, b.Slug),
		Path: s.displayPath(b.Path), PathFull: b.Path, Repos: b.Repos,
		Registered: ago(sn.now, b.CreatedAt), RegisteredAt: stamp(b.CreatedAt),
		Derived: ago(sn.now, b.DerivedAt), Total: b.Total,
	}
	v.Segs, v.Counts, v.BarLabel = progress(b)

	byID := map[string]api.Package{}
	for _, p := range b.Packages {
		byID[strings.ToUpper(p.ID)] = p
		v.Options = append(v.Options, pkgOption{ID: p.ID, Label: p.ID + " " + p.Title})
	}
	ref := func(id string) pkgRef {
		r := pkgRef{ID: id, Anchor: pkgAnchor(id)}
		if p, ok := byID[strings.ToUpper(id)]; ok {
			r.ID, r.Title = p.ID, p.Title
		}
		return r
	}
	for _, a := range b.Alerts {
		av := alertView{Kind: a.Kind, Label: alertLabels[a.Kind], Class: alertClasses[a.Kind], Message: a.Message}
		if av.Label == "" {
			av.Label = stateLabel(a.Kind)
		}
		if !a.Since.IsZero() {
			av.Since = ago(sn.now, a.Since)
		}
		for _, id := range a.Packages {
			av.Packages = append(av.Packages, ref(id))
		}
		v.Alerts = append(v.Alerts, av)
	}
	for _, w := range b.Warnings {
		v.Warnings = append(v.Warnings, warningView{Kind: w.Kind, Line: w.Line, ID: w.ID, Message: w.Message})
	}
	for _, u := range b.Unmatched {
		v.Unmatched = append(v.Unmatched, unmatchedOf(u))
	}
	return v
}

var alertLabels = map[string]string{
	"unlocked":       "dependencies unlocked",
	"stalled":        "stalled",
	"lane_collision": "lane collision",
	"off_plan":       "off-plan",
}

var alertClasses = map[string]string{
	"unlocked":       "ok",
	"stalled":        "warn",
	"lane_collision": "bad",
	"off_plan":       "warn",
}

// badgeClasses colour a package's badges.
var badgeClasses = map[string]string{
	"needs_you":         "need",
	"ci_failing":        "bad",
	"conflict":          "bad",
	"waiting_on_you":    "need",
	"changes_requested": "bad",
	"review_requested":  "need",
	"draft":             "quiet",
	"stalled":           "warn",
	"quiet":             "warn",
	"collision":         "bad",
	"lane_conflict":     "bad",
	"low_confidence":    "warn",
	"you":               "need",
}

// linkKindOrder is the order of a package's links: who works on it,
// where, then what came of it.
var linkKindOrder = map[string]int{
	store.LinkAgent:        0,
	store.LinkWorktree:     1,
	store.LinkBranch:       2,
	store.LinkPR:           3,
	store.LinkCloudSession: 4,
}

func (s *Server) cardOf(sn *snapshot, p api.Package) cardView {
	c := cardView{
		ID: p.ID, Title: p.Title, Lifecycle: p.Lifecycle, LifecycleLabel: lifecycleLabel(p.Lifecycle),
		DoneInPlan: p.DoneInPlan, DoneManual: p.DoneManual,
	}
	// A lifecycle with no previous one is only as old as the first state
	// pass that saw the package: not worth an age.
	if !p.Since.IsZero() && p.PrevLifecycle != "" {
		c.Since, c.SinceAt = ago(sn.now, p.Since), stamp(p.Since)
		c.Was = strings.ToLower(lifecycleLabel(p.PrevLifecycle))
	}
	if len(p.Cycle) > 0 {
		c.Cycle = strings.Join(p.Cycle, " → ")
	}
	hasPR := false
	links := slices.Clone(p.Links)
	slices.SortStableFunc(links, func(a, b api.Link) int { return cmp.Compare(linkKindOrder[a.Kind], linkKindOrder[b.Kind]) })
	for _, l := range links {
		if l.Kind == store.LinkPR {
			hasPR = true
		}
		c.Links = append(c.Links, s.linkViewOf(sn, l))
	}
	c.CanMarkDone = !hasPR && !p.DoneManual && !p.DoneInPlan && p.Lifecycle != "done"
	return c
}

var linkKindLabels = map[string]string{
	store.LinkAgent:        "agent",
	store.LinkWorktree:     "worktree",
	store.LinkBranch:       "branch",
	store.LinkPR:           "PR",
	store.LinkCloudSession: "cloud",
}

var methodLabels = map[string]string{
	store.MatchSpawn:    "spawn",
	store.MatchPlanLine: "Plan: line",
	store.MatchTitle:    "PR title",
	store.MatchBranch:   "branch name",
	store.MatchManual:   "manual",
}

var methodTitles = map[string]string{
	store.MatchSpawn:    "linked when the agent was spawned for this package",
	store.MatchPlanLine: "the PR body's Plan: line names this package",
	store.MatchTitle:    "the PR title ends with this package's (ID)",
	store.MatchBranch:   "the branch name names this package",
	store.MatchManual:   "linked by hand on the dashboard",
}

// targetLabel is a short name for a link target, for "via …".
func targetLabel(t api.Target) string {
	switch t.Kind {
	case store.LinkPR:
		if i := strings.LastIndexByte(t.Ref, '#'); i >= 0 {
			return "PR #" + t.Ref[i+1:]
		}
	case store.LinkBranch:
		if _, br, ok := strings.Cut(t.Ref, ":"); ok {
			return "branch " + br
		}
	case store.LinkWorktree:
		return "worktree " + filepath.Base(t.Ref)
	case store.LinkAgent:
		return "agent"
	case store.LinkCloudSession:
		return "cloud session"
	}
	return t.Kind + " " + t.Ref
}

// how says how a link was made: its method and confidence, and what it
// was reached through.
func how(l api.Link) (text, title string) {
	text = cmp.Or(methodLabels[l.Method], l.Method)
	title = cmp.Or(methodTitles[l.Method], l.Method)
	if l.Confidence != "" {
		text += " · " + l.Confidence
		title += "; confidence " + l.Confidence
	}
	if l.Confidence == store.ConfidenceLow {
		title += " (a branch-name token: check it)"
	}
	if l.Via != nil {
		text = "via " + targetLabel(*l.Via) + " (" + text + ")"
		title = "reached through " + targetLabel(*l.Via) + ", which is linked: " + title
	}
	return text, title
}

func (s *Server) linkViewOf(sn *snapshot, l api.Link) linkView {
	v := linkView{Kind: l.Kind, KindLabel: cmp.Or(linkKindLabels[l.Kind], l.Kind), Ref: l.Ref, Low: l.Confidence == store.ConfidenceLow}
	v.How, v.HowTitle = how(l)
	fact := func(text, class, title string) {
		if text != "" {
			v.Facts = append(v.Facts, factView{Text: text, Class: class, Title: title})
		}
	}
	switch l.Kind {
	case store.LinkAgent:
		a := l.Agent
		v.Label = agentLabel(a, l.Ref)
		v.Go = s.goButtonOfAgent(sn, a)
		if a == nil {
			fact("not seen yet", "quiet", "")
			break
		}
		fact(a.Kind, "", "")
		switch {
		case !a.Live:
			fact("ended", "quiet", "")
		case a.NeedsYou:
			text := "needs you"
			if a.Reason != "" {
				text += ": " + needReason(a.Reason)
			}
			if !a.NeedsYouSince.IsZero() {
				text += " · " + ago(sn.now, a.NeedsYouSince)
			}
			fact(text, "need", "")
		case a.State == "":
			fact("live", "ok", "no state reported yet (the mod reports it)")
		default:
			fact(stateLabel(a.State), stateClass(a.State), "")
		}
		if a.LastAction != "" {
			fact("last: "+a.LastAction, "", "")
		}
		if a.Live && !a.LastEventAt.IsZero() {
			fact("quiet for "+ago(sn.now, a.LastEventAt), "", "last event "+stamp(a.LastEventAt))
		}
		if a.WorktreePath != "" {
			fact("in "+filepath.Base(a.WorktreePath), "", a.WorktreePath)
		}
	case store.LinkWorktree:
		v.Label, v.Title, v.Code = s.displayPath(l.Ref), l.Ref, true
		if w := l.Worktree; w != nil {
			if w.Branch != "" {
				fact(w.Branch, "", "branch")
			} else if w.Detached {
				fact("detached", "quiet", "")
			}
			if !w.RemovedAt.IsZero() {
				fact("removed "+ago(sn.now, w.RemovedAt)+" ago", "quiet", "")
			} else if w.Locked {
				fact("locked", "", "")
			}
			if !w.LastCommitAt.IsZero() {
				fact("last commit "+ago(sn.now, w.LastCommitAt)+" ago", "", stamp(w.LastCommitAt))
			}
		}
	case store.LinkBranch:
		repo, br, ok := strings.Cut(l.Ref, ":")
		v.Label, v.Code = l.Ref, true
		if ok {
			v.Label = br
			fact(repo, "", "")
		}
	case store.LinkPR:
		v.Label = l.Ref
		if i := strings.LastIndexByte(l.Ref, '#'); i >= 0 {
			v.Label, v.Title = "#"+l.Ref[i+1:], l.Ref
		}
		pr := l.PR
		if pr == nil {
			fact("not read from GitHub yet", "quiet", "")
			break
		}
		v.URL = pr.URL
		fact(pr.Title, "title", "")
		state, class := linkPRState(pr)
		fact(state, class, "")
		if pr.State == store.PROpen {
			v.CI, v.CIClass = ciOf(pr.ChecksState)
			review, rclass := reviewOf(pr)
			fact(review, rclass, "")
		}
	case store.LinkCloudSession:
		v.Label, v.URL = "cloud session", l.Ref
	default:
		v.Label = l.Ref
	}
	return v
}

// stateClass is the CSS class of a session state.
func stateClass(state string) string {
	switch state {
	case needsYou:
		return "need"
	case string(sessions.StateError):
		return "bad"
	case string(sessions.StateWorking):
		return "ok"
	}
	return "quiet"
}

// agentLabel names a linked agent: its name, else its id, its job's or its
// session's, shortened.
func agentLabel(a *api.LinkAgent, ref string) string {
	if a == nil {
		return shortRef(ref)
	}
	switch {
	case a.Name != "":
		return a.Name
	case a.AgentID != "":
		return "agent " + shortID(a.AgentID)
	case a.JobID != "":
		return "job " + shortID(a.JobID)
	case a.SessionID != "":
		return "session " + shortID(a.SessionID)
	}
	return shortRef(ref)
}

func shortID(id string) string {
	if len(id) > 10 {
		return id[:10] + "…"
	}
	return id
}

func shortRef(ref string) string {
	if len(ref) > 24 {
		return ref[:24] + "…"
	}
	return ref
}

func linkPRState(pr *api.LinkPR) (string, string) {
	switch pr.State {
	case store.PRMerged:
		return "merged", "ok"
	case store.PRClosed:
		return "closed", "quiet"
	case store.PROpen:
		if pr.Draft {
			return "draft", "quiet"
		}
		return "open", ""
	}
	return strings.ToLower(pr.State), ""
}

// ciOf is a CI dot's label and class.
func ciOf(checks string) (string, string) {
	switch strings.ToUpper(checks) {
	case "", "NONE":
		return "no CI", "none"
	case "SUCCESS":
		return "CI green", "ok"
	case "FAILURE", "ERROR":
		return "CI red", "bad"
	case "PENDING", "EXPECTED":
		return "CI running", "pending"
	}
	return "CI " + strings.ToLower(checks), "pending"
}

// reviewOf is an open PR's review state: the review decision when there
// is one (these accounts never have one today), else what
// mergeStateStatus says.
func reviewOf(pr *api.LinkPR) (string, string) {
	switch pr.ReviewDecision {
	case "APPROVED":
		return "approved", "ok"
	case "CHANGES_REQUESTED":
		return "changes requested", "bad"
	case "REVIEW_REQUIRED":
		return "review requested", "need"
	}
	switch pr.MergeStateStatus {
	case "CLEAN", "HAS_HOOKS":
		return "ready to merge", "need"
	case "DIRTY":
		return "conflict", "bad"
	case "BEHIND":
		return "behind its base", "warn"
	case "BLOCKED":
		return "merge blocked", "warn"
	case "UNSTABLE":
		return "checks not green", "warn"
	}
	return "", ""
}

// githubRepo matches a GitHub nameWithOwner.
var githubRepo = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`)

func unmatchedOf(u api.Unmatched) unmatchedView {
	v := unmatchedView{Kind: u.Kind, KindLabel: cmp.Or(linkKindLabels[u.Kind], u.Kind), Ref: u.Ref,
		Repo: u.Repo, Branch: u.Branch, Title: u.Title, Note: u.Note, Label: u.Ref}
	switch u.Kind {
	case store.LinkPR:
		if u.Number > 0 {
			v.Label = "#" + strconv.Itoa(u.Number)
			if githubRepo.MatchString(u.Repo) {
				v.URL = "https://github.com/" + u.Repo + "/pull/" + strconv.Itoa(u.Number)
			}
		}
		if u.State != "" {
			v.State, v.StateClass = linkPRState(&api.LinkPR{State: u.State})
		}
	case store.LinkBranch:
		if u.Branch != "" {
			v.Label, v.Branch = u.Branch, ""
		}
	}
	return v
}

// ---- Form answers ----

// refusedText is what a refused form shows: the service's message for a
// refusal, or a pointer to the log for aimd's own failure.
func (s *Server) refusedText(r *http.Request, prefix string, err error) string {
	var refusal interface{ HTTPStatus() int }
	if errors.As(err, &refusal) {
		return prefix + err.Error()
	}
	s.log.Error("dashboard form", "path", r.URL.Path, "err", err)
	return prefix + "aimd failed; see aimd.log."
}

// formValue is a trimmed form field.
func formValue(r *http.Request, name string) string { return strings.TrimSpace(r.PostFormValue(name)) }
