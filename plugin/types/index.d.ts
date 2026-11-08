// The ai-manager mod's state contract: the values its band draws from,
// kept in $.state so a reload keeps them.

/** A file aimd's detector says looks like a dispatch plan. */
export type PlanOffer = {
  /** The file as aimd resolved it. */
  path: string
  /** Its base name, as the band shows it. */
  file: string
  packages: number
  lanes: number
  /** The slug it would get. */
  slug: string
}

/** A package an unlinked agent could be linked to. */
export type LinkCandidate = {
  plan: string
  id: string
  title: string
}

/** An agent spawned without a clear package, offered in the band. */
export type UnlinkedSpawn = {
  /** The agent id, or the cloud session URL for a remote agent. */
  key: string
  sessionId: string
  agentId?: string
  cloudUrl?: string
  /** The Agent call's short description. */
  description: string
  candidates: LinkCandidate[]
}

/**
 * The band's progress line for one plan, as aimd's plan
 * summary counts it: `sharing · 3/15 done · 1 ready · 2 need you · 7 to merge`.
 */
export type PlanProgress = {
  slug: string
  done: number
  total: number
  /** Packages ready to dispatch: the length of the summary's `ready`. */
  ready: number
  /** The summary's `needs_you`. */
  needsYou: number
  /** The summary's `to_merge` on a plan whose lead merges; 0 hides it. */
  toMerge: number
  /** The other plans registered for this repo, shown as "+N plans". */
  more: number
}

declare module 'claude-code' {
  interface PluginState {
    'ai-manager': {
      offer: Shaped<PlanOffer | null>
      unlinked: Shaped<UnlinkedSpawn[]>
      /** The progress line the band shows; null shows none. */
      progress: Shaped<PlanProgress | null>
      /** Plans this session registered or dispatched agents for, latest first (slugs). */
      mine: Shaped<string[]>
    }
  }
}
