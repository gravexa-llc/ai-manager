package plans

import (
	"reflect"
	"testing"
)

func TestSplitRow(t *testing.T) {
	tests := []struct {
		line string
		want []string
	}{
		{"| a | b |", []string{"a", "b"}},
		{"a | b", []string{"a", "b"}},
		{"| a | b", []string{"a", "b"}},
		{"a | b |", []string{"a", "b"}},
		{"  | a | b |  ", []string{"a", "b"}},
		{"|a|", []string{"a"}},
		{"| | |", []string{"", ""}},
		{"| a |  | c |", []string{"a", "", "c"}},
		// Escaped pipes become literal pipes.
		{`| a \| b | c |`, []string{"a | b", "c"}},
		{`| a | ends with \|`, []string{"a", "ends with |"}},
		// An escaped backslash doesn't escape the pipe after it.
		{`| a \\| b |`, []string{`a \\`, "b"}},
		// Other escapes are kept as written.
		{`| \*a\* | b |`, []string{`\*a\*`, "b"}},
		// Pipes inside code spans don't split, escaped or not.
		{"| `a | b` | c |", []string{"`a | b`", "c"}},
		{"| `a \\| b` | c |", []string{"`a | b`", "c"}},
		{"| ``a ` | b`` | c |", []string{"``a ` | b``", "c"}},
		{"| `x` | `y` |", []string{"`x`", "`y`"}},
		// An escaped backtick doesn't open a code span.
		{"| \\`a | b` |", []string{"\\`a", "b`"}},
		// A backtick with no closing run is literal.
		{"| `unclosed | b |", []string{"`unclosed", "b"}},
		{"| ``two | `one` |", []string{"``two", "`one`"}},
		// Non-ASCII text survives.
		{"| **A1 Café** ✅ | — |", []string{"**A1 Café** ✅", "—"}},
	}
	for _, tt := range tests {
		if got := splitRow(tt.line); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("splitRow(%q) = %q, want %q", tt.line, got, tt.want)
		}
	}
}

func TestScanFindsTables(t *testing.T) {
	md := "# Title\n" + // 1
		"\n" + // 2
		"## Section one\n" + // 3
		"| A | B | C |\n" + // 4
		"|:---|:---:|---:|\n" + // 5
		"| 1 | 2 | 3 |\n" + // 6
		"| short |\n" + // 7
		"| x | y | z | extra |\n" + // 8
		"\n" + // 9
		"| not | a | row |\n" + // 10: after a blank line, without a delimiter row
		"\n" + // 11
		"### Section two ###\n" + // 12
		"H1 | H2\n" + // 13: no outer pipes
		"--|--\n" + // 14
		"a | b\n" + // 15
		"text without a pipe ends the table\n" + // 16
		"| c | d |\n" // 17
	doc := scan([]byte(md))
	if doc.title != "Title" {
		t.Errorf("title = %q", doc.title)
	}
	if len(doc.tables) != 2 {
		t.Fatalf("found %d tables, want 2: %+v", len(doc.tables), doc.tables)
	}

	t1 := doc.tables[0]
	if t1.line != 4 || t1.heading != "Section one" || !reflect.DeepEqual(t1.header, []string{"A", "B", "C"}) {
		t.Errorf("table 1 = line %d, heading %q, header %q", t1.line, t1.heading, t1.header)
	}
	wantRows := []row{
		{line: 6, cells: []string{"1", "2", "3"}, written: 3},
		{line: 7, cells: []string{"short", "", ""}, written: 1},
		{line: 8, cells: []string{"x", "y", "z"}, written: 4},
	}
	if !reflect.DeepEqual(t1.rows, wantRows) {
		t.Errorf("table 1 rows = %+v, want %+v", t1.rows, wantRows)
	}

	t2 := doc.tables[1]
	if t2.line != 13 || t2.heading != "Section two" || len(t2.rows) != 1 || t2.rows[0].line != 15 {
		t.Errorf("table 2 = %+v", t2)
	}
}

