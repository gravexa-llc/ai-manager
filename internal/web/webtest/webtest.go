// Package webtest signs a test's requests in to aimd's handler
// (web.Server.Handler), the way `aim open` signs a browser in.
package webtest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// SignIn asks h for a one-time sign-in link with token, follows it, and
// returns the session cookie it sets. host is the Host header aimd expects
// ("127.0.0.1:7777"). A request carrying the cookie may read; a write still
// needs the token or a page's CSRF token.
func SignIn(t testing.TB, h http.Handler, host, token string) *http.Cookie {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/sign-in", nil)
	req.Host = host
	req.Header.Set("X-AIM-Token", token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var link struct {
		Path string `json:"path"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &link) != nil || link.Path == "" {
		t.Fatalf("POST /api/sign-in = %d %s", rec.Code, rec.Body)
	}
	req = httptest.NewRequest(http.MethodGet, link.Path, nil)
	req.Host = host
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Value != "" {
			return c
		}
	}
	t.Fatalf("GET %s = %d, and set no cookie", link.Path, rec.Code)
	return nil
}
