package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/gravexa-llc/ai-manager/deploy"
	"github.com/gravexa-llc/ai-manager/internal/config"
)

// On Linux `aim up` and `aim down` do with a systemd user unit what they do
// with a launchd agent on a Mac: the unit starts aimd at login and restarts
// it if it fails. Everything outside the process goes through sys, so tests
// never install anything or run systemctl.

const upUsageLinux = `usage: aim up [--print]

Installs aimd as a systemd user unit (~/.config/systemd/user/aimd.service),
enables it and starts it. It resolves claude, gh and git where your shell
finds them, since a user unit's PATH is only the system one; run it again
after moving them.

A user unit stops when your last login session ends, unless lingering is on
for your account: loginctl enable-linger
(over ssh, usually: sudo loginctl enable-linger $USER)

  --print   print the unit instead of installing it
`

const downUsageLinux = `usage: aim down

Stops aimd (systemctl --user stop) and removes the systemd user unit that
` + "`aim up`" + ` installed, so it doesn't start again at login. Its data in
$AIM_HOME stays.
`

// systemctlTimeout bounds one systemctl or loginctl call.
const systemctlTimeout = 30 * time.Second

// journalHint is where systemd keeps what aimd.log doesn't get.
const journalHint = "journalctl --user -u " + deploy.SystemdUnit

// noSystemd is `aim up`'s answer on a Linux without systemd's user manager.
const noSystemd = "systemctl isn't on PATH, so there is no systemd user manager to run aimd; run `aimd --foreground` under your own supervisor instead"

// unitPath is where the user unit goes: systemd reads user units from
// $XDG_CONFIG_HOME/systemd/user, which is ~/.config/systemd/user unless
// the variable names another absolute folder.
func unitPath(userHome string) string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if !filepath.IsAbs(base) {
		base = filepath.Join(userHome, ".config")
	}
	return filepath.Join(base, "systemd", "user", deploy.SystemdUnit)
}

// unitBare matches text systemd reads as one word without quotes.
var unitBare = regexp.MustCompile(`^[A-Za-z0-9_@+=:,./-]+$`)

// unitWord writes s as one word of a unit file setting: as it is when it
// has nothing systemd treats specially, else in double quotes with
// backslashes and quotes escaped. A % is doubled, since systemd expands
// specifiers (%h, %u, ...) in every setting the template uses. A control
// character is refused: a line break would start another setting.
func unitWord(s string) (string, error) {
	if err := unitText(s); err != nil {
		return "", err
	}
	s = strings.ReplaceAll(s, "%", "%%")
	if unitBare.MatchString(strings.ReplaceAll(s, "%%", "")) {
		return s, nil
	}
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`, nil
}

// unitText refuses text a unit file can't hold on one line.
func unitText(s string) error {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%q has a control character, which a systemd unit can't hold", s)
		}
	}
	return nil
}

// unitExec writes a program path for ExecStart: one word. systemd doesn't
// expand variables in the program, so a $ stays as it is, and it refuses a
// program whose path has a quote or a backslash, however it is written.
func unitExec(program string) (string, error) {
	if !filepath.IsAbs(program) {
		return "", fmt.Errorf("%s isn't an absolute path", program)
	}
	if strings.ContainsAny(program, `"'\`) {
		return "", fmt.Errorf("systemd can't run a program whose path has a quote or a backslash: %s", program)
	}
	return unitWord(program)
}

// unitDir writes a path for WorkingDirectory, which takes the rest of the
// line as it is: no quotes, only % doubled.
func unitDir(dir string) (string, error) {
	if err := unitText(dir); err != nil {
		return "", err
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("%s isn't an absolute path", dir)
	}
	return strings.ReplaceAll(dir, "%", "%%"), nil
}

// unitEnv writes one Environment assignment.
func unitEnv(key, value string) (string, error) {
	return unitWord(key + "=" + value)
}

// renderUnit fills the unit template.
func renderUnit(d plistData) ([]byte, error) {
	tmpl, err := template.New("unit").Funcs(template.FuncMap{
		"exec": unitExec, "dir": unitDir, "env": unitEnv,
	}).Parse(deploy.UnitTemplate)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, d); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// systemctlError is a systemctl command that failed.
type systemctlError struct {
	verb string // daemon-reload, enable, restart, ...
	out  string
	err  error
}

func (e *systemctlError) Error() string {
	msg := fmt.Sprintf("systemctl --user %s: %v", e.verb, e.err)
	if e.out != "" {
		msg += ": " + e.out
	}
	// What systemctl says when the account has no user manager to talk
	// to: a shell from su or sudo, or a container.
	if strings.Contains(e.out, "Failed to connect to bus") {
		msg += " (no systemd user session here: log in directly rather than through su or sudo, or run `aimd --foreground` under your own supervisor)"
	}
	return msg
}

func (e *systemctlError) Unwrap() error { return e.err }

// runSystemctl runs one `systemctl --user` command under its own timeout.
// A failure carries systemctl's output.
func runSystemctl(s sys, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), systemctlTimeout)
	defer cancel()
	out, err := s.systemctl(ctx, args...)
	if err != nil {
		return &systemctlError{verb: args[0], out: strings.TrimSpace(string(out)), err: err}
	}
	return nil
}

