// The pure rules of the mod's reporting (wait.ts): labels, reasons, the
// first line of an answer, permission summaries, redaction and wait
// details. The examples are the ones internal/events/eventstest checks in
// Go (TestLabel, TestLabelIsRedacted, TestReasonAndLastSaidAreRedacted,
// TestWaitFor), so the mod and aimd's mapping agree.

import { expect, test } from 'claude-code/testing'

import { label, lastSaid, planLines, reason, redact, truncate, waitFor } from './wait'

test('labels: a short summary of each call, never its output', () => {
  const cwd = '/r'
  const cases: [string, Record<string, unknown>, string][] = [
    ['Bash', { command: 'make check', description: 'Run the\nchecks' }, 'Bash: Run the checks'],
    ['Bash', { command: '/usr/bin/env FOO=1 secret-thing' }, 'Bash: env'],
    ['Edit', { file_path: '/r/internal/x.go' }, 'Edit internal/x.go'],
    ['Write', { file_path: '/elsewhere/y.md', content: '[file content]' }, 'Write y.md'],
    ['Read', { file_path: '/r/../etc/z' }, 'Read z'],
    ['Agent', { description: 'wt probe', prompt: '[prompt]' }, 'Agent: wt probe'],
    ['Grep', { pattern: 'a'.repeat(100) }, `Grep: ${'a'.repeat(60)}`],
    ['WebFetch', { url: 'https://example.com/a?b=c' }, 'WebFetch example.com'],
    ['SendMessage', { to: 'a1', message: '[message]' }, 'SendMessage to a1'],
    ['EnterWorktree', { name: 'w' }, 'EnterWorktree w'],
    ['ExitWorktree', { action: 'keep' }, 'ExitWorktree keep'],
    ['ToolSearch', { query: 'select:X' }, 'ToolSearch'],
  ]
  for (const [tool, input, want] of cases) expect(label(tool, input, cwd), `${tool} ${JSON.stringify(input)}`).toBe(want)
})

test('a permission waits on the tool and one line of what the call does, secrets redacted', () => {
  const long = 'x'.repeat(400)
  const cases: [string, Record<string, unknown>, unknown][] = [
    ['Bash', { command: 'mkdir perm-main', description: 'Create directory perm-main' }, { kind: 'permission', tool: 'Bash', summary: 'mkdir perm-main' }],
    ['Bash', { command: 'cd /tmp &&\n  npm test' }, { kind: 'permission', tool: 'Bash', summary: 'cd /tmp && npm test' }],
    ['Bash', { command: `echo ${long}` }, { kind: 'permission', tool: 'Bash', summary: `echo ${long}`.slice(0, 300) }],
    [
      'Bash',
      { command: "curl -H 'Authorization: Bearer abcdefghijklmnop1234' https://user:pw@example.com/x?api_key=s3cr3t" },
      { kind: 'permission', tool: 'Bash', summary: "curl -H 'Authorization: [redacted]' https://[redacted]@example.com/x?api_key=[redacted]" },
    ],
    [
      'Bash',
      { command: 'GITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz0123 gh pr list --password hunter2' },
      { kind: 'permission', tool: 'Bash', summary: 'GITHUB_TOKEN=[redacted] gh pr list --password [redacted]' },
    ],
    ['Write', { file_path: '/tmp/d2probe/plan.txt', content: 'ok\n' }, { kind: 'permission', tool: 'Write', summary: '/tmp/d2probe/plan.txt' }],
    ['WebFetch', { url: 'https://example.com/a', prompt: 'summarise' }, { kind: 'permission', tool: 'WebFetch', summary: 'https://example.com/a' }],
    ['Grep', { pattern: 'TODO', path: 'src' }, { kind: 'permission', tool: 'Grep', summary: 'TODO in src' }],
    [
      'mcp__github__create_issue',
      { title: 'Bug', body: 'line one\nline two', labels: ['x'], draft: true },
      { kind: 'permission', tool: 'mcp__github__create_issue', summary: 'body: line one line two, draft: true, title: Bug' },
    ],
  ]
  for (const [tool, input, want] of cases) expect(waitFor(tool, input), tool).toEqual(want)
})

