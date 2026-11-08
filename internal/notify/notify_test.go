package notify

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/store"
)

// fakeStore serves whatever rows the test sets.
type fakeStore struct {
	mu   sync.Mutex
	rows []store.Session
}

func (f *fakeStore) ListSessions(context.Context, store.SessionFilter) ([]store.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows, nil
}

func (f *fakeStore) set(rows ...store.Session) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = rows
}

// fakeSender keeps what it was asked to send.
type fakeSender struct {
	mu   sync.Mutex
	sent []Notification
	err  error
}

func (f *fakeSender) Send(_ context.Context, n Notification) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, n)
	return f.err
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

// clock is a fake clock.
type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

var t0 = time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)

type harness struct {
	st     *fakeStore
	sender *fakeSender
	clock  *clock
	w      *Watcher
	ready  bool
}

func newHarness(enabled bool) *harness {
	h := &harness{st: &fakeStore{}, sender: &fakeSender{}, clock: &clock{now: t0}, ready: true}
	h.w = New(Options{
		Enabled: enabled,
		Store:   h.st,
		Sender:  h.sender,
		After:   2 * time.Minute,
		Ready:   func() bool { return h.ready },
		Now:     h.clock.Now,
	})
	return h
}

// check runs one check at now+d and returns the keys notified.
func (h *harness) check(t *testing.T, d time.Duration) []string {
	t.Helper()
	h.clock.now = t0.Add(d)
	sent, err := h.w.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, n := range sent {
		keys = append(keys, n.Key)
	}
	return keys
}

func job(key string, since time.Time) store.Session {
	return store.Session{Key: key, JobID: strings.TrimPrefix(key, "job:"), Kind: "background", Name: "sharing reports",
		Cwd: "/r/pivot", Repo: "/r/pivot", Branch: "share/v1-email-reports", State: "needs_you", StateReason: "reply", NeedsYouSince: since}
}

func tab(key string, since time.Time) store.Session {
	return store.Session{Key: key, PID: 42, Kind: "interactive", Cwd: "/r/ai-manager", State: "needs_you",
		StateReason: "permission prompt", NeedsYouSince: since}
}

func working(key, kind string) store.Session {
	return store.Session{Key: key, Kind: kind, State: "working"}
}

// The spells already there at the first check are old news.
func TestBaseline(t *testing.T) {
	h := newHarness(true)
	h.st.rows = []store.Session{job("job:aaaa0001", t0.Add(-90*24*time.Hour)), tab("pid:42", t0.Add(-time.Hour))}
	if got := h.check(t, 0); len(got) != 0 {
		t.Errorf("first check notified %v", got)
	}
	if got := h.check(t, 10*time.Minute); len(got) != 0 {
		t.Errorf("later check notified %v", got)
	}
}

// Nothing is checked, and so nothing baselined, until the first session
// poll.
func TestWaitsForReady(t *testing.T) {
	h := newHarness(true)
	h.ready = false
	h.st.rows = []store.Session{job("job:aaaa0001", t0)}
	if got := h.check(t, 0); len(got) != 0 {
		t.Errorf("notified before ready: %v", got)
	}
	h.ready = true
	if got := h.check(t, time.Second); len(got) != 0 {
		t.Errorf("first ready check notified %v (it is the baseline)", got)
	}
}

func TestBackgroundBlocked(t *testing.T) {
	h := newHarness(true)
	h.st.rows = []store.Session{working("job:022c44bf", "background")}
	h.check(t, 0) // baseline: nothing needs you

	h.st.rows = []store.Session{job("job:022c44bf", t0.Add(10*time.Second))}
	if got := h.check(t, 10*time.Second); !slices.Equal(got, []string{"job:022c44bf"}) {
		t.Fatalf("blocked job: notified %v", got)
	}
	n := h.sender.sent[0]
	if n.Title != "Background job needs you" || n.Subtitle != "sharing reports" || n.Body != "waiting for your reply · pivot · share/v1-email-reports" {
		t.Errorf("notification = %+v", n)
	}
	// Once per spell.
	if got := h.check(t, 20*time.Second); len(got) != 0 {
		t.Errorf("repeated: %v", got)
	}
	if got := h.check(t, time.Hour); len(got) != 0 {
		t.Errorf("repeated an hour later: %v", got)
	}

	// It gets its reply, works, and blocks again: a new spell.
	h.st.rows = []store.Session{working("job:022c44bf", "background")}
	h.check(t, 2*time.Hour)
	h.st.rows = []store.Session{job("job:022c44bf", t0.Add(3*time.Hour))}
	if got := h.check(t, 3*time.Hour); !slices.Equal(got, []string{"job:022c44bf"}) {
		t.Errorf("second spell: notified %v", got)
	}

	// A later NeedsYouSince is a new spell even if no check saw it stop.
	h.st.rows = []store.Session{job("job:022c44bf", t0.Add(4*time.Hour))}
	if got := h.check(t, 4*time.Hour); !slices.Equal(got, []string{"job:022c44bf"}) {
		t.Errorf("spell restarted between checks: notified %v", got)
	}
}

