import { Button, Space, Typography } from 'antd'

import { CheckCircleOutlined, ClockCircleOutlined, CloseCircleOutlined, CopyOutlined, PackageOutlined } from '../icons'
import { ScannerMark } from './icons'
import type { SecurityRegistration } from '../api/types'
import { RunCard, RunLogButton, runEventsFromLog } from './runpanel'
import type { RunChip, RunStep } from './runpanel'
import type { RunTile } from './runtiles'
import { c } from '../uikit'

/**
 * A replication to Anchore, WHILE IT RUNS.
 *
 * # What this replaces
 *
 * A button that spun for as long as the work took and then a toast. Everything
 * the run knew - which images it was submitting, how many Anchore already had,
 * which one it refused and why - existed only inside the request handling the
 * click, so a reader who reloaded the page was shown the word "registering" and
 * nothing else, and a reader who navigated away lost it entirely. That is
 * indistinguishable from a job that has hung, which is precisely the state
 * somebody is in for the whole of a slow run.
 *
 * # Why it is the same panel as the compliance check
 *
 * Because it is the same screen: minutes of work against somebody else's
 * system, watched by the same person on the same release, read to answer the
 * same two questions. The only thing that differs is what is being counted -
 * images here, Helm charts there - so the shape lives in RunPanel and this
 * supplies the facts. See runpanel.tsx.
 */

/**
 * The three stages a replication goes through.
 *
 * Named after what happens to the IMAGES, because that is what the reader is
 * waiting on. "Take stock / Submit / Group" named the Coordinator's internal
 * steps: true, and it left somebody watching a bar unable to say which of the
 * three had anything to do with their release appearing in Anchore.
 */
const STEPS: { key: string; label: string }[] = [
  { key: 'discovering', label: 'Discover images' },
  { key: 'submitting', label: 'Replicate images' },
  { key: 'associating', label: 'Create application' },
]

export function ReplicationRunPanel({ registration, onStop, stopping, finished, onClose }: {
  registration: SecurityRegistration
  onStop?: () => void
  stopping?: boolean
  /** The run has ended and this panel is its record. */
  finished?: boolean
  onClose?: () => void
}) {
  const p = registration.progress
  const label = registration.label
  const failed = registration.state === 'failed'

  // A finished run has no live position: the row's progress column is cleared
  // when it ends. Its transcript is on the registration, which is where the
  // panel reads from once the work is over.
  const events = runEventsFromLog(p?.log ?? registration.log, p?.startedAt)
  // A failed run never reached the application step - nothing was accepted, so
  // there was nothing to attach - and a route drawing it as done says work
  // happened that did not.
  const stage = p?.stage ? p.stage : finished && !failed ? 'associating' : 'discovering'
  const at = STEPS.findIndex((s) => s.key === stage)

  const steps: RunStep[] = STEPS.map((s, i) => ({
    key: s.key,
    label: s.label,
    state: finished
      ? failed
        ? i < STEPS.length - 1 ? 'done' : 'pending'
        : 'done'
      : i === at ? 'current' : i < at ? 'done' : 'pending',
  }))

  const active: RunChip[] = (p?.active ?? []).map((name) => ({
    key: name,
    label: name,
    // The mark, because these are container images. At eleven pixels, beside a
    // parallelism count and a stage name, a bare hyphenated string is not
    // obviously the name of a thing being submitted rather than another label.
    icon: <PackageOutlined />,
  }))

  return (
    <RunCard
      title={finished
        ? failed
          ? `Replication to ${label} failed`
          : `Replication to ${label} finished`
        : `Replication to ${label}`}
      titleIcon={finished ? <ScannerMark provider={registration.provider} size={16} /> : undefined}
      subtitle={
        !finished && p && p.here === false
          ? 'Running on another Coordinator. This position is up to one heartbeat old.'
          : undefined
      }
      elapsed={p?.elapsed}
      startedAt={p?.startedAt}
      finished={finished}
      tone={finished ? (failed ? 'danger' : 'ok') : undefined}
      onClose={onClose}
      onStop={onStop}
      stopping={stopping}
      stopLabel="Stop replication"
      stopHint={
        'Releases the claim immediately, so the release can be replicated again. '
        + `Images ${label} has already accepted remain registered: submission is per image, `
        + 'so stopping affects only those not yet sent.'
      }
      label={headline(registration, finished)}
      // The scanner's own reason once the run is over: the live detail
      // describes a stage that is no longer running.
      detail={finished ? registration.error : p?.detail}
      // The remedy, where there is one, as a warning rather than a caption -
      // it is the only thing on a failed panel the reader can act on.
      notes={finished
        ? registration.remedy ? [registration.remedy] : undefined
        : p?.notes}
      done={p?.done ?? registration.submitted + failedCount(registration)}
      total={p?.total ?? registration.expected}
      failed={failedCount(registration)}
      concurrency={finished ? undefined : p?.concurrency}
      concurrencyLabel="images in parallel"
      concurrencyHint={
        `Images submitted to ${label} at once. Set for ${label}'s benefit: it pulls and `
        + 'analyses each one itself, so a higher limit does not shorten the analysis.'
      }
      active={active}
      steps={steps}
      tiles={tilesFor(registration)}
      events={events}
      eventsLabel="Replication log"
      footer={<OutcomeCopyButton registration={registration} />}
    />
  )
}

/** How many images the scanner refused, live or from the finished record. */
function failedCount(r: SecurityRegistration): number {
  if (r.progress?.failed !== undefined) return r.progress.failed
  return Math.max(0, r.expected - r.submitted)
}

