package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// setup points HOME and AIM_HOME at fresh temp dirs and returns them.
func setup(t *testing.T) (userHome, aimHome string) {
	t.Helper()
	userHome = t.TempDir()
	aimHome = filepath.Join(t.TempDir(), "aim")
	t.Setenv("HOME", userHome)
	t.Setenv(HomeEnv, aimHome)
	return userHome, aimHome
}

func writeConfig(t *testing.T, home, body string) {
	t.Helper()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ConfigPath(home), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestHome(t *testing.T) {
	userHome, aimHome := setup(t)

	got, err := Home()
	if err != nil {
		t.Fatal(err)
	}
	if got != aimHome {
		t.Errorf("Home() with AIM_HOME = %q, want %q", got, aimHome)
	}

	t.Setenv(HomeEnv, "")
	got, err = Home()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(userHome, ".ai-manager"); got != want {
		t.Errorf("Home() without AIM_HOME = %q, want %q", got, want)
	}

	t.Setenv(HomeEnv, "~/aim-test")
	got, err = Home()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(userHome, "aim-test"); got != want {
		t.Errorf("Home() with AIM_HOME=~/aim-test = %q, want %q", got, want)
	}
}

func TestPaths(t *testing.T) {
	home := t.TempDir()
	for name, got := range map[string]string{
		ConfigFile: ConfigPath(home),
		TokenFile:  TokenPath(home),
		LogFile:    LogPath(home),
	} {
		if want := filepath.Join(home, name); got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
	}
}

func TestLoadDefaults(t *testing.T) {
	userHome, aimHome := setup(t)

	cfg, err := Load(aimHome)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Home:   aimHome,
		Roots:  []string{filepath.Join(userHome, "Projects")},
		Owners: map[string]string{},
		Port:   7777,
		Poll: Poll{
			Sessions:     10 * time.Second,
			GitHubActive: 60 * time.Second,
			GitHubIdle:   5 * time.Minute,
			Worktrees:    2 * time.Minute,

			SessionsUnwatched: 30 * time.Second,
			SessionsModLive:   60 * time.Second,
		},
		State:              StateSettings{StalledAfter: 24 * time.Hour, QuietAfter: 15 * time.Minute},
		EventRetentionDays: 30,
		Notify:             Notify{Enabled: true, After: 2 * time.Minute},
		Sessions:           Sessions{Monitor: "opt-in"},
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("Load() with no file =\n%+v\nwant\n%+v", cfg, want)
	}
}

func TestLoadFull(t *testing.T) {
	userHome, aimHome := setup(t)
	writeConfig(t, aimHome, `
roots = ["~/code", "/srv/repos", "~"]
port = 8080
event_retention_days = 7
github_token_file = "~/secrets/gh-pat"

[owners]
example-org = "example-user"
other-org = "example-user"

[poll]
sessions = "1m"
github_active = "30s"
github_idle = "10m"
worktrees = "1h30m"
sessions_unwatched = "45s"
sessions_mod_live = "2m"

[state]
stalled_after = "36h"
quiet_after = "20m"

[notify]
enabled = false
after = "5m"

[sessions]
monitor = "all"
`)

	cfg, err := Load(aimHome)
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Home:   aimHome,
		Roots:  []string{filepath.Join(userHome, "code"), "/srv/repos", userHome},
		Owners: map[string]string{"example-org": "example-user", "other-org": "example-user"},
		Port:   8080,
		Poll: Poll{
			Sessions:     time.Minute,
			GitHubActive: 30 * time.Second,
			GitHubIdle:   10 * time.Minute,
			Worktrees:    90 * time.Minute,

			SessionsUnwatched: 45 * time.Second,
			SessionsModLive:   2 * time.Minute,
		},
		State:              StateSettings{StalledAfter: 36 * time.Hour, QuietAfter: 20 * time.Minute},
		EventRetentionDays: 7,
		GitHubTokenFile:    filepath.Join(userHome, "secrets", "gh-pat"),
		Notify:             Notify{Enabled: false, After: 5 * time.Minute},
		Sessions:           Sessions{Monitor: "all"},
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("Load() =\n%+v\nwant\n%+v", cfg, want)
	}
}

