//go:build unix

package github

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These tests run gh as a process: a stand-in script on PATH, never the
// real gh. None of them depends on how fast the script starts. The ones
// meant to succeed run without a timeout, and the ones about stopping gh
// use a gh that never finishes by itself, so a loaded machine changes how
// long they take, not how they end.

// fakeGHScript answers like gh: a token for account good, and for anything
// else a reason on stderr, something on stdout that must not leak, and
// exit 1.
const fakeGHScript = `#!/bin/sh
case "$*" in
  "auth token --hostname github.com --user=good") echo "tok-from-gh" ;;
  *) echo "should-not-leak" ; echo "no oauth token found for github.com" >&2 ; exit 1 ;;
esac
`

// hungGHScript is a gh that never finishes by itself, and uses no CPU
// while it waits. It writes "started" to the FIFO started next to it
// (${0%/*} is the script's directory), then reads the FIFO hold, where
// nothing is ever written.
const hungGHScript = `#!/bin/sh
echo started > "${0%/*}/started"
read -r line < "${0%/*}/hold"
`

// installGH makes script the only program on PATH, as gh, and returns its
// directory.
func installGH(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return dir
}

// installHungGH installs hungGHScript as gh and returns its started FIFO,
// from which the test can read "started\n" once gh runs. The test holds
// both FIFOs open for reading and writing until it ends, so none of gh's
// opens waits, and only a kill ends gh while the test runs. Should the
// test binary die first, gh's read of hold ends and gh exits: nothing is
// left behind.
func installHungGH(t *testing.T) *os.File {
	t.Helper()
	dir := installGH(t, hungGHScript)
	var fifos []*os.File
	for _, name := range []string{"started", "hold"} {
		path := filepath.Join(dir, name)
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		fifos = append(fifos, f)
	}
	return fifos[0]
}

// runGH runs the gh on PATH; Token turns its output and failures into a
// token or ErrAuth, and its timeout ends a gh that hangs.
func TestRunGH(t *testing.T) {
	// Not about the timeout, so none: gh's start can take seconds on a
	// loaded machine, and that must not decide the outcome.
	t.Run("token", func(t *testing.T) {
		installGH(t, fakeGHScript)
		g := GHTokens(map[string]string{"org": "good"}).(*ghTokens)
		g.timeout = 0
		if got, err := g.Token(context.Background(), "org"); err != nil || got != "tok-from-gh" {
			t.Errorf("Token = %q, %v", got, err)
		}
	})

	t.Run("gh fails", func(t *testing.T) {
		installGH(t, fakeGHScript)
		g := GHTokens(map[string]string{"bad-org": "bad"}).(*ghTokens)
		g.timeout = 0
		_, err := g.Token(context.Background(), "bad-org")
		if !errors.Is(err, ErrAuth) || !strings.Contains(err.Error(), "no oauth token found") {
			t.Fatalf("error = %v; want ErrAuth with gh's stderr", err)
		}
		if strings.Contains(err.Error(), "should-not-leak") {
			t.Errorf("error %q carries gh's stdout", err)
		}
	})

	// A gh that hangs is ended by Token's timeout. gh never finishes, so
	// only the timeout can end the call, whatever its length. It stops gh
	// from starting ("context deadline exceeded") or kills it ("signal:
	// killed"), depending on how fast gh starts; either way no gh is left
	// running. The next subtest makes sure of the kill.
	t.Run("timeout ends a hung gh", func(t *testing.T) {
		installHungGH(t)
		g := GHTokens(nil).(*ghTokens)
		g.timeout = 50 * time.Millisecond
		_, err := g.Token(context.Background(), "org")
		msg := fmt.Sprint(err)
		timedOut := strings.Contains(msg, "signal: killed") || strings.Contains(msg, "context deadline exceeded")
		if !errors.Is(err, ErrAuth) || !timedOut {
			t.Errorf("error = %v; want ErrAuth from the timeout", err)
		}
	})

	// The kill itself: once gh is running, ending runGH's context kills
	// it. gh says when it is running, so this waits for that, not a time.
	t.Run("ended context kills a running gh", func(t *testing.T) {
		started := installHungGH(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := runGH(ctx, "auth", "token")
			done <- err
			// Ends the read below if gh never got to write.
			_, _ = started.WriteString("runGH returned\n")
		}()

		line, err := bufio.NewReader(started).ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line != "started\n" {
			t.Fatalf("gh ended before it was canceled: %v", <-done)
		}
		cancel()
		if err := <-done; err == nil || err.Error() != "signal: killed" {
			t.Errorf("runGH after cancel: error %v, want signal: killed", err)
		}
	})
}