/** What it is doing, as a sentence rather than a stage name. */
function headline(r: SecurityRegistration, finished?: boolean): string {
  if (finished) {
    return r.state === 'failed'
      ? `${r.label} accepted none of this release's images`
      : `${r.submitted.toLocaleString()} of ${r.expected.toLocaleString()} images replicated to ${r.label}`
  }
  switch (r.progress?.stage) {
    case 'submitting':
      return 'Submitting images for analysis'
    case 'associating':
      return `Creating the application version in ${r.label}`
    default:
      return 'Discovering the images in this release'
  }
}

/**
 * The counters.
 *
 * Rejections are here beside the acceptances, and coloured, because they change
 * what the answer means. `done` counts ATTEMPTS, so a run whose every image was
 * refused reached "154 of 154" with a tile reading "154 replicated" - the exact
 * opposite of what had happened.
 */
function tilesFor(r: SecurityRegistration): RunTile[] {
  const p = r.progress
  const tiles: RunTile[] = []
  const failed = failedCount(r)
  // Live: attempts minus refusals. Finished: what the run recorded.
  const replicated = p ? Math.max(0, p.done - failed) : r.submitted

  if (r.expected > 0) {
    tiles.push({
      label: 'Images in this release',
      value: r.expected.toLocaleString(),
      hint: `Artifacts ${r.label} can analyse. Charts, files and signatures are excluded: `
        + 'they contain nothing it scans.',
    })
    tiles.push({
      label: 'Replicated',
      value: replicated.toLocaleString(),
      tone: c.ok,
      icon: <PackageOutlined style={{ color: c.ok }} />,
      hint: `Images ${r.label} accepted. It pulls and analyses them on its own schedule; `
        + 'sync this release to collect the results.',
      detail: <OutcomeList values={r.outcomes?.replicated} />,
    })
  }
  if (failed > 0) {
    tiles.push({
      label: 'Failed',
      value: failed.toLocaleString(),
      tone: c.danger,
      icon: <CloseCircleOutlined style={{ color: c.danger }} />,
      hint: `Images ${r.label} refused. It reports no findings for these, which records that `
        + 'no analysis was requested rather than that they are clean. The reason is in the '
        + 'replication log.',
      detail: <OutcomeList values={r.outcomes?.failed} />,
    })
  }
  const statuses = p?.statuses
  const analysing = statuses?.analyzing ?? 0
  const analysed = statuses?.analyzed ?? (p ? 0 : r.analysed)
  if (analysing > 0) {
    tiles.push({
      label: 'Analysing',
      value: analysing.toLocaleString(),
      icon: <ClockCircleOutlined style={{ color: c.brand }} />,
      hint: `${r.label} is analysing these images. Their results appear on the next vulnerability sync.`,
    })
  }
  if (analysed > 0) {
    tiles.push({
      label: 'Analysed',
      value: analysed.toLocaleString(),
      tone: c.ok,
      icon: <CheckCircleOutlined style={{ color: c.ok }} />,
      hint: `${r.label} has already completed analysis for these images.`,
      detail: <OutcomeList values={r.outcomes?.analysed} />,
    })
  }
  return tiles
}

function OutcomeList({ values }: { values?: string[] }) {
  const shown = values?.slice(0, 3) ?? []
  if (shown.length === 0) return <Typography.Text type="secondary">No image paths were recorded for this run.</Typography.Text>
  return (
    <Space direction="vertical" size={6} style={{ width: 380, maxWidth: 'min(380px, calc(100vw - 48px))', maxHeight: 180, overflowY: 'auto', paddingInlineEnd: 4 }}>
      {shown.map((value) => (
        <Typography.Text key={value} copyable={{ text: value }} style={{ fontFamily: 'monospace', fontSize: 11, overflowWrap: 'anywhere' }}>
          {value}
        </Typography.Text>
      ))}
      {(values?.length ?? 0) > shown.length && (
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>+{values!.length - shown.length} more</Typography.Text>
      )}
    </Space>
  )
}

function OutcomeCopyButton({ registration }: { registration: SecurityRegistration }) {
  const outcomes = registration.outcomes
  const all = [
    ...(outcomes?.replicated ?? []),
    ...(outcomes?.analysed ?? []),
    ...(outcomes?.failed ?? []),
  ]
  if (all.length === 0) return null
  return (
    <div style={{ display: 'flex', justifyContent: 'flex-end' }}>
      <Button size="small" icon={<CopyOutlined />} onClick={() => void navigator.clipboard?.writeText(all.join('\n'))}>
        Copy image paths
      </Button>
    </div>
  )
}

/**
 * The transcript of a replication that has FINISHED, behind a button.
 *
 * The counterpart of the sync's Sync log and the compliance run's Run log, in
 * the same place on the same kind of row - because "which image did Anchore
 * refuse, and why" is asked after the run rather than during it, and these
 * three are read by the same people on the same release.
 */
export function ReplicationLogButton({ registration, size = 'small' }: {
  registration: SecurityRegistration
  size?: 'small' | 'middle'
}) {
  const events = runEventsFromLog(registration.log)
  if (events.length === 0) return null
  return (
    <RunLogButton
      events={events}
      size={size}
      label="Replication log"
      title={`Replication to ${registration.label}`}
      note={
        <Space direction="vertical" size={2}>
          <span>
            From the last replication
            {registration.registeredAt
              ? `, which finished ${new Date(registration.registeredAt).toLocaleString()}.`
              : '.'}
          </span>
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            Times are clock times: a replication is short enough that the gaps between its
            lines are the interesting part.
          </Typography.Text>
        </Space>
      }
    />
  )
}
