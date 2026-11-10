// The mod's reporting: each event kind from the recorded shapes
// (testdata/events/interactive-*.jsonl, background-*.jsonl), the
// batching, aimd down or slow, and every reporting hook failing open.

import type { AgentInfo, On, TurnStepResult, TurnUsage } from 'claude-code'
import { expect, mock, test, type Engine } from 'claude-code/testing'

import { CONTEXT, REPO, SESSION_ID, SPAWN, SPAWNED, bashResult } from './fixtures'
import { OPTIONS, START, TOKEN, world, type Reply, type Routes } from './testkit'

type World = ReturnType<typeof world>
type Ev = Record<string, unknown> & { kind: string; session_id: string; at: string }

/** The session ids R1's interactive run had before and after /clear. */
const BEFORE_CLEAR = '55f811f4-a146-450f-b8a3-91f1fe9753bb'
const AFTER_CLEAR = '7b5451e8-a02f-44dd-b8a1-6f68ed1a940f'
/** The background subagent that asked for a permission in that run. */
const SUB = 'a6cabd35f43276105'

/** AskUserQuestion's input as recorded (interactive-dialogs-clear.jsonl, seq 49). */
const QUESTION = {
  questions: [
    {
      question: 'Do you prefer red or blue?',
      header: 'Color',
      multiSelect: false,
      options: [
        { label: 'Red', description: 'Choose red as your preferred color' },
        { label: 'Blue', description: 'Choose blue as your preferred color' },
      ],
    },
  ],
}
/** ExitPlanMode's input (seq 104): the engine adds the plan and its file; the plan here is a stand-in. */
const PLAN = { plan: '# Plan\n\n1. Create plan.txt\n2. Put ok in it\n', planFilePath: '~/.claude/plans/make-a-one-line-plan.md' }

/** What a test's world answers beneath the plugin, with switches the test flips. */
type Below = { verdict: 'allow' | 'ask'; agents: AgentInfo[]; usage: TurnUsage | null }

function reporting(on: On, routes: Routes = {}, env: Record<string, string> = {}) {
  const w = world(on, { 'GET /api/context': { status: 200, body: CONTEXT }, 'POST /events': { status: 200, body: {} }, ...routes }, REPO, SESSION_ID, env)
  const below: Below = { verdict: 'allow', agents: [], usage: null }
  on('session.start', (_, e) => ({ cwd: e.cwd }))
  on('session.end', (_, e) => ({ sessionId: e.sessionId }))
  on('prompt.submit', (_, e) => ({ text: e.text, origin: e.origin }))
  on('turn.start', (_, e) => ({ turnId: e.turnId }))
  on('turn.complete', (_, e) => ({ text: e.answer }))
  on('turn.step', async function* (_, e) {
    yield { kind: 'text', index: 0, text: '[assistant text]' }
    return { turnId: e.turnId, index: e.index, answer: '[assistant text]', toolUses: [], stopReason: 'end_turn', usage: below.usage }
  })
  on('tool.check', () => ({ decision: below.verdict }))
  on('classic.PermissionRequest', () => ({ decision: { behavior: 'allow' } }))
  // No settings hook runs beneath: the classic events answer nothing.
  on('classic.SessionStart', () => ({}))
  on('classic.Notification', () => ({}))
  on('classic.StopFailure', () => ({}))
  on('classic.WorktreeCreate', () => ({}))
  on('classic.SubagentStart', () => ({}))
  on('agent.list', () => ({ value: below.agents }))
  on('agent.spawn', () => SPAWNED)
  on('tool.call', { tool: 'Bash' }, () => bashResult(''))
  on('tool.call', { tool: 'Edit' }, () => ({ result: { filePath: `${REPO}/internal/x.go` }, text: 'ok' }))
  on('tool.call', { tool: 'AskUserQuestion' }, () => ({ result: { answers: {} }, text: '[tool output]' }))
  on('tool.call', { tool: 'ExitPlanMode' }, () => ({ result: {}, text: '[tool output]' }))
  return { ...w, below }
}

/** Every event POSTed to /events so far, in order. */
function posted(w: World): Ev[] {
  return w.sent('POST /events').flatMap(r => (r.body?.events as Ev[] | undefined) ?? [])
}

function ofKind(w: World, kind: string): Ev[] {
  return posted(w).filter(e => e.kind === kind)
}

