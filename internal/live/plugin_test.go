package live

import (
	"context"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/store"
)

// The plugin build a session loaded, from its session_start, goes on its
// row and stays there: through a later session_start that doesn't say
// (an older mod's /clear, a compact), the poller adopting the row, and an
// aimd restart.
func TestPluginBuildOnTheSessionRow(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	const build, root = "v1.0.0-3-gabc1234", "/r/app/plugin"
	s := start("s1", t0)
	s.PluginBuild, s.PluginRoot = build, root
	h.post(s)
	check := func(where string, row store.Session) {
		t.Helper()
		if row.PluginBuild != build || row.PluginRoot != root {
			t.Errorf("%s: plugin %q in %q, want %q in %q", where, row.PluginBuild, row.PluginRoot, build, root)
		}
	}
	check("after session_start", h.session("s1"))
	if a := h.apiSession("s1"); a.PluginBuild != build || a.PluginRoot != root || a.PluginOutdated {
		t.Errorf("/api/sessions: %+v", a)
	}

	again := start("s1", t0.Add(time.Minute))
	again.Source = "compact"
	h.post(again, tool("s1", "", "Bash: make", "t1", t0.Add(2*time.Minute)))
	check("after a session_start without it", h.session("s1"))

	// The poller lists it: its row adopts the mod's.
	at := t0.Add(3 * time.Minute)
	h.clock.set(at)
	rows := h.recv.RefineSessions(ctx, []store.Session{{Key: "pid:42", SessionID: "s1", PID: 42, State: StateWorking, LastSeenAt: at}}, at)
	must(t, h.st.UpsertSessions(ctx, rows...))
	row, err := h.st.GetSession(ctx, "pid:42")
	must(t, err)
	check("after the poller adopted it", row)

	// A new receiver (aimd restarted) seeds from the row and keeps it.
	h2 := newHarnessOn(t, h.st)
	h2.post(tool("s1", "", "Bash: make", "t2", t0.Add(4*time.Minute)))
	row, err = h.st.GetSession(ctx, "pid:42")
	must(t, err)
	check("after a restart", row)

	// A session whose mod didn't say has none.
	h.post(start("s2", t0))
	if row := h.session("s2"); row.PluginBuild != "" || row.PluginRoot != "" {
		t.Errorf("unreported: %+v", row)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
