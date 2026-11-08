package plans

import (
	"strings"
)

// This file holds the markdown side of the parser: splitting a file into
// lines, skipping fenced code and HTML comments, tracking headings, and
// finding GFM pipe tables. It's written by hand with the standard library:
// the parser needs only headings and pipe tables with each row's line
// number, which is a few dozen lines of code, and a markdown library would
// be a new dependency that builds a full syntax tree on every .md write.

// lineKind says what a line is to the table scanner.
type lineKind int

const (
	lineText    lineKind = iota // ordinary text; may be a table row
	lineBlank                   // blank, or a code fence: ends a table
	lineHeading                 // ATX heading: ends a table
)

// mdLine is one line outside code blocks and HTML comments.
type mdLine struct {
	num   int // 1-based line number in the file
	kind  lineKind
	text  string // the line without its line ending
	level int    // heading level, for lineHeading
	head  string // heading text as plain text, for lineHeading
}

// lineReader yields a file's lines with fenced code and HTML comments
// removed. A fence's opening and closing lines come back as blank lines, so
// a fence ends a table; the code between them and comment lines are skipped
// entirely, so a commented-out table row doesn't split its table.
type lineReader struct {
	src       string // the whole file, converted once so lines are substrings
	pos       int
	num       int
	fence     string // the opening fence (e.g. "```") while inside a code block
	inComment bool
}

func newLineReader(src []byte) *lineReader {
	return &lineReader{src: strings.TrimPrefix(string(src), "\xef\xbb\xbf")}
}

// next returns the next visible line, or false at the end of the file.
func (r *lineReader) next() (mdLine, bool) {
	for r.pos < len(r.src) {
		var raw string
		if end := strings.IndexByte(r.src[r.pos:], '\n'); end < 0 {
			raw = r.src[r.pos:]
			r.pos = len(r.src)
		} else {
			raw = r.src[r.pos : r.pos+end]
			r.pos += end + 1
		}
		r.num++
		text := strings.TrimSuffix(raw, "\r")
		trimmed := strings.TrimSpace(text)

		if r.fence != "" {
			if isFenceClose(trimmed, r.fence) {
				r.fence = ""
				return mdLine{num: r.num, kind: lineBlank, text: text}, true
			}
			continue
		}
		if r.inComment {
			if strings.Contains(trimmed, "-->") {
				r.inComment = false
			}
			continue
		}
		if f := fenceOpen(trimmed); f != "" {
			r.fence = f
			return mdLine{num: r.num, kind: lineBlank, text: text}, true
		}
		if rest, ok := strings.CutPrefix(trimmed, "<!--"); ok {
			if !strings.Contains(rest, "-->") {
				r.inComment = true
			}
			continue
		}
		if trimmed == "" {
			return mdLine{num: r.num, kind: lineBlank, text: text}, true
		}
		if level, head, ok := atxHeading(trimmed); ok {
			return mdLine{num: r.num, kind: lineHeading, text: text, level: level, head: head}, true
		}
		return mdLine{num: r.num, kind: lineText, text: text}, true
	}
	return mdLine{}, false
}

// fenceOpen returns the fence marker (three or more backticks or tildes) a
// trimmed line opens with, or "".
func fenceOpen(s string) string {
	if len(s) < 3 || (s[0] != '`' && s[0] != '~') {
		return ""
	}
	n := 0
	for n < len(s) && s[n] == s[0] {
		n++
	}
	if n < 3 {
		return ""
	}
	if s[0] == '`' && strings.Contains(s[n:], "`") {
		return "" // a backtick fence's info string can't hold a backtick
	}
	return s[:n]
}

// isFenceClose reports whether a trimmed line closes the fence: the same
// character, at least as many times, and nothing else.
func isFenceClose(s, fence string) bool {
	if len(s) < len(fence) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] != fence[0] {
			return false
		}
	}
	return true
}

// atxHeading parses "# Title", "## Title ##" and so on.
func atxHeading(s string) (level int, text string, ok bool) {
	for level < len(s) && s[level] == '#' {
		level++
	}
	if level == 0 || level > 6 {
		return 0, "", false
	}
	rest := s[level:]
	if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
		return 0, "", false // "#hashtag" is text
	}
	rest = strings.TrimSpace(rest)
	// An optional closing sequence of #s, preceded by a space.
	if t := strings.TrimRight(rest, "#"); t != rest && (t == "" || strings.HasSuffix(t, " ") || strings.HasSuffix(t, "\t")) {
		rest = strings.TrimSpace(t)
	}
	return level, plain(rest), true
}

// table is one GFM pipe table.
type table struct {
	line    int      // line of the header row
	heading string   // nearest heading above the table, as plain text
	header  []string // header cells
	rows    []row
}

