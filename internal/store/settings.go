package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/timezone"
)

// This file holds the Settings page's settings (migration 0010): the alert
// switches, the inbox's timings, the keyboard shortcuts, the theme and
// the time zone, and the per-plan overrides of who merges a plan's
// PRs. They live in aimd's store, not in config.toml, so the page can
// change them.
//
// A setting has a key and a value in its stored form: "on" or "off" for a
// switch, a number of minutes or days for a timing, a word for the theme,
// "system" or an IANA name for the time zone.
// A missing row is the default (DefaultSettings), and so is a row this
// build can't read, so a database written by a newer build never breaks an
// older one.

// Setting keys.
const (
	// SettingTabCount: the needs-you count in the browser tab's title.
	SettingTabCount = "tab_count"
	// SettingNotifications: aimd's macOS notifications (internal/notify).
	SettingNotifications = "notifications"
	// SettingChime: a chime for new permission prompts and questions.
	SettingChime = "chime"
	// SettingHighlight: new inbox items stay highlighted until opened.
	SettingHighlight = "highlight"
	// SettingShortcuts: the app's keyboard shortcuts.
	SettingShortcuts = "shortcuts"
	// SettingBackToOpen: minutes after Go before an item still waiting
	// returns to Open (BackToOpenChoices).
	SettingBackToOpen = "back_to_open_minutes"
	// SettingKeepDone: days a Done item is kept (KeepDoneChoices).
	SettingKeepDone = "keep_done_days"
	// SettingTheme: the app's colors, one of ThemeChoices.
	SettingTheme = "theme"
	// SettingTimeZone: the zone the app's clock times are in,
	// TimeZoneSystem or an IANA name such as "America/New_York".
	SettingTimeZone = "time_zone"
)

// SettingNames lists the keys in the Settings page's order.
var SettingNames = []string{SettingTabCount, SettingNotifications, SettingChime, SettingHighlight,
	SettingBackToOpen, SettingKeepDone, SettingShortcuts, SettingTheme, SettingTimeZone}

// The values a switch holds.
const (
	On  = "on"
	Off = "off"
)

// The timings' choices, as the spec gives them.
var (
	BackToOpenChoices = []int{5, 10, 30} // minutes
	KeepDoneChoices   = []int{7, 30, 90} // days
)

// The themes: System follows the Mac's appearance (the browser's
// prefers-color-scheme); Dark and Light force one.
const (
	ThemeSystem = "system"
	ThemeDark   = "dark"
	ThemeLight  = "light"
)

// ThemeChoices are the themes, in the Settings page's order.
var ThemeChoices = []string{ThemeSystem, ThemeDark, ThemeLight}

// TimeZoneSystem is the time zone of the machine aimd runs on
// (timezone.Machine). Any other value is an IANA name that loads
// (timezone.Load).
const TimeZoneSystem = "system"

// Settings are the stored settings, each its default unless changed.
type Settings struct {
	TabCount      bool
	Notifications bool
	Chime         bool
	Highlight     bool
	Shortcuts     bool
	// BackToOpenMinutes is one of BackToOpenChoices, KeepDoneDays one of
	// KeepDoneChoices.
	BackToOpenMinutes int
	KeepDoneDays      int
	// Theme is one of ThemeChoices.
	Theme string
	// TimeZone is TimeZoneSystem or an IANA name that loads. A stored
	// name this machine can't load any more reads as TimeZoneSystem.
	TimeZone string
}

// DefaultSettings are the settings before any change: all four alerts
// (the count in the tab title, notifications, the chime and highlighting)
// and the shortcuts on, as the user chose in the spec's Q&A; back to Open
// after 10 minutes; Done kept 30 days; the System theme; the system's
// time zone.
func DefaultSettings() Settings {
	return Settings{TabCount: true, Notifications: true, Chime: true, Highlight: true, Shortcuts: true,
		BackToOpenMinutes: 10, KeepDoneDays: 30, Theme: ThemeSystem, TimeZone: TimeZoneSystem}
}

// BackToOpen is the back-to-Open delay.
func (s Settings) BackToOpen() time.Duration {
	return time.Duration(s.BackToOpenMinutes) * time.Minute
}

// KeepDone is how long a Done item is kept.
func (s Settings) KeepDone() time.Duration {
	return time.Duration(s.KeepDoneDays) * 24 * time.Hour
}

// Value is a setting's value in its stored form, or "" for an unknown key.
func (s Settings) Value(key string) string {
	onOff := func(b bool) string {
		if b {
			return On
		}
		return Off
	}
	switch key {
	case SettingTabCount:
		return onOff(s.TabCount)
	case SettingNotifications:
		return onOff(s.Notifications)
	case SettingChime:
		return onOff(s.Chime)
	case SettingHighlight:
		return onOff(s.Highlight)
	case SettingShortcuts:
		return onOff(s.Shortcuts)
	case SettingBackToOpen:
		return strconv.Itoa(s.BackToOpenMinutes)
	case SettingKeepDone:
		return strconv.Itoa(s.KeepDoneDays)
	case SettingTheme:
		return s.Theme
	case SettingTimeZone:
		return s.TimeZone
	}
	return ""
}

// ErrBadSetting is returned for an unknown key or a value a setting can't
// hold. Its message says what the setting takes.
var ErrBadSetting = errors.New("store: bad setting")

