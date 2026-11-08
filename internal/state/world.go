package state

import (
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/worktrees"
)

// A repo's base branches are never matched, never listed as unmatched and
// never carry a link from one target to another: work doesn't happen on
// them. They are its default branch and every branch some PR targets (its
// base), unless that is itself the head of a PR. guessedDefaults stand in
// for the default branch of a repo whose real one isn't known
// (Input.DefaultBranches).
var guessedDefaults = map[string]bool{"main": true, "master": true, "trunk": true, "develop": true}

// world is an Input indexed for the matcher and Derive.
type world struct {
	in    *Input
	plans []*planIdx
	// repoNames maps worktrees.PathKey(main checkout) to the GitHub name.
	repoNames map[string]string
	// prs are the same-repo PRs; prByRef has every PR, by refKey.
	prs     []*store.PR
	prByRef map[string]*store.PR
	// branches has every non-default branch of a PR or a worktree, by
	// branch key; branchList is the same, sorted by ref.
	branches   map[string]*branchInfo
	branchList []*branchInfo
	// bases holds, by repo key, the branches PRs target.
	bases map[string]map[string]bool
	// defaults holds, by repo key, the default branches GitHub named.
	defaults map[string]string
	// dead holds the tombstones, by plan, package and target.
	dead map[deadKey]bool
	// claimed holds, by plan, the targets with a spawn or manual link
	// (Match only).
	claimed map[int64]map[string]bool
}

// planIdx is a plan indexed for lookups.
type planIdx struct {
	*Plan
	slug  string                    // lower case
	repos map[string]bool           // repo keys
	pkgs  map[string]*store.Package // by canonID
	order []*store.Package
}

type deadKey struct {
	plan   int64
	pkg    string // canonID
	target string // targetKey
}

// branchInfo is one branch of one repo.
type branchInfo struct {
	repoName string // GitHub's nameWithOwner, if known
	repoPath string // a main checkout's path, if known
	name     string
	ref      string // store.BranchRef
	key      string // branchKey
	// firstSeen is the earliest sighting of the branch: its PRs' and
	// worktrees' first sightings. Zero if none is known.
	firstSeen time.Time
	prs       []*store.PR
	worktrees []*store.Worktree
}

// repoKey is how repos compare: GitHub names in lower case, paths as
// worktrees.PathKey. An empty name and path give "".
func repoKey(name, path string) string {
	if name != "" {
		return strings.ToLower(name)
	}
	if path != "" {
		return worktrees.PathKey(path)
	}
	return ""
}

// normRepo turns an entry of Plan.Repos (or a session's Repo) into a repo
// key: an absolute path as worktrees.PathKey, anything else as a GitHub
// name in lower case, without a trailing ".git" or slash.
func normRepo(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if filepath.IsAbs(s) {
		return worktrees.PathKey(s)
	}
	s = strings.TrimSuffix(strings.TrimSuffix(strings.ToLower(s), "/"), ".git")
	return strings.TrimPrefix(s, "https://github.com/")
}

func branchKey(repo, branch string) string { return repo + ":" + branch }

// targetKey is how targets compare: the PR repo, the branch's repo and the
// worktree path are folded as repos are; refs of other kinds compare
// exactly.
func targetKey(t Target) string {
	ref := t.Ref
	switch t.Kind {
	case store.LinkPR:
		ref = strings.ToLower(ref)
	case store.LinkBranch:
		if repo, branch, ok := strings.Cut(ref, ":"); ok {
			ref = branchKey(normRepo(repo), branch)
		}
	case store.LinkWorktree:
		ref = worktrees.PathKey(ref)
	}
	return t.Kind + " " + ref
}

