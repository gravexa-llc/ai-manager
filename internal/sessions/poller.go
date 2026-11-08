package sessions

import (
	"context"
	"errors"
	"sync"
	"time"
)

// Poll intervals: often while someone may be looking, so a session that
// starts waiting shows within seconds, and less often otherwise. A call
// costs little (see DefaultTimeout). The caller picks one and changes it
// with Poller.SetInterval as things change.
const (
	// IntervalWatched: the dashboard has a viewer, or a session needs you
	// or is working.
	IntervalWatched = 10 * time.Second
	// IntervalUnwatched: nobody is looking.
	IntervalUnwatched = 30 * time.Second
	// IntervalModLive: the mod reports live, so polling is only a backstop.
	IntervalModLive = 60 * time.Second

	// DefaultInterval is used when Options.Interval is unset. It matches
	// config.Default().Poll.Sessions.
	DefaultInterval = IntervalWatched
	// DefaultDebounce is how long a Kick waits for more kicks before it polls.
	DefaultDebounce = 500 * time.Millisecond
	// DefaultMinGap is the least time between the start of one poll and the
	// start of a kicked one.
	DefaultMinGap = 2 * time.Second
)

// ErrRunning is returned by Run when the poller is already running.
var ErrRunning = errors.New("sessions: poller already running")

// Trigger is what caused a poll.
type Trigger string

// Triggers.
const (
	TriggerStart    Trigger = "start"    // the first poll when Run starts
	TriggerInterval Trigger = "interval" // the interval elapsed
	TriggerKick     Trigger = "kick"     // Kick was called
)

// Result is the outcome of one poll.
type Result struct {
	Trigger Trigger
	// Snapshot is the new listing. Zero when Err is set.
	Snapshot Snapshot
	// Events are the differences from the last good snapshot. After the
	// first good poll every session is Appeared. A failed poll has none,
	// so a broken call never reports sessions as gone.
	Events []Event
	Err    error
}

// Options configure a Poller. Zero values mean the defaults.
type Options struct {
	// Lister runs each listing. Its zero value runs the real claude.
	Lister Lister
	// Interval between polls; changeable later with SetInterval.
	// Zero or less means DefaultInterval.
	Interval time.Duration
	// Debounce for Kick. Zero or less means DefaultDebounce.
	Debounce time.Duration
	// MinGap for Kick. Zero or less means DefaultMinGap.
	MinGap time.Duration
	// OnResult is called after every poll, from Run's goroutine. A slow
	// callback delays the next poll. Nil means results are only kept for
	// Latest.
	OnResult func(Result)
}

// Poller runs a Lister on an interval and on demand.
//
// A poll happens when Run starts, then each time the interval has passed
// since the last poll began, and after a Kick. A Kick waits Debounce for
// more kicks to coalesce, and never polls sooner than MinGap after the
// last poll began; any poll that starts after a kick satisfies it. Kicks
// that arrive while a poll runs lead to one more poll afterwards.
//
// Polls never overlap: one runs at a time, OnResult included. A poll that
// runs past the next interval tick skips that tick rather than queuing
// it: the next poll is at the first tick (the last start plus a whole
// number of intervals) at or after the slow poll ended. On a loaded
// machine a slow `claude agents` call so doesn't turn into back-to-back
// calls.
type Poller struct {
	lister   Lister
	debounce time.Duration
	minGap   time.Duration
	onResult func(Result)

	kick  chan struct{}
	reset chan struct{}

	mu        sync.Mutex
	interval  time.Duration
	latest    Snapshot
	hasLatest bool
	running   bool
}

// NewPoller returns a poller. Call Run to start it.
func NewPoller(opts Options) *Poller {
	p := &Poller{
		lister:   opts.Lister,
		debounce: opts.Debounce,
		minGap:   opts.MinGap,
		onResult: opts.OnResult,
		kick:     make(chan struct{}, 1),
		reset:    make(chan struct{}, 1),
		interval: opts.Interval,
	}
	if p.interval <= 0 {
		p.interval = DefaultInterval
	}
	if p.debounce <= 0 {
		p.debounce = DefaultDebounce
	}
	if p.minGap <= 0 {
		p.minGap = DefaultMinGap
	}
	return p
}

