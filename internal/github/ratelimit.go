package github

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Verdict says how much polling the GitHub budget allows right now.
type Verdict string

// Budget verdicts, from the lowest recent `remaining`.
const (
	// BudgetNormal: poll on the usual active and idle intervals.
	BudgetNormal Verdict = "normal"
	// BudgetIdleOnly: remaining is below IdleOnlyBelow; poll everything on
	// the idle interval.
	BudgetIdleOnly Verdict = "idle-only"
	// BudgetStop: remaining is below StopBelow, or GitHub asked us to back
	// off; don't poll until Budget.ResumeAt (on-demand reads only).
	BudgetStop Verdict = "stop"
)

// Budget thresholds on the lowest recent `remaining`.
const (
	IdleOnlyBelow = 1000
	StopBelow     = 200
)

// Budget is the rate-limit state across recent responses.
//
// GitHub's 5000 points an hour are shared with every other use of the same
// token (every agent's gh calls), and consecutive responses have been seen to
// alternate between two windows with different resetAt and remaining. So
// Remaining is the lowest value among the windows that haven't reset yet,
// not the latest one.
type Budget struct {
	Verdict Verdict
	// Known is false until a response has reported a rate limit; the verdict
	// is then BudgetNormal.
	Known bool
	// Remaining is the lowest remaining among live windows.
	Remaining int
	// Limit is that window's limit (5000 for a user token).
	Limit int
	// ResetAt is when that window resets.
	ResetAt time.Time
	// ResumeAt is when a BudgetStop verdict ends: ResetAt, or the end of a
	// Retry-After backoff. Zero unless the verdict is BudgetStop.
	ResumeAt time.Time
	// LastCost is the cost GitHub reported for the last query.
	LastCost int
}

// RateLimit is one response's rateLimit (or X-RateLimit-* headers).
type RateLimit struct {
	Cost      int       `json:"cost"`
	Remaining int       `json:"remaining"`
	Limit     int       `json:"limit"`
	Used      int       `json:"used"`
	ResetAt   time.Time `json:"resetAt"`
}

// maxWindows bounds how many distinct reset windows are remembered.
const maxWindows = 8

// rateTracker remembers the lowest remaining seen per reset window.
type rateTracker struct {
	mu         sync.Mutex
	windows    map[int64]RateLimit // resetAt (unix seconds) → lowest remaining seen
	retryUntil time.Time           // secondary-limit backoff (Retry-After)
	lastCost   int
}

// observe records one rate-limit report.
func (r *rateTracker) observe(rl RateLimit, now time.Time) {
	if rl.ResetAt.IsZero() {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.windows == nil {
		r.windows = map[int64]RateLimit{}
	}
	if rl.Cost > 0 { // headers carry no cost
		r.lastCost = rl.Cost
	}
	r.expire(now)
	key := rl.ResetAt.Unix()
	if old, ok := r.windows[key]; ok && old.Remaining <= rl.Remaining {
		return
	}
	r.windows[key] = rl
	for len(r.windows) > maxWindows {
		// Drop the window that resets first: it's the least relevant.
		var first int64
		for k := range r.windows {
			if first == 0 || k < first {
				first = k
			}
		}
		delete(r.windows, first)
	}
}

// backoff records a Retry-After from a secondary rate limit.
func (r *rateTracker) backoff(until time.Time) {
	r.mu.Lock()
	if until.After(r.retryUntil) {
		r.retryUntil = until
	}
	r.mu.Unlock()
}

// expire drops windows that have reset. Caller holds mu.
func (r *rateTracker) expire(now time.Time) {
	for k, w := range r.windows {
		if !w.ResetAt.After(now) {
			delete(r.windows, k)
		}
	}
}

func (r *rateTracker) budget(now time.Time) Budget {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expire(now)
	b := Budget{Verdict: BudgetNormal, LastCost: r.lastCost}
	for _, w := range r.windows {
		if !b.Known || w.Remaining < b.Remaining || (w.Remaining == b.Remaining && w.ResetAt.After(b.ResetAt)) {
			b.Known, b.Remaining, b.Limit, b.ResetAt = true, w.Remaining, w.Limit, w.ResetAt
		}
	}
	switch {
	case b.Known && b.Remaining < StopBelow:
		b.Verdict, b.ResumeAt = BudgetStop, b.ResetAt
	case b.Known && b.Remaining < IdleOnlyBelow:
		b.Verdict = BudgetIdleOnly
	}
	if r.retryUntil.After(now) {
		b.Verdict = BudgetStop
		if r.retryUntil.After(b.ResumeAt) {
			b.ResumeAt = r.retryUntil
		}
	}
	return b
}

// headerRateLimit reads X-RateLimit-* headers, which GitHub sends on every
// response, including errors that carry no rateLimit in the body.
func headerRateLimit(h http.Header) (RateLimit, bool) {
	if res := h.Get("X-RateLimit-Resource"); res != "" && res != "graphql" {
		return RateLimit{}, false
	}
	rem, err1 := strconv.Atoi(h.Get("X-RateLimit-Remaining"))
	reset, err2 := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64)
	if err1 != nil || err2 != nil {
		return RateLimit{}, false
	}
	rl := RateLimit{Remaining: rem, ResetAt: time.Unix(reset, 0).UTC()}
	rl.Limit, _ = strconv.Atoi(h.Get("X-RateLimit-Limit"))
	rl.Used, _ = strconv.Atoi(h.Get("X-RateLimit-Used"))
	return rl, true
}

// retryAfter reads a Retry-After header (seconds), as sent with a secondary
// rate limit.
func retryAfter(h http.Header, now time.Time) (time.Time, bool) {
	s, err := strconv.Atoi(h.Get("Retry-After"))
	if err != nil || s < 0 {
		return time.Time{}, false
	}
	return now.Add(time.Duration(s) * time.Second), true
}
