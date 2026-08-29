import {
  Alert, Card, Col, Descriptions, Progress, Row, Select, Space, Steps, Table, Tag, Tooltip,
  Typography,
} from 'antd'
// The working-surface table: resizable, reorderable, pinnable columns whose
// layout each person keeps. See `tablekit/README.md` for which tables get it.
import { Table as DataTable } from '../tablekit'
import { LoadingOutlined } from '../icons'
import { useState } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import {
  useReplication, useSyncs, useTransfer, useTransferFailures, useTransferJobs,
} from '../api/queries'
import { isLive, kindName, repositoryOf, transferVersion } from '../domain/derive'
import {
  bytes, elapsedSeconds, formatAbsolute, formatBytes, formatCount, formatDuration, formatSpeed,
} from '../domain/format'
import { NA, Stat, Value } from '../components/value'
import {
  CopyProgress, etaSeconds, MeasuredProgress, PromotionProgress, StateStrip,
  type StripState,
} from '../components/progress'
import { RepoLink, TimeAgo, TransferStateTag } from '../components/chips'
import {
  ARTIFACT_ICONS, Icon, IndexEditIcon, LayersIcon, OciIcon, type IconComponent,
} from '../components/icons'
import { PriorityControl, QueueControls } from '../components/queuecontrols'
import { ErrorState, PageHeader, SavedBreakdown, SavedPanel } from '../components/layout'
import { c, mono } from '../uikit'
import type { ContentGroup, Job } from '../api/types'

/**
 * Page 4 - Download.
 *
 * Answers: what is happening to this release right now, and what did it cost?
 *
 * # The asymmetry this page exists to preserve
 *
 * Step 1 is OUR work: we move the bytes, so we count them, and it gets a
 * measured bar with a speed and an ETA. Step 2 is QUAY'S work: we configure
 * the mirror and Quay pulls the content itself, so we can report that a sync
 * started, that it finished and what it produced - and nothing else.
 *
 * Two steps of one operation, two different kinds of truth, shown differently
 * on purpose (docs/design/18 §6.1, 19 §6).
 */

/**
 * The jobs behind a download.
 *
 * # Why this belongs on the page at all
 *
 * A download is thousands of jobs, and the summary above is a rollup of them.
 * When it stops moving, the rollup cannot say why - "2486/2489" names nothing
 * anybody can act on. `transferctl transfers jobs` has always been able to
 * answer that; this is the same answer, on the page somebody is already
 * looking at.
 *
 * # Collapsed by default, filtered by state
 *
 * Thousands of rows is not a table anybody reads top to bottom, so the panel
 * opens on demand and the filter starts wherever the trouble is: failures if
 * there are any, otherwise what is running right now.
 */
function JobsPanel({ transferId, hasFailures }: { transferId: string; hasFailures: boolean }) {
  const [state, setState] = useState<string | undefined>(hasFailures ? 'failed' : 'leased')
  const jobs = useTransferJobs(transferId, state)
  const rows = jobs.data?.jobs ?? []

  return (
    <Card
      title="Jobs"
      extra={
        <Space>
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            {rows.length} shown
          </Typography.Text>
          <Select
            size="small"
            style={{ minWidth: 170 }}
            value={state ?? 'all'}
            onChange={(v) => setState(v === 'all' ? undefined : v)}
            // The same words the State column uses, so the filter and the rows
            // it produces are not two vocabularies for one set of states.
            options={[
              { value: 'leased', label: 'Running' },
              { value: 'failed', label: 'Failed' },
              { value: 'pending', label: 'Queued' },
              { value: 'blocked', label: 'Waiting on layers' },
              { value: 'succeeded', label: 'Copied' },
              { value: 'skipped', label: 'Already there' },
              { value: 'all', label: 'Every job' },
            ]}
          />
        </Space>
      }
    >
      <DataTable<Job>
        tableEnhancedKey="download-jobs"
        allow_export
        size="small"
        loading={jobs.isLoading}
        dataSource={rows}
        rowKey={(j) => j.id}
        pagination={{ pageSize: 20, showSizeChanger: false, size: 'small' }}
        scroll={{ x: 900 }}
        locale={{
          emptyText: (
            <Typography.Text type="secondary">
              No job is in that state right now.
            </Typography.Text>
          ),
        }}
        columns={[
          {
            /*
             * Everything that identifies a job, in one cell and no expander.
             *
             * The expander held three things and every one of them was already
             * on the page: the parent artifact (this IS that artifact's job),
             * the destination (the whole page is about one destination), and
             * the digest (which now sits here, where it belongs). What is left
             * that a row cannot hold - the error - has a column of its own.
             */
            title: 'Artifact',
            width: 320,
            render: (_, j) => (
              <Space size={6}>
                <Icon as={JOB_ICONS[j.kind] ?? OciIcon} size={15} title={j.kind} />
                <Space direction="vertical" size={0} style={{ minWidth: 0 }}>
                  <Tooltip title={j.digest}>
                    <Typography.Text style={{ fontFamily: mono, fontSize: 12, maxWidth: 270 }} ellipsis>
                      {j.digest}
                    </Typography.Text>
                  </Tooltip>
                  {j.parent?.ref && (
                    <Typography.Text type="secondary" style={{ fontSize: 11, maxWidth: 270 }} ellipsis>
                      {j.parent.ref}
                    </Typography.Text>
                  )}
                </Space>
              </Space>
            ),
          },
          {
            // One cell for how far and how big. Two number columns made a
            // reader do the division themselves, on every row.
            title: 'Progress',
            width: 190,
            render: (_, j) => <JobProgress job={j} />,
          },
          { title: 'State', width: 130, render: (_, j) => <JobStateTag job={j} /> },
          {
            title: 'Attempts',
            width: 90,
            align: 'right',
            render: (_, j) => (
              <Typography.Text type={j.attempts > 1 ? 'warning' : undefined}>
                {j.attempts}/{j.maxAttempts}
              </Typography.Text>
            ),
          },
          {
            title: 'Worker',
            width: 140,
            render: (_, j) => <Value>{j.leaseOwner ?? null}</Value>,
          },
          {
            // Only a failed job has one, so the column is empty for most rows -
            // which is the point: the eye goes straight to the ones that do.
            title: 'Error',
            render: (_, j) =>
              j.lastError ? (
                <Tooltip title={j.lastError}>
                  <Typography.Text type="danger" style={{ fontSize: 12 }} ellipsis>
                    {j.lastErrorClass ? `${j.lastErrorClass}: ` : ''}{j.lastError}
                  </Typography.Text>
                </Tooltip>
              ) : null,
          },
        ]}
      />
    </Card>
  )
}

