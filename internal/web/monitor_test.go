package web

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/monitor"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// The monitored-sessions world (internal/monitor), on top of the aim plan's
// board (registerAim):
//
//	pid:42 c6 agent         manually linked to aim · C6: an agent, on the board only
//	pid:43 aim lead         registers the plan again with its session id;
//	                        its subagent is spawn-linked to D1
//	pid:44 loose session    nothing: not monitored
//	pid:45 watched one      waits on you; watched through the API
//	pid:46 waiting stranger waits on you; not monitored
//	pid:47 no id yet        no session id: can't be watched
//	pid:48 headless run     headless, waits on you, watched
//
// PRs: #21 (D1, red CI) is linked to a package, #31 is on the watched
// session's branch, #30 (ready to merge) is nobody's.
const watchedRepo = "/w/ai"

func newMonitorEnv(t *testing.T, mode string) *boardEnv {
	t.Helper()
	e := newBoardEnv(t)
	repos := fakeRepos{watchedRepo: aimRepo}
	mon := &monitor.Source{
		Mode:  mode,
		Store: e.st,
		Plans: func(ctx context.Context) ([]api.Plan, error) {
			list, err := e.svc.Plans(ctx)
			return list.Plans, err
		},
		RepoNames: repos.RepoNames,
	}
	e.srv = New(Options{Port: testPort, Token: testToken, Store: e.st, Plans: e.svc, Monitor: mon, Repos: repos,
		HomeDir: e.tmp, Now: func() time.Time { return t0 }})
	e.h = e.srv.routes()

	e.registerAim()
	ctx := context.Background()
	must(t, e.st.UpsertSessions(ctx,
		store.Session{Key: "pid:45", SessionID: "watch-1", PID: 45, Kind: "interactive", Name: "watched one", Cwd: watchedRepo,
			Repo: watchedRepo, WorktreePath: watchedRepo, Branch: "feat/watched", State: "needs_you", StateReason: "permission prompt",
			NeedsYouSince: t0.Add(-20 * time.Minute), LastSeenAt: t0},
		store.Session{Key: "pid:46", SessionID: "stranger-1", PID: 46, Kind: "interactive", Name: "waiting stranger", Cwd: "/w/other",
			State: "needs_you", StateReason: "permission prompt", NeedsYouSince: t0.Add(-30 * time.Minute), LastSeenAt: t0},
		store.Session{Key: "pid:47", PID: 47, Kind: "interactive", Name: "no id yet", Cwd: "/w/x", State: "idle", LastSeenAt: t0},
		store.Session{Key: "pid:48", SessionID: "headless-1", PID: 48, Kind: "interactive", Headless: true, Name: "headless run",
			Cwd: "/w/h", State: "needs_you", NeedsYouSince: t0.Add(-time.Minute), LastSeenAt: t0},
	))
	must(t, e.st.UpsertPRs(ctx, store.PR{Repo: aimRepo, Number: 31, HeadBranch: "feat/watched", Title: "Watched work",
		State: store.PROpen, BaseRef: "main", MergeStateStatus: "CLEAN", URL: "https://github.com/" + aimRepo + "/pull/31",
		UpdatedAt: t0.Add(-2 * time.Hour), FirstSeenAt: t0.Add(-2 * time.Hour)}))
	// Rule 1: the lead registers the plan (again) with its session id.
	e.api("POST", "/api/plans", api.RegisterRequest{Path: e.aimPlan, Repos: []string{aimRepo}, SessionID: "lead-1"}, http.StatusOK)
	// Rule 2: /aim watch, as the mod sends it.
	e.watchAPI("POST", "watch-1", http.StatusOK)
	e.watchAPI("POST", "headless-1", http.StatusOK)
	return e
}

