// Package term brings a Claude Code session to the front in iTerm2 or
// Terminal.app, for aim go and the dashboard's Go buttons.
//
// # Focusing an interactive session
//
// No plugin is needed. From the session's pid:
//
//  1. its controlling terminal, from `ps -o tty= -p <pid>` (e.g. ttys021,
//     which becomes /dev/ttys021);
//  2. the iTerm2 session whose `tty` property is that path, found with
//     AppleScript; iTerm2 is activated, then that session, its tab and its
//     window are selected;
//  3. failing that, the Terminal.app tab whose `tty` is that path:
//     Terminal is activated, then the tab is selected and its window
//     brought to the front.
//
// The app is activated before the window is brought forward, so macOS
// switches to the desktop the window is on and doesn't pull the window to
// the current one (focusOrder).
//
// When the pid has no terminal, or no tab has its tty, the session's
// ITERM_SESSION_ID (which the mod may report, as w0t1p0:<guid>) is a
// second key in iTerm2: the session whose `id` is the guid. It is
// optional. Neither app is ever launched to look for a session.
//
// # Opening a background session
//
// `claude attach <id>` opens a background session in a terminal. It is
// typed into the shell of a new iTerm2 tab (default profile) when iTerm2
// is running, else of a new Terminal.app window (`do script`, which
// launches Terminal if need be), so that ← (back to Agent View) and
// Ctrl+Z (back to the shell) work as `claude attach --help` describes,
// and the tab stays a normal shell afterwards. A tab made to run the
// command alone would have no shell to drop back to.
//
// # Safety
//
// The scripts are constants. The values they act on arrive as arguments
// (internal/osascript), never as script text, and each is validated first:
// a tty must match ^/dev/ttys[0-9]+$, an iTerm2 session id must be a guid,
// a job id must match the `claude agents --json` id format (8 lower-case
// hex digits), and claude's path must be absolute and plain (it is
// resolved at startup, never taken from a request). The attach command is
// built from those two alone. Both apps are addressed by bundle id.
package term

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/osascript"
)

// Bundle ids, which the scripts address the apps by, and the apps' names
// as results report them.
const (
	ITermBundleID    = "com.googlecode.iterm2"
	TerminalBundleID = "com.apple.Terminal"

	AppITerm    = "iTerm2"
	AppTerminal = "Terminal"
)

// Validation patterns.
var (
	// ttyRE is a macOS pseudo-terminal device, as iTerm2's `tty` property
	// spells it.
	ttyRE = regexp.MustCompile(`^/dev/ttys[0-9]{1,6}$`)
	// jobIDRE is a background job's `id` in `claude agents --json`: 8
	// lower-case hex digits.
	jobIDRE = regexp.MustCompile(`^[0-9a-f]{8}$`)
	// guidRE is an iTerm2 session's `id`, the part of ITERM_SESSION_ID
	// after the colon.
	guidRE = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)
	// claudePathRE is the characters claude's path may have: it is typed
	// into a shell inside single quotes, so no quote, backslash, dollar,
	// backtick or control character.
	claudePathRE = regexp.MustCompile(`^/[A-Za-z0-9._+@/ -]+$`)
)

// ValidTTY reports whether s is a tty path the scripts accept.
func ValidTTY(s string) bool { return ttyRE.MatchString(s) }

// ValidJobID reports whether s is a background job id the scripts accept.
func ValidJobID(s string) bool { return jobIDRE.MatchString(s) }

// SessionGUID returns the iTerm2 session id in an ITERM_SESSION_ID value
// ("w0t1p0:<guid>", or the guid alone), or "" when it has none.
func SessionGUID(itermSessionID string) string {
	g := itermSessionID
	if i := strings.LastIndexByte(g, ':'); i >= 0 {
		g = g[i+1:]
	}
	if !guidRE.MatchString(g) {
		return ""
	}
	return strings.ToUpper(g)
}

// CheckClaudePath reports why p can't be used as claude's path in the
// attach command, or nil if it can: it must be absolute, clean and made
// only of letters, digits, spaces and . _ + @ / -.
func CheckClaudePath(p string) error {
	switch {
	case p == "":
		return errors.New("claude wasn't found on PATH when aimd started")
	case !filepath.IsAbs(p) || filepath.Clean(p) != p:
		return fmt.Errorf("claude's path %q isn't absolute and clean", p)
	case !claudePathRE.MatchString(p):
		return fmt.Errorf("claude's path %q has characters aim won't type into a shell", p)
	}
	return nil
}

