package term

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
)

// fakeRunner stands in for osascript: it answers each script with what
// answers holds for it ("" if nothing), or fails with err, and records
// every call.
type fakeRunner struct {
	answers map[string]string
	err     error
	calls   []call
}

type call struct {
	script string
	args   []string
}

func (f *fakeRunner) Run(_ context.Context, script string, args ...string) (string, error) {
	f.calls = append(f.calls, call{script, args})
	return f.answers[script], f.err
}

// scripts lists the scripts run, by name.
func (f *fakeRunner) scripts() []string {
	names := map[string]string{
		focusITermScript: "focus iTerm2", focusTerminalScript: "focus Terminal",
		attachITermScript: "attach iTerm2", attachTerminalScript: "attach Terminal",
	}
	var out []string
	for _, c := range f.calls {
		out = append(out, names[c.script])
	}
	return out
}

// runner answers the iTerm2 and Terminal focus scripts.
func runner(iterm, terminal string) *fakeRunner {
	return &fakeRunner{answers: map[string]string{focusITermScript: iterm, focusTerminalScript: terminal}}
}

// fakePS answers TTY from a map; a pid it doesn't know has no process.
type fakePS map[int]struct {
	tty string
	err error
}

func (f fakePS) TTY(_ context.Context, pid int) (string, error) {
	r, ok := f[pid]
	if !ok {
		return "", ErrNoProcess
	}
	return r.tty, r.err
}

const guid = "0A1B2C3D-0000-4000-8000-00000000AB01"

var ps42 = fakePS{42: {tty: "/dev/ttys021"}}

func TestFocusInITerm(t *testing.T) {
	r := runner("ok tty", "")
	f, err := Terminal{Runner: r, TTYs: ps42}.Focus(context.Background(), 42, "")
	if err != nil {
		t.Fatal(err)
	}
	if f.App != AppITerm || f.TTY != "/dev/ttys021" || f.Via != "tty" {
		t.Errorf("Focused = %+v", f)
	}
	if !slices.Equal(r.scripts(), []string{"focus iTerm2"}) || !slices.Equal(r.calls[0].args, []string{"/dev/ttys021", ""}) {
		t.Errorf("calls = %+v", r.calls)
	}
}

// Not in iTerm2 (or iTerm2 not running): the Terminal.app tab with the tty.
func TestFocusInTerminal(t *testing.T) {
	for _, iterm := range []string{"not-found", "not-running"} {
		r := runner(iterm, "ok tty")
		f, err := Terminal{Runner: r, TTYs: ps42}.Focus(context.Background(), 42, "")
		if err != nil {
			t.Fatalf("iTerm2 %s: %v", iterm, err)
		}
		if f.App != AppTerminal || f.TTY != "/dev/ttys021" || f.Via != "tty" {
			t.Errorf("iTerm2 %s: Focused = %+v", iterm, f)
		}
		if !slices.Equal(r.scripts(), []string{"focus iTerm2", "focus Terminal"}) || !slices.Equal(r.calls[1].args, []string{"/dev/ttys021"}) {
			t.Errorf("iTerm2 %s: calls = %+v", iterm, r.calls)
		}
	}
}

// In neither app: the error names both. Neither running: says so.
func TestFocusNowhere(t *testing.T) {
	for _, tc := range []struct {
		iterm, terminal string
		want            error
	}{
		{"not-found", "not-found", ErrNotFound},
		{"not-running", "not-found", ErrNotFound},
		{"not-found", "not-running", ErrNotFound},
		{"not-running", "not-running", ErrNotRunning},
	} {
		r := runner(tc.iterm, tc.terminal)
		_, err := Terminal{Runner: r, TTYs: ps42}.Focus(context.Background(), 42, "")
		if !errors.Is(err, tc.want) {
			t.Errorf("%s/%s: err = %v, want %v", tc.iterm, tc.terminal, err, tc.want)
		}
		if tc.want == ErrNotFound && err.Error() != "no iTerm2 or Terminal tab has tty /dev/ttys021" {
			t.Errorf("%s/%s: message %q", tc.iterm, tc.terminal, err)
		}
		if tc.want == ErrNotRunning && err.Error() != "neither iTerm2 nor Terminal is running" {
			t.Errorf("message %q", err)
		}
	}
}

