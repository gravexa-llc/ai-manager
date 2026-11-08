package github

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/config"
)

// TokenSource supplies the token used to read an owner's repos. Tokens are
// cached in memory; they never appear in errors or logs.
type TokenSource interface {
	// Token returns the token for reading owner's repos.
	Token(ctx context.Context, owner string) (string, error)
	// Forget drops the cached token used for owner, after GitHub rejected
	// it, so the next Token call fetches it again.
	Forget(owner string)
}

// TokensFromConfig returns the token source cfg describes: the PAT in
// cfg.GitHubTokenFile when set, otherwise gh's token for each owner's
// account in cfg.Owners (gh's active account for unmapped owners).
func TokensFromConfig(cfg config.Config) TokenSource {
	if cfg.GitHubTokenFile != "" {
		return PATFile(cfg.GitHubTokenFile)
	}
	return GHTokens(cfg.Owners)
}

// ghTimeout bounds one `gh auth token` run.
const ghTimeout = 10 * time.Second

// githubHost is the only host aim reads. Passed to gh explicitly so GH_HOST
// can't point it elsewhere.
const githubHost = "github.com"

// loginRE matches a GitHub login, so an account from config can't be read
// by gh as a flag.
var loginRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)

// GHTokens returns a token source that runs `gh auth token --user <account>`
// for an owner mapped to an account in owners (matched case-insensitively),
// and `gh auth token` (gh's active account) for any other owner. Each
// account's token is fetched once and cached until Forget.
func GHTokens(owners map[string]string) TokenSource {
	g := &ghTokens{
		accounts: make(map[string]string, len(owners)),
		cache:    map[string]string{},
		run:      runGH,
		timeout:  ghTimeout,
	}
	for owner, account := range owners {
		g.accounts[strings.ToLower(owner)] = account
	}
	return g
}

type ghTokens struct {
	accounts map[string]string // lower-case owner → account
	run      func(ctx context.Context, args ...string) ([]byte, error)
	// timeout bounds each run; GHTokens sets ghTimeout. Zero means no
	// bound but the caller's context: tests that aren't about the timeout
	// use it, so they don't race gh's start on a loaded machine.
	timeout time.Duration

	mu    sync.Mutex
	cache map[string]string // account ("" = active) → token
}

func (g *ghTokens) account(owner string) string {
	return g.accounts[strings.ToLower(owner)]
}

func (g *ghTokens) Token(ctx context.Context, owner string) (string, error) {
	account := g.account(owner)
	g.mu.Lock()
	tok, ok := g.cache[account]
	g.mu.Unlock()
	if ok {
		return tok, nil
	}

	args := []string{"auth", "token", "--hostname", githubHost}
	who := "gh's active account"
	if account != "" {
		if !loginRE.MatchString(account) {
			return "", fmt.Errorf("%w: owner %s: account %q in [owners] is not a GitHub login", ErrAuth, owner, account)
		}
		args = append(args, "--user="+account)
		who = "account " + account
	}
	if g.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, g.timeout)
		defer cancel()
	}
	out, err := g.run(ctx, args...)
	if err != nil {
		return "", fmt.Errorf("%w: gh auth token for %s (owner %s): %v", ErrAuth, who, owner, err)
	}
	tok = strings.TrimSpace(string(out))
	if tok == "" || strings.ContainsAny(tok, " \t\n") {
		return "", fmt.Errorf("%w: gh auth token for %s (owner %s) printed no token", ErrAuth, who, owner)
	}

	g.mu.Lock()
	g.cache[account] = tok
	g.mu.Unlock()
	return tok, nil
}

func (g *ghTokens) Forget(owner string) {
	g.mu.Lock()
	delete(g.cache, g.account(owner))
	g.mu.Unlock()
}

// runGH runs gh and returns its stdout. On failure the error carries gh's
// stderr (which holds a reason, never the token) and not its stdout.
func runGH(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := firstLine(stderr.String()); msg != "" {
			return nil, fmt.Errorf("%v: %s", err, msg)
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

// firstLine returns the first non-empty line of s, cut to 200 bytes.
func firstLine(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			if len(line) > 200 {
				line = line[:200]
			}
			return line
		}
	}
	return ""
}

// PATFile returns a token source that reads a personal access token from
// path, for every owner. The file is checked like aimd's token
// (config.ReadSecretFile: regular file, mode 0600 or narrower, not empty),
// read on first use and cached until Forget.
func PATFile(path string) TokenSource {
	return &patFile{path: path}
}

type patFile struct {
	path string

	mu  sync.Mutex
	tok string
}

func (p *patFile) Token(context.Context, string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tok != "" {
		return p.tok, nil
	}
	tok, err := config.ReadSecretFile(p.path)
	if err != nil {
		return "", fmt.Errorf("%w: github_token_file: %v", ErrAuth, err)
	}
	p.tok = tok
	return tok, nil
}

func (p *patFile) Forget(string) {
	p.mu.Lock()
	p.tok = ""
	p.mu.Unlock()
}

// ErrAuth means no usable token: gh failed or isn't logged in to the
// account, the PAT file is missing or too open, or GitHub rejected the token
// (HTTP 401).
var ErrAuth = errors.New("github: auth")
