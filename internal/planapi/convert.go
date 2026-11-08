package planapi

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/gravexa-llc/ai-manager/internal/inbox"
	"github.com/gravexa-llc/ai-manager/internal/plans"
	"github.com/gravexa-llc/ai-manager/internal/state"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// planJSON turns a plan and its derived state into its board. pkgs are
// the plan's stored packages, for the done markers. Its band is left for
// the reads to fill in (withBands), since its need-you count follows the
// Needs you inbox, which changes between state passes.
func planJSON(p store.Plan, st state.PlanState, pkgs []store.Package, derived time.Time) api.Plan {
	done := map[string]store.Package{}
	for _, pkg := range pkgs {
		done[strings.ToUpper(pkg.PkgID)] = pkg
	}
	b := api.Plan{
		Slug:                p.Slug,
		Title:               p.Title,
		Path:                p.SourcePath,
		Repos:               nonNil(p.Repos),
		RegisteredInSession: p.RegisteredInSession,
		Merges:              cmp.Or(st.Merges, plans.MergesYou),
		CreatedAt:           p.CreatedAt,
		UpdatedAt:           p.UpdatedAt,
		Total:               len(st.Packages),
		Counts:              map[string]int{},
		ReadyToDispatch:     nonNil(st.ReadyToDispatch),
		Packages:            make([]api.Package, 0, len(st.Packages)),
		Lanes:               make([]api.Lane, 0, len(st.Lanes)),
		Unmatched:           []api.Unmatched{},
		HiddenUnmatched:     []api.Unmatched{},
		Warnings:            []api.Warning{},
		Alerts:              []api.Alert{},
		DerivedAt:           derived,
	}
	for _, lc := range state.Lifecycles {
		b.Counts[string(lc)] = st.Counts[lc]
	}
	for _, pkg := range st.Packages {
		jp := packageJSON(p.Slug, pkg)
		if sp, ok := done[strings.ToUpper(pkg.ID)]; ok {
			jp.DoneInPlan, jp.DoneManual = sp.DoneInPlan, sp.DoneManual
			if sp.Provisional {
				provisionalJSON(&jp, p, sp)
				b.Provisional++
			}
		}
		b.Packages = append(b.Packages, jp)
	}
	for _, l := range st.Lanes {
		b.Lanes = append(b.Lanes, api.Lane{Name: l.Name, Packages: nonNil(l.Packages), Rule: l.Rule, SingleWriter: l.SingleWriter})
	}
	for _, u := range st.Unmatched {
		b.Unmatched = append(b.Unmatched, unmatchedJSON(u))
	}
	for _, u := range st.Hidden {
		b.HiddenUnmatched = append(b.HiddenUnmatched, unmatchedJSON(u))
	}
	for _, w := range st.Warnings {
		b.Warnings = append(b.Warnings, api.Warning{Kind: w.Kind, Line: w.Line, ID: w.ID, Message: w.Message})
	}
	for _, a := range st.Alerts {
		b.Alerts = append(b.Alerts, alertJSON(a))
	}
	return b
}

func unmatchedJSON(u state.Unmatched) api.Unmatched {
	return api.Unmatched{
		Target: target(u.Target), Repo: u.Repo, Branch: u.Branch, Number: u.Number,
		Title: u.Title, State: u.State, Note: u.Note, Since: u.Since, New: u.New, HiddenAt: u.HiddenAt,
	}
}

func packageJSON(slug string, p state.PackageState) api.Package {
	jp := api.Package{
		ID: p.ID, Title: p.Title, What: p.What, Repo: p.Repo, Size: p.Size, Owner: p.Owner, Lane: p.Lane,
		Needs: nonNil(p.Needs), NeedsNote: p.NeedsNote, NeedsYou: p.NeedsYou, Yours: p.Yours,
		Lifecycle: string(p.Lifecycle), PrevLifecycle: string(p.PrevLifecycle), Since: p.Since,
		BlockedBy: p.BlockedBy, Cycle: p.Cycle,
		Badges:     make([]api.Badge, 0, len(p.Badges)),
		Links:      make([]api.Link, 0, len(p.Links)),
		Convention: convention(slug, p.ID, p.Title),
	}
	for _, b := range p.Badges {
		jp.Badges = append(jp.Badges, api.Badge{Kind: string(b.Kind), Text: b.Text, Ref: targetPtr(b.Ref), Since: b.Since})
	}
	for _, l := range p.Links {
		jp.Links = append(jp.Links, linkJSON(l))
	}
	return jp
}

