package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
)

const downUsage = `usage: aim down

Stops aimd (launchctl bootout) and removes the launchd agent that ` + "`aim up`" + `
installed, so it doesn't start again at login. Its data in $AIM_HOME stays.
`

func cmdDown(args []string, stdout, stderr io.Writer) int {
	return down(realSys(), args, stdout, stderr)
}

func down(s sys, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		if args[0] == "-h" || args[0] == "-help" || args[0] == "--help" {
			fmt.Fprint(stdout, downUsage)
			return 0
		}
		fmt.Fprint(stderr, downUsage)
		return 2
	}
	plan, err := planPaths(s)
	if err != nil {
		fmt.Fprintf(stderr, "aim down: %v\n", err)
		return 1
	}

	// "Stopped" means launchd no longer has the service, not only that
	// bootout returned: unload waits for that, so `aim down` followed at
	// once by `aim up` doesn't race launchd.
	wasLoaded := loaded(s)
	stillLoaded := false
	if wasLoaded {
		if err := unload(s); errors.Is(err, errStillLoaded) {
			stillLoaded = true
		} else if err != nil {
			fmt.Fprintf(stderr, "aim down: %v\n", err)
			return 1
		}
	}
	removed := true
	if err := os.Remove(plan); errors.Is(err, fs.ErrNotExist) {
		removed = false
	} else if err != nil {
		fmt.Fprintf(stderr, "aim down: remove %s: %v\n", plan, err)
		return 1
	}
	if stillLoaded {
		fmt.Fprintf(stderr, "aim down: removed the launchd agent, but launchd still had aimd %s after bootout; check with `launchctl print %s`\n",
			unloadWait, serviceTarget(s.uid))
		return 1
	}
	switch {
	case wasLoaded:
		fmt.Fprintln(stdout, "aimd stopped")
	case removed:
		fmt.Fprintln(stdout, "aimd wasn't running; removed its launchd agent")
	default:
		fmt.Fprintln(stdout, "aimd wasn't running")
	}
	return 0
}

// planPaths returns the agent's plist path.
func planPaths(s sys) (string, error) {
	userHome, err := s.userHome()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	return launchAgentPath(userHome), nil
}
