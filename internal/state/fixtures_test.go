package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/github"
	"github.com/gravexa-llc/ai-manager/internal/plans"
	"github.com/gravexa-llc/ai-manager/internal/store"
)

func readTestdata(tb testing.TB, parts ...string) []byte {
	tb.Helper()
	b, err := os.ReadFile(filepath.Join(append([]string{"..", "..", "testdata"}, parts...)...))
	if err != nil {
		tb.Fatal(err)
	}
	return b
}

// fixturePlan registers a plan fixture from testdata/plans the way the plan
// service (internal/planapi) does:
// slug from plans.Slug, packages from the parse, the file as SourceMD.
func fixturePlan(tb testing.TB, id int64, name, path string, created time.Time, repos ...string) Plan {
	tb.Helper()
	src := readTestdata(tb, "plans", name)
	parsed := plans.Parse(src)
	pkgs := StorePackages(parsed)
	for i := range pkgs {
		pkgs[i].PlanID = id
	}
	return Plan{
		Plan: store.Plan{ID: id, Slug: plans.Slug(path, src), Title: parsed.Title, SourcePath: path,
			SourceMD: string(src), Repos: repos, CreatedAt: created},
		Packages: pkgs,
	}
}

// fixturePRs reads the recorded GraphQL answers (testdata/github) into
// store rows, the way the GitHub poller stores them. A PR node's repo is
// its own repository field (search answers) or its parent repo's
// nameWithOwner (per-branch answers). FirstSeenAt is the PR's createdAt,
// as if aimd had been polling when it opened. A PR in several files is
// kept once.
func fixturePRs(tb testing.TB, files ...string) []store.PR {
	tb.Helper()
	seen := map[string]bool{}
	var out []store.PR
	var walk func(v any, repo string)
	walk = func(v any, repo string) {
		switch v := v.(type) {
		case []any:
			for _, e := range v {
				walk(e, repo)
			}
		case map[string]any:
			if r, ok := v["nameWithOwner"].(string); ok {
				repo = r
			}
			if _, ok := v["number"]; ok {
				if _, ok := v["headRefName"]; ok {
					pr := fixturePR(tb, v, repo)
					if k := store.PRRef(pr.Repo, pr.Number); !seen[k] {
						seen[k] = true
						out = append(out, pr)
					}
					return
				}
			}
			keys := make([]string, 0, len(v))
			for k := range v {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				walk(v[k], repo)
			}
		}
	}
	for _, f := range files {
		var v any
		if err := json.Unmarshal(readTestdata(tb, "github", f), &v); err != nil {
			tb.Fatalf("%s: %v", f, err)
		}
		walk(v, "")
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Number < out[j].Number
	})
	return out
}

func fixturePR(tb testing.TB, n map[string]any, repo string) store.PR {
	tb.Helper()
	str := func(k string) string { s, _ := n[k].(string); return s }
	tm := func(k string) time.Time {
		s := str(k)
		if s == "" {
			return time.Time{}
		}
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			tb.Fatalf("%s: %v", k, err)
		}
		return t
	}
	if r, ok := n["repository"].(map[string]any); ok {
		repo, _ = r["nameWithOwner"].(string)
	}
	pr := store.PR{
		Repo: repo, Number: int(n["number"].(float64)),
		HeadBranch: str("headRefName"), HeadRefOID: str("headRefOid"), BaseRef: str("baseRefName"),
		Title: str("title"), State: str("state"), Mergeable: str("mergeable"),
		MergeStateStatus: str("mergeStateStatus"), ReviewDecision: str("reviewDecision"), URL: str("url"),
		UpdatedAt: tm("updatedAt"), MergedAt: tm("mergedAt"), ClosedAt: tm("closedAt"), FirstSeenAt: tm("createdAt"),
	}
	pr.Draft, _ = n["isDraft"].(bool)
	pr.IsCrossRepository, _ = n["isCrossRepository"].(bool)
	if a, ok := n["author"].(map[string]any); ok {
		pr.Author, _ = a["login"].(string)
	}
	if r, ok := n["statusCheckRollup"].(map[string]any); ok {
		pr.ChecksState, _ = r["state"].(string)
	}
	if ref, ok := github.ParsePlanLine(str("body")); ok {
		pr.PlanSlug, pr.PlanPkg = ref.Slug, ref.ID
	}
	return pr
}

