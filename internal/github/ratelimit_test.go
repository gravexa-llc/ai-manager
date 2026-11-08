package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestBudgetThresholds(t *testing.T) {
	for _, tc := range []struct {
		remaining int
		want      Verdict
	}{
		{5000, BudgetNormal},
		{1000, BudgetNormal},
		{999, BudgetIdleOnly},
		{200, BudgetIdleOnly},
		{199, BudgetStop},
		{0, BudgetStop},
	} {
		t.Run(strconv.Itoa(tc.remaining), func(t *testing.T) {
			var r rateTracker
			reset := t0.Add(20 * time.Minute)
			r.observe(RateLimit{Cost: 1, Remaining: tc.remaining, Limit: 5000, ResetAt: reset}, t0)
			b := r.budget(t0)
			if b.Verdict != tc.want || !b.Known || b.Remaining != tc.remaining || b.Limit != 5000 || !b.ResetAt.Equal(reset) {
				t.Errorf("budget = %+v, want %s", b, tc.want)
			}
			if tc.want == BudgetStop && !b.ResumeAt.Equal(reset) {
				t.Errorf("ResumeAt = %v, want the reset %v", b.ResumeAt, reset)
			}
			if tc.want != BudgetStop && !b.ResumeAt.IsZero() {
				t.Errorf("ResumeAt = %v for %s", b.ResumeAt, tc.want)
			}
			// Once the window resets, nothing is known and polling is normal.
			if b := r.budget(reset); b.Known || b.Verdict != BudgetNormal {
				t.Errorf("after reset: %+v", b)
			}
		})
	}
}

// Consecutive responses can alternate between two windows. The budget
// follows the lowest remaining among the windows that haven't reset.
func TestBudgetAlternatingWindows(t *testing.T) {
	a := time.Date(2026, 11, 6, 5, 37, 12, 0, time.UTC)
	b := time.Date(2026, 11, 6, 6, 7, 31, 0, time.UTC)
	var r rateTracker
	for i, rl := range []RateLimit{
		{Cost: 1, Remaining: 4976, ResetAt: a},
		{Cost: 1, Remaining: 4992, ResetAt: b},
		{Cost: 1, Remaining: 4938, ResetAt: a},
		{Cost: 1, Remaining: 4981, ResetAt: b},
		{Cost: 1, Remaining: 4950, ResetAt: a}, // out of order: lower one stands
	} {
		r.observe(rl, t0.Add(time.Duration(i)*time.Second))
	}
	if got := r.budget(t0.Add(time.Minute)); got.Remaining != 4938 || !got.ResetAt.Equal(a) {
		t.Errorf("budget = %+v, want 4938 resetting at %v", got, a)
	}
	// Window a resets: b's lowest is what's left.
	if got := r.budget(a.Add(time.Second)); got.Remaining != 4981 || !got.ResetAt.Equal(b) {
		t.Errorf("after a resets: %+v, want 4981 resetting at %v", got, b)
	}
	if got := r.budget(b); got.Known {
		t.Errorf("after both reset: %+v", got)
	}
}

// One low window is enough to stop, however healthy the other looks.
func TestBudgetLowWindowWins(t *testing.T) {
	var r rateTracker
	r.observe(RateLimit{Remaining: 4900, ResetAt: t0.Add(50 * time.Minute)}, t0)
	r.observe(RateLimit{Remaining: 150, ResetAt: t0.Add(10 * time.Minute)}, t0)
	r.observe(RateLimit{Remaining: 4899, ResetAt: t0.Add(50 * time.Minute)}, t0)
	if b := r.budget(t0); b.Verdict != BudgetStop || b.Remaining != 150 || !b.ResumeAt.Equal(t0.Add(10*time.Minute)) {
		t.Errorf("budget = %+v; want stop until the low window resets", b)
	}
}

func TestBudgetBoundedWindows(t *testing.T) {
	var r rateTracker
	for i := range 20 {
		r.observe(RateLimit{Remaining: 3000 + i, ResetAt: t0.Add(time.Duration(i+1) * time.Minute)}, t0)
	}
	if len(r.windows) > maxWindows {
		t.Errorf("%d windows kept, want at most %d", len(r.windows), maxWindows)
	}
}

