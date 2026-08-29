import { useState } from 'react'
import {
  Alert, Button, Card, Descriptions, Select, Space, Table, Tag, Tooltip, Typography,
} from 'antd'
import { ThunderboltOutlined } from '../icons'
import { useCalibrate, useProducts } from '../api/queries'
import { formatBytes, formatCount, formatDuration, formatSpeed } from '../domain/format'
import { NA, Value } from '../components/value'
import { c, mono, StatusPill } from '../uikit'
import type { CalibrationLevel, CalibrationSide, CalibrationSuggestion } from '../api/types'

/**
 * Is the speed we are getting the speed this path can do?
 *
 * # The question this exists for
 *
 * A download reporting a few hundred kilobytes a second has three possible
 * causes and they need three different people: the link is genuinely that
 * slow, the path is going somewhere it need not (a proxy that should be
 * bypassed, or one that should not be), or we are asking for too few streams
 * at once to fill a link that could carry far more. Watching a download tells
 * you which of the three it is NOT: nothing. The number is the same in all
 * three cases.
 *
 * The Coordinator has been able to answer this since internal/calibrate landed
 * - it sweeps concurrency levels against both registries, finds the knee, tests
 * the proxy against a direct route, and returns configuration keys with the
 * measurement behind each one. Nothing in this interface reached it, so the
 * answer existed only for somebody with a shell and the CLI.
 *
 * # Why it is a button and never automatic
 *
 * It moves real data. The read probe pulls blobs from the source; the write
 * probe opens upload sessions on the target, streams bytes into them and
 * cancels them - nothing is committed anywhere, and both registries see
 * genuine load for minutes. That is not something to do because a page
 * mounted.
 *
 * # Why the result is not kept
 *
 * A calibration is a measurement of a network as it was on Tuesday. Storing
 * one would manufacture exactly the stale number this replaces: the question
 * is whether the speed on screen right now is the best this path can do, and
 * that has to be measured now.
 */

const SEVERITY_TONE = {
  critical: 'danger',
  recommended: 'review',
  info: 'neutral',
} as const

/** How long one concurrency level runs, and therefore how long the whole run is. */
const BUDGET_CHOICES = [
  { value: 5, label: 'Quick (about a minute)' },
  { value: 12, label: 'Standard (a few minutes)' },
  { value: 25, label: 'Thorough (five minutes or so)' },
]

