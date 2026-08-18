import {
  Alert, Card, Col, Descriptions, Row, Select, Space, Steps, Table, Tag, Tooltip, Typography,
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
import { ARTIFACT_ICONS, Icon } from '../components/icons'
import { PriorityControl, QueueControls } from '../components/queuecontrols'
import { ErrorState, PageHeader, SavedPanel } from '../components/layout'
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
          expandable={{
            // Only where there is something to expand INTO. An expander on
            // every row promises detail that most rows do not have.
            rowExpandable: (j) => Boolean(j.lastError || j.parent?.ref || j.targetTags?.length),
            expandedRowRender: (j) => (
              <Descriptions size="small" column={1} style={{ marginBlock: 4 }}>
                {j.parent?.ref && (
                  <Descriptions.Item label="Part of">
                    <span style={{ fontFamily: mono, fontSize: 12 }}>{j.parent.ref}</span>
                    {j.parent.shared && (
                      <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                        {' '}— shared with other components, so this is an example
                      </Typography.Text>
                    )}
                  </Descriptions.Item>
                )}
                <Descriptions.Item label="Digest">
                  <span style={{ fontFamily: mono, fontSize: 11 }}>{j.digest}</span>
                </Descriptions.Item>
                {j.targetRepository && (
                  <Descriptions.Item label="Into">
                    <span style={{ fontFamily: mono, fontSize: 12 }}>
                      {j.targetRepository}
                      {j.targetTags?.length ? `:${j.targetTags.join(', :')}` : ''}
                    </span>
                  </Descriptions.Item>
                )}
                {j.lastError && (
                  <Descriptions.Item label="Last error">
                    <Typography.Text type="danger" style={{ fontSize: 12 }}>
                      {j.lastErrorClass ? `${j.lastErrorClass}: ` : ''}{j.lastError}
                    </Typography.Text>
                  </Descriptions.Item>
                )}
              </Descriptions>
            ),
          }}
          columns={[
            {
              title: 'What',
              width: 230,
              render: (_, j) => (
                <Space direction="vertical" size={0}>
                  <Space size={6}>
                    <Tag style={{ marginInlineEnd: 0 }}>{j.kind}</Tag>
                    <Typography.Text style={{ fontFamily: mono, fontSize: 11 }}>
                      {shortDigest(j.digest)}
                    </Typography.Text>
                  </Space>
                  {j.parent?.ref && (
                    <Typography.Text type="secondary" style={{ fontSize: 11 }} ellipsis>
                      {j.parent.ref}
                    </Typography.Text>
                  )}
                </Space>
              ),
            },
            { title: 'Size', width: 90, align: 'right', render: (_, j) => <Value>{formatBytes(bytes(j.sizeBytes))}</Value> },
            { title: 'State', width: 120, render: (_, j) => <JobStateTag job={j} /> },
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
              width: 150,
              render: (_, j) => <Value>{j.leaseOwner ?? null}</Value>,
            },
            {
              title: 'Moved',
              width: 110,
              align: 'right',
              render: (_, j) => <Value>{formatBytes(bytes(j.bytesTransferred))}</Value>,
            },
          ]}
      />
    </Card>
  )
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

/** sha256:abcd… — enough to recognise, short enough for a column. */
function shortDigest(digest: string): string {
  const hex = digest.includes(':') ? digest.split(':')[1] ?? '' : digest
  return hex.slice(0, 12)
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
        // All three names, because none of them identifies the download on its
        // own: a product has many packages, and one version tag exists in
        // every repository the product watches. Not "Downloading …" either —
        // this page outlives the download, and the present tense on a finished
        // one reads as still running. The state tag below says which it is.
        title={t ? `${t.product} · ${repositoryOf(t.source)} · ${transferVersion(t)}` : 'Download'}
        description="What happened to this release, and what it cost"
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

      <Card style={{ marginBottom: 16 }} loading={transfer.isLoading}>
        <Steps
          current={step}
          status={failed ? 'error' : undefined}
          items={[
            { title: `Downloading to ${t?.targetName ?? 'internal storage'}`, description: t?.target },
            ...(mirrored
              ? [{ title: 'Configuring mirror', description: 'Configured by us, synced by the registry' }]
              : []),
            { title: 'Verification', description: 'Signature checked at the destination' },
            { title: 'Completed', description: done ? 'Landed' : 'Not yet' },
          ]}
        />
      </Card>

      <Row gutter={[16, 16]}>
        <Col xs={24} xl={15}>
          <Space direction="vertical" size={16} style={{ width: '100%' }}>
            <Card title={mirrored ? `Step 1 — downloading to ${t?.targetName ?? 'internal storage'}` : `Downloading to ${t?.targetName ?? 'internal storage'}`}>
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
                <Table
                  size="small"
                  pagination={false}
                  dataSource={failures.data.failures}
                  rowKey={(f) => f.message}
                  columns={[
                    { title: 'Cause', render: (_, f) => f.message },
                    { title: 'Artifacts', align: 'right', width: 90, render: (_, f) => <Value>{formatCount(f.failed)}</Value> },
                    {
                      title: 'Retryable',
                      width: 110,
                      render: (_, f) =>
                        f.retryable ? <Tag color="blue">Worth retrying</Tag> : <Tag>Will not succeed on retry</Tag>,
                    },
                  ]}
                />
              </Card>
            ) : null}
          </Space>
        </Col>

        <Col xs={24} xl={9}>
          <Card title="Download Summary">
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
                <Value>{formatBytes(saved)}</Value>
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
      {t && (
        <div style={{ marginTop: 16 }}>
          <JobsPanel transferId={t.id} hasFailures={Boolean(failures.data?.failures?.length)} />
        </div>
      )}
    </>
  )
}
