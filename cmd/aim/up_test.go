package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// fakeSys is a machine with claude, gh and git in made-up folders, a fake
// launchctl that only records its calls, and a fake aimd status.
type fakeSys struct {
	mu     sync.Mutex
	calls  [][]string
	loaded bool
	// unloadPolls is how many prints after a bootout still find the
	// service, as launchd finishes removing it after bootout returns. A
	// bootstrap in that time fails with exit status 5, as on a real Mac.
	unloadPolls int
	unloading   int
	// bootstrapErrs are the exit statuses of the next bootstraps, 0 for
	// one that works. Once they run out, bootstraps work.
	bootstrapErrs []int
	status        []byte
	statusOK      bool
}

// exitErr is a failed launchctl's exit status, with the ExitCode method
// *exec.ExitError has.
type exitErr int

func (e exitErr) Error() string { return "exit status " + strconv.Itoa(int(e)) }
func (e exitErr) ExitCode() int { return int(e) }

func (f *fakeSys) sys(t *testing.T, userHome string, tools map[string]string) sys {
	exe := filepath.Join(t.TempDir(), "aim")
	aimd := filepath.Join(filepath.Dir(exe), "aimd")
	if err := os.WriteFile(aimd, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return sys{
		lookPath: func(name string) (string, error) {
			if p, ok := tools[name]; ok {
				return p, nil
			}
			return "", exec.ErrNotFound
		},
		executable: func() (string, error) { return exe, nil },
		userHome:   func() (string, error) { return userHome, nil },
		uid:        501,
		launchctl: func(ctx context.Context, args ...string) ([]byte, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.calls = append(f.calls, slices.Clone(args))
			switch args[0] {
			case "print":
				if f.unloading > 0 {
					f.unloading--
					return nil, nil
				}
				if !f.loaded {
					return []byte("Could not find service"), exitErr(113)
				}
			case "bootstrap":
				if f.unloading > 0 {
					return []byte("Bootstrap failed: 5: Input/output error"), exitErr(5)
				}
				if len(f.bootstrapErrs) > 0 {
					code := f.bootstrapErrs[0]
					f.bootstrapErrs = f.bootstrapErrs[1:]
					if code != 0 {
						return []byte("Bootstrap failed: " + strconv.Itoa(code) + ": some error"), exitErr(code)
					}
				}
				f.loaded = true
			case "bootout":
				f.loaded = false
				f.unloading = f.unloadPolls
			}
			return nil, nil
		},
		httpGet: func(ctx context.Context, url string) ([]byte, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			if !f.statusOK || url != "http://127.0.0.1:7777/api/status" {
				return nil, errors.New("connection refused")
			}
			return f.status, nil
		},
	}
}

func (f *fakeSys) launchctlCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, strings.Join(c, " "))
	}
	return out
}

// shortWaits shortens the waits around launchctl for one test. The number
// of bootstrap tries stays as it is.
func shortWaits(t *testing.T) {
	t.Helper()
	wait, poll, delay := unloadWait, unloadPoll, bootstrapDelay
	t.Cleanup(func() { unloadWait, unloadPoll, bootstrapDelay = wait, poll, delay })
	unloadWait, unloadPoll, bootstrapDelay = 2*time.Second, time.Millisecond, time.Millisecond
}

const (
	printCall   = "print gui/501/com.gravexa.aimd"
	bootoutCall = "bootout gui/501/com.gravexa.aimd"
)

var testTools = map[string]string{
	"claude": "/opt/tools/claude-bin/claude", // a stable link; its folder goes in PATH
	"gh":     "/opt/homebrew/bin/gh",
	"git":    "/usr/bin/git",
}

// setHome points AIM_HOME at a temp dir and clears the other passed-through
// variables.
func setHome(t *testing.T) (aimHome, userHome string) {
	t.Helper()
	userHome = t.TempDir()
	aimHome = filepath.Join(userHome, "aim-home")
	t.Setenv("AIM_HOME", aimHome)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("GH_CONFIG_DIR", "")
	return aimHome, userHome
}

// plistDoc is the structure of the generated plist, decoded generically.
type plistDoc struct {
	XMLName xml.Name `xml:"plist"`
	Dict    struct {
		Inner []byte `xml:",innerxml"`
	} `xml:"dict"`
}

