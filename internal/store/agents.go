package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Agent kinds.
const (
	AgentLead     = "lead"     // a session's main loop; AgentID is ""
	AgentSubagent = "subagent" // spawned with the Agent tool
	AgentTeammate = "teammate"
)

// Agent is a row of the agents table: one loop inside a session. A plain
// session is one lead row with an empty AgentID.
type Agent struct {
	SessionID     string // Claude Code's session id (the mod's $.session.id())
	AgentID       string // "" for the lead
	Kind          string // AgentLead, AgentSubagent, AgentTeammate
	AgentType     string
	Background    bool
	Model         string
	ParentAgentID string
	WorktreePath  string
	State         string
	LastAction    string // a short label, never a prompt or tool output
	// Tokens is the loop's latest model response's size (input, cache
	// read, cache write and output tokens), as Claude Code's agent list
	// counts it: a subagent's from agent_list, the main loop's from its
	// tool and turn_complete events (migration 0012 says the main loop's
	// row keeps 0: newer mods report it). 0 when not reported.
	Tokens    int64
	StartedAt time.Time
	EndedAt   time.Time
}

const agentColumns = `session_id, agent_id, kind, agent_type, background, model,
	parent_agent_id, worktree_path, state, last_action, tokens, started_at, ended_at`

func scanAgent(sc scanner) (Agent, error) {
	var a Agent
	err := sc.Scan(&a.SessionID, &a.AgentID, &a.Kind, &a.AgentType, &a.Background, &a.Model,
		&a.ParentAgentID, &a.WorktreePath, &a.State, &a.LastAction, &a.Tokens, msTime{&a.StartedAt}, msTime{&a.EndedAt})
	return a, err
}

// UpsertAgents writes each agent, keyed on (SessionID, AgentID), in one
// transaction. Every field is written as given.
func (s *Store) UpsertAgents(ctx context.Context, agents ...Agent) error {
	return s.writeTx(ctx, func(tx *sql.Tx) error {
		for _, a := range agents {
			if a.SessionID == "" {
				return errors.New("store: upsert agent: empty session id")
			}
			_, err := tx.ExecContext(ctx, `
				INSERT INTO agents (`+agentColumns+`)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT (session_id, agent_id) DO UPDATE SET
					kind = excluded.kind,
					agent_type = excluded.agent_type,
					background = excluded.background,
					model = excluded.model,
					parent_agent_id = excluded.parent_agent_id,
					worktree_path = excluded.worktree_path,
					state = excluded.state,
					last_action = excluded.last_action,
					tokens = excluded.tokens,
					started_at = excluded.started_at,
					ended_at = excluded.ended_at`,
				a.SessionID, a.AgentID, a.Kind, a.AgentType, a.Background, a.Model,
				a.ParentAgentID, a.WorktreePath, a.State, a.LastAction, a.Tokens, ms(a.StartedAt), ms(a.EndedAt))
			if err != nil {
				return fmt.Errorf("store: upsert agent %s/%s: %w", a.SessionID, a.AgentID, err)
			}
		}
		return nil
	})
}

// ListAgents returns the agents of one session, or of every session when
// sessionID is empty, the lead first, then by start time.
func (s *Store) ListAgents(ctx context.Context, sessionID string) ([]Agent, error) {
	q := `SELECT ` + agentColumns + ` FROM agents`
	var args []any
	if sessionID != "" {
		q += ` WHERE session_id = ?`
		args = append(args, sessionID)
	}
	q += ` ORDER BY session_id, agent_id <> '', coalesce(started_at, 0), agent_id`
	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
