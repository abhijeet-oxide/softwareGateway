import { useMemo, useState, type ReactNode } from 'react'
import { Card, Segmented, Space, Tooltip, Typography } from 'antd'
import { Link } from 'react-router-dom'
import { useAvailability } from '../api/queries'
import { formatCount, formatDuration, formatPercent } from '../domain/format'
import { TimeAgo } from './chips'
import { c, StatusPill } from '../uikit'
import type { AvailabilityResponse, AvailabilityStatus, AvailabilityWindow, Outage } from '../api/types'

/**
 * WAS THE SERVICE UP, AND HOW OFTEN HAS IT NOT BEEN.
 *
 * # Why this is on the Overview and not on a dashboard
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
    <Card
      title={
        <Space size={8}>
          Service availability
          {data && <StatusPill tone={toneFor(data.status)}>{labelFor(data.status)}</StatusPill>}
        </Space>
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
      loading={availability.isLoading}
    >
      {data ? <Recorded data={data} /> : <NotRecorded />}
    </Card>
  )
}

/**
 * A deployment where nothing has been recorded yet.
 *
 * It says so rather than showing an empty timeline, because an empty timeline
 * is indistinguishable from an unbroken one and means the opposite.
 */
function NotRecorded() {
  return (
    <Typography.Text type="secondary" style={{ fontSize: 13 }}>
      No availability has been recorded. The Coordinator records the periods it was serving; the
      record begins when a Coordinator carrying it first starts.
    </Typography.Text>
  )
}

function Recorded({ data }: { data: AvailabilityResponse }) {
  const outages = data.outages ?? []
  const segments = useMemo(() => timeline(data), [data])

  if (!data.recordedFrom) return <NotRecorded />

  return (
    <>
      <Space size={24} wrap style={{ marginBottom: 12 }}>
        <Figure
          label={data.status === 'DOWN' ? 'Not serving since' : 'Serving since'}
          value={<TimeAgo at={data.statusSince} />}
        />
        <Figure label="Uptime" value={formatPercent(data.uptime * 100) ?? '—'} />
        <Figure
          label="Outages"
          value={
            outages.length === 0
              ? 'None'
              : `${formatCount(outages.length)} · ${formatDuration(data.downSeconds) ?? '0s'}`
          }
        />
        {data.degradedSeconds > 0 && (
          <Figure label="Degraded" value={formatDuration(data.degradedSeconds) ?? '0s'} />
        )}
        <Figure label="Starts" value={formatCount(data.starts) ?? '0'} />
      </Space>

      {/*
        THE WINDOW, DRAWN TO SCALE. A list of outages answers "what happened";
        this answers "when", which is the question somebody arrives with - they
        were told it broke at about nine, and they want to see whether anything
        happened at about nine.

        Segments are proportional to real time, so a two-minute outage in a day
        is a sliver rather than a third of the strip: an outage drawn larger
        than it was is a panel arguing with the number beside it.
      */}
      <div
        style={{
          display: 'flex',
          height: 10,
          borderRadius: 3,
          overflow: 'hidden',
          background: c.border,
          marginBottom: 6,
        }}
      >
        {segments.map((seg, i) => (
          <Tooltip key={i} title={seg.title}>
            <div style={{ flexGrow: seg.weight, background: seg.colour, minWidth: 1 }} />
          </Tooltip>
        ))}
      </div>
      {/* The strip's two ends, labelled as an axis rather than as a sentence:
          the left edge is where the record being drawn starts, which is the
          window asked for or the start of the record, whichever is later. */}
      <Space style={{ width: '100%', justifyContent: 'space-between', marginBottom: 12 }}>
        <Typography.Text type="secondary" style={{ fontSize: 11 }}>
          <TimeAgo at={data.since} />
        </Typography.Text>
        <Typography.Text type="secondary" style={{ fontSize: 11 }}>
          Now
        </Typography.Text>
      </Space>

      {outages.length > 0 && (
        <Space orientation="vertical" size={2} style={{ width: '100%' }}>
          {/* Newest first: the one somebody is asking about is almost always
              the last one. */}
          {[...outages].reverse().slice(0, 4).map((o) => (
            <Typography.Text key={o.began} style={{ fontSize: 12 }}>
              <span style={{ color: c.danger }}>●</span>{' '}
              {o.ongoing ? 'Not serving since ' : 'Not serving for '}
              {o.ongoing ? null : <b>{formatDuration(o.seconds) ?? '0s'}</b>}
              {o.ongoing ? <TimeAgo at={o.began} /> : <>, ending <TimeAgo at={o.ended} /></>}
            </Typography.Text>
          ))}
          {outages.length > 4 && (
            <Typography.Text type="secondary" style={{ fontSize: 11 }}>
              {formatCount(outages.length - 4)} earlier outages are not listed.
            </Typography.Text>
          )}
        </Space>
      )}

      {/*
        THE RESOLUTION, STATED. Every number here is sampled, and a record that
        did not say how often would invite somebody to conclude that a
        thirty-second outage did not happen because it is not listed.
      */}
      <Typography.Text
        type="secondary"
        style={{ fontSize: 11, display: 'block', marginTop: 10 }}
      >
        Recorded by the Coordinator every {formatDuration(data.beatSeconds) ?? '15s'}. An
        interruption shorter than that is not recorded. <Link to="/settings">View health</Link>
      </Typography.Text>
    </>
  )
}