// watchAPI sends POST or DELETE /api/sessions/watch with the token.
func (e *boardEnv) watchAPI(method, sessionID string, status int) api.WatchResult {
	e.t.Helper()
	rec := e.api(method, "/api/sessions/watch", api.WatchRequest{SessionID: sessionID}, status)
	var res api.WatchResult
	if status == http.StatusOK {
		decode(e.t, rec, &res)
	}
	return res
}

func sessionsJSON(t *testing.T, e *boardEnv, target string) api.Sessions {
	t.Helper()
	rec := do(t, e.h, "GET", target, nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", target, rec.Code, rec.Body)
	}
	var out api.Sessions
	decode(t, rec, &out)
	return out
}

func TestAPISessionsMonitored(t *testing.T) {
	e := newMonitorEnv(t, monitor.ModeOptIn)
	got := sessionsJSON(t, e, "/api/sessions")
	if got.Monitor != "opt-in" || got.HiddenHeadless != 1 || len(got.Sessions) != 6 {
		t.Fatalf("monitor %q, hidden %d, %d sessions", got.Monitor, got.HiddenHeadless, len(got.Sessions))
	}
	why := map[string][]string{}
	for _, s := range got.Sessions {
		if s.MonitoredWhy == nil {
			t.Errorf("%s: monitored_why is null", s.Key)
		}
		if s.Monitored != (len(s.MonitoredWhy) > 0) {
			t.Errorf("%s: monitored %v with %d reasons", s.Key, s.Monitored, len(s.MonitoredWhy))
		}
		for _, w := range s.MonitoredWhy {
			why[s.Key] = append(why[s.Key], w.Reason+":"+w.Plan+":"+w.Pkg+":"+w.Text)
		}
	}
	want := map[string][]string{
		"pid:42": {"agent:aim:C6:agent of C6"},
		"pid:43": {"plan:aim::registered plan aim", "agent:aim:D1:runs the agent of D1"},
		"pid:45": {"watch:::you asked to watch it"},
	}
	for k, w := range want {
		if strings.Join(why[k], "|") != strings.Join(w, "|") {
			t.Errorf("%s: why = %q, want %q", k, why[k], w)
		}
	}
	if len(why) != len(want) || got.NotMonitored != 3 {
		t.Errorf("monitored %v, not_monitored %d; want only %d monitored and 3 not", why, got.NotMonitored, len(want))
	}
	// ?all=1 still means "with the headless runs"; the watched one is
	// monitored.
	all := sessionsJSON(t, e, "/api/sessions?all=1")
	if len(all.Sessions) != 7 || all.HiddenHeadless != 0 {
		t.Fatalf("?all=1: %d sessions, %d hidden", len(all.Sessions), all.HiddenHeadless)
	}
	for _, s := range all.Sessions {
		if s.Key == "pid:48" && (!s.Monitored || s.MonitoredWhy[0].Reason != "watch" || s.MonitoredWhy[0].Since.IsZero()) {
			t.Errorf("headless watched: %+v", s)
		}
	}
}

