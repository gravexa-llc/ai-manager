package worktrees

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// parseFixture parses a testdata/worktrees capture (.porcelain or
// .porcelain-z) into worktrees, as a scan would before touching the disk.
func parseFixture(t *testing.T, name string) []Worktree {
	t.Helper()
	recs, err := parsePorcelain(fixture(t, name), strings.HasSuffix(name, "-z"))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	wts := make([]Worktree, len(recs))
	for i, r := range recs {
		wts[i] = newWorktree(r, i == 0)
	}
	return wts
}

func TestFixturesWellFormed(t *testing.T) {
	names, err := filepath.Glob(filepath.Join("..", "..", "testdata", "worktrees", "*.porcelain*"))
	if err != nil || len(names) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	var total, detached, scratch int
	for _, path := range names {
		name := filepath.Base(path)
		wts := parseFixture(t, name)
		if len(wts) == 0 {
			t.Errorf("%s: no records", name)
		}
		for _, w := range wts {
			if w.Path == "" {
				t.Errorf("%s: record without a path", name)
			}
			if w.Bare {
				if w.Head != "" || w.Branch != "" {
					t.Errorf("%s: bare record %s has HEAD or branch", name, w.Path)
				}
				continue
			}
			if !isSHA(w.Head) {
				t.Errorf("%s: %s: HEAD %q", name, w.Path, w.Head)
			}
			if (w.Branch == "") != w.Detached {
				t.Errorf("%s: %s: branch %q, detached %v", name, w.Path, w.Branch, w.Detached)
			}
		}
		if !strings.HasSuffix(name, "-z") {
			total += len(wts)
			for _, w := range wts {
				if w.Detached {
					detached++
				}
				if w.ScratchpadSession != "" {
					scratch++
				}
			}
		}
	}
	// The captures hold every detached and scratchpad record the capture counted.
	if detached != 62 || scratch != 60 {
		t.Errorf("fixtures: %d detached, %d scratchpad records; the capture counted 62 and 60", detached, scratch)
	}
	t.Logf("%d records in %d fixtures", total, len(names))
}

func TestFixturesPlainAndNULAgree(t *testing.T) {
	for _, repo := range []string{"ai-manager", "relay", "pivot-app"} {
		plain := parseFixture(t, repo+".porcelain")
		nul := parseFixture(t, repo+".porcelain-z")
		if !reflect.DeepEqual(plain, nul) {
			t.Errorf("%s: plain and -z parse differently:\n%+v\n%+v", repo, plain, nul)
		}
	}
}

func TestFixtureAIManagerLocks(t *testing.T) {
	wts := parseFixture(t, "ai-manager.porcelain-z")
	if len(wts) != 4 {
		t.Fatalf("got %d records, want 4", len(wts))
	}
	if !wts[0].Main || wts[0].Branch != "main" || wts[0].Locked {
		t.Errorf("main record: %+v", wts[0])
	}
	start := time.Date(2026, 11, 6, 1, 23, 32, 0, time.UTC)
	for _, w := range wts[1:] {
		if w.Main || !w.Locked || w.Lock == nil {
			t.Fatalf("%s: want a locked, parsed agent worktree: %+v", w.Path, w)
		}
		folder := strings.TrimPrefix(filepath.Base(w.Path), "agent-")
		if w.Lock.Kind != "agent" || w.Lock.AgentID != folder || w.AgentID != folder {
			t.Errorf("%s: lock %+v, AgentID %q; want agent %s", w.Path, w.Lock, w.AgentID, folder)
		}
		if w.Lock.PID != 91470 || !w.Lock.Start.Equal(start) {
			t.Errorf("%s: lock pid %d start %v; want 91470 %v", w.Path, w.Lock.PID, w.Lock.Start, start)
		}
		if w.Branch != "worktree-agent-"+folder {
			t.Errorf("%s: branch %q", w.Path, w.Branch)
		}
	}
}

func TestFixtureVanishedLockedRecord(t *testing.T) {
	// Captured while an agent's worktree was being removed: the lock
	// suppresses "prunable", so only a disk check can tell it's gone.
	wts := parseFixture(t, "ai-manager-vanished.porcelain")
	w := wts[3]
	if !strings.HasSuffix(w.Path, "agent-ac9f1ccbdb1535b88") || !w.Locked || w.Prunable {
		t.Errorf("vanished record: %+v", w)
	}
}

