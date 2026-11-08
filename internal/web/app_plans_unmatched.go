package web

// A plan's Unmatched lane on its page: the plan's pane lists the PRs and
// branches in its repos that no package of any plan links, split by when
// they appeared (api.Unmatched.New: a PR opened, or a branch first seen in
// a worktree, after the plan was registered). The new ones come first;
// the older ones are in a collapsed "Older (N)". Each has a Link form,
// whose package picker starts on "Pick a package…", and Not part of this
// plan, which hides it from this plan's lane for good (the plan service's
// HideUnmatched, with an Undo). What you hid is in a collapsed
// "Hidden (N)", each with Restore.
//
// # Forms
//
//   - POST /app/plans/{slug}/hide  kind, ref, hidden=1|0: Not part of this
//     plan, or Restore, each with an Undo that does the other. It links
//     and unlinks nothing.

import (
	"cmp"
	"net/http"
	"slices"

	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// planUnmatched is the plan pane's Unmatched lane.
type planUnmatched struct {
	// New appeared after the plan was registered (Registered says when),
	// Older before, newest first; Hidden is what you said isn't part of
	// this plan, most recently hidden first.
	New, Older, Hidden umList
	Registered         string
}

// umList is a part of the lane, with what its rows' forms need.
type umList struct {
	Rows []umRow
	Form *umForm
}

// umForm is what an Unmatched row's forms need: the plan's form path, the
// page's CSRF token and return, and the packages for the Link picker.
type umForm struct {
	Action, CSRF, Return string
	Options              []pkgOption
}

// umRow is one PR or branch: unmatchedView (board.go), with when it
// appeared or was hidden.
type umRow struct {
	unmatchedView
	// When says when it appeared ("opened 3d ago", "first seen 2h ago"),
	// or for a hidden one when it was hidden; WhenTitle the full time.
	When, WhenTitle string
}

// unmatchedNew counts the lane's new entries, for the list's note.
func unmatchedNew(b api.Plan) int {
	n := 0
	for _, u := range b.Unmatched {
		if u.New {
			n++
		}
	}
	return n
}

// planUnmatchedOf is the plan pane's Unmatched lane. d gives the forms'
// CSRF token and return.
func planUnmatchedOf(sn *snapshot, b api.Plan, d *appData, options []pkgOption) planUnmatched {
	f := &umForm{Action: appPlanURL(b.Slug), CSRF: d.CSRF, Return: d.Return, Options: options}
	v := planUnmatched{New: umList{Form: f}, Older: umList{Form: f}, Hidden: umList{Form: f}}
	if !b.CreatedAt.IsZero() {
		v.Registered = stamp(sn.now, b.CreatedAt)
	}
	newest := func(x, y api.Unmatched) int { return y.Since.Compare(x.Since) }
	us := slices.Clone(b.Unmatched)
	slices.SortStableFunc(us, newest)
	for _, u := range us {
		r := umRow{unmatchedView: unmatchedOf(u)}
		if !u.Since.IsZero() {
			what := "opened "
			if u.Kind == store.LinkBranch {
				what = "first seen "
			}
			r.When, r.WhenTitle = what+ago(sn.now, u.Since)+" ago", stamp(sn.now, u.Since)
		}
		if u.New {
			v.New.Rows = append(v.New.Rows, r)
		} else {
			v.Older.Rows = append(v.Older.Rows, r)
		}
	}
	for _, u := range b.HiddenUnmatched {
		r := umRow{unmatchedView: unmatchedOf(u)}
		if !u.HiddenAt.IsZero() {
			r.When, r.WhenTitle = "hidden "+ago(sn.now, u.HiddenAt)+" ago", stamp(sn.now, u.HiddenAt)
		}
		v.Hidden.Rows = append(v.Hidden.Rows, r)
	}
	return v
}

// unmatchedChoices are the Link picker's choices from the Unmatched lane:
// the new ones, then the older ones, newest first in each; hidden ones
// aren't offered.
func unmatchedChoices(b api.Plan) []linkGroup {
	us := slices.Clone(b.Unmatched)
	slices.SortStableFunc(us, func(x, y api.Unmatched) int { return y.Since.Compare(x.Since) })
	var fresh, older []linkChoice
	for _, u := range us {
		v := unmatchedOf(u)
		label := v.KindLabel + " " + v.Label
		if v.Title != "" {
			label += " · " + v.Title
		}
		if v.Repo != "" {
			label += " (" + v.Repo + ")"
		}
		c := linkChoice{Value: u.Kind + " " + u.Ref, Label: label}
		if u.New {
			fresh = append(fresh, c)
		} else {
			older = append(older, c)
		}
	}
	var out []linkGroup
	if len(fresh) > 0 {
		out = append(out, linkGroup{Label: "Unmatched", Choices: fresh})
	}
	if len(older) > 0 {
		out = append(out, linkGroup{Label: "Unmatched, older than the plan", Choices: older})
	}
	return out
}

// appPlanHide is POST /app/plans/{slug}/hide (kind, ref, hidden=1|0): Not
// part of this plan, or Restore. Each answers with an Undo that does the
// other.
func (s *Server) appPlanHide(w http.ResponseWriter, r *http.Request) {
	if !s.appPlansOn(w, r) {
		return
	}
	slug := r.PathValue("slug")
	req := api.HideRequest{Plan: slug, Kind: formValue(r, "kind"), Ref: formValue(r, "ref"), Hidden: formValue(r, "hidden") != "0"}
	res, err := s.opts.Plans.HideUnmatched(r.Context(), req)
	if err != nil {
		prefix := "Not hidden: "
		if !req.Hidden {
			prefix = "Not restored: "
		}
		s.appPlanRefuse(w, r, slug, prefix, err)
		return
	}
	loc := planReturn(r, slug, "")
	what := cmp.Or(linkKindLabels[res.Kind], res.Kind) + " " + res.Ref
	undo := &appUndo{Action: appPlanURL(slug) + "/hide", Fields: map[string]string{"kind": res.Kind, "ref": res.Ref, "hidden": "1", "return": loc}}
	t := &appToast{Undo: undo}
	switch {
	case res.Hidden:
		t.Text = "Hid " + what + " from this plan's Unmatched list: not part of this plan. Nothing was linked or unlinked."
		if !res.Changed {
			t.Text = what + " was hidden from this plan's Unmatched list already."
		}
		undo.Fields["hidden"] = "0"
	default:
		t.Text = "Restored " + what + " to this plan's Unmatched list."
		if !res.Changed {
			t.Text = what + " wasn't hidden from this plan."
		}
	}
	s.appReply(w, r, loc, t)
}
