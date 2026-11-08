package plans

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func readFixture(tb testing.TB, name string) []byte {
	tb.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "plans", name))
	if err != nil {
		tb.Fatal(err)
	}
	return b
}

// pkgWant is the part of a Package the golden tests pin down. Needs is
// comma-separated.
type pkgWant struct {
	ID, Title, Repo, Size string
	Needs, Note, NeedsYou string
	Done                  bool
	Lane                  string
}

func checkPackages(t *testing.T, p Plan, want []pkgWant) {
	t.Helper()
	var got []pkgWant
	for _, pkg := range p.Packages {
		got = append(got, pkgWant{
			ID: pkg.ID, Title: pkg.Title, Repo: pkg.Repo, Size: pkg.Size,
			Needs: strings.Join(pkg.Needs, ","), Note: pkg.Note, NeedsYou: pkg.NeedsYou,
			Done: pkg.Done, Lane: pkg.Lane,
		})
	}
	for i := 0; i < len(got) || i < len(want); i++ {
		switch {
		case i >= len(got):
			t.Errorf("missing package %+v", want[i])
		case i >= len(want):
			t.Errorf("unexpected package %+v", got[i])
		case got[i] != want[i]:
			t.Errorf("package %d:\n got %+v\nwant %+v", i, got[i], want[i])
		}
	}
}

func checkLanes(t *testing.T, p Plan, want []Lane) {
	t.Helper()
	if len(p.Lanes) != len(want) {
		t.Fatalf("got %d lanes, want %d: %+v", len(p.Lanes), len(want), p.Lanes)
	}
	for i := range want {
		if !reflect.DeepEqual(p.Lanes[i], want[i]) {
			t.Errorf("lane %d:\n got %+v\nwant %+v", i, p.Lanes[i], want[i])
		}
	}
}