// Set changes one setting from its stored form. An unknown key or a value
// the setting can't hold is an ErrBadSetting, and leaves s as it was.
func (s *Settings) Set(key, value string) error {
	flag := func(b *bool) error {
		switch value {
		case On:
			*b = true
		case Off:
			*b = false
		default:
			return fmt.Errorf("%w: %s is on or off", ErrBadSetting, key)
		}
		return nil
	}
	choice := func(n *int, choices []int, unit string) error {
		v, err := strconv.Atoi(value)
		if err != nil || !slices.Contains(choices, v) {
			return fmt.Errorf("%w: %s is %s %s", ErrBadSetting, key, choiceList(choices), unit)
		}
		*n = v
		return nil
	}
	switch key {
	case SettingTabCount:
		return flag(&s.TabCount)
	case SettingNotifications:
		return flag(&s.Notifications)
	case SettingChime:
		return flag(&s.Chime)
	case SettingHighlight:
		return flag(&s.Highlight)
	case SettingShortcuts:
		return flag(&s.Shortcuts)
	case SettingBackToOpen:
		return choice(&s.BackToOpenMinutes, BackToOpenChoices, "minutes")
	case SettingKeepDone:
		return choice(&s.KeepDoneDays, KeepDoneChoices, "days")
	case SettingTheme:
		if !slices.Contains(ThemeChoices, value) {
			return fmt.Errorf("%w: %s is %s", ErrBadSetting, key, wordList(ThemeChoices))
		}
		s.Theme = value
		return nil
	case SettingTimeZone:
		if value != TimeZoneSystem {
			if _, err := timezone.Load(value); err != nil {
				return fmt.Errorf("%w: %s is %s or a time zone this machine knows, such as America/New_York", ErrBadSetting, key, TimeZoneSystem)
			}
		}
		s.TimeZone = value
		return nil
	}
	return fmt.Errorf("%w: there is no setting %q", ErrBadSetting, key)
}

// choiceList writes 5, 10 or 30.
func choiceList(cs []int) string {
	parts := make([]string, len(cs))
	for i, c := range cs {
		parts[i] = strconv.Itoa(c)
	}
	return wordList(parts)
}

// wordList writes system, dark or light.
func wordList(parts []string) string {
	if len(parts) < 2 {
		return strings.Join(parts, "")
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " or " + parts[len(parts)-1]
}

// Settings reads the settings: the defaults, with the stored rows over
// them. A row this build can't read is skipped.
func (s *Store) Settings(ctx context.Context) (Settings, error) {
	set := DefaultSettings()
	rows, err := s.r.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return set, fmt.Errorf("store: read settings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return DefaultSettings(), fmt.Errorf("store: read settings: %w", err)
		}
		_ = set.Set(k, v) // the default stands for what this build can't read
	}
	if err := rows.Err(); err != nil {
		return DefaultSettings(), fmt.Errorf("store: read settings: %w", err)
	}
	return set, nil
}

// SetSettings stores the settings in values (key → stored form), all or
// none: an unknown key or a bad value is an ErrBadSetting and stores
// nothing. It returns the settings as they are now.
func (s *Store) SetSettings(ctx context.Context, values map[string]string) (Settings, error) {
	var check Settings
	for k, v := range values {
		if err := check.Set(k, v); err != nil {
			return Settings{}, err
		}
	}
	now := s.now().UnixMilli()
	err := s.writeTx(ctx, func(tx *sql.Tx) error {
		for k, v := range values {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
				ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
				k, v, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Settings{}, fmt.Errorf("store: save settings: %w", err)
	}
	return s.Settings(ctx)
}

// Who merges a plan's PRs, as a plan_merges row says it (internal/plans'
// MergesYou and MergesLead).
const (
	MergesYou  = "you"
	MergesLead = "lead"
)

// PlanMerges returns the overrides of who merges each plan's PRs, by the
// plan's lower-case slug: MergesYou or MergesLead. A plan without one
// isn't in the map; its Merges line decides.
func (s *Store) PlanMerges(ctx context.Context) (map[string]string, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT p.slug, m.merges FROM plan_merges m JOIN plans p ON p.id = m.plan_id`)
	if err != nil {
		return nil, fmt.Errorf("store: read plan merges: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var slug, merges string
		if err := rows.Scan(&slug, &merges); err != nil {
			return nil, fmt.Errorf("store: read plan merges: %w", err)
		}
		if merges == MergesYou || merges == MergesLead {
			out[strings.ToLower(slug)] = merges
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read plan merges: %w", err)
	}
	return out, nil
}

// SetPlanMerges overrides who merges the PRs of the plan with the slug
// (any case): MergesYou or MergesLead, or "" to drop the override, so the
// plan's Merges line decides again. A plan that isn't registered is
// ErrNotFound; a bad value is an ErrBadSetting.
func (s *Store) SetPlanMerges(ctx context.Context, slug, merges string) error {
	if merges != "" && merges != MergesYou && merges != MergesLead {
		return fmt.Errorf("%w: who merges is %s or %s", ErrBadSetting, MergesYou, MergesLead)
	}
	p, err := s.GetPlanBySlug(ctx, slug)
	if err != nil {
		return err
	}
	if merges == "" {
		_, err = s.w.ExecContext(ctx, `DELETE FROM plan_merges WHERE plan_id = ?`, p.ID)
	} else {
		_, err = s.w.ExecContext(ctx, `
			INSERT INTO plan_merges (plan_id, merges, updated_at) VALUES (?, ?, ?)
			ON CONFLICT (plan_id) DO UPDATE SET merges = excluded.merges, updated_at = excluded.updated_at`,
			p.ID, merges, s.now().UnixMilli())
	}
	if err != nil {
		return fmt.Errorf("store: set plan merges %q: %w", slug, err)
	}
	return nil
}
