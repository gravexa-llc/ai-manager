package plans

import (
	"fmt"
	"sort"
	"strings"
)

// Plan is what Parse finds in a plan file.
type Plan struct {
	// Title is the first level-1 heading, or the first heading of any level
	// if there's no level-1 heading.
	Title string
	// Packages holds one entry per package ID, in file order.
	Packages []Package
	// Lanes holds the lanes table's rows, in file order.
	Lanes []Lane
	// Merges says who merges the plan's PRs: MergesYou (the default) or
	// MergesLead, from its Merges line (see Merges).
	Merges string
	// Warnings holds everything the parser couldn't read cleanly, sorted by
	// line.
	Warnings []Warning
}

// Package is one row of a package table.
type Package struct {
	ID    string // "A2"
	Title string // "Offline route planning", as plain text
	What  string // the What column, as written (markdown)
	Repo  string // the Repo column, as plain text
	Size  string // the Size column, as plain text
	Owner string // the Owner column, as plain text
	// Status is the Status column, as plain text (a ✅ is kept).
	Status string

	// Needs lists the packages of this plan that must be done first, in the
	// order the Needs cell names them.
	Needs []string
	// Note is the rest of the Needs cell, as written: free text such as
	// "Sam + reviewer approve" or "you: run the check", and any ID-shaped
	// word that isn't a package of this plan.
	Note string
	// NeedsYou is the text of the Needs cell's `you:` clause ("run the check"
	// for "S2; you: run the check"): a step you take for a package that
	// someone else carries out. It doesn't make the package yours.
	NeedsYou string

	// Yours is true when the Owner column says "you": the package is your
	// own step, done by a ✅ in the plan or Mark done on the dashboard.
	Yours bool
	// Done is true when the row has a ✅ anywhere, or a Status of done,
	// merged or ✅.
	Done bool

	// Lane is the name of the lane whose row lists this package, or "".
	Lane string

	Line    int    // the row's line in the file (1-based)
	Heading string // the nearest heading above the row's table
}

// Lane is one row of the lanes table.
type Lane struct {
	Name string
	// Packages lists the IDs the lane names, in order. It may include IDs
	// that aren't packages of this plan; each of those is also a warning.
	Packages []string
	// Rule is the row's other cells, as written, joined with " · ".
	Rule string
	// SingleWriter is true when the row says "single-writer" (or "single
	// writer", or "one agent at a time"): at most one agent at a time works
	// in this lane.
	SingleWriter bool

	Line    int
	Heading string
}

// WarningKind classifies a Warning.
type WarningKind string

// Warning kinds.
const (
	WarnNoID            WarningKind = "no-id"             // a package-table row without a package ID
	WarnDuplicateID     WarningKind = "duplicate-id"      // a second row with an ID already used; the first row wins
	WarnUnknownID       WarningKind = "unknown-id"        // Needs or a lane names an ID that isn't a package of this plan
	WarnLooseID         WarningKind = "loose-id"          // a Needs part starts with an ID but has other words, so it isn't read as a dependency
	WarnSelfNeed        WarningKind = "self-need"         // a package needs itself
	WarnRaggedRow       WarningKind = "ragged-row"        // a row with more or fewer cells than its header
	WarnNoLaneName      WarningKind = "no-lane-name"      // a lanes-table row with an empty Lane cell
	WarnTwoLanes        WarningKind = "two-lanes"         // a package listed in more than one lane; the first wins
	WarnNoPackageColumn WarningKind = "no-package-column" // a table with a Needs column but no package column
	WarnNoNeedsColumn   WarningKind = "no-needs-column"   // a table with a Package column but no Needs column
	WarnUnknownMerges   WarningKind = "unknown-merges"    // a Merges line whose value isn't you or lead; read as you
)

// Warning is something in the plan the parser couldn't read cleanly.
type Warning struct {
	Line    int
	Kind    WarningKind
	ID      string // the package concerned, if any
	Message string
}

func (w Warning) String() string { return fmt.Sprintf("line %d: %s", w.Line, w.Message) }

// Package returns the package with the given ID, ignoring case.
func (p Plan) Package(id string) (Package, bool) {
	for _, pkg := range p.Packages {
		if strings.EqualFold(pkg.ID, id) {
			return pkg, true
		}
	}
	return Package{}, false
}

