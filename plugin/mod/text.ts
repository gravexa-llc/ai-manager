// Pure text helpers for the ai-manager mod: the `Package:` and `Plan:`
// lines, package IDs, the convention block, `gh pr create` and PR URLs.
// Nothing here touches `$`, so it is cheap to run on every Bash call.

/** A package as the mod links it: its plan, ID and convention data. */
export type Pkg = {
  plan: string
  id: string
  title: string
  suggestedBranch: string
  planLine: string
}

/** A `<slug> · <ID>` reference read from a line. */
export type Ref = { slug: string; id: string }

// The separators people (and agents) write between slug and ID, mapped to
// "·". The same list, in the same order, as internal/github/planline.go.
const SEPARATORS: readonly (readonly [string, string])[] = [
  ['&middot;', '·'], ['&#183;', '·'], ['&#xb7;', '·'], ['&#xB7;', '·'], ['&bull;', '·'],
  ['•', '·'], ['∙', '·'], ['⋅', '·'], ['・', '·'], ['･', '·'], ['‧', '·'], ['●', '·'],
  ['–', '·'], ['—', '·'], ['―', '·'],
  [' - ', ' · '], [' -- ', ' · '],
  ['|', '·'], ['/', '·'], ['\\', '·'], [':', '·'], [',', '·'], ['#', '·'], [';', '·'],
]

// One pass, left to right, first listed separator wins at each position:
// what Go's strings.Replacer does.
function normalizeSeparators(s: string): string {
  let out = ''
  let i = 0
  outer: while (i < s.length) {
    for (const [from, to] of SEPARATORS) {
      if (s.startsWith(from, i)) {
        out += to
        i += from.length
        continue outer
      }
    }
    out += s[i]
    i += 1
  }
  return out
}

const VALUE = /^([A-Za-z0-9][A-Za-z0-9._-]*?)(\s*·[\s·]*|\s+)([A-Za-z][A-Za-z0-9._-]*)(.*)$/
const PREFIXES: Record<'plan' | 'package', RegExp> = {
  plan: /^[*_]{0,3}plan[*_]{0,3}\s*:[*_]{0,3}\s*/i,
  package: /^[*_]{0,3}package[*_]{0,3}\s*:[*_]{0,3}\s*/i,
}

function stripDecoration(line: string): string {
  for (;;) {
    const before = line
    line = line.trim()
    if (line.startsWith('>')) line = line.slice(1)
    for (const marker of ['- ', '* ', '+ ']) {
      if (line.startsWith(marker)) line = line.slice(marker.length)
    }
    if (line.startsWith('<!--')) {
      line = line.slice(4)
      if (line.endsWith('-->')) line = line.slice(0, -3)
    }
    line = line.trim()
    if (line === before) return line
  }
}

function parseLine(raw: string, prefix: RegExp): Ref | undefined {
  const line = stripDecoration(raw)
  const head = prefix.exec(line)
  if (head === null) return undefined
  let rest = line.slice(head[0].length).replace(/^[*_` ]+|[*_` ]+$/g, '').trim()
  rest = normalizeSeparators(rest.replaceAll('`', '').replaceAll('*', ''))
  const m = VALUE.exec(rest)
  if (m === null) return undefined
  const slug = m[1] ?? ''
  const sep = m[2] ?? ''
  const id = (m[3] ?? '').replace(/[.\-_]+$/, '')
  const tail = m[4] ?? ''
  if (id === '') return undefined
  if (tail !== '' && !' \t·.!?)]>'.includes(tail[0] ?? '')) return undefined
  if (!sep.includes('·')) {
    // Space-separated only: stricter, to keep prose out.
    if (!/[0-9]/.test(id)) return undefined
    if (tail.replace(/^[ \t.!]+|[ \t.!]+$/g, '') !== '') return undefined
  }
  return { slug, id }
}

/**
 * The first `<kind>: <slug> · <ID>` line in text, read as aimd reads a PR
 * body's Plan: line (internal/github/planline.go): any case, emphasis,
 * list and quote markers, the common separators; fenced code skipped.
 */
