package web

// What every page of the app reads (app.go): one snapshot of the store and
// the daemon per request, the safe-to-remove verdicts, and the formatting
// helpers the pages' view models share. The pages themselves are the
// app_<page>.go files.
//
// Everything shown comes from untrusted text (PR titles, branch names,
// session names, waitingFor). Templates never mark anything safe:
// html/template escapes it all, and filters URLs that aren't http(s).

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/monitor"
	"github.com/gravexa-llc/ai-manager/internal/sessions"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/term"
	"github.com/gravexa-llc/ai-manager/internal/timezone"
	"github.com/gravexa-llc/ai-manager/internal/web/api"
	"github.com/gravexa-llc/ai-manager/internal/worktrees"
)

// RepoNamer maps each repo's main checkout path (as stored in
// worktrees.main_repo and sessions.repo) to the GitHub nameWithOwner its
// PRs are stored under. The daemon implements it (RepoNames).
type RepoNamer interface {
	RepoNames() map[string]string
}

// SafeToRemove gives the safe-to-remove verdicts. The rules are `aim
// prune`'s (internal/prune), and they need git in each worktree at
// decision time (ignored files at risk, rule 4′, detached reachability), so
// the dashboard decides nothing itself: it shows what this returns.
// PruneVerdicts (prune.go) is the one aimd wires. It is called on every
// render of the Worktrees page, the Overview's cleanup count and the
// menu's badge, so it must answer from a cache.
type SafeToRemove interface {
	// Verdicts returns the verdict of each worktree it knows, keyed by
	// worktree path; worktrees it leaves out show no verdict. Before its
	// first answer it returns ErrVerdictsPending.
	Verdicts(ctx context.Context) (map[string]Verdict, error)
}

// Verdict is one worktree's safe-to-remove verdict.
type Verdict struct {
	// Category is the verdict's name, shown as is: in internal/prune's
	// terms main, safe, safe-closed, scratch, locked, prunable or not-safe.
	Category string
	// Removable is true when `aim prune --apply` would remove it.
	Removable bool
	// Reasons say why, shown joined with "; ".
	Reasons []string
}

// Thresholds.
const (
	// staleDraftAfter flags a draft PR with no update for this long.
	staleDraftAfter = 30 * 24 * time.Hour
)

// snapshot is what one render reads from the store and the daemon.
type snapshot struct {
	// now is in zone, the Time zone setting's (Server.timeZone), so the
	// formatting helpers (clock, stamp, dueText, the timeline's ticks)
	// write every clock time and day in it.
	now       time.Time
	zone      timezone.Zone
	sessions  []store.Session  // live rows, headless included
	worktrees []store.Worktree // live rows
	prs       []store.PR       // every state
	status    api.Status
	terms     map[string]term.App // session key → terminal app (TerminalSource)
	// mon says which sessions and PRs are monitored (monitor.go); boards
	// are the plans' boards, for the session lists' plan groups.
	mon    *monitor.Set
	boards []api.Plan
	// expired are the background jobs whose conversation Claude Code
	// deleted, by session key (Options.Jobs; app_sessions.go). They never
	// need you.
	expired map[string]sessions.JobExpiry
}

// load reads a snapshot.
func (s *Server) load(ctx context.Context) (*snapshot, error) {
	sess, err := s.opts.Store.ListSessions(ctx, store.SessionFilter{})
	if err != nil {
		return nil, err
	}
	wts, err := s.opts.Store.ListWorktrees(ctx, store.WorktreeFilter{})
	if err != nil {
		return nil, err
	}
	prs, err := s.opts.Store.ListPRs(ctx, store.PRFilter{})
	if err != nil {
		return nil, err
	}
	mon := s.monitorSet(ctx, sess, wts)
	expired := s.expiredJobs(sess)
	zone := s.timeZone(s.settings(ctx))
	snap := &snapshot{
		now:       s.now().In(zone.Loc),
		zone:      zone,
		sessions:  sess,
		worktrees: wts,
		prs:       prs,
		status:    s.buildStatus(sess, wts, prs, mon, expired),
		terms:     s.terminals(),
		mon:       mon,
		expired:   expired,
	}
	if s.opts.Plans != nil && !mon.All() {
		if list, err := s.opts.Plans.Plans(ctx); err == nil {
			snap.boards = list.Plans
		}
	}
	return snap, nil
}

// board returns a plan's board by slug (any case), or nil.
func (sn *snapshot) board(slug string) *api.Plan {
	for i := range sn.boards {
		if strings.EqualFold(sn.boards[i].Slug, slug) {
			return &sn.boards[i]
		}
	}
	return nil
}

// verdictState is what SafeToRemove answered for one render.
type verdictState struct {
	// Wired is false when no SafeToRemove is set: pages show facts only.
	Wired bool
	// Pending is true before its first answer (ErrVerdictsPending).
	Pending bool
	// Error says why it failed.
	Error string
	// byPath holds the verdicts, keyed by worktrees.PathKey.
	byPath map[string]Verdict
}

// get returns the verdict for a worktree path.
func (v verdictState) get(path string) (Verdict, bool) {
	vd, ok := v.byPath[worktrees.PathKey(path)]
	return vd, ok
}

// verdicts calls SafeToRemove.
func (s *Server) verdicts(ctx context.Context) verdictState {
	if s.opts.SafeToRemove == nil {
		return verdictState{}
	}
	raw, err := s.opts.SafeToRemove.Verdicts(ctx)
	switch {
	case errors.Is(err, ErrVerdictsPending):
		return verdictState{Wired: true, Pending: true}
	case err != nil:
		s.log.Error("safe-to-remove verdicts", "err", err)
		return verdictState{Wired: true, Error: "the safe-to-remove verdict failed: " + err.Error()}
	}
	v := verdictState{Wired: true, byPath: make(map[string]Verdict, len(raw))}
	for path, verdict := range raw {
		v.byPath[worktrees.PathKey(path)] = verdict
	}
	return v
}

