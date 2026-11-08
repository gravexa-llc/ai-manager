package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/monitor"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// fakeBuilds is a PluginBuilds: the build on disk in each folder.
type fakeBuilds map[string]string

func (f fakeBuilds) OnDisk(_ context.Context, root string) string { return f[root] }

const (
	testPluginRoot = "/w/app/plugin"
	oldBuild       = "v1.0.0-3-gabc1234"
	newBuild       = "v1.0.0-5-gdef5678"
)

// newPluginEnv is newSessionsEnv's world with plugin builds: on disk,
// newBuild in testPluginRoot; "older wait" (pid:50, watched) and "fresh
// job" (job:live1, not monitored) loaded oldBuild there, "idle watched"
// (pid:51) newBuild, and "no build" (pid:52, watched) reported events but
// no build.
func newPluginEnv(t *testing.T, builds PluginBuilds) *boardEnv {
	t.Helper()
	e, _ := newSessionsEnv(t, monitor.ModeOptIn, func(o *Options) {
		o.PluginBuilds = builds
		o.Status = fakeStatus{api.Status{Version: "v1.0.0-14-g0123abc", StartedAt: t0.Add(-3 * time.Hour), Now: t0}}
	})
	ctx := context.Background()
	must(t, e.st.UpsertSessions(ctx, store.Session{Key: "pid:52", SessionID: "watch-4", PID: 52, Kind: "interactive",
		Name: "no build", Cwd: "/w/seven", State: "idle", LastSeenAt: t0}))
	e.watchAPI("POST", "watch-4", 200)
	for key, u := range map[string]store.SessionLive{
		"pid:50":    {State: needsYou, StateReason: "input needed", NeedsYouSince: t0.Add(-time.Hour), PluginBuild: oldBuild, PluginRoot: testPluginRoot},
		"pid:51":    {State: "idle", PluginBuild: newBuild, PluginRoot: testPluginRoot},
		"pid:52":    {State: "idle", LastEventAt: t0.Add(-time.Minute)},
		"job:live1": {State: "working", PluginBuild: oldBuild, PluginRoot: testPluginRoot},
	} {
		must(t, e.st.UpdateSessionLive(ctx, key, u))
	}
	return e
}