/** Settles `work` with the mocked clock moved on by at most `ms`; fails if it is still pending. */
async function within<T>(w: World, ms: number, work: Promise<T>): Promise<T> {
  let done = false
  const tracked = work.then(value => {
    done = true
    return value
  })
  await w.clock.settle()
  if (!done && ms > 0) await w.clock.advance(ms)
  expect(done, `still waiting after ${ms} ms`).toBe(true)
  return tracked
}

/** One model request in a loop (no agentId: the main loop), read to its end as the engine reads it. */
async function step($: Engine, agentId?: string): Promise<{ chunks: unknown[]; result: TurnStepResult }> {
  return drain($.turn.step({ turnId: 't1', index: 0, model: 'claude-haiku-5-5', messageCount: 3, ...(agentId !== undefined ? { agentId } : {}) }))
}

/** A stream's chunks and the value it returns, read to its end. */
async function drain<C, R>(stream: AsyncGenerator<C, R>): Promise<{ chunks: C[]; result: R }> {
  const chunks: C[] = []
  for (;;) {
    const r = await stream.next()
    if (r.done === true) return { chunks, result: r.value }
    chunks.push(r.value)
  }
}

/** A response's usage, as turn.step's result carries it. */
const usage = (input: number, read: number, write: number, output: number): TurnUsage => ({
  input_tokens: input,
  cache_read_input_tokens: read,
  cache_creation_input_tokens: write,
  output_tokens: output,
  model: 'claude-haiku-5-5',
})

/** Starts the session as the engine does: classic.SessionStart, then session.start. */
async function begin($: Engine, w: World, id = SESSION_ID): Promise<void> {
  await $.classic.SessionStart({ session_id: id, source: 'startup' })
  await $.session.start(START)
  await w.clock.settle()
}

const ISO = /^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$/

// --- each event kind ----------------------------------------------------------

test('session_start carries the facts; events wait for the batch and go with the token', { options: OPTIONS }, async ($, on) => {
  const w = reporting(on)
  await begin($, w)
  expect(posted(w)).toEqual([])
  await w.clock.advance(1000)
  const posts = w.sent('POST /events')
  expect(posts.length).toBe(1)
  expect(posts[0]?.headers['X-AIM-Token']).toBe(TOKEN)
  expect(posts[0]?.headers['Content-Type']).toBe('application/json')
  const [start] = ofKind(w, 'session_start')
  expect(start).toMatchObject({
    session_id: SESSION_ID,
    cwd: REPO,
    repo: REPO,
    worktree: REPO,
    branch: 'main',
    host: 'laptop',
    interactive: true,
    source: 'startup',
  })
  expect(start?.at).toMatch(ISO)
  expect(start?.background).toBeUndefined()
  expect(start?.iterm_session_id).toBeUndefined()
})

test('a background session says so (it says interactive too, R1); ITERM_SESSION_ID rides along', { options: OPTIONS }, async ($, on) => {
  const w = reporting(on, {}, { CLAUDE_CODE_SESSION_KIND: 'bg', ITERM_SESSION_ID: 'w0t1p0:ABC' })
  await begin($, w)
  await w.clock.advance(1000)
  expect(ofKind(w, 'session_start')[0]).toMatchObject({ interactive: true, background: true, iterm_session_id: 'w0t1p0:ABC' })
})

test('a permission dialog goes at once, with the command it waits on; the call then closes it', { options: OPTIONS }, async ($, on) => {
  const w = reporting(on)
  await begin($, w)
  w.below.verdict = 'ask'
  const ran = await within(w, 0, $.tool.call({ tool: 'Bash', command: 'mkdir perm-main', description: 'Create directory perm-main' }))
  expect(ran.text).toBe('')
  // Sent before any batch timer: needs_you doesn't wait. Only the dialog
  // (classic.PermissionRequest) reports it: tool.check's ask is the mode's
  // decider's first, and in auto mode its classifier answers it unseen.
  const asks = ofKind(w, 'needs_you')
  expect(asks.map(a => a.source)).toEqual(['permission_request'])
  expect(asks[0]).toMatchObject({
    session_id: SESSION_ID,
    reason: 'Bash: Create directory perm-main',
    wait: { kind: 'permission', tool: 'Bash', summary: 'mkdir perm-main' },
  })
  expect(asks[0]?.agent_id).toBeUndefined()
  const id = asks[0]?.tool_use_id
  expect(typeof id).toBe('string')
  await w.clock.advance(1000)
  const [done] = ofKind(w, 'tool')
  expect(done).toMatchObject({ tool: 'Bash', label: 'Bash: Create directory perm-main', cwd: REPO, tool_use_id: id })
})

