package sessions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// expiredNow is the tests' clock.
var expiredNow = time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

// claudeDirFixture is a fake Claude Code config directory: jobs, their
// state.json, transcripts under projects/, and settings.json.
type claudeDirFixture struct {
	t   *testing.T
	dir string
}

func newClaudeDir(t *testing.T) *claudeDirFixture {
	t.Helper()
	return &claudeDirFixture{t: t, dir: t.TempDir()}
}

func (f *claudeDirFixture) write(rel, content string) {
	f.t.Helper()
	p := filepath.Join(f.dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// job writes jobs/<id>/state.json with the given fields.
func (f *claudeDirFixture) job(id string, fields map[string]any) {
	f.t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		f.t.Fatal(err)
	}
	f.write(filepath.Join("jobs", id, "state.json"), string(b))
}

func (f *claudeDirFixture) transcript(project, sessionID string) {
	f.write(filepath.Join("projects", project, sessionID+".jsonl"), "{}\n")
}

func TestJobsExpiry(t *testing.T) {
	old := expiredNow.Add(-45 * 24 * time.Hour)
	recent := expiredNow.Add(-3 * 24 * time.Hour)
	for _, c := range []struct {
		name     string
		setup    func(f *claudeDirFixture)
		job      string
		extra    []string
		expired  bool
		days     int
		text     string
		updated  time.Time
		noUpdate bool // UpdatedAt comes from the file's mtime
	}{
		{
			name: "old job, transcript deleted",
			setup: func(f *claudeDirFixture) {
				f.job("022c44bf", map[string]any{"sessionId": "022c44bf-c611", "updatedAt": old.UnixMilli(), "state": "blocked"})
				f.transcript("-Users-x-Projects-other", "someone-else")
			},
			job: "022c44bf", expired: true, days: 30, updated: old,
			text: "expired: its conversation was deleted after 30 days",
		},
		{
			name: "old job, transcript kept",
			setup: func(f *claudeDirFixture) {
				f.job("47bedaa8", map[string]any{"sessionId": "47bedaa8-c526", "updatedAt": old.UnixMilli()})
				f.transcript("-Users-x-Projects-acme-www", "47bedaa8-c526")
			},
			job: "47bedaa8", days: 30, updated: old,
		},
		{
			name: "respawned job: the conversation is under resumeSessionId",
			setup: func(f *claudeDirFixture) {
				f.job("199dc94c", map[string]any{"sessionId": "1adba5b1-new", "resumeSessionId": "199dc94c-orig", "updatedAt": old.UnixMilli()})
				f.transcript("-Users-x-Projects-a", "199dc94c-orig")
			},
			job: "199dc94c", days: 30, updated: old,
		},
		{
			name: "the session row's id finds the transcript",
			setup: func(f *claudeDirFixture) {
				f.job("aa11bb22", map[string]any{"sessionId": "gone-1", "updatedAt": old.UnixMilli()})
				f.transcript("-p", "row-id-1")
			},
			job: "aa11bb22", extra: []string{"row-id-1"}, days: 30, updated: old,
		},
		{
			name: "recent job without a transcript yet",
			setup: func(f *claudeDirFixture) {
				f.job("cc33dd44", map[string]any{"sessionId": "new-1", "updatedAt": recent.UnixMilli()})
			},
			job: "cc33dd44", days: 30, updated: recent,
		},
		{
			name: "cleanupPeriodDays from settings.json",
			setup: func(f *claudeDirFixture) {
				f.write("settings.json", `{"model": "x", "cleanupPeriodDays": 2}`)
				f.job("cc33dd44", map[string]any{"sessionId": "new-1", "updatedAt": recent.UnixMilli()})
			},
			job: "cc33dd44", expired: true, days: 2, updated: recent,
			text: "expired: its conversation was deleted after 2 days",
		},
		{
			name: "cleanupPeriodDays 0",
			setup: func(f *claudeDirFixture) {
				f.write("settings.json", `{"cleanupPeriodDays": 0}`)
				f.job("cc33dd44", map[string]any{"sessionId": "new-1", "updatedAt": expiredNow.Add(-time.Minute).Format(time.RFC3339)})
			},
			job: "cc33dd44", expired: true, days: 0, updated: expiredNow.Add(-time.Minute),
			text: "expired: its conversation was deleted (cleanupPeriodDays is 0)",
		},
		{
			name: "a bad cleanupPeriodDays is the default",
			setup: func(f *claudeDirFixture) {
				f.write("settings.json", `{"cleanupPeriodDays": -4}`)
				f.job("cc33dd44", map[string]any{"sessionId": "new-1", "updatedAt": recent.UnixMilli()})
			},
			job: "cc33dd44", days: 30, updated: recent,
		},
		{
			name: "unreadable settings.json is the default",
			setup: func(f *claudeDirFixture) {
				f.write("settings.json", `{not json`)
				f.job("ee55ff66", map[string]any{"sessionId": "s-1", "updatedAt": old.UnixMilli()})
			},
			job: "ee55ff66", expired: true, days: 30, updated: old,
			text: "expired: its conversation was deleted after 30 days",
		},
		{
			name: "updatedAt as an RFC 3339 string, no projects folder",
			setup: func(f *claudeDirFixture) {
				f.job("ab12cd34", map[string]any{"sessionId": "s-2", "updatedAt": old.Format(time.RFC3339Nano)})
			},
			job: "ab12cd34", expired: true, days: 30, updated: old,
			text: "expired: its conversation was deleted after 30 days",
		},
		{
			// The format Claude Code writes on the lead's Mac: RFC 3339 in
			// UTC with milliseconds.
			name: "updatedAt as Claude Code writes it, RFC 3339 with milliseconds",
			setup: func(f *claudeDirFixture) {
				f.job("ab12cd36", map[string]any{"sessionId": "s-4", "updatedAt": "2026-09-20T16:32:26.047Z"})
				f.transcript("-p", "someone-else")
			},
			job: "ab12cd36", expired: true, days: 30, updated: time.Date(2026, 9, 20, 16, 32, 26, 47e6, time.UTC),
			text: "expired: its conversation was deleted after 30 days",
		},
		{
			name: "a recent job in that format isn't expired",
			setup: func(f *claudeDirFixture) {
				f.job("ab12cd37", map[string]any{"sessionId": "s-5", "updatedAt": "2026-11-06T11:32:26.047Z"})
			},
			job: "ab12cd37", days: 30, updated: time.Date(2026, 11, 6, 11, 32, 26, 47e6, time.UTC),
		},
		{
			name: "no updatedAt: the file's age",
			setup: func(f *claudeDirFixture) {
				f.job("ab12cd35", map[string]any{"sessionId": "s-3"})
				p := filepath.Join(f.dir, "jobs", "ab12cd35", "state.json")
				if err := os.Chtimes(p, old, old); err != nil {
					f.t.Fatal(err)
				}
			},
			job: "ab12cd35", expired: true, days: 30, updated: old, noUpdate: true,
			text: "expired: its conversation was deleted after 30 days",
		},
		{
			name:  "no state.json",
			setup: func(f *claudeDirFixture) {},
			job:   "deadbeef", days: 30,
		},
		{
			name: "state.json isn't JSON",
			setup: func(f *claudeDirFixture) {
				f.write(filepath.Join("jobs", "deadbee0", "state.json"), "{")
			},
			job: "deadbee0", days: 30,
		},
		{
			name: "no session id anywhere",
			setup: func(f *claudeDirFixture) {
				f.job("deadbee1", map[string]any{"updatedAt": old.UnixMilli()})
			},
			job: "deadbee1", days: 30, updated: old,
		},
		{
			name: "a job id that isn't one is never looked up",
			setup: func(f *claudeDirFixture) {
				f.job("x", map[string]any{"sessionId": "s", "updatedAt": old.UnixMilli()})
			},
			job: "../jobs/x",
		},
		{
			name: "a session id that isn't one is ignored",
			setup: func(f *claudeDirFixture) {
				f.job("deadbee2", map[string]any{"sessionId": "*", "updatedAt": old.UnixMilli()})
				f.transcript("-p", "anything")
			},
			job: "deadbee2", days: 30, updated: old,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newClaudeDir(t)
			c.setup(f)
			got := NewJobs(f.dir, func() time.Time { return expiredNow }).Expiry(c.job, c.extra...)
			if got.Expired != c.expired || got.Days != c.days || got.Text() != c.text {
				t.Errorf("Expiry = %+v (%q), want expired %v, days %d, %q", got, got.Text(), c.expired, c.days, c.text)
			}
			if !c.updated.IsZero() && !got.UpdatedAt.Equal(c.updated) {
				t.Errorf("UpdatedAt = %v, want %v", got.UpdatedAt, c.updated)
			}
		})
	}
}

