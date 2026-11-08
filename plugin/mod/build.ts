// Which plugin build this session loaded, for its session_start.
//
// A session loads the mod once, when it starts, from the plugin's folder
// (a checkout's plugin/), and keeps it until it restarts; a `git pull`
// there doesn't reach it. So the mod reports the build it loaded:
// `git describe --tags --always` of the last commit that changed the
// plugin's folder, with "-dirty" when its tracked files differ from that
// commit, e.g. "v1.0.0-12-gabc1234". aimd works out the build on disk the
// same way (internal/worktrees' DescribeDir) and flags a session whose
// build differs. Commits that don't touch the plugin don't change it, and
// nobody bumps a version by hand: plugin.json stays 1.0.0 between releases.
//
// It fails open: no git, a folder that isn't a checkout, a timeout or
// anything else is no build (the field is left out), never an error. Each
// git call has BUILD_TIMEOUT_MS, and the caller runs this unawaited, off
// the hook's path.

import type { EngineInterface } from 'claude-code'

/** How long each git call may take. Two run one after the other at most. */
export const BUILD_TIMEOUT_MS = 1000

/** What session_start carries about the plugin. */
export type PluginBuild = { plugin_build?: string; plugin_root?: string }

/** The git commands, as argv after `git`, run in the plugin's folder. */
export const BUILD_LOG = ['log', '-1', '--format=%H', '--', '.']
export const BUILD_STATUS = ['--no-optional-locks', 'status', '--porcelain', '--untracked-files=no', '--ignore-submodules=dirty', '--', '.']
export const buildDescribe = (commit: string): string[] => ['describe', '--tags', '--always', commit]

const SHA = /^[0-9a-f]{40}([0-9a-f]{24})?$/

/** The plugin's folder and the build in it; {} when the folder is unknown. Never throws. */
export async function pluginBuild($: EngineInterface): Promise<PluginBuild> {
  let root = ''
  try {
    const r: unknown = $.plugin.root
    if (typeof r === 'string') root = r
  } catch {
    return {}
  }
  if (root === '') return {}
  const out: PluginBuild = { plugin_root: root }
  try {
    const git = (args: string[]) => $.process.run(['git', ...args], { cwd: root, timeoutMs: BUILD_TIMEOUT_MS })
    const [log, status] = await Promise.all([git(BUILD_LOG), git(BUILD_STATUS)])
    const commit = log.exitCode === 0 ? log.stdout.trim() : ''
    if (!SHA.test(commit) || status.exitCode !== 0) return out
    const described = await git(buildDescribe(commit))
    const name = described.exitCode === 0 ? described.stdout.trim() : ''
    if (name === '' || /\s/.test(name)) return out
    out.plugin_build = status.stdout.trim() === '' ? name : `${name}-dirty`
  } catch {
    // no build: fail open
  }
  return out
}
