// aimdUrl must be this machine: which addresses count, and that to any
// other the mod sends nothing (no token, no events, no plan calls), opens
// nothing, says so once, and still passes every event on unchanged.

import type { On } from 'claude-code'
import { expect, test, type Engine } from 'claude-code/testing'

import { NOT_LOCAL, isLoopback, localBase, request, settings } from './aimd'
import { CONTEXT, SPAWN, SPAWNED, SUMMARY, bashResult } from './fixtures'
import { BAND, COMMAND, OPTIONS, START, TOKEN, world, type Routes } from './testkit'

const TOOL = 'mcp__ai-manager__plan_status'

/** aimdUrl values the mod talks to, and the base it fetches from for each. */
const LOCAL: [string, string][] = [
  ['http://127.0.0.1:7777', 'http://127.0.0.1:7777'],
  ['http://127.0.0.1:7777/', 'http://127.0.0.1:7777'],
  ['http://127.0.0.1:7777///', 'http://127.0.0.1:7777'],
  ['http://127.0.0.1', 'http://127.0.0.1'],
  ['http://localhost:7777', 'http://localhost:7777'],
  ['http://localhost:7778/', 'http://localhost:7778'],
  ['https://localhost:7777', 'https://localhost:7777'],
  ['HTTP://LOCALHOST:7777', 'http://localhost:7777'],
  ['Http://LocalHost:7777/', 'http://localhost:7777'],
  ['http://[::1]:7777', 'http://[::1]:7777'],
  ['http://[::1]/', 'http://[::1]'],
  ['  http://127.0.0.1:7777  ', 'http://127.0.0.1:7777'],
  // A query or fragment is not part of the base.
  ['http://127.0.0.1:7777/?x=1#y', 'http://127.0.0.1:7777'],
]

/** aimdUrl values the mod sends nothing to. */
const NOT: string[] = [
  'http://aimd.example:7777',
  'https://aimd.example',
  'http://127.0.0.1@evil.example',
  'http://127.0.0.1:7777@evil.example/',
  'http://localhost@evil.example:7777',
  'http://user:secret@127.0.0.1:7777',
  'http://user@localhost:7777',
  'http://127.0.0.1.evil.example',
  'http://127.0.0.1.evil.example:7777/',
  'http://localhost.evil.example',
  'http://evil.example/127.0.0.1',
  'http://evil.example/?localhost',
  'http://evil.example#127.0.0.1',
  'http://notlocalhost:7777',
  'http://localhost.:7777',
  'http://192.0.2.10:7777',
  'http://0.0.0.0:7777',
  'http://[2001:db8::1]:7777',
  'ftp://127.0.0.1:7777',
  'file:///tmp/aimd',
  'ws://localhost:7777',
  'javascript:alert(1)',
  '127.0.0.1:7777',
  'localhost:7777',
  'localhost',
  '//127.0.0.1:7777',
  'http://',
  'http://127.0.0.1:port',
  'not a url',
  '',
]

test('which aimdUrl values are local: a table', () => {
  for (const [raw, base] of LOCAL) {
    expect(isLoopback(raw), raw).toBe(true)
    expect(localBase(raw), raw).toBe(base)
    expect(settings({ aimdUrl: raw }), raw).toMatchObject({ local: true, url: base })
  }
  for (const raw of NOT) {
    expect(isLoopback(raw), raw).toBe(false)
    expect(localBase(raw), raw).toBeUndefined()
  }
  // The setting unset or blank is the default, which is local.
  for (const unset of [{}, { aimdUrl: '' }, { aimdUrl: '   ' }, { aimdUrl: 7777 }]) {
    expect(settings(unset)).toMatchObject({ local: true, url: 'http://127.0.0.1:7777' })
  }
  // Not local: the settings keep none of it (credentials included).
  for (const raw of NOT.filter(one => one.trim() !== '')) {
    const s = settings({ token: TOKEN, aimdUrl: raw })
    expect(s, raw).toMatchObject({ local: false, url: '' })
  }
})

const ROUTES: Routes = {
  'GET /api/context': { status: 200, body: CONTEXT },
  'GET /api/plans/aim/summary': { status: 200, body: { ...SUMMARY, needs_you: 0 } },
  'POST /events': { status: 200, body: {} },
  'POST /api/links/spawn': { status: 201, body: {} },
  'POST /api/sessions/watch': { status: 200, body: { message: 'watching this session.' } },
  'POST /api/plans': { status: 201, body: { created: true, slug: 'sharing', title: 'x', path: '/tmp/probe/docs/sharing-plan.md', packages: 3 } },
}

/** A session with core beneath it, recording what reaches core. */
function below(on: On) {
  const w = world(on, ROUTES)
  const seen: { prompts: string[]; briefs: string[]; commands: string[] } = { prompts: [], briefs: [], commands: [] }
  on('session.start', (_, e) => ({ cwd: e.cwd }))
  on('prompt.submit', (_, e) => {
    seen.prompts.push(e.text)
    return { text: e.text, origin: e.origin }
  })
  on('turn.complete', (_, e) => ({ text: e.answer }))
  on('agent.spawn', (_, e) => {
    seen.briefs.push(e.prompt)
    return SPAWNED
  })
  on('tool.call', { tool: 'Bash' }, (_, e) => {
    seen.commands.push(e.command)
    return bashResult('ok\n')
  })
  return { ...w, seen }
}

