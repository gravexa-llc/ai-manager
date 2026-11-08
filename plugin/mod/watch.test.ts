import type { On } from 'claude-code'
import { expect, test } from 'claude-code/testing'

import { CONTEXT, SESSION_ID } from './fixtures'
import { COMMAND, OPTIONS, START, TOKEN, world, type Reply, type Routes } from './testkit'

// /aim watch and /aim unwatch: the mod sends the session's id
// to aimd with the token and shows aimd's one-line answer.

type World = ReturnType<typeof world>

function watched(message: string, watching = true) {
  return {
    session_id: SESSION_ID,
    watched: watching,
    changed: true,
    known: true,
    monitored: watching,
    monitored_why: watching ? [{ reason: 'watch', text: 'you asked to watch it' }] : [],
    monitor: 'opt-in',
    message,
  }
}

const ROUTES: Routes = {
  'GET /api/context': { status: 200, body: CONTEXT },
  'POST /api/sessions/watch': {
    status: 200,
    body: watched('Watching this session: it shows under Needs you and in notifications when it waits on you.'),
  },
  'DELETE /api/sessions/watch': {
    status: 200,
    body: watched('Stopped watching this session; it stays monitored: registered plan aim.', false),
  },
}

function session(on: On, routes: Routes = ROUTES, sessionId?: Error) {
  const w = sessionId === undefined ? world(on, routes) : world(on, routes, undefined, sessionId)
  on('session.start', (_, e) => ({ cwd: e.cwd }))
  return w
}

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

test('/aim watch sends this session to aimd and shows its answer', { options: OPTIONS }, async ($, on) => {
  const w = session(on)
  await $.session.start(START)
  const out = await $.command.run({ command: 'aim', args: 'watch', ...COMMAND })
  expect(out.text).toBe('ai-manager: Watching this session: it shows under Needs you and in notifications when it waits on you.')
  const sent = w.sent('POST /api/sessions/watch')
  expect(sent.length).toBe(1)
  expect(sent[0]?.body).toEqual({ session_id: SESSION_ID })
  expect(sent[0]?.headers['X-AIM-Token']).toBe(TOKEN)
  expect(w.opened).toEqual([])
})

test('/aim unwatch sends DELETE, in any case and spacing', { options: OPTIONS }, async ($, on) => {
  const w = session(on)
  await $.session.start(START)
  const out = await $.command.run({ command: 'aim', args: '  UnWatch ', ...COMMAND })
  expect(out.text).toBe('ai-manager: Stopped watching this session; it stays monitored: registered plan aim.')
  const sent = w.sent('DELETE /api/sessions/watch')
  expect(sent.length).toBe(1)
  expect(sent[0]?.body).toEqual({ session_id: SESSION_ID })
  expect(sent[0]?.headers['X-AIM-Token']).toBe(TOKEN)
  expect(w.sent('POST /api/sessions/watch').length).toBe(0)
})

test('/aim registers its arguments and still opens the dashboard bare', { options: OPTIONS }, async ($, on) => {
  const w = session(on)
  await $.session.start(START)
  expect(w.registered.hints.aim).toBe('[watch | unwatch]')
  const out = await $.command.run({ command: 'aim', args: '', ...COMMAND })
  // This session's plan in the app (band.test.ts has the cases).
  expect(out.text).toBe('Opened plan aim in the ai-manager dashboard: http://127.0.0.1:17999/app/plans/aim')
  expect(w.sent('POST /api/sessions/watch').length).toBe(0)
})

test('/aim with something else says how to use it, and calls nothing', { options: OPTIONS }, async ($, on) => {
  const w = session(on)
  await $.session.start(START)
  for (const args of ['wach', 'watch now', 'unwatch watch']) {
    const out = await $.command.run({ command: 'aim', args, ...COMMAND })
    expect(out.text).toContain('Usage: /aim')
  }
  expect(w.sent('POST /api/sessions/watch').length + w.sent('DELETE /api/sessions/watch').length).toBe(0)
  expect(w.opened).toEqual([])
})

test("/aim watch passes on what aimd refuses", { options: OPTIONS }, async ($, on) => {
  session(on, {
    ...ROUTES,
    'POST /api/sessions/watch': { status: 401, body: { error: 'missing or wrong X-AIM-Token' } },
    'DELETE /api/sessions/watch': { status: 500, body: { error: "aimd couldn't save the watch; see aimd.log" } },
  })
  await $.session.start(START)
  const watch = await $.command.run({ command: 'aim', args: 'watch', ...COMMAND })
  expect(watch.text).toBe("ai-manager couldn't watch this session: missing or wrong X-AIM-Token")
  const unwatch = await $.command.run({ command: 'aim', args: 'unwatch', ...COMMAND })
  expect(unwatch.text).toBe("ai-manager couldn't unwatch this session: aimd couldn't save the watch; see aimd.log")
})

for (const failure of ['refuse', 'hang'] as const) {
  const name = failure === 'refuse' ? 'down' : 'slow'

  test(`aimd ${name}: /aim watch and unwatch say so within ~1 s, and fail nothing`, { options: OPTIONS }, async ($, on) => {
    const reply: Reply = failure
    const w = session(on, { 'GET /api/context': reply, 'POST /api/sessions/watch': reply, 'DELETE /api/sessions/watch': reply })
    await within(w, 1000, $.session.start(START))
    const watch = await within(w, 1000, $.command.run({ command: 'aim', args: 'watch', ...COMMAND }))
    expect(watch.text).toBe(
      "ai-manager: aimd isn't answering at http://127.0.0.1:17999, so nothing changed. Start it with `aim up`, then run /aim watch again.",
    )
    const unwatch = await within(w, 1000, $.command.run({ command: 'aim', args: 'unwatch', ...COMMAND }))
    expect(unwatch.text).toContain('then run /aim unwatch again.')
  })
}

test('/aim watch without a session id says so and calls nothing', { options: OPTIONS }, async ($, on) => {
  const w = session(on, ROUTES, new Error('no session yet'))
  await $.session.start(START)
  const out = await $.command.run({ command: 'aim', args: 'watch', ...COMMAND })
  expect(out.text).toBe("ai-manager couldn't tell this session's id, so it can't watch it.")
  expect(w.sent('POST /api/sessions/watch').length).toBe(0)
})
