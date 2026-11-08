package api

// GoRequest is the JSON body of POST /api/go. A dashboard form sends the
// same key as a form field instead (key, csrf_token, return).
type GoRequest struct {
	// Key names the session to go to: a session key (pid:123, job:ab12cd34,
	// session:<id>), an agent link's ref (<session id>/<agent id>), an
	// agent id (agent:<id>, agent-<id> or the bare id), a pid, a job id, a
	// session id, or a unique prefix of a session's name, cwd or cwd's last
	// folder.
	Key string `json:"key"`
}

// Go actions.
const (
	// GoFocused: an interactive session's tab was selected and its app
	// (iTerm2 or Terminal) activated.
	GoFocused = "focused"
	// GoAttached: a background session was opened with `claude attach
	// <id>` in a new iTerm2 tab, or a new Terminal window.
	GoAttached = "attached"
)

// GoResult is the answer to POST /api/go.
type GoResult struct {
	// Key is the session acted on (its store key). For a subagent it is
	// the lead's.
	Key string `json:"key"`
	// Title names that session: its name, or its cwd's last folder.
	Title string `json:"title"`
	// Action is GoFocused or GoAttached.
	Action string `json:"action"`
	// App is where: "iTerm2" or "Terminal".
	App string `json:"app,omitempty"`
	// Via is how a focused session's tab was found: "tty" or
	// "iterm_session_id".
	Via string `json:"via,omitempty"`
	// TTY is a focused session's terminal, when ps gave one.
	TTY string `json:"tty,omitempty"`
	// Command is what an attached session's new tab runs.
	Command string `json:"command,omitempty"`
	// AgentID is set when the key named a subagent: Go can't select a
	// subagent inside its lead (that needs the mod's $.ui.view), so it
	// went to the lead session instead.
	AgentID string `json:"agent_id,omitempty"`
	// Message says what happened, for people.
	Message string `json:"message"`
}

// GoError is the body of a refused POST /api/go: {"error": ...}, plus the
// matching sessions when the key was ambiguous (409).
type GoError struct {
	Error      string        `json:"error"`
	Candidates []GoCandidate `json:"candidates,omitempty"`
}

// GoCandidate is one session an ambiguous key matched.
type GoCandidate struct {
	Key   string `json:"key"`
	Title string `json:"title"`
	Kind  string `json:"kind,omitempty"`
	Cwd   string `json:"cwd,omitempty"`
}
