package planapi_test

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// leads lists the stored plan leads as "session plan".
func (e *env) leads() []string {
	e.t.Helper()
	ms, err := e.st.ListMonitored(context.Background())
	if err != nil {
		e.t.Fatal(err)
	}
	var out []string
	for _, m := range ms {
		if m.Reason == store.MonitorPlan {
			out = append(out, m.SessionID+" "+m.PlanSlug)
		}
	}
	return out
}

// The first monitoring rule: POST /api/plans with a session id makes that
// session a monitored lead of the plan; without one, nobody is.
func TestRegisterMarksTheLead(t *testing.T) {
	e := newEnv(t)
	ok[api.RegisterResponse](t, e.req("POST", "/api/plans", api.RegisterRequest{Path: e.sharing}), http.StatusCreated)
	if got := e.leads(); len(got) != 0 {
		t.Errorf("registered without a session: leads %v", got)
	}
	ok[api.RegisterResponse](t, e.req("POST", "/api/plans", api.RegisterRequest{Path: e.aimPlan, SessionID: "lead-1"}), http.StatusCreated)
	// Again from the same session, and from a second lead (an update).
	ok[api.RegisterResponse](t, e.req("POST", "/api/plans", api.RegisterRequest{Path: e.aimPlan, SessionID: "lead-1"}), http.StatusOK)
	ok[api.RegisterResponse](t, e.req("POST", "/api/plans", api.RegisterRequest{Path: e.sharing, SessionID: "lead-2"}), http.StatusOK)
	if got, want := e.leads(), []string{"lead-1 aim", "lead-2 sharing"}; !slices.Equal(got, want) {
		t.Errorf("leads = %v, want %v", got, want)
	}
	// Unregistering a plan ends its leads.
	ok[api.Unregistered](t, e.req("DELETE", "/api/plans/aim", nil), http.StatusOK)
	if got, want := e.leads(), []string{"lead-2 sharing"}; !slices.Equal(got, want) {
		t.Errorf("leads after unregister = %v, want %v", got, want)
	}
}
