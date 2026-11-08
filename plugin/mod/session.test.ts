import type { On } from 'claude-code'
import { expect, mock, test, type Engine } from 'claude-code/testing'

import { BOARD, COMPOSE, CONTEXT, LINKED_CONTEXT, SECTIONS, SPAWN, SPAWNED, SUMMARY, WORKTREE, bashResult } from './fixtures'
import { BAND, COMMAND, OPTIONS, START, world, type Reply, type Routes } from './testkit'

type World = ReturnType<typeof world>

const TOOL = 'mcp__ai-manager__plan_status'
const SECTION_ID = 'ai-manager:package'

function composing(on: On) {
  on('session.start', (_, e) => ({ cwd: e.cwd }))
  on('prompt.compose', () => ({ sections: [...SECTIONS] }))
}

// --- prompt.compose ---------------------------------------------------------

test("prompt.compose adds the linked package's block, from the session.start cache", { options: OPTIONS }, async ($, on) => {
  const w = world(on, { 'GET /api/context': { status: 200, body: LINKED_CONTEXT } }, WORKTREE)
  composing(on)
  await $.session.start({ ...START, cwd: WORKTREE })
  // The band's first refresh asks aimd on its own; compose doesn't.
  await w.clock.settle()
  const before = w.requests.length
  const first = await $.prompt.compose(COMPOSE)
  const second = await $.prompt.compose(COMPOSE)
  expect(w.requests.length).toBe(before)
  expect(first.sections.map(s => s.id)).toEqual(['lean_body', 'env_info_simple', SECTION_ID])
  const section = first.sections.find(s => s.id === SECTION_ID)
  expect(section?.scope).toBe('session')
  expect(section?.text).toContain('[ai-manager] This session works on package aim · C4 (Mod v1: acting)')
  expect(section?.text).toContain('Suggested branch: aim/c4-mod-v1-acting')
  expect(section?.text).toContain('Plan: aim · C4')
  expect(second.sections).toEqual(first.sections)
  expect(w.sent('GET /api/context')[0]?.query).toBe(`?cwd=${encodeURIComponent(WORKTREE)}`)
})

test('prompt.compose adds nothing without a linked package, or with appendConventions off', { options: OPTIONS }, async ($, on) => {
  world(on, { 'GET /api/context': { status: 200, body: CONTEXT } })
  composing(on)
  await $.session.start(START)
  expect((await $.prompt.compose(COMPOSE)).sections).toEqual([...SECTIONS])
})

test('appendConventions off: no section', { options: { ...OPTIONS, appendConventions: false } }, async ($, on) => {
  world(on, { 'GET /api/context': { status: 200, body: LINKED_CONTEXT } }, WORKTREE)
  composing(on)
  await $.session.start({ ...START, cwd: WORKTREE })
  expect((await $.prompt.compose(COMPOSE)).sections).toEqual([...SECTIONS])
})

test('a link found after the prompt went out reaches the model as a new row, once', { options: OPTIONS }, async ($, on) => {
  let context: unknown = CONTEXT
  const w = world(on, { 'GET /api/context': () => ({ status: 200, body: context }), 'POST /api/links/spawn': { status: 201, body: {} } }, WORKTREE)
  const rows = mock.session(on)
  composing(on)
  on('agent.spawn', () => SPAWNED)
  await $.session.start({ ...START, cwd: WORKTREE })
  expect((await $.prompt.compose(COMPOSE)).sections).toEqual([...SECTIONS])
  context = LINKED_CONTEXT
  await w.clock.advance(61_000)
  await $.agent.spawn({ ...SPAWN, prompt: 'Look around.', description: 'look', subagentType: 'Explore' })
  await w.clock.advance(61_000)
  await $.agent.spawn({ ...SPAWN, prompt: 'Look again.', description: 'look', subagentType: 'Explore' })
  const appended = rows.appended().filter(r => r.door === 'note')
  expect(appended.length).toBe(1)
  expect(JSON.stringify(appended[0]?.message.content)).toContain('This session works on package aim · C4')
  // The system prompt itself stays as first sent.
  expect((await $.prompt.compose(COMPOSE)).sections).toEqual([...SECTIONS])
})

// --- plan_status --------------------------------------------------------------

const PLAN_ROUTES: Routes = {
  'GET /api/context': { status: 200, body: CONTEXT },
  'GET /api/plans/aim/summary': { status: 200, body: SUMMARY },
  'GET /api/plans/aim': { status: 200, body: BOARD },
  'GET /api/plans/sharing/summary': { status: 200, body: { ...SUMMARY, slug: 'sharing' } },
  'GET /api/plans/sharing': { status: 200, body: { ...BOARD, slug: 'sharing', packages: [] } },
}

function parsed(out: { result?: unknown }) {
  return JSON.parse(String(out.result)) as {
    repo: unknown
    plans: { summary: typeof SUMMARY; board: { slug: string; packages: { id: string; links: { pr?: Record<string, unknown> }[] }[] } }[]
    note?: string
  }
}

test('plan_status is registered in the prompt where the repo has plans', { options: OPTIONS }, async ($, on) => {
  const w = world(on, PLAN_ROUTES)
  composing(on)
  await $.session.start(START)
  expect(w.registered.tools).toEqual([{ name: 'plan_status', isDeferred: false }])
})

test('plan_status is left behind ToolSearch where the repo has none', { options: OPTIONS }, async ($, on) => {
  const w = world(on, { 'GET /api/context': { status: 200, body: { ...CONTEXT, plans: [] } } })
  composing(on)
  await $.session.start(START)
  expect(w.registered.tools).toEqual([{ name: 'plan_status', isDeferred: undefined }])
})

