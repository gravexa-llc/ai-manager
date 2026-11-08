package worktrees

import "testing"

func TestParseGitHubURL(t *testing.T) {
	for _, tc := range []struct {
		url         string
		owner, name string
		ok          bool
	}{
		{"https://github.com/example-org/repo.git", "example-org", "repo", true},
		{"https://github.com/example-org/repo", "example-org", "repo", true},
		{"https://github.com/example-org/repo/", "example-org", "repo", true},
		{"http://GitHub.com/Example-Org/Repo.git", "Example-Org", "Repo", true},
		{"https://user:secret@github.com/o/n.git", "o", "n", true},
		{"git@github.com:o/n.git", "o", "n", true},
		{"git@github.com:o/n", "o", "n", true},
		{"github.com:o/n.git", "o", "n", true},
		{"ssh://git@github.com/o/n.git", "o", "n", true},
		{"ssh://git@github.com:22/o/n.git", "o", "n", true},
		{"ssh://git@ssh.github.com:443/o/n.git", "o", "n", true},
		{"git://github.com/o/n.git", "o", "n", true},
		{"git+ssh://git@github.com/o/my.repo.git", "o", "my.repo", true},
		{"  https://github.com/o/n.git\n", "o", "n", true},
		{"https://www.github.com/o/n", "o", "n", true},
		{"ext::sh -c touch% /tmp/x", "", "", false},
		{"https://github.com/-bad/n", "", "", false},
		{"https://github.com/o/..", "", "", false},
		{"https://github.com/o/n%20x", "", "", false},

		{"git@gitlab.com:o/n.git", "", "", false},
		{"https://github.example.com/o/n.git", "", "", false},
		{"git@github-work:o/n.git", "", "", false}, // an ssh alias: see ParseGitHubURLVia
		{"ssh://git@github-acme/acme/app.git", "", "", false},
		{"/srv/git/n.git", "", "", false},
		{"./relative:path", "", "", false},
		{"file:///srv/github.com/o/n.git", "", "", false},
		{"https://github.com/o", "", "", false},
		{"https://github.com/o/n/pulls", "", "", false},
		{"https://github.com//n", "", "", false},
		{"", "", "", false},
	} {
		owner, name, ok := ParseGitHubURL(tc.url)
		if owner != tc.owner || name != tc.name || ok != tc.ok {
			t.Errorf("ParseGitHubURL(%q) = %q, %q, %v; want %q, %q, %v", tc.url, owner, name, ok, tc.owner, tc.name, tc.ok)
		}
	}
}

// aliases is an ssh config as hostName sees it: github-acme and
// github-alt are Host blocks for GitHub, git-internal one for another
// server.
func aliases(host string) string {
	return map[string]string{
		"github-acme":  "github.com",
		"github-alt":   "ssh.github.com",
		"git-internal": "git.example.com",
	}[host]
}

func TestParseGitHubURLVia(t *testing.T) {
	asked := map[string]int{}
	hostName := func(host string) string {
		asked[host]++
		return aliases(host)
	}
	for _, tc := range []struct {
		url         string
		owner, name string
		ok          bool
	}{
		{"git@github-acme:acme/app.git", "acme", "app", true},
		{"github-acme:acme/app", "acme", "app", true},
		{"ssh://git@github-acme/acme/app.git", "acme", "app", true},
		{"ssh://git@github-acme:22/acme/app.git", "acme", "app", true},
		{"git+ssh://git@github-alt/example-org/web.git", "example-org", "web", true},
		{"git@github.com:acme/app.git", "acme", "app", true}, // GitHub's own: not asked
		{"git@git-internal:acme/app.git", "", "", false},     // an alias, of another server
		{"git@unknown-alias:acme/app.git", "", "", false},    // ssh doesn't know it
		{"https://github-acme/acme/app.git", "", "", false},  // not ssh: never an alias
		{"git://github-acme/acme/app.git", "", "", false},
		{"git@github-acme:acme", "", "", false}, // no repo name
		{"ext::ssh -G github-acme", "", "", false},
		{"/srv/github-acme:acme/app", "", "", false}, // a local path
	} {
		owner, name, ok := ParseGitHubURLVia(tc.url, hostName)
		if owner != tc.owner || name != tc.name || ok != tc.ok {
			t.Errorf("ParseGitHubURLVia(%q) = %q, %q, %v; want %q, %q, %v", tc.url, owner, name, ok, tc.owner, tc.name, tc.ok)
		}
	}
	for _, never := range []string{"github.com", "ext", "/srv/github-acme"} {
		if asked[never] > 0 {
			t.Errorf("hostName was asked about %q", never)
		}
	}
	if o, n, ok := ParseGitHubURLVia("git@github-acme:acme/app.git", nil); ok {
		t.Errorf("a nil hostName read an alias as %s/%s", o, n)
	}
}