func TestFixturePivotAgents(t *testing.T) {
	wts := parseFixture(t, "pivot-app.porcelain")
	if len(wts) != 6 {
		t.Fatalf("got %d records, want 6", len(wts))
	}
	// A finished agent: unlocked, branch renamed, id from its folder.
	if w := wts[1]; w.Locked || w.Branch != "share/v1-email-reports" || w.AgentID != "a73d6cff14b7bf150" {
		t.Errorf("finished agent: %+v", w)
	}
	// The stale lock in the capture: lead pid 36430.
	w := wts[2]
	if w.Lock == nil || w.Lock.PID != 36430 || w.AgentID != "ab66668f3b601afd9" ||
		!w.Lock.Start.Equal(time.Date(2026, 10, 9, 21, 16, 46, 0, time.UTC)) {
		t.Errorf("stale lock: %+v %+v", w, w.Lock)
	}
}

func TestFixtureRelayPrunableLowercase(t *testing.T) {
	wts := parseFixture(t, "relay.porcelain-z")
	if len(wts) != 2 {
		t.Fatalf("got %d records, want 2", len(wts))
	}
	w := wts[1]
	if !w.Prunable || w.PrunableReason != "gitdir file points to non-existent location" {
		t.Errorf("prunable: %+v", w)
	}
	if w.Branch != "claude/eager-blackwell-b5eea4" || w.AgentID != "" {
		t.Errorf("branch %q, AgentID %q", w.Branch, w.AgentID)
	}
	// git recorded ~/projects/relay for the repo at ~/Projects/relay.
	if got := within(w.Path, wts[0].Path); got != caseInsensitive {
		t.Errorf("within(%s, %s) = %v on a case-insensitive=%v system", w.Path, wts[0].Path, got, caseInsensitive)
	}
}

func TestFixtureBareAndDetached(t *testing.T) {
	bare := parseFixture(t, "toolkit-dev-bare.porcelain")
	if len(bare) != 1 || !bare[0].Bare || !bare[0].Main {
		t.Errorf("bare: %+v", bare)
	}
	g := parseFixture(t, "toolkit.porcelain")
	if g[0].Branch != "user/sample-data-refresh-2026-08-18" || !g[0].Main {
		t.Errorf("toolkit main on a feature branch: %+v", g[0])
	}
	if !g[2].Detached || g[2].Branch != "" {
		t.Errorf("toolkit-release: %+v", g[2])
	}
}

func TestFixtureScratchpadSessions(t *testing.T) {
	for _, name := range []string{"dogwood-backend.porcelain", "meadow.porcelain"} {
		for _, w := range parseFixture(t, name) {
			inScratch := strings.Contains(w.Path, "/scratchpad/")
			if inScratch != (w.ScratchpadSession != "") {
				t.Errorf("%s: %s: session %q", name, w.Path, w.ScratchpadSession)
			}
			if inScratch && w.ScratchpadSession != "5213f886-247d-496b-a20e-50b0e6c1973f" {
				t.Errorf("%s: %s: session %q", name, w.Path, w.ScratchpadSession)
			}
		}
	}
}

func TestParsePorcelainEdgeCases(t *testing.T) {
	nul := "worktree /r\x00HEAD " + strings.Repeat("a", 40) + "\x00branch refs/heads/main\x00\x00" +
		"worktree /r/w1\x00HEAD " + strings.Repeat("b", 40) + "\x00detached\x00locked\x00prunable\x00\x00" +
		"worktree /r/w 2\x00HEAD " + strings.Repeat("c", 40) + "\x00branch refs/heads/x\x00locked line1\nline2 \"q\"\x00future-attr yes\x00\x00" +
		"worktree /r/last\x00HEAD " + strings.Repeat("d", 40) + "\x00detached" // no terminators
	recs, err := parsePorcelain([]byte(nul), true)
	if err != nil {
		t.Fatal(err)
	}
	want := []record{
		{Path: "/r", Head: strings.Repeat("a", 40), Branch: "refs/heads/main"},
		{Path: "/r/w1", Head: strings.Repeat("b", 40), Detached: true, Locked: true, Prunable: true},
		{Path: "/r/w 2", Head: strings.Repeat("c", 40), Branch: "refs/heads/x", Locked: true, LockReason: "line1\nline2 \"q\""},
		{Path: "/r/last", Head: strings.Repeat("d", 40), Detached: true},
	}
	if !reflect.DeepEqual(recs, want) {
		t.Errorf("-z:\n got %+v\nwant %+v", recs, want)
	}

	// Without -z, git C-quotes the same reason.
	plain := "worktree /r/w 2\nHEAD " + strings.Repeat("c", 40) + "\nbranch refs/heads/x\nlocked \"line1\\nline2 \\\"q\\\"\"\n\n"
	recs, err = parsePorcelain([]byte(plain), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].LockReason != "line1\nline2 \"q\"" {
		t.Errorf("plain: %+v", recs)
	}

	if _, err := parsePorcelain([]byte("HEAD abc\x00\x00"), true); err == nil {
		t.Error("attribute before worktree: no error")
	}
	if recs, err := parsePorcelain(nil, true); err != nil || len(recs) != 0 {
		t.Errorf("empty input: %v %v", recs, err)
	}
}

