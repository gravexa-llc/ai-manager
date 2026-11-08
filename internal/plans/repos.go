package plans

import (
	"regexp"
	"strings"
)

var (
	// reposLine matches a line that lists the plan's repos: "Repos: a/b,
	// c/d", with optional list or blockquote markers, emphasis and code
	// markers around the label. The list is the rest of the line.
	reposLine = regexp.MustCompile(`(?i)^(?:[>*+-]\s*)*[*_]*repos[*_]*\s*:[*_]*\s*(.*)$`)
	// repoName is GitHub's owner/name shape: an owner login (letters,
	// digits, hyphens; not starting with a hyphen) and a repo name.
	repoName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`)
)

// Repos returns the GitHub repos a plan's line "Repos: owner/name,
// owner/name" lists, in order, without repeats (compared ignoring case).
//
// The line may sit in a list or a blockquote, and the label may be bold
// ("**Repos:**"). Entries are separated by commas, semicolons or spaces;
// code markers, a "https://github.com/" prefix and a trailing ".git" are
// dropped, and anything that isn't owner/name is ignored. The first such
// line outside code blocks, HTML comments and headings counts, even when
// none of its entries is a repo. A plan without one gives nil: aimd then
// registers it for the repo the file is in.
//
// The line names every repo the plan's packages live in, the plan's own
// repo included when packages live there too.
func Repos(src []byte) []string {
	r := newLineReader(src)
	for {
		l, ok := r.next()
		if !ok {
			return nil
		}
		if l.kind != lineText {
			continue
		}
		m := reposLine.FindStringSubmatch(strings.TrimSpace(l.text))
		if m == nil {
			continue
		}
		return repoList(m[1])
	}
}

// repoList splits the list part of a Repos line into owner/name entries.
func repoList(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t'
	}) {
		f = strings.Trim(f, "`*_()[]<>")
		f = strings.TrimSuffix(f, ".")
		for _, p := range []string{"https://github.com/", "http://github.com/", "github.com/"} {
			if len(f) > len(p) && strings.EqualFold(f[:len(p)], p) {
				f = f[len(p):]
			}
		}
		f = strings.TrimSuffix(strings.TrimSuffix(f, "/"), ".git")
		if !repoName.MatchString(f) || seen[strings.ToLower(f)] {
			continue
		}
		seen[strings.ToLower(f)] = true
		out = append(out, f)
	}
	return out
}