func TestPlistGeneration(t *testing.T) {
	aimHome, userHome := setHome(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "/cfg/claude & co")
	f := &fakeSys{}
	s := f.sys(t, userHome, testTools)

	plan, err := planAgent(s)
	if err != nil {
		t.Fatal(err)
	}
	if plan.plistPath != filepath.Join(userHome, "Library", "LaunchAgents", "com.gravexa.aimd.plist") {
		t.Errorf("plist path = %s", plan.plistPath)
	}
	if len(plan.warnings) != 0 {
		t.Errorf("warnings = %q", plan.warnings)
	}
	wantEnv := []plistEnv{
		{"PATH", "/opt/tools/claude-bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin"},
		{"AIMD_LAUNCHD", "1"},
		{"AIM_HOME", aimHome},
		{"CLAUDE_CONFIG_DIR", "/cfg/claude & co"},
	}
	if !slices.Equal(plan.data.Env, wantEnv) {
		t.Errorf("env = %q, want %q", plan.data.Env, wantEnv)
	}

	out, err := renderPlist(plan.data)
	if err != nil {
		t.Fatal(err)
	}
	var doc plistDoc
	if err := xml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("plist isn't well-formed XML: %v\n%s", err, out)
	}
	text := string(out)
	for _, want := range []string{
		"<key>Label</key>\n\t<string>com.gravexa.aimd</string>",
		"<string>" + filepath.Join(filepath.Dir(mustExe(t, s)), "aimd") + "</string>\n\t\t<string>--foreground</string>",
		"<key>PATH</key>\n\t\t<string>/opt/tools/claude-bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>",
		"<key>AIMD_LAUNCHD</key>\n\t\t<string>1</string>",
		"<key>CLAUDE_CONFIG_DIR</key>\n\t\t<string>/cfg/claude &amp; co</string>",
		"<key>WorkingDirectory</key>\n\t<string>" + aimHome + "</string>",
		"<key>StandardErrorPath</key>\n\t<string>" + filepath.Join(aimHome, "aimd.launchd.log") + "</string>",
		"<key>RunAtLoad</key>\n\t<true/>",
		"<key>SuccessfulExit</key>\n\t\t<false/>",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("plist lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "{{") {
		t.Errorf("unfilled template action in:\n%s", text)
	}

	// plutil, where there is one, must accept it.
	if plutil, err := exec.LookPath("plutil"); err == nil {
		p := filepath.Join(t.TempDir(), "x.plist")
		if err := os.WriteFile(p, out, 0o644); err != nil {
			t.Fatal(err)
		}
		if msg, err := exec.Command(plutil, "-lint", p).CombinedOutput(); err != nil {
			t.Errorf("plutil -lint: %v: %s", err, msg)
		}
	}
}

func mustExe(t *testing.T, s sys) string {
	exe, err := s.executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

func TestPlistMissingTools(t *testing.T) {
	_, userHome := setHome(t)
	f := &fakeSys{}
	s := f.sys(t, userHome, map[string]string{"git": "/usr/bin/git"})
	plan, err := planAgent(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.warnings) != 2 || !strings.Contains(plan.warnings[0], "claude") || !strings.Contains(plan.warnings[1], "gh") {
		t.Errorf("warnings = %q, want claude and gh", plan.warnings)
	}
	if plan.data.Env[0].Value != "/usr/bin:/bin:/usr/sbin:/sbin" {
		t.Errorf("PATH = %q", plan.data.Env[0].Value)
	}

	// No aimd anywhere is an error.
	s.executable = func() (string, error) { return filepath.Join(t.TempDir(), "aim"), nil }
	if _, err := planAgent(s); err == nil || !strings.Contains(err.Error(), "aimd not found") {
		t.Errorf("planAgent without aimd: %v", err)
	}
	s.lookPath = func(name string) (string, error) {
		if name == "aimd" {
			return "/usr/local/bin/aimd", nil
		}
		return "", exec.ErrNotFound
	}
	if plan, err := planAgent(s); err != nil || plan.data.Program != "/usr/local/bin/aimd" {
		t.Errorf("aimd from PATH: %q, %v", plan.data.Program, err)
	}
}

func TestUpPrintInstallsNothing(t *testing.T) {
	aimHome, userHome := setHome(t)
	f := &fakeSys{}
	var stdout, stderr bytes.Buffer
	if code := up(f.sys(t, userHome, testTools), []string{"--print"}, &stdout, &stderr); code != 0 {
		t.Fatalf("up --print = %d: %s", code, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "<?xml") || !strings.Contains(stdout.String(), "com.gravexa.aimd") {
		t.Errorf("stdout = %s", stdout.String())
	}
	if calls := f.launchctlCalls(); len(calls) != 0 {
		t.Errorf("--print ran launchctl: %q", calls)
	}
	for _, p := range []string{filepath.Join(userHome, "Library"), aimHome} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("--print created %s", p)
		}
	}
}

func TestUpAndDown(t *testing.T) {
	aimHome, userHome := setHome(t)
	st, _ := json.Marshal(api.Status{})
	f := &fakeSys{status: st, statusOK: true}
	s := f.sys(t, userHome, testTools)
	plist := filepath.Join(userHome, "Library", "LaunchAgents", "com.gravexa.aimd.plist")

	var stdout, stderr bytes.Buffer
	if code := up(s, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("up = %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "http://127.0.0.1:7777") {
		t.Errorf("stdout = %q", stdout.String())
	}
	if fi, err := os.Stat(plist); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("plist: %v, %v", fi, err)
	}
	if fi, err := os.Stat(aimHome); err != nil || !fi.IsDir() {
		t.Errorf("home not created: %v", err)
	}
	want := []string{
		"print gui/501/com.gravexa.aimd",
		"bootstrap gui/501 " + plist,
	}
	if got := f.launchctlCalls(); !slices.Equal(got, want) {
		t.Errorf("launchctl calls = %q, want %q", got, want)
	}

	// Running it again reloads.
	f.calls = nil
	stdout.Reset()
	if code := up(s, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("second up = %d: %s", code, stderr.String())
	}
	want = []string{
		printCall,
		bootoutCall,
		printCall, // gone: go on
		"bootstrap gui/501 " + plist,
	}
	if got := f.launchctlCalls(); !slices.Equal(got, want) {
		t.Errorf("launchctl calls on reload = %q, want %q", got, want)
	}

	f.calls = nil
	stdout.Reset()
	if code := down(s, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("down = %d: %s", code, stderr.String())
	}
	want = []string{printCall, bootoutCall, printCall}
	if got := f.launchctlCalls(); !slices.Equal(got, want) {
		t.Errorf("launchctl calls on down = %q, want %q", got, want)
	}
	if _, err := os.Stat(plist); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("plist still there after down: %v", err)
	}
	if !strings.Contains(stdout.String(), "aimd stopped") {
		t.Errorf("down said %q", stdout.String())
	}

	// Down again: nothing to do.
	stdout.Reset()
	if code := down(s, nil, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "wasn't running") {
		t.Errorf("second down = %d, %q", code, stdout.String())
	}
}

// upFake is a fakeSys whose aimd answers once loaded, with the waits
// around launchctl shortened.
func upFake(t *testing.T, f *fakeSys) (sys, string) {
	t.Helper()
	shortWaits(t)
	_, userHome := setHome(t)
	f.status, _ = json.Marshal(api.Status{})
	f.statusOK = true
	return f.sys(t, userHome, testTools), filepath.Join(userHome, "Library", "LaunchAgents", "com.gravexa.aimd.plist")
}

// The failure this fixes: bootout returns while launchd is still removing
// aimd, and a bootstrap then fails with exit status 5.
func TestUpReloadWaitsForBootout(t *testing.T) {
	f := &fakeSys{loaded: true, unloadPolls: 3}
	s, plist := upFake(t, f)
	var stdout, stderr bytes.Buffer
	if code := up(s, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("up = %d: %s", code, stderr.String())
	}
	want := append([]string{printCall, bootoutCall}, slices.Repeat([]string{printCall}, 4)...)
	want = append(want, "bootstrap gui/501 "+plist)
	if got := f.launchctlCalls(); !slices.Equal(got, want) {
		t.Errorf("launchctl calls = %q, want %q", got, want)
	}
	if stderr.Len() > 0 {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestUpRetriesBootstrapOnStatus5(t *testing.T) {
	f := &fakeSys{bootstrapErrs: []int{5}}
	s, plist := upFake(t, f)
	var stdout, stderr bytes.Buffer
	if code := up(s, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("up = %d: %s", code, stderr.String())
	}
	want := []string{printCall, "bootstrap gui/501 " + plist, "bootstrap gui/501 " + plist}
	if got := f.launchctlCalls(); !slices.Equal(got, want) {
		t.Errorf("launchctl calls = %q, want %q", got, want)
	}
	if !strings.Contains(stdout.String(), "aimd is running") || stderr.Len() > 0 {
		t.Errorf("stdout %q, stderr %q", stdout.String(), stderr.String())
	}
}

func TestUpBootstrapKeepsFailing(t *testing.T) {
	for _, tc := range []struct {
		name      string
		f         *fakeSys
		bootstrap int // calls
		stderr    []string
	}{
		{
			name:      "status 5 every time",
			f:         &fakeSys{loaded: true, bootstrapErrs: []int{5, 5, 5, 5, 5, 5}},
			bootstrap: bootstrapTries,
			stderr:    []string{"Bootstrap failed: 5", "(tried 4 times)"},
		},
		{
			name:      "another status is not retried",
			f:         &fakeSys{loaded: true, bootstrapErrs: []int{37}},
			bootstrap: 1,
			stderr:    []string{"launchctl bootstrap: exit status 37: Bootstrap failed: 37"},
		},
		{
			// unload gives up waiting; bootstrap is tried anyway, and fails
			// with 5 while launchd still has the service.
			name:      "service never goes",
			f:         &fakeSys{loaded: true, unloadPolls: 1 << 30},
			bootstrap: bootstrapTries,
			stderr:    []string{"Bootstrap failed: 5: Input/output error", "(tried 4 times)"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := upFake(t, tc.f)
			unloadWait = 20 * time.Millisecond
			var stdout, stderr bytes.Buffer
			if code := up(s, nil, &stdout, &stderr); code != 1 {
				t.Fatalf("up = %d, want 1: %s", code, stderr.String())
			}
			n := 0
			for _, c := range tc.f.launchctlCalls() {
				if strings.HasPrefix(c, "bootstrap ") {
					n++
				}
			}
			if n != tc.bootstrap {
				t.Errorf("%d bootstraps, want %d: %q", n, tc.bootstrap, tc.f.launchctlCalls())
			}
			for _, want := range append(tc.stderr, "aim up: aimd is stopped now; run `aim up` again") {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr lacks %q:\n%s", want, stderr.String())
				}
			}
			if tc.bootstrap == 1 && strings.Contains(stderr.String(), "tried") {
				t.Errorf("stderr counts tries for one try:\n%s", stderr.String())
			}
			if stdout.Len() > 0 {
				t.Errorf("stdout = %q", stdout.String())
			}
		})
	}
}

func TestDownWaitsForBootout(t *testing.T) {
	f := &fakeSys{loaded: true, unloadPolls: 2}
	s, plist := upFake(t, f)
	writePlist(t, plist)
	var stdout, stderr bytes.Buffer
	if code := down(s, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("down = %d: %s", code, stderr.String())
	}
	want := append([]string{printCall, bootoutCall}, slices.Repeat([]string{printCall}, 3)...)
	if got := f.launchctlCalls(); !slices.Equal(got, want) {
		t.Errorf("launchctl calls = %q, want %q", got, want)
	}
	if stdout.String() != "aimd stopped\n" {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func TestDownReportsAServiceThatDoesntGo(t *testing.T) {
	f := &fakeSys{loaded: true, unloadPolls: 1 << 30}
	s, plist := upFake(t, f)
	unloadWait = 20 * time.Millisecond
	writePlist(t, plist)
	var stdout, stderr bytes.Buffer
	if code := down(s, nil, &stdout, &stderr); code != 1 {
		t.Fatalf("down = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "launchd still had aimd 20ms after bootout") || stdout.Len() > 0 {
		t.Errorf("stdout %q, stderr %q", stdout.String(), stderr.String())
	}
	// It is going (bootout worked), so it mustn't start again at login.
	if _, err := os.Stat(plist); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("plist still there: %v", err)
	}
}

func writePlist(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("<plist/>"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// exitCode must see the status in a real launchctl failure, which is an
// *exec.ExitError inside a launchctlError.
func TestExitCode(t *testing.T) {
	err := exec.Command("/bin/sh", "-c", "exit 5").Run()
	if got := exitCode(err); got != 5 {
		t.Errorf("exitCode(%v) = %d, want 5", err, got)
	}
	wrapped := &launchctlError{verb: "bootstrap", out: "Bootstrap failed: 5: Input/output error", err: err}
	if got := exitCode(wrapped); got != 5 {
		t.Errorf("exitCode(%v) = %d, want 5", wrapped, got)
	}
	if got := exitCode(errors.New("exit status 5")); got != -1 {
		t.Errorf("exitCode of a plain error = %d, want -1", got)
	}
}

func TestUpReportsADaemonThatDoesntAnswer(t *testing.T) {
	defer func(d time.Duration) { upWait = d }(upWait)
	upWait = 300 * time.Millisecond
	_, userHome := setHome(t)
	f := &fakeSys{}
	s := f.sys(t, userHome, testTools)
	var stdout, stderr bytes.Buffer
	start := time.Now()
	code := up(s, nil, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "doesn't answer") || !strings.Contains(stderr.String(), "aimd.launchd.log") {
		t.Errorf("up = %d, %q", code, stderr.String())
	}
	if time.Since(start) < upWait {
		t.Errorf("gave up after %s, want %s", time.Since(start), upWait)
	}
}

func TestUpUsage(t *testing.T) {
	var stdout, stderr bytes.Buffer
	f := &fakeSys{}
	s := f.sys(t, t.TempDir(), testTools)
	if code := up(s, []string{"--bogus"}, &stdout, &stderr); code != 2 {
		t.Errorf("up --bogus = %d", code)
	}
	if code := up(s, []string{"-h"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "usage: aim up") {
		t.Errorf("up -h = %d, %q", code, stdout.String())
	}
	if code := down(s, []string{"extra"}, io.Discard, io.Discard); code != 2 {
		t.Errorf("down extra = %d", code)
	}
	if len(f.launchctlCalls()) != 0 {
		t.Error("usage errors ran launchctl")
	}
}