func TestWatchAPI(t *testing.T) {
	e := newMonitorEnv(t, monitor.ModeOptIn)
	body := `{"session_id": "stranger-1"}`
	// Write auth: no token, a wrong one, and no CSRF token are refused.
	for _, hdr := range []map[string]string{nil, {TokenHeader: "nope"}} {
		if rec := do(t, e.h, "POST", "/api/sessions/watch", strings.NewReader(body), hdr); rec.Code != http.StatusUnauthorized {
			t.Errorf("POST with %v = %d, want 401", hdr, rec.Code)
		}
		if rec := do(t, e.h, "DELETE", "/api/sessions/watch", strings.NewReader(body), hdr); rec.Code != http.StatusUnauthorized {
			t.Errorf("DELETE with %v = %d, want 401", hdr, rec.Code)
		}
	}
	if rec := do(t, e.h, "POST", "/api/sessions/watch", strings.NewReader(body), map[string]string{CSRFHeader: "forged"}); rec.Code != http.StatusForbidden {
		t.Errorf("a forged CSRF token = %d, want 403", rec.Code)
	}
	for _, bad := range []string{`{}`, `{"session_id": ""}`, `{"session_id": "a b"}`, `{"session_id": "../x"}`, `not json`} {
		rec := do(t, e.h, "POST", "/api/sessions/watch", strings.NewReader(bad), map[string]string{TokenHeader: testToken})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST %s = %d, want 400", bad, rec.Code)
		}
	}

	res := e.watchAPI("POST", "stranger-1", http.StatusOK)
	if !res.Watched || !res.Changed || !res.Known || res.Key != "pid:46" || res.Title != "waiting stranger" || !res.Monitored ||
		res.Monitor != "opt-in" || res.WatchedSince.IsZero() || len(res.MonitoredWhy) != 1 || res.MonitoredWhy[0].Reason != "watch" {
		t.Errorf("watch: %+v", res)
	}
	wantIn(t, "watch message", res.Message, "Watching this session")
	if again := e.watchAPI("POST", "stranger-1", http.StatusOK); again.Changed || !again.Watched || again.Message != "This session was already watched." {
		t.Errorf("watch again: %+v", again)
	}
	off := e.watchAPI("DELETE", "stranger-1", http.StatusOK)
	if off.Watched || !off.Changed || off.Monitored || len(off.MonitoredWhy) != 0 {
		t.Errorf("unwatch: %+v", off)
	}
	wantIn(t, "unwatch message", off.Message, "Stopped watching this session", "no longer monitored")
	if again := e.watchAPI("DELETE", "stranger-1", http.StatusOK); again.Changed || again.Message != "This session wasn't watched." {
		t.Errorf("unwatch again: %+v", again)
	}

	// The lead stays monitored through its plan, watched or not.
	lead := e.watchAPI("DELETE", "lead-1", http.StatusOK)
	if lead.Changed || !lead.Monitored || lead.Message != "This session wasn't watched; it is monitored anyway: registered plan aim · runs the agent of D1." {
		t.Errorf("unwatch the lead: %+v", lead)
	}
	e.watchAPI("POST", "lead-1", http.StatusOK)
	lead = e.watchAPI("DELETE", "lead-1", http.StatusOK)
	if !lead.Changed || lead.Message != "Stopped watching this session; it stays monitored: registered plan aim · runs the agent of D1." {
		t.Errorf("watch and unwatch the lead: %+v", lead)
	}

	// A session aimd hasn't listed yet can be watched; it says so.
	early := e.watchAPI("POST", "not-polled-yet", http.StatusOK)
	if early.Known || early.Key != "" || !early.Monitored || !early.Changed {
		t.Errorf("an unlisted session: %+v", early)
	}
	wantIn(t, "unlisted message", early.Message, "hasn't listed it yet")

	// A dashboard page may call it with its CSRF token instead.
	tok := e.srv.CSRF().Token()
	rec := do(t, e.h, "POST", "/api/sessions/watch", strings.NewReader(`{"session_id": "loose-1"}`), map[string]string{CSRFHeader: tok})
	if rec.Code != http.StatusOK {
		t.Fatalf("with a CSRF token: %d %s", rec.Code, rec.Body)
	}
}

// openTitles are the titles of the Needs you inbox's Open items.
func openTitles(t *testing.T, e *boardEnv) []string {
	t.Helper()
	var out []string
	for _, m := range ititleRE.FindAllStringSubmatch(get(t, e.h, "/app/inbox?tab=open&fragment=list"), -1) {
		out = append(out, html.UnescapeString(m[1]))
	}
	return out
}

var ititleRE = regexp.MustCompile(`<span class="ititle">([^<]*)</span>`)

// hasPrefixIn reports whether one of titles starts with prefix.
func hasPrefixIn(titles []string, prefix string) bool {
	return slices.ContainsFunc(titles, func(s string) bool { return strings.HasPrefix(s, prefix) })
}

