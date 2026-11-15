// Agents the mod links without asking (a title naming several packages of
// one plan), the note that asks the spawning loop when it can't tell, the
// plan_link tool that answers it, and how the band's link offers expire.

import type { On } from 'claude-code'
import { expect, mock, test, type Engine } from 'claude-code/testing'

import { CONTEXT, SESSION_ID, SPAWN, SPAWNED } from './fixtures'
import { MAX_RANGE, titleIds } from './text'
import { BAND, OPTIONS, START, world, type Routes } from './testkit'

const LINK = 'mcp__ai-manager__plan_link'
const LINKED = { status: 201, body: { links: [], packages: [] } }
const ROUTES: Routes = {
  'GET /api/context': { status: 200, body: CONTEXT },
  'POST /api/links/spawn': LINKED,
}

/** A lead session over the fake aimd: the briefs core receives and the notes the mod appends. */
function lead(on: On, routes: Routes = ROUTES) {
  const w = world(on, routes)
  const briefs: string[] = []
  const rows = mock.session(on)
  on('session.start', (_, e) => ({ cwd: e.cwd }))
  on('agent.spawn', (_, e) => {
    briefs.push(e.prompt)
    return SPAWNED
  })
  const notes = () => rows.appended().filter(r => r.door === 'note').map(r => ({ agentId: r.agentId, text: JSON.stringify(r.message.content) }))
  return { ...w, briefs, notes }
}

async function linkLine($: Engine): Promise<string | undefined> {
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  const found = await ui.find({ type: 'Text', text: /^Link / })
  await ui.unmount()
  return found?.text
}

// --- titleIds ---------------------------------------------------------------

test('titleIds spells out ranges, in the title order, each ID once', () => {
  const none = () => false
  expect(titleIds('front end U1-U3', none)).toEqual(['U1', 'U2', 'U3'])
  expect(titleIds('U1–U3 and U5', none)).toEqual(['U1', 'U2', 'U3', 'U5'])
  expect(titleIds('B1—B4 back end', none)).toEqual(['B1', 'B2', 'B3', 'B4'])
  expect(titleIds('S1..S3', none)).toEqual(['S1', 'S2', 'S3'])
  expect(titleIds('C3 to C5', none)).toEqual(['C3', 'C4', 'C5'])
  expect(titleIds('C3 through C4', none)).toEqual(['C3', 'C4'])
  expect(titleIds('U1-3', none)).toEqual(['U1', 'U2', 'U3'])
  expect(titleIds('SEC-1–SEC-2', none)).toEqual(['SEC-1', 'SEC-2'])
  expect(titleIds('A2, then U1-U2, then A2 again', none)).toEqual(['A2', 'U1', 'U2'])
  // Not ranges: a different prefix, backwards, too long, or a package of that name.
  expect(titleIds('U1-S3', none)).toEqual(['U1-S3'])
  expect(titleIds('U3-U1', none)).toEqual(['U3-U1'])
  expect(titleIds(`U1-U${MAX_RANGE + 1}`, none)).toEqual([`U1-U${MAX_RANGE + 1}`])
  expect(titleIds('fix U1-U3', id => id === 'U1-U3')).toEqual(['U1-U3'])
  expect(titleIds('a general helper', none)).toEqual([])
})

// --- a title naming several packages of one plan ------------------------------

test('a title naming a range of one plan links the agent to each package, with no question', { options: OPTIONS }, async ($, on) => {
  const w = lead(on)
  await $.session.start(START)
  await $.agent.spawn({ ...SPAWN, prompt: 'Build the acting mod. Skills are done; see D2 later.', description: 'mod work C3-C5' })
  await w.clock.settle()
  expect(w.sent('POST /api/links/spawn').map(r => r.body)).toEqual(
    ['C3', 'C4', 'C5'].map(pkg => ({ session_id: SESSION_ID, agent_id: SPAWNED.agentId, plan: 'aim', pkg })),
  )
  expect(w.briefs[0]).toContain('This agent works on package aim · C3')
  expect(w.briefs[0]).toContain('It also works on C4, C5 of the same plan')
  expect(w.notes()).toEqual([])
  expect(await linkLine($)).toBeUndefined()
})

test('a title naming packages of two plans is still a question', { options: OPTIONS }, async ($, on) => {
  const w = lead(on)
  await $.session.start(START)
  await $.agent.spawn({ ...SPAWN, prompt: '[prompt]', description: 'C4 and V1' })
  await w.clock.settle()
  expect(w.sent('POST /api/links/spawn')).toEqual([])
  expect(w.notes().length).toBe(1)
  expect(await linkLine($)).toBe('Link C4 and V1 to: ')
})

// --- the note to the loop that spawned it --------------------------------------

