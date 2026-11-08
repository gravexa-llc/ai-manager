package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/config"
	"github.com/gravexa-llc/ai-manager/internal/github"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
	"github.com/gravexa-llc/ai-manager/internal/worktrees"
)

// These tests run in a synctest bubble: time is fake and only moves when
// every goroutine is blocked, so the schedules are checked to the
// millisecond. The lister, scanner, GitHub client, store and repo watcher
// are fakes (fakes_test.go).

const sec = time.Second

func TestSessionIntervalSwitching(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, nil)
		base := time.Now()
		r.start(t)

		time.Sleep(65 * sec) // unwatched: 30s
		if got := r.d.SessionInterval(); got != 30*sec {
			t.Errorf("unwatched interval = %s, want 30s", got)
		}
		r.d.SetWatchers(1) // a dashboard connects: 10s, due 10s after the last poll
		if got := r.d.SessionInterval(); got != 10*sec {
			t.Errorf("watched interval = %s, want 10s", got)
		}
		time.Sleep(30 * sec) // t=95
		r.d.SetWatchers(2)
		r.d.SetWatchers(0)   // it leaves: back to 30s
		time.Sleep(60 * sec) // t=155
		r.d.SetWatchers(3)
		r.d.SetModLive(true) // the mod reports live: 60s even while watched
		if got := r.d.SessionInterval(); got != 60*sec {
			t.Errorf("mod-live interval = %s, want 60s", got)
		}
		time.Sleep(70 * sec) // t=225
		synctest.Wait()

		got := offsets(base, r.runner.callTimes())
		want := []time.Duration{0, 30 * sec, 60 * sec, 70 * sec, 80 * sec, 90 * sec, 120 * sec, 150 * sec, 210 * sec}
		if !slices.Equal(got, want) {
			t.Errorf("polls at %v, want %v", got, want)
		}
		if st := r.d.Status(); st.Sources[0].IntervalSeconds != 60 || st.Watchers != 3 || !st.ModLive {
			t.Errorf("status = %+v", st.Sources[0])
		}
	})
}

func TestSessionIntervalsFromConfig(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, func(c *config.Config) {
			c.Poll.Sessions = 5 * sec
			c.Poll.SessionsUnwatched = 20 * sec
			c.Poll.SessionsModLive = 45 * sec
		})
		base := time.Now()
		r.start(t)
		time.Sleep(41 * sec)
		r.d.SetWatchers(1)
		time.Sleep(10 * sec) // t=51
		r.d.SetModLive(true)
		time.Sleep(50 * sec) // t=101
		synctest.Wait()
		got := offsets(base, r.runner.callTimes())
		want := []time.Duration{0, 20 * sec, 40 * sec, 45 * sec, 50 * sec, 95 * sec}
		if !slices.Equal(got, want) {
			t.Errorf("polls at %v, want %v", got, want)
		}
	})
}

func TestSessionKicks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, nil)
		base := time.Now()
		r.start(t)
		time.Sleep(5 * sec)
		// A burst of registry changes is one poll, 500ms later.
		r.kickSessions()
		r.kickSessions()
		r.kickSessions()
		time.Sleep(time.Second) // t=6
		// A kick right after a poll waits out the 2s gap from that poll.
		r.kickSessions()
		time.Sleep(4 * sec) // t=10
		synctest.Wait()
		got := offsets(base, r.runner.callTimes())
		want := []time.Duration{0, 5500 * time.Millisecond, 7500 * time.Millisecond}
		if !slices.Equal(got, want) {
			t.Errorf("polls at %v, want %v", got, want)
		}
	})
}