test('a question waits on its questions and option labels, capped', () => {
  const asked = {
    questions: [
      {
        question: 'Do you prefer red or blue?',
        header: 'Color',
        multiSelect: false,
        options: [{ label: 'Red', description: 'd' }, { label: 'Blue' }],
      },
      { question: 'Which ones?', multiSelect: true, options: [{ label: 'A' }] },
    ],
  }
  expect(waitFor('AskUserQuestion', asked)).toEqual({
    kind: 'question',
    tool: 'AskUserQuestion',
    questions: [
      { header: 'Color', question: 'Do you prefer red or blue?', options: ['Red', 'Blue'] },
      { question: 'Which ones?', options: ['A'], multi_select: true },
    ],
  })
  expect(waitFor('AskUserQuestion', { questions: 'not a list' })).toBeUndefined()
  const many = {
    questions: Array.from({ length: 6 }, (_, i) => ({
      question: `Q${i}`,
      options: Array.from({ length: 8 }, (_, j) => ({ label: `${'o'.repeat(120)}${j}` })),
    })),
  }
  const w = waitFor('AskUserQuestion', many)
  expect(w?.kind).toBe('question')
  if (w?.kind !== 'question') return
  expect(w.questions.length).toBe(4)
  expect(w.questions[0]?.options?.length).toBe(6)
  expect(w.questions[0]?.options?.[0]?.length).toBe(100)
})

test("a plan approval waits on the plan's first lines", () => {
  const plan = '\n# Plan\n\n1. Add the field   \n2. Test it\n3. a\n4. b\n5. c\n6. d\n7. e\n8. f\n9. g\n10. h\n11. i\n12. j\n'
  expect(waitFor('ExitPlanMode', { plan, planFilePath: '/x.md' })).toEqual({
    kind: 'plan',
    tool: 'ExitPlanMode',
    plan: '# Plan\n\n1. Add the field\n2. Test it\n3. a\n4. b\n5. c\n6. d\n7. e\n8. f\n9. g\n10. h',
  })
  expect(waitFor('ExitPlanMode', {})).toBeUndefined()
  expect(planLines('x'.repeat(5000)).length).toBe(1000)
  expect(planLines('step 1\ntoken=abc\n')).toBe('step 1\ntoken=abc')
  expect(waitFor('ExitPlanMode', { plan: 'step 1\nexport API_KEY=abc123\n' })).toEqual({ kind: 'plan', tool: 'ExitPlanMode', plan: 'step 1\nexport API_KEY=[redacted]' })
})

test('truncate counts characters, not UTF-16 units; redact leaves ordinary text alone', () => {
  expect(truncate('日本語テキスト', 3)).toBe('日本語')
  expect(truncate('😀😀😀', 2)).toBe('😀😀')
  expect(redact('gh auth status && go test ./...')).toBe('gh auth status && go test ./...')
})

