import type { On } from 'claude-code'
import { expect, test } from 'claude-code/testing'

import { CONTEXT, RESUME, SESSION_ID, SPAWN, SPAWNED } from './fixtures'
import { BAND, OPTIONS, START, TOKEN, world, type Routes } from './testkit'

const ROUTES: Routes = {
  'GET /api/context': { status: 200, body: CONTEXT },
  'POST /api/links/spawn': { status: 201, body: { links: [], packages: [] } },
}

/** A session started over the fake aimd, recording the briefs core receives. */
function lead(on: On, routes: Routes = ROUTES, spawned: { model: string; agentId?: string } = SPAWNED) {
  const w = world(on, routes)
  const briefs: string[] = []
  on('session.start', (_, e) => ({ cwd: e.cwd }))
  on('agent.spawn', (_, e) => {
    briefs.push(e.prompt)
    return spawned
  })
  return { ...w, briefs }
}

const SURFACES = ['terminal', 'desktop'] as const

test('a Package: line finds the package, appends the block and links the agent', { options: OPTIONS }, async ($, on) => {
  const w = lead(on)
  await $.session.start(START)
  const prompt = 'Package: aim · C4\n\nBuild C4 from ROADMAP.md. It needs C3 and C5.'
  const out = await $.agent.spawn({ ...SPAWN, prompt, description: 'mod acting' })
  await w.clock.settle()

  expect(out.agentId).toBe(SPAWNED.agentId)
  expect(w.briefs[0]).toContain(prompt)
  expect(w.briefs[0]).toContain('[ai-manager] This agent works on package aim · C4 (Mod v1: acting)')
  expect(w.briefs[0]).toContain('Suggested branch: aim/c4-mod-v1-acting')
  expect(w.briefs[0]).toContain('Put this line in the pull request body: Plan: aim · C4')
  const links = w.sent('POST /api/links/spawn')
  expect(links.length).toBe(1)
  expect(links[0]?.body).toEqual({ session_id: SESSION_ID, agent_id: SPAWNED.agentId, plan: 'aim', pkg: 'C4' })
  expect(links[0]?.headers['X-AIM-Token']).toBe(TOKEN)
  expect(w.sent('GET /api/context')[0]?.query).toBe(`?cwd=${encodeURIComponent('/tmp/probe')}`)
})

test('the Package: line wins over IDs elsewhere, in any case and separator', { options: OPTIONS }, async ($, on) => {
  const w = lead(on)
  await $.session.start(START)
  await $.agent.spawn({ ...SPAWN, prompt: '**Package:** AIM - c4\nNot V1, not D2.', description: 'V1 email reports' })
  await w.clock.settle()
  expect(w.sent('POST /api/links/spawn')[0]?.body).toMatchObject({ plan: 'aim', pkg: 'C4' })
})

test('without a Package: line, an ID in the description finds it', { options: OPTIONS }, async ($, on) => {
  const w = lead(on)
  await $.session.start(START)
  await $.agent.spawn({ ...SPAWN, prompt: 'Build it from ROADMAP.md; it needs C3 and C5.', description: 'C4 mod acting' })
  await w.clock.settle()
  expect(w.sent('POST /api/links/spawn')[0]?.body).toMatchObject({ plan: 'aim', pkg: 'C4' })
  expect(w.briefs[0]).toContain('Plan: aim · C4')
})

test('else exactly one package ID in the prompt finds it', { options: OPTIONS }, async ($, on) => {
  const w = lead(on)
  await $.session.start(START)
  await $.agent.spawn({ ...SPAWN, prompt: 'Send the weekly email reports (V1) to each owner.', description: 'email reports' })
  await w.clock.settle()
  expect(w.sent('POST /api/links/spawn')[0]?.body).toMatchObject({ plan: 'sharing', pkg: 'V1' })
  expect(w.briefs[0]).toContain('Suggested branch: sharing/v1-email-reports')
})

