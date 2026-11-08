package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/config"
	"github.com/gravexa-llc/ai-manager/internal/prune"
	"github.com/gravexa-llc/ai-manager/internal/prune/prunetest"
	"github.com/gravexa-llc/ai-manager/internal/sessions"
	"github.com/gravexa-llc/ai-manager/internal/store"
	"github.com/gravexa-llc/ai-manager/internal/worktrees"
)

// cleanupEnv is the app over an A3 test world (prunetest): the store holds
// the world's worktrees as the daemon's scan would have written them, the
// verdicts are A3's on the world, and Cleanup checks again with the world's
// fake GitHub, session list and process table. Nothing here touches a
// repo outside the world's temp dir.
type cleanupEnv struct {
	w       *prunetest.World
	st      *store.Store
	srv     *Server
	h       http.Handler
	cleanup *Cleanup
	rows    map[string]store.Worktree // case name → stored row
	changes int                       // OnChange calls
	mu      sync.Mutex
}

func newCleanupEnv(t *testing.T) *cleanupEnv {
	t.Helper()
	w := prunetest.BuildWorld(t)
	e := &cleanupEnv{w: w, st: newTestStore(t), rows: map[string]store.Worktree{}}
	scanner := worktrees.New(worktrees.Options{Roots: []string{w.Root}})
	ctx := context.Background()
	// The daemon's scan, a minute ago.
	seen := time.Now().Add(-time.Minute)
	for _, repo := range scanner.Scan(ctx).Repos {
		for _, wt := range repo.Worktrees {
			row := store.Worktree{Path: wt.Path, MainRepo: repo.Path, Branch: wt.Branch, Detached: wt.Detached, HeadSHA: wt.Head,
				Locked: wt.Locked, LockedReason: wt.LockReason, PrunableReason: wt.PrunableReason, LastSeen: seen}
			must(t, e.st.UpsertWorktrees(ctx, row))
			if name := w.Name(wt.Path); name != "" {
				e.rows[name] = row
			}
		}
	}
	m, err := prune.NewMatcher(config.DefaultRebuildable)
	must(t, err)
	checker := &prune.Checker{Scanner: scanner, GitHub: w.GitHub, Sessions: w.Sessions, Procs: w.Procs, Rebuildable: m}
	// The page's verdicts: A3's, worked out now (the cache PruneVerdicts
	// keeps).
	rep, err := checker.Evaluate(ctx, prune.Options{})
	must(t, err)
	verdicts := map[string]Verdict{}
	for _, rr := range rep.Repos {
		for _, it := range rr.Items {
			verdicts[it.Worktree.Path] = Verdict{Category: string(it.Category), Removable: it.Category.Removable(false), Reasons: it.Reasons}
		}
	}
	e.cleanup = NewCleanup(CleanupOptions{
		Checker: checker,
		Log:     e.st,
		OnChange: func() {
			e.mu.Lock()
			e.changes++
			e.mu.Unlock()
		},
	})
	e.srv = newDash(t, e.st, func(o *Options) {
		o.SafeToRemove = fakeVerdicts{v: verdicts}
		o.Cleanup = e.cleanup
	})
	e.h = e.srv.Handler()
	return e
}

// pickOf is the confirmation's value for a world case, as the page shows
// it.
func (e *cleanupEnv) pickOf(t *testing.T, name string) string {
	t.Helper()
	row, ok := e.rows[name]
	if !ok {
		t.Fatalf("no stored row for %s", name)
	}
	return wtID(row.Path) + ":" + row.HeadSHA + ":" + row.Branch
}

// post sends a page form: from the script (JSON) or as a plain form.
func (e *cleanupEnv) post(t *testing.T, target string, vals url.Values, asJSON bool) *httptest.ResponseRecorder {
	t.Helper()
	vals.Set(CSRFField, e.srv.CSRF().Token())
	hdr := map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": "null", "Sec-Fetch-Site": "same-origin"}
	if asJSON {
		hdr = map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Accept": "application/json", CSRFHeader: e.srv.CSRF().Token()}
	}
	return do(t, e.h, "POST", target, strings.NewReader(vals.Encode()), hdr)
}

// remove posts the confirmation for the named cases.
func (e *cleanupEnv) remove(t *testing.T, run string, names ...string) (*httptest.ResponseRecorder, appAnswer) {
	t.Helper()
	vals := url.Values{"run": {run}, "return": {"/app/worktrees?pick=x&confirm=1&q="}}
	for _, n := range names {
		vals.Add("wt", e.pickOf(t, n))
	}
	rec := e.post(t, "/app/worktrees/remove", vals, true)
	var a appAnswer
	decode(t, rec, &a)
	return rec, a
}

