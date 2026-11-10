// Provisional packages: a brief whose Package: line names a package its registered plan's
// file has no row for. The spawn goes ahead and links the agent as
// provisional, and the lead gets a note to add the row; requirePlanRow
// refuses the spawn instead. Anything else (an unregistered slug, no
// Package: line, a package the file has, aimd down) is as before.

import type { On } from 'claude-code'
import { expect, mock, test } from 'claude-code/testing'

import { CONTEXT, REPO, SESSION_ID, SPAWN, SPAWNED } from './fixtures'
import { OPTIONS, START, world, type Routes } from './testkit'

const NOTE = "[ai-manager] Package W12 isn't in ROADMAP.md's tables. Add its row so ai-manager tracks it; it shows as provisional until then."

/** The context with the plan file's path, as aimd sends it. */
const WITH_PATH = {
  ...CONTEXT,
  plans: CONTEXT.plans.map(p => ({ ...p, path: p.slug === 'aim' ? `${REPO}/ROADMAP.md` : `${REPO}/docs/sharing-plan.md` })),
}

/** The same once aimd has W12 as a provisional package. */
const WITH_W12 = {
  ...WITH_PATH,
  plans: WITH_PATH.plans.map(p =>
    p.slug !== 'aim'
      ? p
      : {
          ...p,
          packages: [
            ...p.packages,
            { id: 'W12', title: 'Unregister', lifecycle: 'working', suggested_branch: 'aim/w12-unregister', plan_line: 'Plan: aim · W12', provisional: true },
          ],
        },
  ),
}

const ROUTES: Routes = {
  'GET /api/context': { status: 200, body: WITH_PATH },
  'POST /api/links/spawn': { status: 201, body: { links: [], packages: [] } },
}

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

const BRIEF = 'Package: aim · W12\n\nAdd an Unregister button to the plan page.'

test('an unknown ID: the spawn goes ahead, linked as provisional, and the lead gets the note', { options: OPTIONS }, async ($, on) => {
  const w = lead(on)
  await $.session.start(START)
  const out = await $.agent.spawn({ ...SPAWN, prompt: BRIEF, description: 'W12 unregister button' })
  await w.clock.settle()

  expect(out.agentId).toBe(SPAWNED.agentId)
  // The conventions, so the branch and the PR name it too.
  expect(w.briefs[0]).toContain('[ai-manager] This agent works on package aim · W12 (Unregister button)')
  expect(w.briefs[0]).toContain('Suggested branch: aim/w12-unregister-button')
  expect(w.briefs[0]).toContain('Put this line in the pull request body: Plan: aim · W12')
  expect(w.sent('POST /api/links/spawn')[0]?.body).toEqual({
    session_id: SESSION_ID, agent_id: SPAWNED.agentId, plan: 'aim', pkg: 'W12', provisional: true, title: 'W12 unregister button',
  })
  // The cached context said W12 was missing: aimd was asked again first.
  expect(w.sent('GET /api/context').length).toBe(2)
  const notes = w.notes()
  expect(notes.length).toBe(1)
  expect(notes[0]?.agentId).toBeUndefined()
  expect(notes[0]?.text).toContain(NOTE)

  // A second agent for it: linked the same way, no second note.
  await $.agent.spawn({ ...SPAWN, prompt: BRIEF, description: 'W12 again' })
  await w.clock.settle()
  expect(w.sent('POST /api/links/spawn').length).toBe(2)
  expect(w.notes().length).toBe(1)
})

test("a provisional package's title is redacted before it goes to aimd", { options: OPTIONS }, async ($, on) => {
  const w = lead(on, { ...ROUTES, 'GET /api/context': { status: 200, body: WITH_W12 } })
  await $.session.start(START)
  await $.agent.spawn({ ...SPAWN, prompt: BRIEF, description: 'W12 sync with GITHUB_TOKEN=ghp_0123456789abcdefABCDEF0123456789abcd' })
  await w.clock.settle()
  expect(w.sent('POST /api/links/spawn')[0]?.body).toMatchObject({ pkg: 'W12', provisional: true, title: 'W12 sync with GITHUB_TOKEN=[redacted]' })
})

