package web

// Dark mode: the Theme setting and how the shell applies it, and the
// app's colors as tokens (app-shell.css and each page's stylesheet), with
// the contrast and lightness rules they must meet in both themes.

import (
	"context"
	"fmt"
	"io/fs"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// htmlTag is a page's <html …> start tag.
func htmlTag(t *testing.T, page string) string {
	t.Helper()
	i, j := strings.Index(page, "<html "), strings.Index(page, "<head>")
	if i < 0 || j < i {
		t.Fatalf("no <html> tag in %.200s", page)
	}
	return page[i:j]
}

// The theme: System by default, so <html> has no data-theme and the media
// query follows macOS; Dark and Light draw data-theme on every page, so it
// loads in its colors, and the menu carries the setting to open tabs.
func TestSettingTheme(t *testing.T) {
	e := newSettingsEnv(t)
	for _, p := range []string{"/app/", "/app/inbox", "/app/settings"} {
		page := get(t, e.h, p)
		wantNotIn(t, p, htmlTag(t, page), "data-theme")
		wantIn(t, p, page, `<meta name="color-scheme" content="light dark">`)
		wantIn(t, p, section(t, page, "menu"), `data-keys="on" data-theme="system"></span>`)
	}

	// The Appearance card: System / Dark / Light, System pressed, each a
	// button of one form posting the theme.
	content := get(t, e.h, "/app/settings?fragment=content")
	wantIn(t, "appearance", content,
		`<h2 id="set-appearance">Appearance</h2>`,
		`<div class="trow-title" id="seg-theme-title">Theme</div>`,
		"System follows your Mac&#39;s appearance, light or dark, and changes with it.",
		`<form method="post" action="/app/settings/set" data-pane="content" class="seg" role="group" aria-labelledby="seg-theme-title">`,
		`<input type="hidden" name="key" value="theme">`,
		`<button type="submit" id="seg-theme-system" name="value" value="system" class="on" aria-pressed="true">System</button>`,
		`<button type="submit" id="seg-theme-dark" name="value" value="dark" aria-pressed="false">Dark</button>`,
		`<button type="submit" id="seg-theme-light" name="value" value="light" aria-pressed="false">Light</button>`,
	)
	if i, j := strings.Index(content, "seg-theme-system"), strings.Index(content, "seg-theme-dark"); i > j || j > strings.Index(content, "seg-theme-light") {
		t.Error("the themes aren't in the order System, Dark, Light")
	}

	// Each value, as the script posts it: its toast, with an Undo back to
	// what it was, and every page drawn in it.
	for _, c := range []struct{ value, toast, was, attr, scheme string }{
		{store.ThemeDark, "Theme: Dark", store.ThemeSystem, ` data-theme="dark"`, "dark"},
		{store.ThemeLight, "Theme: Light", store.ThemeDark, ` data-theme="light"`, "light"},
		{store.ThemeSystem, "Theme: System, following your Mac", store.ThemeLight, "", "light dark"},
	} {
		a := e.set(store.SettingTheme, c.value)
		if a.Toast == nil || a.Toast.Text != c.toast || a.Toast.Undo == nil || a.Toast.Undo.Action != "/app/settings/set" ||
			a.Toast.Undo.Fields["key"] != "theme" || a.Toast.Undo.Fields["value"] != c.was || a.Toast.Undo.Fields["undo"] != "1" {
			t.Fatalf("%s: answer %+v", c.value, a.Toast)
		}
		if got := e.srv.settings(context.Background()).Theme; got != c.value {
			t.Errorf("%s: stored %q", c.value, got)
		}
		for _, p := range []string{"/app/", "/app/inbox", "/app/plans/sharing", "/app/settings"} {
			page := get(t, e.h, p)
			tag := htmlTag(t, page)
			if c.attr == "" {
				wantNotIn(t, c.value+" "+p, tag, "data-theme")
			} else {
				wantIn(t, c.value+" "+p, tag, c.attr+">")
			}
			wantIn(t, c.value+" "+p, page, `<meta name="color-scheme" content="`+c.scheme+`">`)
		}
		// Open tabs get it with their next menu refresh.
		wantIn(t, c.value+" menu", get(t, e.h, "/app/sessions?fragment=menu"), `data-theme="`+c.value+`"></span>`)
		wantIn(t, c.value+" card", get(t, e.h, "/app/settings?fragment=content"),
			`id="seg-theme-`+c.value+`" name="value" value="`+c.value+`" class="on" aria-pressed="true">`)
	}

	// Undo, as the toast's button posts it: back to Light, with no Undo of
	// its own.
	b := e.answer(e.postSettings("/app/settings/set", url.Values{"key": {"theme"}, "value": {"light"}, "return": {"/app/settings"}, "undo": {"1"}}, true), http.StatusOK)
	if b.Toast == nil || b.Toast.Text != "Theme: Light" || b.Toast.Undo != nil || e.srv.settings(context.Background()).Theme != store.ThemeLight {
		t.Errorf("undo answer %+v", b.Toast)
	}

	// Without the script: a 303 to the page, drawn dark, with the toast and
	// its Undo as a form.
	rec := e.postSettings("/app/settings/set", url.Values{"key": {"theme"}, "value": {"dark"}, "return": {"/app/settings"}}, false)
	loc := rec.Header().Get("Location")
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "/app/settings?toast=") {
		t.Fatalf("form post = %d to %q", rec.Code, loc)
	}
	page := get(t, e.h, loc)
	wantIn(t, "plain form", htmlTag(t, page), ` data-theme="dark">`)
	wantIn(t, "plain form", section(t, page, "toasts"), "<span>Theme: Dark</span>",
		`<input type="hidden" name="key" value="theme">`, `<input type="hidden" name="value" value="light">`, `<input type="hidden" name="undo" value="1">`)

	// A theme it doesn't know is refused, and changes nothing.
	for _, v := range []string{"Dark", "sepia", ""} {
		c := e.answer(e.postSettings("/app/settings/set", url.Values{"key": {"theme"}, "value": {v}}, true), http.StatusBadRequest)
		if c.Error != "Can't save that: theme is system, dark or light." {
			t.Errorf("theme %q: %+v", v, c)
		}
	}
	if got := e.srv.settings(context.Background()).Theme; got != store.ThemeDark {
		t.Errorf("a refused theme stored %q", got)
	}
}

