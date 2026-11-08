package plans

import (
	"reflect"
	"strings"
	"testing"
)

// kinds returns the kinds of p's warnings, in order.
func kinds(p Plan) []WarningKind {
	var k []WarningKind
	for _, w := range p.Warnings {
		k = append(k, w.Kind)
	}
	return k
}

func ids(p Plan) []string {
	var s []string
	for _, pkg := range p.Packages {
		s = append(s, pkg.ID)
	}
	return s
}

func TestIsID(t *testing.T) {
	for _, s := range []string{"G1", "A2", "W3", "T1", "V1", "R0", "S1", "Q4", "C7", "B3b-2", "B3b", "AB12", "ABCD1", "A-1", "SEC-1", "C1-2a", "A10"} {
		if !IsID(s) {
			t.Errorf("IsID(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "A", "ID", "1A", "12", "pivot", "v1", "a1", "mp3", "ABCDE1", "A1-", "A1--2", "A1_b", "A1.2", "A 1", "-A1", "Ä1", "A1234567890", "Sharing"} {
		if IsID(s) {
			t.Errorf("IsID(%q) = true, want false", s)
		}
	}
}

func TestPackageTableHeaders(t *testing.T) {
	tests := []struct {
		header   string
		isTable  bool
		warnKind WarningKind // expected warning for a table that isn't read, if any
	}{
		{"| Package | Needs |", true, ""},
		{"| package | needs |", true, ""},
		{"| PACKAGE | NEEDS |", true, ""},
		{"| **Package** | **Needs** |", true, ""},
		{"| Package | Needs: |", true, ""},
		{"| Package | Needs (IDs) |", true, ""},
		{"| Package | Depends on |", true, ""},
		{"| Package | Depends On |", true, ""},
		{"| Package | Depends |", true, ""},
		{"| Name | Dependencies |", true, ""},
		{"| ID | Deps |", true, ""},
		{"| Task | Requires |", true, ""},
		{"| Step | Blocked by |", true, ""},
		{"| Work package | Prerequisites |", true, ""},
		{"| Pkg | Needs |", true, ""},
		// No dependency column: never a package table.
		{"| Package | What |", false, WarnNoNeedsColumn},
		{"| Name | What |", false, ""},
		{"| # | Decision |", false, ""},
		{"| # | Answered by |", false, ""},
		{"| Option | After |", false, ""},
		// A dependency column but no package column.
		{"| Signal | Needs |", false, WarnNoPackageColumn},
		{"| Packages | Needs |", false, WarnNoPackageColumn},
	}
	for _, tt := range tests {
		md := tt.header + "\n|---|---|\n| **A1 One** | — |\n| **A2 Two** | A1 |\n"
		p := Parse([]byte(md))
		if got := len(p.Packages) > 0; got != tt.isTable {
			t.Errorf("%s: read as a package table = %v, want %v", tt.header, got, tt.isTable)
		}
		var want []WarningKind
		if tt.warnKind != "" {
			want = []WarningKind{tt.warnKind}
		}
		if got := kinds(p); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: warnings %v, want %v", tt.header, got, want)
		}
	}
}

func TestOptionalColumns(t *testing.T) {
	md := "| Package | Description | Repository | Estimate | Who | State | Blocked by | Extra |\n" +
		"|---|---|---|---|---|---|---|---|\n" +
		"| **A1 One** | Does `x` | **pivot** | M | Sam | In review | — | ignored |\n"
	p := Parse([]byte(md))
	want := Package{
		ID: "A1", Title: "One", What: "Does `x`", Repo: "pivot", Size: "M", Owner: "Sam", Status: "In review",
		Line: 3,
	}
	if len(p.Packages) != 1 || !reflect.DeepEqual(p.Packages[0], want) {
		t.Errorf("got %+v\nwant %+v", p.Packages, want)
	}

	// Columns may come in any order, and only Package and Needs are required.
	p = Parse([]byte("| Needs | Package |\n|---|---|\n| — | **A1 One** |\n| A1 | **A2 Two** |\n"))
	if got := ids(p); !reflect.DeepEqual(got, []string{"A1", "A2"}) || !reflect.DeepEqual(p.Packages[1].Needs, []string{"A1"}) {
		t.Errorf("reordered columns: %+v", p.Packages)
	}
}

func TestParsePackageCell(t *testing.T) {
	tests := []struct {
		cell, id, title string
	}{
		{"**A2 Conflict handling**", "A2", "Conflict handling"},
		{"**R0 Fold findings into the plan** ✅", "R0", "Fold findings into the plan"},
		{"✅ **R0 Fold findings**", "R0", "Fold findings"},
		{"**A2 Phone ✅**", "A2", "Phone"},
		{"**A2** Conflict handling", "A2", "Conflict handling"},
		{"**A2:** Conflict handling", "A2", "Conflict handling"},
		{"**A2** — Conflict handling", "A2", "Conflict handling"},
		{"**A2.** Phone", "A2", "Phone"},
		{"**A2: Phone**", "A2", "Phone"},
		{"__A2 Phone__", "A2", "Phone"},
		{"[**A2 Phone**](https://example.com)", "A2", "Phone"},
		{"**[A2 Phone](https://example.com)**", "A2", "Phone"},
		{"A2 Conflict handling", "A2", "Conflict handling"},
		{"A2", "A2", ""},
		{"**A2**", "A2", ""},
		{"**`C1` Parser**", "C1", "Parser"},
		{"**C4 Mod v1: acting**", "C4", "Mod v1: acting"},
		{"**A2 Conflict `handling`**", "A2", "Conflict handling"},
		{"**B3b-2 Surge split**", "B3b-2", "Surge split"},
		{"**A2 Phone", "A2", "Phone"}, // unclosed bold
		{"*A2* italic", "A2", "italic"},
	}
	for _, tt := range tests {
		id, title, ok := parsePackageCell(tt.cell)
		if !ok || id != tt.id || title != tt.title {
			t.Errorf("parsePackageCell(%q) = %q, %q, %v; want %q, %q", tt.cell, id, title, ok, tt.id, tt.title)
		}
	}
	for _, cell := range []string{"", "**Sam listens**", "Sam listens", "**pivot app**", "**Family (US)**", "**1.1**", "Step **A2 Phone**", "—"} {
		if id, _, ok := parsePackageCell(cell); ok {
			t.Errorf("parsePackageCell(%q) = %q, want no ID", cell, id)
		}
	}
}

func TestRowsWithoutID(t *testing.T) {
	md := "| Package | Needs |\n" + // 1
		"|---|---|\n" + // 2
		"| **A1 One** | — |\n" + // 3
		"| **Sam listens** | A1 |\n" + // 4
		"| | |\n" + // 5
		"| Some prose | — |\n" + // 6
		"| **A2 Two** | A1 |\n" // 7
	p := Parse([]byte(md))
	if got := ids(p); !reflect.DeepEqual(got, []string{"A1", "A2"}) {
		t.Errorf("packages = %v", got)
	}
	var lines []int
	for _, w := range p.Warnings {
		if w.Kind != WarnNoID {
			t.Errorf("unexpected warning %v", w)
		}
		lines = append(lines, w.Line)
	}
	if !reflect.DeepEqual(lines, []int{4, 5, 6}) {
		t.Errorf("no-id warnings on lines %v, want [4 5 6]", lines)
	}
	if !strings.Contains(p.Warnings[0].Message, "Sam listens") {
		t.Errorf("warning doesn't quote the cell: %q", p.Warnings[0].Message)
	}
}

func TestDuplicateIDs(t *testing.T) {
	md := "## One\n" + // 1
		"| Package | Needs |\n" + // 2
		"|---|---|\n" + // 3
		"| **A1 First** | — |\n" + // 4
		"| **B3b-2 Split** | — |\n" + // 5
		"\n" + // 6
		"## Two\n" + // 7
		"| Package | Needs |\n" + // 8
		"|---|---|\n" + // 9
		"| **A1 Second** | B3b-2 |\n" + // 10
		"| **B3B-2 Again** | — |\n" + // 11: IDs compare ignoring case
		"| **A2 Two** | A1 |\n" // 12
	p := Parse([]byte(md))
	if got := ids(p); !reflect.DeepEqual(got, []string{"A1", "B3b-2", "A2"}) {
		t.Errorf("packages = %v", got)
	}
	if a1, _ := p.Package("A1"); a1.Title != "First" || a1.Line != 4 || a1.Heading != "One" {
		t.Errorf("the first A1 should win: %+v", a1)
	}
	if !reflect.DeepEqual(kinds(p), []WarningKind{WarnDuplicateID, WarnDuplicateID}) {
		t.Fatalf("warnings = %v", p.Warnings)
	}
	w := p.Warnings[0]
	if w.Line != 10 || w.ID != "A1" || !strings.Contains(w.Message, "line 4") {
		t.Errorf("warning = %+v", w)
	}
	if p.Warnings[1].Line != 11 {
		t.Errorf("warning = %+v", p.Warnings[1])
	}
}

func TestRaggedRows(t *testing.T) {
	md := "| Package | What | Needs |\n" + // 1
		"|---|---|---|\n" + // 2
		"| **A1 One** | x | — |\n" + // 3
		"| **A2 Two** | no Needs cell |\n" + // 4
		"| **A3 Three** | x | A1 | extra |\n" // 5
	p := Parse([]byte(md))
	if got := ids(p); !reflect.DeepEqual(got, []string{"A1", "A2", "A3"}) {
		t.Errorf("packages = %v", got)
	}
	if a2, _ := p.Package("A2"); a2.Needs != nil || a2.What != "no Needs cell" {
		t.Errorf("A2 = %+v", a2)
	}
	if a3, _ := p.Package("A3"); !reflect.DeepEqual(a3.Needs, []string{"A1"}) {
		t.Errorf("A3 = %+v", a3)
	}
	if !reflect.DeepEqual(kinds(p), []WarningKind{WarnRaggedRow, WarnRaggedRow}) || p.Warnings[0].Line != 4 || p.Warnings[1].Line != 5 {
		t.Errorf("warnings = %v", p.Warnings)
	}
}

// needsPlan is a plan with packages A1, A2, B3b-2, T1 (whose Needs cell is
// cell) and Z1, defined after T1.
func needsPlan(cell string) string {
	return "| Package | Needs |\n|---|---|\n" +
		"| **A1 One** | — |\n" +
		"| **A2 Two** | — |\n" +
		"| **B3b-2 Split** | — |\n" +
		"| **T1 Test** | " + cell + " |\n" +
		"| **Z1 Later** | — |\n"
}

func TestNeeds(t *testing.T) {
	tests := []struct {
		cell  string
		needs []string
		note  string
		you   string
		warns []WarningKind
	}{
		// None.
		{cell: ""},
		{cell: "—"},
		{cell: "–"},
		{cell: "-"},
		{cell: "none"},
		{cell: "N/A"},
		// IDs of this plan become dependencies, in order, once each.
		{cell: "A1", needs: []string{"A1"}},
		{cell: "A1, A2", needs: []string{"A1", "A2"}},
		{cell: "A2; A1", needs: []string{"A2", "A1"}},
		{cell: "A1,A2", needs: []string{"A1", "A2"}},
		{cell: "A1, A1", needs: []string{"A1"}},
		{cell: "A1, , A2,", needs: []string{"A1", "A2"}},
		{cell: "`A1`, **A2**", needs: []string{"A1", "A2"}},
		{cell: "A1 ✅", needs: []string{"A1"}},
		{cell: "A1.", needs: []string{"A1"}},
		{cell: "Z1", needs: []string{"Z1"}},              // defined later in the file
		{cell: "B3B-2", needs: []string{"B3b-2"}},        // the plan's spelling
		{cell: "A1, —", needs: []string{"A1"}},           // a dash part means nothing
		{cell: "A1 , A2 ;", needs: []string{"A1", "A2"}}, // spacing
		{cell: "A1;A2;Z1", needs: []string{"A1", "A2", "Z1"}},
		// Other text is the note, with its separators.
		{cell: "Sam + reviewer approve", note: "Sam + reviewer approve"},
		{cell: "A1, Sam + reviewer approve", needs: []string{"A1"}, note: "Sam + reviewer approve"},
		{cell: "Sam approves, A1, then ships", needs: []string{"A1"}, note: "Sam approves, then ships"},
		{cell: "everything above", note: "everything above"},
		{cell: "a1", note: "a1"},             // lowercase isn't an ID
		{cell: "pivot#58", note: "pivot#58"}, // nor is this
		// IDs inside prose stay prose; a leading dash is dropped from the note.
		{cell: "— (its interactive part is A1)", note: "(its interactive part is A1)"},
		{cell: "- see A2", note: "see A2"},
		// you: clauses.
		{cell: "A1; you: run the check", needs: []string{"A1"}, note: "you: run the check", you: "run the check"},
		{cell: "A1, A2; you: OK, since it runs in every session", needs: []string{"A1", "A2"},
			note: "you: OK, since it runs in every session", you: "OK, since it runs in every session"},
		{cell: "You: approve; A1", needs: []string{"A1"}, note: "You: approve", you: "approve"},
		{cell: "**you:** approve", note: "**you:** approve", you: "approve"},
		{cell: "you: approve; you: deploy", note: "you: approve; you: deploy", you: "approve; deploy"},
		{cell: "you: approve; then A1 ships", note: "you: approve; then A1 ships", you: "approve"},
		{cell: "ask you: maybe", note: "ask you: maybe"}, // not at the start of a part
		{cell: "you", note: "you"},   // no colon
		{cell: "you:", note: "you:"}, // nothing to do
		// Warnings.
		{cell: "Z9", note: "Z9", warns: []WarningKind{WarnUnknownID}},
		{cell: "A1, Z9", needs: []string{"A1"}, note: "Z9", warns: []WarningKind{WarnUnknownID}},
		{cell: "A1 and A2", note: "A1 and A2", warns: []WarningKind{WarnLooseID}},
		{cell: "A1: after review", note: "A1: after review", warns: []WarningKind{WarnLooseID}},
		{cell: "Q4 2026", note: "Q4 2026"}, // Q4 isn't a package here, so no loose-id warning
		{cell: "T1, A1", needs: []string{"A1"}, note: "T1", warns: []WarningKind{WarnSelfNeed}},
	}
	for _, tt := range tests {
		p := Parse([]byte(needsPlan(tt.cell)))
		got, ok := p.Package("T1")
		if !ok {
			t.Fatalf("%q: no T1 in %v", tt.cell, ids(p))
		}
		if !reflect.DeepEqual(got.Needs, tt.needs) || got.Note != tt.note || got.NeedsYou != tt.you {
			t.Errorf("Needs %q: got needs %q, note %q, you %q; want %q, %q, %q",
				tt.cell, got.Needs, got.Note, got.NeedsYou, tt.needs, tt.note, tt.you)
		}
		if got := kinds(p); !reflect.DeepEqual(got, tt.warns) {
			t.Errorf("Needs %q: warnings %v, want %v", tt.cell, p.Warnings, tt.warns)
		}
		for _, w := range p.Warnings {
			if w.Line != got.Line || w.ID != "T1" {
				t.Errorf("Needs %q: warning %+v should name T1 on line %d", tt.cell, w, got.Line)
			}
		}
		if got.Yours {
			t.Errorf("Needs %q: a Needs cell never makes the package yours", tt.cell)
		}
	}
}

func TestOwner(t *testing.T) {
	md := "| Package | Owner | Needs |\n|---|---|---|\n" +
		"| **A1 Sam listens** | you | — |\n" +
		"| **A2 Two** | You | A1 |\n" +
		"| **A3 Three** | **you** | A1 |\n" +
		"| **A4 Four** | Sam | A1 |\n" +
		"| **A5 Five** | | A1; you: approve |\n" +
		"| **A6 Six** | you and Sam | A1 |\n"
	p := Parse([]byte(md))
	want := map[string]bool{"A1": true, "A2": true, "A3": true, "A4": false, "A5": false, "A6": false}
	for _, pkg := range p.Packages {
		if pkg.Yours != want[pkg.ID] {
			t.Errorf("%s (owner %q): Yours = %v", pkg.ID, pkg.Owner, pkg.Yours)
		}
	}
	if a4, _ := p.Package("A4"); a4.Owner != "Sam" {
		t.Errorf("A4 owner = %q", a4.Owner)
	}
	if a5, _ := p.Package("A5"); a5.NeedsYou != "approve" || a5.Yours {
		t.Errorf("A5 = %+v", a5)
	}
}

func TestDone(t *testing.T) {
	tests := []struct {
		row  string // Package | What | Status | Needs
		done bool
	}{
		{"| **A2 Two** ✅ | | | — |", true},
		{"| ✅ **A2 Two** | | | — |", true},
		{"| **A2 Two** | shipped ✅ | | — |", true},
		{"| **A2 Two** | | | A1 ✅ |", true},
		{"| **A2 Two** | | done | — |", true},
		{"| **A2 Two** | | Done. | — |", true},
		{"| **A2 Two** | | DONE | — |", true},
		{"| **A2 Two** | | merged | — |", true},
		{"| **A2 Two** | | Merged (#12) | — |", true},
		{"| **A2 Two** | | **Done** | — |", true},
		{"| **A2 Two** | | ✅ | — |", true},
		{"| **A2 Two** | | complete | — |", true},
		{"| **A2 Two** | | completed | — |", true},
		{"| **A2 Two** | | | — |", false},
		{"| **A2 Two** | | not done | — |", false},
		{"| **A2 Two** | | in progress | — |", false},
		{"| **A2 Two** | | undone | — |", false},
		{"| **A2 Two** | | merging | — |", false},
		{"| **A2 Two** | | ✔ | — |", false},
	}
	for _, tt := range tests {
		md := "| Package | What | Status | Needs |\n|---|---|---|---|\n| **A1 One** | | | — |\n" + tt.row + "\n"
		p := Parse([]byte(md))
		a2, ok := p.Package("A2")
		if !ok || a2.Done != tt.done {
			t.Errorf("%s: Done = %v, want %v", tt.row, a2.Done, tt.done)
		}
		if a1, _ := p.Package("A1"); a1.Done {
			t.Errorf("%s: A1 shouldn't be done", tt.row)
		}
	}
	p := Parse([]byte("| Package | Status | Needs |\n|---|---|---|\n| **A1 One** | ✅ | — |\n| **A2 Two** | — | A1 |\n"))
	if a1, _ := p.Package("A1"); a1.Status != "✅" {
		t.Errorf("Status keeps the ✅: %q", a1.Status)
	}
}

const lanePackages = "| Package | Needs |\n|---|---|\n" +
	"| **A1 One** | — |\n| **A2 Two** | A1 |\n| **G1 Geodata** | — |\n| **V1 Routes** | G1 |\n"

func TestLanes(t *testing.T) {
	t.Run("packages column", func(t *testing.T) {
		md := "## Lanes\n" + // 1
			"| Lane | Packages | Rule |\n" + // 2
			"|---|---|---|\n" + // 3
			"| **Tiles** | A1, A2 | **single-writer**: one agent in `tiles/`. |\n" + // 4
			"| Routes | V1 | Parallel. |\n" + // 5
			"\n" + lanePackages
		p := Parse([]byte(md))
		want := []Lane{
			{Name: "Tiles", Packages: []string{"A1", "A2"}, Rule: "**single-writer**: one agent in `tiles/`.", SingleWriter: true, Line: 4, Heading: "Lanes"},
			{Name: "Routes", Packages: []string{"V1"}, Rule: "Parallel.", Line: 5, Heading: "Lanes"},
		}
		if !reflect.DeepEqual(p.Lanes, want) {
			t.Errorf("lanes = %+v\nwant %+v", p.Lanes, want)
		}
		got := map[string]string{}
		for _, pkg := range p.Packages {
			got[pkg.ID] = pkg.Lane
		}
		if !reflect.DeepEqual(got, map[string]string{"A1": "Tiles", "A2": "Tiles", "G1": "", "V1": "Routes"}) {
			t.Errorf("package lanes = %v", got)
		}
		if len(p.Warnings) != 0 {
			t.Errorf("warnings = %v", p.Warnings)
		}
	})

	t.Run("IDs in other columns aren't members", func(t *testing.T) {
		md := lanePackages + "\n| Lane | Packages, in order | Runs alongside |\n|---|---|---|\n" +
			"| Map tiles | A1 → A2 | everything; only one agent at a time edits the tile index |\n" +
			"| Routes | V1 | everything, once G1 is approved |\n"
		p := Parse([]byte(md))
		if len(p.Lanes) != 2 || !reflect.DeepEqual(p.Lanes[0].Packages, []string{"A1", "A2"}) || !reflect.DeepEqual(p.Lanes[1].Packages, []string{"V1"}) {
			t.Fatalf("lanes = %+v", p.Lanes)
		}
		if !p.Lanes[0].SingleWriter || p.Lanes[1].SingleWriter {
			t.Errorf("single-writer = %v, %v", p.Lanes[0].SingleWriter, p.Lanes[1].SingleWriter)
		}
		if g1, _ := p.Package("G1"); g1.Lane != "" {
			t.Errorf("G1 is only mentioned in a rule, but got lane %q", g1.Lane)
		}
	})

	t.Run("Package column", func(t *testing.T) {
		md := lanePackages + "\n| Lane | Package | Notes |\n|---|---|---|\n| Text | A1 | G1 first |\n"
		p := Parse([]byte(md))
		if len(p.Lanes) != 1 || !reflect.DeepEqual(p.Lanes[0].Packages, []string{"A1"}) || p.Lanes[0].Rule != "G1 first" {
			t.Errorf("lanes = %+v", p.Lanes)
		}
	})

	t.Run("no packages column", func(t *testing.T) {
		md := lanePackages + "\n| Lane | Rule |\n|---|---|\n| Text | A1 then A2; one agent at a time |\n"
		p := Parse([]byte(md))
		want := []Lane{{Name: "Text", Packages: []string{"A1", "A2"}, Rule: "A1 then A2; one agent at a time", SingleWriter: true, Line: 10}}
		if !reflect.DeepEqual(p.Lanes, want) {
			t.Errorf("lanes = %+v\nwant %+v", p.Lanes, want)
		}
	})

	t.Run("single-writer markers", func(t *testing.T) {
		for rule, want := range map[string]bool{
			"single-writer":                  true,
			"**Single-Writer** for `web/`":   true,
			"SINGLE WRITER":                  true,
			"One agent at a time edits this": true,
			"Parallel":                       false,
			"one writer, many readers":       false,
			"":                               false,
		} {
			p := Parse([]byte(lanePackages + "\n| Lane | Packages | Rule |\n|---|---|---|\n| L | A1 | " + rule + " |\n"))
			if len(p.Lanes) != 1 || p.Lanes[0].SingleWriter != want {
				t.Errorf("rule %q: lanes %+v, want SingleWriter %v", rule, p.Lanes, want)
			}
		}
	})

	t.Run("warnings", func(t *testing.T) {
		md := "| Lane | Packages | Rule |\n" + // 1
			"|---|---|---|\n" + // 2
			"| Text | A1, Q7, A1 | |\n" + // 3: unknown ID, repeated ID
			"| Again | A1 | |\n" + // 4: A1 already in Text
			"| | A2 | no name |\n" + // 5
			"| Short |\n" + // 6: ragged
			"\n" + lanePackages
		p := Parse([]byte(md))
		wantLanes := []Lane{
			{Name: "Text", Packages: []string{"A1", "Q7"}, Line: 3},
			{Name: "Again", Packages: []string{"A1"}, Line: 4},
			{Name: "Short", Line: 6},
		}
		if !reflect.DeepEqual(p.Lanes, wantLanes) {
			t.Errorf("lanes = %+v\nwant %+v", p.Lanes, wantLanes)
		}
		if a1, _ := p.Package("A1"); a1.Lane != "Text" {
			t.Errorf("A1 lane = %q, want the first lane", a1.Lane)
		}
		if a2, _ := p.Package("A2"); a2.Lane != "" {
			t.Errorf("A2 lane = %q", a2.Lane)
		}
		want := []Warning{
			{Line: 3, Kind: WarnUnknownID, ID: "Q7"},
			{Line: 4, Kind: WarnTwoLanes, ID: "A1"},
			{Line: 5, Kind: WarnNoLaneName},
			{Line: 6, Kind: WarnRaggedRow},
		}
		if len(p.Warnings) != len(want) {
			t.Fatalf("warnings = %v", p.Warnings)
		}
		for i, w := range p.Warnings {
			if w.Line != want[i].Line || w.Kind != want[i].Kind || w.ID != want[i].ID {
				t.Errorf("warning %d = %+v, want %+v", i, w, want[i])
			}
		}
	})

	t.Run("a package table with a Lane column stays a package table", func(t *testing.T) {
		p := Parse([]byte("| Package | Lane | Needs |\n|---|---|---|\n| **A1 One** | Text | — |\n| **A2 Two** | Text | A1 |\n"))
		if len(p.Packages) != 2 || len(p.Lanes) != 0 {
			t.Errorf("packages %v, lanes %+v", ids(p), p.Lanes)
		}
	})
}

func TestHeadingsLinesAndWarningOrder(t *testing.T) {
	md := "# Plan\n" + // 1
		"## Lanes\n" + // 2
		"| Lane | Packages |\n" + // 3
		"|---|---|\n" + // 4
		"| L | A1, Q9 |\n" + // 5: unknown ID
		"## Phase 1\n" + // 6
		"Some text.\n" + // 7
		"\n" + // 8
		"| Package | Needs |\n" + // 9
		"|---|---|\n" + // 10
		"| **A1 One** | — |\n" + // 11
		"| no id | — |\n" + // 12
		"### Phase 2\n" + // 13
		"| Package | Needs |\n" + // 14
		"|---|---|\n" + // 15
		"| **A2 Two** | A1, Z8 |\n" // 16: unknown ID
	p := Parse([]byte(md))
	if p.Title != "Plan" {
		t.Errorf("title = %q", p.Title)
	}
	a1, _ := p.Package("A1")
	a2, _ := p.Package("A2")
	if a1.Line != 11 || a1.Heading != "Phase 1" || a2.Line != 16 || a2.Heading != "Phase 2" {
		t.Errorf("A1 at %d under %q, A2 at %d under %q", a1.Line, a1.Heading, a2.Line, a2.Heading)
	}
	if len(p.Lanes) != 1 || p.Lanes[0].Line != 5 || p.Lanes[0].Heading != "Lanes" {
		t.Errorf("lanes = %+v", p.Lanes)
	}
	var lines []int
	for _, w := range p.Warnings {
		lines = append(lines, w.Line)
	}
	if !reflect.DeepEqual(lines, []int{5, 12, 16}) {
		t.Errorf("warning lines = %v, want [5 12 16] (sorted)", lines)
	}
	if s := p.Warnings[0].String(); !strings.HasPrefix(s, "line 5: ") || !strings.Contains(s, "Q9") {
		t.Errorf("String() = %q", s)
	}
}

func TestPackageLookup(t *testing.T) {
	p := Parse([]byte(needsPlan("—")))
	if pkg, ok := p.Package("b3b-2"); !ok || pkg.ID != "B3b-2" {
		t.Errorf("Package(b3b-2) = %+v, %v", pkg, ok)
	}
	if _, ok := p.Package("Q1"); ok {
		t.Error("Package(Q1) found a package")
	}
}

// FuzzParse checks that Parse never panics and that its output is
// consistent, whatever the input. `go test` runs the seeds only.
func FuzzParse(f *testing.F) {
	for _, name := range []string{"sharing-plan.md", "ROADMAP.md", "edge-cases.md", "design-doc.md"} {
		f.Add(readFixture(f, name))
	}
	f.Add([]byte("| Package | Needs |\n|---|---|\n| **A1 x** | A1, `A2 | ; you: |\n| `` | \\"))
	f.Fuzz(func(t *testing.T, src []byte) {
		p := Parse(src)
		seen := map[string]bool{}
		for _, pkg := range p.Packages {
			key := strings.ToLower(pkg.ID)
			if !IsID(pkg.ID) || seen[key] || pkg.Line < 1 {
				t.Fatalf("bad package %+v", pkg)
			}
			seen[key] = true
		}
		for _, pkg := range p.Packages {
			for _, n := range pkg.Needs {
				if !seen[strings.ToLower(n)] || n == pkg.ID {
					t.Fatalf("%s needs %s", pkg.ID, n)
				}
			}
		}
		for i := 1; i < len(p.Warnings); i++ {
			if p.Warnings[i].Line < p.Warnings[i-1].Line {
				t.Fatalf("warnings out of order: %v", p.Warnings)
			}
		}
		d := Detect(src)
		lanes := len(p.Lanes)
		if len(p.Packages) == 0 {
			lanes = 0
		}
		if d.IsPlan != (len(p.Packages) >= MinPackages) || d.Packages != len(p.Packages) || d.Lanes != lanes || d.Title != p.Title {
			t.Fatalf("Detect %+v disagrees with Parse (%d packages, %d lanes, %q)", d, len(p.Packages), len(p.Lanes), p.Title)
		}
	})
}
