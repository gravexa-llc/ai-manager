package monitor

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

var t0 = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

const (
	repo = "/Users/someone/Projects/app"
	wt   = repo + "/.claude/worktrees/agent-a1"
)

// The sessions of the fixture world:
//
//	pid:1   lead    registered plan aim
//	pid:2   watched you asked to watch it
//	pid:3   T1      the board links it to aim · T1 (through its worktree)
//	pid:4   lead2   the board links one of its subagents to aim · T2
//	job:j1  job     the board links the background job to sharing · S1
//	pid:5   in-lock works in a worktree locked for an agent of pid:1
//	pid:6   loose   nothing
func sessions() []store.Session {
	return []store.Session{
		{Key: "pid:1", SessionID: "s-lead", PID: 1, Name: "lead", Cwd: repo, Repo: repo, WorktreePath: repo, Branch: "main"},
		{Key: "pid:2", SessionID: "s-watch", PID: 2, Name: "watched", Cwd: "/tmp/notes"},
		{Key: "pid:3", SessionID: "s-t1", PID: 3, Name: "T1", Cwd: repo + "-t1", Repo: repo, WorktreePath: repo + "-t1", Branch: "aim/t1-thing"},
		{Key: "pid:4", SessionID: "s-lead2", PID: 4, Name: "lead2", Cwd: repo, Repo: repo, WorktreePath: repo, Branch: "main"},
		{Key: "job:j1", JobID: "j1", SessionID: "s-job", Kind: "background", Name: "job", Cwd: repo, Repo: repo},
		{Key: "pid:5", SessionID: "s-lock", PID: 5, Name: "in-lock", Cwd: wt + "/src", Repo: repo, WorktreePath: wt, Branch: "worktree-agent-a1"},
		{Key: "pid:6", SessionID: "s-loose", PID: 6, Name: "loose", Cwd: "/tmp/elsewhere"},
	}
}

func boards() []api.Plan {
	return []api.Plan{
		{Slug: "aim", Packages: []api.Package{
			{ID: "T1", Links: []api.Link{
				{Target: api.Target{Kind: "branch", Ref: "o/app:aim/t1-thing"}},
				{Target: api.Target{Kind: "pr", Ref: "o/app#7"}},
				{Target: api.Target{Kind: "agent", Ref: "s-t1/"}, Via: &api.Target{Kind: "worktree", Ref: repo + "-t1"},
					Agent: &api.LinkAgent{SessionID: "s-t1", Kind: "interactive", Live: true}},
			}},
			{ID: "T2", Links: []api.Link{{Target: api.Target{Kind: "agent", Ref: "s-lead2/a9"}}}},
			{ID: "T3", Links: []api.Link{{Target: api.Target{Kind: "agent", Ref: "s-lead2/a8"}}}},
		}},
		{Slug: "sharing", Packages: []api.Package{
			{ID: "S1", Links: []api.Link{{Target: api.Target{Kind: "agent", Ref: "job:j1"},
				Agent: &api.LinkAgent{SessionID: "s-job", JobID: "j1", Kind: "background"}}}},
		}},
	}
}

func explicit() []store.MonitoredSession {
	return []store.MonitoredSession{
		{SessionID: "s-watch", Reason: store.MonitorWatch, Since: t0},
		{SessionID: "s-lead", Reason: store.MonitorPlan, PlanID: 1, PlanSlug: "aim", Since: t0.Add(time.Minute)},
	}
}

func worktreeRows() []store.Worktree {
	return []store.Worktree{
		{Path: repo, MainRepo: repo, Branch: "main"},
		{Path: repo + "-t1", MainRepo: repo, Branch: "aim/t1-thing"},
		{Path: wt, MainRepo: repo, Branch: "worktree-agent-a1", Locked: true, LockAgentID: "a1", LockPID: 1},
	}
}

func input() Input {
	return Input{
		Mode: ModeOptIn, Sessions: sessions(), Explicit: explicit(), Plans: boards(),
		Worktrees: worktreeRows(), RepoNames: map[string]string{repo: "o/app"},
	}
}

func byKey(t *testing.T, rows []store.Session, key string) store.Session {
	t.Helper()
	for _, r := range rows {
		if r.Key == key {
			return r
		}
	}
	t.Fatalf("no session %s", key)
	return store.Session{}
}