// failPage logs an error and answers 500 without details.
func (s *Server) failPage(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("dashboard", "path", r.URL.Path, "err", err)
	http.Error(w, "aimd couldn't render this page; see aimd.log", http.StatusInternalServerError)
}

// ---- What needs you, without the inbox ----

// needsYouTotal counts what needs you when there is no inbox to count
// (inboxCount's fallback): the monitored sessions waiting on you (headless
// runs, dismissed alerts and expired background jobs aside), and the PRs
// the PR rule says need you (sn.mon.PRWait).
func needsYouTotal(sn *snapshot) int {
	n := 0
	for _, row := range sn.sessions {
		if row.Headless || row.State != needsYou || !sn.mon.Monitored(row) || sn.jobExpired(row) {
			continue
		}
		if !sessionJSON(row, sn.now).NeedsYouDismissed {
			n++
		}
	}
	for _, p := range sn.prs {
		if sn.mon.PRWait(p).NeedsYou != "" {
			n++
		}
	}
	return n
}

// byWait orders by when a wait began, oldest first; unknown times last.
func byWait(a, b time.Time) int {
	if a.IsZero() != b.IsZero() {
		if a.IsZero() {
			return 1
		}
		return -1
	}
	return a.Compare(b)
}

// needReason is the text shown for a needs-you session's reason.
func needReason(reason string) string {
	switch reason {
	case sessions.ReasonReply:
		return "waiting for your reply"
	case "", sessions.ReasonWaiting:
		return "waiting"
	}
	return reason
}

// ---- Sessions and PRs ----

// sessionTitle names a session: its name, else its cwd's last element,
// else its key.
func sessionTitle(row store.Session) string {
	switch {
	case row.Name != "":
		return row.Name
	case row.Cwd != "":
		return filepath.Base(row.Cwd)
	}
	return row.Key
}

func stateLabel(state string) string {
	if state == "" {
		return "unknown"
	}
	return strings.ReplaceAll(state, "_", " ")
}

// staleDraft reports whether p is an open draft with no update for
// staleDraftAfter.
func staleDraft(now time.Time, p store.PR) bool {
	return p.State == store.PROpen && p.Draft && !p.UpdatedAt.IsZero() && now.Sub(p.UpdatedAt) >= staleDraftAfter
}

func prStateLabel(p store.PR) string {
	if p.State == store.PROpen && p.Draft {
		return "draft"
	}
	return strings.ToLower(p.State)
}

// prIndex finds a branch's same-repo PRs.
type prIndex map[string][]store.PR // lower-case nameWithOwner + "\x00" + branch → PRs

func indexPRs(prs []store.PR) prIndex {
	idx := prIndex{}
	for _, p := range prs {
		if p.IsCrossRepository || p.HeadBranch == "" {
			continue
		}
		k := strings.ToLower(p.Repo) + "\x00" + p.HeadBranch
		idx[k] = append(idx[k], p)
	}
	return idx
}

// best returns the PR to show for a branch: the newest open one, else the
// newest one. nil when the repo or branch is unknown or has no PR.
func (idx prIndex) best(repo, branch string) *store.PR {
	if repo == "" || branch == "" {
		return nil
	}
	var best *store.PR
	for _, p := range idx[strings.ToLower(repo)+"\x00"+branch] {
		switch {
		case best == nil,
			p.State == store.PROpen && best.State != store.PROpen,
			(p.State == store.PROpen) == (best.State == store.PROpen) && p.Number > best.Number:
			best = &p
		}
	}
	return best
}

// ---- Formatting ----
//
// Clock times are written in now's time zone, and days ("today",
// "tomorrow", "Jan 2") are its days: a page passes sn.now, which is in
// the Time zone setting's zone. Machine-readable times (datetime=
// attributes, the API's JSON) stay in UTC.

// ago is how long before now t was, in one unit: 45s, 12m, 5h, 3d. It is
// "" for a zero time.
func ago(now, t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := max(0, now.Sub(t))
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d/time.Second)) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	}
	return strconv.Itoa(int(d/(24*time.Hour))) + "d"
}

// clock formats t in now's time zone: the time of day when it is the same
// day, else the date too.
func clock(now, t time.Time) string {
	t = t.In(now.Location())
	if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return t.Format("15:04")
	}
	return t.Format("Jan 2 15:04")
}

// stamp is a full timestamp for a tooltip, in now's time zone (RFC 3339,
// with its offset), or "unknown".
func stamp(now, t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.In(now.Location()).Format(time.RFC3339)
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// shortPath shows HomeDir as ~.
func (s *Server) shortPath(p string) string {
	h := strings.TrimSuffix(s.opts.HomeDir, "/")
	switch {
	case h == "" || p == "":
		return p
	case p == h:
		return "~"
	case strings.HasPrefix(p, h+"/"):
		return "~" + p[len(h):]
	}
	return p
}

// maxPathLen is the longest path shown whole; longer ones keep their
// first and last parts (the full path is in the tooltip).
const maxPathLen = 56

// displayPath is a path as shown: HomeDir as ~, and the middle of a long
// path (another session's scratchpad, say) elided.
func (s *Server) displayPath(p string) string {
	p = s.shortPath(p)
	if len(p) <= maxPathLen {
		return p
	}
	parts := strings.Split(p, "/")
	if len(parts) <= 6 {
		return p
	}
	return strings.Join(parts[:3], "/") + "/…/" + strings.Join(parts[len(parts)-3:], "/")
}
