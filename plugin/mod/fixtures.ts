// Test fixtures for the mod's tests (not imported by register.tsx). Event
// shapes are copied from testdata/events/ (Claude Code 2.1.318), with
// the scrubbed "[prompt]" placeholders replaced by briefs to match on.

/** The lead's session and the repo it runs in. */
export const SESSION_ID = '80896248-da48-41df-966f-269da7b80083'
export const REPO = '/tmp/probe'
export const WORKTREE = '/tmp/probe/.claude/worktrees/agent-a5bd483a78c5f4783'

/**
 * agent.spawn's input for a default (background) spawn from the main loop:
 * testdata/events/subagents-default-background.jsonl, seq 23.
 */
export const SPAWN = {
  tool_use_id: 'toolu_01PJYoxQKxPnrk5B5737n7iM',
  prompt: '[prompt]',
  description: 'fg probe',
  subagentType: 'general-purpose',
  provider: { plugin: 'engine', tier: 'core' },
  parentModel: 'claude-haiku-5-5',
  permissionMode: 'default',
  background: true,
  fork: false,
} as const

/** Its result: seq 27. */
export const SPAWNED = { model: 'claude-haiku-5-5', agentId: 'a61cc06fc9f51079d' }

/**
 * A SendMessage wake of a named agent:
 * testdata/events/named-agent-sendmessage-resume.jsonl, seq 71.
 */
export const RESUME = {
  tool_use_id: 'toolu_01MVZMXKM9G6U46vJGd3kzkp',
  prompt: '[prompt]',
  description: 'teammate probe',
  subagentType: 'general-purpose',
  provider: { plugin: 'engine', tier: 'core' },
  parentModel: 'claude-haiku-5-5',
  permissionMode: 'default',
  background: true,
  fork: false,
  resume: { agentId: 'ab38c36cb55a33059' },
  name: 'scout',
} as const

/** A Bash call's result as core answers it: seq 74 of the same file. */
export function bashResult(stdout: string) {
  return {
    result: { stdout, stderr: '', interrupted: false, isImage: false, noOutputExpected: false },
    text: stdout,
  }
}

/** prompt.compose's input under -p: print-session-minimal.jsonl, seq 10. */
export const COMPOSE = {
  model: 'claude-haiku-5-5',
  promptModel: 'claude-haiku-5-5',
  surfaces: [],
  tools: ['Agent', 'Bash', 'Edit', 'Write'],
  outputStyle: null,
  traits: ['lean', 'print', 'skills'],
} as const

/** The sections core composes (seq 11), texts shortened. */
export const SECTIONS = [
  { id: 'lean_body', scope: 'shared', text: '[body]' },
  { id: 'env_info_simple', scope: 'session', text: '[env]' },
] as const

// --- aimd answers (docs/api.md) ---------------------------------------------

function pkg(plan: string, id: string, title: string, lifecycle: string, short: string) {
  return {
    id,
    title,
    lifecycle,
    suggested_branch: `${plan}/${id.toLowerCase()}-${short}`,
    plan_line: `Plan: ${plan} · ${id}`,
  }
}

/** GET /api/context for the lead's repo: two plans, A1 in both. */
export const CONTEXT = {
  cwd: REPO,
  repo: { path: REPO, name: 'example-user/probe' },
  worktree: { path: REPO, branch: 'main', main: true },
  plans: [
    {
      slug: 'aim',
      title: 'ai-manager — build plan',
      band: 'aim · 2/6 done · 2 ready · 0 need you',
      packages: [
        pkg('aim', 'C3', 'Plan API', 'done', 'plan-api'),
        pkg('aim', 'C4', 'Mod v1: acting', 'ready', 'mod-v1-acting'),
        pkg('aim', 'C5', 'Skills', 'done', 'skills'),
        pkg('aim', 'D2', 'Mod v2: reporting', 'blocked', 'mod-v2-reporting'),
        pkg('aim', 'A1', 'Worktree scanner', 'ready', 'worktree-scanner'),
      ],
    },
    {
      slug: 'sharing',
      title: 'Pivot share links',
      band: 'sharing · 0/3 done · 3 ready · 0 need you',
      packages: [
        pkg('sharing', 'V1', 'Email reports', 'ready', 'email-reports'),
        pkg('sharing', 'V2', 'Report QA', 'ready', 'report-qa'),
        pkg('sharing', 'A1', 'Share tokens', 'ready', 'share-tokens'),
      ],
    },
  ],
  package: null,
}

/** The same, for a session in a worktree whose branch is linked to aim · C4. */
export const LINKED_CONTEXT = {
  ...CONTEXT,
  cwd: WORKTREE,
  worktree: { path: WORKTREE, branch: 'aim/c4-mod-v1-acting' },
  package: {
    ...pkg('aim', 'C4', 'Mod v1: acting', 'working', 'mod-v1-acting'),
    plan: 'aim',
    link: { kind: 'branch', ref: '/tmp/probe:aim/c4-mod-v1-acting', method: 'branch', confidence: 'high', inferred: true },
  },
}

export const DETECTED = {
  path: `${REPO}/docs/sharing-plan.md`,
  is_plan: true,
  title: 'Pivot share links',
  packages: 14,
  lanes: 3,
  slug: 'sharing',
  registered: false,
  dismissed: false,
  offer: true,
}

export const SUMMARY = {
  slug: 'aim',
  title: 'ai-manager — build plan',
  band: 'aim · 2/6 done · 2 ready · 0 need you',
  total: 6,
  done: 2,
  counts: { blocked: 1, ready: 2, working: 0, in_review: 0, done: 2, dropped: 0 },
  ready: [{ id: 'C4', title: 'Mod v1: acting', lifecycle: 'ready' }],
  needs_you: 0,
  needs_you_packages: [],
  unlocked: [],
  packages: [],
  warnings: 0,
  alerts: [],
}

export const BOARD = {
  slug: 'aim',
  title: 'ai-manager — build plan',
  path: `${REPO}/ROADMAP.md`,
  repos: ['example-user/probe'],
  band: SUMMARY.band,
  packages: [
    {
      ...pkg('aim', 'C3', 'Plan API', 'done', 'plan-api'),
      lane: 'Store',
      needs: [],
      badges: [],
      links: [
        {
          kind: 'pr',
          ref: 'example-user/probe#20',
          method: 'plan_line',
          confidence: 'exact',
          inferred: true,
          pr: { repo: 'example-user/probe', number: 20, url: 'https://github.com/example-user/probe/pull/20', state: 'MERGED', head_branch: 'aim/c3-plan-api' },
        },
      ],
    },
  ],
  unmatched: [],
  warnings: [],
  alerts: [],
}
