// Package eventstest turns recorded probe logs (testdata/events/*.jsonl,
// one line per hook call of a throwaway mod that logged every hook) into
// the events the real mod sends (internal/events).
//
// The translation is the mapping the mod (plugin/mod) implements, written
// as code: which hook raises which event, which field goes where, and how
// labels are made. aimd's tests replay the recorded sessions through POST
// /events with it, so if the contract and this mapping drift apart, a test
// says so.
//
// Mapping, by hook (the probe logs each hook's input as phase "in" and its
// result as "out"):
//
//	classic.SessionStart        remembered: its source (startup, resume, clear, compact);
//	                            once session.start has run, a new session id (after
//	                            /clear: session.start doesn't fire again) is a
//	                            session_start with the facts already known, and
//	                            prev_session_id after a /clear
//	session.start               session_start: cwd, interactive (isInteractive), source,
//	                            repo and worktree from $.session.repo().root and
//	                            $.session.root(), branch from git, host, ITERM_SESSION_ID,
//	                            background (CLAUDE_CODE_SESSION_KIND=bg)
//	prompt.submit               prompt: origin_kind = origin.kind
//	turn.start                  turn_start: agent_id = agentId, cwd = $.session.cwd()
//	tool.call (in)              remembered for its label; needs_you for AskUserQuestion
//	                            (source ask_user_question) and ExitPlanMode (exit_plan_mode),
//	                            with their wait (the questions, the plan)
//	tool.check (decision ask)   needs_you: source tool_check, reason = the tool's label,
//	                            wait = WaitFor(tool, input); only when approver is
//	                            "person" or absent
//	classic.PermissionRequest   needs_you: source permission_request, agent_id = agent_id,
//	                            wait = WaitFor(tool_name, tool_input)
//	tool.call (out)             tool: tool, label, tool_use_id, cwd = $.session.cwd()
//	classic.Notification        idle (idle_prompt), or needs_you (source notification)
//	                            only while no ask is pending: it carries no agent_id
//	                            even for a subagent's dialog
//	classic.StopFailure         error: reason = error
//	turn.complete               turn_complete: reason, last_said = first line of answer
//	classic.WorktreeCreate      worktree_create: name
//	agent.spawn                 remembered: background and parentAgentId, by the agentId
//	                            its result returns (resumes included)
//	$.agent.list()              agent_list: id, status, type, background, parent_id
//	session.end                 session_end: reason (and new_session_id after /clear)
//
// Every event gets the session id ($.session.id()) and the hook's time. The
// probe cuts strings to 200 characters and the fixtures are scrubbed, so
// last_said and labels here are placeholders like "[assistant text]".
package eventstest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/events"
)

// record is one line of a probe log.
type record struct {
	Seq        int             `json:"seq"`
	T          time.Time       `json:"t"`
	SessionID  string          `json:"sessionId"`
	Event      string          `json:"event"`
	Phase      string          `json:"phase"`
	SessionCwd string          `json:"sessionCwd"`
	AgentID    string          `json:"agentId"`
	Data       json.RawMessage `json:"data"`
}

// LoadS2 reads a probe log file and translates it.
func LoadS2(path string) ([]events.Event, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	evs, err := ReadS2(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return evs, nil
}

// ReadS2 reads a probe log and returns the events the mod would have
// sent, in order.
func ReadS2(r io.Reader) ([]events.Event, error) {
	var recs []record
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return nil, fmt.Errorf("line %d: %w", len(recs)+1, err)
		}
		recs = append(recs, rec)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	t := newTranslator(recs)
	for i := range recs {
		if err := t.record(i); err != nil {
			return nil, fmt.Errorf("seq %d (%s %s): %w", recs[i].Seq, recs[i].Event, recs[i].Phase, err)
		}
	}
	return t.out, nil
}

// toolCall is a tool.call input the translator keeps for its label.
type toolCall struct {
	tool  string
	input map[string]any
	agent string
}

// spawn is what agent.spawn says about an agent.
type spawn struct {
	background bool
	parent     string
}

