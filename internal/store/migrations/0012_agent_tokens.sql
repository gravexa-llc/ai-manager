-- 0012_agent_tokens: a subagent's token count. The conventions are 0001's.
--
-- tokens is what Claude Code's agent list shows as "↓ N tokens": the size
-- of the agent's latest model response, its input, cache read, cache
-- write and output tokens added up. It is not a running total: each
-- response replaces it. The mod sends it on agent_list (internal/events'
-- Agent.Tokens); 0 means not reported (an older mod, or no response yet).
-- The main loop's row keeps 0.
ALTER TABLE agents ADD COLUMN tokens INTEGER NOT NULL DEFAULT 0;
