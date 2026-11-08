package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/config"
)

func configWithOwners(owners map[string]string) config.Config {
	cfg := config.Default()
	if owners != nil {
		cfg.Owners = owners
	}
	return cfg
}

var pivotV1 = []RepoBranches{{Owner: "acme", Name: "pivot", Branches: []string{"share/v1-email-reports"}}}

func TestNewDefaults(t *testing.T) {
	c := New(configWithOwners(map[string]string{"b-org": "x", "A-org": "x"}), Options{})
	if c.endpoint != DefaultEndpoint || c.timeout != DefaultTimeout || c.maxBranches != DefaultMaxBranchesPerQuery {
		t.Errorf("defaults: endpoint %q, timeout %v, max %d", c.endpoint, c.timeout, c.maxBranches)
	}
	if _, ok := c.tokens.(*ghTokens); !ok {
		t.Errorf("tokens = %T, want gh tokens", c.tokens)
	}
	if want := []string{"A-org", "b-org"}; fmt.Sprint(c.owners) != fmt.Sprint(want) {
		t.Errorf("owners = %v, want %v", c.owners, want)
	}
	if b := c.Budget(); b.Known || b.Verdict != BudgetNormal {
		t.Errorf("Budget() before any call = %+v; want unknown, normal", b)
	}
	if h := c.Health(); h.Stale() || !h.LastOK.IsZero() {
		t.Errorf("Health() before any call = %+v", h)
	}
}

func TestIsQuery(t *testing.T) {
	for doc, want := range map[string]bool{
		"query { viewer { login } }":            true,
		"  # comment\nquery($a: String!) { x }": true,
		searchDoc:                               true,
		"mutation { addStar(input: {}) { clientMutationId } }": false,
		"{ viewer { login } }":                                 false, // shorthand: refused, we never send it
		"query { x } mutation { y }":                           false,
		"# query\nmutation { y }":                              false,
		"subscription { x }":                                   false,
		"":                                                     false,
	} {
		if got := isQuery(doc); got != want {
			t.Errorf("isQuery(%q) = %v, want %v", doc, got, want)
		}
	}
	q, _ := buildBranchQuery([]chunkRepo{{repo: 0, branches: []int{0}, owner: "o", name: "n"}},
		[]RepoPRs{{Branches: []BranchPRs{{Branch: "mutation"}}}})
	if !isQuery(q) {
		t.Errorf("a branch named %q made the query unsendable:\n%s", "mutation", q)
	}
}

// do refuses anything but a query before touching the network.
func TestDoRefusesMutation(t *testing.T) {
	f := newFakeGitHub(t)
	c := f.client(nil, nil, Options{})
	_, err := c.do(context.Background(), "o", "tok", "mutation { deleteRepository(input: {}) { clientMutationId } }", nil)
	if !errors.Is(err, ErrNotQuery) {
		t.Errorf("do(mutation) error = %v, want ErrNotQuery", err)
	}
	if n := len(f.reqs()); n != 0 {
		t.Errorf("%d requests sent", n)
	}
}

func TestRequestShape(t *testing.T) {
	f := newFakeGitHub(t)
	f.client(staticTokens("secret-token"), nil, Options{}).BranchPRs(context.Background(), pivotV1)
	reqs := f.reqs()
	if len(reqs) != 1 {
		t.Fatalf("%d requests", len(reqs))
	}
	got := reqs[0]
	if got.Path != "/" {
		t.Errorf("path %q", got.Path)
	}
	for k, want := range map[string]string{
		"Authorization": "bearer secret-token",
		"Content-Type":  "application/json",
		"Accept":        "application/json",
		"User-Agent":    "ai-manager",
	} {
		if v := got.Header.Get(k); v != want {
			t.Errorf("%s = %q, want %q", k, v, want)
		}
	}
}

// When the token can't be had, every repo comes back stale instead of the
// call failing; once auth works again they're fresh; a later failure says
// how old the data is.
func TestAuthFailureMeansStale(t *testing.T) {
	f := newFakeGitHub(t)
	toks := staticTokens("tok")
	clk := &clock{now: t0}
	c := f.client(toks, clk, Options{})
	authErr := fmt.Errorf("%w: gh auth token for gh's active account: exit status 1: no oauth token found", ErrAuth)

	// 1. Never worked: stale since the first failure.
	toks.setErr("*", authErr)
	res := c.BranchPRs(context.Background(), pivotV1)
	r := res.Repos[0]
	if !errors.Is(r.Err, ErrAuth) || !r.StaleSince.Equal(t0) || !errors.Is(r.Branches[0].Err, ErrAuth) || r.Branches[0].PRs != nil {
		t.Errorf("first failure: %+v", r)
	}
	if res.Queries != 0 || len(f.reqs()) != 0 {
		t.Errorf("sent %d queries without a token", len(f.reqs()))
	}
	h := c.Health()
	if !h.Stale() || !errors.Is(h.Err, ErrAuth) || !h.StaleSince.Equal(t0) || !h.LastOK.IsZero() {
		t.Errorf("Health() = %+v", h)
	}

	// 2. Still failing a minute later: still stale since t0.
	clk.Advance(time.Minute)
	if r := c.BranchPRs(context.Background(), pivotV1).Repos[0]; !r.StaleSince.Equal(t0) {
		t.Errorf("second failure StaleSince = %v, want %v", r.StaleSince, t0)
	}

	// 3. Auth back: fresh.
	clk.Advance(time.Minute)
	t2 := clk.Now()
	toks.setErr("*", nil)
	r = c.BranchPRs(context.Background(), pivotV1).Repos[0]
	if r.Err != nil || !r.StaleSince.IsZero() || len(r.Branches[0].PRs) != 1 {
		t.Errorf("after recovery: %+v", r)
	}
	if h := c.Health(); h.Stale() || !h.LastOK.Equal(t2) {
		t.Errorf("Health() after recovery = %+v", h)
	}

	// 4. GitHub rejects the token: stale since the last success, and the
	// cached token is dropped so the next call fetches a new one.
	clk.Advance(5 * time.Minute)
	f.acceptOnly("other")
	r = c.BranchPRs(context.Background(), pivotV1).Repos[0]
	if !errors.Is(r.Err, ErrAuth) || !strings.Contains(r.Err.Error(), "401") || !r.StaleSince.Equal(t2) {
		t.Errorf("after 401: Err %v, StaleSince %v; want ErrAuth, %v", r.Err, r.StaleSince, t2)
	}
	if len(toks.forgets) != 1 || toks.forgets[0] != "acme" {
		t.Errorf("Forget calls = %v, want [acme]", toks.forgets)
	}
	if h := c.Health(); !h.Stale() || !h.StaleSince.Equal(t2) {
		t.Errorf("Health() after 401 = %+v; want stale since %v", h, t2)
	}
}

