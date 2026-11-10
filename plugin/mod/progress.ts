// The band's progress line: which plan it shows, the line's
// words, and the dashboard page it opens. Pure: no $ and no aimd here; the
// counts are aimd's plan summary as it answered them (docs/api.md), never
// worked out in the mod.

import type { PlanProgress } from '../types'

import type { Summary } from './aimd'
import { MAX_SLUG, clean } from './text'

const count = (v: unknown): number | undefined =>
  typeof v === 'number' && Number.isInteger(v) && v >= 0 ? v : undefined

/**
 * The progress for a plan from its summary, with `more` other plans; undefined
 * when the answer lacks a count the line needs (an aimd the mod doesn't know).
 */
export function progressOf(summary: unknown, more: number): PlanProgress | undefined {
  if (typeof summary !== 'object' || summary === null) return undefined
  const s = summary as Partial<Record<keyof Summary, unknown>>
  const done = count(s.done)
  const total = count(s.total)
  const needsYou = count(s.needs_you)
  if (typeof s.slug !== 'string' || s.slug === '' || done === undefined || total === undefined) return undefined
  if (!Array.isArray(s.ready) || needsYou === undefined) return undefined
  // "To merge" is the lead's to do: only on a plan whose lead merges.
  const toMerge = s.merges === 'lead' ? (count(s.to_merge) ?? 0) : 0
  return { slug: s.slug, done, total, ready: s.ready.length, needsYou, toMerge, more: Math.max(0, more) }
}

/** `2 need you`, `1 needs you`. */
export function needYouText(n: number): string {
  return `${n} ${n === 1 ? 'needs' : 'need'} you`
}

/** The line's parts after the slug: `3/15 done`, `1 ready`, `2 need you`, `7 to merge` (only when some). */
export function progressParts(p: PlanProgress): string[] {
  const parts = [`${p.done}/${p.total} done`, `${p.ready} ready`, needYouText(p.needsYou)]
  if (p.toMerge > 0) parts.push(`${p.toMerge} to merge`)
  return parts
}

/** "+2 plans", or '' with no other plan. */
export function moreText(more: number): string {
  if (more <= 0) return ''
  return `+${more} ${more === 1 ? 'plan' : 'plans'}`
}

/** The whole line: `sharing · 3/15 done · 1 ready · 2 need you · 7 to merge · +1 plan`. */
export function progressLine(p: PlanProgress): string {
  const more = moreText(p.more)
  return [clean(p.slug, MAX_SLUG), ...progressParts(p), ...(more === '' ? [] : [more])].join(' · ')
}

/**
 * The repo's plans in the order the band picks from: the one this session
 * works on, then the ones it registered or dispatched agents for (latest
 * first), then the rest as aimd lists them (registration order). Only plans
 * registered for this repo count; slugs compare in any case.
 */
export function planOrder(repoPlans: readonly string[], preferred: readonly (string | undefined)[]): string[] {
  const out: string[] = []
  const seen = new Set<string>()
  const add = (slug: string) => {
    const key = slug.toLowerCase()
    if (seen.has(key)) return
    seen.add(key)
    out.push(slug)
  }
  for (const want of preferred) {
    if (want === undefined) continue
    const slug = repoPlans.find(p => p.toLowerCase() === want.toLowerCase())
    if (slug !== undefined) add(slug)
  }
  for (const slug of repoPlans) add(slug)
  return out
}

/** The new dashboard's page for a plan, or its home (`/app/`) without one. */
export function appUrl(base: string, slug?: string): string {
  return slug === undefined ? `${base}/app/` : `${base}/app/plans/${encodeURIComponent(slug)}`
}

/** `mine` with `slug` put first, at most `max` long. */
export function withFirst(mine: readonly string[], slug: string, max = 10): string[] {
  return [slug, ...mine.filter(one => one.toLowerCase() !== slug.toLowerCase())].slice(0, max)
}

/** Whether two progress values draw the same line (so an unchanged poll writes nothing). */
export function sameProgress(a: PlanProgress | null, b: PlanProgress | null): boolean {
  if (a === null || b === null) return a === b
  return (
    a.slug === b.slug &&
    a.done === b.done &&
    a.total === b.total &&
    a.ready === b.ready &&
    a.needsYou === b.needsYou &&
    a.toMerge === b.toMerge &&
    a.more === b.more
  )
}
