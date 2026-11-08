package plans

import (
	"slices"
	"testing"
)

func TestReposFixtures(t *testing.T) {
	for _, f := range []string{"ROADMAP.md", "sharing-plan.md", "edge-cases.md", "design-doc.md"} {
		if got := Repos(readFixture(t, f)); got != nil {
			t.Errorf("Repos(%s) = %q, want nil (no Repos line)", f, got)
		}
	}
}

func TestRepos(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      []string
	}{
		{"none", "# T\n\nNo marker here.\n", nil},
		{"plain", "Repos: acme/pivot, acme/pivot-web\n", []string{"acme/pivot", "acme/pivot-web"}},
		{"bold label, code markers", "**Repos:** `o/a`, `o/b`\n", []string{"o/a", "o/b"}},
		{"in a list", "- Repos: o/a; o/b\n", []string{"o/a", "o/b"}},
		{"in a blockquote", "> repos: o/a o/b\n", []string{"o/a", "o/b"}},
		{"urls and .git", "Repos: https://github.com/o/a.git, github.com/o/b/\n", []string{"o/a", "o/b"}},
		{"repeats dropped ignoring case", "Repos: o/a, O/A, o/b\n", []string{"o/a", "o/b"}},
		{"non-repos ignored", "Repos: o/a, pivot, and o/b.\n", []string{"o/a", "o/b"}},
		{"first line wins", "Repos: o/a\nRepos: o/b\n", []string{"o/a"}},
		{"first line wins even if empty", "Repos: tbd\nRepos: o/b\n", nil},
		{"in a fence ignored", "```\nRepos: o/x\n```\nRepos: o/a\n", []string{"o/a"}},
		{"in a comment ignored", "<!--\nRepos: o/x\n-->\n", nil},
		{"in a heading ignored", "# Repos: o/x\n", nil},
		{"mid-line ignored", "The repos: o/x and o/y\n", nil},
		{"bad owner", "Repos: -bad/x, o/a\n", []string{"o/a"}},
	} {
		if got := Repos([]byte(tc.src)); !slices.Equal(got, tc.want) {
			t.Errorf("%s: Repos(%q) = %q, want %q", tc.name, tc.src, got, tc.want)
		}
	}
}
