import type { On } from 'claude-code'
import { expect, test, type Engine } from 'claude-code/testing'

import { CONTEXT, REPO, SESSION_ID, SPAWN, SPAWNED, bashResult } from './fixtures'
import { prEnds, prRefIn } from './text'
import { OPTIONS, START, world, type Routes } from './testkit'

const ROUTES: Routes = {
  'GET /api/context': { status: 200, body: CONTEXT },
  'POST /api/links/spawn': { status: 201, body: { links: [], packages: [] } },
  'POST /api/links/pr': { status: 201, body: { links: [], packages: [] } },
  'POST /api/prs/refresh': { status: 202, body: { prs: [], queued: 1 } },
}

/** A session whose Bash prints `out` and exits as `isError` says, and whose GitHub MCP tools answer. */
async function session($: Engine, on: On, opts: { out?: string; isError?: boolean; routes?: Routes } = {}) {
  const w = world(on, opts.routes ?? ROUTES)
  const ran: string[] = []
  on('session.start', (_, e) => ({ cwd: e.cwd }))
  on('agent.spawn', () => SPAWNED)
  on('tool.call', { tool: 'Bash' }, (_, e) => {
    ran.push(e.command)
    const r = bashResult(opts.out ?? '')
    return opts.isError === true ? { ...r, isError: true } : r
  })
  on('tool.call', { tool: /pull_request$/ }, () => {
    ran.push('mcp')
    return { result: { content: [{ type: 'text', text: '{"merged": true}' }] }, text: '{"merged": true}' }
  })
  await $.session.start(START)
  await w.clock.settle()
  return { ...w, ran }
}

const bash = ($: Engine, command: string, agentId?: string) =>
  agentId === undefined ? $.tool.call({ tool: 'Bash', command }) : $.tool.call({ tool: 'Bash', command, agentId })

test('prEnds reads the PR each gh pr merge or close names, past its flags', () => {
  expect(prEnds('gh pr merge 12 --squash --delete-branch')).toEqual([{ action: 'merge', ref: '12', repo: '' }])
  expect(prEnds('gh pr merge --squash -b "Done (#3)" -t subject 12')).toEqual([{ action: 'merge', ref: '12', repo: '' }])
  expect(prEnds('gh pr merge https://github.com/acme/app/pull/7 --rebase')).toEqual([
    { action: 'merge', ref: 'https://github.com/acme/app/pull/7', repo: '' },
  ])
  expect(prEnds('gh pr close 4 -R example-org/web -c "superseded"')).toEqual([{ action: 'close', ref: '4', repo: 'example-org/web' }])
  expect(prEnds('gh pr merge --repo=acme/app 9 && gh pr close -Racme/app 10')).toEqual([
    { action: 'merge', ref: '9', repo: 'acme/app' },
    { action: 'close', ref: '10', repo: 'acme/app' },
  ])
  expect(prEnds('cd ~/Projects/app && gh pr merge --auto --squash')).toEqual([{ action: 'merge', ref: '', repo: '' }])
  expect(prEnds("gh pr merge feature/x --body-file - <<'EOF'\nmerge 99\nEOF")).toEqual([{ action: 'merge', ref: 'feature/x', repo: '' }])
  expect(prEnds('gh pr view 12')).toEqual([])
  expect(prEnds('echo "gh pr merges nothing"')).toEqual([])
  expect(prEnds('gh pr create -t x -b y')).toEqual([])
  expect(prRefIn('✓ Squashed and merged pull request acme/app#12 (Shell)')).toBe('acme/app#12')
  expect(prRefIn('no PR here #12')).toBeUndefined()
})

test('a merge that ran asks aimd to read the PR, with the cwd for a bare number', { options: OPTIONS }, async ($, on) => {
  const w = await session($, on)
  const out = await bash($, 'gh pr merge 12 --squash --delete-branch')
  await w.clock.settle()
  expect(out.deny).toBeUndefined()
  expect(w.ran).toEqual(['gh pr merge 12 --squash --delete-branch'])
  expect(w.sent('POST /api/prs/refresh').map(r => r.body)).toEqual([{ ref: '12', action: 'merge', cwd: REPO }])
})

test('a close names its -R repo; a URL needs no cwd to read', { options: OPTIONS }, async ($, on) => {
  const w = await session($, on)
  await bash($, 'gh pr close 4 -R example-org/web -c "superseded"', SPAWNED.agentId)
  await bash($, 'gh pr merge https://github.com/acme/app/pull/7 --rebase', SPAWNED.agentId)
  await w.clock.settle()
  expect(w.sent('POST /api/prs/refresh').map(r => r.body)).toEqual([
    { ref: '4', repo: 'example-org/web', action: 'close', cwd: REPO },
    { ref: 'https://github.com/acme/app/pull/7', action: 'merge', cwd: REPO },
  ])
})