// TestSharingPlan pins down what the parser reads from a realistic plan
// (testdata/plans/sharing-plan.md): several package tables, one without
// Repo and Size columns, free text in Needs, a lanes table with a
// single-writer rule, tables that aren't package tables, and Merges and
// Repos lines inside a code block.
func TestSharingPlan(t *testing.T) {
	p := Parse(readFixture(t, "sharing-plan.md"))
	if p.Title != "Pivot share links: delivery plan" {
		t.Errorf("title = %q", p.Title)
	}
	checkPackages(t, p, []pkgWant{
		// Stage 1. G1's Needs is free text, not a dependency.
		{ID: "G1", Title: "Redaction rules", Repo: "pivot", Size: "S", Note: "Priya + Theo sign off"},
		{ID: "A1", Title: "Share tokens", Repo: "pivot", Size: "L", Lane: "Access"},
		{ID: "A2", Title: "Token revocation", Repo: "pivot", Size: "S", Needs: "A1", Lane: "Access"},
		{ID: "A3", Title: "Embed endpoint", Repo: "pivot", Size: "S", Needs: "W1", Lane: "Viewer"},
		{ID: "A4", Title: "Snapshot renderer", Repo: "pivot", Size: "M", Lane: "Snapshots"},
		{ID: "W1", Title: "Public viewer", Repo: "web", Size: "M", Lane: "Viewer"},
		{ID: "W2", Title: "Share dialog", Repo: "web", Size: "S", Needs: "W1", Lane: "Viewer"},
		// Stage 2.
		{ID: "T1", Title: "Tile filters", Repo: "pivot", Size: "M", Needs: "A1,G1", Lane: "Access"},
		{ID: "V1", Title: "Email reports", Repo: "pivot", Size: "M", Needs: "G1", Lane: "Email"},
		{ID: "W3", Title: "Embed guide", Repo: "web", Size: "M", Needs: "W1,G1", Lane: "Viewer"},
		{ID: "S1", Title: "Scheduled snapshots", Repo: "pivot", Size: "M", Needs: "A4,G1", Lane: "Snapshots"},
		// Stage 3: no Repo or Size column. Q4's "everything above" is a note.
		{ID: "Q1", Title: "Load test", Needs: "T1,V1"},
		{ID: "Q2", Title: "Leak check", Needs: "T1", Lane: "Access"},
		{ID: "Q3", Title: "Pilot with two customers", Needs: "Q2,V1"},
		{ID: "Q4", Title: "Security review", Note: "everything above"},
	})
	checkLanes(t, p, []Lane{
		{Name: "Access", Packages: []string{"A1", "A2", "T1", "Q2"}, Rule: "everything; only one agent at a time edits the access schema", SingleWriter: true, Line: 99, Heading: "Agents and lanes"},
		// G1 in the rule cell is not a member of the Email lane.
		{Name: "Email", Packages: []string{"V1"}, Rule: "everything, once G1 is approved", Line: 100, Heading: "Agents and lanes"},
		{Name: "Viewer", Packages: []string{"W1", "W2", "W3", "A3"}, Rule: "everything (separate repo)", Line: 101, Heading: "Agents and lanes"},
		{Name: "Snapshots", Packages: []string{"A4", "S1"}, Rule: "everything", Line: 102, Heading: "Agents and lanes"},
	})
	for id, want := range map[string]struct {
		line    int
		heading string
	}{
		"G1": {45, "Stage 1: Tokens and the viewer"},
		"W2": {51, "Stage 1: Tokens and the viewer"},
		"T1": {60, "Stage 2: Embeds and snapshots"},
		"Q4": {72, "Stage 3: Checks before launch"},
	} {
		pkg, _ := p.Package(id)
		if pkg.Line != want.line || pkg.Heading != want.heading {
			t.Errorf("%s at line %d under %q, want %d under %q", id, pkg.Line, pkg.Heading, want.line, want.heading)
		}
	}
	if a1, _ := p.Package("A1"); !strings.HasPrefix(a1.What, "`internal/share/token.go`: one signed token") || !strings.Contains(a1.What, `"share\t%s · %s"`) {
		t.Errorf("A1 What = %q", a1.What)
	}
	// The audience and settled-questions tables have no dependency column,
	// the rollout steps are a list, not a table, and the Merges and Repos
	// lines are in a code block: none of them are read.
	if len(p.Warnings) != 0 {
		t.Errorf("warnings: %v", p.Warnings)
	}
}

