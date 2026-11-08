package web

// The terminal tags and which Go buttons are enabled.

import (
	"context"
	"html/template"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/term"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// fakeTerms is a TerminalSource with fixed answers.
type fakeTerms map[string]term.App

func (f fakeTerms) Terminals() map[string]term.App { return f }

var (
	appITerm2   = term.App{Kind: term.KindITerm2, Name: term.AppITerm}
	appTerminal = term.App{Kind: term.KindTerminal, Name: term.AppTerminal}
	appTmux     = term.App{Kind: term.KindTmux, Name: "tmux"}
)

// goCase is a live session, the terminal app the daemon says it runs in
// (absent: the daemon didn't say), and what the dashboard should show:
// its tag, and its Go button enabled (off == "") or disabled with off as
// its title.
type goCase struct {
	row      store.Session
	app      *term.App
	tag      string
	tagClass string
	off      string
}

func appP(a term.App) *term.App { return &a }

func goCases() []goCase {
	at := func(r store.Session) store.Session {
		r.LastSeenAt, r.StartedAt = t0, t0.Add(-time.Hour)
		return r
	}
	return []goCase{
		{at(store.Session{Key: "pid:42", SessionID: "s-42", PID: 42, Kind: "interactive", Name: "needy tab", Cwd: "/w/a",
			State: needsYou, StateReason: "permission prompt", NeedsYouSince: t0.Add(-5 * time.Minute)}),
			appP(appITerm2), "iTerm2", "term-iterm2", ""},
		{at(store.Session{Key: "pid:43", SessionID: "s-43", PID: 43, Kind: "interactive", Name: "quiet tab", Cwd: "/w/b", State: "idle"}),
			appP(appTerminal), "Terminal", "term-terminal", ""},
		{at(store.Session{Key: "pid:44", SessionID: "s-44", PID: 44, Kind: "interactive", Name: "headless run", Cwd: "/tmp/p", Headless: true, State: "working"}),
			appP(appITerm2), "iTerm2", "term-iterm2", "Headless `claude -p` run; Go only switches to interactive sessions."},
		// The daemon never says a background session has a terminal; if
		// something did, the dashboard still wouldn't believe it.
		{at(store.Session{Key: "job:022c44bf", JobID: "022c44bf", SessionID: "s-job", PID: 53221, Kind: "background", Name: "blocked job", Cwd: "/w/c",
			State: needsYou, StateReason: "reply", NeedsYouSince: t0.Add(-time.Hour)}),
			appP(appITerm2), "no terminal", "term-none", "Background session: no terminal tab to switch to. Open it with `claude attach 022c44bf`."},
		{at(store.Session{Key: "pid:45", SessionID: "s-45", PID: 45, Kind: "background", Name: "jobless bg", Cwd: "/w/d", State: "idle"}),
			appP(term.NoTerminal), "no terminal", "term-none", "Background session: no terminal tab to switch to."},
		{at(store.Session{Key: "pid:46", SessionID: "s-46", PID: 46, Kind: "interactive", Name: "tmux pane", Cwd: "/w/e",
			State: needsYou, StateReason: "permission prompt", NeedsYouSince: t0.Add(-2 * time.Minute)}),
			appP(appTmux), "tmux", "term-tmux", "Runs inside tmux; Go can only switch to iTerm2 and Terminal tabs."},
		{at(store.Session{Key: "pid:47", SessionID: "s-47", PID: 47, Kind: "interactive", Name: "ghostty tab", Cwd: "/w/f", State: "idle"}),
			appP(term.App{Kind: term.KindOther, Name: "Ghostty"}), "Ghostty", "term-other", "Runs in Ghostty; Go can only switch to iTerm2 and Terminal tabs."},
		{at(store.Session{Key: "pid:48", SessionID: "s-48", PID: 48, Kind: "interactive", Name: "mystery tab", Cwd: "/w/g", State: "idle"}),
			appP(term.App{Kind: term.KindOther}), "other app", "term-other", "Runs in another app; Go can only switch to iTerm2 and Terminal tabs."},
		{at(store.Session{Key: "pid:49", SessionID: "s-49", PID: 49, Kind: "interactive", Name: "no tty", Cwd: "/w/h", State: "idle"}),
			appP(term.NoTerminal), "no terminal", "term-none", "No terminal."},
		// Reported by the mod after the last poll: the daemon hasn't seen it.
		{at(store.Session{Key: "session:s-50", SessionID: "s-50", Kind: "interactive", Name: "mod first", Cwd: "/w/i", State: "idle"}),
			nil, "no terminal", "term-none", "No terminal."},
	}
}

// goWorld stores the cases' sessions and returns the daemon's answers.
func goWorld(t *testing.T) (*store.Store, fakeTerms) {
	t.Helper()
	st := newTestStore(t)
	terms := fakeTerms{}
	for _, c := range goCases() {
		must(t, st.UpsertSessions(context.Background(), c.row))
		if c.app != nil {
			terms[c.row.Key] = *c.app
		}
	}
	return st, terms
}

func termServer(t *testing.T, g GoService, st Store, terms TerminalSource) *Server {
	t.Helper()
	return New(Options{Port: testPort, Token: testToken, Store: st, Go: g, Terminals: terms, Now: func() time.Time { return t0 }})
}

// attr is s escaped as html/template escapes an attribute value.
func attr(t *testing.T, s string) string {
	t.Helper()
	var b strings.Builder
	must(t, template.Must(template.New("").Parse(`<a title="{{.}}">`)).Execute(&b, s))
	v := strings.TrimPrefix(b.String(), `<a title="`)
	return strings.TrimSuffix(v, `">`)
}

// Every session shows its terminal app, and a Go button that is enabled
// only for an interactive session in iTerm2 or Terminal: on the Sessions
// page's detail (headless runs aren't listed there).
func TestTerminalTagsAndGoButtons(t *testing.T) {
	st, terms := goWorld(t)
	h := termServer(t, &stubGo{}, st, terms).Handler()
	for _, c := range goCases() {
		if c.row.Headless {
			continue
		}
		where := "detail of " + c.row.Name
		d := get(t, h, "/app/sessions?"+url.Values{"s": {c.row.Key}, "fragment": {"detail"}}.Encode())
		wantIn(t, where, d, `<span class="tag `+c.tagClass+`" title="`, `">`+c.tag+`</span>`)
		if c.off == "" {
			wantIn(t, where, d, `action="/app/sessions/go"`, `name="key" value="`+c.row.Key+`"`)
			wantNotIn(t, where, d, "disabled data-key=\"g\"")
			continue
		}
		wantIn(t, where, d, `data-off="`+attr(t, c.off)+`"`)
		wantNotIn(t, where, d, `action="/app/sessions/go"`)
	}

	// Without a terminal source every session has none, and no Go.
	unknown := get(t, termServer(t, &stubGo{}, st, nil).Handler(), "/app/sessions?"+url.Values{"s": {"pid:43"}, "fragment": {"detail"}}.Encode())
	wantNotIn(t, "no terminal source", unknown, `action="/app/sessions/go"`, "term-iterm2")
	wantIn(t, "no terminal source", unknown, attr(t, "No terminal."))
}

func TestSessionsAPITerminal(t *testing.T) {
	st, terms := goWorld(t)
	var body api.Sessions
	decode(t, do(t, termServer(t, nil, st, terms).Handler(), "GET", "/api/sessions?all=1", nil, nil), &body)
	got := map[string][2]string{}
	for _, s := range body.Sessions {
		got[s.Key] = [2]string{s.Terminal, s.TerminalApp}
	}
	for key, want := range map[string][2]string{
		"pid:42":       {"iterm2", "iTerm2"},
		"pid:43":       {"terminal", "Terminal"},
		"pid:44":       {"iterm2", "iTerm2"}, // headless, but the API says where it runs
		"job:022c44bf": {"none", ""},
		"pid:45":       {"none", ""},
		"pid:46":       {"tmux", "tmux"},
		"pid:47":       {"other", "Ghostty"},
		"pid:48":       {"other", ""},
		"pid:49":       {"none", ""},
		"session:s-50": {"none", ""},
	} {
		if got[key] != want {
			t.Errorf("%s: terminal %q, terminal_app %q; want %q, %q", key, got[key][0], got[key][1], want[0], want[1])
		}
	}

	// terminal is always there; terminal_app only when there is a name.
	rec := do(t, termServer(t, nil, st, nil).Handler(), "GET", "/api/sessions", nil, nil)
	raw := rec.Body.String()
	if n := strings.Count(raw, `"terminal":"none"`); n != 9 {
		t.Errorf("without a terminal source, %d sessions say none, want 9 (all but the headless one): %s", n, raw)
	}
	wantNotIn(t, "without a terminal source", raw, "terminal_app")
}

// Every reason, as the brief words it.
func TestGoOff(t *testing.T) {
	inTab := store.Session{Kind: "interactive", PID: 1}
	for _, c := range []struct {
		row  store.Session
		app  term.App
		want string
	}{
		{inTab, appITerm2, ""},
		{inTab, appTerminal, ""},
		{store.Session{PID: 1}, appTerminal, ""}, // kind not listed: the terminal decides
		{store.Session{Kind: "background", JobID: "0a1b2c3d", PID: 1}, appITerm2, "Background session: no terminal tab to switch to. Open it with `claude attach 0a1b2c3d`."},
		{store.Session{Kind: "background"}, term.NoTerminal, "Background session: no terminal tab to switch to."},
		{store.Session{Kind: "interactive", PID: 1, Headless: true}, appTerminal, "Headless `claude -p` run; Go only switches to interactive sessions."},
		{inTab, appTmux, "Runs inside tmux; Go can only switch to iTerm2 and Terminal tabs."},
		{inTab, term.App{Kind: term.KindOther, Name: "Visual Studio Code"}, "Runs in Visual Studio Code; Go can only switch to iTerm2 and Terminal tabs."},
		{inTab, term.App{Kind: term.KindOther}, "Runs in another app; Go can only switch to iTerm2 and Terminal tabs."},
		{inTab, term.NoTerminal, "No terminal."},
		{inTab, term.App{}, "No terminal."},
	} {
		if got := goOff(c.row, c.app); got != c.want {
			t.Errorf("goOff(%+v, %+v) = %q, want %q", c.row, c.app, got, c.want)
		}
	}
}
