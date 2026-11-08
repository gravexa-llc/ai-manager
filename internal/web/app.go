package web

// The web app's shared shell: the three-pane app served under /app/,
// which is the dashboard (GET / and the old pages' URLs redirect here;
// redirects.go). internal/web/APP.md is the guide for building its pages;
// this file is the shell they all share.
//
// # Pages
//
// Each page is an appPage, defined in its own app_<page>.go with its own
// template in templates/app/<page>.html. The shell (templates/app/shell.html)
// draws the menu, the header and either one content area (Panes 1: the
// "content" block) or a list and a detail pane (Panes 2: the "list" and
// "detail" blocks).
//
// # Fragments
//
// With ?fragment=<id> a page renders one region alone: "menu", "summary"
// (the header's title and summary), "list" and "detail" or "content", or
// one of the page's own fragments. The script (static/app-shell.js) swaps
// it into the element with that id, for pane links and forms (data-pane)
// and for live refresh from /api/stream (data-live). Every fragment answer
// carries a fresh CSRF token in X-AIM-CSRF, so a page left open for days
// keeps posting.
//
// # The toast protocol
//
// A page's forms POST to routes under /app/ (CSRF.Protect checks them). The
// handler answers with appReply or appRefuse:
//
//   - to the script, which sends Accept: application/json, with JSON
//     {"location": "/app/…", "toast": {"text", "kind", "undo": {"action",
//     "fields", "label"}}} (an appAnswer), or {"error": "…"} with a 4xx or
//     5xx status. The script moves to location, refreshes the panes the
//     form names and the menu, and shows the toast; its Undo button posts
//     undo.fields to undo.action.
//   - to a plain form post, with a 303 to location and the toast sealed in
//     ?toast= (signed with the CSRF key, so a link can't put words on the
//     page), which the page shows with its Undo as a form.

import (
	"bytes"
	"cmp"
	"context"
	"crypto/hmac"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"maps"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/store"
)

// appTemplates are the app's templates: the shell and one file per page.
//
//go:embed templates/app/*.html
var appTemplates embed.FS

// parseApp parses the shell and one page's template file. The files are
// embedded, so an error is a bug the tests catch.
func parseApp(file string) *template.Template {
	return template.Must(template.New(file).Funcs(templateFuncs).ParseFS(appTemplates,
		"templates/app/shell.html", "templates/app/"+file))
}

// appPage is one page of the app.
type appPage struct {
	// pattern is its GET route, e.g. "/app/inbox".
	pattern string
	// name is its menu entry: overview, inbox, plans, sessions,
	// worktrees, prs or settings.
	name string
	// title is the header's default title; fill may change it.
	title string
	// panes is 1 (a single content area) or 2 (a list and a detail pane).
	panes int
	// listLabel and detailLabel name the two panes for screen readers.
	listLabel, detailLabel string
	// topics are the /api/stream topics its panes and summary show: a
	// change to one refreshes them.
	topics []string
	// fragments are the page's own live regions, besides the shell's.
	fragments []string
	tmpl      *template.Template
	// fill builds the page's data: d.View, and d.Title or d.Summary when
	// they depend on it. d.Fragment says which region is wanted ("" for the
	// whole page), so a fill may skip work the region doesn't show. An
	// errNoPage answers 404.
	fill func(s *Server, r *http.Request, sn *snapshot, d *appData) error
	// routes, if set, adds the page's other routes (its form posts).
	routes func(s *Server)
	// keys are the page's own keys (its data-key elements), as the ?
	// sheet lists them, in the page's words: g is Go on one page and Open
	// on GitHub on another.
	keys []appKey
}

// appKey is one line of the ? sheet: a key and what it does.
type appKey struct {
	Key, Label string
}

// undoKey is the ? sheet's u on a page that doesn't bind it: the toast's
// Undo, which the shell binds.
var undoKey = appKey{"u", "Undo the last change, while its toast shows"}

// helpKeys are a page's lines for the ? sheet: its own keys, and the
// toast's u unless the page says what u does.
func helpKeys(keys []appKey) []appKey {
	out := slices.Clone(keys)
	if !slices.ContainsFunc(keys, func(k appKey) bool { return k.Key == "u" }) {
		out = append(out, undoKey)
	}
	return out
}

// appPages are the app's pages, in menu order.
var appPages = []*appPage{appOverview, appInbox, appPlan, appSessions, appWorktrees, appPRs, appSettings}

// shellFragments are the regions every page has.
var shellFragments = []string{"menu", "summary"}

// hasFragment reports whether ?fragment=id names one of p's regions.
func (p *appPage) hasFragment(id string) bool {
	switch {
	case slices.Contains(shellFragments, id), slices.Contains(p.fragments, id):
		return true
	case id == "list" || id == "detail":
		return p.panes == 2
	case id == "content":
		return p.panes != 2
	}
	return false
}