test('a call nobody is asked about sends no needs_you', { options: OPTIONS }, async ($, on) => {
  const w = reporting(on)
  await begin($, w)
  await within(w, 0, $.tool.call({ tool: 'Bash', command: 'make check', description: 'Run the checks' }))
  await within(w, 0, $.tool.call({ tool: 'Edit', file_path: `${REPO}/internal/x.go`, old_string: 'a', new_string: 'b' }))
  await w.clock.advance(1000)
  expect(ofKind(w, 'needs_you')).toEqual([])
  expect(ofKind(w, 'tool').map(e => e.tool)).toEqual(['Bash', 'Edit'])
})

test('an ask nobody but a host answers is not reported from tool.check', { options: OPTIONS }, async ($, on) => {
  const w = reporting(on)
  await begin($, w)
  await w.clock.advance(1000)
  const before = posted(w).length
  // A $.tool.check query: no call, no tool_use_id, nobody to ask.
  await $.tool.check({ tool: 'Bash', input: { command: 'rm -rf x' } })
  await w.clock.advance(1000)
  expect(posted(w).slice(before).filter(e => e.kind === 'needs_you')).toEqual([])
})

test('a question waits on its questions and options; a plan approval on the plan', { options: OPTIONS }, async ($, on) => {
  const w = reporting(on)
  await begin($, w)
  await within(w, 0, $.tool.call({ tool: 'AskUserQuestion', ...QUESTION }))
  const [question] = ofKind(w, 'needs_you')
  expect(question).toMatchObject({
    source: 'ask_user_question',
    reason: 'AskUserQuestion',
    wait: {
      kind: 'question',
      tool: 'AskUserQuestion',
      questions: [{ header: 'Color', question: 'Do you prefer red or blue?', options: ['Red', 'Blue'] }],
    },
  })
  await within(w, 0, $.tool.call({ tool: 'ExitPlanMode', ...PLAN }))
  const plan = ofKind(w, 'needs_you').find(e => e.source === 'exit_plan_mode')
  expect(plan?.wait).toEqual({ kind: 'plan', tool: 'ExitPlanMode', plan: '# Plan\n\n1. Create plan.txt\n2. Put ok in it' })
})

test("a subagent's ask names it; its Notification, which names no one, isn't sent while an ask is open", { options: OPTIONS }, async ($, on) => {
  const w = reporting(on)
  await begin($, w)
  await $.classic.PermissionRequest({
    tool_name: 'Bash',
    tool_input: { command: 'mkdir perm-sub && echo done', description: 'Create perm-sub directory' },
    agent_id: SUB,
    agent_type: 'general-purpose',
  })
  await w.clock.settle()
  expect(ofKind(w, 'needs_you')).toEqual([
    expect.objectContaining({
      agent_id: SUB,
      source: 'permission_request',
      reason: 'Bash: Create perm-sub directory',
      wait: { kind: 'permission', tool: 'Bash', summary: 'mkdir perm-sub && echo done' },
    }),
  ])
  await $.classic.Notification({ notification_type: 'permission_prompt', message: 'Claude needs your permission' })
  await w.clock.advance(1000)
  expect(ofKind(w, 'needs_you').length).toBe(1)

  // The subagent's turn moves on: a later Notification is a dialog the mod didn't see.
  await $.turn.complete({ agentId: SUB, answer: 'done', durationMs: 5, isAborted: false, turnId: 't1', reason: 'answer' })
  await $.classic.Notification({ notification_type: 'permission_prompt', message: 'Claude needs your permission' })
  await w.clock.settle()
  expect(ofKind(w, 'needs_you')[1]).toMatchObject({ source: 'notification', reason: 'permission_prompt' })
  expect(ofKind(w, 'needs_you')[1]?.agent_id).toBeUndefined()

  await $.classic.Notification({ notification_type: 'idle_prompt', message: 'Claude is waiting for your input' })
  await w.clock.advance(1000)
  expect(ofKind(w, 'idle').length).toBe(1)
})

