package config

import (
	"testing"
	"time"
)

// A [notify] table that sets one key keeps the other's default.
func TestNotifyPartial(t *testing.T) {
	for _, tc := range []struct {
		body string
		want Notify
	}{
		{"[notify]\nafter = \"30s\"", Notify{Enabled: true, After: 30 * time.Second}},
		{"[notify]\nenabled = false", Notify{Enabled: false, After: 2 * time.Minute}},
		{"port = 7777", DefaultNotify()},
	} {
		_, aimHome := setup(t)
		writeConfig(t, aimHome, tc.body)
		cfg, err := Load(aimHome)
		if err != nil {
			t.Fatalf("%q: %v", tc.body, err)
		}
		if cfg.Notify != tc.want {
			t.Errorf("%q: Notify = %+v, want %+v", tc.body, cfg.Notify, tc.want)
		}
	}
}
