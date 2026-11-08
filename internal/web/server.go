// Package web is aimd's HTTP layer: the read API (GET /api/sessions,
// /api/worktrees, /api/prs, /api/status), the change stream
// (GET /api/stream), the mod's POST /events (events.go), the plan API
// (plans_api.go), Go (POST /api/go; see golink.go), monitored sessions
// (POST and DELETE /api/sessions/watch; see monitor.go), the dashboard:
// the web app under /app/ (app.go and APP.md), which GET / and the old
// pages' URLs redirect to (redirects.go), and the request checks every
// route goes through.
//
// # Request checks
//
// aimd listens on 127.0.0.1 only. On top of that, every request passes:
//
//   - CheckHost: the Host header must be 127.0.0.1:<port> or
//     localhost:<port>, which blocks DNS rebinding.
//   - No CORS: no Access-Control-* header is ever sent, so other origins
//     can't read responses or send preflighted requests.
//   - Writes (any method but GET, HEAD, OPTIONS) under /api/ and /events
//     need the X-AIM-Token header (RequireToken); under /api/ a dashboard
//     page's script may send its CSRF token in X-CSRF-Token instead
//     (apiAuth).
//     Writes anywhere else are the dashboard's own forms and need a CSRF
//     token (CSRF.Protect).
//   - Every response carries Content-Security-Policy: default-src 'self'
//     (so no inline script or style, and nothing loaded from elsewhere),
//     X-Frame-Options: DENY and Referrer-Policy: no-referrer.
//
// The response bodies are the types in internal/web/api.
package web

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/monitor"
	"github.com/gravexa-llc/ai-manager/internal/prwait"
	"github.com/gravexa-llc/ai-manager/internal/sessions"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/timezone"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// Store is the part of *store.Store the handlers read.
type Store interface {
	ListSessions(ctx context.Context, f store.SessionFilter) ([]store.Session, error)
	ListWorktrees(ctx context.Context, f store.WorktreeFilter) ([]store.Worktree, error)
	ListPRs(ctx context.Context, f store.PRFilter) ([]store.PR, error)
	// ListAgents gives a board's subagents as they are now (fresh.go).
	ListAgents(ctx context.Context, sessionID string) ([]store.Agent, error)
	// WatchSession and UnwatchSession back /aim watch, /aim unwatch and
	// the Sessions page's Watch and Stop monitoring (monitor.go).
	WatchSession(ctx context.Context, sessionID string, at time.Time) (store.MonitoredSession, bool, error)
	UnwatchSession(ctx context.Context, sessionIDs ...string) (int, error)
}

var _ Store = (*store.Store)(nil)

// StatusSource reports the pollers' health (the daemon). The counts are
// filled in from the store by the handler.
type StatusSource interface {
	Status() api.Status
}

// Options configure a Server.
type Options struct {
	// Port is the port aimd listens on; the Host check and CSRF origins
	// use it.
	Port int
	// Token is aimd's token, required on writes and keying CSRF tokens.
	Token  string
	Store  Store
	Status StatusSource
	// Hub serves /api/stream. Nil means a hub nobody publishes to.
	Hub *Hub
	// Plans serves the plan API (plans_api.go). Nil leaves it out.
	Plans PlanService
	// Go serves POST /api/go and the dashboard's Go buttons (golink.go).
	// Nil leaves them out.
	Go GoService
	// Terminals says what terminal app each live session runs in (the
	// daemon), for GET /api/sessions, the dashboard's terminal tags and
	// which Go buttons are enabled. Nil means every session has none.
	Terminals TerminalSource
	// Monitor decides which sessions are monitored (internal/monitor,
	// monitor.go): the Needs you inbox, the session lists and GET
	// /api/sessions follow it. aimd shares one with its notifier. Nil
	// monitors every session.
	Monitor *monitor.Source
	// Events takes POST /events (events.go). Nil drops what arrives.
	Events EventSink
	// Waits says what each session waits on (the event receiver,
	// internal/live), for GET /api/sessions' waiting_on. Nil leaves it out.
	Waits WaitSource
	// Repos maps main checkout paths to GitHub names (the daemon's
	// RepoNames), which the dashboard needs to join worktrees to their PRs.
	// Nil means worktrees show no PR.
	Repos RepoNamer
	// SafeToRemove gives each worktree's safe-to-remove verdict (`aim
	// prune`'s rules, internal/prune). Nil means the Worktrees page shows the
	// facts only.
	SafeToRemove SafeToRemove
	// Cleanup removes worktrees and clears prunable records from the app's
	// Worktrees & cleanup page, and keeps the removal log (cleanup.go).
	// Nil leaves the page read-only.
	Cleanup *Cleanup
	// Jobs tells which background jobs have expired: Claude Code deleted
	// their conversation (*sessions.Jobs; app_sessions.go). Nil means none
	// has.
	Jobs ExpiredJobs
	// PluginBuilds tells which plugin build is on disk in a plugin folder
	// now (*pluginbuild.Cache), so a session that loaded an older one is
	// flagged. Nil means no session is flagged.
	PluginBuilds PluginBuilds
	// NotifyDisabled is config notify.enabled = false: aimd sends no
	// notifications whatever the Settings page's switch says, and the page
	// says so.
	NotifyDisabled bool
	// HomeDir, if set, is shown as ~ at the start of paths.
	HomeDir string
	// Now is the clock for ages; time.Now if nil.
	Now func() time.Time
	// SystemZone is the machine's time zone, the Time zone setting's
	// System; nil reads it from /etc/localtime (timezone.Machine).
	SystemZone func() timezone.Zone
	// Logger receives handler errors; nil discards them.
	Logger *slog.Logger
}

