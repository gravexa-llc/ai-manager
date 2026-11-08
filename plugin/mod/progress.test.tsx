import type { On } from 'claude-code'
import { expect, mock, test, type Engine } from 'claude-code/testing'

import { CONTEXT, DETECTED, SPAWN, SPAWNED, WORKTREE, bashResult } from './fixtures'
import { BAND, COMMAND, OPTIONS, START, world, type Reply, type Routes } from './testkit'

// The band's progress line: one line for one of the repo's
// plans, read from aimd's plan summary (done, total, ready, needs_you,
// merges and to_merge), refreshed every 15 s and on the events that matter,
// opening the plan's page in the new dashboard when pressed; and plain /aim
// opening the same page.

type World = ReturnType<typeof world>

const SURFACES = ['terminal', 'desktop'] as const
const BASE = 'http://127.0.0.1:17999'

/** GET /api/plans/{slug}/summary as docs/api.md has it, with `over` on top. */
function summary(slug: string, over: Record<string, unknown> = {}) {
  return {
    slug,
    title: `${slug} plan`,
    merges: 'you',
    band: `${slug} · 3/15 done · 1 ready · 2 need you`,
    total: 15,
    done: 3,
    counts: { blocked: 8, ready: 2, working: 1, in_review: 1, done: 3, dropped: 0 },
    // Ready to dispatch: not the yours package the counts' 2 includes.
    ready: [{ id: 'A2', title: 'Route planner', lifecycle: 'ready' }],
    needs_you: 2,
    needs_you_packages: [],
    unlocked: [],
    packages: [],
    warnings: 0,
    alerts: [],
    derived_at: '2026-11-06T10:00:00Z',
    ...over,
  }
}

/** The repo's context with these plans, in this order. */
function contextWith(...slugs: string[]) {
  return { ...CONTEXT, plans: slugs.map(slug => ({ slug, title: `${slug} plan`, band: '', packages: [] })) }
}

const ROUTES: Routes = {
  'GET /api/context': { status: 200, body: CONTEXT },
  'GET /api/plans/aim/summary': { status: 200, body: summary('aim') },
  'GET /api/plans/sharing/summary': { status: 200, body: summary('sharing', { done: 0, total: 3, ready: [], needs_you: 1 }) },
}

function session(on: On, routes: Routes = ROUTES, cwd?: string) {
  const w = cwd === undefined ? world(on, routes) : world(on, routes, cwd)
  on('session.start', (_, e) => ({ cwd: e.cwd }))
  on('turn.complete', (_, e) => ({ text: e.answer }))
  return w
}

/** The summary requests made so far, for any plan. */
function summaries(w: World): string[] {
  return w.requests.filter(r => r.method === 'GET' && /^\/api\/plans\/[^/]+\/summary$/.test(r.path)).map(r => r.path)
}

/** The band's progress line as drawn on `surface` (the Button's label), or undefined when none. */
async function line($: Engine, surface: (typeof SURFACES)[number] = 'terminal'): Promise<string | undefined> {
  const ui = await $.ui.mount({ ...BAND, surface })
  const found = await ui.find({ type: 'Button', key: 'progress' })
  await ui.unmount()
  return found === undefined ? undefined : String(found.props.label)
}

async function start($: Engine, w: World, input: object = START): Promise<void> {
  await $.session.start(input as typeof START)
  await w.clock.settle()
}

const turnEnd = ($: Engine) => $.turn.complete({ answer: 'done', durationMs: 1, isAborted: false, turnId: 't', reason: 'answer' })

// --- the line: 0, 1 and several plans ---------------------------------------------

test('no plan for the repo: no line, and no summary asked for', { options: OPTIONS }, async ($, on) => {
  const w = session(on, { ...ROUTES, 'GET /api/context': { status: 200, body: contextWith() } })
  await start($, w)
  for (const surface of SURFACES) {
    expect(await line($, surface)).toBeUndefined()
    const ui = await $.ui.mount({ ...BAND, surface })
    expect((await ui.find({ type: 'Text' }))?.text).toBe('engine')
    await ui.unmount()
  }
  expect(summaries(w)).toEqual([])
})

