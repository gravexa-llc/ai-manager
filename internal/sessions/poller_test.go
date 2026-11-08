package sessions

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// The poller tests run in a synctest bubble: time is fake and only moves
// when every goroutine in the bubble is blocked, so the timings below are
// exact and the tests take no real time.

type recorder struct {
	mu      sync.Mutex
	results []Result
}

func (r *recorder) add(res Result) {
	r.mu.Lock()
	r.results = append(r.results, res)
	r.mu.Unlock()
}

func (r *recorder) get() []Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.results)
}

// harness runs a poller in the current bubble.
type harness struct {
	t    *testing.T
	t0   time.Time
	p    *Poller
	f    *fakeRunner
	rec  *recorder
	stop context.CancelFunc
	done chan error
}

func newHarness(t *testing.T, f *fakeRunner, opts Options) *harness {
	t.Helper()
	h := &harness{t: t, t0: time.Now(), f: f, rec: &recorder{}, done: make(chan error, 1)}
	opts.Lister.Runner = f
	opts.OnResult = h.rec.add
	h.p = NewPoller(opts)
	return h
}

func (h *harness) run() {
	ctx, cancel := context.WithCancel(context.Background())
	h.stop = cancel
	go func() { h.done <- h.p.Run(ctx) }()
	synctest.Wait()
}

// at sleeps until offset d from the start of the test, then lets the
// poller settle.
func (h *harness) at(d time.Duration) {
	h.t.Helper()
	if wait := time.Until(h.t0.Add(d)); wait > 0 {
		time.Sleep(wait)
	} else if wait < 0 {
		h.t.Fatalf("at(%v): already at %v", d, time.Since(h.t0))
	}
	synctest.Wait()
}

func (h *harness) shutdown() {
	h.t.Helper()
	h.stop()
	if err := <-h.done; err != nil {
		h.t.Errorf("Run returned %v", err)
	}
}

// polls returns when each call started, as offsets from the test start.
func (h *harness) polls() []time.Duration {
	var out []time.Duration
	for _, c := range h.f.Calls() {
		out = append(out, c.at.Sub(h.t0))
	}
	return out
}

func (h *harness) wantPolls(want ...time.Duration) {
	h.t.Helper()
	if got := h.polls(); !slices.Equal(got, want) {
		h.t.Errorf("polls at %v, want %v", got, want)
	}
}

func (h *harness) wantTriggers(want ...Trigger) {
	h.t.Helper()
	var got []Trigger
	for _, r := range h.rec.get() {
		got = append(got, r.Trigger)
	}
	if !slices.Equal(got, want) {
		h.t.Errorf("triggers %v, want %v", got, want)
	}
}

const ms = time.Millisecond

func TestPollerInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, &fakeRunner{}, Options{Interval: 30 * time.Second})
		h.run()
		h.at(95 * time.Second)
		h.wantPolls(0, 30*time.Second, 60*time.Second, 90*time.Second)
		h.wantTriggers(TriggerStart, TriggerInterval, TriggerInterval, TriggerInterval)
		h.shutdown()
	})
}

func TestPollerDefaults(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, &fakeRunner{}, Options{})
		if h.p.Interval() != DefaultInterval {
			t.Errorf("Interval() = %v", h.p.Interval())
		}
		h.run()
		h.at(25 * time.Second)
		h.wantPolls(0, 10*time.Second, 20*time.Second)

		// A kick: 500ms debounce, and 2s after the last poll at the earliest.
		h.p.Kick()
		h.at(25*time.Second + 499*ms)
		h.wantPolls(0, 10*time.Second, 20*time.Second)
		h.at(25*time.Second + 500*ms)
		h.wantPolls(0, 10*time.Second, 20*time.Second, 25*time.Second+500*ms)
		h.shutdown()
	})
}

func TestPollerSetInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, &fakeRunner{}, Options{Interval: 30 * time.Second})
		h.run()

		// Shorter: the next poll is due 10s after the last one began.
		h.at(5 * time.Second)
		h.p.SetInterval(10 * time.Second)
		if h.p.Interval() != 10*time.Second {
			t.Errorf("Interval() = %v", h.p.Interval())
		}
		h.at(25 * time.Second)
		h.wantPolls(0, 10*time.Second, 20*time.Second)

		// Longer: due 60s after the poll at 20s.
		h.p.SetInterval(60 * time.Second)
		h.at(79 * time.Second)
		h.wantPolls(0, 10*time.Second, 20*time.Second)
		h.at(85 * time.Second)
		h.wantPolls(0, 10*time.Second, 20*time.Second, 80*time.Second)

		// Shorter than the time already waited: poll at once.
		h.at(100 * time.Second)
		h.p.SetInterval(10 * time.Second)
		synctest.Wait()
		h.wantPolls(0, 10*time.Second, 20*time.Second, 80*time.Second, 100*time.Second)
		h.at(110 * time.Second)
		h.wantPolls(0, 10*time.Second, 20*time.Second, 80*time.Second, 100*time.Second, 110*time.Second)

		// Zero or less means the default.
		h.p.SetInterval(0)
		if h.p.Interval() != DefaultInterval {
			t.Errorf("Interval() after SetInterval(0) = %v", h.p.Interval())
		}
		h.shutdown()
	})
}

func TestPollerSetIntervalBeforeRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, &fakeRunner{}, Options{Interval: 30 * time.Second})
		h.p.SetInterval(5 * time.Second)
		h.run()
		h.at(11 * time.Second)
		h.wantPolls(0, 5*time.Second, 10*time.Second)
		h.shutdown()
	})
}

func TestPollerKickDebounce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, &fakeRunner{}, Options{Interval: 30 * time.Second})
		h.run()

		// A burst of kicks coalesces into one poll, 500ms after the first.
		h.at(10 * time.Second)
		h.p.Kick()
		h.at(10*time.Second + 100*ms)
		h.p.Kick()
		h.p.Kick()
		h.at(10*time.Second + 400*ms)
		h.p.Kick()
		h.at(12 * time.Second)
		h.wantPolls(0, 10*time.Second+500*ms)

		// The next kick waits for the 2s gap after that poll began.
		h.p.Kick()
		h.at(12*time.Second + 499*ms)
		h.wantPolls(0, 10*time.Second+500*ms)
		h.at(12*time.Second + 500*ms)
		h.wantPolls(0, 10*time.Second+500*ms, 12*time.Second+500*ms)

		// A kicked poll restarts the interval.
		h.at(42*time.Second + 499*ms)
		h.wantPolls(0, 10*time.Second+500*ms, 12*time.Second+500*ms)
		h.at(42*time.Second + 500*ms)
		h.wantPolls(0, 10*time.Second+500*ms, 12*time.Second+500*ms, 42*time.Second+500*ms)
		h.wantTriggers(TriggerStart, TriggerKick, TriggerKick, TriggerInterval)
		h.shutdown()
	})
}

func TestPollerKickRightAfterStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, &fakeRunner{}, Options{Interval: 30 * time.Second})
		h.p.Kick() // before Run: covered by the first poll
		h.run()
		h.p.Kick()
		h.at(5 * time.Second)
		h.wantPolls(0, 2*time.Second)
		h.shutdown()
	})
}

func TestPollerCustomDebounce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, &fakeRunner{}, Options{Interval: time.Minute, Debounce: 3 * time.Second, MinGap: time.Second})
		h.run()
		h.at(10 * time.Second)
		h.p.Kick()
		h.at(20 * time.Second)
		h.wantPolls(0, 13*time.Second)
		h.shutdown()
	})
}

func TestPollerKickDuringPoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Each call takes 1s.
		h := newHarness(t, &fakeRunner{delay: time.Second}, Options{Interval: 30 * time.Second})
		h.run() // the first poll runs 0s-1s
		h.at(10 * time.Second)
		h.p.Kick() // poll at 10.5s, running until 11.5s
		h.at(11 * time.Second)
		h.p.Kick() // during that poll: one more, 2s after it began
		h.p.Kick()
		h.at(20 * time.Second)
		h.wantPolls(0, 10*time.Second+500*ms, 12*time.Second+500*ms)
		h.shutdown()
	})
}