func TestLoadPartialKeepsDefaults(t *testing.T) {
	userHome, aimHome := setup(t)
	writeConfig(t, aimHome, `
port = 9000

[poll]
sessions = "1m"
`)

	cfg, err := Load(aimHome)
	if err != nil {
		t.Fatal(err)
	}
	def := Default()
	if cfg.Port != 9000 {
		t.Errorf("Port = %d, want 9000", cfg.Port)
	}
	if cfg.Poll.Sessions != time.Minute {
		t.Errorf("Poll.Sessions = %s, want 1m", cfg.Poll.Sessions)
	}
	if cfg.Poll.GitHubActive != def.Poll.GitHubActive || cfg.Poll.GitHubIdle != def.Poll.GitHubIdle || cfg.Poll.Worktrees != def.Poll.Worktrees ||
		cfg.Poll.SessionsUnwatched != def.Poll.SessionsUnwatched || cfg.Poll.SessionsModLive != def.Poll.SessionsModLive {
		t.Errorf("unset poll intervals changed: %+v", cfg.Poll)
	}
	if want := []string{filepath.Join(userHome, "Projects")}; !reflect.DeepEqual(cfg.Roots, want) {
		t.Errorf("Roots = %q, want %q", cfg.Roots, want)
	}
	if cfg.EventRetentionDays != 30 {
		t.Errorf("EventRetentionDays = %d, want 30", cfg.EventRetentionDays)
	}
	if cfg.Owners == nil || len(cfg.Owners) != 0 {
		t.Errorf("Owners = %v, want empty map", cfg.Owners)
	}
}

func TestLoadGitHubTokenFile(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		want        func(userHome, aimHome string) string
	}{
		{"unset", "", func(_, _ string) string { return "" }},
		{"relative to home", "github-token", func(_, a string) string { return filepath.Join(a, "github-token") }},
		{"relative subdir", "secrets/../pat", func(_, a string) string { return filepath.Join(a, "pat") }},
		{"tilde", "~/.config/aim/pat", func(u, _ string) string { return filepath.Join(u, ".config", "aim", "pat") }},
		{"absolute", "/etc/aim/pat", func(_, _ string) string { return "/etc/aim/pat" }},
		// A path that merely contains a token-like name is still a path.
		{"token-like name in a dir", "~/ghp_backup", func(u, _ string) string { return filepath.Join(u, "ghp_backup") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			userHome, aimHome := setup(t)
			writeConfig(t, aimHome, "github_token_file = \""+tc.value+"\"\n")

			cfg, err := Load(aimHome)
			if err != nil {
				t.Fatal(err)
			}
			if want := tc.want(userHome, aimHome); cfg.GitHubTokenFile != want {
				t.Errorf("GitHubTokenFile = %q, want %q", cfg.GitHubTokenFile, want)
			}
		})
	}
}

func TestLoadRefusesInlineGitHubToken(t *testing.T) {
	for _, tok := range []string{
		"ghp_0123456789abcdefABCDEF0123456789abcd",
		"github_pat_11AAAAAAA0123456789_abcdefghijklmnop",
		"gho_0123456789abcdefABCDEF0123456789abcd",
		"  ghs_0123456789abcdef  ",
	} {
		t.Run(strings.TrimSpace(tok)[:4], func(t *testing.T) {
			_, aimHome := setup(t)
			writeConfig(t, aimHome, "github_token_file = \""+tok+"\"\n")

			_, err := Load(aimHome)
			if err == nil {
				t.Fatal("Load() accepted a token as github_token_file")
			}
			if !strings.Contains(err.Error(), "not the token itself") {
				t.Errorf("error = %q, want it to say the value must be a path", err)
			}
			if strings.Contains(err.Error(), strings.TrimSpace(tok)) {
				t.Errorf("error %q echoes the token", err)
			}
		})
	}

	// A token under any other key is an unknown key, and the error names
	// only the key.
	_, aimHome := setup(t)
	writeConfig(t, aimHome, "github_token = \"ghp_secretsecret\"\n")
	_, err := Load(aimHome)
	if err == nil || !strings.Contains(err.Error(), "unknown keys: github_token") {
		t.Fatalf("Load() error = %v, want unknown key github_token", err)
	}
	if strings.Contains(err.Error(), "ghp_secretsecret") {
		t.Errorf("error %q echoes the token", err)
	}
}

