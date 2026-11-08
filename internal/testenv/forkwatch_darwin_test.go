package testenv

import (
	"os"
	"os/exec"
	"slices"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// This binary was exec'd by go test, so the flag the watchdog reads must
// be set on it.
func TestExecFlagOnThisProcess(t *testing.T) {
	p, err := unix.SysctlKinfoProc("kern.proc.pid", os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if p.Proc.P_flag&pExec == 0 {
		t.Errorf("P_flag %#x lacks P_EXEC on an exec'd process", p.Proc.P_flag)
	}
}

// A child that has exec'd is listed with exec set and its start time, and
// is never killed, however old.
func TestExecdChildIsLeftAlone(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	pid := cmd.Process.Pid

	var c child
	deadline := time.Now().Add(10 * time.Second)
	for {
		kids, err := children(os.Getpid())
		if err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(kids, func(k child) bool { return k.pid == pid })
		if i >= 0 && kids[i].exec {
			c = kids[i]
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child %d not listed as exec'd: %+v", pid, kids)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if c.ppid != os.Getpid() || time.Since(c.started) < 0 || time.Since(c.started) > time.Minute {
		t.Errorf("child = %+v", c)
	}

	if n := killStuck(os.Getpid(), 0, time.Now().Add(time.Hour)); n != 0 {
		t.Errorf("killStuck killed %d exec'd children", n)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Errorf("the exec'd child is gone: %v", err)
	}
}

func TestStuck(t *testing.T) {
	now := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	kids := []child{
		{pid: 1, exec: false, started: now.Add(-time.Minute)},                  // stuck
		{pid: 2, exec: false, started: now.Add(-time.Second)},                  // just forked
		{pid: 3, exec: true, started: now.Add(-time.Hour)},                     // exec'd long ago
		{pid: 4, exec: false, started: now.Add(-stuckAfter - time.Nanosecond)}, // just over
	}
	var got []int
	for _, c := range stuck(kids, stuckAfter, now) {
		got = append(got, c.pid)
	}
	if !slices.Equal(got, []int{1, 4}) {
		t.Errorf("stuck = %v, want [1 4]", got)
	}
}

// Starting and stopping the watchdog with no kills leaves the code alone.
func TestStartStop(t *testing.T) {
	stop := startWatchdog()
	if code := stop(0); code != 0 {
		t.Errorf("stop(0) = %d", code)
	}
	stop = startWatchdog()
	if code := stop(3); code != 3 {
		t.Errorf("stop(3) = %d", code)
	}
}
