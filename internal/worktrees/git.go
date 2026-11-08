package worktrees

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ErrNotRepo is the error (inside an *fs.PathError) for a path that git
// says is not in a repository.
var ErrNotRepo = errors.New("not a git repository")

// GitError is a failed git call.
type GitError struct {
	Args   []string // arguments after "git"
	Stderr string   // what git printed on stderr, trimmed
	Err    error    // the exit error, or context.DeadlineExceeded on a timeout
}

func (e *GitError) Error() string {
	msg := "git " + strings.Join(e.Args, " ") + ": " + e.Err.Error()
	if line, _, _ := strings.Cut(e.Stderr, "\n"); line != "" {
		msg += ": " + line
	}
	return msg
}

func (e *GitError) Unwrap() error { return e.Err }

// runFunc runs git with args in dir, adding env to the scanner's base
// environment, and returns its stdout. Tests wrap it.
type runFunc func(ctx context.Context, dir string, env []string, args ...string) ([]byte, error)

// git runs one git call under the scanner's concurrency limit and per-call
// timeout.
func (s *Scanner) git(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, &GitError{Args: args, Err: ctx.Err()}
	}
	defer func() { <-s.sem }()

	ctx, cancel := context.WithTimeout(ctx, s.opts.Timeout)
	defer cancel()
	return s.run(ctx, dir, env, args...)
}

// hardening are the options every git call starts with. The scanner runs
// git in repos it merely found on disk, whose own config could otherwise
// make git run programs:
//   - core.fsmonitor=false: no fsmonitor hook or daemon during status;
//   - core.hooksPath=/dev/null: no hooks (none of the commands used run
//     hooks, but a future one might);
//   - core.pager=cat: never a pager;
//   - protocol.allow=never: no transport, so no lazy fetch from a promisor
//     remote, no ssh and no core.sshCommand, on every git version. A repo's
//     own protocol.<name>.allow outranks it, so baseEnv also sets
//     GIT_ALLOW_PROTOCOL, which outranks all protocol config;
//   - credential.helper= (empty): clears the helper list, so no credential
//     helper runs;
//   - --no-optional-locks: status never rewrites the index (as does
//     GIT_OPTIONAL_LOCKS=0 in baseEnv).
//
// Filter drivers, the other way config runs programs during status, are
// overridden per worktree (see Scanner.filterOverrides).
var hardening = []string{
	"-c", "core.fsmonitor=false",
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.pager=cat",
	"-c", "protocol.allow=never",
	"-c", "credential.helper=",
	"--no-optional-locks",
}

// execGit returns a runFunc that executes the git binary at path, with the
// hardening options before args and baseEnv as the environment.
func execGit(path string) runFunc { return execGitEnv(path, baseEnv(os.Environ())) }

// execGitEnv is execGit with the base environment given (tests use it to
// simulate an older git that ignores GIT_NO_LAZY_FETCH).
func execGitEnv(path string, base []string) runFunc { return execGitWait(path, base, pipeWaitDelay) }

// pipeWaitDelay bounds how long a git call waits, once git has exited or
// been killed, for its output pipes to reach end-of-file (see
// runCaptured). It is a generous bound, not a deadline a working call
// meets: on a loaded machine a pipe can stay open well over a second after
// git exits.
const pipeWaitDelay = 10 * time.Second

// execGitWait is execGitEnv with the pipe wait given (tests shorten it).
func execGitWait(path string, base []string, waitDelay time.Duration) runFunc {
	return func(ctx context.Context, dir string, env []string, args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, path, append(append([]string(nil), hardening...), args...)...)
		cmd.Dir = dir
		cmd.Env = append(append([]string(nil), base...), env...)
		stdout, stderr, err := runCaptured(cmd, waitDelay)
		if err != nil {
			if ctx.Err() != nil {
				err = ctx.Err()
			}
			return nil, &GitError{Args: args, Stderr: strings.TrimSpace(string(stderr)), Err: err}
		}
		if ctx.Value(withStderrKey{}) != nil {
			return append(stdout, stderr...), nil
		}
		return stdout, nil
	}
}