// machineZone is the machine's time zone, for a Server without
// Options.SystemZone. The tests set it to UTC.
var machineZone = timezone.NewMachine("/etc/localtime").Zone

// systemZone is the Time zone setting's System: Options.SystemZone, or
// the machine's.
func (s *Server) systemZone() timezone.Zone {
	if s.opts.SystemZone != nil {
		return s.opts.SystemZone()
	}
	return machineZone()
}

// Server holds aimd's routes.
type Server struct {
	opts Options
	mux  *http.ServeMux
	csrf *CSRF
	now  func() time.Time
	log  *slog.Logger
	// inbox serializes the Needs you inbox's sync passes (app_inbox_sync.go).
	inbox inboxState
}

// New returns a server for opts.
func New(opts Options) *Server {
	s := &Server{
		opts: opts,
		mux:  http.NewServeMux(),
		csrf: NewCSRF(opts.Token, opts.Port),
		now:  opts.Now,
		log:  opts.Logger,
	}
	if s.opts.Hub == nil {
		s.opts.Hub = NewHub(nil)
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	s.mux.HandleFunc("GET /api/sessions", s.sessions)
	s.mux.HandleFunc("POST /api/sessions/watch", s.postWatch)
	s.mux.HandleFunc("DELETE /api/sessions/watch", s.deleteWatch)
	s.mux.HandleFunc("GET /api/worktrees", s.worktrees)
	s.mux.HandleFunc("GET /api/prs", s.prs)
	s.mux.HandleFunc("GET /api/status", s.status)
	s.mux.Handle("GET /api/stream", s.opts.Hub)
	if s.opts.Plans != nil {
		s.planRoutes(s.opts.Plans)
	}
	if s.opts.Go != nil {
		s.mux.HandleFunc("POST /api/go", s.postGo)
	}
	s.mux.HandleFunc("POST /events", s.postEvents)
	s.mux.HandleFunc("GET /static/{file}", serveStatic)
	s.routeApp()
	s.routeRedirects()
	return s
}

// CSRF returns the CSRF protection, for templates to call Token.
func (s *Server) CSRF() *CSRF { return s.csrf }

// Handler returns the routes behind the request checks.
func (s *Server) Handler() http.Handler {
	api := s.apiAuth(s.mux)
	forms := s.csrf.Protect(s.mux)
	route := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isAPIPath(r.URL.Path) {
			api.ServeHTTP(w, r)
			return
		}
		forms.ServeHTTP(w, r)
	})
	return CheckHost(s.opts.Port, securityHeaders(route))
}

// isAPIPath reports whether a path is for programs (token auth) rather
// than the dashboard's pages (CSRF).
func isAPIPath(p string) bool {
	return p == "/api" || strings.HasPrefix(p, "/api/") || p == "/events" || strings.HasPrefix(p, "/events/")
}

// truthy reads a query flag: 1, true, yes or on.
func truthy(v string) bool {
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// writeJSON writes v as a JSON response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	_ = enc.Encode(v)
}

// writeError writes {"error": msg}.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// fail logs a store error and answers 500 without details.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("api", "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "aimd couldn't read its database; see aimd.log")
}

// sessions is GET /api/sessions: live sessions, headless `claude -p` runs
// left out unless ?all=1, each with its terminal app, whether it is
// monitored, and, for a request that may see it (detailAllowed), what it
// waits on when it needs you, as the mod reported it.
func (s *Server) sessions(w http.ResponseWriter, r *http.Request) {
	rows, err := s.opts.Store.ListSessions(r.Context(), store.SessionFilter{})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	all := truthy(r.URL.Query().Get("all"))
	detail := s.detailAllowed(r)
	now := s.now()
	terms := s.terminals()
	mon := s.monitorSet(r.Context(), rows, nil)
	out := api.Sessions{Sessions: []api.Session{}, Monitor: mon.Mode()}
	for _, row := range rows {
		if row.Headless && !all {
			out.HiddenHeadless++
			continue
		}
		js := withMonitored(withTerminal(sessionJSON(row, now), appOf(terms, row)), mon, row)
		js.PluginOnDisk, js.PluginOutdated = s.pluginOf(r.Context(), row)
		if detail {
			js.WaitingOn = s.waitingOn(js)
		}
		if !js.Monitored {
			out.NotMonitored++
		}
		out.Sessions = append(out.Sessions, js)
	}
	writeJSON(w, http.StatusOK, out)
}

