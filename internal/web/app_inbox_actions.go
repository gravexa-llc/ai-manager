package web

// The Needs you inbox's actions and API:
//
//   - POST /app/inbox/go, /snooze, /done, /open and /undo: the detail
//     pane's forms (fields item, csrf_token, return; snooze for /snooze;
//     move for /undo), answered with the toast protocol (APP.md). Every
//     move's toast has an Undo, which posts /app/inbox/undo with the move.
//     Go switches to the session's terminal tab first (Go's rules in
//     golink.go, s.goTo), or, for a PR, opens it on GitHub: the page's
//     script opens the tab, and without the script the form posts into a
//     new tab that the answer sends to the PR. For your step, Go is Open
//     in plan: the answer goes to the package on its plan's page. Its
//     Mark done is the plan page's own form (POST /app/plans/{slug}/done),
//     which comes back here.
//   - GET /api/inbox?tab=, GET /api/inbox/{id} and POST /api/inbox/{id}:
//     the same for programs (docs/api.md), with the token on writes. With
//     the token, a waiting session item also carries what the mod
//     reported it waits on (waiting_on, as GET /api/sessions gives it).

import (
	"cmp"
	"errors"
	"net/http"
	"strconv"

	"github.com/gravexa-llc/ai-manager/internal/inbox"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// inboxActions are the form routes' actions.
var inboxActions = []string{inbox.ActGo, inbox.ActSnooze, inbox.ActDone, inbox.ActOpen, inbox.ActUndo}

// inboxRoutes adds the inbox's form routes and its API.
func inboxRoutes(s *Server) {
	for _, a := range inboxActions {
		s.mux.HandleFunc("POST /app/inbox/"+a, s.inboxForm(a))
	}
	s.mux.HandleFunc("GET /api/inbox", s.getInbox)
	s.mux.HandleFunc("GET /api/inbox/{id}", s.getInboxItem)
	s.mux.HandleFunc("POST /api/inbox/{id}", s.postInboxItem)
}

// inboxFail is an action refused or failed: the status and what to say.
type inboxFail struct {
	status int
	msg    string
}

// inboxDo makes a move on item id. Go first switches to the item's session
// (Go's rules) or checks its PR's link; the other moves are internal/inbox
// alone. It returns the item as it is now, the move it added and what to
// say about it.
func (s *Server) inboxDo(r *http.Request, id int64, a inbox.Action) (store.InboxItem, store.InboxMove, string, *inboxFail) {
	ctx := r.Context()
	st, ok := s.inboxStore()
	if !ok {
		return store.InboxItem{}, store.InboxMove{}, "", &inboxFail{http.StatusNotFound, "This aimd has no inbox."}
	}
	it, err := st.InboxItem(ctx, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return it, store.InboxMove{}, "", &inboxFail{http.StatusNotFound, "That item is gone: Done items are kept for " + keptText(s.inboxSettings(ctx)) + "."}
	case err != nil:
		s.log.Error("inbox item", "id", id, "err", err)
		return it, store.InboxMove{}, "", &inboxFail{http.StatusInternalServerError, "aimd couldn't read the inbox; see aimd.log."}
	}
	msg := ""
	if a.Do == inbox.ActGo {
		var fail *inboxFail
		if a.What, msg, fail = s.inboxGoTo(r, it); fail != nil {
			return it, store.InboxMove{}, "", fail
		}
	}
	it, mv, err := inbox.Act(ctx, st, id, a, s.now(), s.inboxSettings(ctx))
	if err != nil {
		if ref, ok := inbox.IsRefusal(err); ok {
			return it, mv, "", &inboxFail{http.StatusConflict, ref.Reason}
		}
		if errors.Is(err, store.ErrNotFound) {
			return it, mv, "", &inboxFail{http.StatusNotFound, "That item is gone: Done items are kept for " + keptText(s.inboxSettings(ctx)) + "."}
		}
		s.log.Error("inbox action", "id", id, "action", a.Do, "err", err)
		return it, mv, "", &inboxFail{http.StatusInternalServerError, "aimd couldn't save that; see aimd.log."}
	}
	s.opts.Hub.Publish(api.TopicInbox)
	if msg == "" {
		msg = mv.Text
		if a.Do == inbox.ActDone {
			msg = "Marked done"
		}
	}
	return it, mv, msg, nil
}

// inboxGoTo is Go's first half: for a session, switch to its terminal tab
// (only where Go's rules enable Go); for a PR, check it has a GitHub link.
// It returns what Go did, for the history and the toast.
func (s *Server) inboxGoTo(r *http.Request, it store.InboxItem) (what, msg string, fail *inboxFail) {
	if !it.Waiting() {
		return "", "", &inboxFail{http.StatusConflict, "It stopped waiting (" + it.EndReason + ")."}
	}
	if it.State != inbox.Open && it.State != inbox.InProgress {
		return "", "", &inboxFail{http.StatusConflict, "Go moves an Open item to In progress; this one is " + inbox.StateLabel(it.State) + "."}
	}
	if it.Source == inbox.SourcePlan && it.Kind == inbox.KindYourStep {
		// Open in plan: the form's answer goes to the package on its plan's
		// page (inboxForm).
		return "opened in its plan", "Opened " + cmp.Or(it.Pkg, "it") + " in its plan · moved to In progress", nil
	}
	if it.Source == inbox.SourcePlan {
		return "", "", &inboxFail{http.StatusUnprocessableEntity, "Go is for sessions and PRs. Add the row to the plan file; the item closes by itself."}
	}
	if it.Source == inbox.SourcePR {
		if !githubURL(it.PRURL) {
			return "", "", &inboxFail{http.StatusUnprocessableEntity, "This PR has no GitHub link."}
		}
		return "opened on GitHub", "Opened on GitHub · moved to In progress", nil
	}
	if s.opts.Go == nil {
		return "", "", &inboxFail{http.StatusNotFound, "Go isn't available on this aimd."}
	}
	rows, err := s.opts.Store.ListSessions(r.Context(), store.SessionFilter{})
	if err != nil {
		s.log.Error("inbox go", "err", err)
		return "", "", &inboxFail{http.StatusInternalServerError, "aimd couldn't read its sessions; see aimd.log."}
	}
	row, ok := liveRowOf(&snapshot{sessions: rows}, it)
	if !ok {
		return "", "", &inboxFail{http.StatusConflict, "Its session isn't running any more."}
	}
	if off := goOff(row, appOf(s.terminals(), row)); off != "" {
		return "", "", &inboxFail{http.StatusUnprocessableEntity, off}
	}
	res, err := s.goTo(r, row.Key)
	if err != nil {
		var refusal interface{ HTTPStatus() int }
		if errors.As(err, &refusal) {
			return "", "", &inboxFail{refusal.HTTPStatus(), "Go: " + err.Error()}
		}
		return "", "", &inboxFail{http.StatusInternalServerError, "Go failed; see aimd.log."}
	}
	app := res.App
	if app == "" {
		app = "its terminal"
	}
	return "switched to " + app, "Switched to " + app + " · moved to In progress", nil
}

// inboxForm is one action's form route.
func (s *Server) inboxForm(do string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ret := appReturn(r)
		id, err := strconv.ParseInt(formValue(r, "item"), 10, 64)
		if err != nil || id <= 0 {
			s.appRefuse(w, r, http.StatusBadRequest, ret, "Which item? The form has no item.")
			return
		}
		a := inbox.Action{Do: do, Snooze: formValue(r, "snooze")}
		if do == inbox.ActUndo {
			a.Move, _ = strconv.ParseInt(formValue(r, "move"), 10, 64)
		}
		it, mv, msg, fail := s.inboxDo(r, id, a)
		if fail != nil {
			s.appRefuse(w, r, fail.status, ret, fail.msg)
			return
		}
		// Open in plan, your step's Go, lands on the package in its plan,
		// and its Undo comes back there.
		if do == inbox.ActGo && it.Kind == inbox.KindYourStep {
			ret = itemPkgURL(it)
		}
		t := &appToast{Text: msg}
		if do != inbox.ActUndo && mv.ID > 0 {
			t.Undo = &appUndo{Action: "/app/inbox/undo", Fields: map[string]string{
				"item": strconv.FormatInt(id, 10), "move": strconv.FormatInt(mv.ID, 10), "return": ret,
			}}
		}
		// Without the script, a PR's Go form posts into a new tab: send it
		// to the PR.
		if do == inbox.ActGo && it.Source == inbox.SourcePR && !wantsJSON(r) && githubURL(it.PRURL) {
			http.Redirect(w, r, it.PRURL, http.StatusSeeOther)
			return
		}
		s.appReply(w, r, ret, t)
	}
}

