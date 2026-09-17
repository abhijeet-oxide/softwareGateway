import { useId, useMemo, useState } from 'react'
import { c } from '../uikit'

export interface SparkPoint {
  /** What the bucket is called when somebody hovers it. */
  label: string
  value: number
}

/**
 * ONE SERIES OVER TIME, at the size of a card.
 *
 * # What this is for, and what it is not
 *
 * A trend: is this going up, down, or holding? It carries no axis and no
 * gridlines, because at ninety pixels tall neither would be readable and both
 * would be ink that is not data. The two figures above it carry the magnitude;
 * this carries the shape, and the hover carries any single value somebody
 * wants to read off it.
 *
 * It is deliberately not a chart with a legend. One series needs none - the
 * label above it already says what is plotted - and a legend box with one
 * swatch in it restates the title and costs the height the trend is drawn in.
 *
 * # Every value is reachable without hovering
 *
 * A tooltip may enhance and must never gate. The reader who cannot hover has
 * the totals stated above the plot and the full table on the report this links
 * to, so nothing here is the only way to a number.
 *
 * # Gaps are drawn as zero, and that is a decision
 *
 * The series comes from a query that groups by day, so a day on which nothing
 * happened produces NO ROW rather than a zero. Plotting what arrives would
 * space seven days across however many of them had traffic, which draws a
 * quiet week as a busy one. The caller fills the gaps; this draws what it is
 * given, evenly spaced.
 */
export function Sparkline({
  points,
  height = 84,
  format,
  tone = c.brand,
}: {
  points: SparkPoint[]
  height?: number
  /** Renders a value for the hover. The chart never formats a number itself. */
  format: (value: number) => string
  tone?: string
}) {
  const gradientId = useId()
  const [hover, setHover] = useState<number | undefined>()

  // A viewBox rather than measured pixels: the card decides the width, and an
  // SVG that scales to it needs no resize observer to stay correct.
  const W = 300
  const H = height
  // Room for the end marker and its ring, so neither is clipped by the frame.
  const PAD = 6

  const { area, line, dots, peak } = useMemo(() => {
    const peak = Math.max(1, ...points.map((p) => p.value))
    const step = points.length > 1 ? (W - PAD * 2) / (points.length - 1) : 0
    const dots = points.map((p, i) => ({
      x: PAD + i * step,
      // A flat zero sits ON the baseline rather than a pixel above it: an empty
      // day is nothing, and nothing has no height.
      y: PAD + (1 - p.value / peak) * (H - PAD * 2),
      point: p,
    }))
    if (dots.length === 0) return { area: '', line: '', dots, peak }
    if (dots.length === 1) {
      // One day is not a trend. It is drawn as a level so the card is not
      // empty, and says as much as it honestly can.
      const only = dots[0]!
      const flat = `M ${PAD} ${only.y} L ${W - PAD} ${only.y}`
      return { area: `${flat} L ${W - PAD} ${H - PAD} L ${PAD} ${H - PAD} Z`, line: flat, dots, peak }
    }
    const line = curve(dots)
    const area = `${line} L ${dots[dots.length - 1]!.x.toFixed(2)} ${H - PAD} L ${dots[0]!.x.toFixed(2)} ${H - PAD} Z`
    return { area, line, dots, peak }
  }, [points, H])

  if (points.length === 0) return null

  const last = dots[dots.length - 1]!
  const active = hover !== undefined ? dots[hover] : undefined

  return (
    <div style={{ position: 'relative' }}>
      <svg
        viewBox={`0 0 ${W} ${H}`}
        preserveAspectRatio="none"
        style={{ width: '100%', height, display: 'block' }}
        role="img"
        aria-label={`Trend over ${points.length} days. Highest ${format(peak)}.`}
        onMouseLeave={() => setHover(undefined)}
      >
        <defs>
          {/* The fill is a WASH - the hue at a tenth - fading out downwards.
              A saturated block this size would be the loudest thing on the
              page, and the line is what carries the shape anyway. */}
          <linearGradient id={gradientId} x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stopColor={tone} stopOpacity="0.20" />
            <stop offset="100%" stopColor={tone} stopOpacity="0.02" />
          </linearGradient>
        </defs>

        <path d={area} fill={`url(#${gradientId})`} />
        {/* vectorEffect keeps the stroke 2px after the non-uniform scale that
            preserveAspectRatio="none" applies - without it the line thins and
            thickens with the card's width. */}
        <path
          d={line}
          fill="none"
          stroke={tone}
          strokeWidth={2}
          strokeLinecap="round"
          strokeLinejoin="round"
          vectorEffect="non-scaling-stroke"
        />

        {/* The end of the series is the one point worth marking: it is where
            the trend has got to, which is the number the figures above state. */}
        {active === undefined && (
          <circle cx={last.x} cy={last.y} r={4} fill={tone} stroke={c.surface} strokeWidth={2}
                  vectorEffect="non-scaling-stroke" />
        )}
        {active && (
          <>
            <line x1={active.x} y1={PAD} x2={active.x} y2={H - PAD}
                  stroke={c.borderStrong} strokeWidth={1} vectorEffect="non-scaling-stroke" />
            <circle cx={active.x} cy={active.y} r={4.5} fill={tone} stroke={c.surface} strokeWidth={2}
                    vectorEffect="non-scaling-stroke" />
          </>
        )}

        {/* THE HIT AREAS, and they are the full height of the plot rather than
            the dot. A four-pixel target is a target nobody hits; a column per
            point means the pointer only has to be over the right day. */}
        {dots.map((d, i) => (
          <rect
            key={d.point.label}
            x={d.x - (W - PAD * 2) / Math.max(points.length - 1, 1) / 2}
            y={0}
            width={(W - PAD * 2) / Math.max(points.length - 1, 1)}
            height={H}
            fill="transparent"
            onMouseEnter={() => setHover(i)}
          />
        ))}
      </svg>

      {active && (
        <div
          style={{
            position: 'absolute',
            // Clamped to the plot's own width. Centred on the point is right
            // for every point but the first and the last, where it would hang
            // off the card and be clipped by it.
            left: `clamp(58px, ${((active.x / W) * 100).toFixed(2)}%, calc(100% - 58px))`,
            // ANCHORED TO THE POINT, not to the top of the plot. Pinned to the
            // top it covered the two figures above the chart - the tooltip
            // hiding the totals it is a detail of. Above the marker normally,
            // below it when the marker is high enough that above would leave
            // the plot.
            top: `${(active.y / H) * 100}%`,
            transform:
              active.y > H * 0.45
                ? 'translate(-50%, calc(-100% - 10px))'
                : 'translate(-50%, 10px)',
            pointerEvents: 'none',
            whiteSpace: 'nowrap',
            borderRadius: 6,
            background: 'var(--tooltip-bg)',
            color: 'var(--tooltip-fg)',
            padding: '4px 8px',
            fontSize: 11.5,
            lineHeight: 1.4,
            boxShadow: 'var(--el-2)',
          }}
        >
          <div style={{ opacity: 0.75 }}>{active.point.label}</div>
          <div style={{ fontWeight: 600 }}>{format(active.point.value)}</div>
        </div>
      )}
    </div>
  )
}