func summaries(set *Set, rows []store.Session) map[string]string {
	out := map[string]string{}
	for _, r := range rows {
		if set.Monitored(r) {
			out[r.Key] = Summary(set.Why(r))
		}
	}
	return out
}

func TestRules(t *testing.T) {
	in := input()
	set := Compute(in)
	got := summaries(set, in.Sessions)
	want := map[string]string{
		"pid:1":  "registered plan aim",
		"pid:2":  "you asked to watch it",
		"pid:3":  "agent of T1",
		"pid:4":  "runs the agents of T2, T3",
		"job:j1": "agent of S1",
		"pid:5":  "agent of lead",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("monitored:\n got %v\nwant %v", got, want)
	}
	if set.Monitored(byKey(t, in.Sessions, "pid:6")) || set.Why(byKey(t, in.Sessions, "pid:6")) != nil {
		t.Error("the loose session is monitored")
	}
	if set.Mode() != ModeOptIn || set.All() {
		t.Errorf("mode %q, all %v", set.Mode(), set.All())
	}

	// The reasons carry what the dashboard groups and labels by.
	lead := set.Why(byKey(t, in.Sessions, "pid:1"))
	if len(lead) != 1 || lead[0].Reason != ReasonPlan || lead[0].Plan != "aim" || !lead[0].Since.Equal(t0.Add(time.Minute)) {
		t.Errorf("lead: %+v", lead)
	}
	if w := set.Why(byKey(t, in.Sessions, "pid:2")); PlanOf(w) != "" || !Has(w, ReasonWatch) || !w[0].Since.Equal(t0) {
		t.Errorf("watched: %+v", w)
	}
	if w := set.Why(byKey(t, in.Sessions, "pid:3")); PlanOf(w) != "aim" || w[0].Pkg != "T1" || w[0].Subagent {
		t.Errorf("T1: %+v", w)
	}
	// A subagent's reason names the subagent, so a wait can be told its
	// package; the session's own reason names none.
	if w := set.Why(byKey(t, in.Sessions, "pid:4")); len(w) != 2 || !w[0].Subagent || w[0].Pkg != "T2" || w[0].Agent != "a9" {
		t.Errorf("lead2: %+v", w)
	}
	if w := set.Why(byKey(t, in.Sessions, "pid:3")); w[0].Agent != "" {
		t.Errorf("T1's own session names an agent: %+v", w)
	}
	if w := set.Why(byKey(t, in.Sessions, "job:j1")); PlanOf(w) != "sharing" {
		t.Errorf("job: %+v", w)
	}
	// Lineage: under the lead's plan, named after the lead.
	if w := set.Why(byKey(t, in.Sessions, "pid:5")); len(w) != 1 || w[0].Lead != "pid:1" || w[0].LeadTitle != "lead" || PlanOf(w) != "aim" {
		t.Errorf("lineage: %+v", w)
	}
}

// Nothing explicit and no boards: only lineage could add anything, and it
// has no monitored lead to start from.
func TestNothingMonitored(t *testing.T) {
	in := input()
	in.Explicit, in.Plans = nil, nil
	set := Compute(in)
	for _, r := range in.Sessions {
		if set.Monitored(r) {
			t.Errorf("%s is monitored: %v", r.Key, Summary(set.Why(r)))
		}
	}
}

// A watch made before a /clear still holds when the row has the new id and
// keeps the old one as PrevSessionID, before or after the store has moved
// the watch.
func TestWatchFollowsClear(t *testing.T) {
	row := store.Session{Key: "pid:9", SessionID: "after", PrevSessionID: "before", PID: 9}
	for _, id := range []string{"before", "after"} {
		set := Compute(Input{Sessions: []store.Session{row}, Explicit: []store.MonitoredSession{{SessionID: id, Reason: store.MonitorWatch}}})
		if !set.Monitored(row) || !Has(set.Why(row), ReasonWatch) {
			t.Errorf("watch under %q: not monitored", id)
		}
	}
	// A session whose id merely starts the same isn't.
	set := Compute(Input{Sessions: []store.Session{row}, Explicit: []store.MonitoredSession{{SessionID: "aft", Reason: store.MonitorWatch}}})
	if set.Monitored(row) {
		t.Error("another session's watch counted")
	}
}

