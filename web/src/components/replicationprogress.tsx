import { Space, Typography } from 'antd'

import { PackageOutlined } from '../icons'
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
  const stage = p?.stage ? p.stage : finished ? 'associating' : 'discovering'
  const at = STEPS.findIndex((s) => s.key === stage)

  const steps: RunStep[] = STEPS.map((s, i) => ({
    key: s.key,
    label: s.label,
    state: finished ? 'done' : i === at ? 'current' : i < at ? 'done' : 'pending',
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
        : `Replicating this release to ${label}`}
      titleIcon={<ScannerMark provider={registration.provider} size={16} />}
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
    />
  )
}

/** How many images the scanner refused, live or from the finished record. */
function failedCount(r: SecurityRegistration): number {
  if (r.progress?.failed !== undefined) return r.progress.failed
  return r.state === 'failed' ? r.expected : 0
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
      return `Replicating images to ${r.label}`
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
      hint: `Images ${r.label} accepted. It pulls and analyses them on its own schedule; `
        + 'sync this release to collect the results.',
    })
  }
  if (failed > 0) {
    tiles.push({
      label: 'Rejected',
      value: failed.toLocaleString(),
      tone: c.danger,
      hint: `Images ${r.label} refused. It reports no findings for these, which records that `
        + 'no analysis was requested rather than that they are clean. The reason is in the '
        + 'replication log.',
    })
  }
  if (r.alreadyKnown > 0) {
    tiles.push({
      label: 'Already registered',
      value: r.alreadyKnown.toLocaleString(),
      hint: `${r.label} already held these. Submission by digest is idempotent, so they were `
        + 'not analysed again.',
    })
  }
  return tiles
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