// A job that appears already blocked after the baseline is news too.
func TestNewJobAlreadyBlocked(t *testing.T) {
	h := newHarness(true)
	h.check(t, 0)
	h.st.rows = []store.Session{job("job:aaaa0002", t0.Add(time.Minute))}
	if got := h.check(t, time.Minute); !slices.Equal(got, []string{"job:aaaa0002"}) {
		t.Errorf("notified %v", got)
	}
}

func TestInteractiveReminder(t *testing.T) {
	h := newHarness(true)
	h.check(t, 0)
	since := t0.Add(time.Minute)
	h.st.rows = []store.Session{tab("pid:42", since)}
	// Claude Code has told you already; aimd waits for notify.after.
	for _, d := range []time.Duration{time.Minute, 2 * time.Minute, 2*time.Minute + 59*time.Second} {
		if got := h.check(t, d); len(got) != 0 {
			t.Errorf("at %s: notified %v", d, got)
		}
	}
	if got := h.check(t, 3*time.Minute); !slices.Equal(got, []string{"pid:42"}) {
		t.Fatalf("at 3m (waited 2m): notified %v", got)
	}
	n := h.sender.sent[0]
	if n.Title != "Waiting on you for 2m" || n.Subtitle != "ai-manager" || n.Body != "permission prompt · ai-manager" {
		t.Errorf("notification = %+v", n)
	}
	if got := h.check(t, 10*time.Minute); len(got) != 0 {
		t.Errorf("repeated: %v", got)
	}
}

func TestInteractiveDismissed(t *testing.T) {
	h := newHarness(true)
	h.check(t, 0)
	row := tab("pid:42", t0)
	row.NeedsYouDismissedAt = t0.Add(30 * time.Second)
	h.st.rows = []store.Session{row}
	if got := h.check(t, 5*time.Minute); len(got) != 0 {
		t.Errorf("dismissed: notified %v", got)
	}
	// A dismissal from an earlier spell doesn't count.
	row = tab("pid:42", t0.Add(10*time.Minute))
	row.NeedsYouDismissedAt = t0.Add(30 * time.Second)
	h.st.rows = []store.Session{row}
	if got := h.check(t, 13*time.Minute); !slices.Equal(got, []string{"pid:42"}) {
		t.Errorf("new spell after an old dismissal: notified %v", got)
	}
}

func TestSkips(t *testing.T) {
	h := newHarness(true)
	h.check(t, 0)
	headless := tab("pid:43", t0)
	headless.Headless = true
	noSince := job("job:aaaa0003", time.Time{})
	h.st.rows = []store.Session{headless, noSince, working("pid:44", "interactive")}
	if got := h.check(t, time.Hour); len(got) != 0 {
		t.Errorf("notified %v", got)
	}
}

