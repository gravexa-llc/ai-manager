// Package config resolves ai-manager's home directory and loads its
// settings and token.
//
// The home directory is $AIM_HOME, or ~/.ai-manager when AIM_HOME is unset
// or empty. It holds config.toml, the token file and aimd's log. A missing
// config.toml means every default applies. A full config.toml looks like:
//
//	roots = ["~/Projects"]   # where to look for repos; ~ is expanded
//	port = 7777              # aimd listens on 127.0.0.1:<port>
//	event_retention_days = 30
//	github_token_file = "github-token"  # optional; see below
//
//	[owners]                 # GitHub owner → gh account used to read it
//	example-org = "example-user"
//
//	[poll]                   # Go durations: "10s", "5m", "1h30m"
//	sessions = "10s"         # claude agents --json, while a dashboard is open
//	sessions_unwatched = "30s"  # the same, while nobody is looking
//	sessions_mod_live = "60s"   # the same, once the mod reports live
//	github_active = "60s"    # repos with something active
//	github_idle = "5m"       # every other repo
//	worktrees = "2m"         # git worktree rescan
//
//	[state]                  # package and session state
//	stalled_after = "24h"    # commits, no live agent, no PR for this long
//	quiet_after = "15m"      # a working session with no mod event this long is quiet
//
//	[notify]                 # aimd's desktop notifications (see Notify)
//	enabled = true
//	after = "2m"             # remind about an interactive session waiting this long
//
//	[sessions]               # which sessions aimd monitors (see Sessions)
//	monitor = "opt-in"       # or "all": every session, as before
//
// GitHub is read with `gh auth token --user <account>` for an owner listed
// in [owners], and with `gh auth token` (gh's active account) for any other
// owner. github_token_file instead names a file holding a personal access
// token, used for every owner. The token itself never goes in config.toml:
// the file must be a regular file with mode 0600 or narrower, like aimd's
// token (see ReadSecretFile). A relative path is relative to the home
// directory, and ~ is expanded.
//
// Unknown keys are an error, so a typo doesn't silently fall back to a
// default.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// HomeEnv names the environment variable that overrides the home directory.
const HomeEnv = "AIM_HOME"

// File names inside the home directory.
const (
	ConfigFile = "config.toml"
	TokenFile  = "token"
	LogFile    = "aimd.log"
)

// Config is the contents of config.toml with defaults filled in.
type Config struct {
	// Home is the directory the config was loaded from. Not read from the file.
	Home string `toml:"-"`

	// Roots are directories scanned for git repos, with ~ expanded.
	Roots []string `toml:"roots"`

	// Owners maps a GitHub owner (user or org) to the gh account that reads it.
	Owners map[string]string `toml:"owners"`

	// Port is aimd's listening port on 127.0.0.1.
	Port int `toml:"port"`

	// Poll holds the polling intervals.
	Poll Poll `toml:"poll"`

	// State holds the package-state thresholds.
	State StateSettings `toml:"state"`

	// EventRetentionDays is how long event summaries are kept.
	EventRetentionDays int `toml:"event_retention_days"`

	// GitHubTokenFile is the path of a file holding a GitHub personal access
	// token, absolute after Load. Empty means use gh's tokens. Read the file
	// with ReadSecretFile.
	GitHubTokenFile string `toml:"github_token_file"`

	// Prune configures aim ls and aim prune.
	Prune Prune `toml:"prune"`

	// Notify configures aimd's desktop notifications.
	Notify Notify `toml:"notify"`

	// Sessions says which sessions aimd monitors.
	Sessions Sessions `toml:"sessions"`
}

// Prune is the [prune] table:
//
//	[prune]
//	rebuildable = ["node_modules/", ".next/", "dist/"]  # replaces the defaults
//
// `git worktree remove` deletes ignored files without asking. An ignored
// path inside a rebuildable one is fine to lose; any other ignored path is
// "at risk", and keeps its worktree from being pruned. Entries are matched
// like .gitignore patterns, relative to the worktree: a trailing "/"
// matches directories only (without one, an entry matches files and
// directories), an entry with no other "/" matches at any depth, an entry
// with one (or a leading "/") is anchored at the worktree root, "**/" in
// front makes it match at any depth again, and *, ? and [...] match within
// one path component.
type Prune struct {
	// Rebuildable is nil when config.toml doesn't set it; see
	// RebuildablePaths.
	Rebuildable []string `toml:"rebuildable"`
}

