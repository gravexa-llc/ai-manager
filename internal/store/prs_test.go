package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPRsRoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	open := PR{
		Repo:              "example-org/app",
		Number:            7,
		HeadBranch:        "aim/b1-store",
		HeadRefOID:        "0123abcd",
		BaseRef:           "main",
		IsCrossRepository: false,
		Title:             "Store (B1)",
		Author:            "example-user",
		State:             PROpen,
		Draft:             true,
		Mergeable:         "MERGEABLE",
		MergeStateStatus:  "CLEAN",
		ReviewDecision:    "",
		ChecksState:       "SUCCESS",
		PlanSlug:          "aim",
		PlanPkg:           "B1",
		URL:               "https://github.com/example-org/app/pull/7",
		CreatedAt:         at(-2 * time.Hour),
		UpdatedAt:         at(time.Hour),
		FirstSeenAt:       at(-time.Hour),
	}
	merged := PR{Repo: "example-org/app", Number: 3, HeadBranch: "aim/a1-scan", State: PRMerged, Mergeable: "UNKNOWN", UpdatedAt: at(0), MergedAt: at(0), ClosedAt: at(0)}
	fork := PR{Repo: "example-org/web", Number: 11, HeadBranch: "patch-1", IsCrossRepository: true, State: PROpen}
	must(t, s.UpsertPRs(ctx, open, merged, fork))
	merged.FirstSeenAt = t0 // zero means now
	fork.FirstSeenAt = t0

	got, err := s.GetPR(ctx, "Example-Org/App", 7) // nameWithOwner compares case-insensitively
	must(t, err)
	equal(t, "PR", got, open)

	all, err := s.ListPRs(ctx, PRFilter{})
	must(t, err)
	equal(t, "all PRs", all, []PR{merged, open, fork})

	for _, tc := range []struct {
		f    PRFilter
		want []PR
	}{
		{PRFilter{Repo: "example-org/app"}, []PR{merged, open}},
		{PRFilter{State: PROpen}, []PR{open, fork}},
		{PRFilter{Repo: "example-org/app", HeadBranch: "aim/b1-store"}, []PR{open}},
		{PRFilter{HeadBranch: "AIM/B1-STORE"}, nil}, // branch names are case-sensitive
		{PRFilter{PlanSlug: "AIM"}, []PR{open}},
	} {
		got, err := s.ListPRs(ctx, tc.f)
		must(t, err)
		equal(t, "ListPRs", got, tc.want)
	}

	// An update rewrites every field but FirstSeenAt.
	open.State = PRMerged
	open.Draft = false
	open.Mergeable = "UNKNOWN"
	open.ChecksState = ""
	open.MergedAt = at(2 * time.Hour)
	open.ClosedAt = at(2 * time.Hour)
	update := open
	update.FirstSeenAt = at(3 * time.Hour) // ignored: the stored PR keeps its first sighting
	update.CreatedAt = time.Time{}         // not known to this writer: the stored one stays
	must(t, s.UpsertPRs(ctx, update))
	got, err = s.GetPR(ctx, open.Repo, open.Number)
	must(t, err)
	equal(t, "PR after update", got, open)

	// A creation time arriving later is stored.
	merged.CreatedAt = at(-48 * time.Hour)
	must(t, s.UpsertPRs(ctx, merged))
	got, err = s.GetPR(ctx, merged.Repo, merged.Number)
	must(t, err)
	equal(t, "PR given its creation time", got, merged)
}

// OpenedAt is GitHub's creation time, else the first sighting.
func TestPROpenedAt(t *testing.T) {
	p := PR{FirstSeenAt: at(time.Hour)}
	equal(t, "without createdAt", p.OpenedAt(), at(time.Hour))
	p.CreatedAt = at(-time.Hour)
	equal(t, "with createdAt", p.OpenedAt(), at(-time.Hour))
}

func TestPRErrors(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.GetPR(ctx, "example-org/app", 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetPR on a missing PR: %v, want ErrNotFound", err)
	}
	for _, p := range []PR{{Number: 1}, {Repo: "example-org/app"}, {Repo: "example-org/app", Number: -1}} {
		if err := s.UpsertPRs(ctx, p); err == nil {
			t.Errorf("UpsertPRs(%+v) succeeded", p)
		}
	}
}

func TestPRRef(t *testing.T) {
	if got := PRRef("example-org/app", 171); got != "example-org/app#171" {
		t.Errorf("PRRef = %q", got)
	}
}
