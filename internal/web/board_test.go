package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/config"
	"github.com/gravexa-llc/ai-manager/internal/github"
	"github.com/gravexa-llc/ai-manager/internal/monitor"
	"github.com/gravexa-llc/ai-manager/internal/planapi"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// boardEnv is the dashboard over a temp store and the real plan service,
// with both plan fixtures under a configured root (the app's plan, inbox,
// sessions and PR tests use it):
//
//   - root/pivot/docs/plans/sharing-plan.md, registered for
//     acme/pivot and acme/pivot-web, with the recorded
//     PRs (testdata/github);
//   - root/ai-manager/ROADMAP.md, registered for example-user/ai-manager, with
//     fake PRs, sessions and agent links.
type boardEnv struct {
	t                *testing.T
	tmp, root        string
	sharing, aimPlan string
	st               *store.Store
	svc              *planapi.Service
	srv              *Server
	h                http.Handler
}

func newBoardEnv(t *testing.T) *boardEnv {
	t.Helper()
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	e := &boardEnv{t: t, tmp: tmp, root: filepath.Join(tmp, "root")}
	e.sharing = e.copyFixture("sharing-plan.md", filepath.Join(e.root, "pivot", "docs", "plans", "sharing-plan.md"))
	e.aimPlan = e.copyFixture("ROADMAP.md", filepath.Join(e.root, "ai-manager", "ROADMAP.md"))
	e.st = newTestStore(t)
	cfg := config.Default()
	cfg.Roots = []string{e.root}
	e.svc = planapi.New(planapi.Options{
		Store:  e.st,
		Config: cfg,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return t0 },
	})
	t.Cleanup(e.svc.Wait)
	e.srv = New(Options{Port: testPort, Token: testToken, Store: e.st, Plans: e.svc, HomeDir: tmp, Now: func() time.Time { return t0 }})
	e.h = e.srv.routes()
	return e
}

// monitored makes the server decide which sessions and PRs are monitored
// as aimd does (internal/monitor), in the given mode, from the plan
// service's boards: so the Needs you inbox, and with it the plan summaries'
// need-you counts, put what waits on the plan it is on.
func (e *boardEnv) monitored(mode string) {
	e.t.Helper()
	mon := &monitor.Source{Mode: mode, Store: e.st, Plans: func(ctx context.Context) ([]api.Plan, error) {
		list, err := e.svc.Plans(ctx)
		return list.Plans, err
	}}
	e.srv = New(Options{Port: testPort, Token: testToken, Store: e.st, Plans: e.svc, Monitor: mon, HomeDir: e.tmp, Now: func() time.Time { return t0 }})
	e.h = e.srv.routes()
}

func (e *boardEnv) write(path, content string) string {
	e.t.Helper()
	must(e.t, os.MkdirAll(filepath.Dir(path), 0o755))
	must(e.t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

func (e *boardEnv) copyFixture(name, to string) string {
	e.t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "..", "testdata", "plans", name))
	must(e.t, err)
	return e.write(to, string(src))
}

// api sends a plan API request with the token and fails unless it gets
// the status.
func (e *boardEnv) api(method, target string, body any, status int) *httptest.ResponseRecorder {
	e.t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		must(e.t, err)
		r = strings.NewReader(string(b))
	}
	rec := do(e.t, e.h, method, target, r, map[string]string{TokenHeader: testToken})
	if rec.Code != status {
		e.t.Fatalf("%s %s = %d, want %d: %s", method, target, rec.Code, status, rec.Body)
	}
	return rec
}

// post submits a dashboard form the way a browser does (Origin: null,
// same-origin), with a fresh CSRF token unless vals has one.
func (e *boardEnv) post(target string, vals url.Values) *httptest.ResponseRecorder {
	e.t.Helper()
	if vals == nil {
		vals = url.Values{}
	}
	if _, ok := vals[CSRFField]; !ok {
		vals.Set(CSRFField, e.srv.CSRF().Token())
	}
	return do(e.t, e.h, "POST", target, strings.NewReader(vals.Encode()), map[string]string{
		"Content-Type": "application/x-www-form-urlencoded", "Origin": "null", "Sec-Fetch-Site": "same-origin",
	})
}

