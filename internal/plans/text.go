package plans

import (
	"regexp"
	"strings"
	"unicode"
)

// idPattern is the shape of a package ID: one to four uppercase ASCII
// letters, an optional hyphen, a digit, then letters and digits, optionally
// in further hyphen-separated groups. See IsID.
var idPattern = regexp.MustCompile(`^[A-Z]{1,4}-?[0-9][A-Za-z0-9]*(?:-[A-Za-z0-9]+)*$`)

// maxIDLen keeps IDs short, so that longer tokens such as "ABCD1234-final"
// don't read as IDs.
const maxIDLen = 10

// IsID reports whether s has the shape of a package ID: one to four
// uppercase ASCII letters, an optional hyphen, then a digit, followed by
// letters and digits, optionally in hyphen-separated groups; at most 10
// characters in all. G1, A2, R0, W3, B3b-2 and SEC-1 are IDs; x86, v1, D,
// 1A, ID and HTTPS2 are not. Case matters for the leading letters, so
// lowercase words such as "x86" or "mp3" don't read as IDs.
func IsID(s string) bool {
	return len(s) <= maxIDLen && idPattern.MatchString(s)
}

const doneMark = "✅"

var (
	// linkRe matches an inline link, keeping its text.
	linkRe = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)

	// markupReplacer removes the emphasis and code markers plain() drops.
	markupReplacer = strings.NewReplacer("**", "", "__", "", "~~", "", "`", "")
)

// plain returns s as plain text: links reduced to their text, bold,
// strikethrough and code markers removed, ✅ removed, and whitespace
// collapsed. Single `*` and `_` stay, since they're often part of a word.
func plain(s string) string {
	if strings.Contains(s, "](") {
		s = linkRe.ReplaceAllString(s, "$1")
	}
	s = markupReplacer.Replace(s)
	s = strings.ReplaceAll(s, doneMark, "")
	return strings.Join(strings.Fields(s), " ")
}

// cleanToken strips what may surround an ID in a Needs cell: whitespace,
// emphasis and code markers, ✅, and a trailing period.
func cleanToken(s string) string {
	s = strings.TrimSpace(plain(s))
	s = strings.TrimSuffix(s, ".")
	return strings.TrimSpace(s)
}

// isNone reports whether a Needs cell (or one part of it) means "nothing":
// empty, a dash (-, – or —), "none" or "n/a".
func isNone(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "-", "–", "—", "none", "n/a":
		return true
	}
	return false
}

// stripLeadingDash removes a dash and the space after it from the start of
// s: "— (its interactive part is R1)" reads as "none (…)", and the note
// keeps "(its interactive part is R1)".
func stripLeadingDash(s string) string {
	for _, d := range []string{"—", "–", "-"} {
		if rest, ok := strings.CutPrefix(s, d); ok && rest != "" && (rest[0] == ' ' || rest[0] == '\t') {
			return strings.TrimSpace(rest)
		}
	}
	return s
}

// firstWord splits s at its first run of whitespace.
func firstWord(s string) (word, rest string) {
	s = strings.TrimSpace(s)
	i := strings.IndexFunc(s, unicode.IsSpace)
	if i < 0 {
		return s, ""
	}
	return s[:i], strings.TrimSpace(s[i:])
}

// idTokens returns the ID-shaped words in s, in order: any run of letters,
// digits and hyphens that IsID accepts. "A1 → A2, T1 → Q2" gives A1, A2, T1
// and Q2.
func idTokens(s string) []string {
	var ids []string
	for _, w := range strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-'
	}) {
		if w = strings.Trim(w, "-"); IsID(w) {
			ids = append(ids, w)
		}
	}
	return ids
}

// normHeader normalizes a header cell for matching: plain text, lowercase,
// no trailing colon, single spaces.
func normHeader(s string) string {
	return strings.TrimSpace(strings.TrimSuffix(strings.ToLower(plain(s)), ":"))
}

// headerIs reports whether a normalized header is one of names, alone or
// followed by a parenthesized remark ("needs (ids)").
func headerIs(h string, names ...string) bool {
	for _, n := range names {
		if h == n || strings.HasPrefix(h, n+" (") {
			return true
		}
	}
	return false
}

// clip shortens s for a warning message.
func clip(s string) string {
	const max = 60
	if r := []rune(s); len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}
