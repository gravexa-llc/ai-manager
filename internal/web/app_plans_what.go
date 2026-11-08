package web

// A package's What cell on the plan page: the package table's What column
// (or Description, Summary, Scope) as the plan writes it, in markdown.
//
// The cell is untrusted text, so it is never marked safe. whatSpans reads
// the little markdown a table cell holds into spans that the template
// writes out, escaped: code spans, bold and italics. A link shows its text,
// a <br> breaks the line, and everything else (raw HTML, images, headings)
// is shown as the text it is.

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// mdSpan is a run of a What cell's text with one style: Code, or Bold
// and/or Italic, or plain. Break is a line break and has no text.
type mdSpan struct {
	Text               string
	Code, Bold, Italic bool
	Break              bool
}

// pkgWhat is the package pane's What: the package's ID (for its element's
// id), its spans, and Long when it is clamped to a few lines with a "more"
// toggle.
type pkgWhat struct {
	ID    string
	Spans []mdSpan
	Long  bool
}

// whatLongRunes is how long a What cell can be and still show whole: about
// four lines in the narrowest detail pane. A longer one is clamped to four
// lines (app-plan.css), with "more".
const whatLongRunes = 180

// whatTipRunes bounds the What a graph box's or a card's tooltip shows.
const whatTipRunes = 300

// pkgWhatOf is package id's What for its pane, nil when the cell is empty.
func pkgWhatOf(id, what string) *pkgWhat {
	spans := whatSpans(what)
	if len(spans) == 0 {
		return nil
	}
	breaks := 0
	for _, sp := range spans {
		if sp.Break {
			breaks++
		}
	}
	return &pkgWhat{ID: id, Spans: spans, Long: utf8.RuneCountInString(spansText(spans)) > whatLongRunes || breaks >= 3}
}

// whatTip is a What cell as plain text for a tooltip, cut at whatTipRunes.
func whatTip(what string) string {
	s := strings.Join(strings.Fields(spansText(whatSpans(what))), " ")
	if utf8.RuneCountInString(s) <= whatTipRunes {
		return s
	}
	r := []rune(s)[:whatTipRunes]
	return strings.TrimRightFunc(string(r), unicode.IsSpace) + "…"
}

// spansText is the spans' text, a break as a space.
func spansText(spans []mdSpan) string {
	var b strings.Builder
	for _, sp := range spans {
		if sp.Break {
			b.WriteByte(' ')
		}
		b.WriteString(sp.Text)
	}
	return b.String()
}

var (
	// mdLink is an inline link or image, keeping its text: [text](url).
	mdLink = regexp.MustCompile(`^!?\[([^\]]*)\]\([^)]*\)`)
	// mdBreak is a <br> in a table cell, the usual way to break a line
	// there.
	mdBreak = regexp.MustCompile(`(?i)^<br\s*/?>`)
)

// mdToken is a piece of a cell as whatSpans scans it: text, a code span, a
// line break, or a run of emphasis delimiters (* or _, one or two).
type mdToken struct {
	text  string
	code  bool
	brk   bool
	delim byte // '*' or '_'; 0 for the others
	n     int  // the run's length, 1 or 2
	open  bool // it can open emphasis, close it, or both
	close bool
	pair  int // the index of the token it pairs with, or -1
}

// whatSpans reads a What cell's inline markdown: `code` (a run of
// backticks closed by a run as long), **bold** or __bold__, *italic* or
// _italic_, backslash escapes, links as their text, and <br> as a break.
// Emphasis needs a closing delimiter of the same kind and length: a lone
// "*" or a snake_case word stays as written, and "_" doesn't work inside a
// word. Runs of three or more delimiters are text. Whitespace runs become
// one space.
func whatSpans(s string) []mdSpan {
	toks := mdTokens(strings.TrimSpace(s))
	mdPair(toks)
	var out []mdSpan
	bold, italic := false, false
	add := func(sp mdSpan) {
		if sp.Text == "" && !sp.Break {
			return
		}
		if n := len(out); n > 0 && !sp.Break && !out[n-1].Break &&
			out[n-1].Code == sp.Code && out[n-1].Bold == sp.Bold && out[n-1].Italic == sp.Italic && !sp.Code {
			out[n-1].Text += sp.Text
			return
		}
		out = append(out, sp)
	}
	for _, t := range toks {
		switch {
		case t.brk:
			add(mdSpan{Break: true})
		case t.code:
			add(mdSpan{Text: t.text, Code: true, Bold: bold, Italic: italic})
		case t.delim != 0 && t.pair >= 0:
			if t.n == 2 {
				bold = !bold
			} else {
				italic = !italic
			}
		default:
			add(mdSpan{Text: t.text, Bold: bold, Italic: italic})
		}
	}
	for i := range out {
		if !out[i].Code {
			out[i].Text = collapseSpace(out[i].Text)
		}
	}
	return out
}

