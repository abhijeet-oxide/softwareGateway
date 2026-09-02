import { useCallback, useMemo } from 'react'
import { useSearchParams } from 'react-router-dom'

import type { Package } from '../api/types'
import { packageReference } from './derive'

/**
 * Choosing two releases to compare, as a thing the URL holds.
 *
 * # What this replaced
 *
 * A page of its own with a product select, a two-position mode switch, two
 * release dropdowns, a swap button and sometimes a fourth select for the
 * source. Six controls to express "these two", and the two dropdowns were the
 * worst of it: a list of two hundred releases, rendered as a name and a version
 * in a box 320 pixels wide, with no status, no date, no repository and no way
 * to filter by anything but a substring. Everything a reader needs in order to
 * DECIDE which two releases to compare - what is in production, what arrived
 * last week, what has vulnerabilities - was on the packages listing they had
 * just left.
 *
 * So the choosing happens there. The listing is already the place where a
 * person can search, filter by status, sort by date and read the counts; making
 * it the selection surface costs a checkbox column and buys every one of those
 * for free.
 *
 * # Why the selection lives in the URL and not in component state
 *
 * Because it has to survive three things that all destroy component state:
 * changing the search box, changing the product filter, and the round trip to
 * the report and back. "Pick one, search for the other, lose the first" is the
 * exact failure that makes a two-step selection feel broken, and it is what
 * every naive implementation of this does.
 *
 * It also makes a half-made selection shareable, which is not the point but is
 * a pleasant consequence: a link to "these two, compared" is the same link with
 * one more parameter.
 */

/**
 * What the reader is comparing FOR.
 *
 * # Why this is asked before the two releases and not after
 *
 * Because it changes which releases are worth choosing between. A comparison of
 * vulnerabilities against a release nobody has scanned cannot say anything, so
 * offering those rows is offering a choice that does not work - and the reader
 * only finds out two clicks later, on a page that has to explain its own
 * refusal.
 *
 * Asked first, it costs one two-position switch and removes the rows that
 * cannot answer. It is also the answer to "which tab should the report open
 * on", which is a question the reader would otherwise have to answer again.
 */
export type Intent = 'contents' | 'vulnerabilities'

/** One end of a comparison: which product, and which release within it. */
export interface Pick {
  product: string
  /** `repository:tag`, or the bare tag where a product has one repository. */
  ref: string
}

/** The whole selection, either end of which may be unmade. */
export interface Selection {
  /** Whether the listing is in selection mode at all. */
  active: boolean
  /** What the comparison is for. Contents unless the reader says otherwise. */
  intent: Intent
  a?: Pick
  b?: Pick
}

/** Whether a selection can be compared. */
export function complete(s: Selection): boolean {
  return Boolean(s.a && s.b && s.a.ref !== s.b.ref)
}

/** The reference this listing row would contribute. */
export function pickOf(product: string, pkg: Package): Pick {
  return { product, ref: packageReference(pkg) }
}

/** Whether two picks name the same release. */
export function samePick(a?: Pick, b?: Pick): boolean {
  return Boolean(a && b && a.product === b.product && a.ref === b.ref)
}

/**
 * The parameters a selection occupies.
 *
 * Named rather than positional, and the product is shared: comparing across
 * products is not a thing the API can do - two products are two sets of
 * repositories with two sets of credentials - so a selection has one product
 * by construction, and the listing disables the rows that would break that.
 */
const PARAM = { mode: 'compare', product: 'cmp', a: 'a', b: 'b', intent: 'for' } as const

/** Marks a listing product filter that comparison selection added itself. */
export const COMPARISON_PRODUCT_FILTER = 'compareProductFilter'

/**
 * Reads and writes the selection in the query string.
 *
 * Returns operations rather than a setter, because every caller wants a verb -
 * toggle this row, clear both, leave selection mode - and a raw setter would
 * put the "one product only" rule in each of them.
 */