func TestPollerIntervalPollSatisfiesKick(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, &fakeRunner{}, Options{Interval: 10 * time.Second})
		h.run()
		h.at(9*time.Second + 800*ms)
		h.p.Kick() // due at 10.3s, but the interval poll at 10s covers it
		h.at(15 * time.Second)
		h.wantPolls(0, 10*time.Second)
		h.wantTriggers(TriggerStart, TriggerInterval)
		h.shutdown()
	})
}

func TestPollerResults(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		at2115 := readFixture(t, "active-2026-11-05T2115.json")
		at2117 := readFixture(t, "active-with-claude-p-2026-11-05T2117.json")
		at2127 := readFixture(t, "active-2026-11-05T2127.json")
		boom := errors.New("claude exploded")
		outputs := []struct {
			out []byte
			err error
		}{
			{out: []byte("not json")}, // a bad first call: nothing is known yet
			{out: at2115},
			{out: at2117},
			{err: boom},
			{out: []byte("")},
			{out: at2127},
		}
		f := &fakeRunner{next: func(n int) ([]byte, error) {
			o := outputs[min(n, len(outputs)-1)]
			return o.out, o.err
		}}
		h := newHarness(t, f, Options{Interval: 10 * time.Second})
		if _, ok := h.p.Latest(); ok {
			t.Error("Latest() before any poll")
		}
		h.run()
		h.at(55 * time.Second)
		h.shutdown()

		res := h.rec.get()
		if len(res) != 6 {
			t.Fatalf("%d results", len(res))
		}
		if res[0].Err == nil || len(res[0].Events) != 0 || res[0].Trigger != TriggerStart {
			t.Errorf("result 0: %+v", res[0])
		}
		if res[1].Err != nil || len(res[1].Events) != 16 || res[1].Events[0].Kind != Appeared {
			t.Errorf("result 1: err %v, %d events", res[1].Err, len(res[1].Events))
		}
		if got := summary(res[2].Events); !slices.Equal(got, []string{"appeared pid:48169"}) {
			t.Errorf("result 2: %q", got)
		}
		// Failed polls report the error and no events: nothing is gone.
		for _, i := range []int{3, 4} {
			if res[i].Err == nil || len(res[i].Events) != 0 || len(res[i].Snapshot.Sessions) != 0 {
				t.Errorf("result %d: %+v", i, res[i])
			}
		}
		if !errors.Is(res[3].Err, boom) {
			t.Errorf("result 3: err %v", res[3].Err)
		}
		// The diff is against the last good snapshot.
		if got := summary(res[5].Events); !slices.Equal(got, []string{"gone pid:48169"}) {
			t.Errorf("result 5: %q", got)
		}
		if !res[5].Snapshot.All || !res[5].Events[0].At.Equal(res[5].Snapshot.At) {
			t.Errorf("result 5 snapshot: All %v At %v", res[5].Snapshot.All, res[5].Snapshot.At)
		}

		latest, ok := h.p.Latest()
		if !ok || len(latest.Sessions) != 16 || !latest.At.Equal(h.t0.Add(50*time.Second)) {
			t.Errorf("Latest() = %d sessions at %v, %v", len(latest.Sessions), latest.At.Sub(h.t0), ok)
		}
	})
}

func TestPollerTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// A call that hangs is cut off by the lister's timeout and the
		// poller carries on.
		f := &fakeRunner{delay: time.Hour}
		h := newHarness(t, f, Options{Interval: 10 * time.Second, Lister: Lister{Timeout: 3 * time.Second}})
		h.run()
		h.at(29 * time.Second) // the poll at 20s timed out at 23s
		h.wantPolls(0, 10*time.Second, 20*time.Second)
		for i, r := range h.rec.get() {
			if !errors.Is(r.Err, context.DeadlineExceeded) {
				t.Errorf("result %d: err %v", i, r.Err)
			}
		}
		for _, c := range f.Calls() {
			if got := c.deadline.Sub(c.at); got != 3*time.Second {
				t.Errorf("call deadline %v after start, want 3s", got)
			}
		}
		h.shutdown()
	})
}