// Without a settings store, the theme is System.
func TestSettingThemeWithoutStore(t *testing.T) {
	st := newTestStore(t)
	seedDash(t, st)
	h := newDash(t, plainStore{st}, func(o *Options) { o.Plans = appPlans }).Handler()
	page := get(t, h, "/app/settings")
	wantNotIn(t, "page", htmlTag(t, page), "data-theme")
	wantIn(t, "page", page, `data-theme="system"></span>`, `id="seg-theme-system" name="value" value="system" class="on"`)
}

func TestSettingThemeAPI(t *testing.T) {
	e := newSettingsEnv(t)
	var got api.Settings
	decode(t, do(t, e.h, "GET", "/api/settings", nil, nil), &got)
	if got.Theme != "system" {
		t.Errorf("default theme %q", got.Theme)
	}
	token := map[string]string{"X-AIM-Token": testToken}
	post := func(body string) (int, api.Settings) {
		rec := do(t, e.h, "POST", "/api/settings", strings.NewReader(body), token)
		var s api.Settings
		if rec.Code == http.StatusOK {
			decode(t, rec, &s)
		}
		return rec.Code, s
	}
	if code, s := post(`{"theme": "dark"}`); code != http.StatusOK || s.Theme != "dark" || !s.Chime {
		t.Errorf("POST dark = %d %+v", code, s)
	}
	wantIn(t, "page", htmlTag(t, get(t, e.h, "/app/")), ` data-theme="dark">`)
	// All or none: a bad theme changes nothing, the chime included.
	for _, body := range []string{`{"chime": false, "theme": "blue"}`, `{"chime": false, "theme": ""}`, `{"theme": 1}`} {
		if code, _ := post(body); code != http.StatusBadRequest {
			t.Errorf("POST %s = %d", body, code)
		}
	}
	if set := e.srv.settings(context.Background()); set.Theme != "dark" || !set.Chime {
		t.Errorf("a refused POST changed the settings: %+v", set)
	}
	// Leaving it out leaves it.
	if code, s := post(`{"chime": false}`); code != http.StatusOK || s.Theme != "dark" {
		t.Errorf("POST without theme = %d %+v", code, s)
	}
	if code, s := post(`{"theme": "system"}`); code != http.StatusOK || s.Theme != "system" {
		t.Errorf("POST system = %d %+v", code, s)
	}
}