// GitHub down or slow: stale, never a hang or a failed call.
func TestUnavailableMeansStale(t *testing.T) {
	for _, tc := range []struct {
		name   string
		handle func(w http.ResponseWriter, r *http.Request)
		// timeout is the client's per-request timeout; zero means the
		// default. Only the slow server needs a short one: a short
		// timeout on the others races their answer on a loaded machine.
		timeout time.Duration
		want    string
	}{
		{"502", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "bad gateway", http.StatusBadGateway)
		}, 0, "HTTP 502"},
		{"garbage", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "<html>unicorn</html>")
		}, 0, "decode response"},
		{"no data", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `{}`)
		}, 0, "no data"},
		{"timeout", func(w http.ResponseWriter, r *http.Request) {
			// Read the body so the server notices when the client hangs
			// up, then never answer: only the client's timeout ends the
			// call.
			_, _ = io.Copy(io.Discard, r.Body)
			<-r.Context().Done()
		}, 50 * time.Millisecond, "deadline exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(tc.handle))
			defer srv.Close()
			clk := &clock{now: t0}
			c := New(config.Config{}, Options{Endpoint: srv.URL, Tokens: staticTokens("tok-xyz"), Timeout: tc.timeout, Now: clk.Now})
			start := time.Now()
			res := c.BranchPRs(context.Background(), pivotV1)
			if d := time.Since(start); d > DefaultTimeout {
				t.Errorf("took %v", d)
			}
			r := res.Repos[0]
			if !errors.Is(r.Err, ErrUnavailable) || !strings.Contains(r.Err.Error(), tc.want) || !r.StaleSince.Equal(t0) {
				t.Errorf("Err %v, StaleSince %v; want ErrUnavailable containing %q", r.Err, r.StaleSince, tc.want)
			}
			if strings.Contains(r.Err.Error(), "tok-xyz") {
				t.Errorf("error %q holds the token", r.Err)
			}
			if h := c.Health(); !h.Stale() {
				t.Errorf("Health() = %+v, want stale", h)
			}
		})
	}

	t.Run("connection refused", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close()
		c := New(config.Config{}, Options{Endpoint: url, Tokens: staticTokens("tok-xyz")})
		res := c.OpenPRs(context.Background(), []string{"acme"})
		if st := res.Owners[0]; !errors.Is(st.Err, ErrUnavailable) || st.StaleSince.IsZero() {
			t.Errorf("owner = %+v; want ErrUnavailable and stale", st)
		}
		if strings.Contains(res.Owners[0].Err.Error(), "tok-xyz") {
			t.Errorf("error %q holds the token", res.Owners[0].Err)
		}
	})
}

func TestForbidden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"Resource protected by organization SAML enforcement."}`)
	}))
	defer srv.Close()
	c := New(config.Config{}, Options{Endpoint: srv.URL, Tokens: staticTokens("t")})
	r := c.BranchPRs(context.Background(), pivotV1).Repos[0]
	if !errors.Is(r.Err, ErrForbidden) || errors.Is(r.Err, ErrRateLimited) {
		t.Errorf("Err = %v, want ErrForbidden", r.Err)
	}
	if b := c.Budget(); b.Verdict != BudgetNormal {
		t.Errorf("a plain 403 changed the budget: %+v", b)
	}
}

func TestRedact(t *testing.T) {
	if got := redact("Post https://x/?t=abc123: dial", "abc123"); strings.Contains(got, "abc123") {
		t.Errorf("redact left the token: %q", got)
	}
	if got := redact("plain", ""); got != "plain" {
		t.Errorf("redact with no token = %q", got)
	}
}

// The caller's context still bounds the call.
func TestCallerContextCanceled(t *testing.T) {
	f := newFakeGitHub(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := f.client(nil, nil, Options{}).BranchPRs(ctx, pivotV1).Repos[0]
	if !errors.Is(r.Err, ErrUnavailable) || !strings.Contains(r.Err.Error(), "canceled") {
		t.Errorf("Err = %v; want ErrUnavailable from a canceled context", r.Err)
	}
}