type translator struct {
	recs []record
	out  []events.Event

	source   map[string]string    // session id → classic.SessionStart source
	cwd      map[string]string    // loop (agent id, "" = main) → latest cwd
	calls    map[string]*toolCall // tool_use_id → its tool.call input
	checking map[string]string    // loop → tool_use_id of the tool.check in progress
	approver map[string]string    // loop → that tool.check's approver ("" when absent)
	asked    map[string]string    // loop → tool_use_id of the last ask
	pending  []spawn              // agent.spawn inputs waiting for their result
	spawns   map[string]spawn     // agent id → spawn
	// open is the asks not yet answered: tool_use_id → its loop. A
	// Notification is only sent while it is empty (it names no loop).
	open map[string]string
	// lastSession is the session id of the latest session_start: after a
	// /clear the module stays loaded, so the next session_start (source
	// clear) names it as prev_session_id. started is that session_start,
	// whose facts a session_start from classic.SessionStart repeats.
	lastSession string
	started     *events.Event
}

func newTranslator(recs []record) *translator {
	return &translator{
		recs:     recs,
		source:   map[string]string{},
		cwd:      map[string]string{},
		calls:    map[string]*toolCall{},
		checking: map[string]string{},
		approver: map[string]string{},
		asked:    map[string]string{},
		spawns:   map[string]spawn{},
		open:     map[string]string{},
	}
}

// answered forgets the open asks of a loop (its turn moved on), or the
// one ask with the tool_use_id.
func (t *translator) answered(loop, id string) {
	for k, l := range t.open {
		if (id != "" && k == id) || (id == "" && l == loop) {
			delete(t.open, k)
		}
	}
}

func (t *translator) emit(rec record, e events.Event) {
	e.SessionID = rec.SessionID
	e.At = rec.T
	t.out = append(t.out, e)
}

