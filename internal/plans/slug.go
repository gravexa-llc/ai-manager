package plans

import (
	"path/filepath"
	"regexp"
	"strings"
)

var (
	// slugLine matches a line that declares the slug: "Plan slug: x", with
	// optional list or blockquote markers, emphasis and code markers.
	slugLine = regexp.MustCompile(`(?i)^(?:[>*+-]\s*)*[*_]*plan slug[*_]*\s*:[*_]*\s*` + "`?" + `([A-Za-z0-9][A-Za-z0-9._-]*)`)
	// slugInline matches "plan slug `x`" anywhere in a line, as in a
	// status line ("Status: v2.3 · … · plan slug `aim` · …").
	slugInline = regexp.MustCompile("(?i)\\bplan slug:?\\s*`([A-Za-z0-9][A-Za-z0-9._-]*)`")
)

// Slug returns the plan's slug: the name a `Plan: <slug> · <ID>` line and a
// `<slug>/<id>-…` branch use for it.
//
// An explicit marker wins: a line "Plan slug: x", or "plan slug `x`"
// anywhere in a line (a plan may say it in its status line). The
// first marker outside code blocks and HTML comments counts. Without one,
// the slug is the file name's stem minus a trailing "-plan", so
// docs/plans/sharing-plan.md is "sharing". Either way it is
// lowercased.
func Slug(path string, src []byte) string {
	r := newLineReader(src)
	for {
		l, ok := r.next()
		if !ok {
			break
		}
		if l.kind != lineText {
			continue
		}
		text := strings.TrimSpace(l.text)
		m := slugLine.FindStringSubmatch(text)
		if m == nil {
			m = slugInline.FindStringSubmatch(text)
		}
		if m != nil {
			if s := strings.TrimRight(m[1], "._-"); s != "" {
				return strings.ToLower(s)
			}
		}
	}
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	stem = strings.ToLower(stem)
	if s := strings.TrimSuffix(stem, "-plan"); s != "" {
		stem = s
	}
	return stem
}
