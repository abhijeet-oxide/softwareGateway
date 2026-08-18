import { useState } from 'react'
import { Button, Card, Space, Table, Tag, Tooltip, Typography } from 'antd'
import { CheckCircleFilled, CloseCircleFilled, SafetyOutlined, ThunderboltOutlined } from '@ant-design/icons'
import { useConnectivity, useProducts } from '../api/queries'
import { useCan } from '../auth/permissions'
import { RepoLink } from '../components/chips'
import { Value } from '../components/value'
import { formatCount } from '../domain/format'
import { EmptyStateCard, ErrorState, PageHeader } from '../components/layout'
import { semantic } from '../theme'
import type { Repository } from '../api/types'

/**
 * Page 7 — Repositories.
 *
 * Answers: where is our software stored, and are those places healthy?
 *
 * Its real job is answering "is it us or is it them" at the start of an
 * incident, which is why the connectivity result names the step that failed —
 * credential, TLS, DNS, proxy — rather than reporting a red dot.
 *
 * No credential value appears here, ever. Status only.
 */

interface Row {
  product: string
  repo: Repository
  kind: 'Source' | 'Target'
}

export default function Repositories() {
  const products = useProducts()
  const [checking, setChecking] = useState(false)
  const connectivity = useConnectivity(checking)
  const mayOperate = useCan('operate')

  const rows: Row[] = []
  for (const p of products.data?.products ?? []) {
    for (const s of p.sources ?? []) rows.push({ product: p.productId, repo: s, kind: 'Source' })
    for (const t of p.targets ?? []) rows.push({ product: p.productId, repo: t, kind: 'Target' })
  }

  const resultFor = (product: string, name: string) =>
    connectivity.data?.products
      ?.find((p) => p.product === product)
      ?.repositories?.find((r) => r.name === name)

  if (products.isError) {
    return (
      <>
        <PageHeader title="Repositories" description="Where our software is stored, and whether those places are healthy" />
        <ErrorState error={products.error} retry={() => void products.refetch()} />
      </>
    )
  }

  return (
    <>
      <PageHeader
        title="Repositories"
        description="Where our software is stored, and whether those places are healthy"
        extra={
          <Tooltip
            title={
              mayOperate
                ? 'Makes a real connection to every configured registry and reports what happened at each step.'
                : 'You do not have permission to run a connectivity check.'
            }
          >
            <Button
              type="primary"
              icon={<ThunderboltOutlined />}
              disabled={!mayOperate}
              loading={connectivity.isFetching}
              onClick={() => { setChecking(true); void connectivity.refetch() }}
            >
              Check connectivity
            </Button>
          </Tooltip>
        }
      />

      {!products.isLoading && rows.length === 0 ? (
        <EmptyStateCard
          title="No repositories are configured"
          explanation="Sources and targets are defined per product in Git. Once a product is configured, its registries appear here."
          action={<Button href="/products">View products</Button>}
        />
      ) : (
        <Card styles={{ body: { padding: 0 } }}>
          <Table<Row>
            loading={products.isLoading}
            dataSource={rows}
            rowKey={(r) => `${r.product}-${r.kind}-${r.repo.name}`}
            pagination={false}
            scroll={{ x: 1000 }}
            expandable={{
              expandedRowRender: (r) => {
                const result = resultFor(r.product, r.repo.name)
                return (
                  <Space direction="vertical" size={10} style={{ width: '100%' }}>
                    <Space size={16} wrap>
                      <Typography.Text type="secondary">
                        <SafetyOutlined /> Credential:{' '}
                        {r.repo.enabled ? 'configured' : 'not configured'}
                      </Typography.Text>
                      <Typography.Text type="secondary">
                        Concurrency: <Value>{formatCount(r.repo.concurrency?.perRegistry)}</Value> in flight
                        {r.repo.concurrency?.requestsPerSecond
                          ? `, ${r.repo.concurrency.requestsPerSecond}/s ceiling`
                          : ''}
                      </Typography.Text>
                    </Space>

                    {result?.steps?.length ? (
                      <Table
                        size="small"
                        pagination={false}
                        dataSource={result.steps}
                        rowKey={(s) => s.name}
                        columns={[
                          { title: 'Step', render: (_, s) => s.name },
                          {
                            title: 'Result',
                            width: 110,
                            render: (_, s) =>
                              s.status === 'OK' ? (
                                <Tag color="green">OK</Tag>
                              ) : s.status === 'SKIPPED' ? (
                                <Tag>Skipped</Tag>
                              ) : (
                                <Tag color="red">Failed</Tag>
                              ),
                          },
                          { title: 'Detail', render: (_, s) => <Value>{s.detail}</Value> },
                        ]}
                      />
                    ) : (
                      <Typography.Text type="secondary">
                        No connectivity result yet. Run a check to find out whether this registry is
                        reachable from here.
                      </Typography.Text>
                    )}

                    {result?.error && (
                      <Typography.Text type="danger">{result.error}</Typography.Text>
                    )}
                  </Space>
                )
              },
            }}
            columns={[
              {
                title: 'Repository',
                fixed: 'left',
                width: 180,
                render: (_, r) => (
                  <Space direction="vertical" size={0}>
                    <Typography.Text strong>{r.repo.name}</Typography.Text>
                    <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                      {r.product}
                    </Typography.Text>
                  </Space>
                ),
              },
              {
                title: 'Role',
                width: 120,
                render: (_, r) => (
                  <Space size={4} direction="vertical">
                    <Tag color={r.kind === 'Source' ? 'blue' : 'purple'}>{r.kind}</Tag>
                    {r.repo.environment && <Tag color={r.repo.environment === 'production' ? 'success' : 'default'}>{r.repo.environment}</Tag>}
                  </Space>
                ),
              },
              { title: 'Type', width: 110, render: (_, r) => r.repo.type },
              {
                title: 'Location',
                width: 300,
                render: (_, r) => (
                  <RepoLink
                    url={`https://${r.repo.registry.replace(/^https?:\/\//, '')}/${r.repo.repository ?? ''}`}
                  />
                ),
              },
              {
                title: 'Stores',
                width: 160,
                render: (_, r) =>
                  r.repo.repositoryDiscovery
                    ? 'Discovered from the catalog'
                    : `${r.repo.repositories?.length ?? (r.repo.repository ? 1 : 0)} repositories`,
              },
              {
                title: 'Health',
                width: 130,
                render: (_, r) => {
                  const result = resultFor(r.product, r.repo.name)
                  if (!result) {
                    return (
                      <Tooltip title="Nobody has checked this registry yet. That is not the same as it being unhealthy.">
                        <Typography.Text type="secondary">Not checked</Typography.Text>
                      </Tooltip>
                    )
                  }
                  return result.status === 'OK' ? (
                    <Space size={4}>
                      <CheckCircleFilled style={{ color: semantic.success }} />
                      <span>Reachable</span>
                    </Space>
                  ) : (
                    <Space size={4}>
                      <CloseCircleFilled style={{ color: semantic.error }} />
                      <span>{result.status === 'SKIPPED' ? 'Skipped' : 'Failed'}</span>
                    </Space>
                  )
                },
              },
              {
                title: 'Enabled',
                width: 100,
                render: (_, r) => (r.repo.enabled ? <Tag color="green">Yes</Tag> : <Tag>No</Tag>),
              },
            ]}
          />
        </Card>
      )}
    </>
  )
}
