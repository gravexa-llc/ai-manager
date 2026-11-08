// Package notify posts a macOS notification from aimd when a session needs
// you and Claude Code wouldn't tell you itself.
//
// Claude Code already notifies when an interactive session needs you, so
// aimd doesn't repeat that. It notifies only:
//
//   - when a background job becomes blocked (its state turns needs_you:
//     it ended its turn asking for a reply, or a dialog is open in it),
//     at once;
//   - when an interactive session has needed you for longer than
//     notify.after (2m by default) and you haven't dismissed it on the
//     dashboard: a reminder.
//
// Either way, only about a monitored session (internal/monitor;
// Options.Monitored): with sessions.monitor = "opt-in", a plan lead, a
// session you watch, or a plan's agent. Others are skipped as if they
// didn't need you.
//
// And only about a wait that is an Open item of the Needs you inbox
// (internal/inbox; Options.Inbox): never one you moved to In
// progress, snoozed or marked Done. A wait skipped that way counts as not
// needing you, so when its item comes back to Open (a snooze ends, Go's
// delay runs out) it is notified about then. Each notification carries
// its item's page (Notification.URL, /app/inbox?item=<id>). The
// `display notification` sender can't act on it: macOS opens the app that
// posted the notification (Script Editor) when it is clicked, and
// AppleScript gives no way to attach a link. A sender that can (a helper
// app with UNUserNotificationCenter, or terminal-notifier's -open) is a
// follow-up.
//
// At most one notification per session per needs-you spell. A spell is a
// stretch of needs_you for one session key; it ends when the session stops
// needing you (or goes), and a later NeedsYouSince starts a new one. The
// spells aimd finds when it starts (its first check after the first session
// poll) count as already notified, so a restart doesn't repeat old news,
// such as jobs blocked for months.
//
// The Settings page's macOS notifications switch (Options.On) is asked
// before every check. While it is off nothing is
// sent, and turning it back on starts afresh, as aimd's start does: the
// waits that began meanwhile count as already notified, so the switch
// never brings a burst of old news.
//
// Notifications go through `display notification` in osascript, with the
// text passed as arguments (internal/osascript), never as script text.
package notify

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/gravexa-llc/ai-manager/internal/osascript"
	"github.com/gravexa-llc/ai-manager/internal/sessions"
	"github.com/gravexa-llc/ai-manager/internal/store"
)

// Notification is one notification.
type Notification struct {
	// Key is the session's store key (not shown).
	Key      string
	Title    string
	Subtitle string
	Body     string
	// URL is where a click on it should land: its inbox item's page
	// (Options.Inbox), or "". The osascript sender can't attach it.
	URL string
}

// Sender posts notifications.
type Sender interface {
	Send(ctx context.Context, n Notification) error
}

// script posts argv 1 as the body, argv 2 as the title and argv 3 as the
// subtitle.
const script = `on run argv
	display notification (item 1 of argv) with title (item 2 of argv) subtitle (item 3 of argv)
end run`

// Script returns the notification script, for review.
func Script() string { return script }

// Osascript is the Sender that runs `display notification`.
type Osascript struct {
	// Runner runs the script; nil means osascript.Exec{}.
	Runner osascript.Runner
}

// Send implements Sender.
func (o Osascript) Send(ctx context.Context, n Notification) error {
	r := o.Runner
	if r == nil {
		r = osascript.Exec{}
	}
	_, err := r.Run(ctx, script, Clean(n.Body), Clean(n.Title), Clean(n.Subtitle))
	return err
}

// maxText bounds each field of a notification, in runes.
const maxText = 200