// The script copies the menu's data-theme onto <html> after each swap:
// data-theme="dark" or "light", none for System, so the stylesheet's media
// query follows macOS. It runs the script's own flags and setFlag in node
// over a stand-in document. (The color-scheme meta stays as aimd drew it:
// the stylesheet's color-scheme on :root rules once it has loaded.)
func TestAppScriptAppliesTheme(t *testing.T) {
	js := string(staticFiles["app-shell.js"].data)
	wantIn(t, "flags", jsFunc(t, js, "flags"), `setFlag("data-theme", t === "system" ? null : t);`)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node isn't installed")
	}
	harness := `
function el(attrs) {
  return {
    a: Object.assign({}, attrs),
    getAttribute(n) { return n in this.a ? this.a[n] : null; },
    setAttribute(n, v) { this.a[n] = String(v); },
    removeAttribute(n) { delete this.a[n]; },
  };
}
var root = el({"data-assets": "x"});
var menuFlags = el({"data-tab-count": "on", "data-chime": "on", "data-keys": "on"});
var doc = {
  querySelector(s) {
    return s === "#menu [data-flags]" ? menuFlags : null;
  },
  querySelectorAll() { return []; },
};
` + jsFunc(t, js, "flags") + jsFunc(t, js, "setFlag") + `
var out = [];
["dark", "light", "system", "dark", "dark", "system"].forEach(function (v) {
  menuFlags.setAttribute("data-theme", v);
  flags();
  out.push(v + ":" + root.getAttribute("data-theme") + ":" + root.getAttribute("data-keys"));
});
console.log(out.join("|"));
`
	path := filepath.Join(t.TempDir(), "theme.js")
	must(t, os.WriteFile(path, []byte(harness), 0o644))
	b, err := exec.Command(node, path).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, b)
	}
	// The other flags stay as they were (data-keys on is no attribute).
	want := "dark:dark:null|light:light:null|system:null:null|dark:dark:null|dark:dark:null|system:null:null"
	if got := strings.TrimSpace(string(b)); got != want {
		t.Errorf("theme steps:\n got %s\nwant %s", got, want)
	}
}

// ---- The colors are tokens ----

// cssRule is one rule of a stylesheet: its selector, the @media it is in
// ("" for none), and its declarations.
type cssRule struct {
	media, selector string
	decls           []cssDecl
}

type cssDecl struct{ prop, value string }

var cssCommentRE = regexp.MustCompile(`(?s)/\*.*?\*/`)

// parseCSS reads the app's stylesheets: rules, and rules inside @media.
// It is enough for the app's own CSS, which the tests keep plain.
func parseCSS(src string) ([]cssRule, error) {
	return parseCSSRules(cssCommentRE.ReplaceAllString(src, ""), "")
}