// log reads the removal log from the store.
func (e *cleanupEnv) log(t *testing.T) []store.Removal {
	t.Helper()
	l, err := e.st.ListRemovals(context.Background(), time.Time{}, 0)
	must(t, err)
	return l
}

func (e *cleanupEnv) branchExists(t *testing.T, branch string) bool {
	t.Helper()
	return strings.TrimSpace(prunetest.Git(t, e.w.Repo, "branch", "--list", branch)) != ""
}

// TestWorktreesPageWorld renders the page over the A3 world: each verdict
// in its group, checkboxes on the safe and scratch rows only, no main
// checkout.
func TestWorktreesPageWorld(t *testing.T) {
	e := newCleanupEnv(t)
	page := get(t, e.h, "/app/worktrees")
	list := section(t, page, "list")
	rows := list[:strings.Index(list, "<dialog")] // the confirmation lists rows again
	groups := map[string][]string{}
	for _, part := range strings.Split(rows, `<div class="ghead"`)[1:] {
		title := part[strings.Index(part, "<span>")+6 : strings.Index(part, "</span>")]
		for name, p := range e.w.WT {
			if strings.Contains(part, `title="`+p+`"`) {
				groups[title] = append(groups[title], name)
			}
		}
	}
	for title, want := range map[string][]string{
		"Safe to remove":                  {"merged", "squash", "ff", "donejob", "basemissing"},
		"Scratch: detached, safe by rule": {"detached-main", "detached-surge", "scratch-dead"},
		"Locked: never removed":           {"locked-live", "locked-stale", "locked-reused", "locked-other"},
		"Prunable records: folder gone":   {"gone", "nogit"},
	} {
		if !sameSet(groups[title], want) {
			t.Errorf("%s: %v, want %v", title, groups[title], want)
		}
	}
	for _, name := range []string{"main", "plain-main"} {
		if strings.Contains(list, `title="`+e.w.WT[name]+`"`) {
			t.Errorf("the main checkout %s is listed", name)
		}
	}
	wantIn(t, "list", list, "Main checkouts (2) are never candidates.", "1 untracked file", "PR #71 is open",
		"only aim prune --include-closed removes it")
	// Checkboxes: exactly the 8 safe and scratch rows.
	if n := strings.Count(list, `name="pick"`); n != 8 {
		t.Errorf("%d checkboxes, want 8", n)
	}
	for _, name := range []string{"dirty", "locked-live", "gone", "closed"} {
		if strings.Contains(list, `id="pick-`+wtID(e.w.WT[name])+`"`) {
			t.Errorf("%s has a checkbox", name)
		}
	}
	wantIn(t, "summary", section(t, page, "summary"), "safe to remove", "2 records whose folder is gone")
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]int{}
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		m[x]--
	}
	for _, n := range m {
		if n != 0 {
			return false
		}
	}
	return true
}

func TestWorktreesRemoveSafe(t *testing.T) {
	e := newCleanupEnv(t)
	rec, a := e.remove(t, "run-1", "merged", "detached-main")
	if rec.Code != http.StatusOK || a.Toast == nil {
		t.Fatalf("remove = %d %+v", rec.Code, a)
	}
	// No Undo, and the toast says why; the selection is gone from the URL.
	if a.Toast.Undo != nil || a.Toast.Kind != "" ||
		!strings.HasPrefix(a.Toast.Text, "Removed 2 worktrees and deleted 1 branch. There's no Undo: a removal can't be undone.") {
		t.Errorf("toast %+v", a.Toast)
	}
	if a.Location != "/app/worktrees" {
		t.Errorf("location %q", a.Location)
	}
	for _, name := range []string{"merged", "detached-main"} {
		if prunetest.Exists(e.w.WT[name]) {
			t.Errorf("%s: folder still there", name)
		}
	}
	if e.branchExists(t, "merged") {
		t.Error("branch merged not deleted")
	}
	if e.changes != 1 {
		t.Errorf("OnChange called %d times", e.changes)
	}

	// The log, newest first, and the page: both removed rows gone from
	// the list until the next scan drops them from the store.
	log := e.log(t)
	if len(log) != 2 {
		t.Fatalf("log: %+v", log)
	}
	for _, l := range log {
		if l.Action != store.RemovalRemoved || l.Repo != e.w.Repo {
			t.Errorf("log entry %+v", l)
		}
		if (l.Branch == "merged") != l.BranchDeleted {
			t.Errorf("log entry %+v", l)
		}
	}
	detail := get(t, e.h, "/app/worktrees?fragment=detail")
	wantIn(t, "log", detail, "<h2 class=\"wt-log-title\">Removal log</h2>", "Kept 30 days.",
		" removed <code", "wt/merged</code> and deleted <code>merged</code>", "wt/detached-main</code>", "detached at "+e.w.C0[:8])
	list := get(t, e.h, "/app/worktrees?fragment=list")
	for _, name := range []string{"merged", "detached-main"} {
		if strings.Contains(list, `title="`+e.w.WT[name]+`"`) {
			t.Errorf("removed %s still listed", name)
		}
	}
	wantIn(t, "list", list, `title="`+e.w.WT["squash"]+`"`)
}