// The examples internal/events/redact_test.go checks too (redactCases
// there): change both together.
const REDACT_CASES: readonly (readonly [string, string])[] = [
  // Env-style assignments whose name says it holds a secret.
  ['GITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz0123 gh pr list', 'GITHUB_TOKEN=[redacted] gh pr list'],
  ['export FOO_KEY=abc123; npm test', 'export FOO_KEY=[redacted]; npm test'],
  ["DB_PASSWORD='hunter 2' ./run", 'DB_PASSWORD=[redacted] ./run'],
  ['aws_secret_access_key="xyz" make', 'aws_secret_access_key=[redacted] make'],
  ['PGPASSWD=x OLDPWD=/tmp env', 'PGPASSWD=[redacted] OLDPWD=[redacted] env'],
  ['MY_AUTH=a SESSION_COOKIE=b AZURE_CREDENTIAL=c', 'MY_AUTH=[redacted] SESSION_COOKIE=[redacted] AZURE_CREDENTIAL=[redacted]'],
  ['curl https://x.test/api?api_key=s3cr3t&page=2', 'curl https://x.test/api?api_key=[redacted]&page=2'],
  // Header values, and -H arguments carrying a secret.
  ["curl -H 'Authorization: Bearer abc.def' https://x.test", "curl -H 'Authorization: [redacted]' https://x.test"],
  ['curl -H "Cookie: sid=123; theme=dark" x.test', 'curl -H "Cookie: [redacted]" x.test'],
  ["curl --header 'X-Api-Key: abc' x.test", "curl --header 'X-Api-Key: [redacted]' x.test"],
  ["curl -H 'X-Auth-Token: zzz' x.test", "curl -H 'X-Auth-Token: [redacted]' x.test"],
  // A credential in a URL.
  ['git clone https://user:pa55@github.com/o/r.git', 'git clone https://[redacted]@github.com/o/r.git'],
  // Well-known token shapes, wherever they are.
  ['echo ghp_abcdefghijklmnopqrstuvwxyz0123', 'echo [redacted]'],
  ['use gho_abcdefghijklmnopqrstuvwx and ghs_abcdefghijklmnopqrstuvwx', 'use [redacted] and [redacted]'],
  ['token github_pat_11ABCDEFG0123456789_abcdefghij', 'token [redacted]'],
  ['OPENAI sk-abcdefghijklmnopqrstuvwx1234', 'OPENAI [redacted]'],
  ['slack xoxb-123456789012-abcdef', 'slack [redacted]'],
  ['aws AKIAABCDEFGHIJKLMNOP', 'aws [redacted]'],
  // Long base64 or hex runs after = or :.
  ['--sig=0123456789abcdef0123456789abcdef', '--sig=[redacted]'],
  ['value: QUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVo0NTY3ODk=', 'value: [redacted]'],
  // --password / --token style flags.
  ['mysql --password hunter2 db', 'mysql --password [redacted] db'],
  ['cli --token=abc --api-key xyz', 'cli --token=[redacted] --api-key [redacted]'],

  // Near misses: left alone.
  ['git log --author=sam', 'git log --author=sam'],
  ['author: Jane Doe', 'author: Jane Doe'],
  ['echo $GITHUB_TOKEN | wc -c', 'echo $GITHUB_TOKEN | wc -c'],
  ['gh auth token --user example-user', 'gh auth token --user example-user'],
  ['cat --token-file ~/.ai-manager/token', 'cat --token-file ~/.ai-manager/token'],
  ['pip install sk-learn ghp_short', 'pip install sk-learn ghp_short'],
  ['PATH=/usr/local/bin:/usr/bin make check', 'PATH=/usr/local/bin:/usr/bin make check'],
  ['ls /tmp:/Users/me/Projects/ai-manager/internal/live/state', 'ls /tmp:/Users/me/Projects/ai-manager/internal/live/state'],
  ['go test -run=TestReplayInteractiveDialogsWaitingOnEverything', 'go test -run=TestReplayInteractiveDialogsWaitingOnEverything'],
  ['The Authorization header is missing', 'The Authorization header is missing'],
  ['Reset the password for the test user', 'Reset the password for the test user'],
  ['mkdir perm-main && echo done', 'mkdir perm-main && echo done'],
]

test('redact: each kind of secret goes, near misses stay, and a second pass changes nothing', () => {
  for (const [input, want] of REDACT_CASES) {
    const got = redact(input)
    expect(got, input).toBe(want)
    expect(redact(got), `again: ${got}`).toBe(got)
  }
})

test('a question, its options and a plan are redacted too; the summary keeps its 300-character cap', () => {
  const asked = {
    questions: [{ header: 'Key', question: 'Use API_KEY=abc123 or the vault?', options: [{ label: "curl -H 'Authorization: Bearer x'" }, { label: 'Vault' }] }],
  }
  expect(waitFor('AskUserQuestion', asked)).toEqual({
    kind: 'question',
    tool: 'AskUserQuestion',
    questions: [{ header: 'Key', question: 'Use API_KEY=[redacted] or the vault?', options: ["curl -H 'Authorization: [redacted]'", 'Vault'] }],
  })
  const long = waitFor('Bash', { command: `GITHUB_TOKEN=ghp_abcdefghijklmnopqrstuvwxyz0123 ${'x'.repeat(500)}` })
  expect(long?.kind === 'permission' ? long.summary?.length : 0).toBe(300)
  expect(long?.kind === 'permission' ? long.summary?.startsWith('GITHUB_TOKEN=[redacted] ') : false).toBe(true)
})

// A made-up token, as the examples below use it.
const FAKE_TOKEN = 'ghp_0123456789abcdefABCDEF0123456789abcd'

