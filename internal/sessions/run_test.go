package sessions

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/config"
)

// fakeRunner returns canned output and records each call. It never runs
// claude.
type fakeRunner struct {
	mu    sync.Mutex
	calls []fakeCall
	// next returns the output for call n (0-based). Nil means "[]".
	next func(n int) ([]byte, error)
	// delay is how long each call takes (fake time under synctest).
	delay time.Duration
	// inFlight counts the calls running now, maxInFlight the most ever.
	inFlight, maxInFlight int
}

type fakeCall struct {
	cmd      Cmd
	at       time.Time
	deadline time.Time
}

func (f *fakeRunner) Run(ctx context.Context, cmd Cmd) ([]byte, error) {
	f.mu.Lock()
	n := len(f.calls)
	dl, _ := ctx.Deadline()
	f.calls = append(f.calls, fakeCall{cmd: cmd, at: time.Now(), deadline: dl})
	next := f.next
	f.inFlight++
	f.maxInFlight = max(f.maxInFlight, f.inFlight)
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.inFlight--
		f.mu.Unlock()
	}()

	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if next == nil {
		return []byte("[]"), nil
	}
	return next(n)
}

func (f *fakeRunner) Calls() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeRunner) MaxInFlight() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxInFlight
}

func TestListerCommand(t *testing.T) {
	data := readFixture(t, "all-2026-11-05T2115.json")
	for _, tt := range []struct {
		name string
		l    Lister
		args []string
	}{
		{"default lists all", Lister{}, []string{"agents", "--json", "--all"}},
		{"active only", Lister{ActiveOnly: true}, []string{"agents", "--json"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeRunner{next: func(int) ([]byte, error) { return data, nil }}
			tt.l.Runner = f
			before := time.Now()
			snap, err := tt.l.List(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			calls := f.Calls()
			if len(calls) != 1 {
				t.Fatalf("%d calls", len(calls))
			}
			c := calls[0]
			if !slices.Equal(c.cmd.Args, tt.args) {
				t.Errorf("args %q, want %q", c.cmd.Args, tt.args)
			}
			if !slices.Contains(c.cmd.Env, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1") {
				t.Errorf("env %q lacks the no-traffic variable", c.cmd.Env)
			}
			if c.deadline.IsZero() || c.deadline.Sub(c.at) > DefaultTimeout {
				t.Errorf("deadline %v for a call at %v, want within %v", c.deadline, c.at, DefaultTimeout)
			}
			if snap.All == tt.l.ActiveOnly {
				t.Errorf("snapshot All = %v with ActiveOnly %v", snap.All, tt.l.ActiveOnly)
			}
			if snap.At.Before(before) || snap.At.After(time.Now()) {
				t.Errorf("snapshot At %v not during the call", snap.At)
			}
			if len(snap.Sessions) != 17 {
				t.Errorf("%d sessions", len(snap.Sessions))
			}
		})
	}
}

func TestListerTimeout(t *testing.T) {
	f := &fakeRunner{delay: time.Minute}
	l := Lister{Runner: f, Timeout: 20 * time.Millisecond}
	start := time.Now()
	_, err := l.List(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "timed out after 20ms") {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %v", d)
	}
}

// A runner that ignores its context and answers late has still timed out.
type lateRunner struct{ delay time.Duration }

func (r lateRunner) Run(context.Context, Cmd) ([]byte, error) {
	time.Sleep(r.delay)
	return []byte("[]"), nil
}

func TestListerLateRunner(t *testing.T) {
	_, err := Lister{Runner: lateRunner{50 * time.Millisecond}, Timeout: 10 * time.Millisecond}.List(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a timeout", err)
	}
}

func TestListerErrors(t *testing.T) {
	boom := errors.New("exit status 1")
	for _, tt := range []struct {
		name    string
		out     string
		err     error
		wantErr string
	}{
		{name: "runner error", err: boom, wantErr: "exit status 1"},
		{name: "bad output", out: "Error: something", wantErr: "not a JSON array"},
		{name: "empty output", out: "", wantErr: "empty output"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeRunner{next: func(int) ([]byte, error) { return []byte(tt.out), tt.err }}
			_, err := Lister{Runner: f}.List(context.Background())
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			if tt.err != nil && !errors.Is(err, tt.err) {
				t.Errorf("err = %v does not wrap %v", err, tt.err)
			}
		})
	}
}

// fakeClaude writes a shell script standing in for claude. It is not
// claude and never touches ~/.claude.
func fakeClaude(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs /bin/sh")
	}
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExecRunner(t *testing.T) {
	// Prints its args and the no-traffic variable as one JSON entry.
	path := fakeClaude(t, `printf '[{"pid": 1, "kind": "interactive", "status": "idle", "args": "%s", "traffic": "%s", "stdin": "%s"}]\n' "$*" "$CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC" "$(cat)"`)
	t.Setenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "0") // overridden by Cmd.Env

	snap, err := Lister{Runner: ExecRunner{Path: path}}.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Sessions) != 1 {
		t.Fatalf("sessions: %+v", snap.Sessions)
	}
	extra := snap.Sessions[0].Extra
	for k, want := range map[string]string{
		"args":    `"agents --json --all"`,
		"traffic": `"1"`,
		"stdin":   `""`,
	} {
		if got := string(extra[k]); got != want {
			t.Errorf("%s = %s, want %s", k, got, want)
		}
	}
}

func TestExecRunnerExitStatus(t *testing.T) {
	path := fakeClaude(t, `echo 'not logged in' >&2; exit 3`)
	_, err := ExecRunner{Path: path}.Run(context.Background(), Cmd{Args: []string{"agents", "--json"}})
	if err == nil || !strings.Contains(err.Error(), "exit status 3") || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("err = %v", err)
	}
	if !strings.HasPrefix(err.Error(), "claude agents --json: ") {
		t.Errorf("err = %v, want it to name the command", err)
	}
}

func TestExecRunnerTimeout(t *testing.T) {
	// The grandchild keeps stdout open after sh is killed; WaitDelay
	// stops Run waiting for it.
	path := fakeClaude(t, `sleep 3; echo '[]'`)
	start := time.Now()
	l := Lister{Runner: ExecRunner{Path: path, WaitDelay: 200 * time.Millisecond}, Timeout: 100 * time.Millisecond}
	_, err := l.List(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("Run took %v after a 100ms timeout", d)
	}
}

func TestExecRunnerMissingBinary(t *testing.T) {
	_, err := ExecRunner{Path: filepath.Join(t.TempDir(), "no-such-claude")}.Run(context.Background(), Cmd{})
	if err == nil {
		t.Fatal("no error for a missing binary")
	}
}

func TestExecRunnerOutputCap(t *testing.T) {
	w := &capped{max: 4}
	if n, err := w.Write([]byte("abc")); n != 3 || err != nil {
		t.Fatal(n, err)
	}
	if n, err := w.Write([]byte("defg")); n != 4 || err != nil {
		t.Fatal(n, err)
	}
	if w.buf.String() != "abcd" || !w.over {
		t.Errorf("buf %q over %v", w.buf.String(), w.over)
	}
}

func TestDefaultsMatchConfig(t *testing.T) {
	if got := config.Default().Poll.Sessions; got != DefaultInterval {
		t.Errorf("config default poll.sessions %v, sessions.DefaultInterval %v", got, DefaultInterval)
	}
}
