package state

import (
	"strings"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/plans"
	"github.com/gravexa-llc/ai-manager/internal/store"
)

// now is the test clock; the mini plan is registered a day before.
var (
	now        = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	registered = now.Add(-24 * time.Hour)
)

const (
	repo     = "example-org/app"
	repoPath = "/work/app"
	wtRoot   = "/work/app/.claude/worktrees/"
)

// miniPlan exercises every rule: a dependency, a single-writer lane and an
// ordinary one, a hyphenated ID, your own step and a `you:` step.
const miniPlan = `# Mini plan

Plan slug: aim

| Lane | Packages | Rule |
|---|---|---|
| Store | B1, B2 | single-writer |
| Other | C1, C2 | anything goes |

| Package | Owner | Needs |
|---|---|---|
| **A1 One** | | — |
| **A2 Two** | | A1 |
| **B1 Store one** | | — |
| **B2 Store two** | | — |
| **B3b-2 Odd** | | — |
| **C1 Other one** | | — |
| **C2 Other two** | | — |
| **Y1 Yours** | you | — |
| **R1 Check** | | A1; you: run the check |
`

// mdPlan registers a plan written in markdown.
func mdPlan(t *testing.T, id int64, md string, created time.Time, repos ...string) Plan {
	t.Helper()
	parsed := plans.Parse([]byte(md))
	if len(parsed.Warnings) > 0 && !strings.Contains(md, "<!-- warnings ok -->") {
		t.Fatalf("plan warnings: %v", parsed.Warnings)
	}
	pkgs := StorePackages(parsed)
	for i := range pkgs {
		pkgs[i].PlanID = id
	}
	return Plan{Plan: store.Plan{ID: id, Slug: plans.Slug("x-plan.md", []byte(md)), Title: parsed.Title, SourceMD: md,
		Repos: repos, CreatedAt: created}, Packages: pkgs}
}

// baseInput is the mini plan in repo, with its main checkout on main.
func baseInput(t *testing.T) Input {
	return Input{
		Now:       now,
		Plans:     []Plan{mdPlan(t, 1, miniPlan, registered, repo)},
		Worktrees: []store.Worktree{{Path: repoPath, MainRepo: repoPath, Branch: "main", FirstSeen: registered.Add(-time.Hour)}},
		RepoNames: map[string]string{repoPath: repo},
	}
}

func pr(n int, branch, title string) store.PR {
	return store.PR{Repo: repo, Number: n, HeadBranch: branch, BaseRef: "main", Title: title, State: store.PROpen,
		Mergeable: "MERGEABLE", MergeStateStatus: "BLOCKED", FirstSeenAt: registered.Add(time.Hour)}
}

func merged(p store.PR) store.PR {
	p.State, p.MergedAt, p.ClosedAt = store.PRMerged, registered.Add(2*time.Hour), registered.Add(2*time.Hour)
	p.MergeStateStatus, p.Mergeable = "UNKNOWN", "UNKNOWN"
	return p
}

func closed(p store.PR) store.PR {
	p.State, p.ClosedAt = store.PRClosed, registered.Add(2*time.Hour)
	p.MergeStateStatus, p.Mergeable = "UNKNOWN", "UNKNOWN"
	return p
}

// worktree is a linked worktree of repo, first seen after registration,
// with no commits of its own.
func worktree(name, branch string) store.Worktree {
	return store.Worktree{Path: wtRoot + name, MainRepo: repoPath, Branch: branch,
		FirstSeen: registered.Add(time.Hour), LastCommitAt: registered.Add(-time.Hour), LastSeen: now}
}

// seenSince sets when a worktree was first seen.
func seenSince(w store.Worktree, at time.Time) store.Worktree {
	w.FirstSeen = at
	return w
}

// withCommits gives a worktree a commit at the given time.
func withCommits(w store.Worktree, at time.Time) store.Worktree {
	w.LastCommitAt = at
	return w
}

// session is a live interactive session working in dir.
func session(id, dir string) store.Session {
	return store.Session{SessionID: id, Key: "pid:" + id, Kind: "interactive", Cwd: dir, WorktreePath: dir,
		State: "working", FirstSeenAt: registered, LastSeenAt: now}
}

func link(pkg, kind, ref, method, conf string) store.Link {
	return store.Link{PlanID: 1, PkgID: pkg, Kind: kind, Ref: ref, MatchMethod: method, Confidence: conf}
}

func derive(in Input) PlanState { return Derive(in, Match(in), Options{})[0] }

func pkgOf(t *testing.T, ps PlanState, id string) PackageState {
	t.Helper()
	p, ok := ps.Package(id)
	if !ok {
		t.Fatalf("no package %s", id)
	}
	return p
}

// refs lists a package's links as "kind ref" (direct) or "kind ref < via".
func refs(p PackageState) []string {
	var out []string
	for _, l := range p.Links {
		s := l.Kind + " " + l.Ref
		if !l.Via.IsZero() {
			s += " < " + l.Via.Ref
		}
		out = append(out, s)
	}
	return out
}

func unmatchedNotes(ps PlanState) map[string]string {
	out := map[string]string{}
	for _, u := range ps.Unmatched {
		out[u.Ref] = u.Note
	}
	return out
}

func alertKinds(ps PlanState) []string {
	var out []string
	for _, a := range ps.Alerts {
		out = append(out, string(a.Kind))
	}
	return out
}

func prRef(n int) string { return store.PRRef(repo, n) }