// AttachCommand is the shell command that opens a background job:
// '<claude>' attach <jobID>. Both values are validated.
func AttachCommand(claude, jobID string) (string, error) {
	if err := CheckClaudePath(claude); err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if !ValidJobID(jobID) {
		return "", fmt.Errorf("%w: job id %q isn't 8 lower-case hex digits", ErrInvalid, jobID)
	}
	return "'" + claude + "' attach " + jobID, nil
}

// Errors. Each is returned wrapped, with details.
var (
	// ErrInvalid: a value failed validation, so no script ran.
	ErrInvalid = errors.New("refused an invalid value")
	// ErrNoTTY: the process has no controlling terminal (ps says ??).
	ErrNoTTY = errors.New("the process has no terminal")
	// ErrNoProcess: ps knows no such process.
	ErrNoProcess = errors.New("no such process")
	// ErrNotRunning: neither iTerm2 nor Terminal is running, so neither
	// has a session to focus.
	ErrNotRunning = errors.New("neither iTerm2 nor Terminal is running")
	// ErrNotFound: no iTerm2 or Terminal tab has the tty (or the iTerm2
	// session id). It is wrapped as "no iTerm2 or Terminal tab has tty
	// /dev/ttys021".
	ErrNotFound = errors.New("no iTerm2 or Terminal tab has")
)

// TTYs finds a process's controlling terminal.
type TTYs interface {
	// TTY returns the pid's terminal as a path (/dev/ttys021), ErrNoTTY
	// if it has none, or ErrNoProcess if there is no such process.
	TTY(ctx context.Context, pid int) (string, error)
}

// DefaultPSTimeout bounds one ps call when PS.Timeout is zero. ps takes a
// fraction of a second, but was killed at 5 seconds with the load
// average above 80.
const DefaultPSTimeout = 10 * time.Second

// PS is TTYs with `/bin/ps -o tty= -p <pid>`, and takes process
// snapshots (Procs).
type PS struct {
	// Timeout bounds one call; zero means DefaultPSTimeout.
	Timeout time.Duration
}

var _ TTYs = PS{}

func (p PS) timeout() time.Duration {
	if p.Timeout <= 0 {
		return DefaultPSTimeout
	}
	return p.Timeout
}

// TTY implements TTYs.
func (p PS) TTY(ctx context.Context, pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("%w: pid %d", ErrInvalid, pid)
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout())
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/ps", "-o", "tty=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && ctx.Err() == nil {
			return "", fmt.Errorf("%w: pid %d", ErrNoProcess, pid)
		}
		return "", fmt.Errorf("ps: %w", err)
	}
	return ParseTTY(string(out))
}

// ParseTTY turns the output of `ps -o tty=` (e.g. "ttys021 ") into a tty
// path. "??" or nothing means no terminal (ErrNoTTY).
func ParseTTY(out string) (string, error) {
	v := strings.TrimSpace(out)
	switch v {
	case "", "?", "??", "-":
		return "", ErrNoTTY
	}
	tty := "/dev/" + v
	if !ValidTTY(tty) {
		return "", fmt.Errorf("%w: tty %q", ErrInvalid, v)
	}
	return tty, nil
}

// Script answers.
const (
	answerOK         = "ok"
	answerOKTTY      = "ok tty"
	answerOKID       = "ok id"
	answerNotFound   = "not-found"
	answerNotRunning = "not-running"
)

// runningScript answers "running" when the app whose bundle id is argv 1
// runs, "not-running" when it doesn't or isn't installed.
//
// It is asked before a script that tells iTerm2 anything. osascript
// compiles a whole script before it runs a line of it, and a `tell
// application id "…"` block needs the app's dictionary to compile: on a
// Mac without iTerm2 the iTerm2 scripts fail as a "syntax error: Can't get
// application id" before their own "is running" line is reached, and Go
// failed for every session. This script names no app in its text, so it
// compiles anywhere. Terminal.app is part of macOS, so its scripts need no
// such check.
const runningScript = `on run argv
	try
		if application id (item 1 of argv) is running then return "running"
	end try
	return "not-running"
end run`

// focusOrder: both focus scripts activate the app first, wait until it
// is the front app (at most two seconds), and only then bring the window
// forward.
//
// A window brought forward while its app is in the background is put on
// the desktop (Space) the person is looking at: the terminal is pulled
// out of the desktop they arranged it on. Brought forward by the active
// app, as its Window menu does, macOS switches to the window's desktop
// instead, when "When switching to an application, switch to a Space with
// open windows for the application" is on in System Settings. Measured
// with Terminal.app on macOS 26: raise-then-activate pulled the window
// over, activate-then-raise switched desktops.
//
// The app is activated only once the session is found, so a session that
// isn't there doesn't bring the app forward.