func TestSessionsHeadlessNeedsYouAndEnding(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, nil)
		base := time.Now()
		old := base.Add(-time.Hour).UnixMilli()
		// A row left live by an earlier aimd that the listing no longer has.
		r.st.sessions["pid:99"] = store.Session{Key: "pid:99", Kind: "interactive"}
		r.runner.set(
			entry{PID: 1, SessionID: "s1", Cwd: "/r/a", Kind: "interactive", StartedAt: old, Status: "idle"},
			entry{PID: 2, SessionID: "s2", Cwd: "/tmpdir/probe", Kind: "interactive", StartedAt: old, Status: "busy"},
			entry{ID: "j1", PID: 3, SessionID: "s3", Cwd: "/r/b", Kind: "background", StartedAt: base.UnixMilli(), Status: "idle", State: "blocked"},
			entry{PID: 4, SessionID: "s4", Cwd: "/r/a", Kind: "interactive", StartedAt: base.UnixMilli(), Status: "waiting", WaitingFor: "permission prompt"},
			entry{ID: "j0", Cwd: "/r/b", Kind: "background", StartedAt: old, State: "done"}, // finished before aimd started
		)
		r.start(t)
		synctest.Wait()

		live := r.st.liveSessions()
		if len(live) != 4 {
			t.Fatalf("live sessions = %v, want pid:1, pid:2, job:j1, pid:4", keys(live))
		}
		if _, ok := live["job:j0"]; ok {
			t.Error("a job that finished before aimd started was written")
		}
		check := func(key string, headless bool, state, reason string) {
			t.Helper()
			got := r.st.liveSessions()[key]
			if got.Headless != headless || got.State != state || got.StateReason != reason || got.Host != "testhost" {
				t.Errorf("%s = headless %v, %s (%s), host %q; want headless %v, %s (%s)",
					key, got.Headless, got.State, got.StateReason, got.Host, headless, state, reason)
			}
		}
		check("pid:1", false, "idle", "idle")
		check("pid:2", true, "working", "busy") // cwd under $TMPDIR
		check("job:j1", false, "needs_you", "reply")
		check("pid:4", true, "needs_you", "permission prompt") // seen once, just started
		if got := r.st.endedKeys(); len(got) != 1 || got["pid:99"].IsZero() {
			t.Errorf("ended = %v, want pid:99 (left over from an earlier run)", got)
		}
		if since := r.st.liveSessions()["pid:4"].NeedsYouSince; !since.Equal(base) {
			t.Errorf("pid:4 needs you since %v, want %v", since, base)
		}

		time.Sleep(30 * sec) // next poll: pid:4 has been listed for 30s
		synctest.Wait()
		check("pid:4", false, "needs_you", "permission prompt")
		check("pid:2", true, "working", "busy") // a temp dir stays headless
		if since := r.st.liveSessions()["pid:4"].NeedsYouSince; !since.Equal(base) {
			t.Errorf("needs-you start moved to %v, want %v", since, base)
		}

		// pid:4 exits and the job finishes (no process, state done).
		r.runner.set(
			entry{PID: 1, SessionID: "s1", Cwd: "/r/a", Kind: "interactive", StartedAt: old, Status: "idle"},
			entry{PID: 2, SessionID: "s2", Cwd: "/tmpdir/probe", Kind: "interactive", StartedAt: old, Status: "busy"},
			entry{ID: "j1", SessionID: "s3", Cwd: "/r/b", Kind: "background", StartedAt: base.UnixMilli(), State: "done"},
			entry{ID: "j0", Cwd: "/r/b", Kind: "background", StartedAt: old, State: "done"},
		)
		r.kickSessions() // polls 2s after the last poll
		time.Sleep(3 * sec)
		synctest.Wait()
		ended := r.st.endedKeys()
		for _, k := range []string{"pid:4", "job:j1"} {
			if ended[k].IsZero() {
				t.Errorf("%s not ended; ended = %v", k, ended)
			}
		}
		if live := r.st.liveSessions(); len(live) != 2 {
			t.Errorf("live after exits = %v, want pid:1 and pid:2", keys(live))
		}
	})
}

func TestSessionPollFailureEndsNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, nil)
		r.runner.set(entry{PID: 1, SessionID: "s1", Cwd: "/r/a", Kind: "interactive", Status: "idle"})
		r.start(t)
		synctest.Wait()
		r.runner.mu.Lock()
		r.runner.err = errors.New("executable file not found in $PATH")
		r.runner.mu.Unlock()
		time.Sleep(30 * sec)
		synctest.Wait()

		if len(r.st.endedKeys()) != 0 || len(r.st.liveSessions()) != 1 {
			t.Errorf("a failed poll changed sessions: live %v, ended %v", keys(r.st.liveSessions()), r.st.endedKeys())
		}
		src := r.d.Status().Sources[0]
		if !src.Stale || src.FailingSince.IsZero() || !strings.Contains(src.Error, "not found") {
			t.Errorf("sessions source = %+v, want stale with the error", src)
		}
		failingSince := src.FailingSince
		time.Sleep(30 * sec)
		synctest.Wait()
		if got := r.d.Status().Sources[0].FailingSince; !got.Equal(failingSince) {
			t.Errorf("failing_since moved from %v to %v", failingSince, got)
		}
		if r.notes.count(api.TopicStatus) == 0 {
			t.Error("going stale wasn't notified")
		}

		r.runner.mu.Lock()
		r.runner.err = nil
		r.runner.mu.Unlock()
		time.Sleep(30 * sec)
		synctest.Wait()
		if src := r.d.Status().Sources[0]; src.Stale || src.Error != "" || src.LastOK.IsZero() {
			t.Errorf("after recovery = %+v", src)
		}
	})
}