test("one plan: the line is its summary's counts, on every surface", { options: OPTIONS }, async ($, on) => {
  const w = session(on, { ...ROUTES, 'GET /api/context': { status: 200, body: contextWith('aim') } })
  await start($, w)
  expect(summaries(w)).toEqual(['/api/plans/aim/summary'])
  for (const surface of SURFACES) expect(await line($, surface)).toBe('aim · 3/15 done · 1 ready · 2 need you')
  // Drawn as parts: the slug bold, the need-you count in the warning color.
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  const button = await ui.find({ type: 'Button', key: 'progress' })
  expect(button?.props.plain).toBe(true)
  expect((await ui.find({ type: 'Text', text: 'aim' }))?.props.bold).toBe(true)
  expect((await ui.find({ type: 'Text', text: '2 need you' }))?.props.color).toBe('warning')
  await ui.unmount()
})

test('several plans: the first, with +N for the rest', { options: OPTIONS }, async ($, on) => {
  const w = session(on, { ...ROUTES, 'GET /api/context': { status: 200, body: contextWith('aim', 'sharing', 'surge') } })
  await start($, w)
  expect(await line($)).toBe('aim · 3/15 done · 1 ready · 2 need you · +2 plans')
  expect(summaries(w)).toEqual(['/api/plans/aim/summary'])
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  expect((await ui.find({ type: 'Text', text: ' · +2 plans' }))?.props.dimColor).toBe(true)
  await ui.unmount()
})

test('several plans: the one this session registers comes first', { options: OPTIONS }, async ($, on) => {
  const registered = { created: true, slug: 'sharing', title: 'Pivot share links', path: DETECTED.path, packages: 3 }
  const w = session(on, { ...ROUTES, 'POST /api/plans': { status: 201, body: registered } })
  await start($, w)
  expect(await line($)).toBe('aim · 3/15 done · 1 ready · 2 need you · +1 plan')
  await $.command.run({ command: 'plan-register', args: DETECTED.path, ...COMMAND })
  // Within 2 s of the last refresh: the next one waits out the gap.
  await w.clock.advance(2000)
  expect(await line($)).toBe('sharing · 0/3 done · 0 ready · 1 needs you · +1 plan')
  // And it stays first on later refreshes.
  await w.clock.advance(15_000)
  expect(await line($)).toBe('sharing · 0/3 done · 0 ready · 1 needs you · +1 plan')
})

test('several plans: the one this session dispatches an agent for comes first', { options: OPTIONS }, async ($, on) => {
  const context = {
    ...CONTEXT,
    plans: [CONTEXT.plans[0], { ...CONTEXT.plans[1], packages: [{ id: 'V1', title: 'Email reports', lifecycle: 'ready', suggested_branch: 'sharing/v1-email-reports', plan_line: 'Plan: sharing · V1' }] }],
  }
  const w = session(on, { ...ROUTES, 'GET /api/context': { status: 200, body: context }, 'POST /api/links/spawn': { status: 201, body: {} } })
  on('agent.spawn', () => SPAWNED)
  await start($, w)
  await w.clock.advance(5000)
  await $.agent.spawn({ ...SPAWN, prompt: 'Package: sharing · V1\nSend the email reports.', description: 'email reports' })
  await w.clock.settle()
  // The spawn link moved the plan: refreshed at once, more than 2 s after the last.
  expect(summaries(w)).toEqual(['/api/plans/aim/summary', '/api/plans/sharing/summary'])
  expect(await line($)).toBe('sharing · 0/3 done · 0 ready · 1 needs you · +1 plan')
})

test('several plans: the one this session works on comes first', { options: OPTIONS }, async ($, on) => {
  const linked = {
    ...CONTEXT,
    cwd: WORKTREE,
    worktree: { path: WORKTREE, branch: 'sharing/v1-email-reports' },
    package: {
      plan: 'sharing', id: 'V1', title: 'Email reports', lifecycle: 'working', suggested_branch: 'sharing/v1-email-reports', plan_line: 'Plan: sharing · V1',
      link: { kind: 'branch', ref: '/tmp/probe:sharing/v1-email-reports', method: 'branch', confidence: 'high' },
    },
  }
  const w = session(on, { ...ROUTES, 'GET /api/context': { status: 200, body: linked } }, WORKTREE)
  await start($, w, { ...START, cwd: WORKTREE })
  expect(await line($)).toBe('sharing · 0/3 done · 0 ready · 1 needs you · +1 plan')
})

