// Pure helpers for the mod's reporting: the last-action label of a tool
// call, a reason, the first line of an answer, and what a needs-you waits
// on, each redacted. internal/events and internal/events/eventstest (Label,
// Reason, LastSaid, WaitFor, Summary, Redact) hold the same rules in Go,
// and both sides test them on the same examples. Nothing here touches `$`,
// so it is cheap on every tool call.

import { absolute, baseName } from './text'

// The caps of internal/events, in characters (code points).
export const MAX_SUMMARY = 300
export const MAX_KIND = 64
export const MAX_TOOL = 128
export const MAX_QUESTIONS = 4
export const MAX_OPTIONS = 6
export const MAX_OPTION = 100
export const MAX_PLAN = 1000
export const MAX_PLAN_LINES = 12

/** One question of an AskUserQuestion call, as a wait carries it. */
export type WaitQuestion = { header?: string; question: string; options?: string[]; multi_select?: boolean }

/** What a needs_you waits on (internal/events Wait). */
export type Wait =
  | { kind: 'permission'; tool: string; summary?: string }
  | { kind: 'question'; tool: string; questions: WaitQuestion[] }
  | { kind: 'plan'; tool: string; plan: string }

type Input = Readonly<Record<string, unknown>>

const str = (o: Input | undefined, k: string): string => {
  const v = o?.[k]
  return typeof v === 'string' ? v : ''
}

/** s cut to at most n characters (code points), as Go's events.Truncate. */
export function truncate(s: string, n: number): string {
  if (s.length <= n) return s
  const chars = Array.from(s)
  return chars.length <= n ? s : chars.slice(0, n).join('')
}

/** Every run of whitespace, line breaks included, folded into one space; the ends trimmed. */
export function oneLine(s: string): string {
  return s.split(/\s+/).filter(w => w !== '').join(' ')
}

/** s up to its first line break, trimmed. */
export function firstLine(s: string): string {
  const i = s.search(/[\r\n]/)
  return (i < 0 ? s : s.slice(0, i)).trim()
}

/** A plan's first MAX_PLAN_LINES lines and MAX_PLAN characters, blank lines at the ends and trailing spaces dropped. */
export function planLines(s: string): string {
  const lines = s
    .replace(/\r\n?/g, '\n')
    .replace(/^[\n \t]+|[\n \t]+$/g, '')
    .split('\n')
    .slice(0, MAX_PLAN_LINES)
    .map(l => l.replace(/[ \t]+$/, ''))
  return truncate(lines.join('\n'), MAX_PLAN).replace(/[\n \t]+$/, '')
}

/** What redact puts in a secret's place. */
export const REDACTED = '[redacted]'

