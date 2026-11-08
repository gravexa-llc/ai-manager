package worktrees

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSSH writes an ssh stand-in into dir and returns its path. It
// answers `ssh -G -- <host>` from a made-up config (github-acme and
// github-alt are GitHub aliases, git-internal another server's, any other
// host is its own HostName) and appends each call's arguments to
// <path>.calls. It never reads a real ssh config.
func fakeSSH(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "ssh")
	write(t, path, `#!/bin/sh
echo "$*" >> "$0.calls"
[ "$1" = "-G" ] && [ "$2" = "--" ] || exit 255
case "$3" in
github-acme) printf 'host github-acme\nuser git\nhostname github.com\nport 22\n' ;;
github-alt) printf 'hostname ssh.github.com\nport 443\n' ;;
git-internal) printf 'hostname git.example.com\n' ;;
broken) exit 255 ;;
slow) exec sleep 10 ;;
*) printf 'hostname %s\n' "$3" ;;
esac
`)
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// sshCalls returns the fake's calls so far.
func sshCalls(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path + ".calls")
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func TestScanResolvesSSHAliases(t *testing.T) {
	parallel(t)
	requireGit(t)
	base := tempDir(t)
	root := filepath.Join(base, "root")
	for name, origin := range map[string]string{
		"app":   "git@github-acme:acme/app.git",                   // an alias of github.com
		"alt":   "ssh://git@github-alt:443/example-org/alt.git",   // of ssh.github.com
		"tools": "git@git-internal:acme/tools.git",                // of another server
		"web":   "https://github.com/example-org/web.git",         // GitHub's own: ssh isn't asked
		"lost":  "git@broken:acme/lost.git",                       // ssh fails
		"twice": "git@github-acme:acme/twice.git",                 // the same alias again
		"local": filepath.Join(base, "srv", "github-acme:acme/x"), // a path, not a host
	} {
		dir := filepath.Join(root, name)
		newRepo(t, dir, d1)
		gitT(t, dir, "remote", "add", "origin", origin)
	}
	ssh := fakeSSH(t, base)
	s := New(Options{Roots: []string{root}, SSH: ssh})

	want := map[string]string{"app": "acme/app", "alt": "example-org/alt", "web": "example-org/web", "twice": "acme/twice"}
	for range 2 { // the second scan answers from the cache
		res := s.Scan(context.Background())
		for _, r := range res.Repos {
			name := filepath.Base(r.Path)
			got := ""
			if o, n, ok := r.GitHubRepo(); ok {
				got = o + "/" + n
			}
			if got != want[name] {
				t.Errorf("%s: GitHubRepo = %q, want %q (HostNames %v)", name, got, want[name], r.HostNames)
			}
		}
		if app := repoByPath(t, res, filepath.Join(root, "app")); app.HostNames["github-acme"] != "github.com" {
			t.Errorf("app: HostNames = %v, want github-acme → github.com", app.HostNames)
		}
		if web := repoByPath(t, res, filepath.Join(root, "web")); web.HostNames != nil {
			t.Errorf("web: HostNames = %v, want none", web.HostNames)
		}
	}
	calls := sshCalls(t, ssh)
	counts := map[string]int{}
	for _, c := range calls {
		counts[c]++
	}
	wantCalls := map[string]int{"-G -- github-acme": 1, "-G -- github-alt": 1, "-G -- git-internal": 1, "-G -- broken": 1}
	if len(counts) != len(wantCalls) {
		t.Errorf("ssh calls = %q, want each of %v once", calls, wantCalls)
	}
	for c, n := range wantCalls {
		if counts[c] != n {
			t.Errorf("ssh %q ran %d times, want %d (calls %q)", c, counts[c], n, calls)
		}
	}
}

