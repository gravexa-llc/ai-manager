package daemon

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/github"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// searchSource returns the github_search source from the daemon's status.
func searchSource(t *testing.T, r *rig) api.Source {
	t.Helper()
	for _, s := range r.d.Status().Sources {
		if s.Name == api.SourceGitHubSearch {
			return s
		}
	}
	t.Fatal("no github_search source")
	return api.Source{}
}

// The review-request search runs right after the open-PR search, on
// its cadence. Its PRs are stored, someone else's included, and marked as
// asking for your review until a search of their owner no longer finds
// them. A failed or cut-off search changes no request.
func TestReviewRequestSearch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := ghRig(t)
		theirs := github.PR{Repo: "o/brand", Number: 2, State: github.StateOpen, HeadRef: "feat/tagline", HeadOID: "d59f",
			Mergeable: github.Mergeable, MergeState: github.MergeStateClean, Checks: github.ChecksNone, Author: "other-user"}
		mine := github.PR{Repo: "o/a", Number: 7, State: github.StateOpen, HeadRef: "feat-a", HeadOID: "h1",
			Mergeable: github.Mergeable, MergeState: github.MergeStateClean, Checks: github.ChecksNone}
		r.gh.review = []github.PR{theirs, mine}
		base := time.Now()
		r.start(t)
		synctest.Wait()

		_, search := r.gh.calls()
		r.gh.mu.Lock()
		reviews := slices.Clone(r.gh.reviewReq)
		r.gh.mu.Unlock()
		if len(search) != 1 || len(reviews) != 1 || !reviews[0].at.Equal(search[0].at) || !slices.Equal(reviews[0].owner, []string{"o"}) {
			t.Fatalf("searches %+v, review searches %+v; want one of each, together, for [o]", search, reviews)
		}
		for _, got := range []store.PR{r.st.pr("o/brand", 2), r.st.pr("o/a", 7)} {
			if !got.ReviewRequestedAt.Equal(base) || got.State != "OPEN" {
				t.Errorf("%s#%d = %+v, want stored with a review request since %v", got.Repo, got.Number, got, base)
			}
		}
		if calls := r.st.reviews(); len(calls) != 1 || !slices.Equal(calls[0].owners, []string{"o"}) || len(calls[0].found) != 2 {
			t.Errorf("SetReviewRequests calls = %+v", calls)
		}
		if n := r.notes.count(api.TopicPRs); n < 1 {
			t.Errorf("PR notifications = %d, want at least 1", n)
		}

		// You review #7: the next search doesn't find it.
		r.gh.mu.Lock()
		r.gh.review = []github.PR{theirs}
		r.gh.mu.Unlock()
		notes := r.notes.count(api.TopicPRs)
		time.Sleep(300 * sec)
		synctest.Wait()
		if got := r.st.pr("o/a", 7); got.ReviewRequested() {
			t.Errorf("#7 after your review = %+v", got)
		}
		if got := r.st.pr("o/brand", 2); !got.ReviewRequestedAt.Equal(base) {
			t.Errorf("#2 lost its first-seen time: %+v", got)
		}
		if n := r.notes.count(api.TopicPRs); n != notes+1 {
			t.Errorf("PR notifications = %d, want %d (one for the cleared request)", n, notes+1)
		}

		// The owner's search fails: nothing changes, and the source says so.
		r.gh.mu.Lock()
		r.gh.review = nil
		r.gh.reviewErr = map[string]error{"o": errors.New("github: unavailable")}
		r.gh.mu.Unlock()
		time.Sleep(300 * sec)
		synctest.Wait()
		if got := r.st.pr("o/brand", 2); !got.ReviewRequested() {
			t.Errorf("a failed search cleared #2's request: %+v", got)
		}
		if calls := r.st.reviews(); len(calls) != 2 {
			t.Errorf("SetReviewRequests calls after a failed search = %d, want still 2", len(calls))
		}
		if src := searchSource(t, r); !strings.Contains(src.Error, "unavailable") || src.FailingSince.IsZero() {
			t.Errorf("github_search after a failed review search = %+v", src)
		}

		// A cut-off search can't clear anything either.
		r.gh.mu.Lock()
		r.gh.reviewErr, r.gh.reviewTruncated = nil, true
		r.gh.mu.Unlock()
		time.Sleep(300 * sec)
		synctest.Wait()
		if got := r.st.pr("o/brand", 2); !got.ReviewRequested() {
			t.Errorf("a cut-off search cleared #2's request: %+v", got)
		}
		if src := searchSource(t, r); src.Error != "" || !src.FailingSince.IsZero() {
			t.Errorf("github_search after a good search = %+v", src)
		}

		// Back to normal: an empty answer clears the last request.
		r.gh.mu.Lock()
		r.gh.reviewTruncated = false
		r.gh.mu.Unlock()
		time.Sleep(300 * sec)
		synctest.Wait()
		if got := r.st.pr("o/brand", 2); got.ReviewRequested() {
			t.Errorf("#2 after an empty search = %+v", got)
		}
	})
}
