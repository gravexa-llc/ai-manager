package config

import "testing"

// sessions.monitor is "opt-in" unless config.toml says "all".
func TestSessionsMonitor(t *testing.T) {
	for _, tc := range []struct {
		body string
		want string
	}{
		{"", MonitorOptIn},
		{"[sessions]", MonitorOptIn},
		{"[sessions]\nmonitor = \"opt-in\"", MonitorOptIn},
		{"[sessions]\nmonitor = \"all\"", MonitorAll},
	} {
		_, aimHome := setup(t)
		writeConfig(t, aimHome, tc.body)
		cfg, err := Load(aimHome)
		if err != nil {
			t.Fatalf("%q: %v", tc.body, err)
		}
		if cfg.Sessions.Monitor != tc.want {
			t.Errorf("%q: sessions.monitor = %q, want %q", tc.body, cfg.Sessions.Monitor, tc.want)
		}
	}
}
