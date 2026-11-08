package daemon

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/config"
	"github.com/gravexa-llc/ai-manager/internal/github"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
	"github.com/gravexa-llc/ai-manager/internal/worktrees"
)

// placeholder is the row POST /api/links/pr stores for a PR aimd hasn't
// read yet.
func placeholder(repo string, n int) store.PR {
	return store.PR{Repo: repo, Number: n, State: store.PROpen, URL: fmt.Sprintf("https://github.com/%s/pull/%d", repo, n)}
}

// numString writes one PRsByNumber request as "acme/app[5 6]".
func numString(c ghCall) string {
	var parts []string
	for _, rn := range c.nums {
		parts = append(parts, fmt.Sprintf("%s/%s%v", rn.Owner, rn.Name, rn.Numbers))
	}
	return strings.Join(parts, " ")
}

// githubSource returns the github source's status.
func githubSource(t *testing.T, d *Daemon) api.Source {
	t.Helper()
	for _, s := range d.Status().Sources {
		if s.Name == api.SourceGitHub {
			return s
		}
	}
	t.Fatal("no github source")
	return api.Source{}
}

// A PR stored only by its URL is read by number on idle ticks: once
// read it is a PR like any other; a repo that can't be read shows in
// Status until it can.
func TestUnreadPRsReadByNumber(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := ghRig(t)
		for _, p := range []store.PR{placeholder("acme/app", 5), placeholder("acme/app", 6), placeholder("example-org/web", 9)} {
			r.st.prs[prKey(p.Repo, p.Number)] = p
		}
		// A placeholder of a fork's PR can't be told from one of the
		// repo's own; this one was read already, and isn't asked again.
		r.st.prs[prKey("acme/app", 7)] = store.PR{Repo: "acme/app", Number: 7, State: store.PROpen, HeadBranch: "fix", IsCrossRepository: true}
		merged := time.Now().Add(-3 * time.Hour)
		r.gh.byNumber = map[string]github.PR{
			prKey("acme/app", 5): {Repo: "acme/app", Number: 5, State: github.StateMerged, HeadRef: "aim/w1-shell",
				Title: "App shell (W1)", UpdatedAt: merged, MergedAt: merged, ClosedAt: merged},
			prKey("acme/app", 6): {Repo: "acme/app", Number: 6, State: github.StateOpen, HeadRef: "aim/w2-inbox",
				Title: "Inbox (W2)", UpdatedAt: merged, Mergeable: github.Mergeable, MergeState: github.MergeStateClean},
		}
		r.gh.numberErr = map[string]error{"example-org/web": fmt.Errorf("%w: Could not resolve to a Repository", github.ErrNotFound)}
		base := time.Now()
		r.start(t)
		synctest.Wait()

		calls := r.gh.numberCalls()
		if len(calls) != 1 || numString(calls[0]) != "acme/app[5 6] example-org/web[9]" {
			t.Fatalf("PRsByNumber calls = %d (%v), want one for acme/app[5 6] example-org/web[9]", len(calls), calls)
		}
		if got := r.st.pr("acme/app", 5); got.State != "MERGED" || got.HeadBranch != "aim/w1-shell" || got.Unread() {
			t.Errorf("#5 after the read = %+v, want merged", got)
		}
		if got := r.st.pr("acme/app", 6); got.State != "OPEN" || got.MergeStateStatus != "CLEAN" || got.Unread() {
			t.Errorf("#6 after the read = %+v, want open and clean", got)
		}
		if got := r.st.pr("example-org/web", 9); !got.Unread() {
			t.Errorf("#9 = %+v, want still a placeholder", got)
		}
		if n := r.notes.count(api.TopicPRs); n < 1 {
			t.Error("no PR notification after the read")
		}
		un := r.d.Status().GitHub.Unreadable
		if len(un) != 1 || un[0].Repo != "example-org/web" || !strings.Contains(un[0].Error, "Could not resolve") || !un[0].Since.Equal(base) {
			t.Errorf("Unreadable = %+v, want example-org/web since %v", un, base)
		}
		if src := githubSource(t, r.d); src.PartialErrors != 1 || src.Stale {
			t.Errorf("github source = %+v, want 1 partial error, not stale", src)
		}
		if gs := r.d.Status().GitHub; gs.LastQueries != 2 || gs.LastCost != 2 {
			t.Errorf("last tick sent %d queries for %d points, want 2 and 2", gs.LastQueries, gs.LastCost)
		}

		// Non-idle ticks don't read by number, and leave the repo listed.
		time.Sleep(4*time.Minute + 30*sec)
		synctest.Wait()
		if n := len(r.gh.numberCalls()); n != 1 {
			t.Errorf("%d PRsByNumber calls by 4m30s, want 1", n)
		}
		if un := r.d.Status().GitHub.Unreadable; len(un) != 1 {
			t.Errorf("Unreadable after active ticks = %+v", un)
		}

		// The next idle tick asks only for what is still unread; once
		// GitHub answers, the repo is readable again.
		r.gh.mu.Lock()
		delete(r.gh.numberErr, "example-org/web")
		r.gh.byNumber[prKey("example-org/web", 9)] = github.PR{Repo: "example-org/web", Number: 9, State: github.StateClosed,
			HeadRef: "spike", UpdatedAt: base}
		r.gh.mu.Unlock()
		status := r.notes.count(api.TopicStatus)
		time.Sleep(time.Minute)
		synctest.Wait()
		calls = r.gh.numberCalls()
		if len(calls) != 2 || numString(calls[1]) != "example-org/web[9]" {
			t.Fatalf("PRsByNumber calls = %v, want a second for example-org/web[9]", calls)
		}
		if got := r.st.pr("example-org/web", 9); got.State != "CLOSED" || got.Unread() {
			t.Errorf("#9 = %+v, want closed", got)
		}
		if un := r.d.Status().GitHub.Unreadable; un != nil {
			t.Errorf("Unreadable = %+v, want none", un)
		}
		if r.notes.count(api.TopicStatus) == status {
			t.Error("no status notification when the repo became readable")
		}

		// Nothing unread: the idle tick sends no PRsByNumber.
		time.Sleep(5 * time.Minute)
		synctest.Wait()
		if n := len(r.gh.numberCalls()); n != 2 {
			t.Errorf("%d PRsByNumber calls with nothing unread, want 2", n)
		}
	})
}