// The verdict comes from what the client actually reads, on every call.
func TestBudgetFromResponses(t *testing.T) {
	f := newFakeGitHub(t)
	clk := &clock{now: t0}
	c := f.client(nil, clk, Options{})
	reset := t0.Add(40 * time.Minute)
	for _, tc := range []struct {
		remaining int
		want      Verdict
	}{
		{4500, BudgetNormal},
		{950, BudgetIdleOnly},
		{120, BudgetStop},
	} {
		f.setRate(RateLimit{Cost: 1, Remaining: tc.remaining, Limit: 5000, ResetAt: reset})
		c.BranchPRs(context.Background(), pivotV1)
		if b := c.Budget(); b.Verdict != tc.want || b.Remaining != tc.remaining || b.LastCost != 1 {
			t.Errorf("after remaining %d: %+v, want %s", tc.remaining, b, tc.want)
		}
	}
	clk.Advance(41 * time.Minute)
	if b := c.Budget(); b.Verdict != BudgetNormal {
		t.Errorf("after the reset: %+v", b)
	}

	// The search reads it too.
	f.search = fixtureSearch(t, "open_prs_mine_first_read.json")
	c.OpenPRs(context.Background(), threeOwners)
	if b := c.Budget(); b.Remaining != 4982 || !b.ResetAt.Equal(time.Date(2026, 11, 6, 6, 7, 31, 0, time.UTC)) {
		t.Errorf("after the search: %+v; want the fixture's 4982", b)
	}
}

// Rate limited: GitHub refuses with 403 and X-RateLimit-Remaining: 0, or
// asks for a backoff with Retry-After. Either way: stale, and stop.
func TestRateLimitedResponses(t *testing.T) {
	reset := t0.Add(25 * time.Minute)
	for _, tc := range []struct {
		name     string
		status   int
		header   map[string]string
		body     string
		resumeAt time.Time
	}{
		{"primary 403", http.StatusForbidden, map[string]string{
			"X-RateLimit-Remaining": "0", "X-RateLimit-Limit": "5000", "X-RateLimit-Reset": fmt.Sprint(reset.Unix()), "X-RateLimit-Resource": "graphql",
		}, `{"message":"API rate limit exceeded"}`, reset},
		{"primary 200 RATE_LIMITED", http.StatusOK, map[string]string{
			"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": fmt.Sprint(reset.Unix()),
		}, `{"data":null,"errors":[{"type":"RATE_LIMITED","message":"API rate limit exceeded for user ID 1."}]}`, reset},
		{"secondary", http.StatusForbidden, map[string]string{"Retry-After": "90"},
			`{"message":"You have exceeded a secondary rate limit."}`, t0.Add(90 * time.Second)},
		{"429", http.StatusTooManyRequests, map[string]string{"Retry-After": "30"}, `{}`, t0.Add(30 * time.Second)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			f.handle = func(w http.ResponseWriter, _ fakeRequest) bool {
				for k, v := range tc.header {
					w.Header().Set(k, v)
				}
				rawJSON(w, tc.status, []byte(tc.body))
				return true
			}
			clk := &clock{now: t0}
			c := f.client(nil, clk, Options{})
			r := c.BranchPRs(context.Background(), pivotV1).Repos[0]
			if !errors.Is(r.Err, ErrRateLimited) || !r.StaleSince.Equal(t0) {
				t.Errorf("Err %v, StaleSince %v; want ErrRateLimited, stale", r.Err, r.StaleSince)
			}
			b := c.Budget()
			if b.Verdict != BudgetStop || !b.ResumeAt.Equal(tc.resumeAt) {
				t.Errorf("Budget() = %+v; want stop until %v", b, tc.resumeAt)
			}
			clk.now = tc.resumeAt
			if b := c.Budget(); b.Verdict == BudgetStop {
				t.Errorf("still stopped at ResumeAt: %+v", b)
			}
		})
	}
}

func TestHeaderRateLimit(t *testing.T) {
	h := http.Header{}
	if _, ok := headerRateLimit(h); ok {
		t.Error("empty headers parsed")
	}
	h.Set("X-RateLimit-Remaining", "4321")
	h.Set("X-RateLimit-Reset", "1793943432")
	h.Set("X-RateLimit-Limit", "5000")
	h.Set("X-RateLimit-Used", "679")
	rl, ok := headerRateLimit(h)
	if !ok || rl.Remaining != 4321 || rl.Limit != 5000 || rl.Used != 679 || rl.ResetAt.Unix() != 1793943432 {
		t.Errorf("headerRateLimit = %+v, %v", rl, ok)
	}
	// Another resource's counters (core, search) say nothing about GraphQL.
	h.Set("X-RateLimit-Resource", "core")
	if _, ok := headerRateLimit(h); ok {
		t.Error("core resource headers parsed as GraphQL")
	}
}
