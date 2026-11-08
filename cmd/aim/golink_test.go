package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"syscall"
	"testing"

	"github.com/gravexa-llc/ai-manager/internal/config"
	"github.com/gravexa-llc/ai-manager/internal/focus"
	"github.com/gravexa-llc/ai-manager/internal/term"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// fakeAimd answers aim go's POST.
type fakeAimd struct {
	status int
	body   string
	err    error

	url, token string
	sent       api.GoRequest
	posts      int
}

func (f *fakeAimd) post(_ context.Context, url, token string, body []byte) (int, []byte, error) {
	f.posts++
	f.url, f.token = url, token
	_ = json.Unmarshal(body, &f.sent)
	return f.status, []byte(f.body), f.err
}

// directTargets are what `claude agents --json` would list.
type directTargets []focus.Target

func (d directTargets) Targets(context.Context) ([]focus.Target, error) { return d, nil }
func (directTargets) LeadOf(ctx context.Context, id string) (focus.Lead, error) {
	return focus.ListerSource{}.LeadOf(ctx, id)
}

// directTerm records the focus without running osascript.
type directTerm struct{ focused []int }

func (d *directTerm) Focus(_ context.Context, pid int, _ string) (term.Focused, error) {
	d.focused = append(d.focused, pid)
	return term.Focused{App: term.AppITerm, TTY: "/dev/ttys021", Via: "tty"}, nil
}
func (d *directTerm) Attach(context.Context, string) (term.Attached, error) {
	return term.Attached{}, nil
}

type goHarness struct {
	aimd   *fakeAimd
	term   *directTerm
	direct int
	sys    goSys
}

func newGoHarness(t *testing.T, withToken bool) *goHarness {
	t.Helper()
	aimHome, _ := setHome(t)
	if withToken {
		if _, err := config.EnsureToken(aimHome); err != nil {
			t.Fatal(err)
		}
	}
	h := &goHarness{aimd: &fakeAimd{}, term: &directTerm{}}
	h.sys = goSys{
		post: h.aimd.post,
		direct: func() *focus.Service {
			h.direct++
			return &focus.Service{Source: directTargets{
				{Key: "pid:42", PID: 42, Kind: "interactive", Name: "lead"},
				{Key: "pid:43", PID: 43, Kind: "interactive", Name: "leader"},
			}, Terminal: h.term}
		},
	}
	return h
}

