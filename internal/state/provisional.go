package state

// Provisional packages: work that names a package its plan's
// tables don't have yet. See the package documentation.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gravexa-llc/ai-manager/internal/plans"
	"github.com/gravexa-llc/ai-manager/internal/store"
)

// ProvisionalQuiet is how long a provisional package stays once nothing of
// its work is left: no live agent linked to it, no worktree on its
// branches, no open or merged PR. Then it goes, links and all. A package
// named only by a branch whose worktree was removed (a closed branch, no
// PR) goes a day later; one whose PR merged stays until its row lands,
// since done work should be in the plan file.
const ProvisionalQuiet = 24 * time.Hour

// provisionalTouch is how stale a stored sighting, or a package's last
// activity, may get before a pass writes it again: a pass every few
// seconds needn't write every time.
const provisionalTouch = time.Minute

// Unknown finds the work in a snapshot that names a package its plan's
// tables don't have, given the matcher's answer for the
// snapshot. For each registered plan, in the plan's repos:
//
//   - an open same-repo PR whose Plan: line names the plan and an ID it
//     has no row for;
//   - a same-repo branch named "<slug>/<id>-…" for an ID the plan has no
//     row for, while there is work on it: a worktree has it checked out
//     and it has no PR, or one of its PRs is open. A branch whose PR has a
//     Plan: line for the plan is left to that line.
//
// Each is a sighting of a provisional package: a new one, or one already
// provisional, whose source it refreshes. Left alone: plans that aren't
// registered for the repo (or slugs that name no plan), IDs the plan file
// has, merged and closed PRs and the branches they leave behind (done work
// isn't announced late), and anything already linked to one of the file's
// packages (by you, by a spawn, or by a better rule of the matcher). The
// spawn source isn't here: the mod reports it (planapi's spawn link). The
// sightings' At is in.Now.
func Unknown(in Input, m Matches) []store.Sighting {
	w := newWorld(&in)
	fileHas := func(p *planIdx, id string) bool {
		pkg := p.pkg(id)
		return pkg != nil && !pkg.Provisional
	}
	// The targets linked to one of the file's packages, by plan.
	byID := map[int64]*planIdx{}
	for _, p := range w.plans {
		byID[p.ID] = p
	}
	linked := map[int64]map[string]bool{}
	mark := func(l store.Link) {
		t := Target{l.Kind, linkRef(l)}
		p := byID[l.PlanID]
		if p == nil || !fileHas(p, l.PkgID) || w.isDead(l.PlanID, l.PkgID, t) {
			return
		}
		if linked[p.ID] == nil {
			linked[p.ID] = map[string]bool{}
		}
		linked[p.ID][targetKey(t)] = true
	}
	for _, l := range in.Links {
		if !l.Inferred {
			mark(l)
		}
	}
	for _, ls := range m.Links {
		for _, l := range ls {
			mark(l)
		}
	}
	prLinked := func(p *planIdx, pr *store.PR) bool {
		return linked[p.ID][targetKey(Target{store.LinkPR, store.PRRef(pr.Repo, pr.Number)})]
	}

	var out []store.Sighting
	seen := map[string]bool{}
	add := func(p *planIdx, id, kind, ref, title string) {
		k := fmt.Sprint(p.ID, " ", canonID(id), " ", kind, " ", ref)
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, store.Sighting{PlanID: p.ID, PkgID: id, Kind: kind, Ref: ref, Title: title, At: in.Now})
	}

	for _, pr := range w.prs {
		if pr.State != store.PROpen || pr.PlanSlug == "" {
			continue
		}
		for _, p := range w.plansFor(repoKey(pr.Repo, "")) {
			if p.slug != strings.ToLower(pr.PlanSlug) {
				continue
			}
			if id := ProvisionalID(pr.PlanPkg); id != "" && !fileHas(p, id) && !prLinked(p, pr) {
				add(p, id, store.ProvisionalPR, store.PRRef(pr.Repo, pr.Number), prTitle(pr.Title))
			}
			break
		}
	}

	for _, b := range w.branchList {
		if !atWork(b) {
			continue
		}
		scope := w.plansFor(b.repoKeys()...)
		sort.SliceStable(scope, func(i, j int) bool { return len(scope[i].slug) > len(scope[j].slug) })
		for _, p := range scope {
			if p.slug == "" || !strings.HasPrefix(strings.ToLower(b.name), p.slug+"/") {
				continue
			}
			id, title := branchID(b.name[len(p.slug)+1:])
			if id == "" || fileHas(p, id) || linked[p.ID][targetKey(Target{store.LinkBranch, b.ref})] {
				break
			}
			left := false // to a PR: its Plan: line, or its link to a file's package
			for _, pr := range b.prs {
				left = left || strings.EqualFold(pr.PlanSlug, p.slug) || prLinked(p, pr)
			}
			if !left {
				add(p, id, store.ProvisionalBranch, b.ref, title)
			}
			break
		}
	}
	return out
}

