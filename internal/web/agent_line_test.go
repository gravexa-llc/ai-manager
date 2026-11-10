package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/store"
)

// renderLine draws an agent's line with a page's templates.
func renderLine(t *testing.T, page string, n *agentNow) string {
	t.Helper()
	var buf strings.Builder
	must(t, parseApp(page).ExecuteTemplate(&buf, "agent-now", n))
	return buf.String()
}

// The shared line: how long it has run and its tokens when known, "no
// action reported yet" before its first action, how a finished one ended
// and how long it ran. Every page draws it the same.
func TestAgentLine(t *testing.T) {
	started := t0.Add(-(9*time.Minute + 38*time.Second))
	n := agentLine(t0, "general-purpose", "Bash: Run the tests", started, 335_912)
	if n.Who != "general-purpose" || n.Doing != "Bash: Run the tests" || n.Ran != "9m 38s" || n.Tokens != "335.9k tokens" || n.Ended != "" {
		t.Errorf("line = %+v", n)
	}
	if !strings.HasPrefix(n.RanTitle, "running since 2026-11-05T11:50:22") || !strings.HasPrefix(n.TokensTitle, "335,912 tokens: its latest model response") {
		t.Errorf("titles = %q, %q", n.RanTitle, n.TokensTitle)
	}
	line := renderLine(t, "sessions.html", n)
	wantIn(t, "line", line, `<span class="agent-now"><strong class="agent-now-who">general-purpose</strong>`,
		` <span class="agent-now-doing" title="its last action, as the plugin reported it">Bash: Run the tests</span>`,
		`<span class="agent-now-stat"><span title="running since `, `">9m 38s</span> · <span title="335,912 tokens: `, `">335.9k tokens</span></span></span>`)
	checkNoInline(t, "line", line)
	// The plan page draws the very same line.
	if plan := renderLine(t, "plan.html", n); plan != line {
		t.Errorf("the plan page's line differs:\n%s\nfrom the Sessions page's:\n%s", plan, line)
	}

	// Nothing known yet: no action, no start, no tokens.
	bare := agentLine(t0, "Explore", "", time.Time{}, 0)
	if bare.Ran != "" || bare.Tokens != "" || bare.RanTitle != "" || bare.TokensTitle != "" {
		t.Errorf("bare line = %+v", bare)
	}
	line = renderLine(t, "sessions.html", bare)
	wantIn(t, "bare line", line, `<strong class="agent-now-who">Explore</strong> <span class="agent-now-doing agent-now-quiet">no action reported yet</span></span>`)
	wantNotIn(t, "bare line", line, "agent-now-stat")
	// The elapsed alone, or the tokens alone.
	wantIn(t, "elapsed alone", renderLine(t, "sessions.html", agentLine(t0, "main", "", t0.Add(-45*time.Second), 0)),
		`<span class="agent-now-stat"><span title="running since 2026-11-05T11:59:15Z">45s</span></span>`)
	wantIn(t, "tokens alone", renderLine(t, "sessions.html", agentLine(t0, "main", "", time.Time{}, 950)),
		`<span class="agent-now-stat"><span title="950 tokens: `, `">950 tokens</span></span>`)

	// Finished: how it ended in place of what it did, how long it ran, its
	// last count.
	done := endedLine(t0, "general-purpose", "completed", t0.Add(-time.Hour), t0.Add(-40*time.Minute), 52_000)
	if done.Ended != "completed" || done.Ran != "20m 0s" || done.Tokens != "52k tokens" ||
		done.RanTitle != "ran from 2026-11-05T11:00:00Z to 2026-11-05T11:20:00Z" {
		t.Errorf("finished line = %+v", done)
	}
	line = renderLine(t, "sessions.html", done)
	wantIn(t, "finished line", line, `<strong class="agent-now-who">general-purpose</strong> <span class="agent-now-doing agent-now-quiet">completed</span>`,
		`>20m 0s</span> · <span title="52,000 tokens: `)
	// A row with no final state, and no end time: "ended", no run time.
	if odd := endedLine(t0, "agent a4444444…", "running", t0.Add(-time.Hour), time.Time{}, 0); odd.Ended != "ended" || odd.Ran != "" || odd.Tokens != "" {
		t.Errorf("odd finished line = %+v", odd)
	}
}

