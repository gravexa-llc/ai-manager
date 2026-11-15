package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// fakeSystemd is a fake `systemctl --user` that only records its calls,
// and loginctl's answer about lingering.
type fakeSystemd struct {
	mu     sync.Mutex
	calls  []string
	active bool
	// fail is the verb whose call fails, with failOut as its output.
	fail, failOut string
	lingerOn      bool
	lingerKnown   bool
}

// linuxSys is a Linux machine: fakeSys's tools and aimd status, with
// systemctl on PATH and fs in place of systemd. XDG_CONFIG_HOME is unset,
// so the unit goes under userHome/.config.
func linuxSys(t *testing.T, f *fakeSys, fs *fakeSystemd, userHome string) (sys, string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", "")
	tools := map[string]string{"systemctl": "/usr/bin/systemctl"}
	for k, v := range testTools {
		tools[k] = v
	}
	s := f.sys(t, userHome, tools)
	s.goos = "linux"
	s.systemctl = func(_ context.Context, args ...string) ([]byte, error) {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		fs.calls = append(fs.calls, strings.Join(args, " "))
		if args[0] == fs.fail {
			return []byte(fs.failOut), exitErr(1)
		}
		switch args[0] {
		case "is-active":
			if !fs.active {
				return nil, exitErr(3)
			}
		case "restart":
			fs.active = true
		case "stop":
			fs.active = false
		}
		return nil, nil
	}
	s.linger = func() (bool, bool) { return fs.lingerOn, fs.lingerKnown }
	return s, filepath.Join(userHome, ".config", "systemd", "user", "aimd.service")
}

func (fs *fakeSystemd) taken() []string {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	calls := fs.calls
	fs.calls = nil
	return calls
}

func TestUnitGeneration(t *testing.T) {
	aimHome, userHome := setHome(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "/cfg/claude & co")
	s, _ := linuxSys(t, &fakeSys{}, &fakeSystemd{}, userHome)
	plan, err := planAgent(s)
	if err != nil {
		t.Fatal(err)
	}
	out, err := renderUnit(plan.data)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	for _, want := range []string{
		"ExecStart=" + filepath.Join(filepath.Dir(mustExe(t, s)), "aimd") + " --foreground\n",
		"WorkingDirectory=" + aimHome + "\n",
		"Environment=PATH=/opt/tools/claude-bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin\n",
		"Environment=AIMD_LAUNCHD=1\n",
		"Environment=AIM_HOME=" + aimHome + "\n",
		`Environment="CLAUDE_CONFIG_DIR=/cfg/claude & co"` + "\n",
		"Restart=on-failure\n",
		"WantedBy=default.target\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("unit lacks %q:\n%s", want, text)
		}
	}
}

