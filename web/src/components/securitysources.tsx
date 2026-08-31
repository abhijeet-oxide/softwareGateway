import { Segmented, Select, Space, Tooltip, Typography } from 'antd'
import { useMemo } from 'react'

import type { SecurityFinding, SecuritySourceCounts } from '../api/types'
import { c } from '../uikit'

/**
 * Comparing what two or three scanners said, without building a query builder.
 *
 * # The problem this is the smallest answer to
 *
 * Today one scanner answers. Tomorrow there are three - JFrog Xray, Anchore and
 * an internal one - and the questions people will ask of three scanners are
 * genuinely set-shaped: what did all of them find, what did only Astra find,
 * what did Anchore and Astra agree on that Xray missed. Those are the questions
 * that justify running three scanners at all; a page that can only show a union
 * makes the second and third scanner unauditable.
 *
 * The obvious answer is a filter builder with AND, OR and NOT. That is the
 * answer that gets built, demoed, and never used, because the person who needs
 * it most is the one who does not think in set algebra.
 *
 * # What this does instead
 *
 * Two controls, and the first is enough for most days.
 *
 *   1. A segmented switch: All sources, or one scanner. "What does Xray say" is
 *      the common question and it is one click.
 *   2. A single select, "Reported by", holding the WHOLE truth table as named
 *      options: only in X, in X and Y but not Z, in all three. For three
 *      scanners that is seven rows, which is a list somebody reads - and it is
 *      generated rather than written, so a fourth scanner needs no new UI.
 *
 * Nobody composes anything. Every question is already a row, phrased as a
 * sentence, and the row says how many findings it selects before you pick it.
 *
 * # Why the whole truth table and not "the useful ones"
 *
 * Because which ones are useful depends on which scanner somebody trusts, and
 * that is not a decision this component can make. Seven named rows cost one
 * dropdown; guessing wrong costs the reader the only question they came with.
 */

/** Which scanners a finding must have come from. */
export type SourceFilter =
  /** Everything, whoever reported it. */
  | { mode: 'any' }
  /** Reported by this scanner, whoever else also did. */
  | { mode: 'includes'; provider: string }
  /**
   * Reported by EXACTLY this set and no other scanner.
   *
   * The set is what makes "only in Anchore" and "in Anchore and Astra but not
   * Xray" one concept rather than two features.
   */
  | { mode: 'exactly'; providers: string[] }

/** The filter that hides nothing. */
export const anySource: SourceFilter = { mode: 'any' }

/** Whether a filter is the default, for deciding whether to show a reset. */
export function isAnySource(f: SourceFilter): boolean { return f.mode === 'any' }

/**
 * Which scanners reported one finding.
 *
 * Falls back to `provider` because a finding stored before a second scanner
 * existed carries no `sources`, and reading that as "reported by nobody" would
 * hide every row on the deployment that has the most of them.
 */
export function sourcesOf(f: Pick<SecurityFinding, 'sources' | 'provider'>): string[] {
  if (f.sources && f.sources.length > 0) return f.sources
  return f.provider ? [f.provider] : []
}

/** Whether one finding passes the filter. */
export function matchesSource(f: Pick<SecurityFinding, 'sources' | 'provider'>, filter: SourceFilter): boolean {
  if (filter.mode === 'any') return true
  const got = sourcesOf(f)
  if (filter.mode === 'includes') return got.includes(filter.provider)
  if (got.length !== filter.providers.length) return false
  return filter.providers.every((p) => got.includes(p))
}

/** The filter as a URL-safe token, so a view survives a reload and a share. */
export function encodeSourceFilter(f: SourceFilter): string | undefined {
  if (f.mode === 'any') return undefined
  if (f.mode === 'includes') return `in:${f.provider}`
  return `only:${[...f.providers].sort().join('+')}`
}

/** The inverse of encodeSourceFilter, tolerant of anything it does not know. */
export function decodeSourceFilter(raw: string | undefined): SourceFilter {
  const token = (raw ?? '').trim()
  if (token.startsWith('in:')) {
    const provider = token.slice(3)
    return provider ? { mode: 'includes', provider } : anySource
  }
  if (token.startsWith('only:')) {
    const providers = token.slice(5).split('+').filter(Boolean)
    return providers.length > 0 ? { mode: 'exactly', providers } : anySource
  }
  return anySource
}

/**
 * The subsets of the scanners, as the questions people ask.
 *
 * Generated rather than written down, so a third and fourth scanner need no
 * change here - and capped, because sixteen rows is a list nobody reads. Past
 * the cap the exact-set options are dropped and the two that always make sense
 * - every scanner, and only one scanner - are kept.
 */
const maxExactCombinations = 3

function subsetsOf(providers: string[]): string[][] {
  const out: string[][] = []
  for (let mask = 1; mask < 1 << providers.length; mask += 1) {
    const set: string[] = []
    for (let i = 0; i < providers.length; i += 1) {
      const provider = providers[i]
      if (provider && (mask & (1 << i))) set.push(provider)
    }
    out.push(set)
  }
  // Smallest first, so "only in X" - the question people actually ask - is at
  // the top rather than buried under the combinations.
  return out.sort((a, b) => (a.length !== b.length ? a.length - b.length : a.join().localeCompare(b.join())))
}

