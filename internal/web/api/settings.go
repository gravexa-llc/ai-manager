package api

// The Settings page's settings: GET and POST /api/settings.
// docs/api.md describes them.

// Settings is GET /api/settings, and the answer to POST /api/settings:
// every setting, each its default until changed.
type Settings struct {
	// The alert switches: the needs-you count in the tab title, aimd's
	// macOS notifications, a chime for new permission prompts and
	// questions on the Needs you page, and the highlight of new inbox
	// items.
	TabCount      bool `json:"tab_count"`
	Notifications bool `json:"notifications"`
	Chime         bool `json:"chime"`
	Highlight     bool `json:"highlight"`
	// BackToOpenMinutes is how long after Go an inbox item that still
	// waits returns to Open: 5, 10 or 30. KeepDoneDays is how long Done
	// items are kept: 7, 30 or 90.
	BackToOpenMinutes int `json:"back_to_open_minutes"`
	KeepDoneDays      int `json:"keep_done_days"`
	// Shortcuts turns the app's keyboard shortcuts on.
	Shortcuts bool `json:"shortcuts"`
	// Theme is the app's colors: "system" follows the Mac's
	// appearance, "dark" and "light" force one.
	Theme string `json:"theme"`
	// TimeZone is the zone the dashboard writes clock times in: "system"
	// (the machine's), or an IANA name such as "America/New_York".
	TimeZone string `json:"time_zone"`
	// SystemTimeZone names the machine's zone, which "system" is
	// (read-only).
	SystemTimeZone string `json:"system_time_zone"`
	// Merges says who merges each registered plan's PRs, in registration
	// order.
	Merges []PlanMerges `json:"merges"`
}

// PlanMerges is who merges one plan's PRs.
type PlanMerges struct {
	Plan  string `json:"plan"`
	Title string `json:"title"`
	// Merges is who merges them now, "you" or "lead": the override if
	// there is one, else the plan's Merges line (Line).
	Merges   string `json:"merges"`
	Line     string `json:"line"`
	Override bool   `json:"override"`
}

// SettingsUpdate is the body of POST /api/settings: the settings to
// change, any of them. Merges maps a plan's slug to "you" or "lead", or to
// "" (or null) to drop the override, so the plan's Merges line decides
// again.
type SettingsUpdate struct {
	TabCount          *bool              `json:"tab_count"`
	Notifications     *bool              `json:"notifications"`
	Chime             *bool              `json:"chime"`
	Highlight         *bool              `json:"highlight"`
	BackToOpenMinutes *int               `json:"back_to_open_minutes"`
	KeepDoneDays      *int               `json:"keep_done_days"`
	Shortcuts         *bool              `json:"shortcuts"`
	Theme             *string            `json:"theme"`
	TimeZone          *string            `json:"time_zone"`
	Merges            map[string]*string `json:"merges"`
}