func TestSSHHostNameTimesOut(t *testing.T) {
	ssh := fakeSSH(t, tempDir(t))
	run := sshHostName(ssh)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if name, err := run(ctx, "slow"); err == nil {
		t.Errorf("a slow ssh answered %q", name)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("a slow ssh took %v to give up", took)
	}
	if name, err := run(context.Background(), "github-alt"); err != nil || name != "ssh.github.com" {
		t.Errorf("github-alt = %q, %v; want ssh.github.com", name, err)
	}
	if _, err := run(context.Background(), "-oProxyCommand=x"); err == nil {
		t.Error("an option was passed to ssh as a host")
	}
	for _, c := range sshCalls(t, ssh) {
		if strings.Contains(c, "-oProxyCommand") {
			t.Errorf("ssh ran with %q", c)
		}
	}
}

func TestParseSSHHostName(t *testing.T) {
	for _, tc := range []struct {
		out, want string
		ok        bool
	}{
		{"host github-acme\nuser git\nhostname GitHub.com\nport 22\n", "github.com", true},
		{"hostname ssh.github.com", "ssh.github.com", true},
		{"host x\nport 22\n", "", false},
		{"hostname \n", "", false},
		{"", "", false},
	} {
		got, err := parseSSHHostName([]byte(tc.out))
		if got != tc.want || (err == nil) != tc.ok {
			t.Errorf("parseSSHHostName(%q) = %q, %v; want %q, ok %v", tc.out, got, err, tc.want, tc.ok)
		}
	}
}

func TestHostResolverCaches(t *testing.T) {
	now := d1
	var mu sync.Mutex
	runs := map[string]int{}
	answers := map[string]string{"github-acme": "github.com"}
	fail := false
	h := newHostResolver(func(ctx context.Context, host string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		runs[host]++
		if fail {
			return "", errors.New("ssh failed")
		}
		return answers[host], nil
	}, func() time.Time { mu.Lock(); defer mu.Unlock(); return now })
	advance := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	ctx := context.Background()

	// Many scans at once ask ssh once.
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if got := h.hostName(ctx, "github-acme"); got != "github.com" {
				t.Errorf("hostName = %q, want github.com", got)
			}
		})
	}
	wg.Wait()
	if runs["github-acme"] != 1 {
		t.Errorf("ssh ran %d times for 8 lookups, want 1", runs["github-acme"])
	}
	if h.hostName(ctx, "GitHub-Acme") != "github.com" || runs["github-acme"] != 1 {
		t.Errorf("a lookup in another case ran ssh again: %v", runs)
	}

	// After hostTTL it asks again; a failure then keeps the last answer,
	// and the host is asked again only after hostRetry.
	advance(hostTTL)
	mu.Lock()
	fail = true
	mu.Unlock()
	if got := h.hostName(ctx, "github-acme"); got != "github.com" || runs["github-acme"] != 2 {
		t.Errorf("after a failure: hostName = %q with %d runs; want the last answer, 2 runs", got, runs["github-acme"])
	}
	advance(hostRetry - time.Second)
	h.hostName(ctx, "github-acme")
	if runs["github-acme"] != 2 {
		t.Errorf("asked again %v after a failure, before hostRetry", hostRetry-time.Second)
	}
	advance(time.Second)
	h.hostName(ctx, "github-acme")
	if runs["github-acme"] != 3 {
		t.Errorf("not asked again after hostRetry: %d runs", runs["github-acme"])
	}

	// A host that never resolved reads as unknown.
	if got := h.hostName(ctx, "nobody"); got != "" {
		t.Errorf("a failed host = %q, want empty", got)
	}

	// A cancelled scan's failure isn't kept.
	mu.Lock()
	fail = false
	answers["github-alt"] = "ssh.github.com"
	mu.Unlock()
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	h2 := newHostResolver(func(ctx context.Context, host string) (string, error) {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "ssh.github.com", nil
	}, time.Now)
	if got := h2.hostName(cctx, "github-alt"); got != "" {
		t.Errorf("a cancelled lookup = %q", got)
	}
	if got := h2.hostName(ctx, "github-alt"); got != "ssh.github.com" {
		t.Errorf("after a cancelled lookup, hostName = %q, want ssh.github.com", got)
	}
}