// atWork reports whether a branch has work on it now: a worktree that
// still exists and no PR yet, or an open PR.
func atWork(b *branchInfo) bool {
	if len(b.prs) == 0 {
		for _, wt := range b.worktrees {
			if wt.RemovedAt.IsZero() {
				return true
			}
		}
		return false
	}
	for _, pr := range b.prs {
		if pr.State == store.PROpen {
			return true
		}
	}
	return false
}

// ProvisionalID reads a package ID as a provisional package spells it: as
// written when it has an ID's shape (plans.IsID), else in upper case
// ("w12" is W12); "" when it isn't an ID either way.
func ProvisionalID(s string) string {
	s = strings.TrimSpace(s)
	if plans.IsID(s) {
		return s
	}
	if u := strings.ToUpper(s); plans.IsID(u) {
		return u
	}
	return ""
}

// branchID reads the package ID and a title from the part of a branch
// name after "<slug>/": the ID is the first hyphen group (with the next
// one too when the first is letters only, as in sec-1), and any later
// groups that hold a digit (b3b-2); the title is the rest, in words. So
// "w12-unregister-button" is W12, "Unregister button", and "b3b-2-fix" is
// B3B-2, "Fix". Only the first path segment counts.
func branchID(rest string) (id, title string) {
	seg, _, _ := strings.Cut(rest, "/")
	groups := strings.Split(seg, "-")
	if len(groups) == 0 || groups[0] == "" {
		return "", ""
	}
	id = groups[0]
	i := 1
	if !strings.ContainsFunc(id, unicode.IsDigit) && i < len(groups) && groups[i] != "" && unicode.IsDigit(rune(groups[i][0])) {
		id += "-" + groups[i]
		i++
	}
	for ; i < len(groups) && strings.ContainsFunc(groups[i], unicode.IsDigit); i++ {
		if len(id)+1+len(groups[i]) > 10 {
			break
		}
		id += "-" + groups[i]
	}
	id = ProvisionalID(id)
	if id == "" {
		return "", ""
	}
	return id, upperFirst(strings.Join(strings.Fields(strings.Join(groups[i:], " ")), " "))
}

// prTitle is a PR's title without its trailing "(ID)".
func prTitle(title string) string {
	title = strings.TrimSpace(title)
	if loc := titleIDRE.FindStringIndex(title); loc != nil {
		title = strings.TrimSpace(title[:loc[0]])
	}
	return title
}

// ProvisionalTitle makes a provisional package's title from what named it
// (a spawn's description, say): without the ID itself, leading or as a
// trailing "(ID)", and with its first letter in upper case. "W12:
// unregister button" is "Unregister button".
func ProvisionalTitle(id, s string) string {
	s = prTitle(s)
	if word, rest, ok := strings.Cut(s, " "); ok || word != "" {
		if canonID(strings.TrimRight(word, ":.,·-")) == canonID(id) {
			s = strings.TrimLeft(rest, " :·-–—")
		}
	}
	return upperFirst(strings.TrimSpace(s))
}

func upperFirst(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if n == 0 {
		return s
	}
	return string(unicode.ToUpper(r)) + s[n:]
}

