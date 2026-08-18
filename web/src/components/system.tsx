import { Card, Progress, Space, Table, Tag, Tooltip, Typography } from 'antd'
import { Link } from 'react-router-dom'
import { useVersion, useWorkers } from '../api/queries'
import { formatCount } from '../domain/format'
import { NA, Value } from './value'
import { TimeAgo } from './chips'
import { semantic } from '../theme'
import type { Worker } from '../api/types'

/**
 * The worker fleet, on the page where somebody would first notice it missing.
 *
 * # Why this belongs on the Overview
 *
 * "Nothing is downloading" has two completely different causes — there is
 * nothing to download, or there is nobody to download it. The Coordinator
 * plans transfers and workers move the bytes, so a fleet of zero means every
 * download sits at zero per cent forever with nothing on the page saying why.
 *
 * # What each number means
 *
 * `activeJobs` is what the WORKER says it is running; `leasedJobs` is what the
 * Coordinator has handed it. The two disagreeing is worth seeing rather than
 * averaging away: a worker holding jobs the Coordinator has already reaped is
 * about to report completions nobody will accept.
 *
 * STALE is derived from the heartbeat rather than announced — a worker that
 * was killed never got to say so.
 */
export function SystemPanel() {
  const workers = useWorkers()
  const version = useVersion()

  const fleet = workers.data?.workers ?? []
  const active = fleet.filter((w) => w.state === 'ACTIVE')
  const stale = fleet.filter((w) => w.state === 'STALE')

  const capacity = active.reduce((n, w) => n + (w.maxConcurrency ?? 0), 0)
  const running = active.reduce((n, w) => n + (w.activeJobs ?? 0), 0)
  const utilisation = capacity > 0 ? (running / capacity) * 100 : undefined

  return (
    <Card
      title={
        <Space size={8}>
          System
          {stale.length > 0 && (
            <Tag color="red">{stale.length} stale</Tag>
          )}
        </Space>
      }
      extra={<Link to="/settings">View health</Link>}
      loading={workers.isLoading}
    >
      {fleet.length === 0 ? (
        <Typography.Text type="secondary" style={{ fontSize: 13 }}>
          No worker has reported in. The Coordinator plans downloads and workers move the bytes, so
          nothing will transfer until at least one is running.
        </Typography.Text>
      ) : (
        <>
          <Space size={24} wrap style={{ marginBottom: 10 }}>
            <Typography.Text style={{ fontSize: 13 }}>
              <Typography.Text strong>{active.length}</Typography.Text>{' '}
              {active.length === 1 ? 'worker' : 'workers'} running
            </Typography.Text>
            <Typography.Text type="secondary" style={{ fontSize: 13 }}>
              <Value>{formatCount(running)}</Value> of <Value>{formatCount(capacity)}</Value> slots in use
            </Typography.Text>
          </Space>

          {/*
            A LEVEL, not a journey. This is how much of the fleet's capacity is
            in use right now, and it only earns a bar while something is
            actually running.

            It used to render unconditionally, which meant an idle fleet showed
            a bar pinned at 0% — and a progress bar that never moves reads as
            stalled work rather than as no work. So idle is stated in words,
            and the bar appears when there is something for it to measure.
          */}
          {running > 0 && utilisation !== undefined ? (
            <Tooltip title={`${running} jobs running across ${active.length} workers, out of ${capacity} configured slots.`}>
              <div style={{ marginBottom: 4 }}>
                <Typography.Text type="secondary" style={{ fontSize: 11 }}>
                  Capacity in use
                </Typography.Text>
                <Progress
                  percent={Number(utilisation.toFixed(0))}
                  size="small"
                  strokeColor={utilisation > 90 ? semantic.warning : undefined}
                />
              </div>
            </Tooltip>
          ) : (
            <Typography.Text type="secondary" style={{ fontSize: 12, display: 'block', marginBottom: 8 }}>
              No jobs running.
            </Typography.Text>
          )}

          <Table<Worker>
            size="small"
            pagination={false}
            dataSource={fleet}
            rowKey={(w) => w.workerId}
            showHeader={false}
            columns={[
              {
                title: 'Worker',
                render: (_, w) => (
                  <Space direction="vertical" size={0}>
                    <Typography.Text style={{ fontSize: 12 }} className="mono">
                      {w.workerId}
                    </Typography.Text>
                    <Typography.Text type="secondary" style={{ fontSize: 11 }}>
                      {w.version ? `v${w.version}` : <NA />} · last heard <TimeAgo at={w.lastHeartbeat} />
                    </Typography.Text>
                  </Space>
                ),
              },
              {
                title: 'Load',
                width: 70,
                align: 'right',
                render: (_, w) => (
                  <Typography.Text style={{ fontSize: 12 }}>
                    <Value>{formatCount(w.activeJobs)}</Value>/<Value>{formatCount(w.maxConcurrency)}</Value>
                  </Typography.Text>
                ),
              },
              {
                title: 'State',
                width: 90,
                render: (_, w) => (
                  <Tooltip
                    title={
                      w.state === 'STALE'
                        ? 'This worker stopped sending heartbeats. Its jobs return to the queue automatically.'
                        : w.state === 'DRAINING'
                          ? 'Finishing what it holds and taking no new work.'
                          : 'Leasing and running jobs normally.'
                    }
                  >
                    <Tag
                      color={w.state === 'ACTIVE' ? 'green' : w.state === 'STALE' ? 'red' : 'orange'}
                      style={{ marginInlineEnd: 0 }}
                    >
                      {w.state}
                    </Tag>
                  </Tooltip>
                ),
              },
            ]}
          />
        </>
      )}

      <Typography.Text type="secondary" style={{ fontSize: 11, display: 'block', marginTop: 10 }}>
        Coordinator <Value>{version.data?.version}</Value>
        {version.data?.component ? ` · ${version.data.component}` : ''}
      </Typography.Text>
    </Card>
  )
}
