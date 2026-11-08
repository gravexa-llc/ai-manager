package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/store"
)

// searchEnv is planEnv with the Search v2 plan registered for acme/app,
// whose repo has an old PR no package claims (#45, opened months before
// the plan) and a new one (#46, opened after it).
func searchEnv(t *testing.T) *planEnv {
	t.Helper()
	e := newPlanEnv(t)
	pr := func(n int, branch, title string, created time.Time) store.PR {
		return store.PR{Repo: "acme/app", Number: n, HeadBranch: branch, BaseRef: "main", Title: title, State: store.PROpen,
			URL: "https://github.com/acme/app/pull/" + strconv.Itoa(n), CreatedAt: created, UpdatedAt: created}
	}
	// The plan is registered now (the store's clock): #46 is after it.
	must(t, e.st.UpsertPRs(context.Background(),
		pr(45, "docs/readme-typo", "Fix a typo in the README", time.Now().Add(-90*24*time.Hour)),
		pr(46, "try/tokenizer", "Try a new tokenizer", time.Now().Add(time.Hour))))
	e.registerSearch()
	return e
}

// Unmatched lists the new PRs first, and the older ones in a collapsed
// "Older (N)". Each Link picker starts on a disabled "Pick a package…".
// The list's note counts the new ones only.
func TestAppPlanUnmatchedNewAndOlder(t *testing.T) {
	e := searchEnv(t)
	page := get(t, e.h, "/app/plans/search-v2")
	checkNoInline(t, "plan page", page)
	pane := inner(t, page, "detail")
	older := strings.Index(pane, `<details id="plan-unmatched-older" class="um-group">`)
	at46, at45 := strings.Index(pane, ">#46<"), strings.Index(pane, ">#45<")
	if older < 0 || at46 < 0 || at45 < 0 || at46 > older || at45 < older {
		t.Fatalf("want #46 above Older and #45 in it (older %d, #46 %d, #45 %d):\n%s", older, at46, at45, pane)
	}
	wantIn(t, "older", pane, `<summary class="label">Older (1)</summary>`, "opened ", "Try a new tokenizer", "Fix a typo in the README")
	wantNotIn(t, "older", pane, `<details id="plan-unmatched-older" class="um-group" open`, "plan-unmatched-hidden")
	wantIn(t, "picker", pane, `<label class="pd-field">Link to <select name="pkg" required>
<option value="" disabled selected>Pick a package…</option>
<option value="A1">A1 Index builder</option>`,
		`<form method="post" action="/app/plans/search-v2/hide" data-pane="list detail" class="um-form">`,
		`<input type="hidden" name="hidden" value="1">`, `>Not part of this plan</button>`)
	if n := strings.Count(pane, ">Not part of this plan</button>"); n != 2 {
		t.Errorf("%d Not part of this plan buttons, want 2", n)
	}
	wantIn(t, "note", inner(t, page, "list"), `<a href="/app/plans/search-v2?view=graph#plan-unmatched" data-pane="list detail">1 unmatched</a>`)

	// The package pane's picker: new ones, then older, none preselected.
	d := get(t, e.h, "/app/plans/search-v2?pkg=A1&fragment=detail")
	wantIn(t, "pane picker", d, `<option value="" disabled selected>Pick a PR, branch or session…</option>
<optgroup label="Unmatched">
<option value="pr acme/app#46">PR #46 · Try a new tokenizer (acme/app)</option>`,
		`<optgroup label="Unmatched, older than the plan">
<option value="pr acme/app#45">`)
}

