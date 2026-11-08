package prune

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ProcessTable tells whether a process is running and when it started, to
// tell a live Claude worktree lock from a stale one.
type ProcessTable interface {
	// StartTime returns when process pid started. running is false, with
	// no error, when there is no such process.
	StartTime(ctx context.Context, pid int) (start time.Time, running bool, err error)
}

// PS is the ProcessTable that asks ps(1): `ps -o lstart= -p <pid>`, in UTC
// and the C locale, which prints the start time in the same form Claude
// Code writes in its lock reasons ("Fri Nov  6 01:23:32 2026").
type PS struct{}

// psTimeout bounds one ps call.
const psTimeout = 5 * time.Second

// StartTime implements ProcessTable.
func (PS) StartTime(ctx context.Context, pid int) (time.Time, bool, error) {
	if pid <= 0 {
		return time.Time{}, false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, psTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-o", "lstart=", "-p", strconv.Itoa(pid))
	cmd.Env = append(os.Environ(), "TZ=UTC", "LC_ALL=C")
	out, err := cmd.Output()
	text := strings.TrimSpace(string(out))
	if err != nil {
		// ps exits non-zero, printing nothing, for a pid that doesn't exist.
		var ee *exec.ExitError
		if errors.As(err, &ee) && text == "" && ctx.Err() == nil {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, fmt.Errorf("ps -p %d: %w", pid, err)
	}
	if text == "" {
		return time.Time{}, false, nil
	}
	t, err := time.Parse(time.ANSIC, strings.Join(strings.Fields(text), " "))
	if err != nil {
		// ANSIC pads the day with a space; Fields collapsed it.
		t, err = time.Parse("Mon Jan 2 15:04:05 2006", strings.Join(strings.Fields(text), " "))
	}
	if err != nil {
		return time.Time{}, true, fmt.Errorf("ps -p %d: unexpected start time %q", pid, text)
	}
	return t, true, nil
}
