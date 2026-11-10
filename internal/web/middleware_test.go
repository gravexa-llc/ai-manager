package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestHostCheck(t *testing.T) {
	h := newTestServer(t, newTestStore(t), nil).routes()
	for _, tc := range []struct {
		host string
		code int
	}{
		{"127.0.0.1:7777", http.StatusOK},
		{"localhost:7777", http.StatusOK},
		{"LocalHost:7777", http.StatusOK},
		{"127.0.0.1", http.StatusForbidden},
		{"localhost", http.StatusForbidden},
		{"127.0.0.1:8888", http.StatusForbidden},
		{"evil.example:7777", http.StatusForbidden},
		{"localhost.evil.example:7777", http.StatusForbidden},
		{"[::1]:7777", http.StatusForbidden},
		{"0.0.0.0:7777", http.StatusForbidden},
		{"", http.StatusForbidden},
	} {
		for _, path := range []string{"/api/status", "/api/sessions", "/app/"} {
			rec := do(t, h, "GET", path, nil, map[string]string{"Host": tc.host})
			if rec.Code != tc.code {
				t.Errorf("GET %s with Host %q = %d, want %d", path, tc.host, rec.Code, tc.code)
			}
		}
	}
}

func TestNoCORSHeaders(t *testing.T) {
	h := newTestServer(t, newTestStore(t), nil).routes()
	for _, method := range []string{"GET", "OPTIONS"} {
		rec := do(t, h, method, "/api/sessions", nil, map[string]string{
			"Origin":                        "https://evil.example",
			"Access-Control-Request-Method": "POST",
		})
		for k := range rec.Header() {
			if strings.HasPrefix(strings.ToLower(k), "access-control-") {
				t.Errorf("%s /api/sessions sent CORS header %s", method, k)
			}
		}
		if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q", method, got)
		}
	}
}