// detailAllowed reports whether a JSON read may see what a session waits
// on: it carries aimd's token in X-AIM-Token (as the mod and `aim` send
// it). Other reads under /api/ are open to any local process by design
// (docs/api.md, "Who may read it"), and the wait detail holds the commands
// and questions a person is asked: redacted twice, but not proof against
// every secret. A dashboard page's CSRF token is no credential for this:
// every page embeds one, so anything that can fetch a page has one.
// Server-rendered pages are aimd's own UI and may show the redacted detail.
func (s *Server) detailAllowed(r *http.Request) bool {
	tok := r.Header.Get(TokenHeader)
	return tok != "" && s.opts.Token != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(s.opts.Token)) == 1
}

// waitingOn is what a session that needs you waits on, when the event
// receiver knows; nil otherwise. Only for a request detailAllowed
// lets see it.
func (s *Server) waitingOn(js api.Session) *api.WaitingOn {
	if s.opts.Waits == nil || !js.NeedsYou || js.SessionID == "" {
		return nil
	}
	if w, ok := s.opts.Waits.WaitingOn(js.SessionID); ok {
		return &w
	}
	return nil
}

// needsYou is the session state that needs you (internal/sessions).
const needsYou = "needs_you"

func sessionJSON(r store.Session, now time.Time) api.Session {
	s := api.Session{
		Key:            r.Key,
		SessionID:      r.SessionID,
		JobID:          r.JobID,
		PrevSessionID:  r.PrevSessionID,
		Kind:           r.Kind,
		Headless:       r.Headless,
		Name:           r.Name,
		Host:           r.Host,
		Cwd:            r.Cwd,
		WorktreePath:   r.WorktreePath,
		Repo:           r.Repo,
		Branch:         r.Branch,
		PID:            r.PID,
		ItermSessionID: r.ItermSessionID,
		State:          r.State,
		StateReason:    r.StateReason,
		NeedsYou:       r.State == needsYou,
		LastAction:     r.LastAction,
		LastSaid:       r.LastSaid,
		PluginBuild:    r.PluginBuild,
		PluginRoot:     r.PluginRoot,
		StartedAt:      r.StartedAt,
		FirstSeenAt:    r.FirstSeenAt,
		LastSeenAt:     r.LastSeenAt,
		LastEventAt:    r.LastEventAt,
	}
	if s.NeedsYou && !r.NeedsYouSince.IsZero() {
		s.NeedsYouSince = r.NeedsYouSince
		s.NeedsYouAgeSeconds = int64(max(0, now.Sub(r.NeedsYouSince)) / time.Second)
		s.NeedsYouDismissed = !r.NeedsYouDismissedAt.IsZero() && !r.NeedsYouDismissedAt.Before(r.NeedsYouSince)
	}
	return s
}