// rowOf is the list row (its <a>) of a session key.
func rowOf(t *testing.T, list, key string) string {
	t.Helper()
	i := strings.Index(list, `data-item="`+key+`"`)
	if i < 0 {
		t.Fatalf("no row %s in:\n%s", key, list)
	}
	rest := list[i:]
	if j := strings.Index(rest, "</a>"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

func TestAppSessionsOlderPlugin(t *testing.T) {
	e := newPluginEnv(t, fakeBuilds{testPluginRoot: newBuild})
	tag := `<span class="tag tone-warn" title="It runs plugin ` + oldBuild + `; the plugin on disk is ` + newBuild +
		`. Restart the session to update.">Older plugin</span>`

	list := get(t, e.h, "/app/sessions?fragment=list")
	wantIn(t, "older wait row", rowOf(t, list, "pid:50"), tag)
	wantIn(t, "fresh job row (not monitored)", rowOf(t, list, "job:live1"), tag)
	for _, key := range []string{"pid:51", "pid:52"} {
		wantNotIn(t, key+" row", rowOf(t, list, key), "Older plugin")
	}
	wantIn(t, "summary", get(t, e.h, "/app/sessions?fragment=summary"), "· 2 on an older plugin</p>")

	d := get(t, e.h, "/app/sessions?s=pid%3A50&fragment=detail")
	wantIn(t, "older detail", d, tag, "Older plugin: restart to update",
		"It loaded plugin <code>"+oldBuild+"</code> when it started, and keeps it until it restarts. The plugin on disk is now <code>"+newBuild+"</code>.",
		"<code>claude --resume</code>",
		`<dt>Plugin</dt><dd><code title="loaded from `+testPluginRoot+`">`+oldBuild+`</code></dd>`)

	d = get(t, e.h, "/app/sessions?s=pid%3A51&fragment=detail")
	wantIn(t, "current detail", d, `<dt>Plugin</dt><dd><code title="loaded from `+testPluginRoot+`">`+newBuild+`</code></dd>`)
	wantNotIn(t, "current detail", d, "Older plugin")

	d = get(t, e.h, "/app/sessions?s=pid%3A52&fragment=detail")
	wantIn(t, "unreported detail", d, `<dt>Plugin</dt><dd><span class="hint">Not reported: a plugin from before builds were reported`)
	wantNotIn(t, "unreported detail", d, "Older plugin")
	// A session the plugin never reported from (the poller's only) shows
	// no Plugin row.
	wantNotIn(t, "poll only", get(t, e.h, "/app/sessions?s=job%3Aex1&fragment=detail"), "<dt>Plugin</dt>")

	// Nothing is flagged when aimd can't tell what is on disk.
	for name, builds := range map[string]PluginBuilds{"unknown": fakeBuilds{}, "nil": nil} {
		e := newPluginEnv(t, builds)
		page := get(t, e.h, "/app/sessions?s=pid%3A50")
		wantNotIn(t, name, page, "Older plugin", "on an older plugin")
		wantIn(t, name, page, `>`+oldBuild+`</code></dd>`)
	}
	// The build on disk is what is compared: with oldBuild there, only
	// the session on newBuild differs.
	e = newPluginEnv(t, fakeBuilds{testPluginRoot: oldBuild})
	list = get(t, e.h, "/app/sessions?fragment=list")
	wantNotIn(t, "back on disk", rowOf(t, list, "pid:50"), "Older plugin")
	wantIn(t, "back on disk", rowOf(t, list, "pid:51"), "Older plugin")
}

func TestAPISessionsPluginBuild(t *testing.T) {
	e := newPluginEnv(t, fakeBuilds{testPluginRoot: newBuild})
	var out api.Sessions
	decode(t, do(t, e.h, "GET", "/api/sessions", nil, nil), &out)
	byKey := map[string]api.Session{}
	for _, s := range out.Sessions {
		byKey[s.Key] = s
	}
	if s := byKey["pid:50"]; s.PluginBuild != oldBuild || s.PluginRoot != testPluginRoot || s.PluginOnDisk != newBuild || !s.PluginOutdated {
		t.Errorf("older session: %+v", s)
	}
	if s := byKey["pid:51"]; s.PluginBuild != newBuild || s.PluginOnDisk != newBuild || s.PluginOutdated {
		t.Errorf("current session: %+v", s)
	}
	if s := byKey["pid:52"]; s.PluginBuild != "" || s.PluginRoot != "" || s.PluginOnDisk != "" || s.PluginOutdated {
		t.Errorf("unreported session: %+v", s)
	}
	rec := do(t, e.h, "GET", "/api/sessions", nil, nil)
	for _, field := range []string{`"plugin_build":"` + oldBuild + `"`, `"plugin_root":"` + testPluginRoot + `"`,
		`"plugin_on_disk":"` + newBuild + `"`, `"plugin_outdated":true`} {
		if !strings.Contains(rec.Body.String(), field) {
			t.Errorf("GET /api/sessions has no %s", field)
		}
	}
}

func TestAppSettingsVersions(t *testing.T) {
	e := newPluginEnv(t, fakeBuilds{testPluginRoot: newBuild})
	page := get(t, e.h, "/app/settings")
	card := page[strings.Index(page, `<h2 id="set-versions">`):]
	wantIn(t, "versions", card,
		`<div class="trow-title">aimd</div><div class="trow-desc"><code>v1.0.0-14-g0123abc</code> · running for 3h</div>`,
		`<div class="trow-title">Plugin on disk</div><div class="trow-desc"><code>`+newBuild+`</code> in <code title="`+testPluginRoot+`">`+testPluginRoot+`</code> · 3 live sessions · 2 on an older build</div>`)
	wantNotIn(t, "versions", card, "No live session has reported")

	// No session reported a build, and no daemon status.
	bare, _ := newSessionsEnv(t, monitor.ModeOptIn, nil)
	card = get(t, bare.h, "/app/settings")
	wantIn(t, "bare versions", card, "<code>unknown</code>", "No live session has reported its plugin build yet.")
}