test('/clear: session_end under the old id, then session_start under the new one, from classic.SessionStart', { options: OPTIONS }, async ($, on) => {
  const w = reporting(on)
  await begin($, w, BEFORE_CLEAR)
  await $.session.end({ reason: 'clear', sessionId: BEFORE_CLEAR, resume: { id: BEFORE_CLEAR } })
  await w.clock.settle()
  // session.start doesn't fire again after /clear.
  await $.classic.SessionStart({ session_id: AFTER_CLEAR, source: 'clear' })
  await $.prompt.submit({ text: 'Reply with the word hi.', wait: false, origin: { kind: 'composer' } })
  await w.clock.advance(1000)
  const evs = posted(w).map(e => [e.kind, e.session_id])
  expect(evs).toEqual([
    ['session_start', BEFORE_CLEAR],
    ['session_end', BEFORE_CLEAR],
    ['session_start', AFTER_CLEAR],
    ['prompt', AFTER_CLEAR],
  ])
  const [first, , second] = posted(w)
  expect(ofKind(w, 'session_end')[0]?.reason).toBe('clear')
  expect(second).toMatchObject({ source: 'clear', prev_session_id: BEFORE_CLEAR, cwd: REPO, repo: REPO, branch: 'main', interactive: true })
  expect(first?.prev_session_id).toBeUndefined()
})

test('prompts, turns and tools are summaries: no prompt text, the first line of the answer, a label', { options: OPTIONS }, async ($, on) => {
  const w = reporting(on)
  await begin($, w)
  const secret = 'Deploy with password hunter2 to prod'
  await $.prompt.submit({ text: secret, wait: false, origin: { kind: 'composer' } })
  await $.turn.start({ text: secret, turnId: 't1' })
  await $.tool.call({ tool: 'Edit', file_path: `${REPO}/internal/x.go`, old_string: 'a', new_string: 'b' })
  await $.turn.complete({ answer: `Done: the tests pass.\nHere is more: ${'x'.repeat(400)}`, durationMs: 9, isAborted: false, turnId: 't1', reason: 'answer' })
  await $.prompt.submit({ text: '<task-notification>…', wait: false, origin: { kind: 'task-notification' } })
  await w.clock.advance(1000)
  expect(JSON.stringify(posted(w))).not.toContain('hunter2')
  expect(JSON.stringify(posted(w))).not.toContain('Here is more')
  expect(ofKind(w, 'prompt').map(e => e.origin_kind)).toEqual(['composer', 'task-notification'])
  expect(ofKind(w, 'turn_start')[0]).toMatchObject({ cwd: REPO })
  expect(ofKind(w, 'tool')[0]).toMatchObject({ tool: 'Edit', label: 'Edit internal/x.go', cwd: REPO })
  expect(ofKind(w, 'turn_complete')[0]).toMatchObject({ reason: 'answer', last_said: 'Done: the tests pass.' })
  // Every event of the batch went in one POST, in order.
  expect(w.sent('POST /events').length).toBe(1)
})

test('a secret in a label or in the first line of the answer is redacted before it is sent', { options: OPTIONS }, async ($, on) => {
  const w = reporting(on)
  await begin($, w)
  const token = 'ghp_0123456789abcdefABCDEF0123456789abcd'
  await $.turn.start({ text: 'go', turnId: 't1' })
  w.below.verdict = 'ask'
  await within(w, 0, $.tool.call({ tool: 'Bash', command: `GITHUB_TOKEN=${token} git push`, description: `Export GITHUB_TOKEN=${token} and push` }))
  await $.turn.complete({
    answer: `Pushed to https://user:pw@example.com/acme/app.git with Bearer abcdefghijklmnop1234\n${token}`,
    durationMs: 9,
    isAborted: false,
    turnId: 't1',
    reason: 'answer',
  })
  await w.clock.advance(1000)
  const sent = JSON.stringify(posted(w))
  for (const secret of [token, 'user:pw@', 'abcdefghijklmnop1234']) expect(sent.includes(secret), secret).toBe(false)
  expect(ofKind(w, 'tool').map(e => e.label)).toEqual(['Bash: Export GITHUB_TOKEN=[redacted] and push'])
  expect(ofKind(w, 'needs_you')[0]).toMatchObject({
    reason: 'Bash: Export GITHUB_TOKEN=[redacted] and push',
    wait: { kind: 'permission', tool: 'Bash', summary: 'GITHUB_TOKEN=[redacted] git push' },
  })
  expect(ofKind(w, 'turn_complete')[0]).toMatchObject({
    reason: 'answer',
    last_said: 'Pushed to https://[redacted]@example.com/acme/app.git with Bearer [redacted]',
  })
})