/**
 * How far this job has got, and how big it is.
 *
 * A bar only where a bar means something: a blob is bytes moving and has a
 * position; a manifest is one small PUT that either happened or did not, and a
 * 0%/100% bar for it is noise down the whole column. Both still say the size,
 * because that is what makes a slow job legible.
 */
function JobProgress({ job }: { job: Job }) {
  const size = bytes(job.sizeBytes)
  const moved = bytes(job.bytesTransferred)
  const running = job.state === 'LEASED'
  const measurable = job.kind === 'blob' && size !== undefined && size > 0

  return (
    <Space direction="vertical" size={0} style={{ width: '100%' }}>
      {measurable && (
        <Progress
          percent={Number(Math.min(100, ((moved ?? 0) / size) * 100).toFixed(1))}
          size="small"
          showInfo={false}
          status={job.state === 'FAILED' ? 'exception' : running ? 'active' : undefined}
        />
      )}
      <Typography.Text type="secondary" style={{ fontSize: 11 }}>
        {moved !== undefined && moved > 0
          ? `${formatBytes(moved)} of ${formatBytes(size)}`
          : formatBytes(size) ?? ''}
      </Typography.Text>
    </Space>
  )
}

/**
 * What a job moves, as a mark.
 *
 * The same two words the queue uses - a manifest names things, a blob is the
 * content itself - so the column reads without the reader translating.
 */
const JOB_ICONS: Record<string, IconComponent> = {
  manifest: IndexEditIcon,
  blob: LayersIcon,
}

/**
 * When a step happened, under the step.
 *
 * Absent where there is no moment to report, rather than a dash or a guess: a
 * step that has not been reached has no time, and inventing one is the whole
 * class of mistake this application avoids elsewhere.
 */
function StepTime({ at }: { at?: string }) {
  const when = formatAbsolute(at)
  if (!when) return null
  return (
    <Typography.Text type="secondary" style={{ fontSize: 11 }}>{when}</Typography.Text>
  )
}

/**
 * A job's state, in the words somebody would use for it.
 *
 * # Why the queue's own vocabulary does not go on the page
 *
 * `LEASED` is a true and precise description of a row in the queue - a worker
 * has taken a lease on it - and it is not what anybody reading this table
 * wants to know, which is whether the thing is running. The same goes for the
 * rest: `PENDING` is queued, `BLOCKED` is waiting for the layers underneath it,
 * `SUCCEEDED` is copied, and `SKIPPED` is the destination already having it.
 *
 * The internal name is not lost - it is on the hover, next to the sentence
 * explaining what the state means - so somebody comparing this against
 * `transferctl` or a log line can still line the two up.
 */
const JOB_STATES: Record<string, { label: string; colour: string; meaning: string }> = {
  LEASED: {
    label: 'Running',
    colour: 'processing',
    meaning: 'A worker has this job now and is moving it.',
  },
  PENDING: {
    label: 'Queued',
    colour: 'blue',
    meaning: 'Ready to run, waiting for a free worker. A job that has just failed also '
      + 'waits here, out its retry backoff, before it is offered again.',
  },
  BLOCKED: {
    label: 'Waiting on layers',
    colour: 'warning',
    meaning: 'Deliberately not runnable yet: a manifest cannot be pushed until every '
      + 'layer it names has landed.',
  },
  SUCCEEDED: {
    label: 'Copied',
    colour: 'green',
    meaning: 'We pushed this to the destination.',
  },
  SKIPPED: {
    label: 'Already there',
    colour: 'default',
    meaning: 'The destination already held this content, so nothing was pushed.',
  },
  FAILED: {
    label: 'Failed',
    colour: 'error',
    meaning: 'This job gave up. The cause is in the error column.',
  },
  CANCELLED: {
    label: 'Cancelled',
    colour: 'default',
    meaning: 'The download was stopped before this job ran.',
  },
}

