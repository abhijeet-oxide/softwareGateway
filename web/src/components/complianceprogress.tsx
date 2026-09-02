import { useState } from 'react'
import {
  Alert, Button, Card, Drawer, Progress, Space, Tag, Timeline, Tooltip, Typography,
} from 'antd'
import { CopyOutlined, FileTextOutlined, HelmOutlined, LoadingOutlined } from '../icons'
import { formatBytes, formatCount, formatDuration, formatRelative } from '../domain/format'
import { RunTiles } from './runtiles'
import type { RunTile } from './runtiles'
import { c, mono } from '../uikit'
import type {
  ComplianceProgress, ComplianceProgressEvent, ComplianceRun, ComplianceStage,
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
 * Things that CHANGE, and a record of what the run has done. The elapsed clock ticks.
 * The names of the charts in flight rotate. The counts climb, and they are
 * counts of real objects - charts fetched, megabytes read, objects rendered.
 * The run log fills in with what each chart produced. Any one of those moving says the
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
          <span>Compliance check in progress</span>
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
        'Releases the claim immediately, so the release can be checked again. No verdict '
        + 'is recorded: the run ends as cancelled, because a partial check is not a result.'
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
              : 'Starting'}
          </Typography.Text>
          {/*
            OF THIS STAGE, said explicitly. An estimate that looks like it
            covers the whole run and turns out to cover a fifth of it is worse
            than no estimate: somebody leaves, comes back, and trusts nothing
            the page says afterwards.
          */}
          {/*
            A WHOLE SECOND or nothing. An estimate that rounds to zero renders
            as "Estimated 0s remaining in this stage", which is a number that
            says the stage is over while the bar is still moving - and on a warm
            render cache, where a stage really does take under a second, that is
            what it said every time.
          */}
          {progress.estimate && progress.estimate >= 1 ? (
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              Estimated {formatDuration(progress.estimate)} remaining in this stage
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
                'Charts processed concurrently. The download limit is set for the vendor '
                + "registry's benefit; the render limit is set by this Coordinator's CPU "
                + 'count, because helm template is CPU-bound.'
              }
            >
              <Tag style={{ margin: 0, fontSize: 11 }}>
                {progress.concurrency} charts in parallel
              </Tag>
            </Tooltip>
          )}
          {/*
            The mark, because these are charts. The row beside them carries a
            parallelism count and a stage name, and at eleven pixels a bare
            string of hyphenated words is not obviously the name of a thing
            being fetched rather than another label.
          */}
          {active.slice(0, 6).map((a) => (
            <Tag key={a} icon={<HelmOutlined />} style={{ margin: 0, fontFamily: mono, fontSize: 11 }}>
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
    case 'resolving': return 'Discover'
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
  const tiles: RunTile[] = []

  if (k.chartsFound > 0) {
    tiles.push({
      label: 'Charts discovered', value: formatCount(k.chartsFound) ?? '0',
      hint: 'Artifacts in this release classified as Helm charts',
    })
  }
  if (k.chartsReused > 0) {
    tiles.push({
      label: 'Charts reused', value: formatCount(k.chartsReused) ?? '0', tone: c.ok,
      hint: 'Served from the render cache: identical chart bytes were rendered before '
        + 'under identical inputs, so these were neither downloaded nor rendered again.',
    })
  }
  if (k.chartsFetched > 0 || progress.stage === 'fetching') {
    tiles.push({
      label: 'Charts downloaded', value: formatCount(k.chartsFetched) ?? '0',
      hint: k.bytes > 0 ? `${formatBytes(k.bytes)} read from the vendor registry` : undefined,
    })
  }
  if (k.chartsRendered > 0 || progress.stage === 'rendering') {
    tiles.push({
      label: 'Charts rendered', value: formatCount(k.chartsRendered) ?? '0',
      hint: 'Charts helm converted into Kubernetes objects',
    })
  }
  if (k.objects > 0) {
    tiles.push({
      label: 'Objects rendered', value: formatCount(k.objects) ?? '0',
      hint: k.checks > 0 ? `Evaluated against ${k.checks} checks` : undefined,
    })
  }
  if (k.chartsSkipped > 0) {
    tiles.push({
      label: 'Downloads skipped', value: formatCount(k.chartsSkipped) ?? '0', tone: c.pending,
      hint: 'Refused by a budget, or the registry did not respond. Each is recorded on '
        + 'the run: an unchecked chart is not a chart that passed.',
    })
  }
  if (k.chartsFailed > 0) {
    tiles.push({
      label: 'Renders failed', value: formatCount(k.chartsFailed) ?? '0', tone: c.danger,
      hint: 'Every check requiring one of these reports as unchecked, and the run is '
        + 'inconclusive rather than a pass.',
    })
  }
  return <RunTiles tiles={tiles} />
}

/**
 * The run log: what the run has done, newest first.
 *
 * # Why a log and not a spinner
 *
 * A spinner is a claim that something is happening; a log is the record. Each
 * line names a chart and what became of it, so the panel answers the two
 * questions a spinner cannot: what is this actually doing, and - when a chart
 * refuses to render nine minutes in - which one, and why, without waiting for
 * the run to end to find out.
 */
function EventLog({ events }: { events: ComplianceProgressEvent[] }) {
  if (events.length === 0) return null
  return (
    <div>
      <Typography.Text type="secondary" style={{ fontSize: 11 }}>
        Run log
      </Typography.Text>
      <div style={{ marginTop: 8, maxHeight: 260, overflowY: 'auto', paddingRight: 8 }}>
        <ComplianceRunLog events={events} />
      </div>
    </div>
  )
}

/**
 * The transcript itself, in the shape the vulnerability sync log uses.
 *
 * # Why this is its own component
 *
 * Because it is read twice: under the bar while the check runs, and out of the
 * finished run's record afterwards. The second is the one that was missing -
 * the log lived in the Coordinator's memory, so the moment the check ended the
 * only account of what it had done disappeared, and "which charts refused, and
 * what did the nine minutes go on" is a question people ask afterwards.
 *
 * A TIMELINE, because a run IS one: a sequence of things that happened, in
 * order, with gaps between them that mean something. This is the shape the
 * vulnerability sync log uses and a reader has already learned it there - a
 * second transcript style for the same kind of information is a second thing to
 * learn for no gain.
 *
 * Newest first while the run is live, because the interesting line is the one
 * that just arrived; oldest first when the run is over, because a finished
 * transcript is read as a sequence.
 */
export function ComplianceRunLog({ events, newestFirst = true }: {
  events: ComplianceProgressEvent[]
  newestFirst?: boolean
}) {
  const ordered = newestFirst ? [...events].reverse() : events
  return (
    <Timeline
      mode="left"
      items={ordered.map((e, i) => ({
        key: `${e.sec}-${i}`,
        color: toneOf(e.kind),
        children: (
          <Space size={10} align="start" style={{ lineHeight: 1.35 }}>
            {/*
              The elapsed second, not a clock time. A run is minutes long and
              every line is about how far into it something happened; wall-clock
              times would need subtracting before they said anything. The
              absolute time is on hover, for a log read a week later.
            */}
            <Typography.Text
              type="secondary"
              style={{
                fontFamily: mono, fontSize: 11, whiteSpace: 'nowrap',
                minWidth: '4.5ch', display: 'inline-block', textAlign: 'right',
              }}
              title={e.at ? new Date(e.at).toLocaleString() : undefined}
            >
              {formatDuration(e.sec) ?? '0s'}
            </Typography.Text>
            {/*
              The kind is a WORD as well as a colour, on the lines where it
              changes what the line means. Everything on this page reads
              correctly in greyscale, and a transcript whose only signal is
              the colour of a six-pixel dot is the easiest place to forget
              that.
            */}
            {(e.kind === 'fail' || e.kind === 'warn') && (
              <Tag
                color={e.kind === 'fail' ? 'red' : 'orange'}
                style={{ margin: 0, fontSize: 10, lineHeight: '16px' }}
              >
                {e.kind === 'fail' ? 'Failed' : 'Warning'}
              </Tag>
            )}
            <span style={{ fontSize: 12, color: e.kind === 'fail' ? c.danger : c.text }}>
              {e.text}
            </span>
          </Space>
        ),
      }))}
    />
  )
}

/**
 * The finished run's transcript, behind a button.
 *
 * The counterpart of the vulnerability sync's Sync log, in the same place on
 * the same kind of card, because these two features are read by the same people
 * on the same release and there is no reason for one of them to keep its record
 * and the other to throw it away.
 *
 * Absent rather than disabled when a run predates the stored log: a control
 * that cannot do anything is a question the reader has to answer before
 * ignoring it.
 */
export function ComplianceRunLogButton({ run, size = 'small' }: {
  run: ComplianceRun
  size?: 'small' | 'middle'
}) {
  const [open, setOpen] = useState(false)
  const events = run.log ?? []
  if (events.length === 0) return null

  const plain = events
    .map((e) => `${formatDuration(e.sec) ?? '0s'} [${e.kind}] ${e.text}`)
    .join('\n')

  return (
    <>
      <Button size={size} icon={<FileTextOutlined />} onClick={() => setOpen(true)}>
        Run log
      </Button>
      <Drawer
        title="Compliance run log"
        width={720}
        open={open}
        onClose={() => setOpen(false)}
        extra={
          <Button
            size="small"
            icon={<CopyOutlined />}
            onClick={() => void navigator.clipboard?.writeText(plain)}
          >
            Copy
          </Button>
        }
      >
        <Space direction="vertical" size={16} style={{ width: '100%' }}>
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            {run.finishedAt
              ? `From the check that finished ${formatRelative(run.finishedAt)}.`
              : 'From the last check.'}
            {' '}Times are seconds from the start of the run.
          </Typography.Text>
          {run.logTruncated && (
            <Alert
              type="info"
              showIcon
              message={`This log holds the last ${events.length} lines of the run`}
              description={
                'A run keeps a bounded transcript and drops routine progress before it drops a '
                + 'failure, so every chart that refused is here and some of the ones that '
                + 'rendered are not. The coverage table is the complete list.'
              }
            />
          )}
          <ComplianceRunLog events={events} newestFirst={false} />
        </Space>
      </Drawer>
    </>
  )
}

/**
 * What colour a line's dot is.
 *
 * # Why these are the four the sync log uses
 *
 * They were not. `ok` was the body text colour and `info` was the muted one, so
 * on a run whose lines are almost all info and ok - which is every run that
 * works - the timeline was a column of grey dots, and the two colours that
 * meant something were lost among fifty that meant nothing. A dot the same
 * colour as the text beside it is not a signal.
 */
function toneOf(kind: ComplianceProgressEvent['kind']): string {
  switch (kind) {
    case 'fail': return c.danger
    case 'warn': return c.pending
    case 'ok': return c.ok
    default: return c.brand
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