// inboxJSON is an item as the API gives it.
func inboxJSON(it store.InboxItem, moves []store.InboxMove) api.InboxItem {
	out := api.InboxItem{
		ID: it.ID, URL: InboxItemURL(it.ID), Source: it.Source, Ref: it.Ref,
		Kind: it.Kind, KindLabel: inbox.KindLabel(it.Kind), State: it.State, New: it.SeenAt.IsZero(),
		Title: it.Title, Subtitle: it.Subtitle, Detail: it.Detail, Plan: it.PlanSlug, Pkg: it.Pkg,
		SessionKey: it.SessionKey, JobID: it.JobID, Repo: it.Repo, Branch: it.Branch, Folder: it.Folder,
		PR: it.PR, PRURL: it.PRURL, Why: it.Why,
		Since: it.Since, StateAt: it.StateAt, DueAt: it.DueAt, Snooze: it.Snooze,
		EndedAt: it.EndedAt, EndReason: it.EndReason,
	}
	for _, m := range moves {
		out.History = append(out.History, moveJSON(m))
	}
	return out
}

// apiWaits returns, for an API read that may see it (detailAllowed: aimd's
// token, never a page's CSRF token), what each waiting session item waits
// on as the mod reported it. It returns nil for any other read, and
// when aimd has no reports.
func (s *Server) apiWaits(r *http.Request) func(store.InboxItem) *api.WaitingOn {
	if s.opts.Waits == nil || !s.detailAllowed(r) {
		return nil
	}
	rows, err := s.opts.Store.ListSessions(r.Context(), store.SessionFilter{})
	if err != nil {
		s.log.Error("inbox wait detail", "err", err)
		return nil
	}
	sn := &snapshot{sessions: rows}
	return func(it store.InboxItem) *api.WaitingOn {
		if !it.Waiting() {
			return nil
		}
		row, live := liveRowOf(sn, it)
		if w := s.sessionWait(row, live); w != nil && waitFits(it.Kind, w.Kind) {
			return w
		}
		return nil
	}
}

