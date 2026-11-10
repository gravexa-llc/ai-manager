package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/web/api"
	"github.com/gravexa-llc/ai-manager/internal/web/webtest"
)

const testHost = "127.0.0.1:7777"

// doCookie is do with a cookie.
func doCookie(t *testing.T, h http.Handler, method, target string, c *http.Cookie, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	req.Host = testHost
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// signInLink asks for a one-time link with the token.
func signInLink(t *testing.T, h http.Handler, body string) api.SignInLink {
	t.Helper()
	rec := do(t, h, "POST", "/api/sign-in", strings.NewReader(body), map[string]string{TokenHeader: testToken})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/sign-in = %d %s", rec.Code, rec.Body)
	}
	var link api.SignInLink
	decode(t, rec, &link)
	return link
}

func TestNothingServedWithoutSignIn(t *testing.T) {
	h := newTestServer(t, newTestStore(t), nil).Handler()
	for _, path := range []string{
		"/", "/app/", "/app/sessions/", "/app/worktrees/", "/app/settings/", "/sessions",
		"/api/status", "/api/sessions", "/api/worktrees", "/api/prs", "/api/stream",
	} {
		rec := do(t, h, "GET", path, nil, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without a credential = %d, want 401", path, rec.Code)
		}
		// The point of the check: no page, so no CSRF token to lift.
		if body := rec.Body.String(); strings.Contains(body, "csrf") {
			t.Errorf("GET %s without a credential mentions csrf:\n%s", path, body)
		}
		if strings.HasPrefix(path, "/api/") != strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
			t.Errorf("GET %s: Content-Type %q", path, rec.Header().Get("Content-Type"))
		}
	}
	// A form post without a session is refused before its CSRF token is looked at.
	if rec := do(t, h, "POST", "/app/settings/set", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("POST /app/settings/set without a credential = %d, want 401", rec.Code)
	}
	// The script and stylesheet the sign-in page links are open.
	if rec := do(t, h, "GET", "/static/icon.svg", nil, nil); rec.Code != http.StatusOK {
		t.Errorf("GET /static/icon.svg = %d, want 200", rec.Code)
	}
	// The token is a credential for reads as it is for writes.
	if rec := do(t, h, "GET", "/api/status", nil, map[string]string{TokenHeader: testToken}); rec.Code != http.StatusOK {
		t.Errorf("GET /api/status with the token = %d, want 200", rec.Code)
	}
	if rec := do(t, h, "GET", "/api/status", nil, map[string]string{TokenHeader: "wrong"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/status with a wrong token = %d, want 401", rec.Code)
	}
}

func TestSignInLinkNeedsTheToken(t *testing.T) {
	h := newTestServer(t, newTestStore(t), nil).Handler()
	if rec := do(t, h, "POST", "/api/sign-in", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("POST /api/sign-in without the token = %d, want 401", rec.Code)
	}
	if rec := do(t, h, "POST", "/api/sign-in", nil, map[string]string{TokenHeader: "wrong"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("POST /api/sign-in with a wrong token = %d, want 401", rec.Code)
	}
}

func TestSignIn(t *testing.T) {
	now := t0
	s := newTestServer(t, newTestStore(t), nil)
	s.now = func() time.Time { return now }
	h := s.Handler()

	link := signInLink(t, h, "")
	if !strings.HasPrefix(link.Path, "/app/sign-in?code=") || link.ExpiresIn != 60 {
		t.Fatalf("link = %+v", link)
	}
	rec := do(t, h, "GET", link.Path, nil, nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/app/" {
		t.Fatalf("GET the link = %d to %q, want 303 to /app/", rec.Code, rec.Header().Get("Location"))
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %v", cookies)
	}
	c := cookies[0]
	if c.Name != "aim_session_7777" || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.MaxAge != 30*24*3600 {
		t.Errorf("cookie = %+v", c)
	}

	// Signed in: pages and reads are served.
	for _, path := range []string{"/app/", "/api/status", "/api/sessions"} {
		if rec := doCookie(t, h, "GET", path, c, nil); rec.Code != http.StatusOK {
			t.Errorf("GET %s signed in = %d, want 200", path, rec.Code)
		}
	}
	// A session alone writes nothing: a write still needs the token or a
	// page's CSRF token.
	if rec := doCookie(t, h, "POST", "/api/sessions/watch", c, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("POST /api/sessions/watch with only a session = %d, want 401", rec.Code)
	}
	if rec := doCookie(t, h, "POST", "/app/settings/set", c, nil); rec.Code != http.StatusForbidden {
		t.Errorf("POST /app/settings/set with only a session = %d, want 403", rec.Code)
	}
	// With the page's CSRF token the dashboard's script may call the API.
	rec = doCookie(t, h, "POST", "/api/sessions/watch", c, map[string]string{CSRFHeader: s.csrf.Token()})
	if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
		t.Errorf("POST /api/sessions/watch signed in with a CSRF token = %d %s", rec.Code, rec.Body)
	}
	// The wait detail stays with the token.
	if s.detailAllowed(func() *http.Request { r := httptest.NewRequest("GET", "/api/sessions", nil); r.AddCookie(c); return r }()) {
		t.Error("a session cookie may see the wait detail")
	}

	// The link works once.
	if rec := do(t, h, "GET", link.Path, nil, nil); rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) != 0 {
		t.Errorf("GET the link again = %d with cookies %v, want 401 and none", rec.Code, rec.Result().Cookies())
	}
	// A signed-in browser that opens an old link is sent on, with no new cookie.
	if rec := doCookie(t, h, "GET", link.Path, c, nil); rec.Code != http.StatusSeeOther || len(rec.Result().Cookies()) != 0 {
		t.Errorf("GET the used link signed in = %d, want 303", rec.Code)
	}

	// The session ends after SessionMaxAge.
	now = t0.Add(SessionMaxAge - time.Minute)
	if rec := doCookie(t, h, "GET", "/app/", c, nil); rec.Code != http.StatusOK {
		t.Errorf("GET /app/ a minute before the session ends = %d", rec.Code)
	}
	now = t0.Add(SessionMaxAge)
	if rec := doCookie(t, h, "GET", "/app/", c, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /app/ after the session ended = %d, want 401", rec.Code)
	}
}