func TestWorktreeScheduling(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, nil)
		old := time.Now().Add(-24 * time.Hour)
		outside := repo("x", old, "main")
		outside.Path, outside.GitDir = "/elsewhere/x", "/elsewhere/x/.git"
		for i := range outside.Worktrees {
			outside.Worktrees[i].Path = "/elsewhere/x"
		}
		r.scanner.set(&worktrees.Result{Repos: []worktrees.Repo{repo("a", old, "main", "feat"), outside}})
		r.runner.set(
			entry{PID: 1, Cwd: "/r/a-feat", Kind: "interactive", StartedAt: old.UnixMilli(), Status: "busy"},
			entry{PID: 2, Cwd: "/elsewhere/x", Kind: "interactive", StartedAt: old.UnixMilli(), Status: "idle"},
			entry{PID: 3, Cwd: "/tmpdir/y", Kind: "interactive", StartedAt: old.UnixMilli(), Status: "busy"},
		)
		base := time.Now()
		r.start(t)
		synctest.Wait()

		scans, _ := r.scanner.calls()
		if len(scans) != 1 || scans[0].at != base {
			t.Fatalf("scans = %+v, want one at start", scans)
		}
		// Live non-headless cwds are passed in; the temp-dir one isn't.
		if got := scans[0].args; !slices.Equal(got, []string{"/r/a-feat", "/elsewhere/x"}) {
			t.Errorf("first scan extras = %q", got)
		}
		// The session rows know their worktree. The first poll ran before the
		// first scan; the next one (t=30) fills these in.
		time.Sleep(30 * sec)
		synctest.Wait()
		if s := r.st.liveSessions()["pid:1"]; s.WorktreePath != "/r/a-feat" || s.Repo != "/r/a" || s.Branch != "feat" {
			t.Errorf("pid:1 = worktree %q repo %q branch %q", s.WorktreePath, s.Repo, s.Branch)
		}
		if w := r.watcher.watched(); !slices.Equal(w, []string{"/r/a/.git", "/elsewhere/x/.git"}) {
			t.Errorf("watched = %q", w)
		}

		// A session in a new directory asks for a scan, held 15s after the
		// last one; the session outside the roots has gone, but its repo
		// stays tracked.
		r.runner.set(
			entry{PID: 1, Cwd: "/r/a-feat", Kind: "interactive", StartedAt: old.UnixMilli(), Status: "busy"},
			entry{PID: 5, Cwd: "/new/place", Kind: "interactive", StartedAt: old.UnixMilli(), Status: "idle"},
		)
		time.Sleep(40*sec - time.Since(base)) // t=40
		r.kickSessions()
		time.Sleep(10 * sec) // t=50
		synctest.Wait()
		scans, _ = r.scanner.calls()
		if len(scans) != 2 || scans[1].at.Sub(base).Round(100*time.Millisecond) != 40500*time.Millisecond {
			t.Fatalf("scans at %v, want a second at 40.5s", scanTimes(base, scans))
		}
		if got := scans[1].args; !slices.Equal(got, []string{"/r/a-feat", "/new/place", "/elsewhere/x"}) {
			t.Errorf("kicked scan extras = %q", got)
		}

		// A WatchPaths change rescans just that repo after 1s; events
		// during the 5s gap that follows make one more rescan.
		r.watcher.events <- "/r/a/.git"
		time.Sleep(1500 * time.Millisecond) // t=51.5
		r.watcher.events <- "/r/a/.git"
		r.watcher.events <- "/r/a/.git"
		r.watcher.events <- "/not/watched"
		time.Sleep(10 * sec) // t=61.5
		synctest.Wait()
		_, repoScans := r.scanner.calls()
		if got := scanTimes(base, repoScans); !slices.Equal(got, []time.Duration{51 * sec, 56 * sec}) {
			t.Errorf("repo rescans at %v, want 51s and 56s", got)
		}
		for _, c := range repoScans {
			if !slices.Equal(c.args, []string{"/r/a/.git"}) {
				t.Errorf("repo rescan of %q, want /r/a/.git", c.args)
			}
		}

		// The interval restarts after the kicked scan: next at 40.5 + 120.
		time.Sleep(200*sec - time.Since(base))
		synctest.Wait()
		scans, _ = r.scanner.calls()
		want := []time.Duration{0, 40500 * time.Millisecond, 160500 * time.Millisecond}
		if got := scanTimes(base, scans); !slices.Equal(got, want) {
			t.Errorf("full scans at %v, want %v", got, want)
		}
	})
}

