package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// The plan API end to end (real store, repos and state engine) is tested
// in internal/planapi. These tests cover the HTTP side: auth, routing and
// how errors are answered, over a stub service.

// stubPlans answers every call with its err, or a fixed body.
type stubPlans struct {
	err   error
	calls []string
}

type refusal struct{ status int }

func (r refusal) Error() string   { return "refused for a reason" }
func (r refusal) HTTPStatus() int { return r.status }

func (s *stubPlans) note(name string) error { s.calls = append(s.calls, name); return s.err }

func (s *stubPlans) Plans(context.Context) (api.PlanList, error) {
	return api.PlanList{Plans: []api.Plan{}}, s.note("Plans")
}
func (s *stubPlans) Plan(_ context.Context, slug string) (api.Plan, error) {
	return api.Plan{Slug: slug}, s.note("Plan " + slug)
}
func (s *stubPlans) Summary(_ context.Context, slug string, since time.Time) (api.PlanSummary, error) {
	return api.PlanSummary{Slug: slug, Since: since}, s.note("Summary " + slug)
}
func (s *stubPlans) Register(_ context.Context, req api.RegisterRequest) (api.RegisterResponse, error) {
	return api.RegisterResponse{Created: true, Path: req.Path}, s.note("Register " + req.Path)
}
func (s *stubPlans) Unregister(_ context.Context, slug string) (api.Unregistered, error) {
	return api.Unregistered{Slug: slug}, s.note("Unregister " + slug)
}
func (s *stubPlans) Detect(_ context.Context, path string) (api.Detection, error) {
	return api.Detection{Path: path}, s.note("Detect " + path)
}
func (s *stubPlans) Dismiss(_ context.Context, path string) (api.Dismissal, error) {
	return api.Dismissal{Path: path}, s.note("Dismiss " + path)
}
func (s *stubPlans) LinkSpawn(_ context.Context, req api.SpawnLinkRequest) (api.LinkResult, error) {
	return api.LinkResult{}, s.note("LinkSpawn " + req.Pkg)
}
func (s *stubPlans) LinkPR(_ context.Context, req api.PRLinkRequest) (api.LinkResult, error) {
	return api.LinkResult{}, s.note("LinkPR " + req.URL)
}
func (s *stubPlans) RefreshPRs(_ context.Context, req api.PRRefreshRequest) (api.PRRefreshResult, error) {
	return api.PRRefreshResult{}, s.note("RefreshPRs " + req.Ref)
}
func (s *stubPlans) LinkManual(_ context.Context, req api.LinkRequest) (api.LinkResult, error) {
	return api.LinkResult{}, s.note("LinkManual " + req.Ref)
}
func (s *stubPlans) Unlink(_ context.Context, req api.LinkRequest) (api.LinkResult, error) {
	return api.LinkResult{}, s.note("Unlink " + req.Ref)
}
func (s *stubPlans) UnlinkID(_ context.Context, id int64) (api.LinkResult, error) {
	return api.LinkResult{}, s.note("UnlinkID")
}
func (s *stubPlans) HideUnmatched(context.Context, api.HideRequest) (api.HideResult, error) {
	return api.HideResult{}, s.note("HideUnmatched")
}
func (s *stubPlans) SetDone(_ context.Context, slug, id string, done bool) (api.DoneResult, error) {
	if done {
		return api.DoneResult{}, s.note("Done " + slug + " " + id)
	}
	return api.DoneResult{}, s.note("Undone " + slug + " " + id)
}
func (s *stubPlans) Context(_ context.Context, cwd string) (api.Context, error) {
	return api.Context{Cwd: cwd}, s.note("Context " + cwd)
}
func (s *stubPlans) Timeline(_ context.Context, slug string, q api.TimelineQuery) (api.Timeline, error) {
	return api.Timeline{Slug: slug, Query: q}, s.note("Timeline " + slug)
}

func planServer(t *testing.T, p PlanService) *Server {
	t.Helper()
	return New(Options{Port: testPort, Token: testToken, Store: newTestStore(t), Plans: p, Now: func() time.Time { return t0 }})
}

