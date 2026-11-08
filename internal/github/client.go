// Package github reads pull requests from GitHub's GraphQL API for aimd and
// aim: the PRs of tracked branches (BranchPRs), the user's open PRs across
// the configured owners (OpenPRs), and the open PRs there that ask for the
// user's review (ReviewRequests).
//
// It is read-only: it only ever sends queries, built from fixed templates,
// and refuses anything else. It has no scheduler; the caller decides when
// to poll, using Budget to back off.
//
// Tokens come from gh (`gh auth token --user <account>` per owner, from the
// config's owner → account map; gh's active account for an unmapped owner)
// or from a PAT file (config.Config.GitHubTokenFile). They are cached in
// memory and never logged or put in errors.
//
// Failures don't fail a call. When GitHub, the network or auth is down,
// each affected repo or owner comes back with Err set and StaleSince saying
// how old the caller's data is, and the rest of the batch is unaffected.
// A query of PR fields without nested connections costs 1 point, however
// many repos and branches it holds.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/config"
)

// DefaultEndpoint is GitHub's GraphQL endpoint.
const DefaultEndpoint = "https://api.github.com/graphql"

// Defaults for Options.
const (
	// DefaultTimeout bounds each HTTP request. GitHub gives up on a GraphQL
	// query at 10s; this leaves room to receive its answer.
	DefaultTimeout = 20 * time.Second
	// DefaultMaxBranchesPerQuery is where BranchPRs splits a batch. All 59
	// branches in one query took 3-4.5s when measured, against GitHub's 10s
	// limit.
	DefaultMaxBranchesPerQuery = 50
)

const (
	// prsPerBranch is how many PRs are read per head branch, newest first.
	// Branch names get reused (13 PRs on one); TotalCount says when there
	// are more.
	prsPerBranch = 5
	// maxResponseBytes caps a response body (59 branches with bodies is a
	// few hundred KB).
	maxResponseBytes = 32 << 20
	userAgent        = "ai-manager"
)

// Errors, matched with errors.Is. An Err in a result wraps one of these,
// ErrAuth, or a *QueryError.
var (
	// ErrNotFound: the repo doesn't exist or the token can't see it.
	ErrNotFound = errors.New("github: not found")
	// ErrRateLimited: GitHub refused the query for rate limiting. Budget
	// then says BudgetStop.
	ErrRateLimited = errors.New("github: rate limited")
	// ErrForbidden: the token may not read this (HTTP 403, or a FORBIDDEN
	// error such as an org's SSO requirement).
	ErrForbidden = errors.New("github: forbidden")
	// ErrUnavailable: GitHub couldn't be reached or failed (network error,
	// timeout, HTTP 5xx, unreadable response).
	ErrUnavailable = errors.New("github: unavailable")
	// ErrNotQuery: the client was asked to send something other than a
	// query. It never does.
	ErrNotQuery = errors.New("github: refusing to send anything but a query")
	// ErrInvalid: the caller passed an empty or malformed owner, repo or
	// branch name.
	ErrInvalid = errors.New("github: invalid request")
)

// QueryError is one entry of a GraphQL response's errors that applies to
// part of a batch, such as a repo GitHub couldn't resolve.
type QueryError struct {
	Type    string // NOT_FOUND, FORBIDDEN, RATE_LIMITED, ... ; may be empty
	Message string
}

func (e *QueryError) Error() string {
	if e.Type == "" {
		return "github: " + e.Message
	}
	return "github: " + e.Type + ": " + e.Message
}

// Is maps GitHub's error types onto ErrNotFound, ErrForbidden and
// ErrRateLimited.
func (e *QueryError) Is(target error) bool {
	switch e.Type {
	case "NOT_FOUND":
		return target == ErrNotFound
	case "FORBIDDEN":
		return target == ErrForbidden
	case "RATE_LIMITED":
		return target == ErrRateLimited
	}
	return false
}

// Options configure a Client. The zero value uses the defaults.
type Options struct {
	// Endpoint is the GraphQL URL; DefaultEndpoint if empty. Tests point it
	// at an httptest server.
	Endpoint string
	// HTTPClient sends the requests; a plain http.Client if nil. Each request
	// is bounded by Timeout either way.
	HTTPClient *http.Client
	// Timeout bounds each HTTP request; DefaultTimeout if zero.
	Timeout time.Duration
	// Tokens supplies tokens; TokensFromConfig(cfg) if nil.
	Tokens TokenSource
	// MaxBranchesPerQuery is where BranchPRs splits a batch;
	// DefaultMaxBranchesPerQuery if zero.
	MaxBranchesPerQuery int
	// Now is the clock; time.Now if nil.
	Now func() time.Time
}

