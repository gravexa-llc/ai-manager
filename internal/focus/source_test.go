package focus

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/sessions"
	"github.com/gravexa-llc/ai-manager/internal/store"
)

func TestStoreSource(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), store.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	t0 := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	if err := st.UpsertSessions(ctx,
		store.Session{SessionID: "s-lead", PID: 91470, Kind: "interactive", Name: "lead", Cwd: "/r/ai-manager", LastSeenAt: t0},
		store.Session{JobID: "022c44bf", SessionID: "s-job", Kind: "background", Cwd: "/r/x", LastSeenAt: t0},
	); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertAgents(ctx,
		store.Agent{SessionID: "s-old", AgentID: "a1111111111111111", Kind: store.AgentSubagent, StartedAt: t0.Add(-time.Hour), EndedAt: t0},
		store.Agent{SessionID: "s-lead", AgentID: "a1111111111111111", Kind: store.AgentSubagent, StartedAt: t0.Add(-2 * time.Hour)},
	); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertWorktrees(ctx, store.Worktree{
		Path: "/r/ai-manager/.claude/worktrees/agent-a2222222222222222", MainRepo: "/r/ai-manager",
		Locked: true, LockAgentID: "a2222222222222222", LockPID: 91470,
	}); err != nil {
		t.Fatal(err)
	}

	src := StoreSource{Store: st}
	targets, err := src.Targets(ctx)
	if err != nil || len(targets) != 2 {
		t.Fatalf("Targets = %+v, %v", targets, err)
	}
	if targets[0].Key != "pid:91470" || targets[1].Key != "job:022c44bf" || targets[1].JobID != "022c44bf" {
		t.Errorf("targets = %+v", targets)
	}
	// The live agent row wins over an ended one that started later.
	if l, err := src.LeadOf(ctx, "a1111111111111111"); err != nil || l.SessionID != "s-lead" {
		t.Errorf("LeadOf from agents = %+v, %v", l, err)
	}
	// Without the mod: the worktree lock names the lead's pid.
	if l, err := src.LeadOf(ctx, "a2222222222222222"); err != nil || l.PID != 91470 {
		t.Errorf("LeadOf from the lock = %+v, %v", l, err)
	}
	if _, err := src.LeadOf(ctx, "a3333333333333333"); !errors.Is(err, ErrUnknownAgent) {
		t.Errorf("unknown agent: %v", err)
	}

	// End to end: an agent id reaches its lead's tab.
	ft := &fakeTerminal{}
	res, err := (&Service{Source: src, Terminal: ft}).Go(ctx, "agent-a2222222222222222")
	if err != nil || res.Key != "pid:91470" || res.AgentID != "a2222222222222222" {
		t.Errorf("Go = %+v, %v", res, err)
	}
}

// fakeLister answers one listing.
type fakeLister struct{ snap sessions.Snapshot }

func (f fakeLister) List(context.Context) (sessions.Snapshot, error) { return f.snap, nil }

func TestListerSource(t *testing.T) {
	src := ListerSource{TempDir: "/private/tmp", Lister: fakeLister{sessions.Snapshot{Sessions: []sessions.Session{
		{PID: 1, Kind: sessions.Interactive, Cwd: "/r/a", Name: "a"},
		{PID: 2, Kind: sessions.Interactive, Cwd: "/private/tmp/p-run"},
		{JobID: "022c44bf", Kind: sessions.Background, State: sessions.JobBlocked, Cwd: "/private/tmp/bg"},
		{JobID: "aaaa0001", Kind: sessions.Background, State: sessions.JobDone}, // finished: not active
	}}}}
	targets, err := src.Targets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 3 {
		t.Fatalf("targets = %+v", targets)
	}
	if targets[0].Key != "pid:1" || targets[0].Headless || !targets[1].Headless || targets[2].Key != "job:022c44bf" || targets[2].Headless {
		t.Errorf("targets = %+v", targets)
	}
	if _, err := src.LeadOf(context.Background(), "a1111111111111111"); !errors.Is(err, ErrUnknownAgent) {
		t.Errorf("LeadOf without aimd: %v", err)
	}
}
