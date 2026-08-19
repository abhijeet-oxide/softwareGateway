import {
  Alert, Card, Col, Descriptions, Progress, Row, Select, Space, Steps, Table, Tag, Tooltip,
  Typography,
} from 'antd'
import { LoadingOutlined } from '@ant-design/icons'
import { useState } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import {
  useReplication, useSyncs, useTransfer, useTransferFailures, useTransferJobs,
} from '../api/queries'
import { isLive, kindName, repositoryOf, transferVersion } from '../domain/derive'
import {
  bytes, elapsedSeconds, formatBytes, formatCount, formatDuration, formatSpeed,
} from '../domain/format'
import { NA, Stat, Value } from '../components/value'
import { MeasuredProgress, StateStrip, type StripState } from '../components/progress'
import { RepoLink, TimeAgo, TransferStateTag } from '../components/chips'
import {
  ARTIFACT_ICONS, Icon, IndexIcon, LayersIcon, OciIcon, type IconComponent,
} from '../components/icons'
import { PriorityControl, QueueControls } from '../components/queuecontrols'
import { ErrorState, PageHeader, SavedBreakdown, SavedPanel } from '../components/layout'
import { mono } from '../theme'
import type { Job } from '../api/types'

/**
 * Page 4 — Download.
 *
 * Answers: what is happening to this release right now, and what did it cost?
 *
 * # The asymmetry this page exists to preserve
 *
 * Step 1 is OUR work: we move the bytes, so we count them, and it gets a
 * measured bar with a speed and an ETA. Step 2 is QUAY'S work: we configure
 * the mirror and Quay pulls the content itself, so we can report that a sync
 * started, that it finished and what it produced — and nothing else.
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
 * When it stops moving, the rollup cannot say why — "2486/2489" names nothing
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
            options={[
              { value: 'leased', label: 'Running now' },
              { value: 'failed', label: 'Failed' },
              { value: 'pending', label: 'Waiting' },
              { value: 'blocked', label: 'Blocked' },
              { value: 'succeeded', label: 'Succeeded' },
              { value: 'skipped', label: 'Already present' },
              { value: 'all', label: 'Every job' },
            ]}
          />
        </Space>
      }
    >
      <Table<Job>
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
             * that a row cannot hold — the error — has a column of its own.
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
            // Only a failed job has one, so the column is empty for most rows —
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
 * The same two words the queue uses — a manifest names things, a blob is the
 * content itself — so the column reads without the reader translating.
 */
const JOB_ICONS: Record<string, IconComponent> = {
  manifest: IndexIcon,
  blob: LayersIcon,
}

/** A job's state, spinning while a worker holds it. */
function JobStateTag({ job }: { job: Job }) {
  const colour: Record<string, string> = {
    SUCCEEDED: 'green',
    SKIPPED: 'default',
    FAILED: 'error',
    LEASED: 'processing',
    PENDING: 'blue',
    BLOCKED: 'warning',
    CANCELLED: 'default',
  }
  // SKIPPED is not a failure and not a shrug: the destination already had it,
  // which is the number that makes a delta download legible.
  const label = job.state === 'SKIPPED' ? 'ALREADY THERE' : job.state
  return (
    <Tooltip title={job.state === 'SKIPPED' ? job.skipReason || 'The destination already held this content.' : undefined}>
      <Tag
        color={colour[job.state] ?? 'default'}
        icon={job.state === 'LEASED' ? <LoadingOutlined spin /> : undefined}
        style={{ marginInlineEnd: 0 }}
      >
        {label}
      </Tag>
    </Tooltip>
  )
}