export function findRefLine(text: string, kind: 'plan' | 'package'): Ref | undefined {
  let inFence = false
  for (const raw of text.split('\n')) {
    const line = raw.replace(/\r$/, '').trim()
    if (line.startsWith('```') || line.startsWith('~~~')) {
      inFence = !inFence
      continue
    }
    if (inFence) continue
    const ref = parseLine(line, PREFIXES[kind])
    if (ref !== undefined) return ref
  }
  return undefined
}

/** Whether a Plan: line in text names the package. */
export function hasPlanLine(text: string, pkg: Pkg): boolean {
  const ref = findRefLine(text, 'plan')
  return ref !== undefined && sameRef(ref, pkg)
}

export function sameRef(ref: Ref, pkg: Pick<Pkg, 'plan' | 'id'>): boolean {
  return ref.slug.toLowerCase() === pkg.plan.toLowerCase() && ref.id.toUpperCase() === pkg.id.toUpperCase()
}

const ID = /^[A-Z]{1,4}-?[0-9][A-Za-z0-9]*(?:-[A-Za-z0-9]+)*$/

/** Whether s has the shape of a package ID (internal/plans IsID). */
export function isID(s: string): boolean {
  return s.length <= 10 && ID.test(s)
}

/** The distinct ID-shaped tokens in text, in order. */
export function idsIn(text: string): string[] {
  const seen = new Set<string>()
  for (const word of text.split(/[^A-Za-z0-9-]+/)) {
    const token = word.replace(/^-+|-+$/g, '')
    if (isID(token)) seen.add(token)
  }
  return [...seen]
}

/** The marker that opens the convention block, so it is added once. */
export const MARK = '[ai-manager]'

/** The convention block appended to a brief, or added to a system prompt. */
export function conventionBlock(pkg: Pkg, who: 'agent' | 'session'): string {
  const title = pkg.title === '' ? '' : ` (${pkg.title})`
  return [
    `${MARK} This ${who} works on package ${pkg.plan} · ${pkg.id}${title} of a plan registered with ai-manager.`,
    `- Suggested branch: ${pkg.suggestedBranch}`,
    `- Put this line in the pull request body: ${pkg.planLine}`,
  ].join('\n')
}

/** The deny text for a PR body that lacks the package's Plan: line. */
export function planLineDeny(pkg: Pkg): string {
  return `Add '${pkg.planLine}' to the PR body and run it again.`
}

// --- packages the plan file doesn't have --------------------------------------

/** A package a brief's Package: line names that its registered plan's tables don't have. */
export type Unknown = { plan: string; id: string; file: string }

/** The note the lead gets when it spawns for an Unknown package. */
export function unknownNote(u: Unknown): string {
  return `${MARK} Package ${u.id} isn't in ${u.file}'s tables. Add its row so ai-manager tracks it; it shows as provisional until then.`
}

/** The deny text when requirePlanRow is on. */
export function unknownDeny(u: Unknown): string {
  return `Package ${u.id} isn't in ${u.file}'s tables. Add its row so ai-manager tracks it, then spawn the agent again (ai-manager's requirePlanRow setting is on).`
}

/** An ID as a provisional package spells it: as written when ID-shaped, else in upper case; undefined when neither is. */
export function provisionalId(id: string): string | undefined {
  if (isID(id)) return id
  const up = id.toUpperCase()
  return isID(up) ? up : undefined
}

/** A title from a spawn's description: without the ID it starts with (or ends with in parentheses), first letter up. */
export function titleFrom(id: string, description: string): string {
  let s = description.trim().replace(/\(\s*[A-Za-z0-9-]+\s*\)\s*$/, '').trim()
  const [word = '', ...rest] = s.split(/\s+/)
  if (word.replace(/[:.,·-]+$/, '').toUpperCase() === id.toUpperCase()) s = rest.join(' ').replace(/^[\s:·–—-]+/, '')
  return s.charAt(0).toUpperCase() + s.slice(1)
}

/**
 * A branch-name part from a title, as aimd makes it (internal/planapi
 * shortName): its first words in lower case, letters and digits only,
 * joined by hyphens, at most four words and 30 characters.
 */
