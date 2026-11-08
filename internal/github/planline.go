package github

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// PlanRef names a package: the plan's slug and the package ID, as written
// on a `Plan: <slug> · <ID>` line. Case is kept as written; the matcher
// normalizes it.
type PlanRef struct {
	Slug string
	ID   string
}

// IsZero reports whether r names nothing.
func (r PlanRef) IsZero() bool { return r.Slug == "" && r.ID == "" }

// String returns the canonical form, "<slug> · <ID>".
func (r PlanRef) String() string {
	if r.IsZero() {
		return ""
	}
	return r.Slug + " · " + r.ID
}

var (
	// planPrefix matches "Plan:" at the start of a line, allowing markdown
	// emphasis around the word or the colon (**Plan:**, _Plan_:).
	planPrefix = regexp.MustCompile(`(?i)^[*_]{0,3}plan[*_]{0,3}\s*:[*_]{0,3}\s*`)

	// planValue matches "<slug><sep><ID>" once separators are normalized to
	// "·". The slug is lazy so that "my-plan · B3b-2" splits at the "·", and
	// a hyphen only separates when spaced (handled in normalizeSeparators),
	// so "aim-A2" is ambiguous and doesn't match.
	planValue = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._-]*?)(\s*·[\s·]*|\s+)([A-Za-z][A-Za-z0-9._-]*)(.*)$`)

	// separatorReplacer maps the separators people (and agents) write
	// between slug and ID to "·": middle dots and bullets, HTML entities for
	// them, dashes, and ASCII stand-ins.
	separatorReplacer = strings.NewReplacer(
		"&middot;", "·", "&#183;", "·", "&#xb7;", "·", "&#xB7;", "·", "&bull;", "·",
		"•", "·", "∙", "·", "⋅", "·", "・", "·", "･", "·", "‧", "·", "●", "·",
		"–", "·", "—", "·", "―", "·",
		" - ", " · ", " -- ", " · ",
		"|", "·", "/", "·", "\\", "·", ":", "·", ",", "·", "#", "·", ";", "·",
	)
)

// ParsePlanLine finds the first `Plan: <slug> · <ID>` line in a PR body and
// returns the slug and ID. It accepts the common variants: any spacing, a
// middle dot or another separator (•, |, /, :, an en or em dash, a spaced
// hyphen, an HTML entity for the dot), markdown emphasis around "Plan:", a
// blockquote or list marker, an HTML comment, and any case for "Plan".
// Text after the ID is allowed when an explicit separator was used
// ("Plan: sharing · A2 — Token revocation"). With only a space between slug and ID
// ("Plan: aim A2") the ID must contain a digit and end the line, so prose
// such as "Plan: see the notes" doesn't match. Lines in fenced code blocks
// are skipped.
//
// The body itself is never stored: callers keep only the PlanRef.
func ParsePlanLine(body string) (PlanRef, bool) {
	inFence := false
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if ref, ok := parsePlanLine(line); ok {
			return ref, true
		}
	}
	return PlanRef{}, false
}

func parsePlanLine(line string) (PlanRef, bool) {
	line = stripLineDecoration(line)
	loc := planPrefix.FindStringIndex(line)
	if loc == nil {
		return PlanRef{}, false
	}
	rest := strings.TrimSpace(strings.Trim(line[loc[1]:], "*_` "))
	rest = strings.ReplaceAll(rest, "`", "")
	rest = strings.ReplaceAll(rest, "*", "")
	rest = separatorReplacer.Replace(rest)

	m := planValue.FindStringSubmatch(rest)
	if m == nil {
		return PlanRef{}, false
	}
	slug, sep, id, tail := m[1], m[2], strings.TrimRight(m[3], ".-_"), m[4]
	if id == "" {
		return PlanRef{}, false
	}
	// Whatever follows the ID has to start at a boundary, not continue it.
	if r, _ := utf8.DecodeRuneInString(tail); tail != "" && !strings.ContainsRune(" \t·.!?)]>", r) {
		return PlanRef{}, false
	}
	if !strings.Contains(sep, "·") {
		// Space-separated only: stricter, to keep prose out.
		if !strings.ContainsAny(id, "0123456789") {
			return PlanRef{}, false
		}
		if strings.Trim(tail, " \t.!") != "" {
			return PlanRef{}, false
		}
	}
	return PlanRef{Slug: slug, ID: id}, true
}

// stripLineDecoration removes markdown and HTML wrapping from the start and
// end of a line: blockquote and list markers, and an HTML comment.
func stripLineDecoration(line string) string {
	for {
		before := line
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, ">")
		for _, marker := range []string{"- ", "* ", "+ "} {
			line = strings.TrimPrefix(line, marker)
		}
		if strings.HasPrefix(line, "<!--") {
			line = strings.TrimSuffix(strings.TrimPrefix(line, "<!--"), "-->")
		}
		line = strings.TrimSpace(line)
		if line == before {
			return line
		}
	}
}
