import { HelmOutlined } from '../icons'
import { formatBytes, formatCount, formatRelative } from '../domain/format'
import { RunCard, RunLog, RunLogButton } from './runpanel'
import type { RunTile } from './runtiles'
import { c } from '../uikit'
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
  const done = new Map((progress.completed ?? []).map((s) => [s.stage, s.seconds]))
  const at = STAGE_ORDER.indexOf(progress.stage)

  return (
    <RunCard
      title="Compliance check in progress"
      elapsed={progress.elapsed}
      startedAt={progress.started}
      onStop={onCancel}
      stopping={cancelling}
      stopLabel="Stop check"
      stopHint={
        'Releases the claim immediately, so the release can be checked again. No verdict '
        + 'is recorded: the run ends as cancelled, because a partial check is not a result.'
      }
      label={progress.label}
      detail={progress.detail}
      done={progress.done}
      total={progress.total}
      estimate={progress.estimate}
      indeterminate={progress.total === 0}
      concurrency={progress.concurrency}
      concurrencyLabel="charts in parallel"
      concurrencyHint={
        'Charts processed concurrently. The download limit is set for the vendor '
        + "registry's benefit; the render limit is set by this Coordinator's CPU "
        + 'count, because helm template is CPU-bound.'
      }
      active={(progress.active ?? []).map((a) => ({
        key: a,
        label: shortName(a),
        icon: <HelmOutlined />,
      }))}
      steps={STAGE_ORDER.map((stage, i) => ({
        key: stage,
        label: labelOf(stage),
        seconds: done.get(stage),
        state: i === at ? 'current' : done.has(stage) ? 'done' : 'pending',
      }))}
      tiles={tilesFor(progress)}
      events={(progress.events ?? []).map((e) => ({
        at: e.at, sec: e.sec, kind: e.kind, text: e.text,
      }))}
    />
  )
}

const STAGE_ORDER: ComplianceStage[] = [
  'resolving', 'fetching', 'rendering', 'evaluating', 'recording',
]

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
function tilesFor(progress: ComplianceProgress): RunTile[] {
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
  return tiles
}

/**
 * The transcript, in the shape every other long job here uses.
 *
 * Kept as a named export because it is read twice - under the bar while the
 * check runs, and out of the finished run's record afterwards - and both
 * callers name it. The drawing is RunLog's; the mapping from a compliance
 * event to a run event is the only thing that is compliance's.
 */
export function ComplianceRunLog({ events, newestFirst = true }: {
  events: ComplianceProgressEvent[]
  newestFirst?: boolean
}) {
  return (
    <RunLog
      newestFirst={newestFirst}
      events={events.map((e) => ({ at: e.at, sec: e.sec, kind: e.kind, text: e.text }))}
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
  const events = run.log ?? []
  return (
    <RunLogButton
      size={size}
      title="Compliance run log"
      events={events.map((e) => ({ at: e.at, sec: e.sec, kind: e.kind, text: e.text }))}
      note={
        (run.finishedAt
          ? `From the check that finished ${formatRelative(run.finishedAt)}.`
          : 'From the last check.')
        + ' Times are seconds from the start of the run.'
      }
      truncatedNote={
        run.logTruncated
          ? `This log holds the last ${events.length} lines of the run. A run keeps a `
            + 'bounded transcript and drops routine progress before it drops a failure, so '
            + 'every chart that refused is here and some of the ones that rendered are not. '
            + 'The coverage table is the complete list.'
          : undefined
      }
    />
  )
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
