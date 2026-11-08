package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/gravexa-llc/ai-manager/deploy"
	"github.com/gravexa-llc/ai-manager/internal/config"
)

// launchdLogFile is where launchd sends aimd's stdout and stderr, next to
// aimd.log. With deploy.LaunchdEnv set, aimd logs only to aimd.log, so this
// holds what that doesn't get: a panic, or the error aimd stops with.
const launchdLogFile = "aimd.launchd.log"

// systemPath ends the PATH aimd runs with under launchd.
var systemPath = []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin"}

// tools are the programs aimd runs; `aim up` resolves them where the user's
// shell finds them, since launchd's PATH is only the system one.
var tools = []string{"claude", "gh", "git"}

// passEnv are environment variables copied into the plist when set, so
// aimd under launchd sees the same Claude config, ai-manager home and gh
// config as the shell that ran `aim up`.
var passEnv = []string{"AIM_HOME", "CLAUDE_CONFIG_DIR", "GH_CONFIG_DIR"}

// sys is everything the up / down / status commands touch outside the
// process, so tests never install anything or run launchctl.
type sys struct {
	lookPath   func(string) (string, error)
	executable func() (string, error)
	userHome   func() (string, error)
	uid        int
	// launchctl runs launchctl with args and returns its combined output.
	launchctl func(ctx context.Context, args ...string) ([]byte, error)
	// httpGet fetches a URL (aimd's status).
	httpGet func(ctx context.Context, url string) ([]byte, error)
}

// realSys is the machine itself.
func realSys() sys {
	return sys{
		lookPath:   exec.LookPath,
		executable: os.Executable,
		userHome:   os.UserHomeDir,
		uid:        os.Getuid(),
		launchctl: func(ctx context.Context, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, "/bin/launchctl", args...).CombinedOutput()
		},
		httpGet: httpGet,
	}
}

// plistEnv is one EnvironmentVariables entry.
type plistEnv struct{ Key, Value string }

// plistData fills deploy.PlistTemplate.
type plistData struct {
	Label      string
	Program    string
	Home       string
	LaunchdLog string
	Env        []plistEnv
}

// agentPlan is what `aim up` installs.
type agentPlan struct {
	data      plistData
	plistPath string // ~/Library/LaunchAgents/<label>.plist
	home      string // the ai-manager home
	warnings  []string
}

// planAgent resolves everything the plist needs. A missing tool is a
// warning (aimd reports that source as failing); a missing aimd binary is
// an error.
func planAgent(s sys) (agentPlan, error) {
	var p agentPlan
	home, err := config.Home()
	if err == nil {
		home, err = filepath.Abs(home) // launchd runs aimd elsewhere
	}
	if err != nil {
		return p, err
	}
	userHome, err := s.userHome()
	if err != nil {
		return p, fmt.Errorf("find home directory: %w", err)
	}
	aimd, err := findAimd(s)
	if err != nil {
		return p, err
	}

	var path []string
	for _, t := range tools {
		found, err := s.lookPath(t)
		if err != nil {
			p.warnings = append(p.warnings, fmt.Sprintf("%s not found on PATH; aimd will report it failing until it is installed and `aim up` is run again", t))
			continue
		}
		abs, err := filepath.Abs(found)
		if err != nil {
			return p, err
		}
		// The folder, not the resolved link target: claude's installer
		// points a stable link at a versioned binary that changes on update.
		if dir := filepath.Dir(abs); !slices.Contains(path, dir) {
			path = append(path, dir)
		}
	}
	for _, d := range systemPath {
		if !slices.Contains(path, d) {
			path = append(path, d)
		}
	}
	env := []plistEnv{
		{"PATH", strings.Join(path, string(os.PathListSeparator))},
		{deploy.LaunchdEnv, "1"},
	}
	for _, k := range passEnv {
		v := os.Getenv(k)
		if v == "" {
			continue
		}
		if k == config.HomeEnv {
			v = home // already expanded and cleaned
		} else if abs, err := filepath.Abs(v); err == nil {
			v = abs
		}
		env = append(env, plistEnv{k, v})
	}

	p.home = home
	p.plistPath = launchAgentPath(userHome)
	p.data = plistData{
		Label:      deploy.LaunchdLabel,
		Program:    aimd,
		Home:       home,
		LaunchdLog: filepath.Join(home, launchdLogFile),
		Env:        env,
	}
	return p, nil
}

// launchAgentPath is where the agent's plist goes.
func launchAgentPath(userHome string) string {
	return filepath.Join(userHome, "Library", "LaunchAgents", deploy.LaunchdLabel+".plist")
}

