package eventstest

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/events"
)

// needs returns the needs_you events of evs.
func needs(evs []events.Event) []events.Event {
	var out []events.Event
	for _, e := range evs {
		if e.Kind == events.NeedsYou {
			out = append(out, e)
		}
	}
	return out
}

// The recorded interactive run: every dialog's needs_you carries
// what it waits on, the subagent's carries its agent id, no Notification
// is sent on top of an ask already reported, and /clear's new id gets a
// session_start from classic.SessionStart.
func TestInteractiveDialogs(t *testing.T) {
	evs := load(t, "interactive-dialogs-clear.jsonl")
	byKind := map[string][]events.Event{}
	for _, e := range needs(evs) {
		if e.Source == events.SourceNotification {
			t.Errorf("a notification needs_you was sent while an ask was open: %+v", e)
		}
		if e.Wait == nil {
			t.Errorf("needs_you without a wait: %+v", e)
			continue
		}
		byKind[e.Wait.Kind] = append(byKind[e.Wait.Kind], e)
	}
	perm := byKind[events.WaitPermission]
	if len(perm) == 0 || perm[0].Wait.Tool != "Bash" || perm[0].Wait.Summary != "mkdir perm-main" || perm[0].AgentID != "" {
		t.Errorf("main-loop permission = %+v", perm)
	}
	var sub *events.Event
	for i := range perm {
		if perm[i].AgentID != "" {
			sub = &perm[i]
			break
		}
	}
	if sub == nil || sub.AgentID != "a6cabd35f43276105" || sub.Wait.Summary != "mkdir perm-sub && echo done" {
		t.Errorf("subagent permission = %+v", sub)
	}
	q := byKind[events.WaitQuestion]
	want := []events.Question{{Header: "Color", Question: "Do you prefer red or blue?", Options: []string{"Red", "Blue"}}}
	if len(q) == 0 || !reflect.DeepEqual(q[0].Wait.Questions, want) || q[0].Source != events.SourceAskUserQuestion {
		t.Errorf("question = %+v", q)
	}
	for _, e := range q {
		if e.AgentID != "" {
			t.Errorf("a subagent asked a question: %+v (R1: subagents have no AskUserQuestion)", e)
		}
	}
	plan := byKind[events.WaitPlan]
	if len(plan) == 0 || plan[0].Wait.Plan != "[plan text]" || plan[0].Source != events.SourceExitPlanMode {
		t.Errorf("plan = %+v", plan)
	}

	var idle int
	var cleared bool
	for i, e := range evs {
		if e.Kind == events.Idle {
			idle++
		}
		if e.Kind == events.SessionEnd && e.Reason == events.ReasonClear {
			next := evs[i+1]
			if next.Kind != events.SessionStart || next.Source != events.SourceClear || next.PrevSessionID != e.SessionID ||
				next.SessionID == e.SessionID || next.Repo != "/tmp/d2probe" || next.Interactive == nil || !*next.Interactive {
				t.Errorf("after /clear: %+v", next)
			}
			cleared = true
		}
	}
	if idle < 2 || !cleared {
		t.Errorf("%d idle events, /clear seen %v", idle, cleared)
	}
}

// A foreground subagent's permission (CLAUDE_CODE_DISABLE_BACKGROUND_TASKS)
// is attributed to it, and its Notification, which names no agent, isn't
// put on the main loop.
func TestForegroundSubagentPermission(t *testing.T) {
	ns := needs(load(t, "interactive-foreground-subagent.jsonl"))
	if len(ns) != 1 || ns[0].Source != events.SourcePermissionRequest {
		t.Fatalf("%d needs_you events, want 1 (permission_request; tool.check raises none): %+v", len(ns), ns)
	}
	for _, e := range ns {
		if e.AgentID != "ab02809c073aa7d32" || e.Wait == nil || e.Wait.Summary != "mkdir perm-fg" {
			t.Errorf("needs_you = %+v", e)
		}
	}
}

// A `claude --bg` session loads the mod: its session_start says
// interactive and background, and its first prompt is the brief.
func TestBackgroundSession(t *testing.T) {
	evs := load(t, "background-session-brief.jsonl")
	s := evs[0]
	if s.Kind != events.SessionStart || s.Interactive == nil || !*s.Interactive || s.Background == nil || !*s.Background {
		t.Errorf("session_start = %+v", s)
	}
	if evs[1].Kind != events.Prompt || evs[1].OriginKind != "composer" {
		t.Errorf("second event = %+v, want the brief's prompt", evs[1])
	}
	idle := load(t, "background-session-no-prompt.jsonl")
	if got := kinds(idle); !reflect.DeepEqual(got, []events.Kind{events.SessionStart, events.SessionEnd}) {
		t.Errorf("a background session without a prompt: %v", got)
	}
}