func TestSSHAliasHost(t *testing.T) {
	for _, tc := range []struct {
		url, host string
		ok        bool
	}{
		{"git@github-acme:acme/app.git", "github-acme", true},
		{"ssh://git@GitHub-Acme:2222/acme/app.git", "GitHub-Acme", true},
		{"git@github.com:acme/app.git", "", false}, // GitHub's own host
		{"git@ssh.github.com:acme/app.git", "", false},
		{"https://github-acme/acme/app.git", "", false},
		{"git@github-acme:not/a/repo", "", false},
		{"git@-oProxyCommand=x:acme/app.git", "", false}, // not a host name
		{"ext::sh -c touch% /tmp/x", "", false},
		{"/srv/git/app.git", "", false},
		{"", "", false},
	} {
		host, ok := SSHAliasHost(tc.url)
		if host != tc.host || ok != tc.ok {
			t.Errorf("SSHAliasHost(%q) = %q, %v; want %q, %v", tc.url, host, ok, tc.host, tc.ok)
		}
	}
}

func TestRepoGitHubRepo(t *testing.T) {
	acme := map[string]string{"github-acme": "github.com", "git-internal": "git.example.com"}
	for _, tc := range []struct {
		desc                string
		remotes             map[string]string
		hostNames           map[string]string
		remote, owner, name string
		ok                  bool
	}{
		{"origin wins", map[string]string{"origin": "git@github.com:me/fork.git", "upstream": "https://github.com/org/repo"}, nil, "origin", "me", "fork", true},
		{"origin not GitHub", map[string]string{"origin": "git@gitlab.com:me/x.git", "zz": "https://github.com/z/z", "gh": "https://github.com/a/b"}, nil, "gh", "a", "b", true},
		{"no origin", map[string]string{"upstream": "https://github.com/org/repo"}, nil, "upstream", "org", "repo", true},
		{"none on GitHub", map[string]string{"origin": "/srv/git/x.git"}, nil, "", "", "", false},
		{"no remotes", nil, nil, "", "", "", false},
		{"origin on an alias", map[string]string{"origin": "git@github-acme:acme/app.git"}, acme, "origin", "acme", "app", true},
		{"an alias, case aside", map[string]string{"origin": "git@GitHub-Acme:acme/app.git"}, acme, "origin", "acme", "app", true},
		{"an alias not resolved", map[string]string{"origin": "git@github-acme:acme/app.git"}, nil, "", "", "", false},
		{"an alias of another server", map[string]string{"origin": "git@git-internal:acme/app.git", "up": "https://github.com/example-org/web"}, acme, "up", "example-org", "web", true},
	} {
		r := Repo{Remotes: tc.remotes, HostNames: tc.hostNames}
		remote, owner, name, ok := r.GitHubRemote()
		if remote != tc.remote || owner != tc.owner || name != tc.name || ok != tc.ok {
			t.Errorf("%s: GitHubRemote() = %q, %q, %q, %v; want %q, %q, %q, %v", tc.desc, remote, owner, name, ok, tc.remote, tc.owner, tc.name, tc.ok)
		}
		if owner, name, ok := r.GitHubRepo(); owner != tc.owner || name != tc.name || ok != tc.ok {
			t.Errorf("%s: GitHubRepo() = %q, %q, %v; want %q, %q, %v", tc.desc, owner, name, ok, tc.owner, tc.name, tc.ok)
		}
	}
}