// runCaptured runs cmd with stdout and stderr on pipes of its own, and
// returns what it wrote with cmd.Wait's error.
//
// The pipes aren't left to os/exec, whose WaitDelay gives up on them a
// fixed time after the process exits and then fails the call with
// exec.ErrWaitDelay, even when git exited 0 (and it can't say whether
// output was lost). A pipe outlives git whenever another process holds a
// copy of its write end: a child git left behind, or far more often a
// process this one forked for another call at the same moment, which keeps
// every inherited descriptor until it execs. Under load that took over a
// second, so a call that had succeeded came back as status unknown.
// Here:
//   - both pipes are read while git runs;
//   - once git has exited, or been killed because ctx ended, each pipe
//     gets waitDelay more to reach end-of-file, which it normally does at
//     once;
//   - a pipe still open after that is read for whatever is in it (see
//     drain) and closed. git has exited by then, so everything it wrote is
//     already in the pipe: the output is complete, and the call's result
//     is git's exit status.
//
// So a call can't hang on a pipe someone else holds (a timeout kill returns
// within the timeout plus waitDelay), and a held pipe never turns a
// successful call into a failure.
func runCaptured(cmd *exec.Cmd, waitDelay time.Duration) (stdout, stderr []byte, err error) {
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return nil, nil, err
	}
	// Files go to the child as they are: os/exec starts no copying of its
	// own, so cmd.Wait waits only for the process.
	cmd.Stdout, cmd.Stderr = outW, errW
	err = cmd.Start()
	// The child has its own copies of the write ends now, or never will.
	outW.Close()
	errW.Close()
	if err != nil {
		outR.Close()
		errR.Close()
		return nil, nil, err
	}
	out, errOut := readPipe(outR), readPipe(errR)
	err = cmd.Wait()
	deadline := time.Now().Add(waitDelay)
	return out.finish(deadline), errOut.finish(deadline), err
}

// pipeReader reads a pipe to its end in the background.
type pipeReader struct {
	f    *os.File
	buf  bytes.Buffer
	done chan error
}

func readPipe(f *os.File) *pipeReader {
	p := &pipeReader{f: f, done: make(chan error, 1)}
	go func() {
		_, err := p.buf.ReadFrom(f)
		p.done <- err
	}()
	return p
}

// finish waits until deadline for the pipe to reach end-of-file. If it
// hasn't by then, it takes what is in the pipe (drain). Either way it
// closes the pipe and returns everything read.
func (p *pipeReader) finish(deadline time.Time) []byte {
	if err := p.f.SetReadDeadline(deadline); err != nil {
		// No deadlines on this platform's pipes (Unix pipes have them):
		// give up the way os/exec does, by closing the pipe.
		p.f.Close()
		<-p.done
		return p.buf.Bytes()
	}
	if err := <-p.done; errors.Is(err, os.ErrDeadlineExceeded) {
		p.drain()
	}
	p.f.Close()
	return p.buf.Bytes()
}

// drainWait is how long drain waits on an empty pipe before it stops. A
// read of a pipe that has data returns at once, so it only bounds the
// last, empty read.
const drainWait = 10 * time.Millisecond

// drainReads caps drain's reads of drainChunk bytes: 2 MiB, more than a
// pipe holds (64 KiB on macOS, 1 MiB at most by default on Linux), so it
// takes everything git left and stops even if a leftover child of git
// keeps writing.
const (
	drainChunk = 32 << 10
	drainReads = 64
)

// drain appends what is in the pipe now to the buffer: it reads until the
// pipe stays empty for drainWait, reaches end-of-file, fails, or has had
// drainReads reads. Each read gets a fresh deadline, since an expired one
// fails a read before it looks at the pipe.
func (p *pipeReader) drain() {
	chunk := make([]byte, drainChunk)
	for range drainReads {
		if p.f.SetReadDeadline(time.Now().Add(drainWait)) != nil {
			return
		}
		n, err := p.f.Read(chunk)
		p.buf.Write(chunk[:n])
		if err != nil {
			return
		}
	}
}

