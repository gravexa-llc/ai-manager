// Package focus is Go (`aim go` and the dashboard's Go buttons): it
// resolves what you typed or clicked to one live session and
// brings that session to the front in iTerm2 or Terminal.app
// (internal/term).
//
//   - An interactive session: its tab is selected, found by the tty of
//     its pid in iTerm2, then in Terminal.app (or by its ITERM_SESSION_ID
//     in iTerm2, when the mod reported one), and that app is activated.
//   - A background session: `claude attach <id>` in a new iTerm2 tab, or a
//     new Terminal.app window when iTerm2 isn't running.
//   - A subagent: it runs inside its lead session, and selecting it there
//     needs the mod ($.ui.view({ agentId })), so Go goes to the lead and
//     says so.
//
// The same code serves aimd's POST /api/go (over the store, StoreSource)
// and `aim go` when aimd isn't running (over `claude agents --json`,
// ListerSource).
package focus

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/gravexa-llc/ai-manager/internal/osascript"
	"github.com/gravexa-llc/ai-manager/internal/term"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// Kinds, as `claude agents --json` spells them.
const (
	KindInteractive = "interactive"
	KindBackground  = "background"
)

// Target is one live session Go can resolve a key to.
type Target struct {
	// Key is the store's key: "pid:<pid>", "job:<id>" or
	// "session:<sessionId>".
	Key           string
	SessionID     string
	PrevSessionID string
	JobID         string
	PID           int
	Kind          string
	Name          string
	Cwd           string
	// ItermSessionID is the session's ITERM_SESSION_ID, when the mod
	// reported it.
	ItermSessionID string
	// Headless marks a `claude -p` run: only an exact key, pid or id
	// reaches it, never a name or cwd prefix.
	Headless bool
}

// Title names a target: its name, else its cwd's last folder, else its
// key.
func (t Target) Title() string {
	switch {
	case t.Name != "":
		return t.Name
	case t.Cwd != "":
		return filepath.Base(t.Cwd)
	}
	return t.Key
}

// Goable reports whether Go has something to act on for t: a background
// session's job id, or an interactive session's pid or iTerm2 session id.
// The dashboard is stricter: it enables Go only where Go can switch to an
// existing iTerm2 or Terminal tab (internal/web, with internal/term's
// terminal apps).
func (t Target) Goable() bool {
	if t.Kind == KindBackground {
		return t.JobID != ""
	}
	return t.PID > 0 || term.SessionGUID(t.ItermSessionID) != ""
}

func (t Target) candidate() api.GoCandidate {
	return api.GoCandidate{Key: t.Key, Title: t.Title(), Kind: t.Kind, Cwd: t.Cwd}
}

// Lead is where a subagent runs: its lead session's id, or (from a
// worktree lock, without the mod) its lead's pid.
type Lead struct {
	SessionID string
	PID       int
}

// ErrUnknownAgent is returned (wrapped) by Source.LeadOf for an agent it
// knows nothing about.
var ErrUnknownAgent = errors.New("unknown agent")

// Source is what Go resolves keys against.
type Source interface {
	// Targets lists the live sessions.
	Targets(ctx context.Context) ([]Target, error)
	// LeadOf says which session the subagent agentID runs in, or
	// ErrUnknownAgent.
	LeadOf(ctx context.Context, agentID string) (Lead, error)
}

// Terminal focuses and opens sessions (*term.Terminal).
type Terminal interface {
	Focus(ctx context.Context, pid int, itermSessionID string) (term.Focused, error)
	Attach(ctx context.Context, jobID string) (term.Attached, error)
}

var _ Terminal = term.Terminal{}

// Error is a refusal: Go couldn't resolve the key, or the session can't
// be reached. Its HTTPStatus is what POST /api/go answers.
type Error struct {
	Status int
	Msg    string
	// Candidates are the sessions an ambiguous key matched.
	Candidates []api.GoCandidate
	// Err is the cause, if any.
	Err error
}

func (e *Error) Error() string   { return e.Msg }
func (e *Error) HTTPStatus() int { return e.Status }
func (e *Error) Unwrap() error   { return e.Err }

// GoCandidates returns the sessions an ambiguous key matched.
func (e *Error) GoCandidates() []api.GoCandidate { return e.Candidates }

func refuse(status int, cause error, format string, args ...any) *Error {
	return &Error{Status: status, Msg: fmt.Sprintf(format, args...), Err: cause}
}

// Service resolves keys and goes to sessions.
type Service struct {
	Source   Source
	Terminal Terminal
	// HomeDir expands a leading ~/ in a cwd prefix.
	HomeDir string
}

// Go resolves key to a live session and brings it to the front. A
// refusal is an *Error; anything else is aimd's own failure.
func (s *Service) Go(ctx context.Context, key string) (api.GoResult, error) {
	t, agentID, err := s.Resolve(ctx, key)
	if err != nil {
		return api.GoResult{}, err
	}
	res, err := s.open(ctx, t)
	if err != nil {
		return res, err
	}
	if agentID != "" {
		res.AgentID = agentID
		res.Message = fmt.Sprintf("Agent %s runs inside its lead session, and aim can't select it there (that needs the mod). %s Open the agent from the lead.", agentID, res.Message)
	}
	return res, nil
}

