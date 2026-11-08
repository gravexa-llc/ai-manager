// Package osascript runs AppleScript with /usr/bin/osascript, for aim go
// (internal/term) and aimd's notifications (internal/notify).
//
// The scripts are constants in the packages that use them. Every value a
// script needs is passed as an argument, which the script reads from its
// `on run argv` handler: no value is ever spliced into a script's text, so
// no value can change what a script does. Callers still validate what they
// pass (internal/term accepts only a tty, an iTerm2 session id, and a
// command built from a validated job id and claude's path).
//
// osascript is run directly with arguments, never through a shell, and
// with a timeout. Tests use a fake [Runner]; nothing in the tests runs
// osascript.
package osascript

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Path is osascript's absolute path.
const Path = "/usr/bin/osascript"

// DefaultTimeout bounds one run when Exec.Timeout is zero.
const DefaultTimeout = 10 * time.Second

// maxOutput caps what Exec keeps of a script's stdout and stderr.
const maxOutput = 64 << 10

// ErrTimeout is returned (wrapped) when a run outlives its timeout.
var ErrTimeout = errors.New("osascript: timed out")

// Runner runs one AppleScript.
type Runner interface {
	// Run runs script with args as its argv and returns its standard
	// output with surrounding white space trimmed. A script that fails to
	// compile or run is an error carrying the end of its stderr.
	Run(ctx context.Context, script string, args ...string) (string, error)
}

// Exec runs /usr/bin/osascript as a child process: `osascript -e <script>
// -- <args>...`, stdin from the null device.
type Exec struct {
	// Path is the binary; empty means Path.
	Path string
	// Timeout bounds each run; zero or less means DefaultTimeout.
	Timeout time.Duration
}

var _ Runner = Exec{}

// Run implements Runner.
func (e Exec) Run(ctx context.Context, script string, args ...string) (string, error) {
	path := e.Path
	if path == "" {
		path = Path
	}
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// "--" ends osascript's options, so an argument starting with "-" is
	// still an argument.
	argv := append([]string{"-e", script, "--"}, args...)
	cmd := exec.CommandContext(ctx, path, argv...)
	cmd.WaitDelay = time.Second
	stdout := &capped{max: maxOutput}
	stderr := &capped{max: 4 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf("%w after %s", ErrTimeout, timeout)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", fmt.Errorf("osascript: %w", ctxErr)
	}
	if err != nil {
		if msg := strings.TrimSpace(stderr.buf.String()); msg != "" {
			return "", fmt.Errorf("osascript: %w: %s", err, clip(msg, 300))
		}
		return "", fmt.Errorf("osascript: %w", err)
	}
	return strings.TrimSpace(stdout.buf.String()), nil
}

// capped is a writer that keeps the first max bytes and drops the rest.
type capped struct {
	buf bytes.Buffer
	max int
}

func (w *capped) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); len(p) > room {
		if room > 0 {
			w.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return w.buf.Write(p)
}

// clip shortens s to at most n bytes, marking the cut.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
