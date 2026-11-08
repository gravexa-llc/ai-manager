import type { On } from 'claude-code'
import { expect, test, type Engine } from 'claude-code/testing'

import { CONTEXT, LINKED_CONTEXT, SESSION_ID, SPAWN, SPAWNED, SUMMARY, WORKTREE, bashResult } from './fixtures'
import { OPTIONS, START, world, type Reply, type Routes } from './testkit'

const PR = 'https://github.com/example-user/probe/pull/21'
const AGENT = SPAWNED.agentId
const DENY = "Add 'Plan: aim · C4' to the PR body and run it again."

const ROUTES: Routes = {
  'GET /api/context': { status: 200, body: CONTEXT },
  'POST /api/links/spawn': { status: 201, body: { links: [], packages: [] } },
  'POST /api/links/pr': { status: 201, body: { links: [], packages: [] } },
}

/** A lead that spawned the C4 agent; core's Bash and MCP tools answer with the PR URL. */
async function linked($: Engine, on: On, routes: Routes = ROUTES) {
  const w = world(on, routes)
  const ran: string[] = []
  on('session.start', (_, e) => ({ cwd: e.cwd }))
  on('agent.spawn', () => SPAWNED)
  on('tool.call', { tool: 'Bash' }, (_, e) => {
    ran.push(e.command)
    return bashResult(`${PR}\n`)
  })
  on('tool.call', { tool: /create_pull_request$/ }, () => {
    ran.push('mcp')
    return { result: { content: [{ type: 'text', text: JSON.stringify({ number: 21, html_url: PR, url: 'https://api.github.com/repos/example-user/probe/pulls/21' }) }] } }
  })
  await $.session.start(START)
  await $.agent.spawn({ ...SPAWN, prompt: 'Package: aim · C4\nBuild it.', description: 'C4' })
  await w.clock.settle()
  return { ...w, ran }
}

/** Runs a Bash call in the C4 agent's loop, another agent's, or the main loop's. */
async function bash($: Engine, command: string, loop: string = AGENT) {
  return loop === MAIN ? $.tool.call({ tool: 'Bash', command }) : $.tool.call({ tool: 'Bash', command, agentId: loop })
}

const MAIN = 'main'

const denied = (out: { deny?: string; text?: string }) => out.deny ?? (out as { text?: string }).text

test('an inline --body without the Plan: line is refused, with what to do', { options: OPTIONS }, async ($, on) => {
  const w = await linked($, on)
  const out = await bash($, 'gh pr create --title "Mod v1: acting (C4)" --body "What changed: the mod."')
  expect(denied(out)).toContain(DENY)
  expect(w.ran).toEqual([])
  expect(w.sent('POST /api/links/pr').length).toBe(0)
})

test('an inline --body with the line runs, and the PR is linked from its output', { options: OPTIONS }, async ($, on) => {
  const w = await linked($, on)
  const out = await bash($, 'gh pr create --title "Mod v1 (C4)" --body "Plan: aim · C4\n\nWhat changed."')
  await w.clock.settle()
  expect(out.deny).toBeUndefined()
  expect(w.ran.length).toBe(1)
  expect(w.sent('POST /api/links/pr')[0]?.body).toEqual({ session_id: SESSION_ID, agent_id: AGENT, url: PR })
})

test('-b, --body=, and $\'...\' quoting are read', { options: OPTIONS }, async ($, on) => {
  const w = await linked($, on)
  expect((await bash($, "gh pr create -t x -b 'Plan: aim · C4'")).deny).toBeUndefined()
  expect((await bash($, 'gh pr create -t x --body="Plan: aim · C4"')).deny).toBeUndefined()
  expect((await bash($, "gh pr create -t x --body $'Intro\\nPlan: aim · C4\\n'")).deny).toBeUndefined()
  expect(denied(await bash($, 'gh pr create -t x -b "no line here"'))).toContain(DENY)
  // A literal \\n in double quotes stays on one line, as GitHub would show it.
  expect(denied(await bash($, 'gh pr create -t x --body "Intro\\nPlan: aim · C4"'))).toContain(DENY)
  expect(w.ran.length).toBe(3)
})