// On a loaded machine `claude agents` can take seconds. A call that
// takes 15s, three times the old bound, succeeds within DefaultTimeout.
func TestPollerSlowCallWithinTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeRunner{delay: 15 * time.Second}
		h := newHarness(t, f, Options{Interval: 30 * time.Second})
		h.run()
		h.at(20 * time.Second)
		res := h.rec.get()
		if len(res) != 1 || res[0].Err != nil || !res[0].Snapshot.At.Equal(h.t0) {
			t.Fatalf("results %+v, want one good poll", res)
		}
		if c := f.Calls()[0]; c.deadline.Sub(c.at) != DefaultTimeout || DefaultTimeout < 15*time.Second {
			t.Errorf("call deadline %v after start, want DefaultTimeout (%v)", c.deadline.Sub(c.at), DefaultTimeout)
		}
		h.shutdown()
	})
}

// Polls never overlap, and a poll that runs past the next tick skips it:
// the next poll is at the first tick at or after it ended, not at once.
func TestPollerSkipsTicksDuringSlowPoll(t *testing.T) {
	for _, tt := range []struct {
		name   string
		delay  time.Duration
		lister Lister
		want   []time.Duration
	}{
		// 0-15s, 20-35s, 40-55s: the ticks at 10s, 30s and 50s are skipped.
		{"slow", 15 * time.Second, Lister{}, []time.Duration{0, 20 * time.Second, 40 * time.Second}},
		// Each call times out after 25s: 0-25s, 30-55s.
		{"timing out", time.Hour, Lister{Timeout: 25 * time.Second}, []time.Duration{0, 30 * time.Second}},
		// Each call times out after 20s, just as a tick is due: it runs then.
		{"timing out on a tick", time.Hour, Lister{}, []time.Duration{0, 20 * time.Second, 40 * time.Second}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := &fakeRunner{delay: tt.delay}
				h := newHarness(t, f, Options{Interval: 10 * time.Second, Lister: tt.lister})
				h.run()
				h.at(59 * time.Second)
				h.wantPolls(tt.want...)
				if n := f.MaxInFlight(); n != 1 {
					t.Errorf("%d calls ran at once, want 1", n)
				}
				h.shutdown()
			})
		})
	}
}

// A shorter interval set while a poll runs skips the ticks that fell
// during it too.
func TestPollerSetIntervalDuringSlowPoll(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := &fakeRunner{delay: 15 * time.Second}
		h := newHarness(t, f, Options{Interval: 30 * time.Second})
		h.run()
		h.at(5 * time.Second)
		h.p.SetInterval(10 * time.Second) // the tick at 10s falls during the poll
		h.at(19 * time.Second)
		h.wantPolls(0)
		h.at(21 * time.Second)
		h.wantPolls(0, 20*time.Second)
		h.shutdown()
	})
}

func TestNextTick(t *testing.T) {
	t0 := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	s := time.Second
	for _, tt := range []struct {
		ran, interval, want time.Duration
	}{
		{0, 10 * s, 10 * s},
		{9 * s, 10 * s, 10 * s},
		{10 * s, 10 * s, 10 * s}, // ended on the tick: it isn't skipped
		{10*s + 1, 10 * s, 20 * s},
		{25 * s, 10 * s, 30 * s},
		{30 * s, 10 * s, 30 * s},
		{95 * s, 30 * s, 120 * s},
	} {
		if got := nextTick(t0, t0.Add(tt.ran), tt.interval).Sub(t0); got != tt.want {
			t.Errorf("nextTick after a %v run every %v = +%v, want +%v", tt.ran, tt.interval, got, tt.want)
		}
	}
}

func TestPollerShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, &fakeRunner{delay: 2 * time.Second}, Options{Interval: 10 * time.Second})
		h.run()
		h.at(11 * time.Second) // the second poll is in flight
		h.shutdown()
		// Only the first poll reported; the cancelled one is dropped.
		h.wantTriggers(TriggerStart)

		// Kick and SetInterval don't block after Run has returned.
		h.p.Kick()
		h.p.Kick()
		h.p.SetInterval(time.Second)
		h.p.SetInterval(2 * time.Second)
	})
}

func TestPollerRunTwice(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, &fakeRunner{}, Options{})
		h.run()
		if err := h.p.Run(context.Background()); !errors.Is(err, ErrRunning) {
			t.Errorf("second Run: %v", err)
		}
		h.shutdown()

		// It can run again once stopped.
		h.done = make(chan error, 1)
		h.run()
		h.shutdown()
	})
}
