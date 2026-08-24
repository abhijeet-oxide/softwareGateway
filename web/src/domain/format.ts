import type { Int64String } from '../api/types'

/**
 * Rendering rules from the UI brief §8. One place, so every page agrees.
 *
 * The rule underneath all of them: a value we do not have renders as an em
 * dash with a reason, never as zero.
 */

/**
 * The words shown where a value is genuinely unavailable.
 *
 * Every formatter below returns `null` rather than this string, so the DECISION
 * to say "not available" is made here and the RENDERING is made once, by
 * <Value> in components/value.tsx - italic and secondary, so an absent value
 * never reads as a measured one.
 *
 * `null` rather than a string because it makes the compiler the enforcement:
 * a site that renders a formatter's output without handling the absent case
 * does not typecheck.
 */
export const NOT_AVAILABLE = 'N/A'

/** Parses an Int64String. Returns undefined for absent, which is not zero. */
export function bytes(value: Int64String | undefined | null): number | undefined {
  if (value === undefined || value === null || value === '') return undefined
  const n = Number(value)
  return Number.isFinite(n) ? n : undefined
}

/** Binary units to one decimal, as the brief specifies: `15.6 GB`. */
export function formatBytes(value: Int64String | number | undefined | null): string | null {
  const n = typeof value === 'number' ? value : bytes(value ?? undefined)
  if (n === undefined) return null
  if (n === 0) return '0 B'

  const units = ['B', 'KB', 'MB', 'GB', 'TB', 'PB']
  const i = Math.min(Math.floor(Math.log(Math.abs(n)) / Math.log(1024)), units.length - 1)
  const scaled = n / 1024 ** i
  // Bytes are a whole count, but a speed computed from a duration is rarely
  // one - `7.113459213686935 B/s` is the same lie in more digits than `7 B/s`.
  return `${i === 0 ? Math.round(scaled) : scaled.toFixed(1)} ${units[i]}`
}

/** Speeds as `142 MB/s`. */
export function formatSpeed(value: Int64String | number | undefined | null): string | null {
  const n = typeof value === 'number' ? value : bytes(value ?? undefined)
  if (n === undefined) return null
  return `${formatBytes(n)}/s`
}

/** Durations as `1h 56m`, and `18s` below a minute. */
export function formatDuration(seconds: number | undefined | null): string | null {
  if (seconds === undefined || seconds === null || !Number.isFinite(seconds) || seconds < 0) {
    return null
  }
  const total = Math.floor(seconds)
  const h = Math.floor(total / 3600)
  const m = Math.floor((total % 3600) / 60)
  const s = total % 60

  if (h > 0) return `${h}h ${String(m).padStart(2, '0')}m`
  if (m > 0) return `${m}m ${String(s).padStart(2, '0')}s`
  return `${s}s`
}

/** Elapsed seconds between two timestamps, or from one to now. */
export function elapsedSeconds(from?: string | null, to?: string | null): number | undefined {
  if (!from) return undefined
  const start = Date.parse(from)
  if (Number.isNaN(start)) return undefined
  const end = to ? Date.parse(to) : Date.now()
  if (Number.isNaN(end)) return undefined
  return (end - start) / 1000
}

/** Relative time - `2h ago`. The absolute form goes in the tooltip. */
export function formatRelative(timestamp: string | undefined | null): string | null {
  if (!timestamp) return null
  const then = Date.parse(timestamp)
  if (Number.isNaN(then)) return null

  const seconds = (Date.now() - then) / 1000
  if (seconds < 0) return 'just now'
  if (seconds < 60) return 'just now'
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`
  if (seconds < 2592000) return `${Math.floor(seconds / 86400)}d ago`
  /*
    The DAY, not the minute.

    This returned the full absolute timestamp - `21 Jul 2026, 04:34 pm` - which
    in a table column wrapped onto three lines and made every row a different
    height. Nobody scanning a Published column wants the minute; whoever does
    has it in the tooltip beside this, which carries formatAbsolute unchanged.
  */
  return formatDay(timestamp)
}

/** `21 Jul 2026` - a date without a time, for a column. */
export function formatDay(timestamp: string | undefined | null): string | null {
  if (!timestamp) return null
  const t = Date.parse(timestamp)
  if (Number.isNaN(t)) return null
  return new Date(t).toLocaleDateString('en-GB', {
    day: '2-digit', month: 'short', year: 'numeric',
  })
}

/** The tooltip form: `16 Aug 2026 08:30 AM`. */
export function formatAbsolute(timestamp: string | undefined | null): string | null {
  if (!timestamp) return null
  const t = Date.parse(timestamp)
  if (Number.isNaN(t)) return null
  return new Date(t).toLocaleString('en-GB', {
    day: '2-digit', month: 'short', year: 'numeric',
    hour: '2-digit', minute: '2-digit', hour12: true,
  })
}

/** A percentage to one decimal. Absent input stays absent - 0% is a claim. */
export function formatPercent(value: number | undefined | null): string | null {
  if (value === undefined || value === null || !Number.isFinite(value)) return null
  return `${value.toFixed(1)}%`
}

/** Counts with a thousands separator. */
export function formatCount(n: number | undefined | null): string | null {
  if (n === undefined || n === null || !Number.isFinite(n)) return null
  return n.toLocaleString('en-GB')
}
