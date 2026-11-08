package planapi

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"

	"github.com/fsnotify/fsnotify"
	"github.com/gravexa-llc/ai-manager/internal/plans"
	"github.com/gravexa-llc/ai-manager/internal/state"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/worktrees"
)

// fileWatcher watches registered plan files through their folders, since
// editors often save by writing a new file and renaming it over the old
// one, which ends a watch on the file itself.
type fileWatcher struct {
	w      *fsnotify.Watcher
	log    *slog.Logger
	events chan string // a watched file (as added) that changed

	mu    sync.Mutex
	files map[string]string // PathKey(file) → file
	dirs  map[string]bool   // folders watched
}

func newFileWatcher(ctx context.Context, log *slog.Logger) (*fileWatcher, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	f := &fileWatcher{w: w, log: log, events: make(chan string, 16), files: map[string]string{}, dirs: map[string]bool{}}
	go f.run(ctx)
	return f, nil
}

// add watches a file. Adding it twice is harmless.
func (f *fileWatcher) add(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[worktrees.PathKey(path)] = path
	dir := filepath.Dir(path)
	if f.dirs[dir] {
		return
	}
	if err := f.w.Add(dir); err != nil {
		f.log.Warn("can't watch a plan's folder; re-register the plan to pick up its changes", "dir", dir, "err", err)
		return
	}
	f.dirs[dir] = true
}

// remove stops reporting a file. Its folder stays watched, which is
// harmless: events for files no plan names are dropped.
func (f *fileWatcher) remove(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.files, worktrees.PathKey(path))
}

func (f *fileWatcher) run(ctx context.Context) {
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
			f.mu.Lock()
			file, watched := f.files[worktrees.PathKey(ev.Name)]
			if dir := filepath.Clean(ev.Name); f.dirs[dir] && (ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename)) {
				delete(f.dirs, dir) // the folder went; add re-adds it
			}
			f.mu.Unlock()
			if watched {
				select {
				case f.events <- file:
				default: // behind; the re-read is debounced anyway
				}
			}
		case err, ok := <-f.w.Errors:
			if !ok {
				return
			}
			f.log.Debug("plan watcher", "err", err)
		}
	}
}

// watch starts watching a registered plan file, once Run has a watcher.
func (s *Service) watch(path string) {
	s.mu.Lock()
	w := s.watcher
	s.mu.Unlock()
	if w != nil && path != "" {
		w.add(path)
	}
}

// unwatch stops watching an unregistered plan's file.
func (s *Service) unwatch(path string) {
	s.mu.Lock()
	w := s.watcher
	s.mu.Unlock()
	if w != nil && path != "" {
		w.remove(path)
	}
}

// resyncPath re-reads the plan registered from the file with the given
// PathKey, and reports whether it changed.
func (s *Service) resyncPath(ctx context.Context, key string) bool {
	ps, err := s.st.ListPlans(ctx, store.PlanFilter{})
	if err != nil {
		s.log.Error("list plans", "err", err)
		return false
	}
	changed := false
	for _, p := range ps {
		if worktrees.PathKey(p.SourcePath) != key {
			continue
		}
		c, err := s.resyncPlan(ctx, p)
		if err != nil {
			s.log.Warn("plan file unreadable; keeping the last copy", "plan", p.Slug, "path", p.SourcePath, "err", err)
			continue
		}
		if c {
			s.log.Info("plan file changed; re-parsed", "plan", p.Slug, "path", p.SourcePath)
		}
		changed = changed || c
	}
	return changed
}

// reparseStored re-parses the copy of a plan stored at its last read and
// syncs its packages with it, as a changed file would be (links, Mark done
// and the lifecycles are kept). The copy is the same, but the parser and
// the store can read more of it than when it was stored: the What column,
// say, which an aimd before migration 0015 parsed but didn't keep. A copy
// that parses to no packages is left alone.
func (s *Service) reparseStored(ctx context.Context, p store.Plan) error {
	parsed := plans.Parse([]byte(p.SourceMD))
	if len(parsed.Packages) == 0 {
		return nil
	}
	return s.st.SyncPackages(ctx, p.ID, state.StorePackages(parsed))
}

// resyncPlan re-reads a registered plan's file and, if it changed, stores
// the new copy, title and packages (links and Mark done are kept for the
// packages still there). A file that now has no packages at all is left
// alone: more likely half-written than emptied on purpose. The file must
// still pass the path checks. It reports whether anything was written.
func (s *Service) resyncPlan(ctx context.Context, p store.Plan) (bool, error) {
	f, err := s.readPlanFile(p.SourcePath)
	if err != nil {
		return false, err
	}
	if string(f.src) == p.SourceMD {
		return false, nil
	}
	parsed := plans.Parse(f.src)
	if len(parsed.Packages) == 0 {
		return false, errors.New("the file has no packages now; keeping the last copy")
	}
	p.SourceMD = string(f.src)
	if parsed.Title != "" {
		p.Title = parsed.Title
	}
	if err := s.st.UpdatePlan(ctx, p); err != nil {
		return false, err
	}
	if err := s.st.SyncPackages(ctx, p.ID, state.StorePackages(parsed)); err != nil {
		return false, err
	}
	return true, nil
}
