package github

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/config"
)

// fakeGH records gh invocations and prints a token per account.
type fakeGH struct {
	mu     sync.Mutex
	calls  [][]string
	tokens map[string]string // account ("" = active) → token
	fail   error
}

func (f *fakeGH) run(_ context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)
	if f.fail != nil {
		return nil, f.fail
	}
	account := ""
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, "--user="); ok {
			account = v
		}
	}
	tok, ok := f.tokens[account]
	if !ok {
		return nil, errors.New("exit status 1: no oauth token found for github.com account " + account)
	}
	return []byte(tok + "\n"), nil
}

func newGHTokens(owners map[string]string, gh *fakeGH) *ghTokens {
	g := GHTokens(owners).(*ghTokens)
	g.run = gh.run
	return g
}

func TestGHTokens(t *testing.T) {
	gh := &fakeGH{tokens: map[string]string{"": "tok-active", "acct-one": "tok-one", "acct-two": "tok-two"}}
	g := newGHTokens(map[string]string{"Org-A": "acct-one", "org-b": "acct-one", "org-c": "acct-two"}, gh)
	ctx := context.Background()

	for _, tc := range []struct{ owner, want string }{
		{"org-a", "tok-one"}, // owner match is case-insensitive
		{"ORG-B", "tok-one"}, // same account: cached, no second gh run
		{"org-c", "tok-two"},
		{"unmapped", "tok-active"},
		{"other-unmapped", "tok-active"},
	} {
		got, err := g.Token(ctx, tc.owner)
		if err != nil || got != tc.want {
			t.Errorf("Token(%q) = %q, %v; want %q", tc.owner, got, err, tc.want)
		}
	}
	want := [][]string{
		{"auth", "token", "--hostname", "github.com", "--user=acct-one"},
		{"auth", "token", "--hostname", "github.com", "--user=acct-two"},
		{"auth", "token", "--hostname", "github.com"},
	}
	if !reflect.DeepEqual(gh.calls, want) {
		t.Errorf("gh calls =\n%q\nwant\n%q", gh.calls, want)
	}

	// Forget drops one account's token; the next call runs gh again.
	g.Forget("org-b")
	gh.tokens["acct-one"] = "tok-one-new"
	if got, _ := g.Token(ctx, "org-a"); got != "tok-one-new" {
		t.Errorf("after Forget: %q, want the new token", got)
	}
	if got, _ := g.Token(ctx, "org-c"); got != "tok-two" {
		t.Errorf("Forget dropped another account's token: %q", got)
	}
	if len(gh.calls) != 4 {
		t.Errorf("%d gh calls, want 4", len(gh.calls))
	}
}

func TestGHTokensFailures(t *testing.T) {
	ctx := context.Background()

	t.Run("not logged in", func(t *testing.T) {
		gh := &fakeGH{tokens: map[string]string{"": "tok-active"}}
		g := newGHTokens(map[string]string{"org": "missing-acct"}, gh)
		_, err := g.Token(ctx, "org")
		if !errors.Is(err, ErrAuth) || !strings.Contains(err.Error(), "missing-acct") || !strings.Contains(err.Error(), "no oauth token") {
			t.Errorf("error = %v; want ErrAuth naming the account and gh's reason", err)
		}
		// A failure isn't cached: once gh works, so does Token.
		gh.tokens["missing-acct"] = "tok-late"
		if got, err := g.Token(ctx, "org"); err != nil || got != "tok-late" {
			t.Errorf("after login: %q, %v", got, err)
		}
	})

	t.Run("empty output", func(t *testing.T) {
		gh := &fakeGH{tokens: map[string]string{"": "  "}}
		if _, err := newGHTokens(nil, gh).Token(ctx, "x"); !errors.Is(err, ErrAuth) || !strings.Contains(err.Error(), "no token") {
			t.Errorf("error = %v; want no token", err)
		}
	})

	t.Run("account that looks like a flag", func(t *testing.T) {
		gh := &fakeGH{}
		_, err := newGHTokens(map[string]string{"org": "--hostname=evil.example"}, gh).Token(ctx, "org")
		if !errors.Is(err, ErrAuth) || !strings.Contains(err.Error(), "not a GitHub login") {
			t.Errorf("error = %v", err)
		}
		if len(gh.calls) != 0 {
			t.Errorf("gh ran with a bad account: %q", gh.calls)
		}
	})

	t.Run("gh missing", func(t *testing.T) {
		gh := &fakeGH{fail: errors.New(`exec: "gh": executable file not found in $PATH`)}
		if _, err := newGHTokens(nil, gh).Token(ctx, "x"); !errors.Is(err, ErrAuth) {
			t.Errorf("error = %v; want ErrAuth", err)
		}
	})
}