// TestWorktreesRemoveRechecks: worktrees that turned unsafe between the
// page and the confirmation are skipped, each with its reason in the toast
// and the log.
func TestWorktreesRemoveRechecks(t *testing.T) {
	e := newCleanupEnv(t)
	w := e.w
	prunetest.Write(t, filepath.Join(w.WT["merged"], "late.txt"), "x\n")
	prunetest.Commit(t, w.WT["squash"], "late commit")
	w.Sessions.Add(sessions.Session{PID: 777, SessionID: "dddddddd-0000-4000-8000-000000000004", Kind: sessions.Interactive,
		Cwd: w.WT["ff"], Status: sessions.StatusBusy})
	prunetest.Git(t, w.Repo, "worktree", "lock", w.WT["donejob"])

	rec, a := e.remove(t, "run-1", "merged", "squash", "ff", "donejob", "basemissing")
	if rec.Code != http.StatusOK || a.Toast == nil || a.Toast.Kind != "error" {
		t.Fatalf("remove = %d %+v", rec.Code, a.Toast)
	}
	wantIn(t, "toast", a.Toast.Text, "Removed 1 worktree and deleted 1 branch. There's no Undo", "Skipped merged: no longer safe: 1 untracked file")
	reasons := map[string]string{}
	for _, l := range e.log(t) {
		reasons[filepath.Base(l.Path)] = l.Action + ": " + l.Detail
	}
	for name, want := range map[string]string{
		"merged":      "skipped: no longer safe: 1 untracked file",
		"squash":      "skipped: no longer safe: 1 commit not in PR #2 or origin/main",
		"ff":          "skipped: no longer safe: in use: cwd of interactive session",
		"donejob":     "skipped: locked, never removed",
		"basemissing": "removed: ",
	} {
		if !strings.HasPrefix(reasons[name], want) {
			t.Errorf("%s: logged %q, want %q", name, reasons[name], want)
		}
		if name != "basemissing" && (!prunetest.Exists(w.WT[name]) || !e.branchExists(t, name)) {
			t.Errorf("%s: folder or branch removed", name)
		}
	}
	wantIn(t, "log", get(t, e.h, "/app/worktrees?fragment=detail"), "skipped <code", "no longer safe: 1 untracked file")
}

// TestWorktreesRemoveNeverLockedOrMain: locked worktrees and main
// checkouts aren't removed, whatever the request says.
func TestWorktreesRemoveNeverLockedOrMain(t *testing.T) {
	e := newCleanupEnv(t)
	rec, a := e.remove(t, "run-1", "locked-live", "locked-stale", "main")
	if rec.Code != http.StatusOK || a.Toast == nil {
		t.Fatalf("remove = %d %+v", rec.Code, a)
	}
	wantIn(t, "toast", a.Toast.Text, "Nothing was removed.", "Skipped locked-live: locked, never removed",
		"1 pick not a worktree aimd lists, left alone")
	if rec, a := e.remove(t, "run-2", "main", "plain-main"); rec.Code != http.StatusConflict || !strings.Contains(a.Error, "None of those") {
		t.Errorf("main checkouts only = %d %+v", rec.Code, a)
	}
	for _, name := range []string{"locked-live", "locked-stale", "main", "plain-main"} {
		if !prunetest.Exists(e.w.WT[name]) {
			t.Errorf("%s removed", name)
		}
	}
	for _, l := range e.log(t) {
		if l.Action == store.RemovalRemoved {
			t.Errorf("logged a removal: %+v", l)
		}
	}
	if !e.branchExists(t, "main") {
		t.Error("main deleted")
	}
}