test('no PR named: the main loop sends its branch, an agent sends nothing', { options: OPTIONS }, async ($, on) => {
  const w = await session($, on)
  await bash($, 'gh pr merge --auto --squash')
  await bash($, 'gh pr merge --squash', SPAWNED.agentId)
  await w.clock.settle()
  expect(w.sent('POST /api/prs/refresh').map(r => r.body)).toEqual([{ ref: '', action: 'merge', cwd: REPO }])
})

test('no PR named, but gh printed the one it merged: that one is read', { options: OPTIONS }, async ($, on) => {
  const w = await session($, on, { out: '✓ Squashed and merged pull request acme/app#21 (Shell)\n' })
  await bash($, 'gh pr merge --squash', SPAWNED.agentId)
  await w.clock.settle()
  expect(w.sent('POST /api/prs/refresh').map(r => r.body)).toEqual([{ ref: 'acme/app#21', action: 'merge', cwd: REPO }])
})

test('a merge that failed tells aimd nothing', { options: OPTIONS }, async ($, on) => {
  const w = await session($, on, { isError: true })
  await bash($, 'gh pr merge 12 --squash')
  await w.clock.settle()
  expect(w.ran.length).toBe(1)
  expect(w.sent('POST /api/prs/refresh').length).toBe(0)
})

test('a command with no merge or close tells aimd nothing', { options: OPTIONS }, async ($, on) => {
  const w = await session($, on)
  await bash($, 'gh pr view 12 && git status')
  await w.clock.settle()
  expect(w.sent('POST /api/prs/refresh').length).toBe(0)
})

for (const reply of ['refuse', 'hang'] as const) {
  test(`aimd ${reply === 'refuse' ? 'down' : 'slow'}: the merge runs and answers as it would without the mod`, { options: OPTIONS }, async ($, on) => {
    const w = await session($, on, { routes: { ...ROUTES, 'POST /api/prs/refresh': reply }, out: 'merged\n' })
    const out = await bash($, 'gh pr merge 12 --squash')
    expect(out.deny).toBeUndefined()
    expect(out.text).toBe('merged\n')
    await w.clock.settle()
    expect(w.ran).toEqual(['gh pr merge 12 --squash'])
    expect(w.sent('POST /api/prs/refresh').length).toBe(1)
  })
}

test("the GitHub MCP server's merge, and an update that closes, are read by number", { options: OPTIONS }, async ($, on) => {
  const w = await session($, on)
  await $.tool.call({ tool: 'mcp__github__merge_pull_request', owner: 'acme', repo: 'app', pullNumber: 12, merge_method: 'squash' })
  await $.tool.call({ tool: 'mcp__github__update_pull_request', owner: 'acme', repo: 'app', pullNumber: 13, state: 'closed' })
  await $.tool.call({ tool: 'mcp__github__update_pull_request', owner: 'acme', repo: 'app', pullNumber: 14, title: 'Renamed' })
  await w.clock.settle()
  expect(w.ran).toEqual(['mcp', 'mcp', 'mcp'])
  expect(w.sent('POST /api/prs/refresh').map(r => r.body)).toEqual([
    { ref: 'acme/app#12', action: 'merge', cwd: REPO },
    { ref: 'acme/app#13', action: 'close', cwd: REPO },
  ])
})

test('a PR created by an agent no package claims is read at once, not linked', { options: OPTIONS }, async ($, on) => {
  const w = await session($, on, { out: 'https://github.com/acme/app/pull/30\n' })
  await bash($, 'gh pr create -t "Tidy" -b "No plan line"')
  await w.clock.settle()
  expect(w.sent('POST /api/links/pr').length).toBe(0)
  expect(w.sent('POST /api/prs/refresh').map(r => r.body)).toEqual([
    { ref: 'https://github.com/acme/app/pull/30', action: 'create', cwd: REPO },
  ])
  // A linked agent's PR goes through POST /api/links/pr, which reads it too.
  await $.agent.spawn({ ...SPAWN, prompt: 'Package: aim · C4\nBuild it.', description: 'C4' })
  await w.clock.settle()
  await bash($, 'gh pr create -t "Mod (C4)" -b "Plan: aim · C4"', SPAWNED.agentId)
  await w.clock.settle()
  expect(w.sent('POST /api/links/pr')[0]?.body).toEqual({ session_id: SESSION_ID, agent_id: SPAWNED.agentId, url: 'https://github.com/acme/app/pull/30' })
  expect(w.sent('POST /api/prs/refresh').length).toBe(1)
})
