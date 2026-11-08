package eventstest

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gravexa-llc/ai-manager/internal/events"
)

// fixtures is the recorded event logs in testdata/events.
func fixtures(t *testing.T) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "..", "..", "testdata", "events", "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) < 10 {
		t.Fatalf("found %d event logs, want 10", len(paths))
	}
	return paths
}

func load(t *testing.T, name string) []events.Event {
	t.Helper()
	evs, err := LoadS2(filepath.Join("..", "..", "..", "testdata", "events", name))
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func kinds(evs []events.Event) []events.Kind {
	var ks []events.Kind
	for _, e := range evs {
		ks = append(ks, e.Kind)
	}
	return ks
}

// Every fixture translates into events that pass the contract's checks
// unchanged: known kinds, a session id and a time on each, nothing over a
// cap. A batch of them round-trips through DecodeBatch.
func TestEveryFixtureTranslatesToValidEvents(t *testing.T) {
	for _, path := range fixtures(t) {
		t.Run(filepath.Base(path), func(t *testing.T) {
			evs, err := LoadS2(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(evs) == 0 {
				t.Fatal("no events")
			}
			if evs[0].Kind != events.SessionStart {
				t.Errorf("first event %s, want session_start", evs[0].Kind)
			}
			if last := evs[len(evs)-1]; last.Kind != events.SessionEnd {
				t.Errorf("last event %s, want session_end", last.Kind)
			}
			for i, e := range evs {
				before, _ := json.Marshal(e)
				if err := e.Normalize(); err != nil {
					t.Fatalf("event %d: %v", i, err)
				}
				after, _ := json.Marshal(e)
				if string(before) != string(after) {
					t.Errorf("event %d changed by Normalize:\n%s\n%s", i, before, after)
				}
				if i > 0 && e.At.Before(evs[i-1].At) {
					t.Errorf("event %d is earlier than the one before it", i)
				}
				t.Logf("%s %-15s agent=%-17s %s", e.At.Format("15:04:05.000"), e.Kind, e.AgentID, summary(e))
			}
			for i := 0; i < len(evs); i += events.MaxBatchEvents {
				chunk := evs[i:min(i+events.MaxBatchEvents, len(evs))]
				body, _ := json.Marshal(map[string]any{"events": chunk})
				b, err := events.DecodeBatch(strings.NewReader(string(body)))
				if err != nil {
					t.Fatal(err)
				}
				if len(b.Events) != len(chunk) || b.Dropped() != 0 {
					t.Fatalf("decoded %d events, dropped %d, want %d and 0", len(b.Events), b.Dropped(), len(chunk))
				}
			}
		})
	}
}

func summary(e events.Event) string {
	var parts []string
	add := func(k, v string) {
		if v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	add("cwd", e.Cwd)
	add("repo", e.Repo)
	add("wt", e.Worktree)
	add("source", e.Source)
	add("origin", e.OriginKind)
	add("reason", e.Reason)
	add("said", e.LastSaid)
	add("label", e.Label)
	add("id", e.ToolUseID)
	add("name", e.Name)
	for _, a := range e.Agents {
		bg := "?"
		if a.Background != nil {
			bg = map[bool]string{true: "bg", false: "fg"}[*a.Background]
		}
		parts = append(parts, a.ID+":"+a.Status+":"+bg)
	}
	return strings.Join(parts, " ")
}

func TestMinimalSession(t *testing.T) {
	evs := load(t, "print-session-minimal.jsonl")
	want := []events.Kind{events.SessionStart, events.Prompt, events.TurnStart, events.TurnComplete, events.AgentList, events.SessionEnd}
	if got := kinds(evs); !slices.Equal(got, want) {
		t.Fatalf("kinds = %v, want %v", got, want)
	}
	start := evs[0]
	if start.Cwd != "/tmp/probe" || start.Repo != "/tmp/probe" || start.Worktree != "/tmp/probe" || start.Source != "startup" {
		t.Errorf("session_start = %+v", start)
	}
	if start.Interactive == nil || *start.Interactive {
		t.Errorf("interactive = %v, want false (claude -p)", start.Interactive)
	}
	if evs[1].OriginKind != "sdk" || !events.FromPerson(evs[1].OriginKind) {
		t.Errorf("prompt origin = %q, want sdk (the person)", evs[1].OriginKind)
	}
	if tc := evs[3]; tc.Reason != events.ReasonAnswer || tc.LastSaid != "[assistant text]" {
		t.Errorf("turn_complete = %+v", tc)
	}
	if evs[5].Reason != "other" {
		t.Errorf("session_end reason = %q, want other", evs[5].Reason)
	}
}

// A permission ask raises needs_you once, from classic.PermissionRequest
// (the dialog), under the tool_use_id tool.check saw; tool.check's own ask
// raises none. The tool event with that id follows.
func TestPermissionAskInSubagent(t *testing.T) {
	evs := load(t, "permission-ask-subagent.jsonl")
	var needs []events.Event
	var tool *events.Event
	for i, e := range evs {
		if e.Kind == events.NeedsYou {
			needs = append(needs, e)
		}
		if e.Kind == events.Tool && len(needs) > 0 && tool == nil {
			tool = &evs[i]
		}
	}
	if len(needs) != 1 {
		t.Fatalf("got %d needs_you, want 1", len(needs))
	}
	const sub = "ad042043bfa112bd0"
	if needs[0].Source != events.SourcePermissionRequest {
		t.Errorf("source %q, want %q", needs[0].Source, events.SourcePermissionRequest)
	}
	for _, n := range needs {
		if n.AgentID != sub || n.ToolUseID == "" || n.ToolUseID != needs[0].ToolUseID || n.Reason != "Bash: Create directory s2-sub-dir" {
			t.Errorf("needs_you = %+v", n)
		}
	}
	if tool == nil || tool.AgentID != sub || tool.ToolUseID != needs[0].ToolUseID {
		t.Errorf("tool after the ask = %+v", tool)
	}
}

// The older mod (Mod.ToolCheckAsks) sends a tool_check needs_you for the
// same call before the dialog's permission_request, under the same
// tool_use_id, with the call's label and wait. The rest of the stream is
// the current mod's.
func TestOlderModToolCheckAsks(t *testing.T) {
	path := filepath.Join("..", "..", "..", "testdata", "events", "permission-ask-main-loop.jsonl")
	old, err := LoadS2Mod(path, Mod{ToolCheckAsks: true})
	if err != nil {
		t.Fatal(err)
	}
	var needs []events.Event
	for _, e := range old {
		if e.Kind == events.NeedsYou {
			needs = append(needs, e)
		}
	}
	if len(needs) != 2 || needs[0].Source != events.SourceToolCheck || needs[1].Source != events.SourcePermissionRequest {
		t.Fatalf("needs_you = %+v, want tool_check then permission_request", needs)
	}
	if needs[0].ToolUseID == "" || needs[0].ToolUseID != needs[1].ToolUseID || needs[0].Reason != needs[1].Reason || needs[0].Wait == nil {
		t.Errorf("tool_check ask %+v, dialog %+v: want the same call, label and a wait", needs[0], needs[1])
	}
	cur := load(t, "permission-ask-main-loop.jsonl")
	if len(old) != len(cur)+1 {
		t.Errorf("older mod sent %d events, current %d: want one more", len(old), len(cur))
	}
}

// A worktree-isolated agent: worktree_create names it, its turn_start and
// tool events carry the worktree as cwd, and agent_list says it runs in
// the background.
func TestWorktreeAgent(t *testing.T) {
	evs := load(t, "subagents-default-background.jsonl")
	const wt = "a5bd483a78c5f4783"
	var created, cwd, listed bool
	for _, e := range evs {
		switch {
		case e.Kind == events.WorktreeCreate && e.Name == "agent-"+wt:
			created = true
		case e.Kind == events.TurnStart && e.AgentID == wt:
			cwd = e.Cwd == "/tmp/probe/.claude/worktrees/agent-"+wt
		case e.Kind == events.AgentList:
			for _, a := range e.Agents {
				if a.ID == wt && a.Background != nil && *a.Background {
					listed = true
				}
			}
		}
	}
	if !created || !cwd || !listed {
		t.Errorf("worktree_create %v, worktree cwd %v, listed in background %v", created, cwd, listed)
	}
}

func TestEnterExitWorktreeCwd(t *testing.T) {
	evs := load(t, "session-enter-exit-worktree.jsonl")
	var cwds []string
	for _, e := range evs {
		if e.Kind == events.Tool {
			cwds = append(cwds, e.Label+" @ "+e.Cwd)
		}
	}
	want := []string{
		"ToolSearch @ /tmp/probe",
		"EnterWorktree s2ew @ /tmp/probe/.claude/worktrees/s2ew",
		"ExitWorktree remove @ /tmp/probe",
	}
	if !slices.Equal(cwds, want) {
		t.Errorf("tool events:\n%s\nwant\n%s", strings.Join(cwds, "\n"), strings.Join(want, "\n"))
	}
}

func TestLabel(t *testing.T) {
	cwd := "/r"
	for _, c := range []struct {
		tool  string
		input map[string]any
		want  string
	}{
		{"Bash", map[string]any{"command": "make check", "description": "Run the\nchecks"}, "Bash: Run the checks"},
		{"Bash", map[string]any{"command": "/usr/bin/env FOO=1 secret-thing"}, "Bash: env"},
		{"Edit", map[string]any{"file_path": "/r/internal/x.go"}, "Edit internal/x.go"},
		{"Write", map[string]any{"file_path": "/elsewhere/y.md", "content": "[file content]"}, "Write y.md"},
		{"Read", map[string]any{"file_path": "/r/../etc/z"}, "Read z"},
		{"Agent", map[string]any{"description": "wt probe", "prompt": "[prompt]"}, "Agent: wt probe"},
		{"Grep", map[string]any{"pattern": strings.Repeat("a", 100)}, "Grep: " + strings.Repeat("a", 60)},
		{"WebFetch", map[string]any{"url": "https://example.com/a?b=c"}, "WebFetch example.com"},
		{"SendMessage", map[string]any{"to": "a1", "message": "[message]"}, "SendMessage to a1"},
		{"EnterWorktree", map[string]any{"name": "w"}, "EnterWorktree w"},
		{"ExitWorktree", map[string]any{"action": "keep"}, "ExitWorktree keep"},
		{"ToolSearch", map[string]any{"query": "select:X"}, "ToolSearch"},
	} {
		if got := Label(c.tool, c.input, cwd); got != c.want {
			t.Errorf("Label(%s, %v) = %q, want %q", c.tool, c.input, got, c.want)
		}
	}
}

// A subagent's turn.step results set its token count on the next
// agent_list: the latest step's usage added up, not a running total. The
// main loop's steps, and a step with no usage, change nothing.
func TestStepTokens(t *testing.T) {
	const sid, sub, other = "lead-session", "a1b2c3d4e5f6a7b8c", "a0000000000000000"
	rec := func(seq int, event, phase, agent, data string) string {
		line := `{"seq": ` + strconv.Itoa(seq) + `, "t": "2026-11-06T09:00:0` + strconv.Itoa(seq) + `.000Z", "sessionId": "` + sid +
			`", "event": "` + event + `", "phase": "` + phase + `"`
		if agent != "" {
			line += `, "agentId": "` + agent + `"`
		}
		return line + `, "data": ` + data + `}`
	}
	list := `{"agents": [{"id": "` + sub + `", "status": "running", "type": "general-purpose"}]}`
	log := strings.Join([]string{
		rec(1, "session.start", "in", "", `{"cwd": "/tmp/probe"}`),
		rec(2, "turn.step", "out", sub, `{"usage": {"input_tokens": 2, "cache_read_input_tokens": 0, "cache_creation_input_tokens": 3800, "output_tokens": 700, "model": "m"}}`),
		rec(3, "agent.list", "out", "", list),
		rec(4, "turn.step", "out", sub, `{"usage": {"input_tokens": 2, "cache_read_input_tokens": 3800, "cache_creation_input_tokens": 21000, "output_tokens": 1500, "model": "m"}}`),
		rec(5, "turn.step", "out", sub, `{"usage": null}`),
		rec(6, "turn.step", "out", "", `{"usage": {"input_tokens": 9, "cache_read_input_tokens": 90000, "output_tokens": 900}}`),
		rec(7, "agent.list", "out", "", `{"agents": [{"id": "`+sub+`", "status": "running", "type": "general-purpose"}, {"id": "`+other+`", "status": "pending", "type": "Explore"}]}`),
		rec(8, "session.end", "in", "", `{"reason": "other"}`),
	}, "\n")
	evs, err := ReadS2(strings.NewReader(log))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range evs {
		if err := e.Normalize(); err != nil {
			t.Fatal(err)
		}
		for _, a := range e.Agents {
			got = append(got, a.ID+"="+strconv.FormatInt(a.Tokens, 10))
		}
	}
	if want := []string{sub + "=4502", sub + "=26302", other + "=0"}; !slices.Equal(got, want) {
		t.Errorf("agent_list tokens = %v, want %v", got, want)
	}
	if n := StepTokens(map[string]any{"input_tokens": "9", "output_tokens": -4.0, "cache_read_input_tokens": 10.0}); n != 10 {
		t.Errorf("StepTokens of odd counts = %d, want 10", n)
	}
	if n := StepTokens(nil); n != 0 {
		t.Errorf("StepTokens(nil) = %d, want 0", n)
	}
}