func parseCSSRules(s, media string) ([]cssRule, error) {
	var rules []cssRule
	for {
		s = strings.TrimSpace(s)
		if s == "" {
			return rules, nil
		}
		open := strings.IndexByte(s, '{')
		if open < 0 {
			return nil, fmt.Errorf("text outside a rule: %.60q", s)
		}
		depth, end := 0, -1
		for i := open; i < len(s) && end < 0; i++ {
			switch s[i] {
			case '{':
				depth++
			case '}':
				if depth--; depth == 0 {
					end = i
				}
			}
		}
		if end < 0 {
			return nil, fmt.Errorf("unclosed rule %.60q", s[:open])
		}
		prelude, body := strings.Join(strings.Fields(s[:open]), " "), s[open+1:end]
		s = s[end+1:]
		if strings.HasPrefix(prelude, "@") {
			inner, err := parseCSSRules(body, prelude)
			if err != nil {
				return nil, err
			}
			rules = append(rules, inner...)
			continue
		}
		r := cssRule{media: media, selector: prelude}
		for _, d := range splitDecls(body) {
			if strings.TrimSpace(d) == "" {
				continue
			}
			p, v, ok := strings.Cut(d, ":")
			if !ok {
				return nil, fmt.Errorf("%s: not a declaration: %q", prelude, d)
			}
			r.decls = append(r.decls, cssDecl{strings.TrimSpace(p), strings.Join(strings.Fields(v), " ")})
		}
		rules = append(rules, r)
	}
}

// splitDecls splits a rule's body at the semicolons outside quotes
// (content: "; ").
func splitDecls(body string) []string {
	var out []string
	var quote byte
	start := 0
	for i := 0; i < len(body); i++ {
		switch c := body[i]; {
		case quote != 0 && c == '\\':
			i++
		case quote != 0 && c == quote:
			quote = 0
		case quote == 0 && (c == '"' || c == '\''):
			quote = c
		case quote == 0 && c == ';':
			out = append(out, body[start:i])
			start = i + 1
		}
	}
	return append(out, body[start:])
}

// The three blocks that define tokens (app-shell.css's header says why).
const (
	tokensLight      = "light"
	tokensDarkSystem = "dark, for System"
	tokensDark       = "dark, for Dark"
)

// tokenBlock says which token block a rule is, or "".
func tokenBlock(r cssRule) string {
	switch {
	case r.media == "" && r.selector == ":root":
		return tokensLight
	case r.media == "@media (prefers-color-scheme: dark)" && r.selector == `:root:not([data-theme="light"])`:
		return tokensDarkSystem
	case r.media == "" && r.selector == `:root[data-theme="dark"]`:
		return tokensDark
	}
	return ""
}

// appCSS are the app's stylesheets, by name.
func appCSS(t *testing.T) map[string][]cssRule {
	t.Helper()
	out := map[string][]cssRule{}
	for name, f := range staticFiles {
		if !strings.HasSuffix(name, ".css") {
			continue
		}
		rules, err := parseCSS(string(f.data))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out[name] = rules
	}
	if len(out) < 8 {
		t.Fatalf("found %d stylesheets", len(out))
	}
	return out
}

// cssNamedColors are CSS's named colors; transparent and currentColor,
// which no theme changes, are allowed.
var cssNamedColors = strings.Fields(`aliceblue antiquewhite aqua aquamarine azure beige bisque black
blanchedalmond blue blueviolet brown burlywood cadetblue chartreuse chocolate coral cornflowerblue
cornsilk crimson cyan darkblue darkcyan darkgoldenrod darkgray darkgreen darkgrey darkkhaki
darkmagenta darkolivegreen darkorange darkorchid darkred darksalmon darkseagreen darkslateblue
darkslategray darkslategrey darkturquoise darkviolet deeppink deepskyblue dimgray dimgrey dodgerblue
firebrick floralwhite forestgreen fuchsia gainsboro ghostwhite gold goldenrod gray green greenyellow
grey honeydew hotpink indianred indigo ivory khaki lavender lavenderblush lawngreen lemonchiffon
lightblue lightcoral lightcyan lightgoldenrodyellow lightgray lightgreen lightgrey lightpink
lightsalmon lightseagreen lightskyblue lightslategray lightslategrey lightsteelblue lightyellow lime
limegreen linen magenta maroon mediumaquamarine mediumblue mediumorchid mediumpurple mediumseagreen
mediumslateblue mediumspringgreen mediumturquoise mediumvioletred midnightblue mintcream mistyrose
moccasin navajowhite navy oldlace olive olivedrab orange orangered orchid palegoldenrod palegreen
paleturquoise palevioletred papayawhip peru pink plum powderblue purple rebeccapurple red rosybrown
royalblue saddlebrown salmon sandybrown seagreen seashell sienna silver skyblue slateblue slategray
slategrey snow springgreen steelblue tan teal thistle tomato turquoise violet wheat white whitesmoke
yellow yellowgreen canvas canvastext linktext visitedtext activetext buttonface buttontext buttonborder
field fieldtext highlight highlighttext selecteditem selecteditemtext mark marktext graytext accentcolor
accentcolortext`)

