package testenv

import (
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// pExec is P_EXEC from <sys/proc.h>: the process has called exec. A child
// without it is still a copy of its parent.
const pExec = 0x00004000

// child is what the watchdog needs to know about a process.
type child struct {
	pid, ppid, pgid int
	exec            bool
	started         time.Time
}

// children lists the processes whose parent is pid. It runs no program:
// starting one could wedge the same way.
func children(pid int) ([]child, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.uid", syscall.Getuid())
	if err != nil {
		return nil, err
	}
	var out []child
	for i := range procs {
		p := &procs[i]
		if int(p.Eproc.Ppid) != pid {
			continue
		}
		out = append(out, child{
			pid:     int(p.Proc.P_pid),
			ppid:    int(p.Eproc.Ppid),
			pgid:    int(p.Eproc.Pgid),
			exec:    p.Proc.P_flag&pExec != 0,
			started: time.Unix(p.Proc.P_starttime.Sec, int64(p.Proc.P_starttime.Usec)*1000),
		})
	}
	return out, nil
}

// stuck returns the children that have gone longer than limit without
// exec.
func stuck(kids []child, limit time.Duration, now time.Time) []child {
	var out []child
	for _, c := range kids {
		if !c.exec && now.Sub(c.started) > limit {
			out = append(out, c)
		}
	}
	return out
}

// killStuck kills pid's children that have been stuck before exec for
// longer than limit, and returns how many it killed.
func killStuck(pid int, limit time.Duration, now time.Time) int {
	kids, err := children(pid)
	if err != nil {
		return 0
	}
	n := 0
	for _, c := range stuck(kids, limit, now) {
		age := now.Sub(c.started)
		// A fork child execs nothing before it is killed, so it has no
		// children of its own; if it had already made its own process
		// group (Setpgid), that group holds only it.
		if c.pgid == c.pid {
			_ = syscall.Kill(-c.pgid, syscall.SIGKILL)
		}
		err := syscall.Kill(c.pid, syscall.SIGKILL)
		if err == syscall.ESRCH { // it exec'd or exited meanwhile
			continue
		}
		report(c.pid, age, err)
		if err == nil {
			n++
		}
	}
	return n
}
