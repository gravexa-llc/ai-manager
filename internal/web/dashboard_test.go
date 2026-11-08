package web

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/store"
)

// The dashboard fixture lives under a fake home, so ~ shortening shows.
const (
	home    = "/Users/someone"
	appRepo = home + "/Projects/app"
	webRepo = home + "/Projects/web"
)

// fakeRepos is a RepoNamer.
type fakeRepos map[string]string

func (f fakeRepos) RepoNames() map[string]string { return f }

// fakeVerdicts is a SafeToRemove.
type fakeVerdicts struct {
	v   map[string]Verdict
	err error
}

func (f fakeVerdicts) Verdicts(context.Context) (map[string]Verdict, error) { return f.v, f.err }

// newDash returns a dashboard server over st with the fixture's repo names
// and home, changed by opt if given.
func newDash(t *testing.T, st Store, opt func(*Options)) *Server {
	t.Helper()
	o := Options{
		Port:    testPort,
		Token:   testToken,
		Store:   st,
		Repos:   fakeRepos{appRepo: "o/app", webRepo: "o/web"},
		HomeDir: home,
		Now:     func() time.Time { return t0 },
	}
	if opt != nil {
		opt(&o)
	}
	return New(o)
}

// seedDash fills a store with what the dashboard shows: sessions that need
// you in each way, worktrees in each state, and PRs.
func seedDash(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	must(t, st.UpsertSessions(ctx,
		store.Session{Key: "pid:10", Kind: "interactive", Name: "lead", Cwd: appRepo, Repo: appRepo, WorktreePath: appRepo, Branch: "main",
			State: "working", StateReason: "busy", StartedAt: t0.Add(-2 * time.Hour), LastSeenAt: t0},
		store.Session{Key: "pid:11", Kind: "interactive", Name: "routes agent", Cwd: appRepo + "-wt/src", Repo: appRepo, WorktreePath: appRepo + "-wt", Branch: "feat",
			State: "needs_you", StateReason: "permission prompt", NeedsYouSince: t0.Add(-10 * time.Minute), StartedAt: t0.Add(-time.Hour), LastSeenAt: t0},
		store.Session{Key: "job:j1", JobID: "j1", Kind: "background", Name: "sharing copy", Cwd: webRepo, Repo: webRepo,
			State: "needs_you", StateReason: "reply", NeedsYouSince: t0.Add(-3 * time.Hour), LastSeenAt: t0},
		store.Session{Key: "job:j2", JobID: "j2", Kind: "background", Name: "dismissed job", Cwd: webRepo, Repo: webRepo,
			State: "needs_you", StateReason: "reply", NeedsYouSince: t0.Add(-time.Hour), LastSeenAt: t0},
		store.Session{Key: "pid:12", Kind: "interactive", Headless: true, Name: "headless run", Cwd: "/private/tmp/x",
			State: "needs_you", StateReason: "permission prompt", NeedsYouSince: t0.Add(-5 * time.Hour), LastSeenAt: t0},
		store.Session{Key: "pid:13", Kind: "interactive", Name: "loose", Cwd: home + "/notes", State: "idle", StateReason: "idle", LastSeenAt: t0},
	))
	must(t, st.DismissNeedsYou(ctx, "job:j2", t0.Add(-30*time.Minute)))

	must(t, st.UpsertWorktrees(ctx,
		store.Worktree{Path: appRepo, MainRepo: appRepo, Branch: "main", HeadSHA: "a1", UpstreamState: "ok", LastCommitAt: t0.Add(-24 * time.Hour), LastSeen: t0},
		store.Worktree{Path: appRepo + "-wt", MainRepo: appRepo, Branch: "feat", HeadSHA: "b2", DirtyCount: 2, UntrackedCount: 1, UpstreamState: "ok", UnpushedCount: 3,
			Locked: true, LockedReason: "claude agent agent-a123 (pid 99)", LockAgentID: "a123", LockPID: 99, LastCommitAt: t0.Add(-time.Hour), LastSeen: t0},
		store.Worktree{Path: appRepo + "-old", MainRepo: appRepo, Branch: "old", HeadSHA: "c3", DirtyCount: -1, UntrackedCount: -1, LastSeen: t0},
		store.Worktree{Path: appRepo + "-det", MainRepo: appRepo, Detached: true, HeadSHA: "d4d4d4d4d4d4d4d4", IgnoredAtRisk: []string{".spike-state/"}, LastSeen: t0},
		store.Worktree{Path: appRepo + "-gone", MainRepo: appRepo, Branch: "gone", DirtyCount: -1, UntrackedCount: -1, PrunableReason: "gitdir file points to non-existent location", LastSeen: t0},
		store.Worktree{Path: webRepo, MainRepo: webRepo, Branch: "main", UpstreamState: "ok", LastSeen: t0},
	))

	must(t, st.UpsertPRs(ctx,
		store.PR{Repo: "o/app", Number: 7, Title: "Routes (V1)", URL: "https://github.com/o/app/pull/7", State: "OPEN", HeadBranch: "feat",
			MergeStateStatus: "CLEAN", ChecksState: "SUCCESS", PlanSlug: "sharing", PlanPkg: "V1", UpdatedAt: t0.Add(-2 * time.Hour)},
		store.PR{Repo: "o/app", Number: 5, Title: "Old work", URL: "https://github.com/o/app/pull/5", State: "MERGED", HeadBranch: "old", MergedAt: t0.Add(-48 * time.Hour)},
		store.PR{Repo: "o/app", Number: 8, Title: "Red build", URL: "https://github.com/o/app/pull/8", State: "OPEN", HeadBranch: "red",
			MergeStateStatus: "UNSTABLE", ChecksState: "FAILURE", UpdatedAt: t0.Add(-30 * time.Minute)},
		store.PR{Repo: "o/web", Number: 3, Title: "Parked draft", URL: "https://github.com/o/web/pull/3", State: "OPEN", Draft: true, HeadBranch: "parked",
			MergeStateStatus: "DRAFT", UpdatedAt: t0.Add(-40 * 24 * time.Hour)},
		store.PR{Repo: "o/web", Number: 4, Title: "Pending checks", URL: "https://github.com/o/web/pull/4", State: "OPEN", HeadBranch: "p",
			MergeStateStatus: "BLOCKED", ChecksState: "PENDING", UpdatedAt: t0.Add(-time.Hour)},
		store.PR{Repo: "o/app", Number: 9, Title: "Fork PR", URL: "https://github.com/o/app/pull/9", State: "MERGED", HeadBranch: "feat", IsCrossRepository: true},
	))
	// Red CI alone doesn't need you, but your review is requested on #8.
	_, err := st.SetReviewRequests(ctx, []string{"o"}, []store.PRKey{{Repo: "o/app", Number: 8}}, t0.Add(-30*time.Minute))
	must(t, err)
}

