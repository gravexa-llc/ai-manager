import { atom, read, update } from 'claude-code'
import type { EngineInterface, Register } from 'claude-code'

import type { LinkCandidate, PlanOffer, PlanProgress, UnlinkedSpawn } from '../types'
import {
  NOT_LOCAL,
  call,
  reachable,
  resetHealth,
  settings,
  type Board,
  type BoardPackage,
  type Context,
  type ContextPlan,
  type Detection,
  type Registered,
  type Settings,
  type Summary,
  type Watched,
} from './aimd'
import { appUrl, moreText, needYouText, planOrder, progressLine, progressOf, sameProgress, withFirst } from './progress'
import {
  argsOf,
  classicRuns,
  noteAsk,
  onAsk,
  onClassicSessionStart,
  onNotification,
  onPrompt,
  onSessionEnd,
  onSessionStart,
  onSpawned,
  onStep,
  onStopFailure,
  onSubagentStart,
  onToolDone,
  onTurnComplete,
  onTurnStart,
  onWorktreeCreate,
  resetReporter,
  safely,
  sawClassic,
} from './report'
import {
  MARK,
  MAX_FIELD,
  MAX_FILE,
  MAX_SLUG,
  absolute,
  ambiguousNote,
  baseName,
  clean,
  cleanData,
  cleanId,
  conventionBlock,
  expandHome,
  findRefLine,
  hasPlanLine,
  idsIn,
  mentionsPrCreate,
  mentionsPrEnd,
  planLineDeny,
  prBody,
  prEnds,
  prRefIn,
  prUrlIn,
  provisionalId,
  sameRef,
  shortName,
  tildePath,
  titleFrom,
  titleIds,
  unknownDeny,
  unknownNote,
  type Pkg,
  type PrBody,
  type Ref,
  type Unknown,
} from './text'
import { MAX_SUMMARY, redactLine } from './wait'

// The ai-manager mod. Acting: links spawned agents to plan packages,
// appends the plan conventions to their briefs, checks the Plan: line on
// their PRs, offers to register plan files, and serves the plan_status
// tool; /aim watch and /aim unwatch ask aimd to monitor this session or
// stop; the band's progress line and plain /aim open the plan's page in
// the dashboard; a brief whose Package: line names a package its
// registered plan file lacks is linked as provisional and the lead told to
// add the row, or refused with requirePlanRow. Reporting (the hooks first
// below, the work in report.ts): sends aimd the session's events as
// summaries (internal/events in the Go code), batched, with what a dialog waits
// on. Every hook fails open: on any error it ends in next(e) unchanged,
// and aimd being down or slow (1 s) changes nothing. Reporting hooks never
// wait on aimd at all.

// The band's state, in $.state so a reload keeps it. Bump a shape tag when
// its value's fields change.
const offerAtom = atom({ plugin: 'ai-manager', key: 'offer' } as const, null, { shape: 'offer-1' })
const unlinkedAtom = atom({ plugin: 'ai-manager', key: 'unlinked' } as const, [], { shape: 'unlinked-2' })
const progressAtom = atom({ plugin: 'ai-manager', key: 'progress' } as const, null, { shape: 'progress-1' })
const mineAtom = atom({ plugin: 'ai-manager', key: 'mine' } as const, [], { shape: 'mine-1' })
// A classic.* hook has run in this process, so report.ts's stand-ins stay
// quiet. A reload starts report.ts over; this is how it knows.
const classicAtom = atom({ plugin: 'ai-manager', key: 'classic' } as const, false, { shape: 'classic-1' })

/** The tool's short name; the model sees it as mcp__ai-manager__plan_status. */
const TOOL = 'plan_status'
const TOOL_NAME = 'mcp__ai-manager__plan_status'
/** The cached /api/context is fetched again at most this often. */
const CONTEXT_TTL_MS = 60_000
/** The PR check refuses only while aimd answered its most recent call less than this long ago. */
const REACHABLE_MS = 120_000
/** The link map's key for the session's own main loop. */
const MAIN = ''
const MAX_UNLINKED = 3
const MAX_CANDIDATES = 5
/** The link tool: the lead links an agent ai-manager couldn't place. Seen as mcp__ai-manager__plan_link. */
const LINK_TOOL = 'plan_link'
const LINK_TOOL_NAME = 'mcp__ai-manager__plan_link'
/** The most packages one plan_link call (or one agent's title) links. */
const MAX_LINKS = 20
/** A link offer in the band goes after this long, if nothing settled it sooner. */
const OFFER_TTL_MS = 30 * 60_000
const SECTION_ID = 'ai-manager:package'
/** The band's progress line is asked for again this long after its last refresh. */
const POLL_MS = 15_000
/** An event that matters (a turn ends, a PR is created) refreshes it, but at most once in this long. */
const KICK_GAP_MS = 2_000

type Timer = { cancel: () => void }

type Linked = Pkg & { sessionId: string }
/**
 * The package found, or the candidates when the brief names several.
 * `unknown` is set when the brief's Package: line names a package its
 * registered plan's file has no row for: `pkg` is then the package as
 * the agent should treat it, provisional on the board.
 */
type Found = { pkg?: Pkg; also?: Pkg[]; candidates: LinkCandidate[]; isAmbiguous: boolean; unknown?: Unknown }
type Pending = {
  sessionId: string
  description: string
  pkg?: Pkg
  /** The other packages of a title that names several of one plan. */
  also?: Pkg[]
  candidates?: LinkCandidate[]
  unknown?: Unknown
  /** The loop that spawned it, to send it the note; undefined for the main loop. */
  parentAgentId?: string
}

const TOOL_DESCRIPTION = [
  "Where the plans registered with ai-manager for this session's repo stand, from aimd: per plan, the summary",
  '(counts, the packages ready to dispatch, the packages waiting on the user and why, packages unlocked since a time)',
  'and the board (every package with its lane, needs, lifecycle, blocked-by, badges, and linked PRs, branches,',
  'worktrees and agents; unmatched PRs and branches; parser warnings; alerts). Packages marked provisional',
  "aren't in the plan file yet: work named them, and suggested_row is the row to add. Call it before answering",
  '"what\'s ready?" or dispatching agents for plan packages.',
].join(' ')

const TOOL_SCHEMA = {
  type: 'object',
  properties: {
    plan: { type: 'string', description: "A plan's slug. Left out: every plan registered for this repo." },
    since: {
      type: 'string',
      description: 'An RFC 3339 time: the summary then lists the packages unlocked (blocked to ready) after it.',
    },
  },
  additionalProperties: false,
}

const LINK_TOOL_DESCRIPTION = [
  'Link an agent this session spawned to the plan package or packages it works on, when ai-manager',
  "couldn't tell from its brief (it says so in a note naming the candidates). The board then tracks the agent, and",
  "aimd links the agent's pull request to the same packages. Not for agents of other sessions.",
].join(' ')

const LINK_TOOL_SCHEMA = {
  type: 'object',
  properties: {
    agent: {
      type: 'string',
      description: "The agent's id, as the Agent tool's result and ai-manager's note give it, or a remote agent's cloud session URL.",
    },
    plan: { type: 'string', description: "The plan's slug." },
    packages: {
      type: 'array',
      items: { type: 'string' },
      minItems: 1,
      maxItems: MAX_LINKS,
      description: 'The IDs of the packages the agent works on, e.g. ["U1", "U2"].',
    },
  },
  required: ['agent', 'plan', 'packages'],
  additionalProperties: false,
}

function toPkg(plan: string, p: { id: string; title: string; suggested_branch?: string; plan_line?: string }): Pkg {
  return {
    plan,
    id: p.id,
    title: p.title,
    suggestedBranch: p.suggested_branch ?? `${plan}/${p.id.toLowerCase()}`,
    planLine: p.plan_line ?? `Plan: ${plan} · ${p.id}`,
  }
}

async function quietly<T>(work: Promise<T>): Promise<T | undefined> {
  try {
    return await work
  } catch {
    return undefined
  }
}

function toast($: EngineInterface, text: string): void {
  try {
    const shown: unknown = $.ui.toast(text)
    if (shown instanceof Promise) shown.catch(() => undefined)
  } catch {
    // A toast is a nicety.
  }
}