test("the line sits above the band's other rows: a plan offer keeps its buttons", { options: OPTIONS }, async ($, on) => {
  const w = session(on, { ...ROUTES, 'POST /api/plans/detect': { status: 200, body: DETECTED } })
  on('tool.call', { tool: 'Write' }, (_, e) => ({ result: { type: 'create', filePath: e.file_path, content: '[file content]', structuredPatch: [], originalFile: null }, text: 'ok' }))
  await start($, w)
  await $.tool.call({ tool: 'Write', file_path: DETECTED.path, content: '| Package | What | Needs |' })
  await w.clock.settle()
  for (const surface of SURFACES) {
    const ui = await $.ui.mount({ ...BAND, surface })
    expect((await ui.findAll({ type: 'Button' })).map(b => b.key)).toEqual(['progress', 'register', 'not-a-plan'])
    await ui.unmount()
  }
})

// --- each count ---------------------------------------------------------------------

test('each count is the summary field as aimd answers it, never counted again in the mod', { options: OPTIONS }, async ($, on) => {
  let body: Record<string, unknown> = summary('aim')
  const w = session(on, { ...ROUTES, 'GET /api/context': { status: 200, body: contextWith('aim') }, 'GET /api/plans/aim/summary': () => ({ status: 200, body }) })
  await start($, w)
  const cases: [Record<string, unknown>, string][] = [
    // done and total; ready is the ready list's length (not counts.ready); needs_you as given.
    [{ done: 12, total: 26, ready: [{ id: 'A' }, { id: 'B' }, { id: 'C' }], needs_you: 0 }, 'aim · 12/26 done · 3 ready · 0 need you'],
    [{ needs_you: 1, needs_you_packages: [] }, 'aim · 3/15 done · 1 ready · 1 needs you'],
    [{ needs_you: 7 }, 'aim · 3/15 done · 1 ready · 7 need you'],
    // To merge: shown on a plan whose lead merges, when there are some.
    [{ merges: 'lead', to_merge: 7 }, 'aim · 3/15 done · 1 ready · 2 need you · 7 to merge'],
    [{ merges: 'lead', to_merge: 1 }, 'aim · 3/15 done · 1 ready · 2 need you · 1 to merge'],
    // Hidden at 0, on a plan you merge yourself (those are need-you), and from an aimd without the field.
    [{ merges: 'lead', to_merge: 0 }, 'aim · 3/15 done · 1 ready · 2 need you'],
    [{ merges: 'you', to_merge: 4 }, 'aim · 3/15 done · 1 ready · 2 need you'],
    [{ merges: 'lead' }, 'aim · 3/15 done · 1 ready · 2 need you'],
  ]
  for (const [over, want] of cases) {
    body = summary('aim', over)
    await w.clock.advance(15_000)
    expect(await line($), JSON.stringify(over)).toBe(want)
  }
})

test('an answer the mod does not know shows no line', { options: OPTIONS }, async ($, on) => {
  let body: unknown = summary('aim')
  const w = session(on, { ...ROUTES, 'GET /api/context': { status: 200, body: contextWith('aim') }, 'GET /api/plans/aim/summary': () => ({ status: 200, body }) })
  await start($, w)
  for (const odd of [summary('aim', { needs_you: undefined }), summary('aim', { ready: 3 }), summary('aim', { done: '3' }), summary('aim', { total: -1 }), null, 'nope']) {
    body = summary('aim')
    await w.clock.advance(15_000)
    expect(await line($)).toBe('aim · 3/15 done · 1 ready · 2 need you')
    body = odd
    await w.clock.advance(15_000)
    expect(await line($), JSON.stringify(odd)).toBeUndefined()
  }
})

// --- the press, and /aim ------------------------------------------------------------------

for (const surface of SURFACES) {
  test(`pressing the line opens the plan's page in the new dashboard (${surface})`, { options: OPTIONS }, async ($, on) => {
    const w = session(on)
    await start($, w)
    const ui = await $.ui.mount({ ...BAND, surface })
    await ui.press({ key: 'progress' })
    await ui.unmount()
    expect(w.opened).toEqual([`open ${BASE}/app/plans/aim`])
    expect(w.toasts).toEqual([])
  })
}

