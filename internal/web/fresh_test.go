package web

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/store"
)

// Right after a state pass, the fresh facts are the pass's own: the
// dashboard's copy of the board's rules agrees with internal/state on a
// live session, a waiting subagent, a dismissed ask and the PRs.
func TestFreshFactsMatchThePass(t *testing.T) {
	e := newBoardEnv(t)
	e.registerAim()
	ctx := context.Background()
	must(t, e.st.UpdateSessionLive(ctx, "pid:42", store.SessionLive{State: "needs_you", StateReason: "permission prompt",
		NeedsYouSince: t0.Add(-3 * time.Minute), LastAction: "Bash: make check", LastEventAt: t0.Add(-time.Minute)}))
	must(t, e.st.DismissNeedsYou(ctx, "pid:43", t0)) // nothing to dismiss: kept as is
	must(t, e.st.UpsertAgents(ctx,
		store.Agent{SessionID: "lead-1", AgentID: "agent-d1", Kind: store.AgentSubagent, State: "waiting", LastAction: "Edit: live.go", StartedAt: t0.Add(-time.Hour)},
		store.Agent{SessionID: "sess-c6", Kind: store.AgentLead, State: "running", LastAction: "Read x.go"},
	))
	// Any write runs a pass.
	e.api("POST", "/api/plans/aim/packages/R1/done", nil, 200)

	b, err := e.svc.Plan(ctx, "aim")
	must(t, err)
	sn, err := e.srv.load(ctx)
	must(t, err)
	f, err := e.srv.freshFacts(ctx, sn)
	must(t, err)
	got := f.apply(b)
	checked := 0
	for i, p := range b.Packages {
		for j, l := range p.Links {
			g := got.Packages[i].Links[j]
			if !reflect.DeepEqual(g, l) {
				t.Errorf("%s link %s %s:\n fresh %+v %+v\n  pass %+v %+v", p.ID, l.Kind, l.Ref, g.Agent, g.PR, l.Agent, l.PR)
			}
			if l.Agent != nil || l.PR != nil {
				checked++
			}
		}
	}
	if checked < 10 {
		t.Errorf("only %d links with an agent or a PR compared", checked)
	}
	if (freshFacts{}).apply(b).Packages[0].Links == nil && len(b.Packages[0].Links) > 0 {
		t.Error("a zero freshFacts changed the board")
	}
}
