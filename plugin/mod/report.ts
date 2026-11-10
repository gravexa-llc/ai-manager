// The mod's reporting: events in the shape aimd's internal/events package
// defines, queued in memory and POSTed to aimd's /events in batches, with
// the token.
//
// - Batches go about once a second; needs_you, error and session_end go
//   at once. A POST carries at most 200 events and well under 256 KB.
// - The queue holds 200 events. When aimd is down, slow (1 s) or broken
//   (5xx), the batch goes back to the queue, the oldest events beyond 200
//   are dropped, and the next try waits 2, 4, 8, then 15 s. A 4xx answer
//   drops the batch: it would fail again.
// - Nothing here makes a hook wait: report() only queues and arms a
//   $.clock timer, and every $ call it needs (the session id, the cwd, the
//   agent list, the POST) runs unawaited. Nothing here throws.
//
// The hooks themselves are in register.tsx; this module keeps what they
// need between calls: the session id the events are stamped with, the
// session's facts for a session_start after /clear, the asks not yet
// answered, the spawned agents' flags and token counts, the main loop's
// token count, and the 30 s agent_list ticker.

import type { EngineInterface } from 'claude-code'

import { request, type Settings } from './aimd'
import { pluginBuild } from './build'
import { label, lastSaid, reason, waitFor, type Wait } from './wait'

/** One event as aimd's internal/events defines it. */
export type AimEvent = {
  kind: string
  session_id: string
  at: string
  agent_id?: string
  cwd?: string
  [field: string]: unknown
}

/** An event before it is stamped with the session id (and, unless given, the time). */
type Draft = { kind: string; at?: string; agent_id?: string; cwd?: string; [field: string]: unknown }

export const MAX_QUEUE = 200
export const MAX_BATCH = 200
/** A batch's JSON is kept under this many characters: 256 KB even at 3 bytes a character. */
export const MAX_BATCH_CHARS = 80_000
export const BATCH_MS = 1000
export const MAX_BACKOFF_MS = 15_000
export const AGENT_TICK_MS = 30_000

type Timer = { cancel: () => void }
type Spawned = { background: boolean; parent?: string }

// --- module state (reset by register(), as a reload does) -----------------

let settings: Settings | null = null
let queue: AimEvent[] = []
let timer: Timer | null = null
let inflight = false
let failures = 0

/** The session id events are stamped with; '' until known. */
let sid = ''
/** session.start has run in this module (so a new id in classic.SessionStart is a /clear or a /resume). */
let started = false
/** classic.SessionStart's source, for the session_start that follows it. */
let source = ''
/** The session_start fields, sent again under a new id after /clear. */
let facts: Record<string, unknown> = {}
/** Each loop's last known cwd (MAIN for the main loop). */
let cwds = new Map<string, string>()
/** The asks not yet answered: tool_use_id → loop. A Notification is sent only while it is empty. */
let asks = new Map<string, string>()
/** Each loop's last ask (classic.PermissionRequest carries no tool_use_id). */
let lastAsk = new Map<string, string>()
let spawned = new Map<string, Spawned>()
/** Each subagent's token count (onStep), for agent_list. */
let tokens = new Map<string, number>()
/** The main loop's token count (onStep), for its tool and turn_complete events; 0 until its first response. */
let mainTokens = 0
let ticker: Timer | null = null

const MAIN = ''

/** Forgets everything, as a reload does. */
export function resetReporter(s: Settings): void {
  if (timer !== null) quietCancel(timer)
  if (ticker !== null) quietCancel(ticker)
  settings = s
  queue = []
  timer = null
  inflight = false
  failures = 0
  sid = ''
  started = false
  source = ''
  facts = {}
  cwds = new Map()
  asks = new Map()
  lastAsk = new Map()
  spawned = new Map()
  tokens = new Map()
  mainTokens = 0
  ticker = null
}

function quietCancel(t: Timer): void {
  try {
    t.cancel()
  } catch {
    // already gone
  }
}

// --- the queue and the transport ------------------------------------------

function now(): string {
  return new Date().toISOString()
}