// Lineage chains: an agent of an agent of a monitored lead is monitored,
// whatever order the rows come in; the worktree's own session id is a
// lineage too; a main checkout never is.
func TestLineage(t *testing.T) {
	rows := []store.Session{
		{Key: "pid:30", SessionID: "grand", PID: 30, Cwd: "/r/x-c"},
		{Key: "pid:20", SessionID: "child", PID: 20, Cwd: "/r/x-b"},
		{Key: "pid:10", SessionID: "root", PID: 10, Cwd: "/r/x"},
		{Key: "pid:40", SessionID: "by-id", PID: 40, Cwd: "/r/x-d/deep/er"},
		{Key: "pid:50", SessionID: "main", PID: 50, Cwd: "/r/x/sub", WorktreePath: "/r/x"},
	}
	wts := []store.Worktree{
		{Path: "/r/x", MainRepo: "/r/x", Locked: true, LockPID: 10},
		{Path: "/r/x-b", MainRepo: "/r/x", Locked: true, LockPID: 10},
		{Path: "/r/x-c", MainRepo: "/r/x", Locked: true, LockPID: 20},
		{Path: "/r/x-d", MainRepo: "/r/x", SessionID: "child"},
		{Path: "/r/x-e", MainRepo: "/r/x", LockPID: 10, RemovedAt: t0},
	}
	set := Compute(Input{Sessions: rows, Worktrees: wts, Explicit: []store.MonitoredSession{{SessionID: "root", Reason: store.MonitorWatch}}})
	for key, lead := range map[string]string{"pid:20": "pid:10", "pid:30": "pid:20", "pid:40": "pid:20"} {
		w := set.Why(byKey(t, rows, key))
		if len(w) != 1 || w[0].Reason != ReasonAgent || w[0].Lead != lead {
			t.Errorf("%s: %+v, want agent of %s", key, w, lead)
		}
	}
	if set.Monitored(byKey(t, rows, "pid:50")) {
		t.Error("a session in the lead's main checkout counted as its agent")
	}
	// A removed worktree is no lineage.
	rows = append(rows, store.Session{Key: "pid:60", PID: 60, Cwd: "/r/x-e"})
	if set := Compute(Input{Sessions: rows, Worktrees: wts, Explicit: []store.MonitoredSession{{SessionID: "root", Reason: store.MonitorWatch}}}); set.Monitored(byKey(t, rows, "pid:60")) {
		t.Error("a removed worktree's lock counted")
	}
}

func TestPRs(t *testing.T) {
	in := input()
	// pid:6 (not monitored) works on a branch with a PR too.
	in.Sessions[6].Repo, in.Sessions[6].Branch = repo, "loose-branch"
	set := Compute(in)
	for _, tc := range []struct {
		name string
		pr   store.PR
		want bool
	}{
		{"linked by a board", store.PR{Repo: "o/app", Number: 7, HeadBranch: "whatever"}, true},
		{"linked, other case", store.PR{Repo: "O/App", Number: 7}, true},
		{"a monitored session's branch", store.PR{Repo: "o/app", Number: 8, HeadBranch: "aim/t1-thing"}, true},
		{"the lineage agent's branch", store.PR{Repo: "o/app", Number: 9, HeadBranch: "worktree-agent-a1"}, true},
		{"from a fork", store.PR{Repo: "o/app", Number: 10, HeadBranch: "aim/t1-thing", IsCrossRepository: true}, false},
		{"another repo", store.PR{Repo: "o/web", Number: 11, HeadBranch: "aim/t1-thing"}, false},
		{"an unmonitored session's branch", store.PR{Repo: "o/app", Number: 12, HeadBranch: "loose-branch"}, false},
		{"nobody's", store.PR{Repo: "o/app", Number: 13, HeadBranch: "old"}, false},
	} {
		if got := set.PR(tc.pr); got != tc.want {
			t.Errorf("%s: PR() = %v, want %v", tc.name, got, tc.want)
		}
	}
	// Without repo names only the board's links count.
	in.RepoNames = nil
	if Compute(in).PR(store.PR{Repo: "o/app", Number: 8, HeadBranch: "aim/t1-thing"}) {
		t.Error("a branch counted without the repo's GitHub name")
	}
}