func TestTokenRequiredForWrites(t *testing.T) {
	s := newTestServer(t, newTestStore(t), nil)
	s.mux.HandleFunc("POST /api/test-write", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	h := s.routes()
	for _, tc := range []struct {
		desc, method, path, token string
		code                      int
	}{
		{"read needs no token", "GET", "/api/sessions", "", http.StatusOK},
		{"write without token", "POST", "/api/test-write", "", http.StatusUnauthorized},
		{"write with wrong token", "POST", "/api/test-write", "nope", http.StatusUnauthorized},
		{"write with a prefix of the token", "POST", "/api/test-write", testToken[:5], http.StatusUnauthorized},
		{"write with the token", "POST", "/api/test-write", testToken, http.StatusNoContent},
		{"delete without token", "DELETE", "/api/sessions", "", http.StatusUnauthorized},
		{"put without token", "PUT", "/api/prs", "", http.StatusUnauthorized},
		{"write to a read route with the token reaches the mux", "POST", "/api/sessions", testToken, http.StatusMethodNotAllowed},
		{"events without token", "POST", "/events", "", http.StatusUnauthorized},
		// "{}" has no events array: the handler (events.go) refuses it.
		{"events with token reaches the handler", "POST", "/events", testToken, http.StatusBadRequest},
	} {
		hdr := map[string]string{}
		if tc.token != "" {
			hdr[TokenHeader] = tc.token
		}
		rec := do(t, h, tc.method, tc.path, strings.NewReader("{}"), hdr)
		if rec.Code != tc.code {
			t.Errorf("%s: %s %s = %d, want %d (%s)", tc.desc, tc.method, tc.path, rec.Code, tc.code, rec.Body)
		}
	}

	// An empty configured token never matches an empty header.
	open := RequireToken("", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	if rec := do(t, open, "POST", "/api/x", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("empty token: POST = %d, want 401", rec.Code)
	}
}

func TestCSRFTokens(t *testing.T) {
	c := NewCSRF(testToken, testPort)
	now := t0
	c.now = func() time.Time { return now }

	tok := c.Token()
	if !c.Valid(tok) {
		t.Fatal("fresh token not valid")
	}
	if tok == c.Token() {
		t.Error("two tokens are equal; each page should get its own")
	}
	restarted := NewCSRF(testToken, testPort)
	restarted.now = c.now
	if !restarted.Valid(tok) {
		t.Error("a token isn't valid after a restart with the same aimd token")
	}
	other := NewCSRF("other-token", testPort)
	other.now = c.now
	if other.Valid(tok) {
		t.Error("a token signed under another aimd token is valid")
	}
	for _, bad := range []string{"", "x", tok[:len(tok)-2], tok + "AA", strings.ToUpper(tok)} {
		if c.Valid(bad) {
			t.Errorf("Valid(%q) = true", bad)
		}
	}
	// Flip one byte of the nonce: the MAC must catch it.
	b := []byte(tok)
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	if c.Valid(string(b)) {
		t.Error("tampered token is valid")
	}

	now = t0.Add(CSRFMaxAge - time.Second)
	if !c.Valid(tok) {
		t.Error("token just under the max age isn't valid")
	}
	now = t0.Add(CSRFMaxAge)
	if c.Valid(tok) {
		t.Error("token at the max age is still valid")
	}
	now = t0.Add(-2 * time.Minute)
	if c.Valid(tok) {
		t.Error("token from the future is valid")
	}
}

func TestCSRFProtect(t *testing.T) {
	s := newTestServer(t, newTestStore(t), nil)
	s.mux.HandleFunc("POST /test-form", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	h := s.routes()
	tok := s.CSRF().Token()
	form := func(v string) string { return url.Values{CSRFField: {v}}.Encode() }
	formHdr := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	with := func(extra map[string]string) map[string]string {
		m := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}

	for _, tc := range []struct {
		desc string
		body string
		hdr  map[string]string
		code int
	}{
		{"no token", "", formHdr, http.StatusForbidden},
		{"bad token", form("forged"), formHdr, http.StatusForbidden},
		{"form token", form(tok), formHdr, http.StatusNoContent},
		{"header token", "", map[string]string{CSRFHeader: tok}, http.StatusNoContent},
		{"same origin", form(tok), with(map[string]string{"Origin": "http://127.0.0.1:7777", "Sec-Fetch-Site": "same-origin"}), http.StatusNoContent},
		{"localhost origin", form(tok), with(map[string]string{"Origin": "http://localhost:7777"}), http.StatusNoContent},
		{"other origin with a valid token", form(tok), with(map[string]string{"Origin": "https://evil.example"}), http.StatusForbidden},
		{"other port origin", form(tok), with(map[string]string{"Origin": "http://127.0.0.1:8000"}), http.StatusForbidden},
		{"cross-site fetch with a valid token", form(tok), with(map[string]string{"Sec-Fetch-Site": "cross-site"}), http.StatusForbidden},
		{"same-site but other origin", form(tok), with(map[string]string{"Sec-Fetch-Site": "same-site"}), http.StatusForbidden},
		{"aimd token is not a form token", "", map[string]string{TokenHeader: testToken}, http.StatusForbidden},
		// A form post from a page with Referrer-Policy: no-referrer carries
		// Origin: null; it passes only when the browser says same-origin.
		{"null origin, same-origin fetch", form(tok), with(map[string]string{"Origin": "null", "Sec-Fetch-Site": "same-origin"}), http.StatusNoContent},
		{"null origin without fetch metadata", form(tok), with(map[string]string{"Origin": "null"}), http.StatusForbidden},
		{"null origin, cross-site fetch", form(tok), with(map[string]string{"Origin": "null", "Sec-Fetch-Site": "cross-site"}), http.StatusForbidden},
		{"null origin, same-origin fetch, no token", "", with(map[string]string{"Origin": "null", "Sec-Fetch-Site": "same-origin"}), http.StatusForbidden},
	} {
		rec := do(t, h, "POST", "/test-form", strings.NewReader(tc.body), tc.hdr)
		if rec.Code != tc.code {
			t.Errorf("%s: POST /test-form = %d, want %d (%s)", tc.desc, rec.Code, tc.code, rec.Body)
		}
	}

	// Reads pass without a token.
	if rec := do(t, h, "GET", "/app/", nil, nil); rec.Code != http.StatusOK {
		t.Errorf("GET /app/ = %d, want 200", rec.Code)
	}
}
