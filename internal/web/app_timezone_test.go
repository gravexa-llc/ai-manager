package web

import (
	"context"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/timezone"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// The Time zone setting: System (the machine's zone) or a zone picked in
// Settings, and every clock time and day the app writes in it.

// zoneOf loads a zone by name.
func zoneOf(t *testing.T, name string) timezone.Zone {
	t.Helper()
	loc, err := timezone.Load(name)
	must(t, err)
	return timezone.Zone{Name: name, Loc: loc}
}

// fixedZone is an Options.SystemZone: a machine in z.
func fixedZone(z timezone.Zone) func() timezone.Zone {
	return func() timezone.Zone { return z }
}

// lateNY is late on Nov 5 in New York (21:00), already Nov 6 in UTC
// (02:00): the same instants fall on different days.
var lateNY = time.Date(2026, 11, 6, 2, 0, 0, 0, time.UTC)

func TestClockInZones(t *testing.T) {
	ny := zoneOf(t, "America/New_York")
	earlier := lateNY.Add(-150 * time.Minute)                // Nov 5 23:30 UTC, 18:30 in New York
	morning := time.Date(2026, 11, 6, 14, 0, 0, 0, time.UTC) // Nov 6 14:00 UTC, 09:00 in New York
	for _, c := range []struct {
		zone                              timezone.Zone
		clock, due, stamp, started, ticks string
	}{
		{timezone.UTC, "Nov 5 23:30", "14:00", "2026-11-05T23:30:00Z", "Nov 5 23:30 (2h ago)", "Nov 5 06:00 · 12:00 · 18:00 · Nov 6"},
		{ny, "18:30", "tomorrow 09:00", "2026-11-05T18:30:00-05:00", "18:30 (2h ago)", "Nov 5 · 06:00 · 12:00 · 18:00"},
	} {
		now := lateNY.In(c.zone.Loc)
		got := []string{clock(now, earlier), dueText(now, morning), stamp(now, earlier), startedText(now, earlier)}
		if want := []string{c.clock, c.due, c.stamp, c.started}; !slices.Equal(got, want) {
			t.Errorf("%s: clock, due, stamp, started = %q, want %q", c.zone.Name, got, want)
		}
		// The axis over the last day: midnight is the zone's.
		lo := now.Add(-24 * time.Hour)
		pct := func(x time.Time) float64 { return float64(x.Sub(lo)) / float64(24*time.Hour) * 100 }
		var labels []string
		for _, tk := range timelineTicks(lo, now, pct, func(f float64) string { return strconv.FormatFloat(f, 'f', 1, 64) }) {
			labels = append(labels, tk.Label)
		}
		if got := strings.Join(labels, " · "); got != c.ticks {
			t.Errorf("%s: ticks %s, want %s", c.zone.Name, got, c.ticks)
		}
	}
}

// The Timeline draws its axis and tips in the setting's zone and says
// which zone that is.
func TestTimelineInTimeZone(t *testing.T) {
	ny := zoneOf(t, "America/New_York")
	earlier := lateNY.Add(-150 * time.Minute)
	b := api.Plan{Slug: "p", Title: "P", Total: 1, Counts: map[string]int{"working": 1}, Packages: []api.Package{
		{ID: "W1", Title: "Routes", Lifecycle: "working", Since: earlier},
	}}
	tl := api.Timeline{Entries: []api.TimelineEntry{
		{At: earlier, Kind: api.TimelineLifecycle, Packages: []string{"W1"}, Lifecycle: "working", Prev: "ready"},
	}}
	srv, st := newApp(t, func(o *Options) {
		o.Plans = tlPlans{fakePlans: fakePlans{plans: []api.Plan{b}, sums: map[string]api.PlanSummary{"p": {}}}, tl: tl}
		o.Now = func() time.Time { return lateNY }
		o.SystemZone = fixedZone(ny)
	})
	h := srv.Handler()

	// System: the machine's zone, New York.
	list := inner(t, get(t, h, "/app/plans/p?view=timeline"), "list")
	wantIn(t, "System", list, "W1 Routes: Working since 18:30 (2h)", `>Nov 5</text>`, `>18:00</text>`, "Times in America/New_York.")
	wantNotIn(t, "System", list, `>Nov 6</text>`, "23:30")

	// UTC, picked in Settings: the same history, a day later at midnight.
	_, err := st.SetSettings(context.Background(), map[string]string{store.SettingTimeZone: "UTC"})
	must(t, err)
	list = inner(t, get(t, h, "/app/plans/p?view=timeline"), "list")
	wantIn(t, "UTC", list, "W1 Routes: Working since Nov 5 23:30 (2h)", `>Nov 5 06:00</text>`, `>Nov 6</text>`, "Times in UTC.")
	wantNotIn(t, "UTC", list, "18:30", "America/New_York")
}

// Settings › Appearance › Time zone: a menu of System (naming the
// machine's zone), the common zones and all of them; a change is saved,
// checked, undoable, and redraws every open page.
func TestTimeZoneSetting(t *testing.T) {
	e := newSettingsEnv(t)
	e.srv.opts.SystemZone = fixedZone(zoneOf(t, "America/New_York"))
	content := get(t, e.h, "/app/settings?fragment=content")
	wantIn(t, "row", content,
		`<label class="trow-title" for="sel-time_zone">Time zone</label>`,
		`<form method="post" action="/app/settings/set" data-pane="content" class="set-zone">`,
		`<input type="hidden" name="key" value="time_zone">`,
		`<select id="sel-time_zone" name="value" aria-describedby="sel-time_zone-desc">`,
		`<option value="system" selected>System (America/New_York)</option>`,
		`<optgroup label="Common">`, `<option value="Asia/Tokyo">Asia/Tokyo</option>`, `<option value="UTC">UTC</option>`,
		`<button type="submit" id="sel-time_zone-set" class="btn sm">Set</button>`,
	)
	if timezone.All() != nil {
		wantIn(t, "row", content, `<optgroup label="All zones">`)
	}
	if n := strings.Count(content, " selected>"); n != 1 {
		t.Errorf("%d options selected", n)
	}

	// The hub's topics after a change.
	c, _ := e.srv.opts.Hub.add()
	defer e.srv.opts.Hub.remove(c)
	topics := func() []string {
		c.mu.Lock()
		defer c.mu.Unlock()
		got := slices.Sorted(maps.Keys(c.pending))
		c.pending = map[string]uint64{}
		return got
	}

	a := e.set(store.SettingTimeZone, "Asia/Tokyo")
	if a.Toast == nil || a.Toast.Text != "Time zone: Asia/Tokyo" || a.Toast.Undo == nil || a.Toast.Undo.Fields["value"] != store.TimeZoneSystem {
		t.Fatalf("answer %+v", a.Toast)
	}
	if got := e.srv.settings(context.Background()).TimeZone; got != "Asia/Tokyo" {
		t.Errorf("stored %q", got)
	}
	if got, want := topics(), []string{"inbox", "plans", "prs", "sessions", "status", "worktrees"}; !slices.Equal(got, want) {
		t.Errorf("a new zone publishes %v, want every topic %v", got, want)
	}
	content = get(t, e.h, "/app/settings?fragment=content")
	wantIn(t, "chosen", content, `<option value="system">System (America/New_York)</option>`, `<option value="Asia/Tokyo" selected>Asia/Tokyo</option>`)
	if n := strings.Count(content, " selected>"); n != 1 {
		t.Errorf("%d options selected (a zone is in both lists, chosen once)", n)
	}

	// Undo goes back to System; another setting publishes only the inbox.
	u := a.Toast.Undo
	vals := url.Values{}
	for k, v := range u.Fields {
		vals.Set(k, v)
	}
	a = e.answer(e.postSettings(u.Action, vals, true), http.StatusOK)
	if a.Toast == nil || a.Toast.Text != "Time zone: System (America/New_York)" || a.Toast.Undo != nil {
		t.Errorf("undo %+v", a.Toast)
	}
	topics()
	e.set(store.SettingChime, store.Off)
	if got := topics(); !slices.Equal(got, []string{"inbox"}) {
		t.Errorf("the chime publishes %v", got)
	}

	// A name that isn't a zone is refused, and nothing changes.
	for _, bad := range []string{"Mars/Olympus_Mons", "Local", "", "../zoneinfo/UTC"} {
		rec := e.postSettings("/app/settings/set", url.Values{"key": {store.SettingTimeZone}, "value": {bad}, "return": {"/app/settings"}}, true)
		a = e.answer(rec, http.StatusBadRequest)
		if a.Error != "Can't save that: time_zone is system or a time zone this machine knows, such as America/New_York." {
			t.Errorf("%q: %q", bad, a.Error)
		}
	}
	if got := e.srv.settings(context.Background()).TimeZone; got != store.TimeZoneSystem {
		t.Errorf("after refusals: %q", got)
	}
}

// GET /api/settings names the zone and the machine's; POST changes it,
// all or none.
func TestTimeZoneAPI(t *testing.T) {
	e := newSettingsEnv(t)
	e.srv.opts.SystemZone = fixedZone(zoneOf(t, "America/New_York"))
	var got api.Settings
	decode(t, do(t, e.h, "GET", "/api/settings", nil, nil), &got)
	if got.TimeZone != "system" || got.SystemTimeZone != "America/New_York" {
		t.Errorf("GET: time_zone %q, system_time_zone %q", got.TimeZone, got.SystemTimeZone)
	}
	token := map[string]string{"X-AIM-Token": testToken}
	rec := do(t, e.h, "POST", "/api/settings", strings.NewReader(`{"time_zone": "Europe/Berlin"}`), token)
	got = api.Settings{}
	decode(t, rec, &got)
	if rec.Code != http.StatusOK || got.TimeZone != "Europe/Berlin" || got.SystemTimeZone != "America/New_York" {
		t.Errorf("POST = %d %+v", rec.Code, got)
	}
	if rec := do(t, e.h, "POST", "/api/settings", strings.NewReader(`{"chime": false, "time_zone": "Local"}`), token); rec.Code != http.StatusBadRequest {
		t.Errorf("POST a bad zone = %d %s", rec.Code, rec.Body)
	}
	if set := e.srv.settings(context.Background()); !set.Chime || set.TimeZone != "Europe/Berlin" {
		t.Errorf("a refused POST changed something: %+v", set)
	}
}

// zoneStore is a store whose time zone setting is a given name, even one
// that doesn't load (store.Settings never gives one; this checks the
// pages wouldn't fail if it did).
type zoneStore struct {
	*store.Store
	zone string
}

func (z zoneStore) Settings(ctx context.Context) (store.Settings, error) {
	set, err := z.Store.Settings(ctx)
	set.TimeZone = z.zone
	return set, err
}

// A zone that stops loading is the machine's: no error page.
func TestTimeZoneFallsBackToSystem(t *testing.T) {
	st := newTestStore(t)
	seedDash(t, st)
	srv := newDash(t, zoneStore{st, "Mars/Olympus_Mons"}, func(o *Options) {
		o.Plans = appPlans
		o.SystemZone = fixedZone(zoneOf(t, "America/New_York"))
	})
	h := srv.Handler()
	settings := get(t, h, "/app/settings?fragment=content")
	wantIn(t, "settings", settings, `<option value="system" selected>System (America/New_York)</option>`)
	wantNotIn(t, "settings", settings, "Mars")
	wantIn(t, "inbox", get(t, h, "/app/inbox?fragment=list"), `<span class="iwait" title="waiting since 2026-11-05T06:50:00-05:00">10m</span>`)
}

// The pages write their clock times, tooltips and days in the chosen
// zone; datetime= attributes stay UTC.
func TestPagesInTimeZone(t *testing.T) {
	e := newSettingsEnv(t)
	e.set(store.SettingTimeZone, "America/New_York")
	// Needs you: the wait's tooltip, and a snooze's end.
	wantIn(t, "inbox", get(t, e.h, "/app/inbox?fragment=list"), `<span class="iwait" title="waiting since 2026-11-05T06:50:00-05:00">10m</span>`)
	id := e.id("o/app#8")
	e.answer(e.post("snooze", url.Values{"item": {id}, "snooze": {"15m"}, "return": {"/app/inbox?tab=open"}}, true), http.StatusOK)
	wantIn(t, "snoozed", get(t, e.h, "/app/inbox?tab=snoozed&fragment=list"), `<span class="iwait" title="snoozed until 2026-11-05T07:15:00-05:00">until 07:15</span>`)
	// Sessions: when one started; its line says how long it has run, with
	// the start in its tooltip.
	wantIn(t, "session", get(t, e.h, "/app/sessions?s=pid:11&fragment=detail"), "<dt>Started</dt><dd>06:00</dd>",
		`<span title="running since 2026-11-05T06:00:00-05:00">1h 0m</span>`)

	// "Tomorrow 9am" is the zone's: at 21:00 in Tokyo, the next morning
	// there is 00:00 UTC.
	e.set(store.SettingTimeZone, "Asia/Tokyo")
	id = e.id("o/app#7")
	e.answer(e.post("snooze", url.Values{"item": {id}, "snooze": {"tomorrow"}, "return": {"/app/inbox?tab=open"}}, true), http.StatusOK)
	if due := e.item("o/app#7").DueAt; !due.Equal(time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("tomorrow 9am in Tokyo = %v", due.UTC())
	}
	// (html/template writes the offset's + as &#43; in an attribute.)
	wantIn(t, "snoozed", get(t, e.h, "/app/inbox?tab=snoozed&fragment=list"),
		`title="snoozed until 2026-11-06T09:00:00&#43;09:00">until tomorrow 09:00</span>`,
		`title="snoozed until 2026-11-05T21:15:00&#43;09:00">until 21:15</span>`)
}

func TestOverviewInTimeZone(t *testing.T) {
	st := ovActivityWorld(t)
	_, err := st.SetSettings(context.Background(), map[string]string{store.SettingTimeZone: "America/New_York"})
	must(t, err)
	page := get(t, newDash(t, st, func(o *Options) { o.Plans = ovBoards }).Handler(), "/app/")
	wantIn(t, "feed", section(t, page, "ov-activity"), `<time class="when" datetime="2026-11-05T11:50:00Z" title="06:50">10m</time>`)
}
