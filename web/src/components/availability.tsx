import { useMemo, useState } from 'react'
import { Segmented, Tooltip, Typography } from 'antd'
import { useAvailability } from '../api/queries'
import { formatCount, formatDuration, formatPercent } from '../domain/format'
import { TimeAgo } from './chips'
import { PanelFoot } from './panel'
import { c, CardHead, Figure, SectionCard, StatusPill } from '../uikit'
import { PulseOutlined } from '../icons'
import type { PillTone } from '../uikit'
import type { AvailabilityResponse, AvailabilityStatus, AvailabilityWindow } from '../api/types'

/**
 * WAS THE SERVICE UP, AND HOW OFTEN HAS IT NOT BEEN.
 *
 * # Why this is in the product and not only on a dashboard
 *
 * Because it is the first question anybody has about a service, and the answer
 * lived somewhere the people asking could not reach. The deployment ships a
 * metrics stack that holds `up` for whoever has it open and knows what to type
 * into it; a release manager who was told "the gateway was down this morning"
 * had nowhere in the product to check.
 *
 * What filled that vacuum was worse than nothing. The only availability claim
 * the interface made was inferred by the browser from failing requests, so one
 * endpoint with a broken query produced a backend that appeared to go down,
 * come back, and go down again every few seconds - a statement about the whole
 * service drawn from evidence about one page. This panel is the service's own
 * record, which is the only thing entitled to make that statement.
 *
 * # What it deliberately does NOT show
 *
 * Latency, error rates, or anything per-endpoint. Those are the dashboard's,
 * and a card on the Overview that tried to be one would be a worse dashboard
 * and a worse answer. This says up, down, when, and how often.
 */
export function AvailabilityPanel() {
  const [window, setWindow] = useState<AvailabilityWindow>('24h')
  const availability = useAvailability(window)
  const data = availability.data

  return (
    <SectionCard
      className="ui-card-lead"
      style={{ height: '100%' }}
      title={
        <CardHead
          icon={<PulseOutlined />}
          tone="brand"
          title="Service availability"
          status={
            data && <StatusPill tone={toneFor(data.status)} size="sm">{labelFor(data.status)}</StatusPill>
          }
        />
      }
      extra={
        <Segmented
          size="small"
          value={window}
          onChange={(v) => setWindow(v as AvailabilityWindow)}
          options={[
            { label: '24h', value: '24h' },
            { label: '7d', value: '7d' },
            { label: '30d', value: '30d' },
          ]}
        />
      }
    >
      {data?.recordedFrom ? <Recorded data={data} /> : <NotRecorded loading={availability.isLoading} />}
    </SectionCard>
  )
}

/**
 * A deployment where nothing has been recorded yet.
 *
 * It says so rather than drawing an empty strip, because an empty strip is
 * indistinguishable from an unbroken one and means the opposite.
 */
function NotRecorded({ loading }: { loading: boolean }) {
  return (
    <Typography.Text type="secondary" style={{ fontSize: 13 }}>
      {loading
        ? 'Reading the availability record.'
        : 'No availability has been recorded. The Coordinator records the periods it was serving; ' +
          'the record begins when a Coordinator carrying it first starts.'}
    </Typography.Text>
  )
}