// menuTopics are the topics the menu's counts come from.
const menuTopics = "sessions prs worktrees plans inbox"

// appData is what every app template gets.
type appData struct {
	// Page is the menu entry to highlight (appPage.name); PlanSlug, on a
	// plan's page, the plan's.
	Page     string
	PlanSlug string
	Title    string
	Summary  string
	Panes    int
	// ListLabel and DetailLabel name the panes (Panes 2).
	ListLabel, DetailLabel string
	// Topics are the page's live topics, space-separated (data-live).
	Topics     string
	MenuTopics string
	// Fragment is ?fragment=, or "" for the whole page.
	Fragment string
	// Path is the page's path, Return its path and query without
	// ?fragment= or ?toast=, and Query the search box's ?q=.
	Path, Return, Query string
	CSRF                string
	AssetVersion        string
	// Menu and TabTitle are filled for the whole page and the menu
	// fragment only.
	Menu     appMenu
	TabTitle string
	Flags    appFlags
	// Keys are the page's lines of the ? sheet (helpKeys).
	Keys []appKey
	// Toast is a form's answer, from ?toast= (no script).
	Toast *toastView
	// View is the page's own data, set by its fill; its template reads
	// it as .View.
	View any
}

// appFlags are the alert, keyboard and theme settings the shell obeys,
// from the stored settings (appSettingsFlags). The page draws them
// on <html>, and the menu carries them in data-flags, so an open tab
// follows a change at its next menu refresh.
type appFlags struct {
	// TabCount puts the needs-you count in the tab title.
	TabCount bool
	// Chime lets the script ring for new [data-chime] elements.
	Chime bool
	// Keys turns the keyboard shortcuts on.
	Keys bool
	// Theme is store.ThemeSystem, ThemeDark or ThemeLight.
	Theme string
}

// ForcedTheme is <html>'s data-theme: "dark" or "light" when the Theme
// setting forces one, "" for System, where the stylesheet's media query
// follows the Mac's appearance.
func (f appFlags) ForcedTheme() string {
	if f.Theme == store.ThemeDark || f.Theme == store.ThemeLight {
		return f.Theme
	}
	return ""
}

// ColorScheme is the page's color-scheme meta tag, so the browser paints
// the theme's background and controls before the stylesheet loads.
func (f appFlags) ColorScheme() string {
	return cmp.Or(f.ForcedTheme(), "light dark")
}

// ThemeFlag is the menu's data-theme: the setting, System when unset.
func (f appFlags) ThemeFlag() string {
	return cmp.Or(f.ForcedTheme(), store.ThemeSystem)
}

// appMenu is the menu's data.
type appMenu struct {
	// NeedsYou is the Needs you badge and the tab title's count
	// (inboxCount).
	NeedsYou int
	// Plans are the registered plans; PlansNote says why there are none.
	Plans     []appMenuPlan
	PlansNote string
	// Sessions counts the monitored live sessions (sessionCounts).
	Sessions int
	// SafeToRemove counts the worktrees safe to remove (0: no badge).
	SafeToRemove int
	// PRs counts the open PRs (openPRCount).
	PRs int
}

// appMenuPlan is one plan's menu entry.
type appMenuPlan struct {
	Slug, Title, URL string
	// NeedsYou counts its packages waiting on you.
	NeedsYou int
	// On is true on this plan's page.
	On bool
}

// appPlanURL is a plan's page in the app.
func appPlanURL(slug string) string { return "/app/plans/" + url.PathEscape(slug) }

// routeApp adds the app's routes.
func (s *Server) routeApp() {
	s.mux.HandleFunc("GET /app", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/app/", http.StatusMovedPermanently)
	})
	for _, p := range appPages {
		pattern := p.pattern
		if pattern == "/app/" {
			pattern = "/app/{$}"
		}
		s.mux.HandleFunc("GET "+pattern, s.appHandler(p))
		if p.routes != nil {
			p.routes(s)
		}
	}
}

