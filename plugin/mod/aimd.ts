// The mod's client for aimd's plan API (docs/api.md). Every call goes
// through $.http.fetch with a 1 s timeout and the X-AIM-Token header, and
// never throws: a failure is an answer with ok false.

import type { EngineInterface, PluginOptions } from 'claude-code'

/** How long the mod waits on aimd, per call. */
export const TIMEOUT_MS = 1000

export type Answer<T> =
  | { ok: true; status: number; data: T }
  | { ok: false; status: number; error: string }

export type Settings = {
  token: string
  url: string
  band: boolean
  appendConventions: boolean
  prCheck: boolean
  /** Refuse a spawn whose Package: line names a package its plan file lacks; off by default. */
  requirePlanRow: boolean
}

export function settings(options: PluginOptions): Settings {
  const text = (v: unknown, d: string) => (typeof v === 'string' && v.trim() !== '' ? v.trim() : d)
  const flag = (v: unknown) => v !== false
  return {
    token: text(options.token, ''),
    url: text(options.aimdUrl, 'http://127.0.0.1:7777').replace(/\/+$/, ''),
    band: flag(options.band),
    appendConventions: flag(options.appendConventions),
    prCheck: flag(options.prCheck),
    requirePlanRow: options.requirePlanRow === true,
  }
}

// aimd's health as the mod last saw it: whether its most recent call was
// answered, and when. A 4xx is an answer (aimd is up and said no); a
// timeout, a refused connection or a 5xx is not.
const health = { answered: false, at: Number.NEGATIVE_INFINITY }

/** Forgets what the mod knew of aimd's health, as a reload does. */
export function resetHealth(): void {
  health.answered = false
  health.at = Number.NEGATIVE_INFINITY
}

/** Whether aimd counts as up: its most recent call was answered, less than `withinMs` ago. */
export async function reachable($: EngineInterface, withinMs: number): Promise<boolean> {
  if (!health.answered) return false
  return (await $.clock.now()) - health.at < withinMs
}

/** One call to aimd. `body` makes it a write, with the token. */
export async function call<T>(
  $: EngineInterface,
  s: Settings,
  method: 'GET' | 'POST' | 'DELETE',
  path: string,
  body?: unknown,
): Promise<Answer<T>> {
  const got = await request<T>($, s, method, path, body)
  try {
    health.at = await $.clock.now()
    health.answered = got.ok || (got.status >= 400 && got.status < 500)
  } catch {
    health.answered = false
  }
  return got
}

/**
 * One call to aimd that leaves its health alone: the event reporter's
 * (report.ts), so the PR check's "aimd is up" stays what the plan API last
 * answered. Never throws.
 */
export async function request<T>(
  $: EngineInterface,
  s: Settings,
  method: 'GET' | 'POST' | 'DELETE',
  path: string,
  body?: unknown,
): Promise<Answer<T>> {
  const headers: Record<string, string> = { Accept: 'application/json' }
  if (s.token !== '') headers['X-AIM-Token'] = s.token
  if (body !== undefined) headers['Content-Type'] = 'application/json'
  const init = body === undefined ? { method, headers } : { method, headers, body: JSON.stringify(body) }
  const stop = new AbortController()
  let timedOut = false
  try {
    // No timers in a hooks module: the timeout is a $.clock wait, raced.
    const timer = $.clock.sleep(TIMEOUT_MS, { signal: stop.signal }).then(
      () => {
        timedOut = true
        return null
      },
      () => null,
    )
    const fetched = $.http.fetch(s.url + path, init)
    // Left behind on a timeout, its late answer or failure goes nowhere.
    fetched.catch(() => undefined)
    const res = await Promise.race([fetched, timer])
    if (res === null) return { ok: false, status: 0, error: `aimd did not answer within ${TIMEOUT_MS} ms` }
    let data: unknown
    try {
      data = res.text === '' ? undefined : JSON.parse(res.text)
    } catch {
      data = undefined
    }
    if (!res.ok) {
      const message =
        typeof data === 'object' && data !== null && typeof (data as { error?: unknown }).error === 'string'
          ? (data as { error: string }).error
          : `aimd answered ${res.status}`
      return { ok: false, status: res.status, error: message }
    }
    return { ok: true, status: res.status, data: data as T }
  } catch (err) {
    return { ok: false, status: 0, error: `aimd isn't reachable at ${s.url} (${String(err).slice(0, 200)})` }
  } finally {
    if (!timedOut) stop.abort()
  }
}

// --- Shapes of the answers the mod reads (docs/api.md) ------------------

export type ContextPlanPackage = {
  id: string
  title: string
  repo?: string
  lifecycle: string
  suggested_branch: string
  plan_line: string
  /** Not in the plan file yet: work named it. */
  provisional?: boolean
}

export type ContextPlan = {
  slug: string
  title: string
  /** The plan file (absent from older aimd). */
  path?: string
  band: string
  packages: ContextPlanPackage[]
}

export type ContextPackage = {
  plan: string
  id: string
  title: string
  lifecycle: string
  suggested_branch: string
  plan_line: string
  /** How the worktree or branch is tied to it. */
  link?: { kind: string; ref: string; method: string; confidence: string }
}

export type Context = {
  cwd: string
  repo: { path: string; name?: string } | null
  worktree?: { path: string; branch?: string; main?: boolean } | null
  plans: ContextPlan[]
  package?: ContextPackage | null
}

export type Detection = {
  path: string
  is_plan: boolean
  title?: string
  packages: number
  lanes: number
  slug: string
  registered: boolean
  dismissed: boolean
  offer: boolean
}

export type Registered = {
  created: boolean
  slug: string
  title: string
  path: string
  packages: number
}

/**
 * The fields of GET /api/plans/{slug}/summary the band's progress line
 * reads. The mod counts nothing itself: each number is aimd's.
 */
export type Summary = {
  slug: string
  /** Who merges the plan's PRs: `you` or `lead`. */
  merges?: string
  total: number
  done: number
  /** The packages ready to dispatch. */
  ready: { id: string }[]
  /** What waits on you for this plan. */
  needs_you: number
  /** Ready-to-merge PRs on a plan whose lead merges them; absent from older aimd. */
  to_merge?: number
}

/** The answer to POST and DELETE /api/sessions/watch. */
export type Watched = {
  session_id: string
  watched: boolean
  changed: boolean
  known: boolean
  monitored: boolean
  monitored_why: { reason: string; plan?: string; pkg?: string; text: string }[]
  monitor: string
  /** What happened, in one line, for the mod to show as is. */
  message: string
}

export type BoardPackage = ContextPlanPackage & {
  lane?: string
  owner?: string
  yours?: boolean
  needs?: string[]
  needs_note?: string
  needs_you?: string
  blocked_by?: string[]
  cycle?: string[]
  badges?: { kind: string; text: string }[]
  links?: BoardLink[]
  /** Not in the plan file yet, and the row to paste. */
  suggested_row?: string
}

export type BoardLink = {
  kind: string
  ref: string
  method: string
  confidence: string
  via?: { kind: string; ref: string }
  pr?: Record<string, unknown>
  agent?: Record<string, unknown>
  worktree?: Record<string, unknown>
}

export type Board = {
  slug: string
  title: string
  path: string
  repos: string[]
  band: string
  packages: BoardPackage[]
  unmatched: unknown[]
  warnings: unknown[]
  alerts: unknown[]
  derived_at?: string
}