// Clean makes s fit a notification: control characters (new lines
// included) become spaces, runs of spaces collapse, and it is cut to
// maxText runes.
func Clean(s string) string {
	var b strings.Builder
	space := false
	n := 0
	for _, r := range s {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			space = b.Len() > 0
			continue
		}
		if space {
			b.WriteByte(' ')
			n++
			space = false
		}
		if n >= maxText {
			b.WriteString("…")
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// Sessions is the part of *store.Store the watcher reads.
type Sessions interface {
	ListSessions(ctx context.Context, f store.SessionFilter) ([]store.Session, error)
}

// Options configure a Watcher. Store and Sender are required.
type Options struct {
	// Enabled is config notify.enabled: when false, Run returns at once
	// and Check sends nothing.
	Enabled bool
	Store   Sessions
	Sender  Sender
	// After is how long an interactive session must need you before the
	// reminder (config notify.after).
	After time.Duration
	// Ready reports whether the session list is fresh (the first session
	// poll has run). Until it does, nothing is checked, so the first check
	// sees the real state to start from. Nil means always ready.
	Ready func() bool
	// Monitored says which of the live sessions are monitored
	// (internal/monitor): only those are notified about. A session that
	// isn't is skipped as if it didn't need you, so one that becomes
	// monitored while it waits is notified about then. An error skips the
	// check (Check returns it). Nil monitors every session.
	Monitored func(ctx context.Context, rows []store.Session) (func(store.Session) bool, error)
	// Inbox says which of the live sessions' waits are Open items of the
	// Needs you inbox (internal/inbox), and each one's page: only those are
	// notified about, and the page goes in Notification.URL. A session
	// whose wait isn't an Open item (In progress, Snoozed, Done, or not an
	// item yet) is skipped as if it didn't need you. An error skips the
	// check (Check returns it). Nil notifies without the inbox, as before.
	Inbox func(ctx context.Context, rows []store.Session) (func(store.Session) (url string, open bool), error)
	// On is the Settings page's macOS notifications switch,
	// asked before each check: while it says false, Check sends nothing
	// and forgets the spells it knew, so the first check after it turns
	// back on counts what waits then as already notified. Nil means on.
	// config notify.enabled still comes first: when it is false, nothing
	// is checked at all.
	On func(ctx context.Context) bool
	// Every is how often Run checks; zero means DefaultEvery.
	Every time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Logger receives send failures; nil discards them.
	Logger *slog.Logger
}

// DefaultEvery is how often Run checks by default: an interactive
// reminder lands within this long after notify.after.
const DefaultEvery = 10 * time.Second

// sendTimeout bounds one notification.
const sendTimeout = 10 * time.Second

// Watcher checks the sessions and notifies.
type Watcher struct {
	opts Options

	mu        sync.Mutex
	baselined bool
	spells    map[string]*spell
}

// spell is one needs-you spell of a session key.
type spell struct {
	since    time.Time
	notified bool
}

// New returns a watcher.
func New(opts Options) *Watcher {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Every <= 0 {
		opts.Every = DefaultEvery
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	return &Watcher{opts: opts, spells: map[string]*spell{}}
}

// Run checks every opts.Every until ctx is done.
func (w *Watcher) Run(ctx context.Context) error {
	if !w.opts.Enabled {
		return nil
	}
	t := time.NewTicker(w.opts.Every)
	defer t.Stop()
	for {
		if _, err := w.Check(ctx); err != nil && ctx.Err() == nil {
			w.opts.Logger.Warn("notify: read sessions", "err", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Check reads the sessions once and sends what is due. It returns what it
// sent; a failed send is logged, and that spell still counts as notified,
// so a broken osascript can't repeat itself every check.
func (w *Watcher) Check(ctx context.Context) ([]Notification, error) {
	if !w.opts.Enabled || (w.opts.Ready != nil && !w.opts.Ready()) {
		return nil, nil
	}
	if w.opts.On != nil && !w.opts.On(ctx) {
		w.mu.Lock()
		w.spells, w.baselined = map[string]*spell{}, false
		w.mu.Unlock()
		return nil, nil
	}
	rows, err := w.opts.Store.ListSessions(ctx, store.SessionFilter{})
	if err != nil {
		return nil, err
	}
	if w.opts.Monitored != nil {
		keep, err := w.opts.Monitored(ctx, rows)
		if err != nil {
			return nil, err
		}
		var kept []store.Session
		for _, r := range rows {
			if keep(r) {
				kept = append(kept, r)
			}
		}
		rows = kept
	}
	var urls map[string]string
	if w.opts.Inbox != nil {
		open, err := w.opts.Inbox(ctx, rows)
		if err != nil {
			return nil, err
		}
		urls = map[string]string{}
		var kept []store.Session
		for _, r := range rows {
			if u, ok := open(r); ok {
				kept = append(kept, r)
				urls[r.Key] = u
			}
		}
		rows = kept
	}
	due := w.due(rows, w.opts.Now())
	for i := range due {
		due[i].URL = urls[due[i].Key]
	}
	for _, n := range due {
		sctx, cancel := context.WithTimeout(ctx, sendTimeout)
		if err := w.opts.Sender.Send(sctx, n); err != nil {
			w.opts.Logger.Warn("notify: send", "key", n.Key, "err", err)
		} else {
			w.opts.Logger.Info("notified", "key", n.Key, "title", n.Title)
		}
		cancel()
	}
	return due, nil
}

// due updates the spells from rows and returns the notifications due now.
func (w *Watcher) due(rows []store.Session, now time.Time) []Notification {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []Notification
	seen := map[string]bool{}
	for _, r := range rows {
		if r.Headless || r.State != string(sessions.StateNeedsYou) || r.NeedsYouSince.IsZero() {
			continue
		}
		seen[r.Key] = true
		sp := w.spells[r.Key]
		if sp == nil || r.NeedsYouSince.After(sp.since) {
			// A new spell. Those present at the first check are old news.
			sp = &spell{since: r.NeedsYouSince, notified: !w.baselined}
			w.spells[r.Key] = sp
		}
		if sp.notified {
			continue
		}
		if !r.NeedsYouDismissedAt.IsZero() && !r.NeedsYouDismissedAt.Before(r.NeedsYouSince) {
			continue // you've seen it
		}
		waited := now.Sub(r.NeedsYouSince)
		switch {
		case r.Kind == string(sessions.Background):
			out = append(out, Notification{Key: r.Key, Title: "Background job needs you",
				Subtitle: title(r), Body: body(r)})
		case waited >= w.opts.After:
			out = append(out, Notification{Key: r.Key, Title: "Waiting on you for " + age(waited),
				Subtitle: title(r), Body: body(r)})
		default:
			continue
		}
		sp.notified = true
	}
	for k := range w.spells {
		if !seen[k] {
			delete(w.spells, k)
		}
	}
	w.baselined = true
	return out
}

// title names a session: its name, else its cwd's last folder.
func title(r store.Session) string {
	switch {
	case r.Name != "":
		return r.Name
	case r.Cwd != "":
		return filepath.Base(r.Cwd)
	}
	return r.Key
}

// body says why it needs you and where it works.
func body(r store.Session) string {
	reason := r.StateReason
	switch reason {
	case sessions.ReasonReply:
		reason = "waiting for your reply"
	case "", sessions.ReasonWaiting:
		reason = "waiting"
	}
	where := r.Repo
	if where == "" {
		where = r.Cwd
	}
	if where != "" {
		where = filepath.Base(where)
		if r.Branch != "" {
			where += " · " + r.Branch
		}
		return reason + " · " + where
	}
	return reason
}

// age writes a wait as 45s, 2m or 3h.
func age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", max(0, int(d/time.Second)))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	default:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
}
