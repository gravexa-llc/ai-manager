package web

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// seed fills a store with a few rows of each kind.
func seed(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	must(t, st.UpsertSessions(ctx,
		store.Session{Key: "pid:1", SessionID: "s1", Kind: "interactive", Cwd: "/r/a", Repo: "/r/a", State: "working", StartedAt: t0.Add(-time.Hour), LastSeenAt: t0},
		store.Session{Key: "pid:2", SessionID: "s2", Kind: "interactive", Cwd: "/r/b", State: "needs_you", StateReason: "permission prompt",
			NeedsYouSince: t0.Add(-5 * time.Minute), LastSeenAt: t0},
		store.Session{Key: "job:abc", JobID: "abc", Kind: "background", Cwd: "/r/c", State: "needs_you", StateReason: "reply",
			NeedsYouSince: t0.Add(-48 * time.Hour), LastSeenAt: t0},
		store.Session{Key: "pid:3", Kind: "interactive", Headless: true, Cwd: "/tmp/x", State: "working", LastSeenAt: t0},
	))
	must(t, st.DismissNeedsYou(ctx, "job:abc", t0.Add(-time.Hour)))
	must(t, st.UpsertSessions(ctx, store.Session{Key: "pid:9", Kind: "interactive", Cwd: "/gone", State: "idle", LastSeenAt: t0}))
	must(t, st.EndSession(ctx, "pid:9", t0))

	must(t, st.UpsertWorktrees(ctx,
		store.Worktree{Path: "/r/a", MainRepo: "/r/a", Branch: "main", HeadSHA: "aaa", DirtyCount: 2, UntrackedCount: 1, UpstreamState: "ok", LastSeen: t0},
		store.Worktree{Path: "/r/a-wt", MainRepo: "/r/a", Branch: "feature", DirtyCount: -1, UntrackedCount: -1, PrunableReason: "gitdir file points to non-existent location", LastSeen: t0},
		store.Worktree{Path: "/r/a-old", MainRepo: "/r/a", Branch: "old", LastSeen: t0},
	))
	must(t, st.MarkWorktreeRemoved(ctx, "/r/a-old", t0))

	must(t, st.UpsertPRs(ctx,
		store.PR{Repo: "o/a", Number: 1, Title: "green", State: "OPEN", MergeStateStatus: "CLEAN", ChecksState: "SUCCESS", HeadBranch: "feature", PlanSlug: "aim", PlanPkg: "B2"},
		store.PR{Repo: "o/a", Number: 2, Title: "red", State: "OPEN", MergeStateStatus: "UNSTABLE", ChecksState: "FAILURE", HeadBranch: "red"},
		store.PR{Repo: "o/a", Number: 3, Title: "draft", State: "OPEN", Draft: true, MergeStateStatus: "DRAFT", HeadBranch: "d"},
		store.PR{Repo: "o/b", Number: 4, Title: "merged", State: "MERGED", MergedAt: t0, HeadBranch: "m"},
		store.PR{Repo: "o/b", Number: 5, Title: "pending", State: "OPEN", MergeStateStatus: "BLOCKED", ChecksState: "PENDING", HeadBranch: "p"},
	))
}

func TestSessionsEndpoint(t *testing.T) {
	st := newTestStore(t)
	seed(t, st)
	h := newTestServer(t, st, nil).Handler()

	var got api.Sessions
	rec := do(t, h, "GET", "/api/sessions", nil, nil)
	if rec.Code != 200 {
		t.Fatalf("GET /api/sessions = %d: %s", rec.Code, rec.Body)
	}
	decode(t, rec, &got)
	if got.HiddenHeadless != 1 || len(got.Sessions) != 3 {
		t.Fatalf("got %d sessions, %d hidden; want 3 and 1: %+v", len(got.Sessions), got.HiddenHeadless, got)
	}
	byKey := map[string]api.Session{}
	for _, s := range got.Sessions {
		byKey[s.Key] = s
	}
	if _, ok := byKey["pid:9"]; ok {
		t.Error("an ended session is listed")
	}
	if s := byKey["pid:2"]; !s.NeedsYou || s.NeedsYouAgeSeconds != 300 || s.StateReason != "permission prompt" || s.NeedsYouDismissed {
		t.Errorf("pid:2 = %+v, want needs you for 300s, not dismissed", s)
	}
	if s := byKey["job:abc"]; !s.NeedsYou || !s.NeedsYouDismissed || s.NeedsYouAgeSeconds != 48*3600 {
		t.Errorf("job:abc = %+v, want needs you for 48h, dismissed", s)
	}
	if s := byKey["pid:1"]; s.NeedsYou || s.NeedsYouAgeSeconds != 0 || !s.NeedsYouSince.IsZero() || s.Repo != "/r/a" {
		t.Errorf("pid:1 = %+v, want working, no needs-you fields", s)
	}

	rec = do(t, h, "GET", "/api/sessions?all=1", nil, nil)
	got = api.Sessions{}
	decode(t, rec, &got)
	if len(got.Sessions) != 4 || got.HiddenHeadless != 0 {
		t.Errorf("?all=1: %d sessions, %d hidden; want 4 and 0", len(got.Sessions), got.HiddenHeadless)
	}

	// An empty store answers an empty list, not null.
	rec = do(t, newTestServer(t, newTestStore(t), nil).Handler(), "GET", "/api/sessions", nil, nil)
	if !strings.Contains(rec.Body.String(), `"sessions":[]`) {
		t.Errorf("empty store body = %s, want an empty list", rec.Body)
	}
}

