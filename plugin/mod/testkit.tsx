// The world beneath the mod in its tests: a fake aimd answering
// $.http.fetch, a mocked clock, and the session's ids. Not imported by
// register.tsx.

import type { On } from 'claude-code'
import { mock } from 'claude-code/testing'

import { BUILD_LOG, BUILD_STATUS, buildDescribe } from './build'
import { REPO, SESSION_ID } from './fixtures'

/** The commit the plugin folder's last change is, in the fake git. */
export const BUILD_COMMIT = 'abc1234'.padEnd(40, '0')

/** One request the mod made to aimd. */
export type Request = {
  method: string
  path: string
  query: string
  body: Record<string, unknown> | undefined
  headers: Record<string, string>
}

/** How the fake aimd answers a route: a status and body, a hang, or a refusal. */
export type Reply = { status: number; body?: unknown } | 'hang' | 'refuse'

export type Routes = Record<string, Reply | ((req: Request) => Reply)>

export const TOKEN = 'test-token'
export const OPTIONS = { token: TOKEN, aimdUrl: 'http://127.0.0.1:17999' }

/**
 * Hooks the test's `on` with a fake aimd (routes keyed `METHOD /path`), a
 * mocked clock and the session nouns the mod reads. Unrouted paths get 404.
 * `sessionId` is what $.session.id() answers; an Error makes it fail.
 * `env` is the environment $.env.get reads (none set by default).
 */
export function world(
  on: On,
  routes: Routes,
  cwd = REPO,
  sessionId: string | Error = SESSION_ID,
  env: Readonly<Record<string, string>> = {},
) {
  const clock = mock.clock(on, { now: 1_000_000 })
  mock.env(on, env)
  const requests: Request[] = []
  const toasts: string[] = []
  const registered: {
    commands: string[]
    /** Each command's argument hint, by name. */
    hints: Record<string, string | undefined>
    tools: { name: string; isDeferred?: boolean }[]
  } = { commands: [], hints: {}, tools: [] }
  const files: Record<string, string> = {}
  const opened: string[] = []
  /** How opening the dashboard goes: `ok` (exit 0), `fail` (exit 1) or `throw`. */
  const browser: { opens: 'ok' | 'fail' | 'throw' } = { opens: 'ok' }
  /**
   * The plugin folder's git, for the build session_start reports
   * (build.ts): what describe says, whether the folder has changes, or
   * that git fails there (`fail`: not a checkout) or rejects (`reject`: a
   * timeout). `calls` are the build's git calls: the folder each ran in
   * and its timeout.
   */
  const build: {
    describe: string
    dirty: boolean
    git: 'ok' | 'fail' | 'reject'
    calls: { cwd: string | undefined; timeoutMs: number | undefined }[]
  } = { describe: 'v1.0.0-3-gabc1234', dirty: false, git: 'ok', calls: [] }
  const buildCalls = (): Record<string, string> => ({
    [['git', ...BUILD_LOG].join(' ')]: `${BUILD_COMMIT}\n`,
    [['git', ...BUILD_STATUS].join(' ')]: build.dirty ? ' M mod/register.tsx\n' : '',
    [['git', ...buildDescribe(BUILD_COMMIT)].join(' ')]: `${build.describe}\n`,
  })

  on('session.id', () => {
    if (sessionId instanceof Error) throw sessionId
    return { value: sessionId }
  })
  on('session.root', () => ({ value: cwd }))
  on('session.cwd', () => ({ value: cwd }))
  on('command.register', (_, e) => {
    registered.commands.push(e.name)
    registered.hints[e.name] = e.argumentHint
    return { value: { command: e.name } }
  })
  on('tool.register', (_, e) => {
    registered.tools.push({ name: e.name, isDeferred: e.isDeferred })
    return { value: { tool: `mcp__ai-manager__${e.name}` } }
  })
  on('ui.toast', (_, e) => {
    toasts.push(e.text)
    return { value: undefined }
  })
  on('fs.read', (_, e) => {
    const text = files[e.path]
    if (text === undefined) throw new Error(`ENOENT: ${e.path}`)
    return { value: text }
  })
  on('session.repo', () => ({ value: { root: REPO, remote: null, internal: false, name: null } }))
  on('process.run', (_, e) => {
    // The reporter's facts at session start (git's branch, the hostname,
    // the plugin's build); anything else is the dashboard being opened.
    const facts: Record<string, string> = { 'git rev-parse --abbrev-ref HEAD': 'main\n', hostname: 'laptop\n' }
    const line = e.argv.join(' ')
    const built = buildCalls()[line]
    if (built !== undefined) {
      build.calls.push({ cwd: e.init?.cwd, timeoutMs: e.init?.timeoutMs })
      if (build.git === 'reject') return { deny: 'git: timed out' }
      const failed = build.git === 'fail'
      return {
        value: {
          exitCode: failed ? 128 : 0,
          stdout: failed ? '' : built,
          stderr: failed ? 'fatal: not a git repository' : '',
          isStdoutTruncated: false,
          isStderrTruncated: false,
        },
      }
    }
    const stdout = facts[line]
    if (stdout === undefined) {
      if (browser.opens === 'throw') throw new Error(`${e.argv[0]}: not found`)
      if (browser.opens === 'ok') opened.push(line)
    }
    const exitCode = stdout === undefined && browser.opens === 'fail' ? 1 : 0
    return { value: { exitCode, stdout: stdout ?? '', stderr: '', isStdoutTruncated: false, isStderrTruncated: false } }
  })
  // The engine's own drawing, where the mod passes (the band with nothing to offer).
  on('ui.render', ($, e) => {
    const { Text } = $.ui.resolve(e)
    return <Text>engine</Text>
  })
  on('http.fetch', async (_, e) => {
    const url = new URL(e.url)
    const method = e.init?.method ?? 'GET'
    const req: Request = {
      method,
      path: url.pathname,
      query: url.search,
      body: e.init?.body === undefined ? undefined : (JSON.parse(e.init.body) as Record<string, unknown>),
      headers: e.init?.headers ?? {},
    }
    requests.push(req)
    const route = routes[`${method} ${url.pathname}`]
    const reply: Reply = route === undefined ? { status: 404, body: { error: 'no such route' } } : typeof route === 'function' ? route(req) : route
    if (reply === 'refuse') throw new Error('connect ECONNREFUSED 127.0.0.1:17999')
    if (reply === 'hang') {
      // Slower than the mod waits: answers only once the clock passes a minute.
      await clock.sleep(60_000)
      return { value: { status: 200, ok: true, headers: {}, text: '{}' } }
    }
    const text = reply.body === undefined ? '' : JSON.stringify(reply.body)
    return { value: { status: reply.status, ok: reply.status >= 200 && reply.status < 300, headers: {}, text } }
  })

  /** The requests made to one route. */
  const sent = (route: string) => requests.filter(r => `${r.method} ${r.path}` === route)

  return { clock, requests, sent, toasts, registered, files, opened, browser, build }
}

/** Starts the session as the engine does. */
export const START = { cwd: REPO, surface: 'terminal', isInteractive: true } as const

/** The band's props on a terminal 100 columns wide. */
export const BAND = {
  plugin: 'ai-manager',
  component: 'AbovePrompt',
  props: {
    hasSurvey: false,
    isWorking: false,
    maxRows: 10,
    bodyColumns: 95,
    scroll: { offset: 0, bodyRows: 9 },
    view: {},
  },
} as const

export const COMMAND = {
  origin: { kind: 'composer' },
  presentation: { isFullscreen: false, columns: 100 },
} as const