/** A job's state, spinning while a worker holds it. */
function JobStateTag({ job }: { job: Job }) {
  const state = JOB_STATES[job.state]
  const meaning = job.state === 'SKIPPED' && job.skipReason
    ? `The destination already held this content (${job.skipReason}).`
    : state?.meaning
  return (
    <Tooltip
      title={
        state
          ? <span>{meaning} <Typography.Text type="secondary" style={{ fontSize: 11 }}>
              Queue state: {job.state}.
            </Typography.Text></span>
          : undefined
      }
    >
      <Tag
        color={state?.colour ?? 'default'}
        icon={job.state === 'LEASED' ? <LoadingOutlined spin /> : undefined}
        style={{ marginInlineEnd: 0 }}
      >
        {state?.label ?? job.state}
      </Tag>
    </Tooltip>
  )
}

/**
 * One kind's LAYERS - the pushes underneath its components.
 *
 * A component is `copied` only once its last layer and its manifest have both
 * landed, which is the right answer to "what does the destination hold" and a
 * useless one to "how far along is this": every row of the table read `0`
 * copied and `0` present for the whole download and then moved at the end.
 *
 * The layer counts come from the API where it has them. An older coordinator
 * does not send them, and the component counts are what it has - a coarser
 * answer rather than an empty column.
 */
function layers(group: ContentGroup): { total: number; copied: number; present: number } {
  if ((group.units ?? 0) > 0) {
    return {
      total: group.units ?? 0,
      copied: group.unitsCopied ?? 0,
      present: group.unitsPresent ?? 0,
    }
  }
  return { total: group.total, copied: group.copied, present: group.present }
}

/**
 * How many of this kind there ARE, in the unit the kind is counted in.
 *
 * # Files are counted as files
 *
 * A vendor ships its configuration as one `generic` component carrying a
 * hundred and twelve named layers. Counting components said `Files 2` on a
 * release the release page - which has counted files as files since it learnt
 * to list them - reported as `Files 112`. Two true numbers from two
 * populations, on two pages about the same release, and nothing on either
 * saying which was which.
 *
 * So the count follows the row's own noun: images are components, charts are
 * components, and files are files. The components are not lost - they are on
 * the hover, which is also where the reason lives.
 */
function totalOf(group: ContentGroup): number {
  return group.files && group.files > 0 ? group.files : group.total
}

/**
 * A layer count, in the colour the bar uses for the same fact.
 *
 * Copied is the bar's own stroke, already-present its green portion, so a
 * reader moving between the bar and the table is not asked to re-learn which
 * is which. A ZERO is drawn muted rather than in its tone: a column of
 * confident blue noughts claims activity that has not happened, and what the
 * eye should find in this table is the rows that are moving.
 */
function LayerCount({ value, tone, strong }: {
  value: number
  tone: 'moved' | 'present'
  strong?: boolean
}) {
  if (!value) return <Value>{formatCount(0)}</Value>
  return (
    <Typography.Text
      strong={strong}
      style={{ color: tone === 'moved' ? c.brand : c.ok, fontVariantNumeric: 'tabular-nums' }}
    >
      {formatCount(value)}
    </Typography.Text>
  )
}

/** What the layer count is counting, and why it is the number that moves. */
function layerBreakdown(group: ContentGroup): string {
  const l = layers(group)
  const going = [`${formatCount(l.copied) ?? 0} copied`, `${formatCount(l.present) ?? 0} already there`]
  const left = l.total - l.copied - l.present
  if (left > 0) going.push(`${formatCount(left)} still to go`)
  return `${formatCount(l.total)} layers: ${going.join(', ')}. A layer is what actually `
    + 'moves - each is pushed, mounted or skipped on its own - so these are the counts '
    + 'that change while the download runs.'
}

/** What the count is counting, and how those components are going. */
function totalBreakdown(group: ContentGroup): string {
  const going = [
    `${formatCount(group.copied) ?? 0} copied`,
    `${formatCount(group.present) ?? 0} already there`,
  ]
  if (group.outstanding) going.push(`${formatCount(group.outstanding)} still going`)
  if (group.failed) going.push(`${formatCount(group.failed)} failed`)

  const unit = group.files && group.files > 0
    ? `${formatCount(group.files)} files, carried by ${formatCount(group.total)} `
      + `${group.total === 1 ? 'component' : 'components'}. `
    : ''
  return `${unit}Components: ${going.join(', ')}. A component counts only once every `
    + 'layer under it and the manifest naming it have landed.'
}

