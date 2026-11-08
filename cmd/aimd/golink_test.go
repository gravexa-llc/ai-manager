package main

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/config"
	"github.com/gravexa-llc/ai-manager/internal/monitor"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// fakeBoards is the plan service's Plans.
type fakeBoards struct {
	list api.PlanList
	err  error
}

func (f fakeBoards) Plans(context.Context) (api.PlanList, error) { return f.list, f.err }

// The notifier's filter follows sessions.monitor: in "opt-in", the watched
// session and the plan's agent; in "all", every session.
func TestMonitoredFilter(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), store.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rows := []store.Session{
		{Key: "pid:1", SessionID: "watched"},
		{Key: "pid:2", SessionID: "agent"},
		{Key: "pid:3", SessionID: "loose"},
	}
	if _, _, err := st.WatchSession(ctx, "watched", time.Time{}); err != nil {
		t.Fatal(err)
	}
	boards := fakeBoards{list: api.PlanList{Plans: []api.Plan{{Slug: "aim", Packages: []api.Package{
		{ID: "T1", Links: []api.Link{{Target: api.Target{Kind: "agent", Ref: "agent/"}}}},
	}}}}}
	source := func(mode string, b fakeBoards) *monitor.Source {
		return newMonitor(st, config.Sessions{Monitor: mode}, b, func() map[string]string { return nil })
	}
	for _, tc := range []struct {
		mode string
		want []bool
	}{
		{config.MonitorOptIn, []bool{true, true, false}},
		{config.MonitorAll, []bool{true, true, true}},
	} {
		keep, err := monitoredFilter(source(tc.mode, boards))(ctx, rows)
		if err != nil {
			t.Fatal(err)
		}
		for i, r := range rows {
			if keep(r) != tc.want[i] {
				t.Errorf("%s: %s kept %v, want %v", tc.mode, r.Key, keep(r), tc.want[i])
			}
		}
	}
	if _, err := monitoredFilter(source(config.MonitorOptIn, fakeBoards{err: errors.New("no plans")}))(ctx, rows); err == nil {
		t.Error("a failing plan service gave no error")
	}
}

// The notifier's inbox filter keeps the sessions whose wait is an Open
// item, by session row id, each with its item's page on aimd.
func TestInboxFilter(t *testing.T) {
	ctx := context.Background()
	open := func(context.Context) (map[int64]int64, error) { return map[int64]int64{7: 42}, nil }
	keep, err := inboxFilter(open, 7777)(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if u, ok := keep(store.Session{ID: 7}); !ok || u != "http://127.0.0.1:7777/app/inbox?item=42" {
		t.Errorf("session 7 = %q, %v", u, ok)
	}
	if _, ok := keep(store.Session{ID: 8}); ok {
		t.Error("session 8 has no Open item but is kept")
	}
	failing := func(context.Context) (map[int64]int64, error) { return nil, errors.New("db gone") }
	if _, err := inboxFilter(failing, 7777)(ctx, nil); err == nil {
		t.Error("a failing inbox gave no error")
	}
}

func TestSessionsPolled(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		srcs []api.Source
		want bool
	}{
		{nil, false},
		{[]api.Source{{Name: api.SourceSessions, Runs: 1, LastRun: now}}, false}, // ran, failed
		{[]api.Source{{Name: "github", LastOK: now}}, false},
		{[]api.Source{{Name: "github"}, {Name: api.SourceSessions, LastOK: now}}, true},
	} {
		if got := sessionsPolled(api.Status{Sources: tc.srcs}); got != tc.want {
			t.Errorf("sessionsPolled(%+v) = %v", tc.srcs, got)
		}
	}
}

// The notifier asks the Settings page's switch before each check; a
// store it can't read says on, so nothing that needs you goes quiet.
func TestNotificationsOn(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), store.FileName))
	if err != nil {
		t.Fatal(err)
	}
	on := notificationsOn(st, slog.New(slog.DiscardHandler))
	if !on(ctx) {
		t.Error("off by default")
	}
	if _, err := st.SetSettings(ctx, map[string]string{store.SettingNotifications: store.Off}); err != nil {
		t.Fatal(err)
	}
	if on(ctx) {
		t.Error("on after the switch went off")
	}
	st.Close()
	if !on(ctx) {
		t.Error("a store that can't be read says off")
	}
}

// aimd's monitor source applies the Settings page's overrides of who
// merges before it decides what a PR waits on.
func TestMonitorMergesOverride(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), store.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.InsertPlan(ctx, store.Plan{Slug: "aim"}); err != nil {
		t.Fatal(err)
	}
	boards := fakeBoards{list: api.PlanList{Plans: []api.Plan{{Slug: "aim", Merges: "you", Packages: []api.Package{
		{ID: "T1", Links: []api.Link{{Target: api.Target{Kind: "pr", Ref: "o/app#7"}}}},
	}}}}}
	src := newMonitor(st, config.Sessions{Monitor: config.MonitorOptIn}, boards, func() map[string]string { return nil })
	ready := store.PR{Repo: "o/app", Number: 7, State: store.PROpen, MergeStateStatus: "CLEAN"}
	waits := func() string {
		t.Helper()
		set, err := src.Load(ctx, nil, []store.Worktree{})
		if err != nil {
			t.Fatal(err)
		}
		return set.PRWait(ready).WaitingOn
	}
	if got := waits(); got != "you" {
		t.Errorf("the plan's line: waiting on %s", got)
	}
	if err := st.SetPlanMerges(ctx, "aim", store.MergesLead); err != nil {
		t.Fatal(err)
	}
	if got := waits(); got != "lead" {
		t.Errorf("overridden to lead: waiting on %s", got)
	}
}