export function shortName(title: string): string {
  const words = title.toLowerCase().split(/[^a-z0-9]+/).filter(w => w !== '')
  let out = ''
  for (const [i, w] of words.entries()) {
    if (i === 4 || (out !== '' && out.length + 1 + w.length > 30)) break
    out = out === '' ? w : `${out}-${w}`
  }
  return out.length > 30 ? out.slice(0, 30).replace(/-+$/, '') : out
}

// --- gh pr create --------------------------------------------------------

const GH_PR_CREATE = /(?:^|[\s;&|()`/])gh\s+pr\s+create(?![\w-])/

/** The cheap check on every Bash call: does the command run `gh pr create`? */
export function mentionsPrCreate(command: string): boolean {
  return GH_PR_CREATE.test(command)
}

/** What a `gh pr create` command says about its body. */
export type PrBody =
  | { kind: 'none' } // no gh pr create in it
  | { kind: 'allow' } // --web; --fill or --template without --body; no body given
  | { kind: 'text'; text: string } // inline body, heredoc, or -F - (stdin)
  | { kind: 'file'; path: string } // -F <path>

const HEREDOC = /(?<!<)<<(?!<)-?[ \t]*(['"]?)([A-Za-z_][A-Za-z0-9_]*)\1/g

// Takes heredoc bodies out of a command: the shell text left, and the bodies.
function splitHeredocs(command: string): { shell: string; bodies: string[] } {
  const shell: string[] = []
  const bodies: string[] = []
  const pending: string[] = []
  let body: string[] = []
  for (const line of command.split('\n')) {
    if (pending.length > 0) {
      if (line.trim() === pending[0]) {
        pending.shift()
        bodies.push(body.join('\n'))
        body = []
        shell.push('')
      } else {
        body.push(line)
      }
      continue
    }
    shell.push(line)
    for (const m of line.matchAll(HEREDOC)) pending.push(m[2] ?? '')
  }
  if (body.length > 0) bodies.push(body.join('\n'))
  return { shell: shell.join('\n'), bodies }
}

// The index of the `)` that closes the `(` at open, skipping quotes.
function closingParen(s: string, open: number): number {
  let depth = 0
  for (let i = open; i < s.length; i++) {
    const c = s[i]
    if (c === "'") {
      const j = s.indexOf("'", i + 1)
      i = j < 0 ? s.length : j
    } else if (c === '"') {
      i += 1
      while (i < s.length && s[i] !== '"') i += s[i] === '\\' ? 2 : 1
    } else if (c === '(') {
      depth += 1
    } else if (c === ')') {
      depth -= 1
      if (depth === 0) return i
    }
  }
  return s.length - 1
}

const OPERATORS = ['&&', '||', ';;', '|&', ';', '|', '&', '\n']

// Splits shell text into words (quotes removed) and operator tokens. Good
// enough for finding gh's flags; not a full shell parser.
function shellWords(s: string): string[] {
  const out: string[] = []
  let cur = ''
  let has = false
  const flush = () => {
    if (has) out.push(cur)
    cur = ''
    has = false
  }
  let i = 0
  outer: while (i < s.length) {
    const c = s[i] ?? ''
    if (c === ' ' || c === '\t') {
      flush()
      i += 1
      continue
    }
    for (const op of OPERATORS) {
      if (s.startsWith(op, i)) {
        flush()
        out.push(op)
        i += op.length
        continue outer
      }
    }
    if (c === '(' || c === ')') {
      flush()
      out.push(c)
      i += 1
      continue
    }
    has = true
    if (c === "'") {
      const j = s.indexOf("'", i + 1)
      const end = j < 0 ? s.length : j
      cur += s.slice(i + 1, end)
      i = end + 1
    } else if (c === '$' && s[i + 1] === "'") {
      // ANSI-C quoting: $'a\nb'
      let j = i + 2
      while (j < s.length && s[j] !== "'") {
        if (s[j] === '\\' && j + 1 < s.length) {
          const e = s[j + 1]
          cur += e === 'n' ? '\n' : e === 't' ? '\t' : e === "'" ? "'" : e === '\\' ? '\\' : `\\${e}`
          j += 2
        } else {
          cur += s[j]
          j += 1
        }
      }
      i = j + 1
    } else if (c === '"') {
      let j = i + 1
      while (j < s.length && s[j] !== '"') {
        const d = s[j]
        if (d === '\\' && j + 1 < s.length && '$`"\\\n'.includes(s[j + 1] ?? '')) {
          if (s[j + 1] !== '\n') cur += s[j + 1]
          j += 2
        } else if (d === '$' && s[j + 1] === '(') {
          const k = closingParen(s, j + 1)
          cur += s.slice(j, k + 1)
          j = k + 1
        } else {
          cur += d
          j += 1
        }
      }
      i = j + 1
    } else if (c === '\\' && i + 1 < s.length) {
      if (s[i + 1] !== '\n') cur += s[i + 1]
      i += 2
    } else if (c === '$' && s[i + 1] === '(') {
      const k = closingParen(s, i + 1)
      cur += s.slice(i, k + 1)
      i = k + 1
    } else {
      cur += c
      i += 1
    }
  }
  flush()
  return out
}