// lifecycles maps each lifecycle to the IDs in it, in plan order.
func lifecycles(ps PlanState) map[Lifecycle][]string {
	out := map[Lifecycle][]string{}
	for _, p := range ps.Packages {
		out[p.Lifecycle] = append(out[p.Lifecycle], p.ID)
	}
	return out
}

// directLinks lists a plan state's direct links as "ID kind ref method/confidence".
func directLinks(ps PlanState) []string {
	var out []string
	for _, p := range ps.Packages {
		for _, l := range p.Links {
			if l.Via.IsZero() {
				out = append(out, p.ID+" "+l.Kind+" "+l.Ref+" "+l.Method+"/"+l.Confidence)
			}
		}
	}
	return out
}

func badgeKinds(p PackageState) []string {
	var out []string
	for _, b := range p.Badges {
		out = append(out, string(b.Kind))
	}
	return out
}

func deepEq[T any](t *testing.T, what string, got, want T) {
	t.Helper()
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if string(g) != string(w) {
		t.Errorf("%s:\n got %s\nwant %s", what, g, w)
	}
}

var sharingRepos = []string{"acme/pivot", "acme/pivot-web"}

// sharingInput is the sharing plan against the recorded PRs: the two
// per-branch answers the brief names, plus the open-PR search, which holds
// pivot #215, #216 and pivot-web #31 (the per-branch fixtures were
// recorded for fewer branches) and PRs of unrelated repos.
func sharingInput(t *testing.T, registered time.Time) Input {
	return Input{
		Now:   time.Date(2026, 11, 6, 6, 0, 0, 0, time.UTC),
		Plans: []Plan{fixturePlan(t, 1, "sharing-plan.md", "docs/plans/sharing-plan.md", registered, sharingRepos...)},
		PRs:   fixturePRs(t, "branch_prs_pivot.json", "branch_prs_pivot-web.json", "open_prs_mine_first_read.json"),
	}
}

