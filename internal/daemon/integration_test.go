package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/config"
	"github.com/gravexa-llc/ai-manager/internal/github"
	"github.com/gravexa-llc/ai-manager/internal/sessions"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/term"
	"github.com/gravexa-llc/ai-manager/internal/testenv"
	"github.com/gravexa-llc/ai-manager/internal/web"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
	"github.com/gravexa-llc/ai-manager/internal/worktrees"
)

// TestMain isolates git from the machine, as internal/worktrees does: no
// system or global config, fixed identities, and a ceiling at the temp dir.
func TestMain(m *testing.M) {
	done := testenv.Setup() // a small TMPDIR, and the fork watchdog (see testenv)
	os.Exit(done(func() int {
		dir, err := os.MkdirTemp("", "aim-b2-git")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer os.RemoveAll(dir)
		cfg := filepath.Join(dir, "gitconfig")
		if err := os.WriteFile(cfg, []byte("[init]\n\tdefaultBranch = main\n"), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		tmp, err := filepath.EvalSymlinks(os.TempDir())
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		for k, v := range map[string]string{
			"GIT_CONFIG_GLOBAL":       cfg,
			"GIT_CONFIG_NOSYSTEM":     "1",
			"GIT_CEILING_DIRECTORIES": tmp,
			"GIT_AUTHOR_NAME":         "Test",
			"GIT_AUTHOR_EMAIL":        "test@example.com",
			"GIT_COMMITTER_NAME":      "Test",
			"GIT_COMMITTER_EMAIL":     "test@example.com",
			"HOME":                    dir,
		} {
			os.Setenv(k, v)
		}
		return m.Run()
	}()))
}

func gitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// world is a temp root holding repo "app" (origin github.com/o/app) with a
// linked worktree "app-feat" on branch feat, and a store.
type world struct {
	root, app, feat string
	st              *store.Store
}

func newWorld(t *testing.T) *world {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := &world{root: filepath.Join(tmp, "root")}
	w.app = filepath.Join(w.root, "app")
	w.feat = filepath.Join(w.root, "app-feat")
	if err := os.MkdirAll(w.app, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, w.app, "init", "-q")
	if err := os.WriteFile(filepath.Join(w.app, "README"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, w.app, "add", "README")
	gitT(t, w.app, "commit", "-q", "-m", "first")
	gitT(t, w.app, "remote", "add", "origin", "https://github.com/o/app.git")
	gitT(t, w.app, "worktree", "add", "-q", "-b", "feat", w.feat)

	w.st, err = store.Open(context.Background(), filepath.Join(tmp, store.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.st.Close() })
	return w
}

// daemon returns a daemon on the world's store and the real scanner, with
// a fake lister and GitHub.
func (w *world) daemon(t *testing.T, runner *fakeRunner, gh *fakeGitHub, watcher RepoWatcher, notify func(string)) *Daemon {
	cfg := config.Default()
	cfg.Roots = []string{w.root}
	cfg.Owners = map[string]string{"o": "acct"}
	return New(Options{
		Config:        cfg,
		Store:         w.st,
		Lister:        sessions.Lister{Runner: runner},
		Scanner:       worktrees.New(worktrees.OptionsFromConfig(cfg)),
		GitHub:        gh,
		RepoWatcher:   watcher,
		Notify:        notify,
		TempDirs:      []string{"/nonexistent-tmp"},
		Procs:         func(context.Context) (term.Procs, error) { return term.Procs{}, nil },
		WatchSessions: func(ctx context.Context, kick func()) error { <-ctx.Done(); return nil },
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func TestStoreIntegration(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	prCreated := time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC)
	gh := &fakeGitHub{
		prs: map[string][]github.PR{"o/app:feat": {{
			Repo: "o/app", Number: 12, State: github.StateOpen, HeadRef: "feat", HeadOID: "abc",
			Mergeable: github.Mergeable, MergeState: github.MergeStateClean, Checks: github.ChecksSuccess,
			Title: "Feature", URL: "https://github.com/o/app/pull/12", CreatedAt: prCreated,
		}}},
		defaults: map[string]string{"o/app": "main"},
	}
	d := w.daemon(t, &fakeRunner{}, gh, newFakeWatcher(), nil)
	d.runCtx = ctx

	// Worktrees.
	d.fullScan(ctx)
	wts, err := w.st.ListWorktrees(ctx, store.WorktreeFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(wts) != 2 || wts[0].Path != w.app || wts[1].Path != w.feat {
		t.Fatalf("worktrees = %+v", wts)
	}
	for _, wt := range wts {
		if wt.MainRepo != w.app || wt.DirtyCount != 0 || wt.HeadSHA == "" || wt.LastCommitAt.IsZero() || wt.UpstreamState != store.UpstreamNone {
			t.Errorf("row = %+v", wt)
		}
	}

	// Sessions, resolved against the scan.
	now := time.Now().Truncate(time.Millisecond)
	snap := sessions.Snapshot{At: now, All: true, Sessions: []sessions.Session{
		{PID: 10, SessionID: "s10", Cwd: filepath.Join(w.feat, "sub"), Kind: sessions.Interactive,
			StartedAt: now.Add(-time.Hour), Status: sessions.StatusWaiting, WaitingFor: sessions.WaitingPermission},
		{JobID: "j1", SessionID: "s11", Cwd: "/somewhere/else", Kind: sessions.Background, StartedAt: now.Add(-time.Hour), State: sessions.JobWorking},
	}}
	d.onSessions(sessions.Result{Snapshot: snap, Events: sessions.Diff(sessions.Snapshot{}, snap)})
	got, err := w.st.GetSession(ctx, "pid:10")
	if err != nil {
		t.Fatal(err)
	}
	if got.WorktreePath != w.feat || got.Repo != w.app || got.Branch != "feat" || got.State != "needs_you" ||
		got.StateReason != "permission prompt" || !got.NeedsYouSince.Equal(now) || got.Headless {
		t.Errorf("session = %+v", got)
	}
	if job, err := w.st.GetSession(ctx, "job:j1"); err != nil || job.State != "working" || job.Repo != "" {
		t.Errorf("job = %+v, %v", job, err)
	}

	// GitHub: tick 0 asks for every branch, then stores the PR.
	d.githubTick(ctx)
	branch, search := gh.calls()
	if len(branch) != 1 || reqString(branch[0]) != "o/app[main feat]" || len(search) != 1 {
		t.Errorf("GitHub calls: branch %+v, search %+v", branch, search)
	}
	pr, err := w.st.GetPR(ctx, "o/app", 12)
	if err != nil || pr.HeadBranch != "feat" || pr.MergeStateStatus != "CLEAN" || pr.ChecksState != "SUCCESS" ||
		!pr.CreatedAt.Equal(prCreated) || pr.FirstSeenAt.Before(pr.CreatedAt) {
		t.Errorf("PR = %+v, %v", pr, err)
	}

	// The worktree goes; the session leaves.
	gitT(t, w.app, "worktree", "remove", w.feat)
	d.fullScan(ctx)
	gone, err := w.st.GetWorktree(ctx, w.feat)
	if err != nil || gone.RemovedAt.IsZero() {
		t.Errorf("removed worktree = %+v, %v", gone, err)
	}
	next := sessions.Snapshot{At: now.Add(time.Minute), All: true, Sessions: snap.Sessions[1:]}
	d.onSessions(sessions.Result{Snapshot: next, Events: sessions.Diff(snap, next)})
	if _, err := w.st.GetSession(ctx, "pid:10"); err == nil {
		t.Error("the session that left is still live")
	}
	all, _ := w.st.ListSessions(ctx, store.SessionFilter{IncludeEnded: true})
	for _, s := range all {
		if s.Key == "pid:10" && !s.EndedAt.Equal(now.Add(time.Minute)) {
			t.Errorf("ended at %v, want %v", s.EndedAt, now.Add(time.Minute))
		}
	}

	st := d.Status()
	for _, src := range st.Sources[:4] {
		if src.Runs == 0 || src.Stale {
			t.Errorf("source %+v", src)
		}
	}
}

func TestRepoNames(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	gh := &fakeGitHub{defaults: map[string]string{"o/app": "main"}}
	d := w.daemon(t, &fakeRunner{}, gh, newFakeWatcher(), nil)
	d.runCtx = ctx
	if got := d.RepoNames(); len(got) != 0 {
		t.Errorf("before a scan: RepoNames = %v, want none", got)
	}
	// From the remote, keyed by the main checkout path the rows store.
	d.fullScan(ctx)
	if got := d.RepoNames(); len(got) != 1 || got[w.app] != "o/app" {
		t.Errorf("after a scan: RepoNames = %v, want %s → o/app", got, w.app)
	}
	// Once GitHub answers under another name (the repo was transferred),
	// that is the name its PRs are stored under.
	gh.answerAs = map[string]string{"o/app": "neworg/app"}
	d.githubTick(ctx)
	if got := d.RepoNames(); got[w.app] != "neworg/app" {
		t.Errorf("after a transfer: RepoNames = %v, want %s → neworg/app", got, w.app)
	}
	if got := d.CanonicalRepo("O/App"); got != "neworg/app" {
		t.Errorf("CanonicalRepo(O/App) = %q, want neworg/app", got)
	}
	// The default branch is stored under the name GitHub answered with,
	// where internal/state looks it up.
	repos, err := w.st.ListRepos(ctx)
	if err != nil || len(repos) != 1 || repos[0].Name != "neworg/app" || repos[0].DefaultBranch != "main" {
		t.Errorf("stored repos %+v (%v), want neworg/app on main", repos, err)
	}
	gh.defaults = map[string]string{"o/app": "trunk"}
	d.githubTick(ctx)
	if repos, err := w.st.ListRepos(ctx); err != nil || len(repos) != 1 || repos[0].DefaultBranch != "trunk" {
		t.Errorf("after the default branch changed: %+v (%v)", repos, err)
	}
}

func TestFSWatcher(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	for _, p := range []string{a, b} {
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w, err := NewFSWatcher(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	w.Set([]string{a, filepath.Join(dir, "missing")})

	expect := func(want string) {
		t.Helper()
		select {
		case got := <-w.Events():
			if got != want {
				t.Fatalf("event for %q, want %q", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no event for %q", want)
		}
	}
	drain := func() {
		for {
			select {
			case <-w.Events():
			case <-time.After(200 * time.Millisecond):
				return
			}
		}
	}

	if err := os.WriteFile(filepath.Join(a, "x"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	expect(a)
	drain()

	// Swap the watch to b: a goes quiet.
	w.Set([]string{b})
	if err := os.WriteFile(filepath.Join(a, "y"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(b, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	expect(b)
}

// TestEndToEnd runs the daemon with the real store, scanner and fsnotify
// watcher behind the real web server, and follows the change events a
// dashboard would get.
func TestEndToEnd(t *testing.T) {
	w := newWorld(t)
	runner := &fakeRunner{}
	old := time.Now().Add(-time.Hour).UnixMilli()
	runner.set(entry{PID: 20, SessionID: "s20", Cwd: w.feat, Kind: "interactive", StartedAt: old, Status: "busy"})
	gh := &fakeGitHub{
		prs:      map[string][]github.PR{"o/app:feat": {{Repo: "o/app", Number: 3, State: github.StateOpen, HeadRef: "feat", Title: "f"}}},
		defaults: map[string]string{"o/app": "main"},
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	var d *Daemon
	hub := web.NewHub(func(n int) { d.SetWatchers(n) })
	d = w.daemon(t, runner, gh, nil, hub.Publish) // nil watcher: the real fsnotify one
	// pid 20 runs in a Terminal.app tab.
	d.procs = func(context.Context) (term.Procs, error) {
		return term.ParseProcs("" +
			"    1     0 ??       /sbin/launchd\n" +
			"  669     1 ??       /System/Applications/Utilities/Terminal.app/Contents/MacOS/Terminal\n" +
			"   18   669 ttys004  login\n" +
			"   19    18 ttys004  -zsh\n" +
			"   20    19 ttys004  claude\n"), nil
	}
	srv := httptest.NewUnstartedServer(web.New(web.Options{Port: port, Token: "tok", Store: w.st, Status: d, Hub: hub, Terminals: d}).Handler())
	srv.Listener.Close()
	srv.Listener = ln
	srv.Config.RegisterOnShutdown(hub.Close)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		d.Run(ctx)
	}()
	srv.Start()
	defer func() {
		srv.Close()
		cancel()
		<-done
	}()

	// aimd answers no read without a credential: the token, as `aim` sends it.
	get := func(url string) (*http.Response, error) {
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set(web.TokenHeader, "tok")
		return http.DefaultClient.Do(req)
	}
	resp, err := get(srv.URL + "/api/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	topics := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var c api.Change
				if json.Unmarshal([]byte(data), &c) == nil && c.Topic != "" {
					topics <- c.Topic
				}
			}
		}
		close(topics)
	}()
	waitTopic := func(want string) {
		t.Helper()
		deadline := time.After(15 * time.Second)
		for {
			select {
			case got, ok := <-topics:
				if !ok {
					t.Fatalf("stream ended waiting for %q", want)
				}
				if got == want {
					return
				}
			case <-deadline:
				t.Fatalf("no %q change event", want)
			}
		}
	}
	getJSON := func(path string, v any) {
		t.Helper()
		r, err := get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(v); err != nil {
			t.Fatal(err)
		}
	}

	waitTopic(api.TopicPRs) // the first GitHub tick comes after the first scan and poll
	var ss api.Sessions
	getJSON("/api/sessions", &ss)
	if len(ss.Sessions) != 1 || ss.Sessions[0].Key != "pid:20" {
		t.Fatalf("sessions = %+v", ss)
	}
	if s := ss.Sessions[0]; s.Terminal != "terminal" || s.TerminalApp != "Terminal" {
		t.Errorf("pid:20 runs in %q (%q), want terminal (Terminal)", s.Terminal, s.TerminalApp)
	}
	var prs api.PRs
	getJSON("/api/prs", &prs)
	if len(prs.PRs) != 1 || prs.PRs[0].Number != 3 {
		t.Errorf("prs = %+v", prs)
	}
	if got := d.SessionInterval(); got != 10*time.Second {
		t.Errorf("session interval with a dashboard connected = %s, want 10s", got)
	}

	// A new worktree shows up through fsnotify, long before the 2-minute scan.
	two := filepath.Join(w.root, "app-two")
	gitT(t, w.app, "worktree", "add", "-q", "-b", "two", two)
	deadline := time.Now().Add(15 * time.Second)
	for {
		waitTopic(api.TopicWorktrees)
		var wts api.Worktrees
		getJSON("/api/worktrees", &wts)
		found := false
		for _, wt := range wts.Worktrees {
			found = found || wt.Path == two
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("worktree %s never listed: %+v", two, wts)
		}
	}

	var st api.Status
	getJSON("/api/status", &st)
	if st.Watchers != 1 || st.Counts.Sessions != 1 || st.Counts.Worktrees != 3 || st.Counts.OpenPRs != 1 {
		t.Errorf("status = watchers %d, counts %+v", st.Watchers, st.Counts)
	}
}
