package inbox

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/store"
)

// t0 is the tests' "now": a Friday, 10:00 UTC.
var t0 = time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)

var set = Settings{BackToOpen: 10 * time.Minute, KeepDone: 30 * 24 * time.Hour, Morning: 9, Location: time.UTC}

func openStore(t *testing.T, path string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	return openStore(t, filepath.Join(t.TempDir(), store.FileName))
}

// sess is session row id's wait, begun at since.
func sess(id int64, reason string, since time.Time) Episode {
	return Episode{Source: SourceSession, Ref: SessionRef(id), Kind: SessionKind(reason), Since: since,
		Detail: Core(reason), Sig: Core(reason), Title: "session " + SessionRef(id), SessionKey: "pid:" + SessionRef(id)}
}

// pr is a PR's wait.
func pr(ref, kind, head string, since time.Time) Episode {
	return Episode{Source: SourcePR, Ref: ref, Kind: kind, Since: since, Detail: KindLabel(kind),
		Sig: kind + "|" + head, Title: ref, PR: ref, PRURL: "https://github.com/" + strings.Replace(ref, "#", "/pull/", 1)}
}

func sync(t *testing.T, st *store.Store, now time.Time, stopped map[string]string, eps ...Episode) Result {
	t.Helper()
	res, err := Sync(context.Background(), st, Snapshot{Episodes: eps, Stopped: stopped}, now, set)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// all returns every item, oldest first.
func all(t *testing.T, st *store.Store) []store.InboxItem {
	t.Helper()
	items, err := st.ListInbox(context.Background(), store.InboxFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return items
}

// only returns the one item there is.
func only(t *testing.T, st *store.Store) store.InboxItem {
	t.Helper()
	items := all(t, st)
	if len(items) != 1 {
		t.Fatalf("%d items, want 1: %+v", len(items), items)
	}
	return items[0]
}

func item(t *testing.T, st *store.Store, id int64) store.InboxItem {
	t.Helper()
	it, err := st.InboxItem(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return it
}

// history returns an item's history texts.
func history(t *testing.T, st *store.Store, id int64) []string {
	t.Helper()
	moves, err := st.InboxMoves(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range moves {
		out = append(out, m.Text)
	}
	return out
}

func act(t *testing.T, st *store.Store, id int64, a Action, now time.Time) (store.InboxItem, store.InboxMove) {
	t.Helper()
	it, m, err := Act(context.Background(), st, id, a, now, set)
	if err != nil {
		t.Fatalf("%s on %d: %v", a.Do, id, err)
	}
	return it, m
}

func refused(t *testing.T, st *store.Store, id int64, a Action, now time.Time, want string) {
	t.Helper()
	_, _, err := Act(context.Background(), st, id, a, now, set)
	r, ok := IsRefusal(err)
	if !ok || !strings.Contains(r.Reason, want) {
		t.Errorf("%s on %d = %v, want a refusal saying %q", a.Do, id, err, want)
	}
}

func wantState(t *testing.T, it store.InboxItem, state string) {
	t.Helper()
	if it.State != state {
		t.Errorf("item %d is %s, want %s", it.ID, it.State, state)
	}
}

func TestSessionKind(t *testing.T) {
	for reason, want := range map[string]string{
		"permission prompt":                           KindPermission,
		"permission prompt: Bash: Create directory x": KindPermission,
		"agent a04f: permission prompt: Bash: x":      KindPermission,
		"sandbox request":                             KindPermission,
		"input needed":                                KindQuestion,
		"question: Which route?":                      KindQuestion,
		"plan approval":                               KindPlan,
		"goal proposal":                               KindPlan,
		"reply":                                       KindReply,
		"dialog open":                                 KindPrompt,
		"waiting":                                     KindPrompt,
		"notification: x":                             KindPrompt,
	} {
		if got := SessionKind(reason); got != want {
			t.Errorf("SessionKind(%q) = %s, want %s", reason, got, want)
		}
	}
	if Core("agent a04f: question: x") != "question: x" || AgentOf("agent a04f: question: x") != "a04f" {
		t.Error("Core/AgentOf don't split the subagent off")
	}
	if Core("agent of mine") != "agent of mine" || AgentOf("permission prompt") != "" {
		t.Error("Core/AgentOf split a reason that names no subagent")
	}
}

// A wait opens an item; the same wait keeps it, without writing; when it
// stops, the item goes Done with the reason; a later wait is a new item.
func TestSyncEpisodes(t *testing.T) {
	st := newStore(t)
	if res := sync(t, st, t0, nil, sess(1, "permission prompt", t0.Add(-2*time.Minute))); res.Created != 1 {
		t.Fatalf("first pass %+v, want one item", res)
	}
	it := only(t, st)
	wantState(t, it, Open)
	if !it.Since.Equal(t0.Add(-2*time.Minute)) || it.Kind != KindPermission || !it.SeenAt.IsZero() || !it.Waiting() {
		t.Errorf("new item %+v", it)
	}
	if h := history(t, st, it.ID); !slices.Equal(h, []string{"Started waiting: permission prompt"}) {
		t.Errorf("history %q", h)
	}
	// The same wait: nothing to write.
	if res := sync(t, st, t0.Add(time.Minute), nil, sess(1, "permission prompt", t0.Add(-2*time.Minute))); res.Changed() {
		t.Errorf("an unchanged pass wrote %+v", res)
	}
	// The mod tells the same wait better, and from an earlier start: the
	// same item, richer, no "changed".
	sync(t, st, t0.Add(2*time.Minute), nil, sess(1, "permission prompt: Bash: Create directory x", t0.Add(-3*time.Minute)))
	it = only(t, st)
	if it.Detail != "permission prompt: Bash: Create directory x" || !it.Since.Equal(t0.Add(-3*time.Minute)) {
		t.Errorf("refined item %+v", it)
	}
	// And the poll's generic words again don't lose it.
	sync(t, st, t0.Add(3*time.Minute), nil, sess(1, "permission prompt", t0.Add(-3*time.Minute)))
	if it = only(t, st); it.Detail != "permission prompt: Bash: Create directory x" || len(history(t, st, it.ID)) != 1 {
		t.Errorf("generic reason after the rich one: %+v, history %q", it, history(t, st, it.ID))
	}
	// It stops waiting.
	sync(t, st, t0.Add(4*time.Minute), map[string]string{SessionRef(1): "it stopped waiting"})
	it = only(t, st)
	wantState(t, it, Done)
	if it.Waiting() || it.EndReason != "it stopped waiting" {
		t.Errorf("ended item %+v", it)
	}
	if h := history(t, st, it.ID); h[len(h)-1] != "Done: it stopped waiting" {
		t.Errorf("history %q", h)
	}
	// A new wait of the same session is a new item; so is one whose ref is
	// gone and back.
	sync(t, st, t0.Add(5*time.Minute), nil, sess(1, "question: Which route?", t0.Add(5*time.Minute)))
	items := all(t, st)
	if len(items) != 2 || items[1].Kind != KindQuestion || items[1].State != Open || items[0].State != Done {
		t.Fatalf("after a new wait: %+v", items)
	}
	// A later wait while the item still waits (the poll never saw the
	// gap): the old episode ends, a new one begins.
	sync(t, st, t0.Add(6*time.Minute), nil, sess(1, "question: Which route?", t0.Add(6*time.Minute)))
	items = all(t, st)
	if len(items) != 3 || items[1].State != Done || !strings.HasPrefix(items[1].EndReason, "a new wait began") || items[2].State != Open {
		t.Errorf("after a later wait: %+v", items)
	}
	// Another kind of wait from the same PR is a new episode too.
	sync(t, st, t0.Add(7*time.Minute), nil, pr("o/a#1", KindReview, "h1", t0))
	sync(t, st, t0.Add(8*time.Minute), nil, pr("o/a#1", KindMerge, "h1", t0))
	var reviews, merges []store.InboxItem
	for _, it := range all(t, st) {
		switch it.Kind {
		case KindReview:
			reviews = append(reviews, it)
		case KindMerge:
			merges = append(merges, it)
		}
	}
	if len(reviews) != 1 || reviews[0].State != Done || len(merges) != 1 || merges[0].State != Open {
		t.Errorf("review then merge: %+v %+v", reviews, merges)
	}
	// A ref in neither list is gone.
	sync(t, st, t0.Add(9*time.Minute), nil)
	if it := item(t, st, merges[0].ID); it.EndReason != "the PR is gone" {
		t.Errorf("gone PR: %+v", it)
	}
}

// Go: In progress, back to Open after the delay if it still waits.
func TestGoTimer(t *testing.T) {
	st := newStore(t)
	ep := sess(1, "permission prompt", t0)
	sync(t, st, t0, nil, ep)
	id := only(t, st).ID
	it, m := act(t, st, id, Action{Do: ActGo, What: "switched to Terminal"}, t0.Add(time.Minute))
	wantState(t, it, InProgress)
	if !it.DueAt.Equal(t0.Add(11*time.Minute)) || m.Text != "Go: switched to Terminal" || m.From != Open || it.SeenAt.IsZero() {
		t.Errorf("after Go: %+v, move %+v", it, m)
	}
	sync(t, st, t0.Add(10*time.Minute), nil, ep)
	wantState(t, item(t, st, id), InProgress)
	sync(t, st, t0.Add(11*time.Minute), nil, ep)
	it = item(t, st, id)
	wantState(t, it, Open)
	if !it.DueAt.IsZero() {
		t.Errorf("due time kept after the return: %v", it.DueAt)
	}
	if h := history(t, st, id); h[len(h)-1] != "Back to Open: still waiting" {
		t.Errorf("history %q", h)
	}
	// Go again, then it stops waiting before the delay: Done, with why.
	act(t, st, id, Action{Do: ActGo}, t0.Add(12*time.Minute))
	sync(t, st, t0.Add(13*time.Minute), map[string]string{SessionRef(1): "it stopped waiting"})
	it = item(t, st, id)
	wantState(t, it, Done)
	if h := history(t, st, id); h[len(h)-1] != "Done: it stopped waiting" {
		t.Errorf("history %q", h)
	}
}

// A timed snooze ends at its time, or early when the wait changes.
func TestSnoozeTimed(t *testing.T) {
	st := newStore(t)
	ep := pr("o/a#1", KindReview, "h1", t0)
	sync(t, st, t0, nil, ep)
	id := only(t, st).ID
	it, m := act(t, st, id, Action{Do: ActSnooze, Snooze: Snooze15m}, t0)
	wantState(t, it, Snoozed)
	if !it.DueAt.Equal(t0.Add(15*time.Minute)) || it.Snooze != Snooze15m || m.Text != "Snoozed for 15 minutes" {
		t.Errorf("snoozed: %+v, move %+v", it, m)
	}
	sync(t, st, t0.Add(14*time.Minute), nil, ep)
	wantState(t, item(t, st, id), Snoozed)
	sync(t, st, t0.Add(15*time.Minute), nil, ep)
	it = item(t, st, id)
	wantState(t, it, Open)
	if it.Snooze != "" || !it.DueAt.IsZero() {
		t.Errorf("snooze kept after it ended: %+v", it)
	}
	if h := history(t, st, id); h[len(h)-1] != "Back to Open: the snooze ended" {
		t.Errorf("history %q", h)
	}

	// An hour's snooze wakes early when new commits land.
	act(t, st, id, Action{Do: ActSnooze, Snooze: Snooze1h}, t0.Add(20*time.Minute))
	sync(t, st, t0.Add(30*time.Minute), nil, ep)
	wantState(t, item(t, st, id), Snoozed)
	changed := pr("o/a#1", KindReview, "h2", t0)
	changed.Detail = "your review is requested"
	sync(t, st, t0.Add(31*time.Minute), nil, changed)
	wantState(t, item(t, st, id), Open)
	if h := history(t, st, id); h[len(h)-1] != "Back to Open: it changed: your review is requested" {
		t.Errorf("history %q", h)
	}
}

// "Until it changes" has no time: only a change brings it back.
func TestSnoozeUntilChange(t *testing.T) {
	st := newStore(t)
	ep := sess(1, "question: Keep take 2?", t0)
	sync(t, st, t0, nil, ep)
	id := only(t, st).ID
	it, _ := act(t, st, id, Action{Do: ActSnooze, Snooze: SnoozeChange}, t0)
	if !it.DueAt.IsZero() || it.Snooze != SnoozeChange {
		t.Errorf("snoozed until it changes: %+v", it)
	}
	sync(t, st, t0.Add(48*time.Hour), nil, ep)
	wantState(t, item(t, st, id), Snoozed)
	sync(t, st, t0.Add(49*time.Hour), nil, sess(1, "question: Record a third take?", t0))
	wantState(t, item(t, st, id), Open)

	// A change while Open is noted, not a move.
	sync(t, st, t0.Add(50*time.Hour), nil, sess(1, "question: Keep take 3?", t0))
	it = item(t, st, id)
	wantState(t, it, Open)
	if h := history(t, st, id); h[len(h)-1] != "It changed: question: Keep take 3?" || it.Detail != "question: Keep take 3?" {
		t.Errorf("history %q, detail %q", h, it.Detail)
	}

	// Snoozed, then it stops waiting: Done with the reason.
	act(t, st, id, Action{Do: ActSnooze, Snooze: SnoozeChange}, t0.Add(51*time.Hour))
	sync(t, st, t0.Add(52*time.Hour), map[string]string{SessionRef(1): "it stopped waiting"})
	it = item(t, st, id)
	wantState(t, it, Done)
	if it.EndReason != "it stopped waiting" {
		t.Errorf("ended while snoozed: %+v", it)
	}
}

func TestSnoozeUntilTomorrow(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tz database")
	}
	s := set
	s.Location = ny
	for _, c := range []struct{ now, want time.Time }{
		// Friday 10:00 → Saturday 9:00.
		{time.Date(2026, 11, 6, 10, 0, 0, 0, ny), time.Date(2026, 11, 7, 9, 0, 0, 0, ny)},
		// 23:30 → the next morning.
		{time.Date(2026, 11, 6, 23, 30, 0, 0, ny), time.Date(2026, 11, 7, 9, 0, 0, 0, ny)},
		// 02:00 is still the night before: this morning.
		{time.Date(2026, 11, 7, 2, 0, 0, 0, ny), time.Date(2026, 11, 7, 9, 0, 0, 0, ny)},
		// Across the end of daylight saving time (Nov 1 2026): 9am local.
		{time.Date(2026, 10, 31, 12, 0, 0, 0, ny), time.Date(2026, 11, 1, 9, 0, 0, 0, ny)},
	} {
		got, err := s.SnoozeUntil(SnoozeTomorrow, c.now.UTC())
		if err != nil || !got.Equal(c.want) {
			t.Errorf("tomorrow from %v = %v, %v; want %v", c.now, got, err, c.want)
		}
	}
	if c, ok := s.Snooze(SnoozeTomorrow); !ok || c.Label != "Until tomorrow 9am" {
		t.Errorf("tomorrow's label %+v", c)
	}
	if _, err := s.SnoozeUntil("2d", t0); err == nil {
		t.Error("an unknown snooze has a time")
	}
}

// Done, back to Open, Unsnooze, and what each state refuses.
func TestMovesAndRefusals(t *testing.T) {
	st := newStore(t)
	ep := sess(1, "reply", t0)
	sync(t, st, t0, nil, ep)
	id := only(t, st).ID
	refused(t, st, id, Action{Do: ActOpen}, t0, "Open already")
	refused(t, st, id, Action{Do: ActSnooze, Snooze: "2d"}, t0, "no snooze")
	refused(t, st, id, Action{Do: "fly"}, t0, "no action")

	it, m := act(t, st, id, Action{Do: ActDone}, t0)
	wantState(t, it, Done)
	if m.By != store.InboxByYou || m.Text != "Done" || it.Waiting() == false {
		t.Errorf("done: %+v, move %+v", it, m)
	}
	refused(t, st, id, Action{Do: ActDone}, t0, "Done already")
	refused(t, st, id, Action{Do: ActGo}, t0, "Go moves an Open item")
	refused(t, st, id, Action{Do: ActSnooze, Snooze: Snooze1h}, t0, "can't be snoozed")
	// Done by you while it still waits: the pass leaves it Done.
	sync(t, st, t0.Add(time.Minute), nil, ep)
	wantState(t, item(t, st, id), Done)

	it, m = act(t, st, id, Action{Do: ActOpen}, t0.Add(2*time.Minute))
	wantState(t, it, Open)
	if m.Text != "Moved back to Open" {
		t.Errorf("back to Open: %q", m.Text)
	}
	act(t, st, id, Action{Do: ActSnooze, Snooze: SnoozeTomorrow}, t0.Add(3*time.Minute))
	it, m = act(t, st, id, Action{Do: ActOpen}, t0.Add(4*time.Minute))
	if it.State != Open || m.Text != "Unsnoozed" || !it.DueAt.IsZero() {
		t.Errorf("unsnooze: %+v, %q", it, m.Text)
	}
	act(t, st, id, Action{Do: ActGo}, t0.Add(5*time.Minute))
	it, m = act(t, st, id, Action{Do: ActOpen}, t0.Add(6*time.Minute))
	if it.State != Open || m.Text != "Back to Open" {
		t.Errorf("In progress back to Open: %+v, %q", it, m.Text)
	}

	// Its episode ends while Done: noted, still Done.
	act(t, st, id, Action{Do: ActDone}, t0.Add(7*time.Minute))
	sync(t, st, t0.Add(8*time.Minute), map[string]string{SessionRef(1): "it stopped waiting"})
	it = item(t, st, id)
	if h := history(t, st, id); it.State != Done || it.Waiting() || h[len(h)-1] != "Stopped waiting: it stopped waiting" {
		t.Errorf("ended while Done: %+v, %q", it, h)
	}
	// Moved back to Open after its episode ended: it stays Open (your
	// call), and only Done and back to Open are left.
	act(t, st, id, Action{Do: ActOpen}, t0.Add(9*time.Minute))
	sync(t, st, t0.Add(10*time.Minute), nil)
	wantState(t, item(t, st, id), Open)
	refused(t, st, id, Action{Do: ActGo}, t0.Add(10*time.Minute), "stopped waiting")
	refused(t, st, id, Action{Do: ActSnooze, Snooze: Snooze15m}, t0.Add(10*time.Minute), "stopped waiting")
	act(t, st, id, Action{Do: ActDone}, t0.Add(11*time.Minute))

	if _, _, err := Act(context.Background(), st, 999, Action{Do: ActDone}, t0, set); err == nil {
		t.Error("an action on no item worked")
	}
}

// Undo restores the state, due time and snooze the move left, and only
// for your latest move.
func TestUndo(t *testing.T) {
	st := newStore(t)
	ep := sess(1, "permission prompt", t0)
	sync(t, st, t0, nil, ep)
	id := only(t, st).ID

	_, goMove := act(t, st, id, Action{Do: ActGo}, t0)
	_, snooze := act(t, st, id, Action{Do: ActSnooze, Snooze: Snooze1h}, t0.Add(time.Minute))
	// Only the latest move undoes.
	refused(t, st, id, Action{Do: ActUndo, Move: goMove.ID}, t0.Add(2*time.Minute), "nothing to undo")
	it, m := act(t, st, id, Action{Do: ActUndo, Move: snooze.ID}, t0.Add(2*time.Minute))
	wantState(t, it, InProgress)
	if !it.DueAt.Equal(t0.Add(10*time.Minute)) || it.Snooze != "" || m.Text != "Undone: back to In progress" {
		t.Errorf("undone snooze: %+v, %q", it, m.Text)
	}
	// The same move can't be undone twice.
	refused(t, st, id, Action{Do: ActUndo, Move: snooze.ID}, t0.Add(3*time.Minute), "nothing to undo")

	// After aimd moved it (back to Open), your earlier move can't be undone.
	sync(t, st, t0.Add(10*time.Minute), nil, ep)
	wantState(t, item(t, st, id), Open)
	refused(t, st, id, Action{Do: ActUndo, Move: goMove.ID}, t0.Add(11*time.Minute), "nothing to undo")

	// Done, then undo: Open again.
	_, done := act(t, st, id, Action{Do: ActDone}, t0.Add(12*time.Minute))
	it, _ = act(t, st, id, Action{Do: ActUndo, Move: done.ID}, t0.Add(13*time.Minute))
	wantState(t, it, Open)

	// Undo of a snooze after the episode ended would revive a timer on a
	// finished wait: refused.
	_, snooze = act(t, st, id, Action{Do: ActSnooze, Snooze: Snooze15m}, t0.Add(14*time.Minute))
	_, done = act(t, st, id, Action{Do: ActDone}, t0.Add(15*time.Minute))
	sync(t, st, t0.Add(16*time.Minute), map[string]string{SessionRef(1): "it stopped waiting"})
	refused(t, st, id, Action{Do: ActUndo, Move: done.ID}, t0.Add(17*time.Minute), "stopped waiting")
}

// Done items whose episode ended go after KeepDone; a Done item still
// waiting stays, or the next pass would open it again.
func TestPrune(t *testing.T) {
	st := newStore(t)
	old, kept, waiting := sess(1, "reply", t0), sess(2, "reply", t0), sess(3, "reply", t0)
	sync(t, st, t0, nil, old, kept, waiting)
	items := all(t, st)
	act(t, st, items[2].ID, Action{Do: ActDone}, t0)
	// 1 ends at once, 2 ten days later.
	sync(t, st, t0.Add(time.Minute), nil, kept, waiting)
	sync(t, st, t0.Add(10*24*time.Hour), nil, waiting)
	res := sync(t, st, t0.Add(31*24*time.Hour), nil, waiting)
	if res.Pruned != 1 {
		t.Errorf("pruned %d, want 1", res.Pruned)
	}
	left := all(t, st)
	if len(left) != 2 || left[0].Ref != kept.Ref || left[1].Ref != waiting.Ref {
		t.Fatalf("left %+v", left)
	}
	if h, err := st.InboxMoves(context.Background(), items[0].ID); err != nil || len(h) != 0 {
		t.Errorf("the pruned item's history: %v, %v", h, err)
	}
	if res := sync(t, st, t0.Add(41*24*time.Hour), nil, waiting); res.Pruned != 1 || len(all(t, st)) != 1 {
		t.Errorf("second prune %+v", res)
	}
}

// Items, their history and their timers outlive aimd: a restart reopens
// the store, and the first pass applies what came due meanwhile.
func TestRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), store.FileName)
	st := openStore(t, path)
	ep, other := sess(1, "permission prompt", t0), pr("o/a#2", KindMerge, "h", t0)
	sync(t, st, t0, nil, ep, other)
	items := all(t, st)
	act(t, st, items[0].ID, Action{Do: ActGo}, t0)
	act(t, st, items[1].ID, Action{Do: ActSnooze, Snooze: SnoozeChange}, t0)
	st.Close()

	st = openStore(t, path)
	again := all(t, st)
	if len(again) != 2 || again[0].ID != items[0].ID || again[0].State != InProgress || !again[0].DueAt.Equal(t0.Add(10*time.Minute)) ||
		again[1].State != Snoozed || again[1].Snooze != SnoozeChange {
		t.Fatalf("after a restart: %+v", again)
	}
	// aimd was down past Go's delay: the first pass brings it back; the
	// same waits keep their items (no new ids).
	sync(t, st, t0.Add(time.Hour), nil, ep, other)
	after := all(t, st)
	if len(after) != 2 || after[0].ID != items[0].ID || after[0].State != Open || after[1].State != Snoozed {
		t.Errorf("first pass after a restart: %+v", after)
	}
}

