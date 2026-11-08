package term

// Which terminal app a session runs in, read from the process
// tree so aimd can tell on every session poll without AppleScript or the
// Automation permission. The dashboard tags each session with it and
// offers Go only where Go can switch to an existing tab: iTerm2 and
// Terminal.app.
//
// One snapshot per poll, `ps -A -o pid=,ppid=,tty=,comm=` (Procs), then
// for each session's pid (Procs.AppOf):
//
//  1. no such process, or no tty (ps says ??): none;
//  2. else the process that made the session's pty, found as the nearest
//     ancestor on another tty (usually none): the shell and login between
//     them share the session's tty. As recorded in testdata/ps, that is
//     claude ← -zsh ← login ← Terminal (Terminal.app), and
//     claude ← -zsh ← login ← iTermServer-<version> ← iTerm2 (iTerm2 keeps
//     its ptys in iTermServer, which outlives an iTerm2 restart);
//  3. that process is iTerm2 or iTermServer: iterm2; Terminal: terminal;
//     a tmux server: tmux (Go can't pick a tmux pane); claude's
//     background pty host (claude bg-pty-host): none; anything else:
//     other, named by its .app bundle (Visual Studio Code, Ghostty, Warp,
//     ...) or its command.
//
// Asking for the pty's maker rather than for any terminal app up the tree
// gets nesting right: claude in tmux in an iTerm2 tab is tmux, and claude
// in VS Code started from a Terminal tab is other, since in neither does
// an iTerm2 or Terminal tab have claude's tty.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"strconv"
	"strings"
)

// Terminal kinds, as GET /api/sessions spells them (api.Session.Terminal).
const (
	KindITerm2   = "iterm2"
	KindTerminal = "terminal"
	KindTmux     = "tmux"
	KindOther    = "other"
	KindNone     = "none"
)

// App is the terminal app a process runs in.
type App struct {
	// Kind is KindITerm2, KindTerminal, KindTmux, KindOther or KindNone.
	Kind string
	// Name is the app's name, for people: AppITerm, AppTerminal or "tmux",
	// or for KindOther the app's name when the process tree shows one
	// ("Visual Studio Code", "Ghostty", "SSH"). Empty for KindNone.
	Name string
}

// NoTerminal is a process without a terminal: no pid, no such process, no
// tty, or a background session.
var NoTerminal = App{Kind: KindNone}

// Switchable reports whether Go can switch to the app's tab: iTerm2 and
// Terminal.app, the apps Terminal.Focus looks in.
func (a App) Switchable() bool { return a.Kind == KindITerm2 || a.Kind == KindTerminal }

// Proc is one process in a snapshot.
type Proc struct {
	PID, PPID int
	// TTY is ps's tty column: ttys021, or ?? for none.
	TTY string
	// Comm is ps's comm column: on macOS the process's argv[0], usually
	// its path, or a title it set ("claude bg-pty-host").
	Comm string
}

// Procs is a process snapshot, by pid.
type Procs map[int]Proc

// psProcArgs is the ps command line Procs reads: every process, with no
// header. comm is last because it may contain spaces.
var psProcArgs = []string{"-A", "-o", "pid=,ppid=,tty=,comm="}

// Procs takes a snapshot of every process with `/bin/ps -A -o
// pid=,ppid=,tty=,comm=`. A run cut off by its timeout returns an error
// that wraps context.DeadlineExceeded and says so, rather than ps's
// "signal: killed".
func (p PS) Procs(ctx context.Context) (Procs, error) {
	timeout := p.timeout()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/ps", psProcArgs...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("ps: timed out after %s: %w", timeout, ctx.Err())
	}
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return nil, fmt.Errorf("ps: %w: %s", err, msg)
		}
		return nil, fmt.Errorf("ps: %w", err)
	}
	procs := ParseProcs(string(out))
	if len(procs) == 0 {
		return nil, fmt.Errorf("ps listed no processes")
	}
	return procs, nil
}

// ParseProcs reads the output of `ps -o pid=,ppid=,tty=,comm=`. A line it
// can't read is skipped.
func ParseProcs(out string) Procs {
	procs := Procs{}
	for line := range strings.Lines(out) {
		pidS, rest := nextField(line)
		ppidS, rest := nextField(rest)
		tty, rest := nextField(rest)
		comm := strings.TrimSpace(rest)
		pid, err1 := strconv.Atoi(pidS)
		ppid, err2 := strconv.Atoi(ppidS)
		if err1 != nil || err2 != nil || pid < 0 || ppid < 0 || tty == "" || comm == "" {
			continue
		}
		procs[pid] = Proc{PID: pid, PPID: ppid, TTY: tty, Comm: comm}
	}
	return procs
}

// nextField splits off s's first blank-separated field.
func nextField(s string) (field, rest string) {
	s = strings.TrimLeft(s, " \t")
	if i := strings.IndexAny(s, " \t\r\n"); i >= 0 {
		return s[:i], s[i:]
	}
	return s, ""
}

// hasTTY reports whether a ps tty column names a terminal.
func hasTTY(tty string) bool {
	switch tty {
	case "", "?", "??", "-":
		return false
	}
	return true
}

// maxDepth bounds a walk up the tree, which a snapshot taken while
// processes come and go could in principle make loop.
const maxDepth = 64

// AppOf says which terminal app the process pid runs in.
func (p Procs) AppOf(pid int) App {
	self, ok := p[pid]
	if pid <= 0 || !ok || !hasTTY(self.TTY) {
		return NoTerminal
	}
	cur := self
	for range maxDepth {
		if cur.PPID <= 0 || cur.PPID == cur.PID {
			break
		}
		parent, ok := p[cur.PPID]
		if !ok {
			break
		}
		if parent.TTY != self.TTY {
			return ptyMaker(parent)
		}
		cur = parent
	}
	// No ancestor on another tty: the tree is cut short.
	return App{Kind: KindOther}
}

// ptyMaker classifies the process that made a session's pty.
func ptyMaker(p Proc) App {
	base := path.Base(p.Comm)
	switch {
	case base == "iTerm2" || strings.HasPrefix(base, "iTermServer"):
		return App{Kind: KindITerm2, Name: AppITerm}
	case base == "Terminal":
		return App{Kind: KindTerminal, Name: AppTerminal}
	case base == "tmux" || strings.HasPrefix(base, "tmux:") || strings.HasPrefix(base, "tmux "):
		return App{Kind: KindTmux, Name: "tmux"}
	case strings.HasPrefix(p.Comm, "claude bg-"):
		// claude bg-pty-host gives each background job a pty of its own;
		// no app shows it.
		return NoTerminal
	case p.PID == 1:
		// launchd: the shell that had the tab is gone. The tab may still
		// be there, but nothing says whose it is.
		return App{Kind: KindOther}
	}
	return App{Kind: KindOther, Name: appName(p.Comm)}
}

// appName names the program at comm for people: the outermost .app bundle
// on its path ("/Applications/Visual Studio Code.app/Contents/Frameworks/
// Code Helper (Plugin).app/..." is "Visual Studio Code"), "SSH" for sshd,
// else the command's last path element.
func appName(comm string) string {
	if i := strings.Index(comm, ".app/"); i > 0 {
		return path.Base(comm[:i])
	}
	base := path.Base(comm)
	if strings.HasPrefix(base, "sshd") {
		return "SSH"
	}
	return base
}
