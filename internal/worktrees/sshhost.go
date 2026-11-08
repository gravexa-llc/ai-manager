package worktrees

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// sshTimeout bounds one `ssh -G`, which only reads config files. A var so
// the tests can raise it: on a loaded machine, starting the tests' fake ssh
// script can take longer than that, and a run cut off reads as "not GitHub".
var sshTimeout = 2 * time.Second

// Resolving SSH host aliases (see the package comment).
const (
	// hostTTL is how long a host's answer is kept: an alias's HostName
	// changes only when the user edits their ssh config.
	hostTTL = time.Hour
	// hostRetry is how long after a failed run a host is asked about
	// again. Until then it keeps its last answer, if it had one.
	hostRetry = 5 * time.Minute
	// sshOutputMax caps what is read from ssh.
	sshOutputMax = 1 << 20
)

// hostNames resolves the SSH host aliases the remotes name (SSHAliasHost)
// to their HostNames. nil when there are none, or none resolved.
func (s *Scanner) hostNames(ctx context.Context, remotes map[string]string) map[string]string {
	var out map[string]string
	for _, u := range remotes {
		host, ok := SSHAliasHost(u)
		if !ok {
			continue
		}
		key := strings.ToLower(host)
		if _, done := out[key]; done {
			continue
		}
		if name := s.hosts.hostName(ctx, host); name != "" {
			if out == nil {
				out = map[string]string{}
			}
			out[key] = name
		}
	}
	return out
}

// hostResolver asks ssh for hosts' HostNames and keeps the answers. It is
// safe for concurrent use; ssh runs for one host at a time.
type hostResolver struct {
	run func(ctx context.Context, host string) (string, error)
	now func() time.Time

	runMu sync.Mutex // held while ssh runs
	mu    sync.Mutex // guards cache
	cache map[string]hostEntry
}

// hostEntry is one host's answer.
type hostEntry struct {
	name string    // the HostName; "" if never resolved
	next time.Time // when to ask again
}

func newHostResolver(run func(ctx context.Context, host string) (string, error), now func() time.Time) *hostResolver {
	return &hostResolver{run: run, now: now, cache: map[string]hostEntry{}}
}

// hostName returns the HostName ssh's config gives host, lower-case, or
// "" when it isn't known: ssh failed, timed out or printed no hostname.
func (h *hostResolver) hostName(ctx context.Context, host string) string {
	key := strings.ToLower(host)
	if e, ok := h.cached(key); ok {
		return e.name
	}
	h.runMu.Lock()
	defer h.runMu.Unlock()
	if e, ok := h.cached(key); ok {
		return e.name // another scan asked while this one waited
	}
	name, err := h.run(ctx, host)
	if err != nil && ctx.Err() != nil {
		// The scan was cancelled, not ssh's fault: ask again next time.
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.cache[key].name
	}
	now := h.now()
	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.cache[key]
	if err != nil {
		e.next = now.Add(hostRetry) // keep the last answer meanwhile
	} else {
		e = hostEntry{name: name, next: now.Add(hostTTL)}
	}
	h.cache[key] = e
	return e.name
}

// cached returns host's entry while it is fresh.
func (h *hostResolver) cached(key string) (hostEntry, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	e, ok := h.cache[key]
	return e, ok && h.now().Before(e.next)
}

// sshHostName returns a resolver that runs `<bin> -G -- <host>` and reads
// its hostname line. ssh -G prints the configuration it would use for the
// host (Host and Match blocks applied) and exits without connecting. The
// host has been checked to look like a host name (sshHostRE), and -- ends
// ssh's options, so a remote can't pass ssh an option.
func sshHostName(bin string) func(ctx context.Context, host string) (string, error) {
	return func(ctx context.Context, host string) (string, error) {
		if !sshHostRE.MatchString(host) {
			return "", errors.New("ssh -G: not a host name")
		}
		ctx, cancel := context.WithTimeout(ctx, sshTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, "-G", "--", host)
		cmd.WaitDelay = time.Second
		var out limitedBuffer
		cmd.Stdout = &out
		if err := cmd.Run(); err != nil {
			return "", err
		}
		return parseSSHHostName(out.Bytes())
	}
}

// parseSSHHostName reads the hostname line of `ssh -G` output.
func parseSSHHostName(out []byte) (string, error) {
	for line := range strings.Lines(string(out)) {
		k, v, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok && strings.EqualFold(k, "hostname") {
			if v = strings.ToLower(strings.TrimSpace(v)); v != "" {
				return v, nil
			}
		}
	}
	return "", errors.New("ssh -G printed no hostname")
}

// limitedBuffer keeps the first sshOutputMax bytes written to it.
type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := sshOutputMax - b.Len(); room < len(p) {
		if room > 0 {
			b.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return b.Buffer.Write(p)
}