// get fetches a page and fails unless it is 200 HTML.
func get(t *testing.T, h http.Handler, target string) string {
	t.Helper()
	rec := do(t, h, "GET", target, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", target, rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("GET %s: Content-Type %q", target, ct)
	}
	return rec.Body.String()
}

// section returns the HTML of the element with the given id, up to the
// closing tag of its kind.
func section(t *testing.T, page, id string) string {
	t.Helper()
	i := strings.Index(page, `id="`+id+`"`)
	if i < 0 {
		t.Fatalf("no element with id %q in page", id)
	}
	start := strings.LastIndex(page[:i], "<")
	tag := page[start+1 : start+1+strings.IndexAny(page[start+1:], " >")]
	end := strings.Index(page[i:], "</"+tag+">")
	if end < 0 {
		t.Fatalf("element %q isn't closed", id)
	}
	return page[start : i+end]
}

func wantIn(t *testing.T, where, body string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(body, s) {
			t.Errorf("%s: missing %q", where, s)
		}
	}
}

func wantNotIn(t *testing.T, where, body string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if strings.Contains(body, s) {
			t.Errorf("%s: should not contain %q", where, s)
		}
	}
}

// Every answer carries the security headers: the app's pages and
// fragments, the static files, the API and the old pages' redirects.
func TestDashboardHeaders(t *testing.T) {
	st := newTestStore(t)
	seedDash(t, st)
	h := newDash(t, st, nil).Handler()
	for _, p := range []string{"/", "/sessions", "/plans/aim", "/app/", "/app/sessions", "/app/worktrees", "/app/?fragment=menu",
		"/static/app-shell.js", "/static/app-shell.css", "/static/icon.svg", "/api/status"} {
		rec := do(t, h, "GET", p, nil, nil)
		if rec.Code >= 400 {
			t.Errorf("GET %s = %d", p, rec.Code)
			continue
		}
		hd := rec.Header()
		if csp := hd.Get("Content-Security-Policy"); !strings.HasPrefix(csp, "default-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("GET %s: Content-Security-Policy = %q", p, csp)
		}
		for k, want := range map[string]string{"X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer", "X-Content-Type-Options": "nosniff"} {
			if got := hd.Get(k); got != want {
				t.Errorf("GET %s: %s = %q, want %q", p, k, got, want)
			}
		}
		if rec.Code != http.StatusOK || strings.HasPrefix(p, "/static/") {
			continue
		}
		if cc := hd.Get("Cache-Control"); cc != "no-store" {
			t.Errorf("GET %s: Cache-Control = %q, want no-store", p, cc)
		}
	}
	// A foreign Host gets nothing, not even a redirect.
	for _, p := range []string{"/", "/sessions", "/app/worktrees"} {
		if rec := do(t, h, "GET", p, nil, map[string]string{"Host": "evil.example:7777"}); rec.Code != http.StatusForbidden {
			t.Errorf("GET %s with a foreign Host = %d, want 403", p, rec.Code)
		}
	}
}

