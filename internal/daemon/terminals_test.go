package daemon

import (
	"bytes"
	"errors"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/term"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// lockedBuffer is a log sink the daemon's goroutines can share.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// recordedProcs is the process snapshot recorded on the lead's Mac.
func recordedProcs(t *testing.T) term.Procs {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "ps", "mac-2026-11-06.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return term.ParseProcs(string(b))
}

// Each session poll takes one process snapshot and says what terminal app
// each session runs in. A failed snapshot keeps the last answers.
func TestSessionTerminals(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, nil)
		var log lockedBuffer
		r.d.log = slog.New(slog.NewTextHandler(&log, nil))
		procs := recordedProcs(t)
		r.procs.set(procs, nil)
		old := time.Now().Add(-time.Hour).UnixMilli()
		lead := entry{PID: 91470, SessionID: "lead", Cwd: "/r/a", Kind: "interactive", StartedAt: old, Status: "idle"}
		inTerminal := entry{PID: 69087, SessionID: "t", Cwd: "/r/b", Kind: "interactive", StartedAt: old, Status: "busy"}
		// A background job's process has a tty: claude bg-pty-host's.
		job := entry{ID: "0a1b2c3d", PID: 53221, SessionID: "bg", Cwd: "/r/c", Kind: "background", StartedAt: old, Status: "idle", State: "blocked"}
		gone := entry{PID: 4242, SessionID: "gone", Cwd: "/r/d", Kind: "interactive", StartedAt: old, Status: "idle"}
		r.runner.set(lead, inTerminal, job, gone)
		r.start(t)
		synctest.Wait()

		iTerm2 := term.App{Kind: term.KindITerm2, Name: term.AppITerm}
		terminal := term.App{Kind: term.KindTerminal, Name: term.AppTerminal}
		want := map[string]term.App{
			"pid:91470":    iTerm2,
			"pid:69087":    terminal,
			"job:0a1b2c3d": term.NoTerminal,
			"pid:4242":     term.NoTerminal, // not in the snapshot
		}
		if got := r.d.Terminals(); !maps.Equal(got, want) {
			t.Errorf("Terminals() = %v, want %v", got, want)
		}
		if n := r.procs.count(); n != 1 {
			t.Errorf("%d process snapshots after one poll, want 1", n)
		}
		notified := r.notes.count(api.TopicSessions)

		// Identical polls: nothing changed, nothing is notified.
		time.Sleep(30 * time.Second)
		synctest.Wait()
		if n := r.notes.count(api.TopicSessions); n != notified {
			t.Errorf("an identical poll notified sessions (%d → %d)", notified, n)
		}

		// ps fails: the last answers stand, and a new session has none.
		// The failure is logged once, though its message changes.
		fresh := entry{PID: 73461, SessionID: "new", Cwd: "/r/e", Kind: "interactive", StartedAt: old, Status: "idle"}
		r.runner.set(lead, inTerminal, job, gone, fresh)
		for _, msg := range []string{"ps: signal: killed", "ps: timed out after 10s: context deadline exceeded"} {
			r.procs.set(nil, errors.New(msg))
			time.Sleep(30 * time.Second)
			synctest.Wait()
		}
		want["pid:73461"] = term.NoTerminal
		if got := r.d.Terminals(); !maps.Equal(got, want) {
			t.Errorf("after ps failed: Terminals() = %v, want %v", got, want)
		}
		if n := strings.Count(log.String(), "process snapshot failed"); n != 1 {
			t.Errorf("the failure was logged %d times, want once:\n%s", n, log.String())
		}

		// ps works again; the new session is found, and that change alone
		// is notified.
		notified = r.notes.count(api.TopicSessions)
		r.procs.set(procs, nil)
		time.Sleep(30 * time.Second)
		synctest.Wait()
		want["pid:73461"] = terminal
		if got := r.d.Terminals(); !maps.Equal(got, want) {
			t.Errorf("after ps recovered: Terminals() = %v, want %v", got, want)
		}
		if !strings.Contains(log.String(), "process snapshot works again") {
			t.Errorf("recovery not logged:\n%s", log.String())
		}
		if n := r.notes.count(api.TopicSessions); n != notified+1 {
			t.Errorf("a terminal found: sessions notified %d times, want %d", n, notified+1)
		}

		// Only background sessions left: no snapshot is needed.
		calls := r.procs.count()
		r.runner.set(job)
		time.Sleep(30 * time.Second)
		synctest.Wait()
		if n := r.procs.count(); n != calls {
			t.Errorf("a snapshot was taken for background sessions only")
		}
		if got := r.d.Terminals(); !maps.Equal(got, map[string]term.App{"job:0a1b2c3d": term.NoTerminal}) {
			t.Errorf("background only: Terminals() = %v", got)
		}
	})
}
