package plans

import "testing"

func TestSlugFixtures(t *testing.T) {
	for _, tc := range []struct{ path, fixture, want string }{
		{"ROADMAP.md", "ROADMAP.md", "aim"}, // "plan slug `aim`" in the status line
		{"docs/plans/sharing-plan.md", "sharing-plan.md", "sharing"},
		{"edge-cases.md", "edge-cases.md", "edge-cases"},
	} {
		if got := Slug(tc.path, readFixture(t, tc.fixture)); got != tc.want {
			t.Errorf("Slug(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestSlug(t *testing.T) {
	for _, tc := range []struct{ name, path, src, want string }{
		{"stem", "docs/Surge-Plan.md", "# Surge\n", "surge"},
		{"stem without -plan", "/x/roadmap.md", "", "roadmap"},
		{"only -plan", "-plan.md", "", "-plan"},
		{"no extension", "notes", "", "notes"},
		{"marker line", "x-plan.md", "# T\n\nPlan slug: GEO3D-EU\n", "geo3d-eu"},
		{"marker in a list, bold, code", "x-plan.md", "- **Plan slug:** `routes`\n", "routes"},
		{"marker in a blockquote", "x-plan.md", "> Plan slug: q4\n", "q4"},
		{"trailing period", "x-plan.md", "Plan slug: aim.\n", "aim"},
		{"inline in a status line", "x-plan.md", "Status: v1 · plan slug `meadow` · draft\n", "meadow"},
		{"inline with a colon", "x-plan.md", "Owner Sam, Plan slug: `meadow`, draft\n", "meadow"},
		{"first marker wins", "x-plan.md", "Plan slug: one\nPlan slug: two\n", "one"},
		{"inline needs code markers", "x-plan.md", "the plan slug is decided later\n", "x"},
		{"marker in a fence is ignored", "x-plan.md", "```\nPlan slug: nope\n```\n", "x"},
		{"marker in a comment is ignored", "x-plan.md", "<!--\nPlan slug: nope\n-->\n", "x"},
		{"marker in a heading is ignored", "x-plan.md", "# Plan slug: nope\n", "x"},
		{"empty marker", "x-plan.md", "Plan slug:\n", "x"},
	} {
		if got := Slug(tc.path, []byte(tc.src)); got != tc.want {
			t.Errorf("%s: Slug(%q, %q) = %q, want %q", tc.name, tc.path, tc.src, got, tc.want)
		}
	}
}
