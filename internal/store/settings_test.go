package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestSettingsDefaults(t *testing.T) {
	s := newStore(t)
	got, err := s.Settings(context.Background())
	must(t, err)
	want := Settings{TabCount: true, Notifications: true, Chime: true, Highlight: true, Shortcuts: true,
		BackToOpenMinutes: 10, KeepDoneDays: 30, Theme: ThemeSystem}
	equal(t, "defaults", got, want)
	equal(t, "DefaultSettings", DefaultSettings(), want)
	if got.BackToOpen() != 10*time.Minute || got.KeepDone() != 30*24*time.Hour {
		t.Errorf("durations %v, %v", got.BackToOpen(), got.KeepDone())
	}
	for _, k := range SettingNames {
		if got.Value(k) == "" {
			t.Errorf("Value(%q) is empty", k)
		}
	}
	equal(t, "stored forms", []string{got.Value(SettingChime), got.Value(SettingTabCount), got.Value(SettingBackToOpen), got.Value(SettingKeepDone), got.Value(SettingTheme), got.Value("nope")},
		[]string{"on", "on", "10", "30", "system", ""})
}

func TestSetSettings(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	got, err := s.SetSettings(ctx, map[string]string{SettingChime: Off, SettingTabCount: Off, SettingBackToOpen: "5", SettingKeepDone: "90", SettingTheme: ThemeDark})
	must(t, err)
	want := DefaultSettings()
	want.Chime, want.TabCount, want.BackToOpenMinutes, want.KeepDoneDays, want.Theme = false, false, 5, 90, ThemeDark
	equal(t, "after a change", got, want)
	again, err := s.Settings(ctx)
	must(t, err)
	equal(t, "read back", again, want)

	// Changing it again replaces the row.
	got, err = s.SetSettings(ctx, map[string]string{SettingChime: On})
	must(t, err)
	if !got.Chime {
		t.Error("chime still off")
	}

	// All or none: one bad value stores nothing.
	for _, bad := range []map[string]string{
		{SettingHighlight: Off, SettingBackToOpen: "15"},
		{SettingHighlight: Off, SettingKeepDone: "1"},
		{SettingHighlight: Off, SettingShortcuts: "yes"},
		{SettingHighlight: Off, SettingTheme: "Dark"},
		{SettingHighlight: Off, SettingTheme: ""},
		{SettingHighlight: Off, "volume": "11"},
	} {
		if _, err := s.SetSettings(ctx, bad); !errors.Is(err, ErrBadSetting) {
			t.Errorf("SetSettings(%v) = %v, want ErrBadSetting", bad, err)
		}
	}
	now, err := s.Settings(ctx)
	must(t, err)
	if !now.Highlight {
		t.Error("a refused change stored part of itself")
	}
}

// A row this build can't read is the default; a key from a newer build is
// ignored.
func TestSettingsIgnoreWhatTheyCantRead(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_, err := s.w.Exec(`INSERT INTO settings (key, value, updated_at) VALUES ('keep_done_days', '3', 0), ('volume', '11', 0), ('chime', 'off', 0), ('theme', 'sepia', 0)`)
	must(t, err)
	got, err := s.Settings(ctx)
	must(t, err)
	want := DefaultSettings()
	want.Chime = false
	equal(t, "settings", got, want)
}

func TestSettingSetMessages(t *testing.T) {
	var set Settings
	for _, c := range []struct{ key, value, want string }{
		{SettingBackToOpen, "7", "store: bad setting: back_to_open_minutes is 5, 10 or 30 minutes"},
		{SettingKeepDone, "x", "store: bad setting: keep_done_days is 7, 30 or 90 days"},
		{SettingChime, "true", "store: bad setting: chime is on or off"},
		{SettingTheme, "auto", "store: bad setting: theme is system, dark or light"},
		{"nope", "on", `store: bad setting: there is no setting "nope"`},
	} {
		if err := set.Set(c.key, c.value); err == nil || err.Error() != c.want {
			t.Errorf("Set(%q, %q) = %v, want %s", c.key, c.value, err, c.want)
		}
	}
	if set != (Settings{}) {
		t.Errorf("a refused Set changed the settings: %+v", set)
	}
}

func TestPlanMerges(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	aim, err := s.InsertPlan(ctx, Plan{Slug: "aim"})
	must(t, err)
	_, err = s.InsertPlan(ctx, Plan{Slug: "Sharing"})
	must(t, err)

	got, err := s.PlanMerges(ctx)
	must(t, err)
	equal(t, "no overrides", got, map[string]string{})

	must(t, s.SetPlanMerges(ctx, "AIM", MergesLead))
	must(t, s.SetPlanMerges(ctx, "sharing", MergesYou))
	got, err = s.PlanMerges(ctx)
	must(t, err)
	equal(t, "overrides", got, map[string]string{"aim": "lead", "sharing": "you"})

	// Set again replaces; "" drops it.
	must(t, s.SetPlanMerges(ctx, "aim", MergesYou))
	must(t, s.SetPlanMerges(ctx, "sharing", ""))
	must(t, s.SetPlanMerges(ctx, "sharing", "")) // dropping none is fine
	got, err = s.PlanMerges(ctx)
	must(t, err)
	equal(t, "after a reset", got, map[string]string{"aim": "you"})

	if err := s.SetPlanMerges(ctx, "nope", MergesLead); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown plan: %v", err)
	}
	if err := s.SetPlanMerges(ctx, "nope", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("reset of an unknown plan: %v", err)
	}
	if err := s.SetPlanMerges(ctx, "aim", "agent"); !errors.Is(err, ErrBadSetting) {
		t.Errorf("bad value: %v", err)
	}

	// Unregistering the plan drops its override.
	must(t, s.DeletePlan(ctx, aim))
	got, err = s.PlanMerges(ctx)
	must(t, err)
	equal(t, "after unregistering", got, map[string]string{})
}

// 0010 adds the settings tables to a database at version 9 and keeps what
// it held; an override follows its plan's row.
func TestMigration0010(t *testing.T) {
	ctx := context.Background()
	ms, err := loadMigrations(migrationFS)
	must(t, err)
	at := -1
	for i, m := range ms {
		if m.name == "0010_settings.sql" {
			at = i
		}
	}
	if at != 9 {
		t.Fatalf("0010_settings.sql is migration %d", at+1)
	}
	db := rawDB(t)
	must(t, migrate(ctx, db, ms[:at]))
	_, err = db.Exec(`INSERT INTO plans (slug, created_at, updated_at) VALUES ('aim', 0, 0)`)
	must(t, err)
	must(t, migrate(ctx, db, ms[:at+1]))
	equal(t, "versions", versions(t, db)[at-1:], []int{9, 10})

	var n int
	must(t, db.QueryRow(`SELECT count(*) FROM settings`).Scan(&n))
	if n != 0 {
		t.Errorf("settings has %d rows after the migration", n)
	}
	_, err = db.Exec(`INSERT INTO plan_merges (plan_id, merges, updated_at) SELECT id, 'lead', 0 FROM plans WHERE slug = 'aim'`)
	must(t, err)
	if _, err := db.Exec(`INSERT INTO plan_merges (plan_id, merges, updated_at) VALUES (99, 'lead', 0)`); err == nil {
		t.Error("an override of no plan was accepted")
	}
	_, err = db.Exec(`DELETE FROM plans WHERE slug = 'aim'`)
	must(t, err)
	must(t, db.QueryRow(`SELECT count(*) FROM plan_merges`).Scan(&n))
	if n != 0 {
		t.Errorf("plan_merges kept %d rows of a deleted plan", n)
	}
}