// follow checks that a form answered 303 to a dashboard path starting with
// prefix, and returns the page it points at.
func (e *boardEnv) follow(rec *httptest.ResponseRecorder, prefix string) string {
	e.t.Helper()
	loc := rec.Header().Get("Location")
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(loc, prefix) {
		e.t.Fatalf("answer %d to %q, want 303 to %s…: %s", rec.Code, loc, prefix, rec.Body)
	}
	loc, _, _ = strings.Cut(loc, "#")
	return get(e.t, e.h, loc)
}

// registerSharing registers the sharing plan with the recorded PRs.
func (e *boardEnv) registerSharing() {
	e.t.Helper()
	must(e.t, e.st.UpsertPRs(context.Background(), recordedPRs(e.t,
		"branch_prs_pivot.json", "branch_prs_pivot-web.json", "open_prs_mine_first_read.json")...))
	e.api("POST", "/api/plans", api.RegisterRequest{Path: e.sharing, Repos: []string{"acme/pivot", "acme/pivot-web"}}, http.StatusCreated)
}

const aimRepo = "example-user/ai-manager"

// aimSteps counts the aim plan's packages that are your step and ready:
// R1 and B4, ready with a "you:" step. Each is a Your step item in Needs
// you (yourstep.go), so it counts wherever the inbox's Open items do.
const aimSteps = 2

// registerAim registers the roadmap fixture with fake ai-manager PRs: the
// merged packages by their Plan: line, D1 open with red CI and a spawned
// agent, C4 a draft, D3 matched only by a branch token (low confidence),
// C6 worked on by a live session linked by hand, and one PR no package
// claims. Sessions: the C6 session, D1's lead, and one on no plan.
func (e *boardEnv) registerAim() {
	e.t.Helper()
	ctx := context.Background()
	pr := func(n int, branch, title, state, pkg string) store.PR {
		p := store.PR{Repo: aimRepo, Number: n, HeadBranch: branch, Title: title, State: state, BaseRef: "main",
			URL: "https://github.com/" + aimRepo + "/pull/" + strconv.Itoa(n), UpdatedAt: t0.Add(-time.Hour), FirstSeenAt: t0.Add(-48 * time.Hour)}
		if pkg != "" {
			p.PlanSlug, p.PlanPkg = "aim", pkg
		}
		if state == store.PRMerged {
			p.MergedAt = t0.Add(-24 * time.Hour)
		}
		return p
	}
	var prs []store.PR
	for i, pkg := range []string{"A1", "A2", "A3", "A4", "B1", "B2", "B3", "C1", "C2", "C3", "C5"} {
		prs = append(prs, pr(7+i, "aim/"+strings.ToLower(pkg)+"-x", "Work ("+pkg+")", store.PRMerged, pkg))
	}
	d1 := pr(21, "aim/d1-events", "Event receiver and live state (D1)", store.PROpen, "")
	d1.ChecksState, d1.MergeStateStatus = "FAILURE", "UNSTABLE"
	c4 := pr(22, "aim/c4-mod", "Mod v1 (C4)", store.PROpen, "C4")
	c4.Draft, c4.MergeStateStatus = true, "DRAFT"
	d3 := pr(23, "feature/d3-graph", "Graph work", store.PROpen, "")
	d3.FirstSeenAt = time.Now().Add(time.Hour) // after the plan is registered
	stray := pr(30, "docs/readme", "Tidy the README", store.PROpen, "")
	stray.MergeStateStatus = "CLEAN"
	prs = append(prs, d1, c4, d3, stray)
	must(e.t, e.st.UpsertPRs(ctx, prs...))

	must(e.t, e.st.UpsertSessions(ctx,
		store.Session{Key: "pid:42", SessionID: "sess-c6", Kind: "interactive", Name: "c6 agent", Cwd: "/w/c6", State: "working", StartedAt: t0.Add(-time.Hour), LastSeenAt: t0},
		store.Session{Key: "pid:43", SessionID: "lead-1", Kind: "interactive", Name: "aim lead", Cwd: "/w/aim", State: "working", StartedAt: t0.Add(-2 * time.Hour), LastSeenAt: t0},
		store.Session{Key: "pid:44", SessionID: "loose-1", Kind: "interactive", Name: "loose session", Cwd: "/w/notes", State: "idle", StartedAt: t0.Add(-time.Hour), LastSeenAt: t0},
	))
	must(e.t, e.st.UpdateSessionLive(ctx, "pid:42", store.SessionLive{State: "working", LastAction: "Bash: make check", LastEventAt: t0.Add(-12 * time.Minute)}))
	must(e.t, e.st.UpsertAgents(ctx, store.Agent{SessionID: "lead-1", AgentID: "agent-d1", Kind: store.AgentSubagent, State: "running", LastAction: "Edit: live.go", StartedAt: t0.Add(-time.Hour)}))

	e.api("POST", "/api/plans", api.RegisterRequest{Path: e.aimPlan, Repos: []string{aimRepo}}, http.StatusCreated)
	e.api("POST", "/api/links/manual", api.LinkRequest{Plan: "aim", Pkg: "C6", Kind: "agent", Ref: "sess-c6"}, http.StatusCreated)
	e.api("POST", "/api/links/spawn", api.SpawnLinkRequest{SessionID: "lead-1", AgentID: "agent-d1", Plan: "aim", Pkg: "D1"}, http.StatusCreated)
}