// collapseSpace turns each run of whitespace into one space.
func collapseSpace(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !space {
				b.WriteByte(' ')
			}
			space = true
			continue
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

// mdTokens scans a cell into tokens.
func mdTokens(s string) []mdToken {
	var toks []mdToken
	var text strings.Builder
	flush := func() {
		if text.Len() > 0 {
			toks = append(toks, mdToken{text: text.String(), pair: -1})
			text.Reset()
		}
	}
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s) && isMDPunct(s[i+1]):
			text.WriteByte(s[i+1])
			i += 2
		case c == '`':
			n := runOf(s, i, '`')
			end := closingTicks(s, i+n, n)
			if end < 0 {
				text.WriteString(s[i : i+n])
				i += n
				continue
			}
			flush()
			code := s[i+n : end]
			if len(code) > 2 && code[0] == ' ' && code[len(code)-1] == ' ' && strings.Trim(code, " ") != "" {
				code = code[1 : len(code)-1]
			}
			toks = append(toks, mdToken{text: code, code: true, pair: -1})
			i = end + n
		case c == '[' || (c == '!' && i+1 < len(s) && s[i+1] == '['):
			m := mdLink.FindStringSubmatchIndex(s[i:])
			if m == nil {
				text.WriteByte(c)
				i++
				continue
			}
			// The link's text is read on, as part of the cell.
			s = s[:i] + s[i+m[2]:i+m[3]] + s[i+m[1]:]
		case c == '<' && mdBreak.MatchString(s[i:]):
			flush()
			toks = append(toks, mdToken{brk: true, pair: -1})
			i += len(mdBreak.FindString(s[i:]))
		case c == '*' || c == '_':
			n := runOf(s, i, c)
			if n > 2 {
				text.WriteString(s[i : i+n])
				i += n
				continue
			}
			before, _ := utf8.DecodeLastRuneInString(s[:i])
			after, _ := utf8.DecodeRuneInString(s[i+n:])
			if i == 0 {
				before = ' '
			}
			if i+n == len(s) {
				after = ' '
			}
			t := mdToken{text: s[i : i+n], delim: c, n: n, pair: -1,
				open: !unicode.IsSpace(after), close: !unicode.IsSpace(before)}
			if c == '_' { // not inside a word: snake_case stays
				t.open = t.open && !isWordRune(before)
				t.close = t.close && !isWordRune(after)
			}
			flush()
			toks = append(toks, t)
			i += n
		default:
			text.WriteByte(c)
			i++
		}
	}
	flush()
	return toks
}

// mdPair pairs emphasis delimiters: a closer with the nearest opener of
// the same kind and length before it. Openers between the two are left
// unpaired (text), so emphasis nests but never overlaps.
func mdPair(toks []mdToken) {
	var stack []int
	for i := range toks {
		t := &toks[i]
		if t.delim == 0 {
			continue
		}
		if t.close {
			for j := len(stack) - 1; j >= 0; j-- {
				o := &toks[stack[j]]
				if o.delim == t.delim && o.n == t.n {
					o.pair, t.pair = i, stack[j]
					stack = stack[:j]
					break
				}
			}
			if t.pair >= 0 {
				continue
			}
		}
		if t.open {
			stack = append(stack, i)
		}
	}
}

func isMDPunct(c byte) bool { return strings.IndexByte("\\`*_[]()<>#!|~-.+{}", c) >= 0 }

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// runOf counts the run of c starting at s[i].
func runOf(s string, i int, c byte) int {
	n := 0
	for i+n < len(s) && s[i+n] == c {
		n++
	}
	return n
}

// closingTicks finds the next run of exactly n backticks at or after
// from, and returns its start, or -1.
func closingTicks(s string, from, n int) int {
	for j := from; j < len(s); {
		if s[j] != '`' {
			j++
			continue
		}
		k := runOf(s, j, '`')
		if k == n {
			return j
		}
		j += k
	}
	return -1
}