/**
 * The series as a smooth path, and MONOTONE smoothing specifically.
 *
 * # Why not a plain polyline, and why not an ordinary spline
 *
 * A polyline is honest and reads as jagged at this size, where every day is
 * forty pixels apart. An ordinary cubic spline reads beautifully and LIES: it
 * overshoots between points, so a day of nothing followed by a busy one dips
 * below zero on the way up, and the curve draws a peak higher than the highest
 * day. A reader cannot tell an interpolated extreme from a measured one.
 *
 * Fritsch-Carlson tangents are the fix. The curve is smooth, and it is
 * constrained never to leave the interval between two neighbouring values - so
 * it cannot invent a peak, a trough, or a negative day. Where the data turns,
 * the tangent is flattened to zero, which is what makes a local maximum sit ON
 * the point rather than past it.
 */
function curve(points: { x: number; y: number }[]): string {
  const n = points.length
  if (n < 2) return ''

  // Secants, then tangents constrained to them.
  const dx: number[] = []
  const dy: number[] = []
  const slope: number[] = []
  for (let i = 0; i < n - 1; i++) {
    dx.push(points[i + 1]!.x - points[i]!.x)
    dy.push(points[i + 1]!.y - points[i]!.y)
    slope.push(dx[i]! === 0 ? 0 : dy[i]! / dx[i]!)
  }

  const m: number[] = [slope[0] ?? 0]
  for (let i = 1; i < n - 1; i++) {
    const a = slope[i - 1]!
    const b = slope[i]!
    // A TURNING POINT GETS A FLAT TANGENT. Without this the curve carries on
    // in the direction it was going and sails past the value it is meant to
    // touch, which is the overshoot this whole function exists to avoid.
    m.push(a * b <= 0 ? 0 : (2 * a * b) / (a + b))
  }
  m.push(slope[n - 2] ?? 0)

  let d = `M ${points[0]!.x.toFixed(2)} ${points[0]!.y.toFixed(2)}`
  for (let i = 0; i < n - 1; i++) {
    const p0 = points[i]!
    const p1 = points[i + 1]!
    const third = dx[i]! / 3
    d += ` C ${(p0.x + third).toFixed(2)} ${(p0.y + m[i]! * third).toFixed(2)}` +
         ` ${(p1.x - third).toFixed(2)} ${(p1.y - m[i + 1]! * third).toFixed(2)}` +
         ` ${p1.x.toFixed(2)} ${p1.y.toFixed(2)}`
  }
  return d
}