// Client reads PRs from GitHub. It is safe for concurrent use.
type Client struct {
	endpoint    string
	http        *http.Client
	timeout     time.Duration
	tokens      TokenSource
	maxBranches int
	now         func() time.Time
	owners      []string // configured owners, for OpenPRs

	rate rateTracker

	mu        sync.Mutex
	fresh     map[string]freshness // "repo:o/n" or "search:o" → freshness
	health    freshness
	lastErr   error
	canonical map[string]string // lower-case requested "owner/name" → nameWithOwner
}

// New returns a client for the owners and auth settings in cfg.
func New(cfg config.Config, opts Options) *Client {
	c := &Client{
		endpoint:    opts.Endpoint,
		http:        opts.HTTPClient,
		timeout:     opts.Timeout,
		tokens:      opts.Tokens,
		maxBranches: opts.MaxBranchesPerQuery,
		now:         opts.Now,
		fresh:       map[string]freshness{},
		canonical:   map[string]string{},
	}
	if c.endpoint == "" {
		c.endpoint = DefaultEndpoint
	}
	if c.http == nil {
		c.http = &http.Client{}
	}
	if c.timeout <= 0 {
		c.timeout = DefaultTimeout
	}
	if c.tokens == nil {
		c.tokens = TokensFromConfig(cfg)
	}
	if c.maxBranches <= 0 {
		c.maxBranches = DefaultMaxBranchesPerQuery
	}
	if c.now == nil {
		c.now = time.Now
	}
	for owner := range cfg.Owners {
		c.owners = append(c.owners, owner)
	}
	sortFold(c.owners)
	return c
}

// Budget returns the rate-limit verdict from the responses seen so far.
func (c *Client) Budget() Budget { return c.rate.budget(c.now()) }

// Health is whether the last call reached GitHub with working auth.
type Health struct {
	// LastOK is when a call last completed with no auth or transport
	// failure. Zero if none has.
	LastOK time.Time
	// StaleSince is zero while healthy. Otherwise it is LastOK, or, if no
	// call has succeeded yet, when the first one failed.
	StaleSince time.Time
	// Err is the last call's auth or transport failure; nil while healthy.
	// Repos GitHub couldn't find don't count.
	Err error
}

// Stale reports whether the last call failed.
func (h Health) Stale() bool { return h.Err != nil }

// Health reports whether the last BranchPRs, OpenPRs or ReviewRequests
// call reached GitHub.
func (c *Client) Health() Health {
	c.mu.Lock()
	defer c.mu.Unlock()
	h := Health{LastOK: c.health.lastOK, Err: c.lastErr}
	if h.Err != nil {
		h.StaleSince = c.health.staleSince()
	}
	return h
}

// Aliases returns the repos GitHub answered for under a different name
// than the one asked for, typically after a transfer: lower-case requested
// "owner/name" → the nameWithOwner GitHub returned. Later BranchPRs calls
// query the returned name.
func (c *Client) Aliases() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]string, len(c.canonical))
	for k, v := range c.canonical {
		out[k] = v
	}
	return out
}

// freshness tracks one source's last success and current failure streak.
type freshness struct {
	lastOK       time.Time
	failingSince time.Time
}

func (f freshness) staleSince() time.Time {
	if !f.lastOK.IsZero() {
		return f.lastOK
	}
	return f.failingSince
}

// markFresh records a success for key.
func (c *Client) markFresh(key string, now time.Time) {
	c.mu.Lock()
	c.fresh[key] = freshness{lastOK: now}
	c.mu.Unlock()
}

// markStale records a failure for key and returns its StaleSince.
func (c *Client) markStale(key string, now time.Time) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	f := c.fresh[key]
	if f.failingSince.IsZero() {
		f.failingSince = now
	}
	c.fresh[key] = f
	return f.staleSince()
}

// endCall records a call's overall outcome for Health. err is the call's
// first token or request failure, or nil; a repo GitHub couldn't find is
// not one.
func (c *Client) endCall(now time.Time, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		c.health = freshness{lastOK: now}
		c.lastErr = nil
		return
	}
	if c.health.failingSince.IsZero() {
		c.health.failingSince = now
	}
	c.lastErr = err
}

// gqlResponse is a GraphQL response: data keyed by top-level alias.
type gqlResponse struct {
	Data   map[string]json.RawMessage `json:"data"`
	Errors []gqlError                 `json:"errors"`
}

type gqlError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Path    []any  `json:"path"`
}

