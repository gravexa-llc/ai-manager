package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// Load tolerance. On a loaded Mac `claude agents --json` can take
// seconds; one call that times out mustn't mark the sessions source
// failing. These run in a synctest bubble like scheduler_test.go: the
// fake claude's slowness is fake time, so nothing depends on how fast this
// machine is. By default sessions are polled every 30s (unwatched) and
// each call is cut off after sessions.DefaultTimeout, 20s.

func sessionsSource(t *testing.T, r *rig) api.Source {
	t.Helper()
	src := r.d.Status().Sources[0]
	if src.Name != api.SourceSessions {
		t.Fatalf("first source is %q", src.Name)
	}
	return src
}

// wantLogged checks how many times each message is in the log.
func wantLogged(t *testing.T, log *lockedBuffer, want map[string]int) {
	t.Helper()
	text := log.String()
	for msg, n := range want {
		if got := strings.Count(text, `msg="`+msg); got != n {
			t.Errorf("%q logged %d times, want %d:\n%s", msg, got, n, text)
		}
	}
}

func logTo(r *rig) *lockedBuffer {
	var log lockedBuffer
	r.d.log = slog.New(slog.NewTextHandler(&log, nil))
	return &log
}

// A slow call within the limit works, and one timeout leaves the source
// healthy and the sessions as they were.
func TestSessionOneTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, nil)
		log := logTo(r)
		r.runner.set(entry{PID: 1, SessionID: "s1", Cwd: "/r/a", Kind: "interactive", Status: "idle"})
		base := time.Now()
		r.start(t)
		synctest.Wait() // a good poll at 0s

		// A call that takes 15s, three times the old 5s bound, works.
		r.runner.setDelay(15 * sec)
		time.Sleep(50 * sec) // the poll at 30s answers at 45s
		synctest.Wait()
		if src := sessionsSource(t, r); src.Error != "" || !src.LastOK.Equal(base.Add(30*sec)) {
			t.Errorf("after a slow poll: %+v", src)
		}

		// One call times out: the poll at 60s gives up at 80s.
		statusNotes := r.notes.count(api.TopicStatus)
		r.runner.setDelay(time.Hour)
		time.Sleep(35 * sec) // t=85
		synctest.Wait()
		src := sessionsSource(t, r)
		if src.Stale || !src.FailingSince.IsZero() || !strings.Contains(src.Error, "timed out after 20s") {
			t.Errorf("after one timeout: %+v, want not stale, with the timeout as its error", src)
		}
		if live, ended := r.st.liveSessions(), r.st.endedKeys(); len(live) != 1 || len(ended) != 0 {
			t.Errorf("a timeout changed sessions: live %v, ended %v", keys(live), ended)
		}
		if n := r.notes.count(api.TopicStatus); n != statusNotes {
			t.Errorf("status notified %d times for one timeout, want 0", n-statusNotes)
		}

		// The next poll, at 90s, works.
		r.runner.setDelay(0)
		time.Sleep(10 * sec) // t=95
		synctest.Wait()
		if src := sessionsSource(t, r); src.Stale || src.Error != "" || !src.LastOK.Equal(base.Add(90*sec)) {
			t.Errorf("after recovery: %+v", src)
		}
		wantLogged(t, log, map[string]int{"source timed out": 1, "source failing": 0, "source recovered": 1})
	})
}

