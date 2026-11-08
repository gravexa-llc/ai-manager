-- 0013_unnamed_agents: end the subagent rows that were never agents. The
-- conventions are 0001's.
--
-- Up to this version aimd gave every agent id an event carried a row.
-- Claude Code's own forks of an agent (compaction, memory) carry an id no
-- agent list names, and run in the agent's cwd; a fork's tool attempt is
-- refused but still fires tool.call. So each such fork got a subagent row
-- in the agent's worktree, state running, that nothing ever ended, and it
-- kept the agent's finished package "working" (and "collision: 2 live
-- agents") for as long as the session lived. aimd now writes a row only
-- for a loop something named as an agent (internal/live's loop.named);
-- this ends the rows written before that.
--
-- Such a row has no agent_type (no agent list ever listed it) and no
-- worktree of its own (a worktree-isolated agent's is .../agent-<its id>,
-- from worktree_create). It ends as completed, at its first event.
UPDATE agents
SET state    = 'completed',
    ended_at = coalesce(started_at, CAST(strftime('%s', 'now') AS INTEGER) * 1000)
WHERE kind = 'subagent'
  AND agent_type = ''
  AND ended_at IS NULL
  AND state NOT IN ('completed', 'failed', 'killed')
  AND substr(worktree_path, -length('/agent-' || agent_id)) <> ('/agent-' || agent_id) COLLATE NOCASE;
