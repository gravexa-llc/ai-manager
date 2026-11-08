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
			api.Status{Version: "v1.0.0-3-gabc1234", Now: now, Counts: api.Counts{StaleSources: []string{}}},
			"0 sessions need you · 0 PRs need you · stale: none · GitHub budget unknown · aimd v1.0.0-3-gabc1234",
		},
		{
			"things need you",
			api.Status{
				Version: "v1.0.0-3-gabc1234",
				Now:     now,
				Counts: api.Counts{
					SessionsNeedingYou: 2, OldestNeedsYouSince: now.Add(-12*time.Minute - 30*time.Second),
					PRsNeedingYou: 1, StaleSources: []string{"github", "sessions"},
				},
				GitHub: api.GitHubStatus{Budget: api.Budget{Verdict: "normal", Known: true, Remaining: 4890, Limit: 5000}},
			},
			"2 sessions need you (oldest 12m) · 1 PR needs you · stale: github, sessions · GitHub 4890/5000 · aimd v1.0.0-3-gabc1234",
		},
		{
			"one session, waiting for days; idle-only",
			api.Status{
				Version: "v1.0.0-3-gabc1234",
				Now:     now,
				Counts:  api.Counts{SessionsNeedingYou: 1, OldestNeedsYouSince: now.Add(-100 * time.Hour), PRsNeedingYou: 3},
				GitHub:  api.GitHubStatus{Budget: api.Budget{Verdict: "idle-only", Known: true, Remaining: 900, Limit: 5000}},
			},
			"1 session needs you (oldest 4d) · 3 PRs need you · stale: none · GitHub 900/5000 idle-only · aimd v1.0.0-3-gabc1234",
		},
		{
			"stopped; an aimd that doesn't say its version",
			api.Status{
				Now:    now,
				GitHub: api.GitHubStatus{Budget: api.Budget{Verdict: "stop", Known: true, Remaining: 150, Limit: 5000}},
			},
			"0 sessions need you · 0 PRs need you · stale: none · GitHub 150/5000 stopped · aimd version unknown",
		},
		{
			"a repo GitHub can't read",
			api.Status{
				Version: "v1.0.0-3-gabc1234",
				Now:     now,
				GitHub: api.GitHubStatus{Budget: api.Budget{Verdict: "normal", Known: true, Remaining: 4890, Limit: 5000},
					Unreadable: []api.RepoError{{Repo: "acme/app", Error: "github: not found"}}},
			},
			"0 sessions need you · 0 PRs need you · stale: none · GitHub can't read acme/app · GitHub 4890/5000 · aimd v1.0.0-3-gabc1234",
		},
		{
			"two repos GitHub can't read",
			api.Status{
				Version: "v1.0.0-3-gabc1234",
				Now:     now,
				GitHub: api.GitHubStatus{Budget: api.Budget{Verdict: "normal", Known: true, Remaining: 4890, Limit: 5000},
					Unreadable: []api.RepoError{{Repo: "acme/app"}, {Repo: "example-org/web"}}},
			},
			"0 sessions need you · 0 PRs need you · stale: none · GitHub can't read 2 repos · GitHub 4890/5000 · aimd v1.0.0-3-gabc1234",
		},
	} {
		if got := statusLine(tc.st, "v1.0.0-3-gabc1234"); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.desc, got, tc.want)
		}
	}
}

func TestVersionPart(t *testing.T) {
	for _, tc := range []struct{ running, aim, want string }{
		{"v1.0.0-3-gabc1234", "v1.0.0-3-gabc1234", "aimd v1.0.0-3-gabc1234"},
		{"v1.0.0-3-gabc1234", "v1.0.0-5-gdef5678", "aimd v1.0.0-3-gabc1234 is running; aim is v1.0.0-5-gdef5678; run aim up to restart it"},
		{"v1.0.0-3-gabc1234-dirty", "v1.0.0-3-gabc1234", "aimd v1.0.0-3-gabc1234-dirty is running; aim is v1.0.0-3-gabc1234; run aim up to restart it"},
		// A dev build on either side is never compared.
		{"dev", "v1.0.0-5-gdef5678", "aimd dev"},
		{"v1.0.0-3-gabc1234", "dev", "aimd v1.0.0-3-gabc1234"},
		{"", "v1.0.0-5-gdef5678", "aimd version unknown"},
	} {
		if got := versionPart(tc.running, tc.aim); got != tc.want {
			t.Errorf("versionPart(%q, %q) = %q, want %q", tc.running, tc.aim, got, tc.want)
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
	st := api.Status{
		Version: "v1.0.0-3-gabc1234",
		Now:     time.Now(),
		Counts:  api.Counts{PRsNeedingYou: 2},
		GitHub:  api.GitHubStatus{Budget: api.Budget{Verdict: "normal", Known: true, Remaining: 10, Limit: 5000}},
	}
	body, _ := json.Marshal(st)
	f := &fakeSys{status: body, statusOK: true}
	s := f.sys(t, userHome, testTools)
	was := version
	t.Cleanup(func() { version = was })

	// The aimd running is this aim's build: its version, one line.
	version = "v1.0.0-3-gabc1234"
	var stdout, stderr bytes.Buffer
	if code := status(s, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("status = %d: %s", code, stderr.String())
	}
	if got := stdout.String(); got != "0 sessions need you · 2 PRs need you · stale: none · GitHub 10/5000 · aimd v1.0.0-3-gabc1234\n" {
		t.Errorf("status printed %q", got)
	}

	// aim was updated, and the old aimd still runs: one line still, saying so.
	version = "v1.0.0-5-gdef5678"
	stdout.Reset()
	if code := status(s, nil, &stdout, &stderr); code != 0 {
		t.Fatalf("status = %d: %s", code, stderr.String())
	}
	want := "0 sessions need you · 2 PRs need you · stale: none · GitHub 10/5000 · aimd v1.0.0-3-gabc1234 is running; aim is v1.0.0-5-gdef5678; run aim up to restart it\n"
	if got := stdout.String(); got != want {
		t.Errorf("status with another aimd printed %q, want %q", got, want)
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
