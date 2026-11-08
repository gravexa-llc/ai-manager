package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// Migration 0004 applies over a database at 0003 and keeps its rows; the
// packages it had start with no history.
func TestMigrate0004(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), FileName)
	db := rawDBAt(t, path)
	ms, err := loadMigrations(migrationFS)
	must(t, err)
	if len(ms) < 4 || ms[3].name != "0004_history.sql" {
		t.Fatalf("migration 4 = %+v", ms[3])
	}
	must(t, migrate(ctx, db, ms[:3]))
	_, err = db.Exec(`INSERT INTO plans (slug, created_at, updated_at) VALUES ('aim', 1, 1)`)
	must(t, err)
	_, err = db.Exec(`INSERT INTO packages (plan_id, pkg_id, lifecycle, lifecycle_at) VALUES (1, 'A1', 'ready', 5)`)
	must(t, err)
	must(t, db.Close())

	s, err := Open(ctx, path)
	must(t, err)
	defer s.Close()
	s.now = func() time.Time { return t0 }
	v, err := s.SchemaVersion(ctx)
	must(t, err)
	if v < 4 {
		t.Fatalf("schema version %d after open", v)
	}
	pkgs, err := s.ListPackages(ctx, 1)
	must(t, err)
	if len(pkgs) != 1 || pkgs[0].Lifecycle != "ready" {
		t.Fatalf("packages = %+v", pkgs)
	}
	h, err := s.ListLifecycleHistory(ctx, 1, HistoryFilter{})
	must(t, err)
	equal(t, "history of rows from before 0004", len(h), 0)

	// The next change is recorded, from the stored lifecycle on.
	must(t, s.SetLifecycles(ctx, 1, []PackageLifecycle{{PkgID: "A1", Lifecycle: "working", Prev: "ready", At: at(time.Minute)}}))
	h, err = s.ListLifecycleHistory(ctx, 1, HistoryFilter{})
	must(t, err)
	equal(t, "history", h, []LifecycleChange{{ID: h[0].ID, PlanID: 1, PkgID: "A1", Lifecycle: "working", Prev: "ready", At: at(time.Minute)}})
}

// SetLifecycles writes a history row for each real change, in order, and
// none for a lifecycle that didn't change or a package that's gone. The
// rows go with their package and plan.
func TestLifecycleHistory(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	plan := newPlan(t, s, "aim")
	other := newPlan(t, s, "other")
	must(t, s.SyncPackages(ctx, plan, []Package{{PkgID: "A1"}, {PkgID: "B1"}}))
	must(t, s.SyncPackages(ctx, other, []Package{{PkgID: "A1"}}))

	must(t, s.SetLifecycles(ctx, plan, []PackageLifecycle{
		{PkgID: "A1", Lifecycle: "ready", At: at(time.Minute)},
		{PkgID: "b1", Lifecycle: "blocked", At: at(time.Minute)},
		{PkgID: "Z9", Lifecycle: "ready"}, // gone: no row
	}))
	must(t, s.SetLifecycles(ctx, other, []PackageLifecycle{{PkgID: "A1", Lifecycle: "done", At: at(time.Minute)}}))
	must(t, s.SetLifecycles(ctx, plan, []PackageLifecycle{
		{PkgID: "A1", Lifecycle: "working", Prev: "ready", At: at(2 * time.Minute)},
		{PkgID: "B1", Lifecycle: "blocked", At: at(3 * time.Minute)}, // no change: no row
	}))
	must(t, s.SetLifecycles(ctx, plan, []PackageLifecycle{{PkgID: "A1", Lifecycle: "in_review", Prev: "working", At: at(4 * time.Minute)}}))

	h, err := s.ListLifecycleHistory(ctx, plan, HistoryFilter{})
	must(t, err)
	type row struct {
		pkg, lc, prev string
		at            time.Time
	}
	var got []row
	for _, c := range h {
		if c.PlanID != plan || c.ID == 0 {
			t.Errorf("row %+v", c)
		}
		got = append(got, row{c.PkgID, c.Lifecycle, c.Prev, c.At})
	}
	equal(t, "history, newest first", got, []row{
		{"A1", "in_review", "working", at(4 * time.Minute)},
		{"A1", "working", "ready", at(2 * time.Minute)},
		{"B1", "blocked", "", at(time.Minute)},
		{"A1", "ready", "", at(time.Minute)},
	})
	// The newest two at the same time come in insert order, newest first.
	if h[2].ID < h[3].ID {
		t.Errorf("ties not newest insert first: %+v", h)
	}

	a1, err := s.ListLifecycleHistory(ctx, plan, HistoryFilter{PkgID: "a1", Limit: 2})
	must(t, err)
	if len(a1) != 2 || a1[0].Lifecycle != "in_review" || a1[1].Lifecycle != "working" {
		t.Errorf("A1, limit 2 = %+v", a1)
	}

	// A package dropped from the plan takes its history with it; a plan
	// unregistered takes all of it.
	must(t, s.SyncPackages(ctx, plan, []Package{{PkgID: "A1"}}))
	h, err = s.ListLifecycleHistory(ctx, plan, HistoryFilter{})
	must(t, err)
	equal(t, "rows after B1 left the plan", len(h), 3)
	must(t, s.DeletePlan(ctx, plan))
	h, err = s.ListLifecycleHistory(ctx, plan, HistoryFilter{})
	must(t, err)
	equal(t, "rows after unregister", len(h), 0)
	h, err = s.ListLifecycleHistory(ctx, other, HistoryFilter{})
	must(t, err)
	equal(t, "the other plan's rows", len(h), 1)
}