// row is one body row of a table.
type row struct {
	line int
	// cells holds the row's cells, trimmed, with `\|` unescaped, padded with
	// empty cells or cut to the header's width.
	cells []string
	// written is how many cells the row had as written; it differs from the
	// header's width for a ragged row.
	written int
}

// document is what scan finds in a markdown file.
type document struct {
	title  string
	tables []table
}

// scan finds the title and every pipe table in src.
func scan(src []byte) document {
	var lines []mdLine
	r := newLineReader(src)
	for {
		l, ok := r.next()
		if !ok {
			break
		}
		lines = append(lines, l)
	}

	var doc document
	doc.title = titleOf(lines)
	heading := ""
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		if l.kind == lineHeading {
			heading = l.head
			continue
		}
		if l.kind != lineText || i+1 >= len(lines) || !strings.Contains(l.text, "|") {
			continue
		}
		header := splitRow(l.text)
		if !isDelimiterRow(lines[i+1], len(header)) {
			continue
		}
		t := table{line: l.num, heading: heading, header: header}
		j := i + 2
		for ; j < len(lines) && lines[j].kind == lineText && strings.Contains(lines[j].text, "|"); j++ {
			cells := splitRow(lines[j].text)
			written := len(cells)
			for len(cells) < len(header) {
				cells = append(cells, "")
			}
			t.rows = append(t.rows, row{line: lines[j].num, cells: cells[:len(header)], written: written})
		}
		doc.tables = append(doc.tables, t)
		i = j - 1
	}
	return doc
}

// titleOf returns the text of the first non-empty level-1 heading, or of the first
// heading of any level if there is no level-1 heading.
func titleOf(lines []mdLine) string {
	first := ""
	for _, l := range lines {
		if l.kind != lineHeading || l.head == "" {
			continue
		}
		if l.level == 1 {
			return l.head
		}
		if first == "" {
			first = l.head
		}
	}
	return first
}

// isDelimiterRow reports whether l is a table's delimiter row (`|---|:--:|`)
// for a header with n cells.
func isDelimiterRow(l mdLine, n int) bool {
	if l.kind != lineText || !strings.Contains(l.text, "-") {
		return false
	}
	cells := splitRow(l.text)
	if len(cells) != n {
		return false
	}
	for _, c := range cells {
		c = strings.TrimSuffix(strings.TrimPrefix(c, ":"), ":")
		if c == "" || strings.Trim(c, "-") != "" {
			return false
		}
	}
	return true
}

// splitRow splits a table line into trimmed cells. Leading and trailing
// pipes are optional. A pipe doesn't split a cell when it's escaped (`\|`,
// which becomes `|`) or inside an inline code span (`a | b`). A run of
// backticks with no matching run later on the line is literal text, as in
// CommonMark, so a stray backtick can't swallow the rest of the row.
func splitRow(line string) []string {
	s := strings.TrimSpace(line)
	if !strings.ContainsAny(s, "\\`") {
		// Fast path: no escapes or code spans, so every pipe splits.
		s = strings.TrimPrefix(s, "|")
		if strings.HasSuffix(s, "|") {
			s = s[:len(s)-1]
		}
		cells := strings.Split(s, "|")
		for i, c := range cells {
			cells[i] = strings.TrimSpace(c)
		}
		return cells
	}
	var cells []string
	var cell strings.Builder
	i := 0
	if strings.HasPrefix(s, "|") {
		i = 1
	}
	trailingPipe := false
	for i < len(s) {
		trailingPipe = false
		switch c := s[i]; {
		case c == '\\' && i+1 < len(s):
			if s[i+1] == '|' {
				cell.WriteByte('|')
			} else {
				cell.WriteString(s[i : i+2])
			}
			i += 2
		case c == '`':
			n := runLen(s, i, '`')
			end := closingRun(s, i+n, n)
			if end < 0 {
				cell.WriteString(s[i : i+n])
				i += n
				continue
			}
			cell.WriteString(strings.ReplaceAll(s[i:end+n], `\|`, "|"))
			i = end + n
		case c == '|':
			cells = append(cells, strings.TrimSpace(cell.String()))
			cell.Reset()
			i++
			trailingPipe = i == len(s)
		default:
			cell.WriteByte(c)
			i++
		}
	}
	if !trailingPipe {
		cells = append(cells, strings.TrimSpace(cell.String()))
	}
	return cells
}

// runLen counts the run of c starting at s[i].
func runLen(s string, i int, c byte) int {
	n := 0
	for i+n < len(s) && s[i+n] == c {
		n++
	}
	return n
}

// closingRun finds the next run of exactly n backticks at or after from, and
// returns its start, or -1.
func closingRun(s string, from, n int) int {
	for j := from; j < len(s); {
		if s[j] != '`' {
			j++
			continue
		}
		k := runLen(s, j, '`')
		if k == n {
			return j
		}
		j += k
	}
	return -1
}
