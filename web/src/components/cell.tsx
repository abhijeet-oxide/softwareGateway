import type { ReactNode } from 'react'
import { c, mono as monoFont } from '../uikit'

/**
 * A table cell that says one thing on top and its qualifier underneath.
 *
 * Several tables need this - a check's category over its mechanism, a
 * package's name over the product and version it belongs to - and each had
 * built its own out of a vertical Space and an inline font size. The result
 * was rows of visibly different heights on adjacent screens, because a
 * two-line cell's height is decided by numbers nobody was comparing: the gap
 * between the lines, the secondary font size, and the line-height each
 * inherits.
 *
 * So the numbers live here, once. A stacked cell is exactly two lines tall
 * wherever it appears, which is what lets a table with one of them sit beside
 * a table without one and still look like the same product.
 *
 * `lines` carries the secondary row. Entries that are null or empty are
 * dropped rather than rendered as a blank line, so a package with no version
 * does not get a taller row than one with a version.
 */
export function CellStack({ title, lines, mono }: {
  title: ReactNode
  lines?: ReactNode[]
  mono?: boolean
}) {
  const rest = (lines ?? []).filter((l) => l !== null && l !== undefined && l !== '' && l !== false)
  return (
    <div style={{ display: 'flex', flexDirection: 'column', gap: 1, minWidth: 0 }}>
      <span
        style={{
          lineHeight: 1.35,
          fontFamily: mono ? monoFont : undefined,
          overflow: 'hidden',
          textOverflow: 'ellipsis',
          whiteSpace: 'nowrap',
        }}
      >
        {title}
      </span>
      {rest.length > 0 && (
        <span
          style={{
            fontSize: 11,
            lineHeight: 1.35,
            color: c.text3,
            display: 'flex',
            gap: 6,
            alignItems: 'center',
            minWidth: 0,
            overflow: 'hidden',
            textOverflow: 'ellipsis',
            whiteSpace: 'nowrap',
          }}
        >
          {rest.map((l, i) => (
            <span key={i} style={{ display: 'inline-flex', gap: 6, alignItems: 'center', minWidth: 0 }}>
              {i > 0 && <span aria-hidden style={{ opacity: 0.5 }}>·</span>}
              {l}
            </span>
          ))}
        </span>
      )}
    </div>
  )
}