function Recorded({ data }: { data: AvailabilityResponse }) {
  const outages = data.outages ?? []
  const segments = useMemo(() => buckets(data), [data])
  const ongoing = outages.some((o) => o.ongoing)

  return (
    <>
      <div className="ui-figures" style={{ marginBottom: 16 }}>
        <Figure label="Uptime" value={formatPercent(data.uptime * 100) ?? '—'} />
        <Figure
          label={data.status === 'DOWN' ? 'Not serving since' : 'Serving since'}
          value={<TimeAgo at={data.statusSince} />}
        />
        <Figure
          label="Outages"
          value={outages.length === 0 ? 'None' : `${formatCount(outages.length)} · ${formatDuration(data.downSeconds) ?? '0s'}`}
        />
        <Figure label="Starts" value={formatCount(data.starts) ?? '0'} />
      </div>

      {/*
        THE WINDOW, CUT INTO EQUAL BUCKETS. A number says how much; this says
        WHEN, which is the question somebody arrives with - they were told it
        broke at about nine, and they want to see whether anything happened at
        about nine.

        Each segment is the same slice of time, so the shape is a sequence of
        intervals rather than a proportion of a bar. A tooltip names the period
        and what happened in it; the sentence below carries the same fact in
        words, so nothing here is stated by colour alone.
      */}
      <div className="ui-strip">
        {segments.map((seg) => (
          <Tooltip key={seg.key} title={seg.title}>
            <div className="ui-strip-seg" style={{ background: seg.colour }} />
          </Tooltip>
        ))}
      </div>
      <div style={{ display: 'flex', justifyContent: 'space-between', marginTop: 6 }}>
        <Typography.Text type="secondary" style={{ fontSize: 11 }}>
          <TimeAgo at={data.since} />
        </Typography.Text>
        <Typography.Text type="secondary" style={{ fontSize: 11 }}>
          Now
        </Typography.Text>
      </div>

      <PanelFoot to="/settings" action="View health">
        <span style={{ color: outages.length === 0 ? c.ok : c.danger, marginInlineEnd: 6 }}>●</span>
        {outages.length === 0 ? (
          <>No interruptions recorded. Checked every {formatDuration(data.beatSeconds) ?? '15s'}.</>
        ) : ongoing ? (
          <>Not serving since <TimeAgo at={data.statusSince} />.</>
        ) : (
          <>
            Last interruption <TimeAgo at={outages[outages.length - 1]!.ended} />, lasting{' '}
            {formatDuration(outages[outages.length - 1]!.seconds) ?? '0s'}.
          </>
        )}
      </PanelFoot>
    </>
  )
}

interface Bucket {
  key: string
  colour: string
  title: string
}

/**
 * The window as equal segments, each coloured by what happened in it.
 *
 * # Why buckets rather than the outages drawn to scale
 *
 * Because an outage drawn to scale in a thirty-day window is a hairline: the
 * honest proportion is invisible, so the picture says "nothing happened" about
 * a day the service spent an hour down. Equal buckets trade exact width for
 * legibility at a fixed one - a bucket is down if ANY of it was down, which
 * over-reports duration and never under-reports an incident. The figures above
 * carry the exact minutes; this carries where they fell.
 *
 * `partial` exists so the two cannot be confused: a bucket that was down for
 * part of itself is not the same as one that was down throughout, and colouring
 * both red would turn a two-minute blip into a red block an hour wide.
 */
function buckets(data: AvailabilityResponse, count = 44): Bucket[] {
  const from = Date.parse(data.since)
  const to = Date.parse(data.now)
  const span = to - from
  if (!Number.isFinite(span) || span <= 0) return []

  const width = span / count
  const outages = (data.outages ?? [])
    .map((o) => ({ from: Date.parse(o.began), to: Date.parse(o.ended) }))
    .filter((o) => Number.isFinite(o.from) && Number.isFinite(o.to))

  const out: Bucket[] = []
  for (let i = 0; i < count; i++) {
    const start = from + i * width
    const end = start + width
    let down = 0
    for (const o of outages) {
      const overlap = Math.min(end, o.to) - Math.max(start, o.from)
      if (overlap > 0) down += overlap
    }
    const when = `${clock(start)} to ${clock(end)}`
    if (down <= 0) {
      out.push({ key: String(start), colour: c.ok, title: `Serving · ${when}` })
    } else if (down >= width - 1) {
      out.push({ key: String(start), colour: c.danger, title: `Not serving · ${when}` })
    } else {
      out.push({
        key: String(start),
        colour: c.pending,
        title: `Not serving for ${formatDuration(down / 1000) ?? '0s'} · ${when}`,
      })
    }
  }
  return out
}

/** A bucket's boundary, in the reader's own clock. */
function clock(at: number): string {
  return new Date(at).toLocaleString(undefined, {
    day: '2-digit',
    month: 'short',
    hour: '2-digit',
    minute: '2-digit',
  })
}

function toneFor(status: AvailabilityStatus): PillTone {
  switch (status) {
    case 'HEALTHY':
      return 'ok'
    case 'DEGRADED':
      return 'pending'
    case 'DOWN':
      return 'danger'
    default:
      return 'neutral'
  }
}

/**
 * NOT RECORDED IS NOT DOWN. The two are the same absence in the data and
 * opposite facts to a reader: one is an outage, the other is a deployment that
 * has not been recording long enough to say.
 */
function labelFor(status: AvailabilityStatus): string {
  switch (status) {
    case 'HEALTHY':
      return 'Serving'
    case 'DEGRADED':
      return 'Serving, degraded'
    case 'DOWN':
      return 'Not serving'
    default:
      return 'Not recorded'
  }
}