func TestSharingPlanAgainstRecordedPRs(t *testing.T) {
	// Registered before the PRs opened, so the low-confidence branch token
	// of #219 (whose recorded title lost its "(T1)" to scrubbing) counts.
	in := sharingInput(t, time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	if in.Plans[0].Slug != "sharing" {
		t.Fatalf("slug = %q", in.Plans[0].Slug)
	}
	m := Match(in)
	ps := Derive(in, m, Options{})[0]

	deepEq(t, "direct links", directLinks(ps), []string{
		"A2 pr acme/pivot#215 title/high",
		"A3 pr acme/pivot#217 title/high",
		"A4 pr acme/pivot#216 title/high",
		"W2 pr acme/pivot-web#31 title/high",
		"T1 pr acme/pivot#219 branch/low",
		"V1 pr acme/pivot#214 title/high",
		"W3 pr acme/pivot-web#32 title/high",
		"S1 pr acme/pivot#218 title/high",
	})
	deepEq(t, "lifecycles", lifecycles(ps), map[Lifecycle][]string{
		InReview: {"A2", "A3", "A4", "W2", "T1", "V1", "W3", "S1"},
		Ready:    {"G1", "A1", "W1", "Q4"}, // Q4's "everything above" is a note, not a dependency
		Blocked:  {"Q1", "Q2", "Q3"},
	})
	deepEq(t, "counts", ps.Counts, map[Lifecycle]int{InReview: 8, Ready: 4, Blocked: 3})
	deepEq(t, "ready to dispatch", ps.ReadyToDispatch, []string{"G1", "A1", "W1", "Q4"})
	for id, want := range map[string][]string{"Q1": {"T1", "V1"}, "Q2": {"T1"}, "Q3": {"Q2", "V1"}, "A2": {"A1"}} {
		p, _ := ps.Package(id)
		deepEq(t, id+" blocked by", p.BlockedBy, want)
	}

	// Every open PR is CLEAN with no draft: waiting on you. T1's link came
	// from a branch token.
	for _, p := range ps.Packages {
		switch {
		case p.ID == "T1":
			deepEq(t, "T1 badges", badgeKinds(p), []string{"waiting_on_you", "low_confidence"})
		case p.Lifecycle == InReview:
			deepEq(t, p.ID+" badges", badgeKinds(p), []string{"waiting_on_you"})
		default:
			deepEq(t, p.ID+" badges", badgeKinds(p), []string(nil))
		}
	}
	// A PR reaches its head branch.
	v1, _ := ps.Package("V1")
	if len(v1.Links) != 2 || v1.Links[1].Ref != "acme/pivot:share/v1-email-reports" || v1.Links[1].Via.Ref != "acme/pivot#214" || v1.Links[1].Method != store.MatchTitle {
		t.Errorf("V1 links = %+v", v1.Links)
	}

	// pivot-web #33 merged after registration and names no package.
	if len(ps.Unmatched) != 1 || ps.Unmatched[0].Ref != "acme/pivot-web#33" || ps.Unmatched[0].Note != "" {
		t.Errorf("unmatched = %+v", ps.Unmatched)
	}
	if len(ps.Warnings) != 0 || len(ps.Alerts) != 0 {
		t.Errorf("warnings %+v, alerts %+v", ps.Warnings, ps.Alerts)
	}
}

// Registered after the PRs were first seen, as it would be today: #219's
// branch token no longer counts, so T1 is blocked and #219 is unmatched,
// saying why.
func TestSharingPlanRegisteredLate(t *testing.T) {
	in := sharingInput(t, time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC))
	ps := Derive(in, Match(in), Options{})[0]
	deepEq(t, "lifecycles", lifecycles(ps), map[Lifecycle][]string{
		InReview: {"A2", "A3", "A4", "W2", "V1", "W3", "S1"},
		Ready:    {"G1", "A1", "W1", "Q4"},
		Blocked:  {"T1", "Q1", "Q2", "Q3"},
	})
	var got []string
	for _, u := range ps.Unmatched {
		got = append(got, u.Ref+": "+u.Note)
	}
	deepEq(t, "unmatched", got, []string{
		"acme/pivot#219: its branch names T1 only as a token, and the branch was first seen before plan sharing was registered",
		"acme/pivot-web#33: ",
	})
}

// aimPRs are fourteen PRs for the roadmap fixture (titles, head branches,
// merge times, Plan: lines). All are merged. #1, #10 and #11 are
// lead PRs with no package; #12 has no Plan: line but its branch is under
// aim/ and names S2.
func aimPRs() []store.PR {
	const repo = "example-user/ai-manager"
	at := func(hm string) time.Time {
		t, _ := time.Parse("2006-01-02T15:04Z", "2026-11-06T"+hm+"Z")
		return t
	}
	pr := func(n int, branch, title, slug, pkg, merged string) store.PR {
		return store.PR{Repo: repo, Number: n, HeadBranch: branch, BaseRef: "main", Title: title, State: store.PRMerged,
			Mergeable: "UNKNOWN", MergeStateStatus: "UNKNOWN", PlanSlug: slug, PlanPkg: pkg,
			MergedAt: at(merged), ClosedAt: at(merged), UpdatedAt: at(merged), FirstSeenAt: at(merged)}
	}
	return []store.PR{
		pr(1, "aim/plan-v2.2", "Plan v2.2: mod-first reporting and work packages", "", "", "05:07"),
		pr(2, "aim/f1-repo-scaffold", "Repo scaffold (F1)", "aim", "F1", "05:21"),
		pr(3, "aim/s2-mod-event-probe", "Mod event probe (S2)", "aim", "S2", "05:37"),
		pr(4, "aim/s1-session-worktree-probes", "Session and worktree probes (S1)", "aim", "S1", "05:38"),
		pr(5, "aim/s3-github-query-budget", "GitHub query budget (S3)", "aim", "S3", "05:42"),
		pr(6, "aim/r0-fold-findings", "Fold phase-0 findings into the plan (R0)", "aim", "R0", "05:48"),
		pr(7, "aim/a4-session-poller", "Session poller (A4)", "aim", "A4", "06:01"),
		pr(8, "aim/a1-worktree-scanner", "Worktree scanner (A1)", "aim", "A1", "06:30"),
		pr(9, "aim/a2-github-client", "GitHub client (A2)", "aim", "A2", "06:12"),
		pr(10, "aim/local-ci", "Local CI on the lead's Mac", "", "", "06:16"),
		pr(11, "aim/ci-merge-base", "Local CI: test the merge with main, narrow what PR code can reach", "", "", "06:18"),
		pr(12, "aim/s2-check-doc", "S2 interactive check: complete steps for R1", "", "", "06:24"),
		pr(13, "aim/b1-store", "Store (B1)", "aim", "B1", "06:26"),
		pr(14, "aim/c1-plan-parser", "Plan parser and detector (C1)", "aim", "C1", "06:35"),
	}
}