// DefaultRebuildable is the rebuildable list when config.toml sets none.
var DefaultRebuildable = []string{
	// Dependency and build output folders.
	"node_modules/", ".next/", "dist/", "build/", "target/", "cdk.out/",
	".venv/", "__pycache__/", ".turbo/", "coverage/", "lambdas/bin/",
	// Swift Package Manager's and Xcode's build output folders.
	".build/", "DerivedData/",
	// Generated files, at any depth: Next.js's type stub, TypeScript's
	// incremental build info, and Finder's folder metadata.
	"next-env.d.ts", "*.tsbuildinfo", ".DS_Store",
}

// RebuildablePaths returns the rebuildable list: the configured one, or
// DefaultRebuildable when config.toml doesn't set one (an empty list means
// nothing is rebuildable). It fails on an entry that is empty, has a "."
// or ".." component, or isn't a valid pattern.
func (p Prune) RebuildablePaths() ([]string, error) {
	if p.Rebuildable == nil {
		return slices.Clone(DefaultRebuildable), nil
	}
	for _, e := range p.Rebuildable {
		trimmed := strings.Trim(e, "/")
		if trimmed == "" {
			return nil, fmt.Errorf("config: prune.rebuildable: empty entry %q", e)
		}
		for _, c := range strings.Split(trimmed, "/") {
			if c == "" || c == "." || c == ".." {
				return nil, fmt.Errorf("config: prune.rebuildable: entry %q has an empty, . or .. component", e)
			}
			if _, err := filepath.Match(c, ""); err != nil {
				return nil, fmt.Errorf("config: prune.rebuildable: entry %q: %w", e, err)
			}
		}
	}
	return slices.Clone(p.Rebuildable), nil
}

// Poll holds the polling intervals.
type Poll struct {
	Sessions     time.Duration `toml:"sessions"`
	GitHubActive time.Duration `toml:"github_active"`
	GitHubIdle   time.Duration `toml:"github_idle"`
	Worktrees    time.Duration `toml:"worktrees"`

	// SessionsUnwatched and SessionsModLive are the session poll interval
	// while no dashboard is connected, and once the mod reports live.
	// Sessions is the interval while a dashboard is connected.
	SessionsUnwatched time.Duration `toml:"sessions_unwatched"`
	SessionsModLive   time.Duration `toml:"sessions_mod_live"`
}

// StateSettings holds the thresholds the plan board's package state uses.
type StateSettings struct {
	// StalledAfter is how long a package can have commits on its branch
	// with no live agent and no PR before it is flagged stalled.
	StalledAfter time.Duration `toml:"stalled_after"`
	// QuietAfter is how long a working session can go without a mod event
	// before it is quiet. The event receiver (internal/live) and the
	// package badges (internal/state) both read it.
	QuietAfter time.Duration `toml:"quiet_after"`
}

// Default returns the configuration used when config.toml sets nothing.
// Roots are returned unexpanded ("~/Projects"); Load expands them.
func Default() Config {
	return Config{
		Roots:  []string{"~/Projects"},
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
		Notify:             DefaultNotify(),
		Sessions:           DefaultSessions(),
	}
}

// Home returns the ai-manager home directory: $AIM_HOME if set and
// non-empty, else ~/.ai-manager. The directory may not exist yet.
func Home() (string, error) {
	if h := os.Getenv(HomeEnv); h != "" {
		return expandHome(h)
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config: find home directory: %w", err)
	}
	return filepath.Join(userHome, ".ai-manager"), nil
}

// ConfigPath returns the path of config.toml in home.
func ConfigPath(home string) string { return filepath.Join(home, ConfigFile) }

// TokenPath returns the path of the token file in home.
func TokenPath(home string) string { return filepath.Join(home, TokenFile) }

// LogPath returns the path of aimd's log in home.
func LogPath(home string) string { return filepath.Join(home, LogFile) }