test('agent.spawn: the flags, a snapshot, and one every 30 s while agents run', { options: OPTIONS }, async ($, on) => {
  const w = reporting(on)
  await begin($, w)
  w.below.agents = [{ id: SPAWNED.agentId, description: 'fg probe', type: 'general-purpose', status: 'running' }]
  await within(w, 1000, $.agent.spawn({ ...SPAWN, prompt: 'Look around.', description: 'look' }))
  await w.clock.advance(1000)
  expect(ofKind(w, 'agent_list')).toEqual([
    expect.objectContaining({ agents: [{ id: SPAWNED.agentId, status: 'running', type: 'general-purpose', background: true }] }),
  ])
  await w.clock.advance(30_000)
  expect(ofKind(w, 'agent_list').length).toBe(2)
  w.below.agents = [{ id: SPAWNED.agentId, description: 'fg probe', type: 'general-purpose', status: 'completed' }]
  await w.clock.advance(30_000)
  await w.clock.advance(30_000)
  await w.clock.advance(1000)
  const lists = ofKind(w, 'agent_list')
  expect(lists.length).toBe(3)
  expect(lists[2]).toMatchObject({ agents: [expect.objectContaining({ status: 'completed' })] })
})

test("turn.step: a subagent's latest response size rides on agent_list, as the CLI's agent list counts it; the main loop's doesn't", { options: OPTIONS }, async ($, on) => {
  const w = reporting(on)
  await begin($, w)
  w.below.agents = [{ id: SPAWNED.agentId, description: 'look', type: 'general-purpose', status: 'running' }]
  // Before any response: no count.
  await within(w, 1000, $.agent.spawn({ ...SPAWN, prompt: 'Look around.', description: 'look' }))
  // Two responses: the second's size replaces the first's (not a running total).
  w.below.usage = usage(2, 0, 3800, 700)
  const first = await within(w, 0, step($, SPAWNED.agentId))
  expect(first.chunks).toEqual([{ kind: 'text', index: 0, text: '[assistant text]' }])
  expect(first.result.usage).toEqual(w.below.usage)
  w.below.usage = usage(2, 3800, 21_000, 1500)
  await within(w, 0, step($, SPAWNED.agentId))
  // A step with no usage (it failed), and the main loop's, change nothing.
  w.below.usage = null
  await within(w, 0, step($, SPAWNED.agentId))
  w.below.usage = usage(9, 90_000, 0, 900)
  await within(w, 0, step($))
  await w.clock.advance(30_000)
  await w.clock.advance(1000)
  const lists = ofKind(w, 'agent_list')
  expect(lists.length).toBe(2)
  expect(lists[0]).toMatchObject({ agents: [{ id: SPAWNED.agentId, status: 'running', type: 'general-purpose', background: true }] })
  expect((lists[0]?.agents as Record<string, unknown>[])[0]?.tokens).toBeUndefined()
  expect(lists[1]).toMatchObject({ agents: [{ id: SPAWNED.agentId, tokens: 26_302 }] })
})