func TestAimPlanAgainstItsPRs(t *testing.T) {
	registered := time.Date(2026, 11, 6, 7, 0, 0, 0, time.UTC)
	in := Input{
		Now:   registered.Add(time.Hour),
		Plans: []Plan{fixturePlan(t, 1, "ROADMAP.md", "ROADMAP.md", registered, "example-user/ai-manager")},
		PRs:   aimPRs(),
	}
	if in.Plans[0].Slug != "aim" {
		t.Fatalf("slug = %q", in.Plans[0].Slug)
	}
	ps := Derive(in, Match(in), Options{})[0]

	deepEq(t, "lifecycles", lifecycles(ps), map[Lifecycle][]string{
		Done:    {"F1", "S1", "S2", "S3", "R0", "A1", "A2", "A4", "B1", "C1"},
		Ready:   {"R1", "A3", "B2", "C2", "C5"},
		Blocked: {"B3", "B4", "C3", "C4", "C6", "C7", "D1", "D2", "D3", "E1", "E2"},
	})
	got := directLinks(ps)
	for _, want := range []string{
		"S2 pr example-user/ai-manager#3 plan_line/exact",
		"S2 pr example-user/ai-manager#12 branch/high", // no Plan: line; aim/s2-… names it
		"C1 pr example-user/ai-manager#14 plan_line/exact",
	} {
		found := false
		for _, g := range got {
			found = found || g == want
		}
		if !found {
			t.Errorf("missing link %q in %q", want, got)
		}
	}
	if len(got) != 11 {
		t.Errorf("%d direct links, want 11: %q", len(got), got)
	}
	// The lead PRs closed before the plan was registered: history, not
	// Unmatched.
	if len(ps.Unmatched) != 0 {
		t.Errorf("unmatched = %+v", ps.Unmatched)
	}
	for id, want := range map[string][]string{
		"R1": {"you"}, // "you: run the check"
		"B4": {"you"}, // blocked on B3, with your OK to install
		"F1": nil,     // done
		"C2": nil,     // ready
		"B3": nil,     // blocked
		"C7": {"you"}, // "you: OK, since it runs in every session"
		"E2": {"you"}, // "you: try Go on a few sessions"
		"D2": nil,     // no you: step
	} {
		p, _ := ps.Package(id)
		deepEq(t, id+" badges", badgeKinds(p), want)
	}
	r1, _ := ps.Package("R1")
	if r1.Badges[0].Text != "you: run the check" {
		t.Errorf("R1 badge = %q", r1.Badges[0].Text)
	}
	deepEq(t, "ready to dispatch", ps.ReadyToDispatch, []string{"R1", "A3", "B2", "C2", "C5"})
}