func TestFocusWithItermSessionID(t *testing.T) {
	r := runner("ok tty", "")
	if _, err := (Terminal{Runner: r, TTYs: ps42}).Focus(context.Background(), 42, "w0t1p0:"+strings.ToLower(guid)); err != nil {
		t.Fatal(err)
	}
	if got := r.calls[0].args; !slices.Equal(got, []string{"/dev/ttys021", guid}) {
		t.Errorf("args = %q, want the tty and the upper-case guid", got)
	}

	// No terminal, but the mod reported ITERM_SESSION_ID: the guid alone,
	// in iTerm2 only (Terminal has no such id).
	r = runner("ok id", "")
	f, err := Terminal{Runner: r, TTYs: fakePS{42: {err: ErrNoTTY}}}.Focus(context.Background(), 42, "w0t1p0:"+guid)
	if err != nil || f.Via != "iterm_session_id" || f.App != AppITerm {
		t.Fatalf("Focus = %+v, %v", f, err)
	}
	if got := r.calls[0].args; !slices.Equal(got, []string{"", guid}) {
		t.Errorf("args = %q", got)
	}
	r = runner("not-found", "ok tty")
	_, err = Terminal{Runner: r, TTYs: fakePS{42: {err: ErrNoTTY}}}.Focus(context.Background(), 42, "w0t1p0:"+guid)
	if !errors.Is(err, ErrNotFound) || !slices.Equal(r.scripts(), []string{"focus iTerm2"}) {
		t.Errorf("guid not found: %v, ran %v", err, r.scripts())
	}
	if err.Error() != "no iTerm2 or Terminal tab has iTerm2 session id "+guid {
		t.Errorf("message %q", err)
	}
}

func TestFocusRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ps    fakePS
		pid   int
		iterm string
		want  error
	}{
		{"no terminal", fakePS{42: {err: ErrNoTTY}}, 42, "", ErrNoTTY},
		{"no process", fakePS{}, 42, "", ErrNoProcess},
		{"no pid, no id", fakePS{}, 0, "", ErrInvalid},
		{"malformed iterm id", fakePS{}, 0, "w0t1p0:not-a-guid", ErrInvalid},
		{"a ps that didn't validate", fakePS{42: {tty: `/dev/ttys1" & do shell script "x`}}, 42, "", ErrInvalid},
		{"not a pseudo-terminal", fakePS{42: {tty: "/dev/console"}}, 42, "", ErrInvalid},
	} {
		r := runner("ok tty", "ok tty")
		_, err := Terminal{Runner: r, TTYs: tc.ps}.Focus(context.Background(), tc.pid, tc.iterm)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
		if len(r.calls) != 0 {
			t.Errorf("%s: ran a script: %+v", tc.name, r.calls)
		}
	}
}

func TestFocusOddAnswers(t *testing.T) {
	for _, r := range []*fakeRunner{runner("huh", ""), runner("not-found", "huh")} {
		if _, err := (Terminal{Runner: r, TTYs: ps42}).Focus(context.Background(), 42, ""); err == nil || !strings.Contains(err.Error(), "unexpected") {
			t.Errorf("odd answer: %v", err)
		}
	}
	boom := errors.New("osascript broke")
	r := &fakeRunner{err: boom}
	if _, err := (Terminal{Runner: r, TTYs: ps42}).Focus(context.Background(), 42, ""); !errors.Is(err, boom) || len(r.calls) != 1 {
		t.Errorf("runner error: %v after %d calls", err, len(r.calls))
	}
}

const testClaude = "/Users/someone/.local/bin/claude"