export default function DownloadDetail() {
  const { transferId } = useParams()
  const navigate = useNavigate()

  const transfer = useTransfer(transferId)
  const failures = useTransferFailures(transferId)

  const t = transfer.data
  const syncs = useSyncs(t?.product, t?.targetName)
  const replication = useReplication(t?.product)

  if (transfer.isError) {
    return (
      <>
        <PageHeader title="Download" description="What is happening, and what it cost" />
        <ErrorState error={transfer.error} retry={() => void transfer.refetch()} />
      </>
    )
  }

  const progress = t?.progress
  const transferred = bytes(progress?.bytesTransferred)
  const planned = bytes(progress?.plannedBytes)
  const saved = bytes(progress?.savedBytes) ?? bytes(progress?.dedupeSkippedBytes)
  const content = bytes(progress?.contentBytes)

  const elapsed = elapsedSeconds(t?.startedAt, t?.completedAt)
  // A speed we can defend: bytes WE moved over the time WE were moving them.
  // Both absent for a delegated transfer, and the components below refuse to
  // render a bar for one.
  const speed = elapsed && transferred && elapsed > 0 ? transferred / elapsed : undefined
  const remaining = bytes(progress?.outstandingBytes)
  const eta = speed && remaining ? remaining / speed : undefined

  const running = t ? isLive(t.state) : false
  const failed = t?.state === 'FAILED'
  const done = t?.state === 'SUCCEEDED'

  // Step 2 reads the mirror's own state, never a byte count.
  const lastSync = syncs.data?.syncs?.[0]
  /*
   * Is there a mirror at all?
   *
   * Most deployments have none: the download ends at internal storage and
   * nothing is delegated to a registry. Showing "Step 2 — Configuring Mirror
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
   * then the page has a state and nothing else — which is exactly the moment
   * somebody asks whether the thing is broken. It usually is not: the fleet is
   * busy with higher-priority or earlier work, and this download is next.
   *
   * So the answer is assembled from what the transfer actually knows, and
   * stays silent for anything running, settled or paused.
   */
  const waiting = (() => {
    if (!t || t.state !== 'READY' && t.state !== 'PENDING' && t.state !== 'PLANNING') return null
    if (t.state === 'PLANNING' || (t.state === 'PENDING' && !progress?.jobsPlanned)) {
      return 'This download is still being planned: the release is being walked to work out what has to move.'
    }
    const workers = progress?.workers ?? 0
    const pending = progress?.jobsWaiting ?? progress?.jobsOutstanding ?? 0
    return `${formatCount(pending) ?? '0'} jobs are ready to run and no worker has taken one yet`
      + (workers > 0
        ? `. ${formatCount(workers)} workers are busy with other work; this download starts as they free up.`
        : '. The fleet is occupied elsewhere, or no worker is currently connected.')
      + ' Raising the priority below moves it ahead of work that has not started.'
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
         * All three are needed to identify a download — a product has many
         * packages, and one version tag exists in every repository the product
         * watches — but they are not equally interesting to somebody who has
         * just clicked into one. Three names joined by dots read as a path and
         * gave the version, the thing they came for, the least prominence.
         *
         * Not "Downloading …" either: this page outlives the download, and the
         * present tense on a finished one reads as still running. The state tag
         * says which it is.
         */
        title={t ? `${repositoryOf(t.source)} · ${transferVersion(t)}` : 'Download'}
        description={t ? `${t.product} — what happened to this release, and what it cost`
          : 'What happened to this release, and what it cost'}
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
              hasFailures={Boolean(failures.data?.failures?.length)}
              onDeleted={() => navigate('/downloads')}
            />
          )
        }
      />

      {waiting && (
        <Alert
          style={{ marginBottom: 16 }}
          type="info"
          showIcon
          message="Queued — nothing has been handed to a worker yet"
          description={waiting}
        />
      )}

      {/*
        A stepper, and nothing else.
        
        It carried a line of explanation under every step — the target's host,
        "signature checked at the destination", "not yet" — which is three
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
            { title: 'Downloading' },
            ...(mirrored ? [{ title: 'Mirroring' }] : []),
            { title: 'Verification' },
            { title: 'Completed' },
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
              title={mirrored
                ? `Step 1 — downloading to ${t?.targetName ?? 'internal storage'}`
                : `Downloading to ${t?.targetName ?? 'internal storage'}`}
            >
              <Space direction="vertical" size={12} style={{ width: '100%' }}>
                <RepoLink url={t?.target ? `https://${t.target}` : undefined} label={t?.targetName} />

                <MeasuredProgress
                  transferred={transferred}
                  total={planned}
                  strategy={t?.strategy ?? 'copy'}
                  speedBytesPerSecond={running ? speed : undefined}
                />

                <Table
                  size="small"
                  pagination={false}
                  dataSource={t?.content ?? []}
                  rowKey={(c) => c.kind}
                  columns={[
                    {
                      title: 'Type',
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
                    { title: 'Total', align: 'right', width: 80, render: (_, c) => <Value>{formatCount(c.total)}</Value> },
                    { title: 'Copied', align: 'right', width: 80, render: (_, c) => <Value>{formatCount(c.copied)}</Value> },
                    {
                      title: 'Already present',
                      align: 'right',
                      width: 130,
                      render: (_, c) => <Value>{formatCount(c.present)}</Value>,
                    },
                    {
                      title: 'Progress',
                      width: 180,
                      render: (_, c) => (
                        <MeasuredProgress
                          transferred={c.copied + c.present}
                          total={c.total}
                          strategy={t?.strategy ?? 'copy'}
                          showText={false}
                        />
                      ),
                    },
                  ]}
                />

                {saved !== undefined && saved > 0 && (
                  <SavedPanel
                    savedBytes={formatBytes(saved) ?? ''}
                    totalBytes={formatBytes(content) ?? undefined}
                    content={t?.content}
                    skips={progress?.skips}
                  />
                )}
              </Space>
            </Card>

            {mirrored && (
              <Card title="Step 2 — configuring the mirror">
                <Space direction="vertical" size={12} style={{ width: '100%' }}>
                  <StateStrip
                    state={mirrorState}
                    label={
                      mirrorState === 'done'
                        ? 'Mirror configured and first sync completed'
                        : mirrorState === 'failed'
                          ? 'The mirror reported a failure'
                          : mirrorState === 'running'
                            ? 'Configured — waiting for Quay to finish its first sync'
                            : 'Not configured yet'
                    }
                    events={[
                      { label: 'Configured', at: lastSync?.startedAt },
                      { label: 'First sync completed', at: lastSync?.completedAt },
                    ]}
                    message={
                      lastSync?.message ??
                      'Quay pulls this content itself once configured, so there are no bytes for us to count here — only what it reports.'
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
          <Card title="Download Summary" style={{ width: '100%' }}>
            <Descriptions column={1} size="small">
              <Descriptions.Item label="Total size">
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
                <SavedBreakdown content={t?.content} skips={progress?.skips}>
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
                looks wrong — the right version from the wrong repository, or
                the right content at the wrong destination, both of which read
                as success everywhere else — and both are links, because the
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
        and was being asked to share half a page with the progress panel — at
        which point the error column, the one somebody opened it for, was
        three words wide.
      */}
      {/*
        Failures and jobs, both full width and both below the two columns.
        A failure's cause is a sentence from a registry — the whole sentence is
        the useful part — and it was being wrapped into a half-width column
        three words at a time.
      */}
            {failures.data?.failures?.length ? (
        <Card
          title="Failures"
          styles={{ header: { color: '#C4262E' } }}
          // The retry belongs NEXT TO the failures, not only in the
          // header: this is where somebody is reading when they decide
          // to do something about them, and a retry resumes rather than
          // restarting — nothing already moved is moved again.
          extra={
            t && <QueueControls transfer={t} hasFailures onDeleted={() => navigate('/downloads')} />
          }
        >
          {failures.data.failures.some((f) => f.retryable) && (
            <Typography.Paragraph type="secondary" style={{ fontSize: 13 }}>
              Failures marked retryable are retried automatically, a few minutes apart and a few
              times over, before the download is left for somebody to look at. Retry now if you
              have already dealt with the cause — it resumes rather than restarting.
            </Typography.Paragraph>
          )}
          <Table
            size="small"
            pagination={false}
            dataSource={failures.data.failures}
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
                        ? 'A second attempt could plausibly succeed, so the system retries these by itself — a few minutes apart, a few times over, before leaving them for a person.'
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
          <JobsPanel transferId={t.id} hasFailures={Boolean(failures.data?.failures?.length)} />
        </div>
      )}
    </>
  )
}