func TestParseLockReason(t *testing.T) {
	for _, tc := range []struct {
		reason string
		ok     bool
		want   ClaudeLock
	}{
		{
			reason: "claude agent agent-a740b0899d775992b (pid 91470 start Fri Nov  6 01:23:32 2026)",
			ok:     true,
			want: ClaudeLock{Kind: "agent", Name: "agent-a740b0899d775992b", AgentID: "a740b0899d775992b",
				PID: 91470, Start: time.Date(2026, 11, 6, 1, 23, 32, 0, time.UTC), StartText: "Fri Nov  6 01:23:32 2026"},
		},
		{
			reason: "claude agent agent-ab1 (pid 7 start Sun Oct 18 09:05:00 2026)\n",
			ok:     true,
			want: ClaudeLock{Kind: "agent", Name: "agent-ab1", AgentID: "ab1", PID: 7,
				Start: time.Date(2026, 10, 18, 9, 5, 0, 0, time.UTC), StartText: "Sun Oct 18 09:05:00 2026"},
		},
		{
			// Some other Claude lock: kept, with no agent id; bad start time.
			reason: "claude session s2w (pid 12 start sometime)",
			ok:     true,
			want:   ClaudeLock{Kind: "session", Name: "s2w", PID: 12, StartText: "sometime"},
		},
		{reason: ""},
		{reason: "moving to a new disk"},
		{reason: "claude agent agent-a1 (pid x start Fri Nov  6 01:23:32 2026)"},
		{reason: "claude agent agent-a1"},
	} {
		got, ok := ParseLockReason(tc.reason)
		if ok != tc.ok || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseLockReason(%q) = %+v, %v; want %+v, %v", tc.reason, got, ok, tc.want, tc.ok)
		}
	}
}

func TestParseTrack(t *testing.T) {
	for _, tc := range []struct {
		in            string
		gone          bool
		ahead, behind int
		err           bool
	}{
		{in: ""},
		{in: "gone", gone: true},
		{in: "[gone]", gone: true},
		{in: "ahead 4", ahead: 4},
		{in: "[ahead 4]", ahead: 4},
		{in: "behind 28", behind: 28},
		{in: "ahead 1, behind 79", ahead: 1, behind: 79},
		{in: "[ahead 3, behind 5]", ahead: 3, behind: 5},
		{in: "sideways 2", err: true},
		{in: "ahead x", err: true},
	} {
		gone, ahead, behind, err := parseTrack(tc.in)
		if (err != nil) != tc.err || gone != tc.gone || ahead != tc.ahead || behind != tc.behind {
			t.Errorf("parseTrack(%q) = %v %d %d %v", tc.in, gone, ahead, behind, err)
		}
	}
}

