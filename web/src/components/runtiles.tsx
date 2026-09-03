import { Tooltip, Typography } from 'antd'
import { c } from '../uikit'

/**
 * The counters a long-running job puts on screen while it works.
 *
 * # Why this is shared between Security and Compliance
 *
 * They are the same screen. Both are a job against a vendor's registry that
 * takes minutes, both are watched by the same person on the same release, and
 * both have to answer the same question - "is this working, and what is the
 * answer going to be missing" - before it finishes. They answered it in two
 * different visual languages: the compliance run drew counters of real things,
 * and the vulnerability sync drew a row of grey sentences separated by
 * middots. A reader who learned one had to learn the other for no gain.
 *
 * # Why failures are counted here rather than reported at the end
 *
 * A sync against a scanner that is timing out looks identical to a healthy one
 * for two minutes and then delivers the bad news at once, by which time the
 * person who could have stopped it has gone. A tile that says "12 not
 * retrieved" at minute two is the difference between fixing the cause and
 * reading a result nobody can use.
 */
export interface RunTile {
  label: string
  value: string
  /** A colour for the number where the number changes what the answer means. */
  tone?: string
  hint?: string
}

export function RunTiles({ tiles }: { tiles: RunTile[] }) {
  if (tiles.length === 0) return null
  return (
    /*
      A FLEX ROW, not antd's grid.

      Row's gutter is implemented with negative margins on the row and matching
      padding on each column, so the tiles sat wider than everything above and
      below them and ate part of the 16px the surrounding stack sets. The panel's
      gaps came out 16, 16, 10, 16 - close enough to look like a mistake and not
      close enough to look deliberate. A gap does what it says and touches
      nothing outside the row.

      Capped, not stretched. Four tiles across a wide page grew to 320px each,
      and a two-digit number in a 320px box reads as a mistake; the cap keeps
      them the size of the number they hold and lets the row end where the
      facts do.
    */
    <div style={{ display: 'flex', flexWrap: 'wrap', gap: 12, width: '100%' }}>
      {tiles.map((t) => (
        <Tooltip key={t.label} title={t.hint}>
          <div
            style={{
              flex: '1 1 150px',
              minWidth: 140,
              maxWidth: 260,
              border: `1px solid ${c.border}`,
              borderRadius: 8,
              padding: '8px 10px',
              background: c.surface2,
            }}
          >
            <Typography.Text type="secondary" style={{ fontSize: 11 }}>
              {t.label}
            </Typography.Text>
            <div style={{ fontSize: 20, lineHeight: '26px', color: t.tone ?? c.text }}>
              {t.value}
            </div>
          </div>
        </Tooltip>
      ))}
    </div>
  )
}