// Module state: register() starts it over, as a reload does (the band's
// state is in $.state). The validator wants every function $ is passed to
// declared at the top of the file, so the state lives here too.
let s: Settings = settings({})
/** The user was told aimdUrl isn't a local address: once a session. */
let warned = false
let ctx: Context | null = null
let ctxCwd = ''
let ctxTriedAt = Number.NEGATIVE_INFINITY
let ctxLoading: Promise<Context | null> | null = null
/** Whether the last context fetch was answered: when not, aimd is down or slow. */
let ctxOk = false
/** agentId (MAIN for the main loop) → the package it is linked to. */
let links = new Map<string, Linked>()
/** Spawns that returned no agentId, by tool_use_id, until the Agent result names a cloud session. */
let pending = new Map<string, Pending>()
let lastDetected: string | undefined
/** The system-prompt section as first composed for this thread: null for none, undefined before. */
let composed: string | null | undefined
/** The package whose block was appended to the conversation after the prompt went out. */
let appendedFor: string | undefined
/** The packages missing from their plan file the lead was told about, by loop and package. */
let noted = new Set<string>()
/** The band's progress line refreshes: the band is on and something draws it (not a -p run). */
let live = false
let refreshing = false
/** An event asked for a refresh while one ran: another follows it. */
let again = false
let lastRefreshAt = Number.NEGATIVE_INFINITY
let poll: Timer | null = null
/** When `poll` fires. */
let pollAt = Number.POSITIVE_INFINITY
/** classicAtom is set (or was found set): it needs no other write. */
let classicKept = false
/** The one look in classicAtom since register(), once started. */
let classicRecall: Promise<void> | null = null

// --- the cached context ------------------------------------------------

async function loadContext($: EngineInterface): Promise<Context | null> {
  const got = await call<Context>($, s, 'GET', `/api/context?cwd=${encodeURIComponent(ctxCwd)}`)
  if (got.ok && got.data !== undefined && Array.isArray(got.data.plans)) {
    ctx = got.data
    ctxOk = true
    await adoptMain($)
  } else {
    ctxOk = false
  }
  return ctx
}

/** The cached context, fetched again when older than a minute (or forced). */
async function context($: EngineInterface, force = false): Promise<Context | null> {
  if (ctxCwd === '') return ctx
  const now = await $.clock.now()
  if (!force && now - ctxTriedAt < CONTEXT_TTL_MS) return ctx
  if (ctxLoading !== null) return ctxLoading
  ctxTriedAt = now
  ctxLoading = loadContext($).finally(() => {
    ctxLoading = null
  })
  return ctxLoading
}

/** The session's own worktree or branch is linked: its main loop counts as linked. */
async function adoptMain($: EngineInterface): Promise<void> {
  const p = ctx?.package
  // A low-confidence link (a bare ID token in the branch) is a guess: not enough to act on.
  if (p === undefined || p === null || p.link?.confidence === 'low') {
    links.delete(MAIN)
    return
  }
  const pkg = toPkg(p.plan, p)
  links.set(MAIN, { ...pkg, sessionId: '' })
  // Linked after the system prompt went out without it: tell the model in
  // a new row, once per package (prompt.compose has already run).
  const key = `${pkg.plan}·${pkg.id}`
  if (composed === null && s.appendConventions && appendedFor !== key) {
    appendedFor = key
    await quietly(
      $.session.append({ message: { type: 'user', content: [{ type: 'text', text: conventionBlock(pkg, 'session') }] } }),
    )
  }
}

/**
 * The convention block to put beside a prompt, or undefined for none: for
 * a linked session whose system prompt this plugin never composed, in a
 * process where no classic hook has run either (both are handed past it
 * together). A slash command's submission takes none.
 */
function conventionsBeside(e: { command?: unknown; isMeta?: unknown }): string | undefined {
  try {
    if (!s.appendConventions || composed !== undefined || classicRuns() || e.command !== undefined || e.isMeta === true) return undefined
    const p = ctx?.package
    if (!links.has(MAIN) || p === undefined || p === null) return undefined
    const pkg = toPkg(p.plan, p)
    const key = `${pkg.plan}·${pkg.id}`
    if (appendedFor === key) return undefined
    appendedFor = key
    return conventionBlock(pkg, 'session')
  } catch {
    return undefined
  }
}

/** A classic.* hook ran: report.ts's stand-ins stop, and classicAtom keeps that past a reload. */
function heardClassic($: EngineInterface): void {
  sawClassic()
  if (classicKept) return
  classicKept = true
  void quietly(update($, classicAtom, () => true))
}

/**
 * A reload starts report.ts over, as if no classic hook had run, and the
 * ones that did (classic.SessionStart above all) don't come again. Whether
 * one ran before it in this process is in classicAtom, looked up once, on
 * the first event that could arm a stand-in. The stand-ins wait seconds
 * before they speak; the look takes far less.
 */
function recallClassic($: EngineInterface): void {
  classicRecall ??= quietly(read($, classicAtom)).then(seen => {
    if (seen !== true) return
    classicKept = true
    sawClassic()
  })
}

function linkedFor(agentId: string | undefined): Linked | undefined {
  return links.get(agentId ?? MAIN)
}

async function sessionIdFor($: EngineInterface, linked: Linked): Promise<string> {
  return linked.sessionId !== '' ? linked.sessionId : $.session.id()
}

// --- finding the package ------------------------------------------------

function packagesWithId(plans: readonly ContextPlan[], ids: readonly string[]): Pkg[] {
  const out: Pkg[] = []
  for (const id of ids) {
    for (const plan of plans) {
      const p = plan.packages.find(one => one.id === id)
      if (p !== undefined && !out.some(o => o.plan === plan.slug && o.id === p.id)) out.push(toPkg(plan.slug, p))
    }
  }
  return out
}

/** A Package: line naming a plan this repo's context doesn't list: ask aimd for that plan. */
async function byRefRemote($: EngineInterface, ref: Ref, description: string): Promise<Named | undefined> {
  const got = await call<Board>($, s, 'GET', `/api/plans/${encodeURIComponent(ref.slug)}`)
  // Not registered (or aimd down): left alone.
  if (!got.ok || !Array.isArray(got.data?.packages)) return undefined
  const plan: ContextPlan = { slug: got.data.slug, title: got.data.title, path: got.data.path, band: '', packages: got.data.packages }
  return named(plan, ref, description)
}

/** The package a Package: line names in a registered plan, and whether its file lacks the row. */
type Named = { pkg: Pkg; unknown?: Unknown }

/**
 * The package `ref` names in a registered plan: the file's row; or, when
 * the file has none, the provisional package aimd shows (or will make) for
 * it, with `unknown` set. An ID that isn't one is left alone.
 */
function named(plan: ContextPlan, ref: Ref, description: string): Named | undefined {
  const p = plan.packages.find(one => sameRef(ref, { plan: plan.slug, id: one.id }))
  if (p !== undefined && p.provisional !== true) return { pkg: toPkg(plan.slug, p) }
  const id = p?.id ?? provisionalId(ref.id)
  if (id === undefined) return undefined
  const unknown: Unknown = { plan: plan.slug, id, file: baseName(plan.path ?? '') || 'the plan file' }
  if (p !== undefined) return { pkg: toPkg(plan.slug, p), unknown }
  const title = titleFrom(id, description)
  const short = shortName(title)
  return {
    pkg: toPkg(plan.slug, { id, title, suggested_branch: `${plan.slug.toLowerCase()}/${id.toLowerCase()}${short === '' ? '' : `-${short}`}` }),
    unknown,
  }
}

/**
 * The package a Package: line names. A plan of this repo is looked up in
 * the context; one the file lacks is asked of aimd again first, since the
 * row may have just landed, and counts as missing only once aimd has
 * answered (aimd down: the line finds nothing). A plan of
 * another repo is asked of aimd only while it answers, to stay within 1 s.
 */