// focusITermScript activates iTerm2, then selects the iTerm2 session whose
// tty is argv 1, or failing that whose id is argv 2 (either may be empty),
// with its tab and window. It answers "ok tty", "ok id", "not-found" or
// "not-running"; it never launches iTerm2.
const focusITermScript = `on run argv
	set wantTTY to item 1 of argv
	set wantID to item 2 of argv
	if not (application id "` + ITermBundleID + `" is running) then return "not-running"
	tell application id "` + ITermBundleID + `"
		if wantTTY is not "" then
			repeat with w in windows
				repeat with t in tabs of w
					repeat with s in sessions of t
						if (tty of s) is wantTTY then
							activate
							repeat 40 times
								if frontmost then exit repeat
								delay 0.05
							end repeat
							if miniaturized of w then set miniaturized of w to false
							select w
							select t
							select s
							return "ok tty"
						end if
					end repeat
				end repeat
			end repeat
		end if
		if wantID is not "" then
			repeat with w in windows
				repeat with t in tabs of w
					repeat with s in sessions of t
						if (id of s) is wantID then
							activate
							repeat 40 times
								if frontmost then exit repeat
								delay 0.05
							end repeat
							if miniaturized of w then set miniaturized of w to false
							select w
							select t
							select s
							return "ok id"
						end if
					end repeat
				end repeat
			end repeat
		end if
	end tell
	return "not-found"
end run`

// focusTerminalScript activates Terminal, then selects the Terminal.app
// tab whose tty is argv 1 and makes its window Terminal's front one. It
// answers "ok tty", "not-found" or "not-running"; it never launches
// Terminal, and a tty no tab has leaves Terminal where it was.
//
// The order is what keeps a window on its own desktop (focusOrder).
const focusTerminalScript = `on run argv
	set wantTTY to item 1 of argv
	if not (application id "` + TerminalBundleID + `" is running) then return "not-running"
	tell application id "` + TerminalBundleID + `"
		repeat with w in windows
			repeat with t in tabs of w
				if (tty of t) is wantTTY then
					activate
					repeat 40 times
						if frontmost then exit repeat
						delay 0.05
					end repeat
					if miniaturized of w then set miniaturized of w to false
					set selected of t to true
					set frontmost of w to true
					return "ok tty"
				end if
			end repeat
		end repeat
	end tell
	return "not-found"
end run`

// attachITermScript, when iTerm2 is running, activates it, makes a new
// tab with the default profile in the current window (or a new window
// when there is none), and types argv 1 into its shell. It answers "ok",
// or "not-running" without launching iTerm2.
//
// The text goes to the session of the tab (or window) this script made,
// which the create command returns, and never to whichever session is
// current afterwards: when the new tab isn't the current one, that is
// some other session, and the command would be typed into it. If the
// create command returns nothing the script fails and types nothing.
const attachITermScript = `on run argv
	set cmd to item 1 of argv
	if not (application id "` + ITermBundleID + `" is running) then return "not-running"
	tell application id "` + ITermBundleID + `"
		activate
		if (count of windows) is 0 then
			set made to (create window with default profile)
		else
			set w to current window
			if w is missing value then set w to window 1
			tell w to set made to (create tab with default profile)
		end if
		set s to current session of made
		tell s to write text cmd
	end tell
	return "ok"
end run`

// attachTerminalScript types argv 1 into the shell of a new Terminal.app
// window (do script; Terminal is launched if need be) and activates
// Terminal. It answers "ok". do script without "in" always makes its own
// window and runs there, so it has no existing tab to mistake for it.
const attachTerminalScript = `on run argv
	set cmd to item 1 of argv
	tell application id "` + TerminalBundleID + `"
		do script cmd
		activate
	end tell
	return "ok"
end run`

// Scripts are the AppleScripts this package runs, for tests and review.
type Scripts struct {
	Running, FocusITerm, FocusTerminal, AttachITerm, AttachTerminal string
}

// AllScripts returns the scripts.
func AllScripts() Scripts {
	return Scripts{
		Running:        runningScript,
		FocusITerm:     focusITermScript,
		FocusTerminal:  focusTerminalScript,
		AttachITerm:    attachITermScript,
		AttachTerminal: attachTerminalScript,
	}
}

// Terminal focuses and opens sessions in iTerm2 or Terminal.app.
type Terminal struct {
	// Runner runs the scripts; nil means osascript.Exec{}.
	Runner osascript.Runner
	// TTYs finds a pid's terminal; nil means PS{}.
	TTYs TTYs
	// Claude is claude's absolute path, resolved at startup (never from a
	// request). Empty means Attach refuses.
	Claude string
}

// Focused says how a session was found.
type Focused struct {
	// App is AppITerm or AppTerminal: where the session's tab is.
	App string
	// TTY is the session's terminal, when ps gave one.
	TTY string
	// Via is "tty" or "iterm_session_id": which key matched.
	Via string
}