// The old dashboard's Dismiss counts as Done once.
func TestDismiss(t *testing.T) {
	st := newStore(t)
	ep := sess(1, "reply", t0)
	sync(t, st, t0, nil, ep)
	id := only(t, st).ID
	ep.DismissedAt = t0.Add(time.Minute)
	sync(t, st, t0.Add(time.Minute), nil, ep)
	it := item(t, st, id)
	wantState(t, it, Done)
	if h := history(t, st, id); h[len(h)-1] != "Done: dismissed on the old dashboard" {
		t.Errorf("history %q", h)
	}
	act(t, st, id, Action{Do: ActOpen}, t0.Add(2*time.Minute))
	sync(t, st, t0.Add(3*time.Minute), nil, ep)
	wantState(t, item(t, st, id), Open)
	ep.DismissedAt = t0.Add(4 * time.Minute)
	sync(t, st, t0.Add(4*time.Minute), nil, ep)
	wantState(t, item(t, st, id), Done)

	// A wait dismissed before its item existed starts Done.
	dismissed := sess(2, "reply", t0)
	dismissed.DismissedAt = t0.Add(time.Minute)
	sync(t, st, t0.Add(5*time.Minute), nil, ep, dismissed)
	for _, it := range all(t, st) {
		if it.Ref == dismissed.Ref {
			wantState(t, it, Done)
		}
	}
}

