package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/focus"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// stubGo answers every Go with res or err, and keeps the keys asked for.
type stubGo struct {
	res  api.GoResult
	err  error
	keys []string
}

func (s *stubGo) Go(_ context.Context, key string) (api.GoResult, error) {
	s.keys = append(s.keys, key)
	return s.res, s.err
}

func goServer(t *testing.T, g GoService, st Store) *Server {
	t.Helper()
	if st == nil {
		st = newTestStore(t)
	}
	return New(Options{Port: testPort, Token: testToken, Store: st, Go: g, Now: func() time.Time { return t0 }})
}

// formHeaders are what a browser sends with a dashboard form.
var formHeaders = map[string]string{
	"Content-Type": "application/x-www-form-urlencoded", "Origin": "null", "Sec-Fetch-Site": "same-origin",
}

func TestGoAPI(t *testing.T) {
	g := &stubGo{res: api.GoResult{Key: "pid:42", Action: api.GoFocused, Message: "Focused lead in iTerm2 (/dev/ttys021)."}}
	h := goServer(t, g, nil).routes()
	tok := map[string]string{TokenHeader: testToken}

	rec := do(t, h, "POST", "/api/go", strings.NewReader(`{"key":"lead"}`), tok)
	var res api.GoResult
	decode(t, rec, &res)
	if rec.Code != http.StatusOK || res.Key != "pid:42" || res.Action != "focused" || len(g.keys) != 1 || g.keys[0] != "lead" {
		t.Errorf("POST /api/go = %d %+v, keys %v", rec.Code, res, g.keys)
	}

	// An ambiguous key: 409, with the candidates.
	g.err = &focus.Error{Status: http.StatusConflict, Msg: `"run" matches 2 sessions`,
		Candidates: []api.GoCandidate{{Key: "pid:1", Title: "a"}, {Key: "pid:2", Title: "b"}}}
	rec = do(t, h, "POST", "/api/go", strings.NewReader(`{"key":"run"}`), tok)
	var refusal api.GoError
	decode(t, rec, &refusal)
	if rec.Code != http.StatusConflict || refusal.Error != `"run" matches 2 sessions` || len(refusal.Candidates) != 2 {
		t.Errorf("ambiguous: %d %+v", rec.Code, refusal)
	}

	g.err = &focus.Error{Status: http.StatusUnprocessableEntity, Msg: "no iTerm2 tab has lead"}
	if rec := do(t, h, "POST", "/api/go", strings.NewReader(`{"key":"lead"}`), tok); rec.Code != http.StatusUnprocessableEntity ||
		!strings.Contains(rec.Body.String(), "no iTerm2 tab has lead") || strings.Contains(rec.Body.String(), "candidates") {
		t.Errorf("unreachable: %d %s", rec.Code, rec.Body)
	}
	g.err = errors.New("disk on fire")
	if rec := do(t, h, "POST", "/api/go", strings.NewReader(`{"key":"lead"}`), tok); rec.Code != http.StatusInternalServerError {
		t.Errorf("failure: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "POST", "/api/go", strings.NewReader(`not json`), tok); rec.Code != http.StatusBadRequest {
		t.Errorf("bad body: %d", rec.Code)
	}

	// Without a Go service there is no route.
	if rec := do(t, goServer(t, nil, nil).routes(), "POST", "/api/go", strings.NewReader(`{"key":"x"}`), tok); rec.Code != http.StatusNotFound {
		t.Errorf("no service: %d", rec.Code)
	}
}

// POST /api/go takes aimd's token, or a dashboard page's CSRF token in the
// header. A form post needs the token too: the pages' Go forms
// post under /app/, so a csrf_token field doesn't open /api/. Nothing else
// reaches Go.
func TestGoAuth(t *testing.T) {
	g := &stubGo{res: api.GoResult{Message: "ok"}}
	srv := goServer(t, g, nil)
	h := srv.routes()
	csrf := srv.CSRF().Token()
	json := func() *strings.Reader { return strings.NewReader(`{"key":"lead"}`) }
	form := func(tok string) *strings.Reader {
		return strings.NewReader(url.Values{"key": {"lead"}, CSRFField: {tok}, "return": {"/sessions"}}.Encode())
	}
	with := func(base map[string]string, kv ...string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	for _, c := range []struct {
		name   string
		body   *strings.Reader
		hdr    map[string]string
		status int
	}{
		{"nothing", json(), nil, http.StatusUnauthorized},
		{"wrong token", json(), map[string]string{TokenHeader: "nope"}, http.StatusUnauthorized},
		{"token", json(), map[string]string{TokenHeader: testToken}, http.StatusOK},
		{"csrf header", json(), map[string]string{CSRFHeader: csrf, "Origin": "http://127.0.0.1:7777"}, http.StatusOK},
		{"bad csrf header", json(), map[string]string{CSRFHeader: "forged"}, http.StatusForbidden},
		{"csrf header, other origin", json(), map[string]string{CSRFHeader: csrf, "Origin": "http://evil.example"}, http.StatusForbidden},
		{"form with a CSRF field", form(csrf), formHeaders, http.StatusUnauthorized},
		{"form without a token", form(""), formHeaders, http.StatusUnauthorized},
		{"form from another site", form(csrf), with(formHeaders, "Sec-Fetch-Site", "cross-site"), http.StatusUnauthorized},
		{"form, wrong token header", form(csrf), with(formHeaders, TokenHeader, "nope"), http.StatusUnauthorized},
		{"wrong Host", json(), map[string]string{TokenHeader: testToken, "Host": "evil.example:7777"}, http.StatusForbidden},
	} {
		g.keys = nil
		rec := do(t, h, "POST", "/api/go", c.body, c.hdr)
		if rec.Code != c.status {
			t.Errorf("%s: %d, want %d (%s)", c.name, rec.Code, c.status, rec.Body)
		}
		if reached := len(g.keys) > 0; reached != (c.status < 400) {
			t.Errorf("%s: reached Go = %v", c.name, reached)
		}
	}
}