// provisionalJSON fills in a provisional package's fields: what named
// it, and the row to paste into the plan file, in the file's own format.
func provisionalJSON(jp *api.Package, p store.Plan, sp store.Package) {
	jp.Provisional = true
	jp.FirstSeen = sp.FirstSeen
	jp.SuggestedRow = plans.SuggestRow([]byte(p.SourceMD), jp.ID, jp.Title, jp.Lifecycle == string(state.Done))
	for _, src := range sp.Sources {
		jp.Sources = append(jp.Sources, api.ProvisionalSource{Kind: src.Kind, Ref: src.Ref, Title: src.Title, FirstSeen: src.FirstSeen, LastSeen: src.LastSeen})
	}
}

func linkJSON(l state.Link) api.Link {
	jl := api.Link{
		Target: target(l.Target), Method: l.Method, Confidence: l.Confidence,
		ID: l.ID, Inferred: l.Inferred, Via: targetPtr(l.Via),
	}
	if pr := l.PR; pr != nil {
		jl.PR = &api.LinkPR{
			Repo: pr.Repo, Number: pr.Number, Title: pr.Title, URL: pr.URL, State: pr.State, Unread: pr.Unread(), Draft: pr.Draft,
			HeadBranch: pr.HeadBranch, ChecksState: pr.ChecksState, Mergeable: pr.Mergeable,
			MergeStateStatus: pr.MergeStateStatus, ReviewDecision: pr.ReviewDecision,
			MergedAt: pr.MergedAt, ClosedAt: pr.ClosedAt,
		}
	}
	if wt := l.Worktree; wt != nil {
		jl.Worktree = &api.LinkWorktree{
			Path: wt.Path, MainRepo: wt.MainRepo, Branch: wt.Branch, Detached: wt.Detached,
			Locked: wt.Locked, LastCommitAt: wt.LastCommitAt, RemovedAt: wt.RemovedAt,
		}
	}
	if a := l.Agent; a != nil {
		jl.Agent = &api.LinkAgent{
			SessionID: a.SessionID, JobID: a.JobID, AgentID: a.AgentID, Kind: a.Kind, Name: a.Name,
			State: a.State, Live: a.Live, NeedsYou: a.NeedsYou, NeedsYouSince: a.NeedsYouSince,
			Reason: a.Reason, LastAction: a.LastAction, LastEventAt: a.LastEventAt, WorktreePath: a.WorktreePath,
			StartedAt: a.StartedAt, Tokens: a.Tokens,
		}
	}
	return jl
}

func alertJSON(a state.Alert) api.Alert {
	return api.Alert{Kind: string(a.Kind), Packages: a.Packages, Lane: a.Lane, Ref: targetPtr(a.Ref), Message: a.Message, Since: a.Since}
}

func target(t state.Target) api.Target { return api.Target{Kind: t.Kind, Ref: t.Ref} }

func targetPtr(t state.Target) *api.Target {
	if t.IsZero() {
		return nil
	}
	jt := target(t)
	return &jt
}

