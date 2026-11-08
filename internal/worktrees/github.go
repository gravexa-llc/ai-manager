package worktrees

import (
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// githubHosts are the hosts a GitHub remote can name: the web host (with or
// without www), and the SSH-over-443 host GitHub documents for networks
// that block port 22.
var githubHosts = []string{"github.com", "www.github.com", "ssh.github.com"}

// GitHub's rules for owner logins and repo names.
var (
	githubOwnerRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)
	githubNameRE  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
)

// ParseGitHubURL returns the owner and repo name a GitHub remote URL points
// at. It accepts the forms git does for github.com: https://, http://,
// git://, ssh:// (with or without a port) and scp-style git@github.com:o/n,
// with or without a user, a trailing .git or a trailing slash. The host is
// matched case-insensitively, and the owner and name must follow GitHub's
// rules for logins and repo names. Any other host (an SSH alias from
// ~/.ssh/config, GitHub Enterprise, a local path) gives ok false: only
// github.com is read.
//
// The names are returned as written; GitHub answers with the canonical
// nameWithOwner, which is what PRs are keyed on.
func ParseGitHubURL(rawURL string) (owner, name string, ok bool) {
	s := strings.TrimSpace(rawURL)
	var host, path string
	if i := strings.Index(s, "://"); i >= 0 {
		u, err := url.Parse(s)
		if err != nil {
			return "", "", false
		}
		switch strings.ToLower(u.Scheme) {
		case "https", "http", "ssh", "git", "git+ssh", "ssh+git":
		default:
			return "", "", false
		}
		host, path = u.Hostname(), u.Path
	} else {
		// scp-style: [user@]host:path. A colon after a slash is a local path.
		colon := strings.Index(s, ":")
		if colon < 0 || strings.Contains(s[:colon], "/") {
			return "", "", false
		}
		host, path = s[:colon], s[colon+1:]
		if at := strings.LastIndex(host, "@"); at >= 0 {
			host = host[at+1:]
		}
	}
	if !slices.Contains(githubHosts, strings.ToLower(host)) {
		return "", "", false
	}
	path = strings.Trim(path, "/")
	path = strings.TrimSuffix(path, ".git")
	owner, name, found := strings.Cut(path, "/")
	if !found || !githubOwnerRE.MatchString(owner) || !githubNameRE.MatchString(name) || name == "." || name == ".." {
		return "", "", false
	}
	return owner, name, true
}

// GitHubRepo returns the GitHub owner and name the repo's remotes point at;
// see GitHubRemote.
func (r *Repo) GitHubRepo() (owner, name string, ok bool) {
	_, owner, name, ok = r.GitHubRemote()
	return owner, name, ok
}

// GitHubRemote returns the remote that names the repo on GitHub, with the
// owner and name it points at: origin when origin is a GitHub URL,
// otherwise the first GitHub remote by remote name. ok is false when no
// remote is on github.com.
func (r *Repo) GitHubRemote() (remote, owner, name string, ok bool) {
	if u, has := r.Remotes["origin"]; has {
		if o, n, ok := ParseGitHubURL(u); ok {
			return "origin", o, n, true
		}
	}
	names := make([]string, 0, len(r.Remotes))
	for n := range r.Remotes {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, rn := range names {
		if o, n, ok := ParseGitHubURL(r.Remotes[rn]); ok {
			return rn, o, n, true
		}
	}
	return "", "", "", false
}