// Session names, branches, reasons, worktree paths and locks, PR titles,
// authors, URLs and plan refs are untrusted: no app page shows any of it
// as markup.
func TestDashboardEscapesUntrustedText(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	evil := `"><img src=x onerror=alert(1)>`
	script := `<script>alert(1)</script>`
	evilRepo := home + `/Projects/x"><svg onload=alert(2)>`
	must(t, st.UpsertSessions(ctx,
		store.Session{Key: "pid:1", SessionID: "s-1", Kind: "interactive", Name: evil, Cwd: evilRepo + "/" + script, Repo: evilRepo, Branch: `b"><svg onload=alert(3)>`,
			State: "needs_you", StateReason: script, NeedsYouSince: t0.Add(-time.Minute), LastSeenAt: t0},
		store.Session{Key: `pid:2"><script>alert(4)</script>`, Kind: script, Name: "odd key", Cwd: "/x", State: script, StateReason: evil, LastSeenAt: t0},
	))
	must(t, st.UpsertWorktrees(ctx,
		store.Worktree{Path: evilRepo, MainRepo: evilRepo, Branch: script, LastSeen: t0},
		store.Worktree{Path: evilRepo + "-wt", MainRepo: evilRepo, Branch: `feat"><svg onload=alert(3)>`, Locked: true, LockedReason: script, LockAgentID: evil,
			PrunableReason: script, IgnoredAtRisk: []string{script}, LastSeen: t0},
	))
	must(t, st.UpsertPRs(ctx,
		store.PR{Repo: `o/app"><script>alert(5)</script>`, Number: 1, Title: script, URL: "javascript:alert(1)", State: "OPEN", MergeStateStatus: "CLEAN",
			HeadBranch: `feat"><svg onload=alert(3)>`, Author: evil, PlanSlug: script, PlanPkg: evil, UpdatedAt: t0.Add(-time.Hour)},
		store.PR{Repo: "o/app", Number: 2, Title: evil, URL: `https://github.com/o/app/pull/2"><script>alert(6)</script>`, State: "OPEN", Draft: true,
			HeadBranch: "x", UpdatedAt: t0.Add(-time.Hour)},
	))
	h := newDash(t, st, func(o *Options) {
		o.Repos = fakeRepos{evilRepo: `o/app"><script>alert(5)</script>`}
		o.SafeToRemove = fakeVerdicts{v: map[string]Verdict{evilRepo + "-wt": {Category: script, Removable: true, Reasons: []string{evil}}}}
	}).Handler()

	pages := []string{"/app/", "/app/sessions", "/app/sessions?s=pid:1", "/app/worktrees", "/app/worktrees?fragment=list",
		"/app/prs", "/app/prs?pr=o/app%232", "/app/inbox", "/app/inbox?fragment=list", "/app/sessions?q=" + url.QueryEscape(script)}
	inbox := get(t, h, "/app/inbox?fragment=list")
	if m := regexp.MustCompile(`href="(/app/inbox\?item=\d+[^"]*)"`).FindStringSubmatch(inbox); m != nil {
		pages = append(pages, strings.ReplaceAll(m[1], "&amp;", "&"))
	}
	for _, p := range pages {
		body := get(t, h, p)
		wantNotIn(t, p, body, "<script>alert", "<img src=x", "<svg onload", `href="javascript:`, `"><script`, "onerror=alert(1)>")
		for _, tag := range scriptRE.FindAllString(body, -1) {
			if !pageScriptRE.MatchString(tag) {
				t.Errorf("%s has a script tag that isn't the shell's or the page's: %s", p, tag)
			}
		}
	}
	wantIn(t, "sessions", get(t, h, "/app/sessions?s=pid:1"), "&#34;&gt;&lt;img src=x onerror=alert(1)&gt;")
	// The quote can't end the attribute.
	wantIn(t, "PR detail", get(t, h, "/app/prs?pr=o/app%232"), `href="https://github.com/o/app/pull/2%22%3e%3cscript`)
}

