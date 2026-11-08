package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	for _, tc := range []struct {
		args       []string
		code       int
		stdout     string
		stderrPart string
	}{
		{nil, 2, "", "usage: aimd"},
		{[]string{"-version"}, 0, "aimd dev\n", ""},
		{[]string{"--version"}, 0, "aimd dev\n", ""},
		{[]string{"-h"}, 0, usage, ""},
		{[]string{"--help"}, 0, usage, ""},
		{[]string{"serve"}, 2, "", `unknown argument "serve"`},
		{[]string{"--foreground", "extra"}, 2, "", `unknown argument "extra"`},
		{[]string{"--bogus"}, 2, "", "flag provided but not defined"},
		{[]string{"--port", "70000", "--foreground"}, 2, "", "out of range"},
		{[]string{"--debug"}, 2, "", "usage: aimd --foreground"},
	} {
		var stdout, stderr bytes.Buffer
		code := run(tc.args, &stdout, &stderr)
		if code != tc.code || stdout.String() != tc.stdout || !strings.Contains(stderr.String(), tc.stderrPart) {
			t.Errorf("run(%q) = %d, stdout %q, stderr %q; want %d, %q, stderr containing %q",
				tc.args, code, stdout.String(), stderr.String(), tc.code, tc.stdout, tc.stderrPart)
		}
	}
}
