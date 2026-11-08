package github

import "testing"

func TestParsePlanLine(t *testing.T) {
	aimA2 := PlanRef{Slug: "aim", ID: "A2"}
	for _, tc := range []struct {
		name string
		body string
		want PlanRef // zero: no match
	}{
		// The canonical form, as CLAUDE.md asks for it.
		{"canonical", "Plan: aim · A2\n\n## What changed\n", aimA2},
		{"crlf", "Plan: aim · A2\r\n\r\n## What changed\r\n", aimA2},
		{"not first line", "Some intro.\n\nPlan: aim · A2\n", aimA2},
		{"first of two", "Plan: aim · A2\nPlan: other · B1\n", aimA2},
		{"leading space", "   Plan: aim · A2   ", aimA2},

		// Spacing.
		{"no spaces", "Plan:aim·A2", aimA2},
		{"extra spaces", "Plan  :   aim   ·   A2", aimA2},
		{"tab", "Plan:\taim\t·\tA2", aimA2},

		// Separators.
		{"bullet", "Plan: aim • A2", aimA2},
		{"dot operator", "Plan: aim ⋅ A2", aimA2},
		{"bullet operator", "Plan: aim ∙ A2", aimA2},
		{"spaced hyphen", "Plan: aim - A2", aimA2},
		{"double hyphen", "Plan: aim -- A2", aimA2},
		{"en dash", "Plan: aim – A2", aimA2},
		{"em dash unspaced", "Plan: aim—A2", aimA2},
		{"pipe", "Plan: aim | A2", aimA2},
		{"slash", "Plan: aim/A2", aimA2},
		{"colon", "Plan: aim: A2", aimA2},
		{"comma", "Plan: aim, A2", aimA2},
		{"hash", "Plan: aim #A2", aimA2},
		{"html entity", "Plan: aim &middot; A2", aimA2},
		{"numeric entity", "Plan: aim &#183; A2", aimA2},
		{"space only", "Plan: aim A2", aimA2},

		// Markdown and HTML around it.
		{"bold label", "**Plan:** aim · A2", aimA2},
		{"bold label colon outside", "**Plan**: aim · A2", aimA2},
		{"bold everything", "**Plan: aim · A2**", aimA2},
		{"italic label", "_Plan:_ aim · A2", aimA2},
		{"code value", "Plan: `aim · A2`", aimA2},
		{"blockquote", "> Plan: aim · A2", aimA2},
		{"list item", "- Plan: aim · A2", aimA2},
		{"html comment", "<!-- Plan: aim · A2 -->", aimA2},
		{"lower case label", "plan: aim · A2", aimA2},
		{"upper case label", "PLAN: aim · A2", aimA2},

		// IDs and slugs as they appear in real plans.
		{"sub-ID", "Plan: surge · B3b-2", PlanRef{"surge", "B3b-2"}},
		{"hyphenated slug", "Plan: sharing-plan · V1", PlanRef{"sharing-plan", "V1"}},
		{"hyphenated slug spaced hyphen", "Plan: sharing-plan - V1", PlanRef{"sharing-plan", "V1"}},
		{"digit slug", "Plan: geo3d · T1", PlanRef{"geo3d", "T1"}},
		{"case kept", "Plan: AIM · a2", PlanRef{"AIM", "a2"}},
		{"no digit with explicit separator", "Plan: aim · Q", PlanRef{"aim", "Q"}},
		{"trailing period", "Plan: aim · A2.", aimA2},
		{"trailing text after separator", "Plan: aim · A2 — GitHub client", aimA2},
		{"trailing parenthesis", "Plan: aim · A2 (GitHub client)", aimA2},

		// Not plan lines.
		{"empty", "", PlanRef{}},
		{"no plan line", "## What changed\nStuff.\n", PlanRef{}},
		{"package line", "Package: aim · A2", PlanRef{}},
		{"mid-sentence", "This follows the Plan: aim · A2 line.", PlanRef{}},
		{"no ID", "Plan: aim", PlanRef{}},
		{"no ID after separator", "Plan: aim · ", PlanRef{}},
		{"prose", "Plan: see the notes", PlanRef{}},
		{"prose with a digit word", "Plan: aim v2 rollout", PlanRef{}},
		{"unseparated hyphen", "Plan: aim-A2", PlanRef{}},
		{"ID starting with a digit", "Plan: aim · 0.8", PlanRef{}},
		{"ID glued to text", "Plan: aim · A2(x)", PlanRef{}},
		{"in a code fence", "```\nPlan: aim · A2\n```\n", PlanRef{}},
		{"in a tilde fence", "~~~md\nPlan: aim · A2\n~~~\n", PlanRef{}},
		{"after a code fence", "```\nPlan: other · B1\n```\nPlan: aim · A2\n", aimA2},
		{"label only", "Plan:", PlanRef{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParsePlanLine(tc.body)
			if got != tc.want || ok != !tc.want.IsZero() {
				t.Errorf("ParsePlanLine(%q) = %+v, %v; want %+v, %v", tc.body, got, ok, tc.want, !tc.want.IsZero())
			}
		})
	}
}

func TestPlanRefString(t *testing.T) {
	if s := (PlanRef{"aim", "A2"}).String(); s != "aim · A2" {
		t.Errorf("String() = %q", s)
	}
	if s := (PlanRef{}).String(); s != "" {
		t.Errorf("zero String() = %q", s)
	}
}

func TestMergeabilityPending(t *testing.T) {
	for m, want := range map[Mergeability]bool{Mergeable: false, Conflicting: false, MergePending: true, "": true, "SOMETHING_NEW": true} {
		if got := m.Pending(); got != want {
			t.Errorf("%q.Pending() = %v, want %v", m, got, want)
		}
	}
	// Unknown values are normalized to MergePending when read.
	if pr := (rawPR{Mergeable: "SOMETHING_NEW"}).toPR("o/r"); pr.Mergeable != MergePending || pr.MergeState != MergeStateUnknown || pr.Checks != ChecksNone {
		t.Errorf("toPR = %+v", pr)
	}
}