// The secrets redact replaces, in order. internal/events/redact.go has the
// same patterns in the same order (its redactions): change both.
const REDACTIONS: readonly (readonly [RegExp, string])[] = [
  // An env-style assignment whose name says it holds a secret:
  // GITHUB_TOKEN=…, export FOO_KEY=…, ?api_key=…. The name starts a word
  // (so --author=x is a flag, not an assignment).
  [
    /(^|[\s;&|(?,{"'])([A-Za-z_][A-Za-z0-9_.-]*(?:key|token|secret|passw(?:or)?d|pwd|auth|credential|cookie)[A-Za-z0-9_.-]*)(\s*=\s*)("[^"]*"|'[^']*'|[^\s"',;&|)]+)/gi,
    `$1$2$3${REDACTED}`,
  ],
  // Authorization and Cookie header values, to the end of the quoted argument or the line.
  [/\b((?:proxy-)?authorization|(?:set-)?cookie)(\s*:\s*)([^'"\r\n]+)/gi, `$1$2${REDACTED}`],
  // Any other -H / --header whose name says it holds a secret.
  [
    /((?:^|\s)(?:-H|--header)(?:\s+|=)['"]?[A-Za-z0-9-]*(?:key|token|secret|auth|cookie|credential|passw(?:or)?d)[A-Za-z0-9-]*\s*:\s*)([^'"\r\n]+)/gi,
    `$1${REDACTED}`,
  ],
  // A --password / --token style flag's value.
  [
    /((?:^|\s)--?(?:password|passwd|token|secret|api-key|apikey|access-key|secret-key|private-key|auth-token|client-secret)(?:\s+|=))("[^"]*"|'[^']*'|[^\s"']+)/gi,
    `$1${REDACTED}`,
  ],
  // A Bearer or Basic credential on its own.
  [/\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{12,}/gi, `$1 ${REDACTED}`],
  // A user:password@ in a URL.
  [/(:\/\/)[^/\s:@]+:[^/\s@]+@/g, `$1${REDACTED}@`],
  // Well-known token shapes.
  [/\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|sk-[A-Za-z0-9_-]{20,}|xox[abposr]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16})\b/g, REDACTED],
]

/** A run of 32 or more base64 or hex characters after = or :. */
const LONG_RUN = /([=:]\s*)([A-Za-z0-9+_-]{32,}={0,2})/g

/**
 * s with the secrets it can spot replaced by "[redacted]" (events.Redact
 * in Go): a net for the obvious cases, not a guarantee. Env-style
 * assignments whose name holds KEY, TOKEN, SECRET, PASSWORD, PASSWD, PWD,
 * AUTH, CREDENTIAL or COOKIE; Authorization and Cookie headers and secret
 * -H headers; --password / --token style flags; Bearer and Basic
 * credentials; user:password@ in URLs; GitHub, OpenAI-style, Slack and
 * AWS token shapes; runs of 32+ base64 or hex characters (with a letter
 * and a digit) after = or :.
 */
export function redact(s: string): string {
  if (s === '') return s
  let out = s
  for (const [re, by] of REDACTIONS) out = out.replace(re, by)
  return out.replace(LONG_RUN, (m: string, sep: string, run: string) =>
    /[0-9]/.test(run) && /[a-z]/i.test(run) ? `${sep}${REDACTED}` : m,
  )
}

/**
 * s on one line, redacted and cut to n characters (events.RedactLine); cut
 * to 1000 first, so a huge value costs little.
 *
 * redact comes before the cut to n: cut first, and a secret straddling the
 * cut could lose the part its pattern needs (a ghp_ token left with 15 of
 * its characters matches nothing) and stay, half of it visible. Redacted
 * first, the cut can only land in ordinary text or inside "[redacted]".
 * The cut at 1000 can still split a secret; its half is at the end of
 * those 1000 characters, and shows only if redaction shrank them to under
 * n.
 */
export function redactLine(s: string, n: number): string {
  return truncate(oneLine(redact(oneLine(truncate(s, 1000)))), n)
}

/**
 * s up to its first line break, redacted and cut to n characters
 * (events.RedactFirstLine): a turn_complete's last_said. Redacted before
 * the cut, as redactLine is and for the same reason; its spacing is kept.
 */
export function redactFirstLine(s: string, n: number): string {
  return truncate(redact(truncate(firstLine(s), 1000)), n)
}

/** An event's reason: on one line, redacted, at most 300 characters (eventstest.Reason). */
export function reason(s: string): string {
  return redactLine(s, MAX_SUMMARY)
}

/** A turn_complete's last_said: the first line of the answer, redacted, at most 300 characters (eventstest.LastSaid). */
export function lastSaid(answer: string): string {
  return redactFirstLine(answer, MAX_SUMMARY)
}

/** p relative to cwd when p is inside it, else p's base name (eventstest.relPath). */
function relPath(p: string, cwd: string): string {
  if (cwd !== '') {
    const full = absolute(p, cwd)
    const dir = absolute(cwd, '/')
    if (full === dir) return '.'
    if (full.startsWith(`${dir === '/' ? '' : dir}/`)) return full.slice(dir === '/' ? 1 : dir.length + 1)
  }
  return baseName(p)
}

/** The host (with any port) of a URL, or '' when it has none. */
function hostOf(url: string): string {
  return /^[A-Za-z][A-Za-z0-9+.-]*:\/\/(?:[^/?#@]*@)?([^/?#]+)/.exec(url)?.[1] ?? ''
}

/**
 * The last-action label of a tool call (a `tool` event's label, and a
 * needs_you's reason): a short summary, never the tool's output or a
 * file's contents (eventstest.Label). It is redacted and then cut to 300
 * characters, in that order (redactLine says why): a handful of regexes
 * over at most 1000 characters, on every tool call.
 */
export function label(tool: string, input: Input, cwd: string): string {
  return redactLine(rawLabel(tool, input, cwd), MAX_SUMMARY)
}

/** label before the whole of it is redacted and cut. */
function rawLabel(tool: string, input: Input, cwd: string): string {
  switch (tool) {
    case 'Bash': {
      const d = oneLine(str(input, 'description'))
      if (d !== '') return `Bash: ${d}`
      const first = str(input, 'command').split(/\s+/).find(w => w !== '')
      if (first !== undefined) return `Bash: ${baseName(first) || first}`
      break
    }
    case 'Edit':
    case 'MultiEdit':
    case 'Write':
    case 'Read':
    case 'NotebookEdit': {
      const p = str(input, 'file_path') || str(input, 'notebook_path')
      if (p !== '') return `${tool} ${relPath(p, cwd)}`
      break
    }
    case 'Agent':
    case 'Task': {
      const d = oneLine(str(input, 'description'))
      return d !== '' ? `Agent: ${d}` : 'Agent'
    }
    case 'Grep':
    case 'Glob': {
      const p = str(input, 'pattern')
      // Redacted before its own cut to 60, as the whole label is before its cut.
      if (p !== '') return `${tool}: ${redactLine(p, 60)}`
      break
    }
    case 'WebFetch': {
      const h = hostOf(str(input, 'url'))
      if (h !== '') return `WebFetch ${h}`
      break
    }
    case 'SendMessage': {
      const to = str(input, 'to')
      if (to !== '') return `SendMessage to ${to}`
      break
    }
    case 'EnterWorktree': {
      const n = str(input, 'name')
      if (n !== '') return `EnterWorktree ${n}`
      break
    }
    case 'ExitWorktree': {
      const a = str(input, 'action')
      if (a !== '') return `ExitWorktree ${a}`
      break
    }
  }
  return tool
}

/**
 * A permission's one-line summary of what the call does (eventstest.Summary):
 * the Bash command, the file path, the URL..., at most 300 characters,
 * never its output, with the secrets redact spots replaced.
 */
export function summary(tool: string, input: Input): string {
  let s = ''
  switch (tool) {
    case 'Bash':
      s = str(input, 'command')
      break
    case 'Edit':
    case 'MultiEdit':
    case 'Write':
    case 'Read':
    case 'NotebookEdit':
      s = str(input, 'file_path') || str(input, 'notebook_path')
      break
    case 'WebFetch':
      s = str(input, 'url')
      break
    case 'WebSearch':
      s = str(input, 'query')
      break
    case 'Agent':
    case 'Task':
      s = str(input, 'description') || str(input, 'subagent_type')
      break
    case 'Grep':
    case 'Glob': {
      s = str(input, 'pattern')
      const p = str(input, 'path')
      if (p !== '' && s !== '') s += ` in ${p}`
      break
    }
    case 'SendMessage': {
      const to = str(input, 'to')
      if (to !== '') s = `to ${to}`
      break
    }
    case 'EnterWorktree':
      s = str(input, 'name')
      break
    case 'ExitWorktree':
      s = str(input, 'action')
      break
    case 'Skill':
      s = str(input, 'skill')
      break
    default: {
      const parts: string[] = []
      for (const k of Object.keys(input).sort()) {
        const v = input[k]
        if (typeof v === 'string') parts.push(`${k}: ${truncate(oneLine(v), 60)}`)
        else if (typeof v === 'boolean' || (typeof v === 'number' && Number.isFinite(v))) parts.push(`${k}: ${String(v)}`)
      }
      s = parts.join(', ')
    }
  }
  // Cut first, so a long heredoc costs little, then redact and cut again.
  return truncate(oneLine(redact(truncate(oneLine(s), 1000))), MAX_SUMMARY)
}

/**
 * What a needs_you about a call of `tool` waits on (eventstest.WaitFor), or
 * undefined when there is nothing to say: AskUserQuestion's questions,
 * ExitPlanMode's plan (its first lines), or a permission's summary.
 */
export function waitFor(tool: string, input: Input | undefined): Wait | undefined {
  if (tool === '') return undefined
  const args: Input = input !== null && typeof input === 'object' ? input : {}
  if (tool === 'AskUserQuestion') {
    const list = Array.isArray(args.questions) ? (args.questions as unknown[]) : []
    const questions: WaitQuestion[] = []
    for (const item of list) {
      if (questions.length === MAX_QUESTIONS) break
      const m = item !== null && typeof item === 'object' ? (item as Input) : {}
      const q: WaitQuestion = { question: redactLine(str(m, 'question'), MAX_SUMMARY) }
      const header = redactLine(str(m, 'header'), MAX_KIND)
      if (header !== '') q.header = header
      const options: string[] = []
      for (const o of Array.isArray(m.options) ? (m.options as unknown[]) : []) {
        const om = o !== null && typeof o === 'object' ? (o as Input) : {}
        const optLabel = redactLine(str(om, 'label'), MAX_OPTION)
        if (optLabel !== '' && options.length < MAX_OPTIONS) options.push(optLabel)
      }
      if (options.length > 0) q.options = options
      if (m.multiSelect === true) q.multi_select = true
      if (q.question !== '' || options.length > 0) questions.push(q)
    }
    return questions.length > 0 ? { kind: 'question', tool, questions } : undefined
  }
  if (tool === 'ExitPlanMode') {
    const plan = planLines(redact(planLines(str(args, 'plan'))))
    return plan !== '' ? { kind: 'plan', tool, plan } : undefined
  }
  const s = summary(tool, args)
  return s !== '' ? { kind: 'permission', tool: truncate(tool, MAX_TOOL), summary: s } : { kind: 'permission', tool: truncate(tool, MAX_TOOL) }
}