// seeProvisional stores the snapshot's sightings (Unknown) that tell the
// store something new: a new package, a new source, or one not written
// for provisionalTouch. It reports whether it made a package, so the pass
// reloads and the matcher can link to it.
func seeProvisional(ctx context.Context, st *store.Store, in Input, m Matches) (bool, error) {
	sightings := Unknown(in, m)
	if len(sightings) == 0 {
		return false, nil
	}
	have := map[string]*store.Package{} // plan id, canonical ID
	for i := range in.Plans {
		for j := range in.Plans[i].Packages {
			pkg := &in.Plans[i].Packages[j]
			have[fmt.Sprint(pkg.PlanID, " ", canonID(pkg.PkgID))] = pkg
		}
	}
	fresh := func(t time.Time) bool { return !t.IsZero() && in.Now.Sub(t) < provisionalTouch }
	var write []store.Sighting
	for _, sg := range sightings {
		pkg := have[fmt.Sprint(sg.PlanID, " ", canonID(sg.PkgID))]
		if pkg != nil && fresh(pkg.ActiveAt) && hasFreshSource(pkg, sg, fresh) {
			continue
		}
		write = append(write, sg)
	}
	if len(write) == 0 {
		return false, nil
	}
	made, err := st.SeeProvisional(ctx, write)
	if err != nil {
		return false, fmt.Errorf("state: %w", err)
	}
	return made > 0, nil
}

func hasFreshSource(pkg *store.Package, sg store.Sighting, fresh func(time.Time) bool) bool {
	for _, src := range pkg.Sources {
		if src.Kind == sg.Kind && src.Ref == sg.Ref && fresh(src.LastSeen) && (sg.Title == "" || sg.Title == src.Title) {
			return true
		}
	}
	return false
}

// provisionalAtWork reports whether a provisional package has work on it
// now: a live agent, a worktree that still exists, an open or merged PR.
// A remote agent's cloud session doesn't count: aimd can't see it run.
func provisionalAtWork(p PackageState) bool {
	for _, l := range p.Links {
		switch {
		case l.Agent != nil && l.Agent.Live:
			return true
		case l.Worktree != nil && l.Worktree.RemovedAt.IsZero():
			return true
		case l.PR != nil && (l.PR.State == store.PROpen || l.PR.State == store.PRMerged):
			return true
		}
	}
	return false
}

// keepProvisional moves on the last activity of the provisional packages
// with work on them, and expires those with none for ProvisionalQuiet. It
// reports whether it expired any, so the pass derives again without them.
func keepProvisional(ctx context.Context, st *store.Store, in Input, states []PlanState) (bool, error) {
	stored := map[string]*store.Package{}
	for i := range in.Plans {
		for j := range in.Plans[i].Packages {
			pkg := &in.Plans[i].Packages[j]
			if pkg.Provisional {
				stored[fmt.Sprint(pkg.PlanID, " ", canonID(pkg.PkgID))] = pkg
			}
		}
	}
	expired := false
	for _, ps := range states {
		var touch, old []string
		for _, p := range ps.Packages {
			sp := stored[fmt.Sprint(ps.PlanID, " ", canonID(p.ID))]
			if !p.Provisional || sp == nil {
				continue
			}
			switch {
			case provisionalAtWork(p):
				if in.Now.Sub(sp.ActiveAt) >= provisionalTouch {
					touch = append(touch, p.ID)
				}
			case in.Now.Sub(sp.ActiveAt) > ProvisionalQuiet:
				old = append(old, p.ID)
			}
		}
		if err := st.SetProvisionalActive(ctx, ps.PlanID, touch, in.Now); err != nil {
			return false, fmt.Errorf("state: %w", err)
		}
		n, err := st.ExpireProvisional(ctx, ps.PlanID, old, in.Now.Add(-ProvisionalQuiet))
		if err != nil {
			return false, fmt.Errorf("state: %w", err)
		}
		expired = expired || n > 0
	}
	return expired, nil
}