// SyncAlerts raises new alerts, keeps open ones as they were raised, and
// clears the ones a pass no longer finds; a cleared alert that comes back
// is raised again.
func TestSyncAlerts(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	plan := newPlan(t, s, "aim")
	stalled := PlanAlert{Kind: "stalled", Key: "A1", Packages: []string{"A1"}, Message: "A1 is stalled for 3h", RaisedAt: at(-3 * time.Hour)}
	lane := PlanAlert{Kind: "lane_collision", Key: "app text", Packages: []string{"A2", "T1"}, Message: "Lane App text has 2 agents"}

	ch, err := s.SyncAlerts(ctx, plan, []PlanAlert{stalled, lane, {Kind: "stalled"}}, at(time.Minute))
	must(t, err)
	equal(t, "first pass", ch, AlertChanges{Raised: 2})

	// Again, with a newer message and different case: nothing changes.
	stalled.Message, lane.Key = "A1 is stalled for 4h", "App Text"
	ch, err = s.SyncAlerts(ctx, plan, []PlanAlert{stalled, lane}, at(2*time.Minute))
	must(t, err)
	equal(t, "same alerts", ch, AlertChanges{})

	// The lane clears, then the stall too and the lane comes back.
	ch, err = s.SyncAlerts(ctx, plan, []PlanAlert{stalled}, at(3*time.Minute))
	must(t, err)
	equal(t, "lane cleared", ch, AlertChanges{Cleared: 1})
	lane.RaisedAt = at(4 * time.Minute)
	ch, err = s.SyncAlerts(ctx, plan, []PlanAlert{lane}, at(4*time.Minute))
	must(t, err)
	equal(t, "stall cleared, lane back", ch, AlertChanges{Raised: 1, Cleared: 1})

	all, err := s.ListPlanAlerts(ctx, plan, HistoryFilter{})
	must(t, err)
	type row struct {
		kind, key, msg  string
		raised, cleared time.Time
	}
	var got []row
	for _, a := range all {
		got = append(got, row{a.Kind, a.Key, a.Message, a.RaisedAt, a.ClearedAt})
	}
	equal(t, "alerts, latest first", got, []row{
		{"lane_collision", "App Text", "Lane App text has 2 agents", at(4 * time.Minute), time.Time{}},
		{"stalled", "A1", "A1 is stalled for 3h", at(-3 * time.Hour), at(4 * time.Minute)},
		{"lane_collision", "app text", "Lane App text has 2 agents", at(time.Minute), at(3 * time.Minute)}, // raised at the pass's time
	})
	equal(t, "packages", all[0].Packages, []string{"A2", "T1"})

	t1, err := s.ListPlanAlerts(ctx, plan, HistoryFilter{PkgID: "t1"})
	must(t, err)
	equal(t, "alerts naming T1", len(t1), 2)
	one, err := s.ListPlanAlerts(ctx, plan, HistoryFilter{Limit: 1})
	must(t, err)
	equal(t, "limit 1", one[0].ID, all[0].ID)

	// The same alert twice in a pass still clears the others.
	ch, err = s.SyncAlerts(ctx, plan, []PlanAlert{stalled, lane}, at(5*time.Minute))
	must(t, err)
	equal(t, "stall back", ch, AlertChanges{Raised: 1})
	ch, err = s.SyncAlerts(ctx, plan, []PlanAlert{lane, lane}, at(6*time.Minute))
	must(t, err)
	equal(t, "lane twice", ch, AlertChanges{Cleared: 1})

	must(t, s.DeletePlan(ctx, plan))
	all, err = s.ListPlanAlerts(ctx, plan, HistoryFilter{})
	must(t, err)
	equal(t, "alerts after unregister", len(all), 0)
}

// ListLoopEvents reads the given loops only, newest first, without the
// excluded kinds, up to the limit.
func TestListLoopEvents(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	must(t, s.InsertEvents(ctx,
		Event{SessionID: "s1", Event: "tool", ToolName: "Bash", Summary: "Bash: make check", At: at(1 * time.Second)},
		Event{SessionID: "s1", AgentID: "a1", Event: "tool", ToolName: "Edit", Summary: "Edit x.go", At: at(2 * time.Second)},
		Event{SessionID: "s1", AgentID: "a1", Event: "turn_start", At: at(3 * time.Second)},
		Event{SessionID: "s1", AgentID: "a2", Event: "tool", Summary: "other agent", At: at(4 * time.Second)},
		Event{SessionID: "s2", Event: "turn_complete", Summary: "Done.", At: at(5 * time.Second)},
		Event{SessionID: "s3", Event: "tool", Summary: "not linked", At: at(6 * time.Second)},
	))
	got, err := s.ListLoopEvents(ctx, EventFilter{
		Loops:   []EventLoop{{SessionID: "s1", AgentID: "a1"}, {SessionID: "s2"}, {SessionID: "s1"}},
		Exclude: []string{"turn_start"},
	})
	must(t, err)
	var sums []string
	for _, e := range got {
		sums = append(sums, e.Summary)
	}
	equal(t, "events", sums, []string{"Done.", "Edit x.go", "Bash: make check"})

	got, err = s.ListLoopEvents(ctx, EventFilter{Loops: []EventLoop{{SessionID: "s1", AgentID: "a1"}}, Limit: 1})
	must(t, err)
	if len(got) != 1 || got[0].Event != "turn_start" {
		t.Errorf("limit 1 = %+v", got)
	}
	got, err = s.ListLoopEvents(ctx, EventFilter{})
	must(t, err)
	equal(t, "no loops", len(got), 0)
}
