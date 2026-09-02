import { Button, Card, Col, Progress, Row, Space, Tag, Tooltip, Typography } from 'antd'
import { LoadingOutlined } from '../icons'
import { formatBytes, formatCount, formatDuration } from '../domain/format'
import { c, mono } from '../uikit'
import type {
  ComplianceProgress, ComplianceProgressEvent, ComplianceStage,
} from '../api/types'

/**
 * A compliance run, while it is running.
 *
 * # What this replaces, and why it was not enough
 *
 * A bar, a stage name, and "0 of 95". For four to fifteen minutes - which is
 * what a real orb costs: ninety-five charts pulled out of a vendor registry,
 * each rendered twice, seventy-three checks against several thousand objects -
 * that is indistinguishable from a hang. The question somebody asks in front of
 * one is not "how far along is it". It is "is this working at all", and a
 * percentage that has not moved is not an answer to it.
 *
 * # What answers it
 *
 * Things that CHANGE, and things that have HAPPENED. The elapsed clock ticks.
 * The names of the charts in flight rotate. The counts climb, and they are
 * counts of real objects - charts fetched, megabytes read, objects rendered.
 * The log fills in with what each chart did. Any one of those moving says the
 * run is alive; all of them still says it is stuck, which is also worth
 * knowing and used to be unknowable.
 *
 * The estimate is the current STAGE's, and says so. A whole-run estimate would
 * have to guess the cost of stages that have not started, whose cost depends on
 * what the ones running now produce - and a confident number that turns out
 * four times wrong is worse than no number.
 */
export function ComplianceRunPanel({ progress, onCancel, cancelling }: {
  progress: ComplianceProgress
  onCancel?: () => void
  cancelling?: boolean
}) {
  return (
    <Card
      title={
        <Space size={10}>
          <LoadingOutlined spin style={{ color: c.brand }} />
          <span>Checking this release</span>
          <Typography.Text type="secondary" style={{ fontSize: 12, fontWeight: 400 }}>
            {formatDuration(progress.elapsed) ?? '0s'} elapsed
          </Typography.Text>
        </Space>
      }
      extra={<StopCheck onCancel={onCancel} cancelling={cancelling} />}
    >
      <Space direction="vertical" size={16} style={{ width: '100%' }}>
        <StageBar progress={progress} />
        <StageRoute progress={progress} />
        <RunCounts progress={progress} />
        <EventLog events={progress.events ?? []} />
      </Space>
    </Card>
  )
}

/**
 * The stop, in the shape every other long job in this interface uses.
 *
 * It was a text link, which read as navigation on a card whose every other
 * control is a button - and it was the one control on the page that could
 * abandon minutes of work against somebody's registry. A danger button, with
 * the sentence saying what stopping actually promises, is what the analysis bar
 * on the Details tab does, and there is no reason for these two to differ.
 */
function StopCheck({ onCancel, cancelling }: { onCancel?: () => void; cancelling?: boolean }) {
  if (!onCancel) return null
  return (
    <Tooltip
      title={
        'Frees the release for another attempt straight away. Nothing is recorded: the run '
        + 'ends as cancelled rather than as a verdict, because a partial check is not a result.'
      }
    >
      <Button size="small" danger loading={cancelling} onClick={onCancel}>
        Stop check
      </Button>
    </Tooltip>
  )
}