// A watch survives /clear: seen only by the poller (a new session id
// under the same pid), and again through the mod's handover.
func TestWatchSurvivesClear(t *testing.T) {
	e := newMonitorEnv(t, monitor.ModeOptIn)
	ctx := context.Background()
	inInbox := func(what string, want bool) {
		t.Helper()
		if got := slices.Contains(openTitles(t, e), "watched one"); got != want {
			t.Errorf("%s: watched one in Needs you = %v, want %v", what, got, want)
		}
	}
	inInbox("before", true)
	row, err := e.st.GetSession(ctx, "pid:45")
	must(t, err)
	row.SessionID = "watch-2"
	must(t, e.st.UpsertSessions(ctx, row))
	inInbox("after a /clear the poller saw", true)
	if _, err := e.st.MoveSessionID(ctx, "watch-2", "watch-3"); err != nil {
		t.Fatal(err)
	}
	inInbox("after a /clear the mod reported", true)
	for _, s := range sessionsJSON(t, e, "/api/sessions").Sessions {
		if s.Key == "pid:45" && (s.SessionID != "watch-3" || !s.Monitored) {
			t.Errorf("after two /clears: %+v", s)
		}
	}
	// /aim unwatch sends the id the session has now.
	if res := e.watchAPI("DELETE", "watch-3", http.StatusOK); !res.Changed {
		t.Errorf("unwatch after /clear: %+v", res)
	}
	inInbox("after unwatch", false)
}

func TestNeedsYouOnlyMonitored(t *testing.T) {
	e := newMonitorEnv(t, monitor.ModeOptIn)
	// Your review is requested on C4's draft (#22, on the plan) and on
	// the stray #30 (nobody's). A review request needs you on any PR, so
	// both are in Needs you.
	_, err := e.st.SetReviewRequests(context.Background(), []string{"example-user"},
		[]store.PRKey{{Repo: aimRepo, Number: 22}, {Repo: aimRepo, Number: 30}}, t0.Add(-time.Hour))
	must(t, err)
	open := openTitles(t, e)
	// #21 (red CI) waits on D1's live agent, and #31 on the watched
	// session still working on its branch.
	for _, want := range []string{"watched one", "ai-manager #22 ·", "ai-manager #30 ·"} {
		if !hasPrefixIn(open, want) {
			t.Errorf("Needs you %q: missing %q", open, want)
		}
	}
	for _, not := range []string{"waiting stranger", "headless run", "ai-manager #21 ·", "ai-manager #31 ·"} {
		if hasPrefixIn(open, not) {
			t.Errorf("Needs you %q: has %q", open, not)
		}
	}

	counts := func() api.Counts {
		var st api.Status
		decode(t, do(t, e.h, "GET", "/api/status", nil, nil), &st)
		return st.Counts
	}
	if c := counts(); c.SessionsNeedingYou != 1 || c.PRsNeedingYou != 2 || c.NotMonitored != 3 || c.Sessions != 6 || c.OpenPRs != 5 {
		t.Errorf("counts = %+v", c)
	}

	// The watched session finishes its turn: its branch's PR is ready, on
	// no plan, so it needs you.
	row, err := e.st.GetSession(context.Background(), "pid:45")
	must(t, err)
	row.State, row.StateReason = "turn_done", ""
	must(t, e.st.UpsertSessions(context.Background(), row))
	if open := openTitles(t, e); !hasPrefixIn(open, "ai-manager #31 · Watched work") || !hasPrefixIn(open, "ai-manager #22 ·") {
		t.Errorf("Needs you after the turn %q: want #31 and #22", open)
	}
	if c := counts(); c.PRsNeedingYou != 3 {
		t.Errorf("counts after the turn = %+v", c)
	}
}

