package daemon

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/config"
	"github.com/gravexa-llc/ai-manager/internal/github"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// searchingGitHub is a fakeGitHub that can also search a repo's PRs
// (PRSearcher).
type searchingGitHub struct {
	*fakeGitHub
	mu    sync.Mutex
	prs   map[string][]github.PR // "owner/name" as asked → PRs (under their canonical repo)
	asked [][]string
}

func (f *searchingGitHub) RepoPRs(ctx context.Context, repos []string) github.RepoPRsResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, slices.Clone(repos))
	var res github.RepoPRsResult
	for _, r := range repos {
		st := github.RepoSearch{Repo: r, PRs: len(f.prs[r])}
		for _, pr := range f.prs[r] {
			st.Name = pr.Repo
			res.PRs = append(res.PRs, pr)
		}
		res.Repos = append(res.Repos, st)
		res.Queries++
	}
	return res
}

func backfillDaemon(t *testing.T, gh GitHub, st Store, n *notes) *Daemon {
	t.Helper()
	return New(Options{
		Config: config.Default(),
		Store:  st,
		GitHub: gh,
		Notify: n.notify,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func TestBackfillPRs(t *testing.T) {
	created := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	gh := &searchingGitHub{fakeGitHub: &fakeGitHub{}, prs: map[string][]github.PR{
		"old-owner/app": {
			{Repo: "new-owner/app", Number: 2, State: github.StateMerged, HeadRef: "aim/c1-plan-parser",
				Plan: github.PlanRef{Slug: "aim", ID: "C1"}, CreatedAt: created, MergedAt: created.Add(time.Hour)},
			{Repo: "new-owner/app", Number: 3, State: github.StateOpen, HeadRef: "aim/c3-plan-api", CreatedAt: created.Add(24 * time.Hour)},
		},
	}}
	st := newFakeStore()
	n := &notes{}
	d := backfillDaemon(t, gh, st, n)

	if d.LastScan() != nil {
		t.Error("LastScan before any scan is not nil")
	}
	if got := d.CanonicalRepo("old-owner/app"); got != "old-owner/app" {
		t.Errorf("CanonicalRepo before GitHub answered = %q", got)
	}
	d.BackfillPRs(context.Background(), []string{"old-owner/app"})

	if len(gh.asked) != 1 || !slices.Equal(gh.asked[0], []string{"old-owner/app"}) {
		t.Errorf("RepoPRs asked %v", gh.asked)
	}
	merged := st.pr("new-owner/app", 2)
	if merged.State != "MERGED" || merged.PlanSlug != "aim" || merged.PlanPkg != "C1" || !merged.FirstSeenAt.Equal(created) {
		t.Errorf("merged PR stored as %+v; want MERGED, Plan: aim · C1, first seen at its creation %v", merged, created)
	}
	if open := st.pr("new-owner/app", 3); !open.FirstSeenAt.Equal(created.Add(24 * time.Hour)) {
		t.Errorf("open PR first seen %v, want its creation", open.FirstSeenAt)
	}
	if n.count(api.TopicPRs) != 1 {
		t.Errorf("prs notified %d times, want 1", n.count(api.TopicPRs))
	}
	if got := d.CanonicalRepo("OLD-OWNER/app"); got != "new-owner/app" {
		t.Errorf("CanonicalRepo after the backfill = %q, want new-owner/app", got)
	}

	// The same answer again changes nothing.
	d.BackfillPRs(context.Background(), []string{"old-owner/app"})
	if n.count(api.TopicPRs) != 1 {
		t.Errorf("an unchanged backfill notified prs again")
	}
}

func TestBackfillPRsSkips(t *testing.T) {
	// A client that can't search: nothing happens.
	plain := &fakeGitHub{}
	st := newFakeStore()
	backfillDaemon(t, plain, st, &notes{}).BackfillPRs(context.Background(), []string{"o/app"})
	if prs, _ := st.ListPRs(context.Background(), store.PRFilter{}); len(prs) != 0 {
		t.Errorf("stored %d PRs without a searcher", len(prs))
	}

	// The budget says stop: no search.
	gh := &searchingGitHub{fakeGitHub: &fakeGitHub{budget: func(time.Time) github.Budget {
		return github.Budget{Verdict: github.BudgetStop}
	}}}
	backfillDaemon(t, gh, newFakeStore(), &notes{}).BackfillPRs(context.Background(), []string{"o/app"})
	if len(gh.asked) != 0 {
		t.Errorf("searched under a stop budget: %v", gh.asked)
	}
}
