import type { On } from 'claude-code'
import { expect, test, type Engine } from 'claude-code/testing'

import { CONTEXT, DETECTED, REPO, SESSION_ID } from './fixtures'
import { BAND, COMMAND, OPTIONS, START, world, type Routes } from './testkit'

const PLAN = DETECTED.path
const REGISTERED = { created: true, slug: 'sharing', title: 'Pivot share links', path: PLAN, packages: 14 }

const ROUTES: Routes = {
  'GET /api/context': { status: 200, body: CONTEXT },
  'POST /api/plans/detect': { status: 200, body: DETECTED },
  'POST /api/plans': { status: 201, body: REGISTERED },
  'POST /api/plans/dismiss': { status: 200, body: { path: PLAN, dismissed: true, dismissed_at: '2026-11-05T21:15:00Z' } },
}

const SURFACES = ['terminal', 'desktop'] as const

function session(on: On, routes: Routes = ROUTES) {
  const w = world(on, routes)
  on('session.start', (_, e) => ({ cwd: e.cwd }))
  on('tool.call', { tool: ['Write', 'Edit'] }, (_, e) => ({
    result: { type: 'create', filePath: e.file_path, content: '[file content]', structuredPatch: [], originalFile: null },
    text: 'ok',
  }))
  return w
}


async function write($: Engine, file_path: string, agentId?: string) {
  const call = { tool: 'Write' as const, file_path, content: '| Package | What | Needs |' }
  return agentId === undefined ? $.tool.call(call) : $.tool.call({ ...call, agentId })
}

test('saving a plan in the main loop raises the Register band', { options: OPTIONS }, async ($, on) => {
  const w = session(on)
  await $.session.start(START)
  await write($, PLAN)
  await w.clock.settle()
  expect(w.sent('POST /api/plans/detect')[0]?.body).toEqual({ path: PLAN })
  for (const surface of SURFACES) {
    const ui = await $.ui.mount({ ...BAND, surface })
    expect((await ui.find({ type: 'Text', text: /looks like/ }))?.text).toBe('sharing-plan.md looks like a dispatch plan: 14 packages, 3 lanes. ')
    expect((await ui.findAll({ type: 'Button' })).map(b => b.props.label)).toEqual(['Register', 'Not a plan'])
    await ui.unmount()
  }
})

test('[Register] registers the file with no model turn and clears the band', { options: OPTIONS }, async ($, on) => {
  const w = session(on)
  await $.session.start(START)
  await write($, PLAN)
  await w.clock.settle()
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  await ui.press({ key: 'register' })
  expect(w.sent('POST /api/plans')[0]?.body).toEqual({ path: PLAN, session_id: SESSION_ID })
  expect(await ui.find({ type: 'Button' })).toBeUndefined()
  expect(w.toasts).toContain('Registered plan sharing (14 packages) from sharing-plan.md.')
  // The context is fetched again, so the new plan's packages count at once.
  expect(w.sent('GET /api/context').length).toBe(2)
  await ui.unmount()
})

test('[Not a plan] dismisses the file and clears the band', { options: OPTIONS }, async ($, on) => {
  const w = session(on)
  await $.session.start(START)
  await write($, PLAN)
  await w.clock.settle()
  const ui = await $.ui.mount({ ...BAND, surface: 'desktop' })
  await ui.press({ key: 'not-a-plan' })
  expect(w.sent('POST /api/plans/dismiss')[0]?.body).toEqual({ path: PLAN })
  expect(await ui.find({ type: 'Button' })).toBeUndefined()
  await ui.unmount()
})

test('a failed [Register] says why and keeps the offer', { options: OPTIONS }, async ($, on) => {
  const w = session(on, { ...ROUTES, 'POST /api/plans': { status: 409, body: { error: 'slug sharing is taken by another file' } } })
  await $.session.start(START)
  await write($, PLAN)
  await w.clock.settle()
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  await ui.press({ key: 'register' })
  expect(w.toasts).toContain("ai-manager couldn't register sharing-plan.md: slug sharing is taken by another file")
  expect((await ui.findAll({ type: 'Button' })).length).toBe(2)
  await ui.unmount()
})

test('no band for a file that is not offered, a non-.md file, or a subagent write', { options: OPTIONS }, async ($, on) => {
  const w = session(on, { ...ROUTES, 'POST /api/plans/detect': { status: 200, body: { ...DETECTED, registered: true, offer: false } } })
  await $.session.start(START)
  await write($, PLAN)
  await write($, `${REPO}/notes.txt`)
  await write($, `${REPO}/docs/other.md`, 'a61cc06fc9f51079d')
  await w.clock.settle()
  expect(w.sent('POST /api/plans/detect').map(r => r.body)).toEqual([{ path: PLAN }])
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  expect(await ui.find({ type: 'Button' })).toBeUndefined()
  expect((await ui.find({ type: 'Text' }))?.text).toBe('engine')
  await ui.unmount()
})

test('band off: no band, though detection still feeds /plan-register', { options: { ...OPTIONS, band: false } }, async ($, on) => {
  const w = session(on)
  await $.session.start(START)
  await write($, PLAN)
  await w.clock.settle()
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  expect(await ui.find({ type: 'Button' })).toBeUndefined()
  await ui.unmount()
  const out = await $.command.run({ command: 'plan-register', args: '', ...COMMAND })
  expect(out.text).toBe('Registered plan sharing (14 packages) from sharing-plan.md.')
})

test('/plan-register [path]: an explicit path, relative to the session, or the last detected', { options: OPTIONS }, async ($, on) => {
  const w = session(on)
  await $.session.start(START)
  expect(w.registered.commands).toEqual(['plan-register', 'aim'])

  const none = await $.command.run({ command: 'plan-register', args: '', ...COMMAND })
  expect(none.text).toContain('No plan file given')
  expect(w.sent('POST /api/plans').length).toBe(0)

  await $.command.run({ command: 'plan-register', args: 'docs/sharing-plan.md', ...COMMAND })
  expect(w.sent('POST /api/plans')[0]?.body).toEqual({ path: PLAN, session_id: SESSION_ID })

  await write($, PLAN)
  await w.clock.settle()
  const out = await $.command.run({ command: 'plan-register', args: '', ...COMMAND })
  expect(w.sent('POST /api/plans')[1]?.body).toEqual({ path: PLAN, session_id: SESSION_ID })
  expect(out.text).toBe('Registered plan sharing (14 packages) from sharing-plan.md.')
  // The band's offer for that file is gone too.
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  expect(await ui.find({ type: 'Button' })).toBeUndefined()
  await ui.unmount()
})

test('/plan-register passes on what aimd says is wrong', { options: OPTIONS }, async ($, on) => {
  session(on, { ...ROUTES, 'POST /api/plans': { status: 422, body: { error: 'notes.md has no package table' } } })
  await $.session.start(START)
  const out = await $.command.run({ command: 'plan-register', args: '/tmp/probe/notes.md', ...COMMAND })
  expect(out.text).toBe("ai-manager couldn't register notes.md: notes.md has no package table")
})

test("/aim opens this session's plan in the new dashboard", { options: OPTIONS }, async ($, on) => {
  const w = session(on)
  await $.session.start(START)
  const out = await $.command.run({ command: 'aim', args: '', ...COMMAND })
  expect(w.opened).toEqual(['open http://127.0.0.1:17999/app/plans/aim'])
  expect(out.text).toBe('Opened plan aim in the ai-manager dashboard: http://127.0.0.1:17999/app/plans/aim')
})