async function byPackageLine($: EngineInterface, ref: Ref, description: string, c: Context | null): Promise<Named | undefined> {
  const same = (p: ContextPlan) => p.slug.toLowerCase() === ref.slug.toLowerCase()
  const plan = (c?.plans ?? []).find(same)
  if (plan === undefined) return ctxOk ? byRefRemote($, ref, description) : undefined
  const first = named(plan, ref, description)
  if (first?.unknown === undefined) return first
  const fresh = await context($, true)
  const again = ctxOk ? (fresh?.plans ?? []).find(same) : undefined
  if (again === undefined) {
    // aimd didn't answer, or the plan is gone: a provisional package is still the one to link; nothing new is made.
    return first.unknown !== undefined && plan.packages.some(p => p.id === first.pkg.id) ? { pkg: first.pkg } : undefined
  }
  return named(again, ref, description)
}

const asCandidate = (p: Pkg): LinkCandidate => ({ plan: p.plan, id: p.id, title: p.title })

/** The order: the Package: line, an ID in the description, one package ID in the prompt. */
async function findPackage(
  $: EngineInterface,
  e: { prompt: string; description: string; workflow?: unknown },
  c: Context | null,
): Promise<Found> {
  const plans = c?.plans ?? []
  const ref = findRefLine(e.prompt, 'package')
  if (ref !== undefined) {
    const got = await byPackageLine($, ref, e.description, c)
    if (got !== undefined) return { pkg: got.pkg, candidates: [], isAmbiguous: false, unknown: got.unknown }
  }
  // A workflow agent's prompt may be a placeholder: only its Package: line counts.
  if (e.workflow !== undefined) return { candidates: [], isAmbiguous: false }
  const isKnown = (id: string) => packagesWithId(plans, [id]).length > 0
  // The title (the Agent call's description) first, ranges spelled out; then the brief's IDs.
  for (const [text, isTitle] of [
    [e.description, true],
    [e.prompt, false],
  ] as const) {
    const found = packagesWithId(plans, isTitle ? titleIds(text, isKnown) : idsIn(text))
    if (found.length === 1) return { pkg: found[0], candidates: [], isAmbiguous: false }
    // A title naming several packages of one plan ("front end U1-U3"): the agent works on all
    // of them. A brief that only mentions several (S3 in passing) settles nothing.
    const [first, ...rest] = found
    if (isTitle && first !== undefined && found.length <= MAX_LINKS && found.every(p => p.plan === first.plan)) {
      return { pkg: first, also: rest, candidates: [], isAmbiguous: false }
    }
    // Two or more of this repo's packages (several IDs, or one ID two plans share).
    if (found.length > 1) return { candidates: found.slice(0, MAX_CANDIDATES).map(asCandidate), isAmbiguous: true }
  }
  // No package ID at all: a general helper, not plan work. Nothing to offer.
  return { candidates: [], isAmbiguous: false }
}

// --- talking to aimd ------------------------------------------------------

async function postSpawn(
  $: EngineInterface,
  sessionId: string,
  target: { agent_id: string } | { cloud_url: string },
  pkg: Pick<Pkg, 'plan' | 'id'>,
  provisional?: { title: string },
): Promise<{ ok: boolean; error?: string }> {
  // A package the plan file lacks: aimd makes it provisional, titled from the spawn's description
  // (redacted, as an event's text is: aimd stores it and serves it).
  const extra = provisional === undefined ? {} : { provisional: true, title: redactLine(provisional.title, MAX_SUMMARY) }
  const got = await call($, s, 'POST', '/api/links/spawn', { session_id: sessionId, ...target, plan: pkg.plan, pkg: pkg.id, ...extra })
  return got.ok ? { ok: true } : { ok: false, error: got.error }
}

/**
 * Links a spawned agent to its package and to the other packages its title
 * named, one call each (aimd keeps one link per package). The first answer
 * is the one that counts for the caller; the others are best effort.
 */
async function postSpawns(
  $: EngineInterface,
  sessionId: string,
  target: { agent_id: string } | { cloud_url: string },
  pkg: Pick<Pkg, 'plan' | 'id'>,
  also: readonly Pick<Pkg, 'plan' | 'id'>[],
  provisional?: { title: string },
): Promise<{ ok: boolean; error?: string }> {
  const first = await postSpawn($, sessionId, target, pkg, provisional)
  for (const other of also) await postSpawn($, sessionId, target, other)
  return first
}

/**
 * Tells the loop that spawned for a package its plan file lacks to add the
 * row, once per package: a user-role row the model reads at its next
 * request. Never throws.
 */
async function noteUnknown($: EngineInterface, u: Unknown, agentId: string | undefined): Promise<void> {
  const key = `${agentId ?? MAIN}\u0000${u.plan.toLowerCase()}\u0000${u.id.toUpperCase()}`
  if (noted.has(key)) return
  noted.add(key)
  const message = { type: 'user' as const, content: [{ type: 'text' as const, text: unknownNote(u) }] }
  await quietly($.session.append(agentId === undefined ? { message } : { agentId, message }))
}

async function postPr($: EngineInterface, linked: Linked, agentId: string | undefined, url: string): Promise<void> {
  const sessionId = await sessionIdFor($, linked)
  await call($, s, 'POST', '/api/links/pr', { session_id: sessionId, agent_id: agentId ?? '', url })
}

/**
 * Asks aimd to read a PR from GitHub now (POST /api/prs/refresh): a tool
 * call just created, merged or closed it. Only which PR is sent; aimd
 * reads its state from GitHub. Never throws.
 */
async function postRefresh($: EngineInterface, pr: { ref: string; repo?: string; action: 'create' | 'merge' | 'close' }): Promise<void> {
  const cwd = (await quietly($.session.cwd())) ?? ctxCwd
  await call($, s, 'POST', '/api/prs/refresh', { ...pr, cwd })
}

/** postRefresh from a hook: unawaited, then the band's line follows. */
function refreshSoon($: EngineInterface, pr: { ref: string; repo?: string; action: 'create' | 'merge' | 'close' }): void {
  void postRefresh($, pr).then(() => kick($), () => kick($))
}

/** What a Bash call printed: its text, stdout and stderr. */
function bashOutput(ran: { text?: string; result?: unknown }): string {
  const r = ran.result as { stdout?: unknown; stderr?: unknown } | undefined
  return [ran.text ?? '', typeof r?.stdout === 'string' ? r.stdout : '', typeof r?.stderr === 'string' ? r.stderr : ''].join('\n')
}

async function registerPlan($: EngineInterface, path: string): Promise<string> {
  const sessionId = await quietly($.session.id())
  const got = await call<Registered>($, s, 'POST', '/api/plans', { path, session_id: sessionId })
  if (!got.ok) {
    // The whole path, home as ~: a wrong guess at where the file is shows at once.
    const home = await quietly($.env.get('HOME'))
    return `ai-manager couldn't register ${clean(tildePath(path, home), MAX_FIELD)}: ${got.error}`
  }
  const offer = await read($, offerAtom)
  if (offer !== null && (offer.path === path || offer.path === got.data.path)) await update($, offerAtom, () => null)
  await context($, true)
  if (typeof got.data.slug === 'string' && got.data.slug !== '') await rememberPlan($, got.data.slug)
  kick($)
  const verb = got.data.created ? 'Registered' : 'Updated'
  return `${verb} plan ${clean(got.data.slug, MAX_SLUG)} (${got.data.packages} packages) from ${clean(baseName(got.data.path), MAX_FILE)}.`
}

/**
 * /aim watch and /aim unwatch: asks aimd to monitor this
 * session, or to stop, and says what happened in one line. aimd down or
 * slow is an answer too, never an error.
 */
async function setWatch($: EngineInterface, on: boolean): Promise<string> {
  const verb = on ? 'watch' : 'unwatch'
  const sessionId = await quietly($.session.id())
  if (typeof sessionId !== 'string' || sessionId === '') {
    return `ai-manager couldn't tell this session's id, so it can't ${verb} it.`
  }
  if (!s.local) return NOT_LOCAL
  const got = await call<Watched>($, s, on ? 'POST' : 'DELETE', '/api/sessions/watch', { session_id: sessionId })
  if (got.ok) {
    const message = clean(got.data?.message, MAX_FIELD)
    return `ai-manager: ${message !== '' ? message : on ? 'watching this session.' : 'stopped watching this session.'}`
  }
  if (got.status === 0) {
    return `ai-manager: aimd isn't answering at ${s.url}, so nothing changed. Start it with \`aim up\`, then run /aim ${verb} again.`
  }
  return `ai-manager couldn't ${verb} this session: ${got.error}`
}

