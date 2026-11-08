package state

import (
	"regexp"
	"strings"
)

// canonID folds a package ID for comparison: upper case, without the
// optional hyphen after its leading letters, so "sec-1", "SEC-1" and
// "SEC1" are all "SEC1", and "b-3b-2" and "B3b-2" are both "B3B-2".
func canonID(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	i := 0
	for i < len(s) && s[i] >= 'A' && s[i] <= 'Z' {
		i++
	}
	if i > 0 && i+1 < len(s) && s[i] == '-' {
		s = s[:i] + s[i+1:]
	}
	return s
}

// titleIDRE matches a trailing "(ID)" in a PR title, in any case, so
// "Sharing notifications (V1)" gives V1. The shape is plans.IsID's.
var titleIDRE = regexp.MustCompile(`\(\s*([A-Za-z]{1,4}-?[0-9][A-Za-z0-9]*(?:-[A-Za-z0-9]+)*)\s*\)\s*$`)

// titleID returns the ID in a PR title's trailing parentheses.
func titleID(title string) (string, bool) {
	m := titleIDRE.FindStringSubmatch(strings.TrimSpace(title))
	if m == nil || len(m[1]) > 10 {
		return "", false
	}
	return m[1], true
}

func isAlnum(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}

// branchWords splits a branch name into lowercase words at every character
// that isn't an ASCII letter or digit: "share/v1-email-reports" is share,
// v1, email, reports.
func branchWords(branch string) []string {
	return strings.FieldsFunc(strings.ToLower(branch), func(r rune) bool { return !isAlnum(r) })
}

// idForms returns the word sequences a package ID can appear as in a
// branch name split by branchWords: its hyphen-separated groups, and the
// same with the leading letters split off. V1 is [v1] or [v, 1]; B3b-2 is
// [b3b, 2] or [b, 3b, 2], so "surge-b-3b-2" names B3b-2.
func idForms(id string) [][]string {
	groups := strings.Split(strings.ToLower(canonID(id)), "-")
	forms := [][]string{groups}
	g := groups[0]
	i := 0
	for i < len(g) && g[i] >= 'a' && g[i] <= 'z' {
		i++
	}
	if i > 0 && i < len(g) {
		alt := append([]string{g[:i], g[i:]}, groups[1:]...)
		forms = append(forms, alt)
	}
	return forms
}

// containsWords reports whether seq appears in words as consecutive
// elements.
func containsWords(words, seq []string) bool {
	if len(seq) == 0 {
		return false
	}
outer:
	for i := 0; i+len(seq) <= len(words); i++ {
		for j, w := range seq {
			if words[i+j] != w {
				continue outer
			}
		}
		return true
	}
	return false
}

// namesID reports whether a branch, split into words, names the package
// ID as a token.
func namesID(words []string, id string) bool {
	for _, f := range idForms(id) {
		if containsWords(words, f) {
			return true
		}
	}
	return false
}

// leadingIDs returns the prefixes of s that could be an ID followed by a
// separator, longest first: for "b3b-2-fix" they are "b3b-2-fix",
// "b3b-2" and "b3b". A prefix ends where a '-' or '/' follows it.
func leadingIDs(s string) []string {
	var out []string
	out = append(out, s)
	for i := len(s) - 1; i > 0; i-- {
		if s[i] == '-' || s[i] == '/' {
			out = append(out, s[:i])
		}
	}
	return out
}