/** Queues a stamped event and arranges its POST. Never throws, never waits. */
function enqueue($: EngineInterface, ev: AimEvent, urgent: boolean): void {
  try {
    queue.push(ev)
    if (queue.length > MAX_QUEUE) queue.splice(0, queue.length - MAX_QUEUE)
    if (urgent && failures === 0) void flush($)
    else arm($, failures === 0 ? BATCH_MS : backoff())
  } catch {
    // reporting is best effort
  }
}

function backoff(): number {
  return Math.min(BATCH_MS * 2 ** Math.max(1, failures), MAX_BACKOFF_MS)
}

function arm($: EngineInterface, ms: number): void {
  if (timer !== null) return
  try {
    timer = $.clock.after(ms, () => {
      timer = null
      void flush($)
    })
  } catch {
    timer = null
  }
}

/** The next batch off the front of the queue: at most MAX_BATCH events and MAX_BATCH_CHARS. */
function take(): AimEvent[] {
  let chars = 0
  let n = 0
  while (n < queue.length && n < MAX_BATCH) {
    const size = JSON.stringify(queue[n]).length + 1
    if (n > 0 && chars + size > MAX_BATCH_CHARS) break
    chars += size
    n += 1
  }
  return queue.splice(0, n)
}

/** POSTs what is queued, batch by batch. One runs at a time; never throws. */
export async function flush($: EngineInterface): Promise<void> {
  if (inflight || settings === null) return
  const s = settings
  if (!s.local) {
    // Not a local aimd (aimd.ts): nothing goes to it, and nothing waits for it.
    queue = []
    return
  }
  inflight = true
  try {
    while (queue.length > 0) {
      const batch = take()
      const got = await request($, s, 'POST', '/events', { events: batch })
      if (!got.ok && (got.status === 0 || got.status >= 500)) {
        // Down, slow or broken: keep the newest 200 and try again later.
        queue = [...batch, ...queue]
        if (queue.length > MAX_QUEUE) queue = queue.slice(queue.length - MAX_QUEUE)
        failures += 1
        arm($, backoff())
        return
      }
      // Sent, or refused for good (a 4xx): either way it is done with.
      failures = 0
    }
  } catch {
    // reporting is best effort
  } finally {
    inflight = false
  }
}

/** Stamps a draft with the session id (asked for when not known yet) and queues it. */
function emit($: EngineInterface, draft: Draft, urgent = false): void {
  try {
    const at = draft.at ?? now()
    if (sid !== '') {
      enqueue($, { ...draft, session_id: sid, at }, urgent)
      return
    }
    void quietly($.session.id()).then(id => {
      if (typeof id !== 'string' || id === '') return
      if (sid === '') sid = id
      enqueue($, { ...draft, session_id: id, at }, urgent)
    })
  } catch {
    // reporting is best effort
  }
}

async function quietly<T>(work: Promise<T>): Promise<T | undefined> {
  try {
    return await work
  } catch {
    return undefined
  }
}

/** Runs fn, swallowing what it throws: reporting never breaks a hook. */
export function safely(fn: () => void): void {
  try {
    fn()
  } catch {
    // reporting is best effort
  }
}

function withAgent(agentId: unknown): { agent_id?: string } {
  return typeof agentId === 'string' && agentId !== '' ? { agent_id: agentId } : {}
}

const str = (v: unknown): string => (typeof v === 'string' ? v : '')

// --- what the hooks report ---------------------------------------------------

/** classic.SessionStart: remembers the id and source; after /clear (session.start won't fire again), reports the new id. */
export function onClassicSessionStart($: EngineInterface, e: { session_id?: unknown; source?: unknown }): void {
  const id = str(e.session_id)
  const src = str(e.source)
  if (started && id !== '' && sid !== '' && id !== sid) {
    const prev = sid
    sid = id
    asks.clear()
    lastAsk.clear()
    mainTokens = 0 // a new conversation: the next response's size is its own
    emit($, { kind: 'session_start', ...facts, source: src, ...(src === 'clear' ? { prev_session_id: prev } : {}) })
    return
  }
  if (id !== '') sid = id
  source = src
}

/** session.start: the session's facts, gathered unawaited (git and hostname take ~15 ms), then session_start, with the plugin build (build.ts). */
export function onSessionStart($: EngineInterface, e: { cwd: string; isInteractive?: boolean }): void {
  started = true
  const at = now()
  cwds.set(MAIN, e.cwd)
  void startFacts($, e, at)
}

