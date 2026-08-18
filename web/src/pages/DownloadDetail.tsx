import { App, Button, Card, Col, Descriptions, Row, Space, Steps, Table, Tag, Tooltip, Typography } from 'antd'
import { PauseOutlined, PlayCircleOutlined, ReloadOutlined, StopOutlined } from '@ant-design/icons'
import { useParams } from 'react-router-dom'
import {
  useProduct, useSyncs, useTransfer, useTransferControl, useTransferFailures,
} from '../api/queries'
import { useCan } from '../auth/permissions'
import { isLive, kindName, transferVersion } from '../domain/derive'
import {
  bytes, elapsedSeconds, formatBytes, formatCount, formatDuration, formatSpeed,
} from '../domain/format'
import { NA, Stat, Value } from '../components/value'
import { MeasuredProgress, StateStrip, type StripState } from '../components/progress'
import { RepoLink, TimeAgo } from '../components/chips'
import { ARTIFACT_ICONS, Icon } from '../components/icons'
import { ErrorState, PageHeader, SavedPanel } from '../components/layout'
import { mono } from '../theme'

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

export default function DownloadDetail() {
  const { transferId } = useParams()
  const { message } = App.useApp()

  const transfer = useTransfer(transferId)
  const failures = useTransferFailures(transferId)
  const product = useProduct(transfer.data?.product)
  const control = useTransferControl(transferId!)
  const mayOperate = useCan('operate', { product: transfer.data?.product })

  const t = transfer.data
  const syncs = useSyncs(t?.product, t?.targetName)

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
  const mirrorState: StripState = lastSync?.state === 'succeeded'
    ? 'done'
    : lastSync?.state === 'failed'
      ? 'failed'
      : lastSync
        ? 'running'
        : 'pending'

  const step = failed ? (transferred ? 1 : 0) : done ? 3 : running ? 0 : 0

  const act = async (verb: 'retry' | 'pause' | 'resume' | 'stop') => {
    try {
      await control.mutateAsync(verb)
      message.success(
        verb === 'retry'
          ? 'Retrying from where it stopped — work already done is not repeated.'
          : `The download was asked to ${verb}.`,
      )
    } catch (e) {
      message.error(e instanceof Error ? e.message : `The download could not ${verb}.`)
    }
  }

  return (
    <>
      <PageHeader
        title={t ? `Downloading ${t.product} ${transferVersion(t)}` : 'Download'}
        description="What is happening to this release right now, and what it cost"
        meta={
          t && (
            <Space size={16}>
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
          <Space>
            {failed && (
              <Tooltip title="Resumes from where it stopped. Artifacts already transferred are not moved again.">
                <Button
                  type="primary"
                  icon={<ReloadOutlined />}
                  disabled={!mayOperate}
                  loading={control.isPending}
                  onClick={() => void act('retry')}
                >
                  Retry
                </Button>
              </Tooltip>
            )}
            {running && t?.state !== 'PAUSED' && (
              <Button icon={<PauseOutlined />} disabled={!mayOperate} onClick={() => void act('pause')}>
                Pause
              </Button>
            )}
            {t?.state === 'PAUSED' && (
              <Button icon={<PlayCircleOutlined />} disabled={!mayOperate} onClick={() => void act('resume')}>
                Resume
              </Button>
            )}
            {running && (
              <Button danger icon={<StopOutlined />} disabled={!mayOperate} onClick={() => void act('stop')}>
                Stop
              </Button>
            )}
          </Space>
        }
      />

      <Card style={{ marginBottom: 16 }} loading={transfer.isLoading}>
        <Steps
          current={step}
          status={failed ? 'error' : undefined}
          items={[
            { title: 'Downloading to JFrog', description: t?.targetName ?? 'Internal storage' },
            { title: 'Configuring Mirror to Quay', description: 'Configured by us, synced by Quay' },
            { title: 'Verification', description: 'Signature checked at the destination' },
            { title: 'Completed', description: done ? 'Landed' : 'Not yet' },
          ]}
        />
      </Card>

      <Row gutter={[16, 16]}>
        <Col xs={24} xl={15}>
          <Space direction="vertical" size={16} style={{ width: '100%' }}>
            <Card title="Step 1 — Downloading to JFrog">
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

            <Card title="Step 2 — Configuring Mirror to Quay">
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

            {failures.data?.failures?.length ? (
              <Card title="What failed" styles={{ header: { color: '#C4262E' } }}>
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
              <Descriptions.Item label="Strategy">
                <Tooltip
                  title={
                    t?.strategy === 'copy'
                      ? 'Our workers moved these bytes, so every figure here is measured.'
                      : 'The destination registry moved these bytes. We did not count them, so no byte figure is shown.'
                  }
                >
                  {t?.strategy ? <Tag>{t.strategy}</Tag> : <NA />}
                </Tooltip>
              </Descriptions.Item>
              <Descriptions.Item label="Reference">
                <Typography.Text style={{ fontFamily: mono, fontSize: 11 }} copyable>
                  <Value>{t?.id}</Value>
                </Typography.Text>
              </Descriptions.Item>
            </Descriptions>

            {product.data && (
              <Space direction="vertical" size={4} style={{ width: '100%', marginTop: 12 }}>
                <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                  Where this download landed
                </Typography.Text>
                {/*
                  This download's OWN target, not every target the product has.
                  Listing them all here would read as "the release reached all
                  of these", which is a claim about work that has not run.
                */}
                {product.data.targets
                  .filter((target) => target.name === t?.targetName)
                  .map((target) => (
                    <RepoLink
                      key={target.name}
                      label={target.name}
                      url={`https://${target.registry.replace(/^https?:\/\//, '')}/${target.repository ?? ''}`}
                    />
                  ))}
                {!product.data.targets.some((target) => target.name === t?.targetName) && (
                  <Value>{t?.targetName}</Value>
                )}
              </Space>
            )}
          </Card>
        </Col>
      </Row>
    </>
  )
}