func TestSignInCodeExpiresAndIsNotGuessable(t *testing.T) {
	now := t0
	s := newTestServer(t, newTestStore(t), nil)
	s.now = func() time.Time { return now }
	h := s.Handler()

	link := signInLink(t, h, "")
	now = t0.Add(SignInCodeTTL)
	if rec := do(t, h, "GET", link.Path, nil, nil); rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) != 0 {
		t.Errorf("GET a link a minute old = %d, want 401 and no cookie", rec.Code)
	}
	for _, q := range []string{"", "?code=", "?code=nope", "?code=" + s.csrf.Token()} {
		if rec := do(t, h, "GET", "/app/sign-in"+q, nil, nil); rec.Code != http.StatusUnauthorized || len(rec.Result().Cookies()) != 0 {
			t.Errorf("GET /app/sign-in%s = %d, want 401 and no cookie", q, rec.Code)
		}
	}
	if a, b := signInLink(t, h, "").Path, signInLink(t, h, "").Path; a == b {
		t.Error("two links carry the same code")
	}
}

func TestSignInReturnStaysInTheApp(t *testing.T) {
	h := newTestServer(t, newTestStore(t), nil).Handler()
	for body, want := range map[string]string{
		`{"return": "/app/plans/search?view=graph"}`: "/app/plans/search?view=graph",
		`{"return": "https://evil.example/app/"}`:    "/app/",
		`{"return": "//evil.example/app/"}`:          "/app/",
		`{"return": "/api/status"}`:                  "/app/",
		`{}`:                                         "/app/",
	} {
		link := signInLink(t, h, body)
		rec := do(t, h, "GET", link.Path, nil, nil)
		if got := rec.Header().Get("Location"); rec.Code != http.StatusSeeOther || got != want {
			t.Errorf("%s: 303 to %q (%d), want %q", body, got, rec.Code, want)
		}
	}
	// The link's own return is checked again when it is followed.
	link := signInLink(t, h, "")
	u, _ := url.Parse(link.Path)
	q := u.Query()
	q.Set("return", "https://evil.example/")
	rec := do(t, h, "GET", "/app/sign-in?"+q.Encode(), nil, nil)
	if got := rec.Header().Get("Location"); got != "/app/" {
		t.Errorf("a link with return=https://evil.example/ goes to %q", got)
	}
}

func TestSessionCookieIsThisAimdsOwn(t *testing.T) {
	st := newTestStore(t)
	s := newTestServer(t, st, nil)
	h := s.Handler()
	c := webtest.SignIn(t, h, testHost, testToken)

	// Another aimd (another token) doesn't accept it, nor a CSRF token in its place.
	other := New(Options{Port: testPort, Token: "another-token", Store: st, Now: func() time.Time { return t0 }}).Handler()
	if rec := doCookie(t, other, "GET", "/app/", c, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("another aimd took the cookie: %d", rec.Code)
	}
	forged := &http.Cookie{Name: c.Name, Value: s.csrf.Token()}
	if rec := doCookie(t, h, "GET", "/app/", forged, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("a CSRF token passed as a session: %d", rec.Code)
	}
	// An aimd on another port reads its own cookie name.
	wrongName := &http.Cookie{Name: "aim_session_7778", Value: c.Value}
	if rec := doCookie(t, h, "GET", "/app/", wrongName, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("a cookie named for another port passed: %d", rec.Code)
	}
}

func TestSignInCodesAreBounded(t *testing.T) {
	s := newSignIn("secret", 7777)
	for range 10 * maxSignInCodes {
		s.newCode(t0)
	}
	if n := len(s.codes); n > maxSignInCodes {
		t.Errorf("%d codes waiting, want at most %d", n, maxSignInCodes)
	}
	code := s.newCode(t0)
	if !s.useCode(code, t0) || s.useCode(code, t0) {
		t.Error("a code doesn't work exactly once")
	}
	var link api.SignInLink
	if err := json.Unmarshal([]byte(`{"path":"/app/sign-in?code=x","expires_in":60}`), &link); err != nil || link.ExpiresIn != 60 {
		t.Errorf("SignInLink JSON: %+v, %v", link, err)
	}
}
