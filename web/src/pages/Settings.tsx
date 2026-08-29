import { useState } from 'react'
import { Alert, Button, Card, Col, Descriptions, Row, Space, Table, Tag, Tooltip, Typography } from 'antd'
import { ThunderboltOutlined } from '../icons'
import { useDeepHealth, useProducts, useVersion, useWorkers } from '../api/queries'
import { useIdentity } from '../auth/permissions'
import { formatCount, formatRelative } from '../domain/format'
import { Value } from '../components/value'
import { ManagedInGit, TimeAgo } from '../components/chips'
import { ErrorState, PageHeader } from '../components/layout'
import { SpeedTest } from '../components/speedtest'
import { AppearanceSettings, c, mono, StatusPill } from '../uikit'

/**
 * Page 10 - Settings.
 *
 * Answers: how is this instance configured, and is it healthy?
 *
 * Everything configurable here is read-only and says why: configuration is
 * GitOps, and a write from this interface would create a second source of
 * truth that Flux reverts minutes later with nothing in any log to explain it
 * (docs/design/19 §4).
 *
 * The Users and roles section renders whatever /whoami reports rather than a
 * role model baked into this file - which is the point of that endpoint.
 */
export default function Settings() {
  const version = useVersion()
  const workers = useWorkers()
  const products = useProducts()
  const { who, loading } = useIdentity()
  const [probing, setProbing] = useState(false)
  const health = useDeepHealth(probing)

  if (version.isError) {
    return (
      <>
        <ErrorState error={version.error} retry={() => void version.refetch()} />
      </>
    )
  }

  const productList = products.data?.products ?? []

  return (
    <>
      <PageHeader
        extra={
          <Button
            icon={<ThunderboltOutlined />}
            loading={health.isFetching}
            onClick={() => { setProbing(true); void health.refetch() }}
          >
            Run health check
          </Button>
        }
      />

      <Row gutter={[16, 16]}>
        {/*
          Appearance, first, because it is the only thing on this page a person
          can actually change: everything below it is managed in Git and shown
          here to be read. The section is the shared design system's, copy
          included - two products that explain the same control in different
          words are two products.
        */}
        <Col xs={24}>
          <Card title="Appearance">
            <AppearanceSettings />
          </Card>
        </Col>

        <Col xs={24} xl={12}>
          <Card title="Users and roles" extra={<ManagedInGit />} loading={loading}>
            {who && !who.authenticated ? (
              <Alert
                type="info"
                showIcon
                message="Authentication is not enabled"
                description={
                  <Space direction="vertical" size={4}>
                    <Typography.Text>
                      This Coordinator accepts every caller as{' '}
                      <Typography.Text code>{who.subject}</Typography.Text> with full permissions.
                      The only control protecting it is network isolation.
                    </Typography.Text>
                    <Typography.Text type="secondary">
                      Everything this interface shows and does is already asked through a permission
                      check, so switching authentication on changes what people can do without
                      changing any page.
                    </Typography.Text>
                  </Space>
                }
              />
            ) : (
              <Descriptions column={1} size="small">
                <Descriptions.Item label="Signed in as"><Value>{who?.subject}</Value></Descriptions.Item>
                <Descriptions.Item label="Method"><Value>{who?.method}</Value></Descriptions.Item>
                <Descriptions.Item label="Tenant">{who?.tenant || 'All'}</Descriptions.Item>
                <Descriptions.Item label="Roles"><Value>{who?.roles?.join(', ')}</Value></Descriptions.Item>
              </Descriptions>
            )}

            <Descriptions column={1} size="small" style={{ marginTop: 12 }}>
              <Descriptions.Item label="Permissions">
                <Space size={4} wrap>
                  {(who?.permissions ?? []).map((p) => (
                    <Tag key={p}>{p === '*' ? 'everything' : p}</Tag>
                  ))}
                </Space>
              </Descriptions.Item>
              <Descriptions.Item label="Visible products">
                {who?.products?.length ? who.products.join(', ') : 'All products'}
              </Descriptions.Item>
            </Descriptions>
          </Card>
        </Col>

        <Col xs={24} xl={12}>
          <Card title="System health" loading={version.isLoading}>
            <Descriptions column={2} size="small">
              <Descriptions.Item label="Version">
                <span style={{ fontFamily: mono }}><Value>{version.data?.version}</Value></span>
              </Descriptions.Item>
              <Descriptions.Item label="Component"><Value>{version.data?.component}</Value></Descriptions.Item>
              <Descriptions.Item label="Commit">
                <span style={{ fontFamily: mono, fontSize: 11 }}><Value>{version.data?.commit}</Value></span>
              </Descriptions.Item>
              <Descriptions.Item label="Built"><Value>{version.data?.buildDate}</Value></Descriptions.Item>
            </Descriptions>

            {health.data ? (
              <Table
                size="small"
                style={{ marginTop: 12 }}
                pagination={false}
                dataSource={health.data.checks}
                rowKey={(c) => c.name}
                columns={[
                  { title: 'Dependency', render: (_, c) => c.name },
                  {
                    title: 'Result',
                    width: 110,
                    render: (_, c) =>
                      c.status === 'ok' || c.status === 'OK'
                        ? <StatusPill tone="ok">OK</StatusPill>
                        : <StatusPill tone="danger">{c.status}</StatusPill>,
                  },
                  { title: 'Detail', render: (_, c) => <Value>{c.detail}</Value> },
                ]}
              />
            ) : (
              <Typography.Text type="secondary" style={{ display: 'block', marginTop: 12 }}>
                Dependencies have not been probed. A health check makes real outbound calls, so it
                runs when asked for rather than on a timer.
              </Typography.Text>
            )}
          </Card>
        </Col>

        <Col xs={24} xl={12}>
          <Card title="Discovery and verification" extra={<ManagedInGit />} loading={products.isLoading}>
            <Table
              size="small"
              pagination={false}
              dataSource={productList}
              rowKey={(p) => p.productId}
              scroll={{ x: 560 }}
              columns={[
                { title: 'Product', render: (_, p) => p.displayName || p.productId },
                {
                  title: 'Discovery interval',
                  width: 150,
                  render: (_, p) => {
                    const seconds = p.sources?.[0]?.discovery?.intervalSeconds
                    return <Value>{seconds ? `${Math.round(seconds / 60)} minutes` : null}</Value>
                  },
                },
                {
                  title: 'Verification',
                  width: 190,
                  render: (_, p) =>
                    p.verification?.enabled ? (
                      <Space size={4} wrap>
                        <StatusPill tone="ok">{p.verification.policy || 'enabled'}</StatusPill>
                        {p.verification.atSource && <Tag>at source</Tag>}
                        {p.verification.atDestination && <Tag>at destination</Tag>}
                      </Space>
                    ) : (
                      <Tag style={{ marginInlineEnd: 0 }}>Disabled</Tag>
                    ),
                },
                {
                  title: 'Config hash',
                  width: 120,
                  render: (_, p) => (
                    <Tooltip title="Identifies the exact configuration document this instance loaded, so you can confirm which revision is in force.">
                      <span style={{ fontFamily: mono, fontSize: 11 }}><Value>{p.configHash?.slice(0, 12)}</Value></span>
                    </Tooltip>
                  ),
                },
              ]}
            />
          </Card>
        </Col>

        {/*
          IS THE SPEED WE ARE GETTING THE SPEED THIS PATH CAN DO?

          Full width, because the answer is two tables and a list of settings.
          Here rather than on a download's own page: it measures a PATH - one
          product's source to one of its targets - not a transfer, and running
          it from a download would imply it was measuring that download, which
          it cannot, because the download is using the connections it would be
          competing with.
        */}
        <Col span={24}>
          <SpeedTest />
        </Col>

        <Col xs={24} xl={12}>
          <Card title="Background workers" loading={workers.isLoading}>
            {(workers.data?.workers ?? []).length === 0 ? (
              <Typography.Text type="secondary">
                No worker has reported in. Downloads are planned by the Coordinator and performed by
                workers, so nothing will transfer until at least one is running.
              </Typography.Text>
            ) : (
              <Table
                size="small"
                pagination={false}
                dataSource={workers.data?.workers ?? []}
                rowKey={(w) => w.workerId}
                scroll={{ x: 520 }}
                columns={[
                  {
                    title: 'Worker',
                    render: (_, w) => <span style={{ fontFamily: mono, fontSize: 12 }}>{w.workerId}</span>,
                  },
                  {
                    title: 'State',
                    width: 110,
                    render: (_, w) => (
                      <Tag color={w.state === 'ACTIVE' ? 'green' : w.state === 'OFFLINE' ? 'red' : 'orange'}>
                        {w.state}
                      </Tag>
                    ),
                  },
                  {
                    title: 'Load',
                    width: 110,
                    render: (_, w) => `${formatCount(w.activeJobs) ?? 0} / ${formatCount(w.maxConcurrency) ?? 0}`,
                  },
                  {
                    title: 'Last heard',
                    width: 120,
                    render: (_, w) => <TimeAgo at={w.lastHeartbeat} />,
                  },
                ]}
              />
            )}
            {workers.data?.workers?.some((w) => w.state === 'STALE') && (
              <Typography.Text style={{ color: c.danger, fontSize: 12 }}>
                A stale worker stopped sending heartbeats. Its jobs are returned to the queue
                automatically; it will not need intervention unless it stays stale.
              </Typography.Text>
            )}
          </Card>
        </Col>

        <Col span={24}>
          <Alert
            type="info"
            showIcon
            message="This interface never edits configuration"
            description={
              <Typography.Text type="secondary">
                Products, downloads, rules, discovery intervals and verification policy are defined in
                Git and reconciled into the cluster. A change made here would be silently reverted
                within minutes, so it is shown and not offered. Requesting work - downloads,
                promotions, syncs - is not configuration and is fully available.
                {productList[0]?.configHash && (
                  <> Loaded configuration: <span style={{ fontFamily: mono }}>{productList[0].configHash.slice(0, 12)}</span>{' '}
                  ({formatRelative(new Date().toISOString())}).</>
                )}
              </Typography.Text>
            }
          />
        </Col>
      </Row>
    </>
  )
}