// Load reads config.toml from home and fills in defaults for anything it
// doesn't set. A missing file is not an error.
func Load(home string) (Config, error) {
	cfg := Default()
	cfg.Home = home
	path := ConfigPath(home)

	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// No file: defaults only.
	case err != nil:
		return Config{}, fmt.Errorf("config: %w", err)
	default:
		md, err := toml.Decode(string(data), &cfg)
		if err != nil {
			return Config{}, fmt.Errorf("config: %s: %w", path, err)
		}
		if undecoded := md.Undecoded(); len(undecoded) > 0 {
			keys := make([]string, len(undecoded))
			for i, k := range undecoded {
				keys[i] = k.String()
			}
			slices.Sort(keys)
			return Config{}, fmt.Errorf("config: %s: unknown keys: %s", path, strings.Join(keys, ", "))
		}
	}

	if cfg.Owners == nil {
		cfg.Owners = map[string]string{}
	}
	for i, r := range cfg.Roots {
		expanded, err := expandHome(r)
		if err != nil {
			return Config{}, err
		}
		cfg.Roots[i] = expanded
	}
	// Checked before anything else touches the value, so a pasted token is
	// never turned into a path or echoed in an error.
	if looksLikeGitHubToken(cfg.GitHubTokenFile) {
		return Config{}, fmt.Errorf("config: %s: github_token_file must be the path of a file holding the token, not the token itself", path)
	}
	if cfg.GitHubTokenFile != "" {
		expanded, err := expandHome(cfg.GitHubTokenFile)
		if err != nil {
			return Config{}, err
		}
		if !filepath.IsAbs(expanded) {
			expanded = filepath.Join(home, expanded)
		}
		cfg.GitHubTokenFile = expanded
	}
	if err := cfg.validate(); err != nil {
		return Config{}, fmt.Errorf("config: %s: %w", path, err)
	}
	return cfg, nil
}

func (c Config) validate() error {
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("port %d out of range 1-65535", c.Port)
	}
	if c.EventRetentionDays < 1 {
		return fmt.Errorf("event_retention_days must be at least 1, got %d", c.EventRetentionDays)
	}
	for _, d := range []struct {
		key string
		val time.Duration
	}{
		{"poll.sessions", c.Poll.Sessions},
		{"poll.github_active", c.Poll.GitHubActive},
		{"poll.github_idle", c.Poll.GitHubIdle},
		{"poll.worktrees", c.Poll.Worktrees},
		{"poll.sessions_unwatched", c.Poll.SessionsUnwatched},
		{"poll.sessions_mod_live", c.Poll.SessionsModLive},
		{"state.stalled_after", c.State.StalledAfter},
		{"state.quiet_after", c.State.QuietAfter},
		{"notify.after", c.Notify.After},
	} {
		if d.val <= 0 {
			return fmt.Errorf("%s must be positive, got %s", d.key, d.val)
		}
	}
	if err := c.Sessions.validate(); err != nil {
		return err
	}
	for _, r := range c.Roots {
		if r == "" {
			return errors.New("roots: empty path")
		}
	}
	for owner, account := range c.Owners {
		if owner == "" || account == "" {
			return fmt.Errorf("owners: empty owner or account (%q = %q)", owner, account)
		}
	}
	return nil
}

// gitHubTokenPrefixes are the prefixes GitHub puts on its tokens: classic
// and fine-grained PATs, OAuth, user-to-server, server-to-server, refresh.
var gitHubTokenPrefixes = []string{"ghp_", "github_pat_", "gho_", "ghu_", "ghs_", "ghr_"}

// looksLikeGitHubToken reports whether s is a GitHub token rather than a
// path, so that a token pasted into github_token_file is refused.
func looksLikeGitHubToken(s string) bool {
	s = strings.TrimSpace(s)
	if strings.ContainsAny(s, `/\`) {
		return false
	}
	for _, p := range gitHubTokenPrefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// expandHome replaces a leading "~" or "~/" with the user's home directory
// and cleans the result. Other paths ("~other/x", relative paths) are only
// cleaned.
func expandHome(p string) (string, error) {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		if p == "" {
			return "", nil
		}
		return filepath.Clean(p), nil
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config: expand %q: %w", p, err)
	}
	return filepath.Join(userHome, strings.TrimPrefix(p, "~")), nil
}