export function SpeedTest({ product: fixedProduct }: {
  /** Scopes the panel to one product, for a page that is already about one. */
  product?: string
}) {
  const products = useProducts()
  const enabled = (products.data?.products ?? []).filter((p) => p.enabled)
  const [chosen, setChosen] = useState<string | undefined>(fixedProduct)
  const product = fixedProduct ?? chosen ?? enabled[0]?.productId
  const [budget, setBudget] = useState(12)
  const [write, setWrite] = useState(true)

  const calibrate = useCalibrate(product)
  const report = calibrate.data

  return (
    <Card
      title="Path speed test"
      extra={
        <Space size={8} wrap>
          {!fixedProduct && (
            <Select
              size="small"
              style={{ minWidth: 160 }}
              value={product}
              onChange={setChosen}
              placeholder="Product"
              options={enabled.map((p) => ({
                value: p.productId, label: p.displayName || p.productId,
              }))}
            />
          )}
          <Select
            size="small"
            style={{ minWidth: 200 }}
            value={budget}
            onChange={setBudget}
            options={BUDGET_CHOICES}
          />
          <Tooltip title="The write probe opens upload sessions on the target, streams bytes into them and cancels them. Nothing is committed. Turning it off measures only the reading half, which is the wrong half if the destination is where the path is slow.">
            <Select
              size="small"
              style={{ minWidth: 170 }}
              value={write ? 'both' : 'read'}
              onChange={(v) => setWrite(v === 'both')}
              options={[
                { value: 'both', label: 'Read and write' },
                { value: 'read', label: 'Read only' },
              ]}
            />
          </Tooltip>
          <Button
            type="primary"
            icon={<ThunderboltOutlined />}
            loading={calibrate.isPending}
            disabled={!product}
            onClick={() => calibrate.mutate({ budgetSeconds: budget, write })}
          >
            {calibrate.isPending ? 'Measuring…' : 'Measure this path'}
          </Button>
        </Space>
      }
    >
      <Space direction="vertical" size={12} style={{ width: '100%' }}>
        <Typography.Text type="secondary">
          Sweeps how many streams at once each registry will actually carry, and reports the
          point past which more streams stop helping. It moves real data in both directions -
          nothing is committed anywhere - and it takes minutes, so it runs when asked and never
          on its own.
        </Typography.Text>

        {calibrate.isPending && (
          <Alert
            type="info"
            showIcon
            message="Measuring"
            description={
              <Typography.Text type="secondary">
                Each concurrency level runs for {budget} seconds against each end of the path.
                Leaving this page abandons the run - the probes stop and nothing is left behind.
              </Typography.Text>
            }
          />
        )}

        {calibrate.isError && (
          <Alert
            type="error"
            showIcon
            message="The path could not be measured"
            description={
              <Space direction="vertical" size={4}>
                <Typography.Text>
                  {calibrate.error instanceof Error
                    ? calibrate.error.message
                    : 'The Coordinator did not answer.'}
                </Typography.Text>
                <Typography.Text type="secondary">
                  A run that cannot start is usually a source or target this product does not
                  declare, or credentials that do not reach one of them. Nothing was changed.
                </Typography.Text>
              </Space>
            }
          />
        )}

        {report && (
          <>
            <Alert
              type="info"
              showIcon
              message={`Measured from ${report.measuredFrom}`}
              description={
                <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                  {/*
                    LOAD-BEARING, not a footnote. The probes ran on the
                    Coordinator, and the bytes of a real download are moved by
                    WORKERS. Where the two are not on the same network, this
                    describes a path no download takes - and somebody acting on
                    it would be tuning the wrong link.
                  */}
                  These probes ran on the Coordinator. Downloads are performed by workers, so
                  this describes what a download would do only where the two share a network.
                  The run took {formatDuration(report.durationSeconds) ?? 'a moment'}.
                </Typography.Text>
              }
            />

            <Suggestions suggestions={report.suggestions} />

            <SidePanel side={report.source} />
            <SidePanel side={report.target} />

            {(report.notes?.length ?? 0) > 0 && (
              <Space direction="vertical" size={2}>
                {report.notes?.map((note) => (
                  <Typography.Text key={note} type="secondary" style={{ fontSize: 12 }}>
                    {note}
                  </Typography.Text>
                ))}
              </Space>
            )}
          </>
        )}
      </Space>
    </Card>
  )
}

/**
 * What to change, and the measurement each one rests on.
 *
 * The evidence column is not decoration. Advice without a number is the
 * guesswork this feature replaces, and an operator who finds one suggestion
 * unfounded stops reading the rest - so every row carries what it was derived
 * from, and a run with nothing to suggest says so rather than showing an empty
 * table.
 */
