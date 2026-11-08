package state

import (
	"strings"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/store"
)

// Each Unmatched entry says when it appeared and whether that was after
// the plan was registered: a PR by GitHub's createdAt (else when aimd first
// stored it), a branch by its worktree's first sighting.
func TestUnmatchedNewAndOlder(t *testing.T) {
	in := baseInput(t)
	old := pr(40, "docs/old-readme", "Tidy the README")
	old.CreatedAt = registered.Add(-30 * 24 * time.Hour)
	old.FirstSeenAt = registered.Add(time.Hour) // the backfill stored it after registration
	fresh := pr(41, "fix/typo", "Fix a typo")
	fresh.CreatedAt = registered.Add(2 * time.Hour)
	unread := pr(42, "chore/deps", "Bump deps") // no createdAt: its first sighting counts
	unread.FirstSeenAt = registered.Add(-time.Hour)
	in.PRs = []store.PR{old, fresh, unread}
	in.Worktrees = append(in.Worktrees,
		seenSince(worktree("old-try", "try/old"), registered.Add(-48*time.Hour)),
		worktree("new-try", "try/new"))

	got := map[string]Unmatched{}
	for _, u := range derive(in).Unmatched {
		got[u.Ref] = u
	}
	for ref, want := range map[string]struct {
		since time.Time
		new   bool
	}{
		prRef(40):         {old.CreatedAt, false},
		prRef(41):         {fresh.CreatedAt, true},
		prRef(42):         {unread.FirstSeenAt, false},
		repo + ":try/old": {registered.Add(-48 * time.Hour), false},
		repo + ":try/new": {registered.Add(time.Hour), true},
	} {
		u, ok := got[ref]
		if !ok {
			t.Errorf("%s isn't unmatched: %+v", ref, got)
			continue
		}
		if !u.Since.Equal(want.since) || u.New != want.new {
			t.Errorf("%s: since %v new %v, want %v %v", ref, u.Since, u.New, want.since, want.new)
		}
	}
}

// A target you hid from a plan leaves its Unmatched lane for Hidden, most
// recently hidden first, matched as targets are (a repo in any case). Other
// plans still list it.
func TestUnmatchedHidden(t *testing.T) {
	in := baseInput(t)
	other := mdPlan(t, 2, strings.Replace(miniPlan, "Plan slug: aim", "Plan slug: web", 1), registered, repo)
	other.ID = 2
	in.Plans = append(in.Plans, other)
	in.PRs = []store.PR{pr(40, "docs/readme", "Tidy the README"), pr(41, "fix/typo", "Fix a typo")}
	in.Worktrees = append(in.Worktrees, worktree("try", "try/search"))
	in.Hidden = []store.HiddenUnmatched{
		{PlanID: 1, Kind: store.LinkPR, Ref: strings.ToUpper(repo) + "#40", HiddenAt: now.Add(-2 * time.Hour)},
		{PlanID: 1, Kind: store.LinkBranch, Ref: repo + ":try/search", HiddenAt: now.Add(-time.Hour)},
	}
	states := Derive(in, Match(in), Options{})

	var shown, hidden []string
	for _, u := range states[0].Unmatched {
		shown = append(shown, u.Ref)
	}
	for _, u := range states[0].Hidden {
		hidden = append(hidden, u.Ref)
		if u.HiddenAt.IsZero() {
			t.Errorf("%s has no HiddenAt", u.Ref)
		}
	}
	deepEq(t, "shown", shown, []string{prRef(41)})
	deepEq(t, "hidden", hidden, []string{repo + ":try/search", prRef(40)})
	if n := len(states[1].Unmatched); n != 3 || len(states[1].Hidden) != 0 {
		t.Errorf("the other plan: %d unmatched, %d hidden; want 3 and 0", n, len(states[1].Hidden))
	}
}

// The What cell goes from the parse to the board as written.
func TestWhatOnTheBoard(t *testing.T) {
	in := baseInput(t)
	in.Plans = []Plan{mdPlan(t, 1, `# Search v2

| Package | What | Needs |
|---|---|---|
| **A1 Index builder** | Builds the `+"`search`"+` index **nightly**. | — |
| **A2 Query API** | | A1 |
`, registered, repo)}
	ps := derive(in)
	if got := pkgOf(t, ps, "A1").What; got != "Builds the `search` index **nightly**." {
		t.Errorf("A1 What = %q", got)
	}
	if got := pkgOf(t, ps, "A2").What; got != "" {
		t.Errorf("A2 What = %q", got)
	}
}