// TestWorktreesRemoveOnlyListed: a request can only name worktrees aimd
// lists, by id; a path, or an id aimd doesn't list, is refused, and a
// stored path git doesn't list as a worktree is left alone.
func TestWorktreesRemoveOnlyListed(t *testing.T) {
	e := newCleanupEnv(t)
	w := e.w
	// A worktree made after aimd's scan: aimd doesn't list it yet.
	late := filepath.Join(w.Root, "wt", "late")
	prunetest.Git(t, w.Repo, "worktree", "add", "-q", "--detach", late, w.C0)
	// A folder in the store that git doesn't list as a worktree.
	notRepo := filepath.Join(w.Root, "not-a-repo")
	must(t, e.st.UpsertWorktrees(context.Background(), store.Worktree{Path: notRepo, MainRepo: w.Repo, LastSeen: time.Now()}))

	for name, c := range map[string]struct {
		wt     string
		status int
		err    string
	}{
		"a path":         {w.WT["merged"] + "::", http.StatusBadRequest, "doesn't name worktrees the way this page does"},
		"a path as id":   {late + ":" + w.C0 + ":", http.StatusBadRequest, "doesn't name worktrees"},
		"bad head":       {wtID(w.WT["merged"]) + ":HEAD:merged", http.StatusBadRequest, "doesn't name worktrees"},
		"unlisted id":    {wtID(late) + ":" + w.C0 + ":", http.StatusConflict, "None of those is a worktree aimd lists"},
		"nothing picked": {"", http.StatusBadRequest, "Nothing was selected"},
	} {
		vals := url.Values{"run": {"run-" + name}}
		if c.wt != "" {
			vals.Set("wt", c.wt)
		}
		rec := e.post(t, "/app/worktrees/remove", vals, true)
		var a appAnswer
		decode(t, rec, &a)
		if rec.Code != c.status || !strings.Contains(a.Error, c.err) {
			t.Errorf("%s: %d %+v, want %d %q", name, rec.Code, a, c.status, c.err)
		}
	}
	// Without its confirmation id.
	for _, target := range []string{"/app/worktrees/remove", "/app/worktrees/prune"} {
		rec := e.post(t, target, url.Values{"wt": {e.pickOf(t, "merged")}, "rec": {wtID(w.WT["gone"])}}, true)
		var a appAnswer
		decode(t, rec, &a)
		if rec.Code != http.StatusBadRequest || !strings.Contains(a.Error, "no confirmation") {
			t.Errorf("%s without run: %d %+v", target, rec.Code, a)
		}
	}
	if !prunetest.Exists(late) || !prunetest.Exists(w.WT["merged"]) || len(e.log(t)) != 0 {
		t.Error("a refused request removed something")
	}

	// The stored folder passes aimd's list, but git doesn't list it.
	vals := url.Values{"run": {"run-notrepo"}, "wt": {wtID(notRepo) + "::"}}
	rec := e.post(t, "/app/worktrees/remove", vals, true)
	var a appAnswer
	decode(t, rec, &a)
	if rec.Code != http.StatusOK || !strings.Contains(a.Toast.Text, "git doesn't list a worktree there now") {
		t.Errorf("not a worktree: %d %+v", rec.Code, a.Toast)
	}
	if !prunetest.Exists(notRepo) {
		t.Error("a folder git doesn't list was removed")
	}
}

func TestWorktreesRemoveNeedsCSRF(t *testing.T) {
	e := newCleanupEnv(t)
	body := url.Values{"run": {"r"}, "wt": {e.pickOf(t, "merged")}}.Encode()
	for name, hdr := range map[string]map[string]string{
		"no token":     {"Content-Type": "application/x-www-form-urlencoded", "Accept": "application/json"},
		"bad token":    {"Content-Type": "application/x-www-form-urlencoded", CSRFHeader: "nope"},
		"cross-origin": {"Content-Type": "application/x-www-form-urlencoded", "Origin": "http://evil.example", CSRFHeader: e.srv.CSRF().Token()},
		"cross-site":   {"Content-Type": "application/x-www-form-urlencoded", "Sec-Fetch-Site": "cross-site", CSRFHeader: e.srv.CSRF().Token()},
	} {
		for _, target := range []string{"/app/worktrees/remove", "/app/worktrees/prune"} {
			if rec := do(t, e.h, "POST", target, strings.NewReader(body), hdr); rec.Code != http.StatusForbidden {
				t.Errorf("%s %s = %d, want 403", name, target, rec.Code)
			}
		}
	}
	if rec := do(t, e.h, "GET", "/app/worktrees/remove", nil, nil); rec.Code == http.StatusOK {
		t.Error("GET /app/worktrees/remove answered 200")
	}
	if !prunetest.Exists(e.w.WT["merged"]) || len(e.log(t)) != 0 {
		t.Error("a refused request removed something")
	}
}

