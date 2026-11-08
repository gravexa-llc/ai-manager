package daemon

import (
	"context"
	"errors"
	"maps"
	"slices"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/sessions"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/term"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
	"github.com/gravexa-llc/ai-manager/internal/worktrees"
)

// onSessions handles one session poll (the Poller's OnResult). It writes
// the live sessions, ends the ones that left, and asks for a worktree scan
// when a session works in a directory the last scan wasn't given.
//
// The live view is Session.Active(): the poller lists with --all, so a
// background job that finishes stays listed with its final state. Such a
// job is written once more with that state and then ended, like a session
// that is gone from the listing.
func (d *Daemon) onSessions(res sessions.Result) {
	defer d.firstSessOnce.Do(func() { close(d.firstSessions) })
	ctx := d.runCtx
	at := res.Snapshot.At
	if at.IsZero() {
		at = time.Now()
	}
	if res.Err != nil {
		d.ran(api.SourceSessions, at, res.Err, 0)
		return
	}
	snap := res.Snapshot

	d.mu.Lock()
	scan := d.scan
	interval := d.cfg.Poll.Sessions
	firstSeen := make(map[sessions.Key]time.Time, len(snap.Sessions))
	var live []liveSession
	var finished []sessions.Session // listed, but no longer active
	for _, s := range snap.Sessions {
		k := s.Key()
		if !s.Active() {
			if _, was := d.firstSeen[k]; was {
				finished = append(finished, s)
			}
			continue
		}
		fs, ok := d.firstSeen[k]
		if !ok {
			fs = at
		}
		firstSeen[k] = fs
		live = append(live, liveSession{s: s, headless: d.headless(s, fs, at, interval)})
	}
	d.firstSeen = firstSeen
	d.mu.Unlock()

	rows := make([]store.Session, 0, len(live)+len(finished))
	for i := range live {
		row := d.sessionRow(live[i].s, live[i].headless, scan, at)
		live[i].repo = row.Repo
		rows = append(rows, row)
	}
	for _, s := range finished {
		rows = append(rows, d.sessionRow(s, false, scan, at))
	}
	if d.refine != nil {
		rows = d.refine(ctx, rows, at)
	}
	terms := d.terminals(ctx, rows)

	var errs []error
	if len(rows) > 0 {
		errs = append(errs, d.st.UpsertSessions(ctx, rows...))
	}

	// End what left: the poller's Gone events, finished jobs, and live rows from an
	// earlier run of aimd that this poll doesn't list.
	liveKeys := map[string]bool{}
	for _, l := range live {
		liveKeys[string(l.s.Key())] = true
	}
	ended := map[string]bool{}
	end := func(key string) {
		if ended[key] || liveKeys[key] {
			return
		}
		ended[key] = true
		if err := d.st.EndSession(ctx, key, at); err != nil && !errors.Is(err, store.ErrNotFound) {
			errs = append(errs, err)
		}
	}
	for _, ev := range res.Events {
		if ev.Kind == sessions.Gone {
			end(string(ev.Key))
		}
	}
	for _, s := range finished {
		end(string(s.Key()))
	}
	stored, err := d.st.ListSessions(ctx, store.SessionFilter{})
	errs = append(errs, err)
	for _, row := range stored {
		if row.FirstSeenAt.After(at) {
			continue // the mod reported it after this poll's snapshot
		}
		end(row.Key)
	}
	if ctx.Err() != nil {
		return // shutting down
	}
	err = errors.Join(errs...)
	d.ran(api.SourceSessions, at, err, 0)

	fp := fingerprint(rows, func(r *store.Session) { r.LastSeenAt = time.Time{}; r.NeedsYouSince = time.Time{} })
	d.mu.Lock()
	d.live = live
	changed := fp != d.sessFP || len(ended) > 0 || !maps.Equal(terms, d.terms)
	d.sessFP = fp
	d.terms = terms
	newCwd := d.needsScanLocked()
	d.mu.Unlock()

	for k := range ended {
		d.log.Info("session ended", "key", k)
	}
	if changed {
		d.notify(api.TopicSessions)
	}
	if newCwd {
		kick(d.scanKick)
	}
}

// Terminals says what terminal app each live session runs in, by session
// key, as of the latest session poll (internal/term): iTerm2, Terminal.app,
// tmux, another app, or none, as for every background session. A key it
// doesn't have has none. The map is replaced on each poll, never changed,
// so callers may keep it but must not change it.
func (d *Daemon) Terminals() map[string]term.App {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.terms
}