export function useComparisonSelection() {
  const [params, setParams] = useSearchParams()

  const selection = useMemo<Selection>(() => {
    const product = params.get(PARAM.product) ?? undefined
    const a = params.get(PARAM.a) ?? undefined
    const b = params.get(PARAM.b) ?? undefined
    return {
      active: params.get(PARAM.mode) === '1',
      intent: params.get(PARAM.intent) === 'vulnerabilities' ? 'vulnerabilities' : 'contents',
      a: product && a ? { product, ref: a } : undefined,
      b: product && b ? { product, ref: b } : undefined,
    }
  }, [params])

  /**
   * Writes a selection derived from whatever is in the URL RIGHT NOW.
   *
   * # Why the change is a function of the current state and not a value
   *
   * Because two clicks can land inside one render. `selection` is derived from
   * the params this render was given, so a second click arriving before React
   * has re-rendered decides against a selection that is already out of date -
   * and the second row silently does not stick. Measured: two ticks 1200ms
   * apart both landed, two 400ms apart lost one.
   *
   * Reading the params inside the updater is the only version of this that
   * cannot be raced, and the failure it prevents is exactly the one that makes
   * a two-step selection feel broken.
   */
  const apply = useCallback((change: (from: Selection) => Selection) => {
    // The LIVE query string, not the one this render was handed.
    //
    // React Router's functional setter looks like the escape hatch and is not:
    // it calls the updater with the `searchParams` captured by the current
    // render, so it is stale in exactly the case the functional form exists to
    // solve. `window.location.search` is not, because the previous write went
    // through history.replaceState synchronously - so a second tick 200ms after
    // the first sees the first.
    //
    // The app mounts under a BrowserRouter (see main.tsx), which is what makes
    // the browser's own URL the authority here.
    const current = new URLSearchParams(window.location.search)
    const product = current.get(PARAM.product) ?? undefined
    const a = current.get(PARAM.a) ?? undefined
    const b = current.get(PARAM.b) ?? undefined
    const next = change({
      active: current.get(PARAM.mode) === '1',
      intent: current.get(PARAM.intent) === 'vulnerabilities' ? 'vulnerabilities' : 'contents',
      a: product && a ? { product, ref: a } : undefined,
      b: product && b ? { product, ref: b } : undefined,
    })

    setParams(() => {
      const out = new URLSearchParams(current)
      if (next.active) out.set(PARAM.mode, '1')
      else out.delete(PARAM.mode)

      const chosen = next.a?.product ?? next.b?.product
      if (chosen) {
        out.set(PARAM.product, chosen)
        if (!current.get('product')) out.set(COMPARISON_PRODUCT_FILTER, '1')
        out.set('product', chosen)
      } else {
        out.delete(PARAM.product)
        if (current.get(COMPARISON_PRODUCT_FILTER) === '1') {
          out.delete('product')
          out.delete(COMPARISON_PRODUCT_FILTER)
        }
      }

      if (next.a) out.set(PARAM.a, next.a.ref)
      else out.delete(PARAM.a)
      if (next.b) out.set(PARAM.b, next.b.ref)
      else out.delete(PARAM.b)

      // Contents is the default and leaves no parameter, so an ordinary
      // comparison link stays short and a shared URL says only what was chosen.
      if (next.intent === 'vulnerabilities') out.set(PARAM.intent, next.intent)
      else out.delete(PARAM.intent)
      return out
      // `replace`, so a selection does not fill the reader's back button with
      // one entry per checkbox. Backing out of a comparison should return them
      // to wherever they came from, not walk them through six clicks.
    }, { replace: true })
  }, [setParams])

  /**
   * Adds or removes one release.
   *
   * # Why the slots fill in order and empty in place
   *
   * Because that is what somebody means. The first click is the base, the
   * second is what it is compared against, and clicking a third time on either
   * of them removes THAT one rather than shuffling the other along - so
   * correcting a mis-click costs one click and does not silently re-cast the
   * comparison the other way round.
   *
   * A third release when both slots are full replaces the SECOND. The first is
   * the one somebody chose deliberately and is usually the baseline; the second
   * is the one they are trying alternatives against, which is the whole shape
   * of "how does this compare with that, and with that".
   */
  const toggle = useCallback((pick: Pick) => apply((current) => {
    if (samePick(current.a, pick)) {
      // Removing the first promotes the second, so the remaining choice is
      // the base rather than an orphan sitting in the second slot.
      return { ...current, active: true, a: current.b, b: undefined }
    }
    if (samePick(current.b, pick)) {
      return { ...current, active: true, b: undefined }
    }
    // A pick from another product replaces the selection rather than being
    // refused silently. The listing disables those rows, so reaching here means
    // the reader changed the product filter - and starting again is what they
    // meant by that.
    if (current.a && current.a.product !== pick.product) {
      return { ...current, active: true, a: pick, b: undefined }
    }
    if (!current.a) {
      return { ...current, active: true, a: pick }
    }
    return { ...current, active: true, b: pick }
  }), [apply])

  const start = useCallback(() => apply((s) => ({ ...s, active: true })), [apply])
  const reset = useCallback(
    () => apply((s) => ({ intent: s.intent, active: true })), [apply])
  const cancel = useCallback(
    () => apply((s) => ({ intent: s.intent, active: false })), [apply])

  /**
   * Changes what the comparison is for, dropping picks the new intent cannot
   * use.
   *
   * # Why a pick is dropped rather than kept and refused
   *
   * Because in the vulnerabilities intent a release with no findings is not a
   * choice that happens to be unavailable - it is not in the list at all. A
   * selection naming a row the reader can no longer see, above a Compare button
   * that would produce a verdict saying nothing, is worse than a slot that
   * visibly empties at the moment the mode changes.
   *
   * `usable` is supplied by the caller because whether a release has findings is
   * the listing's knowledge, not this module's.
   */
  const setIntent = useCallback((intent: Intent, usable: (p: Pick) => boolean) => apply((s) => {
    if (intent !== 'vulnerabilities') return { ...s, active: true, intent }
    const a = s.a && usable(s.a) ? s.a : undefined
    const b = s.b && usable(s.b) ? s.b : undefined
    // A second end with no first is an orphan; promote it.
    return { ...s, active: true, intent, a: a ?? b, b: a ? b : undefined }
  }), [apply])
  /** Swaps which end is the base, for a comparison read the other way round. */
  const swap = useCallback(
    () => apply((s) => ({ ...s, active: true, a: s.b, b: s.a })),
    [apply],
  )

  return { selection, toggle, start, reset, cancel, swap, setIntent }
}

