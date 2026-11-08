package sessions

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// ConfigDirEnv is the environment variable Claude Code reads for its
// config directory, which is ~/.claude when it is unset.
const ConfigDirEnv = "CLAUDE_CONFIG_DIR"

// watchRetry is how often Watch retries a directory it couldn't watch.
const watchRetry = time.Minute

// ClaudeDir returns Claude Code's config directory: $CLAUDE_CONFIG_DIR if
// set and non-empty (made absolute), else ~/.claude. The `claude` that
// Lister runs inherits the same environment, so both look at the same
// place.
func ClaudeDir() (string, error) {
	if d := os.Getenv(ConfigDirEnv); d != "" {
		abs, err := filepath.Abs(d)
		if err != nil {
			return "", fmt.Errorf("sessions: %s: %w", ConfigDirEnv, err)
		}
		return abs, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("sessions: find home directory: %w", err)
	}
	return filepath.Join(home, ".claude"), nil
}

// WatchDirs returns the directories whose changes should trigger
// Poller.Kick: Claude Code's live-process registry (<ClaudeDir>/sessions,
// one <pid>.json per running session, rewritten on status changes) and
// its background job store (<ClaudeDir>/jobs, one <id>/ per job). Both
// change only on transitions. Transcripts (<ClaudeDir>/projects) change
// constantly and are deliberately left out.
func WatchDirs() ([]string, error) {
	dir, err := ClaudeDir()
	if err != nil {
		return nil, err
	}
	return []string{filepath.Join(dir, "sessions"), filepath.Join(dir, "jobs")}, nil
}

// Watch calls kick whenever an entry in one of dirs is created, written,
// removed or renamed, until ctx is done; then it returns nil. It only
// watches each directory itself, not its subdirectories: a job's own
// state.json isn't watched, because a job whose state changes also has a
// process that starts, stops or rewrites its registry file. The poll
// interval covers anything this misses.
//
// A directory that doesn't exist (no background job has ever run, say) or
// can't be watched is retried every minute, and kicks once it is added. A
// watched directory that is removed is retried the same way. When the
// watcher reports an error, such as dropped events, Watch kicks too. Watch
// returns an error only if it can't start a watcher at all.
func Watch(ctx context.Context, dirs []string, kick func()) error {
	return watch(ctx, dirs, kick, watchRetry)
}

func watch(ctx context.Context, dirs []string, kick func(), retry time.Duration) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("sessions: watch: %w", err)
	}
	defer w.Close()

	wanted := make(map[string]bool, len(dirs))
	missing := make(map[string]bool, len(dirs))
	for _, d := range dirs {
		d = filepath.Clean(d)
		wanted[d] = true
		if w.Add(d) != nil {
			missing[d] = true
		}
	}

	ticker := time.NewTicker(retry)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.Events:
			if !ok {
				return errors.New("sessions: watch: watcher closed")
			}
			if ev.Op == fsnotify.Chmod {
				continue // attribute changes only
			}
			if name := filepath.Clean(ev.Name); wanted[name] && (ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename)) {
				_ = w.Remove(name) // may already be gone
				missing[name] = true
			}
			kick()
		case _, ok := <-w.Errors:
			if !ok {
				return errors.New("sessions: watch: watcher closed")
			}
			kick() // events may have been lost
		case <-ticker.C:
			added := false
			for d := range missing {
				if w.Add(d) == nil {
					delete(missing, d)
					added = true
				}
			}
			if added {
				kick()
			}
		}
	}
}