func (h *goHarness) run(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := goCmd(h.sys, args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestGoUsage(t *testing.T) {
	h := newGoHarness(t, true)
	if code, out, _ := h.run("--help"); code != 0 || out != goUsage {
		t.Errorf("--help = %d %q", code, out)
	}
	for _, args := range [][]string{nil, {"a", "b"}, {""}} {
		if code, _, errOut := h.run(args...); code != 2 || !strings.Contains(errOut, "usage: aim go") {
			t.Errorf("%q = %d %q", args, code, errOut)
		}
	}
	if h.aimd.posts+h.direct != 0 {
		t.Error("a usage error went somewhere")
	}
}

func TestGoThroughAimd(t *testing.T) {
	h := newGoHarness(t, true)
	h.aimd.status = 200
	h.aimd.body = `{"key":"pid:42","action":"focused","message":"Focused lead in iTerm2 (/dev/ttys021)."}`
	code, out, errOut := h.run("lead")
	if code != 0 || out != "Focused lead in iTerm2 (/dev/ttys021).\n" {
		t.Errorf("aim go lead = %d %q %q", code, out, errOut)
	}
	tok, _ := config.ReadSecretFile(config.TokenPath(mustHome(t)))
	if h.aimd.url != "http://127.0.0.1:7777/api/go" || h.aimd.token != tok || h.aimd.sent.Key != "lead" || h.direct != 0 {
		t.Errorf("posted %q with token %q, key %q; direct %d", h.aimd.url, h.aimd.token, h.aimd.sent.Key, h.direct)
	}
}

func mustHome(t *testing.T) string {
	t.Helper()
	home, err := config.Home()
	if err != nil {
		t.Fatal(err)
	}
	return home
}

func TestGoRefusedByAimd(t *testing.T) {
	h := newGoHarness(t, true)
	h.aimd.status = 409
	h.aimd.body = `{"error":"\"run\" matches 2 sessions; give more of the name or cwd, or a key","candidates":[{"key":"pid:1","title":"pivot","cwd":"/r/pivot"},{"key":"job:022c44bf","title":"run job"}]}`
	code, _, errOut := h.run("run")
	want := "aim go: \"run\" matches 2 sessions; give more of the name or cwd, or a key\n  pid:1  pivot  /r/pivot\n  job:022c44bf  run job\n"
	if code != 1 || errOut != want {
		t.Errorf("ambiguous = %d\n%s\nwant\n%s", code, errOut, want)
	}

	h.aimd.status, h.aimd.body = 404, "404 page not found\n"
	if code, _, errOut := h.run("x"); code != 1 || !strings.Contains(errOut, "has no /api/go") {
		t.Errorf("old aimd = %d %q", code, errOut)
	}
	h.aimd.status, h.aimd.body = 500, "oops"
	if code, _, errOut := h.run("x"); code != 1 || !strings.Contains(errOut, "HTTP 500") {
		t.Errorf("500 = %d %q", code, errOut)
	}
	if h.direct != 0 {
		t.Error("went direct while aimd answered")
	}
}

func TestGoDirect(t *testing.T) {
	// aimd isn't listening: aim go does it itself.
	h := newGoHarness(t, true)
	h.aimd.err = fmt.Errorf("dial tcp 127.0.0.1:7777: %w", syscall.ECONNREFUSED)
	code, out, errOut := h.run("lead")
	if code != 0 || out != "Focused lead in iTerm2 (/dev/ttys021).\n" || !strings.Contains(errOut, "aimd isn't running") {
		t.Errorf("direct = %d %q %q", code, out, errOut)
	}
	if h.direct != 1 || len(h.term.focused) != 1 || h.term.focused[0] != 42 {
		t.Errorf("direct %d, focused %v", h.direct, h.term.focused)
	}

	// Direct refusals print like aimd's.
	if code, _, errOut := h.run("nobody"); code != 1 || !strings.Contains(errOut, `aim go: no live session matches "nobody"`) {
		t.Errorf("direct refusal = %d %q", code, errOut)
	}
	if code, _, errOut := h.run("agent:a2743fbbaf44b95d0"); code != 1 || !strings.Contains(errOut, "only aimd knows") {
		t.Errorf("agent without aimd = %d %q", code, errOut)
	}

	// No token file: aimd has never run, so don't even ask it.
	h = newGoHarness(t, false)
	if code, _, _ := h.run("lead"); code != 0 || h.aimd.posts != 0 || h.direct != 1 {
		t.Errorf("no token: %d, posts %d, direct %d", code, h.aimd.posts, h.direct)
	}
}

// Any other failure to reach aimd is reported: aimd may be running, so
// going directly could act twice.
func TestGoAimdUnreachable(t *testing.T) {
	h := newGoHarness(t, true)
	h.aimd.err = context.DeadlineExceeded
	if code, _, errOut := h.run("lead"); code != 1 || !strings.Contains(errOut, "aimd on 127.0.0.1:7777") || h.direct != 0 {
		t.Errorf("timeout = %d %q, direct %d", code, errOut, h.direct)
	}
}

// postJSON's error for a port nobody listens on is ECONNREFUSED, which is
// what sends aim go direct.
func TestPostJSONRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	_, _, err = postJSON(context.Background(), "http://"+addr+"/api/go", "tok", []byte(`{}`))
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("err = %v, want ECONNREFUSED", err)
	}
}
