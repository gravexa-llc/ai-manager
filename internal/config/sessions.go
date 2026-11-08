package config

import "fmt"

// Sessions is the [sessions] table: which Claude Code sessions aimd
// monitors (internal/monitor).
//
//	[sessions]
//	monitor = "opt-in"   # or "all"
//
// With "opt-in" (the default) aimd monitors only the sessions you chose: a
// lead that registered a plan, a session where you ran /aim watch, and the
// agents of a registered plan's packages or of a monitored lead. The
// Needs you inbox and the desktop notifications follow only those, and the
// dashboard lists every other session in a collapsed "Not monitored" group.
// "all" monitors every session.
type Sessions struct {
	Monitor string `toml:"monitor"`
}

// The values of sessions.monitor.
const (
	MonitorOptIn = "opt-in"
	MonitorAll   = "all"
)

// DefaultSessions is the [sessions] table when config.toml sets nothing.
func DefaultSessions() Sessions {
	return Sessions{Monitor: MonitorOptIn}
}

func (s Sessions) validate() error {
	switch s.Monitor {
	case MonitorOptIn, MonitorAll:
		return nil
	}
	return fmt.Errorf("sessions.monitor must be %q or %q, got %q", MonitorOptIn, MonitorAll, s.Monitor)
}
