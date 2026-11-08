package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

func TestStatusLine(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		desc string
		st   api.Status
		want string
	}{
		{
			"all quiet, budget unknown",
			api.Status{Now: now, Counts: api.Counts{StaleSources: []string{}}},
			"0 sessions need you · 0 PRs need you · stale: none · GitHub budget unknown",
		},
		{
			"things need you",
			api.Status{
				Now: now,
				Counts: api.Counts{
					SessionsNeedingYou: 2, OldestNeedsYouSince: now.Add(-12*time.Minute - 30*time.Second),
					PRsNeedingYou: 1, StaleSources: []string{"github", "sessions"},
				},
				GitHub: api.GitHubStatus{Budget: api.Budget{Verdict: "normal", Known: true, Remaining: 4890, Limit: 5000}},
			},
			"2 sessions need you (oldest 12m) · 1 PR needs you · stale: github, sessions · GitHub 4890/5000",
		},
		{
			"one session, waiting for days; idle-only",
			api.Status{
				Now:    now,
				Counts: api.Counts{SessionsNeedingYou: 1, OldestNeedsYouSince: now.Add(-100 * time.Hour), PRsNeedingYou: 3},
				GitHub: api.GitHubStatus{Budget: api.Budget{Verdict: "idle-only", Known: true, Remaining: 900, Limit: 5000}},
			},
			"1 session needs you (oldest 4d) · 3 PRs need you · stale: none · GitHub 900/5000 idle-only",
		},
		{
			"stopped",
			api.Status{
				Now:    now,
				GitHub: api.GitHubStatus{Budget: api.Budget{Verdict: "stop", Known: true, Remaining: 150, Limit: 5000}},
			},
			"0 sessions need you · 0 PRs need you · stale: none · GitHub 150/5000 stopped",
		},
	} {
		if got := statusLine(tc.st); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.desc, got, tc.want)
		}
	}
}

func TestShortAge(t *testing.T) {
	for d, want := range map[time.Duration]string{
		-time.Second:     "0s",
		45 * time.Second: "45s",
		90 * time.Second: "1m",
		3 * time.Hour:    "3h",
		47 * time.Hour:   "47h",
		72 * time.Hour:   "3d",
	} {
		if got := shortAge(d); got != want {
			t.Errorf("shortAge(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestStatusCommand(t *testing.T) {
	_, userHome := setHome(t)
	body, _ := json.Marshal(api.Status{
		Now:    time.Now(),
		Counts: api.Counts{PRsNeedingYou: 2},
		GitHub: api.GitHubStatus{Budget: api.Budget{Verdict: "normal", Known: true, Remaining: 10, Limit: 5000}},
	})
	f := &fakeSys{status: body, statusOK: true}
	s := f.sys(t, userHome, testTools)
	var stdout, stderr bytes.Buffer
	if code := status(s, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("status = %d: %s", code, stderr.String())
	}
	if got := stdout.String(); got != "0 sessions need you · 2 PRs need you · stale: none · GitHub 10/5000\n" {
		t.Errorf("status printed %q", got)
	}

	f.statusOK = false
	stdout.Reset()
	if code := status(s, nil, &stdout, &stderr); code != 1 || !strings.Contains(stdout.String(), "aimd not running (127.0.0.1:7777)") {
		t.Errorf("status with aimd down = %d, %q", code, stdout.String())
	}
	if code := status(s, []string{"x"}, &stdout, &stderr); code != 2 {
		t.Errorf("status x = %d", code)
	}
}
