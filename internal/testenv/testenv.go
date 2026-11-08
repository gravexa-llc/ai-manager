// Package testenv prepares the process of a test binary that runs git and
// other programs. Only tests import it. Call Setup first in TestMain, or
// use Main:
//
//	func TestMain(m *testing.M) { os.Exit(testenv.Main(m)) }
//
// Setup does two things.
//
// It moves TMPDIR to a fresh directory under /tmp, so t.TempDir and
// os.MkdirTemp make their directories there. On a Mac the default TMPDIR
// is a per-user folder that can collect ten thousand entries, and the
// worktree scanner, to spell each path as it is on disk, lists every
// directory above it once per scan. Under the
// default, every scan of a test repo listed that folder while every test
// created and removed its temp dirs in it: the prune tests spent most of
// their time in the kernel, and on a loaded machine single git calls ran
// past the scanner's 10-second timeout.
//
// It starts a watchdog for fork children that never exec (see
// forkwatch.go), so a test that hits the race runtime bug in
// https://go.dev/issue/79804 fails in seconds instead of hanging the
// binary, and leaves no spinning orphan.
package testenv

import (
	"fmt"
	"os"
	"runtime"
	"testing"
)

// tempParent is where Setup makes the binary's TMPDIR.
const tempParent = "/tmp"

// Main runs the tests after Setup and returns the exit code.
func Main(m *testing.M) int {
	done := Setup()
	return done(m.Run())
}

// Setup moves TMPDIR and starts the fork watchdog. The returned function
// stops the watchdog, removes the temp dir with whatever the tests left
// in it, puts TMPDIR back, and returns the exit code to use: code, or 1
// if code is 0 and the watchdog had to kill a wedged child.
func Setup() (done func(code int) int) {
	oldTmp, hadTmp := os.LookupEnv("TMPDIR")
	tmp := moveTempDir()
	stop := startWatchdog()
	return func(code int) int {
		code = stop(code)
		if tmp != "" {
			os.RemoveAll(tmp)
			if hadTmp {
				os.Setenv("TMPDIR", oldTmp)
			} else {
				os.Unsetenv("TMPDIR")
			}
		}
		return code
	}
}

// moveTempDir points TMPDIR at a new directory under tempParent and
// returns it, or returns "" and leaves TMPDIR alone if that fails.
func moveTempDir() string {
	if runtime.GOOS == "windows" {
		return ""
	}
	// Unresolved, like the TMPDIR it replaces (/var is /private/var on
	// macOS, as /tmp is /private/tmp): tests resolve temp paths themselves,
	// and code that handles both spellings stays exercised.
	dir, err := os.MkdirTemp(tempParent, "aim-test-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "testenv: keeping TMPDIR: %v\n", err)
		return ""
	}
	os.Setenv("TMPDIR", dir)
	return dir
}