// The Sessions page's detail shows the line a package's pane does: a
// subagent's own, under its title, in place of a Last action row; a lead's
// main loop's (with its tokens, from its agent row), then one per live
// subagent linking to its detail, and the finished ones folded.
func TestAppSessionsAgentLines(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	must(t, st.UpsertSessions(ctx, store.Session{Key: "pid:21", SessionID: "lead-session", PID: 21, Kind: "interactive", Name: "app lead",
		Cwd: appRepo, Repo: appRepo, WorktreePath: appRepo, Branch: "main", State: "working",
		StartedAt: t0.Add(-(2*time.Hour + 5*time.Minute)), LastSeenAt: t0}))
	// The mod's report, as aimd's receiver writes it (internal/live).
	must(t, st.UpdateSessionLive(ctx, "pid:21", store.SessionLive{State: "working", LastAction: "Agent: Write the docs", LastEventAt: t0,
		Cwd: appRepo, Repo: appRepo, WorktreePath: appRepo, Branch: "main"}))
	must(t, st.UpsertAgents(ctx,
		store.Agent{SessionID: "lead-session", Kind: store.AgentLead, State: "running", LastAction: "Agent: Write the docs",
			Tokens: 61_234, StartedAt: t0.Add(-(2*time.Hour + 5*time.Minute))},
		store.Agent{SessionID: "lead-session", AgentID: "a1111111111111111", Kind: store.AgentSubagent, AgentType: "general-purpose",
			State: "running", LastAction: "Bash: Run the tests", Tokens: 335_912, StartedAt: t0.Add(-(9*time.Minute + 38*time.Second))},
		store.Agent{SessionID: "lead-session", AgentID: "a2222222222222222", Kind: store.AgentSubagent, AgentType: "Explore",
			State: "running", StartedAt: t0.Add(-45 * time.Second)},
		store.Agent{SessionID: "lead-session", AgentID: "a3333333333333333", Kind: store.AgentSubagent, AgentType: "general-purpose",
			State: "completed", LastAction: "Edit docs/guide.md", Tokens: 52_000, StartedAt: t0.Add(-time.Hour), EndedAt: t0.Add(-40 * time.Minute)},
	))
	h := newDash(t, st, nil).routes()

	// A subagent: its line under its title; Started is the clock time, as
	// the line says how long it has run.
	sub := get(t, h, "/app/sessions?s=pid%3A21%2Fa1111111111111111&fragment=detail")
	wantIn(t, "subagent", sub, `<p class="subtitle">Subagent of app lead</p>
<div class="sd-now"><span class="agent-now"><strong class="agent-now-who">general-purpose</strong> <span class="agent-now-doing" title="its last action, as the plugin reported it">Bash: Run the tests</span>`,
		`">9m 38s</span> · <span title="335,912 tokens: `, `">335.9k tokens</span>`, "<dt>Started</dt><dd>11:50</dd>")
	wantNotIn(t, "subagent", sub, "Last action", "<div class=\"label\">Agents</div>", "Finished (")
	checkNoInline(t, "subagent", sub)
	// One that hasn't acted yet.
	wantIn(t, "new subagent", get(t, h, "/app/sessions?s=pid%3A21%2Fa2222222222222222&fragment=detail"),
		`<strong class="agent-now-who">Explore</strong> <span class="agent-now-doing agent-now-quiet">no action reported yet</span> <span class="agent-now-stat"><span title="running since 2026-11-05T11:59:15Z">45s</span></span>`)

	// The lead: its own line, with the main loop's tokens; then its live
	// subagents, each linking to its detail; then the finished one, folded.
	lead := get(t, h, "/app/sessions?s=pid%3A21&fragment=detail")
	wantIn(t, "lead", lead,
		`<div class="sd-now"><span class="agent-now"><strong class="agent-now-who">main</strong> <span class="agent-now-doing" title="its last action, as the plugin reported it">Agent: Write the docs</span>`,
		`">2h 5m</span> · <span title="61,234 tokens: `, `">61.2k tokens</span>`, "<dt>Started</dt><dd>09:55</dd>",
		`<div class="label">Agents</div>`,
		`<li><a class="sd-agent" href="/app/sessions?s=pid%3A21%2Fa1111111111111111" data-pane="detail" title="Show this subagent"><span class="agent-now"><strong class="agent-now-who">general-purpose</strong>`,
		`<li><a class="sd-agent" href="/app/sessions?s=pid%3A21%2Fa2222222222222222" data-pane="detail" title="Show this subagent"><span class="agent-now"><strong class="agent-now-who">Explore</strong>`,
		`<details id="sd-finished" class="sd-finished">
<summary class="label">Finished (1)</summary>`,
		`<li class="sd-agent"><span class="agent-now"><strong class="agent-now-who">general-purpose</strong> <span class="agent-now-doing agent-now-quiet">completed</span> <span class="agent-now-stat"><span title="ran from 2026-11-05T11:00:00Z to 2026-11-05T11:20:00Z">20m 0s</span> · <span title="52,000 tokens: `)
	wantNotIn(t, "lead", lead, "Last action", "And 0 earlier", "Edit docs/guide.md")
	if i, j, k := strings.Index(lead, "a1111111111111111"), strings.Index(lead, "a2222222222222222"), strings.Index(lead, "Finished ("); i < 0 || j < i || k < j {
		t.Error("the lead's agents aren't in order: live ones by start, then the finished")
	}
	checkNoInline(t, "lead", lead)
	// The whole page renders the same detail.
	wantIn(t, "page", section(t, get(t, h, "/app/sessions?s=pid%3A21"), "detail"), `<strong class="agent-now-who">main</strong>`, "Finished (1)")

	// A finished subagent's detail is gone, as before.
	wantIn(t, "finished", get(t, h, "/app/sessions?s=pid%3A21%2Fa3333333333333333&fragment=detail"), "That subagent has finished.")
}

