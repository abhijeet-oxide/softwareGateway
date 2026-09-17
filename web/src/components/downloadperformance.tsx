import { useMemo } from 'react'
import { Typography } from 'antd'
import { useReports } from '../api/queries'
import { bytes, formatBytes, formatSpeed } from '../domain/format'
import { Sparkline, type SparkPoint } from './sparkline'
import { PanelFoot } from './panel'
import { CardHead, Figure, SectionCard } from '../uikit'
import { CloudDownloadOutlined } from '../icons'
import type { ReportSummary } from '../api/types'

const DAYS = 7

/**
 * WHAT THIS DEPLOYMENT MOVED, and whether that is going up or down.
 *
 * Two figures and a shape. The figures are the week's answer; the shape is what
 * tells somebody whether today is like the rest of the week, which is the thing
 * a single average cannot say and the reason a summary panel is worth drawing
 * at all rather than printing two numbers.
 *
 * The report page is where the per-day numbers, the per-product split and the
 * failure causes live. This links to it rather than reproducing any of it.
 */
export function DownloadPerformancePanel() {
  const reports = useReports({ period: '7d' })
  const totals = reports.data?.totals
  const series = useMemo(() => dailyBytes(reports.data), [reports.data])

  return (
    <SectionCard
      className="ui-card-lead"
      style={{ height: '100%' }}
      title={<CardHead icon={<CloudDownloadOutlined />} tone="ok" title="Download performance" />}
      extra={
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
          Last 7 days
        </Typography.Text>
      }
    >
      <div className="ui-figures" style={{ marginBottom: 12 }}>
        {/*
          ABSENT IS NOT ZERO. The speed is left out of the report when no
          download whose bytes were counted completed in the period, and
          rendering that as "0 B/s" would report a quiet week as a broken one.
        */}
        <Figure
          label="Average download speed"
          value={formatSpeed(totals?.averageBytesPerSecond) ?? 'Not measured'}
        />
        <Figure label="Total data downloaded" value={formatBytes(totals?.bytesTransferred) ?? '0 B'} />
      </div>

      {series.length > 0 ? (
        <Sparkline points={series} format={(v) => formatBytes(v) ?? '0 B'} />
      ) : (
        <Typography.Text type="secondary" style={{ fontSize: 12, display: 'block', padding: '18px 0' }}>
          No download completed on any day in this period, so there is no trend to draw.
        </Typography.Text>
      )}

      <PanelFoot to="/reports" action="View detailed report" />
    </SectionCard>
  )
}

/**
 * The week as one point per day, oldest first, with the quiet days in it.
 *
 * # Why the gaps have to be filled here
 *
 * The volume query groups by day, so a day on which nothing completed produces
 * NO ROW at all. Handing those rows straight to a plot spaces however many days
 * had traffic evenly across the width - which draws a week with two busy days
 * as a week that was busy throughout, and moves every point to a date it does
 * not belong to. So the seven days are generated and the rows are placed into
 * them: a day with no traffic is a zero, because that is what it was.
 */
function dailyBytes(report: ReportSummary | undefined): SparkPoint[] {
  if (!report) return []
  const byDay = new Map<string, number>()
  for (const v of report.volume ?? []) {
    byDay.set(v.day, bytes(v.bytesTransferred) ?? 0)
  }
  if (byDay.size === 0) return []

  const out: SparkPoint[] = []
  const today = new Date()
  for (let i = DAYS - 1; i >= 0; i--) {
    const at = new Date(today)
    at.setDate(today.getDate() - i)
    // The key the server groups on is a plain calendar day, so the local date
    // is rendered the same way rather than through an instant that a timezone
    // would shift across midnight.
    const key = `${at.getFullYear()}-${pad(at.getMonth() + 1)}-${pad(at.getDate())}`
    out.push({
      label: at.toLocaleDateString(undefined, { weekday: 'short', day: 'numeric', month: 'short' }),
      value: byDay.get(key) ?? 0,
    })
  }
  return out
}

function pad(n: number): string {
  return n < 10 ? `0${n}` : String(n)
}
