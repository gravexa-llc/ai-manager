package web

// The old pages' URLs redirect to the app, and the old
// pages are gone.

import (
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOldURLsRedirect(t *testing.T) {
	srv, _ := newApp(t, nil)
	h := srv.Handler()
	for _, c := range []struct {
		from, to string
		status   int
	}{
		{"/", "/app/", http.StatusFound},
		{"/?fragment=needs", "/app/", http.StatusFound},
		{"/sessions", "/app/sessions", http.StatusPermanentRedirect},
		{"/sessions?all=1", "/app/sessions", http.StatusPermanentRedirect},
		{"/worktrees", "/app/worktrees", http.StatusPermanentRedirect},
		{"/worktrees?fragment=worktrees-list", "/app/worktrees", http.StatusPermanentRedirect},
		{"/plans/sharing", "/app/plans/sharing", http.StatusPermanentRedirect},
		{"/plans/sharing?flash=abc&fragment=plan-board", "/app/plans/sharing", http.StatusPermanentRedirect},
		{"/plans/sharing/graph", "/app/plans/sharing?view=graph", http.StatusPermanentRedirect},
		{"/plans/sharing/timeline", "/app/plans/sharing?view=timeline", http.StatusPermanentRedirect},
		{"/plans/sharing/timeline?pkg=T1&agent=x&n=400", "/app/plans/sharing?pkg=T1&view=timeline", http.StatusPermanentRedirect},
		{"/plans/a%20b", "/app/plans/a%20b", http.StatusPermanentRedirect},
		// An unknown plan still redirects: the app's page says it is unknown.
		{"/plans/nope", "/app/plans/nope", http.StatusPermanentRedirect},
	} {
		rec := do(t, h, "GET", c.from, nil, nil)
		if rec.Code != c.status || rec.Header().Get("Location") != c.to {
			t.Errorf("GET %s = %d to %q, want %d to %q", c.from, rec.Code, rec.Header().Get("Location"), c.status, c.to)
			continue
		}
		// Where it lands is an app page, or the app's own 404 for a plan
		// it doesn't know.
		loc := rec.Header().Get("Location")
		want := http.StatusOK
		if strings.Contains(loc, "/nope") || strings.Contains(loc, "a%20b") {
			want = http.StatusNotFound
		}
		if got := do(t, h, "GET", loc, nil, nil); got.Code != want {
			t.Errorf("GET %s (from %s) = %d, want %d", loc, c.from, got.Code, want)
		}
	}
	// HEAD follows GET.
	if rec := do(t, h, "HEAD", "/sessions", nil, nil); rec.Code != http.StatusPermanentRedirect {
		t.Errorf("HEAD /sessions = %d", rec.Code)
	}
	// Nothing else under the old paths is served.
	for _, p := range []string{"/plans/sharing/board", "/plans", "/home", "/static/app.js", "/static/app.css"} {
		if rec := do(t, h, "GET", p, nil, nil); rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, rec.Code)
		}
	}
}

// / lands on the Overview.
func TestRootIsTheOverview(t *testing.T) {
	srv, _ := newApp(t, nil)
	h := srv.Handler()
	rec := do(t, h, "GET", "/", nil, nil)
	if rec.Code != http.StatusFound {
		t.Fatalf("GET / = %d", rec.Code)
	}
	page := get(t, h, rec.Header().Get("Location"))
	wantIn(t, "overview", page, `data-page="overview"`, "<h1>Overview</h1>", `<section id="ov-plans" class="card"`,
		`<a class="nav on" aria-current="page" href="/app/" data-key="1"`)
}

// The old pages' forms are gone: a valid token gets no route, and the
// app's forms are where writes go.
func TestOldFormsAreGone(t *testing.T) {
	srv, _ := newApp(t, func(o *Options) { o.Go = &stubGo{} })
	h := srv.Handler()
	hdr := map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": "null", "Sec-Fetch-Site": "same-origin"}
	for _, p := range []string{"/dismiss", "/watch", "/unwatch", "/plans", "/plans/sharing/link", "/plans/sharing/unlink",
		"/plans/sharing/done", "/plans/sharing/unregister"} {
		body := url.Values{CSRFField: {srv.CSRF().Token()}, "key": {"pid:11"}, "pkg": {"T1"}}.Encode()
		rec := do(t, h, "POST", p, strings.NewReader(body), hdr)
		if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s = %d, want 404 or 405", p, rec.Code)
		}
	}
	// The dismissal never reached the store: pid:11 still needs you.
	wantIn(t, "menu", get(t, h, "/app/?fragment=menu"), `data-needs-count="4"`)
	// The old Go form flow is gone too: POST /api/go takes JSON with the token,
	// or a page's script's CSRF header; a form with a csrf_token field
	// isn't enough.
	form := url.Values{CSRFField: {srv.CSRF().Token()}, "key": {"pid:11"}, "return": {"/app/sessions"}}.Encode()
	if rec := do(t, h, "POST", "/api/go", strings.NewReader(form), hdr); rec.Code != http.StatusUnauthorized {
		t.Errorf("POST /api/go as a form = %d, want 401", rec.Code)
	}
	if rec := do(t, h, "POST", "/api/go", strings.NewReader(`{"key":"pid:11"}`), map[string]string{TokenHeader: testToken}); rec.Code != http.StatusOK {
		t.Errorf("POST /api/go with the token = %d, want 200 (aim go)", rec.Code)
	}
}

// No old template, script or stylesheet is left: the templates are the
// app's, and every static file is the app's or its icon.
func TestNoOldTemplatesLeft(t *testing.T) {
	entries, err := os.ReadDir("templates")
	must(t, err)
	for _, e := range entries {
		if e.Name() != "app" {
			t.Errorf("templates/%s is left: the app's templates are in templates/app", e.Name())
		}
	}
	old, err := filepath.Glob(filepath.Join("templates", "*.html"))
	must(t, err)
	if len(old) > 0 {
		t.Errorf("old templates left: %v", old)
	}
	for name := range staticFiles {
		if name != "icon.svg" && !strings.HasPrefix(name, "app-") {
			t.Errorf("static/%s isn't the app's", name)
		}
	}
	for _, f := range []string{"app.js", "app.css"} {
		if _, ok := staticFiles[f]; ok {
			t.Errorf("static/%s is left", f)
		}
	}
	if _, err := fs.Stat(assets, "templates"); err == nil {
		t.Error("the static files embed templates")
	}
	// Every app page's template parses with the shell alone.
	files, err := fs.Glob(appTemplates, "templates/app/*.html")
	must(t, err)
	for _, f := range files {
		if strings.HasSuffix(f, "/shell.html") {
			continue
		}
		parseApp(filepath.Base(f))
	}
}