// withStderrKey, set on a call's context by withStderr, makes the exec
// runner return what git wrote on stderr after its stdout, for the few
// commands that report on stderr (worktree prune --verbose).
type withStderrKey struct{}

func withStderr(ctx context.Context) context.Context {
	return context.WithValue(ctx, withStderrKey{}, true)
}

// repoLocatingEnv are variables that would point git at some other
// repository than the one each call names.
var repoLocatingEnv = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE",
	"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
	"GIT_NAMESPACE", "GIT_PREFIX",
}

// baseEnv is environ without the repo-locating variables, plus the settings
// every call uses:
//   - GIT_OPTIONAL_LOCKS=0: git status never refreshes and rewrites the index;
//   - GIT_TERMINAL_PROMPT=0: never wait on a prompt;
//   - GIT_NO_LAZY_FETCH=1: a partial clone never fetches a missing object
//     (git 2.45+; older versions ignore it);
//   - GIT_ALLOW_PROTOCOL= (empty): an allow-list naming no protocol, which
//     overrides protocol.allow and every protocol.<name>.allow, including a
//     repo's own (git 2.6.1+). Without it a repo could set
//     protocol.ext.allow=always and run a command through an ext:: promisor
//     remote despite -c protocol.allow=never;
//   - LC_ALL=C: error messages are in English, so "not a git repository"
//     can be recognised.
//
// Everything else is inherited. In particular GIT_CONFIG_NOSYSTEM and
// GIT_CONFIG_GLOBAL are left alone, so system and global settings such as
// safe.bareRepository and safe.directory still apply.
func baseEnv(environ []string) []string {
	out := make([]string, 0, len(environ)+5)
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if isRepoLocating(k) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1",
		"GIT_ALLOW_PROTOCOL=", "LC_ALL=C")
}

func isRepoLocating(key string) bool {
	for _, k := range repoLocatingEnv {
		if key == k {
			return true
		}
	}
	return false
}

// ceilingEnv stops git's upward repository search at dir's parent, so a
// directory whose .git is broken or gone fails instead of silently
// resolving to an enclosing repo (a worktree under <repo>/.claude/worktrees
// would otherwise report the main checkout's status as its own).
func ceilingEnv(dir string) []string {
	return []string{"GIT_CEILING_DIRECTORIES=" + filepath.Dir(dir)}
}

// isNotRepo reports whether err is git refusing a path that isn't in a
// repository.
func isNotRepo(err error) bool { return stderrHas(err, "not a git repository") }

// isBareRefused reports whether err is git refusing to use a bare repo it
// discovered because safe.bareRepository is "explicit".
func isBareRefused(err error) bool { return stderrHas(err, "safe.bareRepository") }

func stderrHas(err error, s string) bool {
	var ge *GitError
	return errors.As(err, &ge) && strings.Contains(ge.Stderr, s)
}

// exitCode returns git's exit status in err, or -1 if err isn't an exit.
func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// gitDirArg is the --git-dir option for a repo-level call. Naming a git dir
// is explicit use, which safe.bareRepository=explicit allows, so it is only
// used for repos a caller chose: Discover never returns a bare repo it found
// only by walking (see ErrBareSkipped).
func gitDirArg(gitDir string) string { return "--git-dir=" + gitDir }

// configEntries runs `git config -z --get-regexp <pattern>` in dir (naming
// gitDir with --git-dir when it isn't empty) and returns the matching keys
// and values in order. Reading config runs nothing. No match is not an
// error. -z keeps a subsection name with spaces (filter "my driver")
// unambiguous.
func (s *Scanner) configEntries(ctx context.Context, dir, gitDir string, env []string, pattern string) ([][2]string, error) {
	var args []string
	if gitDir != "" {
		args = append(args, gitDirArg(gitDir))
	}
	out, err := s.git(ctx, dir, env, append(args, "config", "-z", "--get-regexp", pattern)...)
	if err != nil {
		if exitCode(err) == 1 { // no matching key
			return nil, nil
		}
		return nil, err
	}
	return parseConfigZ(out), nil
}