func TestAttachInITerm(t *testing.T) {
	r := &fakeRunner{answers: map[string]string{attachITermScript: "ok", attachTerminalScript: "ok"}}
	a, err := Terminal{Runner: r, Claude: testClaude}.Attach(context.Background(), "022c44bf")
	if err != nil {
		t.Fatal(err)
	}
	want := "'" + testClaude + "' attach 022c44bf"
	if a.Command != want || a.App != AppITerm {
		t.Errorf("Attached = %+v, want %q in iTerm2", a, want)
	}
	if !slices.Equal(r.scripts(), []string{"attach iTerm2"}) || !slices.Equal(r.calls[0].args, []string{want}) {
		t.Errorf("calls = %+v", r.calls)
	}
	// A path with a space is fine inside the quotes.
	if cmd, err := AttachCommand("/Applications/Claude Code/bin/claude", "022c44bf"); err != nil || cmd != "'/Applications/Claude Code/bin/claude' attach 022c44bf" {
		t.Errorf("AttachCommand with a space = %q, %v", cmd, err)
	}
}

// iTerm2 not running: a new Terminal window, with the same command.
func TestAttachInTerminal(t *testing.T) {
	r := &fakeRunner{answers: map[string]string{attachITermScript: "not-running", attachTerminalScript: "ok"}}
	a, err := Terminal{Runner: r, Claude: testClaude}.Attach(context.Background(), "022c44bf")
	if err != nil {
		t.Fatal(err)
	}
	want := "'" + testClaude + "' attach 022c44bf"
	if a.App != AppTerminal || a.Command != want {
		t.Errorf("Attached = %+v", a)
	}
	if !slices.Equal(r.scripts(), []string{"attach iTerm2", "attach Terminal"}) || !slices.Equal(r.calls[1].args, []string{want}) {
		t.Errorf("calls = %+v", r.calls)
	}

	r = &fakeRunner{answers: map[string]string{attachITermScript: "not-running", attachTerminalScript: "huh"}}
	if _, err := (Terminal{Runner: r, Claude: testClaude}).Attach(context.Background(), "022c44bf"); err == nil || !strings.Contains(err.Error(), "unexpected") {
		t.Errorf("odd answer: %v", err)
	}
	boom := errors.New("osascript broke")
	r = &fakeRunner{err: boom}
	if _, err := (Terminal{Runner: r, Claude: testClaude}).Attach(context.Background(), "022c44bf"); !errors.Is(err, boom) || len(r.calls) != 1 {
		t.Errorf("runner error: %v after %d calls", err, len(r.calls))
	}
}

func TestAttachRefusesInvalidValues(t *testing.T) {
	good := "/usr/local/bin/claude"
	for _, tc := range []struct{ claude, job string }{
		{good, ""},
		{good, "022C44BF"},
		{good, "022c44b"},
		{good, "022c44bf0"},
		{good, "022c44bf; rm -rf ~"},
		{good, "$(touch x)"},
		{good, "../../x"},
		{"", "022c44bf"},
		{"claude", "022c44bf"},
		{"/usr/local/bin/../bin/claude", "022c44bf"},
		{"/tmp/it's/claude", "022c44bf"},
		{"/tmp/$(x)/claude", "022c44bf"},
		{"/tmp/`x`/claude", "022c44bf"},
		{"/tmp/a\nb/claude", "022c44bf"},
		{`/tmp/a\b/claude`, "022c44bf"},
		{`/tmp/a"b/claude`, "022c44bf"},
	} {
		r := &fakeRunner{answers: map[string]string{attachITermScript: "not-running", attachTerminalScript: "ok"}}
		_, err := Terminal{Runner: r, Claude: tc.claude}.Attach(context.Background(), tc.job)
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("Attach(%q, %q): err = %v, want ErrInvalid", tc.claude, tc.job, err)
		}
		if len(r.calls) != 0 {
			t.Errorf("Attach(%q, %q) ran a script", tc.claude, tc.job)
		}
	}
}