// TestRoadmapFixture pins down what the parser reads from
// testdata/plans/ROADMAP.md, a build plan whose slug is set in its status
// line.
func TestRoadmapFixture(t *testing.T) {
	p := Parse(readFixture(t, "ROADMAP.md"))
	if p.Title != "ai-manager — build plan" {
		t.Errorf("title = %q", p.Title)
	}
	checkPackages(t, p, []pkgWant{
		// Phase 0: everything but R1 is ✅. S2's Needs is a dash plus a
		// remark that mentions R1; R1 is not a dependency of S2.
		{ID: "F1", Title: "Repo scaffold", Size: "S", Done: true},
		{ID: "S1", Title: "Session and worktree probes", Size: "S", Done: true, Lane: "Spike"},
		{ID: "S2", Title: "Mod event probe", Size: "M", Note: "(its interactive part is R1)", Done: true, Lane: "Spike"},
		{ID: "S3", Title: "GitHub query budget", Size: "S", Done: true, Lane: "Spike"},
		{ID: "R0", Title: "Fold findings into the plan", Size: "S", Needs: "S1,S2,S3", Done: true, Lane: "Plan"},
		{ID: "R1", Title: "Interactive-check results", Size: "S", Needs: "S2", Note: "you: run the check", NeedsYou: "run the check"},
		// Phase 1a.
		{ID: "A1", Title: "Worktree scanner", Size: "M", Needs: "F1,S1"},
		{ID: "A2", Title: "GitHub client", Size: "M", Needs: "F1,S3"},
		{ID: "A3", Title: "aim ls and aim prune", Size: "M", Needs: "A1,A2"},
		{ID: "A4", Title: "Session poller", Size: "S", Needs: "F1,S1"},
		// Phase 1b.
		{ID: "B1", Title: "Store", Size: "M", Needs: "F1", Lane: "Store"},
		{ID: "B2", Title: "aimd core", Size: "M", Needs: "A1,A2,A4,B1"},
		{ID: "B3", Title: "Dashboard v1", Size: "M", Needs: "B2", Lane: "Web"},
		{ID: "B4", Title: "Install aimd", Size: "S", Needs: "B3", Note: "you: OK to install the launchd agent", NeedsYou: "OK to install the launchd agent"},
		// Phase 2.
		{ID: "C1", Title: "Plan parser and detector", Size: "M", Needs: "F1"},
		{ID: "C2", Title: "Matcher and package state", Size: "L", Needs: "C1,B1,A2", Lane: "Store"},
		{ID: "C3", Title: "Plan API", Size: "M", Needs: "B2,C2", Lane: "Store"},
		{ID: "C4", Title: "Mod v1: acting", Size: "L", Needs: "C3,S2", Lane: "Mod"},
		{ID: "C5", Title: "Skills", Size: "S", Needs: "C1"},
		{ID: "C6", Title: "Plan board", Size: "M", Needs: "C3,B3", Lane: "Web"},
		{ID: "C7", Title: "Install the plugin", Size: "S", Needs: "C4,C5", Note: "you: OK, since it runs in every session", NeedsYou: "OK, since it runs in every session"},
		// Phase 3.
		{ID: "D1", Title: "Event receiver and live state", Size: "M", Needs: "B2,S2", Lane: "Store"},
		{ID: "D2", Title: "Mod reporting", Size: "M", Needs: "D1,C4,R1", Lane: "Mod"},
		{ID: "D3", Title: "Live UI", Size: "M", Needs: "D1,C6", Lane: "Web"},
		// Phase 4.
		{ID: "E1", Title: "Band progress and /aim", Size: "S", Needs: "D2", Lane: "Mod"},
		{ID: "E2", Title: "Notifications and Go", Size: "M", Needs: "D1", Note: "you: try Go on a few sessions", NeedsYou: "try Go on a few sessions"},
	})
	const lanes = "3. Phases and packages"
	checkLanes(t, p, []Lane{
		{Name: "Spike", Packages: []string{"S1", "S2", "S3"}, Rule: "Parallel. Each writes only its own `testdata/` folder.", Line: 52, Heading: lanes},
		{Name: "Mod", Packages: []string{"C4", "D2", "E1"}, Rule: "**single-writer**: one agent at a time in `plugin/mod/`.", SingleWriter: true, Line: 53, Heading: lanes},
		{Name: "Store", Packages: []string{"B1", "C2", "C3", "D1"}, Rule: "**single-writer** for `internal/store/migrations/`: one open PR at a time adds a migration.", SingleWriter: true, Line: 54, Heading: lanes},
		{Name: "Web", Packages: []string{"B3", "C6", "D3"}, Rule: "**single-writer** for `internal/web/` templates.", SingleWriter: true, Line: 55, Heading: lanes},
		{Name: "Plan", Packages: []string{"R0"}, Rule: "Lead only.", Line: 56, Heading: lanes},
	})
	for id, want := range map[string]struct {
		line    int
		heading string
	}{
		"F1": {62, "Phase 0 — Verify assumptions"},
		"R1": {67, "Phase 0 — Verify assumptions"},
		"A1": {73, "Phase 1a — Inventory CLI, no daemon, no plugin"},
		"B4": {85, "Phase 1b — Daemon and first dashboard"},
		"C7": {97, "Phase 2 — Plans and the mod"},
		"D1": {103, "Phase 3 — Live detail"},
		"E2": {112, "Phase 4 — In-session + iTerm2"},
	} {
		pkg, _ := p.Package(id)
		if pkg.Line != want.line || pkg.Heading != want.heading {
			t.Errorf("%s at line %d under %q, want %d under %q", id, pkg.Line, pkg.Heading, want.line, want.heading)
		}
	}
	// The section 1 table (with its escaped pipes), the decision and
	// open-question tables have no dependency column, and the layout's code
	// block is skipped: none of them are read.
	if len(p.Warnings) != 0 {
		t.Errorf("warnings: %v", p.Warnings)
	}
}

