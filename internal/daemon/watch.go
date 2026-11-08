package daemon

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"

	"github.com/fsnotify/fsnotify"
)

// FSWatcher is the RepoWatcher aimd uses: fsnotify (kqueue on macOS) on
// each repo's WatchPaths. It watches each path itself, not recursively.
type FSWatcher struct {
	w      *fsnotify.Watcher
	log    *slog.Logger
	events chan string

	mu       sync.Mutex
	watched  map[string]bool // paths as given to Set
	failures map[string]bool // paths whose Add failed, logged once
}

// NewFSWatcher starts a watcher that runs until ctx is done.
func NewFSWatcher(ctx context.Context, log *slog.Logger) (*FSWatcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	f := &FSWatcher{
		w:        w,
		log:      log,
		events:   make(chan string, 64),
		watched:  map[string]bool{},
		failures: map[string]bool{},
	}
	go f.run(ctx)
	return f, nil
}

// Set replaces the watched paths. A path that can't be watched (missing,
// or out of file descriptors) is logged once and skipped; the periodic
// full scan still covers its repo.
func (f *FSWatcher) Set(paths []string) {
	want := make(map[string]bool, len(paths))
	for _, p := range paths {
		want[filepath.Clean(p)] = true
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for p := range f.watched {
		if !want[p] {
			_ = f.w.Remove(p) // may already be gone
			delete(f.watched, p)
		}
	}
	for p := range want {
		if f.watched[p] {
			continue
		}
		if err := f.w.Add(p); err != nil {
			if !f.failures[p] {
				f.failures[p] = true
				f.log.Warn("can't watch repo path; it is rescanned on the interval only", "path", p, "err", err)
			}
			continue
		}
		delete(f.failures, p)
		f.watched[p] = true
	}
}

// Events delivers the watched path whose contents changed.
func (f *FSWatcher) Events() <-chan string { return f.events }

func (f *FSWatcher) run(ctx context.Context) {
	defer f.w.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-f.w.Events:
			if !ok {
				return
			}
			if ev.Op == fsnotify.Chmod {
				continue
			}
			if p := f.owner(ev.Name); p != "" {
				if p == filepath.Clean(ev.Name) && (ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename)) {
					// fsnotify dropped the watch; the next Set adds it back.
					f.mu.Lock()
					delete(f.watched, p)
					f.mu.Unlock()
				}
				select {
				case f.events <- p:
				default: // the consumer is behind; it debounces anyway
				}
			}
		case err, ok := <-f.w.Errors:
			if !ok {
				return
			}
			f.log.Debug("repo watcher", "err", err)
		}
	}
}

// owner returns the watched path an event's name belongs to: the path
// itself, or the watched directory holding it.
func (f *FSWatcher) owner(name string) string {
	name = filepath.Clean(name)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.watched[name] {
		return name
	}
	if dir := filepath.Dir(name); f.watched[dir] {
		return dir
	}
	return ""
}