// GET /api/prs says what each PR waits on (internal/prwait), and the plan's Merges line
// decides who merges a ready one.
func TestAPIPRsWaitingOn(t *testing.T) {
	e := newMonitorEnv(t, monitor.ModeOptIn)
	ctx := context.Background()
	// C3 (merged in the fixture world) gets a second, ready PR; C4's
	// draft asks for your review.
	must(t, e.st.UpsertPRs(ctx, store.PR{Repo: aimRepo, Number: 40, HeadBranch: "aim/c3-more", Title: "More plan API (C3)",
		State: store.PROpen, BaseRef: "main", Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN", ChecksState: "SUCCESS",
		PlanSlug: "aim", PlanPkg: "C3", UpdatedAt: t0.Add(-time.Hour), FirstSeenAt: t0.Add(-time.Hour)}))
	_, err := e.st.SetReviewRequests(ctx, []string{"example-user"}, []store.PRKey{{Repo: aimRepo, Number: 22}}, t0.Add(-time.Hour))
	must(t, err)
	e.api("POST", "/api/plans", api.RegisterRequest{Path: e.aimPlan, Repos: []string{aimRepo}, SessionID: "lead-1"}, http.StatusOK)

	type wait struct {
		needsYou, waitingOn, merges, plan, pkg string
		monitored                              bool
	}
	prs := func() map[int]api.PR {
		t.Helper()
		var got api.PRs
		decode(t, do(t, e.h, "GET", "/api/prs?state=open", nil, nil), &got)
		out := map[int]api.PR{}
		for _, p := range got.PRs {
			out[p.Number] = p
		}
		return out
	}
	check := func(what string, got map[int]api.PR, want map[int]wait) {
		t.Helper()
		for n, w := range want {
			p := got[n]
			if g := (wait{p.NeedsYou, p.WaitingOn, p.Merges, p.Plan, p.Pkg, p.Monitored}); g != w {
				t.Errorf("%s: #%d = %+v (%q), want %+v", what, n, g, p.Reason, w)
			}
		}
	}
	got := prs()
	check("merges: you", got, map[int]wait{
		21: {"", "agent", "you", "aim", "D1", true},               // red CI, D1's agent still runs
		22: {"review_requested", "you", "you", "aim", "C4", true}, // a draft, but your review is requested
		30: {"", "none", "", "", "", false},                       // nobody's
		31: {"", "agent", "you", "", "", true},                    // the watched session still works on its branch
		40: {"ready_to_merge", "you", "you", "aim", "C3", true},
	})
	if r := got[40].Reason; r != "ready to merge · this plan merges: you" {
		t.Errorf("#40 reason = %q", r)
	}
	if r := got[30].Reason; r != "not on a registered plan or a monitored session's branch" {
		t.Errorf("#30 reason = %q", r)
	}
	if !got[22].ReviewRequested || !got[22].ReviewRequestedSince.Equal(t0.Add(-time.Hour)) {
		t.Errorf("#22 review requested %v since %v", got[22].ReviewRequested, got[22].ReviewRequestedSince)
	}

	// The plan now says its lead merges: the ready PR waits on the lead;
	// the review request still needs you.
	src, err := os.ReadFile(e.aimPlan)
	must(t, err)
	title, rest, _ := strings.Cut(string(src), "\n")
	e.write(e.aimPlan, title+"\n\nMerges: lead\n"+rest)
	e.api("POST", "/api/plans", api.RegisterRequest{Path: e.aimPlan, Repos: []string{aimRepo}, SessionID: "lead-1"}, http.StatusOK)
	var board api.Plan
	decode(t, e.api("GET", "/api/plans/aim", nil, http.StatusOK), &board)
	if board.Merges != "lead" {
		t.Fatalf("board merges = %q, want lead", board.Merges)
	}
	got = prs()
	check("merges: lead", got, map[int]wait{
		22: {"review_requested", "you", "lead", "aim", "C4", true},
		40: {"", "lead", "lead", "aim", "C3", true},
	})
	if r := got[40].Reason; r != "ready to merge · this plan's lead merges it" {
		t.Errorf("#40 reason = %q", r)
	}
	// Needs you keeps the review request and drops the ready PR; the PRs
	// page says the lead has it.
	open := openTitles(t, e)
	if !hasPrefixIn(open, "ai-manager #22 ·") || hasPrefixIn(open, "ai-manager #40 ·") {
		t.Errorf("Needs you with the lead merging %q: want #22 and not #40", open)
	}
	wantIn(t, "PRs page, lead merges", get(t, e.h, "/app/prs?fragment=list"),
		`<span class="pill tone-lead" title="ready to merge · this plan&#39;s lead merges it">Waiting on lead</span>`)
}

