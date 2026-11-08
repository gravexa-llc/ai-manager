package web

// Redirects: the app under /app/ is the dashboard, and the old pages' URLs
// (the first dashboard's Home, Sessions and Worktrees, and a plan's board,
// graph and timeline) send the browser to their app pages.
//
//   - GET /  302 to /app/, the Overview. It is aimd's root, the address
//     `aim up` prints, so it stays a temporary redirect: browsers don't
//     cache it, and the root stays free for something else later.
//   - GET /sessions, /worktrees, /plans/{slug}, /plans/{slug}/graph and
//     /plans/{slug}/timeline  308 to the app's page, which replaces them
//     for good. The graph and the timeline become the plan page's ?view=,
//     and the timeline's ?pkg= filter its ?pkg= selection. Other old query
//     parameters (?all=, ?flash=, ?fragment=, ?agent=, ?n=) mean nothing
//     there and are dropped.
//
// Old #anchors never reach aimd. The browser keeps them across the
// redirect, and the app's script maps the old ones (Home's sections, a
// board's #pkg-<ID>) to their pages (static/app-shell.js, legacyAnchor).
//
// The old pages' forms (POST /dismiss, /watch, /unwatch, /plans and
// /plans/{slug}/…) are gone: the app's own forms post under /app/.

import (
	"net/http"
	"net/url"
	"strings"
)

// routeRedirects adds the old pages' redirects.
func (s *Server) routeRedirects() {
	s.mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, appOverview.pattern, http.StatusFound)
	})
	s.mux.HandleFunc("GET /sessions", permanent(appSessionsPath))
	s.mux.HandleFunc("GET /worktrees", permanent(appWorktrees.pattern))
	s.mux.HandleFunc("GET /plans/{slug}", planRedirect(""))
	s.mux.HandleFunc("GET /plans/{slug}/graph", planRedirect(planGraph))
	s.mux.HandleFunc("GET /plans/{slug}/timeline", planRedirect(planTimeline))
}

// permanent redirects for good (308) to target, without the old query.
func permanent(target string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target, http.StatusPermanentRedirect)
	}
}

// planRedirect redirects an old plan page for good (308) to the app's
// plan page: in view, or in its default view (the graph) for the old
// board; with the old ?pkg=.
func planRedirect(view string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := url.Values{}
		if view != "" {
			q.Set("view", view)
		}
		if pkg := strings.TrimSpace(r.URL.Query().Get("pkg")); pkg != "" {
			q.Set("pkg", pkg)
		}
		target := appPlanURL(r.PathValue("slug"))
		if len(q) > 0 {
			target += "?" + q.Encode()
		}
		http.Redirect(w, r, target, http.StatusPermanentRedirect)
	}
}