// Interval returns the current poll interval.
func (p *Poller) Interval() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.interval
}

// SetInterval changes the poll interval, effective at once: the next poll
// is due d after the last one began (skipping, as always, ticks that fell
// while that poll ran), and happens immediately if that time has passed.
// Zero or less means DefaultInterval. Safe to call at any time, from any
// goroutine.
func (p *Poller) SetInterval(d time.Duration) {
	if d <= 0 {
		d = DefaultInterval
	}
	p.mu.Lock()
	p.interval = d
	p.mu.Unlock()
	select {
	case p.reset <- struct{}{}:
	default:
	}
}

// Kick asks for a poll soon (see Poller for the debounce). It never
// blocks, and is safe to call at any time, from any goroutine. A kick
// before Run starts is covered by Run's first poll.
func (p *Poller) Kick() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

// Latest returns the last good snapshot, and false if there is none yet.
func (p *Poller) Latest() (Snapshot, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.latest, p.hasLatest
}

// Run polls until ctx is done, then returns nil. A poll in flight is
// cancelled and its result dropped. Run returns ErrRunning if the poller
// is already running.
func (p *Poller) Run(ctx context.Context) error {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return ErrRunning
	}
	p.running = true
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.running = false
		p.mu.Unlock()
	}()

	// Kicks from before Run are covered by the first poll.
	select {
	case <-p.kick:
	default:
	}

	lastStart := p.poll(ctx, TriggerStart)
	lastEnd := time.Now()
	untilDue := func() time.Duration {
		return max(0, time.Until(nextTick(lastStart, lastEnd, p.Interval())))
	}
	tick := time.NewTimer(untilDue())
	defer tick.Stop()

	var kickTimer *time.Timer
	var kickC <-chan time.Time
	stopKick := func() {
		if kickTimer != nil {
			kickTimer.Stop()
			kickTimer, kickC = nil, nil
		}
	}
	defer stopKick()

	pollNow := func(t Trigger) {
		stopKick()
		lastStart = p.poll(ctx, t)
		lastEnd = time.Now()
		tick.Reset(untilDue())
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			pollNow(TriggerInterval)
		case <-kickC:
			pollNow(TriggerKick)
		case <-p.kick:
			// A kick while one is scheduled joins it.
			if kickC == nil {
				due := time.Now().Add(p.debounce)
				if earliest := lastStart.Add(p.minGap); earliest.After(due) {
					due = earliest
				}
				kickTimer = time.NewTimer(time.Until(due))
				kickC = kickTimer.C
			}
		case <-p.reset:
			tick.Reset(untilDue())
		}
	}
}

// nextTick returns when the poll after one that began at start and ended
// at end is due, polling every interval: start+interval, or if the poll
// ran past that, the first start+n*interval at or after end. The ticks
// that fell while it ran are skipped, not queued.
func nextTick(start, end time.Time, interval time.Duration) time.Time {
	next := start.Add(interval)
	if ran := end.Sub(start); interval > 0 && next.Before(end) {
		n := (ran + interval - 1) / interval // ran/interval, rounded up
		next = start.Add(n * interval)
	}
	return next
}

// poll runs one listing, diffs it against the last good one and reports
// it. It returns when the poll started.
func (p *Poller) poll(ctx context.Context, t Trigger) time.Time {
	start := time.Now()
	snap, err := p.lister.List(ctx)
	if ctx.Err() != nil {
		return start // shutting down
	}
	res := Result{Trigger: t, Err: err}
	if err == nil {
		p.mu.Lock()
		prev := p.latest
		p.latest, p.hasLatest = snap, true
		p.mu.Unlock()
		res.Snapshot = snap
		res.Events = Diff(prev, snap)
	}
	if p.onResult != nil {
		p.onResult(res)
	}
	return start
}
