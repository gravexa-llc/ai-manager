package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gravexa-llc/ai-manager/internal/config"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// openHarness fakes aimd's sign-in answer and the browser.
type openHarness struct {
	status int
	body   string
	err    error

	url, token string
	sent       api.SignInRequest
	opened     []string
	browseErr  error
	sys        openSys
}

func newOpenHarness(t *testing.T, withToken bool) (*openHarness, string) {
	t.Helper()
	aimHome, _ := setHome(t)
	token := ""
	if withToken {
		var err error
		if token, err = config.EnsureToken(aimHome); err != nil {
			t.Fatal(err)
		}
	}
	h := &openHarness{status: 200, body: `{"path": "/app/sign-in?code=abc", "expires_in": 60}`}
	h.sys = openSys{
		post: func(_ context.Context, url, token string, body []byte) (int, []byte, error) {
			h.url, h.token = url, token
			_ = json.Unmarshal(body, &h.sent)
			return h.status, []byte(h.body), h.err
		},
		browse: func(_ context.Context, url string) error {
			h.opened = append(h.opened, url)
			return h.browseErr
		},
	}
	return h, token
}

func TestOpenSignsTheBrowserIn(t *testing.T) {
	h, token := newOpenHarness(t, true)
	var out, errb bytes.Buffer
	if code := openCmd(h.sys, nil, &out, &errb); code != 0 {
		t.Fatalf("aim open = %d: %s", code, errb.String())
	}
	if h.url != "http://127.0.0.1:7777/api/sign-in" || h.token != token || h.sent.Return != "" {
		t.Errorf("asked %s with return %q; token sent: %v", h.url, h.sent.Return, h.token == token)
	}
	if len(h.opened) != 1 || h.opened[0] != "http://127.0.0.1:7777/app/sign-in?code=abc" {
		t.Errorf("opened %v", h.opened)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q, want nothing (the link is a credential)", out.String())
	}
}

func TestOpenAPlan(t *testing.T) {
	h, _ := newOpenHarness(t, true)
	var out, errb bytes.Buffer
	if code := openCmd(h.sys, []string{"search plan"}, &out, &errb); code != 0 {
		t.Fatalf("aim open = %d: %s", code, errb.String())
	}
	if h.sent.Return != "/app/plans/search%20plan" {
		t.Errorf("return = %q", h.sent.Return)
	}
}

func TestOpenPrint(t *testing.T) {
	h, _ := newOpenHarness(t, true)
	var out, errb bytes.Buffer
	if code := openCmd(h.sys, []string{"--print"}, &out, &errb); code != 0 {
		t.Fatalf("aim open --print = %d: %s", code, errb.String())
	}
	if out.String() != "http://127.0.0.1:7777/app/sign-in?code=abc\n" || len(h.opened) != 0 {
		t.Errorf("stdout %q, opened %v", out.String(), h.opened)
	}
}

func TestOpenFailures(t *testing.T) {
	t.Run("no token yet", func(t *testing.T) {
		h, _ := newOpenHarness(t, false)
		var out, errb bytes.Buffer
		if code := openCmd(h.sys, nil, &out, &errb); code != 1 || !strings.Contains(errb.String(), "aim up") || h.url != "" {
			t.Errorf("= %d, stderr %q, asked %q", code, errb.String(), h.url)
		}
	})
	t.Run("aimd down", func(t *testing.T) {
		h, _ := newOpenHarness(t, true)
		h.err = errors.New("connection refused")
		var out, errb bytes.Buffer
		if code := openCmd(h.sys, nil, &out, &errb); code != 1 || !strings.Contains(errb.String(), "connection refused") || len(h.opened) != 0 {
			t.Errorf("= %d, stderr %q", code, errb.String())
		}
	})
	t.Run("refused", func(t *testing.T) {
		h, token := newOpenHarness(t, true)
		h.status, h.body = 401, `{"error": "missing or wrong X-AIM-Token"}`
		var out, errb bytes.Buffer
		code := openCmd(h.sys, nil, &out, &errb)
		if code != 1 || !strings.Contains(errb.String(), "missing or wrong") || len(h.opened) != 0 {
			t.Errorf("= %d, stderr %q", code, errb.String())
		}
		if strings.Contains(errb.String(), token) {
			t.Error("stderr holds the token")
		}
	})
	t.Run("a link outside the app", func(t *testing.T) {
		h, _ := newOpenHarness(t, true)
		h.body = `{"path": "//evil.example/"}`
		var out, errb bytes.Buffer
		if code := openCmd(h.sys, nil, &out, &errb); code != 1 || len(h.opened) != 0 {
			t.Errorf("= %d, opened %v", code, h.opened)
		}
	})
	t.Run("no browser", func(t *testing.T) {
		h, _ := newOpenHarness(t, true)
		h.browseErr = errors.New("exit status 1")
		var out, errb bytes.Buffer
		if code := openCmd(h.sys, nil, &out, &errb); code != 1 || !strings.Contains(errb.String(), "/app/sign-in?code=abc") {
			t.Errorf("= %d, stderr %q", code, errb.String())
		}
	})
	t.Run("usage", func(t *testing.T) {
		h, _ := newOpenHarness(t, true)
		var out, errb bytes.Buffer
		if code := openCmd(h.sys, []string{"a", "b"}, &out, &errb); code != 2 {
			t.Errorf("two plans = %d, want 2", code)
		}
		if code := openCmd(h.sys, []string{"--nope"}, &out, &errb); code != 2 {
			t.Errorf("an unknown flag = %d, want 2", code)
		}
	})
}