test("turn.step in the main loop: its latest response size rides on the main loop's tool and turn_complete events, never a subagent's; /clear forgets it", { options: OPTIONS }, async ($, on) => {
  const w = reporting(on)
  await begin($, w, BEFORE_CLEAR)
  // Before its first response: no count.
  await $.turn.start({ text: '[prompt]', turnId: 't1' })
  await $.tool.call({ tool: 'Bash', command: 'make check', description: 'Run the checks' })
  // Two responses: the latest stands, each on the events after it.
  w.below.usage = usage(9, 90_000, 0, 900)
  await within(w, 0, step($))
  await $.tool.call({ tool: 'Bash', command: 'make check', description: 'Run the checks' })
  w.below.usage = usage(3, 90_900, 1200, 400)
  await within(w, 0, step($))
  // A subagent's step and its tool call and turn's end don't touch it, nor carry it.
  w.below.usage = usage(2, 0, 3800, 700)
  await within(w, 0, step($, SUB))
  await $.tool.call({ tool: 'Bash', command: 'ls', description: 'List files', agentId: SUB })
  await $.turn.complete({ agentId: SUB, answer: 'done', durationMs: 5, isAborted: false, turnId: 't2', reason: 'answer' })
  // A step with no usage (it failed) changes nothing.
  w.below.usage = null
  await within(w, 0, step($))
  await $.turn.complete({ answer: 'Done.', durationMs: 9, isAborted: false, turnId: 't1', reason: 'answer' })
  await w.clock.advance(1000)
  expect(ofKind(w, 'tool').map(e => [e.agent_id, e.tokens])).toEqual([
    [undefined, undefined],
    [undefined, 90_909],
    [SUB, undefined],
  ])
  expect(ofKind(w, 'turn_complete').map(e => [e.agent_id, e.tokens])).toEqual([
    [SUB, undefined],
    [undefined, 92_503],
  ])
  // No agent_list carries the main loop's count.
  expect(JSON.stringify(ofKind(w, 'agent_list'))).not.toContain('92503')

  // /clear: a new conversation, whose first events have no count yet.
  await $.session.end({ reason: 'clear', sessionId: BEFORE_CLEAR, resume: { id: BEFORE_CLEAR } })
  await $.classic.SessionStart({ session_id: AFTER_CLEAR, source: 'clear' })
  await $.tool.call({ tool: 'Bash', command: 'ls', description: 'List files' })
  await w.clock.advance(1000)
  const after = ofKind(w, 'tool').filter(e => e.session_id === AFTER_CLEAR)
  expect(after.length).toBe(1)
  expect(after[0]?.tokens).toBeUndefined()
})

test('an API error goes at once; a worktree agent names its worktree', { options: OPTIONS }, async ($, on) => {
  const w = reporting(on)
  await begin($, w)
  await $.classic.WorktreeCreate({ name: 'agent-a5bd483a78c5f4783' })
  await $.classic.StopFailure({ error: 'rate_limit' })
  await w.clock.settle()
  expect(ofKind(w, 'error')).toEqual([expect.objectContaining({ reason: 'rate_limit' })])
  expect(ofKind(w, 'worktree_create')).toEqual([expect.objectContaining({ name: 'agent-a5bd483a78c5f4783' })])
})

test('classic.SubagentStart: agent_start for each run of an agent, the first and a resume, with its type and cwd', { options: OPTIONS }, async ($, on) => {
  const w = reporting(on)
  await begin($, w)
  const agent = 'a1111111111111111'
  const worktree = `${REPO}/.claude/worktrees/agent-${agent}`
  await $.classic.SubagentStart({ agent_id: agent, agent_type: 'general-purpose', cwd: worktree })
  // SendMessage resumes it under the same id, in the session's cwd.
  await $.classic.SubagentStart({ agent_id: agent, agent_type: 'general-purpose', cwd: REPO })
  // One without an id names no agent: nothing is sent.
  await $.classic.SubagentStart({ agent_type: 'general-purpose', cwd: REPO })
  await w.clock.advance(1000)
  expect(ofKind(w, 'agent_start').map(({ agent_id, agent_type, cwd }) => ({ agent_id, agent_type, cwd }))).toEqual([
    { agent_id: agent, agent_type: 'general-purpose', cwd: worktree },
    { agent_id: agent, agent_type: 'general-purpose', cwd: REPO },
  ])
})

// --- the transport ------------------------------------------------------------

/**
 * A fake /events that is down (or slow) until `up` is set, and records
 * only the batches it accepted.
 */
function flaky(failure: Reply) {
  const state = { up: false, delivered: [] as Ev[][], tries: 0 }
  const route = (req: { body: Record<string, unknown> | undefined }): Reply => {
    state.tries += 1
    if (!state.up) return failure
    state.delivered.push((req.body?.events as Ev[] | undefined) ?? [])
    return { status: 200, body: {} }
  }
  return { state, route }
}