function Suggestions({ suggestions }: { suggestions: CalibrationSuggestion[] }) {
  if (suggestions.length === 0) {
    return (
      <Alert
        type="success"
        showIcon
        message="Nothing to change"
        description={
          <Typography.Text type="secondary">
            The configured concurrency is at or near what this path will carry, and no route
            change was measured to help. The speed you are seeing is what this path does.
          </Typography.Text>
        }
      />
    )
  }

  return (
    <Table<CalibrationSuggestion>
      size="small"
      pagination={false}
      dataSource={suggestions}
      rowKey={(s, i) => `${s.setting ?? 'finding'}-${s.scope ?? ''}-${i}`}
      scroll={{ x: 820 }}
      columns={[
        {
          title: 'Severity',
          width: 120,
          render: (_, s) => (
            <StatusPill
              tone={SEVERITY_TONE[s.severity as keyof typeof SEVERITY_TONE] ?? 'neutral'}
              dot={false}
              style={{ marginInlineEnd: 0 }}
            >
              {s.severity}
            </StatusPill>
          ),
        },
        {
          title: 'Setting',
          width: 220,
          render: (_, s) => (s.setting
            ? (
                <Space direction="vertical" size={0}>
                  <Typography.Text style={{ fontFamily: mono, fontSize: 12 }}>
                    {s.setting}
                  </Typography.Text>
                  <Typography.Text type="secondary" style={{ fontSize: 11 }}>{s.scope}</Typography.Text>
                </Space>
              )
            // A finding with no knob behind it is still worth reporting - it is
            // usually the reason a knob would not help.
            : <NA reason="A measured finding with no setting behind it." />),
        },
        {
          title: 'Now',
          width: 170,
          render: (_, s) => <Value>{s.current}</Value>,
        },
        {
          title: 'Change to',
          width: 130,
          render: (_, s) => (
            s.suggested
              ? <Tag color={s.severity === 'critical' ? 'red' : undefined}
                     style={{ marginInlineEnd: 0, fontFamily: mono }}>{s.suggested}</Tag>
              : <NA />
          ),
        },
        { title: 'Because', render: (_, s) => s.evidence },
      ]}
    />
  )
}