// A PR number GitHub hasn't got leaves its repo listed as unreadable,
// even while the repo's branches read fine.
func TestUnreadPRMissingNumber(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := ghRig(t)
		r.st.prs[prKey("o/a", 41)] = placeholder("o/a", 41)
		r.start(t)
		synctest.Wait()
		un := r.d.Status().GitHub.Unreadable
		if len(un) != 1 || un[0].Repo != "o/a" {
			t.Fatalf("Unreadable = %+v, want o/a", un)
		}
		// Active ticks read o/a's branches fine; the number stays unread.
		time.Sleep(60 * sec)
		synctest.Wait()
		if un := r.d.Status().GitHub.Unreadable; len(un) != 1 {
			t.Errorf("Unreadable after a good branch read = %+v, want o/a still", un)
		}
		// Unlinked and gone from the store, it isn't asked about, and the
		// repo drops off the list on the next idle tick.
		r.st.mu.Lock()
		delete(r.st.prs, prKey("o/a", 41))
		r.st.mu.Unlock()
		time.Sleep(4 * time.Minute)
		synctest.Wait()
		if un := r.d.Status().GitHub.Unreadable; un != nil {
			t.Errorf("Unreadable after the PR went = %+v, want none", un)
		}
	})
}

// A repo whose remote is an SSH alias of github.com (resolved by the
// scan) is polled like any other, and its owner is searched when config
// names no owners.
func TestAliasRemoteIsPolledAndSearched(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newRig(t, func(c *config.Config) { c.Owners = map[string]string{} })
		old := time.Now().Add(-24 * time.Hour)
		app := repo("app", old, "main", "aim/w1-shell")
		app.Remotes = map[string]string{"origin": "git@github-acme:acme/app.git"}
		app.HostNames = map[string]string{"github-acme": "github.com"}
		other := repo("tools", old, "main", "spike")
		other.Remotes = map[string]string{"origin": "git@git-internal:acme/tools.git"}
		other.HostNames = map[string]string{"git-internal": "git.example.com"}
		r.scanner.set(&worktrees.Result{Repos: []worktrees.Repo{app, other}})
		r.start(t)
		synctest.Wait()

		branch, search := r.gh.calls()
		if len(branch) != 1 || reqString(branch[0]) != "acme/app[main aim/w1-shell]" {
			t.Errorf("BranchPRs = %v, want acme/app[main aim/w1-shell] only", branch)
		}
		if len(search) != 1 || !slices.Equal(search[0].owner, []string{"acme"}) {
			t.Errorf("searches = %+v, want one for [acme]", search)
		}
		if names := r.d.RepoNames(); names["/r/app"] != "acme/app" || names["/r/tools"] != "" {
			t.Errorf("RepoNames = %v, want /r/app → acme/app only", names)
		}
	})
}