async function dismissPlan($: EngineInterface, offer: PlanOffer): Promise<void> {
  const got = await call($, s, 'POST', '/api/plans/dismiss', { path: offer.path })
  if (!got.ok) {
    toast($, `ai-manager: ${got.error}`)
    return
  }
  await update($, offerAtom, current => (current?.path === offer.path ? null : current))
}

async function detect($: EngineInterface, path: string): Promise<void> {
  const got = await call<Detection>($, s, 'POST', '/api/plans/detect', { path })
  if (!got.ok) return
  const d = got.data
  if (d.is_plan) lastDetected = d.path
  if (d.offer) {
    const offer: PlanOffer = { path: d.path, file: baseName(d.path), packages: d.packages, lanes: d.lanes, slug: d.slug }
    await update($, offerAtom, () => offer)
  } else {
    await update($, offerAtom, current => (current?.path === d.path ? null : current))
  }
}

/** Keeps a spawn that returned no agentId until its Agent result arrives (a bounded few). */
function remember(toolUseId: string, waiting: Pending): void {
  pending.set(toolUseId, waiting)
  for (const key of pending.keys()) {
    if (pending.size <= 20) break
    pending.delete(key)
  }
}

async function addUnlinked($: EngineInterface, entry: UnlinkedSpawn): Promise<void> {
  await update($, unlinkedAtom, list => [entry, ...list.filter(one => one.key !== entry.key)].slice(0, MAX_UNLINKED))
}

async function dropUnlinked($: EngineInterface, key: string): Promise<void> {
  await update($, unlinkedAtom, list => list.filter(one => one.key !== key))
}

/** Drops the link offers older than OFFER_TTL_MS. Writes only when one goes, so the band isn't redrawn for nothing. */
async function pruneUnlinked($: EngineInterface): Promise<void> {
  const now = await $.clock.now()
  const list = await read($, unlinkedAtom)
  if (list.some(one => !(now - one.at < OFFER_TTL_MS))) {
    await update($, unlinkedAtom, current => current.filter(one => now - one.at < OFFER_TTL_MS))
  }
}

/**
 * Tells the loop that spawned an agent ai-manager couldn't place which
 * packages its brief mentions, and how to link it (plan_link): a
 * user-role row the model reads at its next request. Never throws.
 */
async function noteAmbiguous(
  $: EngineInterface,
  description: string,
  agent: string,
  candidates: readonly LinkCandidate[],
  parentAgentId: string | undefined,
): Promise<void> {
  const message = { type: 'user' as const, content: [{ type: 'text' as const, text: ambiguousNote(description, agent, candidates) }] }
  await quietly($.session.append(parentAgentId === undefined ? { message } : { agentId: parentAgentId, message }))
}

/** An agent ai-manager couldn't place: the note to the loop that spawned it, and the band's offer (`full` only). */
async function offerLink(
  $: EngineInterface,
  entry: Omit<UnlinkedSpawn, 'at'>,
  parentAgentId: string | undefined,
): Promise<void> {
  await noteAmbiguous($, entry.description, entry.key, entry.candidates, parentAgentId)
  // Only an ambiguous brief from the main loop is offered in the band.
  if (s.bandLinks && parentAgentId === undefined) await addUnlinked($, { ...entry, at: await $.clock.now() })
}

/**
 * plan_link: links an agent of this session (its id, or a remote agent's
 * cloud session URL) to one or more packages of one plan, one call to
 * aimd each, and says what happened in one line. Never throws.
 */
async function linkAgent($: EngineInterface, input: { agent?: unknown; plan?: unknown; packages?: unknown }): Promise<string> {
  const agent = typeof input.agent === 'string' ? input.agent.trim() : ''
  const plan = typeof input.plan === 'string' ? input.plan.trim() : ''
  const asked = Array.isArray(input.packages) ? input.packages : []
  const ids: string[] = []
  for (const one of asked) {
    const id = typeof one === 'string' ? one.trim() : ''
    if (id !== '' && !ids.some(have => have.toUpperCase() === id.toUpperCase())) ids.push(id)
  }
  if (agent === '' || plan === '' || ids.length === 0) {
    return 'Nothing linked: give agent (its id or cloud session URL), plan (its slug) and packages (at least one ID).'
  }
  if (ids.length > MAX_LINKS) return `Nothing linked: at most ${MAX_LINKS} packages at once.`
  const sessionId = await quietly($.session.id())
  if (typeof sessionId !== 'string' || sessionId === '') return "Nothing linked: ai-manager couldn't tell this session's id."
  const target = /^https?:\/\//i.test(agent) ? { cloud_url: agent } : { agent_id: agent }
  const linked: string[] = []
  const failed: string[] = []
  for (const id of ids) {
    const sent = await postSpawn($, sessionId, target, { plan, id })
    if (sent.ok) linked.push(cleanId(id))
    else failed.push(`${cleanId(id)} (${clean(sent.error ?? 'failed', MAX_FIELD)})`)
  }
  const slug = clean(plan, MAX_SLUG)
  if (linked.length === 0) return `Nothing linked to ${slug}: ${failed.join('; ')}.`
  await settleOffer($, agent)
  await rememberPlan($, plan)
  // The PR check asks for the first package's Plan: line, as for an agent linked at spawn.
  if ('agent_id' in target) {
    const first = ids.find(id => linked.includes(cleanId(id))) ?? ids[0] ?? ''
    const known = ctx?.plans.find(p => p.slug === plan)?.packages.find(p => p.id === first)
    links.set(agent, { ...toPkg(plan, known ?? { id: first, title: '' }), sessionId })
  }
  kick($)
  const done = `Linked the agent to ${slug} · ${linked.join(', ')}.`
  return failed.length === 0 ? done : `${done} Not linked: ${failed.join('; ')}.`
}

/** The agent was linked, finished, or stopped: its offer, if any, goes. */
async function settleOffer($: EngineInterface, key: string): Promise<void> {
  const list = await read($, unlinkedAtom)
  if (list.some(one => one.key === key)) await dropUnlinked($, key)
}

async function linkFromBand($: EngineInterface, entry: UnlinkedSpawn, c: LinkCandidate): Promise<void> {
  const target = entry.agentId !== undefined ? { agent_id: entry.agentId } : { cloud_url: entry.cloudUrl ?? '' }
  const sent = await postSpawn($, entry.sessionId, target, { plan: c.plan, id: c.id })
  if (!sent.ok) {
    toast($, `ai-manager: ${sent.error ?? 'the link failed'}`)
    return
  }
  await dropUnlinked($, entry.key)
  await rememberPlan($, c.plan)
  kick($)
  const plan = ctx?.plans.find(p => p.slug === c.plan)
  const known = plan?.packages.find(p => p.id === c.id)
  const pkg = toPkg(c.plan, known ?? { id: c.id, title: c.title })
  if (entry.agentId !== undefined) {
    links.set(entry.agentId, { ...pkg, sessionId: entry.sessionId })
    if (s.appendConventions) {
      // The brief went out without the block: hand it to the running agent.
      await quietly(
        $.session.append({
          agentId: entry.agentId,
          message: { type: 'user', content: [{ type: 'text', text: conventionBlock(pkg, 'agent') }] },
        }),
      )
    }
  }
  toast($, `Linked ${entry.description} to ${clean(c.plan, MAX_SLUG)} · ${cleanId(c.id)}`)
}

async function readText($: EngineInterface, path: string): Promise<string | undefined> {
  const dir = await quietly($.session.cwd())
  const text = await quietly($.fs.read(absolute(path, dir ?? ctxCwd)))
  return typeof text === 'string' ? text : undefined
}

// --- the band's progress line ------------------------------------------------
//
// One line for one of the repo's plans, from aimd's plan summary: refreshed
// POLL_MS after the last refresh, and sooner on an event that matters (at
// most once per KICK_GAP_MS, a burst collapsing into one more). Drawing
// never asks aimd: the band reads $.state. No hook waits on a refresh, and
// any failure (aimd down or slow, an answer the mod doesn't know) shows no
// line rather than an error.