async function startFacts($: EngineInterface, e: { cwd: string; isInteractive?: boolean }, at: string): Promise<void> {
  try {
    const run = (argv: string[]) => quietly($.process.run(argv, { cwd: e.cwd, timeoutMs: 2000 }))
    const [id, repo, root, branch, host, iterm, kind, build] = await Promise.all([
      sid !== '' ? Promise.resolve(sid) : quietly($.session.id()),
      quietly($.session.repo()),
      quietly($.session.root()),
      run(['git', 'rev-parse', '--abbrev-ref', 'HEAD']),
      run(['hostname']),
      quietly($.env.get('ITERM_SESSION_ID')),
      quietly($.env.get('CLAUDE_CODE_SESSION_KIND')),
      quietly(pluginBuild($)),
    ])
    if (sid === '' && typeof id === 'string') sid = id
    const out = (r: { exitCode: number; stdout: string } | undefined) => (r?.exitCode === 0 ? r.stdout.trim() : '')
    const f: Record<string, unknown> = { cwd: e.cwd }
    const repoRoot = (repo as { root?: unknown } | null | undefined)?.root
    if (typeof repoRoot === 'string' && repoRoot !== '') f.repo = repoRoot
    if (typeof root === 'string' && root !== '') f.worktree = root
    if (out(branch) !== '') f.branch = out(branch)
    if (out(host) !== '') f.host = out(host)
    if (typeof iterm === 'string' && iterm !== '') f.iterm_session_id = iterm
    if (typeof e.isInteractive === 'boolean') f.interactive = e.isInteractive
    if (kind === 'bg') f.background = true
    if (build !== undefined) Object.assign(f, build)
    facts = f
    emit($, { kind: 'session_start', at, ...f, ...(source !== '' ? { source } : {}) })
  } catch {
    // reporting is best effort
  }
}

/** session.end: session_end, sent at once (the process may be about to exit). */
export function onSessionEnd($: EngineInterface, e: { reason?: unknown }): void {
  emit($, { kind: 'session_end', reason: reason(str(e.reason)) }, true)
  asks.clear()
  lastAsk.clear()
  stopTicker()
}

/** prompt.submit: its origin kind only, never the text. A person's prompt answers every ask. */
export function onPrompt($: EngineInterface, e: { origin?: { kind?: unknown } }): void {
  const origin = str(e.origin?.kind)
  if (origin === 'composer' || origin === 'sdk' || origin === 'bridge') asks.clear()
  emit($, { kind: 'prompt', origin_kind: origin })
}

/** Forgets a loop's open asks: its turn moved on. */
function loopMovedOn(agentId: string): void {
  for (const [id, loop] of asks) if (loop === agentId) asks.delete(id)
}

/** turn.start: turn_start with the loop's cwd, read inside the hook. */
export function onTurnStart($: EngineInterface, e: { agentId?: string }): void {
  const at = now()
  const loop = e.agentId ?? MAIN
  loopMovedOn(loop)
  void quietly($.session.cwd()).then(cwd => {
    if (typeof cwd === 'string' && cwd !== '') cwds.set(loop, cwd)
    emit($, { kind: 'turn_start', at, ...withAgent(e.agentId), ...(typeof cwd === 'string' && cwd !== '' ? { cwd } : {}) })
  })
}

/** turn.complete: the reason, the first line of the answer (redacted) and, in the main loop, its token count; then an agent_list snapshot. */
export function onTurnComplete($: EngineInterface, e: { agentId?: string; reason?: unknown; answer?: unknown }): void {
  const loop = e.agentId ?? MAIN
  loopMovedOn(loop)
  const said = lastSaid(str(e.answer))
  emit($, {
    kind: 'turn_complete',
    ...withAgent(e.agentId),
    reason: reason(str(e.reason)),
    ...(said !== '' ? { last_said: said } : {}),
    ...mainCount(loop),
  })
  void snapshot($)
}

/** The main loop's token count, for an event in that loop: nothing in a subagent's, or before the first response. */
function mainCount(loop: string): { tokens?: number } {
  return loop === MAIN && mainTokens > 0 ? { tokens: mainTokens } : {}
}