func TestWorktreeRowsAndRemoval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, nil)
		old := time.Now().Add(-24 * time.Hour)
		a := repo("a", old, "main", "feat", "gone")
		a.Worktrees[2].StatusRead = false
		a.Worktrees[2].Missing = true
		a.Worktrees[1].Locked = true
		a.Worktrees[1].LockReason = "claude agent agent-abc (pid 42 start 2026-11-05T00:00:00Z)"
		a.Worktrees[1].Lock = &worktrees.ClaudeLock{AgentID: "abc", PID: 42}
		a.Worktrees[1].AgentID = "abc"
		r.scanner.set(&worktrees.Result{Repos: []worktrees.Repo{a, repo("b", old, "main")}})
		r.start(t)
		synctest.Wait()

		wts, _ := r.st.ListWorktrees(context.Background(), store.WorktreeFilter{})
		if len(wts) != 4 {
			t.Fatalf("worktrees = %d, want 4", len(wts))
		}
		got := map[string]store.Worktree{}
		for _, w := range wts {
			got[w.Path] = w
		}
		if w := got["/r/a-feat"]; !w.Locked || w.LockAgentID != "abc" || w.LockPID != 42 || w.AgentID != "abc" || w.MainRepo != "/r/a" {
			t.Errorf("locked worktree row = %+v", w)
		}
		if w := got["/r/a-gone"]; w.DirtyCount != unknownCount || w.UntrackedCount != unknownCount {
			t.Errorf("unread status stored as %d/%d, want unknown", w.DirtyCount, w.UntrackedCount)
		}
		if n := r.notes.count(api.TopicWorktrees); n != 1 {
			t.Errorf("worktree notifications after first scan = %d, want 1", n)
		}

		// Same result: no change event. Then one worktree goes and repo b
		// disappears entirely.
		time.Sleep(2*time.Minute + sec)
		synctest.Wait()
		if n := r.notes.count(api.TopicWorktrees); n != 1 {
			t.Errorf("worktree notifications after an identical scan = %d, want 1", n)
		}
		a.Worktrees = a.Worktrees[:2]
		r.scanner.set(&worktrees.Result{Repos: []worktrees.Repo{a}})
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		live, _ := r.st.ListWorktrees(context.Background(), store.WorktreeFilter{})
		var paths []string
		for _, w := range live {
			paths = append(paths, w.Path)
		}
		if !slices.Equal(paths, []string{"/r/a", "/r/a-feat"}) {
			t.Errorf("live worktrees = %q, want /r/a and /r/a-feat", paths)
		}
		if n := r.notes.count(api.TopicWorktrees); n != 2 {
			t.Errorf("worktree notifications = %d, want 2", n)
		}
	})
}

// ghRig sets up repos a (active: a live session in it) and b (idle) for
// the GitHub tests.
func ghRig(t *testing.T) *rig {
	r := newRig(t, nil)
	old := time.Now().Add(-24 * time.Hour)
	r.scanner.set(&worktrees.Result{Repos: []worktrees.Repo{repo("a", old, "main", "feat-a"), repo("b", old, "main", "feat-b")}})
	r.runner.set(entry{PID: 1, Cwd: "/r/a-feat-a", Kind: "interactive", StartedAt: old.UnixMilli(), Status: "busy"})
	r.gh.defaults = map[string]string{"o/a": "main", "o/b": "main"}
	return r
}