// TestWorktreesRemoveDoubleSubmit: the same confirmation twice does the
// work once; two confirmations of the same worktree at once remove it once.
func TestWorktreesRemoveDoubleSubmit(t *testing.T) {
	e := newCleanupEnv(t)
	if rec, _ := e.remove(t, "run-1", "merged"); rec.Code != http.StatusOK {
		t.Fatalf("first = %d", rec.Code)
	}
	rec, a := e.remove(t, "run-1", "merged")
	if rec.Code != http.StatusConflict || !strings.Contains(a.Error, "sent already") {
		t.Errorf("second = %d %+v", rec.Code, a)
	}
	if n := len(e.log(t)); n != 1 {
		t.Errorf("%d log entries after a double submit, want 1", n)
	}

	squash, ff := e.pickOf(t, "squash"), e.pickOf(t, "ff")
	// The same confirmation sent twice at once: one does the work.
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Go(func() {
			rec := e.post(t, "/app/worktrees/remove", url.Values{"run": {"run-2"}, "wt": {squash}}, true)
			codes[i] = rec.Code
		})
	}
	wg.Wait()
	if !sameSet([]string{http.StatusText(codes[0]), http.StatusText(codes[1])}, []string{"OK", "Conflict"}) {
		t.Errorf("same confirmation at once: %v", codes)
	}

	// Two confirmations of one worktree at once: one repo lock, so one
	// removes it and the other finds it gone.
	for i := range codes {
		wg.Go(func() {
			rec := e.post(t, "/app/worktrees/remove", url.Values{"run": {"run-ff-" + string(rune('a'+i))}, "wt": {ff}}, true)
			codes[i] = rec.Code
		})
	}
	wg.Wait()
	if codes[0] != http.StatusOK || codes[1] != http.StatusOK {
		t.Errorf("two confirmations: %v", codes)
	}
	var removed, skipped int
	for _, l := range e.log(t) {
		if filepath.Base(l.Path) != "ff" {
			continue
		}
		switch {
		case l.Action == store.RemovalRemoved && l.BranchDeleted:
			removed++
		case l.Action == store.RemovalSkipped && l.Detail == "git doesn't list a worktree there now":
			skipped++
		default:
			t.Errorf("ff: %+v", l)
		}
	}
	if removed != 1 || skipped != 1 {
		t.Errorf("ff removed %d times, skipped %d times", removed, skipped)
	}
}

func TestWorktreesPruneRecords(t *testing.T) {
	e := newCleanupEnv(t)
	w := e.w
	admin := func(name string) bool { return prunetest.Exists(filepath.Join(w.Repo, ".git", "worktrees", name)) }
	post := func(run string, names ...string) appAnswer {
		t.Helper()
		vals := url.Values{"run": {run}}
		for _, n := range names {
			vals.Add("rec", wtID(e.rows[n].Path))
		}
		rec := e.post(t, "/app/worktrees/prune", vals, true)
		var a appAnswer
		decode(t, rec, &a)
		if rec.Code != http.StatusOK {
			t.Fatalf("prune %v = %d %+v", names, rec.Code, a)
		}
		return a
	}
	// One of the two listed: prune would clear the other too, so it doesn't run.
	a := post("run-1", "gone")
	if a.Toast.Kind != "error" || !strings.Contains(a.Toast.Text, "Cleared 0 records") || !strings.Contains(a.Toast.Text, "weren't listed") || !admin("gone") {
		t.Errorf("one listed: %+v", a.Toast)
	}
	a = post("run-2", "gone", "nogit")
	if a.Toast.Kind != "" || a.Toast.Text != "Cleared 2 records with git worktree prune." || admin("gone") || admin("nogit") {
		t.Errorf("both listed: %+v", a.Toast)
	}
	if !prunetest.Exists(filepath.Join(w.WT["nogit"], "keep.txt")) {
		t.Error("prune deleted the nogit folder's files")
	}
	actions := map[string]string{}
	for _, l := range e.log(t) {
		actions[filepath.Base(l.Path)] = l.Action
	}
	if actions["gone"] != store.RemovalPruned || actions["nogit"] != store.RemovalPruned || actions["widgets"] != store.RemovalSkipped {
		t.Errorf("log: %v", actions)
	}
	wantIn(t, "log", get(t, e.h, "/app/worktrees?fragment=detail"), "cleared the record of <code", "git worktree prune: ")
	// Again: refused as sent already.
	vals := url.Values{"run": {"run-2"}, "rec": {wtID(e.rows["gone"].Path)}}
	if rec := e.post(t, "/app/worktrees/prune", vals, true); rec.Code != http.StatusConflict {
		t.Errorf("prune sent twice = %d", rec.Code)
	}
}

// TestWorktreesRemovePlainForm: without the script, a 303 back to the page
// with the toast.
func TestWorktreesRemovePlainForm(t *testing.T) {
	e := newCleanupEnv(t)
	vals := url.Values{"run": {"run-1"}, "wt": {e.pickOf(t, "merged")}, "return": {"/app/worktrees?q=wt&pick=x&confirm=1"}}
	rec := e.post(t, "/app/worktrees/remove", vals, false)
	loc := rec.Header().Get("Location")
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(loc, "/app/worktrees?q=wt&toast=") {
		t.Fatalf("plain post = %d to %q", rec.Code, loc)
	}
	page := get(t, e.h, loc)
	wantIn(t, "toast", section(t, page, "toasts"), "Removed 1 worktree and deleted 1 branch. There&#39;s no Undo")
	wantNotIn(t, "toast", section(t, page, "toasts"), "<form")
	if strings.Contains(section(t, page, "list"), " open>") {
		t.Error("the confirmation is still open")
	}
}