func TestWorktreesEndpoint(t *testing.T) {
	st := newTestStore(t)
	seed(t, st)
	h := newTestServer(t, st, nil).Handler()

	var got api.Worktrees
	decode(t, do(t, h, "GET", "/api/worktrees", nil, nil), &got)
	if len(got.Worktrees) != 2 {
		t.Fatalf("got %d worktrees, want 2 live: %+v", len(got.Worktrees), got)
	}
	a, wt := got.Worktrees[0], got.Worktrees[1]
	if !a.StatusKnown || a.DirtyCount == nil || *a.DirtyCount != 2 || *a.UntrackedCount != 1 {
		t.Errorf("main = %+v, want known counts 2 and 1", a)
	}
	if wt.StatusKnown || wt.DirtyCount != nil || wt.UntrackedCount != nil || wt.PrunableReason == "" {
		t.Errorf("prunable = %+v, want unknown (null) counts", wt)
	}
	rec := do(t, h, "GET", "/api/worktrees", nil, nil)
	if !strings.Contains(rec.Body.String(), `"dirty_count":null`) {
		t.Errorf("unknown counts aren't null in %s", rec.Body)
	}

	got = api.Worktrees{}
	decode(t, do(t, h, "GET", "/api/worktrees?all=1", nil, nil), &got)
	if len(got.Worktrees) != 3 {
		t.Errorf("?all=1: %d worktrees, want 3", len(got.Worktrees))
	}
	got = api.Worktrees{}
	decode(t, do(t, h, "GET", "/api/worktrees?repo=/elsewhere", nil, nil), &got)
	if len(got.Worktrees) != 0 {
		t.Errorf("?repo=/elsewhere: %d worktrees, want 0", len(got.Worktrees))
	}
}