// TestEdgeCasesFixture checks the hand-written fixture that puts most rules
// in one file.
func TestEdgeCasesFixture(t *testing.T) {
	p := Parse(readFixture(t, "edge-cases.md"))
	if p.Title != "Edge-case plan" {
		t.Errorf("title = %q (want the level-1 heading, not the earlier level-2 one)", p.Title)
	}
	checkPackages(t, p, []pkgWant{
		{ID: "X1", Title: "Listen to the takes", Lane: "Listening"},
		{ID: "X2", Title: "Escaped | pipe", Needs: "X1", Done: true, Lane: "Tags"},
		{ID: "X3", Title: "Underscore bold", Needs: "X1,X2", Note: "you: sign off, then tag", NeedsYou: "sign off, then tag", Done: true, Lane: "Tags"},
		{ID: "X4", Title: "Linked", Needs: "X1,X2", Done: true},
		{ID: "X5", Title: "No bold", Lane: "Docs"},
		{ID: "X6", Title: "Unknown and loose", Needs: "X1", Note: "Z9, X2 and X3"},
		{ID: "X7", Title: "Ragged"},
		{ID: "X8", Title: "Self", Needs: "X1", Note: "X8", Done: true},
	})
	x1, _ := p.Package("X1")
	if !x1.Yours || x1.Owner != "you" || x1.What != "`a | b` stays in one cell" || x1.Heading != "Work" {
		t.Errorf("X1 = %+v", x1)
	}
	if x5, _ := p.Package("X5"); x5.Status != "not done" || x5.Done {
		t.Errorf("X5 = %+v", x5)
	}
	checkLanes(t, p, []Lane{
		{Name: "Listening", Packages: []string{"X1"}, Rule: "Single Writer: one listener", SingleWriter: true, Line: 51, Heading: "Lanes"},
		{Name: "Tags", Packages: []string{"X2", "X3", "Q7"}, Line: 52, Heading: "Lanes"},
		{Name: "Again", Packages: []string{"X1"}, Line: 53, Heading: "Lanes"},
		{Name: "Docs", Packages: []string{"X5"}, Rule: "X5 owns `docs/` alone; single-writer", SingleWriter: true, Line: 58, Heading: "Lanes"},
	})
	want := []Warning{
		{Line: 27, Kind: WarnNoID},
		{Line: 28, Kind: WarnDuplicateID, ID: "X2"},
		{Line: 29, Kind: WarnUnknownID, ID: "X6"},
		{Line: 29, Kind: WarnLooseID, ID: "X6"},
		{Line: 30, Kind: WarnRaggedRow},
		{Line: 31, Kind: WarnSelfNeed, ID: "X8"},
		{Line: 35, Kind: WarnNoPackageColumn},
		{Line: 43, Kind: WarnNoNeedsColumn},
		{Line: 52, Kind: WarnUnknownID, ID: "Q7"},
		{Line: 53, Kind: WarnTwoLanes, ID: "X1"},
		{Line: 54, Kind: WarnNoLaneName},
	}
	if len(p.Warnings) != len(want) {
		t.Fatalf("got %d warnings, want %d:\n%v", len(p.Warnings), len(want), p.Warnings)
	}
	for i, w := range p.Warnings {
		if w.Line != want[i].Line || w.Kind != want[i].Kind || w.ID != want[i].ID {
			t.Errorf("warning %d = %+v, want %+v", i, w, want[i])
		}
	}
}

// TestDesignDocFixture: a design doc with tables, bold IDs and the word
// "needs" yields no packages and no warnings.
func TestDesignDocFixture(t *testing.T) {
	p := Parse(readFixture(t, "design-doc.md"))
	if len(p.Packages) != 0 || len(p.Lanes) != 0 || len(p.Warnings) != 0 {
		t.Errorf("got %+v", p)
	}
}