// The examples plugin/mod/wait.test.ts checks too: the two sides follow
// the same rules.
func TestWaitFor(t *testing.T) {
	long := strings.Repeat("x", 400)
	for _, c := range []struct {
		tool  string
		input map[string]any
		want  *events.Wait
	}{
		{"Bash", map[string]any{"command": "mkdir perm-main", "description": "Create directory perm-main"},
			&events.Wait{Kind: "permission", Tool: "Bash", Summary: "mkdir perm-main"}},
		{"Bash", map[string]any{"command": "cd /tmp &&\n  npm test"},
			&events.Wait{Kind: "permission", Tool: "Bash", Summary: "cd /tmp && npm test"}},
		{"Bash", map[string]any{"command": "echo " + long},
			&events.Wait{Kind: "permission", Tool: "Bash", Summary: ("echo " + long)[:300]}},
		{"Bash", map[string]any{"command": "curl -H 'Authorization: Bearer abcdefghijklmnop1234' https://user:pw@example.com/x?api_key=s3cr3t"},
			&events.Wait{Kind: "permission", Tool: "Bash", Summary: "curl -H 'Authorization: [redacted]' https://[redacted]@example.com/x?api_key=[redacted]"}},
		{"Bash", map[string]any{"command": "GITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz0123 gh pr list --password hunter2"},
			&events.Wait{Kind: "permission", Tool: "Bash", Summary: "GITHUB_TOKEN=[redacted] gh pr list --password [redacted]"}},
		{"Write", map[string]any{"file_path": "/tmp/d2probe/plan.txt", "content": "ok\n"},
			&events.Wait{Kind: "permission", Tool: "Write", Summary: "/tmp/d2probe/plan.txt"}},
		{"WebFetch", map[string]any{"url": "https://example.com/a", "prompt": "summarise"},
			&events.Wait{Kind: "permission", Tool: "WebFetch", Summary: "https://example.com/a"}},
		{"Grep", map[string]any{"pattern": "TODO", "path": "src"},
			&events.Wait{Kind: "permission", Tool: "Grep", Summary: "TODO in src"}},
		{"mcp__github__create_issue", map[string]any{"title": "Bug", "body": "line one\nline two", "labels": []any{"x"}, "draft": true},
			&events.Wait{Kind: "permission", Tool: "mcp__github__create_issue", Summary: "body: line one line two, draft: true, title: Bug"}},
		{"AskUserQuestion", map[string]any{"questions": []any{
			map[string]any{"question": "Do you prefer red or blue?", "header": "Color", "multiSelect": false,
				"options": []any{map[string]any{"label": "Red", "description": "d"}, map[string]any{"label": "Blue"}}},
			map[string]any{"question": "Which ones?", "multiSelect": true, "options": []any{map[string]any{"label": "A"}}},
		}}, &events.Wait{Kind: "question", Tool: "AskUserQuestion", Questions: []events.Question{
			{Header: "Color", Question: "Do you prefer red or blue?", Options: []string{"Red", "Blue"}},
			{Question: "Which ones?", Options: []string{"A"}, MultiSelect: true},
		}}},
		{"AskUserQuestion", map[string]any{"questions": "not a list"}, nil},
		{"ExitPlanMode", map[string]any{"plan": "\n# Plan\n\n1. Add the field   \n2. Test it\n3. a\n4. b\n5. c\n6. d\n7. e\n8. f\n9. g\n10. h\n11. i\n12. j\n", "planFilePath": "/x.md"},
			&events.Wait{Kind: "plan", Tool: "ExitPlanMode", Plan: "# Plan\n\n1. Add the field\n2. Test it\n3. a\n4. b\n5. c\n6. d\n7. e\n8. f\n9. g\n10. h"}},
		{"ExitPlanMode", map[string]any{}, nil},
	} {
		got := WaitFor(c.tool, c.input)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("WaitFor(%s, %v)\n got %+v\nwant %+v", c.tool, c.input, got, c.want)
		}
		if got != nil {
			// What the mod sends passes aimd's caps unchanged.
			e := events.Event{Kind: events.NeedsYou, SessionID: "s", At: time.Unix(1, 0), Wait: got}
			if err := e.Normalize(); err != nil || !reflect.DeepEqual(e.Wait, got) {
				t.Errorf("Normalize changed the wait of %s: %+v → %+v (%v)", c.tool, got, e.Wait, err)
			}
		}
	}
}
