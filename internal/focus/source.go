package focus

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gravexa-llc/ai-manager/internal/sessions"
	"github.com/gravexa-llc/ai-manager/internal/store"
)

// StoreReader is the part of *store.Store StoreSource reads.
type StoreReader interface {
	ListSessions(ctx context.Context, f store.SessionFilter) ([]store.Session, error)
	ListAgents(ctx context.Context, sessionID string) ([]store.Agent, error)
	ListWorktrees(ctx context.Context, f store.WorktreeFilter) ([]store.Worktree, error)
}

var _ StoreReader = (*store.Store)(nil)

// StoreSource resolves keys against aimd's store: the live session rows,
// and for subagents the agents the mod reported or, without the mod, the
// worktree locks Claude Code writes for worktree-isolated agents (they
// name the lead's pid).
type StoreSource struct {
	Store StoreReader
}

// TargetOf converts a store row.
func TargetOf(r store.Session) Target {
	return Target{
		Key:            r.Key,
		SessionID:      r.SessionID,
		PrevSessionID:  r.PrevSessionID,
		JobID:          r.JobID,
		PID:            r.PID,
		Kind:           r.Kind,
		Name:           r.Name,
		Cwd:            r.Cwd,
		ItermSessionID: r.ItermSessionID,
		Headless:       r.Headless,
	}
}

// Targets implements Source: the live session rows.
func (s StoreSource) Targets(ctx context.Context) ([]Target, error) {
	rows, err := s.Store.ListSessions(ctx, store.SessionFilter{})
	if err != nil {
		return nil, err
	}
	out := make([]Target, 0, len(rows))
	for _, r := range rows {
		out = append(out, TargetOf(r))
	}
	return out, nil
}

// LeadOf implements Source.
func (s StoreSource) LeadOf(ctx context.Context, agentID string) (Lead, error) {
	agents, err := s.Store.ListAgents(ctx, "")
	if err != nil {
		return Lead{}, err
	}
	// The live one, else the latest started.
	better := func(a, b *store.Agent) bool {
		if a.EndedAt.IsZero() != b.EndedAt.IsZero() {
			return a.EndedAt.IsZero()
		}
		return a.StartedAt.After(b.StartedAt)
	}
	var best *store.Agent
	for i := range agents {
		if a := &agents[i]; a.AgentID == agentID && (best == nil || better(a, best)) {
			best = a
		}
	}
	if best != nil {
		return Lead{SessionID: best.SessionID}, nil
	}
	wts, err := s.Store.ListWorktrees(ctx, store.WorktreeFilter{})
	if err != nil {
		return Lead{}, err
	}
	for _, w := range wts {
		if w.LockAgentID == agentID && w.LockPID > 0 {
			return Lead{PID: w.LockPID}, nil
		}
	}
	return Lead{}, fmt.Errorf("%w: aimd knows no agent %s (the mod reports agents; a worktree-isolated one shows while its worktree is locked)", ErrUnknownAgent, agentID)
}

// Lister lists sessions (sessions.Lister).
type Lister interface {
	List(ctx context.Context) (sessions.Snapshot, error)
}

var _ Lister = sessions.Lister{}

// ListerSource resolves keys against `claude agents --json`, for `aim go`
// when aimd isn't running. It knows no subagents.
type ListerSource struct {
	Lister Lister
	// TempDir marks sessions working under it as headless `claude -p`
	// runs; empty means os.TempDir().
	TempDir string
}

// Targets implements Source: the active sessions in one listing.
func (s ListerSource) Targets(ctx context.Context) ([]Target, error) {
	snap, err := s.Lister.List(ctx)
	if err != nil {
		return nil, err
	}
	tmp := s.TempDir
	if tmp == "" {
		tmp = os.TempDir()
	}
	tmp = filepath.Clean(tmp)
	var out []Target
	for _, ss := range snap.Sessions {
		if !ss.Active() {
			continue
		}
		cwd := filepath.Clean(ss.Cwd)
		out = append(out, Target{
			Key:       string(ss.Key()),
			SessionID: ss.SessionID,
			JobID:     ss.JobID,
			PID:       ss.PID,
			Kind:      string(ss.Kind),
			Name:      ss.Name,
			Cwd:       ss.Cwd,
			Headless:  ss.Kind != sessions.Background && ss.Cwd != "" && (cwd == tmp || strings.HasPrefix(cwd, tmp+"/")),
		})
	}
	return out, nil
}

// LeadOf implements Source: without aimd, no agent is known.
func (ListerSource) LeadOf(_ context.Context, agentID string) (Lead, error) {
	return Lead{}, fmt.Errorf("%w: %s; only aimd knows which session an agent runs in, and it isn't running", ErrUnknownAgent, agentID)
}