func TestPRsEndpoint(t *testing.T) {
	st := newTestStore(t)
	seed(t, st)
	h := newTestServer(t, st, nil).Handler()

	// Your review is requested on #5.
	if _, err := st.SetReviewRequests(context.Background(), []string{"o"}, []store.PRKey{{Repo: "o/b", Number: 5}}, t0.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	var got api.PRs
	decode(t, do(t, h, "GET", "/api/prs", nil, nil), &got)
	if len(got.PRs) != 5 {
		t.Fatalf("got %d PRs, want 5", len(got.PRs))
	}
	// No monitor is wired: every PR counts, on no plan, so you merge.
	type wait struct{ needsYou, waitingOn, merges, reason string }
	want := map[int]wait{
		1: {"ready_to_merge", "you", "you", "ready to merge · on no plan, so you merge it"},
		2: {"", "none", "you", "CI failing · no agent is working on it"},
		3: {"", "none", "you", "draft · no agent is working on it"},
		4: {"", "none", "you", "merged"},
		5: {"review_requested", "you", "you", "your review is requested"},
	}
	for _, p := range got.PRs {
		if w := (wait{p.NeedsYou, p.WaitingOn, p.Merges, p.Reason}); w != want[p.Number] {
			t.Errorf("#%d waits: %+v, want %+v", p.Number, w, want[p.Number])
		}
		if !p.Monitored || p.Plan != "" {
			t.Errorf("#%d: monitored %v, plan %q", p.Number, p.Monitored, p.Plan)
		}
		if p.ReviewRequested != (p.Number == 5) || p.ReviewRequestedSince.IsZero() == (p.Number == 5) {
			t.Errorf("#%d: review_requested %v since %v", p.Number, p.ReviewRequested, p.ReviewRequestedSince)
		}
	}
	// The fields are always there, empty or not.
	var raw struct {
		PRs []map[string]any `json:"prs"`
	}
	decode(t, do(t, h, "GET", "/api/prs?state=open", nil, nil), &raw)
	for _, p := range raw.PRs {
		for _, k := range []string{"needs_you", "waiting_on", "reason", "monitored"} {
			if _, ok := p[k]; !ok {
				t.Errorf("#%v has no %q: %v", p["number"], k, p)
			}
		}
		if _, ok := p["needs_you_reason"]; ok {
			t.Errorf("#%v still has needs_you_reason", p["number"])
		}
	}
	if got.PRs[0].PlanSlug != "aim" || got.PRs[0].PlanPkg != "B2" {
		t.Errorf("#1 plan = %q %q", got.PRs[0].PlanSlug, got.PRs[0].PlanPkg)
	}

	for q, n := range map[string]int{"state=open": 4, "state=MERGED": 1, "state=closed": 0, "state=all": 5, "repo=o/b": 2, "repo=o/b&state=open": 1} {
		got = api.PRs{}
		decode(t, do(t, h, "GET", "/api/prs?"+q, nil, nil), &got)
		if len(got.PRs) != n {
			t.Errorf("?%s: %d PRs, want %d", q, len(got.PRs), n)
		}
	}
	if rec := do(t, h, "GET", "/api/prs?state=weird", nil, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("?state=weird = %d, want 400", rec.Code)
	}
}

func TestStatusEndpoint(t *testing.T) {
	st := newTestStore(t)
	seed(t, st)
	ds := api.Status{
		Version: "test",
		Now:     t0,
		Sources: []api.Source{
			{Name: api.SourceSessions, Runs: 3, LastOK: t0},
			{Name: api.SourceGitHub, Runs: 2, FailingSince: t0.Add(-time.Minute), Error: "github: unavailable", Stale: true},
		},
		GitHub: api.GitHubStatus{Budget: api.Budget{Verdict: "normal", Known: true, Remaining: 4900, Limit: 5000}},
	}
	h := newTestServer(t, st, fakeStatus{ds}).Handler()
	var got api.Status
	rec := do(t, h, "GET", "/api/status", nil, nil)
	if rec.Code != 200 {
		t.Fatalf("GET /api/status = %d: %s", rec.Code, rec.Body)
	}
	decode(t, rec, &got)
	want := api.Counts{
		Sessions:            3,
		SessionsNeedingYou:  1, // job:abc is dismissed
		OldestNeedsYouSince: t0.Add(-5 * time.Minute),
		HeadlessSessions:    1,
		Worktrees:           2,
		OpenPRs:             4,
		PRsNeedingYou:       1, // #1 is ready to merge; red CI (#2) doesn't need you
		StaleSources:        []string{api.SourceGitHub},
	}
	if fmt.Sprint(got.Counts) != fmt.Sprint(want) {
		t.Errorf("counts =\n%+v\nwant\n%+v", got.Counts, want)
	}
	if got.Version != "test" || got.GitHub.Budget.Remaining != 4900 || len(got.Sources) != 2 || got.Sources[1].FailingSince.IsZero() {
		t.Errorf("status = %+v", got)
	}
	if got.Sources[0].FailingSince != (time.Time{}) || strings.Contains(rec.Body.String(), `"failing_since":"0001`) {
		t.Errorf("a healthy source shows a failing_since: %s", rec.Body)
	}
}

func TestStoreErrorsAre500(t *testing.T) {
	st := newTestStore(t)
	h := newTestServer(t, st, nil).Handler()
	st.Close()
	for _, p := range []string{"/api/sessions", "/api/worktrees", "/api/prs", "/api/status"} {
		rec := do(t, h, "GET", p, nil, nil)
		if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "sql") {
			t.Errorf("GET %s on a closed store = %d %s, want a 500 without details", p, rec.Code, rec.Body)
		}
	}
}

// startServer runs the handler on a real loopback listener with the Host
// check set to its port, and returns its base URL and the hub.
func startServer(t *testing.T, onWatchers func(int), keepalive time.Duration) (string, *Hub, *Server) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	hub := NewHub(onWatchers)
	if keepalive > 0 {
		hub.keepalive = keepalive
	}
	s := New(Options{Port: port, Token: testToken, Store: newTestStore(t), Hub: hub})
	srv := httptest.NewUnstartedServer(s.Handler())
	srv.Listener.Close()
	srv.Listener = ln
	srv.Config.RegisterOnShutdown(hub.Close)
	srv.Start()
	t.Cleanup(func() {
		hub.Close()
		srv.Close()
	})
	return srv.URL, hub, s
}