// errorAt returns the error whose path is exactly path, or nil.
func (r *gqlResponse) errorAt(path ...string) error {
	for _, e := range r.Errors {
		if len(e.Path) != len(path) {
			continue
		}
		match := true
		for i, p := range path {
			if s, ok := e.Path[i].(string); !ok || s != p {
				match = false
				break
			}
		}
		if match {
			return &QueryError{Type: e.Type, Message: e.Message}
		}
	}
	return nil
}

// generalError returns the first error without a path, or nil.
func (r *gqlResponse) generalError() error {
	for _, e := range r.Errors {
		if len(e.Path) == 0 {
			return &QueryError{Type: e.Type, Message: e.Message}
		}
	}
	return nil
}

// cost returns the response's rateLimit.cost, or 0.
func (r *gqlResponse) cost() int {
	var rl RateLimit
	if raw, ok := r.Data["rateLimit"]; ok && json.Unmarshal(raw, &rl) == nil {
		return rl.Cost
	}
	return 0
}

// isNull reports whether a field is absent or null.
func isNull(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}

var (
	graphQLComment = regexp.MustCompile(`(?m)#.*$`)
	writeOperation = regexp.MustCompile(`\b(mutation|subscription)\b`)
)

// isQuery reports whether doc is a query and nothing else: it must start
// with `query` (after comments) and must not mention mutation or
// subscription anywhere.
func isQuery(doc string) bool {
	doc = strings.TrimSpace(graphQLComment.ReplaceAllString(doc, ""))
	return strings.HasPrefix(doc, "query") && !writeOperation.MatchString(doc)
}

// do sends one query with token and returns the decoded response. owner is
// the owner the token was fetched for (to Forget it on a 401). It records
// every rate limit it sees. A response with partial data and errors is not
// an error here; the caller reads both.
func (c *Client) do(ctx context.Context, owner, token, query string, vars map[string]any) (*gqlResponse, error) {
	if !isQuery(query) {
		return nil, ErrNotQuery
	}
	payload, err := json.Marshal(struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables,omitempty"`
	}{query, vars})
	if err != nil {
		return nil, fmt.Errorf("github: encode query: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("github: %w", err)
	}
	req.Header.Set("Authorization", "bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrUnavailable, redact(err.Error(), token))
	}
	defer resp.Body.Close()
	now := c.now()
	if rl, ok := headerRateLimit(resp.Header); ok {
		c.rate.observe(rl, now)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read response: %s", ErrUnavailable, redact(err.Error(), token))
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("%w: response over %d bytes", ErrUnavailable, maxResponseBytes)
	}

	switch code := resp.StatusCode; {
	case code == http.StatusUnauthorized:
		c.tokens.Forget(owner)
		return nil, fmt.Errorf("%w: GitHub rejected the token for owner %s (HTTP 401)", ErrAuth, owner)
	case code == http.StatusForbidden || code == http.StatusTooManyRequests:
		if until, ok := retryAfter(resp.Header, now); ok {
			c.rate.backoff(until)
			return nil, fmt.Errorf("%w: secondary rate limit, retry after %s (HTTP %d)", ErrRateLimited, until.Format(time.RFC3339), code)
		}
		if resp.Header.Get("X-RateLimit-Remaining") == "0" || bytes.Contains(bytes.ToLower(body), []byte("rate limit")) {
			return nil, fmt.Errorf("%w: HTTP %d", ErrRateLimited, code)
		}
		return nil, fmt.Errorf("%w: HTTP %d", ErrForbidden, code)
	case code >= 500:
		return nil, fmt.Errorf("%w: HTTP %d", ErrUnavailable, code)
	case code != http.StatusOK:
		return nil, fmt.Errorf("github: unexpected HTTP %d", code)
	}

	var out gqlResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("%w: decode response: %v", ErrUnavailable, err)
	}
	if raw, ok := out.Data["rateLimit"]; ok {
		var rl RateLimit
		if json.Unmarshal(raw, &rl) == nil {
			c.rate.observe(rl, now)
		}
	}
	if out.Data == nil {
		// Nothing came back: the whole query failed.
		if err := out.generalError(); err != nil {
			return nil, err
		}
		if len(out.Errors) > 0 {
			e := out.Errors[0]
			return nil, &QueryError{Type: e.Type, Message: e.Message}
		}
		return nil, fmt.Errorf("%w: response has no data", ErrUnavailable)
	}
	return &out, nil
}

// redact removes the token from s, in case an error ever echoes it.
func redact(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "[token]")
}

// sortFold sorts s case-insensitively, in place.
func sortFold(s []string) {
	slices.SortFunc(s, func(a, b string) int {
		return strings.Compare(strings.ToLower(a), strings.ToLower(b))
	})
}