test('a heredoc body is read from the command', { options: OPTIONS }, async ($, on) => {
  const w = await linked($, on)
  const ok = [
    'gh pr create --title "Mod v1: acting (C4)" --body "$(cat <<\'EOF\'',
    'Plan: aim · C4',
    '',
    "## What changed",
    "Don't merge (it's a draft).",
    'EOF',
    ')"',
  ].join('\n')
  expect((await bash($, ok)).deny).toBeUndefined()
  const missing = ['gh pr create --title t --body "$(cat <<EOF', "It's done.", 'EOF', ')"'].join('\n')
  expect(denied(await bash($, missing))).toContain(DENY)
  const stdin = ['gh pr create --title t --body-file - <<\'EOF\'', '* Plan: aim · C4', 'EOF'].join('\n')
  expect((await bash($, stdin)).deny).toBeUndefined()
  expect(w.ran.length).toBe(2)
})

test('-F / --body-file: the file is read and checked', { options: OPTIONS }, async ($, on) => {
  const w = await linked($, on)
  w.files['/tmp/probe/body.md'] = 'Plan: aim · C4\n\nWhat changed.'
  w.files['/tmp/probe/bare.md'] = 'What changed.'
  expect((await bash($, 'gh pr create -t x -F body.md')).deny).toBeUndefined()
  expect((await bash($, 'gh pr create -t x --body-file=/tmp/probe/body.md')).deny).toBeUndefined()
  expect(denied(await bash($, 'gh pr create -t x --body-file bare.md'))).toContain(
    "Add 'Plan: aim · C4' to the PR body (bare.md) and run it again.",
  )
  // A file the mod can't read is let through: the check fails open.
  expect((await bash($, 'gh pr create -t x -F missing.md')).deny).toBeUndefined()
  expect(w.ran.length).toBe(3)
})

test('--fill, --template and --web are allowed', { options: OPTIONS }, async ($, on) => {
  const w = await linked($, on)
  for (const command of ['gh pr create --fill', 'gh pr create -f --draft', 'gh pr create --template .github/pr.md -t x', 'gh pr create --web', 'gh pr create -t x']) {
    expect((await bash($, command)).deny).toBeUndefined()
  }
  expect(w.ran.length).toBe(5)
})

test("a Plan: line for another package doesn't pass", { options: OPTIONS }, async ($, on) => {
  await linked($, on)
  expect(denied(await bash($, 'gh pr create -t x -b "Plan: aim · C5"'))).toContain(DENY)
})

test('other commands and unlinked agents are not inspected', { options: OPTIONS }, async ($, on) => {
  const w = await linked($, on)
  expect((await bash($, 'gh pr view 21 --json body')).deny).toBeUndefined()
  expect((await bash($, 'echo "gh pr creates nothing"')).deny).toBeUndefined()
  expect((await bash($, 'gh pr create -t x -b "no line"', 'a0000000000000000')).deny).toBeUndefined()
  expect((await bash($, 'gh pr create -t x -b "no line"', MAIN)).deny).toBeUndefined()
  await w.clock.settle()
  expect(w.ran.length).toBe(4)
  expect(w.sent('POST /api/links/pr').length).toBe(0)
})

test('prCheck off: no check, the PR is still linked', { options: { ...OPTIONS, prCheck: false } }, async ($, on) => {
  const w = await linked($, on)
  expect((await bash($, 'gh pr create -t x -b "no line"')).deny).toBeUndefined()
  await w.clock.settle()
  expect(w.sent('POST /api/links/pr')[0]?.body).toMatchObject({ agent_id: AGENT, url: PR })
})

