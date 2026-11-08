// Command aimd is the ai-manager daemon: it polls Claude Code sessions, git
// worktrees and GitHub (internal/daemon), keeps what it finds in SQLite
// (internal/store), and serves it on 127.0.0.1 (internal/web).
//
// It runs in the foreground; `aim up` runs it under launchd.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/gravexa-llc/ai-manager/deploy"
	"github.com/gravexa-llc/ai-manager/internal/config"
	"github.com/gravexa-llc/ai-manager/internal/daemon"
	"github.com/gravexa-llc/ai-manager/internal/github"
	"github.com/gravexa-llc/ai-manager/internal/live"
	"github.com/gravexa-llc/ai-manager/internal/planapi"
	"github.com/gravexa-llc/ai-manager/internal/pluginbuild"
	"github.com/gravexa-llc/ai-manager/internal/prune"
	"github.com/gravexa-llc/ai-manager/internal/sessions"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
	"github.com/gravexa-llc/ai-manager/internal/worktrees"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `usage: aimd --foreground [flags]

aimd is the ai-manager daemon. It polls Claude Code sessions, git worktrees
and GitHub, and serves what it finds on http://127.0.0.1:<port>. It runs in
the foreground, logging to stderr and to $AIM_HOME/aimd.log, which it rotates
to aimd.log.1 at 10 MB.

Use ` + "`aim up`" + ` to run it under launchd. That sets AIMD_LAUNCHD=1, and aimd
then logs only to aimd.log. launchd's stderr file, aimd.launchd.log, keeps
what aimd.log doesn't get: a panic, or the error aimd stops with.

Flags:
  -foreground  run the daemon (required)
  -port N      listen on 127.0.0.1:N instead of config.toml's port
  -debug       log every poll, not only changes and errors
  -version     print the version and exit
  -h, -help    print this help and exit
`

// shutdownTimeout bounds the HTTP server's graceful shutdown.
const shutdownTimeout = 5 * time.Second

// inboxEvery is how often aimd runs a Needs you inbox sync pass on its own
// (pages run one whenever they are read): items time out of In progress
// and wake from a snooze within this long of their due time.
const inboxEvery = 10 * time.Second

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("aimd", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	foreground := fs.Bool("foreground", false, "")
	port := fs.Int("port", 0, "")
	debug := fs.Bool("debug", false, "")
	showVersion := fs.Bool("version", false, "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, usage)
			return 0
		}
		fmt.Fprintf(stderr, "aimd: %v\n\n%s", err, usage)
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "aimd: unknown argument %q\n\n%s", fs.Arg(0), usage)
		return 2
	}
	if *showVersion {
		fmt.Fprintf(stdout, "aimd %s\n", version)
		return 0
	}
	if !*foreground {
		fmt.Fprint(stderr, usage)
		return 2
	}
	if *port < 0 || *port > 65535 {
		fmt.Fprintf(stderr, "aimd: -port %d out of range\n", *port)
		return 2
	}

	// The explicit signal from `aim up`'s plist, never a guess from the
	// environment.
	underLaunchd := os.Getenv(deploy.LaunchdEnv) == "1"

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serve(ctx, *port, *debug, underLaunchd, stderr); err != nil {
		fmt.Fprintf(stderr, "aimd: %v\n", err)
		return 1
	}
	return 0
}