var (
	cssHexRE     = regexp.MustCompile(`#[0-9A-Fa-f]{3,8}\b`)
	cssColorFnRE = regexp.MustCompile(`(?i)\b(rgba?|hsla?|hwb|lab|lch|oklab|oklch|color|color-mix|light-dark)\(`)
	cssStringRE  = regexp.MustCompile(`"[^"]*"|'[^']*'`)
	cssVarRE     = regexp.MustCompile(`var\(--[\w-]+`)
	cssWordRE    = regexp.MustCompile(`[A-Za-z][\w-]*`)
)

// rawColor returns the raw color in a CSS value, or "".
func rawColor(v string) string {
	v = cssStringRE.ReplaceAllString(v, "")
	if m := cssHexRE.FindString(v); m != "" {
		return m
	}
	if m := cssColorFnRE.FindString(v); m != "" {
		return m
	}
	for _, w := range cssWordRE.FindAllString(cssVarRE.ReplaceAllString(v, ""), -1) {
		if slices.Contains(cssNamedColors, strings.ToLower(w)) {
			return w
		}
	}
	return ""
}

// svgColorRE finds a color attribute on an element: SVG's paint and its
// color attributes.
var svgColorRE = regexp.MustCompile(`(?i)\s(fill|stroke|stop-color|flood-color|lighting-color|color)\s*=\s*"([^"]*)"`)

// svgColorOK are the attribute values no theme changes.
func svgColorOK(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "none", "currentcolor", "transparent", "inherit":
		return true
	}
	return strings.HasPrefix(v, "url(#")
}

// TestAppColorsAreTokens fails on a raw color in the app's CSS outside the
// token blocks, and on an SVG color attribute in a template or a page:
// every color has a light and a dark value, so it must be a token.
func TestAppColorsAreTokens(t *testing.T) {
	for name, rules := range appCSS(t) {
		for _, r := range rules {
			if tokenBlock(r) != "" {
				continue
			}
			for _, d := range r.decls {
				if c := rawColor(d.value); c != "" {
					t.Errorf("%s: %s { %s: %s }: %s is a raw color; use a token (app-shell.css)", name, r.selector, d.prop, d.value, c)
				}
			}
		}
	}
	check := func(where, html string) {
		t.Helper()
		for _, m := range svgColorRE.FindAllStringSubmatch(html, -1) {
			if !svgColorOK(m[2]) {
				t.Errorf("%s: %s=%q: color SVG with a class and CSS tokens", where, m[1], m[2])
			}
		}
	}
	files, err := fs.Glob(appTemplates, "templates/app/*.html")
	must(t, err)
	for _, f := range files {
		b, err := fs.ReadFile(appTemplates, f)
		must(t, err)
		check(f, string(b))
	}
	// The pages as drawn, the plan's graph and timeline with their boxes,
	// edges and bars.
	pe := newPlanEnv(t)
	for _, p := range []string{"/app/", "/app/inbox", "/app/sessions", "/app/worktrees", "/app/prs", "/app/settings",
		"/app/plans/aim", "/app/plans/aim?view=graph&pkg=E2", "/app/plans/sharing?view=kanban", "/app/plans/sharing?view=timeline"} {
		check(p, get(t, pe.h, p))
	}

	// The rules catch what they should, and let words that aren't colors be.
	for _, v := range []string{"#fff", "1px solid #E3E6EA", "rgba(0, 0, 0, .5)", "hsl(0 0% 0%)", "white", "0 0 0 2px Red", "oklch(70% .1 200)", "light-dark(#fff, #000)"} {
		if rawColor(v) == "" {
			t.Errorf("rawColor misses %q", v)
		}
	}
	for _, v := range []string{"var(--ink)", "nowrap", "pre-wrap", "transparent", "currentColor", `"Segoe UI", system-ui, sans-serif`, "inset 3px 0 0 var(--accent-line)", `"Show \25BE"`, "1px solid var(--line-soft)"} {
		if c := rawColor(v); c != "" {
			t.Errorf("rawColor(%q) = %q", v, c)
		}
	}
	if m := svgColorRE.FindStringSubmatch(`<rect fill="#C2410C">`); m == nil || svgColorOK(m[2]) {
		t.Error("svgColorRE misses a fill")
	}
}