// Attached says how a background session was opened.
type Attached struct {
	// App is AppITerm (a new tab) or AppTerminal (a new window).
	App string
	// Command is what was typed into the new tab's shell.
	Command string
}

// running reports whether the app with this bundle id runs (runningScript).
func (t Terminal) running(ctx context.Context, bundleID string) (bool, error) {
	out, err := t.runner().Run(ctx, runningScript, bundleID)
	if err != nil {
		return false, err
	}
	switch out {
	case "running":
		return true, nil
	case answerNotRunning:
		return false, nil
	}
	return false, fmt.Errorf("osascript: unexpected answer %q", out)
}

func (t Terminal) runner() osascript.Runner {
	if t.Runner == nil {
		return osascript.Exec{}
	}
	return t.Runner
}

func (t Terminal) ttys() TTYs {
	if t.TTYs == nil {
		return PS{}
	}
	return t.TTYs
}

// Focus brings the tab of the process pid to the front: the iTerm2
// session with its tty or (failing that) with the given ITERM_SESSION_ID,
// else the Terminal.app tab with its tty. pid may be 0 and itermSessionID
// empty, but not both.
func (t Terminal) Focus(ctx context.Context, pid int, itermSessionID string) (Focused, error) {
	var f Focused
	var ttyErr error
	if pid > 0 {
		f.TTY, ttyErr = t.ttys().TTY(ctx, pid)
		if ttyErr != nil {
			f.TTY = ""
		}
	}
	guid := SessionGUID(itermSessionID)
	if f.TTY == "" && guid == "" {
		switch {
		case ttyErr != nil:
			return f, ttyErr
		case itermSessionID != "":
			return f, fmt.Errorf("%w: iTerm2 session id %q", ErrInvalid, itermSessionID)
		}
		return f, fmt.Errorf("%w: no pid and no iTerm2 session id", ErrInvalid)
	}
	if f.TTY != "" && !ValidTTY(f.TTY) { // a TTYs that didn't validate
		return f, fmt.Errorf("%w: tty %q", ErrInvalid, f.TTY)
	}
	// iTerm2 first: it has both keys. Its script is run only when iTerm2
	// runs: it can't be compiled on a Mac without iTerm2 (runningScript).
	out := answerNotRunning
	iterm, err := t.running(ctx, ITermBundleID)
	if err != nil {
		return f, err
	}
	if iterm {
		if out, err = t.runner().Run(ctx, focusITermScript, f.TTY, guid); err != nil {
			return f, err
		}
	}
	switch out {
	case answerOKTTY:
		f.App, f.Via = AppITerm, "tty"
		return f, nil
	case answerOKID:
		f.App, f.Via = AppITerm, "iterm_session_id"
		return f, nil
	case answerNotFound, answerNotRunning:
	default:
		return f, fmt.Errorf("osascript: unexpected answer %q", out)
	}
	running := out != answerNotRunning

	// Then Terminal.app, which only has the tty.
	if f.TTY != "" {
		out, err := t.runner().Run(ctx, focusTerminalScript, f.TTY)
		if err != nil {
			return f, err
		}
		switch out {
		case answerOKTTY:
			f.App, f.Via = AppTerminal, "tty"
			return f, nil
		case answerNotFound:
			running = true
		case answerNotRunning:
		default:
			return f, fmt.Errorf("osascript: unexpected answer %q", out)
		}
	}
	switch {
	case !running:
		return f, ErrNotRunning
	case f.TTY != "":
		return f, fmt.Errorf("%w tty %s", ErrNotFound, f.TTY)
	default:
		return f, fmt.Errorf("%w iTerm2 session id %s", ErrNotFound, guid)
	}
}

// Attach opens the background job jobID with `claude attach <jobID>`: in a
// new iTerm2 tab when iTerm2 is running, else in a new Terminal.app
// window.
func (t Terminal) Attach(ctx context.Context, jobID string) (Attached, error) {
	cmd, err := AttachCommand(t.Claude, jobID)
	if err != nil {
		return Attached{}, err
	}
	a := Attached{App: AppITerm, Command: cmd}
	// As in Focus: the iTerm2 script only when iTerm2 runs (runningScript).
	out := answerNotRunning
	iterm, err := t.running(ctx, ITermBundleID)
	if err == nil && iterm {
		out, err = t.runner().Run(ctx, attachITermScript, cmd)
	}
	if err == nil && out == answerNotRunning {
		a.App = AppTerminal
		out, err = t.runner().Run(ctx, attachTerminalScript, cmd)
	}
	if err != nil {
		return a, err
	}
	if out != answerOK {
		return a, fmt.Errorf("osascript: unexpected answer %q", out)
	}
	return a, nil
}