// Column names, matched after normHeader (lowercase, no markup). A header
// may also add a parenthesized remark, as in "Needs (IDs)".
var (
	packageHeaders = []string{"package", "work package", "pkg", "id", "name", "task", "step"}
	needsHeaders   = []string{"needs", "depends on", "depends", "dependencies", "deps", "requires", "blocked by", "prerequisites"}
	whatHeaders    = []string{"what", "description", "summary", "scope"}
	repoHeaders    = []string{"repo", "repository"}
	sizeHeaders    = []string{"size", "estimate", "effort"}
	ownerHeaders   = []string{"owner", "who", "assignee"}
	statusHeaders  = []string{"status", "state"}
	laneHeaders    = []string{"lane", "lanes"}
)

// columns maps a table's roles to column indexes; -1 means absent.
type columns struct {
	pkg, needs, what, repo, size, owner, status int
	lane, members                               int // lanes tables
}

func classify(header []string) columns {
	c := columns{-1, -1, -1, -1, -1, -1, -1, -1, -1}
	for i, h := range header {
		h = normHeader(h)
		switch {
		case c.pkg < 0 && headerIs(h, packageHeaders...):
			c.pkg = i
		case c.needs < 0 && headerIs(h, needsHeaders...):
			c.needs = i
		case c.what < 0 && headerIs(h, whatHeaders...):
			c.what = i
		case c.repo < 0 && headerIs(h, repoHeaders...):
			c.repo = i
		case c.size < 0 && headerIs(h, sizeHeaders...):
			c.size = i
		case c.owner < 0 && headerIs(h, ownerHeaders...):
			c.owner = i
		case c.status < 0 && headerIs(h, statusHeaders...):
			c.status = i
		case c.lane < 0 && headerIs(h, laneHeaders...):
			c.lane = i
		case c.members < 0 && strings.Contains(h, "package"):
			c.members = i
		}
	}
	return c
}

// parser holds the state of one Parse call.
type parser struct {
	plan     Plan
	byID     map[string]int // lowercase ID → index in plan.Packages
	needs    []string       // the Needs cell of each package, by index
	laneRows []laneRow
}

type laneRow struct {
	t    *table
	r    row
	cols columns
}

// Parse reads a plan file. It never fails: whatever it can't read cleanly
// becomes a Warning, so nothing is dropped silently.
//
// Package tables are found by their header row: a package column plus a
// Needs column. A lanes table is found by its Lane column. Every other table
// is ignored. See the package documentation for the format.
func Parse(src []byte) Plan {
	doc := scan(src)
	p := &parser{byID: map[string]int{}}
	p.plan.Title = doc.title
	var mw *Warning
	if p.plan.Merges, mw = Merges(src); mw != nil {
		p.plan.Warnings = append(p.plan.Warnings, *mw)
	}

	for i := range doc.tables {
		t := &doc.tables[i]
		c := classify(t.header)
		switch {
		case c.pkg >= 0 && c.needs >= 0:
			p.packageTable(t, c)
		case c.needs >= 0:
			p.warn(t.line, WarnNoPackageColumn, "",
				fmt.Sprintf("table has a %q column but no package column (Package, Name, ID, …); not read as a package table", t.header[c.needs]))
		case c.lane >= 0:
			if c.members < 0 {
				c.members = c.pkg // "Lane | Package | Rule"
			}
			for _, r := range t.rows {
				p.laneRows = append(p.laneRows, laneRow{t, r, c})
			}
		case c.pkg >= 0 && normHeader(t.header[c.pkg]) == "package":
			p.warn(t.line, WarnNoNeedsColumn, "",
				"table has a Package column but no Needs column; not read as a package table")
		}
	}

	for i := range p.plan.Packages {
		p.resolveNeeds(&p.plan.Packages[i], p.needs[i])
	}
	for _, lr := range p.laneRows {
		p.lane(lr)
	}

	sort.SliceStable(p.plan.Warnings, func(i, j int) bool { return p.plan.Warnings[i].Line < p.plan.Warnings[j].Line })
	return p.plan
}

func (p *parser) warn(line int, kind WarningKind, id, msg string) {
	p.plan.Warnings = append(p.plan.Warnings, Warning{Line: line, Kind: kind, ID: id, Message: msg})
}

func (p *parser) ragged(t *table, r row) {
	if r.written != len(t.header) {
		p.warn(r.line, WarnRaggedRow, "",
			fmt.Sprintf("row has %d cells, its table's header has %d", r.written, len(t.header)))
	}
}