func newWorld(in *Input) *world {
	w := &world{
		in:        in,
		repoNames: map[string]string{},
		prByRef:   map[string]*store.PR{},
		branches:  map[string]*branchInfo{},
		bases:     map[string]map[string]bool{},
		defaults:  map[string]string{},
		dead:      map[deadKey]bool{},
	}
	for path, name := range in.RepoNames {
		if name != "" {
			w.repoNames[worktrees.PathKey(path)] = name
		}
	}
	for name, branch := range in.DefaultBranches {
		if k := repoKey(name, ""); k != "" && branch != "" {
			w.defaults[k] = branch
		}
	}
	for i := range in.Plans {
		p := &in.Plans[i]
		idx := &planIdx{Plan: p, slug: strings.ToLower(p.Slug), repos: map[string]bool{}, pkgs: map[string]*store.Package{}}
		for _, r := range p.Repos {
			if k := normRepo(r); k != "" {
				idx.repos[k] = true
			}
		}
		for j := range p.Packages {
			pkg := &p.Packages[j]
			idx.pkgs[canonID(pkg.PkgID)] = pkg
			idx.order = append(idx.order, pkg)
		}
		w.plans = append(w.plans, idx)
	}
	for _, t := range in.Tombstones {
		l := store.Link{Kind: t.Kind, SessionID: t.SessionID, JobID: t.JobID, AgentID: t.AgentID, Ref: t.Ref}
		w.dead[deadKey{t.PlanID, canonID(t.PkgID), targetKey(Target{t.Kind, linkRef(l)})}] = true
	}

	// PRs, and the branches they target that aren't themselves a PR's head
	// (a stacked PR targets another PR's branch, which is still work).
	heads := map[string]bool{}
	for i := range in.PRs {
		pr := &in.PRs[i]
		if !pr.IsCrossRepository {
			heads[branchKey(repoKey(pr.Repo, ""), pr.HeadBranch)] = true
		}
	}
	for i := range in.PRs {
		pr := &in.PRs[i]
		w.prByRef[targetKey(Target{store.LinkPR, store.PRRef(pr.Repo, pr.Number)})] = pr
		k := repoKey(pr.Repo, "")
		if pr.BaseRef != "" && !heads[branchKey(k, pr.BaseRef)] {
			if w.bases[k] == nil {
				w.bases[k] = map[string]bool{}
			}
			w.bases[k][pr.BaseRef] = true
		}
	}
	for i := range in.PRs {
		pr := &in.PRs[i]
		if pr.IsCrossRepository {
			continue
		}
		w.prs = append(w.prs, pr)
		if b := w.branch(pr.Repo, "", pr.HeadBranch); b != nil {
			b.prs = append(b.prs, pr)
			// The branch existed when the PR was opened (GitHub's
			// createdAt), which can be long before aimd first saw it.
			b.seen(pr.OpenedAt())
		}
	}
	// Worktrees' branches.
	for i := range in.Worktrees {
		wt := &in.Worktrees[i]
		if wt.Detached || wt.Branch == "" {
			continue
		}
		if b := w.branch(w.repoNames[worktrees.PathKey(wt.MainRepo)], wt.MainRepo, wt.Branch); b != nil {
			b.worktrees = append(b.worktrees, wt)
			b.seen(wt.FirstSeen)
		}
	}
	for _, b := range w.branches {
		w.branchList = append(w.branchList, b)
	}
	sort.Slice(w.branchList, func(i, j int) bool { return w.branchList[i].key < w.branchList[j].key })
	return w
}

// linkRef is a link's ref, filled in for an agent the way the store fills
// it in.
func linkRef(l store.Link) string {
	if l.Kind == store.LinkAgent && l.Ref == "" {
		switch {
		case l.SessionID != "":
			return store.AgentRef(l.SessionID, l.AgentID)
		case l.JobID != "" && l.AgentID == "":
			return store.JobRef(l.JobID)
		}
	}
	return l.Ref
}

// isBase reports whether a branch is its repo's default branch (or, when
// that isn't known, one of guessedDefaults) or some PR's base there.
func (w *world) isBase(repo, branch string) bool {
	if w.bases[repo][branch] {
		return true
	}
	if def, ok := w.defaults[repo]; ok {
		return branch == def
	}
	return guessedDefaults[branch]
}

// branch returns the branch's info, creating it, or nil for a default or
// base branch or a repo with neither name nor path.
func (w *world) branch(name, path, branch string) *branchInfo {
	rk := repoKey(name, path)
	if rk == "" || branch == "" || w.isBase(rk, branch) {
		return nil
	}
	k := branchKey(rk, branch)
	b := w.branches[k]
	if b == nil {
		display := name
		if display == "" {
			display = path
		}
		b = &branchInfo{repoName: name, repoPath: path, name: branch, ref: store.BranchRef(display, branch), key: k}
		w.branches[k] = b
	}
	if b.repoPath == "" {
		b.repoPath = path
	}
	return b
}

func (b *branchInfo) seen(t time.Time) {
	if !t.IsZero() && (b.firstSeen.IsZero() || t.Before(b.firstSeen)) {
		b.firstSeen = t
	}
}

// repoKeys are the keys a branch's repo answers to.
func (b *branchInfo) repoKeys() []string {
	var ks []string
	if b.repoName != "" {
		ks = append(ks, repoKey(b.repoName, ""))
	}
	if b.repoPath != "" {
		ks = append(ks, repoKey("", b.repoPath))
	}
	return ks
}

// plansFor returns the plans registered for any of the repo keys, in plan
// order.
func (w *world) plansFor(keys ...string) []*planIdx {
	var out []*planIdx
	for _, p := range w.plans {
		for _, k := range keys {
			if k != "" && p.repos[k] {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

// isDead reports whether you unlinked the target from the package.
func (w *world) isDead(plan int64, pkg string, t Target) bool {
	return w.dead[deadKey{plan, canonID(pkg), targetKey(t)}]
}

// pkg returns the plan's package with the ID (any case), or nil.
func (p *planIdx) pkg(id string) *store.Package { return p.pkgs[canonID(id)] }

// yours reports whether a package is your own step.
func yours(p *store.Package) bool { return strings.EqualFold(strings.TrimSpace(p.Owner), "you") }