export default function DownloadDetail() {
  const { transferId } = useParams()
  const navigate = useNavigate()
  const [failuresDismissed, setFailuresDismissed] = useState(false)

  const transfer = useTransfer(transferId)
  const failures = useTransferFailures(transferId)

  const t = transfer.data
  const syncs = useSyncs(t?.product, t?.targetName)
  const replication = useReplication(t?.product)

  if (transfer.isError) {
    return (
      <>
        <ErrorState error={transfer.error} retry={() => void transfer.refetch()} />
      </>
    )
  }

  const progress = t?.progress
  const content = bytes(progress?.contentBytes)

  /*
   * BYTES ARE WEIGHED ONCE, however many repositories the content reaches.
   *
   * A component published under its own name as well as inside the bundle
   * needs its layers in two repositories, and the planner counts them twice
   * because two repositories is two pieces of bookkeeping. The second copy
   * costs NO BYTES - the registry mounts it - so counting bytes that way said
   * a 29.8 GB release was 63.7 GB of traffic, which never happens, and made a
   * saving larger than the release it was saving on.
   *
   * So the page draws its bar and its saving from the distinct-content account:
   * one population, moved plus present converging on the release's own size.
   * There is no second total to reconcile and none is shown.
   */
  const transferred = bytes(progress?.contentMovedBytes) ?? bytes(progress?.bytesTransferred)
  const saved = bytes(progress?.contentPresentBytes)
    ?? bytes(progress?.savedBytes)
    ?? bytes(progress?.dedupeSkippedBytes)

  const elapsed = elapsedSeconds(t?.startedAt, t?.completedAt)
  // A speed we can defend: bytes WE moved over the time WE were moving them.
  // Both absent for a delegated transfer, and the components below refuse to
  // render a bar for one.
  const speed = elapsed && transferred && elapsed > 0 ? transferred / elapsed : undefined
  /*
   * THE SAME ETA THE DOWNLOADS LIST SHOWS, from the same helper.
   *
   * This page used to divide its own honest speed by the PLANNER's
   * `outstandingBytes`, which counts per (repository, digest) and therefore
   * counts a component published under its own name twice - while the bar and
   * the saving three lines above already read the distinct-content account, for
   * exactly the reason stated there. So the list said ~3h 22m and this page
   * said 8h 17m about the same download in the same second, and nothing on
   * either screen said which to believe.
   */
  const eta = etaSeconds({ transferred, total: content, saved, elapsedSeconds: elapsed })

  const running = t ? isLive(t.state) : false
  const failed = t?.state === 'FAILED'
  const done = t?.state === 'SUCCEEDED'
  const failureGroups = failures.data?.failures ?? []
  const hasFailures = failureGroups.length > 0 && !failuresDismissed

  // Step 2 reads the mirror's own state, never a byte count.
  const lastSync = syncs.data?.syncs?.[0]
  /*
   * Is there a mirror at all?
   *
   * Most deployments have none: the download ends at internal storage and
   * nothing is delegated to a registry. Showing "Step 2 - Configuring Mirror
   * to Quay" for those made the page describe work that was never going to
   * happen, and left every download looking permanently half-finished.
   *
   * Evidence, not configuration-in-principle: a sync this target has actually
   * reported, or a replication row the reconciler wrote for it.
   */
  const mirrored = Boolean(lastSync) || (replication.data?.replication ?? [])
    .some((r) => r.target === t?.targetName)
  const mirrorState: StripState = lastSync?.state === 'succeeded'
    ? 'done'
    : lastSync?.state === 'failed'
      ? 'failed'
      : lastSync
        ? 'running'
        : 'pending'

  /*
   * Why a download that is READY is not moving.
   *
   * A transfer becomes `running` when a worker leases its first job, and until
   * then the page has a state and nothing else - which is exactly the moment
   * somebody asks whether the thing is broken. It usually is not: the fleet is
   * busy with higher-priority or earlier work, and this download is next.
   *
   * So the answer is assembled from what the transfer actually knows, and
   * stays silent for anything running, settled or paused.
   */
  const waiting = (() => {
    if (!t || t.state !== 'READY' && t.state !== 'PENDING' && t.state !== 'PLANNING') return null
    if (t.state === 'PLANNING' || (t.state === 'PENDING' && !progress?.jobsPlanned)) {
      return 'Planning the release artifacts before download starts. Download will begin when planning is complete.'
    }
    const workers = progress?.workers ?? 0
    const pending = progress?.jobsWaiting ?? progress?.jobsOutstanding ?? 0
    return `${formatCount(pending) ?? '0'} jobs waiting for an available worker.`
      + (workers > 0 ? ` ${formatCount(workers)} currently busy.` : '')
  })()

  /*
   * Which step the stepper is on, counted against the steps that EXIST.
   *
   * It used to be a literal 3 for "completed", which was right only while the
   * mirror step was always there. With no mirror configured the list is one
   * shorter and that 3 pointed past the end, leaving a finished download
   * showing nothing as current.
   */
  const steps = [
    'downloading',
    ...(mirrored ? ['mirror'] : []),
    'verification',
    'completed',
  ]
  const step = done ? steps.indexOf('completed') : 0

  return (
    <>
      <PageHeader
        back={{ to: '/downloads', label: 'All downloads' }}
        /*
         * The package and the version make the title; the product goes
         * underneath.
         *
         * All three are needed to identify a download - a product has many
         * packages, and one version tag exists in every repository the product
         * watches - but they are not equally interesting to somebody who has
         * just clicked into one. Three names joined by dots read as a path and
         * gave the version, the thing they came for, the least prominence.
         *
         * Not "Downloading …" either: this page outlives the download, and the
         * present tense on a finished one reads as still running. The state tag
         * says which it is.
         */
        title={t ? `${t.packageName ?? repositoryOf(t.source)} · ${transferVersion(t)}` : 'Download'}
        description={
          t && (
            <Typography.Text strong>
              {repositoryOf(t.source)} · {t.product}
            </Typography.Text>
          )
        }
        meta={
          t && (
            <Space size={16}>
              <TransferStateTag state={t.state} />
              <Stat title="Elapsed" value={formatDuration(elapsed)} valueStyle={{ fontSize: 18 }} />
              <Stat
                title="ETA"
                value={eta !== undefined && running ? formatDuration(eta) : null}
                reason={
                  running
                    ? 'An estimate needs a measured speed and a known amount left to move. One of them is not established yet.'
                    : 'This download is not running, so there is nothing to estimate.'
                }
                valueStyle={{ fontSize: 18 }}
              />
              <Stat
                title="Speed"
                value={speed !== undefined && running ? formatSpeed(speed) : null}
                reason={
                  running
                    ? 'No bytes have been moved yet, so there is no rate to report.'
                    : 'This download is not running, so there is no current speed.'
                }
                valueStyle={{ fontSize: 18 }}
              />
            </Space>
          )
        }
        extra={
          t && (
            <QueueControls
              transfer={t}
              size="middle"
              hasFailures={hasFailures}
              onRetried={() => setFailuresDismissed(true)}
              onDeleted={() => navigate('/downloads')}
            />
          )
        }
      />

      {waiting && (
        <Alert
          style={{ marginBottom: 12, padding: '7px 12px' }}
          type="info"
          showIcon
          message={<Typography.Text style={{ fontSize: 13 }}>Queued</Typography.Text>}
          description={<Typography.Text type="secondary" style={{ fontSize: 12 }}>{waiting}</Typography.Text>}
        />
      )}

      {/*
        A stepper, and nothing else.

        It carried a line of explanation under every step - the target's host,
        "signature checked at the destination", "not yet" - which is three
        sentences of small grey text saying what the step names already say.
        Where the detail matters it is in the card for that step, one screen
        down. Here the only question is which step this download is on.
      */}
      <Card style={{ marginBottom: 16 }} size="small" loading={transfer.isLoading}>
        <Steps
          size="small"
          current={step}
          status={failed ? 'error' : undefined}
          items={[
            {
              title: t?.strategy === 'relocate' ? 'Promoting' : 'Downloading',
              description: <StepTime at={t?.startedAt} />,
            },
            ...(mirrored
              ? [{
                  title: 'Mirroring',
                  description: <StepTime at={lastSync?.completedAt ?? lastSync?.startedAt} />,
                }]
              : []),
            /*
              Verification carries no time, and does not pretend to. Nothing
              writes a verification result yet - see domain/derive - so the step
              exists to say the stage is in the chain and stays blank until
              there is a moment to report.
            */
            { title: 'Verification', description: <StepTime at={undefined} /> },
            { title: 'Completed', description: <StepTime at={t?.completedAt} /> },
          ]}
        />
      </Card>

      {/*
        Both columns stretch, and the cards inside them fill their column, so
        the download panel and the summary end level with each other instead of
        leaving a ragged step down the middle of the page.
      */}
      <Row gutter={[16, 16]} align="stretch">
        <Col xs={24} xl={15} style={{ display: 'flex' }}>
          <Space direction="vertical" size={16} style={{ width: '100%' }}>
            <Card
              style={{ height: '100%' }}
              loading={transfer.isLoading}
              /*
                A PROMOTION is not a download, and calling it one on the page
                somebody opened to watch it is how a reader concludes they
                clicked the wrong button. The word follows the operation.
              */
              title={promotionTitle(t?.strategy, t?.targetName, mirrored)}
            >
              <Space direction="vertical" size={12} style={{ width: '100%' }}>
                <RepoLink url={t?.target ? `https://${t.target}` : undefined} label={t?.targetName} />

                {/*
                  ONE BAR, over WHAT IS AT THE DESTINATION - bytes we have
                  streamed so far plus bytes that were already there, against
                  what the release weighs.

                  It counted finished COMPONENTS, and a component finishes only
                  when its last layer and its manifest have both landed. So the
                  bar read 0% for the first hour of a thirty-gigabyte download
                  while a hundred workers streamed layers on the same screen and
                  the panel below already reported twelve gigabytes saved. See
                  <CopyProgress> for the whole argument, including why a byte
                  bar is held just short of the end while anything is still to
                  push.
                */}
                {/*
                  A PROMOTION the registry carried out has no artifact
                  breakdown and never will: it created no jobs, so there is
                  nothing for the table below to count. Its honest denominator
                  is NAMES, and that is a different bar rather than this one
                  with different numbers in it.
                */}
                {t?.strategy === 'relocate' ? (
                  <PromotionProgress promotion={t?.promotion} />
                ) : (
                  <CopyProgress
                    groups={t?.content}
                    transferred={transferred}
                    total={content}
                    saved={saved}
                    strategy={t?.strategy ?? 'copy'}
                    speedBytesPerSecond={running ? speed : undefined}
                    live={running}
                  />
                )}

                <Table
                  size="small"
                  pagination={false}
                  dataSource={t?.content ?? []}
                  rowKey={(c) => c.kind}
                  className="dl-contents"
                  // Six columns, all of them narrow. It fits a laptop without
                  // scrolling now that the grouped header is gone.
                  scroll={{ x: 660 }}
                  /*
                    THE TOTALS, so the table reconciles with the line above it.
                    The bar says "N of M layers"; without this row a reader has
                    to add five numbers in their head to check that the table is
                    talking about the same download.
                  */
                  summary={(rows) => {
                    const all = rows as readonly ContentGroup[]
                    if (all.length < 2) return null
                    const sum = (pick: (c: ContentGroup) => number) =>
                      all.reduce((n, c) => n + pick(c), 0)
                    const totals = {
                      total: sum(totalOf),
                      layers: sum((c) => layers(c).total),
                      copied: sum((c) => layers(c).copied),
                      present: sum((c) => layers(c).present),
                    }
                    return (
                      <Table.Summary fixed>
                        <Table.Summary.Row>
                          <Table.Summary.Cell index={0}>
                            <Typography.Text strong>All content</Typography.Text>
                          </Table.Summary.Cell>
                          <Table.Summary.Cell index={1} align="right">
                            <Typography.Text strong>{formatCount(totals.total)}</Typography.Text>
                          </Table.Summary.Cell>
                          <Table.Summary.Cell index={2} align="right">
                            <Typography.Text strong>{formatCount(totals.layers)}</Typography.Text>
                          </Table.Summary.Cell>
                          <Table.Summary.Cell index={3} align="right">
                            <LayerCount value={totals.copied} tone="moved" strong />
                          </Table.Summary.Cell>
                          <Table.Summary.Cell index={4} align="right">
                            <LayerCount value={totals.present} tone="present" strong />
                          </Table.Summary.Cell>
                          <Table.Summary.Cell index={5}>
                            <MeasuredProgress
                              transferred={totals.copied}
                              saved={totals.present}
                              total={totals.layers}
                              strategy={t?.strategy ?? 'copy'}
                              showText={false}
                            />
                          </Table.Summary.Cell>
                        </Table.Summary.Row>
                      </Table.Summary>
                    )
                  }}
                  columns={[
                    {
                      title: 'Type',
                      width: 150,
                      // The same words, and the same marks, the release page
                      // uses for the same components. A download of a release
                      // is that release, one screen later.
                      render: (_, c) => {
                        const name = kindName(c.kind)
                        const icon = ARTIFACT_ICONS[name as keyof typeof ARTIFACT_ICONS]
                        return (
                          <Space size={6}>
                            {icon && <Icon as={icon} size={15} title={name} />}
                            {name}
                          </Space>
                        )
                      },
                    },
                    {
                      /*
                        HOW MANY THERE ARE, in the unit the row's own noun
                        names: images and charts are components, files are
                        files. That is what the release page counts, and a
                        download of a release has to report the same release.

                        Deliberately the column that does NOT move much during a
                        download - a component is only complete when everything
                        under it is - which is why the layers sit beside it.
                      */
                      title: 'Total',
                      align: 'right',
                      width: 90,
                      render: (_, c) => (
                        <Tooltip title={totalBreakdown(c)}>
                          <span><Value>{formatCount(totalOf(c))}</Value></span>
                        </Tooltip>
                      ),
                    },
                    {
                      /*
                        THE LAYERS, on one header line with everything else.

                        These three used to sit under a grouped `Layers` header
                        spanning them, which bought a shared noun and cost a
                        second header row - so the table had two rows of
                        furniture above five rows of data, and `Total` appeared
                        twice at different heights meaning different things.
                        Naming the column `Layers` and letting the two beside it
                        inherit that noun says the same thing on one line.
                      */
                      title: 'Layers',
                      key: 'layers',
                      align: 'right',
                      width: 90,
                      render: (_, c: ContentGroup) => (
                        <Tooltip title={layerBreakdown(c)}>
                          <span><Value>{formatCount(layers(c).total)}</Value></span>
                        </Tooltip>
                      ),
                    },
                    {
                      title: 'Copied',
                      key: 'layersCopied',
                      align: 'right',
                      width: 90,
                      // The bar's own two colours, so the table and the bar
                      // above it say the same thing in the same language:
                      // what we moved, and what was already there.
                      render: (_, c: ContentGroup) => (
                        <LayerCount value={layers(c).copied} tone="moved" />
                      ),
                    },
                    {
                      title: 'Already present',
                      key: 'layersPresent',
                      align: 'right',
                      width: 130,
                      render: (_, c: ContentGroup) => (
                        <LayerCount value={layers(c).present} tone="present" />
                      ),
                    },
                    {
                      /*
                        ON THE TARGET - copied plus already present over the
                        whole population, because the destination cannot tell
                        the two apart and neither can anybody waiting on it.
                      */
                      title: 'On the target',
                      width: 170,
                      render: (_, c) => {
                        const l = layers(c)
                        return (
                          <MeasuredProgress
                            // Split, so the green portion says how much of this
                            // row the destination already had. The bar reaches
                            // the same place either way - what is on the target
                            // is on the target - and the colour is the only
                            // thing that distinguishes how it got there.
                            transferred={l.copied}
                            saved={l.present}
                            total={l.total}
                            strategy={t?.strategy ?? 'copy'}
                            showText={false}
                          />
                        )
                      },
                    },
                  ]}
                />

                {/*
                  TWO POPULATIONS, and the table has to say which is which.

                  `Files 2` beside a release page reading `Files 112` looks like
                  a disagreement and is not one: a vendor's file bundle is ONE
                  component holding a hundred and twelve named layers. The
                  components column counts the first, the rest count the second,
                  and the difference is why an unchanged file in a changed
                  bundle is not copied again.
                */}
                <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                  Counted as the release page counts them: images and charts as components,
                  files as files - a file bundle is one component however many files it
                  carries. Layers are what actually moves: each is pushed, mounted or
                  skipped on its own, and a component is only complete once every layer
                  under it and the manifest naming it have landed.
                </Typography.Text>

                {saved !== undefined && saved > 0 && (
                  <SavedPanel
                    savedBytes={formatBytes(saved) ?? ''}
                    // Against what would have been PUSHED, not against what
                    // the release weighs. `Saved 56.5 GB of 29.8 GB` was two
                    // true numbers from different populations, side by side,
                    // reading as nonsense.
                    totalBytes={formatBytes(content) ?? undefined}
                    transferId={t?.id}
                    content={t?.content}
                  />
                )}
              </Space>
            </Card>

            {mirrored && (
              <Card title="Step 2 - configuring the mirror">
                <Space direction="vertical" size={12} style={{ width: '100%' }}>
                  <StateStrip
                    state={mirrorState}
                    label={
                      mirrorState === 'done'
                        ? 'Mirror configured and first sync completed'
                        : mirrorState === 'failed'
                          ? 'The mirror reported a failure'
                          : mirrorState === 'running'
                            ? 'Configured - waiting for Quay to finish its first sync'
                            : 'Not configured yet'
                    }
                    events={[
                      { label: 'Configured', at: lastSync?.startedAt },
                      { label: 'First sync completed', at: lastSync?.completedAt },
                    ]}
                    message={
                      lastSync?.message ??
                      'Quay pulls this content itself once configured, so there are no bytes for us to count here - only what it reports.'
                    }
                  />
                  {lastSync?.itemsSynced !== undefined && (
                    <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                      Quay reported <Value>{formatCount(lastSync.itemsSynced)}</Value> items synced.
                    </Typography.Text>
                  )}
                </Space>
              </Card>
            )}

          </Space>
        </Col>

        <Col xs={24} xl={9} style={{ display: 'flex' }}>
          <Card title="Download Summary" style={{ width: '100%' }} loading={transfer.isLoading}>
            <Descriptions column={1} size="small">
              {/*
                ONE SIZE. There was a second - everything the transfer had to
                account for, which counts a component published under two names
                twice - and it was twice this one and read as a contradiction.
                It was also not bytes anybody waits for: the second copy is a
                mount, and a mount moves nothing.
              */}
              <Descriptions.Item label="Release size">
                <Value reason="The release has not been measured, so its full size is not known.">
                  {formatBytes(content)}
                </Value>
              </Descriptions.Item>
              <Descriptions.Item label="Downloaded">
                <Value>{formatBytes(transferred)}</Value>
              </Descriptions.Item>
              <Descriptions.Item label="Saved (already present)">
                {/*
                  The same hover as the panel above. This is the line somebody
                  reads when the download has finished and the panel has
                  scrolled away, and "saved 30.1 GB" is exactly as opaque here.
                */}
                <SavedBreakdown transferId={t?.id} content={t?.content}>
                  <Value>{formatBytes(saved)}</Value>
                </SavedBreakdown>
              </Descriptions.Item>
              <Descriptions.Item label="Total time">
                <Value>{formatDuration(elapsed)}</Value>
              </Descriptions.Item>
              <Descriptions.Item label="Average speed">
                <Value reason="A speed needs bytes we moved and a duration we timed. One of them is missing.">
                  {formatSpeed(speed)}
                </Value>
              </Descriptions.Item>
              <Descriptions.Item label="Started"><TimeAgo at={t?.startedAt} /></Descriptions.Item>
              <Descriptions.Item label="Completed"><TimeAgo at={t?.completedAt} /></Descriptions.Item>
              <Descriptions.Item label="Priority">
                {t ? <PriorityControl transfer={t} /> : <NA />}
              </Descriptions.Item>
              {/*
                Where it came FROM and where it went TO, at the foot of the
                summary. They are the two facts a reader checks when a download
                looks wrong - the right version from the wrong repository, or
                the right content at the wrong destination, both of which read
                as success everywhere else - and both are links, because the
                next thing anybody does with either is go and look.
              */}
              <Descriptions.Item label="From">
                {t?.source
                  ? <RepoLink url={`https://${t.source}`} label={t.sourceName || repositoryOf(t.source)} />
                  : <NA />}
              </Descriptions.Item>
              <Descriptions.Item label="Target">
                {t?.target
                  ? <RepoLink url={`https://${t.target}`} label={t.targetName || t.target} />
                  : <NA />}
              </Descriptions.Item>
              <Descriptions.Item label="Reference">
                <Typography.Text style={{ fontFamily: mono, fontSize: 11 }} copyable>
                  <Value>{t?.id}</Value>
                </Typography.Text>
              </Descriptions.Item>
            </Descriptions>

          </Card>
        </Col>
      </Row>

      {/*
        Full width, below both columns. A job list is nine columns of its own
        and was being asked to share half a page with the progress panel - at
        which point the error column, the one somebody opened it for, was
        three words wide.
      */}
      {/*
        Failures and jobs, both full width and both below the two columns.
        A failure's cause is a sentence from a registry - the whole sentence is
        the useful part - and it was being wrapped into a half-width column
        three words at a time.
      */}
            {hasFailures ? (
        <Card
          title="Failures"
          styles={{ header: { color: c.danger } }}
          // The retry belongs NEXT TO the failures, not only in the
          // header: this is where somebody is reading when they decide
          // to do something about them, and a retry resumes rather than
          // restarting - nothing already moved is moved again.
          extra={
            t && (
              <QueueControls
                transfer={t}
                hasFailures
                onRetried={() => setFailuresDismissed(true)}
                onDeleted={() => navigate('/downloads')}
              />
            )
          }
        >
          {failureGroups.some((f) => f.retryable) && (
            <Typography.Paragraph type="secondary" style={{ fontSize: 13 }}>
              Failures marked retryable are retried automatically, a few minutes apart and a few
              times over, before the download is left for somebody to look at. Retry now if you
              have already dealt with the cause - it resumes rather than restarting.
            </Typography.Paragraph>
          )}
          <Table
            size="small"
            pagination={false}
            dataSource={failureGroups}
            rowKey={(f) => f.message}
            columns={[
              { title: 'Cause', render: (_, f) => f.message },
              { title: 'Artifacts', align: 'right', width: 90, render: (_, f) => <Value>{formatCount(f.failed)}</Value> },
              {
                // Yes or no. "Worth retrying" and "Will not succeed on retry"
                // were a sentence each in a column that answers a yes-or-no
                // question, and a sentence belongs on the tooltip.
                title: 'Retryable',
                width: 110,
                render: (_, f) => (
                  <Tooltip
                    title={
                      f.retryable
                        ? 'A second attempt could plausibly succeed, so the system retries these by itself - a few minutes apart, a few times over, before leaving them for a person.'
                        : 'A second attempt would fail the same way: a missing credential, a repository that does not exist, or something the registry will not serve. Retrying is not the fix.'
                    }
                  >
                    <Tag color={f.retryable ? 'blue' : undefined} style={{ marginInlineEnd: 0 }}>
                      {f.retryable ? 'Yes' : 'No'}
                    </Tag>
                  </Tooltip>
                ),
              },
            ]}
          />
        </Card>
      ) : null}

      {t && (
        <div style={{ marginTop: 16 }}>
          <JobsPanel transferId={t.id} hasFailures={hasFailures} />
        </div>
      )}
    </>
  )
}

/**
 * What Step 1 is called.
 *
 * A promotion is not a download. The page is the same page - a transfer moving
 * one release to one destination - and the word has to follow the operation,
 * or somebody watching a promotion reads "Downloading to production" and
 * concludes they pressed the wrong button.
 */
function promotionTitle(
  strategy: string | undefined, target: string | undefined, mirrored: boolean,
): string {
  const where = target ?? 'internal storage'
  if (strategy === 'relocate') {
    return `Promoting to ${where}`
  }
  return mirrored ? `Step 1 - downloading to ${where}` : `Downloading to ${where}`
}
