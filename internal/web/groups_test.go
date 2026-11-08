package web

import (
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// sectionKeyRE finds a list's sections (groups.go) and their keys.
var sectionKeyRE = regexp.MustCompile(`<details [^>]*data-group="([^"]*)"`)

// sectionKeys are a list's sections' keys, in order.
func sectionKeys(list string) []string {
	var out []string
	for _, m := range sectionKeyRE.FindAllStringSubmatch(list, -1) {
		out = append(out, m[1])
	}
	return out
}

// sectionOf returns a list's section with the key: its <details> tag, and
// its HTML up to its closing tag.
func sectionOf(t *testing.T, list, key string) (tag, body string) {
	t.Helper()
	i := strings.Index(list, `data-group="`+key+`"`)
	if i < 0 {
		t.Fatalf("no section %q in:\n%s", key, list)
	}
	start := strings.LastIndex(list[:i], "<details")
	end := strings.Index(list[start:], "</details>")
	if start < 0 || end < 0 {
		t.Fatalf("section %q isn't a closed <details>", key)
	}
	return list[start : start+strings.Index(list[start:], ">")+1], list[start : start+end]
}

// wantSection checks how a section starts: open or collapsed, and forced
// open (data-group-force) or not.
func wantSection(t *testing.T, list, key string, open, force bool) {
	t.Helper()
	tag, _ := sectionOf(t, list, key)
	if got := strings.Contains(tag, " open"); got != open {
		t.Errorf("section %s: open = %v, want %v (%s)", key, got, open, tag)
	}
	if got := strings.Contains(tag, " data-group-force"); got != force {
		t.Errorf("section %s: forced = %v, want %v (%s)", key, got, force, tag)
	}
}

// groupCookieOf is the Group by cookie an answer sets, or nil.
func groupCookieOf(rec interface{ Result() *http.Response }, page string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == groupCookie(page) {
			return c
		}
	}
	return nil
}

// TestGroupByRemembered: a page's Group by comes from ?group=, else the
// cookie a whole page with ?group= set, else the default; a fragment (a
// live refresh) never sets it, and values a page doesn't have are ignored.
func TestGroupByRemembered(t *testing.T) {
	srv, _ := newApp(t, nil)
	h := srv.Handler()
	for _, page := range []struct{ path, name, def, other string }{
		{"/app/prs", "prs", "repo", "plan"},
		{"/app/worktrees", "worktrees", "repo", "status"},
		{"/app/inbox", "inbox", "plan", "session"},
	} {
		on := func(v string) string { return `data-group-by="` + v + `" class="on" aria-current="true">` }
		// The default, and no cookie.
		rec := do(t, h, "GET", page.path, nil, nil)
		wantIn(t, page.path, rec.Body.String(), on(page.def), `data-group-cookie="aim_group_`+page.name+`"`)
		if c := groupCookieOf(rec, page.name); c != nil {
			t.Errorf("%s: set %v without a choice", page.path, c)
		}
		// A whole page with ?group= remembers it, for the app's pages, a
		// year and more.
		rec = do(t, h, "GET", page.path+"?group="+page.other, nil, nil)
		wantIn(t, page.path, rec.Body.String(), on(page.other))
		c := groupCookieOf(rec, page.name)
		if c == nil || c.Value != page.other || c.Path != "/app/" || c.MaxAge < 365*24*3600 || c.SameSite != http.SameSiteLaxMode {
			t.Fatalf("%s?group=%s: cookie %+v", page.path, page.other, c)
		}
		// A URL without ?group= follows it, the list and the whole page.
		hdr := map[string]string{"Cookie": c.Name + "=" + c.Value}
		for _, p := range []string{page.path, page.path + "?fragment=list"} {
			rec = do(t, h, "GET", p, nil, hdr)
			wantIn(t, p+" with the cookie", rec.Body.String(), on(page.other))
			if groupCookieOf(rec, page.name) != nil {
				t.Errorf("%s: set the cookie it was sent", p)
			}
		}
		// The URL wins over the cookie; a fragment doesn't change it.
		rec = do(t, h, "GET", page.path+"?group="+page.def+"&fragment=list", nil, hdr)
		wantIn(t, page.path+" fragment", rec.Body.String(), on(page.def))
		if c := groupCookieOf(rec, page.name); c != nil {
			t.Errorf("%s: a fragment set %v", page.path, c)
		}
		// Junk in the URL or the cookie is the default.
		rec = do(t, h, "GET", page.path+"?group=zzz", nil, map[string]string{"Cookie": c.Name + "=zzz"})
		wantIn(t, page.path+" junk", rec.Body.String(), on(page.def))
		if c := groupCookieOf(rec, page.name); c != nil {
			t.Errorf("%s: remembered junk %v", page.path, c)
		}
		// Each choice's link keeps the rest of the URL.
		wantIn(t, page.path+" links", get(t, h, page.path+"?q=x&fragment=list"),
			`href="`+page.path+`?group=`+page.other+`&amp;q=x"`)
	}
	// Sessions has no Group by.
	wantNotIn(t, "sessions", get(t, h, "/app/sessions"), "data-group-by", `class="gby"`)
}

// The shared parts: the shell's partials, the page script that remembers
// the sections, and the shell's busy rule, which an open section isn't.
func TestGroupsScript(t *testing.T) {
	js := string(staticFiles["app-groups.js"].data)
	wantIn(t, "app-groups.js", js,
		`"details.group[data-group]"`, `"data-group-force"`, "localStorage.getItem", "localStorage.setItem",
		`"aim-groups:" + location.pathname`, `"aim:swap"`, `"a[data-group-by]"`, `searchParams.set("group"`,
		`"data-group-cookie"`, "doc.cookie", "path=/app/", `.rows > li:not(.filtered)`)
	// Storage can throw (a private window, blocked site data): every use is
	// in a try.
	if n, m := strings.Count(js, "localStorage."), strings.Count(js, "try {"); n != 2 || m != 2 {
		t.Errorf("app-groups.js: %d localStorage uses, %d try blocks", n, m)
	}
	wantNotIn(t, "app-groups.js", js, "innerHTML", "insertAdjacentHTML", "outerHTML", "eval(", "http://", "https://")

	shell := string(staticFiles["app-shell.js"].data)
	wantIn(t, "busy", jsFunc(t, shell, "busy"), `a.closest("details[open]:not(.group)")`)
	css := string(staticFiles["app-shell.css"].data)
	wantIn(t, "app-shell.css", css, ".group > summary::before", ".group[open] > summary::before { transform: rotate(90deg); }",
		".group:has(> summary.filtered) { display: none; }", ".gby .seg a")

	// Each page with sections loads the script.
	srv, _ := newApp(t, nil)
	h := srv.Handler()
	for _, p := range []string{"/app/prs", "/app/worktrees", "/app/sessions", "/app/inbox"} {
		wantIn(t, p, get(t, h, p), `<script src="/static/app-groups.js?v=`+assetVersion+`" defer></script>`)
	}
	wantNotIn(t, "overview", get(t, h, "/app/"), "app-groups.js")
}