// reqString writes one BranchPRs request as "o/a[feat-a] o/b[feat-b]".
func reqString(c ghCall) string {
	var parts []string
	for _, rb := range c.repos {
		parts = append(parts, fmt.Sprintf("%s/%s%v", rb.Owner, rb.Name, rb.Branches))
	}
	return strings.Join(parts, " ")
}

func TestGitHubActiveAndIdleTicks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := ghRig(t)
		base := time.Now()
		r.start(t)
		time.Sleep(330 * sec)
		synctest.Wait()

		branch, _ := r.gh.calls()
		var got []string
		for _, c := range branch {
			got = append(got, fmt.Sprintf("%v %s", c.at.Sub(base), reqString(c)))
		}
		want := []string{
			"0s o/a[main feat-a] o/b[main feat-b]", // tick 0: everything; default branches not known yet
			"1m0s o/a[feat-a]",
			"2m0s o/a[feat-a]",
			"3m0s o/a[feat-a]",
			"4m0s o/a[feat-a]",
			"5m0s o/a[feat-a] o/b[feat-b]", // every 5th tick: the idle repos too
		}
		if !slices.Equal(got, want) {
			t.Errorf("BranchPRs calls:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		if gs := r.d.Status().GitHub; gs.ActiveRepos != 1 || gs.IdleRepos != 1 || gs.IdleEvery != 5 || gs.Tick != 6 {
			t.Errorf("github status = %+v", gs)
		}
		// Default branches are stored once, not on every tick.
		var stored []string
		for _, rp := range r.st.repoWrites() {
			stored = append(stored, rp.Name+" "+rp.DefaultBranch)
		}
		if want := []string{"o/a main", "o/b main"}; !slices.Equal(stored, want) {
			t.Errorf("stored default branches %q, want %q", stored, want)
		}

		// A change in b's worktrees makes it active for 30 minutes.
		b := repo("b", base.Add(-24*time.Hour), "main", "feat-b")
		b.Worktrees[1].Head = strings.Repeat("b", 40)
		r.scanner.set(&worktrees.Result{Repos: []worktrees.Repo{repo("a", base.Add(-24*time.Hour), "main", "feat-a"), b}})
		r.watcher.events <- "/r/b/.git"
		time.Sleep(30 * sec) // t=360 tick
		synctest.Wait()
		branch, _ = r.gh.calls()
		if got := reqString(branch[len(branch)-1]); got != "o/a[feat-a] o/b[feat-b]" {
			t.Errorf("tick after b changed = %q, want both", got)
		}
		time.Sleep(31 * time.Minute)
		synctest.Wait()
		branch, _ = r.gh.calls()
		if got := reqString(branch[len(branch)-1]); got != "o/a[feat-a]" {
			t.Errorf("tick 30 minutes after b changed = %q, want a only", got)
		}
	})
}

func TestGitHubIdleEveryFromConfig(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, func(c *config.Config) {
			c.Poll.GitHubActive = 30 * sec
			c.Poll.GitHubIdle = 90 * sec
		})
		old := time.Now().Add(-24 * time.Hour)
		r.scanner.set(&worktrees.Result{Repos: []worktrees.Repo{repo("b", old, "main", "feat-b")}})
		r.gh.defaults = map[string]string{"o/b": "main"}
		base := time.Now()
		r.start(t)
		time.Sleep(200 * sec)
		synctest.Wait()
		branch, _ := r.gh.calls()
		var at []time.Duration
		for _, c := range branch {
			at = append(at, c.at.Sub(base))
		}
		// Only idle repos: every 3rd 30s tick.
		if want := []time.Duration{0, 90 * sec, 180 * sec}; !slices.Equal(at, want) {
			t.Errorf("BranchPRs at %v, want %v", at, want)
		}
	})
}

func TestGitHubBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := ghRig(t)
		base := time.Now()
		resume := base.Add(700 * sec)
		r.gh.budget = func(now time.Time) github.Budget {
			switch at := now.Sub(base); {
			case at < 90*sec:
				return github.Budget{Verdict: github.BudgetNormal, Known: true, Remaining: 4000, Limit: 5000}
			case at < 400*sec:
				return github.Budget{Verdict: github.BudgetIdleOnly, Known: true, Remaining: 900, Limit: 5000}
			case at < 700*sec:
				return github.Budget{Verdict: github.BudgetStop, Known: true, Remaining: 100, Limit: 5000, ResetAt: resume, ResumeAt: resume}
			default:
				return github.Budget{Verdict: github.BudgetNormal, Known: true, Remaining: 5000, Limit: 5000}
			}
		}
		r.start(t)

		time.Sleep(500 * sec) // inside the stop window
		synctest.Wait()
		st := r.d.Status()
		if st.GitHub.Budget.Verdict != "stop" || !st.GitHub.Budget.ResumeAt.Equal(resume) {
			t.Errorf("budget = %+v, want stop until %v", st.GitHub.Budget, resume)
		}
		if gh := st.Sources[2]; !strings.Contains(gh.Skipped, "stop") {
			t.Errorf("github source = %+v, want skipped for the budget", gh)
		}

		time.Sleep(300 * sec) // t=800
		synctest.Wait()
		branch, search := r.gh.calls()
		var bt, stt []time.Duration
		for _, c := range branch {
			bt = append(bt, c.at.Sub(base))
		}
		for _, c := range search {
			stt = append(stt, c.at.Sub(base))
		}
		// normal: 0, 60. idle-only: only tick 5 (300s) runs, with every
		// repo. stop: nothing from 420 to 660, not even idle tick 10 (600).
		// normal again from 720.
		if want := []time.Duration{0, 60 * sec, 300 * sec, 720 * sec, 780 * sec}; !slices.Equal(bt, want) {
			t.Errorf("BranchPRs at %v, want %v", bt, want)
		}
		if got := reqString(branch[2]); got != "o/a[feat-a] o/b[feat-b]" {
			t.Errorf("idle-only tick asked for %q, want every repo", got)
		}
		// The search runs every 5 minutes with no dashboard, on idle ticks
		// only under idle-only, never under stop.
		if want := []time.Duration{0, 300 * sec, 720 * sec}; !slices.Equal(stt, want) {
			t.Errorf("OpenPRs at %v, want %v", stt, want)
		}
	})
}

func TestSearchCadence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := ghRig(t)
		base := time.Now()
		r.start(t)
		time.Sleep(100 * sec)
		r.d.SetWatchers(1)    // overdue at the watched cadence: search now
		time.Sleep(150 * sec) // t=250
		r.d.SetWatchers(0)
		time.Sleep(400 * sec) // t=650
		synctest.Wait()
		_, search := r.gh.calls()
		var at []time.Duration
		for _, c := range search {
			at = append(at, c.at.Sub(base))
			if !slices.Equal(c.owner, []string{"o"}) {
				t.Errorf("searched owners %q, want the configured [o]", c.owner)
			}
		}
		want := []time.Duration{0, 100 * sec, 180 * sec, 240 * sec, 540 * sec}
		if !slices.Equal(at, want) {
			t.Errorf("OpenPRs at %v, want %v", at, want)
		}
	})
}

func TestSearchOwnersFallBackToRemotes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, func(c *config.Config) { c.Owners = map[string]string{} })
		old := time.Now().Add(-time.Hour)
		b := repo("b", old, "main")
		b.Remotes = map[string]string{"origin": "https://github.com/Other/b"}
		r.scanner.set(&worktrees.Result{Repos: []worktrees.Repo{repo("a", old, "main"), b}})
		r.start(t)
		synctest.Wait()
		_, search := r.gh.calls()
		if len(search) != 1 || !slices.Equal(search[0].owner, []string{"o", "Other"}) {
			t.Errorf("searches = %+v, want one for [o Other]", search)
		}
	})
}