// TestAppThemeBlocks checks each stylesheet's token blocks: the two dark
// blocks are the same, and every light color has a dark one.
func TestAppThemeBlocks(t *testing.T) {
	found := 0
	for name, rules := range appCSS(t) {
		blocks := map[string][]cssDecl{}
		for _, r := range rules {
			if b := tokenBlock(r); b != "" {
				if _, dup := blocks[b]; dup {
					t.Errorf("%s: two %s token blocks", name, b)
				}
				blocks[b] = r.decls
			}
			if strings.Contains(r.media, "prefers-color-scheme") && tokenBlock(r) == "" {
				t.Errorf("%s: %s %s: dark colors go in the token blocks only", name, r.media, r.selector)
			}
		}
		if len(blocks) == 0 {
			continue
		}
		found++
		light, sys, dark := blocks[tokensLight], blocks[tokensDarkSystem], blocks[tokensDark]
		if light == nil || sys == nil || dark == nil {
			t.Errorf("%s: has %d of the 3 token blocks (light, dark for System, dark for Dark)", name, len(blocks))
			continue
		}
		if !slices.Equal(sys, dark) {
			for i := range max(len(sys), len(dark)) {
				var a, b cssDecl
				if i < len(sys) {
					a = sys[i]
				}
				if i < len(dark) {
					b = dark[i]
				}
				if a != b {
					t.Errorf("%s: the dark blocks differ at %d: %s: %s (System) vs %s: %s (Dark)", name, i, a.prop, a.value, b.prop, b.value)
					break
				}
			}
		}
		lightProps := map[string]string{}
		for _, d := range light {
			lightProps[d.prop] = d.value
		}
		darkProps := map[string]bool{}
		for _, d := range dark {
			darkProps[d.prop] = true
			if _, ok := lightProps[d.prop]; !ok {
				t.Errorf("%s: dark %s has no light value", name, d.prop)
			}
			if !strings.HasPrefix(d.prop, "--") && d.prop != "color-scheme" {
				t.Errorf("%s: dark block sets %s; it only defines tokens", name, d.prop)
			}
		}
		for p, v := range lightProps {
			if rawColor(v) != "" && !darkProps[p] {
				t.Errorf("%s: %s (%s) has no dark value", name, p, v)
			}
		}
		if name == "app-shell.css" && (lightProps["color-scheme"] != "light" || !slices.Contains(dark, cssDecl{"color-scheme", "dark"})) {
			t.Errorf("app-shell.css: color-scheme is %q light, and not dark in the dark blocks", lightProps["color-scheme"])
		}
	}
	if found < 2 {
		t.Errorf("token blocks in %d stylesheets, want app-shell.css and app-plan.css", found)
	}
}

