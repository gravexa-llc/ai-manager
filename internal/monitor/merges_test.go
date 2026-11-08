package monitor

import (
	"context"
	"errors"
	"testing"

	"github.com/gravexa-llc/ai-manager/internal/prwait"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// The Settings page's override of who merges replaces the
// board's Merges line before Compute reads it, without changing the
// plan service's boards.
func TestSourceMergesOverride(t *testing.T) {
	ctx := context.Background()
	plans := boards()
	plans[0].Merges = prwait.MergesYou
	plans[0].Packages[0].Links[2].Agent.State = "turn_done" // T1's agent is done: #7 is ready
	over := map[string]string{}
	var overErr error
	src := &Source{Mode: ModeOptIn, Store: emptyStore{},
		Plans:     func(context.Context) ([]api.Plan, error) { return plans, nil },
		RepoNames: func() map[string]string { return map[string]string{repo: "o/app"} },
		Merges:    func(context.Context) (map[string]string, error) { return over, overErr },
	}
	ready := store.PR{Repo: "o/app", Number: 7, HeadBranch: "x", State: store.PROpen, Mergeable: "MERGEABLE", MergeStateStatus: "CLEAN"}
	verdict := func() prwait.Verdict {
		t.Helper()
		set, err := src.Load(ctx, sessions(), worktreeRows())
		if err != nil {
			t.Fatal(err)
		}
		return set.PRWait(ready)
	}

	if v := verdict(); v.NeedsYou != prwait.ReadyToMerge || v.WaitingOn != prwait.WaitingYou || v.Merges != prwait.MergesYou {
		t.Errorf("the plan's line (you): %+v", v)
	}
	over["aim"] = prwait.MergesLead
	if v := verdict(); v.NeedsYou != "" || v.WaitingOn != prwait.WaitingLead || v.Merges != prwait.MergesLead {
		t.Errorf("overridden to lead: %+v", v)
	}
	if plans[0].Merges != prwait.MergesYou {
		t.Errorf("the plan service's board was changed: %q", plans[0].Merges)
	}
	// An override of another plan, or none, leaves the line.
	over = map[string]string{"sharing": prwait.MergesLead}
	if v := verdict(); v.NeedsYou != prwait.ReadyToMerge {
		t.Errorf("another plan's override: %+v", v)
	}
	// A line that says lead, overridden back to you.
	plans[0].Merges = prwait.MergesLead
	over = map[string]string{"aim": prwait.MergesYou}
	if v := verdict(); v.NeedsYou != prwait.ReadyToMerge || v.Merges != prwait.MergesYou {
		t.Errorf("a lead line overridden to you: %+v", v)
	}

	// The overrides can't be read: no set, so nothing is hidden.
	overErr = errors.New("boom")
	set, err := src.Load(ctx, sessions(), worktreeRows())
	if err == nil || set != nil {
		t.Errorf("a failed read: %v, %v", set, err)
	}
}

// emptyStore has no stored monitoring.
type emptyStore struct{}

func (emptyStore) ListMonitored(context.Context) ([]store.MonitoredSession, error) { return nil, nil }
func (emptyStore) ListWorktrees(context.Context, store.WorktreeFilter) ([]store.Worktree, error) {
	return nil, nil
}