// findAimd returns the aimd binary: next to this aim binary (make build
// and go install put them together), else on PATH.
func findAimd(s sys) (string, error) {
	if exe, err := s.executable(); err == nil {
		cand := filepath.Join(filepath.Dir(exe), "aimd")
		if fi, err := os.Stat(cand); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return cand, nil
		}
	}
	found, err := s.lookPath("aimd")
	if err != nil {
		return "", errors.New("aimd not found next to aim or on PATH; build it with `make build` or `go install ./cmd/...`")
	}
	return filepath.Abs(found)
}

// renderPlist fills the template.
func renderPlist(d plistData) ([]byte, error) {
	tmpl, err := template.New("plist").Funcs(template.FuncMap{"xml": xmlEscape}).Parse(deploy.PlistTemplate)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, d); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func xmlEscape(s string) (string, error) {
	var b strings.Builder
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		return "", err
	}
	return b.String(), nil
}

// serviceTarget is the launchd service name of the aimd agent for uid.
func serviceTarget(uid int) string {
	return "gui/" + strconv.Itoa(uid) + "/" + deploy.LaunchdLabel
}

// launchctlTimeout bounds one launchctl call.
const launchctlTimeout = 15 * time.Second

// launchctl bootout returns before launchd has finished removing the
// service, and a bootstrap in that window fails with "Bootstrap failed: 5:
// Input/output error". So unload waits for the service to go, and load
// retries a bootstrap that fails with exit status 5. Tests shorten these.
var (
	// unloadWait bounds the wait for the service to go after bootout.
	unloadWait = 10 * time.Second
	// unloadPoll is how often that wait asks launchd.
	unloadPoll = 200 * time.Millisecond
	// bootstrapTries is how many times load runs bootstrap while it fails
	// with exit status 5, bootstrapDelay apart.
	bootstrapTries = 4
	bootstrapDelay = time.Second
)

// bootstrapIOError is launchctl's exit status for "Bootstrap failed: 5:
// Input/output error", which it also gives while the service is still
// being removed.
const bootstrapIOError = 5

// errStillLoaded is unload's error when bootout worked but launchd still
// had the service after unloadWait.
var errStillLoaded = errors.New("launchd still has aimd after bootout")

// runLaunchctl runs one launchctl command under its own timeout. A failure
// carries launchctl's output.
func runLaunchctl(s sys, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), launchctlTimeout)
	defer cancel()
	out, err := s.launchctl(ctx, args...)
	if err != nil {
		return &launchctlError{verb: args[0], out: strings.TrimSpace(string(out)), err: err}
	}
	return nil
}

// launchctlError is a launchctl command that failed.
type launchctlError struct {
	verb string // bootout, bootstrap, ...
	out  string
	err  error
}

func (e *launchctlError) Error() string {
	if e.out == "" {
		return fmt.Sprintf("launchctl %s: %v", e.verb, e.err)
	}
	return fmt.Sprintf("launchctl %s: %v: %s", e.verb, e.err, e.out)
}

func (e *launchctlError) Unwrap() error { return e.err }

// exitCode is the exit status of a failed command (an *exec.ExitError, or
// anything else with an ExitCode method), or -1.
func exitCode(err error) int {
	var ec interface{ ExitCode() int }
	if errors.As(err, &ec) {
		return ec.ExitCode()
	}
	return -1
}

// loaded reports whether launchd has the agent.
func loaded(s sys) bool {
	return runLaunchctl(s, "print", serviceTarget(s.uid)) == nil
}

// unload boots the agent out and waits up to unloadWait for launchd to
// finish removing it. It returns errStillLoaded when the bootout worked
// but the service was still there at the end of the wait.
func unload(s sys) error {
	if err := runLaunchctl(s, "bootout", serviceTarget(s.uid)); err != nil {
		return err
	}
	deadline := time.Now().Add(unloadWait)
	for loaded(s) {
		if time.Now().After(deadline) {
			return errStillLoaded
		}
		time.Sleep(unloadPoll)
	}
	return nil
}

// load bootstraps the agent from plist, trying again while bootstrap fails
// with exit status 5, up to bootstrapTries times in all.
func load(s sys, plist string) error {
	for try := 1; ; try++ {
		err := runLaunchctl(s, "bootstrap", fmt.Sprintf("gui/%d", s.uid), plist)
		if err == nil {
			return nil
		}
		if exitCode(err) != bootstrapIOError || try >= bootstrapTries {
			if try > 1 {
				return fmt.Errorf("%w (tried %d times)", err, try)
			}
			return err
		}
		time.Sleep(bootstrapDelay)
	}
}
