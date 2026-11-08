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
		{nil, 2, "", "usage: aim"},
		{[]string{"version"}, 0, "aim dev\n", ""},
		{[]string{"--version"}, 0, "aim dev\n", ""},
		{[]string{"help"}, 0, usage, ""},
		{[]string{"bogus"}, 2, "", `unknown command "bogus"`},
	} {
		var stdout, stderr bytes.Buffer
		code := run(tc.args, &stdout, &stderr)
		if code != tc.code || stdout.String() != tc.stdout || !strings.Contains(stderr.String(), tc.stderrPart) {
			t.Errorf("run(%q) = %d, stdout %q, stderr %q; want %d, %q, stderr containing %q",
				tc.args, code, stdout.String(), stderr.String(), tc.code, tc.stdout, tc.stderrPart)
		}
	}
}