/**
 * The link to a comparison's report.
 *
 * The report keeps a route of its own rather than rendering inside the listing:
 * it is a page-sized answer somebody waits minutes for, sends to a colleague,
 * and comes back to. What it no longer keeps is a way to CHOOSE - arriving
 * there without a pair sends the reader back to the listing, which is where
 * choosing happens.
 */
export function comparisonHref(a: Pick, b: Pick, intent: Intent = 'contents'): string {
  const q = new URLSearchParams({ product: a.product, a: a.ref, b: b.ref })
  // The report opens on the answer that was asked for. Carrying the intent is
  // what stops the reader choosing it twice - once to filter the list, and
  // again on the tab strip.
  if (intent === 'vulnerabilities') q.set('view', 'security')
  return `/packages/compare?${q.toString()}`
}

/** The link back to the listing, in selection mode, with the pair preserved. */
export function selectionHref(selection: Selection): string {
  const q = new URLSearchParams({ compare: '1' })
  if (selection.intent === 'vulnerabilities') q.set(PARAM.intent, selection.intent)
  const product = selection.a?.product ?? selection.b?.product
  if (product) q.set(PARAM.product, product)
  if (selection.a) q.set(PARAM.a, selection.a.ref)
  if (selection.b) q.set(PARAM.b, selection.b.ref)
  return `/packages?${q.toString()}`
}
