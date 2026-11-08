package state

import (
	"slices"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/store"
)

func TestCanonID(t *testing.T) {
	for in, want := range map[string]string{
		"A1": "A1", "a1": "A1", "SEC-1": "SEC1", "sec1": "SEC1", "B3b-2": "B3B-2", "b-3b-2": "B3B-2",
		" V1 ": "V1", "A-": "A-",
	} {
		if got := canonID(in); got != want {
			t.Errorf("canonID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTitleID(t *testing.T) {
	for title, want := range map[string]string{
		"Sharing notifications (V1)": "V1",
		"Store (b1)":                 "b1",
		"Odd (B3b-2)  ":              "B3b-2",
		"Spaced ( A1 )":              "A1",
		"(A1) at the start":          "",
		"Draft (WIP)":                "",
		"Two (A1, A2)":               "",
		"Too long (ABCD1234567)":     "",
		"Version bump (v2.1)":        "",
		"Matcher and package (C2).":  "",
		"Matcher and package state ": "",
	} {
		got, ok := titleID(title)
		if got != want || ok != (want != "") {
			t.Errorf("titleID(%q) = %q, %v, want %q", title, got, ok, want)
		}
	}
}

func TestBranchTokens(t *testing.T) {
	for _, tc := range []struct {
		branch, id string
		want       bool
	}{
		{"share/v1-email-reports", "V1", true},
		{"surge-b-3b-2", "B3b-2", true},
		{"surge-b3b-2", "B3b-2", true},
		{"surge-b-3b-3", "B3b-2", false},
		{"feature/sec-1", "SEC-1", true},
		{"feature/sec1-x", "SEC-1", true},
		{"api-v20", "V2", false},
		{"v1x", "V1", false},
		{"worktree-agent-a1b2c3", "A1", false},
		{"A1", "A1", true},
	} {
		if got := namesID(branchWords(tc.branch), tc.id); got != tc.want {
			t.Errorf("%s names %s = %v, want %v", tc.branch, tc.id, got, tc.want)
		}
	}
	deepEq(t, "leadingIDs", leadingIDs("b3b-2-fix/x"), []string{"b3b-2-fix/x", "b3b-2-fix", "b3b-2", "b3b"})
}

// TestMatchPRRules covers Match's order for PRs, one rule per case.
func TestMatchPRRules(t *testing.T) {
	for _, tc := range []struct {
		name string
		pr   store.PR
		want string // "PKG method/confidence", or "" for no link
		note string // the Unmatched note when there is no link
	}{
		{"plan line", withPlan(pr(1, "x", "Anything"), "aim", "A2"), "A2 plan_line/exact", ""},
		{"plan line, slug in another case", withPlan(pr(1, "x", ""), "AIM", "a2"), "A2 plan_line/exact", ""},
		{"plan line beats title", withPlan(pr(1, "aim/b1-x", "Thing (A1)"), "aim", "A2"), "A2 plan_line/exact", ""},
		{"plan line with an unknown package settles it", withPlan(pr(1, "aim/b1-x", "Thing (A1)"), "aim", "Z9"), "",
			"its Plan: line names aim · Z9, which isn't a package of aim"},
		{"plan line naming another plan settles it", withPlan(pr(1, "aim/b1-x", "Thing (A1)"), "other", "A1"), "",
			`its Plan: line names other · A1, but no plan "other" is registered for example-org/app`},
		{"title", pr(1, "feature/x", "Thing (A1)"), "A1 title/high", ""},
		{"title in lower case", pr(1, "feature/x", "Thing (a1)"), "A1 title/high", ""},
		{"title with a hyphenated ID", pr(1, "feature/x", "Odd (B3b-2)"), "B3b-2 title/high", ""},
		{"title ID that isn't a package falls through to the branch", pr(1, "aim/c1-x", "Thing (Z9)"), "C1 branch/high", ""},
		{"slug-prefixed branch", pr(1, "aim/c2-matcher", "No ID"), "C2 branch/high", ""},
		{"slug-prefixed branch in upper case", pr(1, "AIM/C2-matcher", "No ID"), "C2 branch/high", ""},
		{"slug-prefixed branch, longest ID", pr(1, "aim/b3b-2-fix", "No ID"), "B3b-2 branch/high", ""},
		{"slug-prefixed branch, ID alone", pr(1, "aim/c1", "No ID"), "C1 branch/high", ""},
		{"slug-prefixed branch naming nothing", pr(1, "aim/ci-merge-base", "Local CI"), "",
			"the branch is under aim/ but names no package of aim"},
		{"branch token, first seen after registration", pr(1, "share/c2-thing", "No ID"), "C2 branch/low", ""},
		{"branch token, first seen before registration", seenAt(pr(1, "share/c2-thing", "No ID"), registered.Add(-time.Minute)), "",
			"its branch names C2 only as a token, and the branch was first seen before plan aim was registered"},
		{"branch token, never seen", seenAt(pr(1, "share/c2-thing", "No ID"), time.Time{}), "",
			"its branch names C2 only as a token, and the branch was first seen before plan aim was registered"},
		// GitHub's createdAt is the first sighting when it's stored: aimd
		// first seeing a PR after registration doesn't make it new.
		{"branch token, opened before registration, stored after", opened(pr(1, "share/c2-thing", "No ID"), registered.Add(-time.Hour)), "",
			"its branch names C2 only as a token, and the branch was first seen before plan aim was registered"},
		{"branch token, opened after registration", opened(pr(1, "share/c2-thing", "No ID"), registered.Add(time.Minute)), "C2 branch/low", ""},
		{"branch naming two packages", pr(1, "fix/a1-a2", "No ID"), "",
			"ambiguous: it could be aim · A1 or aim · A2; name the package in a Plan: line or a <slug>/<id>-… branch"},
		{"branch with no ID", pr(1, "feature/api-v20", "No ID"), "", ""},
		{"PR from a base branch", pr(1, "main", "No ID"), "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInput(t)
			in.PRs = []store.PR{tc.pr}
			m := Match(in)
			var got string
			for _, l := range m.Links[1] {
				got = l.PkgID + " " + l.MatchMethod + "/" + l.Confidence
				if l.Kind != store.LinkPR || l.Ref != prRef(1) {
					t.Errorf("link %+v", l)
				}
			}
			if got != tc.want {
				t.Errorf("link = %q, want %q", got, tc.want)
			}
			ps := Derive(in, m, Options{})[0]
			notes := unmatchedNotes(ps)
			if tc.want != "" {
				if len(notes) != 0 {
					t.Errorf("unmatched = %v", notes)
				}
				return
			}
			if note, ok := notes[prRef(1)]; !ok || note != tc.note {
				t.Errorf("unmatched = %v, want note %q", notes, tc.note)
			}
		})
	}
}

func withPlan(p store.PR, slug, id string) store.PR {
	p.PlanSlug, p.PlanPkg = slug, id
	return p
}

func seenAt(p store.PR, t time.Time) store.PR {
	p.FirstSeenAt = t
	return p
}

// opened sets GitHub's createdAt.
func opened(p store.PR, t time.Time) store.PR {
	p.CreatedAt = t
	return p
}

// A repo's base branches are its default branch, once GitHub has named
// it, and the branches PRs target. Until then main, master, trunk and
// develop stand in for the default.
func TestBaseBranches(t *testing.T) {
	in := baseInput(t) // the main checkout is on main
	in.Worktrees = append(in.Worktrees, worktree("dev", "develop"), worktree("t", "trunk"), worktree("x", "next"))
	unmatchedBranches := func(in Input) []string {
		var out []string
		for _, u := range derive(in).Unmatched {
			if u.Kind == store.LinkBranch {
				out = append(out, u.Branch)
			}
		}
		slices.Sort(out)
		return out
	}

	deepEq(t, "default not known", unmatchedBranches(in), []string{"next"})
	// Another repo's default changes nothing here.
	in.DefaultBranches = map[string]string{"example-org/web": "next"}
	deepEq(t, "another repo's default", unmatchedBranches(in), []string{"next"})

	// GitHub says trunk (the name compares in any case): main and develop
	// are work branches like any other.
	in.DefaultBranches = map[string]string{"Example-Org/App": "trunk"}
	deepEq(t, "default trunk", unmatchedBranches(in), []string{"develop", "main", "next"})

	// A branch a PR targets is a base too.
	in.PRs = []store.PR{withBase(pr(1, "feature/x", "Thing (A1)"), "develop")}
	deepEq(t, "develop a PR base", unmatchedBranches(in), []string{"main", "next"})

	// The real default is never matched, even by a name that would match.
	in = baseInput(t)
	in.DefaultBranches = map[string]string{repo: "aim/a1-one"}
	in.Worktrees = append(in.Worktrees, worktree("a", "aim/a1-one"))
	if links := directLinks(derive(in)); len(links) != 0 {
		t.Errorf("default branch matched: %q", links)
	}
}

func withBase(p store.PR, base string) store.PR {
	p.BaseRef = base
	return p
}

// A branch checked out in a worktree with no PR is matched by its name,
// with its first sighting the worktree's.
func TestMatchWorktreeBranches(t *testing.T) {
	in := baseInput(t)
	old := worktree("old", "draft/c1-idea")
	old.FirstSeen = registered.Add(-time.Hour)
	in.Worktrees = append(in.Worktrees,
		worktree("a", "aim/a1-one"),
		worktree("b", "share/b2-two"),
		old,
		worktree("d", "detached"),
		worktree("e", "main"), // a default branch is never matched or listed
	)
	in.Worktrees[4].Detached, in.Worktrees[4].Branch = true, ""
	ps := derive(in)
	deepEq(t, "direct links", directLinks(ps), []string{
		"A1 branch example-org/app:aim/a1-one branch/high",
		"B2 branch example-org/app:share/b2-two branch/low",
	})
	deepEq(t, "A1 links", refs(pkgOf(t, ps, "A1")), []string{
		"branch example-org/app:aim/a1-one",
		"worktree " + wtRoot + "a < example-org/app:aim/a1-one",
	})
	deepEq(t, "unmatched", unmatchedNotes(ps), map[string]string{
		"example-org/app:draft/c1-idea": "its branch names C1 only as a token, and the branch was first seen before plan aim was registered",
	})
}

// Package IDs are only looked up in plans registered for the PR's or
// branch's repo; cross-repo PRs never count.
func TestMatchRepoScoping(t *testing.T) {
	in := baseInput(t)
	other := pr(2, "feature/x", "Thing (A1)")
	other.Repo = "example-org/web"
	fork := pr(3, "feature/x", "Thing (A2)")
	fork.IsCrossRepository = true
	in.PRs = []store.PR{other, fork}
	ps := derive(in)
	if links := directLinks(ps); len(links) != 0 {
		t.Errorf("links = %q", links)
	}
	if len(ps.Unmatched) != 0 {
		t.Errorf("unmatched = %+v", ps.Unmatched)
	}

	// A plan registered by a main checkout's path matches that repo's
	// worktrees even when the GitHub name isn't known.
	in = baseInput(t)
	in.Plans[0].Repos = []string{repoPath + "/"}
	in.RepoNames = nil
	in.Worktrees = append(in.Worktrees, worktree("a", "aim/a1-one"))
	ps = derive(in)
	deepEq(t, "by path", directLinks(ps), []string{"A1 branch /work/app:aim/a1-one branch/high"})
}

// An ID two plans of the repo share needs the slug.
func TestMatchAmbiguous(t *testing.T) {
	const second = "# Second\n\nPlan slug: other\n\n| Package | Needs |\n|---|---|\n| **A1 Theirs** | — |\n| **D1 Only here** | — |\n"
	in := baseInput(t)
	in.Plans = append(in.Plans, mdPlan(t, 2, second, registered, repo))
	in.PRs = []store.PR{
		pr(1, "feature/x", "Thing (A1)"),                          // ambiguous
		withPlan(pr(2, "feature/y", "Thing (A1)"), "other", "A1"), // the Plan: line names it
		pr(3, "other/a1-x", "No ID"),                              // so does a slug-prefixed branch
		pr(4, "fix/a1-thing", "No ID"),                            // a token is ambiguous too
		pr(5, "feature/z", "Thing (D1)"),                          // only one plan has D1
	}
	m := Match(in)
	states := Derive(in, m, Options{})
	deepEq(t, "aim links", directLinks(states[0]), []string(nil))
	deepEq(t, "other links", directLinks(states[1]), []string{
		"A1 pr example-org/app#2 plan_line/exact",
		"A1 pr example-org/app#3 branch/high",
		"D1 pr example-org/app#5 title/high",
	})
	want := map[string]string{
		prRef(1): "ambiguous: it could be aim · A1 or other · A1; name the package in a Plan: line or a <slug>/<id>-… branch",
		prRef(4): "ambiguous: it could be aim · A1 or other · A1; name the package in a Plan: line or a <slug>/<id>-… branch",
	}
	// Both plans list them: they're in both plans' repo.
	deepEq(t, "aim unmatched", unmatchedNotes(states[0]), want)
	deepEq(t, "other unmatched", unmatchedNotes(states[1]), want)
}

// The matcher never brings back a link you removed, and a spawn or manual
// link settles a target: the matcher proposes nothing else for it.
func TestMatchTombstonesAndClaims(t *testing.T) {
	in := baseInput(t)
	in.PRs = []store.PR{pr(1, "feature/x", "Thing (A1)"), pr(2, "feature/y", "Thing (A1)")}
	in.Tombstones = []store.Tombstone{{PlanID: 1, PkgID: "a1", Kind: store.LinkPR, Ref: prRef(1)}}
	in.Links = []store.Link{link("C1", store.LinkPR, prRef(2), store.MatchManual, store.ConfidenceExact)}
	m := Match(in)
	if len(m.Links[1]) != 0 {
		t.Errorf("matcher links = %+v", m.Links[1])
	}
	ps := Derive(in, m, Options{})[0]
	deepEq(t, "direct links", directLinks(ps), []string{"C1 pr example-org/app#2 manual/exact"})
	deepEq(t, "unmatched", unmatchedNotes(ps), map[string]string{prRef(1): "you unlinked it from aim · A1"})
	if a1 := pkgOf(t, ps, "A1"); a1.Lifecycle != Ready || len(a1.Links) != 0 {
		t.Errorf("A1 = %s with links %q", a1.Lifecycle, refs(a1))
	}
}

// A branch some PR targets is a base and never matched, unless it is a
// PR's head itself: a stacked PR's base is still work.
func TestMatchStackedPRs(t *testing.T) {
	in := baseInput(t)
	bottom := pr(1, "aim/a1-base", "No ID")
	top := pr(2, "aim/a2-top", "No ID")
	top.BaseRef = "aim/a1-base"
	release := pr(3, "aim/c1-x", "No ID")
	release.BaseRef = "release/2"
	in.PRs = []store.PR{bottom, top, release}
	in.Worktrees = append(in.Worktrees, worktree("r", "release/2"))
	ps := derive(in)
	deepEq(t, "links", directLinks(ps), []string{
		"A1 pr example-org/app#1 branch/high",
		"A2 pr example-org/app#2 branch/high",
		"C1 pr example-org/app#3 branch/high",
	})
	if len(ps.Unmatched) != 0 {
		t.Errorf("unmatched = %+v", ps.Unmatched) // release/2 is a base: not listed
	}
}

// Match's answer is ordered, so storing it is deterministic.
func TestMatchIsSorted(t *testing.T) {
	in := baseInput(t)
	in.PRs = []store.PR{pr(3, "x", "(C1)"), pr(1, "y", "(A2)"), pr(2, "z", "(A1)")}
	var got []string
	for _, l := range Match(in).Links[1] {
		got = append(got, l.PkgID+" "+l.Ref)
	}
	deepEq(t, "order", got, []string{"A1 " + prRef(2), "A2 " + prRef(1), "C1 " + prRef(3)})
}
