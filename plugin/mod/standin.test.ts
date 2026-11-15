// The reporter where no classic.* hook reaches the plugin (the
// organization's security default hands those events, and prompt.compose,
// past the plugins a person installs): what it reports in their place, and
// that it leaves all of it to the classic hooks where they do run.

import type { EngineInterface, On } from 'claude-code'
import { expect, test } from 'claude-code/testing'

import { settings } from './aimd'
import { CONTEXT, LINKED_CONTEXT, REPO, SESSION_ID, WORKTREE } from './fixtures'
import { IDLE_MS, flush, noteAsk, onAsk, onToolDone, resetReporter, sawClassic } from './report'
import { OPTIONS, START, world } from './testkit'

type World = ReturnType<typeof world>
type Ev = Record<string, unknown> & { kind: string; session_id: string }

function posted(w: World): Ev[] {
  return w.sent('POST /events').flatMap(r => (r.body?.events as Ev[] | undefined) ?? [])
}

/** A world with nothing beneath the classic hooks: no test here raises one unless it says so. */
function quiet(on: On, context: unknown = CONTEXT, cwd = REPO, sessionId: string | (() => string) = SESSION_ID) {
  const w = world(on, { 'GET /api/context': { status: 200, body: context }, 'POST /events': { status: 200, body: {} } }, cwd, sessionId)
  const prompts: { text: string; context?: readonly string[] }[] = []
  on('session.start', (_, e) => ({ cwd: e.cwd }))
  on('session.end', (_, e) => ({ sessionId: e.sessionId }))
  on('prompt.submit', (_, e) => {
    prompts.push({ text: e.text, ...(e.context !== undefined ? { context: e.context } : {}) })
    return { text: e.text, origin: e.origin }
  })
  on('turn.start', (_, e) => ({ turnId: e.turnId }))
  on('turn.complete', (_, e) => ({ text: e.answer }))
  on('agent.list', () => ({ value: [] }))
  on('classic.SessionStart', () => ({}))
  on('classic.Notification', () => ({}))
  on('prompt.compose', () => ({ sections: [] }))
  return { ...w, prompts }
}

const PROMPT = { text: 'Reply with the word hi.', wait: false, origin: { kind: 'composer' } } as const
const DONE = { answer: 'hi', durationMs: 5, isAborted: false, turnId: 't1', reason: 'answer' } as const

// --- idle -------------------------------------------------------------------

test('no classic hook: a main loop quiet for a minute after its turn is idle; a prompt in between calls it off', { options: OPTIONS }, async ($, on) => {
  const w = quiet(on)
  await $.session.start(START)
  await $.turn.complete(DONE)
  await w.clock.advance(IDLE_MS - 1000)
  expect(posted(w).filter(e => e.kind === 'idle')).toEqual([])
  await w.clock.advance(2000)
  expect(posted(w).filter(e => e.kind === 'idle').length).toBe(1)

  await $.turn.complete(DONE)
  await w.clock.advance(IDLE_MS / 2)
  await $.prompt.submit(PROMPT)
  await w.clock.advance(IDLE_MS)
  expect(posted(w).filter(e => e.kind === 'idle').length).toBe(1)
  // A subagent's turn ending says nothing of the session.
  await $.turn.complete({ ...DONE, agentId: 'a1111111111111111' })
  await w.clock.advance(IDLE_MS + 1000)
  expect(posted(w).filter(e => e.kind === 'idle').length).toBe(1)
})

test('with the classic hooks running, idle is left to classic.Notification', { options: OPTIONS }, async ($, on) => {
  const w = quiet(on)
  await $.classic.SessionStart({ session_id: SESSION_ID, source: 'startup' })
  await $.session.start(START)
  await $.turn.complete(DONE)
  await w.clock.advance(IDLE_MS + 1000)
  expect(posted(w).filter(e => e.kind === 'idle')).toEqual([])
  await $.classic.Notification({ notification_type: 'idle_prompt', message: 'Claude is waiting for your input' })
  await w.clock.advance(1000)
  expect(posted(w).filter(e => e.kind === 'idle').length).toBe(1)
})

// --- /clear -------------------------------------------------------------------

test('no classic hook: after /clear the next event finds the new id, and its session_start goes first', { options: OPTIONS }, async ($, on) => {
  const before = '55f811f4-a146-450f-b8a3-91f1fe9753bb'
  const after = '7b5451e8-a02f-44dd-b8a1-6f68ed1a940f'
  let id = before
  const w = quiet(on, CONTEXT, REPO, () => id)
  await $.session.start(START)
  await w.clock.settle()
  await $.session.end({ reason: 'clear', sessionId: before, resume: { id: before } })
  await w.clock.settle()
  id = after
  await $.prompt.submit(PROMPT)
  await $.turn.start({ text: PROMPT.text, turnId: 't2' })
  await w.clock.advance(1000)
  expect(posted(w).map(e => [e.kind, e.session_id])).toEqual([
    ['session_start', before],
    ['session_end', before],
    ['session_start', after],
    ['prompt', after],
    ['turn_start', after],
  ])
  expect(posted(w)[2]).toMatchObject({ source: 'clear', prev_session_id: before, cwd: REPO, repo: REPO, branch: 'main' })
})

test('no classic hook: a session_end the process does not outlive sends nothing more', { options: OPTIONS }, async ($, on) => {
  const w = quiet(on)
  await $.session.start(START)
  await w.clock.settle()
  await $.session.end({ reason: 'prompt_input_exit', sessionId: SESSION_ID, resume: { id: SESSION_ID } })
  await w.clock.advance(1000)
  expect(posted(w).map(e => e.kind)).toEqual(['session_start', 'session_end'])
})

