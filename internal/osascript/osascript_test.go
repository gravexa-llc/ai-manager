package osascript

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeBinary writes a shell script standing in for osascript (no test
// runs osascript itself) and returns its path.
func fakeBinary(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fake-osascript")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return p
}

// Exec passes the script with -e and the values after "--", each its own
// argument, so a value can't become script or an option.
func TestExecArgs(t *testing.T) {
	bin := fakeBinary(t, `for a in "$@"; do printf '[%s]\n' "$a"; done`)
	out, err := Exec{Path: bin}.Run(context.Background(), "on run argv\nreturn 1\nend run", "-x", `a "quoted" value`, "")
	if err != nil {
		t.Fatal(err)
	}
	want := "[-e]\n[on run argv\nreturn 1\nend run]\n[--]\n[-x]\n[a \"quoted\" value]\n[]"
	if out != want {
		t.Errorf("argv =\n%s\nwant\n%s", out, want)
	}
}

func TestExecFailure(t *testing.T) {
	bin := fakeBinary(t, `echo "execution error: nope (-1728)" >&2; exit 1`)
	_, err := Exec{Path: bin}.Run(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "nope (-1728)") {
		t.Errorf("err = %v, want stderr in it", err)
	}
}

func TestExecTimeout(t *testing.T) {
	bin := fakeBinary(t, `exec sleep 10`)
	start := time.Now()
	_, err := Exec{Path: bin, Timeout: 100 * time.Millisecond}.Run(context.Background(), "x")
	if !errors.Is(err, ErrTimeout) {
		t.Errorf("err = %v, want ErrTimeout", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %s", d)
	}
}
