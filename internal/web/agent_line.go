package web

// An agent's line, as Claude Code's agent list shows it: who, what it is
// doing, how long it has run and its tokens ("general-purpose  Bash: Run
// the tests  19m 30s · 284.1k tokens"). A package's pane on the plan page
// (pkgAgentOf) and the Sessions page's detail (a session's main loop, its
// subagents, and the ones that finished) draw the same line: agentLine or
// endedLine builds it, the "agent-now" template (templates/app/agent.html,
// which parseApp gives every page) draws it, and app-shell.css's
// .agent-now rules style it, so the two pages can't drift apart.

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gravexa-llc/ai-manager/internal/events"
)

// agentNow is an agent's line: who (its type, or its name), what it did
// last (the mod's last-action label: Claude Code's own activity sentence
// isn't readable), how long it has run, and its token count. Each part is
// empty when aimd doesn't know it.
type agentNow struct {
	Who, Doing string
	// Ended is how a finished agent ended ("completed", "failed",
	// "killed"), said in place of what it did; empty while it runs.
	Ended string
	// Ran is how long it has run ("19m 30s"), or ran when it has ended,
	// and RanTitle when it started (and ended).
	Ran, RanTitle string
	// Tokens is its token count ("284.1k tokens"), TokensTitle the exact
	// count and what it counts.
	Tokens, TokensTitle string
}

// tokensWhat says what an agent's token count counts, for its title.
const tokensWhat = "its latest model response's input, cache and output tokens, about the size of its context now, " +
	"as Claude Code's agent list counts it (not a running total)"

// agentLine is a live agent's line: who it is, what it did last, since
// when it runs (zero when unknown) and its latest response's size in
// tokens (0 when unknown).
func agentLine(now time.Time, who, doing string, started time.Time, tokens int64) *agentNow {
	n := &agentNow{Who: who, Doing: doing}
	if !started.IsZero() {
		n.Ran, n.RanTitle = runFor(now.Sub(started)), "running since "+stamp(now, started)
	}
	n.setTokens(tokens)
	return n
}

// endedLine is a finished agent's line: who it is, how it ended (its final
// state, or "ended" when its row has none), how long it ran when both ends
// are known, and its last token count.
func endedLine(now time.Time, who, state string, started, ended time.Time, tokens int64) *agentNow {
	n := &agentNow{Who: who, Ended: "ended"}
	if events.AgentEnded(state) {
		n.Ended = state
	}
	if !started.IsZero() && !ended.IsZero() {
		n.Ran = runFor(ended.Sub(started))
		n.RanTitle = "ran from " + stamp(now, started) + " to " + stamp(now, ended)
	}
	n.setTokens(tokens)
	return n
}

func (n *agentNow) setTokens(tokens int64) {
	if tokens > 0 {
		n.Tokens, n.TokensTitle = tokenCount(tokens)+" tokens", groupDigits(tokens)+" tokens: "+tokensWhat
	}
}

// runFor is how long an agent has run, as Claude Code's agent list says
// it: "45s", "19m 30s", "2h 5m", "3d 4h".
func runFor(d time.Duration) string {
	d = max(0, d)
	s := int64(d / time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", s)
	case d < time.Hour:
		return fmt.Sprintf("%dm %ds", s/60, s%60)
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", s/3600, s%3600/60)
	}
	return fmt.Sprintf("%dd %dh", s/86400, s%86400/3600)
}

// tokenCount is a token count in the agent list's short form: "950",
// "19k", "284.1k", "1.2m".
func tokenCount(n int64) string {
	oneDecimal := func(f float64) string { return strings.TrimSuffix(strconv.FormatFloat(f, 'f', 1, 64), ".0") }
	switch {
	case n < 1000:
		return strconv.FormatInt(n, 10)
	case n < 999_950: // rounds below "1000k"
		return oneDecimal(float64(n)/1e3) + "k"
	}
	return oneDecimal(float64(n)/1e6) + "m"
}

// groupDigits writes n with thousands separators: "284,123".
func groupDigits(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0 && s[i-1] != '-'; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