// Not part of this plan hides a PR from this plan's Unmatched lane, with
// an Undo; Hidden (N) lists it with Restore. It links nothing.
func TestAppPlanUnmatchedHide(t *testing.T) {
	e := searchEnv(t)
	before := e.lifecycle("search-v2", "A1")

	rec, a := e.postJSON("/app/plans/search-v2/hide", url.Values{"kind": {"pr"}, "ref": {"acme/app#45"}, "hidden": {"1"}, "return": {"/app/plans/search-v2?view=kanban"}})
	if rec.Code != http.StatusOK || a.Location != "/app/plans/search-v2?view=kanban" || a.Toast == nil ||
		a.Toast.Text != "Hid PR acme/app#45 from this plan's Unmatched list: not part of this plan. Nothing was linked or unlinked." ||
		a.Toast.Undo == nil || a.Toast.Undo.Action != "/app/plans/search-v2/hide" || a.Toast.Undo.Fields["hidden"] != "0" {
		t.Fatalf("hide = %d %+v (toast %+v)", rec.Code, a, a.Toast)
	}
	pane := get(t, e.h, "/app/plans/search-v2?fragment=detail")
	hidden := pane[strings.Index(pane, `<details id="plan-unmatched-hidden" class="um-group">`):]
	wantIn(t, "hidden", hidden, `<summary class="label">Hidden (1)</summary>`, ">#45<", "hidden ",
		`<input type="hidden" name="hidden" value="0">`, `>Restore</button>`)
	wantNotIn(t, "older gone", pane, "plan-unmatched-older")
	wantNotIn(t, "pane picker", get(t, e.h, "/app/plans/search-v2?pkg=A1&fragment=detail"), `value="pr acme/app#45"`)
	if lc := e.lifecycle("search-v2", "A1"); lc != before {
		t.Errorf("A1 went from %s to %s: hiding changed a package", before, lc)
	}
	for _, p := range e.board("search-v2").Packages {
		if len(p.Links) > 0 {
			t.Errorf("%s has links after hiding: %+v", p.ID, p.Links)
		}
	}

	// Its Undo restores it, with an Undo that hides it again.
	vals := url.Values{}
	for k, v := range a.Toast.Undo.Fields {
		vals.Set(k, v)
	}
	_, a = e.postJSON(a.Toast.Undo.Action, vals)
	if a.Toast == nil || a.Toast.Text != "Restored PR acme/app#45 to this plan's Unmatched list." || a.Toast.Undo == nil || a.Toast.Undo.Fields["hidden"] != "1" {
		t.Errorf("undo toast %+v", a.Toast)
	}
	pane = get(t, e.h, "/app/plans/search-v2?fragment=detail")
	wantIn(t, "restored", pane, `<summary class="label">Older (1)</summary>`)
	wantNotIn(t, "restored", pane, "plan-unmatched-hidden")

	// From a plain form: hide, then Restore from the Hidden list.
	page := e.follow(e.post("/app/plans/search-v2/hide", url.Values{"kind": {"pr"}, "ref": {"acme/app#46"}, "hidden": {"1"}, "return": {"/app/plans/search-v2"}}),
		"/app/plans/search-v2?toast=")
	wantIn(t, "toast", section(t, page, "toasts"), "<span>Hid PR acme/app#46 from this plan&#39;s Unmatched list",
		`<form method="post" action="/app/plans/search-v2/hide" data-pane>`)
	wantIn(t, "nothing new", inner(t, page, "detail"), "Nothing new: every PR and branch since the plan was registered is on a board.")
	wantNotIn(t, "note", inner(t, page, "list"), "#plan-unmatched")
	page = e.follow(e.post("/app/plans/search-v2/hide", url.Values{"kind": {"pr"}, "ref": {"acme/app#46"}, "hidden": {"0"}, "return": {"/app/plans/search-v2"}}),
		"/app/plans/search-v2?toast=")
	wantIn(t, "restored", inner(t, page, "detail"), ">#46<")
	wantNotIn(t, "restored", inner(t, page, "detail"), "plan-unmatched-hidden")

	// Refused: a kind Unmatched never lists, and an unknown plan.
	rec, a = e.postJSON("/app/plans/search-v2/hide", url.Values{"kind": {"agent"}, "ref": {"s1/a1"}, "hidden": {"1"}, "return": {"/app/plans/search-v2"}})
	if rec.Code != http.StatusBadRequest || !strings.HasPrefix(a.Error, "Not hidden: only a pr or a branch") {
		t.Errorf("agent = %d %+v", rec.Code, a)
	}
	rec, a = e.postJSON("/app/plans/nope/hide", url.Values{"kind": {"pr"}, "ref": {"acme/app#45"}, "hidden": {"0"}, "return": {"/app/plans/nope"}})
	if rec.Code != http.StatusNotFound || !strings.HasPrefix(a.Error, "Not restored: no plan") || a.Location != "/app/" {
		t.Errorf("unknown plan = %d %+v", rec.Code, a)
	}
}

// Hiding goes through the app's CSRF check like every plan-page form.
func TestAppPlanUnmatchedHideNeedsCSRF(t *testing.T) {
	e := searchEnv(t)
	vals := url.Values{"kind": {"pr"}, "ref": {"acme/app#45"}, "hidden": {"1"}, CSRFField: {"forged"}}
	rec := e.post("/app/plans/search-v2/hide", vals)
	if rec.Code != http.StatusForbidden {
		t.Errorf("forged token = %d, want 403: %s", rec.Code, rec.Body)
	}
	if b := e.board("search-v2"); len(b.HiddenUnmatched) != 0 {
		t.Errorf("hidden after a refused form: %+v", b.HiddenUnmatched)
	}
}

// A Link form sent without a pick (the pickers start on "Pick …") is
// refused, and says what to pick.
func TestAppPlanLinkNeedsAPick(t *testing.T) {
	e := searchEnv(t)
	rec, a := e.postJSON("/app/plans/search-v2/link", url.Values{"kind": {"pr"}, "ref": {"acme/app#45"}, "return": {"/app/plans/search-v2"}})
	if rec.Code != http.StatusBadRequest || a.Error != "Not linked: pick the package to link it to." {
		t.Errorf("no package = %d %+v", rec.Code, a)
	}
	page := e.follow(e.post("/app/plans/search-v2/link", url.Values{"pkg": {""}, "kind": {"pr"}, "ref": {"acme/app#45"}, "return": {"/app/plans/search-v2"}}),
		"/app/plans/search-v2?")
	wantIn(t, "plain form", section(t, page, "toasts"), "toast error", "Not linked: pick the package to link it to.")
	rec, a = e.postJSON("/app/plans/search-v2/link", url.Values{"pkg": {"A1"}, "target": {""}, "return": {"/app/plans/search-v2?pkg=A1"}})
	if rec.Code != http.StatusBadRequest || a.Error != "Not linked: pick a PR, branch or session to link, or type its ref." {
		t.Errorf("no target = %d %+v", rec.Code, a)
	}
	if lc := e.lifecycle("search-v2", "A1"); lc != "ready" {
		t.Errorf("A1 = %s after refused links", lc)
	}
}