test('a provisional package aimd has: linked to it, and the lead still told', { options: OPTIONS }, async ($, on) => {
  const w = lead(on, { ...ROUTES, 'GET /api/context': { status: 200, body: WITH_W12 } })
  await $.session.start(START)
  await $.agent.spawn({ ...SPAWN, prompt: BRIEF, description: 'more W12' })
  await w.clock.settle()
  expect(w.briefs[0]).toContain('Suggested branch: aim/w12-unregister')
  expect(w.sent('POST /api/links/spawn')[0]?.body).toMatchObject({ plan: 'aim', pkg: 'W12', provisional: true, title: 'more W12' })
  expect(w.notes()[0]?.text).toContain(NOTE)
})

test('the row landed since the context was cached: no note', { options: OPTIONS }, async ($, on) => {
  let context: unknown = WITH_PATH
  const w = lead(on, { ...ROUTES, 'GET /api/context': () => ({ status: 200, body: context }) })
  await $.session.start(START)
  // The lead adds W12's row; aimd re-reads the plan.
  context = {
    ...WITH_PATH,
    plans: WITH_PATH.plans.map(p =>
      p.slug !== 'aim' ? p : { ...p, packages: [...p.packages, { id: 'W12', title: 'Unregister', lifecycle: 'ready', suggested_branch: 'aim/w12-unregister', plan_line: 'Plan: aim · W12' }] },
    ),
  }
  await $.agent.spawn({ ...SPAWN, prompt: BRIEF, description: 'W12' })
  await w.clock.settle()
  expect(w.sent('POST /api/links/spawn')[0]?.body).toEqual({ session_id: SESSION_ID, agent_id: SPAWNED.agentId, plan: 'aim', pkg: 'W12' })
  expect(w.notes().length).toBe(0)
})

test('a subagent that spawns for an unknown ID gets the note itself', { options: OPTIONS }, async ($, on) => {
  const w = lead(on)
  await $.session.start(START)
  await $.agent.spawn({ ...SPAWN, prompt: BRIEF, description: 'nested', parentAgentId: 'a61cc06fc9f51079d' })
  await w.clock.settle()
  expect(w.notes()[0]?.agentId).toBe('a61cc06fc9f51079d')
})

test('an unknown ID in a plan of another repo: provisional too', { options: OPTIONS }, async ($, on) => {
  const w = lead(on, {
    ...ROUTES,
    'GET /api/plans/dark': {
      status: 200,
      body: { slug: 'dark', title: 'Dark', path: '/elsewhere/docs/dark-plan.md', packages: [{ id: 'T2', title: 'Palette', lifecycle: 'ready', suggested_branch: 'dark/t2-palette', plan_line: 'Plan: dark · T2' }] },
    },
  })
  await $.session.start(START)
  await $.agent.spawn({ ...SPAWN, prompt: 'Package: dark · T9\nGo.', description: 'contrast pass' })
  await w.clock.settle()
  expect(w.sent('POST /api/links/spawn')[0]?.body).toMatchObject({ plan: 'dark', pkg: 'T9', provisional: true, title: 'contrast pass' })
  expect(w.notes()[0]?.text).toContain("Package T9 isn't in dark-plan.md's tables.")
})

test('requirePlanRow: the spawn is refused with what to do', { options: { ...OPTIONS, requirePlanRow: true } }, async ($, on) => {
  const w = lead(on)
  await $.session.start(START)
  const out = await $.agent.spawn({ ...SPAWN, prompt: BRIEF, description: 'W12' })
  await w.clock.settle()
  expect(out.deny).toBe(
    "Package W12 isn't in ROADMAP.md's tables. Add its row so ai-manager tracks it, then spawn the agent again (ai-manager's requirePlanRow setting is on).",
  )
  expect(w.briefs.length).toBe(0)
  expect(w.sent('POST /api/links/spawn').length).toBe(0)
  expect(w.notes().length).toBe(0)

  // A package the file has still spawns.
  const ok = await $.agent.spawn({ ...SPAWN, prompt: 'Package: aim · C4\nGo.', description: 'C4' })
  expect(ok.deny).toBeUndefined()
  expect(w.briefs.length).toBe(1)
})