func TestParseRefsFixture(t *testing.T) {
	// The recorded capture used '|' between fields; scans use NULs. Convert the first
	// three fields of each line (refname, upstream, track) to refsFormat.
	var b strings.Builder
	lines := strings.Split(strings.TrimSpace(string(fixture(t, "upstream/dogwood-backend.for-each-ref"))), "\n")
	for _, line := range lines {
		f := strings.Split(line, "|")
		b.WriteString(f[0] + "\x00" + f[1] + "\x00" + f[2] + "\x00\n")
	}
	refs, err := parseRefs([]byte(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != len(lines) {
		t.Fatalf("got %d branches, want %d", len(refs), len(lines))
	}
	counts := map[UpstreamState]int{}
	for _, r := range refs {
		counts[r.State]++
	}
	if counts[UpstreamOK] != 140 || counts[UpstreamGone] != 2 || counts[UpstreamNone] != 31 {
		t.Errorf("states: %v", counts)
	}
	for name, want := range map[string]branchInfo{
		"main":                      {Upstream: "origin/main", State: UpstreamOK, Behind: 28},
		"cf/topic-002":              {Upstream: "origin/main", State: UpstreamOK, Ahead: 1, Behind: 79},
		"fix/233-stale-row-cleanup": {Upstream: "origin/fix/233-stale-row-cleanup", State: UpstreamOK, Ahead: 4},
		"feat/topic-043":            {Upstream: "origin/feat/topic-043", State: UpstreamGone},
		"cf/topic-003":              {State: UpstreamNone},
		"surge-a-2a":                {Upstream: "origin/surge-a-2a", State: UpstreamOK},
	} {
		if got := refs[name]; got != want {
			t.Errorf("%s: %+v, want %+v", name, got, want)
		}
	}

	// Real git output: full refnames, a trailing newline after each NUL.
	refs, err = parseRefs([]byte("refs/heads/a|b\x00origin/a|b\x00ahead 2\x00\nrefs/heads/c\x00\x00\x00\n"))
	if err != nil {
		t.Fatal(err)
	}
	if refs["a|b"].Ahead != 2 || refs["c"].State != UpstreamNone {
		t.Errorf("refs: %+v", refs)
	}
	if _, err := parseRefs([]byte("refs/heads/a\x00x\x00")); err == nil {
		t.Error("short record: no error")
	}
	if refs, err := parseRefs(nil); err != nil || len(refs) != 0 {
		t.Errorf("no branches: %v %v", refs, err)
	}
}

// statusSection returns the `git status --porcelain=v2 --branch` output
// recorded in an upstream/*.txt fixture.
func statusSection(t *testing.T, name string) []byte {
	t.Helper()
	text := string(fixture(t, name))
	_, after, ok := strings.Cut(text, "### git status --porcelain=v2 --branch\n")
	if !ok {
		t.Fatalf("%s: no porcelain=v2 section", name)
	}
	section, _, _ := strings.Cut(after, "###")
	return []byte(section)
}

func TestParseStatus(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		uncommitted, untracked int
	}{
		{"upstream/none-dirty-main-checkout.txt", 1, 0},
		{"upstream/ok.txt", 0, 0},
		{"upstream/ok-merged-pr.txt", 0, 0},
		{"upstream/detached.txt", 0, 0},
	} {
		u, n, err := parseStatus(statusSection(t, tc.name), false)
		if err != nil || u != tc.uncommitted || n != tc.untracked {
			t.Errorf("%s: %d uncommitted, %d untracked, %v; want %d, %d", tc.name, u, n, err, tc.uncommitted, tc.untracked)
		}
	}

	// -z: a rename carries its original path as an extra field.
	z := "# branch.oid abc\x00" +
		"1 .M N... 100644 100644 100644 aaa aaa a.txt\x00" +
		"2 R. N... 100644 100644 100644 bbb bbb R100 new.txt\x00old.txt\x00" +
		"u UU N... 100644 100644 100644 100644 c1 c2 c3 conflict.txt\x00" +
		"? untracked.txt\x00? dir/\x00! ignored.log\x00"
	u, n, err := parseStatus([]byte(z), true)
	if err != nil || u != 3 || n != 2 {
		t.Errorf("-z: %d uncommitted, %d untracked, %v; want 3, 2", u, n, err)
	}
	if _, _, err := parseStatus([]byte("X what\x00"), true); err == nil {
		t.Error("unknown entry: no error")
	}
}

func TestConfigParsing(t *testing.T) {
	// `git config -z --get-regexp` output: key, newline, value, NUL; a bare
	// boolean key has no newline. Subsections keep case, dots and spaces.
	raw := "filter.lfs.clean\ngit-lfs clean -- %f\x00filter.lfs.required\ntrue\x00" +
		"filter.My Driver.process\n/bin/x\x00filter.a.b.smudge\ncat\x00filter.flag\x00" +
		"filter.lfs.smudge\ngit-lfs smudge\nsecond line\x00"
	entries := parseConfigZ([]byte(raw))
	if len(entries) != 6 || entries[5][1] != "git-lfs smudge\nsecond line" || entries[4] != [2]string{"filter.flag", ""} {
		t.Fatalf("parseConfigZ: %q", entries)
	}
	names := filterDrivers(entries)
	if want := []string{"My Driver", "a.b", "lfs"}; !reflect.DeepEqual(names, want) {
		t.Errorf("filterDrivers = %q, want %q", names, want)
	}
	args, err := filterOverrideArgs([]string{"lfs", "My Driver"})
	want := []string{
		"-c", "filter.lfs.clean=", "-c", "filter.lfs.smudge=", "-c", "filter.lfs.process=",
		"-c", "filter.My Driver.clean=", "-c", "filter.My Driver.smudge=", "-c", "filter.My Driver.process=",
	}
	if err != nil || !reflect.DeepEqual(args, want) {
		t.Errorf("filterOverrideArgs = %q, %v", args, err)
	}
	if _, err := filterOverrideArgs([]string{"a=b"}); !errors.Is(err, errFilterName) {
		t.Errorf("name with '=': err %v", err)
	}
	if args, err := filterOverrideArgs(nil); args != nil || err != nil {
		t.Errorf("no drivers: %q %v", args, err)
	}

	remotes := remotesFrom(parseConfigZ([]byte(
		"remote.origin.url\ngit@github.com:o/r.git\x00remote.up.stream.url\nhttps://a\x00" +
			"remote.up.stream.url\nhttps://b\x00remote.origin.pushurl\nx\x00")))
	if want := map[string]string{"origin": "git@github.com:o/r.git", "up.stream": "https://a"}; !reflect.DeepEqual(remotes, want) {
		t.Errorf("remotesFrom = %v, want %v", remotes, want)
	}
}

func TestParseRevListTimestamps(t *testing.T) {
	got, err := parseRevListTimestamps([]byte("1793944158 47441b76f3e7b1cf5cb45ec7d6b158caa63091aa\n"))
	if err != nil || !got["47441b76f3e7b1cf5cb45ec7d6b158caa63091aa"].Equal(time.Unix(1793944158, 0)) {
		t.Errorf("got %v %v", got, err)
	}
	if _, err := parseRevListTimestamps([]byte("nope\n")); err == nil {
		t.Error("bad line: no error")
	}
}

func TestPathHelpers(t *testing.T) {
	if !SamePath("/a/b/../c", "/a/c/") {
		t.Error("SamePath doesn't clean")
	}
	if got := SamePath("/Users/x/Projects/relay", "/Users/x/projects/relay"); got != caseInsensitive {
		t.Errorf("SamePath across case = %v on case-insensitive=%v", got, caseInsensitive)
	}
	if !within("/r/a/b", "/r/a") || !within("/r/a", "/r/a") || within("/r/ab", "/r/a") || within("/r", "/r/a") {
		t.Error("within")
	}
	if !within("/r/a", "/") {
		t.Error("within root")
	}
	for p, want := range map[string]string{
		"/r/.claude/worktrees/agent-a740b0899d775992b":  "a740b0899d775992b",
		"/r/.claude/worktrees/agent-a740b0899d775992b/": "a740b0899d775992b",
		"/r/.claude/worktrees/eager-blackwell-b5eea4":   "",
		"/r/worktrees/agent-a740b0899d775992b":          "",
		"/r/.claude/worktrees/agent-XYZ":                "",
		"/r-agent-a740b0899d775992b":                    "",
	} {
		if got := agentFromPath(p); got != want {
			t.Errorf("agentFromPath(%s) = %q, want %q", p, got, want)
		}
	}
	for p, want := range map[string]string{
		"/private/tmp/claude-501/-Users-u-P-x/5213f886-247d-496b-a20e-50b0e6c1973f/scratchpad/review-1": "5213f886-247d-496b-a20e-50b0e6c1973f",
		"/private/tmp/claude-501/-Users-u-P-x/5213f886-247d-496b-a20e-50b0e6c1973f/scratchpad":          "5213f886-247d-496b-a20e-50b0e6c1973f",
		"/private/tmp/claude-501/-Users-u-P-x/5213f886-247d-496b-a20e-50b0e6c1973f/other/review-1":      "",
		"/Users/u/Projects/x/scratchpad/review-1":                                                       "",
	} {
		if got := scratchpadSession(p); got != want {
			t.Errorf("scratchpadSession(%s) = %q, want %q", p, got, want)
		}
	}
	for s, want := range map[string]bool{
		strings.Repeat("0", 40): false,
		strings.Repeat("a", 40): true,
		strings.Repeat("a", 64): true,
		strings.Repeat("A", 40): false,
		"abc":                   false,
	} {
		if isSHA(s) != want {
			t.Errorf("isSHA(%s) = %v", s, !want)
		}
	}
}