test("plan_status returns each plan's summary and board, as data", { options: OPTIONS }, async ($, on) => {
  const w = world(on, PLAN_ROUTES)
  composing(on)
  await $.session.start(START)
  await w.clock.settle()
  // The band's line already asked for aim's summary once.
  const before = w.sent('GET /api/plans/aim/summary').length
  const out = await $.tool.call({ tool: TOOL })
  const data = parsed(out)
  expect(data.repo).toEqual(CONTEXT.repo)
  expect(data.plans.map(p => p.summary.slug)).toEqual(['aim', 'sharing'])
  expect(data.plans[0]?.summary.ready).toEqual(SUMMARY.ready)
  expect(data.plans[0]?.board.packages[0]?.id).toBe('C3')
  expect(data.plans[0]?.board.packages[0]?.links[0]?.pr).toEqual({
    number: 20,
    url: 'https://github.com/example-user/probe/pull/20',
    state: 'MERGED',
  })
  expect(w.sent('GET /api/plans/aim/summary').length - before).toBe(1)
})

test('plan_status for one plan, since a time', { options: OPTIONS }, async ($, on) => {
  const w = world(on, PLAN_ROUTES)
  composing(on)
  await $.session.start(START)
  await w.clock.settle()
  const out = await $.tool.call({ tool: TOOL, plan: 'aim', since: '2026-11-05T09:00:00Z' })
  expect(parsed(out).plans.length).toBe(1)
  // The first is the band's own, with no since.
  expect(w.sent('GET /api/plans/aim/summary').map(r => r.query)).toEqual(['', `?since=${encodeURIComponent('2026-11-05T09:00:00Z')}`])
  expect(w.sent('GET /api/plans/sharing/summary').length).toBe(0)
})

test('plan_status with no plan registered says how to register one', { options: OPTIONS }, async ($, on) => {
  world(on, { 'GET /api/context': { status: 200, body: { ...CONTEXT, plans: [] } } })
  composing(on)
  await $.session.start(START)
  const data = parsed(await $.tool.call({ tool: TOOL }))
  expect(data.plans).toEqual([])
  expect(data.note).toContain('/plan-register')
})

// --- aimd down or slow: every hook falls through unchanged within ~1 s ------

/** Settles `work` with the mocked clock moved on by at most `ms`; fails if it is still pending. */
async function within<T>(w: World, ms: number, work: Promise<T>): Promise<T> {
  let done = false
  const tracked = work.then(value => {
    done = true
    return value
  })
  await w.clock.settle()
  if (!done) await w.clock.advance(ms)
  expect(done, `still waiting after ${ms} ms`).toBe(true)
  return tracked
}

for (const failure of ['refuse', 'hang'] as const) {
  const name = failure === 'refuse' ? 'down' : 'slow'

  test(`aimd ${name}: every hook falls through unchanged within ~1 s`, { options: OPTIONS }, async ($: Engine, on) => {
    const reply: Reply = failure
    const w = world(on, {
      'GET /api/context': reply,
      'POST /api/plans/detect': reply,
      'POST /api/plans': reply,
      'POST /api/links/spawn': reply,
      'GET /api/plans/aim': reply,
    })
    const briefs: string[] = []
    composing(on)
    on('agent.spawn', (_, e) => {
      briefs.push(e.prompt)
      return SPAWNED
    })
    on('tool.call', { tool: 'Bash' }, () => bashResult('https://github.com/example-user/probe/pull/21'))
    on('tool.call', { tool: 'Write' }, () => ({ result: { type: 'create' }, text: 'ok' }))

    await within(w, 1000, $.session.start(START))
    expect(w.registered.commands).toEqual(['plan-register', 'aim'])
    expect(w.registered.tools).toEqual([{ name: 'plan_status', isDeferred: undefined }])

    expect((await within(w, 0, $.prompt.compose(COMPOSE))).sections).toEqual([...SECTIONS])

    const prompt = 'Package: aim · C4\nBuild it.'
    expect((await within(w, 0, $.agent.spawn({ ...SPAWN, prompt }))).agentId).toBe(SPAWNED.agentId)
    expect(briefs).toEqual([prompt])

    // A minute on, the context is asked for again: that spawn waits 1 s at most.
    await w.clock.advance(61_000)
    await within(w, 1000, $.agent.spawn({ ...SPAWN, prompt, description: 'again' }))
    expect(briefs).toEqual([prompt, prompt])

    const gh = await within(w, 0, $.tool.call({ tool: 'Bash', command: 'gh pr create -t x -b "no line"', agentId: SPAWNED.agentId }))
    expect(gh.deny).toBeUndefined()

    const wrote = await within(w, 0, $.tool.call({ tool: 'Write', file_path: '/tmp/probe/plan.md', content: 'x' }))
    expect(wrote.text).toBe('ok')
    await w.clock.advance(1000)

    const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
    expect((await ui.find({ type: 'Text' }))?.text).toBe('engine')
    await ui.unmount()

    const reg = await within(w, 1000, $.command.run({ command: 'plan-register', args: '/tmp/probe/plan.md', ...COMMAND }))
    expect(reg.text).toContain("ai-manager couldn't register plan.md: aimd")

    const status = await within(w, 1000, $.tool.call({ tool: TOOL }))
    expect(status.deny).toContain("ai-manager's plan_status couldn't reach aimd")
  })
}
