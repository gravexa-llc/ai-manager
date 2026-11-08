package planapi_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/planapi"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// registerSharing registers the sharing plan for pivot (by path) and
// pivot-web (by name).
func (e *env) registerSharing() api.RegisterResponse {
	e.t.Helper()
	return ok[api.RegisterResponse](e.t, e.req("POST", "/api/plans", api.RegisterRequest{
		Path: e.sharing, Repos: []string{e.pivot, "acme/pivot-web"},
	}), http.StatusCreated)
}

func (e *env) board(slug string) api.Plan {
	e.t.Helper()
	return ok[api.Plan](e.t, e.req("GET", "/api/plans/"+slug, nil), http.StatusOK)
}

func pkgOf(t *testing.T, b api.Plan, id string) api.Package {
	t.Helper()
	for _, p := range b.Packages {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("plan %s has no package %s", b.Slug, id)
	return api.Package{}
}

func findLink(p api.Package, kind, ref string) (api.Link, bool) {
	for _, l := range p.Links {
		if l.Kind == kind && strings.EqualFold(l.Ref, ref) {
			return l, true
		}
	}
	return api.Link{}, false
}

func ids(ps []api.SummaryPackage) []string {
	out := []string{}
	for _, p := range ps {
		out = append(out, p.ID)
	}
	return out
}

// Writes need the token, or a dashboard page's CSRF token; reads don't.
func TestAuth(t *testing.T) {
	e := newEnv(t)
	body := api.PathRequest{Path: e.sharing}

	refused(t, "no token", e.send("POST", "/api/plans/detect", body), http.StatusUnauthorized, web.TokenHeader)
	refused(t, "wrong token", e.send("POST", "/api/plans/detect", body, web.TokenHeader+": nope"), http.StatusUnauthorized)
	refused(t, "register without token", e.send("POST", "/api/plans", api.RegisterRequest{Path: e.sharing}), http.StatusUnauthorized)
	refused(t, "unlink without token", e.send("DELETE", "/api/links/1", nil), http.StatusUnauthorized)
	refused(t, "done without token", e.send("POST", "/api/plans/sharing/packages/A1/done", nil), http.StatusUnauthorized)
	ok[api.Detection](t, e.req("POST", "/api/plans/detect", body), http.StatusOK)

	// A dashboard page's CSRF token works instead, from the same origin.
	tok := e.srv.CSRF().Token()
	ok[api.Detection](t, e.send("POST", "/api/plans/detect", body, web.CSRFHeader+": "+tok, "Origin: http://127.0.0.1:7777"), http.StatusOK)
	ok[api.Detection](t, e.send("POST", "/api/plans/detect", body, web.CSRFHeader+": "+tok, "Sec-Fetch-Site: same-origin"), http.StatusOK)
	refused(t, "forged CSRF token", e.send("POST", "/api/plans/detect", body, web.CSRFHeader+": forged"), http.StatusForbidden)
	refused(t, "CSRF token from another origin", e.send("POST", "/api/plans/detect", body, web.CSRFHeader+": "+tok, "Origin: https://evil.example"), http.StatusForbidden)
	refused(t, "CSRF token cross-site", e.send("POST", "/api/plans/detect", body, web.CSRFHeader+": "+tok, "Sec-Fetch-Site: cross-site"), http.StatusForbidden)

	// Reads need neither; the Host check still applies.
	ok[api.PlanList](t, e.send("GET", "/api/plans", nil), http.StatusOK)
	ok[api.Context](t, e.send("GET", "/api/context?cwd="+e.pivot, nil), http.StatusOK)
	if rec := e.send("GET", "/api/plans", nil, "Host: evil.example:7777"); rec.Code != http.StatusForbidden {
		t.Errorf("foreign Host: %d, want 403", rec.Code)
	}
}

// Every endpoint that reads a client-named file refuses anything but a
// .md file of at most 1 MB inside a root or a known repo.
func TestPathSafety(t *testing.T) {
	e := newEnv(t)
	inRoot := func(p string) string { return filepath.Join(e.root, p) }
	e.write(inRoot("notes.txt"), "| Package | Needs |\n|---|---|\n| **A1 X** | — |\n| **A2 Y** | A1 |\n")
	if err := os.Symlink(inRoot("notes.txt"), inRoot("sneaky.md")); err != nil {
		t.Fatal(err)
	}
	stranger := filepath.Join(e.tmp, "elsewhere", "plan.md") // not in a root or a known repo
	e.write(stranger, "# P\n\n| Package | Needs |\n|---|---|\n| **A1 X** | — |\n| **A2 Y** | A1 |\n")
	if err := os.Symlink(stranger, inRoot("link-out.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(inRoot("folder.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	big := inRoot("big.md")
	e.write(big, "# Big\n"+strings.Repeat("x", planapi.MaxPlanBytes))

	cases := []struct {
		name, path string
		status     int
		says       string
	}{
		{"empty", "", http.StatusBadRequest, "path is required"},
		{"relative", "docs/plan.md", http.StatusBadRequest, "must be absolute"},
		{"not markdown", inRoot("notes.txt"), http.StatusBadRequest, "only .md files"},
		{"missing", inRoot("missing.md"), http.StatusNotFound, "no such file"},
		{"symlink to a non-.md file", inRoot("sneaky.md"), http.StatusBadRequest, "isn't a .md file"},
		{"a folder", inRoot("folder.md"), http.StatusBadRequest, "isn't a regular file"},
		{"over 1 MB", big, http.StatusRequestEntityTooLarge, "at most"},
		{"outside roots and repos", stranger, http.StatusForbidden, "outside the configured roots"},
		{"missing, outside", filepath.Join(e.tmp, "elsewhere", "nope.md"), http.StatusForbidden, "outside the configured roots"},
		{"symlink out of the root", inRoot("link-out.md"), http.StatusForbidden, stranger},
	}
	for _, ep := range []string{"/api/plans", "/api/plans/detect", "/api/plans/dismiss"} {
		for _, c := range cases {
			refused(t, ep+" "+c.name, e.req("POST", ep, api.PathRequest{Path: c.path}), c.status, c.says)
		}
	}
	// A bad body is a 400 too.
	refused(t, "not JSON", e.req("POST", "/api/plans", "path="+e.sharing), http.StatusBadRequest, "JSON")
	refused(t, "empty body", e.req("POST", "/api/plans/detect", nil), http.StatusBadRequest, "empty")
	if n, _ := e.st.ListPlans(context.Background(), store.PlanFilter{}); len(n) != 0 {
		t.Errorf("refused requests registered %d plans", len(n))
	}
}

// Detect, dismiss and register, and how each changes what detect says.
func TestRegisterDetectDismiss(t *testing.T) {
	e := newEnv(t)
	d := ok[api.Detection](t, e.req("POST", "/api/plans/detect", api.PathRequest{Path: e.sharing}), http.StatusOK)
	if !d.IsPlan || d.Packages != 15 || d.Lanes != 4 || d.Slug != "sharing" || d.Registered || d.Dismissed || !d.Offer || d.Path != e.sharing {
		t.Errorf("detect before registering = %+v", d)
	}

	// A design doc isn't a plan; dismissing it is remembered.
	doc := e.copyFixture("design-doc.md", filepath.Join(e.pivot, "docs", "cache-plan.md"))
	d = ok[api.Detection](t, e.req("POST", "/api/plans/detect", api.PathRequest{Path: doc}), http.StatusOK)
	if d.IsPlan || d.Offer || d.Slug != "cache" {
		t.Errorf("detect design doc = %+v", d)
	}
	dis := ok[api.Dismissal](t, e.req("POST", "/api/plans/dismiss", api.PathRequest{Path: doc}), http.StatusOK)
	if !dis.Dismissed || dis.Path != doc || dis.DismissedAt.IsZero() {
		t.Errorf("dismiss = %+v", dis)
	}
	if d = ok[api.Detection](t, e.req("POST", "/api/plans/detect", api.PathRequest{Path: doc}), http.StatusOK); !d.Dismissed || d.Offer {
		t.Errorf("detect after dismiss = %+v", d)
	}
	// Registering a file with no package table is refused.
	refused(t, "register a design doc", e.req("POST", "/api/plans", api.RegisterRequest{Path: doc}), http.StatusUnprocessableEntity, "no package table")

	// Dismiss the sharing plan, then register it: registering clears the
	// dismissal.
	ok[api.Dismissal](t, e.req("POST", "/api/plans/dismiss", api.PathRequest{Path: e.sharing}), http.StatusOK)
	reg := e.registerSharing()
	if !reg.Created || reg.Slug != "sharing" || reg.Packages != 15 || reg.ReposFrom != "request" ||
		!slices.Equal(reg.Repos, []string{"acme/pivot", "acme/pivot-web"}) || reg.Backfill != "started" {
		t.Errorf("register = %+v", reg)
	}
	if reg.Summary.Band != "sharing · 0/15 done · 5 ready · 0 need you" { // G1 A1 A4 W1 Q4
		t.Errorf("band = %q", reg.Summary.Band)
	}
	d = ok[api.Detection](t, e.req("POST", "/api/plans/detect", api.PathRequest{Path: e.sharing}), http.StatusOK)
	if !d.Registered || d.Dismissed || d.Offer || d.Slug != "sharing" {
		t.Errorf("detect after register = %+v", d)
	}
	e.svc.Wait()
	if got := e.backfill.asked(); len(got) != 1 || !slices.Equal(got[0], reg.Repos) {
		t.Errorf("backfill asked %v, want %v", got, reg.Repos)
	}

	// Registering again updates the same plan.
	again := ok[api.RegisterResponse](t, e.req("POST", "/api/plans", api.RegisterRequest{Path: e.sharing, Repos: reg.Repos}), http.StatusOK)
	if again.Created || again.Slug != "sharing" {
		t.Errorf("register again = %+v", again)
	}

	// Without repos, a plan is registered for the repo its file is in
	// (known through the scan, outside the roots)…
	aim := ok[api.RegisterResponse](t, e.req("POST", "/api/plans", api.RegisterRequest{Path: e.aimPlan, SessionID: "lead-1"}), http.StatusCreated)
	if aim.Slug != "aim" || aim.ReposFrom != "file" || !slices.Equal(aim.Repos, []string{"example-user/ai-manager"}) || aim.Packages != 26 {
		t.Errorf("register ROADMAP.md = %+v", aim)
	}
	if b := e.board("aim"); b.RegisteredInSession != "lead-1" || b.Path != e.aimPlan {
		t.Errorf("aim board = %s in %q", b.Path, b.RegisteredInSession)
	}
	// …or for the repos its Repos: line names.
	marked := filepath.Join(e.web, "docs", "site-plan.md")
	e.write(marked, "# Site\n\nRepos: acme/pivot-web, `acme/pivot`\n\n| Package | Needs |\n|---|---|\n| **W1 One** | — |\n| **W2 Two** | W1 |\n")
	site := ok[api.RegisterResponse](t, e.req("POST", "/api/plans", api.RegisterRequest{Path: marked}), http.StatusCreated)
	if site.ReposFrom != "plan" || !slices.Equal(site.Repos, []string{"acme/pivot-web", "acme/pivot"}) {
		t.Errorf("register with a Repos: line = %+v", site)
	}

	// Another file with a slug in use is refused, naming the fix.
	dup := e.copyFixture("sharing-plan.md", filepath.Join(e.web, "docs", "sharing-plan.md"))
	refused(t, "slug in use", e.req("POST", "/api/plans", api.RegisterRequest{Path: dup}), http.StatusConflict, `"sharing"`, e.sharing, "Plan slug:")
	// Repos that are neither names nor repos are refused.
	refused(t, "bad repo", e.req("POST", "/api/plans", api.RegisterRequest{Path: e.sharing, Repos: []string{"not a repo"}}), http.StatusBadRequest, "owner/name")
	refused(t, "path outside", e.req("POST", "/api/plans", api.RegisterRequest{Path: e.sharing, Repos: []string{filepath.Join(e.tmp, "elsewhere")}}), http.StatusBadRequest, "isn't in a git repo")

	list := ok[api.PlanList](t, e.req("GET", "/api/plans", nil), http.StatusOK)
	var slugs []string
	for _, p := range list.Plans {
		slugs = append(slugs, p.Slug)
	}
	if !slices.Equal(slugs, []string{"sharing", "aim", "site"}) {
		t.Errorf("GET /api/plans slugs = %v", slugs)
	}
	refused(t, "unknown plan", e.req("GET", "/api/plans/nope", nil), http.StatusNotFound, `"nope"`)
	if e.notes.count(api.TopicPlans) == 0 {
		t.Error("no plans change event after registering")
	}
}

// A linked PR's "opened" timeline entry is GitHub's creation time, or when
// aimd first stored it for a PR stored without one.
func TestTimelinePROpened(t *testing.T) {
	e := newEnv(t)
	created := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	seen := created.Add(26 * time.Hour)
	open := func(n int, branch, id string) store.PR {
		return store.PR{Repo: "example-user/ai-manager", Number: n, HeadBranch: branch, BaseRef: "main", State: store.PROpen,
			Title: "x (" + id + ")", PlanSlug: "aim", PlanPkg: id, FirstSeenAt: seen}
	}
	withCreated := open(16, "aim/c2-matcher-state", "C2")
	withCreated.CreatedAt = created
	e.backfill.set("example-user/ai-manager", withCreated, open(15, "aim/b2-aimd-core", "B2"))
	ok[api.RegisterResponse](t, e.req("POST", "/api/plans", api.RegisterRequest{Path: e.aimPlan}), http.StatusCreated)
	e.svc.Wait()

	tl, err := e.svc.Timeline(context.Background(), "aim", api.TimelineQuery{})
	if err != nil {
		t.Fatal(err)
	}
	opened := map[int64]time.Time{}
	for _, en := range tl.Entries {
		if en.Kind == api.TimelinePR && en.Action == "opened" {
			opened[en.ID] = en.At
		}
	}
	if len(opened) != 2 || !opened[16].Equal(created) || !opened[15].Equal(seen) {
		t.Errorf("opened entries %v, want #16 at %v (created) and #15 at %v (first seen)", opened, created, seen)
	}
}

// The roadmap fixture: the backfilled merged PRs (Plan: lines) make their
// packages done, and what needed them ready.
func TestBoardFromBackfill(t *testing.T) {
	e := newEnv(t)
	merged := func(n int, branch, id string) store.PR {
		at := time.Date(2026, 11, 4, 10, n, 0, 0, time.UTC)
		return store.PR{Repo: "example-user/ai-manager", Number: n, HeadBranch: branch, BaseRef: "main", State: store.PRMerged,
			Title: "x (" + id + ")", PlanSlug: "aim", PlanPkg: id, MergedAt: at, ClosedAt: at, FirstSeenAt: at.Add(-time.Hour)}
	}
	e.backfill.set("example-user/ai-manager",
		merged(7, "aim/a4-session-poller", "A4"), merged(8, "aim/a1-worktree-scanner", "A1"),
		merged(9, "aim/a2-github-client", "A2"), merged(13, "aim/b1-store", "B1"),
		merged(14, "aim/c1-plan-parser", "C1"), merged(15, "aim/b2-aimd-core", "B2"),
		merged(16, "aim/c2-matcher-state", "C2"),
	)
	reg := ok[api.RegisterResponse](t, e.req("POST", "/api/plans", api.RegisterRequest{Path: e.aimPlan}), http.StatusCreated)
	// Before the backfill: only the ✅ packages are done.
	if reg.Summary.Done != 5 {
		t.Errorf("done before the backfill = %d (%s)", reg.Summary.Done, reg.Summary.Band)
	}
	e.svc.Wait()
	b := e.board("aim")
	for _, id := range []string{"F1", "S1", "A1", "A2", "A4", "B1", "B2", "C1", "C2"} {
		if p := pkgOf(t, b, id); p.Lifecycle != "done" {
			t.Errorf("%s = %s, want done", id, p.Lifecycle)
		}
	}
	c2 := pkgOf(t, b, "C2")
	if l, ok := findLink(c2, "pr", "example-user/ai-manager#16"); !ok || l.Method != "plan_line" || l.Confidence != "exact" || l.PR == nil || l.PR.State != "MERGED" {
		t.Errorf("C2's PR link = %+v (found %v)", l, ok)
	}
	for _, id := range []string{"A3", "C3", "C5", "B3", "D1"} {
		if p := pkgOf(t, b, id); p.Lifecycle != "ready" {
			t.Errorf("%s = %s (blocked by %v), want ready", id, p.Lifecycle, p.BlockedBy)
		}
	}
	if p := pkgOf(t, b, "C4"); p.Lifecycle != "blocked" || !slices.Equal(p.BlockedBy, []string{"C3"}) {
		t.Errorf("C4 = %s blocked by %v", p.Lifecycle, p.BlockedBy)
	}
	if b.Band != "aim · 12/26 done · 6 ready · 0 need you" {
		t.Errorf("band = %q", b.Band)
	}
	sum := ok[api.PlanSummary](t, e.req("GET", "/api/plans/aim/summary", nil), http.StatusOK)
	if sum.Band != b.Band || sum.Total != 26 || sum.Done != 12 || sum.Counts["done"] != 12 {
		t.Errorf("summary = %s, total %d, done %d, counts %v", sum.Band, sum.Total, sum.Done, sum.Counts)
	}
	if got := ids(sum.Ready); !slices.Equal(got, []string{"R1", "A3", "B3", "C3", "C5", "D1"}) {
		t.Errorf("ready = %v", got)
	}
	// R1 is ready and its Needs cell has a you: step, but nothing is in
	// the Needs you inbox: that step is on the board, not a need.
	if sum.NeedsYou != 0 || len(sum.NeedsYouPackages) != 0 || len(sum.NeedsYouItems) != 0 || sum.ToMerge != 0 {
		t.Errorf("needs you %d %+v %+v, to merge %d; want none", sum.NeedsYou, sum.NeedsYouPackages, sum.NeedsYouItems, sum.ToMerge)
	}
	if r1 := pkgOf(t, b, "R1"); r1.NeedsYou != "run the check" || !slices.ContainsFunc(r1.Badges, func(x api.Badge) bool { return x.Kind == "you" }) {
		t.Errorf("R1 = %+v, want its you: step and badge", r1)
	}
	if c2 := sum.Packages[15]; c2.ID != "C2" || !slices.Equal(c2.PRs, []string{"example-user/ai-manager#16"}) {
		t.Errorf("summary C2 = %+v", c2)
	}
	// ?since= in the future: nothing unlocked since; a bad time is refused.
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if s := ok[api.PlanSummary](t, e.req("GET", "/api/plans/aim/summary?since="+future, nil), http.StatusOK); len(s.Unlocked) != 0 || s.Since.IsZero() {
		t.Errorf("unlocked since %s = %v", future, ids(s.Unlocked))
	}
	refused(t, "bad since", e.req("GET", "/api/plans/aim/summary?since=yesterday", nil), http.StatusBadRequest, "RFC 3339")
	refused(t, "unknown plan summary", e.req("GET", "/api/plans/nope/summary", nil), http.StatusNotFound)
}

// Mark done unblocks what needs the package, and undoing it blocks it
// again; "unlocked since" lists what it unblocked.
func TestMarkDone(t *testing.T) {
	e := newEnv(t)
	e.registerSharing()
	before := time.Now().Add(-time.Second).UTC()
	b := e.board("sharing")
	if p := pkgOf(t, b, "A2"); p.Lifecycle != "blocked" || !slices.Equal(p.BlockedBy, []string{"A1"}) {
		t.Fatalf("A2 = %s blocked by %v", p.Lifecycle, p.BlockedBy)
	}
	res := ok[api.DoneResult](t, e.req("POST", "/api/plans/sharing/packages/a1/done", nil), http.StatusOK)
	if res.Package.ID != "A1" || res.Package.Lifecycle != "done" || !res.Package.DoneManual {
		t.Errorf("A1 after mark done = %+v", res.Package)
	}
	if !slices.Contains(ids(res.Summary.Ready), "A2") || res.Summary.Done != 1 {
		t.Errorf("after mark done: ready %v, done %d", ids(res.Summary.Ready), res.Summary.Done)
	}
	sum := ok[api.PlanSummary](t, e.req("GET", "/api/plans/sharing/summary?since="+before.Format(time.RFC3339), nil), http.StatusOK)
	if got := ids(sum.Unlocked); !slices.Equal(got, []string{"A2"}) {
		t.Errorf("unlocked since %v = %v, want [A2]", before, got)
	}
	if p := pkgOf(t, e.board("sharing"), "A2"); p.Lifecycle != "ready" || p.PrevLifecycle != "blocked" {
		t.Errorf("A2 = %s (was %s)", p.Lifecycle, p.PrevLifecycle)
	}

	res = ok[api.DoneResult](t, e.req("DELETE", "/api/plans/sharing/packages/A1/done", nil), http.StatusOK)
	if res.Package.Lifecycle != "ready" || res.Package.DoneManual {
		t.Errorf("A1 after undo = %+v", res.Package)
	}
	if p := pkgOf(t, e.board("sharing"), "A2"); p.Lifecycle != "blocked" {
		t.Errorf("A2 after undo = %s", p.Lifecycle)
	}
	refused(t, "unknown package", e.req("POST", "/api/plans/sharing/packages/Z9/done", nil), http.StatusNotFound, "Z9")
	refused(t, "unknown plan", e.req("POST", "/api/plans/nope/packages/A1/done", nil), http.StatusNotFound, "nope")
}

// Unregistering removes the plan with its packages, links and Mark done,
// leaves the file and other plans alone, and lets the file be registered
// afresh.
func TestUnregister(t *testing.T) {
	e := newEnv(t)
	e.registerSharing()
	ok[api.RegisterResponse](t, e.req("POST", "/api/plans", api.RegisterRequest{Path: e.aimPlan}), http.StatusCreated)
	ok[api.LinkResult](t, e.req("POST", "/api/links/manual", api.LinkRequest{Plan: "sharing", Pkg: "T1", Kind: "pr", Ref: "acme/pivot#219"}), http.StatusCreated)
	ok[api.DoneResult](t, e.req("POST", "/api/plans/sharing/packages/A1/done", nil), http.StatusOK)

	refused(t, "without token", e.send("DELETE", "/api/plans/sharing", nil), http.StatusUnauthorized)
	before := e.notes.count(api.TopicPlans)
	got := ok[api.Unregistered](t, e.req("DELETE", "/api/plans/SHARING", nil), http.StatusOK)
	if got.Slug != "sharing" || got.Path != e.sharing || got.Packages != 15 || got.Links < 1 {
		t.Errorf("unregistered = %+v", got)
	}
	if e.notes.count(api.TopicPlans) == before {
		t.Error("unregistering sent no plans change")
	}
	refused(t, "board after unregister", e.req("GET", "/api/plans/sharing", nil), http.StatusNotFound, "sharing")
	list := ok[api.PlanList](t, e.req("GET", "/api/plans", nil), http.StatusOK)
	if len(list.Plans) != 1 || list.Plans[0].Slug != "aim" {
		t.Errorf("plans after unregister = %d, want only aim", len(list.Plans))
	}
	if _, err := os.Stat(e.sharing); err != nil {
		t.Errorf("the plan file went: %v", err)
	}
	refused(t, "twice", e.req("DELETE", "/api/plans/sharing", nil), http.StatusNotFound, "sharing")

	// Registered again, it starts afresh: no manual link, no Mark done.
	again := e.registerSharing()
	if !again.Created {
		t.Error("re-registering didn't create the plan")
	}
	b := e.board("sharing")
	if _, linked := findLink(pkgOf(t, b, "T1"), "pr", "acme/pivot#219"); linked {
		t.Error("the manual link survived unregistering")
	}
	if p := pkgOf(t, b, "A1"); p.DoneManual || p.Lifecycle == "done" {
		t.Errorf("A1 after re-registering = %s, done_manual %v", p.Lifecycle, p.DoneManual)
	}
}

// A board carries the plan's lanes, single-writer marked.
func TestBoardLanes(t *testing.T) {
	e := newEnv(t)
	ok[api.RegisterResponse](t, e.req("POST", "/api/plans", api.RegisterRequest{Path: e.aimPlan}), http.StatusCreated)
	b := e.board("aim")
	var got []string
	for _, l := range b.Lanes {
		got = append(got, l.Name+" "+strconv.FormatBool(l.SingleWriter)+" "+strings.Join(l.Packages, ","))
	}
	want := []string{"Spike false S1,S2,S3", "Mod true C4,D2,E1", "Store true B1,C2,C3,D1", "Web true B3,C6,D3", "Plan false R0"}
	if !slices.Equal(got, want) {
		t.Errorf("lanes = %q, want %q", got, want)
	}
}

// A spawn link puts the agent on its package; the PR it then creates
// lands there too, before any poll.
func TestSpawnAndPRLinks(t *testing.T) {
	e := newEnv(t)
	e.registerSharing()
	spawn := ok[api.LinkResult](t, e.req("POST", "/api/links/spawn", api.SpawnLinkRequest{
		SessionID: "lead-1", AgentID: "agent-v1", Plan: "SHARING", Pkg: "v1",
	}), http.StatusCreated)
	if len(spawn.Links) != 1 || spawn.Links[0].Ref != "lead-1/agent-v1" || spawn.Links[0].Method != "spawn" || spawn.Links[0].ID == 0 ||
		spawn.Links[0].Plan != "sharing" || spawn.Links[0].Pkg != "V1" {
		t.Errorf("spawn links = %+v", spawn.Links)
	}
	if len(spawn.Packages) != 1 {
		t.Fatalf("spawn packages = %+v", spawn.Packages)
	}
	if l, ok := findLink(spawn.Packages[0], "agent", "lead-1/agent-v1"); !ok || l.Method != "spawn" || l.Confidence != "exact" || l.Inferred {
		t.Errorf("V1's agent link = %+v (found %v)", l, ok)
	}

	pr := ok[api.LinkResult](t, e.req("POST", "/api/links/pr", api.PRLinkRequest{
		SessionID: "lead-1", AgentID: "agent-v1", URL: "https://github.com/acme/pivot/pull/214",
	}), http.StatusCreated)
	v1 := pr.Packages[0]
	l, found := findLink(v1, "pr", "acme/pivot#214")
	if !found || l.Method != "spawn" || l.Confidence != "exact" || l.PR == nil || l.PR.State != "OPEN" || l.PR.URL != "https://github.com/acme/pivot/pull/214" || !l.PR.Unread {
		t.Errorf("V1's PR link = %+v (found %v)", l, found)
	}
	if v1.Lifecycle != "in_review" {
		t.Errorf("V1 = %s, want in_review", v1.Lifecycle)
	}
	// The placeholder is asked for at once, not left for the next poll.
	if got := e.refresh.take(); !slices.Equal(got, []string{"acme/pivot#214"}) {
		t.Errorf("refreshes after the PR link = %q, want acme/pivot#214", got)
	}
	// The poll's real row replaces the placeholder and keeps the link.
	must(t, e.st.UpsertPRs(context.Background(), store.PR{Repo: "acme/pivot", Number: 214, HeadBranch: "share/v1-email-reports",
		State: store.PROpen, Title: "Weekly email reports (V1)", MergeStateStatus: "CLEAN", URL: "https://github.com/acme/pivot/pull/214"}))
	ok[api.DoneResult](t, e.req("POST", "/api/plans/sharing/packages/G1/done", nil), http.StatusOK) // runs a pass
	v1 = pkgOf(t, e.board("sharing"), "V1")
	if l, _ := findLink(v1, "pr", "acme/pivot#214"); l.Method != "spawn" || l.PR == nil || l.PR.Title != "Weekly email reports (V1)" || l.PR.Unread {
		t.Errorf("V1's PR link after the poll = %+v", l)
	}
	// Linked again once read, it isn't asked for again.
	ok[api.LinkResult](t, e.req("POST", "/api/links/pr", api.PRLinkRequest{
		SessionID: "lead-1", AgentID: "agent-v1", URL: "https://github.com/acme/pivot/pull/214",
	}), http.StatusCreated)
	if got := e.refresh.take(); len(got) != 0 {
		t.Errorf("refreshes after linking a read PR = %q, want none", got)
	}
	if !slices.ContainsFunc(v1.Badges, func(b api.Badge) bool { return b.Kind == "waiting_on_you" }) {
		t.Errorf("V1 badges = %+v, want waiting_on_you", v1.Badges)
	}

	// A remote agent is linked by its cloud session.
	cloud := ok[api.LinkResult](t, e.req("POST", "/api/links/spawn", api.SpawnLinkRequest{
		SessionID: "lead-1", CloudURL: "https://claude.ai/code/session_abc", Plan: "sharing", Pkg: "W3",
	}), http.StatusCreated)
	if _, ok := findLink(cloud.Packages[0], "cloud_session", "https://claude.ai/code/session_abc"); !ok {
		t.Errorf("W3 links = %+v", cloud.Packages[0].Links)
	}

	refused(t, "PR from an unlinked agent", e.req("POST", "/api/links/pr", api.PRLinkRequest{SessionID: "lead-1", AgentID: "stranger", URL: "https://github.com/acme/pivot/pull/9"}), http.StatusNotFound, "isn't linked")
	refused(t, "PR link with a bad URL", e.req("POST", "/api/links/pr", api.PRLinkRequest{SessionID: "lead-1", AgentID: "agent-v1", URL: "https://example.com/x"}), http.StatusBadRequest, "pull request URL")
	refused(t, "PR link without session", e.req("POST", "/api/links/pr", api.PRLinkRequest{URL: "https://github.com/o/r/pull/1"}), http.StatusBadRequest, "session_id")
	refused(t, "spawn without session", e.req("POST", "/api/links/spawn", api.SpawnLinkRequest{AgentID: "a", Plan: "sharing", Pkg: "V1"}), http.StatusBadRequest, "session_id")
	refused(t, "spawn with both", e.req("POST", "/api/links/spawn", api.SpawnLinkRequest{SessionID: "s", AgentID: "a", CloudURL: "https://x.example/s", Plan: "sharing", Pkg: "V1"}), http.StatusBadRequest, "not both")
	refused(t, "spawn with neither", e.req("POST", "/api/links/spawn", api.SpawnLinkRequest{SessionID: "s", Plan: "sharing", Pkg: "V1"}), http.StatusBadRequest, "required")
	refused(t, "spawn, unknown plan", e.req("POST", "/api/links/spawn", api.SpawnLinkRequest{SessionID: "s", AgentID: "a", Plan: "nope", Pkg: "V1"}), http.StatusNotFound, "nope")
	refused(t, "spawn, unknown package", e.req("POST", "/api/links/spawn", api.SpawnLinkRequest{SessionID: "s", AgentID: "a", Plan: "sharing", Pkg: "Z9"}), http.StatusNotFound, "Z9")
}

// A PR the matcher linked by its title, unlinked by hand, stays unlinked
// on later passes and shows in the Unmatched lane; a manual link brings it
// back.
func TestManualUnlinkStays(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	must(t, e.st.UpsertPRs(ctx, store.PR{Repo: "acme/pivot", Number: 215, HeadBranch: "share/a2-token-revocation",
		State: store.PROpen, Title: "Revoke share tokens (A2)", URL: "https://github.com/acme/pivot/pull/215"}))
	e.registerSharing()
	a2 := pkgOf(t, e.board("sharing"), "A2")
	l, found := findLink(a2, "pr", "acme/pivot#215")
	if !found || l.Method != "title" || l.Confidence != "high" || !l.Inferred || l.ID == 0 {
		t.Fatalf("A2's PR link = %+v (found %v)", l, found)
	}
	if a2.Lifecycle != "in_review" {
		t.Errorf("A2 = %s", a2.Lifecycle)
	}

	un := ok[api.LinkResult](t, e.req("DELETE", "/api/links", api.LinkRequest{Plan: "sharing", Pkg: "A2", Kind: "pr", Ref: "https://github.com/acme/pivot/pull/215"}), http.StatusOK)
	if len(un.Links) != 1 || un.Links[0].Ref != "acme/pivot#215" || un.Links[0].ID != 0 {
		t.Errorf("unlink links = %+v", un.Links)
	}
	if _, found := findLink(un.Packages[0], "pr", "acme/pivot#215"); found || un.Packages[0].Lifecycle != "blocked" {
		t.Errorf("A2 after unlink = %s, links %+v", un.Packages[0].Lifecycle, un.Packages[0].Links)
	}
	// Later passes don't bring it back.
	ok[api.DoneResult](t, e.req("POST", "/api/plans/sharing/packages/G1/done", nil), http.StatusOK)
	ok[api.RegisterResponse](t, e.req("POST", "/api/plans", api.RegisterRequest{Path: e.sharing, Repos: []string{"acme/pivot"}}), http.StatusOK)
	b := e.board("sharing")
	if _, found := findLink(pkgOf(t, b, "A2"), "pr", "acme/pivot#215"); found {
		t.Error("the matcher re-made the unlinked PR link")
	}
	if !slices.ContainsFunc(b.Unmatched, func(u api.Unmatched) bool {
		return u.Ref == "acme/pivot#215" && strings.Contains(u.Note, "unlinked")
	}) {
		t.Errorf("unmatched = %+v, want #215 with an unlinked note", b.Unmatched)
	}
	// Unlinking twice is fine.
	ok[api.LinkResult](t, e.req("DELETE", "/api/links", api.LinkRequest{Plan: "sharing", Pkg: "A2", Kind: "pr", Ref: "acme/pivot#215"}), http.StatusOK)

	// A manual link brings it back, and can be removed by its id.
	man := ok[api.LinkResult](t, e.req("POST", "/api/links/manual", api.LinkRequest{Plan: "sharing", Pkg: "A2", Kind: "pr", Ref: "acme/pivot#215"}), http.StatusCreated)
	l, found = findLink(man.Packages[0], "pr", "acme/pivot#215")
	if !found || l.Method != "manual" || l.Confidence != "exact" || l.Inferred || man.Links[0].ID == 0 {
		t.Fatalf("A2 after the manual link = %+v (found %v)", l, found)
	}
	byID := ok[api.LinkResult](t, e.req("DELETE", "/api/links/"+itoa(man.Links[0].ID), nil), http.StatusOK)
	if _, found := findLink(byID.Packages[0], "pr", "acme/pivot#215"); found {
		t.Error("DELETE /api/links/{id} left the link")
	}
	refused(t, "unknown link id", e.req("DELETE", "/api/links/99999", nil), http.StatusNotFound)
	refused(t, "bad link id", e.req("DELETE", "/api/links/x", nil), http.StatusBadRequest)

	// Manual links of the other kinds, and bad ones.
	for _, c := range []api.LinkRequest{
		{Plan: "sharing", Pkg: "W2", Kind: "branch", Ref: "acme/pivot-web:share/w2-share-dialog"},
		{Plan: "sharing", Pkg: "W2", Kind: "worktree", Ref: e.web},
		{Plan: "sharing", Pkg: "W2", Kind: "agent", Ref: "sess-9/agent-9"},
		{Plan: "sharing", Pkg: "W2", Kind: "agent", Ref: "job:bg-1"},
	} {
		res := ok[api.LinkResult](t, e.req("POST", "/api/links/manual", c), http.StatusCreated)
		if _, found := findLink(res.Packages[0], c.Kind, c.Ref); !found {
			t.Errorf("manual %s %s: links %+v", c.Kind, c.Ref, res.Packages[0].Links)
		}
	}
	for _, c := range []struct {
		req  api.LinkRequest
		says string
	}{
		{api.LinkRequest{Plan: "sharing", Pkg: "W2", Kind: "pr", Ref: "nope"}, "owner/repo#123"},
		{api.LinkRequest{Plan: "sharing", Pkg: "W2", Kind: "branch", Ref: "no-colon"}, "owner/repo:branch"},
		{api.LinkRequest{Plan: "sharing", Pkg: "W2", Kind: "worktree", Ref: "relative/path"}, "absolute"},
		{api.LinkRequest{Plan: "sharing", Pkg: "W2", Kind: "cloud_session", Ref: "ftp://x"}, "URL"},
		{api.LinkRequest{Plan: "sharing", Pkg: "W2", Kind: "thing", Ref: "x"}, "unknown kind"},
		{api.LinkRequest{Plan: "sharing", Pkg: "W2", Kind: "pr"}, "ref is required"},
	} {
		refused(t, "manual "+c.req.Kind+" "+c.req.Ref, e.req("POST", "/api/links/manual", c.req), http.StatusBadRequest, c.says)
	}
}

// The mod's context: the repo of a cwd, its plans, and the package its
// worktree is linked to, with the convention block's data.
func TestContext(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	wt := filepath.Join(e.root, "pivot-v1")
	e.git(e.pivot, "worktree", "add", "-q", "-b", "share/v1-email-reports", wt)
	e.rescan()
	must(t, e.st.UpsertPRs(ctx, store.PR{Repo: "acme/pivot", Number: 214, HeadBranch: "share/v1-email-reports",
		State: store.PROpen, Title: "Weekly email reports (V1)"}))
	must(t, e.st.UpsertWorktrees(ctx, store.Worktree{Path: wt, MainRepo: e.pivot, Branch: "share/v1-email-reports", LastSeen: time.Now()}))
	e.registerSharing()
	ok[api.RegisterResponse](t, e.req("POST", "/api/plans", api.RegisterRequest{Path: e.aimPlan}), http.StatusCreated)

	sub := filepath.Join(wt, "Sources")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	c := ok[api.Context](t, e.req("GET", "/api/context?cwd="+sub, nil), http.StatusOK)
	if c.Repo == nil || c.Repo.Name != "acme/pivot" || c.Repo.Path != e.pivot {
		t.Fatalf("repo = %+v", c.Repo)
	}
	if c.Worktree == nil || c.Worktree.Path != wt || c.Worktree.Branch != "share/v1-email-reports" || c.Worktree.Main {
		t.Errorf("worktree = %+v", c.Worktree)
	}
	if len(c.Plans) != 1 || c.Plans[0].Slug != "sharing" || len(c.Plans[0].Packages) != 15 || c.Plans[0].Band == "" {
		t.Fatalf("plans = %+v", c.Plans)
	}
	if p := c.Plans[0].Packages[2]; p.ID != "A2" || p.Title != "Token revocation" || p.Repo != "pivot" ||
		p.SuggestedBranch != "sharing/a2-token-revocation" || p.PlanLine != "Plan: sharing · A2" {
		t.Errorf("plan package = %+v", p)
	}
	if c.Package == nil {
		t.Fatal("no package for a linked worktree")
	}
	if p := c.Package; p.Plan != "sharing" || p.ID != "V1" || p.Title != "Email reports" || p.SuggestedBranch != "sharing/v1-email-reports" ||
		p.PlanLine != "Plan: sharing · V1" || p.Lifecycle != "in_review" || p.Link.Method != "title" {
		t.Errorf("package = %+v", p)
	}

	// The main checkout: the repo and its plans, no package.
	c = ok[api.Context](t, e.req("GET", "/api/context?cwd="+e.pivot, nil), http.StatusOK)
	if c.Repo == nil || c.Worktree == nil || !c.Worktree.Main || len(c.Plans) != 1 || c.Package != nil {
		t.Errorf("main checkout context = %+v", c)
	}
	// pivot-web has the sharing plan; ai-manager has its own.
	if c = ok[api.Context](t, e.req("GET", "/api/context?cwd="+e.web, nil), http.StatusOK); len(c.Plans) != 1 || c.Plans[0].Slug != "sharing" {
		t.Errorf("web context plans = %+v", c.Plans)
	}
	if c = ok[api.Context](t, e.req("GET", "/api/context?cwd="+e.aim, nil), http.StatusOK); len(c.Plans) != 1 || c.Plans[0].Slug != "aim" {
		t.Errorf("ai-manager context plans = %+v", c.Plans)
	}
	// Outside every repo aimd may look at: an empty context.
	if c = ok[api.Context](t, e.req("GET", "/api/context?cwd="+filepath.Join(e.tmp, "elsewhere"), nil), http.StatusOK); c.Repo != nil || len(c.Plans) != 0 || c.Package != nil {
		t.Errorf("outside context = %+v", c)
	}
	refused(t, "relative cwd", e.req("GET", "/api/context?cwd=src", nil), http.StatusBadRequest, "absolute")
	refused(t, "no cwd", e.req("GET", "/api/context", nil), http.StatusBadRequest, "cwd")
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// A plan's Merges line is on its board and its summary. Without one
// the plan merges "you"; an unknown value reads as "you", with a warning.
func TestBoardMerges(t *testing.T) {
	e := newEnv(t)
	reg := e.registerSharing()
	if reg.Summary.Merges != "you" || e.board("sharing").Merges != "you" {
		t.Errorf("sharing merges: summary %q, board %q; want you (no Merges line)", reg.Summary.Merges, e.board("sharing").Merges)
	}

	table := "\n| Package | Repo | Needs |\n|---|---|---|\n| **L1 One** | pivot | — |\n| **L2 Two** | pivot | L1 |\n"
	lead := filepath.Join(e.pivot, "docs", "lead-plan.md")
	e.write(lead, "# Lead plan\n\nMerges: lead\n"+table)
	ok[api.RegisterResponse](t, e.req("POST", "/api/plans", api.RegisterRequest{Path: lead}), http.StatusCreated)
	b := e.board("lead")
	sum := ok[api.PlanSummary](t, e.req("GET", "/api/plans/lead/summary", nil), http.StatusOK)
	if b.Merges != "lead" || sum.Merges != "lead" || len(b.Warnings) != 0 {
		t.Errorf("lead plan: board merges %q, summary merges %q, warnings %+v; want lead, lead, none", b.Merges, sum.Merges, b.Warnings)
	}

	odd := filepath.Join(e.pivot, "docs", "odd-plan.md")
	e.write(odd, "# Odd plan\n\nMerges: whoever is around\n"+table)
	ok[api.RegisterResponse](t, e.req("POST", "/api/plans", api.RegisterRequest{Path: odd}), http.StatusCreated)
	b = e.board("odd")
	if b.Merges != "you" {
		t.Errorf("odd plan merges %q, want you", b.Merges)
	}
	if len(b.Warnings) != 1 || b.Warnings[0].Kind != "unknown-merges" || b.Warnings[0].Line != 3 {
		t.Errorf("odd plan warnings = %+v, want one unknown-merges on line 3", b.Warnings)
	}
}