/** The arguments of a tool.call's e: everything but the envelope. */
export function argsOf(e: Readonly<Record<string, unknown>>): Record<string, unknown> {
  const out: Record<string, unknown> = {}
  for (const [k, v] of Object.entries(e)) if (k !== 'tool' && k !== 'tool_use_id' && k !== 'agentId') out[k] = v
  return out
}

/** tool.call, after the call: the last-action label, with the loop's cwd after the call ran and, in the main loop, its token count. */
export function onToolDone($: EngineInterface, e: Readonly<Record<string, unknown>>): void {
  const at = now()
  const tool = str(e.tool)
  const id = str(e.tool_use_id)
  const loop = str(e.agentId)
  asks.delete(id)
  const args = argsOf(e)
  const size = mainCount(loop) // the step that made this call, read now
  void quietly($.session.cwd()).then(cwd => {
    const dir = typeof cwd === 'string' && cwd !== '' ? cwd : (cwds.get(loop) ?? '')
    if (dir !== '') cwds.set(loop, dir)
    emit($, {
      kind: 'tool',
      at,
      ...withAgent(loop),
      tool,
      label: label(tool, args, dir),
      ...(id !== '' ? { tool_use_id: id } : {}),
      ...(dir !== '' ? { cwd: dir } : {}),
      ...size,
    })
  })
}

export type Ask = {
  source: 'permission_request' | 'ask_user_question' | 'exit_plan_mode'
  agentId?: string
  toolUseId?: string
  tool: string
  input: unknown
}

/**
 * tool.check answered "ask" on a real call: noted, not reported. The
 * mode's decider takes the ask first, and in auto mode its classifier
 * answers most asks with no dialog at all. If a dialog does open,
 * classic.PermissionRequest reports it, under this call's id (it names no
 * call itself).
 */
export function noteAsk(agentId: string | undefined, toolUseId: string): void {
  lastAsk.set(agentId ?? MAIN, toolUseId)
}

/** A dialog opened: needs_you at once, with what it waits on. */
export function onAsk($: EngineInterface, a: Ask): void {
  const loop = a.agentId ?? MAIN
  const id = a.toolUseId ?? lastAsk.get(loop) ?? ''
  if (id !== '') {
    asks.set(id, loop)
    lastAsk.set(loop, id)
  } else {
    asks.set(`?${loop}`, loop)
  }
  const input = a.input !== null && typeof a.input === 'object' ? (a.input as Record<string, unknown>) : {}
  const wait: Wait | undefined = waitFor(a.tool, input)
  emit(
    $,
    {
      kind: 'needs_you',
      ...withAgent(a.agentId),
      source: a.source,
      reason: label(a.tool, input, cwds.get(loop) ?? ''),
      ...(id !== '' ? { tool_use_id: id } : {}),
      ...(wait !== undefined ? { wait } : {}),
    },
    true,
  )
}

/**
 * classic.Notification: idle_prompt is idle; a dialog's notification is
 * a needs_you only while no ask is open, since it names no loop (not
 * even a subagent's), and the ask was reported already.
 */
export function onNotification($: EngineInterface, e: { notification_type?: unknown; agent_id?: unknown }): void {
  const type = str(e.notification_type)
  if (type === 'idle_prompt') {
    emit($, { kind: 'idle', ...withAgent(e.agent_id) })
    return
  }
  if ((type === 'permission_prompt' || type === 'agent_needs_input' || type === 'elicitation_dialog') && asks.size === 0) {
    emit($, { kind: 'needs_you', ...withAgent(e.agent_id), source: 'notification', reason: reason(type) }, true)
  }
}

/** classic.StopFailure: the API turn failed; sent at once. */
export function onStopFailure($: EngineInterface, e: { error?: unknown; agent_id?: unknown }): void {
  emit($, { kind: 'error', ...withAgent(e.agent_id), reason: reason(str(e.error)) }, true)
}

/** classic.WorktreeCreate: the isolated agent's worktree name. */
export function onWorktreeCreate($: EngineInterface, e: { name?: unknown }): void {
  const name = str(e.name)
  if (name !== '') emit($, { kind: 'worktree_create', name })
}

