package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// PlanService is the plan API's logic (internal/planapi.Service). An
// error with an HTTPStatus() int method is a refusal, answered with that
// status and {"error": its message}; any other error is a 500.
type PlanService interface {
	Plans(ctx context.Context) (api.PlanList, error)
	Plan(ctx context.Context, slug string) (api.Plan, error)
	Summary(ctx context.Context, slug string, since time.Time) (api.PlanSummary, error)
	Register(ctx context.Context, req api.RegisterRequest) (api.RegisterResponse, error)
	Unregister(ctx context.Context, slug string) (api.Unregistered, error)
	Detect(ctx context.Context, path string) (api.Detection, error)
	Dismiss(ctx context.Context, path string) (api.Dismissal, error)
	LinkSpawn(ctx context.Context, req api.SpawnLinkRequest) (api.LinkResult, error)
	LinkPR(ctx context.Context, req api.PRLinkRequest) (api.LinkResult, error)
	// RefreshPRs asks for a PR a tool call created, merged or closed to
	// be read from GitHub now.
	RefreshPRs(ctx context.Context, req api.PRRefreshRequest) (api.PRRefreshResult, error)
	LinkManual(ctx context.Context, req api.LinkRequest) (api.LinkResult, error)
	Unlink(ctx context.Context, req api.LinkRequest) (api.LinkResult, error)
	UnlinkID(ctx context.Context, id int64) (api.LinkResult, error)
	// HideUnmatched hides a PR or branch from a plan's Unmatched lane, or
	// restores it: the plan page's Not part of this plan.
	HideUnmatched(ctx context.Context, req api.HideRequest) (api.HideResult, error)
	SetDone(ctx context.Context, slug, id string, done bool) (api.DoneResult, error)
	Context(ctx context.Context, cwd string) (api.Context, error)
	// Timeline is a plan's history, which the app's plan page draws its
	// timeline view from (app_plans.go).
	Timeline(ctx context.Context, slug string, q api.TimelineQuery) (api.Timeline, error)
}

// maxJSONBytes bounds a plan API request body.
const maxJSONBytes = 64 << 10

// planRoutes adds the plan API (docs/api.md).
func (s *Server) planRoutes(p PlanService) {
	h := planHandlers{s: s, p: p}
	s.mux.HandleFunc("GET /api/plans", h.list)
	s.mux.HandleFunc("POST /api/plans", h.register)
	s.mux.HandleFunc("POST /api/plans/detect", h.detect)
	s.mux.HandleFunc("POST /api/plans/dismiss", h.dismiss)
	s.mux.HandleFunc("GET /api/plans/{slug}", h.get)
	s.mux.HandleFunc("DELETE /api/plans/{slug}", h.unregister)
	s.mux.HandleFunc("GET /api/plans/{slug}/summary", h.summary)
	s.mux.HandleFunc("POST /api/plans/{slug}/packages/{id}/done", h.done(true))
	s.mux.HandleFunc("DELETE /api/plans/{slug}/packages/{id}/done", h.done(false))
	s.mux.HandleFunc("POST /api/links/spawn", h.spawn)
	s.mux.HandleFunc("POST /api/links/pr", h.pr)
	s.mux.HandleFunc("POST /api/prs/refresh", h.refresh)
	s.mux.HandleFunc("POST /api/links/manual", h.manual)
	s.mux.HandleFunc("DELETE /api/links", h.unlink)
	s.mux.HandleFunc("DELETE /api/links/{id}", h.unlinkID)
	s.mux.HandleFunc("GET /api/context", h.context)
}

// apiAuth guards writes under /api/ and /events: they need aimd's token in
// X-AIM-Token (the mod, the CLI), or, under /api/ only, a valid CSRF
// token from one of aimd's own pages (CSRF.Protect: same origin, token
// issued by this aimd), so the dashboard can call the API without knowing
// the token. A page's script sends it in X-CSRF-Token. The pages' own forms
// post under /app/, so a form post to /api/ needs the token like any
// program's write.
func (s *Server) apiAuth(next http.Handler) http.Handler {
	token := RequireToken(s.opts.Token, next)
	page := s.csrf.Protect(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !safeMethod(r.Method) && r.Header.Get(TokenHeader) == "" && r.Header.Get(CSRFHeader) != "" &&
			strings.HasPrefix(r.URL.Path, "/api/") {
			page.ServeHTTP(w, r)
			return
		}
		token.ServeHTTP(w, r)
	})
}

type planHandlers struct {
	s *Server
	p PlanService
}

// answer writes v with status, or the error err.
func (h planHandlers) answer(w http.ResponseWriter, r *http.Request, status int, v any, err error) {
	if err != nil {
		var refusal interface{ HTTPStatus() int }
		if errors.As(err, &refusal) {
			writeError(w, refusal.HTTPStatus(), err.Error())
			return
		}
		h.s.log.Error("plan api", "method", r.Method, "path", r.URL.Path, "err", err)
		writeError(w, http.StatusInternalServerError, "aimd failed: "+err.Error())
		return
	}
	writeJSON(w, status, v)
}