/** The current stage: what it is doing, how far, and how much longer. */
function StageBar({ progress }: { progress: ComplianceProgress }) {
  const percent = progress.total > 0
    ? Math.min(100, Math.round((progress.done / progress.total) * 100))
    : 0
  const active = progress.active ?? []

  return (
    <Space direction="vertical" size={6} style={{ width: '100%' }}>
      <Space style={{ width: '100%', justifyContent: 'space-between' }} align="start" wrap>
        <Space direction="vertical" size={0}>
          <Typography.Text strong>{progress.label}</Typography.Text>
          {progress.detail && (
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              {progress.detail}
            </Typography.Text>
          )}
        </Space>
        <Space direction="vertical" size={0} align="end">
          <Typography.Text style={{ fontFamily: mono, fontSize: 13 }}>
            {progress.total > 0
              ? `${formatCount(progress.done)} of ${formatCount(progress.total)}`
              : 'starting'}
          </Typography.Text>
          {/*
            OF THIS STAGE, said explicitly. An estimate that looks like it
            covers the whole run and turns out to cover a fifth of it is worse
            than no estimate: somebody leaves, comes back, and trusts nothing
            the page says afterwards.
          */}
          {progress.estimate ? (
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              about {formatDuration(progress.estimate)} left on this stage
            </Typography.Text>
          ) : null}
        </Space>
      </Space>

      <Progress
        percent={percent}
        status="active"
        showInfo={false}
        strokeColor={c.brand}
        trailColor={c.track}
      />

      {/*
        WHAT IS IN FLIGHT RIGHT NOW. The single most useful thing on this panel:
        a list that changes every few seconds is a run that is working,
        whatever the bar is doing, and it is the only thing here that
        distinguishes a slow registry from a wedged one.
      */}
      {(active.length > 0 || (progress.concurrency ?? 0) > 0) && (
        <Space size={6} wrap style={{ rowGap: 4 }}>
          {(progress.concurrency ?? 0) > 0 && (
            <Tooltip
              title={
                'Charts handled at once. Downloading is bounded to be polite to the '
                + "vendor's registry; rendering is bounded by this machine's cores, because "
                + 'helm template is CPU-bound.'
              }
            >
              <Tag style={{ margin: 0, fontSize: 11 }}>
                {progress.concurrency} at a time
              </Tag>
            </Tooltip>
          )}
          {active.slice(0, 6).map((a) => (
            <Tag key={a} style={{ margin: 0, fontFamily: mono, fontSize: 11 }}>
              {shortName(a)}
            </Tag>
          ))}
          {active.length > 6 && (
            <Typography.Text type="secondary" style={{ fontSize: 11 }}>
              +{active.length - 6} more
            </Typography.Text>
          )}
        </Space>
      )}
    </Space>
  )
}

const STAGE_ORDER: ComplianceStage[] = [
  'resolving', 'fetching', 'rendering', 'evaluating', 'recording',
]

/**
 * The whole route, with what each finished stage cost.
 *
 * On screen because "eight minutes" is unreadable and "six of those eight were
 * the download" is a decision: it says the wait is the vendor's registry rather
 * than this Coordinator, and it is what somebody quotes when they ask for the
 * concurrency to be raised.
 */
function StageRoute({ progress }: { progress: ComplianceProgress }) {
  const done = new Map((progress.completed ?? []).map((s) => [s.stage, s.seconds]))
  const at = STAGE_ORDER.indexOf(progress.stage)

  return (
    <Space size={0} wrap style={{ rowGap: 6 }}>
      {STAGE_ORDER.map((stage, i) => {
        const seconds = done.get(stage)
        const isDone = seconds !== undefined
        const isNow = i === at
        return (
          <span key={stage} style={{ display: 'inline-flex', alignItems: 'center' }}>
            {i > 0 && (
              <span style={{ color: c.text3, padding: '0 8px', fontSize: 11 }}>›</span>
            )}
            <span
              style={{
                fontSize: 12,
                fontWeight: isNow ? 600 : 400,
                color: isNow ? c.text : isDone ? c.text2 : c.text3,
              }}
            >
              {labelOf(stage)}
              {isDone && (
                <span style={{ color: c.text3, fontFamily: mono, fontSize: 11 }}>
                  {' '}{formatDuration(seconds) ?? '0s'}
                </span>
              )}
            </span>
          </span>
        )
      })}
    </Space>
  )
}

function labelOf(stage: ComplianceStage): string {
  switch (stage) {
    case 'resolving': return 'Find charts'
    case 'fetching': return 'Download'
    case 'rendering': return 'Render'
    case 'evaluating': return 'Evaluate'
    case 'recording': return 'Record'
  }
}

/**
 * What the run has got through, as counts of real things.
 *
 * Refusals and failures are here beside the successes, and coloured, because
 * they are the numbers that change what the answer means: a run that has
 * fetched ninety-two of ninety-five charts is about to produce a report with a
 * hole in it, and knowing that at minute two rather than at minute nine is the
 * difference between fixing the cause and reading a verdict nobody can use.
 */
