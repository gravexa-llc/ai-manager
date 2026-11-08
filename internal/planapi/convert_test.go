package planapi

import (
	"slices"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

func TestConvention(t *testing.T) {
	for _, c := range []struct{ slug, id, title, branch, line string }{
		{"aim", "C3", "Plan API", "aim/c3-plan-api", "Plan: aim · C3"},
		{"sharing", "A2", "Token revocation", "sharing/a2-token-revocation", "Plan: sharing · A2"},
		{"aim", "E1", "Band progress and /aim", "aim/e1-band-progress-and-aim", "Plan: aim · E1"},
		{"aim", "C4", "Mod v1: acting", "aim/c4-mod-v1-acting", "Plan: aim · C4"},
		{"sharing", "Q3", "Pilot with two customers", "sharing/q3-pilot-with-two-customers", "Plan: sharing · Q3"},
		{"x", "B3b-2", "A very long title that goes on and on", "x/b3b-2-a-very-long-title", "Plan: x · B3b-2"},
		{"x", "A1", "Ñandú über café", "x/a1-and-ber-caf", "Plan: x · A1"},
		{"x", "A1", "", "x/a1", "Plan: x · A1"},
		{"x", "A1", "Supercalifragilisticexpialidocious-and-more-words", "x/a1-supercalifragilisticexpialidoc", "Plan: x · A1"},
	} {
		got := convention(c.slug, c.id, c.title)
		if got.SuggestedBranch != c.branch || got.PlanLine != c.line {
			t.Errorf("convention(%q, %q, %q) = %+v, want %q, %q", c.slug, c.id, c.title, got, c.branch, c.line)
		}
	}
}

func TestBand(t *testing.T) {
	for _, c := range []struct {
		done, total, ready, ny int
		want                   string
	}{
		{6, 14, 3, 2, "sharing · 6/14 done · 3 ready · 2 need you"},
		{6, 14, 3, 1, "sharing · 6/14 done · 3 ready · 1 needs you"},
		{0, 15, 5, 0, "sharing · 0/15 done · 5 ready · 0 need you"},
	} {
		if got := band("sharing", c.done, c.total, c.ready, c.ny); got != c.want {
			t.Errorf("band = %q, want %q", got, c.want)
		}
	}
}

// A summary's need-you count is the plan's Open inbox items: each
// is listed, and its reason goes on its package when the plan has it.
// Badges, your own steps and `you:` steps no longer count; to_merge is the
// pass's PRs for the lead.
func TestSummaryNeeds(t *testing.T) {
	at := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	b := api.Plan{Slug: "Sharing", Total: 3, Counts: map[string]int{"done": 1}, ReadyToDispatch: []string{"A2"}, Packages: []api.Package{
		{ID: "A1", Lifecycle: "done"},
		{ID: "A2", Lifecycle: "ready", NeedsYou: "run the check", Badges: []api.Badge{{Kind: "ci_failing", Text: "CI failing on #3"}}},
		{ID: "V1", Lifecycle: "in_review", Badges: []api.Badge{{Kind: "waiting_on_you", Text: "waiting on you on #7"}}},
		{ID: "Y1", Lifecycle: "ready", Yours: true},
	}}
	n := needs{
		items: map[string][]store.InboxItem{"sharing": {
			{ID: 4, Source: "session", Kind: "permission", Title: "Agent · V1", Detail: "permission prompt: Bash: make check", PlanSlug: "sharing", Pkg: "v1", Since: at},
			{ID: 5, Source: "pr", Ref: "o/app#7", Kind: "merge", Title: "app #7 · Reports", PlanSlug: "sharing", Pkg: "V1", PR: "o/app#7", Since: at.Add(time.Minute)},
			{ID: 6, Source: "session", Kind: "reply", Title: "lead", Detail: "waiting for your reply", PlanSlug: "sharing", Since: at.Add(2 * time.Minute)},
			{ID: 7, Source: "session", Kind: "prompt", Title: "gone package", PlanSlug: "sharing", Pkg: "Z9", Since: at.Add(3 * time.Minute)},
		}},
		toMerge: map[string][]string{"sharing": {"o/app#9"}, "other": {"o/app#1"}},
	}
	sum := summaryOf(b, time.Time{}, n)
	if sum.NeedsYou != 4 || len(sum.NeedsYouItems) != 4 || sum.Band != "Sharing · 1/3 done · 1 ready · 4 need you" {
		t.Errorf("needs you %d, items %d, band %q; want 4, 4", sum.NeedsYou, len(sum.NeedsYouItems), sum.Band)
	}
	var reasons []string
	for _, it := range sum.NeedsYouItems {
		reasons = append(reasons, it.Reason)
	}
	if want := []string{"permission prompt: Bash: make check", "ready to merge on #7", "waiting for your reply", "waiting on you"}; !slices.Equal(reasons, want) {
		t.Errorf("item reasons %q, want %q", reasons, want)
	}
	if it := sum.NeedsYouItems[1]; it.ID != 5 || it.URL != "/app/inbox?item=5" || it.KindLabel != "Ready to merge" || it.PR != "o/app#7" || it.Pkg != "V1" || !it.Since.Equal(at.Add(time.Minute)) {
		t.Errorf("PR item = %+v", it)
	}
	if len(sum.NeedsYouPackages) != 1 || sum.NeedsYouPackages[0].ID != "V1" ||
		!slices.Equal(sum.NeedsYouPackages[0].Reasons, []string{"permission prompt: Bash: make check", "ready to merge on #7"}) {
		t.Errorf("needs-you packages = %+v, want V1 alone with its two items", sum.NeedsYouPackages)
	}
	if sum.ToMerge != 1 || !slices.Equal(sum.ToMergePRs, []string{"o/app#9"}) {
		t.Errorf("to merge %d %v, want 1 [o/app#9]", sum.ToMerge, sum.ToMergePRs)
	}

	// Nothing waits: zeros and empty lists, never null.
	quiet := summaryOf(b, time.Time{}, needs{})
	if quiet.NeedsYou != 0 || quiet.NeedsYouItems == nil || quiet.NeedsYouPackages == nil || quiet.ToMerge != 0 || quiet.ToMergePRs == nil {
		t.Errorf("quiet summary = %d %v %v %d %v", quiet.NeedsYou, quiet.NeedsYouItems, quiet.NeedsYouPackages, quiet.ToMerge, quiet.ToMergePRs)
	}
}