test('a Package: line for a plan of another repo is looked up in aimd', { options: OPTIONS }, async ($, on) => {
  const w = lead(on, {
    ...ROUTES,
    'GET /api/plans/dark': {
      status: 200,
      body: { slug: 'dark', packages: [{ id: 'T2', title: 'Dark palette', lifecycle: 'ready', suggested_branch: 'dark/t2-dark-palette', plan_line: 'Plan: dark · T2' }] },
    },
  })
  await $.session.start(START)
  await $.agent.spawn({ ...SPAWN, prompt: 'Package: dark · T2\n\nBuild T2.', description: 'T2 dark palette' })
  await w.clock.settle()
  expect(w.sent('POST /api/links/spawn')[0]?.body).toMatchObject({ plan: 'dark', pkg: 'T2' })
  expect(w.briefs[0]).toContain('Suggested branch: dark/t2-dark-palette')
})

test('several IDs: the brief is left alone and the band offers the candidates', { options: OPTIONS }, async ($, on) => {
  const w = lead(on)
  await $.session.start(START)
  const prompt = 'Do C4, then D2.'
  await $.agent.spawn({ ...SPAWN, prompt, description: 'mod work' })
  await w.clock.settle()

  expect(w.briefs[0]).toBe(prompt)
  expect(w.sent('POST /api/links/spawn').length).toBe(0)
  for (const surface of SURFACES) {
    const ui = await $.ui.mount({ ...BAND, surface })
    expect((await ui.find({ type: 'Text', text: /Link mod work to:/ }))?.text).toContain('Link mod work to:')
    const buttons = (await ui.findAll({ type: 'Button' })).map(b => b.props.label)
    expect(buttons).toEqual(['C4', 'D2', 'Skip'])
    await ui.unmount()
  }

  // A press makes the link, with no model turn, and the row goes.
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  await ui.press({ key: `link-${SPAWNED.agentId}-aim-D2` })
  expect(w.sent('POST /api/links/spawn')[0]?.body).toEqual({ session_id: SESSION_ID, agent_id: SPAWNED.agentId, plan: 'aim', pkg: 'D2' })
  expect(await ui.find({ type: 'Button' })).toBeUndefined()
  expect(w.toasts).toContain('Linked mod work to aim · D2')
  await ui.unmount()
})

test('an ID two plans share is ambiguous: the band names the plans', { options: OPTIONS }, async ($, on) => {
  const w = lead(on)
  await $.session.start(START)
  await $.agent.spawn({ ...SPAWN, prompt: 'Build it.', description: 'A1 work' })
  await w.clock.settle()
  expect(w.sent('POST /api/links/spawn').length).toBe(0)
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  expect((await ui.findAll({ type: 'Button' })).map(b => b.props.label)).toEqual(['aim · A1', 'sharing · A1', 'Skip'])
  await ui.press({ key: `skip-${SPAWNED.agentId}` })
  expect(await ui.find({ type: 'Button' })).toBeUndefined()
  expect(w.sent('POST /api/links/spawn').length).toBe(0)
  await ui.unmount()
})

test('no package ID at all: a general helper gets no offer', { options: OPTIONS }, async ($, on) => {
  const w = lead(on)
  await $.session.start(START)
  for (const subagentType of ['general-purpose', 'Explore']) {
    await $.agent.spawn({ ...SPAWN, prompt: 'Tidy the README and fix the typos.', description: 'readme', subagentType })
  }
  await w.clock.settle()
  expect(w.sent('POST /api/links/spawn').length).toBe(0)
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  expect(await ui.find({ type: 'Button' })).toBeUndefined()
  expect((await ui.find({ type: 'Text' }))?.text).toBe('engine')
  await ui.unmount()
})

test('an ambiguous spawn from a subagent gets no offer', { options: OPTIONS }, async ($, on) => {
  const w = lead(on)
  await $.session.start(START)
  await $.agent.spawn({ ...SPAWN, prompt: 'Do C4, then D2.', description: 'nested', parentAgentId: 'a61cc06fc9f51079d' })
  await w.clock.settle()
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  expect(await ui.find({ type: 'Button' })).toBeUndefined()
  await ui.unmount()
})