test('requirePlanRow with aimd down: nothing is refused', { options: { ...OPTIONS, requirePlanRow: true } }, async ($, on) => {
  const w = lead(on, { 'GET /api/context': 'refuse', 'POST /api/links/spawn': 'refuse' })
  await $.session.start(START)
  const out = await $.agent.spawn({ ...SPAWN, prompt: BRIEF, description: 'W12' })
  await w.clock.settle()
  expect(out.deny).toBeUndefined()
  expect(w.briefs).toEqual([BRIEF])
  expect(w.notes().length).toBe(0)
})

test('aimd goes down after the context was cached: no note, no refusal', { options: { ...OPTIONS, requirePlanRow: true } }, async ($, on) => {
  let up = true
  const w = lead(on, { ...ROUTES, 'GET /api/context': () => (up ? { status: 200, body: WITH_PATH } : 'refuse') })
  await $.session.start(START)
  up = false
  const out = await $.agent.spawn({ ...SPAWN, prompt: BRIEF, description: 'W12' })
  await w.clock.settle()
  expect(out.deny).toBeUndefined()
  expect(w.briefs).toEqual([BRIEF])
  expect(w.sent('POST /api/links/spawn').length).toBe(0)
  expect(w.notes().length).toBe(0)
})

test('aimd slow on the second look: the spawn goes on as if nothing were missing', { options: { ...OPTIONS, requirePlanRow: true } }, async ($, on) => {
  let calls = 0
  const w = lead(on, { ...ROUTES, 'GET /api/context': () => (++calls === 1 ? { status: 200, body: WITH_PATH } : 'hang') })
  await $.session.start(START)
  let done = false
  const spawned = $.agent.spawn({ ...SPAWN, prompt: BRIEF, description: 'W12' }).then(out => {
    done = true
    return out
  })
  await w.clock.settle()
  if (!done) await w.clock.advance(1_000)
  expect(done, 'still waiting after 1 s').toBe(true)
  const out = await spawned
  expect(out.deny).toBeUndefined()
  expect(w.briefs).toEqual([BRIEF])
  expect(w.notes().length).toBe(0)
})

test('left alone: an unregistered slug, no Package: line, a package the file has', { options: { ...OPTIONS, requirePlanRow: true } }, async ($, on) => {
  const w = lead(on)
  await $.session.start(START)
  for (const prompt of ['Package: nope · Z1\nGo.', 'Add the unregister button (W12 in the plan).', 'Package: aim · C4\nGo.', 'Package: aim · notanid\nGo.']) {
    const out = await $.agent.spawn({ ...SPAWN, prompt, description: 'work' })
    expect(out.deny).toBeUndefined()
  }
  await w.clock.settle()
  expect(w.briefs.length).toBe(4)
  for (const sent of w.sent('POST /api/links/spawn')) expect(sent.body?.provisional).toBeUndefined()
  expect(w.sent('POST /api/links/spawn').map(r => r.body?.pkg)).toEqual(['C4'])
  expect(w.notes().length).toBe(0)
})

test('an error in the hook fails open: the brief is passed on unchanged', { options: { ...OPTIONS, requirePlanRow: true } }, async ($, on) => {
  // A context aimd shouldn't send (a package without an id) makes the
  // lookup throw: the hook's catch passes e on as it came.
  const broken = { ...WITH_PATH, plans: [{ slug: 'aim', title: 'x', path: `${REPO}/ROADMAP.md`, band: '', packages: [{ title: 'no id' }] }] }
  const w = lead(on, { ...ROUTES, 'GET /api/context': { status: 200, body: broken } })
  await $.session.start(START)
  const out = await $.agent.spawn({ ...SPAWN, prompt: BRIEF, description: 'W12' })
  await w.clock.settle()
  expect(out.deny).toBeUndefined()
  expect(out.agentId).toBe(SPAWNED.agentId)
  expect(w.briefs).toEqual([BRIEF])
  expect(w.notes().length).toBe(0)
})