func TestPlanRoutes(t *testing.T) {
	stub := &stubPlans{}
	h := planServer(t, stub).routes()
	tok := map[string]string{TokenHeader: testToken}
	for _, c := range []struct {
		method, target, body string
		status               int
		call                 string
	}{
		{"GET", "/api/plans", "", 200, "Plans"},
		{"GET", "/api/plans/aim", "", 200, "Plan aim"},
		{"DELETE", "/api/plans/aim", "", 200, "Unregister aim"},
		{"GET", "/api/plans/aim/summary?since=2026-11-05T10:00:00Z", "", 200, "Summary aim"},
		{"POST", "/api/plans", `{"path":"/r/p.md"}`, 201, "Register /r/p.md"},
		{"POST", "/api/plans/detect", `{"path":"/r/p.md"}`, 200, "Detect /r/p.md"},
		{"POST", "/api/plans/dismiss", `{"path":"/r/p.md"}`, 200, "Dismiss /r/p.md"},
		{"POST", "/api/plans/aim/packages/C3/done", "", 200, "Done aim C3"},
		{"DELETE", "/api/plans/aim/packages/C3/done", "", 200, "Undone aim C3"},
		{"POST", "/api/links/spawn", `{"pkg":"C3"}`, 201, "LinkSpawn C3"},
		{"POST", "/api/links/pr", `{"url":"u"}`, 201, "LinkPR u"},
		{"POST", "/api/prs/refresh", `{"ref":"12"}`, 202, "RefreshPRs 12"},
		{"POST", "/api/links/manual", `{"ref":"r"}`, 201, "LinkManual r"},
		{"DELETE", "/api/links", `{"ref":"r"}`, 200, "Unlink r"},
		{"DELETE", "/api/links/7", "", 200, "UnlinkID"},
		{"GET", "/api/context?cwd=/r", "", 200, "Context /r"},
	} {
		stub.calls = nil
		rec := do(t, h, c.method, c.target, strings.NewReader(c.body), tok)
		if rec.Code != c.status || len(stub.calls) != 1 || stub.calls[0] != c.call {
			t.Errorf("%s %s: %d, calls %v; want %d, %q (%s)", c.method, c.target, rec.Code, stub.calls, c.status, c.call, rec.Body)
		}
	}
	stub.calls = nil
	for _, c := range []struct{ method, target, body, says string }{
		{"GET", "/api/plans/aim/summary?since=soon", "", "RFC 3339"},
		{"POST", "/api/plans", "not json", "JSON"},
		{"POST", "/api/plans", "", "empty"},
		{"DELETE", "/api/links/x", "", "positive number"},
	} {
		rec := do(t, h, c.method, c.target, strings.NewReader(c.body), tok)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), c.says) {
			t.Errorf("%s %s %q: %d %s; want 400 saying %q", c.method, c.target, c.body, rec.Code, rec.Body, c.says)
		}
	}
	if big := do(t, h, "POST", "/api/plans", strings.NewReader(`{"path":"`+strings.Repeat("x", maxJSONBytes)+`"}`), tok); big.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: %d", big.Code)
	}
	if len(stub.calls) != 0 {
		t.Errorf("bad requests reached the service: %v", stub.calls)
	}
}

func TestPlanErrors(t *testing.T) {
	stub := &stubPlans{err: refusal{http.StatusConflict}}
	h := planServer(t, stub).routes()
	rec := do(t, h, "POST", "/api/plans", strings.NewReader(`{"path":"/r/p.md"}`), map[string]string{TokenHeader: testToken})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"error":"refused for a reason"`) {
		t.Errorf("refusal: %d %s", rec.Code, rec.Body)
	}
	stub.err = errors.New("disk on fire")
	rec = do(t, h, "GET", "/api/plans", nil, nil)
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "disk on fire") {
		t.Errorf("failure: %d %s", rec.Code, rec.Body)
	}
}

// Writes under /api/ take the token or a page's CSRF token; /events takes
// only the token.
func TestPlanAuth(t *testing.T) {
	stub := &stubPlans{}
	srv := planServer(t, stub)
	h := srv.routes()
	body := func() *strings.Reader { return strings.NewReader(`{"path":"/r/p.md"}`) }
	csrf := srv.CSRF().Token()
	for _, c := range []struct {
		name   string
		hdr    map[string]string
		status int
	}{
		{"none", nil, http.StatusUnauthorized},
		{"wrong token", map[string]string{TokenHeader: "x"}, http.StatusUnauthorized},
		{"token", map[string]string{TokenHeader: testToken}, http.StatusOK},
		{"csrf", map[string]string{CSRFHeader: csrf, "Origin": "http://localhost:7777"}, http.StatusOK},
		{"bad csrf", map[string]string{CSRFHeader: "nope"}, http.StatusForbidden},
		{"csrf, other origin", map[string]string{CSRFHeader: csrf, "Origin": "http://127.0.0.1:9999"}, http.StatusForbidden},
		{"wrong token and good csrf", map[string]string{TokenHeader: "x", CSRFHeader: csrf}, http.StatusUnauthorized},
	} {
		if rec := do(t, h, "POST", "/api/plans/dismiss", body(), c.hdr); rec.Code != c.status {
			t.Errorf("%s: %d, want %d (%s)", c.name, rec.Code, c.status, rec.Body)
		}
	}
	if rec := do(t, h, "POST", "/events", body(), map[string]string{CSRFHeader: csrf}); rec.Code != http.StatusUnauthorized {
		t.Errorf("/events with a CSRF token: %d, want 401", rec.Code)
	}

	// Without a plan service the routes don't exist.
	bare := New(Options{Port: testPort, Token: testToken, Store: newTestStore(t)}).routes()
	if rec := do(t, bare, "GET", "/api/plans", nil, nil); rec.Code != http.StatusNotFound {
		t.Errorf("GET /api/plans without a service: %d, want 404", rec.Code)
	}
}