func TestPRWritesAndChangeEvents(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := ghRig(t)
		pr := github.PR{Repo: "o/a", Number: 7, State: github.StateOpen, HeadRef: "feat-a", HeadOID: "h1",
			Mergeable: github.Mergeable, MergeState: github.MergeStateClean, Checks: github.ChecksNone,
			Plan: github.PlanRef{Slug: "aim", ID: "B2"}}
		r.gh.prs["o/a:feat-a"] = []github.PR{pr}
		// An open PR in the store that nothing tracks any more.
		r.st.prs[prKey("o/z", 3)] = store.PR{Repo: "o/z", Number: 3, State: "OPEN", HeadBranch: "merged-and-removed"}
		r.start(t)
		synctest.Wait()

		got := r.st.pr("o/a", 7)
		if got.State != "OPEN" || got.Mergeable != "MERGEABLE" || got.ChecksState != "NONE" || got.PlanSlug != "aim" || got.PlanPkg != "B2" {
			t.Errorf("stored PR = %+v", got)
		}
		branch, _ := r.gh.calls()
		if s := reqString(branch[0]); !strings.Contains(s, "o/z[merged-and-removed]") {
			t.Errorf("idle tick %q doesn't refresh the untracked open PR", s)
		}
		if n := r.notes.count(api.TopicPRs); n != 1 {
			t.Errorf("PR notifications = %d, want 1", n)
		}

		// GitHub hasn't recomputed mergeability: keep MERGEABLE while the
		// head is the same, and notify nothing.
		pr.Mergeable, pr.MergeState = github.MergePending, github.MergeStateUnknown
		r.gh.mu.Lock()
		r.gh.prs["o/a:feat-a"] = []github.PR{pr}
		r.gh.mu.Unlock()
		time.Sleep(60 * sec)
		synctest.Wait()
		if got := r.st.pr("o/a", 7); got.Mergeable != "MERGEABLE" || got.MergeStateStatus != "CLEAN" {
			t.Errorf("pending mergeability overwrote the known value: %+v", got)
		}
		if n := r.notes.count(api.TopicPRs); n != 1 {
			t.Errorf("PR notifications after an unchanged tick = %d, want 1", n)
		}

		// A new head commit: UNKNOWN is the truth now.
		pr.HeadOID = "h2"
		r.gh.mu.Lock()
		r.gh.prs["o/a:feat-a"] = []github.PR{pr}
		r.gh.mu.Unlock()
		time.Sleep(60 * sec)
		synctest.Wait()
		if got := r.st.pr("o/a", 7); got.Mergeable != "UNKNOWN" || got.HeadRefOID != "h2" {
			t.Errorf("after a push = %+v, want UNKNOWN", got)
		}
		if n := r.notes.count(api.TopicPRs); n != 2 {
			t.Errorf("PR notifications after a push = %d, want 2", n)
		}
	})
}

func TestSessionChangeEventsOnlyOnChange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, nil)
		old := time.Now().Add(-time.Hour).UnixMilli()
		r.runner.set(entry{PID: 1, Cwd: "/r/a", Kind: "interactive", StartedAt: old, Status: "idle"})
		r.start(t)
		time.Sleep(95 * sec) // 4 identical polls
		synctest.Wait()
		if n := r.notes.count(api.TopicSessions); n != 1 {
			t.Errorf("session notifications after identical polls = %d, want 1", n)
		}
		r.runner.set(entry{PID: 1, Cwd: "/r/a", Kind: "interactive", StartedAt: old, Status: "waiting", WaitingFor: "input needed"})
		time.Sleep(30 * sec)
		synctest.Wait()
		if n := r.notes.count(api.TopicSessions); n != 2 {
			t.Errorf("session notifications after a change = %d, want 2", n)
		}
	})
}

func TestRetention(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, nil)
		base := time.Now()
		r.start(t)
		time.Sleep(49 * time.Hour)
		synctest.Wait()
		r.st.mu.Lock()
		got := offsets(base, r.st.purges)
		r.st.mu.Unlock()
		want := []time.Duration{time.Minute, 24*time.Hour + time.Minute, 48*time.Hour + time.Minute}
		if !slices.Equal(got, want) {
			t.Errorf("purges at %v, want %v", got, want)
		}
		if src := r.d.Status().Sources[4]; src.Name != api.SourceRetention || src.Runs != 3 || src.IntervalSeconds != 86400 {
			t.Errorf("retention source = %+v", src)
		}
	})
}

func scanTimes(base time.Time, calls []scanCall) []time.Duration {
	out := make([]time.Duration, len(calls))
	for i, c := range calls {
		out[i] = c.at.Sub(base).Round(100 * time.Millisecond)
	}
	return out
}

func keys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