// gh pr create flags that take a value (skipped, so a title of "-b" is not
// read as a flag).
const VALUE_FLAGS = new Set([
  '--title', '-t', '--base', '-B', '--head', '-H', '--repo', '-R', '--label', '-l',
  '--assignee', '-a', '--reviewer', '-r', '--milestone', '-m', '--project', '-p', '--recover',
])

/**
 * Reads a Bash command's `gh pr create`: where its body comes from. A
 * heredoc body (`--body "$(cat <<'EOF' ... EOF)"`, `-F - <<EOF`) is read
 * from the command text itself.
 */
export function prBody(command: string): PrBody {
  if (!mentionsPrCreate(command)) return { kind: 'none' }
  const { shell, bodies } = splitHeredocs(command)
  const words = shellWords(shell)
  let start = -1
  for (let i = 0; i + 2 < words.length; i++) {
    const w = words[i] ?? ''
    if ((w === 'gh' || w.endsWith('/gh')) && words[i + 1] === 'pr' && words[i + 2] === 'create') {
      start = i + 3
      break
    }
  }
  if (start < 0) return { kind: 'none' }
  let body: string | undefined
  let file: string | undefined
  for (let i = start; i < words.length; i++) {
    const a = words[i] ?? ''
    if (OPERATORS.includes(a) || a === ')') break
    // The body is written in the browser.
    if (a === '--web' || a === '-w') return { kind: 'allow' }
    // --fill and --template give a body gh writes, unless --body is given
    // too: those are allowed below, as no body at all is.
    if (a === '--template' || a === '-T') i += 1
    else if (a === '--body-file' || a === '-F') {
      file = words[i + 1] ?? ''
      i += 1
    } else if (a.startsWith('--body-file=')) file = a.slice('--body-file='.length)
    else if (a.startsWith('-F') && !a.startsWith('--')) file = a.slice(2)
    else if (a === '--body' || a === '-b') {
      body = words[i + 1] ?? ''
      i += 1
    } else if (a.startsWith('--body=')) body = a.slice('--body='.length)
    else if (a.startsWith('-b') && !a.startsWith('--')) body = a.slice(2)
    else if (VALUE_FLAGS.has(a)) i += 1
  }
  if (file !== undefined) {
    if (file === '-' || file === '/dev/stdin') return { kind: 'text', text: command }
    return { kind: 'file', path: file }
  }
  if (body !== undefined) return { kind: 'text', text: [body, ...bodies].join('\n') }
  return { kind: 'allow' }
}

const PR_URL = /https:\/\/github\.com\/[A-Za-z0-9][A-Za-z0-9-]*\/[A-Za-z0-9._-]+\/pull\/[0-9]+/

/** The first GitHub pull request URL in text. */
export function prUrlIn(text: string): string | undefined {
  return PR_URL.exec(text)?.[0]
}

/** A path made absolute against a directory, `.` and `..` resolved. */
export function absolute(path: string, dir: string): string {
  const joined = path.startsWith('/') ? path : `${dir.replace(/\/+$/, '')}/${path}`
  const parts: string[] = []
  for (const part of joined.split('/')) {
    if (part === '' || part === '.') continue
    if (part === '..') parts.pop()
    else parts.push(part)
  }
  return `/${parts.join('/')}`
}

/** The last part of a path. */
export function baseName(path: string): string {
  return path.split('/').filter(p => p !== '').pop() ?? path
}
