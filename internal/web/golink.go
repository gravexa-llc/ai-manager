package web

// Go (`aim go` and the dashboard's Go buttons): POST /api/go
// brings a session to the front in iTerm2 or Terminal. The logic is
// internal/focus; this is its HTTP side, and the rules for the dashboard's
// Go buttons. The contract is in docs/api.md.
//
// POST /api/go takes {"key": "..."} with X-AIM-Token (`aim go`). The
// answer is an api.GoResult, or an api.GoError with the refusal's status:
// 400 no key, 404 no such live session, 409 an ambiguous key (with the
// candidates), 422 the session can't be reached (no terminal, in no iTerm2
// or Terminal tab, …), 502 or 504 osascript failed or timed out.
//
// The app's pages post their own Go forms under /app/ (the inbox, the
// Overview, Sessions, a plan's package), which call goTo and answer with
// the toast protocol (app.go). A Go button is enabled only where Go can
// switch to an existing tab: an interactive session, not
// headless, that runs in iTerm2 or Terminal.app by its process tree
// (internal/term, read by the daemon on each session poll). Everywhere
// else the button is disabled, with the reason as its title (goOff). Each
// session also shows a tag with its terminal app.

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/gravexa-llc/ai-manager/internal/focus"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/term"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// GoService is Go's logic (*focus.Service). An error with an HTTPStatus()
// int method is a refusal, answered with that status; one that also has a
// GoCandidates() method lists the sessions an ambiguous key matched.
type GoService interface {
	Go(ctx context.Context, key string) (api.GoResult, error)
}

var _ GoService = (*focus.Service)(nil)

