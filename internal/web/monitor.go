package web

// Monitored sessions: the sessions the Needs you inbox, the
// desktop notifications and the dashboard's lists follow. internal/monitor
// decides which they are (Options.Monitor); this is the HTTP side. The
// contract is in docs/api.md.
//
// POST /api/sessions/watch and DELETE /api/sessions/watch, body
// {"session_id": "…"}, watch a session, or stop. The mod's /aim watch and
// /aim unwatch call them with X-AIM-Token. The answer is an
// api.WatchResult, whose message the mod shows as is. The app's Sessions
// page (app_sessions.go) lists the monitored sessions by plan, the
// rest under a collapsed "Not monitored" with Watch, and posts its own
// Watch and Stop monitoring forms to setWatch.

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gravexa-llc/ai-manager/internal/monitor"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
)

// monitorSet decides which sessions are monitored (Options.Monitor). With
// no monitor wired, or when its inputs can't be read (logged), every
// session is: nothing that needs you is hidden.
func (s *Server) monitorSet(ctx context.Context, sessions []store.Session, wts []store.Worktree) *monitor.Set {
	set, err := s.opts.Monitor.Load(ctx, sessions, wts)
	if err != nil {
		s.log.Error("monitored sessions; showing every session as monitored", "err", err)
		return nil
	}
	return set
}

// whyJSON is a session's reasons as GET /api/sessions gives them: [] for
// none.
func whyJSON(ws []monitor.Why) []api.MonitoredWhy {
	out := make([]api.MonitoredWhy, 0, len(ws))
	for _, w := range ws {
		out = append(out, api.MonitoredWhy{
			Reason: w.Reason, Plan: w.Plan, Pkg: w.Pkg, Subagent: w.Subagent, Lead: w.Lead, Since: w.Since, Text: w.Text(),
		})
	}
	return out
}

// withMonitored sets a session's monitored fields in GET /api/sessions.
func withMonitored(js api.Session, set *monitor.Set, row store.Session) api.Session {
	js.Monitored = set.Monitored(row)
	js.MonitoredWhy = whyJSON(set.Why(row))
	return js
}

// sessionIDRE is what a Claude Code session id looks like (a UUID today).
var sessionIDRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// postWatch is POST /api/sessions/watch.
func (s *Server) postWatch(w http.ResponseWriter, r *http.Request) { s.watchAPI(w, r, true) }

// deleteWatch is DELETE /api/sessions/watch.
func (s *Server) deleteWatch(w http.ResponseWriter, r *http.Request) { s.watchAPI(w, r, false) }

func (s *Server) watchAPI(w http.ResponseWriter, r *http.Request, on bool) {
	var req api.WatchRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	id := strings.TrimSpace(req.SessionID)
	if !sessionIDRE.MatchString(id) {
		writeError(w, http.StatusBadRequest, "session_id must be a Claude Code session id, as the mod's $.session.id() gives it")
		return
	}
	res, err := s.setWatch(r.Context(), id, on, "this session")
	if err != nil {
		s.log.Error("watch", "session", id, "on", on, "err", err)
		writeError(w, http.StatusInternalServerError, "aimd couldn't save the watch; see aimd.log")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// setWatch watches the session with Claude Code session id id (on), or
// stops watching it, and says what it is now. subject names the session
// in the message.
func (s *Server) setWatch(ctx context.Context, id string, on bool, subject string) (api.WatchResult, error) {
	rows, err := s.opts.Store.ListSessions(ctx, store.SessionFilter{})
	if err != nil {
		return api.WatchResult{}, err
	}
	row, known := liveSessionByID(rows, id)
	res := api.WatchResult{SessionID: id, Known: known}
	if on {
		m, created, err := s.opts.Store.WatchSession(ctx, id, s.now())
		if err != nil {
			return api.WatchResult{}, err
		}
		res.Watched, res.Changed, res.WatchedSince = true, created, m.Since
	} else {
		ids := []string{id}
		if known {
			// The row's ids: its watch may sit under either until the
			// store has seen its latest /clear.
			ids = append(ids, row.SessionID, row.PrevSessionID)
		}
		n, err := s.opts.Store.UnwatchSession(ctx, ids...)
		if err != nil {
			return api.WatchResult{}, err
		}
		res.Changed = n > 0
	}
	if res.Changed {
		s.log.Info("watch", "session", id, "on", on)
		s.opts.Hub.Publish(api.TopicSessions)
	}

	// What it is now. A session aimd hasn't listed yet is decided as a row
	// of its own, so its watch and plan rows still count.
	if known {
		res.Key, res.Title = row.Key, sessionTitle(row)
	} else {
		row = store.Session{Key: store.ModSessionKeyPrefix + id, SessionID: id}
		rows = append(rows, row)
	}
	set := s.monitorSet(ctx, rows, nil)
	why := set.Why(row)
	res.Monitored, res.MonitoredWhy, res.Monitor = set.Monitored(row), whyJSON(why), set.Mode()
	res.Message = watchMessage(res, why, on, subject)
	return res, nil
}

// watchMessage says what a watch or unwatch did, in one line.
func watchMessage(res api.WatchResult, why []monitor.Why, on bool, subject string) string {
	var others []monitor.Why
	for _, w := range why {
		if w.Reason != monitor.ReasonWatch {
			others = append(others, w)
		}
	}
	still := monitor.Summary(others)
	first, size := utf8.DecodeRuneInString(subject)
	Subject := string(unicode.ToUpper(first)) + subject[size:]
	var msg string
	switch {
	case on && res.Changed:
		msg = "Watching " + subject + ": it shows under Needs you and in notifications when it waits on you."
	case on:
		msg = Subject + " was already watched."
	case res.Changed && still != "":
		msg = "Stopped watching " + subject + "; it stays monitored: " + still + "."
	case res.Changed:
		msg = "Stopped watching " + subject + ": it is no longer monitored, and the dashboard lists it under Not monitored."
	case still != "":
		msg = Subject + " wasn't watched; it is monitored anyway: " + still + "."
	default:
		msg = Subject + " wasn't watched."
	}
	if on && !res.Known {
		msg += " aimd hasn't listed it yet; it will at its next session poll."
	}
	if on && res.Monitor == monitor.ModeAll {
		msg += ` (sessions.monitor is "all", so aimd monitors every session anyway.)`
	}
	return msg
}

// noSessionID is why a session can't be watched from the dashboard.
const noSessionID = "aimd doesn't know this session's Claude Code id yet, so it can't watch it. Try again after the next poll, or run /aim watch in it."

// ---- The Sessions page's buttons ----

// watchButton is a session row's Watch or Stop monitoring button: posting
// Key to /app/sessions/watch, or to /app/sessions/unwatch when Unwatch is
// set, or disabled with the reason Off. The zero value is no button.
type watchButton struct {
	Key     string
	Unwatch bool
	Off     string
}

// noPlanLabel heads the monitored sessions under no plan.
const noPlanLabel = "No plan · watched"

// watchButtonOf is a live session row's Watch or Unwatch button: Watch for
// one that isn't monitored, Unwatch for one you watch, none otherwise and
// in "all" mode.
func watchButtonOf(set *monitor.Set, row store.Session) watchButton {
	switch {
	case set.All():
		return watchButton{}
	case monitor.Has(set.Why(row), monitor.ReasonWatch):
		return watchButton{Key: row.Key, Unwatch: true}
	case set.Monitored(row):
		return watchButton{}
	case row.SessionID == "":
		return watchButton{Off: noSessionID}
	}
	return watchButton{Key: row.Key}
}