/** The repo's plans, the one the band shows first (planOrder). */
async function bandPlans($: EngineInterface, c: Context | null): Promise<string[]> {
  const plans = Array.isArray(c?.plans) ? c.plans : []
  const repoPlans = plans.map(p => p?.slug).filter((slug): slug is string => typeof slug === 'string' && slug !== '')
  if (repoPlans.length === 0) return []
  let mine: unknown
  try {
    mine = await read($, mineAtom)
  } catch {
    mine = []
  }
  const preferred = Array.isArray(mine) ? mine.filter((one): one is string => typeof one === 'string') : []
  return planOrder(repoPlans, [links.get(MAIN)?.plan, ...preferred])
}

/** Remembers a plan this session registered or dispatched for, so the band prefers it. Never throws. */
async function rememberPlan($: EngineInterface, slug: string): Promise<void> {
  try {
    await update($, mineAtom, list => withFirst(Array.isArray(list) ? list : [], slug))
  } catch {
    // The band falls back to the repo's first plan.
  }
}

function cancelPoll(): void {
  const t = poll
  poll = null
  pollAt = Number.POSITIVE_INFINITY
  if (t === null) return
  try {
    t.cancel()
  } catch {
    // already gone
  }
}

/** The next refresh, `ms` from `now`. */
function armPoll($: EngineInterface, ms: number, now: number): void {
  cancelPoll()
  const wait = Math.max(1, ms)
  try {
    poll = $.clock.after(wait, () => {
      poll = null
      pollAt = Number.POSITIVE_INFINITY
      void refreshBand($).catch(() => undefined)
    })
    pollAt = now + wait
  } catch {
    poll = null
  }
}

/** Asks aimd for the shown plan's summary and sets the line; never throws, one at a time. */
async function refreshBand($: EngineInterface): Promise<void> {
  if (!live) return
  if (refreshing) {
    again = true
    return
  }
  refreshing = true
  again = false
  cancelPoll()
  // Each refresh (every POLL_MS at most) also lets old link offers go.
  await quietly(pruneUnlinked($))
  let line: PlanProgress | null = null
  try {
    const c = await context($)
    const slugs = await bandPlans($, c)
    const slug = slugs[0]
    if (slug !== undefined) {
      const got = await call<Summary>($, s, 'GET', `/api/plans/${encodeURIComponent(slug)}/summary`)
      // Not answered (aimd down or slow, the plan unregistered since): no line.
      if (got.ok) line = progressOf(got.data, slugs.length - 1) ?? null
    }
  } catch {
    line = null
  }
  try {
    const shown = await read($, progressAtom)
    if (!sameProgress(shown, line)) await update($, progressAtom, () => line)
  } catch {
    // The line stays as it was.
  }
  let now = lastRefreshAt
  try {
    now = await $.clock.now()
  } catch {
    // keep the last time
  }
  lastRefreshAt = now
  refreshing = false
  const soon = again
  again = false
  if (live) armPoll($, soon ? KICK_GAP_MS : POLL_MS, now)
}

/** An event that matters: refresh now, or once KICK_GAP_MS has passed since the last. */
async function kickBand($: EngineInterface): Promise<void> {
  if (!live) return
  if (refreshing) {
    again = true
    return
  }
  const now = await $.clock.now()
  const due = lastRefreshAt + KICK_GAP_MS
  if (now >= due) await refreshBand($)
  else if (pollAt > due) armPoll($, due - now, now)
}

/** kickBand from a hook: unawaited, and nothing it does reaches the hook. */
function kick($: EngineInterface): void {
  try {
    void kickBand($).catch(() => undefined)
  } catch {
    // best effort
  }
}

/** Opens a URL in the browser (macOS `open`, else `xdg-open`); whether it opened. Never throws. */
async function openUrl($: EngineInterface, url: string): Promise<boolean> {
  for (const opener of ['open', 'xdg-open']) {
    try {
      const ran = await $.process.run([opener, url], { timeoutMs: 5000 })
      if (ran.exitCode === 0) return true
    } catch {
      // try the next
    }
  }
  return false
}

/**
 * Opens a dashboard page (a plan's, or the Overview) in the browser;
 * whether it opened. aimd serves its pages only to a signed-in browser, so
 * the mod asks it (with the token) for a link that signs the browser in
 * and goes on to the page: it works once, within a minute. An aimd from
 * before sign-in has no such link and serves the page as is. The link is a
 * credential until it is used, so it is opened and never shown. Never throws.
 */
async function openApp($: EngineInterface, slug?: string): Promise<boolean> {
  // An aimdUrl that isn't this machine is never opened (aimd.ts, localBase).
  if (!s.local) return false
  const page = appUrl(s.url, slug)
  const link = await call<{ path?: unknown }>($, s, 'POST', '/api/sign-in', { return: page.slice(s.url.length) })
  const path = link.ok && typeof link.data?.path === 'string' && link.data.path.startsWith('/app/') ? link.data.path : undefined
  return openUrl($, path === undefined ? page : s.url + path)
}

/** The band's press: the plan's page in the dashboard, or a toast saying where it is. */
async function openPlan($: EngineInterface, slug: string): Promise<void> {
  if (!s.local) return
  if (!(await openApp($, slug))) toast($, `ai-manager: plan ${clean(slug, MAX_SLUG)}'s page is at ${appUrl(s.url, slug)}`)
}

/** /aim's plan: the one the band shows (or would), undefined when the repo has none or aimd is down. */
async function sessionPlan($: EngineInterface): Promise<string | undefined> {
  try {
    return (await bandPlans($, await context($)))[0]
  } catch {
    return undefined
  }
}

/** Why a PR body fails the check, or undefined when it passes (or can't be read). */
async function bodyVerdict($: EngineInterface, body: PrBody, pkg: Pkg): Promise<string | undefined> {
  if (body.kind === 'text') return hasPlanLine(body.text, pkg) ? undefined : planLineDeny(pkg)
  if (body.kind === 'file') {
    const text = await readText($, body.path)
    if (text === undefined || hasPlanLine(text, pkg)) return undefined
    return planLineDeny(pkg, body.path)
  }
  return undefined
}

