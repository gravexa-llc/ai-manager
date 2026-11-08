package eventstest

import (
	"encoding/json"
	"path/filepath"
	"slices"
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

// A permission ask raises needs_you twice (tool.check, then
// classic.PermissionRequest) for the same tool_use_id, and the tool event
// with that id follows.
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
	if len(needs) != 2 {
		t.Fatalf("got %d needs_you, want 2", len(needs))
	}
	const sub = "ad042043bfa112bd0"
	if needs[0].Source != events.SourceToolCheck || needs[1].Source != events.SourcePermissionRequest {
		t.Errorf("sources %q, %q", needs[0].Source, needs[1].Source)
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