func TestScanRejectsNonTables(t *testing.T) {
	for name, md := range map[string]string{
		"no delimiter row":          "| a | b |\n| c | d |\n",
		"delimiter width differs":   "| a | b |\n|---|\n| c | d |\n",
		"delimiter has text":        "| a | b |\n|---|x--|\n",
		"delimiter without dashes":  "| a | b |\n| : | : |\n",
		"setext heading":            "Title\n---\n",
		"delimiter before a header": "|---|---|\n| a | b |\n",
	} {
		if doc := scan([]byte(md)); len(doc.tables) != 0 {
			t.Errorf("%s: found a table: %+v", name, doc.tables)
		}
	}
}

func TestScanSkipsCodeAndComments(t *testing.T) {
	md := "| P | N |\n" + // 1
		"|---|---|\n" + // 2
		"| a | 1 |\n" + // 3
		"<!-- | hidden | row | -->\n" + // 4: skipped, the table goes on
		"<!--\n" + // 5
		"| also | hidden |\n" + // 6
		"-->\n" + // 7
		"| b | 2 |\n" + // 8
		"```\n" + // 9: a fence ends the table
		"| c | 3 |\n" + // 10
		"```\n" + // 11
		"| d | 4 |\n" + // 12: not a table (no header + delimiter)
		"\n" + // 13
		"~~~~ md\n" + // 14
		"```\n" + // 15: a shorter, different fence doesn't close it
		"# Not a heading\n" + // 16
		"| X | Y |\n" + // 17
		"|---|---|\n" + // 18
		"~~~~\n" + // 19
		"    ```go\n" + // 20: indented fences count too
		"| X | Y |\n" + // 21
		"|---|---|\n" + // 22
		"    ```\n" + // 23
		"# Real title\n" // 24
	doc := scan([]byte(md))
	if len(doc.tables) != 1 {
		t.Fatalf("found %d tables, want 1: %+v", len(doc.tables), doc.tables)
	}
	var lines []int
	for _, r := range doc.tables[0].rows {
		lines = append(lines, r.line)
	}
	if !reflect.DeepEqual(lines, []int{3, 8}) {
		t.Errorf("row lines = %v, want [3 8]", lines)
	}
	if doc.title != "Real title" {
		t.Errorf("title = %q, want the heading outside the fences", doc.title)
	}
}

func TestScanLineEndingsAndBOM(t *testing.T) {
	md := "\xef\xbb\xbf# Title\r\n\r\n| P | N |\r\n|---|---|\r\n| a | b |\r\n"
	doc := scan([]byte(md))
	if doc.title != "Title" {
		t.Errorf("title = %q", doc.title)
	}
	if len(doc.tables) != 1 {
		t.Fatalf("found %d tables", len(doc.tables))
	}
	r := doc.tables[0].rows[0]
	if r.line != 5 || !reflect.DeepEqual(r.cells, []string{"a", "b"}) {
		t.Errorf("row = %+v", r)
	}
}

func TestTitle(t *testing.T) {
	tests := []struct{ md, want string }{
		{"# Plan\n## Later\n", "Plan"},
		{"## Section\n# Plan\n", "Plan"},
		{"## Only a section\n### Sub\n", "Only a section"},
		{"# **Bold** `code` [link](x) #\n", "Bold code link"},
		{"#Not a heading\n", ""},
		{"####### Seven\n", ""},
		{"#\n# Second\n", "Second"}, // empty headings are skipped
		{"```\n# In code\n```\n# Out\n", "Out"},
		{"No headings at all\n", ""},
	}
	for _, tt := range tests {
		if got := scan([]byte(tt.md)).title; got != tt.want {
			t.Errorf("title of %q = %q, want %q", tt.md, got, tt.want)
		}
		if got := quickTitle([]byte(tt.md)); got != tt.want {
			t.Errorf("quickTitle of %q = %q, want %q", tt.md, got, tt.want)
		}
	}
}