// appHandler returns the handler for one page: the whole page, or with
// ?fragment=<id> one region.
func (s *Server) appHandler(p *appPage) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		frag := q.Get("fragment")
		if frag != "" && !p.hasFragment(frag) {
			http.Error(w, "no such fragment", http.StatusNotFound)
			return
		}
		snap, err := s.load(r.Context())
		if err != nil {
			s.failPage(w, r, err)
			return
		}
		d := &appData{
			Page:         p.name,
			Title:        p.title,
			Panes:        cmp.Or(p.panes, 1),
			ListLabel:    p.listLabel,
			DetailLabel:  p.detailLabel,
			Topics:       strings.Join(p.topics, " "),
			MenuTopics:   menuTopics,
			Fragment:     frag,
			Path:         r.URL.Path,
			Return:       appReturnTarget(r.URL),
			Query:        q.Get("q"),
			CSRF:         s.csrf.Token(),
			AssetVersion: assetVersion,
			Flags:        s.appSettingsFlags(r.Context()),
			Keys:         helpKeys(p.keys),
		}
		if t := s.openToast(q.Get("toast")); t != nil && frag == "" {
			d.Toast = &toastView{appToast: *t, CSRF: d.CSRF, Close: d.Return}
		}
		if err := p.fill(s, r, snap, d); err != nil {
			var missing errNoPage
			if errors.As(err, &missing) {
				http.Error(w, "aimd: "+missing.msg, http.StatusNotFound)
				return
			}
			s.failPage(w, r, err)
			return
		}
		if frag == "" || frag == "menu" {
			d.Menu = s.appMenuOf(r.Context(), snap, d)
			d.TabTitle = tabTitle(d.Menu.NeedsYou, d.Flags.TabCount)
		}
		name := "shell"
		if frag != "" {
			name = frag
		}
		var buf bytes.Buffer
		if err := p.tmpl.ExecuteTemplate(&buf, name, d); err != nil {
			s.failPage(w, r, err)
			return
		}
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Cache-Control", "no-store")
		h.Set(appCSRFHeader, d.CSRF)
		_, _ = w.Write(buf.Bytes())
	}
}

// appCSRFHeader carries a fresh CSRF token on every app page answer.
const appCSRFHeader = "X-AIM-CSRF"

// tabTitle is the browser tab's title: "(N) aim" while N things need you,
// with the count turned on.
func tabTitle(n int, count bool) string {
	if !count || n <= 0 {
		return "aim"
	}
	return "(" + strconv.Itoa(n) + ") aim"
}

// appMenuOf builds the menu. Each count comes from its page's file
// (inboxCount, planMenu, sessionCounts, safeToRemoveCount, openPRCount),
// so the package that builds a page owns its badge.
func (s *Server) appMenuOf(ctx context.Context, sn *snapshot, d *appData) appMenu {
	m := appMenu{
		NeedsYou:     s.inboxCount(ctx, sn),
		SafeToRemove: s.safeToRemoveCount(ctx, sn),
		PRs:          openPRCount(sn),
	}
	m.Sessions, _ = sessionCounts(sn)
	m.Plans, m.PlansNote = s.planMenu(ctx)
	for i := range m.Plans {
		m.Plans[i].On = d.Page == appPlan.name && strings.EqualFold(m.Plans[i].Slug, d.PlanSlug)
	}
	return m
}

// appReturnTarget is a page's own path and query without ?fragment=,
// ?toast= or ?flash=.
func appReturnTarget(u *url.URL) string {
	q := u.Query()
	q.Del("fragment")
	q.Del("toast")
	q.Del("flash")
	if len(q) == 0 {
		return u.Path
	}
	return u.Path + "?" + q.Encode()
}