test('a resume is passed on untouched', { options: OPTIONS }, async ($, on) => {
  const w = lead(on)
  await $.session.start(START)
  const prompt = 'Package: aim · C4\nCarry on.'
  await $.agent.spawn({ ...RESUME, prompt })
  await w.clock.settle()
  expect(w.briefs).toEqual([prompt])
  expect(w.sent('POST /api/links/spawn').length).toBe(0)
})

test("a workflow agent's placeholder prompt is skipped; its Package: line is not", { options: OPTIONS }, async ($, on) => {
  const w = lead(on)
  await $.session.start(START)
  const workflow = { runId: 'wf_1', agentIndex: 1 }
  await $.agent.spawn({ ...SPAWN, prompt: '[workflow agent: prompt withheld] C4', description: 'review', workflow })
  await w.clock.settle()
  expect(w.briefs[0]).toBe('[workflow agent: prompt withheld] C4')
  expect(w.sent('POST /api/links/spawn').length).toBe(0)
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  expect(await ui.find({ type: 'Button' })).toBeUndefined()
  await ui.unmount()

  await $.agent.spawn({ ...SPAWN, prompt: 'Package: aim · C4\nGo.', description: 'C4', workflow: { runId: 'wf_1', agentIndex: 2 } })
  await w.clock.settle()
  expect(w.sent('POST /api/links/spawn')[0]?.body).toMatchObject({ plan: 'aim', pkg: 'C4' })
})

test('a remote_launched agent is linked by its cloud session URL', { options: OPTIONS }, async ($, on) => {
  const w = lead(on, ROUTES, { model: 'claude-haiku-5-5' })
  const url = 'https://claude.ai/code/session_01ABC'
  on('tool.call', { tool: 'Agent' }, () => ({
    result: { status: 'remote_launched', taskId: 'r1', sessionUrl: url, description: 'C4 remote', prompt: 'x', outputFile: '/tmp/r1' },
  }))
  await $.session.start(START)
  await $.agent.spawn({ ...SPAWN, prompt: 'Package: aim · C4\nGo.', description: 'C4 remote' })
  await w.clock.settle()
  expect(w.sent('POST /api/links/spawn').length).toBe(0)
  await $.tool.call({ tool: 'Agent', tool_use_id: SPAWN.tool_use_id, prompt: 'Package: aim · C4\nGo.', description: 'C4 remote' })
  await w.clock.settle()
  expect(w.sent('POST /api/links/spawn')[0]?.body).toEqual({ session_id: SESSION_ID, cloud_url: url, plan: 'aim', pkg: 'C4' })
})

test('appendConventions off: the brief is left alone, the agent still linked', { options: { ...OPTIONS, appendConventions: false } }, async ($, on) => {
  const w = lead(on)
  await $.session.start(START)
  const prompt = 'Package: aim · C4\nGo.'
  await $.agent.spawn({ ...SPAWN, prompt, description: 'C4' })
  await w.clock.settle()
  expect(w.briefs[0]).toBe(prompt)
  expect(w.sent('POST /api/links/spawn')[0]?.body).toMatchObject({ plan: 'aim', pkg: 'C4' })
})

test('band off: an ambiguous spawn offers nothing', { options: { ...OPTIONS, band: false } }, async ($, on) => {
  const w = lead(on)
  await $.session.start(START)
  await $.agent.spawn({ ...SPAWN, prompt: 'Do C4, then D2.', description: 'mod work' })
  await w.clock.settle()
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  expect(await ui.find({ type: 'Button' })).toBeUndefined()
  await ui.unmount()
})

test('the context is fetched again only after a minute', { options: OPTIONS }, async ($, on) => {
  const w = lead(on)
  await $.session.start(START)
  await $.agent.spawn({ ...SPAWN, prompt: 'Package: aim · C4', description: 'one' })
  await w.clock.advance(30_000)
  await $.agent.spawn({ ...SPAWN, prompt: 'Package: aim · C4', description: 'two' })
  expect(w.sent('GET /api/context').length).toBe(1)
  await w.clock.advance(31_000)
  await $.agent.spawn({ ...SPAWN, prompt: 'Package: aim · C4', description: 'three' })
  expect(w.sent('GET /api/context').length).toBe(2)
})