// --- the conventions ----------------------------------------------------------

test("no classic hook, no prompt.compose: a linked session's conventions ride beside its next prompt, once", { options: OPTIONS }, async ($, on) => {
  const w = quiet(on, LINKED_CONTEXT, WORKTREE)
  await $.session.start({ ...START, cwd: WORKTREE })
  await w.clock.settle()
  await $.prompt.submit(PROMPT)
  await $.prompt.submit({ ...PROMPT, text: 'And again.' })
  expect(w.prompts.map(p => p.text)).toEqual([PROMPT.text, 'And again.'])
  expect(w.prompts[0]?.context?.length).toBe(1)
  expect(w.prompts[0]?.context?.[0]).toContain('[ai-manager] This session works on package aim · C4 (Mod v1: acting)')
  expect(w.prompts[1]?.context).toBeUndefined()
})

test('where a classic hook runs, the conventions are left to prompt.compose', { options: OPTIONS }, async ($, on) => {
  const w = quiet(on, LINKED_CONTEXT, WORKTREE)
  await $.classic.SessionStart({ session_id: SESSION_ID, source: 'startup' })
  await $.session.start({ ...START, cwd: WORKTREE })
  await w.clock.settle()
  await $.prompt.submit(PROMPT)
  expect(w.prompts[0]?.context).toBeUndefined()
})

test('no classic hook: a session with no linked package gets nothing beside its prompt', { options: OPTIONS }, async ($, on) => {
  const w = quiet(on)
  await $.session.start(START)
  await w.clock.settle()
  await $.prompt.submit(PROMPT)
  expect(w.prompts[0]?.context).toBeUndefined()
})

// --- a permission dialog -------------------------------------------------------
// The test kit raises classic.PermissionRequest for every ask, so these
// drive the reporter itself, over a $ that only keeps time and the POSTs.

type Timer = { at: number; run: () => void; off: boolean }

function bare() {
  let now = 0
  const timers: Timer[] = []
  const events: Ev[] = []
  const $ = {
    clock: {
      after: (ms: number, run: () => void) => {
        const t: Timer = { at: now + ms, run, off: false }
        timers.push(t)
        return {
          cancel: () => {
            t.off = true
          },
        }
      },
      sleep: () => new Promise(() => undefined),
    },
    session: { id: () => Promise.resolve(SESSION_ID), cwd: () => Promise.resolve(REPO) },
    http: {
      fetch: (_url: string, init: { body?: string }) => {
        events.push(...((JSON.parse(init.body ?? '{}') as { events?: Ev[] }).events ?? []))
        return Promise.resolve({ status: 200, ok: true, headers: {}, text: '{}' })
      },
    },
  } as unknown as EngineInterface
  /** Moves time on, running what comes due, then sends what is queued. */
  const advance = async (ms: number) => {
    now += ms
    for (const t of timers.filter(t => !t.off && t.at <= now)) {
      t.off = true
      t.run()
    }
    for (let i = 0; i < 5; i++) await Promise.resolve()
    await flush($)
  }
  resetReporter(settings(OPTIONS))
  return { $, advance, events }
}

const ASK = { toolUseId: 'toolu_01', tool: 'Bash', input: { command: 'mkdir perm-main', description: 'Create directory perm-main' } }

test('no classic hook: an ask is never reported here, however long it waits; a dialog is left to aimd\'s poller', async () => {
  // tool.check's ask, then nothing until the call ends: the same hooks for a
  // call held at a dialog and for one the decider allowed that runs long.
  for (const agentId of [undefined, 'a6cabd35f43276105']) {
    const { advance, events } = bare()
    noteAsk({ ...(agentId !== undefined ? { agentId } : {}), toolUseId: 'toolu_01' })
    await advance(10 * 60_000)
    expect(events.filter(e => e.kind === 'needs_you'), String(agentId)).toEqual([])
  }
})

test('no classic hook: a long call the mode allowed reports only its tool event when it ends (check 1, seen live)', async () => {
  // Auto mode, 2026-11-14: tool.check said ask, the classifier allowed it, and
  // a 52-second build ran. Needs you must not show while it runs.
  const { $, advance, events } = bare()
  noteAsk({ toolUseId: 'toolu_01' })
  await advance(52_000)
  expect(events.filter(e => e.kind === 'needs_you')).toEqual([])
  onToolDone($, { tool: 'Bash', tool_use_id: 'toolu_01', agentId: '', command: 'go build -a ./...', description: 'Rebuild' })
  await advance(1_000)
  expect(events.map(e => e.kind)).toEqual(['tool'])
})

test('with the classic hooks running, a dialog is reported once, from classic.PermissionRequest, under the noted call', async () => {
  const { $, advance, events } = bare()
  noteAsk(ASK)
  // The dialog opens: classic.PermissionRequest, as register.tsx hears it (it names no call).
  sawClassic()
  onAsk($, { source: 'permission_request', tool: 'Bash', input: ASK.input })
  await advance(60_000)
  expect(events.filter(e => e.kind === 'needs_you').map(e => e.tool_use_id)).toEqual(['toolu_01'])
  noteAsk({ ...ASK, toolUseId: 'toolu_02' })
  await advance(60_000)
  expect(events.filter(e => e.kind === 'needs_you').length).toBe(1)
})