test('the GitHub MCP create-PR tool: the same check on body, then the URL is linked', { options: OPTIONS }, async ($, on) => {
  const w = await linked($, on)
  const args = { owner: 'example-user', repo: 'probe', title: 'Mod (C4)', head: 'aim/c4-mod-v1-acting', base: 'main', agentId: AGENT }
  const no = await $.tool.call({ tool: 'mcp__github__create_pull_request', ...args, body: 'What changed.' })
  expect(denied(no)).toContain(DENY)
  expect(w.ran).toEqual([])
  const yes = await $.tool.call({ tool: 'mcp__plugin_github_github__create_pull_request', ...args, body: 'Plan: aim · C4\n\nWhat changed.' })
  await w.clock.settle()
  expect(yes.deny).toBeUndefined()
  expect(w.ran).toEqual(['mcp'])
  expect(w.sent('POST /api/links/pr')[0]?.body).toEqual({ session_id: SESSION_ID, agent_id: AGENT, url: PR })
})

test("a session whose worktree is linked checks its own main loop's PRs", { options: OPTIONS }, async ($, on) => {
  const w = world(on, { ...ROUTES, 'GET /api/context': { status: 200, body: LINKED_CONTEXT } }, WORKTREE)
  on('session.start', (_, e) => ({ cwd: e.cwd }))
  on('tool.call', { tool: 'Bash' }, () => bashResult(PR))
  await $.session.start({ ...START, cwd: WORKTREE })
  expect(denied(await bash($, 'gh pr create -t x -b "no line"', MAIN))).toContain(DENY)
  expect((await bash($, 'gh pr create -t x -b "Plan: aim · C4"', MAIN)).deny).toBeUndefined()
  await w.clock.settle()
  expect(w.sent('POST /api/links/pr')[0]?.body).toEqual({ session_id: SESSION_ID, agent_id: '', url: PR })
})

// --- aimd down: as if the mod weren't installed -------------------------------

/** Routes that answer as given while aimd is up, and refuse the connection while it is down. */
function flaky(): { routes: Routes; setDown: (down: boolean) => void } {
  let down = false
  const flip = (reply: Reply) => () => (down ? 'refuse' : reply)
  return {
    routes: {
      'GET /api/context': flip({ status: 200, body: CONTEXT }),
      'POST /api/links/spawn': flip({ status: 201, body: { links: [], packages: [] } }),
      'POST /api/links/pr': flip({ status: 201, body: { links: [], packages: [] } }),
      // The band's progress line asks for it every 15 s: down with the rest.
      'GET /api/plans/aim/summary': flip({ status: 200, body: SUMMARY }),
    },
    setDown: value => {
      down = value
    },
  }
}

test('aimd down after linking: no refusal, the URL still tried; aimd back: refused again', { options: OPTIONS }, async ($, on) => {
  const aimd = flaky()
  const w = await linked($, on, aimd.routes)
  const noLine = 'gh pr create -t x -b "no line"'
  expect(denied(await bash($, noLine))).toContain(DENY)

  // Down, and nothing heard from aimd for over 2 minutes: allowed, the URL still posted (and lost).
  aimd.setDown(true)
  await w.clock.advance(121_000)
  expect((await bash($, noLine)).deny).toBeUndefined()
  await w.clock.settle()
  expect(w.sent('POST /api/links/pr').length).toBe(1)
  // That post failed: the most recent call failed, so allowed at once too.
  expect((await bash($, noLine)).deny).toBeUndefined()
  await w.clock.settle()

  // Back: allowed until aimd answers a call (this run's PR post), then refused again.
  aimd.setDown(false)
  expect((await bash($, noLine)).deny).toBeUndefined()
  await w.clock.settle()
  expect(denied(await bash($, noLine))).toContain(DENY)
  expect(w.ran.length).toBe(3)
  expect(w.sent('POST /api/links/pr').length).toBe(3)
})

test('the MCP create-PR tool is not refused while aimd is down', { options: OPTIONS }, async ($, on) => {
  const aimd = flaky()
  const w = await linked($, on, aimd.routes)
  aimd.setDown(true)
  await w.clock.advance(121_000)
  const args = { owner: 'example-user', repo: 'probe', title: 't', head: 'h', base: 'main', agentId: AGENT }
  const out = await $.tool.call({ tool: 'mcp__github__create_pull_request', ...args, body: 'no line' })
  await w.clock.settle()
  expect(out.deny).toBeUndefined()
  expect(w.ran).toEqual(['mcp'])
  expect(w.sent('POST /api/links/pr').length).toBe(1)
})