func TestDisabled(t *testing.T) {
	h := newHarness(false)
	h.check(t, 0)
	h.st.rows = []store.Session{job("job:022c44bf", t0.Add(time.Second)), tab("pid:42", t0)}
	if got := h.check(t, time.Hour); len(got) != 0 || len(h.sender.sent) != 0 {
		t.Errorf("disabled: notified %v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.w.Run(ctx); err != nil {
		t.Errorf("disabled Run = %v, want an immediate nil", err)
	}
}

// The Settings page's switch: off sends nothing; back on, what waits
// then is old news, and only what starts waiting afterwards is notified.
func TestSwitchOff(t *testing.T) {
	h := newHarness(true)
	on := true
	h.w.opts.On = func(context.Context) bool { return on }
	h.check(t, 0) // baseline: nothing needs you

	on = false
	h.st.rows = []store.Session{job("job:022c44bf", t0.Add(time.Second)), tab("pid:42", t0)}
	for _, d := range []time.Duration{time.Second, 5 * time.Minute, time.Hour} {
		if got := h.check(t, d); len(got) != 0 {
			t.Errorf("switched off, at %s: notified %v", d, got)
		}
	}
	if len(h.sender.sent) != 0 {
		t.Errorf("switched off: sent %+v", h.sender.sent)
	}

	on = true
	if got := h.check(t, 2*time.Hour); len(got) != 0 {
		t.Errorf("switched back on: notified the old waits %v", got)
	}
	h.st.rows = append(h.st.rows, job("job:aaaa0009", t0.Add(3*time.Hour)))
	if got := h.check(t, 3*time.Hour); !slices.Equal(got, []string{"job:aaaa0009"}) {
		t.Errorf("a new wait after switching back on: notified %v", got)
	}
}

// A failed send still counts, so a broken osascript doesn't retry every
// ten seconds.
func TestSendFailureCountsOnce(t *testing.T) {
	h := newHarness(true)
	h.sender.err = errors.New("osascript: not allowed")
	h.check(t, 0)
	h.st.rows = []store.Session{job("job:022c44bf", t0.Add(time.Second))}
	h.check(t, time.Second)
	h.check(t, time.Minute)
	if len(h.sender.sent) != 1 {
		t.Errorf("sent %d times", len(h.sender.sent))
	}
}

func TestRunChecksOnATicker(t *testing.T) {
	h := newHarness(true)
	h.w.opts.Every = time.Millisecond
	h.w.opts.Now = time.Now
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error)
	go func() { done <- h.w.Run(ctx) }()
	// Wait for the baseline, then block a job.
	for {
		h.w.mu.Lock()
		based := h.w.baselined
		h.w.mu.Unlock()
		if based || ctx.Err() != nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	h.st.set(job("job:022c44bf", time.Now()))
	for h.sender.count() == 0 && ctx.Err() == nil {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Run = %v", err)
	}
	if n := h.sender.count(); n != 1 {
		t.Errorf("sent %d", n)
	}
}

// fakeRunner records the script and its arguments.
type fakeRunner struct {
	script string
	args   []string
}

func (f *fakeRunner) Run(_ context.Context, script string, args ...string) (string, error) {
	f.script, f.args = script, args
	return "", nil
}

func TestOsascriptSender(t *testing.T) {
	r := &fakeRunner{}
	err := Osascript{Runner: r}.Send(context.Background(), Notification{
		Title: "Background job needs you", Subtitle: "say \"hi\"\nthen\tquit", Body: strings.Repeat("x", 300),
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.script != Script() || !strings.HasPrefix(r.script, "on run argv") || strings.Contains(r.script, "do shell script") {
		t.Errorf("script = %q", r.script)
	}
	if len(r.args) != 3 || r.args[1] != "Background job needs you" || r.args[2] != `say "hi" then quit` {
		t.Errorf("args = %q", r.args)
	}
	if body := []rune(r.args[0]); len(body) != maxText+1 || body[maxText] != '…' {
		t.Errorf("body not cut: %d runes", len(body))
	}
}

func TestClean(t *testing.T) {
	for in, want := range map[string]string{
		"  a\n\nb\tc  ": "a b c",
		"\x00x\x1by":    "x y",
		"":              "",
		"ok":            "ok",
	} {
		if got := Clean(in); got != want {
			t.Errorf("Clean(%q) = %q, want %q", in, got, want)
		}
	}
}

// Only monitored sessions are notified about (internal/monitor). A session that
// becomes monitored while it waits is notified about then.
func TestOnlyMonitored(t *testing.T) {
	h := newHarness(true)
	monitored := map[string]bool{"job:mine": true}
	var calls int
	h.w.opts.Monitored = func(_ context.Context, rows []store.Session) (func(store.Session) bool, error) {
		calls++
		return func(r store.Session) bool { return monitored[r.Key] }, nil
	}
	h.check(t, 0) // baseline: nothing waits yet
	h.st.set(job("job:mine", t0.Add(time.Minute)), job("job:other", t0.Add(time.Minute)), tab("pid:42", t0.Add(time.Minute)))
	if got := h.check(t, time.Minute); !slices.Equal(got, []string{"job:mine"}) {
		t.Errorf("notified %v, want only job:mine", got)
	}
	if got := h.check(t, 5*time.Minute); len(got) != 0 {
		t.Errorf("the unmonitored reminder was sent: %v", got)
	}
	// pid:42 becomes monitored (/aim watch in it, say) while it still
	// waits: its reminder is due.
	monitored["pid:42"] = true
	if got := h.check(t, 6*time.Minute); !slices.Equal(got, []string{"pid:42"}) {
		t.Errorf("after watching: notified %v, want pid:42", got)
	}
	if calls != 4 {
		t.Errorf("Monitored called %d times, want once per check", calls)
	}

	// An error skips the check, and says so.
	h.w.opts.Monitored = func(context.Context, []store.Session) (func(store.Session) bool, error) {
		return nil, errors.New("db gone")
	}
	h.st.set(job("job:new", t0.Add(10*time.Minute)))
	h.clock.now = t0.Add(10 * time.Minute)
	if sent, err := h.w.Check(context.Background()); err == nil || len(sent) != 0 {
		t.Errorf("Check with a failing filter = %v, %v", sent, err)
	}
}

// Only Open inbox items are notified about (internal/inbox), each with its
// item's page; a wait you moved to In progress, snoozed or marked Done is
// skipped, and notified about when its item comes back to Open.
func TestOnlyOpenInboxItems(t *testing.T) {
	h := newHarness(true)
	// The inbox's items: Open ones have a page, the others none.
	state := map[string]string{}
	pages := map[string]string{"job:a": "/app/inbox?item=1", "job:b": "/app/inbox?item=2", "pid:42": "/app/inbox?item=3"}
	h.w.opts.Inbox = func(_ context.Context, rows []store.Session) (func(store.Session) (string, bool), error) {
		return func(r store.Session) (string, bool) {
			if state[r.Key] != "open" {
				return "", false
			}
			return pages[r.Key], true
		}, nil
	}
	h.check(t, 0) // baseline: nothing waits yet

	// job:a is Open, job:b snoozed, pid:42 In progress.
	state["job:a"], state["job:b"], state["pid:42"] = "open", "snoozed", "progress"
	h.st.set(job("job:a", t0.Add(time.Minute)), job("job:b", t0.Add(time.Minute)), tab("pid:42", t0.Add(time.Minute)))
	h.clock.now = t0.Add(time.Minute)
	sent, err := h.w.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sent) != 1 || sent[0].Key != "job:a" || sent[0].URL != "/app/inbox?item=1" {
		t.Fatalf("sent %+v, want job:a with its item's page", sent)
	}
	if got := h.check(t, 5*time.Minute); len(got) != 0 {
		t.Errorf("the snoozed and In progress waits were notified: %v", got)
	}
	// job:b's snooze ends and pid:42's Go delay runs out: both come back to
	// Open, and are notified about then (pid:42 has waited past After).
	state["job:b"], state["pid:42"] = "open", "open"
	if got := h.check(t, 11*time.Minute); !slices.Equal(got, []string{"job:b", "pid:42"}) {
		t.Errorf("back to Open: notified %v, want job:b and pid:42", got)
	}
	// Done never notifies, even a new wait.
	state["job:c"] = "done"
	h.st.set(job("job:c", t0.Add(12*time.Minute)))
	if got := h.check(t, 12*time.Minute); len(got) != 0 {
		t.Errorf("a Done item was notified: %v", got)
	}

	// An error skips the check, and says so.
	h.w.opts.Inbox = func(context.Context, []store.Session) (func(store.Session) (string, bool), error) {
		return nil, errors.New("inbox gone")
	}
	h.clock.now = t0.Add(13 * time.Minute)
	if sent, err := h.w.Check(context.Background()); err == nil || len(sent) != 0 {
		t.Errorf("Check with a failing inbox = %v, %v", sent, err)
	}
}