// Resolve finds the live session key names, and the subagent it named, if
// any (then the session is its lead).
func (s *Service) Resolve(ctx context.Context, key string) (Target, string, error) {
	q := strings.TrimSpace(key)
	if q == "" {
		return Target{}, "", refuse(http.StatusBadRequest, nil, "no session given")
	}
	targets, err := s.Source.Targets(ctx)
	if err != nil {
		return Target{}, "", err
	}
	if sid, aid, ok := AgentRef(q); ok {
		// Unless sid is a live session's id, q may be a name with a slash.
		if t, err := (Lead{SessionID: sid}).find(targets); err == nil {
			return t, aid, nil
		}
	}
	if aid, ok := AgentID(q); ok {
		lead, err := s.Source.LeadOf(ctx, aid)
		if errors.Is(err, ErrUnknownAgent) {
			return Target{}, aid, refuse(http.StatusNotFound, err, "%v", err)
		}
		if err != nil {
			return Target{}, aid, err
		}
		t, err := lead.find(targets)
		if err != nil {
			return Target{}, aid, refuse(http.StatusNotFound, err, "agent %s's lead session isn't live", aid)
		}
		return t, aid, nil
	}
	t, err := Match(targets, q, s.HomeDir)
	return t, "", err
}

// open brings t to the front.
func (s *Service) open(ctx context.Context, t Target) (api.GoResult, error) {
	res := api.GoResult{Key: t.Key, Title: t.Title()}
	if t.Kind == KindBackground {
		if t.JobID == "" {
			return res, refuse(http.StatusUnprocessableEntity, nil, "%s is a background session without a job id, so aim can't attach to it", res.Title)
		}
		a, err := s.Terminal.Attach(ctx, t.JobID)
		if err != nil {
			return res, terminalError(res.Title, t, err)
		}
		res.Action, res.App, res.Command = api.GoAttached, a.App, a.Command
		where := "a new iTerm2 tab"
		if a.App == term.AppTerminal {
			where = "a new Terminal window"
		}
		res.Message = fmt.Sprintf("Opened %s in %s (claude attach %s).", res.Title, where, t.JobID)
		return res, nil
	}
	if t.PID <= 0 && term.SessionGUID(t.ItermSessionID) == "" {
		return res, refuse(http.StatusUnprocessableEntity, nil, "%s has no pid and no iTerm2 session id, so aim can't find its tab", res.Title)
	}
	f, err := s.Terminal.Focus(ctx, t.PID, t.ItermSessionID)
	if err != nil {
		return res, terminalError(res.Title, t, err)
	}
	res.Action, res.App, res.Via, res.TTY = api.GoFocused, f.App, f.Via, f.TTY
	if f.Via == "tty" {
		res.Message = fmt.Sprintf("Focused %s in %s (%s).", res.Title, f.App, f.TTY)
	} else {
		res.Message = fmt.Sprintf("Focused %s in %s (by its iTerm2 session id).", res.Title, f.App)
	}
	return res, nil
}

// terminalError turns what term or osascript returned into a refusal.
func terminalError(title string, t Target, err error) error {
	unreachable := http.StatusUnprocessableEntity
	switch {
	case errors.Is(err, term.ErrNoTTY):
		return refuse(unreachable, err, "%s (pid %d) has no terminal, so it isn't in an iTerm2 or Terminal tab", title, t.PID)
	case errors.Is(err, term.ErrNoProcess):
		return refuse(unreachable, err, "%s's process (pid %d) is gone", title, t.PID)
	case errors.Is(err, term.ErrNotRunning):
		return refuse(unreachable, err, "neither iTerm2 nor Terminal is running, so %s isn't in a tab of either", title)
	case errors.Is(err, term.ErrNotFound):
		return refuse(unreachable, err, "%s isn't reachable: %v. Is it running in another terminal app?", title, err)
	case errors.Is(err, term.ErrInvalid):
		return refuse(unreachable, err, "aim refused to go to %s: %v", title, err)
	case errors.Is(err, osascript.ErrTimeout):
		return refuse(http.StatusGatewayTimeout, err, "the terminal app didn't answer in time (%v). If macOS asked whether to let aim control iTerm2 or Terminal, allow it and try again", err)
	}
	return refuse(http.StatusBadGateway, err, "couldn't drive the terminal app: %v", err)
}