func TestRemovalLogKept30Days(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	must(t, st.AddRemovals(ctx,
		store.Removal{At: now.Add(-31 * 24 * time.Hour), Path: "/r/old", Action: store.RemovalRemoved},
		store.Removal{At: now.Add(-29 * 24 * time.Hour), Path: "/r/kept", Action: store.RemovalRemoved},
		store.Removal{At: now.Add(-time.Hour), Path: "/r/new", Action: store.RemovalSkipped, Detail: "no longer safe"},
	))
	c := NewCleanup(CleanupOptions{Checker: &prune.Checker{}, Log: st, Now: func() time.Time { return now }})
	log, err := c.Log(ctx)
	must(t, err)
	if len(log) != 2 || log[0].Path != "/r/new" || log[1].Path != "/r/kept" {
		t.Errorf("log: %+v", log)
	}
	all, err := st.ListRemovals(ctx, time.Time{}, 0)
	must(t, err)
	if len(all) != 2 {
		t.Errorf("%d entries in the store after reading the log, want 2: the old one purged", len(all))
	}
	// A run purges too.
	must(t, st.AddRemovals(ctx, store.Removal{At: now.Add(-40 * 24 * time.Hour), Path: "/r/older", Action: store.RemovalRemoved}))
	c.record(ctx, nil, false)
	if all, _ := st.ListRemovals(ctx, time.Time{}, 0); len(all) != 2 {
		t.Errorf("%d entries after a run, want 2", len(all))
	}
}

// worktreesApp is newApp with a Cleanup whose checker is never reached.
func worktreesApp(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	var c *Cleanup
	srv, st := newApp(t, func(o *Options) {
		c = NewCleanup(CleanupOptions{Checker: &prune.Checker{}, Log: o.Store.(*store.Store)})
		o.Cleanup = c
	})
	return srv, st
}

