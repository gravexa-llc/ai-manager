package planapi_test

import (
	"context"
	"net/http"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// POST /api/prs/refresh names the PR a tool call created, merged or
// closed, as the command did, and aimd asks for it to be read now.
func TestRefreshPRs(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	// A worktree of pivot on feat, whose open PR aimd has; and the same
	// branch name in pivot-web, which is another repo's.
	feat := filepath.Join(e.root, "pivot-feat")
	e.git(e.pivot, "worktree", "add", "-q", "-b", "feat", feat)
	e.rescan()
	must(t, e.st.UpsertPRs(ctx,
		store.PR{Repo: "acme/pivot", Number: 9, HeadBranch: "feat", State: store.PROpen, UpdatedAt: time.Now()},
		store.PR{Repo: "acme/pivot", Number: 3, HeadBranch: "feat", State: store.PRMerged, UpdatedAt: time.Now()},
		store.PR{Repo: "acme/pivot-web", Number: 4, HeadBranch: "feat", State: store.PROpen, UpdatedAt: time.Now()},
		store.PR{Repo: "Example-Org/Web", Number: 5, HeadBranch: "x", State: store.PROpen, UpdatedAt: time.Now()},
	))

	for _, c := range []struct {
		desc string
		req  api.PRRefreshRequest
		want []string
	}{
		{"a URL", api.PRRefreshRequest{Ref: "https://github.com/acme/pivot/pull/12", Action: "merge"}, []string{"acme/pivot#12"}},
		{"owner/repo#n", api.PRRefreshRequest{Ref: "acme/pivot-web#2", Action: "close"}, []string{"acme/pivot-web#2"}},
		{"stored under GitHub's spelling", api.PRRefreshRequest{Ref: "example-org/web#5"}, []string{"Example-Org/Web#5"}},
		{"a number with -R", api.PRRefreshRequest{Ref: "7", Repo: "acme/pivot-web"}, []string{"acme/pivot-web#7"}},
		{"a number with -R as a URL", api.PRRefreshRequest{Ref: "#7", Repo: "https://github.com/acme/pivot-web.git"}, []string{"acme/pivot-web#7"}},
		{"a number in the repo at cwd", api.PRRefreshRequest{Ref: "8", Cwd: e.pivot}, []string{"acme/pivot#8"}},
		{"a number in a worktree of it", api.PRRefreshRequest{Ref: "#8", Cwd: feat}, []string{"acme/pivot#8"}},
		{"no ref: the branch at cwd", api.PRRefreshRequest{Cwd: feat, Action: "merge"}, []string{"acme/pivot#9"}},
		{"a branch", api.PRRefreshRequest{Ref: "feat", Cwd: e.pivot}, []string{"acme/pivot#9"}},
		{"a branch in another repo", api.PRRefreshRequest{Ref: "feat", Repo: "acme/pivot-web"}, []string{"acme/pivot-web#4"}},
		{"a branch with no open PR", api.PRRefreshRequest{Ref: "spike", Cwd: e.pivot}, nil},
	} {
		got := ok[api.PRRefreshResult](t, e.req("POST", "/api/prs/refresh", c.req), http.StatusAccepted)
		asked := e.refresh.take()
		if !slices.Equal(got.PRs, c.want) && !(len(got.PRs) == 0 && len(c.want) == 0) {
			t.Errorf("%s: prs %q, want %q", c.desc, got.PRs, c.want)
		}
		if !slices.Equal(asked, c.want) || got.Queued != len(c.want) {
			t.Errorf("%s: asked to read %q (queued %d), want %q", c.desc, asked, got.Queued, c.want)
		}
	}

	for _, c := range []struct {
		desc string
		req  api.PRRefreshRequest
		says string
	}{
		{"a number, no repo or cwd", api.PRRefreshRequest{Ref: "12"}, "needs repo"},
		{"a relative cwd", api.PRRefreshRequest{Ref: "12", Cwd: "pivot"}, "absolute"},
		{"a cwd in no repo", api.PRRefreshRequest{Ref: "12", Cwd: e.tmp}, "isn't in a GitHub repo"},
		{"a bad repo", api.PRRefreshRequest{Ref: "12", Repo: "not a repo"}, "owner/repo"},
		{"number 0", api.PRRefreshRequest{Ref: "0", Repo: "acme/pivot"}, "PR number"},
		{"no ref, the main checkout", api.PRRefreshRequest{Cwd: e.pivot}, ""},
		{"not a branch", api.PRRefreshRequest{Ref: "a b", Repo: "acme/pivot"}, "neither"},
	} {
		rec := e.req("POST", "/api/prs/refresh", c.req)
		if c.says == "" {
			// The main checkout is on main, which has no PR: nothing to read.
			if got := ok[api.PRRefreshResult](t, rec, http.StatusAccepted); len(got.PRs) != 0 {
				t.Errorf("%s: prs %q, want none", c.desc, got.PRs)
			}
			continue
		}
		refused(t, c.desc, rec, http.StatusBadRequest, c.says)
	}
	if got := e.refresh.take(); len(got) != 0 {
		t.Errorf("refused requests asked to read %q", got)
	}
	refused(t, "no token", e.send("POST", "/api/prs/refresh", api.PRRefreshRequest{Ref: "acme/pivot#1"}), http.StatusUnauthorized)
}
