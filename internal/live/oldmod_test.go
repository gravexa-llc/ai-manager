package live

import (
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/events"
	"github.com/gravexa-llc/ai-manager/internal/events/eventstest"
	"github.com/gravexa-llc/ai-manager/internal/store"
)

// Sessions started before the needs-you fix keep the older mod until they
// restart. It also sends a needs_you (source tool_check) for every call
// tool.check answers "ask", which in auto mode the classifier then allows
// with no dialog. aimd drops those: the dialogs the older mod reports as
// permission_request still need you.

// oldFixture loads a recorded event log as the older mod sent it.
func oldFixture(t *testing.T, name string) []events.Event {
	t.Helper()
	evs, err := eventstest.LoadS2Mod(filepath.Join("..", "..", "testdata", "events", name), eventstest.Mod{ToolCheckAsks: true})
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func isToolCheck(e events.Event) bool {
	return e.Kind == events.NeedsYou && e.Source == events.SourceToolCheck
}

// An older mod's stream derives, event by event, the same session state,
// wait detail and agent rows as the current mod's: its tool_check asks
// change nothing, and the dialogs need you from the permission_request on.
func TestOlderModStreamMatchesCurrent(t *testing.T) {
	for _, name := range []string{
		"permission-ask-main-loop.jsonl",
		"permission-ask-subagent.jsonl",
		"interactive-foreground-subagent.jsonl",
		"interactive-dialogs-clear.jsonl",
	} {
		t.Run(name, func(t *testing.T) {
			old, cur := oldFixture(t, name), fixture(t, name)
			asks := 0
			for _, e := range old {
				if isToolCheck(e) {
					asks++
				}
			}
			if asks == 0 || len(old) != len(cur)+asks {
				t.Fatalf("older mod: %d events with %d tool_check asks; current: %d", len(old), asks, len(cur))
			}
			ho, hc := newHarness(t, nil), newHarness(t, nil)
			needed := false
			i := 0
			for _, e := range old {
				ho.post(e)
				if !isToolCheck(e) {
					if c := cur[i]; c.Kind != e.Kind || !c.At.Equal(e.At) || c.AgentID != e.AgentID {
						t.Fatalf("event %d: older %s %v, current %s %v", i, e.Kind, e.At, c.Kind, c.At)
					}
					hc.post(cur[i])
					i++
				}
				// /clear moves the session to a new id; follow the current mod's.
				sid := cur[max(i-1, 0)].SessionID
				so, sc := ho.session(sid), hc.session(sid)
				if so.State != sc.State || so.StateReason != sc.StateReason || !so.NeedsYouSince.Equal(sc.NeedsYouSince) {
					t.Fatalf("after %s %s at %v: older mod %q (%q, since %v), current %q (%q, since %v)",
						e.Kind, e.Source, e.At, so.State, so.StateReason, so.NeedsYouSince, sc.State, sc.StateReason, sc.NeedsYouSince)
				}
				wo, oko := ho.recv.WaitingOn(sid)
				wc, okc := hc.recv.WaitingOn(sid)
				if oko != okc || !reflect.DeepEqual(wo, wc) {
					t.Fatalf("after %s %s at %v: waiting on %+v (%v), current %+v (%v)", e.Kind, e.Source, e.At, wo, oko, wc, okc)
				}
				if ao, ac := agentStates(ho.agents(sid)), agentStates(hc.agents(sid)); !reflect.DeepEqual(ao, ac) {
					t.Fatalf("after %s %s at %v: agents %v, current %v", e.Kind, e.Source, e.At, ao, ac)
				}
				needed = needed || so.State == StateNeedsYou
			}
			if !needed {
				t.Error("the dialog never needed you")
			}
			// Dropped before the dedupe pass, so the dialog's needs_you for
			// the same call isn't taken for a repeat of the ask.
			so, sc := ho.status().Events, hc.status().Events
			if so.Ignored != int64(asks) || sc.Ignored != 0 || so.Deduped != sc.Deduped || so.Applied != sc.Applied {
				t.Errorf("older mod: ignored %d, deduped %d, applied %d; current: ignored %d, deduped %d, applied %d; want %d ignored, the rest the same",
					so.Ignored, so.Deduped, so.Applied, sc.Ignored, sc.Deduped, sc.Applied, asks)
			}
		})
	}
}

func agentStates(m map[string]store.Agent) map[string]string {
	out := map[string]string{}
	for id, a := range m {
		out[id] = a.State
	}
	return out
}

// In auto mode the classifier allows the call: the older mod's tool_check
// ask, then the tool, and no dialog. The session never needs you, nothing
// waits, and the ask leaves no timeline row.
func TestOlderModClassifierAllowedCall(t *testing.T) {
	h := newHarness(t, nil)
	var evs []events.Event
	for _, e := range oldFixture(t, "permission-ask-main-loop.jsonl") {
		if e.Kind == events.NeedsYou && e.Source == events.SourcePermissionRequest {
			continue // no dialog opened
		}
		evs = append(evs, e)
	}
	sid := evs[0].SessionID
	sawAsk := false
	for _, e := range evs {
		h.post(e)
		sawAsk = sawAsk || isToolCheck(e)
		s := h.session(sid)
		if s.State == StateNeedsYou || !s.NeedsYouSince.IsZero() {
			t.Fatalf("after %s %s: state %q since %v, want no needs-you", e.Kind, e.Source, s.State, s.NeedsYouSince)
		}
		if w, ok := h.recv.WaitingOn(sid); ok {
			t.Fatalf("after %s %s: waiting on %+v", e.Kind, e.Source, w)
		}
		if lead := h.agents(sid)[""]; lead.State == events.AgentWaiting {
			t.Fatalf("after %s %s: lead waiting", e.Kind, e.Source)
		}
		if isToolCheck(e) && h.session(sid).State != StateWorking {
			t.Errorf("right after the ask: %q, want working", h.session(sid).State)
		}
	}
	if !sawAsk {
		t.Fatal("no tool_check ask in the stream")
	}
	if slices.ContainsFunc(h.events(sid), func(r store.Event) bool { return r.Event == string(events.NeedsYou) }) {
		t.Error("the ask left a needs_you row on the timeline")
	}
	if st := h.status().Events; st.Ignored != 1 {
		t.Errorf("ignored = %d, want 1", st.Ignored)
	}
}

// A tool_check needs_you on its own, in a batch with the tool that follows
// a second later, as the older mod sends them: never needs you.
func TestToolCheckAskIgnored(t *testing.T) {
	h := newHarness(t, nil)
	ask := needs("s1", "", "t1", t0.Add(time.Second))
	ask.Source = events.SourceToolCheck
	h.post(start("s1", t0), ask)
	if s := h.session("s1"); s.State == StateNeedsYou {
		t.Errorf("after a tool_check ask: %q (%q)", s.State, s.StateReason)
	}
	h.post(tool("s1", "", "Bash: deploy", "t1", t0.Add(2*time.Second)))
	// The dialog for the next call still needs you.
	h.post(needs("s1", "", "t2", t0.Add(3*time.Second)))
	if s := h.session("s1"); s.State != StateNeedsYou || s.StateReason != "permission prompt: Bash: deploy" {
		t.Errorf("after a permission_request: %q (%q)", s.State, s.StateReason)
	}
}