// Two timeouts in a row mark the source failing, from the first; more
// change nothing and log nothing; a good poll recovers it. The sessions
// stay as last read throughout.
func TestSessionTimeoutsInARow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, nil)
		log := logTo(r)
		r.runner.set(entry{PID: 1, SessionID: "s1", Cwd: "/r/a", Kind: "interactive", Status: "idle"})
		base := time.Now()
		r.start(t)
		synctest.Wait() // a good poll at 0s

		r.runner.setDelay(time.Hour)
		time.Sleep(55 * sec) // the poll at 30s gave up at 50s
		synctest.Wait()
		if src := sessionsSource(t, r); src.Stale {
			t.Errorf("stale after one timeout: %+v", src)
		}
		statusNotes := r.notes.count(api.TopicStatus)

		time.Sleep(30 * sec) // t=85: the poll at 60s gave up at 80s
		synctest.Wait()
		src := sessionsSource(t, r)
		if !src.Stale || !src.FailingSince.Equal(base.Add(50*sec)) || !strings.Contains(src.Error, "timed out after 20s") {
			t.Errorf("after two timeouts: %+v, want stale since the first (50s)", src)
		}
		if n := r.notes.count(api.TopicStatus); n != statusNotes+1 {
			t.Errorf("status notified %d times on going stale, want 1", n-statusNotes)
		}

		time.Sleep(30 * sec) // t=115: the poll at 90s gave up at 110s
		synctest.Wait()
		if src := sessionsSource(t, r); !src.Stale || !src.FailingSince.Equal(base.Add(50*sec)) || src.Runs != 4 {
			t.Errorf("after a third timeout: %+v, want still stale since 50s, 4 runs", src)
		}
		if n := r.notes.count(api.TopicStatus); n != statusNotes+1 {
			t.Errorf("status notified %d times, want 1", n-statusNotes)
		}
		wantLogged(t, log, map[string]int{"source timed out": 1, "source failing": 1, "source recovered": 0})
		if live, ended := r.st.liveSessions(), r.st.endedKeys(); len(live) != 1 || len(ended) != 0 {
			t.Errorf("timeouts changed sessions: live %v, ended %v", keys(live), ended)
		}

		// The poll at 120s works.
		r.runner.setDelay(0)
		time.Sleep(10 * sec) // t=125
		synctest.Wait()
		if src := sessionsSource(t, r); src.Stale || src.Error != "" || !src.LastOK.Equal(base.Add(120*sec)) {
			t.Errorf("after recovery: %+v", src)
		}
		if n := r.notes.count(api.TopicStatus); n != statusNotes+2 {
			t.Errorf("status notified %d times, want 2 (stale, recovered)", n-statusNotes)
		}
		wantLogged(t, log, map[string]int{"source timed out": 1, "source failing": 1, "source recovered": 1})
	})
}

// A timeout marks a source failing at once when its data is old: its last
// success, or before any aimd's start, is more than three intervals back.
// Any other error marks it failing at once.
func TestSourceFailingRules(t *testing.T) {
	timeout := fmt.Errorf("sessions: claude agents --json --all: timed out after 20s: %w", context.DeadlineExceeded)
	broken := errors.New(`sessions: exec: "claude": executable file not found in $PATH`)
	type run struct {
		at  time.Duration // after aimd's start
		err error
	}
	for _, tt := range []struct {
		name string
		runs []run
		want time.Duration // FailingSince after aimd's start; -1 for not failing
	}{
		{"first run times out within three intervals", []run{{90 * sec, timeout}}, -1},
		{"first run times out later", []run{{91 * sec, timeout}}, 91 * sec},
		{"a timeout three intervals after a success", []run{{100 * sec, nil}, {190 * sec, timeout}}, -1},
		{"a timeout more than three intervals after", []run{{100 * sec, nil}, {191 * sec, timeout}}, 191 * sec},
		{"two timeouts in a row", []run{{0, nil}, {20 * sec, timeout}, {50 * sec, timeout}}, 20 * sec},
		{"timeouts with a success between", []run{{0, nil}, {20 * sec, timeout}, {30 * sec, nil}, {50 * sec, timeout}}, -1},
		{"another error", []run{{0, nil}, {20 * sec, broken}}, 20 * sec},
		{"a timeout, then another error", []run{{0, nil}, {20 * sec, timeout}, {30 * sec, broken}}, 20 * sec},
		{"recovered", []run{{0, nil}, {20 * sec, broken}, {30 * sec, nil}}, -1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t, nil) // not started: sessions every 30s
			for _, run := range tt.runs {
				r.d.ran(api.SourceSessions, r.d.startedAt.Add(run.at), run.err, 0)
			}
			src := sessionsSource(t, r)
			switch {
			case tt.want < 0 && (src.Stale || !src.FailingSince.IsZero()):
				t.Errorf("failing since %v, want not failing", src.FailingSince.Sub(r.d.startedAt))
			case tt.want >= 0 && (!src.Stale || !src.FailingSince.Equal(r.d.startedAt.Add(tt.want))):
				t.Errorf("source %+v, want failing since %v", src, tt.want)
			}
			if last := tt.runs[len(tt.runs)-1]; last.err != nil && src.Error != last.err.Error() {
				t.Errorf("error %q, want %q", src.Error, last.err)
			}
		})
	}
}

// A GitHub tick that runs past the next one skips it rather than queuing
// it, so ticks never run back to back.
func TestGitHubSlowTickSkipsTheNext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := ghRig(t)
		r.gh.branchDelay = 90 * sec // each tick takes 90s; they're due every 60s
		base := time.Now()
		r.start(t)
		time.Sleep(400 * sec)
		synctest.Wait()
		branch, _ := r.gh.calls()
		var at []time.Duration
		for _, c := range branch {
			at = append(at, c.at.Sub(base))
		}
		if want := []time.Duration{0, 120 * sec, 240 * sec, 360 * sec}; !slices.Equal(at, want) {
			t.Errorf("BranchPRs at %v, want %v", at, want)
		}
	})
}