for (const failure of ['refuse', 'hang'] as const) {
  const name = failure === 'refuse' ? 'down' : 'slow'

  test(`aimd ${name}: no hook waits; 200 events are kept, the oldest dropped, and sent once it is back`, { options: OPTIONS, timeoutMs: 20_000 }, async ($, on) => {
    const aimd = flaky(failure)
    const w = reporting(on, { 'POST /events': aimd.route })
    await within(w, 0, $.classic.SessionStart({ session_id: SESSION_ID, source: 'startup' }))
    await within(w, 0, $.session.start(START))
    for (let i = 0; i < 250; i++) {
      await within(w, 0, $.prompt.submit({ text: `p${i}`, wait: false, origin: { kind: 'composer' } }))
    }
    w.below.verdict = 'ask'
    await within(w, 0, $.tool.call({ tool: 'Bash', command: 'mkdir x', description: 'Make x' }))
    await within(w, 0, $.session.end({ reason: 'other', sessionId: SESSION_ID, resume: { id: SESSION_ID } }))
    // A minute of tries, all failed; they back off (2, 4, 8, then 15 s).
    for (let i = 0; i < 12; i++) await w.clock.advance(5000)
    expect(aimd.state.delivered).toEqual([])
    expect(aimd.state.tries <= 10).toBe(true)
    aimd.state.up = true
    for (let i = 0; i < 4; i++) await w.clock.advance(5000)
    const evs = aimd.state.delivered.flat()
    expect(evs.length).toBe(200)
    // The newest are kept: the ask (the dialog's only), the tool's label and the end are last.
    expect(evs.map(e => e.kind).slice(-4)).toEqual(['prompt', 'needs_you', 'tool', 'session_end'])
    expect(evs.filter(e => e.kind === 'prompt').length).toBe(197)
    expect(evs.some(e => e.kind === 'session_start')).toBe(false)
  })
}

test('a refused batch (4xx) is dropped, not tried again', { options: OPTIONS }, async ($, on) => {
  const w = reporting(on, { 'POST /events': { status: 401, body: { error: 'bad token' } } })
  await begin($, w)
  await w.clock.advance(1000)
  await w.clock.advance(60_000)
  expect(w.sent('POST /events').length).toBe(1)
  await $.prompt.submit({ text: 'again', wait: false, origin: { kind: 'composer' } })
  await w.clock.advance(1000)
  const second = w.sent('POST /events')[1]?.body?.events as Ev[] | undefined
  expect(second?.map(e => e.kind)).toEqual(['prompt'])
})

test('a big backlog goes in POSTs of at most 200 events and well under 256 KB', { options: OPTIONS, timeoutMs: 20_000 }, async ($, on) => {
  const aimd = flaky('refuse')
  const w = reporting(on, { 'POST /events': aimd.route })
  await begin($, w)
  // 80 plan approvals of about 1000 characters each: a needs_you and a
  // tool event each, with the session_start 161 events, more characters
  // than one POST may carry.
  const plan = Array.from({ length: 12 }, (_, i) => `${i + 1}. ${'step '.repeat(16)}`).join('\n')
  const input = { plan, planFilePath: '/p.md' }
  for (let i = 0; i < 80; i++) await $.tool.call({ tool: 'ExitPlanMode', ...input })
  aimd.state.up = true
  for (let i = 0; i < 4; i++) await w.clock.advance(5000)
  const batches = aimd.state.delivered
  expect(batches.length >= 2).toBe(true)
  for (const b of batches) {
    expect(b.length <= 200).toBe(true)
    // Characters, at most 3 bytes each: under the 256 KB aimd takes.
    expect(JSON.stringify({ events: b }).length * 3 < 256 * 1024).toBe(true)
  }
  expect(batches.flat().length).toBe(161)
})

// --- failing open ---------------------------------------------------------------