func TestJobsExpiryCaches(t *testing.T) {
	f := newClaudeDir(t)
	f.job("022c44bf", map[string]any{"sessionId": "s-1", "updatedAt": expiredNow.Add(-40 * 24 * time.Hour).UnixMilli()})
	now := expiredNow
	j := NewJobs(f.dir, func() time.Time { return now })
	if !j.Expiry("022c44bf").Expired {
		t.Fatal("not expired at first")
	}
	// The transcript turns up (it was elsewhere for a moment): the answer
	// holds until the cache runs out.
	f.transcript("-p", "s-1")
	if !j.Expiry("022c44bf").Expired {
		t.Error("the cached answer changed before its time")
	}
	now = now.Add(jobsCacheTTL)
	if j.Expiry("022c44bf").Expired {
		t.Error("still expired after the cache ran out")
	}
	// A nil Jobs knows of no expired job.
	var none *Jobs
	if none.Expiry("022c44bf").Expired {
		t.Error("a nil Jobs says expired")
	}
}

func TestParseTime(t *testing.T) {
	at := time.Date(2026, 7, 2, 10, 0, 0, 0, time.UTC)
	for raw, want := range map[string]time.Time{
		`1782986400000`:          at,
		`1782986400`:             at,
		`"2026-07-02T10:00:00Z"`: at,
		// As Claude Code writes it (seen on the lead's Mac).
		`"2026-11-06T16:32:26.047Z"`: time.Date(2026, 11, 6, 16, 32, 26, 47e6, time.UTC),
		`"July"`:                     {},
		`null`:                       {},
		`-5`:                         {},
		`{}`:                         {},
	} {
		if got := parseTime(json.RawMessage(raw)); !got.Equal(want) {
			t.Errorf("parseTime(%s) = %v, want %v", raw, got, want)
		}
	}
}