// postGo is POST /api/go.
func (s *Server) postGo(w http.ResponseWriter, r *http.Request) {
	var req api.GoRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	res, err := s.goTo(r, req.Key)
	if err != nil {
		var refusal interface{ HTTPStatus() int }
		if !errors.As(err, &refusal) {
			writeError(w, http.StatusInternalServerError, "aimd failed: "+err.Error())
			return
		}
		body := api.GoError{Error: err.Error()}
		var amb interface{ GoCandidates() []api.GoCandidate }
		if errors.As(err, &amb) {
			body.Candidates = amb.GoCandidates()
		}
		writeJSON(w, refusal.HTTPStatus(), body)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// goTo calls the Go service and logs what it did (keys only: names and
// cwds stay out of the log).
func (s *Server) goTo(r *http.Request, key string) (api.GoResult, error) {
	res, err := s.opts.Go.Go(r.Context(), key)
	var refusal interface{ HTTPStatus() int }
	switch {
	case err == nil:
		s.log.Info("go", "key", res.Key, "action", res.Action, "via", res.Via, "agent", res.AgentID)
	case errors.As(err, &refusal):
		s.log.Info("go refused", "status", refusal.HTTPStatus(), "err", err)
	default:
		s.log.Error("go", "err", err)
	}
	return res, err
}

// goButton is a Go button: enabled, posting Key, or disabled with the
// reason Off as its title. The zero value is no button at all, for when
// Go is off (Options.Go is nil) or there is nothing live to go to.
type goButton struct {
	Key string
	Off string
}

// Why the dashboard doesn't offer Go: the disabled button's title.
const (
	goOffHeadless = "Headless `claude -p` run; Go only switches to interactive sessions."
	goOffTmux     = "Runs inside tmux; Go can only switch to iTerm2 and Terminal tabs."
	goOffNone     = "No terminal."
)

func goOffBackground(jobID string) string {
	const s = "Background session: no terminal tab to switch to."
	if jobID == "" {
		return s
	}
	return s + " Open it with `claude attach " + jobID + "`."
}

func goOffOther(app string) string {
	if app == "" {
		app = "another app"
	}
	return "Runs in " + app + "; Go can only switch to iTerm2 and Terminal tabs."
}

// goOff says why the dashboard doesn't offer Go for a live session, or ""
// when it does: for an interactive session, not headless, whose terminal
// is iTerm2 or Terminal.app, the apps whose tabs Go switches to. A
// background session has no tab: Go would open a new one with `claude
// attach` (POST /api/go and `aim go` still do), which shows a blank tab
// when the job's transcript is gone.
func goOff(row store.Session, app term.App) string {
	switch {
	case row.Kind == focus.KindBackground:
		return goOffBackground(row.JobID)
	case row.Headless:
		return goOffHeadless
	case app.Switchable():
		return ""
	case app.Kind == term.KindTmux:
		return goOffTmux
	case app.Kind == term.KindOther:
		return goOffOther(app.Name)
	}
	return goOffNone
}

// goButtonOf is a live session row's Go button.
func (s *Server) goButtonOf(row store.Session, app term.App) goButton {
	if s.opts.Go == nil {
		return goButton{}
	}
	if off := goOff(row, app); off != "" {
		return goButton{Off: off}
	}
	return goButton{Key: row.Key}
}

// goButtonOfAgent is a plan card's agent link's Go button, or none when
// the agent isn't live. Go goes to the session the link names (a
// subagent's lead), so the button follows that session's rules.
func (s *Server) goButtonOfAgent(sn *snapshot, a *api.LinkAgent) goButton {
	key := s.goKeyOfAgent(a)
	switch {
	case key == "":
		return goButton{}
	case a.JobID != "":
		return goButton{Off: goOffBackground(a.JobID)}
	}
	row, ok := liveSessionByID(sn.sessions, a.SessionID)
	if !ok {
		return goButton{Off: goOffNone}
	}
	if off := goOff(row, appOf(sn.terms, row)); off != "" {
		return goButton{Off: off}
	}
	return goButton{Key: key}
}

// goKeyOfAgent is the key a Go button posts for a plan card's agent link,
// or "" when Go is off or the agent isn't live: a background job by its
// job id, a subagent by its link ref (<session id>/<agent id>, which Go
// takes to the lead), a session by its id.
func (s *Server) goKeyOfAgent(a *api.LinkAgent) string {
	if s.opts.Go == nil || a == nil || !a.Live {
		return ""
	}
	switch {
	case a.JobID != "":
		return "job:" + a.JobID
	case a.SessionID != "" && a.AgentID != "":
		return a.SessionID + "/" + a.AgentID
	case a.SessionID != "":
		return "session:" + a.SessionID
	}
	return ""
}

// liveSessionByID is the live session with id sid or, after a /clear,
// with that previous id: the one Go resolves an agent link to.
func liveSessionByID(rows []store.Session, sid string) (store.Session, bool) {
	if sid == "" {
		return store.Session{}, false
	}
	for _, match := range []func(store.Session) bool{
		func(r store.Session) bool { return r.SessionID == sid },
		func(r store.Session) bool { return r.PrevSessionID == sid },
	} {
		if i := slices.IndexFunc(rows, match); i >= 0 {
			return rows[i], true
		}
	}
	return store.Session{}, false
}

// ---- Terminal tags (internal/term) ----

// TerminalSource says what terminal app each live session runs in, by
// session key: the daemon's Terminals, from its latest session poll. The
// map it returns is never changed.
type TerminalSource interface {
	Terminals() map[string]term.App
}

// terminals returns the live sessions' terminal apps, or nil when nothing
// says (no Options.Terminals): then every session has none.
func (s *Server) terminals() map[string]term.App {
	if s.opts.Terminals == nil {
		return nil
	}
	return s.opts.Terminals.Terminals()
}

// appOf is the terminal app a session row runs in: none for a background
// session, or one the latest session poll didn't see (such as a session
// the mod reported first).
func appOf(terms map[string]term.App, row store.Session) term.App {
	if a, ok := terms[row.Key]; ok && a.Kind != "" && row.Kind != focus.KindBackground {
		return a
	}
	return term.NoTerminal
}

// termTag is a session's terminal tag: what app it runs in.
type termTag struct {
	Class, Label, Title string
}

func termTagOf(row store.Session, a term.App) termTag {
	switch a.Kind {
	case term.KindITerm2:
		return termTag{"term-iterm2", "iTerm2", "Runs in an iTerm2 tab"}
	case term.KindTerminal:
		return termTag{"term-terminal", "Terminal", "Runs in a Terminal tab"}
	case term.KindTmux:
		return termTag{"term-tmux", "tmux", "Runs inside tmux"}
	case term.KindOther:
		if a.Name != "" {
			return termTag{"term-other", a.Name, "Runs in " + a.Name}
		}
		return termTag{"term-other", "other app", "Runs in a terminal app other than iTerm2 and Terminal"}
	}
	if row.Kind == focus.KindBackground {
		return termTag{"term-none", "no terminal", "A background session: claude runs it without a terminal"}
	}
	return termTag{"term-none", "no terminal", "No terminal: no process or no tty at the last poll"}
}

// withTerminal sets a session's terminal fields in GET /api/sessions.
func withTerminal(js api.Session, a term.App) api.Session {
	js.Terminal, js.TerminalApp = a.Kind, a.Name
	return js
}