// The API says a worktree whose git status couldn't be read is unknown,
// never clean.
func TestWorktreeUnknownStatusIsNotClean(t *testing.T) {
	st := newTestStore(t)
	must(t, st.UpsertWorktrees(context.Background(),
		store.Worktree{Path: "/r/a", MainRepo: "/r/a", Branch: "main", LastSeen: t0},
		store.Worktree{Path: "/r/a-x", MainRepo: "/r/a", Branch: "x", DirtyCount: -1, UntrackedCount: -1, LastSeen: t0},
	))
	rec := do(t, newDash(t, st, nil).Handler(), "GET", "/api/worktrees", nil, nil)
	wantIn(t, "api", rec.Body.String(), `"status_known":false,"dirty_count":null,"untracked_count":null`)
}

// Without an inbox store, the menu counts what the store says waits on
// you: monitored sessions (headless runs and dismissed alerts aside) and
// the PRs that need you.
func TestNeedsYouTotal(t *testing.T) {
	st := newTestStore(t)
	seedDash(t, st)
	srv := newDash(t, plainStore{st}, nil)
	sn, err := srv.load(context.Background())
	must(t, err)
	// pid:11's prompt and job:j1's reply; #8's review request and #7, ready
	// on no plan (no monitor: every PR is yours to merge).
	if n := needsYouTotal(sn); n != 4 {
		t.Errorf("needsYouTotal = %d, want 4", n)
	}
	wantIn(t, "menu", get(t, srv.Handler(), "/app/?fragment=menu"), `data-needs-count="4"`)
}

func TestFormatting(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0: "0s", 59 * time.Second: "59s", time.Minute: "1m", 59 * time.Minute: "59m", time.Hour: "1h",
		47 * time.Hour: "47h", 48 * time.Hour: "2d", 40 * 24 * time.Hour: "40d", -time.Hour: "0s",
	} {
		if got := ago(t0, t0.Add(-d)); got != want {
			t.Errorf("ago(%v) = %q, want %q", d, got, want)
		}
	}
	if ago(t0, time.Time{}) != "" {
		t.Error("ago(zero) isn't empty")
	}
	s := &Server{opts: Options{HomeDir: "/Users/someone/"}}
	for in, want := range map[string]string{
		"/Users/someone":            "~",
		"/Users/someone/Projects/x": "~/Projects/x",
		"/Users/someoneelse/x":      "/Users/someoneelse/x",
		"/private/tmp/x":            "/private/tmp/x",
		"":                          "",
	} {
		if got := s.shortPath(in); got != want {
			t.Errorf("shortPath(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"/Users/someone/Projects/app": "~/Projects/app",
		"/private/tmp/claude-501/-Users-someone-Projects-x/0123-4567/scratchpad/review-276": "/private/tmp/…/0123-4567/scratchpad/review-276",
		"/a/very-long-folder-name-that-goes-on-and-on-and-on-and-on-forever":                "/a/very-long-folder-name-that-goes-on-and-on-and-on-and-on-forever",
	} {
		if got := s.displayPath(in); got != want {
			t.Errorf("displayPath(%q) = %q, want %q", in, got, want)
		}
	}
}
