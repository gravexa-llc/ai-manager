package web

// Sections that fold, and Group by: what the pages with grouped lists
// share (Pull requests, Worktrees & cleanup, Sessions and Needs you).
//
// # Sections
//
// Each group of a list is a <details class="group" data-group="<key>">
// whose <summary class="ghead"> is its header, with its count (the
// "group-open" partial in shell.html opens it). The page decides how it
// starts (listGroup.Open): collapsed when nothing in it is on a plan or a
// monitored session's ("Not on a plan", "Not monitored", a repo whose work
// is all off plan), open otherwise; and Force opens it whatever the
// browser remembers, when it holds the selected row or the search (?q=)
// matched rows in it. Without the script that is all: the browser folds
// and opens them. static/app-groups.js remembers what the user folds or
// opens, per browser (localStorage, by page and key), puts it back on
// load and after every live refresh, and opens the sections with rows
// while the header's filter hides the others.
//
// # Group by
//
// A page with a Group by control (Pull requests, Worktrees & cleanup,
// Needs you) reads its choice from ?group=, so links and refreshes keep
// it: the page's own links carry it on when the URL has it. A choice is
// also remembered as the page's default, in the cookie aim_group_<page>,
// which a URL without ?group= follows. app-groups.js sets the cookie when
// a Group by link is clicked; without the script, a whole page loaded with
// ?group= sets it (groupBy). A fragment never does, so a live refresh in
// one tab doesn't change the default another tab follows. A cookie rather
// than a stored setting: it is one browser's view of a list, the server
// render needs it, and a GET may set it without writing to aimd's store.

import (
	"cmp"
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// The Group by choices (?group=).
const (
	groupRepo    = "repo"
	groupPlan    = "plan"
	groupStatus  = "status"  // Worktrees: the cleanup verdicts
	groupSession = "session" // Needs you: each wait's session
)

// Section keys and headers the pages share.
const (
	// notOnPlanKey and notOnPlanLabel are Plan grouping's last section:
	// what is on no registered plan and no monitored session's. It starts
	// collapsed.
	notOnPlanKey   = "plan:-none"
	notOnPlanLabel = "Not on a plan"
	// watchedKey is Plan grouping's section of monitored work on no plan
	// (noPlanLabel).
	watchedKey = "plan:-watched"
)

// listGroup is what a section's <details> carries (the "group-open"
// partial).
type listGroup struct {
	// Key names the section in the browser's memory of what was folded:
	// "repo:acme/app", "plan:search-v2". It is unique on its page.
	Key string
	// Open is how the section starts; Force opens it whatever the
	// browser remembers (data-group-force).
	Open, Force bool
}

// groupChoice is one choice of a page's Group by control (the "group-by"
// partial).
type groupChoice struct {
	Value, Label string
	// URL is the page as it is now with this choice; On marks the current
	// one.
	URL string
	On  bool
}

// groupControl is a page's Group by control.
type groupControl struct {
	// Cookie is the cookie that remembers the choice, for app-groups.js.
	Cookie  string
	Choices []groupChoice
}

// groupCookieAge is how long a Group by is remembered: 400 days, the
// most a browser keeps a cookie.
const groupCookieAge = 400 * 24 * 60 * 60

// groupCookie names the cookie that remembers a page's Group by.
func groupCookie(page string) string { return "aim_group_" + page }

// groupBy is a page's Group by: ?group= when it names one of choices
// (the first is the default), else the one the page's cookie remembers,
// else the default. kept is ?group= when it is valid, which the page's
// own links carry on, and "" otherwise. A whole page loaded with a
// ?group= sets the cookie; a fragment doesn't (see the file comment).
func groupBy(r *http.Request, d *appData, page string, choices []groupChoice) (cur, kept string, ctl groupControl) {
	valid := func(v string) bool {
		return slices.ContainsFunc(choices, func(c groupChoice) bool { return c.Value == v })
	}
	name := groupCookie(page)
	remembered := ""
	if c, err := r.Cookie(name); err == nil && valid(c.Value) {
		remembered = c.Value
	}
	cur = cmp.Or(remembered, choices[0].Value)
	if v := r.URL.Query().Get("group"); valid(v) {
		cur, kept = v, v
		if d.Fragment == "" && v != remembered {
			d.cookies = append(d.cookies, &http.Cookie{Name: name, Value: v, Path: "/app/", MaxAge: groupCookieAge, SameSite: http.SameSiteLaxMode})
		}
	}
	ctl = groupControl{Cookie: name, Choices: slices.Clone(choices)}
	for i := range ctl.Choices {
		ctl.Choices[i].URL = withGroup(r.URL, ctl.Choices[i].Value)
		ctl.Choices[i].On = ctl.Choices[i].Value == cur
	}
	return cur, kept, ctl
}

// withGroup is the page u shows, without ?fragment=, ?toast= or ?flash=,
// with ?group=v.
func withGroup(u *url.URL, v string) string {
	q := u.Query()
	q.Del("fragment")
	q.Del("toast")
	q.Del("flash")
	q.Set("group", v)
	return u.Path + "?" + q.Encode()
}

// registeredPlans are the registered plans in the order they were
// registered (the menu's): the snapshot's boards, or, in "all" mode,
// where the snapshot loads none, the plan service's list.
func (s *Server) registeredPlans(ctx context.Context, sn *snapshot) []api.Plan {
	if len(sn.boards) > 0 || s.opts.Plans == nil {
		return sn.boards
	}
	list, err := s.opts.Plans.Plans(ctx)
	if err != nil {
		s.log.Error("list the plans for a page's groups", "err", err)
		return nil
	}
	return list.Plans
}

// planIndex finds the registered plan with the slug (any case) among
// plans: its index, or -1.
func planIndex(plans []api.Plan, slug string) int {
	if slug == "" {
		return -1
	}
	return slices.IndexFunc(plans, func(p api.Plan) bool { return strings.EqualFold(p.Slug, slug) })
}

// planKey is a registered plan's section key.
func planKey(slug string) string { return "plan:" + strings.ToLower(slug) }

// repoKey is a repo's section key.
func repoKey(repo string) string { return "repo:" + strings.ToLower(repo) }

// byInvolvedThenName orders Repo grouping's sections: those with work on
// a plan or a monitored session's first, then by name in any case.
func byInvolvedThenName(aInvolved bool, aName string, cInvolved bool, cName string) int {
	if aInvolved != cInvolved {
		if aInvolved {
			return -1
		}
		return 1
	}
	return cmp.Compare(strings.ToLower(aName), strings.ToLower(cName))
}