// sessions.monitor = "all": every session and PR.
func TestMonitorAll(t *testing.T) {
	e := newMonitorEnv(t, monitor.ModeAll)
	// Every PR counts; #30 is ready, on no plan, so it needs you. #21 and
	// #31 still have agents working on them.
	open := openTitles(t, e)
	for _, want := range []string{"watched one", "waiting stranger", "ai-manager #30 ·"} {
		if !hasPrefixIn(open, want) {
			t.Errorf("Needs you %q: missing %q", open, want)
		}
	}
	if hasPrefixIn(open, "ai-manager #21 ·") || hasPrefixIn(open, "ai-manager #31 ·") {
		t.Errorf("Needs you %q: has a PR an agent works on", open)
	}
	// No Not monitored group, and no Watch.
	list := get(t, e.h, "/app/sessions?fragment=list")
	wantNotIn(t, "sessions", list, "Not monitored", `action="/app/sessions/watch"`, `action="/app/sessions/unwatch"`)
	wantIn(t, "sessions", list, "loose session")

	got := sessionsJSON(t, e, "/api/sessions")
	if got.Monitor != "all" || got.NotMonitored != 0 {
		t.Errorf("monitor %q, not monitored %d", got.Monitor, got.NotMonitored)
	}
	for _, s := range got.Sessions {
		if !s.Monitored || len(s.MonitoredWhy) == 0 {
			t.Errorf("%s not monitored: %+v", s.Key, s.MonitoredWhy)
		}
		if s.Key == "pid:44" && (s.MonitoredWhy[0].Reason != "all" || s.MonitoredWhy[0].Text != `sessions.monitor is "all"`) {
			t.Errorf("loose: %+v", s.MonitoredWhy)
		}
	}
	res := e.watchAPI("POST", "stranger-1", http.StatusOK)
	wantIn(t, "watch in all mode", res.Message, `sessions.monitor is "all"`)
	var st api.Status
	decode(t, do(t, e.h, "GET", "/api/status", nil, nil), &st)
	if st.Counts.SessionsNeedingYou != 2 || st.Counts.PRsNeedingYou != 1 {
		t.Errorf("counts = %+v", st.Counts)
	}
}

// Without a monitor (tests, and an aimd built without one) every session
// is monitored and the API says so.
func TestNoMonitorWired(t *testing.T) {
	st := newTestStore(t)
	seedDash(t, st)
	h := newDash(t, st, nil).routes()
	rec := do(t, h, "GET", "/api/sessions", nil, nil)
	var got api.Sessions
	decode(t, rec, &got)
	if got.Monitor != "all" {
		t.Errorf("monitor = %q", got.Monitor)
	}
	raw := map[string]any{}
	must(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	first := raw["sessions"].([]any)[0].(map[string]any)
	if first["monitored"] != true || first["monitored_why"] == nil {
		t.Errorf("first session: %v", first)
	}
}

// A message that starts with a session's name capitalizes its first
// letter, whatever its script.
func TestWatchMessageSubject(t *testing.T) {
	res := api.WatchResult{Known: true}
	for subject, want := range map[string]string{
		"this session": "This session was already watched.",
		"équipe":       "Équipe was already watched.",
		"日本":           "日本 was already watched.",
		"x":            "X was already watched.",
	} {
		if got := watchMessage(res, nil, true, subject); got != want {
			t.Errorf("watchMessage(%q) = %q, want %q", subject, got, want)
		}
	}
}