// palette resolves the app's tokens in one theme: the light blocks, with
// the dark ones over them if dark.
func palette(t *testing.T, dark bool) map[string]string {
	t.Helper()
	raw := map[string]string{}
	for _, name := range []string{"app-shell.css", "app-plan.css"} {
		for _, r := range appCSS(t)[name] {
			if b := tokenBlock(r); b == tokensLight || (dark && b == tokensDark) {
				for _, d := range r.decls {
					raw[d.prop] = d.value
				}
			}
		}
	}
	var resolve func(v string, depth int) string
	resolve = func(v string, depth int) string {
		if depth > 8 {
			t.Fatalf("token loop at %s", v)
		}
		if name, ok := strings.CutPrefix(v, "var("); ok {
			return resolve(raw[strings.TrimSuffix(name, ")")], depth+1)
		}
		return v
	}
	out := map[string]string{}
	for k, v := range raw {
		out[k] = resolve(v, 0)
	}
	return out
}

// rgbOf reads #RGB or #RRGGBB.
func rgbOf(t *testing.T, hex string) [3]float64 {
	t.Helper()
	h := strings.TrimPrefix(hex, "#")
	if len(h) == 3 {
		h = string([]byte{h[0], h[0], h[1], h[1], h[2], h[2]})
	}
	n, err := strconv.ParseUint(h, 16, 32)
	if len(h) != 6 || err != nil {
		t.Fatalf("not a #RRGGBB color: %q", hex)
	}
	return [3]float64{float64(n>>16) / 255, float64(n>>8&0xFF) / 255, float64(n&0xFF) / 255}
}

// luminance is WCAG's relative luminance.
func luminance(t *testing.T, hex string) float64 {
	c := rgbOf(t, hex)
	for i, v := range c {
		if v <= 0.04045 {
			c[i] = v / 12.92
		} else {
			c[i] = math.Pow((v+0.055)/1.055, 2.4)
		}
	}
	return 0.2126*c[0] + 0.7152*c[1] + 0.0722*c[2]
}

// contrast is WCAG's contrast ratio.
func contrast(t *testing.T, a, b string) float64 {
	la, lb := luminance(t, a), luminance(t, b)
	return (max(la, lb) + 0.05) / (min(la, lb) + 0.05)
}

// lightness is CIE L*, 0 (black) to 100 (white).
func lightness(t *testing.T, hex string) float64 {
	y := luminance(t, hex)
	if y <= 216.0/24389 {
		return y * 24389 / 27
	}
	return 116*math.Cbrt(y) - 16
}

// palettes are the lifecycle, item-kind and tone palettes' token prefixes.
var (
	lifecyclePalettes = []string{"lc-blocked", "lc-ready", "lc-working", "lc-in_review", "lc-done", "lc-dropped"}
	kindPalettes      = []string{"kind-permission", "kind-question", "kind-plan", "kind-merge", "kind-reply"}
	tonePalettes      = []string{"tone-ok", "tone-warn", "tone-info", "tone-lead", "tone-quiet"}
)