// A lead lists its latest finished subagents only, and says how many
// earlier ones it leaves out.
func TestAppSessionsFinishedCap(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	must(t, st.UpsertSessions(ctx, store.Session{Key: "pid:21", SessionID: "lead-session", PID: 21, Kind: "interactive", Name: "app lead",
		Cwd: appRepo, State: "idle", StartedAt: t0.Add(-3 * time.Hour), LastSeenAt: t0}))
	var rows []store.Agent
	for i := range maxFinished + 3 {
		at := t0.Add(-time.Duration(i+1) * time.Minute)
		rows = append(rows, store.Agent{SessionID: "lead-session", AgentID: "a5" + strings.Repeat(string(rune('a'+i)), 15), Kind: store.AgentSubagent,
			AgentType: "Explore", State: "completed", StartedAt: at.Add(-30 * time.Second), EndedAt: at})
	}
	must(t, st.UpsertAgents(ctx, rows...))
	lead := get(t, newDash(t, st, nil).routes(), "/app/sessions?s=pid%3A21&fragment=detail")
	wantIn(t, "lead", lead, "Finished (23)", `<p class="hint">And 3 earlier.</p>`, `title="ran from 2026-11-05T11:58:30Z to 2026-11-05T11:59:00Z">30s</span>`)
	wantNotIn(t, "lead", lead, `<div class="label">Agents</div>`)
	if n := strings.Count(lead, `<li class="sd-agent">`); n != maxFinished {
		t.Errorf("%d finished lines, want %d", n, maxFinished)
	}
	// The newest first: the one that ended a minute ago leads.
	if i, j := strings.Index(lead, "11:59:00Z"), strings.Index(lead, "11:58:00Z"); i < 0 || j < i {
		t.Error("the finished agents aren't latest first")
	}
}