func TestLoadEmptyRoots(t *testing.T) {
	_, aimHome := setup(t)
	writeConfig(t, aimHome, `roots = []`)

	cfg, err := Load(aimHome)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Roots) != 0 {
		t.Errorf("Roots = %q, want none", cfg.Roots)
	}
}

func TestLoadDefaultsNotShared(t *testing.T) {
	_, aimHome := setup(t)
	writeConfig(t, aimHome, "[owners]\nexample-org = \"example-user\"\n")

	if _, err := Load(aimHome); err != nil {
		t.Fatal(err)
	}
	if def := Default(); len(def.Owners) != 0 || def.Roots[0] != "~/Projects" {
		t.Errorf("Load changed the defaults: %+v", def)
	}
}

func TestLoadErrors(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"unknown key", `prot = 7777`, "unknown keys: prot"},
		{"unknown nested key", "[poll]\nsession = \"10s\"", "unknown keys: poll.session"},
		{"bad duration", "[poll]\nsessions = \"soon\"", "poll"},
		{"zero duration", "[poll]\nworktrees = \"0s\"", "poll.worktrees must be positive"},
		{"negative unwatched", "[poll]\nsessions_unwatched = \"-1s\"", "poll.sessions_unwatched must be positive"},
		{"zero mod live", "[poll]\nsessions_mod_live = \"0s\"", "poll.sessions_mod_live must be positive"},
		{"zero stalled_after", "[state]\nstalled_after = \"0s\"", "state.stalled_after must be positive"},
		{"zero quiet_after", "[state]\nquiet_after = \"0s\"", "state.quiet_after must be positive"},
		{"zero notify.after", "[notify]\nafter = \"0s\"", "notify.after must be positive"},
		{"unknown notify key", "[notify]\nenable = false", "unknown keys: notify.enable"},
		{"unknown state key", "[state]\nstalled = \"1h\"", "unknown keys: state.stalled"},
		{"unknown sessions key", "[sessions]\nmonitored = \"all\"", "unknown keys: sessions.monitored"},
		{"bad sessions.monitor", "[sessions]\nmonitor = \"some\"", `sessions.monitor must be "opt-in" or "all", got "some"`},
		{"empty sessions.monitor", "[sessions]\nmonitor = \"\"", `sessions.monitor must be "opt-in" or "all", got ""`},
		{"port too big", `port = 70000`, "port 70000 out of range"},
		{"port zero", `port = 0`, "port 0 out of range"},
		{"retention zero", `event_retention_days = 0`, "event_retention_days must be at least 1"},
		{"empty root", `roots = [""]`, "roots: empty path"},
		{"empty account", "[owners]\nexample-org = \"\"", "owners: empty owner or account"},
		{"wrong type", `port = "7777"`, "port"},
		{"bad toml", `roots = [`, "config.toml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, aimHome := setup(t)
			writeConfig(t, aimHome, tc.body)

			_, err := Load(aimHome)
			if err == nil {
				t.Fatalf("Load() succeeded, want error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Load() error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestLoadUnreadable(t *testing.T) {
	_, aimHome := setup(t)
	// A directory where the file should be: exists, but can't be read as one.
	if err := os.MkdirAll(ConfigPath(aimHome), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(aimHome); err == nil {
		t.Fatal("Load() succeeded with config.toml as a directory")
	}
}
