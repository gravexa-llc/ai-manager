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
		{"git@github-work:o/n.git", "", "", false}, // an ssh alias: unknown host
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

func TestRepoGitHubRepo(t *testing.T) {
	for _, tc := range []struct {
		desc                string
		remotes             map[string]string
		remote, owner, name string
		ok                  bool
	}{
		{"origin wins", map[string]string{"origin": "git@github.com:me/fork.git", "upstream": "https://github.com/org/repo"}, "origin", "me", "fork", true},
		{"origin not GitHub", map[string]string{"origin": "git@gitlab.com:me/x.git", "zz": "https://github.com/z/z", "gh": "https://github.com/a/b"}, "gh", "a", "b", true},
		{"no origin", map[string]string{"upstream": "https://github.com/org/repo"}, "upstream", "org", "repo", true},
		{"none on GitHub", map[string]string{"origin": "/srv/git/x.git"}, "", "", "", false},
		{"no remotes", nil, "", "", "", false},
	} {
		r := Repo{Remotes: tc.remotes}
		remote, owner, name, ok := r.GitHubRemote()
		if remote != tc.remote || owner != tc.owner || name != tc.name || ok != tc.ok {
			t.Errorf("%s: GitHubRemote() = %q, %q, %q, %v; want %q, %q, %q, %v", tc.desc, remote, owner, name, ok, tc.remote, tc.owner, tc.name, tc.ok)
		}
		if owner, name, ok := r.GitHubRepo(); owner != tc.owner || name != tc.name || ok != tc.ok {
			t.Errorf("%s: GitHubRepo() = %q, %q, %v; want %q, %q, %v", tc.desc, owner, name, ok, tc.owner, tc.name, tc.ok)
		}
	}
}