func TestSeen(t *testing.T) {
	st := newStore(t)
	sync(t, st, t0, nil, sess(1, "reply", t0))
	id := only(t, st).ID
	it, m := act(t, st, id, Action{Do: ActSeen}, t0.Add(time.Minute))
	if !it.SeenAt.Equal(t0.Add(time.Minute)) || m.ID != 0 || len(history(t, st, id)) != 1 {
		t.Errorf("seen: %+v, move %+v", it, m)
	}
	it, _ = act(t, st, id, Action{Do: ActSeen}, t0.Add(time.Hour))
	if !it.SeenAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("seen twice moved SeenAt to %v", it.SeenAt)
	}
}

func TestGroups(t *testing.T) {
	mk := func(id int64, plan string, since time.Duration) store.InboxItem {
		return store.InboxItem{ID: id, PlanSlug: plan, Since: t0.Add(since)}
	}
	items := []store.InboxItem{
		mk(1, "", -time.Hour), mk(2, "aim", -time.Minute), mk(3, "sharing", -time.Minute),
		mk(4, "zeta", -time.Hour), mk(5, "Sharing", -2*time.Hour), mk(6, "aim", -3*time.Hour), mk(7, "beta", 0), mk(8, "aim", -time.Minute),
	}
	var got []string
	for _, g := range Groups(items, []string{"sharing", "aim"}) {
		var ids []string
		for _, it := range g.Items {
			ids = append(ids, store.PRRef("", int(it.ID)))
		}
		got = append(got, strings.ToLower(g.Plan)+":"+strings.Join(ids, ","))
	}
	// Registered plans in their order, other plans by name, no plan last;
	// oldest first, then by id.
	want := []string{"sharing:#5,#3", "aim:#6,#2,#8", "beta:#7", "zeta:#4", ":#1"}
	if !slices.Equal(got, want) {
		t.Errorf("groups %q, want %q", got, want)
	}
}

