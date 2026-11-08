package state

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gravexa-llc/ai-manager/internal/store"
)

// hit is one package a PR or branch matched.
type hit struct {
	plan       *planIdx
	pkg        *store.Package
	method     string
	confidence string
}

// Match proposes the links it infers, most reliable first, for
// every same-repo PR and every branch without a PR in a registered plan's
// repos. It is pure: store its answer with store.ReplaceInferredLinks, and
// pass it to Derive. See the package documentation for the rules.
func Match(in Input) Matches {
	w := newWorld(&in)
	m := Matches{Links: map[int64][]store.Link{}, Notes: map[Target]string{}}
	// A target with a spawn or manual link in a plan is settled there: the
	// matcher proposes nothing else for it in that plan.
	w.claimed = map[int64]map[string]bool{}
	for _, l := range in.Links {
		t := Target{l.Kind, linkRef(l)}
		if l.Inferred || w.isDead(l.PlanID, l.PkgID, t) {
			continue
		}
		if w.claimed[l.PlanID] == nil {
			w.claimed[l.PlanID] = map[string]bool{}
		}
		w.claimed[l.PlanID][targetKey(t)] = true
	}
	for _, pr := range w.prs {
		b := w.branches[branchKey(repoKey(pr.Repo, ""), pr.HeadBranch)]
		hits, note := w.matchPR(pr, b)
		w.add(&m, Target{store.LinkPR, store.PRRef(pr.Repo, pr.Number)}, hits, note)
	}
	for _, b := range w.branchList {
		if len(b.prs) > 0 {
			continue // its PR carries the match, and Derive brings the branch
		}
		scope := w.plansFor(b.repoKeys()...)
		if len(scope) == 0 {
			continue
		}
		hits, note := matchBranch(b, scope)
		w.add(&m, Target{store.LinkBranch, b.ref}, hits, note)
	}
	for id, ls := range m.Links {
		sort.Slice(ls, func(i, j int) bool {
			if a, b := canonID(ls[i].PkgID), canonID(ls[j].PkgID); a != b {
				return a < b
			}
			if ls[i].Kind != ls[j].Kind {
				return ls[i].Kind < ls[j].Kind
			}
			return ls[i].Ref < ls[j].Ref
		})
		m.Links[id] = ls
	}
	return m
}

// add records the hits for a target as links, unless you unlinked them,
// and the note when there is none.
func (w *world) add(m *Matches, t Target, hits []hit, note string) {
	var unlinked []string
	added := false
	for _, h := range hits {
		if w.claimed[h.plan.ID][targetKey(t)] {
			added = true // linked by the mod or by you
			continue
		}
		if w.isDead(h.plan.ID, h.pkg.PkgID, t) {
			unlinked = append(unlinked, h.plan.slug+" · "+h.pkg.PkgID)
			continue
		}
		m.Links[h.plan.ID] = append(m.Links[h.plan.ID], store.Link{
			PlanID: h.plan.ID, PkgID: h.pkg.PkgID, Kind: t.Kind, Ref: t.Ref,
			MatchMethod: h.method, Confidence: h.confidence,
		})
		added = true
	}
	switch {
	case added:
	case len(unlinked) > 0:
		m.Notes[t] = "you unlinked it from " + strings.Join(unlinked, ", ")
	case note != "":
		m.Notes[t] = note
	}
}

// matchPR matches a same-repo PR: its Plan: line, else its title's
// trailing (ID), else its head branch's name. b is its head branch, nil
// for a default or base branch.
func (w *world) matchPR(pr *store.PR, b *branchInfo) ([]hit, string) {
	scope := w.plansFor(repoKey(pr.Repo, ""))
	if len(scope) == 0 {
		return nil, ""
	}
	// 1. The Plan: line names the plan, so it settles the match either
	// way: nothing below can override it.
	if pr.PlanSlug != "" || pr.PlanPkg != "" {
		line := pr.PlanSlug + " · " + pr.PlanPkg
		for _, p := range scope {
			if p.slug != strings.ToLower(pr.PlanSlug) {
				continue
			}
			if pkg := p.pkg(pr.PlanPkg); pkg != nil {
				return []hit{{p, pkg, store.MatchPlanLine, store.ConfidenceExact}}, ""
			}
			return nil, fmt.Sprintf("its Plan: line names %s, which isn't a package of %s", line, p.slug)
		}
		return nil, fmt.Sprintf("its Plan: line names %s, but no plan %q is registered for %s", line, pr.PlanSlug, pr.Repo)
	}
	// 2. The title's trailing (ID).
	if id, ok := titleID(pr.Title); ok {
		var hits []hit
		for _, p := range scope {
			if pkg := p.pkg(id); pkg != nil {
				hits = append(hits, hit{p, pkg, store.MatchTitle, store.ConfidenceHigh})
			}
		}
		switch {
		case len(hits) == 1:
			return hits, ""
		case len(hits) > 1:
			return nil, ambiguous(hits)
		}
	}
	// 3. The head branch's name.
	if b == nil {
		return nil, ""
	}
	return matchBranch(b, scope)
}

// matchBranch matches a branch by its name: "<slug>/<id>-…" with high
// confidence, else a package ID as a token with low confidence, if the
// branch was first seen after the plan was registered.
func matchBranch(b *branchInfo, scope []*planIdx) ([]hit, string) {
	lower := strings.ToLower(b.name)
	byLen := append([]*planIdx(nil), scope...)
	sort.SliceStable(byLen, func(i, j int) bool { return len(byLen[i].slug) > len(byLen[j].slug) })
	for _, p := range byLen {
		rest, ok := strings.CutPrefix(lower, p.slug+"/")
		if !ok || p.slug == "" {
			continue
		}
		for _, cand := range leadingIDs(rest) {
			if pkg := p.pkg(cand); pkg != nil {
				return []hit{{p, pkg, store.MatchBranch, store.ConfidenceHigh}}, ""
			}
		}
		return nil, fmt.Sprintf("the branch is under %s/ but names no package of %s", p.slug, p.slug)
	}

	words := branchWords(b.name)
	var hits []hit
	for _, p := range scope {
		for _, pkg := range p.order {
			if namesID(words, pkg.PkgID) {
				hits = append(hits, hit{p, pkg, store.MatchBranch, store.ConfidenceLow})
			}
		}
	}
	switch {
	case len(hits) == 0:
		return nil, ""
	case len(hits) > 1:
		return nil, ambiguous(hits)
	}
	h := hits[0]
	if b.firstSeen.IsZero() || !b.firstSeen.After(h.plan.CreatedAt) {
		return nil, fmt.Sprintf("its branch names %s only as a token, and the branch was first seen before plan %s was registered", h.pkg.PkgID, h.plan.slug)
	}
	return hits, ""
}

// ambiguous is the note for a target that names packages of more than one
// plan, or more than one package.
func ambiguous(hits []hit) string {
	var names []string
	for _, h := range hits {
		names = append(names, h.plan.slug+" · "+h.pkg.PkgID)
	}
	return "ambiguous: it could be " + strings.Join(names, " or ") + "; name the package in a Plan: line or a <slug>/<id>-… branch"
}
