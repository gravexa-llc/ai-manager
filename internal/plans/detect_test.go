package plans

import (
	"strings"
	"testing"
)

func TestDetectFixtures(t *testing.T) {
	tests := []struct {
		file string
		want Detection
	}{
		{"sharing-plan.md", Detection{IsPlan: true, Title: "Pivot share links: delivery plan", Packages: 15, Lanes: 4}},
		{"ROADMAP.md", Detection{IsPlan: true, Title: "ai-manager — build plan", Packages: 26, Lanes: 5}},
		{"edge-cases.md", Detection{IsPlan: true, Title: "Edge-case plan", Packages: 8, Lanes: 4}},
		// A design doc with tables but no dependency column is not a plan.
		{"design-doc.md", Detection{Title: "Booking cache: design"}},
	}
	for _, tt := range tests {
		if got := Detect(readFixture(t, tt.file)); got != tt.want {
			t.Errorf("Detect(%s) = %+v, want %+v", tt.file, got, tt.want)
		}
	}
}

func TestDetectNotAPlan(t *testing.T) {
	tests := map[string]string{
		"empty":    "",
		"no table": "# Notes\n\nThe app needs a cache | maybe.\n",
		"tables without a dependency column": "# Design\n\n| Package | What |\n|---|---|\n| **A1 One** | x |\n| **A2 Two** | y |\n" +
			"\n| # | Decision | Decided |\n|---|---|---|\n| D1 | x | yes |\n| D2 | y | no |\n",
		"one package":                         "# P\n\n| Package | Needs |\n|---|---|\n| **A1 One** | — |\n",
		"rows without IDs":                    "# P\n\n| Package | Needs |\n|---|---|\n| **Fetcher** | — |\n| **Invalidator** | Fetcher |\n",
		"plan table in fence":                 "# Skill\n\n```markdown\n| Package | Needs |\n|---|---|\n| **A1 One** | — |\n| **A2 Two** | A1 |\n```\n",
		"plan table in comment":               "# Draft\n\n<!--\n| Package | Needs |\n|---|---|\n| **A1 One** | — |\n| **A2 Two** | A1 |\n-->\n",
		"needs column without package column": "# P\n\n| Thing | Needs |\n|---|---|\n| **A1 One** | — |\n| **A2 Two** | A1 |\n",
		"lanes only":                          "# P\n\n| Lane | Packages | Rule |\n|---|---|---|\n| L | A1, A2 | single-writer |\n",
		"lanes, no keyword":                   "# P\n\n| Lane | Packages |\n|---|---|\n| L | A1, A2 |\n",
	}
	for name, md := range tests {
		d := Detect([]byte(md))
		if d.IsPlan || d.Lanes != 0 {
			t.Errorf("%s: Detect = %+v, want not a plan and no lanes", name, d)
		}
	}
}

func TestDetectSmallestPlan(t *testing.T) {
	md := "## Wave\n\n| Package | Depends on |\n|---|---|\n| **A1 One** | — |\n| **A2 Two** | A1 |\n"
	want := Detection{IsPlan: true, Title: "Wave", Packages: 2}
	if got := Detect([]byte(md)); got != want {
		t.Errorf("Detect = %+v, want %+v", got, want)
	}
}

func TestDetectEveryNeedsHeader(t *testing.T) {
	for _, h := range append(append([]string{}, needsHeaders...), "NEEDS", "**Needs**", "`Needs`", "**Ne**eds", "Depends   On", "Blocked\tby", "Needs (IDs)") {
		md := "| Package | " + h + " |\n|---|---|\n| **A1 One** | — |\n| **A2 Two** | A1 |\n"
		if d := Detect([]byte(md)); !d.IsPlan || d.Packages != 2 {
			t.Errorf("header %q: Detect = %+v", h, d)
		}
	}
}

func TestDetectTitleWithoutTables(t *testing.T) {
	md := "Intro.\n\n## Section\n\n```\n# not this\n```\n\n# The title\n\nText.\n"
	if got := Detect([]byte(md)); got.Title != "The title" {
		t.Errorf("Title = %q", got.Title)
	}
}

func BenchmarkDetect(b *testing.B) {
	plan := readFixture(b, "ROADMAP.md")
	design := readFixture(b, "design-doc.md")
	prose := []byte(strings.Repeat("Plain prose with no tables at all, the common case for a .md write.\n", 1000))
	for _, bm := range []struct {
		name string
		src  []byte
	}{{"ROADMAP.md", plan}, {"design-doc.md", design}, {"prose-68KB", prose}} {
		b.Run(bm.name, func(b *testing.B) {
			b.SetBytes(int64(len(bm.src)))
			for b.Loop() {
				Detect(bm.src)
			}
		})
	}
}

func BenchmarkParse(b *testing.B) {
	src := readFixture(b, "ROADMAP.md")
	b.SetBytes(int64(len(src)))
	for b.Loop() {
		Parse(src)
	}
}