func nonNil[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

// convention is the package's convention block data: the suggested branch
// "<slug>/<id>-<short-name>" and the Plan: line.
func convention(slug, id, title string) api.Convention {
	branch := strings.ToLower(slug) + "/" + strings.ToLower(id)
	if short := shortName(title); short != "" {
		branch += "-" + short
	}
	return api.Convention{SuggestedBranch: branch, PlanLine: "Plan: " + slug + " · " + id}
}

// shortName makes a branch-name part from a title: its first words in
// lower case, letters and digits only, joined by hyphens, at most four
// words and 30 characters ("Offline route planning for long trips" is
// "offline-route-planning-for", "Elevation profiles alongside directions"
// is "elevation-profiles-alongside").
func shortName(title string) string {
	words := strings.FieldsFunc(strings.ToLower(title), func(r rune) bool {
		return r > unicode.MaxASCII || !(unicode.IsLetter(r) || unicode.IsDigit(r))
	})
	var out string
	for i, w := range words {
		if i == 4 || (out != "" && len(out)+1+len(w) > 30) {
			break
		}
		if out != "" {
			out += "-"
		}
		out += w
	}
	if len(out) > 30 {
		out = strings.TrimRight(out[:30], "-")
	}
	return out
}

// summaryOf builds a plan's summary from its board and what waits on you
// now (n). since, if not zero, limits Unlocked to packages unlocked after
// it.
func summaryOf(b api.Plan, since time.Time, n needs) api.PlanSummary {
	sum := api.PlanSummary{
		Slug: b.Slug, Title: b.Title, Merges: b.Merges, Total: b.Total, Done: b.Counts[string(state.Done)], Counts: b.Counts,
		Ready: []api.SummaryPackage{}, NeedsYouItems: []api.SummaryItem{}, NeedsYouPackages: []api.SummaryPackage{},
		ToMergePRs: nonNil(slices.Clone(n.toMerge[strings.ToLower(b.Slug)])), Unlocked: []api.SummaryPackage{},
		Packages: make([]api.SummaryPackage, 0, len(b.Packages)), Provisional: []api.SummaryPackage{}, Since: since,
		Warnings: len(b.Warnings), Alerts: b.Alerts, DerivedAt: b.DerivedAt,
	}
	ready := map[string]bool{}
	for _, id := range b.ReadyToDispatch {
		ready[strings.ToUpper(id)] = true
	}
	// What waits on you: the plan's Open items, by package.
	reasons := map[string][]string{}
	for _, it := range n.items[strings.ToLower(b.Slug)] {
		si := summaryItem(it)
		sum.NeedsYouItems = append(sum.NeedsYouItems, si)
		if it.Pkg != "" {
			k := strings.ToUpper(it.Pkg)
			reasons[k] = append(reasons[k], si.Reason)
		}
	}
	for _, p := range b.Packages {
		sp := api.SummaryPackage{ID: p.ID, Title: p.Title, Lifecycle: p.Lifecycle, Yours: p.Yours, BlockedBy: p.BlockedBy, PRs: linkedPRs(p),
			Provisional: p.Provisional, SuggestedRow: p.SuggestedRow}
		sum.Packages = append(sum.Packages, sp)
		if p.Provisional {
			sum.Provisional = append(sum.Provisional, sp)
		}
		if ready[strings.ToUpper(p.ID)] {
			sum.Ready = append(sum.Ready, sp)
		}
		if rs := reasons[strings.ToUpper(p.ID)]; len(rs) > 0 {
			ny := sp
			ny.Reasons = rs
			sum.NeedsYouPackages = append(sum.NeedsYouPackages, ny)
		}
		if p.Lifecycle == string(state.Ready) && p.PrevLifecycle == string(state.Blocked) && (since.IsZero() || p.Since.After(since)) {
			sum.Unlocked = append(sum.Unlocked, sp)
		}
	}
	sum.NeedsYou = len(sum.NeedsYouItems)
	sum.ToMerge = len(sum.ToMergePRs)
	sum.Band = band(b.Slug, sum.Done, sum.Total, len(sum.Ready), sum.NeedsYou)
	return sum
}

// summaryItem is an Open inbox item as a summary lists it.
func summaryItem(it store.InboxItem) api.SummaryItem {
	return api.SummaryItem{
		ID: it.ID, URL: inbox.ItemURL(it.ID), Kind: it.Kind, KindLabel: inbox.KindLabel(it.Kind),
		Title: cmp.Or(it.Title, it.Ref), Pkg: it.Pkg, PR: it.PR, Reason: itemReason(it), Since: it.Since,
	}
}

// itemReason says what an item waits on in a few words: a session's wait
// as the inbox tells it ("permission prompt: Bash: make check", "waiting
// for your reply"), a PR's kind and number ("review requested on #12"), a
// provisional package's missing row, or your step ("your step is ready:
// sign off the rollout").
func itemReason(it store.InboxItem) string {
	what := strings.ToLower(inbox.KindLabel(it.Kind))
	if it.Source == inbox.SourcePR {
		if i := strings.LastIndex(it.PR, "#"); i >= 0 {
			return what + " on " + it.PR[i:]
		}
		return what
	}
	if it.Kind == inbox.KindYourStep {
		return cmp.Or(it.Why, "your step is ready")
	}
	if it.Source == inbox.SourcePlan {
		return what + ": " + cmp.Or(it.Title, it.Pkg) // "plan row missing: W12 isn't in ROADMAP.md"
	}
	return cmp.Or(it.Detail, what)
}

// band is the mod's one-line plan summary: "sharing · 6/14 done · 3 ready
// · 2 need you".
func band(slug string, done, total, ready, needYou int) string {
	verb := "need"
	if needYou == 1 {
		verb = "needs"
	}
	return fmt.Sprintf("%s · %d/%d done · %d ready · %d %s you", slug, done, total, ready, needYou, verb)
}

// linkedPRs returns a package's PRs ("owner/repo#123"), open ones first,
// each once.
func linkedPRs(p api.Package) []string {
	var open, other []string
	seen := map[string]bool{}
	for _, l := range p.Links {
		if l.Kind != store.LinkPR || seen[strings.ToLower(l.Ref)] {
			continue
		}
		seen[strings.ToLower(l.Ref)] = true
		if l.PR != nil && l.PR.State == store.PROpen {
			open = append(open, l.Ref)
		} else {
			other = append(other, l.Ref)
		}
	}
	return append(open, other...)
}
