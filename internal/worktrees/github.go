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

// isGitHubHost reports whether host is one of githubHosts, in any case.
func isGitHubHost(host string) bool {
	return slices.Contains(githubHosts, strings.ToLower(host))
}

// GitHub's rules for owner logins and repo names, and what an SSH host
// must look like before ssh is asked about it.
var (
	githubOwnerRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}$`)
	githubNameRE  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
	sshHostRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,252}$`)
)

// ParseGitHubURL returns the owner and repo name a GitHub remote URL points
// at. It accepts the forms git does for github.com: https://, http://,
// git://, ssh:// (with or without a port) and scp-style git@github.com:o/n,
// with or without a user, a trailing .git or a trailing slash. The host is
// matched case-insensitively, and the owner and name must follow GitHub's
// rules for logins and repo names. Any other host (GitHub Enterprise, a
// local path, an SSH alias from ~/.ssh/config) gives ok false: an alias is
// read with ParseGitHubURLVia, which Repo.GitHubRemote calls with the
// aliases the scan resolved.
//
// The names are returned as written; GitHub answers with the canonical
// nameWithOwner, which is what PRs are keyed on.
func ParseGitHubURL(rawURL string) (owner, name string, ok bool) {
	return ParseGitHubURLVia(rawURL, nil)
}

// ParseGitHubURLVia is ParseGitHubURL that also accepts an SSH remote
// (scp-style, or ssh://) on a host that isn't GitHub's, when hostName says
// the host is one of GitHub's: hostName returns the HostName the user's
// ssh config gives the host (what `ssh -G <host>` prints), or "" when it
// isn't known. That is how an alias such as git@github-work:o/n.git is
// read: a Host block whose HostName is github.com, the usual way to use
// two GitHub accounts. https:// and git:// remotes never go through
// hostName, and a nil hostName accepts GitHub's own hosts only.
func ParseGitHubURLVia(rawURL string, hostName func(host string) string) (owner, name string, ok bool) {
	host, path, ssh, ok := splitRemote(rawURL)
	if !ok {
		return "", "", false
	}
	owner, name, ok = ownerName(path)
	if !ok {
		return "", "", false
	}
	if isGitHubHost(host) {
		return owner, name, true
	}
	if ssh && hostName != nil && sshHostRE.MatchString(host) && isGitHubHost(hostName(host)) {
		return owner, name, true
	}
	return "", "", false
}

// SSHAliasHost returns the host of an SSH remote (scp-style or ssh://)
// that isn't one of GitHub's own hosts but could be an alias of one: it
// looks like a host name, and the path is a valid owner/name. These are
// the hosts the scanner asks ssh about (Repo.HostNames).
func SSHAliasHost(rawURL string) (string, bool) {
	host, path, ssh, ok := splitRemote(rawURL)
	if !ok || !ssh || isGitHubHost(host) || !sshHostRE.MatchString(host) {
		return "", false
	}
	if _, _, ok := ownerName(path); !ok {
		return "", false
	}
	return host, true
}

// splitRemote splits a remote URL into its host and path. ssh is true for
// the forms git reaches over ssh: scp-style [user@]host:path, and ssh://
// (or git+ssh://, ssh+git://). A remote helper (<transport>::<address>),
// a local path or another scheme gives ok false.
func splitRemote(rawURL string) (host, path string, ssh, ok bool) {
	s := strings.TrimSpace(rawURL)
	if i := strings.Index(s, "://"); i >= 0 {
		u, err := url.Parse(s)
		if err != nil {
			return "", "", false, false
		}
		switch strings.ToLower(u.Scheme) {
		case "ssh", "git+ssh", "ssh+git":
			ssh = true
		case "https", "http", "git":
		default:
			return "", "", false, false
		}
		return u.Hostname(), u.Path, ssh, true
	}
	if strings.Contains(s, "::") {
		return "", "", false, false // a remote helper, e.g. ext::
	}
	// scp-style: [user@]host:path. A colon after a slash is a local path.
	colon := strings.Index(s, ":")
	if colon < 0 || strings.Contains(s[:colon], "/") {
		return "", "", false, false
	}
	host, path = s[:colon], s[colon+1:]
	if at := strings.LastIndex(host, "@"); at >= 0 {
		host = host[at+1:]
	}
	return host, path, true, true
}

// ownerName reads "owner/name" from a remote's path, with slashes around
// it and a trailing .git taken off.
func ownerName(path string) (owner, name string, ok bool) {
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
// otherwise the first GitHub remote by remote name. A remote on an SSH
// alias counts when the scan resolved the alias to one of GitHub's hosts
// (HostNames). ok is false when no remote is on GitHub.
func (r *Repo) GitHubRemote() (remote, owner, name string, ok bool) {
	if u, has := r.Remotes["origin"]; has {
		if o, n, ok := ParseGitHubURLVia(u, r.hostName); ok {
			return "origin", o, n, true
		}
	}
	names := make([]string, 0, len(r.Remotes))
	for n := range r.Remotes {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, rn := range names {
		if o, n, ok := ParseGitHubURLVia(r.Remotes[rn], r.hostName); ok {
			return rn, o, n, true
		}
	}
	return "", "", "", false
}

// hostName is the HostName the scan resolved an SSH host to, or "".
func (r *Repo) hostName(host string) string {
	return r.HostNames[strings.ToLower(host)]
}