// worktrees is GET /api/worktrees: worktrees the last scans found, with
// ?all=1 also the removed ones and ?repo=<main checkout path> one repo's.
func (s *Server) worktrees(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rows, err := s.opts.Store.ListWorktrees(r.Context(), store.WorktreeFilter{
		MainRepo:       q.Get("repo"),
		IncludeRemoved: truthy(q.Get("all")),
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := api.Worktrees{Worktrees: make([]api.Worktree, 0, len(rows))}
	for _, row := range rows {
		out.Worktrees = append(out.Worktrees, worktreeJSON(row))
	}
	writeJSON(w, http.StatusOK, out)
}

func worktreeJSON(r store.Worktree) api.Worktree {
	w := api.Worktree{
		Path:           r.Path,
		MainRepo:       r.MainRepo,
		Branch:         r.Branch,
		Detached:       r.Detached,
		HeadSHA:        r.HeadSHA,
		IgnoredAtRisk:  r.IgnoredAtRisk,
		UpstreamState:  r.UpstreamState,
		UnpushedCount:  r.UnpushedCount,
		LastCommitAt:   r.LastCommitAt,
		Locked:         r.Locked,
		LockedReason:   r.LockedReason,
		LockAgentID:    r.LockAgentID,
		LockPID:        r.LockPID,
		PrunableReason: r.PrunableReason,
		SessionID:      r.SessionID,
		AgentID:        r.AgentID,
		FirstSeen:      r.FirstSeen,
		LastSeen:       r.LastSeen,
		RemovedAt:      r.RemovedAt,
	}
	// The daemon stores -1 when git status couldn't be read.
	if r.DirtyCount >= 0 && r.UntrackedCount >= 0 {
		w.StatusKnown = true
		dirty, untracked := r.DirtyCount, r.UntrackedCount
		w.DirtyCount, w.UntrackedCount = &dirty, &untracked
	}
	return w
}

// prs is GET /api/prs: every stored PR, with ?state=open|merged|closed and
// ?repo=<owner/name> to narrow it.
func (s *Server) prs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.PRFilter{Repo: q.Get("repo")}
	switch st := strings.ToUpper(q.Get("state")); st {
	case "", "ALL":
	case store.PROpen, store.PRMerged, store.PRClosed:
		f.State = st
	default:
		writeError(w, http.StatusBadRequest, "state must be open, merged, closed or all")
		return
	}
	rows, err := s.opts.Store.ListPRs(r.Context(), f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	sess, err := s.opts.Store.ListSessions(r.Context(), store.SessionFilter{})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	mon := s.monitorSet(r.Context(), sess, nil)
	out := api.PRs{PRs: make([]api.PR, 0, len(rows))}
	for _, row := range rows {
		out.PRs = append(out.PRs, prJSON(row, mon.PRWait(row)))
	}
	writeJSON(w, http.StatusOK, out)
}

// prJSON is a PR as GET /api/prs gives it, with v, what it waits on.
func prJSON(r store.PR, v prwait.Verdict) api.PR {
	p := api.PR{
		Repo:              r.Repo,
		Number:            r.Number,
		Title:             r.Title,
		URL:               r.URL,
		Author:            r.Author,
		State:             r.State,
		Unread:            r.Unread(),
		Draft:             r.Draft,
		HeadBranch:        r.HeadBranch,
		HeadRefOID:        r.HeadRefOID,
		BaseRef:           r.BaseRef,
		IsCrossRepository: r.IsCrossRepository,
		Mergeable:         r.Mergeable,
		MergeStateStatus:  r.MergeStateStatus,
		ReviewDecision:    r.ReviewDecision,
		ChecksState:       r.ChecksState,
		PlanSlug:          r.PlanSlug,
		PlanPkg:           r.PlanPkg,
		UpdatedAt:         r.UpdatedAt,
		MergedAt:          r.MergedAt,
		ClosedAt:          r.ClosedAt,

		ReviewRequested:      r.ReviewRequested(),
		ReviewRequestedSince: r.ReviewRequestedAt,

		NeedsYou:  v.NeedsYou,
		WaitingOn: v.WaitingOn,
		Merges:    v.Merges,
		Reason:    v.Reason,
		Monitored: v.Monitored,
		Plan:      v.Plan,
		Pkg:       v.Pkg,
	}
	return p
}

// status is GET /api/status: the daemon's view plus counts from the store.
func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	snap, err := s.load(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, snap.status)
}

// buildStatus returns the daemon's status (or a bare one without a
// daemon) with the counts filled in from the store rows: live sessions,
// live worktrees and all PRs. What needs you is counted as needsYouTotal
// counts it: monitored sessions, but never an expired background job
// (expired, by session key: Claude Code deleted its conversation), and the
// PRs that need you (mon.PRWait).
func (s *Server) buildStatus(rows []store.Session, wts []store.Worktree, prs []store.PR, mon *monitor.Set, expired map[string]sessions.JobExpiry) api.Status {
	st := api.Status{Now: s.now()}
	if s.opts.Status != nil {
		st = s.opts.Status.Status()
	}
	st.Watchers = s.opts.Hub.Watchers()
	if s.opts.Events != nil {
		ev := s.opts.Events.Stats()
		st.Events = &ev
	}
	c := api.Counts{StaleSources: []string{}}
	for _, src := range st.Sources {
		if src.Stale {
			c.StaleSources = append(c.StaleSources, src.Name)
		}
	}
	for _, row := range rows {
		if row.Headless {
			c.HeadlessSessions++
			continue
		}
		c.Sessions++
		if !mon.Monitored(row) {
			c.NotMonitored++
			continue
		}
		js := sessionJSON(row, st.Now)
		if _, gone := expired[row.Key]; js.NeedsYou && !js.NeedsYouDismissed && !gone {
			c.SessionsNeedingYou++
			if !js.NeedsYouSince.IsZero() && (c.OldestNeedsYouSince.IsZero() || js.NeedsYouSince.Before(c.OldestNeedsYouSince)) {
				c.OldestNeedsYouSince = js.NeedsYouSince
			}
		}
	}
	c.Worktrees = len(wts)
	for _, p := range prs {
		if p.State != store.PROpen {
			continue
		}
		c.OpenPRs++
		if mon.PRWait(p).NeedsYou != "" {
			c.PRsNeedingYou++
		}
	}
	st.Counts = c
	return st
}