func TestParseTTY(t *testing.T) {
	for out, want := range map[string]string{
		"ttys021 \n": "/dev/ttys021",
		"ttys0":      "/dev/ttys0",
		"  ttys9  ":  "/dev/ttys9",
	} {
		if got, err := ParseTTY(out); err != nil || got != want {
			t.Errorf("ParseTTY(%q) = %q, %v; want %q", out, got, err, want)
		}
	}
	for _, out := range []string{"", "??", "?", "-", "  ??  \n"} {
		if _, err := ParseTTY(out); !errors.Is(err, ErrNoTTY) {
			t.Errorf("ParseTTY(%q): err = %v, want ErrNoTTY", out, err)
		}
	}
	for _, out := range []string{"console", "ttys", "ttys1a", "ttys021; rm", "../ttys1", "tty.usb"} {
		if _, err := ParseTTY(out); !errors.Is(err, ErrInvalid) {
			t.Errorf("ParseTTY(%q): err = %v, want ErrInvalid", out, err)
		}
	}
}

func TestValidators(t *testing.T) {
	for s, want := range map[string]bool{
		"/dev/ttys021": true, "/dev/ttys0": true, "/dev/tty021": false, "/dev/ttys": false,
		"/dev/ttys021\n": false, " /dev/ttys021": false, "/dev/ttys0211111": false,
	} {
		if ValidTTY(s) != want {
			t.Errorf("ValidTTY(%q) = %v", s, !want)
		}
	}
	for s, want := range map[string]bool{"022c44bf": true, "aaaa0001": true, "022c44b": false, "job:022c44bf": false, "022c44bf\n": false} {
		if ValidJobID(s) != want {
			t.Errorf("ValidJobID(%q) = %v", s, !want)
		}
	}
	for in, want := range map[string]string{
		"w0t1p0:" + guid:                    guid,
		guid:                                guid,
		"w1t2p3:" + strings.ToLower(guid):   guid,
		"":                                  "",
		"w0t1p0:":                           "",
		"w0t1p0:" + guid + "\" & beep & \"": "",
	} {
		if got := SessionGUID(in); got != want {
			t.Errorf("SessionGUID(%q) = %q, want %q", in, got, want)
		}
	}
}

// The scripts take every value from argv: none is ever part of their
// text, and none runs a shell. Each addresses its app by bundle id, and
// all but the Terminal attach (Terminal is the fallback that opens) check
// that the app runs before telling it anything, so they never launch it.
func TestScriptsAreConstant(t *testing.T) {
	s := AllScripts()
	for _, tc := range []struct {
		name, script, bundle string
		checksRunning        bool
	}{
		{"focus iTerm2", s.FocusITerm, ITermBundleID, true},
		{"focus Terminal", s.FocusTerminal, TerminalBundleID, true},
		{"attach iTerm2", s.AttachITerm, ITermBundleID, true},
		{"attach Terminal", s.AttachTerminal, TerminalBundleID, false},
	} {
		if !strings.HasPrefix(tc.script, "on run argv") || !strings.Contains(tc.script, `tell application id "`+tc.bundle+`"`) {
			t.Errorf("%s script doesn't read argv or address its app by bundle id", tc.name)
		}
		if strings.Contains(tc.script, "do shell script") {
			t.Errorf("%s script runs a shell", tc.name)
		}
		i, j := strings.Index(tc.script, "is running"), strings.Index(tc.script, "tell application")
		if tc.checksRunning && (i < 0 || i > j) {
			t.Errorf("%s script doesn't check that the app runs before telling it anything", tc.name)
		}
	}
	if !strings.Contains(s.FocusTerminal, "tty of t") || !strings.Contains(s.FocusTerminal, "set selected of t to true") ||
		!strings.Contains(s.AttachTerminal, "do script cmd") {
		t.Error("Terminal scripts don't use Terminal's tab tty, selected and do script")
	}
}

// The real ps: this process exists (with or without a terminal), and pid
// 0 is refused without running anything.
func TestPS(t *testing.T) {
	tty, err := PS{}.TTY(context.Background(), os.Getpid())
	if err != nil && !errors.Is(err, ErrNoTTY) {
		t.Errorf("own pid: %q, %v", tty, err)
	}
	if err == nil && !ValidTTY(tty) {
		t.Errorf("own pid: tty %q", tty)
	}
	if _, err := (PS{}).TTY(context.Background(), 0); !errors.Is(err, ErrInvalid) {
		t.Errorf("pid 0: %v", err)
	}
}