// terminals reads the rows' terminal apps from one process snapshot,
// walking up from each session's pid (term.Procs.AppOf). If the snapshot
// fails, the previous poll's answers stand for the sessions they cover,
// and the failure is logged once until it clears, even if its message
// changes in between (a timeout one poll, ps killed the next).
func (d *Daemon) terminals(ctx context.Context, rows []store.Session) map[string]term.App {
	var procs term.Procs
	var prev map[string]term.App
	if slices.ContainsFunc(rows, mayHaveTerminal) {
		var err error
		procs, err = d.procs(ctx)
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		d.mu.Lock()
		was := d.procsErr
		d.procsErr, prev = msg, d.terms
		d.mu.Unlock()
		switch {
		case err != nil && was == "" && ctx.Err() == nil:
			d.log.Warn("process snapshot failed; terminal apps stay as last seen", "err", err)
		case err == nil && was != "":
			d.log.Info("process snapshot works again")
		}
	}
	out := make(map[string]term.App, len(rows))
	for _, r := range rows {
		a := term.NoTerminal
		switch {
		case !mayHaveTerminal(r):
		case procs != nil:
			a = procs.AppOf(r.PID)
		default:
			if p, ok := prev[r.Key]; ok {
				a = p
			}
		}
		out[r.Key] = a
	}
	return out
}

// mayHaveTerminal reports whether a session row's process may be in a
// terminal app: it has a pid, and isn't a background session (claude's
// job host gives each job a pty that no app shows).
func mayHaveTerminal(r store.Session) bool {
	return r.PID > 0 && r.Kind != string(sessions.Background)
}

// headless reports whether a live entry is a headless `claude -p` run: its
// cwd is under $TMPDIR, or it has been listed for less than one
// watched poll interval, counted from the earlier of its startedAt and
// when aimd first saw it. So an entry seen in only one poll is hidden
// until the next one, while sessions that were already running when aimd
// started show at once. Background jobs are never headless.
func (d *Daemon) headless(s sessions.Session, firstSeen, now time.Time, interval time.Duration) bool {
	for _, t := range d.tempDirs {
		if under(s.Cwd, t) {
			return true
		}
	}
	if s.Kind == sessions.Background {
		return false
	}
	since := firstSeen
	if !s.StartedAt.IsZero() && s.StartedAt.Before(since) {
		since = s.StartedAt
	}
	return now.Sub(since) < interval
}

// sessionRow converts a listed session to its store row. The worktree,
// repo and branch come from the last worktree scan.
func (d *Daemon) sessionRow(s sessions.Session, headless bool, scan *worktrees.Result, at time.Time) store.Session {
	v := s.Verdict()
	row := store.Session{
		Key:         string(s.Key()),
		SessionID:   s.SessionID,
		JobID:       s.JobID,
		Kind:        string(s.Kind),
		Headless:    headless,
		Name:        s.Name,
		Host:        d.host,
		Cwd:         s.Cwd,
		PID:         s.PID,
		State:       string(v.State),
		StateReason: v.Reason,
		StartedAt:   s.StartedAt,
		LastSeenAt:  at,
	}
	if v.NeedsYou() {
		row.NeedsYouSince = at
	}
	if scan != nil && s.Cwd != "" {
		if repo, wt := scan.Lookup(s.Cwd); wt != nil {
			row.WorktreePath = wt.Path
			row.Repo = repo.Path
			row.Branch = wt.Branch
		}
	}
	return row
}

// needsScanLocked reports whether a live session that isn't headless works
// in a directory the last full scan wasn't given. d.mu is held.
func (d *Daemon) needsScanLocked() bool {
	for _, l := range d.live {
		if !l.headless && l.s.Cwd != "" && !d.scanExtras[worktrees.PathKey(l.s.Cwd)] {
			return true
		}
	}
	return false
}

// liveCwds returns the cwds of the live sessions that aren't headless.
func (d *Daemon) liveCwds() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, l := range d.live {
		if !l.headless && l.s.Cwd != "" && !slices.Contains(out, l.s.Cwd) {
			out = append(out, l.s.Cwd)
		}
	}
	return out
}

// under reports whether p is dir or inside it, comparing paths the way the
// worktree scanner does (case-insensitively on macOS).
func under(p, dir string) bool {
	if p == "" || dir == "" {
		return false
	}
	p, dir = worktrees.PathKey(p), worktrees.PathKey(dir)
	if p == dir {
		return true
	}
	if dir != "/" {
		dir += "/"
	}
	return len(p) > len(dir) && p[:len(dir)] == dir
}
