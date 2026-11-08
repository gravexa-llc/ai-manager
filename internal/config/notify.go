package config

import "time"

// Notify is the [notify] table: aimd's desktop notifications
// (internal/notify).
//
//	[notify]
//	enabled = true   # false: aimd posts no notifications
//	after = "2m"     # remind about an interactive session waiting this long
//
// Claude Code already notifies when an interactive session needs you, so
// aimd notifies only when a background job becomes blocked, and reminds
// you about an interactive session that has waited longer than After and
// that you haven't dismissed on the dashboard.
type Notify struct {
	Enabled bool          `toml:"enabled"`
	After   time.Duration `toml:"after"`
}

// DefaultNotify is the [notify] table when config.toml sets nothing.
func DefaultNotify() Notify {
	return Notify{Enabled: true, After: 2 * time.Minute}
}
