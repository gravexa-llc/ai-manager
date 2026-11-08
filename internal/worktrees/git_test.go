package worktrees

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// pipeHolder writes a stand-in for git that prints to stdout and stderr,
// leaves a background `sleep <hold>` holding both pipes, and then runs
// tail (a shell snippet: an exit, or exec sleep to hang). This is what
// happens when another call's fork inherits the pipes and is slow to
// exec, or when git leaves a child behind. The sleeper is killed when the
// test ends.
func pipeHolder(t *testing.T, out string, hold, tail string) string {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	dir := tempDir(t)
	pidFile := filepath.Join(dir, "holder.pid")
	write(t, filepath.Join(dir, "out"), out)
	script := "#!/bin/sh\ncat '" + filepath.Join(dir, "out") + "'\necho to-stderr >&2\n" +
		"sleep " + hold + " &\necho $! > '" + pidFile + "'\n" + tail + "\n"
	path := filepath.Join(dir, "git")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				if p, err := os.FindProcess(pid); err == nil {
					p.Kill()
				}
			}
		}
	})
	return path
}

// A git call that exited 0 while something else still held its pipes
// succeeds once they close, with all its output. The scanner's old
// one-second WaitDelay failed this call with exec.ErrWaitDelay.
func TestGitPipeHeldBrieflyAfterExit(t *testing.T) {
	t.Parallel()
	git := pipeHolder(t, "hello\n", "1.5", "exit 0")
	run := execGitEnv(git, baseEnv(os.Environ()))
	start := time.Now()
	out, err := run(withStderr(context.Background()), tempDir(t), nil, "status")
	d := time.Since(start)
	if err != nil {
		t.Fatalf("call failed after %v: %v", d, err)
	}
	if string(out) != "hello\nto-stderr\n" {
		t.Errorf("output %q", out)
	}
	if d < time.Second || d > pipeWaitDelay {
		t.Errorf("took %v: want the 1.5 s the pipes were held, within the %v bound", d, pipeWaitDelay)
	}
}

// Past the bound, the call stops waiting and still succeeds with
// everything git wrote, here more than a pipe holds at once.
func TestGitPipeHeldPastTheBound(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("0123456789abcdef", 16<<10) // 256 KiB
	git := pipeHolder(t, big, "60", "exit 0")
	const bound = 300 * time.Millisecond
	run := execGitWait(git, baseEnv(os.Environ()), bound)
	start := time.Now()
	out, err := run(context.Background(), tempDir(t), nil, "status")
	d := time.Since(start)
	if err != nil {
		t.Fatalf("call failed after %v: %v", d, err)
	}
	if string(out) != big {
		t.Errorf("got %d bytes of output, want %d", len(out), len(big))
	}
	if d > 10*time.Second {
		t.Errorf("took %v with a %v bound", d, bound)
	}

	// A failure is still a failure, with its stderr.
	git = pipeHolder(t, "", "60", "exit 3")
	_, err = execGitWait(git, baseEnv(os.Environ()), bound)(context.Background(), tempDir(t), nil, "status")
	var ge *GitError
	if !errors.As(err, &ge) || exitCode(err) != 3 || ge.Stderr != "to-stderr" {
		t.Errorf("failing call: %v", err)
	}
}

// A timeout kill doesn't wait on a pipe that git's leftover child holds:
// the call returns about waitDelay after the kill.
func TestGitTimeoutKillWithHeldPipe(t *testing.T) {
	t.Parallel()
	git := pipeHolder(t, "partial\n", "60", "exec sleep 60")
	const bound = 300 * time.Millisecond
	run := execGitWait(git, baseEnv(os.Environ()), bound)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := run(ctx, tempDir(t), nil, "status")
	d := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err %v, want the deadline", err)
	}
	if d > 10*time.Second {
		t.Errorf("took %v: it waited for the held pipe", d)
	}
}

// A git that can't start is an error naming the call.
func TestGitStartFails(t *testing.T) {
	t.Parallel()
	_, err := execGitEnv(filepath.Join(tempDir(t), "no-such-git"), nil)(context.Background(), tempDir(t), nil, "status")
	var ge *GitError
	if !errors.As(err, &ge) || !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err %v", err)
	}
}

// finish takes what is still in a pipe when the wait ends, even if the
// background read never got to it.
func TestPipeReaderDrainsAtDeadline(t *testing.T) {
	for range 20 {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		want := bytes.Repeat([]byte("x"), 4000)
		if _, err := w.Write(want); err != nil {
			t.Fatal(err)
		}
		// w stays open, as a holder would keep it.
		got := readPipe(r).finish(time.Now().Add(-time.Second))
		w.Close()
		if !bytes.Equal(got, want) {
			t.Fatalf("got %d bytes, want %d", len(got), len(want))
		}
	}

	// A holder that never stops writing doesn't keep it reading.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	go func() {
		defer w.Close()
		line := []byte("spam\n")
		for {
			select {
			case <-stop:
				return
			default:
				if _, err := w.Write(line); err != nil {
					return
				}
			}
		}
	}()
	defer close(stop)
	start := time.Now()
	got := readPipe(r).finish(time.Now().Add(50 * time.Millisecond))
	if d := time.Since(start); d > 5*time.Second || len(got) == 0 {
		t.Errorf("finish with a writer that never stops: %v, %d bytes", d, len(got))
	}
}
