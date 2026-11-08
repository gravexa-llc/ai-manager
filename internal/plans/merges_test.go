package plans

import (
	"strings"
	"testing"
)

func TestMergesFixtures(t *testing.T) {
	for _, f := range []string{"ROADMAP.md", "sharing-plan.md", "edge-cases.md", "design-doc.md"} {
		src := readFixture(t, f)
		if got, w := Merges(src); got != MergesYou || w != nil {
			t.Errorf("Merges(%s) = %q, %v; want you, no warning (no Merges line)", f, got, w)
		}
		if p := Parse(src); p.Merges != MergesYou {
			t.Errorf("Parse(%s).Merges = %q, want you", f, p.Merges)
		}
	}
}

func TestMerges(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      string
		warnLine  int // 0: no warning
	}{
		{"none", "# T\n\nNo marker here.\n", MergesYou, 0},
		{"you", "Merges: you\n", MergesYou, 0},
		{"lead", "Merges: lead\n", MergesLead, 0},
		{"any case", "MERGES: Lead\n", MergesLead, 0},
		{"bold label, code value", "**Merges:** `lead`\n", MergesLead, 0},
		{"bold value", "Merges: **lead**\n", MergesLead, 0},
		{"in a list", "- Merges: lead\n", MergesLead, 0},
		{"in a blockquote", "> merges: lead\n", MergesLead, 0},
		{"trailing period", "Merges: you.\n", MergesYou, 0},
		{"with a remark", "Merges: lead (the lead session reviews and merges)\n", MergesLead, 0},
		{"lead session", "Merges: lead session\n", MergesLead, 0},
		{"first line wins", "Merges: lead\nMerges: you\n", MergesLead, 0},
		{"after other lines", "# T\n\nPlan slug: x\nRepos: o/a\nMerges: lead\n", MergesLead, 0},
		{"in a fence ignored", "```\nMerges: lead\n```\n", MergesYou, 0},
		{"in a comment ignored", "<!--\nMerges: lead\n-->\n", MergesYou, 0},
		{"in a heading ignored", "# Merges: lead\n", MergesYou, 0},
		{"mid-line ignored", "The lead merges: everything\n", MergesYou, 0},
		{"in a table ignored", "| Merges: lead | x |\n", MergesYou, 0},
		{"unknown value", "# T\n\nMerges: Sam\n", MergesYou, 3},
		{"the lead", "Merges: the lead\n", MergesYou, 1},
		{"empty value", "Merges:\n", MergesYou, 1},
		{"unknown first line wins", "Merges: both\nMerges: lead\n", MergesYou, 1},
	} {
		got, w := Merges([]byte(tc.src))
		if got != tc.want {
			t.Errorf("%s: Merges(%q) = %q, want %q", tc.name, tc.src, got, tc.want)
		}
		switch {
		case tc.warnLine == 0 && w != nil:
			t.Errorf("%s: unexpected warning %v", tc.name, w)
		case tc.warnLine != 0 && w == nil:
			t.Errorf("%s: no warning, want one on line %d", tc.name, tc.warnLine)
		case w != nil && (w.Line != tc.warnLine || w.Kind != WarnUnknownMerges):
			t.Errorf("%s: warning %+v, want kind %s on line %d", tc.name, *w, WarnUnknownMerges, tc.warnLine)
		}
	}
}

// Parse carries the Merges line into the plan, and an unknown value into
// its warnings, in line order with the others.
func TestParseMerges(t *testing.T) {
	src := "# Plan\n\nMerges: lead\n\n| Package | Needs |\n|---|---|\n| **A1 One** | — |\n| **A2 Two** | A1 |\n"
	if p := Parse([]byte(src)); p.Merges != MergesLead || len(p.Warnings) != 0 {
		t.Errorf("Merges = %q, warnings %v; want lead, none", p.Merges, p.Warnings)
	}

	src = "# Plan\n\n| Package | Needs |\n|---|---|\n| **A1 One** | — |\n| **A2 Two** | A9 |\n\nMerges: the reviewer\n"
	p := Parse([]byte(src))
	if p.Merges != MergesYou {
		t.Errorf("Merges = %q, want you", p.Merges)
	}
	if len(p.Warnings) != 2 {
		t.Fatalf("warnings = %v, want the unknown ID and the Merges line", p.Warnings)
	}
	if w := p.Warnings[0]; w.Kind != WarnUnknownID || w.Line != 6 {
		t.Errorf("first warning = %+v, want unknown-id on line 6", w)
	}
	w := p.Warnings[1]
	if w.Kind != WarnUnknownMerges || w.Line != 8 || w.ID != "" {
		t.Errorf("second warning = %+v, want unknown-merges on line 8", w)
	}
	for _, part := range []string{`"the reviewer"`, "Merges: you", "Merges: lead", `Read as "you"`} {
		if !strings.Contains(w.Message, part) {
			t.Errorf("warning %q lacks %q", w.Message, part)
		}
	}
}