// A provisional package's missing plan row is an episode like a PR's: its
// suggested row is its detail and changes it silently; it ends by itself
// when the row lands, and as gone when the provisional package goes.
func TestPlanRowEpisode(t *testing.T) {
	st := newStore(t)
	row := func(detail string) Episode {
		return Episode{Source: SourcePlan, Ref: PlanRowRef("AIM", "w12"), Kind: KindPlanRow, Since: t0, Detail: detail,
			Sig: KindPlanRow + "|W12", Title: "W12 isn't in ROADMAP.md", PlanSlug: "aim", Pkg: "W12", Why: "work names W12"}
	}
	if ref := PlanRowRef("AIM", "w12"); ref != "planrow:aim/W12" {
		t.Errorf("PlanRowRef = %q", ref)
	}
	if KindLabel(KindPlanRow) != "Plan row missing" {
		t.Errorf("KindLabel(plan_row) = %q", KindLabel(KindPlanRow))
	}
	sync(t, st, t0, nil, row("| **W12 Unregister** |  |  |  |"))
	it := only(t, st)
	if it.State != Open || it.Source != SourcePlan || it.Detail != "| **W12 Unregister** |  |  |  |" {
		t.Fatalf("item = %+v", it)
	}
	moves, err := st.InboxMoves(context.Background(), it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(moves) != 1 || moves[0].Text != "Started needing you: work names W12" {
		t.Errorf("history = %+v", moves)
	}
	// A better title: the row changes, quietly.
	sync(t, st, t0.Add(time.Minute), nil, row("| **W12 Unregister from the plan page** |  |  |  |"))
	if it = only(t, st); it.Detail != "| **W12 Unregister from the plan page** |  |  |  |" {
		t.Errorf("detail = %q", it.Detail)
	}
	moves, _ = st.InboxMoves(context.Background(), it.ID)
	if len(moves) != 1 {
		t.Errorf("a new row made history: %+v", moves)
	}
	// The row lands.
	sync(t, st, t0.Add(2*time.Minute), map[string]string{it.Ref: "its row is in ROADMAP.md now"})
	if it = only(t, st); it.State != Done || it.EndReason != "its row is in ROADMAP.md now" {
		t.Errorf("after the row landed: %+v", it)
	}
	// A new episode, then the package goes.
	sync(t, st, t0.Add(3*time.Minute), nil, row("| **W12 Unregister** |  |  |  |"))
	sync(t, st, t0.Add(4*time.Minute), nil)
	items := all(t, st)
	if len(items) != 2 || items[1].EndReason != "the provisional package is gone" {
		t.Errorf("items = %+v", items)
	}
}

// Your step is an episode like a plan row's: its "you:" step is its
// detail and, closed off in its signature, a change of it (a longer one
// too) wakes a snooze "until it changes"; it ends by itself, and as gone
// when its package goes.
func TestYourStepEpisode(t *testing.T) {
	st := newStore(t)
	step := func(note string) Episode {
		return Episode{Source: SourcePlan, Ref: YourStepRef("Search-V2", "q2"), Kind: KindYourStep, Since: t0, Detail: note,
			Sig: KindYourStep + "|Q2|" + note + "|", Title: "Q2 Ship the index", PlanSlug: "search-v2", Pkg: "Q2",
			Why: "your step is ready: " + note}
	}
	if ref := YourStepRef("Search-V2", "q2"); ref != "step:search-v2/Q2" {
		t.Errorf("YourStepRef = %q", ref)
	}
	if KindLabel(KindYourStep) != "Your step" {
		t.Errorf("KindLabel(your_step) = %q", KindLabel(KindYourStep))
	}
	sync(t, st, t0, nil, step("sign off"))
	it := only(t, st)
	if it.State != Open || it.Kind != KindYourStep || it.Detail != "sign off" {
		t.Fatalf("item = %+v", it)
	}
	act(t, st, it.ID, Action{Do: ActSnooze, Snooze: SnoozeChange}, t0.Add(time.Minute))
	// The same step again: still snoozed.
	sync(t, st, t0.Add(2*time.Minute), nil, step("sign off"))
	wantState(t, only(t, st), Snoozed)
	// A longer step is a change, not the same one told better.
	sync(t, st, t0.Add(3*time.Minute), nil, step("sign off: then tag"))
	it = only(t, st)
	wantState(t, it, Open)
	if h := history(t, st, it.ID); h[len(h)-1] != "Back to Open: it changed: sign off: then tag" {
		t.Errorf("history = %q", h)
	}
	// The plan goes: its episode is gone.
	sync(t, st, t0.Add(4*time.Minute), nil)
	if it = only(t, st); it.Waiting() || it.EndReason != "the package is gone: its plan was unregistered, or its row left the plan file" {
		t.Errorf("after its plan went: %+v", it)
	}
}