// TestWorktreesPageSelections checks each selection renders from its URL,
// whole and as fragments, over the dashboard fixture.
func TestWorktreesPageSelections(t *testing.T) {
	srv, st := worktreesApp(t)
	h := srv.Handler()
	old, det := wtID(appRepo+"-old"), wtID(appRepo+"-det")

	page := get(t, h, "/app/worktrees")
	wantIn(t, "page", page,
		`<link rel="stylesheet" href="/static/app-worktrees.css?v=`+assetVersion+`">`,
		`<script src="/static/app-worktrees.js?v=`+assetVersion+`" defer></script>`,
		"<h1>Worktrees &amp; cleanup</h1>", "<p>6 worktrees · 2 safe to remove · 1 record whose folder is gone</p>",
	)
	list := section(t, page, "list")
	wantIn(t, "list", list,
		`<form id="wt-pick" method="get" action="/app/worktrees" data-pane="list detail">`,
		`<a class="btn sm" href="/app/worktrees?all=1" data-pane="list" data-wt-all>Select all safe</a>`,
		`data-open="wt-confirm" data-wt-open disabled>Remove selected (<span data-wt-n>0</span>)</button>`,
		`<noscript><button type="submit" class="btn primary sm" name="confirm" value="1">Remove selected…</button></noscript>`,
		`<input type="checkbox" class="wt-check" id="pick-`+old+`" name="pick" value="`+old+`" data-wt-pick>`,
		"Safe to remove", "Locked: never removed", "locked by an agent", "Prunable records: folder gone",
		`<form class="wt-prune" method="post" action="/app/worktrees/prune" data-pane="list detail">`,
		`<input type="hidden" name="rec" value="`+wtID(appRepo+"-gone")+`">`,
		"Main checkouts (2) are never candidates.",
		`<dialog id="wt-confirm" class="dialog" aria-labelledby="wt-confirm-title">`,
		`<li data-wt-row="`+old+`" hidden><input type="hidden" name="wt" value="`+old+`:c3:old" disabled>`,
		`<form method="post" action="/app/worktrees/remove" data-pane="list detail">`,
		`<input type="hidden" name="return" value="/app/worktrees">`,
	)
	// -det has no verdict: not safe, no checkbox.
	wantNotIn(t, "list", list, `id="pick-`+det+`"`, `wt-confirm" class="dialog" aria-labelledby="wt-confirm-title" open`)
	wantIn(t, "list", list, "detached at d4d4d4d4</code> · no verdict")
	wantIn(t, "detail", section(t, page, "detail"), "Removal log", "Nothing removed from here yet.")

	// Picked in the URL: checked, counted, and in the confirmation.
	list = get(t, h, "/app/worktrees?pick="+old+"&fragment=list")
	wantIn(t, "picked", list, `value="`+old+`" checked data-wt-pick>`, `data-wt-open>Remove selected (<span data-wt-n>1</span>)`,
		`<li data-wt-row="`+old+`"><input type="hidden" name="wt" value="`+old+`:c3:old"><code class="wt-path"`,
		`Clear selection`)
	// A pick that isn't a removable row is ignored.
	wantIn(t, "bad pick", get(t, h, "/app/worktrees?pick="+det+"&fragment=list"), `data-wt-n>0</span>`)
	// All, and the confirmation opened without the script.
	wantIn(t, "all", get(t, h, "/app/worktrees?all=1&fragment=list"), `value="`+old+`" checked`)
	confirm := get(t, h, "/app/worktrees?pick="+old+"&confirm=1&fragment=list")
	wantIn(t, "confirm", confirm, `<dialog id="wt-confirm" class="dialog" aria-labelledby="wt-confirm-title" open>`,
		`<noscript><a class="btn" href="/app/worktrees?pick=`+old+`">Cancel</a></noscript>`)
	// confirm=1 with nothing picked opens nothing.
	wantNotIn(t, "confirm, none", get(t, h, "/app/worktrees?confirm=1&fragment=list"), "aria-labelledby=\"wt-confirm-title\" open")
	// Search.
	q := get(t, h, "/app/worktrees?q=gone&fragment=list")
	wantIn(t, "search", q, `<input type="hidden" name="q" value="gone">`, "Prunable records")
	wantNotIn(t, "search", q, "Safe to remove", "Locked: never removed")
	wantIn(t, "no match", get(t, h, "/app/worktrees?q=zzz&fragment=list"), "No worktree matches “zzz”.")

	// The log, newest first, in the detail fragment.
	must(t, st.AddRemovals(context.Background(),
		store.Removal{At: time.Now().Add(-2 * time.Hour), Repo: appRepo, Path: appRepo + "-a", Branch: "a", Action: store.RemovalRemoved, BranchDeleted: true},
		store.Removal{At: time.Now().Add(-time.Hour), Repo: appRepo, Path: appRepo + "-b", Branch: "b", Action: store.RemovalSkipped, Detail: "no longer safe: 1 untracked file"},
	))
	detail := get(t, h, "/app/worktrees?fragment=detail")
	b, a := strings.Index(detail, "~/Projects/app-b"), strings.Index(detail, "~/Projects/app-a")
	if b < 0 || a < 0 || b > a {
		t.Errorf("log not newest first:\n%s", detail)
	}
	wantIn(t, "detail", detail, `skipped <code title="~/Projects/app-b">`, "no longer safe: 1 untracked file",
		`removed <code title="~/Projects/app-a">~/Projects/app-a</code> and deleted <code>a</code>`)
	wantNotIn(t, "detail", detail, "Nothing removed from here yet.")

	// Every variant renders without inline code (CSP).
	for _, p := range []string{"/app/worktrees", "/app/worktrees?pick=" + old + "&confirm=1", "/app/worktrees?all=1&q=old",
		"/app/worktrees?fragment=detail", "/app/worktrees?fragment=list&pick=" + old} {
		body := get(t, h, p)
		if m := inlineRE.FindString(body); m != "" {
			t.Errorf("%s: inline code %q", p, m)
		}
		for _, tag := range scriptRE.FindAllString(body, -1) {
			if !regexp.MustCompile(`^<script src="/static/[a-z-]+\.js\?v=[^"]*" defer>$`).MatchString(tag) {
				t.Errorf("%s: inline script %s", p, tag)
			}
		}
	}
}