// packageTable reads one package table's rows.
func (p *parser) packageTable(t *table, c columns) {
	cell := func(r row, i int) string {
		if i < 0 {
			return ""
		}
		return r.cells[i]
	}
	for _, r := range t.rows {
		p.ragged(t, r)
		id, title, ok := parsePackageCell(cell(r, c.pkg))
		if !ok {
			p.warn(r.line, WarnNoID, "",
				fmt.Sprintf("row has no package ID: %q (the package cell should start with a bold ID and title, e.g. **A1 Title**)", clip(cell(r, c.pkg))))
			continue
		}
		if first, dup := p.byID[strings.ToLower(id)]; dup {
			p.warn(r.line, WarnDuplicateID, id,
				fmt.Sprintf("duplicate package ID %s (first used on line %d); this row is ignored", id, p.plan.Packages[first].Line))
			continue
		}
		pkg := Package{
			ID:      id,
			Title:   title,
			What:    cell(r, c.what),
			Repo:    plain(cell(r, c.repo)),
			Size:    plain(cell(r, c.size)),
			Owner:   plain(cell(r, c.owner)),
			Status:  strings.Join(strings.Fields(markupReplacer.Replace(cell(r, c.status))), " "), // plain, but keeps a ✅
			Line:    r.line,
			Heading: t.heading,
		}
		pkg.Yours = strings.EqualFold(pkg.Owner, "you")
		pkg.Done = statusDone(cell(r, c.status))
		for _, v := range r.cells {
			if strings.Contains(v, doneMark) {
				pkg.Done = true
			}
		}
		p.byID[strings.ToLower(id)] = len(p.plan.Packages)
		p.plan.Packages = append(p.plan.Packages, pkg)
		p.needs = append(p.needs, cell(r, c.needs))
	}
}

// parsePackageCell finds the ID and title in a package cell. The canonical
// form is a bold prefix whose first word is the ID: "**A2 Conflict
// handling**". Also accepted: the ID alone in bold with the title after it
// ("**A2** Conflict handling", "**A2:** …"), __bold__, a link around
// either, a ✅ anywhere, and a cell with no bold that starts with an ID.
func parsePackageCell(cell string) (id, title string, ok bool) {
	s := cell
	if strings.Contains(s, "](") {
		s = linkRe.ReplaceAllString(s, "$1")
	}
	s = strings.TrimSpace(strings.ReplaceAll(s, doneMark, ""))
	inner, rest := s, ""
	for _, d := range []string{"**", "__"} {
		if after, found := strings.CutPrefix(s, d); found {
			if end := strings.Index(after, d); end >= 0 {
				inner, rest = after[:end], after[end+len(d):]
			}
			break
		}
	}
	word, after := firstWord(inner)
	word = strings.TrimRight(strings.Trim(word, "`*_"), ":.,")
	if !IsID(word) {
		return "", "", false
	}
	title = plain(after)
	if title == "" {
		title = plain(strings.TrimLeft(strings.TrimSpace(rest), "—–-: \t"))
	}
	return word, title, true
}

// statusDone reports whether a Status cell means done: its first word is
// done, merged, complete or completed (any case), or ✅.
func statusDone(s string) bool {
	if strings.Contains(s, doneMark) {
		return true
	}
	w, _ := firstWord(strings.ToLower(plain(s)))
	switch strings.TrimRight(w, ".,;:!") {
	case "done", "merged", "complete", "completed":
		return true
	}
	return false
}

// needsPart is one comma- or semicolon-separated part of a Needs cell.
type needsPart struct {
	text   string // as written, untrimmed
	sep    string // the separator before it ("," or ";"), "" for the first
	clause int    // index of its semicolon-separated clause
}