// appLocation is where a form's answer sends the browser: v when it is a
// path in the app ("/app/…", with its query and anchor), else "/app/".
func appLocation(v string) string {
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" ||
		strings.HasPrefix(v, "//") || strings.Contains(v, `\`) {
		return "/app/"
	}
	p := path.Clean(u.Path)
	if p != "/app" && !strings.HasPrefix(p, "/app/") {
		return "/app/"
	}
	if strings.HasSuffix(u.Path, "/") && !strings.HasSuffix(p, "/") {
		p += "/"
	}
	out := &url.URL{Path: p, Fragment: u.Fragment}
	q := u.Query()
	q.Del("fragment")
	q.Del("toast")
	q.Del("flash")
	out.RawQuery = q.Encode()
	return out.String()
}

// appReturn is a form's return field as an app location (appLocation).
func appReturn(r *http.Request) string { return appLocation(r.PostFormValue("return")) }

// ---- Toasts ----

// appToast is a message a page shows after a form (the toast protocol in
// the file comment and APP.md).
type appToast struct {
	Text string `json:"text"`
	// Kind is "" (ok) or "error".
	Kind string `json:"kind,omitempty"`
	// Undo, if set, puts an Undo button on the toast.
	Undo *appUndo `json:"undo,omitempty"`
}

// appUndo is a toast's Undo button: it posts Fields to Action, a POST
// route under /app/, with the page's CSRF token.
type appUndo struct {
	Action string            `json:"action"`
	Fields map[string]string `json:"fields,omitempty"`
	// Label is the button's text; "Undo" if empty.
	Label string `json:"label,omitempty"`
}

// toastView is a toast as the shell renders it without the script: its
// Undo is a form, which needs the page's CSRF token, and its close button
// a link back to the page without ?toast=.
type toastView struct {
	appToast
	CSRF, Close string
}

// appAnswer is the JSON a page's script gets back from a form post.
type appAnswer struct {
	Location string    `json:"location,omitempty"`
	Toast    *appToast `json:"toast,omitempty"`
	Error    string    `json:"error,omitempty"`
}

// wantsJSON reports whether the request is the app's script, which asks
// for JSON answers.
func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

// appReply answers a successful form post: JSON for the script, else a
// 303 to loc (an app location) with the toast in ?toast=. t may be nil.
func (s *Server) appReply(w http.ResponseWriter, r *http.Request, loc string, t *appToast) {
	loc = appLocation(loc)
	if t != nil {
		t = s.boundToast(*t)
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, appAnswer{Location: loc, Toast: t})
		return
	}
	if t != nil {
		loc = withQuery(loc, "toast", s.sealToast(*t))
	}
	http.Redirect(w, r, loc, http.StatusSeeOther)
}

// appRefuse answers a form post that failed: JSON {"error": msg} with
// status for the script, else a 303 to loc with msg as an error toast.
func (s *Server) appRefuse(w http.ResponseWriter, r *http.Request, status int, loc, msg string) {
	loc = appLocation(loc)
	if wantsJSON(r) {
		writeJSON(w, status, appAnswer{Location: loc, Error: msg})
		return
	}
	t := s.boundToast(appToast{Text: msg, Kind: "error"})
	http.Redirect(w, r, withQuery(loc, "toast", s.sealToast(*t)), http.StatusSeeOther)
}

// withQuery sets one query parameter on an app location, keeping its
// anchor.
func withQuery(loc, key, value string) string {
	u, err := url.Parse(loc)
	if err != nil {
		return loc
	}
	q := u.Query()
	q.Set(key, value)
	u.RawQuery = q.Encode()
	return u.String()
}

// Toast bounds: a sealed toast rides in a redirect's URL.
const (
	maxToastText   = 300
	maxToastFields = 8
	maxToastField  = 200
	toastMaxAge    = time.Hour
)

// boundToast trims t to the bounds, and drops an Undo that doesn't post to
// the app or carries too much (a bug in the page, so it is logged).
func (s *Server) boundToast(t appToast) *appToast {
	t.Text = cut(t.Text, maxToastText)
	if t.Kind != "error" {
		t.Kind = ""
	}
	if t.Undo == nil {
		return &t
	}
	u := *t.Undo
	t.Undo = nil
	fit := appLocation(u.Action) == u.Action && len(u.Fields) <= maxToastFields
	for k, v := range u.Fields {
		fit = fit && len(k) <= maxToastField && len(v) <= maxToastField
	}
	if !fit {
		s.log.Error("toast undo dropped: not an app path, or too many or too long fields", "action", u.Action)
		return &t
	}
	u.Fields = maps.Clone(u.Fields)
	u.Label = cut(u.Label, 40)
	t.Undo = &u
	return &t
}

// cut shortens s to at most n bytes, on a character boundary, with "…"
// when it cut.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}

// sealedToast is a toast in ?toast=, with the time it was sealed.
type sealedToast struct {
	appToast
	At int64 `json:"at"`
}

// sealToast encodes t for ?toast=, signed with the CSRF key.
func (s *Server) sealToast(t appToast) string {
	body, _ := json.Marshal(sealedToast{appToast: t, At: s.now().Unix()})
	return base64.RawURLEncoding.EncodeToString(body) + "." + base64.RawURLEncoding.EncodeToString(s.csrf.mac(append([]byte("toast\x00"), body...)))
}

// openToast decodes a ?toast= value, or returns nil when it is missing,
// forged or over an hour old.
func (s *Server) openToast(v string) *appToast {
	if v == "" || len(v) > 8192 {
		return nil
	}
	b64, sig64, ok := strings.Cut(v, ".")
	if !ok {
		return nil
	}
	body, err1 := base64.RawURLEncoding.DecodeString(b64)
	sig, err2 := base64.RawURLEncoding.DecodeString(sig64)
	if err1 != nil || err2 != nil || !hmac.Equal(sig, s.csrf.mac(append([]byte("toast\x00"), body...))) {
		return nil
	}
	var t sealedToast
	if json.Unmarshal(body, &t) != nil || t.Text == "" {
		return nil
	}
	if age := s.now().Sub(time.Unix(t.At, 0)); age < -time.Minute || age > toastMaxAge {
		return nil
	}
	return s.boundToast(t.appToast)
}
