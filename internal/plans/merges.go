package plans

import (
	"fmt"
	"regexp"
	"strings"
)

// Who merges a plan's PRs: the values of its Merges line.
const (
	// MergesYou: you merge the plan's PRs. A PR that is ready to merge
	// needs you. It is the default.
	MergesYou = "you"
	// MergesLead: the plan's lead session merges its agents' PRs, so a
	// ready PR waits on the lead, not on you.
	MergesLead = "lead"
)

// mergesLine matches a line that says who merges the plan's PRs: "Merges:
// lead", with optional list or blockquote markers, emphasis and code
// markers around the label. The value is the rest of the line.
var mergesLine = regexp.MustCompile(`(?i)^(?:[>*+-]\s*)*[*_]*merges[*_]*\s*:[*_]*\s*(.*)$`)

// Merges returns who merges the plan's PRs, from its line "Merges: you" or
// "Merges: lead": MergesYou or MergesLead.
//
// The line may sit in a list or a blockquote, the label may be bold
// ("**Merges:**") and the value may be in code markers or bold, with a
// remark after it ("Merges: lead (the lead session reviews and merges)").
// The value is read without regard to case. The first such line outside
// code blocks, HTML comments and headings counts.
//
// A plan without the line gives MergesYou. A line whose value is neither
// "you" nor "lead" also gives MergesYou, with a warning saying so, which
// Parse adds to the plan's warnings: an unknown value is never a silent
// default.
func Merges(src []byte) (string, *Warning) {
	r := newLineReader(src)
	for {
		l, ok := r.next()
		if !ok {
			return MergesYou, nil
		}
		if l.kind != lineText {
			continue
		}
		m := mergesLine.FindStringSubmatch(strings.TrimSpace(l.text))
		if m == nil {
			continue
		}
		word, _ := firstWord(plain(m[1]))
		word = strings.ToLower(strings.Trim(word, "*_`'\".,;:!()[]"))
		switch word {
		case MergesYou, MergesLead:
			return word, nil
		}
		msg := `the Merges line has no value; write "Merges: you" or "Merges: lead". Read as "you"`
		if v := strings.TrimSpace(plain(m[1])); v != "" {
			msg = fmt.Sprintf(`the Merges line says %q; write "Merges: you" or "Merges: lead". Read as "you"`, clip(v))
		}
		return MergesYou, &Warning{Line: l.num, Kind: WarnUnknownMerges, Message: msg}
	}
}