// decode reads a JSON request body into v, whatever its Content-Type
// (curl -d sends form-encoded), up to maxJSONBytes. It answers 400 itself
// and returns false on a bad body.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBytes))
	if err := dec.Decode(v); err != nil {
		msg := "the request body must be a JSON object: " + err.Error()
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &tooBig):
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the request body is over %d bytes", maxJSONBytes))
			return false
		case errors.Is(err, io.EOF):
			msg = "the request body must be a JSON object; it is empty"
		}
		writeError(w, http.StatusBadRequest, msg)
		return false
	}
	return true
}

// The reads that carry a band or a summary run an inbox sync pass first
// (syncForSummaries): their need-you counts are the Needs you inbox's
// Open items.

func (h planHandlers) list(w http.ResponseWriter, r *http.Request) {
	h.s.syncForSummaries(r.Context())
	out, err := h.p.Plans(r.Context())
	h.answer(w, r, http.StatusOK, out, err)
}

func (h planHandlers) get(w http.ResponseWriter, r *http.Request) {
	h.s.syncForSummaries(r.Context())
	out, err := h.p.Plan(r.Context(), r.PathValue("slug"))
	h.answer(w, r, http.StatusOK, out, err)
}

func (h planHandlers) summary(w http.ResponseWriter, r *http.Request) {
	var since time.Time
	if v := r.URL.Query().Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "since must be an RFC 3339 time like 2026-11-05T12:00:00Z: "+strconv.Quote(v))
			return
		}
		since = t
	}
	h.s.syncForSummaries(r.Context())
	out, err := h.p.Summary(r.Context(), r.PathValue("slug"), since)
	h.answer(w, r, http.StatusOK, out, err)
}

func (h planHandlers) register(w http.ResponseWriter, r *http.Request) {
	var req api.RegisterRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	out, err := h.p.Register(r.Context(), req)
	status := http.StatusOK
	if out.Created {
		status = http.StatusCreated
	}
	h.answer(w, r, status, out, err)
}

func (h planHandlers) unregister(w http.ResponseWriter, r *http.Request) {
	out, err := h.p.Unregister(r.Context(), r.PathValue("slug"))
	h.answer(w, r, http.StatusOK, out, err)
}

func (h planHandlers) detect(w http.ResponseWriter, r *http.Request) {
	var req api.PathRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	out, err := h.p.Detect(r.Context(), req.Path)
	h.answer(w, r, http.StatusOK, out, err)
}

func (h planHandlers) dismiss(w http.ResponseWriter, r *http.Request) {
	var req api.PathRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	out, err := h.p.Dismiss(r.Context(), req.Path)
	h.answer(w, r, http.StatusOK, out, err)
}

func (h planHandlers) done(done bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out, err := h.p.SetDone(r.Context(), r.PathValue("slug"), r.PathValue("id"), done)
		h.answer(w, r, http.StatusOK, out, err)
	}
}

func (h planHandlers) spawn(w http.ResponseWriter, r *http.Request) {
	var req api.SpawnLinkRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	out, err := h.p.LinkSpawn(r.Context(), req)
	h.answer(w, r, http.StatusCreated, out, err)
}

func (h planHandlers) pr(w http.ResponseWriter, r *http.Request) {
	var req api.PRLinkRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	out, err := h.p.LinkPR(r.Context(), req)
	h.answer(w, r, http.StatusCreated, out, err)
}

// refresh answers 202: the read happens soon, in the daemon.
func (h planHandlers) refresh(w http.ResponseWriter, r *http.Request) {
	var req api.PRRefreshRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	out, err := h.p.RefreshPRs(r.Context(), req)
	h.answer(w, r, http.StatusAccepted, out, err)
}

func (h planHandlers) manual(w http.ResponseWriter, r *http.Request) {
	var req api.LinkRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	out, err := h.p.LinkManual(r.Context(), req)
	h.answer(w, r, http.StatusCreated, out, err)
}

func (h planHandlers) unlink(w http.ResponseWriter, r *http.Request) {
	var req api.LinkRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	out, err := h.p.Unlink(r.Context(), req)
	h.answer(w, r, http.StatusOK, out, err)
}

func (h planHandlers) unlinkID(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "a link id is a positive number: "+strconv.Quote(r.PathValue("id")))
		return
	}
	out, err := h.p.UnlinkID(r.Context(), id)
	h.answer(w, r, http.StatusOK, out, err)
}

func (h planHandlers) context(w http.ResponseWriter, r *http.Request) {
	h.s.syncForSummaries(r.Context())
	out, err := h.p.Context(r.Context(), r.URL.Query().Get("cwd"))
	h.answer(w, r, http.StatusOK, out, err)
}
