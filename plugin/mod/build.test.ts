// The plugin build session_start reports (build.ts): from the plugin's
// own folder, time-boxed, "-dirty" with changes, left out when git can't
// tell, and the same after /clear.

import type { On } from 'claude-code'
import { expect, test, type Engine } from 'claude-code/testing'

import { BUILD_TIMEOUT_MS } from './build'
import { CONTEXT, REPO, SESSION_ID } from './fixtures'
import { OPTIONS, START, world } from './testkit'

type World = ReturnType<typeof world>
type Ev = Record<string, unknown> & { kind: string; session_id: string }

function reporting(on: On): World {
  const w = world(on, { 'GET /api/context': { status: 200, body: CONTEXT }, 'POST /events': { status: 200, body: {} } })
  on('session.start', (_, e) => ({ cwd: e.cwd }))
  on('session.end', (_, e) => ({ sessionId: e.sessionId }))
  on('classic.SessionStart', () => ({}))
  return w
}

async function begin($: Engine, w: World, id = SESSION_ID): Promise<void> {
  await $.classic.SessionStart({ session_id: id, source: 'startup' })
  await $.session.start(START)
  await w.clock.settle()
}

function starts(w: World): Ev[] {
  return w
    .sent('POST /events')
    .flatMap(r => (r.body?.events as Ev[] | undefined) ?? [])
    .filter(e => e.kind === 'session_start')
}

test('session_start carries the plugin build, worked out in the plugin folder, each git call time-boxed', { options: OPTIONS }, async ($, on) => {
  const w = reporting(on)
  await begin($, w)
  await w.clock.advance(1000)
  const [start] = starts(w)
  expect(start?.plugin_build).toBe('v1.0.0-3-gabc1234')
  // Three git calls (the last change, the status, describe), all in the
  // plugin's own folder ($.plugin.root), not the session's.
  const root = start?.plugin_root
  expect(typeof root === 'string' && root !== '' && root !== REPO).toBe(true)
  expect(w.build.calls.length).toBe(3)
  expect(w.build.calls.every(c => c.cwd === root && c.timeoutMs === BUILD_TIMEOUT_MS)).toBe(true)
})

test('a plugin folder with changes is a -dirty build', { options: OPTIONS }, async ($, on) => {
  const w = reporting(on)
  w.build.dirty = true
  await begin($, w)
  await w.clock.advance(1000)
  expect(starts(w)[0]?.plugin_build).toBe('v1.0.0-3-gabc1234-dirty')
})

for (const git of ['fail', 'reject'] as const) {
  test(`git ${git === 'fail' ? 'failing' : 'timing out'} in the folder: no build, the rest of session_start as ever`, { options: OPTIONS }, async ($, on) => {
    const w = reporting(on)
    w.build.git = git
    await begin($, w)
    await w.clock.advance(1000)
    const [start] = starts(w)
    expect(start).toMatchObject({ session_id: SESSION_ID, cwd: REPO, branch: 'main', host: 'laptop' })
    expect(typeof start?.plugin_root).toBe('string')
    expect(start?.plugin_build).toBeUndefined()
  })
}

test('a describe that says nothing usable is no build', { options: OPTIONS }, async ($, on) => {
  const w = reporting(on)
  w.build.describe = ''
  await begin($, w)
  await w.clock.advance(1000)
  expect(starts(w)[0]?.plugin_build).toBeUndefined()
})

test('after /clear the new session_start carries the same build', { options: OPTIONS }, async ($, on) => {
  const w = reporting(on)
  await begin($, w, 'before-clear')
  await $.session.end({ reason: 'clear', sessionId: 'before-clear', resume: { id: 'before-clear' } })
  await w.clock.settle()
  await $.classic.SessionStart({ session_id: 'after-clear', source: 'clear' })
  await w.clock.advance(1000)
  const [first, second] = starts(w)
  expect(second?.session_id).toBe('after-clear')
  expect(second?.plugin_build).toBe('v1.0.0-3-gabc1234')
  expect(second?.plugin_root).toBe(first?.plugin_root)
  // Worked out once, at the start.
  expect(w.build.calls.length).toBe(3)
})
