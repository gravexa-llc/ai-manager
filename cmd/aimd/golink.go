package main

import (
	"context"
	"log/slog"
	"net"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/config"
	"github.com/gravexa-llc/ai-manager/internal/daemon"
	"github.com/gravexa-llc/ai-manager/internal/focus"
	"github.com/gravexa-llc/ai-manager/internal/monitor"
	"github.com/gravexa-llc/ai-manager/internal/notify"
	"github.com/gravexa-llc/ai-manager/internal/osascript"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/term"
	"github.com/gravexa-llc/ai-manager/internal/web"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// goTimeout bounds one Go script. It is long enough for macOS's first
// "aimd wants to control iTerm2" (or Terminal) prompt to be answered.
const goTimeout = 30 * time.Second

// newGo returns the Go service (POST /api/go, the Go buttons) over the
// store. claude's absolute path is resolved now, from aimd's PATH (which
// `aim up` sets to where the user's shell finds claude); a request never
// supplies it.
func newGo(st *store.Store, homeDir string, log *slog.Logger) *focus.Service {
	claude, err := exec.LookPath("claude")
	if err == nil {
		claude, err = filepath.Abs(claude)
	}
	if err != nil {
		claude = ""
	}
	if err := term.CheckClaudePath(claude); err != nil {
		log.Warn("go: background sessions can't be opened", "err", err)
	}
	return &focus.Service{
		Source:   focus.StoreSource{Store: st},
		Terminal: term.Terminal{Runner: osascript.Exec{Timeout: goTimeout}, TTYs: term.PS{}, Claude: claude},
		HomeDir:  homeDir,
	}
}

// newNotifier returns aimd's notifier (internal/notify) for the [notify]
// settings. It waits for the daemon's first session poll, notifies only
// about the sessions mon says are monitored whose wait is an Open item of
// the Needs you inbox (internal/inbox), and only while the Settings
// page's switch is on.
func newNotifier(st *store.Store, cfg config.Notify, d *daemon.Daemon, mon *monitor.Source,
	inbox func(context.Context, []store.Session) (func(store.Session) (string, bool), error), log *slog.Logger) *notify.Watcher {
	return notify.New(notify.Options{
		Enabled:   cfg.Enabled,
		Store:     st,
		Sender:    notify.Osascript{},
		After:     cfg.After,
		Ready:     func() bool { return sessionsPolled(d.Status()) },
		Monitored: monitoredFilter(mon),
		Inbox:     inbox,
		On:        notificationsOn(st, log),
		Logger:    log,
	})
}

// notificationsOn is the notifier's On: the Settings page's macOS
// notifications switch. When the settings can't be read it says on, the
// default, so a broken read never silences what needs you.
func notificationsOn(st interface {
	Settings(context.Context) (store.Settings, error)
}, log *slog.Logger) func(context.Context) bool {
	return func(ctx context.Context) bool {
		set, err := st.Settings(ctx)
		if err != nil {
			log.Warn("notify: read settings", "err", err)
			return true
		}
		return set.Notifications
	}
}

// inboxFilter is the notifier's Inbox: open runs an inbox sync pass and
// gives the Open items of sessions by session row id (the web server's
// InboxOpenSessions); each notification's URL is its item's page on aimd.
func inboxFilter(open func(context.Context) (map[int64]int64, error), port int) func(context.Context, []store.Session) (func(store.Session) (string, bool), error) {
	base := "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	return func(ctx context.Context, _ []store.Session) (func(store.Session) (string, bool), error) {
		items, err := open(ctx)
		if err != nil {
			return nil, err
		}
		return func(r store.Session) (string, bool) {
			id, ok := items[r.ID]
			if !ok {
				return "", false
			}
			return base + web.InboxItemURL(id), true
		}, nil
	}
}

// PlanBoards is the part of the plan service the monitor reads.
type PlanBoards interface {
	Plans(ctx context.Context) (api.PlanList, error)
}

// newMonitor returns the source of which sessions are monitored
// (internal/monitor) for the [sessions] settings: the store's plan leads
// and watches, the plan service's boards with the Settings page's
// overrides of who merges, and repo names (the daemon's RepoNames).
func newMonitor(st *store.Store, cfg config.Sessions, plans PlanBoards, repoNames func() map[string]string) *monitor.Source {
	return &monitor.Source{
		Mode:  cfg.Monitor,
		Store: st,
		Plans: func(ctx context.Context) ([]api.Plan, error) {
			list, err := plans.Plans(ctx)
			return list.Plans, err
		},
		RepoNames: repoNames,
		Merges:    st.PlanMerges,
	}
}

// monitoredFilter is the notifier's Monitored for mon.
func monitoredFilter(mon *monitor.Source) func(context.Context, []store.Session) (func(store.Session) bool, error) {
	return func(ctx context.Context, rows []store.Session) (func(store.Session) bool, error) {
		set, err := mon.Load(ctx, rows, nil)
		if err != nil {
			return nil, err
		}
		return set.Monitored, nil
	}
}

// sessionsPolled reports whether the session poller has succeeded since
// aimd started: until then the notifier waits, so it starts from the real
// state rather than an empty or stale store.
func sessionsPolled(st api.Status) bool {
	for _, src := range st.Sources {
		if src.Name == api.SourceSessions && !src.LastOK.IsZero() {
			return true
		}
	}
	return false
}