const BRIEF = 'Package: aim · C4\nBuild it.'
const PR_CREATE = 'gh pr create --title "Mod v1 (C4)" --body "What changed."'

/** Everything the mod does that would reach aimd, the dashboard included. */
async function everything($: Engine, w: ReturnType<typeof below>) {
  await $.session.start(START)
  await w.clock.settle()
  await $.prompt.submit({ text: 'Dispatch C4.', wait: false, origin: { kind: 'composer' } })
  const spawned = await $.agent.spawn({ ...SPAWN, prompt: BRIEF, description: 'C4' })
  const ran = await $.tool.call({ tool: 'Bash', command: 'go test ./...' })
  const pr = await $.tool.call({ tool: 'Bash', command: PR_CREATE, agentId: SPAWNED.agentId })
  await $.turn.complete({ answer: 'Done.', durationMs: 9, isAborted: false, turnId: 't1', reason: 'answer' })
  const status = await $.tool.call({ tool: TOOL })
  const aim = await $.command.run({ command: 'aim', args: '', ...COMMAND })
  const watch = await $.command.run({ command: 'aim', args: 'watch', ...COMMAND })
  const registered = await $.command.run({ command: 'plan-register', args: 'docs/sharing-plan.md', ...COMMAND })
  const ui = await $.ui.mount({ ...BAND, surface: 'terminal' })
  const line = await ui.find({ type: 'Button' })
  await ui.unmount()
  // Long enough for every retry, backoff and poll to come round.
  await w.clock.advance(120_000)
  await w.clock.advance(600_000)
  // A second session start (as /clear makes) doesn't say it again.
  await $.session.start(START)
  await w.clock.settle()
  return { spawned, ran, pr, status, aim, watch, registered, line }
}

for (const aimdUrl of ['http://aimd.example:7777', 'http://127.0.0.1@evil.example', 'http://user:secret@127.0.0.1.evil.example/', 'not a url']) {
  test(`aimdUrl ${aimdUrl}: nothing is fetched or opened, one toast, and every event passes through`, { options: { ...OPTIONS, aimdUrl } }, async ($, on) => {
    const w = below(on)
    const out = await everything($, w)

    // Nothing at all went out: no token, no events, no plan calls, no browser.
    expect(w.requests).toEqual([])
    expect(w.opened).toEqual([])

    // Said once, naming the setting and neither the URL nor the token.
    expect(w.toasts).toEqual([NOT_LOCAL])
    const said = JSON.stringify([w.toasts, out.status, out.aim, out.watch, out.registered])
    for (const secret of [TOKEN, 'evil.example', 'aimd.example', 'secret']) expect(said, secret).not.toContain(secret)
    expect(NOT_LOCAL).toContain('aimdUrl')

    // Fail open: core got each event as it came and its answers came back.
    expect(w.seen.prompts).toEqual(['Dispatch C4.'])
    expect(w.seen.briefs).toEqual([BRIEF])
    expect(w.seen.commands).toEqual(['go test ./...', PR_CREATE])
    expect(out.spawned.deny).toBeUndefined()
    expect(out.spawned.agentId).toBe(SPAWNED.agentId)
    expect(out.ran.deny).toBeUndefined()
    expect(out.pr.deny).toBeUndefined()

    // What the user asked for by name says why it did nothing.
    expect(out.status.deny).toBe(NOT_LOCAL)
    expect(out.aim.text).toBe(NOT_LOCAL)
    expect(out.watch.text).toBe(NOT_LOCAL)
    expect(out.registered.text).toContain("couldn't register")
    expect(out.line).toBeUndefined()
  })
}

test('the same session with a local aimdUrl does call aimd, with the token, and says nothing', { options: OPTIONS }, async ($, on) => {
  const w = below(on)
  const out = await everything($, w)
  for (const route of ['GET /api/context', 'POST /events', 'POST /api/links/spawn', 'POST /api/sessions/watch', 'POST /api/plans']) {
    expect(w.sent(route).length, route).toBeGreaterThan(0)
  }
  expect(w.sent('POST /events')[0]?.headers['X-AIM-Token']).toBe(TOKEN)
  expect(w.opened).toEqual(['open http://127.0.0.1:17999/app/plans/aim'])
  expect(w.toasts.filter(t => t === NOT_LOCAL)).toEqual([])
  expect(out.status.deny).toBeUndefined()
  expect(w.seen.commands[0]).toBe('go test ./...')
})

test('request() to an address that is not local answers without fetching, and never throws', { options: OPTIONS }, async ($, on) => {
  const w = world(on, {})
  let got: unknown
  on('session.start', async ($$, e) => {
    for (const aimdUrl of ['http://aimd.example:7777', 'http://user:secret@127.0.0.1@evil.example']) {
      got = await request($$, settings({ token: TOKEN, aimdUrl }), 'POST', '/events', { events: [] })
    }
    return { cwd: e.cwd }
  })
  await $.session.start(START)
  expect(got).toEqual({ ok: false, status: 0, error: "aimdUrl isn't a local address, so nothing was sent" })
  expect(w.requests.filter(r => r.path === '/events')).toEqual([])
})
