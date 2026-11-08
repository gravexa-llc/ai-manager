package term

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixture reads a recorded ps snapshot from testdata/ps.
func fixture(t *testing.T, name string) Procs {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "ps", name))
	if err != nil {
		t.Fatal(err)
	}
	return ParseProcs(string(b))
}

var (
	iterm2   = App{Kind: KindITerm2, Name: AppITerm}
	terminal = App{Kind: KindTerminal, Name: AppTerminal}
	tmux     = App{Kind: KindTmux, Name: "tmux"}
)

func other(name string) App { return App{Kind: KindOther, Name: name} }

// The lead's Mac on 2026-11-06: sessions in Terminal.app and iTerm2
// (iTerm2 translocated, its ptys in iTermServer), and claude's background
// job host.
func TestAppOfRecordedMac(t *testing.T) {
	procs := fixture(t, "mac-2026-11-06.txt")
	if len(procs) != 63 {
		t.Errorf("parsed %d processes, want 63", len(procs))
	}
	for _, c := range []struct {
		name string
		pid  int
		want App
	}{
		{"the lead, in iTerm2", 91470, iterm2},
		{"a shell in another iTerm2 tab", 92697, iterm2},
		{"a session in Terminal", 69087, terminal},
		{"another in Terminal", 76780, terminal},
		{"Terminal's login", 68858, terminal},
		{"a background job's process, on claude bg-pty-host's pty", 53221, NoTerminal},
		{"a spare background process, no tty", 95563, NoTerminal},
		{"claude's background daemon", 43628, NoTerminal},
		{"Terminal itself", 669, NoTerminal},
		{"no such process", 12345, NoTerminal},
		{"no pid", 0, NoTerminal},
		{"a negative pid", -1, NoTerminal},
	} {
		if got := procs.AppOf(c.pid); got != c.want {
			t.Errorf("%s (pid %d): %+v, want %+v", c.name, c.pid, got, c.want)
		}
	}
	// Every claude session in the snapshot is in a tab Go can switch to.
	for pid, p := range procs {
		if p.Comm == "claude" && !procs.AppOf(pid).Switchable() {
			t.Errorf("claude %d [%s]: %+v", pid, p.TTY, procs.AppOf(pid))
		}
	}
}

func TestAppOfOtherApps(t *testing.T) {
	procs := fixture(t, "synthetic-other-apps.txt")
	for _, c := range []struct {
		name string
		pid  int
		want App
	}{
		{"in a tmux pane, tmux server under launchd", 602, tmux},
		{"in a tmux pane, tmux server in an iTerm2 tab", 702, tmux},
		{"the tmux client, in the iTerm2 tab", 512, iterm2},
		{"VS Code's terminal", 803, other("Visual Studio Code")},
		{"VS Code's extension, no tty", 804, NoTerminal},
		{"Ghostty", 903, other("Ghostty")},
		{"Warp", 1002, other("Warp")},
		{"SSH", 1102, other("SSH")},
		{"VS Code started from a Terminal tab", 1206, other("Visual Studio Code")},
		{"script(1) in a Terminal tab", 1501, other("script")},
		{"a subshell in a Terminal tab", 1601, terminal},
		{"iTermServer outliving iTerm2", 1703, iterm2},
		{"iTerm2 without a server", 1803, iterm2},
		{"an orphan: its shell is gone", 1400, other("")},
		{"its parent isn't in the snapshot", 1900, other("")},
		{"a loop", 2000, other("")},
	} {
		if got := procs.AppOf(c.pid); got != c.want {
			t.Errorf("%s (pid %d): %+v, want %+v", c.name, c.pid, got, c.want)
		}
	}
}

func TestParseProcs(t *testing.T) {
	procs := ParseProcs("" +
		"    1     0 ??       /sbin/launchd\n" +
		"56805 56776 ??       ~/Library/Application Support/iTerm2/iTermServer-3.6.6\n" +
		"53152 43628 ??       claude bg-pty-host  \r\n" +
		"\n" +
		"# a comment\n" +
		"  x     1 ??       not a pid\n" +
		"   12     1\n" +
		"   13    -1 ??       negative ppid\n" +
		"\t14\t1\tttys001\tclaude")
	want := Procs{
		1:     {PID: 1, PPID: 0, TTY: "??", Comm: "/sbin/launchd"},
		56805: {PID: 56805, PPID: 56776, TTY: "??", Comm: "~/Library/Application Support/iTerm2/iTermServer-3.6.6"},
		53152: {PID: 53152, PPID: 43628, TTY: "??", Comm: "claude bg-pty-host"},
		14:    {PID: 14, PPID: 1, TTY: "ttys001", Comm: "claude"},
	}
	if len(procs) != len(want) {
		t.Errorf("parsed %d processes, want %d: %+v", len(procs), len(want), procs)
	}
	for pid, w := range want {
		if procs[pid] != w {
			t.Errorf("pid %d: %+v, want %+v", pid, procs[pid], w)
		}
	}
	if len(ParseProcs("")) != 0 {
		t.Error("empty output has processes")
	}
}

func TestAppName(t *testing.T) {
	vscodeHelper := "/Applications/Visual Studio Code.app/Contents/Frameworks/" +
		"Code Helper (Plugin).app/Contents/MacOS/Code Helper (Plugin)"
	for comm, want := range map[string]string{
		vscodeHelper: "Visual Studio Code",
		"/Applications/Cursor.app/Contents/MacOS/Cursor":       "Cursor",
		"/Applications/WezTerm.app/Contents/MacOS/wezterm-gui": "WezTerm",
		"/usr/sbin/sshd":            "SSH",
		"sshd: someone@ttys003":     "SSH",
		"/usr/libexec/sshd-session": "SSH",
		"/usr/bin/script":           "script",
		"node":                      "node",
	} {
		if got := appName(comm); got != want {
			t.Errorf("appName(%q) = %q, want %q", comm, got, want)
		}
	}
}

func TestSwitchable(t *testing.T) {
	for _, a := range []App{iterm2, terminal} {
		if !a.Switchable() {
			t.Errorf("%+v isn't switchable", a)
		}
	}
	for _, a := range []App{tmux, other("Ghostty"), other(""), NoTerminal, {}} {
		if a.Switchable() {
			t.Errorf("%+v is switchable", a)
		}
	}
}

// The real ps: this test's own process is in the snapshot, with its
// parent, and classifying it doesn't fail whatever it runs in.
func TestPSProcs(t *testing.T) {
	procs, err := PS{}.Procs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	self, ok := procs[os.Getpid()]
	if !ok || self.PPID != os.Getppid() || self.Comm == "" {
		t.Errorf("own process: %+v (found %v), want ppid %d", self, ok, os.Getppid())
	}
	switch k := procs.AppOf(os.Getpid()).Kind; k {
	case KindITerm2, KindTerminal, KindTmux, KindOther, KindNone:
	default:
		t.Errorf("own process: kind %q", k)
	}
}

// A snapshot cut off by its deadline says it timed out, and wraps
// context.DeadlineExceeded, rather than reporting ps's "signal: killed".
// The deadline here has passed before ps starts, so nothing runs.
func TestPSProcsTimeout(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, err := PS{Timeout: 3 * time.Second}.Procs(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "ps: timed out after 3s") {
		t.Errorf("err = %v, want a timeout after 3s", err)
	}
	if got := (PS{}).timeout(); got != DefaultPSTimeout {
		t.Errorf("PS{} timeout = %v, want DefaultPSTimeout (%v)", got, DefaultPSTimeout)
	}
}