// TestWorktreesPageReadOnly: without a Cleanup the page shows verdicts, no
// checkboxes or forms.
func TestWorktreesPageReadOnly(t *testing.T) {
	srv, _ := newApp(t, nil)
	list := get(t, srv.Handler(), "/app/worktrees?fragment=list")
	wantIn(t, "read-only", list, "Safe to remove", "This aimd can't remove worktrees")
	wantNotIn(t, "read-only", list, `name="pick"`, "wt-confirm", "/app/worktrees/prune")
	rec := do(t, srv.Handler(), "POST", "/app/worktrees/remove", strings.NewReader("wt=x&csrf_token="+url.QueryEscape(srv.CSRF().Token())),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Accept": "application/json"})
	if rec.Code != http.StatusNotImplemented {
		t.Errorf("remove without a Cleanup = %d", rec.Code)
	}
	// Pending verdicts say so.
	pending, _ := newApp(t, func(o *Options) { o.SafeToRemove = fakeVerdicts{err: ErrVerdictsPending} })
	wantIn(t, "pending", get(t, pending.Handler(), "/app/worktrees?fragment=list"), "aimd is working out which worktrees are safe", "not checked yet")
}

func TestParsePick(t *testing.T) {
	id := wtID("/x/y")
	sha := strings.Repeat("ab", 20)
	for v, ok := range map[string]bool{
		id + ":" + sha + ":main":      true,
		id + "::":                     true,
		id + ":" + sha + ":":          true,
		id + ":" + sha + ":a/b-c":     true,
		id + ":" + sha:                false,
		id:                            false,
		"/x/y::":                      false,
		id + ":abc:main":              false,
		strings.ToUpper(id) + "::":    false,
		id + ":" + sha + ":a\nb":      false,
		id + ":" + sha + ":" + id[:3]: true,
	} {
		if _, _, _, got := parsePick(v); got != ok {
			t.Errorf("parsePick(%q) ok = %v, want %v", v, got, ok)
		}
	}
}

func TestRemovalToast(t *testing.T) {
	long := strings.Repeat("x", 400)
	tt := removalToast([]store.Removal{
		{Path: "/r/a", Action: store.RemovalRemoved, BranchDeleted: true},
		{Path: "/r/b", Action: store.RemovalRemoved, Branch: "b", Detail: "worktree removed, branch b kept: moved"},
		{Path: "/r/c", Action: store.RemovalSkipped, Detail: long},
	}, 0)
	if !strings.HasPrefix(tt.Text, "Removed 2 worktrees and deleted 1 branch. There's no Undo: a removal can't be undone. B: worktree removed") ||
		tt.Kind != "error" || tt.Undo != nil {
		t.Errorf("toast %+v", tt)
	}
	ok := removalToast([]store.Removal{{Path: "/r/a", Action: store.RemovalRemoved}}, 0)
	if ok.Text != "Removed 1 worktree. There's no Undo: a removal can't be undone." || ok.Kind != "" {
		t.Errorf("toast %+v", ok)
	}
}

// TestWorktreesScriptParses checks the page script's syntax with node, when
// it is installed, and that it builds no HTML of its own.
func TestWorktreesScriptParses(t *testing.T) {
	js := string(staticFiles["app-worktrees.js"].data)
	wantNotIn(t, "app-worktrees.js", js, "innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "http://", "https://")
	if len(staticFiles["app-worktrees.css"].data) == 0 {
		t.Error("app-worktrees.css is missing")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node isn't installed")
	}
	f := filepath.Join(t.TempDir(), "app-worktrees.js")
	must(t, os.WriteFile(f, staticFiles["app-worktrees.js"].data, 0o644))
	if out, err := exec.Command(node, "--check", f).CombinedOutput(); err != nil {
		t.Errorf("node --check app-worktrees.js: %v\n%s", err, out)
	}
}

// TestCleanupStoredSessions: a session only aimd's store knows of (the mod
// reported it, say) still keeps its worktree, and while the stored
// sessions can't be trusted nothing is removed.
func TestCleanupStoredSessions(t *testing.T) {
	e := newCleanupEnv(t)
	ctx := context.Background()
	must(t, e.st.UpsertSessions(ctx, store.Session{Key: "pid:900", PID: 900, SessionID: "eeeeeeee-0000-4000-8000-000000000005",
		Kind: "interactive", Cwd: e.w.WT["merged"], State: "working", LastSeenAt: t0}))
	m, err := prune.NewMatcher(config.DefaultRebuildable)
	must(t, err)
	checker := &prune.Checker{Scanner: worktrees.New(worktrees.Options{Roots: []string{e.w.Root}}),
		GitHub: e.w.GitHub, Sessions: e.w.Sessions, Procs: e.w.Procs, Rebuildable: m}
	pick := func(name string) CleanupPick {
		row := e.rows[name]
		return CleanupPick{Repo: row.MainRepo, Pick: prune.Pick{Path: row.Path, Head: row.HeadSHA, Branch: row.Branch}}
	}

	c := NewCleanup(CleanupOptions{Checker: checker, Store: e.st, Status: healthy(), Log: e.st})
	out, err := c.Remove(ctx, "run-1", []CleanupPick{pick("merged"), pick("squash")})
	must(t, err)
	if out[0].Action != store.RemovalSkipped || !strings.Contains(out[0].Detail, "in use: cwd of interactive session eeeeeeee") {
		t.Errorf("merged: %+v", out[0])
	}
	if out[1].Action != store.RemovalRemoved {
		t.Errorf("squash: %+v", out[1])
	}

	stale := healthy()
	stale.st.Sources[0].FailingSince, stale.st.Sources[0].Error = t0, "claude agents failed"
	c = NewCleanup(CleanupOptions{Checker: checker, Store: e.st, Status: stale, Log: e.st})
	out, err = c.Remove(ctx, "run-1", []CleanupPick{pick("ff")})
	must(t, err)
	if out[0].Action != store.RemovalSkipped || !strings.Contains(out[0].Detail, "live sessions unknown") || !prunetest.Exists(e.w.WT["ff"]) {
		t.Errorf("ff with the stored sessions stale: %+v", out[0])
	}
}