function Figure({ label, value }: { label: string; value: ReactNode }) {
  return (
    <Space orientation="vertical" size={0}>
      <Typography.Text type="secondary" style={{ fontSize: 11 }}>
        {label}
      </Typography.Text>
      <Typography.Text style={{ fontSize: 14 }}>{value}</Typography.Text>
    </Space>
  )
}

interface Segment {
  weight: number
  colour: string
  title: string
}

/**
 * The window as proportional segments: served, and not served.
 *
 * Built from the OUTAGES rather than from the runs, because the outages are
 * what the service is certain about - a run is one replica's record and the
 * summary has already unioned them. Anything not inside an outage is time the
 * service was answering.
 */
function timeline(data: AvailabilityResponse): Segment[] {
  const from = Date.parse(data.since)
  const to = Date.parse(data.now)
  const span = to - from
  if (!Number.isFinite(span) || span <= 0) return []

  const out: Segment[] = []
  let cursor = from
  const served = (until: number) => {
    if (until <= cursor) return
    out.push({
      weight: (until - cursor) / span,
      // ONE COLOUR FOR SERVED TIME. Painting it by the CURRENT status would
      // turn a whole day amber because the last minute was degraded, which is
      // a claim about history drawn from a fact about now. The strip answers
      // served or not served; degraded time is stated as a figure above,
      // where it can carry its own number.
      colour: c.ok,
      title: 'Serving',
    })
    cursor = until
  }

  for (const o of sortedOutages(data.outages ?? [])) {
    const began = Date.parse(o.began)
    const ended = Date.parse(o.ended)
    if (!Number.isFinite(began) || !Number.isFinite(ended)) continue
    served(began)
    out.push({
      weight: Math.max(ended - began, 0) / span,
      colour: c.danger,
      title: `Not serving for ${formatDuration(o.seconds) ?? '0s'}`,
    })
    cursor = Math.max(cursor, ended)
  }
  served(to)
  return out
}

function sortedOutages(outages: Outage[]): Outage[] {
  return [...outages].sort((a, b) => Date.parse(a.began) - Date.parse(b.began))
}

function toneFor(status: AvailabilityStatus) {
  switch (status) {
    case 'HEALTHY':
      return 'ok' as const
    case 'DEGRADED':
      return 'pending' as const
    case 'DOWN':
      return 'danger' as const
    default:
      return 'neutral' as const
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