// What systemd would read as a quote, an escape, a specifier or a variable
// is written so that it reads as itself; a line break is refused.
func TestUnitEscaping(t *testing.T) {
	for _, tc := range []struct {
		name string
		fn   func() (string, error)
		want string
	}{
		{"plain word", func() (string, error) { return unitWord("/opt/a-b_c/x.y") }, "/opt/a-b_c/x.y"},
		{"space", func() (string, error) { return unitWord("/opt/my tools/x") }, `"/opt/my tools/x"`},
		{"quote and backslash", func() (string, error) { return unitWord(`a"b\c`) }, `"a\"b\\c"`},
		{"specifier", func() (string, error) { return unitWord("/opt/100%/x") }, "/opt/100%%/x"},
		{"env", func() (string, error) { return unitEnv("K", "a b") }, `"K=a b"`},
		{"exec with a space", func() (string, error) { return unitExec("/opt/my tools/aimd") }, `"/opt/my tools/aimd"`},
		{"exec dollar", func() (string, error) { return unitExec("/opt/$x/aimd") }, `"/opt/$x/aimd"`},
		{"dir with a space", func() (string, error) { return unitDir("/home/some one/.ai-manager") }, "/home/some one/.ai-manager"},
		{"dir specifier", func() (string, error) { return unitDir("/home/x/50%") }, "/home/x/50%%"},
	} {
		if got, err := tc.fn(); err != nil || got != tc.want {
			t.Errorf("%s: %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
	for name, fn := range map[string]func() (string, error){
		"line break in a word":  func() (string, error) { return unitWord("a\nExecStart=/bin/x") },
		"line break in a dir":   func() (string, error) { return unitDir("/a\n/b") },
		"relative program":      func() (string, error) { return unitExec("aimd") },
		"quote in a program":    func() (string, error) { return unitExec(`/opt/a"b/aimd`) },
		"backslash in program":  func() (string, error) { return unitExec(`/opt/a\b/aimd`) },
		"relative working dir":  func() (string, error) { return unitDir("home") },
		"line break in a value": func() (string, error) { return unitEnv("K", "a\rb") },
	} {
		if got, err := fn(); err == nil {
			t.Errorf("%s: %q, want an error", name, got)
		}
	}
}

func TestUnitPathFollowsXDGConfigHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/cfg")
	if got := unitPath("/home/someone"); got != "/cfg/systemd/user/aimd.service" {
		t.Errorf("unit path = %s", got)
	}
	// A relative value is ignored, as the XDG spec says.
	t.Setenv("XDG_CONFIG_HOME", "cfg")
	if got := unitPath("/home/someone"); got != "/home/someone/.config/systemd/user/aimd.service" {
		t.Errorf("unit path = %s", got)
	}
}

func TestUpPrintOnLinuxInstallsNothing(t *testing.T) {
	_, userHome := setHome(t)
	fs := &fakeSystemd{}
	s, unit := linuxSys(t, &fakeSys{}, fs, userHome)
	var stdout, stderr bytes.Buffer
	if code := up(s, []string{"--print"}, &stdout, &stderr); code != 0 {
		t.Fatalf("up --print = %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "[Service]\nExecStart=") {
		t.Errorf("stdout isn't the unit:\n%s", stdout.String())
	}
	if _, err := os.Stat(unit); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("unit installed by --print: %v", err)
	}
	if calls := fs.taken(); len(calls) != 0 {
		t.Errorf("systemctl calls = %q", calls)
	}
}

func TestUpAndDownOnLinux(t *testing.T) {
	aimHome, userHome := setHome(t)
	st, _ := json.Marshal(api.Status{})
	f := &fakeSys{status: st, statusOK: true}
	fs := &fakeSystemd{lingerOn: true, lingerKnown: true}
	s, unit := linuxSys(t, f, fs, userHome)

	var stdout, stderr bytes.Buffer
	if code := up(s, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("up = %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "http://127.0.0.1:7777") || !strings.Contains(stdout.String(), unit) {
		t.Errorf("stdout = %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q", stderr.String())
	}
	if fi, err := os.Stat(unit); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("unit: %v, %v", fi, err)
	}
	if fi, err := os.Stat(aimHome); err != nil || !fi.IsDir() {
		t.Errorf("home not created: %v", err)
	}
	// restart also reloads a running aimd, so a second up runs the same.
	want := []string{"daemon-reload", "enable aimd.service", "restart aimd.service"}
	if got := fs.taken(); !slices.Equal(got, want) {
		t.Errorf("systemctl calls = %q, want %q", got, want)
	}
	if got := f.launchctlCalls(); len(got) != 0 {
		t.Errorf("launchctl calls on Linux = %q", got)
	}

	stdout.Reset()
	if code := down(s, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("down = %d: %s", code, stderr.String())
	}
	want = []string{"is-active --quiet aimd.service", "stop aimd.service", "disable aimd.service", "daemon-reload"}
	if got := fs.taken(); !slices.Equal(got, want) {
		t.Errorf("systemctl calls on down = %q, want %q", got, want)
	}
	if _, err := os.Stat(unit); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("unit still there after down: %v", err)
	}
	if !strings.Contains(stdout.String(), "aimd stopped") {
		t.Errorf("down said %q", stdout.String())
	}

	// Down again: nothing to do, and nothing to disable.
	stdout.Reset()
	if code := down(s, nil, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "wasn't running") {
		t.Errorf("second down = %d, %q", code, stdout.String())
	}
	if got := fs.taken(); !slices.Equal(got, []string{"is-active --quiet aimd.service"}) {
		t.Errorf("systemctl calls on second down = %q", got)
	}
}

// A stopped aimd whose unit is still installed: down removes the unit.
func TestDownRemovesAnIdleUnit(t *testing.T) {
	_, userHome := setHome(t)
	fs := &fakeSystemd{}
	s, unit := linuxSys(t, &fakeSys{}, fs, userHome)
	if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unit, []byte("[Service]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := down(s, nil, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "removed its systemd unit") {
		t.Errorf("down = %d, %q, stderr %q", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(unit); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("unit still there: %v", err)
	}
}

// Without lingering, aimd stops at logout: up says how to change that.
func TestUpNotesLingering(t *testing.T) {
	_, userHome := setHome(t)
	st, _ := json.Marshal(api.Status{})
	for _, tc := range []struct {
		on, known, note bool
	}{{false, true, true}, {true, true, false}, {false, false, false}} {
		fs := &fakeSystemd{lingerOn: tc.on, lingerKnown: tc.known}
		s, _ := linuxSys(t, &fakeSys{status: st, statusOK: true}, fs, userHome)
		var stdout, stderr bytes.Buffer
		if code := up(s, nil, &stdout, &stderr); code != 0 {
			t.Fatalf("up = %d: %s", code, stderr.String())
		}
		if got := strings.Contains(stderr.String(), "loginctl enable-linger"); got != tc.note {
			t.Errorf("linger on=%v known=%v: note %v, stderr %q", tc.on, tc.known, got, stderr.String())
		}
	}
}

func TestUpOnLinuxFailures(t *testing.T) {
	_, userHome := setHome(t)
	st, _ := json.Marshal(api.Status{})

	t.Run("no systemctl", func(t *testing.T) {
		fs := &fakeSystemd{}
		s, unit := linuxSys(t, &fakeSys{}, fs, userHome)
		s.lookPath = func(name string) (string, error) { return "", os.ErrNotExist }
		// aimd is still found next to aim.
		var stdout, stderr bytes.Buffer
		if code := up(s, nil, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "aimd --foreground") {
			t.Errorf("up = %d, stderr %q", code, stderr.String())
		}
		if _, err := os.Stat(unit); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("unit installed without systemd: %v", err)
		}
	})
	t.Run("no user session", func(t *testing.T) {
		fs := &fakeSystemd{fail: "daemon-reload", failOut: "Failed to connect to bus: No medium found"}
		s, _ := linuxSys(t, &fakeSys{status: st, statusOK: true}, fs, userHome)
		var stdout, stderr bytes.Buffer
		if code := up(s, nil, &stdout, &stderr); code != 1 {
			t.Fatalf("up = %d", code)
		}
		for _, want := range []string{"systemctl --user daemon-reload", "Failed to connect to bus", "no systemd user session here"} {
			if !strings.Contains(stderr.String(), want) {
				t.Errorf("stderr lacks %q: %s", want, stderr.String())
			}
		}
		if got := fs.taken(); !slices.Equal(got, []string{"daemon-reload"}) {
			t.Errorf("went on after the failure: %q", got)
		}
	})
	t.Run("aimd doesn't answer", func(t *testing.T) {
		wait := upWait
		upWait = 0
		t.Cleanup(func() { upWait = wait })
		s, _ := linuxSys(t, &fakeSys{}, &fakeSystemd{}, userHome)
		var stdout, stderr bytes.Buffer
		if code := up(s, nil, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "journalctl --user -u aimd.service") {
			t.Errorf("up = %d, stderr %q", code, stderr.String())
		}
	})
}

func TestUpAndDownUsageOnLinux(t *testing.T) {
	_, userHome := setHome(t)
	s, _ := linuxSys(t, &fakeSys{}, &fakeSystemd{}, userHome)
	var stdout, stderr bytes.Buffer
	if code := up(s, []string{"--help"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "systemd user unit") {
		t.Errorf("up --help = %d, %q", code, stdout.String())
	}
	stdout.Reset()
	if code := down(s, []string{"--help"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "systemctl --user stop") {
		t.Errorf("down --help = %d, %q", code, stdout.String())
	}
	if code := up(s, []string{"extra"}, &stdout, &stderr); code != 2 {
		t.Errorf("up extra = %d", code)
	}
}

// A system with neither launchd nor systemd support says what to do.
func TestUpAndDownElsewhere(t *testing.T) {
	_, userHome := setHome(t)
	s := (&fakeSys{}).sys(t, userHome, testTools)
	s.goos = "freebsd"
	for name, cmd := range map[string]func(sys, []string, *bytes.Buffer, *bytes.Buffer) int{
		"up":   func(s sys, a []string, o, e *bytes.Buffer) int { return up(s, a, o, e) },
		"down": func(s sys, a []string, o, e *bytes.Buffer) int { return down(s, a, o, e) },
	} {
		var stdout, stderr bytes.Buffer
		if code := cmd(s, nil, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "aimd --foreground") {
			t.Errorf("%s = %d, stderr %q", name, code, stderr.String())
		}
	}
}
