package planapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/config"
	"github.com/gravexa-llc/ai-manager/internal/planapi"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/testenv"
	"github.com/gravexa-llc/ai-manager/internal/web"
	"github.com/gravexa-llc/ai-manager/internal/worktrees"
)

// TestMain isolates git from the machine, as internal/worktrees does: no
// system or global config, fixed identities, and a ceiling at the temp
// dir.
func TestMain(m *testing.M) {
	done := testenv.Setup() // a small TMPDIR, and the fork watchdog (see testenv)
	os.Exit(done(func() int {
		dir, err := os.MkdirTemp("", "aim-c3-git")
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

const (
	testPort  = 7777
	testToken = "test-token-0123456789"
)

// env is aimd's plan API over temp repos:
//
//   - root/pivot (origin acme/pivot) holds the sharing plan at
//     docs/plans/sharing-plan.md;
//   - root/pivot-web (origin acme/pivot-web);
//   - outside/ai-manager (origin example-user/ai-manager), outside the
//     configured root but known to the worktree scan, holds the roadmap
//     fixture (testdata/plans/ROADMAP.md).
type env struct {
	t                  *testing.T
	tmp, root, outside string
	pivot, web, aim    string
	sharing, aimPlan   string // the plan files

	st       *store.Store
	scanner  *worktrees.Scanner
	scan     atomic.Pointer[worktrees.Result]
	svc      *planapi.Service
	srv      *web.Server
	h        http.Handler
	notes    *notes
	backfill *fakeBackfill
	refresh  *fakeRefresh
}

func newEnv(t *testing.T) *env {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, tmp: tmp, root: filepath.Join(tmp, "root"), outside: filepath.Join(tmp, "outside")}
	e.pivot = e.newRepo(filepath.Join(e.root, "pivot"), "acme/pivot")
	e.web = e.newRepo(filepath.Join(e.root, "pivot-web"), "acme/pivot-web")
	e.aim = e.newRepo(filepath.Join(e.outside, "ai-manager"), "example-user/ai-manager")
	e.sharing = e.copyFixture("sharing-plan.md", filepath.Join(e.pivot, "docs", "plans", "sharing-plan.md"))
	e.aimPlan = e.copyFixture("ROADMAP.md", filepath.Join(e.aim, "ROADMAP.md"))

	e.st, err = store.Open(context.Background(), filepath.Join(tmp, store.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.st.Close() })
	cfg := config.Default()
	cfg.Roots = []string{e.root}
	e.scanner = worktrees.New(worktrees.OptionsFromConfig(cfg))
	e.rescan()
	e.notes = &notes{}
	e.backfill = &fakeBackfill{st: e.st, prs: map[string][]store.PR{}}
	e.refresh = &fakeRefresh{}
	e.svc = planapi.New(planapi.Options{
		Store:    e.st,
		Config:   cfg,
		Scanner:  e.scanner,
		LastScan: e.scan.Load,
		Backfill: e.backfill.run,
		Refresh:  e.refresh.run,
		Notify:   e.notes.notify,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		MinGap:   50 * time.Millisecond,
	})
	t.Cleanup(e.svc.Wait)
	e.srv = web.New(web.Options{Port: testPort, Token: testToken, Store: e.st, Plans: e.svc})
	e.h = e.srv.Handler()
	return e
}

// git runs git in dir.
func (e *env) git(dir string, args ...string) string {
	e.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// newRepo makes a repo with one commit and a GitHub origin.
func (e *env) newRepo(dir, nameWithOwner string) string {
	e.t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		e.t.Fatal(err)
	}
	e.git(dir, "init", "-q")
	e.write(filepath.Join(dir, "README"), "x\n")
	e.git(dir, "add", "README")
	e.git(dir, "commit", "-q", "-m", "first")
	e.git(dir, "remote", "add", "origin", "git@github.com:"+nameWithOwner+".git")
	return dir
}

func (e *env) write(path, content string) {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) copyFixture(name, to string) string {
	e.t.Helper()
	src, err := os.ReadFile(filepath.Join("..", "..", "testdata", "plans", name))
	if err != nil {
		e.t.Fatal(err)
	}
	e.write(to, string(src))
	return to
}

// rescan scans the root and the ai-manager repo, as the daemon would
// once a session works there.
func (e *env) rescan() {
	e.scan.Store(e.scanner.Scan(context.Background(), e.aim))
}

// req sends a request with the token, the JSON body (if not nil) and
// any extra headers ("Header: value"), and returns the recorder.
func (e *env) req(method, target string, body any, hdrs ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.send(method, target, body, append([]string{web.TokenHeader + ": " + testToken}, hdrs...)...)
}

// send is req without the token.
func (e *env) send(method, target string, body any, hdrs ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	var r io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		r = strings.NewReader(b)
	default:
		buf, err := json.Marshal(b)
		if err != nil {
			e.t.Fatal(err)
		}
		r = bytes.NewReader(buf)
	}
	req := httptest.NewRequest(method, target, r)
	req.Host = fmt.Sprintf("127.0.0.1:%d", testPort)
	for _, h := range hdrs {
		k, v, _ := strings.Cut(h, ": ")
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

// ok fails the test unless the response has the status, and decodes it
// into a T.
func ok[T any](t *testing.T, rec *httptest.ResponseRecorder, status int) T {
	t.Helper()
	var v T
	if rec.Code != status {
		t.Fatalf("status %d, want %d: %s", rec.Code, status, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type %q, want JSON", ct)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return v
}

// refused fails the test unless the response is an error with the status
// whose message holds every part.
func refused(t *testing.T, what string, rec *httptest.ResponseRecorder, status int, parts ...string) {
	t.Helper()
	if rec.Code != status {
		t.Errorf("%s: status %d, want %d: %s", what, rec.Code, status, rec.Body)
		return
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Error == "" {
		t.Errorf("%s: body %s is not {\"error\": …}", what, rec.Body)
		return
	}
	for _, p := range parts {
		if !strings.Contains(body.Error, p) {
			t.Errorf("%s: error %q lacks %q", what, body.Error, p)
		}
	}
}

// notes records Notify's topics.
type notes struct {
	mu     sync.Mutex
	topics []string
}

func (n *notes) notify(topic string) {
	n.mu.Lock()
	n.topics = append(n.topics, topic)
	n.mu.Unlock()
}

func (n *notes) count(topic string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	c := 0
	for _, t := range n.topics {
		if t == topic {
			c++
		}
	}
	return c
}

// fakeRefresh stands in for the daemon's RefreshPRs: it records what it
// is asked to read, as "owner/repo#n", and queues every PR.
type fakeRefresh struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeRefresh) run(prs []store.PRKey) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range prs {
		f.calls = append(f.calls, store.PRRef(k.Repo, k.Number))
	}
	return len(prs)
}

// take returns the PRs asked for since the last take.
func (f *fakeRefresh) take() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.calls
	f.calls = nil
	return out
}

// fakeBackfill stands in for the daemon's GitHub backfill: it stores the
// PRs the test put under each repo.
type fakeBackfill struct {
	st *store.Store

	mu    sync.Mutex
	prs   map[string][]store.PR // lower-case repo → PRs to store
	calls [][]string
}

func (f *fakeBackfill) run(ctx context.Context, repos []string) {
	f.mu.Lock()
	f.calls = append(f.calls, slices.Clone(repos))
	var prs []store.PR
	for _, r := range repos {
		prs = append(prs, f.prs[strings.ToLower(r)]...)
	}
	f.mu.Unlock()
	if len(prs) > 0 {
		if err := f.st.UpsertPRs(ctx, prs...); err != nil {
			panic(err)
		}
	}
}

func (f *fakeBackfill) set(repo string, prs ...store.PR) {
	f.mu.Lock()
	f.prs[strings.ToLower(repo)] = prs
	f.mu.Unlock()
}

func (f *fakeBackfill) asked() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}