for (const opens of ['fail', 'throw'] as const) {
  test(`a press that can't open a browser says where the page is (${opens})`, { options: OPTIONS }, async ($, on) => {
    const w = session(on)
    await start($, w)
    w.browser.opens = opens
    const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
    await ui.press({ key: 'progress' })
    await ui.unmount()
    expect(w.toasts).toEqual([`ai-manager: plan aim's page is at ${BASE}/app/plans/aim`])
  })
}

test("/aim opens the page of the plan the band shows; /app/ when the repo has none", { options: OPTIONS }, async ($, on) => {
  let context: unknown = contextWith('aim', 'sharing')
  const registered = { created: true, slug: 'sharing', title: 'Pivot share links', path: DETECTED.path, packages: 3 }
  const w = session(on, { ...ROUTES, 'GET /api/context': () => ({ status: 200, body: context }), 'POST /api/plans': { status: 201, body: registered } })
  await start($, w)
  expect((await $.command.run({ command: 'aim', args: '', ...COMMAND })).text).toBe(`Opened plan aim in the ai-manager dashboard: ${BASE}/app/plans/aim`)
  await $.command.run({ command: 'plan-register', args: DETECTED.path, ...COMMAND })
  expect((await $.command.run({ command: 'aim', args: '', ...COMMAND })).text).toBe(`Opened plan sharing in the ai-manager dashboard: ${BASE}/app/plans/sharing`)
  context = contextWith()
  await w.clock.advance(61_000)
  expect((await $.command.run({ command: 'aim', args: '', ...COMMAND })).text).toBe(`Opened the ai-manager dashboard: ${BASE}/app/`)
  expect(w.opened).toEqual([`open ${BASE}/app/plans/aim`, `open ${BASE}/app/plans/sharing`, `open ${BASE}/app/`])
})

test('/aim with aimd down opens /app/, and says where it is when no browser opens', { options: OPTIONS }, async ($, on) => {
  const w = session(on, { 'GET /api/context': 'refuse' })
  await start($, w)
  expect((await $.command.run({ command: 'aim', args: '', ...COMMAND })).text).toBe(`Opened the ai-manager dashboard: ${BASE}/app/`)
  w.browser.opens = 'throw'
  expect((await $.command.run({ command: 'aim', args: '', ...COMMAND })).text).toBe(`The ai-manager dashboard is at ${BASE}/app/`)
})

// --- settings, aimd down, throttling --------------------------------------------------------

test('band off: no line, and the mod never asks for one', { options: { ...OPTIONS, band: false } }, async ($, on) => {
  const w = session(on)
  await start($, w)
  await turnEnd($)
  await w.clock.advance(60_000)
  for (const surface of SURFACES) expect(await line($, surface)).toBeUndefined()
  expect(summaries(w)).toEqual([])
  // /aim still opens the plan's page.
  expect((await $.command.run({ command: 'aim', args: '', ...COMMAND })).text).toBe(`Opened plan aim in the ai-manager dashboard: ${BASE}/app/plans/aim`)
})

test('a -p run draws no band: no line is asked for', { options: OPTIONS }, async ($, on) => {
  const w = session(on)
  await start($, w, { cwd: START.cwd, surface: null, isInteractive: false })
  await turnEnd($)
  await w.clock.advance(60_000)
  expect(summaries(w)).toEqual([])
})

test('a client that draws attaching later starts the line', { options: OPTIONS }, async ($, on) => {
  const w = session(on)
  on('session.attach', (_, e) => ({ clientId: e.clientId }))
  await start($, w, { cwd: START.cwd, surface: null, isInteractive: false })
  expect(summaries(w)).toEqual([])
  expect(await $.session.attach({ surface: 'desktop', clientId: 'desktop:default' })).toEqual({ clientId: 'desktop:default' })
  await w.clock.settle()
  expect(await line($, 'desktop')).toBe('aim · 3/15 done · 1 ready · 2 need you · +1 plan')
})

