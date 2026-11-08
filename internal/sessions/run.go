package sessions

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// NoTrafficEnv is set on every `claude agents` call. Without it the CLI
// sometimes opens a connection to Anthropic just before it exits, which a
// poller running around the clock shouldn't do.
const NoTrafficEnv = "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1"

// DefaultTimeout bounds one `claude agents --json` call. A call takes
// about 0.16 s on a quiet machine, but more than 5 s with the
// load average above 80, so the bound leaves room for that while
// still catching a call that hangs.
const DefaultTimeout = 20 * time.Second

// maxOutput caps what ExecRunner reads from one call.
const maxOutput = 16 << 20

// Cmd is one `claude` invocation.
type Cmd struct {
	// Args follow the binary name, e.g. ["agents", "--json"].
	Args []string
	// Env holds KEY=value pairs added to the inherited environment. They
	// override inherited values of the same key.
	Env []string
}

func (c Cmd) String() string { return "claude " + strings.Join(c.Args, " ") }

// Runner runs claude and returns its standard output. Run must return
// promptly once ctx is done. Tests use a fake; ExecRunner runs the real
// binary.
type Runner interface {
	Run(ctx context.Context, cmd Cmd) ([]byte, error)
}

// ExecRunner runs the claude binary as a child process.
type ExecRunner struct {
	// Path is the binary. Empty means "claude", looked up on PATH.
	Path string
	// WaitDelay is how long to wait for the child's output to close after
	// ctx is done and the child is killed. Zero means one second.
	WaitDelay time.Duration
}

// Run runs the command with the inherited environment plus cmd.Env and
// stdin from the null device. A non-zero exit is an error that includes
// the end of stderr.
func (r ExecRunner) Run(ctx context.Context, c Cmd) ([]byte, error) {
	path := r.Path
	if path == "" {
		path = "claude"
	}
	cmd := exec.CommandContext(ctx, path, c.Args...)
	cmd.Env = append(os.Environ(), c.Env...)
	cmd.WaitDelay = r.WaitDelay
	if cmd.WaitDelay <= 0 {
		cmd.WaitDelay = time.Second
	}
	stdout := &capped{max: maxOutput}
	stderr := &capped{max: 4 << 10}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	err := cmd.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("%s: %w", c, ctxErr)
	}
	if err != nil {
		if msg := strings.TrimSpace(stderr.buf.String()); msg != "" {
			return nil, fmt.Errorf("%s: %w: %s", c, err, clip([]byte(msg), 500))
		}
		return nil, fmt.Errorf("%s: %w", c, err)
	}
	if stdout.over {
		return nil, fmt.Errorf("%s: output over %d bytes", c, maxOutput)
	}
	return stdout.buf.Bytes(), nil
}

// capped is a writer that keeps the first max bytes and drops the rest.
type capped struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (w *capped) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); len(p) > room {
		w.over = true
		if room > 0 {
			w.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return w.buf.Write(p)
}

// Lister lists sessions with `claude agents --json`. The zero value runs
// the real binary with `--all` and DefaultTimeout.
type Lister struct {
	// Runner runs claude. Nil means ExecRunner{}.
	Runner Runner
	// Timeout bounds each call. Zero or less means DefaultTimeout.
	Timeout time.Duration
	// ActiveOnly drops `--all`. By default the listing includes finished
	// background jobs, so a job that ends shows up as a change of state
	// (working → done or failed) rather than as gone. Session.Active
	// tells the two kinds apart.
	ActiveOnly bool
}

var _ Runner = ExecRunner{}

// Cmd returns the command List runs.
func (l Lister) Cmd() Cmd {
	args := []string{"agents", "--json"}
	if !l.ActiveOnly {
		args = append(args, "--all")
	}
	return Cmd{Args: args, Env: []string{NoTrafficEnv}}
}

// List runs one listing and parses it. The snapshot's At is when the call
// started.
func (l Lister) List(ctx context.Context) (Snapshot, error) {
	runner := l.Runner
	if runner == nil {
		runner = ExecRunner{}
	}
	timeout := l.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	cmd := l.Cmd()

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	at := time.Now()
	out, err := runner.Run(ctx, cmd)
	if err == nil {
		// A runner that ignored ctx and returned late still timed out.
		err = ctx.Err()
	}
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return Snapshot{}, fmt.Errorf("sessions: %s: timed out after %s: %w", cmd, timeout, err)
		}
		return Snapshot{}, fmt.Errorf("sessions: %w", err)
	}
	snap, err := Parse(out)
	if err != nil {
		return Snapshot{}, err
	}
	snap.At = at
	snap.All = !l.ActiveOnly
	return snap, nil
}