// serve runs aimd until ctx is done. underLaunchd keeps the log off stderr
// (see logWriter).
func serve(ctx context.Context, port int, debug, underLaunchd bool, stderr io.Writer) error {
	home, err := config.Home()
	if err != nil {
		return err
	}
	cfg, err := config.Load(home)
	if err != nil {
		return err
	}
	if port > 0 {
		cfg.Port = port
	}
	if err := os.MkdirAll(home, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", home, err)
	}
	logFile, err := openLogFile(config.LogPath(home), logMaxSize, stderr)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	defer logFile.Close()
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(logWriter(logFile, stderr, underLaunchd), &slog.HandlerOptions{Level: level}))

	token, err := config.EnsureToken(home)
	if err != nil {
		return err
	}
	st, err := store.OpenHome(ctx, home)
	if err != nil {
		return err
	}
	defer st.Close()

	// Listen before starting the pollers, so a second aimd fails at once.
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s (is aimd already running?): %w", addr, err)
	}

	var d *daemon.Daemon
	var plans *planapi.Service
	hub := web.NewHub(func(n int) { d.SetWatchers(n) })
	// After a poll or an event batch that changed sessions, worktrees or
	// PRs, the plan state engine runs too (debounced in planapi to once
	// every 5s). The pollers and the event receiver share notify.
	notify := func(topic string) {
		hub.Publish(topic)
		switch topic {
		case api.TopicSessions, api.TopicWorktrees, api.TopicPRs:
			plans.Kick()
		}
	}
	recv := live.New(live.Options{
		Store:      st,
		QuietAfter: cfg.State.QuietAfter,
		SetModLive: func(on bool) { d.SetModLive(on) },
		Notify:     notify,
		Logger:     log,
	})
	scanner := worktrees.New(worktrees.OptionsFromConfig(cfg))
	// One GitHub client, so the dashboard's removals share the
	// pollers' rate-limit budget.
	gh := github.New(cfg, github.Options{})
	d = daemon.New(daemon.Options{
		Config:         cfg,
		Store:          st,
		Scanner:        scanner,
		GitHub:         gh,
		Notify:         notify,
		RefineSessions: recv.RefineSessions,
		Logger:         log,
		Version:        version,
	})
	plans = planapi.New(planapi.Options{
		Store:     st,
		Config:    cfg,
		Scanner:   scanner,
		LastScan:  d.LastScan,
		Canonical: d.CanonicalRepo,
		Backfill:  d.BackfillPRs,
		Refresh:   d.RefreshPRs,
		Notify:    hub.Publish,
		Logger:    log,
	})
	// The dashboard's safe-to-remove verdict: `aim prune`'s rules,
	// answered from the store.
	rebuildable, err := cfg.Prune.RebuildablePaths()
	if err != nil {
		return err
	}
	matcher, err := prune.NewMatcher(rebuildable)
	if err != nil {
		return err
	}
	verdicts := web.NewPruneVerdicts(ctx, web.PruneOptions{
		Checker:   &prune.Checker{Scanner: scanner, Procs: prune.PS{}, Rebuildable: matcher},
		Store:     st,
		Status:    d,
		Canonical: d.CanonicalRepo,
		OnUpdate:  func() { hub.Publish(api.TopicWorktrees) },
		Logger:    log,
	})
	// Removal from the dashboard: the same checks as `aim prune
	// --apply`, from fresh sources (GitHub asked again, `claude agents`
	// run again, and the sessions aimd stored as well), at the moment of
	// removal.
	cleanup := web.NewCleanup(web.CleanupOptions{
		Checker: &prune.Checker{Scanner: scanner, GitHub: gh, Sessions: sessions.Lister{}, Procs: prune.PS{}, Rebuildable: matcher},
		Store:   st,
		Status:  d,
		Log:     st,
		OnChange: func() {
			verdicts.Refresh()
			hub.Publish(api.TopicWorktrees)
		},
		Logger: log,
	})

	homeDir, _ := os.UserHomeDir() // only shortens paths on the dashboard

	// Expired background jobs: Claude Code's config directory, the
	// one the session poller's `claude agents` reads. Without it no job is
	// marked expired.
	var jobs web.ExpiredJobs
	if dir, err := sessions.ClaudeDir(); err == nil {
		jobs = sessions.NewJobs(dir, nil)
	} else {
		log.Warn("expired background jobs: no Claude Code config directory", "err", err)
	}

	// Which sessions are monitored (internal/monitor): one source for the
	// dashboard and the notifier.
	mon := newMonitor(st, cfg.Sessions, plans, d.RepoNames)

	// Go (internal/focus). claude's path is resolved once, here, and never
	// taken from a request.
	goSvc := newGo(st, homeDir, log)

	srv := web.New(web.Options{
		Port:         cfg.Port,
		Token:        token,
		Store:        st,
		Status:       d,
		Hub:          hub,
		Plans:        plans,
		Go:           goSvc,
		Terminals:    d,
		Monitor:      mon,
		Events:       recv,
		Waits:        recv,
		Repos:        d,
		HomeDir:      homeDir,
		SafeToRemove: verdicts,
		Cleanup:      cleanup,
		Jobs:         jobs,
		// The plugin build on disk in each folder sessions load the
		// plugin from, so a session on an older one is flagged.
		PluginBuilds: pluginbuild.New(scanner, nil),
		// The Settings page says when config.toml turns notifications
		// off.
		NotifyDisabled: !cfg.Notify.Enabled,
		Logger:         log,
	})

	// Notifications (internal/notify), only for the Needs you inbox's
	// Open items, and the inbox's own ticker, which applies its timers
	// while no page is open. Both stop before the store closes.
	notifier := newNotifier(st, cfg.Notify, d, mon, inboxFilter(srv.InboxOpenSessions, cfg.Port), log)
	bgCtx, stopBg := context.WithCancel(ctx)
	var bg sync.WaitGroup
	bg.Go(func() { _ = notifier.Run(bgCtx) })
	bg.Go(func() { _ = srv.RunInbox(bgCtx, inboxEvery) })
	defer func() { stopBg(); bg.Wait() }()

	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	httpSrv.RegisterOnShutdown(hub.Close)

	log.Info("aimd starting", "version", version, "addr", "http://"+addr, "home", home, "db", st.Path(), "launchd", underLaunchd)
	ctx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	var running sync.WaitGroup
	running.Go(func() { _ = d.Run(ctx) })
	running.Go(func() { _ = recv.Run(ctx) })
	done := make(chan struct{})
	go func() {
		defer close(done)
		running.Wait()
	}()
	plansDone := make(chan struct{})
	go func() {
		defer close(plansDone)
		_ = plans.Run(ctx)
	}()
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(ln) }()

	var result error
	select {
	case <-ctx.Done():
	case err := <-serveErr:
		result = fmt.Errorf("http server: %w", err)
	}
	log.Info("aimd stopping")
	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		log.Warn("http shutdown", "err", err)
	}
	cancelRun()
	<-done
	<-plansDone
	log.Info("aimd stopped")
	return result
}