// data decodes a record's data into a generic map.
func data(rec record) (map[string]any, error) {
	var m map[string]any
	if len(rec.Data) == 0 || string(rec.Data) == "null" {
		return map[string]any{}, nil
	}
	if err := json.Unmarshal(rec.Data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func (t *translator) record(i int) error {
	rec := t.recs[i]
	d, err := data(rec)
	if err != nil {
		return err
	}
	in, out := rec.Phase == "in", rec.Phase == "out"
	switch {
	case rec.Event == "classic.SessionStart" && in:
		t.source[rec.SessionID] = str(d, "source")
		// After /clear (or a /resume) in a running process, session.start
		// doesn't fire again: the mod reports the new id from here, with
		// what it knew of the session.
		if t.started == nil || rec.SessionID == t.lastSession {
			break
		}
		e := *t.started
		e.Source = str(d, "source")
		e.PrevSessionID = ""
		if e.Source == events.SourceClear {
			e.PrevSessionID = t.lastSession
		}
		t.lastSession = rec.SessionID
		t.emit(rec, e)

	case rec.Event == "session.start" && in:
		e := events.Event{Kind: events.SessionStart, Cwd: str(d, "cwd"), Source: t.source[rec.SessionID]}
		if v, ok := d["isInteractive"].(bool); ok {
			e.Interactive = &v
		}
		e.Repo, e.Worktree = t.sessionRepo(i)
		if t.sessionKind(i) == "bg" {
			bg := true
			e.Background = &bg
		}
		if e.Source == events.SourceClear && t.lastSession != rec.SessionID {
			e.PrevSessionID = t.lastSession
		}
		t.lastSession = rec.SessionID
		t.cwd[""] = e.Cwd
		t.started = &e
		t.emit(rec, e)

	case rec.Event == "prompt.submit" && in:
		origin, _ := d["origin"].(map[string]any)
		t.emit(rec, events.Event{Kind: events.Prompt, OriginKind: str(origin, "kind")})

	case rec.Event == "turn.start" && in:
		agent := str(d, "agentId")
		if rec.SessionCwd != "" {
			t.cwd[agent] = rec.SessionCwd
		}
		t.answered(agent, "")
		t.emit(rec, events.Event{Kind: events.TurnStart, AgentID: agent, Cwd: rec.SessionCwd})

	case rec.Event == "tool.call" && in:
		agent := str(d, "agentId")
		if rec.SessionCwd != "" {
			t.cwd[agent] = rec.SessionCwd
		}
		c := &toolCall{tool: str(d, "tool"), input: map[string]any{}, agent: agent}
		for k, v := range d {
			if k != "tool" && k != "tool_use_id" && k != "agentId" {
				c.input[k] = v
			}
		}
		id := str(d, "tool_use_id")
		t.calls[id] = c
		var source string
		switch c.tool {
		case "AskUserQuestion":
			source = events.SourceAskUserQuestion
		case "ExitPlanMode":
			source = events.SourceExitPlanMode
		}
		if source != "" {
			t.open[id] = agent
			t.emit(rec, events.Event{Kind: events.NeedsYou, AgentID: agent, Source: source,
				Reason: Label(c.tool, c.input, t.cwd[agent]), ToolUseID: id, Wait: WaitFor(c.tool, c.input)})
		}

	case rec.Event == "tool.check" && in:
		agent := str(d, "agentId")
		t.checking[agent] = str(d, "tool_use_id")
		t.approver[agent] = str(d, "approver")

	case rec.Event == "tool.check" && out:
		// tool.check's result has no ids: the hook saw them on its input.
		// The mod sends this only when approver is "person" or absent
		// (absent under -p; "person" in interactive and background
		// sessions).
		agent := rec.AgentID
		if str(d, "decision") != "ask" || (t.approver[agent] != "" && t.approver[agent] != "person") {
			break
		}
		id := t.checking[agent]
		t.asked[agent] = id
		t.open[id] = agent
		e := events.Event{Kind: events.NeedsYou, AgentID: agent, Source: events.SourceToolCheck, Reason: str(d, "tool"), ToolUseID: id}
		if c := t.calls[id]; c != nil {
			e.Reason = Label(c.tool, c.input, t.cwd[agent])
			e.Wait = WaitFor(c.tool, c.input)
		}
		t.emit(rec, e)

	case rec.Event == "classic.PermissionRequest" && in:
		agent := str(d, "agent_id")
		input, _ := d["tool_input"].(map[string]any)
		id := t.asked[agent]
		t.open[id] = agent
		t.emit(rec, events.Event{Kind: events.NeedsYou, AgentID: agent, Source: events.SourcePermissionRequest,
			Reason: Label(str(d, "tool_name"), input, t.cwd[agent]), ToolUseID: id, Wait: WaitFor(str(d, "tool_name"), input)})

	case rec.Event == "classic.PostToolUse" && in:
		// The loop's cwd after the tool ran (EnterWorktree and ExitWorktree
		// change it): what $.session.cwd() returns in tool.call's result.
		if c := str(d, "cwd"); c != "" {
			t.cwd[str(d, "agent_id")] = c
		}

	case rec.Event == "tool.call" && out:
		agent := rec.AgentID
		id := str(d, "tool_use_id")
		e := events.Event{Kind: events.Tool, AgentID: agent, Tool: str(d, "tool"), ToolUseID: id, Cwd: t.cwd[agent]}
		if c := t.calls[id]; c != nil {
			e.Label = Label(c.tool, c.input, t.cwd[agent])
		} else {
			e.Label = e.Tool
		}
		t.answered(agent, id)
		t.emit(rec, e)

	case rec.Event == "classic.Notification" && in:
		agent := str(d, "agent_id")
		switch typ := str(d, "notification_type"); typ {
		case "idle_prompt":
			t.emit(rec, events.Event{Kind: events.Idle, AgentID: agent})
		case "permission_prompt", "agent_needs_input", "elicitation_dialog":
			// It names no loop (not even a subagent's), so an ask the
			// mod already reported isn't sent again on the main loop.
			if len(t.open) == 0 {
				t.emit(rec, events.Event{Kind: events.NeedsYou, AgentID: agent, Source: events.SourceNotification, Reason: typ})
			}
		}

	case rec.Event == "classic.StopFailure" && in:
		t.emit(rec, events.Event{Kind: events.Error, AgentID: str(d, "agent_id"), Reason: str(d, "error")})

	case rec.Event == "turn.complete" && in:
		t.answered(str(d, "agentId"), "")
		t.emit(rec, events.Event{Kind: events.TurnComplete, AgentID: str(d, "agentId"),
			Reason: str(d, "reason"), LastSaid: events.FirstLine(str(d, "answer"))})

	case rec.Event == "classic.WorktreeCreate" && in:
		t.emit(rec, events.Event{Kind: events.WorktreeCreate, AgentID: str(d, "agent_id"), Name: str(d, "name")})

	case rec.Event == "agent.spawn" && in:
		bg, _ := d["background"].(bool)
		t.pending = append(t.pending, spawn{background: bg, parent: str(d, "parentAgentId")})

	case rec.Event == "agent.spawn" && out:
		if len(t.pending) == 0 {
			break
		}
		s := t.pending[0]
		t.pending = t.pending[1:]
		if id := str(d, "agentId"); id != "" {
			t.spawns[id] = s
		}

	case rec.Event == "agent.list":
		e := events.Event{Kind: events.AgentList, Agents: []events.Agent{}}
		list, _ := d["agents"].([]any)
		for _, item := range list {
			m, _ := item.(map[string]any)
			a := events.Agent{ID: str(m, "id"), Status: str(m, "status"), Type: str(m, "type"), ParentID: str(m, "parentId")}
			if s, ok := t.spawns[a.ID]; ok {
				bg := s.background
				a.Background = &bg
				if a.ParentID == "" {
					a.ParentID = s.parent
				}
			}
			e.Agents = append(e.Agents, a)
		}
		t.emit(rec, e)

	case rec.Event == "session.end" && in:
		t.emit(rec, events.Event{Kind: events.SessionEnd, Reason: str(d, "reason")})
	}
	return nil
}

// sessionRepo returns the main checkout and worktree roots the probe
// logged through $.session.repo() / $.session.root() after session.start
// at index i (the mod calls them inside its session.start hook).
func (t *translator) sessionRepo(i int) (repo, root string) {
	sid := t.recs[i].SessionID
	for _, rec := range t.recs[i+1:] {
		if rec.SessionID != sid || rec.Event != "probe.git" {
			continue
		}
		d, err := data(rec)
		if err != nil || str(d, "via") != "$.session.repo" {
			continue
		}
		r, _ := d["repo"].(map[string]any)
		return str(r, "root"), str(d, "root")
	}
	return "", ""
}

// sessionKind returns CLAUDE_CODE_SESSION_KIND as the probe logged it
// after session.start at index i ("bg" in a background session).
func (t *translator) sessionKind(i int) string {
	sid := t.recs[i].SessionID
	for _, rec := range t.recs[i+1:] {
		if rec.SessionID != sid || rec.Event != "probe.env" {
			continue
		}
		if d, err := data(rec); err == nil {
			return str(d, "CLAUDE_CODE_SESSION_KIND")
		}
	}
	return ""
}

// Label is the last-action label the mod makes for a tool call: a short
// summary, never the tool's output or a file's contents.
//
//	Bash                                 "Bash: <description>", else "Bash: <program>"
//	Edit, MultiEdit, Write, Read,        "<Tool> <file path, relative to the cwd when inside it>"
//	NotebookEdit
//	Agent, Task                          "Agent: <description>"
//	Grep, Glob                           "<Tool>: <pattern>", cut to 60 characters
//	WebFetch                             "WebFetch <host>"
//	SendMessage                          "SendMessage to <to>"
//	EnterWorktree                        "EnterWorktree <name>"
//	ExitWorktree                         "ExitWorktree <action>"
//	anything else                        "<Tool>"
func Label(tool string, input map[string]any, cwd string) string {
	switch tool {
	case "Bash":
		if d := events.OneLine(str(input, "description")); d != "" {
			return "Bash: " + d
		}
		if f := strings.Fields(str(input, "command")); len(f) > 0 {
			return "Bash: " + filepath.Base(f[0])
		}
	case "Edit", "MultiEdit", "Write", "Read", "NotebookEdit":
		p := str(input, "file_path")
		if p == "" {
			p = str(input, "notebook_path")
		}
		if p != "" {
			return tool + " " + relPath(p, cwd)
		}
	case "Agent", "Task":
		if d := events.OneLine(str(input, "description")); d != "" {
			return "Agent: " + d
		}
		return "Agent"
	case "Grep", "Glob":
		if p := str(input, "pattern"); p != "" {
			return tool + ": " + events.Truncate(events.OneLine(p), 60)
		}
	case "WebFetch":
		if u, err := url.Parse(str(input, "url")); err == nil && u.Host != "" {
			return "WebFetch " + u.Host
		}
	case "SendMessage":
		if to := str(input, "to"); to != "" {
			return "SendMessage to " + to
		}
	case "EnterWorktree":
		if n := str(input, "name"); n != "" {
			return "EnterWorktree " + n
		}
	case "ExitWorktree":
		if a := str(input, "action"); a != "" {
			return "ExitWorktree " + a
		}
	}
	return tool
}

// relPath is p relative to cwd when p is inside it, else p's base name.
func relPath(p, cwd string) string {
	if cwd != "" {
		if r, err := filepath.Rel(cwd, p); err == nil && r != ".." && !strings.HasPrefix(r, "../") {
			return r
		}
	}
	return filepath.Base(p)
}
