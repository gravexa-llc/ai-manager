package sessions

// Expired background jobs. Claude Code deletes a session's
// transcript (<ClaudeDir>/projects/<project>/<session id>.jsonl) once it is
// older than cleanupPeriodDays (settings.json, 30 by default), but a
// background job's <ClaudeDir>/jobs/<id>/state.json stays, so `claude
// agents --json` keeps listing the job, often as blocked, waiting for a
// reply. Its conversation is gone: `claude attach` would open a blank tab.
// The dashboard marks such a job expired and shows `claude rm <id>`; aimd
// never deletes it.
//
// The files are Claude Code's internals, not a public API, so Jobs
// reads them defensively: anything it can't read or doesn't understand
// leaves the job not expired, and the dashboard shows it as before.

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// DefaultCleanupDays is Claude Code's cleanupPeriodDays when settings.json
// doesn't set it.
const DefaultCleanupDays = 30

// jobsCacheTTL is how long Jobs keeps an answer: a job expires at most
// once, and the dashboard asks on every render.
const jobsCacheTTL = 2 * time.Minute

// JobExpiry is what Jobs found for one background job.
type JobExpiry struct {
	// Expired is true when the job's conversation is gone: no transcript
	// for any of its session ids is left under projects/, and the job was
	// last updated more than Days ago.
	Expired bool
	// Days is cleanupPeriodDays, the age after which Claude Code deletes a
	// transcript.
	Days int
	// UpdatedAt is the job's updatedAt (state.json's modification time
	// when it has none).
	UpdatedAt time.Time
}

// Text says why the job is expired, for the dashboard: "expired: its
// conversation was deleted after 30 days". It is "" when it isn't.
func (e JobExpiry) Text() string {
	switch {
	case !e.Expired:
		return ""
	case e.Days == 0:
		return "expired: its conversation was deleted (cleanupPeriodDays is 0)"
	case e.Days == 1:
		return "expired: its conversation was deleted after 1 day"
	}
	return "expired: its conversation was deleted after " + strconv.Itoa(e.Days) + " days"
}

// Jobs tells which background jobs have expired, reading Claude Code's
// config directory (ClaudeDir). It caches each answer for a couple of
// minutes. The zero value is not usable; use NewJobs.
type Jobs struct {
	dir string
	now func() time.Time
	ttl time.Duration

	mu    sync.Mutex
	cache map[string]cachedExpiry
}

type cachedExpiry struct {
	at time.Time
	e  JobExpiry
}

// NewJobs returns a Jobs reading dir, Claude Code's config directory. now
// is the clock (time.Now if nil).
func NewJobs(dir string, now func() time.Time) *Jobs {
	if now == nil {
		now = time.Now
	}
	return &Jobs{dir: dir, now: now, ttl: jobsCacheTTL, cache: map[string]cachedExpiry{}}
}

// idRE is what a job id or a session id must look like before it goes into
// a path or a glob: Claude Code's are 8 hex characters and UUIDs.
var idRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

// Expiry says whether the background job jobID has expired. sessionIDs
// are more ids its conversation may be under (the session row's), besides
// the sessionId and resumeSessionId in its state.json.
func (j *Jobs) Expiry(jobID string, sessionIDs ...string) JobExpiry {
	if j == nil || !idRE.MatchString(jobID) {
		return JobExpiry{}
	}
	key := jobID
	for _, id := range sessionIDs {
		key += "\x00" + id
	}
	now := j.now()
	j.mu.Lock()
	c, ok := j.cache[key]
	j.mu.Unlock()
	if ok && now.Sub(c.at) < j.ttl && now.Sub(c.at) >= 0 {
		return c.e
	}
	e := j.check(now, jobID, sessionIDs)
	j.mu.Lock()
	for k, old := range j.cache { // drop stale answers, so the map stays small
		if now.Sub(old.at) >= j.ttl {
			delete(j.cache, k)
		}
	}
	j.cache[key] = cachedExpiry{at: now, e: e}
	j.mu.Unlock()
	return e
}

// jobState is the part of a job's state.json Expiry reads.
type jobState struct {
	SessionID       string          `json:"sessionId"`
	ResumeSessionID string          `json:"resumeSessionId"`
	UpdatedAt       json.RawMessage `json:"updatedAt"`
}

// check reads the files. Any error leaves the job not expired.
func (j *Jobs) check(now time.Time, jobID string, extra []string) JobExpiry {
	e := JobExpiry{Days: j.cleanupDays()}
	path := filepath.Join(j.dir, "jobs", jobID, "state.json")
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return e
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return e
	}
	var st jobState
	if json.Unmarshal(b, &st) != nil {
		return e
	}
	e.UpdatedAt = parseTime(st.UpdatedAt)
	if e.UpdatedAt.IsZero() {
		e.UpdatedAt = info.ModTime()
	}
	if now.Sub(e.UpdatedAt) < time.Duration(e.Days)*24*time.Hour {
		return e // too recent for the cleanup to have deleted it
	}
	var ids []string
	for _, id := range append([]string{st.SessionID, st.ResumeSessionID}, extra...) {
		if idRE.MatchString(id) {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return e // nothing to look for
	}
	found, err := j.hasTranscript(ids)
	if err != nil || found {
		return e
	}
	e.Expired = true
	return e
}

// hasTranscript reports whether a transcript for one of ids is left under
// projects/, in any project folder. A projects folder that doesn't exist
// holds none; one that can't be read is an error.
func (j *Jobs) hasTranscript(ids []string) (bool, error) {
	projects := filepath.Join(j.dir, "projects")
	entries, err := os.ReadDir(projects)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	}
	for _, d := range entries {
		if !d.IsDir() {
			continue
		}
		for _, id := range ids {
			if _, err := os.Lstat(filepath.Join(projects, d.Name(), id+".jsonl")); err == nil {
				return true, nil
			} else if !errors.Is(err, fs.ErrNotExist) {
				return false, err
			}
		}
	}
	return false, nil
}

// cleanupDays reads cleanupPeriodDays from settings.json:
// DefaultCleanupDays when it is missing, unreadable or not a whole number
// of days, 0 or more.
func (j *Jobs) cleanupDays() int {
	b, err := os.ReadFile(filepath.Join(j.dir, "settings.json"))
	if err != nil {
		return DefaultCleanupDays
	}
	var s struct {
		CleanupPeriodDays *json.Number `json:"cleanupPeriodDays"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if dec.Decode(&s) != nil || s.CleanupPeriodDays == nil {
		return DefaultCleanupDays
	}
	n, err := strconv.Atoi(s.CleanupPeriodDays.String())
	if err != nil || n < 0 {
		return DefaultCleanupDays
	}
	return n
}

// parseTime reads a time Claude Code wrote: epoch milliseconds (as
// claude agents --json's startedAt), epoch seconds, or an RFC 3339
// string. It returns the zero time for anything else.
func parseTime(raw json.RawMessage) time.Time {
	if len(raw) == 0 {
		return time.Time{}
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return time.Time{}
		}
		return t
	}
	var n float64
	if json.Unmarshal(raw, &n) != nil || n <= 0 {
		return time.Time{}
	}
	if n >= 1e12 { // milliseconds
		return time.UnixMilli(int64(n))
	}
	return time.Unix(int64(n), 0)
}