// find is the live target a Lead names.
func (l Lead) find(targets []Target) (Target, error) {
	if l.SessionID != "" {
		if t, ok := first(targets, func(t Target) bool { return t.SessionID == l.SessionID }); ok {
			return t, nil
		}
		if t, ok := first(targets, func(t Target) bool { return t.PrevSessionID == l.SessionID }); ok {
			return t, nil
		}
		return Target{}, fmt.Errorf("no live session has id %s", l.SessionID)
	}
	if l.PID > 0 {
		if t, ok := first(targets, func(t Target) bool { return t.PID == l.PID }); ok {
			return t, nil
		}
		return Target{}, fmt.Errorf("no live session has pid %d", l.PID)
	}
	return Target{}, errors.New("no lead known")
}

func first(targets []Target, ok func(Target) bool) (Target, bool) {
	i := slices.IndexFunc(targets, ok)
	if i < 0 {
		return Target{}, false
	}
	return targets[i], true
}

// agentIDRE is a subagent's agentId: "a" and 16 hex digits.
var agentIDRE = regexp.MustCompile(`^a[0-9a-f]{16}$`)

// AgentID reads an agent id in the forms aim accepts: agent:<id>,
// agent-<id> (its worktree's folder name) or the bare id.
func AgentID(q string) (string, bool) {
	id := q
	if rest, ok := strings.CutPrefix(q, "agent:"); ok {
		id = rest
	} else if rest, ok := strings.CutPrefix(q, "agent-"); ok {
		id = rest
	}
	if !agentIDRE.MatchString(id) {
		return "", false
	}
	return id, true
}

// AgentRef reads an agent link's ref, <session id>/<agent id>, as the
// plan board stores it.
func AgentRef(q string) (sessionID, agentID string, ok bool) {
	sid, aid, found := strings.Cut(q, "/")
	if !found || sid == "" || aid == "" || strings.ContainsAny(sid, " :~") || strings.ContainsAny(aid, " /") {
		return "", "", false
	}
	return sid, aid, true
}

// Match resolves q among targets, most exact form first:
//
//  1. a store key (pid:123, job:ab12cd34, session:<id>); session:<id>
//     also matches a session whose id (or previous id) is <id>;
//  2. a background job id;
//  3. a pid (all digits);
//  4. a session id (or a previous one);
//  5. a session name, case-insensitively: exactly, else as a prefix; or a
//     prefix of a cwd (~/ expanded with homeDir) or of a cwd's last
//     folder. Headless runs are left out here.
//
// More than one match is a 409 *Error listing them; none is a 404.
func Match(targets []Target, q, homeDir string) (Target, error) {
	if t, ok := first(targets, func(t Target) bool { return t.Key == q }); ok {
		return t, nil
	}
	if sid, ok := strings.CutPrefix(q, "session:"); ok && sid != "" {
		if t, err := (Lead{SessionID: sid}).find(targets); err == nil {
			return t, nil
		}
	}
	if kind, rest, ok := strings.Cut(q, ":"); ok && (kind == "pid" || kind == "job" || kind == "session") {
		return Target{}, refuse(http.StatusNotFound, nil, "no live session has key %s:%s", kind, rest)
	}
	// A job id first: one can be all digits.
	if t, ok := first(targets, func(t Target) bool { return t.JobID != "" && t.JobID == q }); ok {
		return t, nil
	}
	if pid, err := strconv.Atoi(q); err == nil && pid > 0 && isDigits(q) {
		if t, ok := first(targets, func(t Target) bool { return t.PID == pid }); ok {
			return t, nil
		}
		return Target{}, refuse(http.StatusNotFound, nil, "no live session has pid %d", pid)
	}
	if t, err := (Lead{SessionID: q}).find(targets); err == nil {
		return t, nil
	}

	lower := strings.ToLower(q)
	var exact, prefix []Target
	cwdQ := strings.ToLower(expandHome(q, homeDir))
	for _, t := range targets {
		if t.Headless {
			continue
		}
		name := strings.ToLower(t.Name)
		switch {
		case name != "" && name == lower:
			exact = append(exact, t)
		case name != "" && strings.HasPrefix(name, lower),
			t.Cwd != "" && strings.HasPrefix(strings.ToLower(t.Cwd), cwdQ),
			t.Cwd != "" && strings.HasPrefix(strings.ToLower(filepath.Base(t.Cwd)), lower):
			prefix = append(prefix, t)
		}
	}
	for _, set := range [][]Target{exact, prefix} {
		switch len(set) {
		case 0:
			continue
		case 1:
			return set[0], nil
		}
		e := refuse(http.StatusConflict, nil, "%q matches %d sessions; give more of the name or cwd, or a key", q, len(set))
		for _, t := range set {
			e.Candidates = append(e.Candidates, t.candidate())
		}
		return Target{}, e
	}
	return Target{}, refuse(http.StatusNotFound, nil, "no live session matches %q", q)
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// expandHome turns a leading ~/ (or a lone ~) into homeDir.
func expandHome(p, homeDir string) string {
	h := strings.TrimSuffix(homeDir, "/")
	if h == "" {
		return p
	}
	if p == "~" {
		return h
	}
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		return h + "/" + rest
	}
	return p
}