/** One end of the path, and what it actually carried. */
function SidePanel({ side }: { side: CalibrationSide }) {
  const levels = side.levels ?? []
  const best = levels.reduce<CalibrationLevel | undefined>(
    (top, l) => (!top || l.rateBytesPerSecond > top.rateBytesPerSecond ? l : top), undefined)

  return (
    <Card
      size="small"
      type="inner"
      title={
        <Space size={8}>
          <Typography.Text strong style={{ textTransform: 'capitalize' }}>{side.role}</Typography.Text>
          <Typography.Text type="secondary">{side.name}</Typography.Text>
        </Space>
      }
      extra={
        best && (
          <Typography.Text strong style={{ color: c.brand }}>
            {formatSpeed(best.rateBytesPerSecond)} at {best.concurrency} stream
            {best.concurrency === 1 ? '' : 's'}
          </Typography.Text>
        )
      }
    >
      {side.skipped ? (
        <Alert type="info" showIcon message="Not measured" description={side.skipped} />
      ) : (
        <Space direction="vertical" size={10} style={{ width: '100%' }}>
          <Descriptions column={{ xs: 1, sm: 2, lg: 4 }} size="small">
            <Descriptions.Item label="Registry">
              <Typography.Text style={{ fontFamily: mono, fontSize: 12 }}>
                <Value>{side.registry}</Value>
              </Typography.Text>
            </Descriptions.Item>
            <Descriptions.Item label="Round trip">
              <Value>{side.rttMs ? `${side.rttMs.toFixed(0)} ms` : null}</Value>
            </Descriptions.Item>
            <Descriptions.Item label="Best concurrency">
              <Tooltip title="The smallest number of streams within a tenth of the fastest measured. Past this, more streams cost connections and buy nothing.">
                <span><Value>{side.knee ? formatCount(side.knee) : null}</Value></span>
              </Tooltip>
            </Descriptions.Item>
            <Descriptions.Item label="Measured over">
              <Tooltip title="What the read probe opened. A throughput measured over signature blobs is not a throughput over layers, and this is how you tell.">
                <span>
                  <Value>
                    {side.samples
                      ? `${formatCount(side.samples)} blobs, largest ${formatBytes(side.largestSampleBytes) ?? '?'}`
                      : null}
                  </Value>
                </span>
              </Tooltip>
            </Descriptions.Item>
          </Descriptions>

          {side.route.proxyInUse && <RouteNote side={side} />}

          {side.stillClimbing && (
            <Alert
              type="warning"
              showIcon
              message="The sweep ended before the path did"
              description={
                <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                  Throughput was still rising at the highest concurrency tried, so this path
                  will carry more than was measured. The ceiling is above this number, not at it.
                </Typography.Text>
              }
            />
          )}

          <Table<CalibrationLevel>
            size="small"
            pagination={false}
            dataSource={levels}
            rowKey={(l) => String(l.concurrency)}
            scroll={{ x: 700 }}
            columns={[
              {
                title: 'Streams',
                width: 100,
                align: 'right',
                render: (_, l) => (
                  <Space size={6}>
                    <Value>{formatCount(l.concurrency)}</Value>
                    {l.concurrency === side.knee && <Tag color="blue" style={{ marginInlineEnd: 0 }}>knee</Tag>}
                  </Space>
                ),
              },
              {
                title: 'Total rate',
                width: 130,
                align: 'right',
                render: (_, l) => <Value>{formatSpeed(l.rateBytesPerSecond)}</Value>,
              },
              {
                title: 'Per stream',
                width: 130,
                align: 'right',
                render: (_, l) => (
                  <Tooltip title="Total rate divided by streams. When this falls as streams rise, the streams are competing rather than adding.">
                    <span><Value>{formatSpeed(l.perStreamBytesPerSecond)}</Value></span>
                  </Tooltip>
                ),
              },
              {
                title: 'First byte',
                width: 110,
                align: 'right',
                render: (_, l) => <Value>{l.ttfbMs ? `${l.ttfbMs.toFixed(0)} ms` : null}</Value>,
              },
              { title: 'Requests', width: 100, align: 'right', render: (_, l) => <Value>{formatCount(l.requests)}</Value> },
              {
                title: 'Refused',
                width: 190,
                render: (_, l) => {
                  if (!l.errors && !l.throttled) return <NA reason="Every request was served." />
                  return (
                    <Tooltip title={l.firstError || undefined}>
                      <Space size={6}>
                        {Boolean(l.throttled) && (
                          <Tag color="orange" style={{ marginInlineEnd: 0 }}>
                            {formatCount(l.throttled)} throttled
                          </Tag>
                        )}
                        {Boolean(l.errors) && (
                          <Tag color="red" style={{ marginInlineEnd: 0 }}>
                            {formatCount(l.errors)} failed
                          </Tag>
                        )}
                      </Space>
                    </Tooltip>
                  )
                },
              },
            ]}
          />
        </Space>
      )}
    </Card>
  )
}

/**
 * The proxy, and what the traffic would do without it.
 *
 * The single most valuable thing this test produces, and the one an operator
 * cannot establish any other way: a corporate proxy that halves throughput and
 * a corporate proxy that is the only route out look identical from a download.
 */
function RouteNote({ side }: { side: CalibrationSide }) {
  const r = side.route
  const faster = r.directRateBytesPerSecond && r.proxiedRateBytesPerSecond
    && r.directRateBytesPerSecond > r.proxiedRateBytesPerSecond * 1.15

  return (
    <Alert
      type={faster ? 'warning' : 'info'}
      showIcon
      message={faster ? 'A direct route measured faster than the proxy' : 'Traffic goes through a proxy'}
      description={
        <Space direction="vertical" size={2}>
          <Typography.Text style={{ fontFamily: mono, fontSize: 12 }}>{r.configured}</Typography.Text>
          {r.directTested && !r.directReachable && (
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              A direct connection was tried and failed, so the proxy is the only route out
              of here: {r.directDetail}
            </Typography.Text>
          )}
          {Boolean(r.proxiedRateBytesPerSecond) && (
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              Through the proxy {formatSpeed(r.proxiedRateBytesPerSecond)}
              {r.directReachable && `, direct ${formatSpeed(r.directRateBytesPerSecond)}`}.
            </Typography.Text>
          )}
        </Space>
      }
    />
  )
}