/** How a subset reads as a sentence. */
function describeSubset(set: string[], all: SecuritySourceCounts[]): string {
  const label = (p: string) => all.find((s) => s.provider === p)?.label ?? p
  if (set.length === all.length) return 'Found by every scanner'
  if (set.length === 1) return `Only ${label(set[0] ?? '')}`
  const names = set.map(label)
  const missing = all.filter((s) => !set.includes(s.provider)).map((s) => s.label)
  return `${names.join(' and ')} - not ${missing.join(' or ')}`
}

/**
 * The control.
 *
 * Renders NOTHING when one scanner answered. A segmented control with a single
 * position, and a truth table over a set of one, is chrome that teaches a
 * reader to expect a comparison that does not exist.
 */
export function SourceControls({ sources, value, onChange, findings }: {
  sources: SecuritySourceCounts[]
  value: SourceFilter
  onChange: (next: SourceFilter) => void
  /**
   * The rows on screen, for the counts beside each option.
   *
   * A menu of seven set-theoretic questions with no numbers on it is seven
   * guesses; with numbers it is a summary of the disagreement, readable before
   * anything is clicked.
   */
  findings: Pick<SecurityFinding, 'sources' | 'provider'>[]
}) {
  const providers = useMemo(() => sources.map((s) => s.provider), [sources])

  const counts = useMemo(() => {
    const exact = new Map<string, number>()
    const includes = new Map<string, number>()
    for (const f of findings) {
      const got = sourcesOf(f).filter((p) => providers.includes(p)).sort()
      if (got.length === 0) continue
      exact.set(got.join('+'), (exact.get(got.join('+')) ?? 0) + 1)
      for (const p of got) includes.set(p, (includes.get(p) ?? 0) + 1)
    }
    return { exact, includes }
  }, [findings, providers])

  if (sources.length < 2) return null

  const options = [
    { value: 'any', label: `Any scanner (${findings.length.toLocaleString()})` },
    ...(providers.length <= maxExactCombinations
      ? subsetsOf(providers).map((set) => ({
        value: `only:${[...set].sort().join('+')}`,
        label: `${describeSubset(set, sources)} (${(counts.exact.get([...set].sort().join('+')) ?? 0).toLocaleString()})`,
      }))
      : [
        {
          value: `only:${[...providers].sort().join('+')}`,
          label: `Found by every scanner (${(counts.exact.get([...providers].sort().join('+')) ?? 0).toLocaleString()})`,
        },
        ...providers.map((p) => ({
          value: `only:${p}`,
          label: `Only ${sources.find((s) => s.provider === p)?.label ?? p} (${(counts.exact.get(p) ?? 0).toLocaleString()})`,
        })),
      ]),
  ]

  return (
    <Space size={8} wrap>
      {/*
        The common question first, and as one click. "What does Xray say" is
        what somebody asks nine times out of ten, and making them find it in a
        dropdown of set expressions is making the rare case pay for the common
        one.
      */}
      <Segmented
        value={value.mode === 'includes' ? value.provider : 'all'}
        onChange={(v) => onChange(v === 'all' ? anySource : { mode: 'includes', provider: String(v) })}
        options={[
          { value: 'all', label: 'All sources' },
          ...sources.map((s) => ({
            value: s.provider,
            label: (
              <Tooltip
                title={
                  s.onlyHere > 0
                    ? `${s.onlyHere.toLocaleString()} advisories only this scanner reported`
                    : 'Everything this scanner reported was also reported elsewhere'
                }
              >
                <span>
                  {s.label}
                  {s.onlyHere > 0 && (
                    <Typography.Text type="secondary" style={{ fontSize: 11, marginInlineStart: 6 }}>
                      +{s.onlyHere.toLocaleString()}
                    </Typography.Text>
                  )}
                </span>
              </Tooltip>
            ),
          })),
        ]}
      />

      {/*
        And the whole truth table, as sentences, behind one control. Labelled
        "Agreement" rather than "Source" because the segmented switch beside it
        is the source, and two controls both called source is how a reader
        stops reading either.
      */}
      <Select<string>
        value={value.mode === 'exactly' ? `only:${[...value.providers].sort().join('+')}` : 'any'}
        onChange={(v) => onChange(v === 'any' ? anySource : decodeSourceFilter(v))}
        style={{ minWidth: 260 }}
        options={options}
        popupMatchSelectWidth={false}
        // A prefix rather than a placeholder: the control always has a value,
        // and "Agreement: Any scanner" reads as a statement where a bare "Any
        // scanner" reads as a thing that has not been chosen yet.
        prefix={<Typography.Text type="secondary" style={{ color: c.text2 }}>Agreement</Typography.Text>}
      />
    </Space>
  )
}
