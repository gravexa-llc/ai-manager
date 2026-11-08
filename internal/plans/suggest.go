package plans

import "strings"

// defaultHeader is the package table SuggestRow follows when the file has
// none.
var defaultHeader = []string{"Package", "What", "Size", "Needs"}

// SuggestRow is a row to paste into a plan file for a package its tables
// don't have (a provisional package), in the file's own
// format: the columns of its last package table, the one new work is most
// likely added to. The package cell is "**<ID> <title>**", with a ✅ after
// it when the work is done; every other cell is left blank for you to
// fill in. A file without a package table gets "Package | What | Size |
// Needs". A "|" in the title is escaped, and "**" dropped, so the row
// reads back as one package.
func SuggestRow(src []byte, id, title string, done bool) string {
	header := defaultHeader
	pkgCol := 0
	for _, t := range scan(src).tables {
		if c := classify(t.header); c.pkg >= 0 && c.needs >= 0 {
			header, pkgCol = t.header, c.pkg
		}
	}
	title = strings.Join(strings.Fields(strings.ReplaceAll(title, "**", "")), " ")
	cell := strings.TrimSpace(id + " " + title)
	cell = "**" + strings.ReplaceAll(cell, "|", `\|`) + "**"
	if done {
		cell += " " + doneMark
	}
	cells := make([]string, len(header))
	cells[pkgCol] = cell
	var b strings.Builder
	b.WriteString("|")
	for _, c := range cells {
		b.WriteString(" " + c + " |")
	}
	return b.String()
}