// recordedPRs reads the recorded GraphQL answers into store rows, as the
// GitHub poller stores them (internal/state's tests do the same). A PR is
// first seen at its createdAt.
func recordedPRs(t *testing.T, files ...string) []store.PR {
	t.Helper()
	seen := map[string]bool{}
	var out []store.PR
	var walk func(v any, repo string)
	walk = func(v any, repo string) {
		switch v := v.(type) {
		case []any:
			for _, x := range v {
				walk(x, repo)
			}
		case map[string]any:
			if r, ok := v["nameWithOwner"].(string); ok {
				repo = r
			}
			if _, ok := v["number"]; ok {
				if _, ok := v["headRefName"]; ok {
					p := recordedPR(t, v, repo)
					if k := store.PRRef(p.Repo, p.Number); !seen[k] {
						seen[k] = true
						out = append(out, p)
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
		b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "github", f))
		must(t, err)
		var v any
		must(t, json.Unmarshal(b, &v))
		walk(v, "")
	}
	return out
}

func recordedPR(t *testing.T, n map[string]any, repo string) store.PR {
	t.Helper()
	str := func(k string) string { s, _ := n[k].(string); return s }
	tm := func(k string) time.Time {
		if str(k) == "" {
			return time.Time{}
		}
		v, err := time.Parse(time.RFC3339, str(k))
		must(t, err)
		return v
	}
	if r, ok := n["repository"].(map[string]any); ok {
		repo, _ = r["nameWithOwner"].(string)
	}
	p := store.PR{
		Repo: repo, Number: int(n["number"].(float64)), HeadBranch: str("headRefName"), HeadRefOID: str("headRefOid"),
		BaseRef: str("baseRefName"), Title: str("title"), State: str("state"), Mergeable: str("mergeable"),
		MergeStateStatus: str("mergeStateStatus"), ReviewDecision: str("reviewDecision"), URL: str("url"),
		UpdatedAt: tm("updatedAt"), MergedAt: tm("mergedAt"), ClosedAt: tm("closedAt"), FirstSeenAt: tm("createdAt"),
	}
	p.Draft, _ = n["isDraft"].(bool)
	p.IsCrossRepository, _ = n["isCrossRepository"].(bool)
	if r, ok := n["statusCheckRollup"].(map[string]any); ok {
		p.ChecksState, _ = r["state"].(string)
	}
	if ref, ok := github.ParsePlanLine(str("body")); ok {
		p.PlanSlug, p.PlanPkg = ref.Slug, ref.ID
	}
	return p
}

// element returns the HTML of the element with the given id, through its
// matching closing tag (elements of its kind may nest inside).
func element(t *testing.T, page, id string) string {
	t.Helper()
	i := strings.Index(page, `id="`+id+`"`)
	if i < 0 {
		t.Fatalf("no element with id %q in page", id)
	}
	start := strings.LastIndex(page[:i], "<")
	tag := page[start+1 : start+1+strings.IndexAny(page[start+1:], " >")]
	open, closing := "<"+tag, "</"+tag+">"
	depth, pos := 0, start
	for {
		o := strings.Index(page[pos:], open)
		c := strings.Index(page[pos:], closing)
		if c < 0 {
			t.Fatalf("element %q isn't closed", id)
		}
		if o >= 0 && o < c {
			depth++
			pos += o + len(open)
			continue
		}
		depth--
		pos += c + len(closing)
		if depth == 0 {
			return page[start:pos]
		}
	}
}
