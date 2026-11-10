package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/config"
)

const upUsage = `usage: aim up [--print]

Installs aimd as a launchd agent (~/Library/LaunchAgents/com.gravexa.aimd.plist)
and starts it. It resolves claude, gh and git where your shell finds them,
since launchd's PATH is only the system one; run it again after moving them.

  --print   print the plist instead of installing it
`

// upWait is how long `aim up` waits for aimd to answer after starting it.
// Tests shorten it.
var upWait = 10 * time.Second

func cmdUp(args []string, stdout, stderr io.Writer) int {
	return up(realSys(), args, stdout, stderr)
}

func up(s sys, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("aim up", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	printOnly := fs.Bool("print", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, upUsage)
			return 0
		}
		fmt.Fprint(stderr, upUsage)
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
	plist, err := renderPlist(plan.data)
	if err != nil {
		fmt.Fprintf(stderr, "aim up: %v\n", err)
		return 1
	}
	if *printOnly {
		stdout.Write(plist)
		return 0
	}

	cfg, err := config.Load(plan.home)
	if err != nil {
		fmt.Fprintf(stderr, "aim up: %v\n", err)
		return 1
	}
	// launchd needs the working directory and log folder to exist.
	if err := os.MkdirAll(plan.home, 0o700); err != nil {
		fmt.Fprintf(stderr, "aim up: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(filepath.Dir(plan.plistPath), 0o755); err != nil {
		fmt.Fprintf(stderr, "aim up: %v\n", err)
		return 1
	}
	if err := writeFileAtomic(plan.plistPath, plist, 0o644); err != nil {
		fmt.Fprintf(stderr, "aim up: write %s: %v\n", plan.plistPath, err)
		return 1
	}

	if loaded(s) {
		// Reload, so a changed plist or binary takes effect. A service
		// still there after unloadWait is left to load's retries.
		if err := unload(s); err != nil && !errors.Is(err, errStillLoaded) {
			fmt.Fprintf(stderr, "aim up: %v\n", err)
			return 1
		}
	}
	if err := load(s, plan.plistPath); err != nil {
		fmt.Fprintf(stderr, "aim up: %v\n", err)
		fmt.Fprintln(stderr, "aim up: aimd is stopped now; run `aim up` again")
		return 1
	}

	url := fmt.Sprintf("http://127.0.0.1:%d", cfg.Port)
	if _, err := waitStatus(s, cfg.Port, upWait); err != nil {
		fmt.Fprintf(stderr, "aim up: aimd was started but doesn't answer on %s yet (%v); see %s, or %s for a crash at startup\n",
			url, err, config.LogPath(plan.home), plan.data.LaunchdLog)
		return 1
	}
	fmt.Fprintf(stdout, "aimd is running at %s (launchd agent %s)\nOpen the dashboard with `aim open`: it signs your browser in.\n", url, plan.plistPath)
	return 0
}

// waitStatus polls aimd's status until it answers or wait passes.
func waitStatus(s sys, port int, wait time.Duration) ([]byte, error) {
	deadline := time.Now().Add(wait)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), statusTimeout)
		body, err := s.httpGet(ctx, statusURL(port))
		cancel()
		if err == nil {
			return body, nil
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// writeFileAtomic writes data to path through a temp file in the same
// directory, so launchd never reads half a plist.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after the rename
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