// unitActive reports whether systemd is running aimd's unit.
func unitActive(s sys) bool {
	return runSystemctl(s, "is-active", "--quiet", deploy.SystemdUnit) == nil
}

// upSystemd is `aim up` on Linux.
func upSystemd(s sys, args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("aim up", flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	printOnly := fl.Bool("print", false, "")
	if err := fl.Parse(args); err != nil || fl.NArg() > 0 {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, upUsageLinux)
			return 0
		}
		fmt.Fprint(stderr, upUsageLinux)
		return 2
	}

	plan, err := planAgent(s)
	if err != nil {
		fmt.Fprintf(stderr, "aim up: %v\n", err)
		return 1
	}
	for _, w := range plan.warnings {
		fmt.Fprintf(stderr, "aim up: warning: %s\n", w)
	}
	unit, err := renderUnit(plan.data)
	if err != nil {
		fmt.Fprintf(stderr, "aim up: %v\n", err)
		return 1
	}
	if *printOnly {
		stdout.Write(unit)
		return 0
	}
	if _, err := s.lookPath("systemctl"); err != nil {
		fmt.Fprintf(stderr, "aim up: %s\n", noSystemd)
		return 1
	}

	cfg, err := config.Load(plan.home)
	if err != nil {
		fmt.Fprintf(stderr, "aim up: %v\n", err)
		return 1
	}
	userHome, err := s.userHome()
	if err != nil {
		fmt.Fprintf(stderr, "aim up: find home directory: %v\n", err)
		return 1
	}
	path := unitPath(userHome)
	// systemd needs the working directory to exist.
	if err := os.MkdirAll(plan.home, 0o700); err != nil {
		fmt.Fprintf(stderr, "aim up: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintf(stderr, "aim up: %v\n", err)
		return 1
	}
	if err := writeFileAtomic(path, unit, 0o644); err != nil {
		fmt.Fprintf(stderr, "aim up: write %s: %v\n", path, err)
		return 1
	}

	// daemon-reload reads the unit as it now is; restart starts aimd, or
	// stops and starts a running one, so a changed unit or binary takes
	// effect.
	for _, cmd := range [][]string{
		{"daemon-reload"},
		{"enable", deploy.SystemdUnit},
		{"restart", deploy.SystemdUnit},
	} {
		if err := runSystemctl(s, cmd...); err != nil {
			fmt.Fprintf(stderr, "aim up: %v\n", err)
			return 1
		}
	}

	url := fmt.Sprintf("http://127.0.0.1:%d", cfg.Port)
	if _, err := waitStatus(s, cfg.Port, upWait); err != nil {
		fmt.Fprintf(stderr, "aim up: aimd was started but doesn't answer on %s yet (%v); see %s, or `%s` for a crash at startup\n",
			url, err, config.LogPath(plan.home), journalHint)
		return 1
	}
	fmt.Fprintf(stdout, "aimd is running at %s (systemd user unit %s)\nOpen the dashboard with `aim open`: it signs your browser in.\n", url, path)
	if on, known := s.linger(); known && !on {
		fmt.Fprintln(stderr, "aim up: note: aimd stops when your last login session ends, and starts again at your next login. To keep it running, turn lingering on for your account: loginctl enable-linger (over ssh, usually: sudo loginctl enable-linger $USER)")
	}
	return 0
}

// downSystemd is `aim down` on Linux.
func downSystemd(s sys, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		if args[0] == "-h" || args[0] == "-help" || args[0] == "--help" {
			fmt.Fprint(stdout, downUsageLinux)
			return 0
		}
		fmt.Fprint(stderr, downUsageLinux)
		return 2
	}
	userHome, err := s.userHome()
	if err != nil {
		fmt.Fprintf(stderr, "aim down: find home directory: %v\n", err)
		return 1
	}
	path := unitPath(userHome)
	_, statErr := os.Stat(path)
	installed := statErr == nil
	if _, err := s.lookPath("systemctl"); err != nil {
		if installed {
			fmt.Fprintf(stderr, "aim down: systemctl isn't on PATH, so aim can't stop the unit at %s\n", path)
			return 1
		}
		fmt.Fprintln(stdout, "aimd wasn't running")
		return 0
	}

	wasActive := unitActive(s)
	if wasActive {
		if err := runSystemctl(s, "stop", deploy.SystemdUnit); err != nil {
			fmt.Fprintf(stderr, "aim down: %v\n", err)
			return 1
		}
	}
	if installed {
		// disable removes the link that starts aimd at login; it has to
		// run while the unit file is still there.
		if err := runSystemctl(s, "disable", deploy.SystemdUnit); err != nil {
			fmt.Fprintf(stderr, "aim down: %v\n", err)
			return 1
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(stderr, "aim down: remove %s: %v\n", path, err)
			return 1
		}
		if err := runSystemctl(s, "daemon-reload"); err != nil {
			fmt.Fprintf(stderr, "aim down: %v\n", err)
			return 1
		}
	}
	switch {
	case wasActive:
		fmt.Fprintln(stdout, "aimd stopped")
	case installed:
		fmt.Fprintln(stdout, "aimd wasn't running; removed its systemd unit")
	default:
		fmt.Fprintln(stdout, "aimd wasn't running")
	}
	return 0
}