/**
 * classic.SubagentStart: a run of a subagent begins, with the type it was
 * spawned as and its cwd. Claude Code fires it at the start of every run
 * (the first, and each SendMessage resume, which goes on under the same
 * id), wherever no settings hook is set, with the id `$.agent.list()`
 * lists the agent by. Its own forks of an agent (compaction, memory) carry
 * ids no list names, so this never names one: aimd counts a loop as an
 * agent from this, its turn_start or the agent list, never from its tool
 * calls alone.
 */
export function onSubagentStart($: EngineInterface, e: { agent_id?: unknown; agent_type?: unknown; cwd?: unknown }): void {
  const id = str(e.agent_id)
  if (id === '') return
  const type = str(e.agent_type)
  const cwd = str(e.cwd)
  if (cwd !== '') cwds.set(id, cwd)
  emit($, { kind: 'agent_start', agent_id: id, ...(type !== '' ? { agent_type: type } : {}), ...(cwd !== '' ? { cwd } : {}) })
}

/** agent.spawn, after it: remembers the agent's flags, snapshots the list, and ticks every 30 s while agents run. */
export function onSpawned($: EngineInterface, e: { background?: unknown; parentAgentId?: unknown }, agentId: string | undefined): void {
  if (agentId !== undefined && agentId !== '') {
    const p = str(e.parentAgentId)
    spawned.set(agentId, { background: e.background === true, ...(p !== '' ? { parent: p } : {}) })
    if (spawned.size > 500) spawned.delete(spawned.keys().next().value as string)
  }
  void snapshot($)
  if (ticker === null) {
    try {
      ticker = $.clock.every(AGENT_TICK_MS, () => void snapshot($))
    } catch {
      ticker = null
    }
  }
}

/** A usage count as a whole number of tokens: 0 for anything else. */
const count = (v: unknown): number => (typeof v === 'number' && Number.isFinite(v) && v > 0 ? Math.floor(v) : 0)

/**
 * turn.step, after it: a loop's token count. A subagent's goes on the next
 * agent_list; the main loop's (agentId absent) on the main loop's next
 * tool and turn_complete events, which follow every step: a step either
 * asks for tools or ends the turn. It is the count Claude Code's agent
 * list shows ("↓ 284.1k tokens"): the latest response's input, cache
 * read, cache write and output tokens added up, so about the loop's
 * context size now. Not a running total: the CLI's own count takes each
 * response's input in place of the last, and a subagent's final total is
 * exactly that sum for its last response.
 */
export function onStep(agentId: unknown, usage: unknown): void {
  if (usage === null || typeof usage !== 'object') return
  const main = agentId === undefined || agentId === ''
  const id = str(agentId)
  if (!main && id === '') return // not a loop the API names
  const u = usage as Record<string, unknown>
  const n = count(u.input_tokens) + count(u.cache_read_input_tokens) + count(u.cache_creation_input_tokens) + count(u.output_tokens)
  if (n === 0) return
  if (main) {
    mainTokens = n
    return
  }
  tokens.delete(id) // the newest last, so the oldest go first
  tokens.set(id, n)
  if (tokens.size > 500) tokens.delete(tokens.keys().next().value as string)
}

function stopTicker(): void {
  if (ticker !== null) quietCancel(ticker)
  ticker = null
}

/** A $.agent.list() snapshot as agent_list, with each agent's token count; the ticker stops once none runs. */
async function snapshot($: EngineInterface): Promise<void> {
  try {
    const list = await $.agent.list()
    const agents = list.map(a => {
      const known = spawned.get(a.id)
      const parent = a.parentId ?? known?.parent
      const n = tokens.get(a.id)
      return {
        id: a.id,
        status: a.status,
        type: a.type,
        ...(known !== undefined ? { background: known.background } : {}),
        ...(parent !== undefined && parent !== '' ? { parent_id: parent } : {}),
        ...(n !== undefined ? { tokens: n } : {}),
      }
    })
    if (agents.length > 0) emit($, { kind: 'agent_list', agents })
    if (!agents.some(a => a.status === 'pending' || a.status === 'running' || a.status === 'waiting')) stopTicker()
  } catch {
    // reporting is best effort
  }
}