// Each gh run is bounded by ghTimeout: GHTokens sets it, and Token hands
// run a context that ends that long after the call starts. A zero timeout
// (tests only) adds no deadline. The run returns at once, so this checks
// the deadline itself without waiting for it; TestRunGH has a real gh
// ended by a timeout.
func TestGHTokensTimeout(t *testing.T) {
	g := GHTokens(map[string]string{"org": "acct"}).(*ghTokens)
	if g.timeout != ghTimeout {
		t.Fatalf("GHTokens timeout = %v, want %v", g.timeout, ghTimeout)
	}
	var deadline time.Time
	var bounded bool
	g.run = func(ctx context.Context, _ ...string) ([]byte, error) {
		deadline, bounded = ctx.Deadline()
		return []byte("tok\n"), nil
	}

	before := time.Now()
	if _, err := g.Token(context.Background(), "org"); err != nil {
		t.Fatal(err)
	}
	after := time.Now()
	if !bounded || deadline.Before(before.Add(ghTimeout)) || deadline.After(after.Add(ghTimeout)) {
		t.Errorf("gh ran with deadline %v (set: %v); want %v after the call started", deadline, bounded, ghTimeout)
	}

	g.Forget("org")
	g.timeout = 0
	if _, err := g.Token(context.Background(), "org"); err != nil {
		t.Fatal(err)
	}
	if bounded {
		t.Errorf("timeout 0: gh ran with deadline %v; want none", deadline)
	}
}

func TestFirstLine(t *testing.T) {
	if got := firstLine("\n  no oauth token found  \nmore\n"); got != "no oauth token found" {
		t.Errorf("firstLine = %q", got)
	}
	if got := firstLine(strings.Repeat("x", 500)); len(got) != 200 {
		t.Errorf("firstLine kept %d bytes", len(got))
	}
}

func TestPATFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "pat")
	if err := os.WriteFile(path, []byte("ghp_examplepat\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := PATFile(path)
	for _, owner := range []string{"org-a", "org-b"} {
		if got, err := p.Token(ctx, owner); err != nil || got != "ghp_examplepat" {
			t.Errorf("Token(%q) = %q, %v", owner, got, err)
		}
	}

	// Cached: a changed file isn't re-read until Forget.
	if err := os.WriteFile(path, []byte("ghp_rotated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, _ := p.Token(ctx, "org-a"); got != "ghp_examplepat" {
		t.Errorf("not cached: %q", got)
	}
	p.Forget("org-a")
	if got, _ := p.Token(ctx, "org-a"); got != "ghp_rotated" {
		t.Errorf("after Forget: %q", got)
	}

	// Too open: refused, like aimd's token, and the error names no secret.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	p.Forget("")
	_, err := p.Token(ctx, "org-a")
	if !errors.Is(err, ErrAuth) || !strings.Contains(err.Error(), "wider than 0600") {
		t.Errorf("mode 0644: error = %v; want ErrAuth about the mode", err)
	}
	if strings.Contains(err.Error(), "ghp_rotated") {
		t.Errorf("error %q holds the token", err)
	}

	// Missing.
	if _, err := PATFile(filepath.Join(dir, "nope")).Token(ctx, "x"); !errors.Is(err, ErrAuth) {
		t.Errorf("missing file: %v", err)
	}
}

func TestTokensFromConfig(t *testing.T) {
	cfg := config.Default()
	cfg.Owners = map[string]string{"org": "acct"}
	if g, ok := TokensFromConfig(cfg).(*ghTokens); !ok || g.accounts["org"] != "acct" {
		t.Errorf("TokensFromConfig without a PAT = %T %+v", TokensFromConfig(cfg), g)
	}
	cfg.GitHubTokenFile = "/x/pat"
	if p, ok := TokensFromConfig(cfg).(*patFile); !ok || p.path != "/x/pat" {
		t.Errorf("TokensFromConfig with a PAT = %T", TokensFromConfig(cfg))
	}
}

// With a PAT configured, the client never runs gh and sends the PAT.
func TestClientUsesPAT(t *testing.T) {
	f := newFakeGitHub(t)
	path := filepath.Join(t.TempDir(), "pat")
	if err := os.WriteFile(path, []byte("ghp_fromfile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.GitHubTokenFile = path
	cfg.Owners = map[string]string{"acme": "acct-that-gh-would-use"}
	c := New(cfg, Options{Endpoint: f.srv.URL})
	if r := c.BranchPRs(context.Background(), pivotV1).Repos[0]; r.Err != nil {
		t.Fatal(r.Err)
	}
	if tok := f.reqs()[0].Token; tok != "ghp_fromfile" {
		t.Errorf("sent token %q, want the PAT", tok)
	}

	// A PAT file that's too open: stale, not a crash, and the PAT isn't sent.
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	c = New(cfg, Options{Endpoint: f.srv.URL})
	r := c.BranchPRs(context.Background(), pivotV1).Repos[0]
	if !errors.Is(r.Err, ErrAuth) || r.StaleSince.IsZero() {
		t.Errorf("Err %v, StaleSince %v; want ErrAuth and stale", r.Err, r.StaleSince)
	}
	if n := len(f.reqs()); n != 1 {
		t.Errorf("%d requests, want no new one", n)
	}
}