// "all" mode: every session and PR, with the real reasons where there are
// some.
func TestModeAll(t *testing.T) {
	in := input()
	in.Mode = ModeAll
	set := Compute(in)
	if !set.All() || set.Mode() != ModeAll {
		t.Fatal("not all")
	}
	for _, r := range in.Sessions {
		if !set.Monitored(r) {
			t.Errorf("%s not monitored", r.Key)
		}
	}
	if got := Summary(set.Why(byKey(t, in.Sessions, "pid:6"))); got != `sessions.monitor is "all"` {
		t.Errorf("loose: %q", got)
	}
	if got := Summary(set.Why(byKey(t, in.Sessions, "pid:1"))); got != "registered plan aim" {
		t.Errorf("lead: %q", got)
	}
	if !set.PR(store.PR{Repo: "x/y", Number: 1}) {
		t.Error("a PR not counted")
	}
}

func TestNilSet(t *testing.T) {
	var set *Set
	row := store.Session{Key: "pid:1"}
	if !set.All() || !set.Monitored(row) || set.Why(row) != nil || !set.PR(store.PR{}) || set.Mode() != ModeAll {
		t.Error("a nil set doesn't monitor everything")
	}
}

func TestSummary(t *testing.T) {
	for _, tc := range []struct {
		ws   []Why
		want string
	}{
		{nil, ""},
		{[]Why{{Reason: ReasonPlan, Plan: "sharing"}, {Reason: ReasonWatch}}, "registered plan sharing · you asked to watch it"},
		{[]Why{{Reason: ReasonAgent, Plan: "aim", Pkg: "T1"}, {Reason: ReasonAgent, Plan: "aim", Pkg: "T2"}}, "agent of T1, T2"},
		{[]Why{{Reason: ReasonAgent, Plan: "aim", Pkg: "T1", Subagent: true}}, "runs the agent of T1"},
		{[]Why{{Reason: ReasonPlan, Plan: "aim"}, {Reason: ReasonAgent, Plan: "sharing", Pkg: "S1"}, {Reason: ReasonAgent, Plan: "sharing", Pkg: "S2", Subagent: true}},
			"registered plan aim · agent of sharing · S1 · runs the agent of sharing · S2"},
		{[]Why{{Reason: ReasonAgent, Lead: "pid:1"}}, "agent of pid:1"},
		{[]Why{{Reason: ReasonAgent, Lead: "pid:1", LeadTitle: "lead"}}, "agent of lead"},
	} {
		if got := Summary(tc.ws); got != tc.want {
			t.Errorf("Summary(%+v) = %q, want %q", tc.ws, got, tc.want)
		}
	}
}

// Load reads the stored monitoring from a real store, carried over a
// /clear, and fails safe when an input can't be read.
func TestSourceLoad(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), store.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.UpsertSessions(ctx, store.Session{Key: "pid:1", SessionID: "a", PID: 1, LastSeenAt: t0}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.WatchSession(ctx, "a", t0); err != nil {
		t.Fatal(err)
	}
	// /clear, seen by the poller only, then once more through the mod's
	// handover.
	if err := st.UpsertSessions(ctx, store.Session{Key: "pid:1", SessionID: "b", PID: 1, LastSeenAt: t0.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MoveSessionID(ctx, "b", "c"); err != nil {
		t.Fatal(err)
	}
	rows, err := st.ListSessions(ctx, store.SessionFilter{})
	if err != nil {
		t.Fatal(err)
	}
	src := &Source{Mode: ModeOptIn, Store: st}
	set, err := src.Load(ctx, rows, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].SessionID != "c" || !Has(set.Why(rows[0]), ReasonWatch) {
		t.Errorf("after two /clears: %+v monitored %v", rows, set.Why(rows[0]))
	}

	var nilSrc *Source
	if set, err := nilSrc.Load(ctx, rows, nil); set != nil || err != nil {
		t.Errorf("nil source: %v, %v", set, err)
	}
	broken := &Source{Mode: ModeOptIn, Store: st, Plans: func(context.Context) ([]api.Plan, error) { return nil, errors.New("boom") }}
	set, err = broken.Load(ctx, rows, nil)
	if err == nil || set != nil || !set.Monitored(store.Session{Key: "pid:2"}) {
		t.Errorf("a failed input: %v, %v", set, err)
	}
	withPlans := &Source{Mode: ModeOptIn, Store: st, Plans: func(context.Context) ([]api.Plan, error) { return boards(), nil },
		RepoNames: func() map[string]string { return map[string]string{repo: "o/app"} }}
	set, err = withPlans.Load(ctx, rows, []store.Worktree{})
	if err != nil || !set.PR(store.PR{Repo: "o/app", Number: 7}) {
		t.Errorf("with plans: %v", err)
	}
}