// RefreshPRs reads the PRs the plugin saw a tool call create, merge or
// close at once, by number, then at most once every refreshMinGap,
// joining requests in between; GitHub's answer is what is stored.
func TestRefreshPRs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := ghRig(t)
		merged := time.Now()
		r.gh.byNumber = map[string]github.PR{
			prKey("o/a", 7): {Repo: "o/a", Number: 7, State: github.StateMerged, HeadRef: "feat-a", UpdatedAt: merged, MergedAt: merged},
			prKey("o/a", 8): {Repo: "o/a", Number: 8, State: github.StateClosed, HeadRef: "spike", UpdatedAt: merged},
			prKey("o/b", 9): {Repo: "o/b", Number: 9, State: github.StateOpen, HeadRef: "feat-b", UpdatedAt: merged},
		}
		r.st.prs[prKey("o/a", 7)] = store.PR{Repo: "o/a", Number: 7, State: store.PROpen, HeadBranch: "feat-a", UpdatedAt: merged.Add(-time.Hour)}
		r.start(t)
		synctest.Wait() // tick 0: nothing unread, so no PRsByNumber
		if n := len(r.gh.numberCalls()); n != 0 {
			t.Fatalf("%d PRsByNumber calls before any request", n)
		}

		if n := r.d.RefreshPRs([]store.PRKey{{Repo: "o/a", Number: 7}}); n != 1 {
			t.Errorf("RefreshPRs queued %d, want 1", n)
		}
		synctest.Wait()
		calls := r.gh.numberCalls()
		if len(calls) != 1 || numString(calls[0]) != "o/a[7]" {
			t.Fatalf("PRsByNumber calls = %v, want one for o/a[7]", calls)
		}
		if got := r.st.pr("o/a", 7); got.State != "MERGED" {
			t.Errorf("#7 after the read = %+v, want merged", got)
		}

		// Within refreshMinGap, requests wait and join one read; repeats
		// and bad keys aren't queued.
		if n := r.d.RefreshPRs([]store.PRKey{{Repo: "o/a", Number: 8}, {Repo: "O/A", Number: 8}, {Repo: "o/b", Number: 9},
			{Repo: "nope", Number: 1}, {Repo: "o/a", Number: 0}}); n != 2 {
			t.Errorf("RefreshPRs queued %d, want 2", n)
		}
		if n := r.d.RefreshPRs([]store.PRKey{{Repo: "o/b", Number: 9}}); n != 0 {
			t.Errorf("a repeat queued %d, want 0", n)
		}
		time.Sleep(refreshMinGap - sec)
		synctest.Wait()
		if n := len(r.gh.numberCalls()); n != 1 {
			t.Fatalf("%d PRsByNumber calls within refreshMinGap, want 1", n)
		}
		time.Sleep(sec)
		synctest.Wait()
		calls = r.gh.numberCalls()
		if len(calls) != 2 || numString(calls[1]) != "o/a[8] o/b[9]" {
			t.Fatalf("PRsByNumber calls = %v, want a second for o/a[8] o/b[9]", calls)
		}
		if got := r.st.pr("o/a", 8); got.State != "CLOSED" {
			t.Errorf("#8 = %+v, want closed", got)
		}

		// A repo GitHub can't read is listed; a later good read of another
		// repo doesn't clear it.
		r.gh.mu.Lock()
		r.gh.numberErr = map[string]error{"example-org/web": github.ErrNotFound}
		r.gh.mu.Unlock()
		r.d.RefreshPRs([]store.PRKey{{Repo: "example-org/web", Number: 3}})
		time.Sleep(refreshMinGap)
		synctest.Wait()
		if un := r.d.Status().GitHub.Unreadable; len(un) != 1 || un[0].Repo != "example-org/web" {
			t.Errorf("Unreadable = %+v, want example-org/web", un)
		}
		r.d.RefreshPRs([]store.PRKey{{Repo: "o/b", Number: 9}})
		time.Sleep(refreshMinGap)
		synctest.Wait()
		if un := r.d.Status().GitHub.Unreadable; len(un) != 1 {
			t.Errorf("Unreadable after a good read of o/b = %+v, want example-org/web still", un)
		}

		// No read while the budget says stop.
		before := len(r.gh.numberCalls())
		r.gh.mu.Lock()
		r.gh.budget = func(time.Time) github.Budget { return github.Budget{Verdict: github.BudgetStop} }
		r.gh.mu.Unlock()
		r.d.RefreshPRs([]store.PRKey{{Repo: "o/a", Number: 7}})
		time.Sleep(refreshMinGap)
		synctest.Wait()
		if n := len(r.gh.numberCalls()); n != before {
			t.Errorf("%d PRsByNumber calls under a stop budget, want %d", n, before)
		}
	})
}