for (const failure of ['refuse', 'hang'] as const) {
  const name = failure === 'refuse' ? 'down' : 'slow'

  test(`aimd ${name} from the start: no line and no error, and no hook waits for one`, { options: OPTIONS }, async ($, on) => {
    const reply: Reply = failure
    const w = session(on, { 'GET /api/context': reply, 'GET /api/plans/aim/summary': reply })
    let done = false
    const started = $.session.start(START).then(() => {
      done = true
    })
    await w.clock.settle()
    if (!done) await w.clock.advance(1000)
    expect(done, 'session.start waited over 1 s').toBe(true)
    await started
    await w.clock.advance(30_000)
    for (const surface of SURFACES) {
      const ui = await $.ui.mount({ ...BAND, surface })
      expect(await ui.find({ type: 'Button' })).toBeUndefined()
      expect((await ui.find({ type: 'Text' }))?.text).toBe('engine')
      await ui.unmount()
    }
  })

  test(`aimd ${name} after a line was shown: the line goes, and comes back with aimd`, { options: OPTIONS }, async ($, on) => {
    let up = true
    const flip = (reply: Reply) => () => (up ? reply : failure)
    const w = session(on, {
      'GET /api/context': flip({ status: 200, body: CONTEXT }),
      'GET /api/plans/aim/summary': flip({ status: 200, body: summary('aim') }),
    })
    await start($, w)
    expect(await line($)).toBe('aim · 3/15 done · 1 ready · 2 need you · +1 plan')
    up = false
    // A turn ends while aimd doesn't answer: the hook doesn't wait on the refresh it starts.
    await w.clock.advance(5000)
    let ended = false
    const turn = turnEnd($).then(() => {
      ended = true
    })
    await w.clock.settle()
    expect(ended, 'turn.complete waited on aimd').toBe(true)
    await turn
    await w.clock.advance(1000)
    expect(await line($)).toBeUndefined()
    up = true
    await w.clock.advance(15_000)
    expect(await line($)).toBe('aim · 3/15 done · 1 ready · 2 need you · +1 plan')
  })
}

test('the plan unregistered since: no line, rather than an error', { options: OPTIONS }, async ($, on) => {
  let gone = false
  const w = session(on, {
    ...ROUTES,
    'GET /api/plans/aim/summary': () => (gone ? { status: 404, body: { error: 'no plan aim' } } : { status: 200, body: summary('aim') }),
  })
  await start($, w)
  expect(await line($)).toBeDefined()
  gone = true
  await w.clock.advance(15_000)
  expect(await line($)).toBeUndefined()
})

test('throttling: drawing never asks aimd; a poll every 15 s; events refresh at most once per 2 s', { options: OPTIONS }, async ($, on) => {
  const w = session(on)
  on('tool.call', { tool: 'Bash' }, (_, e) => bashResult(e.command.startsWith('gh pr create') ? 'https://github.com/example-user/probe/pull/21\n' : 'ok\n'))
  await start($, w)
  expect(summaries(w).length).toBe(1)

  // However often the band is drawn, nothing is asked.
  for (let i = 0; i < 10; i++) expect(await line($, i % 2 === 0 ? 'terminal' : 'desktop')).toBeDefined()
  expect(summaries(w).length).toBe(1)

  // The poll: 15 s after the last refresh, not sooner.
  await w.clock.advance(14_999)
  expect(summaries(w).length).toBe(1)
  await w.clock.advance(1)
  expect(summaries(w).length).toBe(2)

  // A burst of turn ends right after a refresh: one more, once 2 s have passed.
  for (let i = 0; i < 5; i++) await turnEnd($)
  await w.clock.settle()
  expect(summaries(w).length).toBe(2)
  await w.clock.advance(2000)
  expect(summaries(w).length).toBe(3)
  // ... and the poll starts over from it.
  await w.clock.advance(14_999)
  expect(summaries(w).length).toBe(3)
  await w.clock.advance(1)
  expect(summaries(w).length).toBe(4)

  // Long after the last refresh, an event refreshes at once.
  await w.clock.advance(5000)
  await turnEnd($)
  await w.clock.settle()
  expect(summaries(w).length).toBe(5)

  // A PR created (here by an unlinked session's gh) is an event too.
  await w.clock.advance(3000)
  await $.tool.call({ tool: 'Bash', command: 'gh pr create -t x -b y' })
  await w.clock.settle()
  expect(summaries(w).length).toBe(6)
  // Other Bash calls are not.
  await w.clock.advance(3000)
  await $.tool.call({ tool: 'Bash', command: 'ls' })
  await w.clock.settle()
  expect(summaries(w).length).toBe(6)
})