test('a brief that only mentions several packages: the lead is asked, with plan_link', { options: OPTIONS }, async ($, on) => {
  const w = lead(on)
  await $.session.start(START)
  await $.agent.spawn({ ...SPAWN, prompt: 'Do C4, then D2.', description: 'mod work' })
  await w.clock.settle()
  const notes = w.notes()
  expect(notes.length).toBe(1)
  expect(notes[0]?.agentId).toBeUndefined()
  const text = notes[0]?.text ?? ''
  expect(text).toContain('the agent \\"mod work\\"')
  expect(text).toContain('aim · C4 (Mod v1: acting), aim · D2 (Mod v2: reporting)')
  expect(text).toContain(`call plan_link with agent \\"${SPAWNED.agentId}\\"`)
  expect(text).toContain('If it isn\'t plan work, do nothing.')
})

test('the lead is asked in every band mode; only full offers it in the band', { options: { ...OPTIONS, band: 'status' } }, async ($, on) => {
  const w = lead(on)
  await $.session.start(START)
  await $.agent.spawn({ ...SPAWN, prompt: 'Do C4, then D2.', description: 'mod work' })
  await w.clock.settle()
  expect(w.notes().length).toBe(1)
  expect(await linkLine($)).toBeUndefined()
})

test("an agent's own spawn: the note goes to that agent, and the band offers nothing", { options: OPTIONS }, async ($, on) => {
  const w = lead(on)
  await $.session.start(START)
  await $.agent.spawn({ ...SPAWN, prompt: 'Do C4, then D2.', description: 'helper', parentAgentId: 'parent-agent' })
  await w.clock.settle()
  expect(w.notes().map(n => n.agentId)).toEqual(['parent-agent'])
  expect(await linkLine($)).toBeUndefined()
})

// --- plan_link -----------------------------------------------------------------

test('plan_link links the agent to each package, and its band offer goes', { options: OPTIONS }, async ($, on) => {
  const w = lead(on)
  await $.session.start(START)
  await $.agent.spawn({ ...SPAWN, prompt: 'Do C4, then D2.', description: 'mod work' })
  await w.clock.settle()
  expect(await linkLine($)).toBe('Link mod work to: ')

  const out = await $.tool.call({ tool: LINK, agent: SPAWNED.agentId, plan: 'aim', packages: ['C4', 'D2', 'c4'] })
  expect(out.result).toBe('Linked the agent to aim · C4, D2.')
  expect(w.sent('POST /api/links/spawn').map(r => r.body)).toEqual([
    { session_id: SESSION_ID, agent_id: SPAWNED.agentId, plan: 'aim', pkg: 'C4' },
    { session_id: SESSION_ID, agent_id: SPAWNED.agentId, plan: 'aim', pkg: 'D2' },
  ])
  expect(await linkLine($)).toBeUndefined()
})

test('plan_link takes a remote agent by its cloud session URL', { options: OPTIONS }, async ($, on) => {
  const w = lead(on)
  await $.session.start(START)
  const url = 'https://claude.ai/code/session_example'
  const out = await $.tool.call({ tool: LINK, agent: url, plan: 'sharing', packages: ['V1'] })
  expect(out.result).toBe('Linked the agent to sharing · V1.')
  expect(w.sent('POST /api/links/spawn')[0]?.body).toEqual({ session_id: SESSION_ID, cloud_url: url, plan: 'sharing', pkg: 'V1' })
})

test('plan_link says what it could not link, and asks for what is missing', { options: OPTIONS }, async ($, on) => {
  const w = lead(on, { ...ROUTES, 'POST /api/links/spawn': { status: 404, body: { error: 'plan aim has no package Z9' } } })
  await $.session.start(START)
  const missing = await $.tool.call({ tool: LINK, agent: SPAWNED.agentId, plan: 'aim', packages: [] })
  expect(String(missing.result)).toContain('Nothing linked: give agent')
  const failed = await $.tool.call({ tool: LINK, agent: SPAWNED.agentId, plan: 'aim', packages: ['Z9'] })
  expect(failed.result).toBe('Nothing linked to aim: Z9 (plan aim has no package Z9).')
  expect(w.sent('POST /api/links/spawn').length).toBe(1)
})

// --- link offers expire ----------------------------------------------------------

test('a link offer goes when its agent stops', { options: OPTIONS }, async ($, on) => {
  const w = lead(on)
  on('classic.SubagentStop', () => ({}))
  await $.session.start(START)
  await $.agent.spawn({ ...SPAWN, prompt: 'Do C4, then D2.', description: 'mod work' })
  await w.clock.settle()
  expect(await linkLine($)).toBe('Link mod work to: ')
  await $.classic.SubagentStop({
    stop_hook_active: false,
    agent_id: SPAWNED.agentId,
    agent_transcript_path: '/tmp/probe/agent.jsonl',
    agent_type: 'general-purpose',
  })
  await w.clock.settle()
  expect(await linkLine($)).toBeUndefined()
})

test('a link offer goes after 30 minutes', { options: OPTIONS }, async ($, on) => {
  const w = lead(on)
  await $.session.start(START)
  await $.agent.spawn({ ...SPAWN, prompt: 'Do C4, then D2.', description: 'mod work' })
  await w.clock.settle()
  await w.clock.advance(29 * 60_000)
  expect(await linkLine($)).toBe('Link mod work to: ')
  await w.clock.advance(2 * 60_000)
  expect(await linkLine($)).toBeUndefined()
})