// TestAppPaletteContrast checks text against what it sits on, in both
// themes: 4.5:1 for text (WCAG AA), small grey labels included, and 3:1
// for focus rings and the selection cursor.
func TestAppPaletteContrast(t *testing.T) {
	type pair struct {
		fg  string
		bgs []string
		min float64
	}
	text := 4.5
	pairs := []pair{
		{"--ink", []string{"--panel", "--ground", "--ground-2", "--raised", "--hover", "--hover-strong", "--sel", "--new"}, text},
		{"--ink-2", []string{"--panel", "--ground", "--ground-2", "--hover-strong", "--sel"}, text},
		{"--muted", []string{"--panel", "--ground", "--ground-2", "--raised", "--hover", "--sel", "--new", "--accent-soft"}, text},
		{"--placeholder", []string{"--ground"}, text},
		{"--link", []string{"--panel", "--ground", "--ground-2", "--raised", "--sel"}, text},
		{"--link-hover", []string{"--panel", "--ground-2"}, text},
		{"--accent-ink", []string{"--accent-soft", "--ground-2", "--panel"}, text},
		{"--on-accent", []string{"--accent", "--accent-hover"}, text},
		{"--on-badge", []string{"--badge", "--b-review", "--b-lead", "--b-snoozed", "--b-working", "--b-prov"}, text},
		{"--disabled-ink", []string{"--disabled"}, text},
		{"--menu-ink", []string{"--menu", "--menu-hover"}, text},
		{"--menu-strong", []string{"--menu", "--menu-hover", "--menu-on"}, text},
		{"--menu-head", []string{"--menu"}, text},
		{"--menu-mini", []string{"--menu", "--menu-hover", "--menu-on"}, text},
		{"--menu-focus", []string{"--menu"}, text},
		{"--ok-ink", []string{"--panel", "--ground"}, text},
		{"--warn-ink", []string{"--panel", "--warn-ground"}, text},
		{"--bad-ink", []string{"--panel"}, text},
		{"--need-ink", []string{"--panel"}, text},
		{"--code-ink", []string{"--code"}, text},
		{"--code-tool", []string{"--code"}, text},
		{"--toast-ink", []string{"--toast", "--toast-error"}, text},
		{"--accent-line", []string{"--panel", "--ground", "--sel", "--hover"}, 3},
		{"--menu-focus", []string{"--menu-on"}, 3},
		{"--toast-focus", []string{"--toast"}, 3},
	}
	for _, p := range slices.Concat(lifecyclePalettes, kindPalettes, tonePalettes) {
		// Pills (its fill), a kind's label and a package id on rows and
		// panes (panel, a selected row).
		pairs = append(pairs, pair{"--" + p + "-text", []string{"--" + p + "-fill", "--panel", "--sel"}, text})
	}
	// The arithmetic and the tokens' resolution.
	if r := contrast(t, "#FFFFFF", "#000"); math.Abs(r-21) > 1e-9 {
		t.Fatalf("white on black is %v:1", r)
	}
	if l, d := palette(t, false), palette(t, true); l["--fill"] != l["--lc-blocked-fill"] || d["--fill"] != d["--lc-blocked-fill"] ||
		l["--panel"] != "#FFFFFF" || d["--panel"] == l["--panel"] || d["--b-review"] == "" {
		t.Fatalf("palettes: light panel %s fill %s, dark panel %s fill %s", l["--panel"], l["--fill"], d["--panel"], d["--fill"])
	}
	for _, dark := range []bool{false, true} {
		theme := map[bool]string{false: "light", true: "dark"}[dark]
		pal := palette(t, dark)
		for _, p := range pairs {
			for _, bg := range p.bgs {
				fg, b := pal[p.fg], pal[bg]
				if fg == "" || b == "" {
					t.Errorf("%s: no token %s or %s", theme, p.fg, bg)
					continue
				}
				if r := contrast(t, fg, b); r < p.min {
					t.Errorf("%s: %s %s on %s %s is %.2f:1, want %.1f:1", theme, p.fg, fg, bg, b, r, p.min)
				}
			}
		}
	}
}

// TestAppPaletteLightness checks that the lifecycles' bars, and the item
// kinds', differ in lightness and not only in hue (8 L* apart), in both
// themes: the stacked bars, legends, dots and graph boxes stay apart in
// grey and for color-blind eyes.
func TestAppPaletteLightness(t *testing.T) {
	for _, dark := range []bool{false, true} {
		theme := map[bool]string{false: "light", true: "dark"}[dark]
		pal := palette(t, dark)
		for _, set := range [][]string{lifecyclePalettes, kindPalettes} {
			for i, a := range set {
				for _, b := range set[i+1:] {
					la, lb := lightness(t, pal["--"+a+"-bar"]), lightness(t, pal["--"+b+"-bar"])
					if math.Abs(la-lb) < 8 {
						t.Errorf("%s: %s-bar (L* %.0f) and %s-bar (L* %.0f) are too close in lightness", theme, a, la, b, lb)
					}
				}
			}
		}
	}
}