// sseEvent is one parsed event.
type sseEvent struct{ name, data string }

// readEvents parses a text/event-stream into a channel, skipping comments.
func readEvents(t *testing.T, resp *http.Response) <-chan sseEvent {
	ch := make(chan sseEvent, 16)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(resp.Body)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if ev.name != "" || ev.data != "" {
					ch <- ev
				}
				ev = sseEvent{}
			case strings.HasPrefix(line, "event: "):
				ev.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				ev.data = strings.TrimPrefix(line, "data: ")
			}
		}
	}()
	return ch
}

func next(t *testing.T, ch <-chan sseEvent) sseEvent {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("stream ended")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event within 5s")
	}
	return sseEvent{}
}

func TestStreamChangeEvents(t *testing.T) {
	var mu sync.Mutex
	var counts []int
	watchers := make(chan int, 16)
	base, hub, _ := startServer(t, func(n int) {
		mu.Lock()
		counts = append(counts, n)
		mu.Unlock()
		watchers <- n
	}, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/api/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("GET /api/stream = %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	events := readEvents(t, resp)
	if ev := next(t, events); ev.name != "ready" {
		t.Fatalf("first event = %+v, want ready", ev)
	}
	if n := <-watchers; n != 1 || hub.Watchers() != 1 {
		t.Fatalf("watchers after connect = %d (hub %d), want 1", n, hub.Watchers())
	}

	hub.Publish(api.TopicSessions)
	ev := next(t, events)
	var c api.Change
	if err := json.Unmarshal([]byte(ev.data), &c); err != nil || ev.name != "change" || c.Topic != api.TopicSessions || c.Seq != 1 {
		t.Fatalf("event = %+v (%v), want change sessions seq 1", ev, err)
	}
	hub.Publish(api.TopicPRs)
	hub.Publish(api.TopicWorktrees)
	got := map[string]bool{}
	for range 2 {
		ev := next(t, events)
		if err := json.Unmarshal([]byte(ev.data), &c); err != nil {
			t.Fatal(err)
		}
		got[c.Topic] = true
	}
	if !got[api.TopicPRs] || !got[api.TopicWorktrees] {
		t.Errorf("got topics %v, want prs and worktrees", got)
	}

	// A second client counts too; closing both brings it back to 0.
	req2, _ := http.NewRequestWithContext(ctx, "GET", base+"/api/stream", nil)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	if n := <-watchers; n != 2 {
		t.Errorf("watchers after second connect = %d, want 2", n)
	}
	resp2.Body.Close()
	if n := <-watchers; n != 1 {
		t.Errorf("watchers after second close = %d, want 1", n)
	}
	cancel()
	if n := <-watchers; n != 0 {
		t.Errorf("watchers after close = %d, want 0", n)
	}
}

func TestStreamCoalescesAndCloses(t *testing.T) {
	base, hub, _ := startServer(t, nil, 0)
	resp, err := http.Get(base + "/api/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	events := readEvents(t, resp)
	next(t, events) // ready
	if !waitWatchers(hub, 1) {
		t.Fatal("the stream isn't counted")
	}

	// A burst on one topic arrives as at most a few events, the last with
	// the latest seq.
	for range 50 {
		hub.Publish(api.TopicSessions)
	}
	var last api.Change
	for last.Seq != 50 {
		ev := next(t, events)
		if err := json.Unmarshal([]byte(ev.data), &last); err != nil {
			t.Fatal(err)
		}
	}

	hub.Close()
	select {
	case _, ok := <-events:
		for ok {
			_, ok = <-events
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stream didn't end after Close")
	}
	// New streams are refused once closed.
	resp2, err := http.Get(base + "/api/stream")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("stream after Close = %d, want 503", resp2.StatusCode)
	}
}

func TestStreamKeepalive(t *testing.T) {
	base, _, _ := startServer(t, nil, 20*time.Millisecond)
	resp, err := http.Get(base + "/api/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(line, ": keepalive") {
			return
		}
	}
	t.Fatal("no keepalive")
}

func TestStreamRefusesWrongHost(t *testing.T) {
	base, _, _ := startServer(t, nil, 0)
	req, _ := http.NewRequest("GET", base+"/api/stream", nil)
	req.Host = "evil.example"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("stream with a foreign Host = %d, want 403", resp.StatusCode)
	}
}

// waitWatchers waits up to a second for the hub to count n clients.
func waitWatchers(h *Hub, n int) bool {
	for range 100 {
		if h.Watchers() == n {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