// resolveNeeds splits a Needs cell into dependencies, the note and the
// `you:` text, once every package ID in the plan is known.
//
// The cell is split on commas and semicolons. A part that is exactly an ID
// of this plan (after removing emphasis, code markers and ✅) is a
// dependency. Every other part stays in the note, joined back with its
// original separators. A part that is ID-shaped but not a package of this
// plan is also a warning, and so is a part that starts with a known ID but
// has more words ("A1 and A2"), since that's usually a missing comma. IDs
// inside prose, as in "(its interactive part is R1)", are not dependencies.
func (p *parser) resolveNeeds(pkg *Package, cell string) {
	if isNone(cell) {
		return
	}
	var parts []needsPart
	start, sep, clause := 0, "", 0
	for i, r := range cell {
		if r == ',' || r == ';' {
			parts = append(parts, needsPart{cell[start:i], sep, clause})
			sep = string(r)
			if r == ';' {
				clause++
			}
			start = i + 1
		}
	}
	parts = append(parts, needsPart{cell[start:], sep, clause})

	var kept []needsPart
	seen := map[string]bool{}
	for _, part := range parts {
		tok := cleanToken(part.text)
		if isNone(tok) {
			continue
		}
		if IsID(tok) {
			idx, known := p.byID[strings.ToLower(tok)]
			switch {
			case !known:
				p.warn(pkg.Line, WarnUnknownID, pkg.ID,
					fmt.Sprintf("%s needs %s, which isn't a package of this plan; kept in the note", pkg.ID, tok))
			case p.plan.Packages[idx].ID == pkg.ID:
				p.warn(pkg.Line, WarnSelfNeed, pkg.ID, fmt.Sprintf("%s needs itself; kept in the note", pkg.ID))
			default:
				if id := p.plan.Packages[idx].ID; !seen[id] {
					seen[id] = true
					pkg.Needs = append(pkg.Needs, id)
				}
				continue
			}
		} else if w, more := firstWord(tok); more != "" {
			w = strings.TrimRight(w, ":.,")
			if _, known := p.byID[strings.ToLower(w)]; known && IsID(w) {
				p.warn(pkg.Line, WarnLooseID, pkg.ID,
					fmt.Sprintf("%s's Needs part %q starts with an ID but has other words, so it isn't read as a dependency; separate IDs with commas", pkg.ID, clip(tok)))
			}
		}
		kept = append(kept, part)
	}

	var note strings.Builder
	for i, part := range kept {
		if i > 0 {
			note.WriteString(part.sep)
		}
		note.WriteString(part.text)
	}
	pkg.Note = stripLeadingDash(strings.TrimSpace(note.String()))
	pkg.NeedsYou = needsYou(kept)
}

// needsYou returns the text of the `you:` clauses among the kept parts: from
// a part starting with "you:" (any case) to the end of its semicolon
// clause. Several such clauses are joined with "; ".
func needsYou(kept []needsPart) string {
	var clauses []string
	for i := 0; i < len(kept); i++ {
		tok := cleanToken(kept[i].text)
		if len(tok) < 4 || !strings.EqualFold(tok[:4], "you:") {
			continue
		}
		text := strings.TrimSpace(tok[4:])
		for i+1 < len(kept) && kept[i+1].clause == kept[i].clause {
			i++
			text += kept[i].sep + " " + plain(kept[i].text)
		}
		if text = strings.TrimSpace(text); text != "" {
			clauses = append(clauses, text)
		}
	}
	return strings.Join(clauses, "; ")
}

// lane reads one lanes-table row.
func (p *parser) lane(lr laneRow) {
	t, r, c := lr.t, lr.r, lr.cols
	p.ragged(t, r)
	l := Lane{Name: plain(r.cells[c.lane]), Line: r.line, Heading: t.heading}
	if l.Name == "" {
		p.warn(r.line, WarnNoLaneName, "", "lanes-table row has no lane name; ignored")
		return
	}
	var rule []string
	for i, v := range r.cells {
		if i == c.lane {
			continue
		}
		if c.members < 0 || i == c.members {
			l.Packages = append(l.Packages, idTokens(v)...)
		}
		if i != c.members && v != "" {
			rule = append(rule, v)
		}
	}
	l.Rule = strings.Join(rule, " · ")
	lower := strings.ToLower(strings.Join(r.cells, " "))
	l.SingleWriter = strings.Contains(lower, "single-writer") ||
		strings.Contains(lower, "single writer") ||
		strings.Contains(lower, "one agent at a time")

	seen := map[string]bool{}
	ids := l.Packages[:0]
	for _, id := range l.Packages {
		idx, known := p.byID[strings.ToLower(id)]
		if !known {
			p.warn(r.line, WarnUnknownID, id,
				fmt.Sprintf("lane %q lists %s, which isn't a package of this plan", l.Name, id))
		} else {
			id = p.plan.Packages[idx].ID
			pkg := &p.plan.Packages[idx]
			switch {
			case pkg.Lane == "":
				pkg.Lane = l.Name
			case pkg.Lane != l.Name:
				p.warn(r.line, WarnTwoLanes, id,
					fmt.Sprintf("%s is listed in lanes %q and %q; it stays in %q", id, pkg.Lane, l.Name, pkg.Lane))
			}
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	l.Packages = ids
	p.plan.Lanes = append(p.plan.Lanes, l)
}
