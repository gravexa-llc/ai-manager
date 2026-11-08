package testenv

// The fork watchdog.
//
// Under the race detector on darwin, Go 1.26 before 1.26.5 can wedge a
// fork child before it calls exec: the child spins in the race runtime at
// 100% of a core, forever (https://go.dev/issue/79804, fixed in 1.26.5
// and 1.27). The parent waits in os/exec's Cmd.Start for the exec that
// never comes, so the test hangs until the binary's -timeout, and the
// child outlives the binary as an orphan. A context deadline can't help:
// Start hasn't returned a process to kill.
//
// The watchdog looks at this process's children every second and kills
// any that has been a fork of this binary, not yet exec'd, for longer than
// stuckAfter (and its process group, should it have made one). The parent's Start then returns and the call fails with "signal:
// killed", so the test fails in seconds instead of hanging, and nothing
// is left running. Setup's done function makes the binary exit non-zero
// after a kill, even if the failed call was tolerated, and the watchdog
// says why on stderr.

import (
	"fmt"
	"os"
	"sync"
	"time"
)

// stuckAfter is how long a fork child may go without exec before it is
// killed. A healthy one execs within milliseconds, even on a loaded
// machine.
const stuckAfter = 15 * time.Second

// watchInterval is how often the children are checked.
const watchInterval = time.Second

// startWatchdog starts the watchdog. The returned function stops it,
// kills any child still not exec'd however young (the tests are over, so
// nothing is waiting for it), and returns code, or 1 if code is 0 and the
// watchdog killed a child.
//
// What it can't cover: a binary that dies abnormally (a panic in a test,
// or the -timeout alarm) while a child is wedged leaves that child behind,
// since the watchdog dies with it. stuckAfter keeps that window short.
func startWatchdog() (stop func(code int) int) {
	w := &watchdog{pid: os.Getpid(), done: make(chan struct{})}
	w.wg.Go(w.run)
	return func(code int) int {
		close(w.done)
		w.wg.Wait()
		w.killed += killStuck(w.pid, 0, time.Now())
		if n := w.killed; n > 0 && code == 0 {
			fmt.Fprintf(os.Stderr, "testenv: failing the run: %d fork child(ren) were stuck before exec and killed\n", n)
			return 1
		}
		return code
	}
}

type watchdog struct {
	pid    int
	done   chan struct{}
	wg     sync.WaitGroup
	killed int // written by run only; read after wg.Wait
}

func (w *watchdog) run() {
	t := time.NewTicker(watchInterval)
	defer t.Stop()
	for {
		select {
		case <-w.done:
			return
		case <-t.C:
			w.killed += killStuck(w.pid, stuckAfter, time.Now())
		}
	}
}

// report says on stderr what was killed and why.
func report(pid int, age time.Duration, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "testenv: pid %d has been a fork of this test binary for %v without calling exec, and killing it failed: %v\n",
			pid, age.Round(time.Second), err)
		return
	}
	fmt.Fprintf(os.Stderr, "testenv: killed pid %d, a fork of this test binary stuck for %v before exec "+
		"(the race runtime bug in https://go.dev/issue/79804; upgrade to Go 1.26.5 or later). "+
		"The process start it belonged to fails with \"signal: killed\".\n",
		pid, age.Round(time.Second))
}