// withWait adds what an item waits on, when waits (apiWaits) may say.
func withWait(j api.InboxItem, it store.InboxItem, waits func(store.InboxItem) *api.WaitingOn) api.InboxItem {
	if waits != nil {
		j.WaitingOn = waits(it)
	}
	return j
}

func moveJSON(m store.InboxMove) api.InboxMove {
	return api.InboxMove{ID: m.ID, At: m.At, By: m.By, Text: m.Text, From: m.From, To: m.To}
}

// getInbox is GET /api/inbox?tab=open|progress|snoozed|done|all (open by
// default), after a sync pass.
func (s *Server) getInbox(w http.ResponseWriter, r *http.Request) {
	st, ok := s.inboxStore()
	if !ok {
		writeError(w, http.StatusNotFound, "this aimd has no inbox")
		return
	}
	ctx := r.Context()
	tab := r.URL.Query().Get("tab")
	if tab == "" {
		tab = inbox.Open
	}
	if tab != "all" && !validTab(tab) {
		writeError(w, http.StatusBadRequest, "tab is open, progress, snoozed, done or all")
		return
	}
	if _, err := s.SyncInbox(ctx); err != nil {
		s.log.Error("inbox sync", "err", err)
	}
	counts, err := st.InboxCounts(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	f := store.InboxFilter{State: tab}
	if tab == "all" {
		f.State = ""
	}
	items, err := st.ListInbox(ctx, f)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := api.Inbox{Tab: tab, Counts: map[string]int{}, Items: []api.InboxItem{}}
	for _, state := range inbox.States {
		out.Counts[state] = counts[state]
	}
	waits := s.apiWaits(r)
	for _, g := range inbox.Groups(items, s.inboxPlans(ctx).order) {
		for _, it := range g.Items {
			out.Items = append(out.Items, withWait(inboxJSON(it, nil), it, waits))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// pathItem reads {id}.
func pathItem(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "the item id is a positive number")
		return 0, false
	}
	return id, true
}

// getInboxItem is GET /api/inbox/{id}: one item with its history.
func (s *Server) getInboxItem(w http.ResponseWriter, r *http.Request) {
	st, ok := s.inboxStore()
	if !ok {
		writeError(w, http.StatusNotFound, "this aimd has no inbox")
		return
	}
	id, ok := pathItem(w, r)
	if !ok {
		return
	}
	it, err := st.InboxItem(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no inbox item "+strconv.FormatInt(id, 10)+" (Done items are kept for "+keptText(s.inboxSettings(r.Context()))+")")
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	moves, err := st.InboxMoves(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, withWait(inboxJSON(it, moves), it, s.apiWaits(r)))
}

// postInboxItem is POST /api/inbox/{id}: one action, as the page's forms
// make it.
func (s *Server) postInboxItem(w http.ResponseWriter, r *http.Request) {
	id, ok := pathItem(w, r)
	if !ok {
		return
	}
	var req api.InboxActRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	switch req.Action {
	case inbox.ActGo, inbox.ActSnooze, inbox.ActDone, inbox.ActOpen, inbox.ActUndo, inbox.ActSeen:
	default:
		writeError(w, http.StatusBadRequest, "action is go, snooze, done, open, undo or seen")
		return
	}
	it, mv, msg, fail := s.inboxDo(r, id, inbox.Action{Do: req.Action, Snooze: req.Snooze, Move: req.Move})
	if fail != nil {
		writeError(w, fail.status, fail.msg)
		return
	}
	st, _ := s.inboxStore()
	moves, err := st.InboxMoves(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	res := api.InboxActResult{Item: withWait(inboxJSON(it, moves), it, s.apiWaits(r)), Message: msg}
	if mv.ID > 0 {
		m := moveJSON(mv)
		res.Move = &m
	}
	if res.Message == "" {
		res.Message = "Opened"
	}
	writeJSON(w, http.StatusOK, res)
}
