package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestInboxStore(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	waiting := InboxItem{Source: "session", Ref: "session:1", Kind: "permission", State: InboxOpen, Since: at(-time.Minute), StateAt: t0, Title: "lead"}
	var id int64
	must(t, s.UpdateInbox(ctx, func(tx *InboxTx) error {
		var err error
		if id, err = tx.Insert(waiting); err != nil {
			return err
		}
		_, err = tx.AddMove(InboxMove{ItemID: id, By: InboxByAimd, Text: "Started waiting", To: InboxOpen})
		return err
	}))
	got, err := s.InboxItem(ctx, id)
	must(t, err)
	waiting.ID, waiting.CreatedAt = id, t0
	equal(t, "item", got, waiting)
	moves, err := s.InboxMoves(ctx, id)
	must(t, err)
	if len(moves) != 1 || !moves[0].At.Equal(t0) || moves[0].Text != "Started waiting" {
		t.Errorf("moves %+v", moves)
	}

	// One waiting item per ref: a second is refused until the first ends.
	err = s.UpdateInbox(ctx, func(tx *InboxTx) error { _, err := tx.Insert(waiting); return err })
	if err == nil {
		t.Fatal("a second waiting item for session:1 was stored")
	}
	must(t, s.UpdateInbox(ctx, func(tx *InboxTx) error {
		it, err := tx.Item(id)
		if err != nil {
			return err
		}
		it.State, it.EndedAt, it.EndReason = InboxDone, t0, "it stopped waiting"
		if err := tx.Save(it); err != nil {
			return err
		}
		next := waiting
		next.Since = t0
		_, err = tx.Insert(next)
		return err
	}))

	// Filters and counts.
	all, err := s.ListInbox(ctx, InboxFilter{})
	must(t, err)
	open, err := s.ListInbox(ctx, InboxFilter{State: InboxOpen, Waiting: true})
	must(t, err)
	none, err := s.ListInbox(ctx, InboxFilter{IDs: []int64{}})
	must(t, err)
	if len(all) != 2 || len(open) != 1 || open[0].ID == id || len(none) != 0 {
		t.Errorf("all %d, open %+v, none %d", len(all), open, len(none))
	}
	counts, err := s.InboxCounts(ctx)
	must(t, err)
	equal(t, "counts", counts, map[string]int{InboxOpen: 1, InboxDone: 1})

	// Pruning: only Done items that ended and went Done before the cutoff.
	if ok, err := s.InboxPrunable(ctx, t0); err != nil || ok {
		t.Errorf("prunable at t0 = %v, %v", ok, err)
	}
	if ok, err := s.InboxPrunable(ctx, at(time.Hour)); err != nil || !ok {
		t.Errorf("prunable an hour later = %v, %v", ok, err)
	}
	must(t, s.UpdateInbox(ctx, func(tx *InboxTx) error {
		n, err := tx.Prune(at(time.Hour))
		if n != 1 {
			t.Errorf("pruned %d, want 1", n)
		}
		return err
	}))
	if _, err := s.InboxItem(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("pruned item: %v", err)
	}
	if moves, _ := s.InboxMoves(ctx, id); len(moves) != 0 {
		t.Errorf("the pruned item's history is still there: %+v", moves)
	}

	// Bad rows are refused before SQLite sees them.
	for name, bad := range map[string]InboxItem{
		"no ref":   {Source: "pr", Kind: "merge", State: InboxOpen, Since: t0, StateAt: t0},
		"no since": {Source: "pr", Ref: "o/r#1", Kind: "merge", State: InboxOpen, StateAt: t0},
		"state":    {Source: "pr", Ref: "o/r#1", Kind: "merge", State: "later", Since: t0, StateAt: t0},
	} {
		if err := s.UpdateInbox(ctx, func(tx *InboxTx) error { _, err := tx.Insert(bad); return err }); err == nil {
			t.Errorf("%s: stored", name)
		}
	}
	if err := s.UpdateInbox(ctx, func(tx *InboxTx) error {
		return tx.Save(InboxItem{ID: 999, Ref: "x", State: InboxOpen, Since: t0, StateAt: t0})
	}); !errors.Is(err, ErrNotFound) {
		t.Errorf("saving no item: %v", err)
	}
}