function RunCounts({ progress }: { progress: ComplianceProgress }) {
  const k = progress.counts
  const tiles: { label: string; value: string; tone?: string; hint?: string }[] = []

  if (k.chartsFound > 0) {
    tiles.push({
      label: 'Charts found', value: formatCount(k.chartsFound) ?? '0',
      hint: 'Artifacts in this release classified as Helm charts',
    })
  }
  if (k.chartsFetched > 0 || progress.stage === 'fetching') {
    tiles.push({
      label: 'Downloaded', value: formatCount(k.chartsFetched) ?? '0',
      hint: k.bytes > 0 ? `${formatBytes(k.bytes)} read from the vendor registry` : undefined,
    })
  }
  if (k.chartsRendered > 0 || progress.stage === 'rendering') {
    tiles.push({
      label: 'Rendered', value: formatCount(k.chartsRendered) ?? '0',
      hint: 'Charts helm turned into Kubernetes objects',
    })
  }
  if (k.objects > 0) {
    tiles.push({
      label: 'Objects', value: formatCount(k.objects) ?? '0',
      hint: k.checks > 0 ? `to be judged against ${k.checks} checks` : undefined,
    })
  }
  if (k.chartsSkipped > 0) {
    tiles.push({
      label: 'Not fetched', value: formatCount(k.chartsSkipped) ?? '0', tone: c.pending,
      hint: 'A budget, a truncation, or a registry that would not answer. Each one is '
        + 'recorded on the run: a chart nobody checked is not a chart that passed.',
    })
  }
  if (k.chartsFailed > 0) {
    tiles.push({
      label: 'Would not render', value: formatCount(k.chartsFailed) ?? '0', tone: c.danger,
      hint: 'Every check that needed one of these will report as undecided, and the run '
        + 'will be inconclusive rather than a pass.',
    })
  }
  if (tiles.length === 0) return null

  return (
    <Row gutter={[12, 12]}>
      {tiles.map((t) => (
        <Col key={t.label} xs={12} sm={8} lg={4}>
          <Tooltip title={t.hint}>
            <div
              style={{
                border: `1px solid ${c.border}`,
                borderRadius: 6,
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
        </Col>
      ))}
    </Row>
  )
}

/**
 * What has happened, newest first.
 *
 * # Why a log and not a spinner
 *
 * A spinner is a claim that something is happening; a log is the evidence. Each
 * line names a chart and what became of it, so the panel answers the two
 * questions a spinner cannot: what is this actually doing, and - when a chart
 * refuses to render nine minutes in - which one, and why, without waiting for
 * the run to end to find out.
 *
 * Newest first because the interesting line is the one that just arrived. The
 * server keeps a bounded ring and drops ordinary progress before it drops a
 * failure, so the lines that survive a long run are the ones worth scrolling to.
 */
function EventLog({ events }: { events: ComplianceProgressEvent[] }) {
  if (events.length === 0) return null
  const newest = [...events].reverse()

  return (
    <div>
      <Typography.Text type="secondary" style={{ fontSize: 11 }}>
        What has happened
      </Typography.Text>
      <div
        style={{
          marginTop: 4,
          maxHeight: 190,
          overflowY: 'auto',
          border: `1px solid ${c.border}`,
          borderRadius: 6,
          background: c.surface2,
        }}
      >
        {newest.map((e, i) => (
          <div
            key={`${e.sec}-${i}`}
            style={{
              display: 'flex',
              gap: 10,
              padding: '3px 10px',
              fontSize: 12,
              lineHeight: '18px',
              borderTop: i === 0 ? undefined : `1px solid ${c.border}`,
            }}
          >
            <span
              style={{
                fontFamily: mono, fontSize: 11, color: c.text3,
                minWidth: '4.5ch', textAlign: 'right', flexShrink: 0,
              }}
            >
              {formatDuration(e.sec) ?? '0s'}
            </span>
            <span style={{ color: toneOf(e.kind), wordBreak: 'break-word' }}>{e.text}</span>
          </div>
        ))}
      </div>
    </div>
  )
}

function toneOf(kind: ComplianceProgressEvent['kind']): string {
  switch (kind) {
    case 'fail': return c.danger
    case 'warn': return c.pending
    case 'ok': return c.text
    default: return c.text2
  }
}

/**
 * A chart's name without its repository path.
 *
 * The in-flight tags are read at a glance while they change, and
 * `orbs/cfx-5000-k8s/cfx-network:orb_24.7.3099` is not read at a glance. The
 * whole reference is on the coverage table when the run is over, where there is
 * room for it and time to read it.
 */
function shortName(ref: string): string {
  const tail = ref.split('/').pop() ?? ref
  return tail.split(':')[0] || tail
}