test('every reporting hook fails open: with every $ call broken and odd inputs, each answers what is beneath, at once', { options: OPTIONS }, async ($, on) => {
  const clock = mock.clock(on, { now: 1_000_000 })
  const boom = (): never => {
    throw new Error('boom')
  }
  on('session.id', boom)
  on('session.cwd', boom)
  on('session.root', boom)
  on('session.repo', boom)
  on('process.run', boom)
  on('env.get', boom)
  on('agent.list', boom)
  on('http.fetch', boom)
  on('command.register', boom)
  on('tool.register', boom)
  on('classic.SessionStart', () => ({}))
  on('classic.Notification', () => ({}))
  on('classic.StopFailure', () => ({}))
  on('classic.WorktreeCreate', () => ({}))
  on('classic.SubagentStart', () => ({}))
  on('classic.PermissionRequest', () => ({ decision: { behavior: 'allow' } }))
  on('session.start', () => ({ cwd: '/below' }))
  on('session.end', () => ({ sessionId: 'below' }))
  on('prompt.submit', () => ({ text: 'below' }))
  on('turn.start', () => ({ turnId: 'below' }))
  on('turn.complete', () => ({ text: 'below' }))
  on('turn.step', async function* (_, e) {
    yield { kind: 'text', index: 0, text: 'below' }
    return odd<TurnStepResult>({ turnId: e.turnId, index: e.index, answer: 'below', toolUses: [], stopReason: null, usage: { input_tokens: 'many', output_tokens: -1 } })
  })
  on('tool.check', () => ({ decision: 'ask' }))
  on('agent.spawn', () => SPAWNED)
  on('tool.call', () => ({ result: 'below', text: 'below' }))

  const settled = async <T>(work: Promise<T>): Promise<T> => {
    let done = false
    const tracked = work.then(v => {
      done = true
      return v
    })
    await clock.settle()
    expect(done, 'a reporting hook waited').toBe(true)
    return tracked
  }
  const odd = <T>(v: unknown) => v as T

  expect(await settled($.classic.SessionStart({ session_id: odd<string>(42), source: odd<'startup'>(null) }))).toEqual({})
  expect(await settled($.session.start(START))).toEqual({ cwd: '/below' })
  expect((await settled($.prompt.submit({ text: 'x', wait: false, origin: odd({ kind: 7 }) }))).text).toBe('below')
  expect(await settled($.turn.start({ text: 'x', turnId: 't' }))).toEqual({ turnId: 'below' })
  // turn.step with odd usage, in the main loop, an odd loop and a subagent's.
  for (const agentId of [undefined, odd<string>(7), 'a1']) {
    const out = await settled(drain($.turn.step({ turnId: 't', index: 0, model: 'm', messageCount: 1, ...(agentId !== undefined ? { agentId } : {}) })))
    expect(out.chunks).toEqual([{ kind: 'text', index: 0, text: 'below' }])
    expect(out.result.answer).toBe('below')
  }
  for (const call of [
    { tool: 'AskUserQuestion', questions: 'nope' },
    { tool: 'AskUserQuestion', questions: [null, 3, { options: 'x' }] },
    { tool: 'ExitPlanMode', plan: 42 },
    { tool: 'Bash', command: odd<string>({ not: 'a string' }) },
    { tool: 'mcp__x__y', a: { deep: [1, 2] }, b: Number.NaN },
  ]) {
    const out = await settled($.tool.call(odd<never>(call)))
    expect(out.text, call.tool).toBe('below')
  }
  expect(await settled($.classic.PermissionRequest({ tool_name: odd<string>(undefined), tool_input: null }))).toEqual({ decision: { behavior: 'allow' } })
  expect(await settled($.classic.Notification({ notification_type: odd<string>(9), message: '' }))).toEqual({})
  expect(await settled($.classic.StopFailure({ error: odd('rate_limit') }))).toEqual({})
  expect(await settled($.classic.WorktreeCreate({ name: odd<string>(null) }))).toEqual({})
  expect(await settled($.classic.SubagentStart({ agent_id: 'a1111111111111111', agent_type: 'general-purpose', cwd: '/x' }))).toEqual({})
  expect(await settled($.classic.SubagentStart({ agent_id: odd<string>({ not: 'an id' }), agent_type: odd<string>(3) }))).toEqual({})
  // agent.spawn also asks aimd for the context, which fails at once here.
  expect((await settled($.agent.spawn({ ...SPAWN, prompt: 'Package: aim · C4' }))).agentId).toBe(SPAWNED.agentId)
  expect(await settled($.turn.complete({ answer: odd<string>(5), durationMs: 1, isAborted: false, turnId: 't', reason: 'answer' }))).toEqual({ text: 'below' })
  expect(await settled($.session.end({ reason: 'other', sessionId: 'x', resume: { id: 'x' } }))).toEqual({ sessionId: 'below' })
  // The timers the reporter armed fire into the same broken world.
  await clock.advance(120_000)
})