test('events while a refresh runs bring one more after it, not one each', { options: OPTIONS }, async ($, on) => {
  let slow = false
  const w = session(on, { ...ROUTES, 'GET /api/plans/aim/summary': () => (slow ? 'hang' : { status: 200, body: summary('aim') }) })
  await start($, w)
  await w.clock.advance(5000)
  slow = true
  await turnEnd($)
  await w.clock.settle()
  expect(summaries(w).length).toBe(2)
  // That refresh waits on aimd (1 s at most); four more turns end meanwhile.
  for (let i = 0; i < 4; i++) await turnEnd($)
  await w.clock.settle()
  expect(summaries(w).length).toBe(2)
  slow = false
  await w.clock.advance(1000)
  // It gave up on aimd: no line. One more refresh follows, 2 s on.
  expect(await line($)).toBeUndefined()
  expect(summaries(w).length).toBe(2)
  await w.clock.advance(2000)
  expect(summaries(w).length).toBe(3)
  expect(await line($)).toBe('aim · 3/15 done · 1 ready · 2 need you · +1 plan')
  await w.clock.advance(14_000)
  expect(summaries(w).length).toBe(3)
})

// --- failing open ------------------------------------------------------------------

test('every hook the line touches fails open: with every $ call broken, each answers what is beneath, at once', { options: OPTIONS }, async ($, on) => {
  const clock = mock.clock(on, { now: 1_000_000 })
  const boom = (): never => {
    throw new Error('boom')
  }
  for (const noun of [
    'session.id', 'session.cwd', 'session.root', 'session.repo', 'process.run', 'env.get', 'agent.list', 'http.fetch',
    'command.register', 'tool.register', 'ui.toast', 'state.get', 'state.set',
  ] as const) {
    on(noun, boom)
  }
  on('session.start', () => ({ cwd: '/below' }))
  on('session.attach', () => ({ clientId: 'below' }))
  on('session.end', () => ({ sessionId: 'below' }))
  on('turn.complete', () => ({ text: 'below' }))
  on('agent.spawn', () => SPAWNED)
  on('tool.call', () => ({ result: 'https://github.com/example-user/probe/pull/21', text: 'https://github.com/example-user/probe/pull/21' }))
  on('ui.render', ($$, e) => {
    const { Text } = $$.ui.resolve(e)
    return <Text>engine</Text>
  })

  const settled = async <T,>(work: Promise<T>): Promise<T> => {
    let done = false
    const tracked = work.then(v => {
      done = true
      return v
    })
    await clock.settle()
    expect(done, 'a hook waited').toBe(true)
    return tracked
  }

  expect(await settled($.session.start(START))).toEqual({ cwd: '/below' })
  expect(await settled($.session.attach({ surface: 'desktop', clientId: 'desktop:default' }))).toEqual({ clientId: 'below' })
  expect(await settled(turnEnd($))).toEqual({ text: 'below' })
  expect((await settled($.agent.spawn({ ...SPAWN, prompt: 'Package: aim · C4' }))).agentId).toBe(SPAWNED.agentId)
  expect((await settled($.tool.call({ tool: 'Bash', command: 'gh pr create -t x -b y' }))).text).toContain('/pull/21')
  expect((await settled($.tool.call({ tool: 'Bash', command: 'gh pr create -t x -b y', agentId: SPAWNED.agentId }))).text).toContain('/pull/21')
  expect((await settled($.tool.call({ tool: 'mcp__github__create_pull_request', owner: 'o', repo: 'r', title: 't', head: 'h', base: 'main', body: 'b' }))).text).toContain('/pull/21')
  for (const surface of SURFACES) {
    const ui = await $.ui.mount({ ...BAND, surface })
    expect((await ui.find({ type: 'Text' }))?.text).toBe('engine')
    await ui.unmount()
  }
  // /aim answers for itself: where the dashboard is, with nothing reachable.
  expect((await settled($.command.run({ command: 'aim', args: '', ...COMMAND }))).text).toBe(`The ai-manager dashboard is at ${BASE}/app/`)
  // The poll's timers fire into the same broken world.
  await clock.advance(120_000)
  expect(await settled($.session.end({ reason: 'other', sessionId: 'x', resume: { id: 'x' } }))).toEqual({ sessionId: 'below' })
  await clock.advance(60_000)
})