// The examples internal/events/eventstest checks too (TestLabelIsRedacted,
// TestReasonAndLastSaidAreRedacted): change both together.
test('a label is redacted before it is cut; ordinary text and paths stay', () => {
  const cases: [string, Record<string, unknown>, string][] = [
    ['Bash', { command: 'x', description: `Export GITHUB_TOKEN=${FAKE_TOKEN} and push` }, 'Bash: Export GITHUB_TOKEN=[redacted] and push'],
    ['Bash', { command: 'x', description: 'Call the API with Bearer abcdefghijklmnop1234' }, 'Bash: Call the API with Bearer [redacted]'],
    ['Agent', { description: 'Clone https://user:pw@example.com/acme/app.git' }, 'Agent: Clone https://[redacted]@example.com/acme/app.git'],
    ['Grep', { pattern: FAKE_TOKEN }, 'Grep: [redacted]'],
    ['WebFetch', { url: 'https://user:pw@example.com/a' }, 'WebFetch example.com'],
    // A secret across a cut is redacted first, never left in half.
    ['Grep', { pattern: `${'x'.repeat(50)} ${FAKE_TOKEN}` }, `Grep: ${'x'.repeat(50)} [redacted`],
    ['Bash', { command: 'x', description: `${'y'.repeat(280)} GITHUB_TOKEN=${FAKE_TOKEN}` }, `Bash: ${'y'.repeat(280)} GITHUB_TOKEN=`],
    // Ordinary text and paths pass unchanged.
    ['Bash', { command: 'x', description: 'Run the tests in ~/Projects/app' }, 'Bash: Run the tests in ~/Projects/app'],
    ['Edit', { file_path: '/r/internal/auth/token.go' }, 'Edit internal/auth/token.go'],
    ['Read', { file_path: '/r/config/secrets.env' }, 'Read config/secrets.env'],
    ['Grep', { pattern: 'password\\s*=' }, 'Grep: password\\s*='],
    ['SendMessage', { to: 'token-auditor' }, 'SendMessage to token-auditor'],
  ]
  for (const [tool, input, want] of cases) {
    const got = label(tool, input, '/r')
    expect(got, `${tool} ${JSON.stringify(input)}`).toBe(want)
    expect(got.includes('ghp_'), `part of the token: ${got}`).toBe(false)
  }
})

test('a reason and the first line of an answer are redacted before they are cut', () => {
  const reasons: [string, string][] = [
    ['answer', 'answer'],
    ['rate_limit', 'rate_limit'],
    ['clear', 'clear'],
    ['permission_prompt', 'permission_prompt'],
    [`request failed:\n GITHUB_TOKEN=${FAKE_TOKEN} rejected`, 'request failed: GITHUB_TOKEN=[redacted] rejected'],
    ['401 from https://user:pw@example.com/ with Bearer abcdefghijklmnop1234', '401 from https://[redacted]@example.com/ with Bearer [redacted]'],
  ]
  for (const [input, want] of reasons) expect(reason(input), input).toBe(want)
  const said: [string, string][] = [
    ['Done: the tests pass.\nHere is more.', 'Done: the tests pass.'],
    [`I set GITHUB_TOKEN=${FAKE_TOKEN} in ~/Projects/app/.env\nNext, the deploy.`, 'I set GITHUB_TOKEN=[redacted] in ~/Projects/app/.env'],
    ['The header was Bearer abcdefghijklmnop1234, now revoked.', 'The header was Bearer [redacted], now revoked.'],
    ['Pushed to https://user:pw@example.com/acme/app.git', 'Pushed to https://[redacted]@example.com/acme/app.git'],
    // A secret across the cut is redacted first, never left in half.
    [`${'z'.repeat(290)} ${FAKE_TOKEN}\nmore`, `${'z'.repeat(290)} [redacted`],
    // Ordinary text and paths pass unchanged, spacing included.
    ['Edited internal/auth/token.go and  ~/Projects/app/README.md', 'Edited internal/auth/token.go and  ~/Projects/app/README.md'],
    ['The password check in acme/app now passes: 12 tests.', 'The password check in acme/app now passes: 12 tests.'],
  ]
  for (const [input, want] of said) {
    const got = lastSaid(input)
    expect(got, input).toBe(want)
    expect(got.includes('ghp_'), `part of the token: ${got}`).toBe(false)
  }
})