export const register: Register = (on, options) => {
  s = settings(options)
  warned = false
  ctx = null
  ctxCwd = ''
  ctxTriedAt = Number.NEGATIVE_INFINITY
  ctxLoading = null
  ctxOk = false
  resetHealth()
  links = new Map()
  pending = new Map()
  lastDetected = undefined
  composed = undefined
  appendedFor = undefined
  noted = new Set()
  cancelPoll()
  live = false
  refreshing = false
  again = false
  lastRefreshAt = Number.NEGATIVE_INFINITY
  resetReporter(s)
  classicKept = false
  classicRecall = null

  // --- reporting: each hook queues an event and passes e on --------------------
  // Nothing here waits on aimd or on a $ call: the work is in report.ts,
  // unawaited, and safely() swallows anything it throws.

  on('classic.SessionStart', ($, e, next) => {
    safely(() => heardClassic($))
    safely(() => onClassicSessionStart($, e))
    return next(e)
  }).catch(($, e, next) => next(e))

  // session.start, session.end and agent.spawn report from the hooks below
  // (an event takes one hook without a matcher).

  on('prompt.submit', ($, e, next) => {
    safely(() => recallClassic($))
    safely(() => onPrompt($, e))
    // prompt.compose's convention block, where prompt.compose doesn't
    // reach this plugin (the organization's security default hands it past,
    // with the classic events): it rides beside a linked session's next
    // prompt instead, once per package.
    const block = conventionsBeside(e)
    return block === undefined ? next(e) : next({ ...e, context: [...(e.context ?? []), block] })
  }).catch(($, e, next) => next(e))

  on('turn.start', ($, e, next) => {
    safely(() => onTurnStart($, e))
    return next(e)
  }).catch(($, e, next) => next(e))

  // Each model request: once its response is in, the loop's token count: a
  // subagent's for the next agent_list, the main loop's for its next tool
  // and turn_complete events. The chunks pass through untouched.
  on('turn.step', async function* ($, e, next) {
    const out = yield* next(e)
    safely(() => onStep(e.agentId, out?.usage))
    return out
  }).catch(async function* ($, e, next) {
    return yield* next(e)
  })

  on('turn.complete', ($, e, next) => {
    safely(() => recallClassic($))
    safely(() => onTurnComplete($, e))
    // A turn's end (the main loop's or an agent's) may have moved the plan.
    kick($)
    return next(e)
  }).catch(($, e, next) => next(e))

  // Every tool call: a question or a plan approval opens its dialog before
  // the call runs; any call reports its last-action label after it.
  on('tool.call', async ($, e, next) => {
    safely(() => {
      if (e.tool === 'AskUserQuestion' || e.tool === 'ExitPlanMode') {
        onAsk($, {
          source: e.tool === 'AskUserQuestion' ? 'ask_user_question' : 'exit_plan_mode',
          agentId: e.agentId,
          toolUseId: e.tool_use_id,
          tool: e.tool,
          input: argsOf(e),
        })
      }
    })
    // next(e) runs tool.check, any permission dialog and the tool itself.
    const ran = await next(e)
    safely(() => onToolDone($, e))
    return ran
  }).catch(($, e, next) => next(e))

  // An ask on a real call that a person could answer: approver "person",
  // or absent (as under -p). A host's ask isn't the person's. It is only
  // noted: the mode's decider answers it first (auto mode's classifier,
  // with no dialog), and classic.PermissionRequest reports a dialog.
  on('tool.check', async ($, e, next) => {
    safely(() => recallClassic($))
    const verdict = await next(e)
    safely(() => {
      if (verdict.decision === 'ask' && e.tool_use_id !== undefined && (e.approver === undefined || e.approver === 'person')) {
        noteAsk({ ...(e.agentId !== undefined ? { agentId: e.agentId } : {}), toolUseId: e.tool_use_id })
      }
    })
    return verdict
  }).catch(($, e, next) => next(e))

  on('classic.PermissionRequest', ($, e, next) => {
    safely(() => heardClassic($))
    safely(() => onAsk($, { source: 'permission_request', agentId: e.agent_id, tool: e.tool_name, input: e.tool_input }))
    return next(e)
  }).catch(($, e, next) => next(e))

  on('classic.Notification', ($, e, next) => {
    safely(() => heardClassic($))
    safely(() => onNotification($, e))
    return next(e)
  }).catch(($, e, next) => next(e))

  on('classic.StopFailure', ($, e, next) => {
    safely(() => heardClassic($))
    safely(() => onStopFailure($, e))
    return next(e)
  }).catch(($, e, next) => next(e))

  on('classic.WorktreeCreate', ($, e, next) => {
    safely(() => heardClassic($))
    safely(() => onWorktreeCreate($, e))
    return next(e)
  }).catch(($, e, next) => next(e))

  // Each run of a subagent, a resumed one's too: what makes a loop an
  // agent for aimd, rather than one of Claude Code's own forks.
  on('classic.SubagentStart', ($, e, next) => {
    safely(() => heardClassic($))
    safely(() => onSubagentStart($, e))
    return next(e)
  }).catch(($, e, next) => next(e))

  // An agent that stops needs no offer to link it any more: its PR, if any, links it.
  on('classic.SubagentStop', ($, e, next) => {
    safely(() => heardClassic($))
    void settleOffer($, e.agent_id).catch(() => undefined)
    return next(e)
  }).catch(($, e, next) => next(e))

  // --- session start: context, commands, the tool -----------------------------

  on('session.start', async ($, e, next) => {
    // Reporting first: it gathers the session's facts unawaited.
    safely(() => onSessionStart($, e))
    if (!s.local && !warned) {
      warned = true
      toast($, NOT_LOCAL)
    }
    ctxCwd = (await quietly($.session.root())) || e.cwd
    ctx = null
    ctxTriedAt = Number.NEGATIVE_INFINITY
    composed = undefined
    appendedFor = undefined
    links.delete(MAIN)
    const c = await quietly(context($, true))
    const hasPlans = (c?.plans.length ?? 0) > 0
    await quietly(
      $.command.register({
        name: 'plan-register',
        description: 'Register a dispatch plan with ai-manager (default: the last file detected as a plan)',
        argumentHint: '[path]',
        immediate: true,
      }),
    )
    await quietly(
      $.command.register({
        name: 'aim',
        description: "Open this session's plan in the ai-manager dashboard; /aim watch or /aim unwatch to monitor this session or stop",
        argumentHint: '[watch | unwatch]',
        immediate: true,
      }),
    )
    await quietly(
      $.tool.register({
        name: TOOL,
        description: TOOL_DESCRIPTION,
        inputSchema: TOOL_SCHEMA,
        // In the prompt's tool list where this repo has plans; behind ToolSearch elsewhere.
        ...(hasPlans ? { isDeferred: false } : {}),
      }),
    )
    await quietly(
      $.tool.register({
        name: LINK_TOOL,
        description: LINK_TOOL_DESCRIPTION,
        inputSchema: LINK_TOOL_SCHEMA,
        ...(hasPlans ? { isDeferred: false } : {}),
      }),
    )
    // The band's progress line, where something draws the band (not a -p
    // run): its first refresh, unawaited, then one every POLL_MS.
    live = s.band && (e.isInteractive === true || (e.surface !== null && e.surface !== undefined))
    if (live) void refreshBand($).catch(() => undefined)
    return next(e)
  }).catch(($, e, next) => next(e))

  // A client that draws (the desktop app, a phone) joined a session that
  // started drawing nowhere: the band's line starts refreshing.
  on('session.attach', ($, e, next) => {
    if (s.band && (e.surface === 'terminal' || e.surface === 'desktop') && !live) {
      live = true
      kick($)
    }
    return next(e)
  }).catch(($, e, next) => next(e))

  on('session.end', ($, e, next) => {
    safely(() => recallClassic($))
    safely(() => onSessionEnd($, e))
    // After /clear the next thread composes its own system prompt.
    composed = undefined
    appendedFor = undefined
    // The session itself ending: the band's line stops refreshing.
    if (e.reason !== 'clear') {
      live = false
      cancelPoll()
    }
    return next(e)
  }).catch(($, e, next) => next(e))

  // --- prompt.compose: the convention block for a linked session -------------

  on('prompt.compose', async ($, e, next) => {
    const out = await next(e)
    if (!s.appendConventions) return out
    // Served from the session.start cache, never from aimd, and the same
    // answer for the whole thread (the prompt is snapshotted).
    if (composed === undefined) {
      const p = ctx?.package
      composed = links.has(MAIN) && p !== undefined && p !== null ? conventionBlock(toPkg(p.plan, p), 'session') : null
    }
    if (composed === null || out.sections.some(one => one.id === SECTION_ID)) return out
    return { ...out, sections: [...out.sections, { id: SECTION_ID, text: composed, scope: 'session' }] }
  }).catch(($, e, next) => next(e))

  // --- agent.spawn: find the package, append the conventions, link ---------

  on('agent.spawn', async ($, e, next) => {
    // A resume is a SendMessage wake: it can't be rewritten and is linked already.
    if (e.resume !== undefined) {
      const resumed = await next(e)
      safely(() => onSpawned($, e, resumed.agentId))
      return resumed
    }
    const c = await context($)
    const found = await findPackage($, e, c)
    const pkg = found.pkg
    const also = found.also ?? []
    const unknown = found.unknown
    // requirePlanRow (off by default): a brief for a package its plan
    // file lacks is refused, saying what to do. Only once aimd has said so
    // (found.unknown): with aimd down nothing is refused.
    if (unknown !== undefined && s.requirePlanRow) return { deny: unknownDeny(unknown) }
    const rewrite = pkg !== undefined && s.appendConventions && !e.prompt.includes(MARK)
    const out = await next(rewrite ? { ...e, prompt: `${e.prompt.trimEnd()}\n\n${conventionBlock(pkg, 'agent', also)}` } : e)
    // Reporting: the agent's flags, and an agent_list snapshot.
    safely(() => onSpawned($, e, out.agentId))
    if (out.deny !== undefined) return out
    const sessionId = await $.session.id()
    if (pkg !== undefined) {
      // This session dispatches for the plan: the band prefers it.
      void rememberPlan($, pkg.plan)
      const provisional = unknown === undefined ? undefined : { title: e.description }
      if (out.agentId !== undefined) {
        // The PR check asks for the first package's Plan: line; aimd links the PR to every package the agent has.
        links.set(out.agentId, { ...pkg, sessionId })
        // Linked, the packages leave the ready count: the band's line follows.
        const target = { agent_id: out.agentId }
        void postSpawns($, sessionId, target, pkg, also, provisional).then(() => kick($), () => kick($))
      } else {
        // A remote agent: its cloud session URL arrives on the Agent tool's result.
        remember(e.tool_use_id, { sessionId, description: e.description, pkg, also, unknown })
      }
      // The plan file lacks the package's row: the lead is told to add it.
      if (unknown !== undefined) await noteUnknown($, unknown, e.parentAgentId)
      return out
    }
    // Several packages and no way to tell: the loop that spawned it is asked (and, in `full`, the band offers it).
    if (found.isAmbiguous) {
      if (out.agentId !== undefined) {
        const entry = { key: out.agentId, sessionId, agentId: out.agentId, description: e.description, candidates: found.candidates }
        await offerLink($, entry, e.parentAgentId)
      } else {
        remember(e.tool_use_id, { sessionId, description: e.description, candidates: found.candidates, parentAgentId: e.parentAgentId })
      }
    }
    return out
  }).catch(($, e, next) => next(e))

  // The Agent tool's result: a remote agent's cloud session URL.
  on('tool.call', { tool: 'Agent' }, async ($, e, next) => {
    const ran = await next(e)
    if (pending.size === 0) return ran
    const waiting = pending.get(e.tool_use_id)
    pending.delete(e.tool_use_id)
    const result = ran.result as { status?: unknown; sessionUrl?: unknown } | undefined
    if (waiting === undefined || result?.status !== 'remote_launched' || typeof result.sessionUrl !== 'string') return ran
    const url = result.sessionUrl
    if (waiting.pkg !== undefined) {
      const provisional = waiting.unknown === undefined ? undefined : { title: waiting.description }
      const sent = postSpawns($, waiting.sessionId, { cloud_url: url }, waiting.pkg, waiting.also ?? [], provisional)
      void sent.then(() => kick($), () => kick($))
    } else if (waiting.candidates !== undefined) {
      const entry = { key: url, sessionId: waiting.sessionId, cloudUrl: url, description: waiting.description, candidates: waiting.candidates }
      await offerLink($, entry, waiting.parentAgentId)
    }
    return ran
  }).catch(($, e, next) => next(e))

  // --- the PR check: Bash gh pr create and the GitHub MCP tool --------------

  on('tool.call', { tool: 'Bash' }, async ($, e, next) => {
    // Hot path: a local regex first, then the in-memory link map.
    if (!mentionsPrCreate(e.command)) return next(e)
    const linked = linkedFor(e.agentId)
    // Refused only while aimd is up: with aimd down, as if the mod weren't installed.
    if (linked !== undefined && s.prCheck && (await reachable($, REACHABLE_MS))) {
      const deny = await bodyVerdict($, prBody(e.command), linked)
      if (deny !== undefined) return { deny }
    }
    const ran = await next(e)
    if (ran.deny === undefined && ran.isError !== true) {
      const stdout = (ran.result as { stdout?: unknown } | undefined)?.stdout
      const url = prUrlIn(`${ran.text ?? ''}\n${typeof stdout === 'string' ? stdout : ''}`)
      // A PR was created: linked, aimd links it and reads it at once; else
      // aimd only reads it. Either way the band's line follows.
      if (url !== undefined && linked !== undefined) void postPr($, linked, e.agentId, url).then(() => kick($), () => kick($))
      else if (url !== undefined) refreshSoon($, { ref: url, action: 'create' })
    }
    return ran
  }).catch(($, e, next) => next(e))

  on('tool.call', { tool: /^mcp__.*create_pull_request$/ }, async ($, e, next) => {
    const linked = linkedFor(e.agentId)
    if (linked !== undefined && s.prCheck && (await reachable($, REACHABLE_MS))) {
      const body = (e as { body?: unknown }).body
      if (!hasPlanLine(typeof body === 'string' ? body : '', linked)) return { deny: planLineDeny(linked) }
    }
    const ran = await next(e)
    if (ran.deny === undefined && ran.isError !== true) {
      const url = prUrlIn(ran.text ?? '') ?? prUrlIn(JSON.stringify(ran.result ?? null))
      if (url !== undefined && linked !== undefined) void postPr($, linked, e.agentId, url).then(() => kick($), () => kick($))
      else if (url !== undefined) refreshSoon($, { ref: url, action: 'create' })
    }
    return ran
  }).catch(($, e, next) => next(e))

  // --- PRs merged or closed: aimd reads them now -----------------------------
  // Never blocks or changes the call: it has run by the time aimd is told,
  // and only a call that worked is reported. GitHub stays the source of
  // truth: aimd reads the PR by number, and the polls are the backstop.

  on('tool.call', { tool: 'Bash' }, async ($, e, next) => {
    // Hot path: a local regex, then nothing.
    if (!mentionsPrEnd(e.command)) return next(e)
    const ran = await next(e)
    try {
      if (ran.deny === undefined && ran.isError !== true) {
        const ends = prEnds(e.command)
        const out = bashOutput(ran)
        // gh prints the PR it merged only on a terminal; when it does, that names it best.
        const printed = ends.length === 1 ? (prUrlIn(out) ?? prRefIn(out)) : undefined
        for (const end of ends) {
          const ref = end.ref !== '' ? end.ref : printed
          // No PR named: the current branch's, which only the main loop's cwd tells.
          if (ref === undefined && e.agentId !== undefined) continue
          refreshSoon($, { ref: ref ?? '', ...(end.repo !== '' ? { repo: end.repo } : {}), action: end.action })
        }
      }
    } catch {
      // The call has run; telling aimd is a nicety.
    }
    return ran
  }).catch(($, e, next) => next(e))

  on('tool.call', { tool: /^mcp__.*(?:merge_pull_request|update_pull_request)$/ }, async ($, e, next) => {
    const ran = await next(e)
    try {
      const a = e as { tool: string; owner?: unknown; repo?: unknown; pullNumber?: unknown; pull_number?: unknown; state?: unknown }
      const merge = /merge_pull_request$/.test(a.tool)
      const n = Number(a.pullNumber ?? a.pull_number)
      if (ran.deny === undefined && ran.isError !== true && (merge || a.state === 'closed') &&
        typeof a.owner === 'string' && typeof a.repo === 'string' && Number.isInteger(n) && n > 0) {
        refreshSoon($, { ref: `${a.owner}/${a.repo}#${n}`, action: merge ? 'merge' : 'close' })
      }
    } catch {
      // As above.
    }
    return ran
  }).catch(($, e, next) => next(e))

  // --- Write / Edit in the main loop: plan detection ---------------------------

  on('tool.call', { tool: ['Write', 'Edit'] }, async ($, e, next) => {
    const ran = await next(e)
    // Hot path: only the main loop's .md writes go to aimd, unawaited.
    if (e.agentId === undefined && ran.deny === undefined && ran.isError !== true && /\.md$/i.test(e.file_path)) {
      void detect($, e.file_path)
    }
    return ran
  }).catch(($, e, next) => next(e))

  // --- the band ---------------------------------------------------------------

  on('ui.render', { component: 'AbovePrompt' }, async ($, e, next) => {
    if (!s.band || e.props.hasSurvey) return next(e)
    const offer = await read($, offerAtom)
    // Offers kept from before a switch to `status` stay in $.state, undrawn.
    const unlinked = s.bandLinks ? await read($, unlinkedAtom) : []
    // Drawn from $.state alone: drawing never asks aimd (refreshBand does).
    const progress = await read($, progressAtom)
    if (offer === null && unlinked.length === 0 && progress === null) return next(e)
    const { Box, Text, Button } = $.ui.resolve(e)
    return (
      <Box flexDirection="column">
        {progress !== null && (
          // One press (a click, or Enter once the band has the focus) opens the plan's page.
          <Button key="progress" plain label={progressLine(progress)} onPress={() => void openPlan($, progress.slug).catch(() => undefined)}>
            <Text bold>{clean(progress.slug, MAX_SLUG)}</Text>
            {` · ${progress.done}/${progress.total} done · ${progress.ready} ready · `}
            <Text {...(progress.needsYou > 0 ? { color: 'warning' } : {})}>
              {needYouText(progress.needsYou)}
            </Text>
            {progress.toMerge > 0 ? ` · ${progress.toMerge} to merge` : ''}
            {progress.more > 0 ? <Text dimColor>{` · ${moreText(progress.more)}`}</Text> : ''}
          </Button>
        )}
        {offer !== null && (
          <Box key="offer" flexDirection="row">
            <Text>
              {clean(offer.file, MAX_FILE)} looks like a dispatch plan: {offer.packages} packages, {offer.lanes} lanes.{' '}
            </Text>
            <Button
              key="register"
              label="Register"
              variant="primary"
              onPress={() =>
                void registerPlan($, offer.path).then(message => toast($, message))
              }
            />
            {/* Ignore: plan or not, ai-manager isn't wanted for this file now. */}
            <Button key="ignore" label="Ignore" onPress={() => void dismissPlan($, offer)} />
          </Box>
        )}
        {unlinked.map(entry => {
          const manyPlans = new Set(entry.candidates.map(c => c.plan)).size > 1
          return (
            <Box key={`unlinked-${entry.key}`} flexDirection="row">
              <Text>Link {entry.description === '' ? 'the new agent' : entry.description} to: </Text>
              {entry.candidates.map(c => (
                <Button
                  key={`link-${entry.key}-${c.plan}-${c.id}`}
                  label={manyPlans ? `${clean(c.plan, MAX_SLUG)} · ${cleanId(c.id)}` : cleanId(c.id)}
                  onPress={() => void linkFromBand($, entry, c)}
                />
              ))}
              <Button key={`skip-${entry.key}`} label="Skip" onPress={() => void dropUnlinked($, entry.key)} />
            </Box>
          )
        })}
      </Box>
    )
  }).catch(($, e, next) => next(e))

  // --- commands ---------------------------------------------------------------

  on('command.run', { command: 'plan-register' }, async ($, e) => {
    const arg = e.args.trim().replace(/^(['"])(.*)\1$/, '$2')
    const dir = (await quietly($.session.cwd())) ?? ctxCwd
    // ~/… is the home folder, as in a shell; anything else not starting with / is relative to the session's folder.
    const expanded = arg !== '' ? expandHome(arg, await quietly($.env.get('HOME'))) : undefined
    if (arg !== '' && expanded === undefined) {
      return { text: `ai-manager can't tell where ${clean(arg, MAX_FIELD)} is: only ~/… is understood, as your home folder. Give the full path.` }
    }
    const path = expanded !== undefined ? absolute(expanded, dir) : lastDetected
    if (path === undefined) {
      return { text: 'No plan file given, and none detected in this session yet. Run /plan-register <path to the plan .md>.' }
    }
    return { text: await registerPlan($, path) }
  }).catch(($, e, next) => next(e))

  on('command.run', { command: 'aim' }, async ($, e) => {
    const words = e.args.trim().toLowerCase().split(/\s+/).filter(w => w !== '')
    if (words.length === 1 && (words[0] === 'watch' || words[0] === 'unwatch')) {
      return { text: await setWatch($, words[0] === 'watch') }
    }
    if (words.length > 0) {
      return {
        text: 'Usage: /aim opens the ai-manager dashboard; /aim watch monitors this session (Needs you, notifications); /aim unwatch stops.',
      }
    }
    if (!s.local) return { text: NOT_LOCAL }
    // The new app: this session's plan (the one the band shows), else its home.
    const slug = await sessionPlan($)
    const url = appUrl(s.url, slug)
    // The slug is a plan's own text: shown cleaned (text.ts). The URL holds it percent-encoded.
    const shown = slug === undefined ? '' : clean(slug, MAX_SLUG)
    if (await openApp($, slug)) {
      return { text: slug === undefined ? `Opened the ai-manager dashboard: ${url}` : `Opened plan ${shown} in the ai-manager dashboard: ${url}` }
    }
    return { text: slug === undefined ? `The ai-manager dashboard is at ${url}` : `Plan ${shown}'s page in the ai-manager dashboard is at ${url}` }
  }).catch(($, e, next) => next(e))

  // --- the plan_status tool -----------------------------------------------------

  // plan_link: the loop that spawned an agent ai-manager couldn't place links it.
  on('tool.call', { tool: LINK_TOOL_NAME }, async ($, e) => {
    if (!s.local) return { deny: NOT_LOCAL }
    return { result: await linkAgent($, e as { agent?: unknown; plan?: unknown; packages?: unknown }) }
  }).catch(($, e, next) => next(e))

  on('tool.call', { tool: TOOL_NAME }, async ($, e) => {
    if (!s.local) return { deny: NOT_LOCAL }
    const input = e as { plan?: unknown; since?: unknown }
    // Asked again at once if the last try failed: aimd may be back.
    const c = await context($, ctx === null || !ctxOk)
    if (!ctxOk) return { deny: `ai-manager's plan_status couldn't reach aimd at ${s.url}. Is aimd running?` }
    const wanted = typeof input.plan === 'string' && input.plan.trim() !== '' ? input.plan.trim() : undefined
    const slugs = wanted !== undefined ? [wanted] : (c?.plans ?? []).map(p => p.slug)
    if (slugs.length === 0) {
      const where = c?.repo?.name ?? c?.repo?.path ?? ctxCwd
      return {
        result: JSON.stringify(
          cleanData({
            repo: c?.repo ?? null,
            plans: [],
            note: `No plan is registered for ${where}. Register one with /plan-register <path to the plan .md>.`,
          }),
        ),
      }
    }
    const since = typeof input.since === 'string' && input.since.trim() !== '' ? `?since=${encodeURIComponent(input.since.trim())}` : ''
    const plans = await Promise.all(
      slugs.map(async slug => {
        const path = `/api/plans/${encodeURIComponent(slug)}`
        const [summary, board] = await Promise.all([
          call<Record<string, unknown>>($, s, 'GET', `${path}/summary${since}`),
          call<Board>($, s, 'GET', path),
        ])
        if (!summary.ok) return { slug, error: summary.error, status: summary.status }
        return { summary: summary.data, board: board.ok ? compactBoard(board.data) : { error: board.error } }
      }),
    )
    if (plans.every(p => 'status' in p && p.status === 0)) {
      return { deny: `ai-manager's plan_status couldn't reach aimd at ${s.url}. Is aimd running?` }
    }
    // Every string in it is a plan's, GitHub's or a session's: each stays on one line, and short.
    return { result: JSON.stringify(cleanData({ repo: c?.repo ?? null, plans })) }
  }).catch(($, e, next) => next(e))
}

/** The board without what the model doesn't need: nested link detail cut to the facts. */
function compactBoard(b: Board): Record<string, unknown> {
  const pkg = (p: BoardPackage) => ({
    id: p.id,
    title: p.title,
    lane: p.lane,
    owner: p.owner,
    yours: p.yours,
    needs: p.needs,
    needs_note: p.needs_note,
    needs_you: p.needs_you,
    lifecycle: p.lifecycle,
    blocked_by: p.blocked_by,
    cycle: p.cycle,
    badges: (p.badges ?? []).map(one => one.text),
    links: (p.links ?? []).map(l => ({
      kind: l.kind,
      ref: l.ref,
      method: l.method,
      confidence: l.confidence,
      via: l.via?.ref,
      pr: l.pr === undefined ? undefined : pick(l.pr, ['number', 'url', 'title', 'state', 'draft', 'checks_state', 'mergeable', 'merge_state_status', 'review_decision']),
      agent: l.agent === undefined ? undefined : pick(l.agent, ['name', 'kind', 'state', 'live', 'needs_you', 'reason']),
    })),
    suggested_branch: p.suggested_branch,
    plan_line: p.plan_line,
    // Not in the plan file yet, and the row the lead should add.
    provisional: p.provisional,
    suggested_row: p.suggested_row,
  })
  return {
    slug: b.slug,
    title: b.title,
    path: b.path,
    repos: b.repos,
    packages: (b.packages ?? []).map(pkg),
    unmatched: b.unmatched,
    warnings: b.warnings,
    alerts: b.alerts,
    derived_at: b.derived_at,
  }
}

function pick(o: Record<string, unknown>, keys: readonly string[]): Record<string, unknown> {
  const out: Record<string, unknown> = {}
  for (const k of keys) if (o[k] !== undefined) out[k] = o[k]
  return out
}
