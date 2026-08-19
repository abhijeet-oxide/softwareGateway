import { Alert, Button, Card, Col, Row, Space, Table, Tag, Tooltip, Typography } from 'antd'
import { ArrowRightOutlined } from '@ant-design/icons'
import { Link } from 'react-router-dom'
import {
  useDownloadsForAll, useProducts, useReplicationForAll, useRulesForAll, useTransfers,
} from '../api/queries'
import { isLive, repositoryOf, transferVersion } from '../domain/derive'
import { bytes, elapsedSeconds, formatBytes, formatDuration } from '../domain/format'
import { NA, Value } from '../components/value'
import { ManagedInGit, TimeAgo, TransferStateTag } from '../components/chips'
import { EmptyStateCard, ErrorState, PageHeader } from '../components/layout'
import { DownloadProgress } from '../components/progress'
import { PriorityControl, QueueControls } from '../components/queuecontrols'
import { mono } from '../theme'
import type { AutoDownloadRuleView, DownloadView, ReplicationView, Transfer } from '../api/types'

/**
 * Page 6 — Downloads.
 *
 * Answers: what is downloading right now, what has finished, and what comes in
 * automatically?
 *
 * # This page starts nothing
 *
 * A download is started from the package being downloaded, where the reader
 * can see what they are asking for. Here they are watching, and a page that
 * both watches and acts had a product selector at the top that scoped only
 * half of it — the ongoing list is estate-wide, and a selector above it
 * implied otherwise.
 *
 * # The distinction the configuration half is built on
 *
 * A DOWNLOAD is what happens: where software goes, in what order, and what has
 * to verify on the way. It carries no pattern.
 *
 * An AUTO-DOWNLOAD RULE is when it happens by itself: a version pattern and
 * the name of the download it triggers. It performs nothing of its own.
 *
 * A rule fires the same download a person runs by hand. If this page makes
 * that look like two mechanisms, it has failed.
 *
 * # And what this page cannot do
 *
 * Change configuration. Git owns it and Flux reconciles it, so a write here
 * would be reverted minutes later with nothing in any log to explain it
 * (docs/design/19 §4). There is no enable/disable toggle on a rule, because
 * there is nothing behind one.
 */

function chainFlow(chain: string[] | undefined) {
  if (!chain?.length) return <NA reason="This download names no targets, so there is no chain to resolve." />
  return (
    <Space size={4} wrap>
      {chain.map((step, i) => (
        <span key={`${step}-${i}`}>
          {i > 0 && <ArrowRightOutlined style={{ fontSize: 10, color: '#98A2B3', margin: '0 4px' }} />}
          <Tag style={{ marginInlineEnd: 0 }}>{step}</Tag>
        </span>
      ))}
    </Space>
  )
}

/** A download's package, ellipsised, with the full path on the tooltip. */
function PackageCell({ source, width = 200 }: { source?: string; width?: number }) {
  const name = repositoryOf(source)
  if (!name) return <NA />
  return (
    <Tooltip title={source}>
      <Typography.Text style={{ fontFamily: mono, fontSize: 12, maxWidth: width }} ellipsis={{ tooltip: false }}>
        {name}
      </Typography.Text>
    </Tooltip>
  )
}

/** One product's row in a configuration table. */
type WithProduct<T> = T & { product: string }

export default function Downloads() {
  const products = useProducts()
  const productList = (products.data?.products ?? []).filter((p) => p.enabled)
  const names = productList.map((p) => p.productId)

  const transfers = useTransfers({ pageSize: 100 })
  const downloadsPerProduct = useDownloadsForAll(names)
  const rulesPerProduct = useRulesForAll(names)
  const replicationPerProduct = useReplicationForAll(names)

  const all = transfers.data?.transfers ?? []
  const ongoing = all.filter((t) => isLive(t.state) || t.state === 'PAUSED')
  const finished = all.filter((t) => !isLive(t.state) && t.state !== 'PAUSED')

  // Flattened with the product each row belongs to, since the tables now cover
  // the estate rather than one product at a time.
  const downloads: WithProduct<DownloadView>[] = downloadsPerProduct.flatMap((q, i) =>
    (q.data?.downloads ?? []).map((d) => ({ ...d, product: names[i]! })))
  const rules: AutoDownloadRuleView[] = rulesPerProduct.flatMap((q) => q.data?.rules ?? [])
  const drifted: ReplicationView[] = replicationPerProduct.flatMap(
    (q) => (q.data?.replication ?? []).filter((r) => r.drift?.drifted))

  if (products.isError) {
    return (
      <>
        <PageHeader title="Downloads" description="What is downloading now, and what comes in automatically" />
        <ErrorState error={products.error} retry={() => void products.refetch()} />
      </>
    )
  }

  return (
    <>
      <PageHeader
        title="Downloads"
        description="What is downloading now, and what comes in automatically"
      />

      {drifted.length > 0 && (
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: 16 }}
          message="A registry's configuration has drifted from what Git says"
          description={
            <Space direction="vertical" size={4}>
              {drifted.map((d) => (
                <Typography.Text key={`${d.product}-${d.target}`}>
                  <strong>{d.product} · {d.target}</strong>:{' '}
                  {d.drift?.fields?.map((f) => `${f.field} is ${f.observed}, Git says ${f.desired}`).join('; ')
                    || d.drift?.reason}
                </Typography.Text>
              ))}
              <Typography.Text type="secondary">
                Applying pushes what Git already says into the registry. It changes nothing in Git.
              </Typography.Text>
            </Space>
          }
        />
      )}

      <Row gutter={[16, 16]}>
        <Col span={24}>
          <Card
            title={`Ongoing downloads${ongoing.length ? ` (${ongoing.length})` : ''}`}
            loading={transfers.isLoading}
            styles={{ body: { padding: ongoing.length ? 0 : undefined } }}
          >
            {!transfers.isLoading && ongoing.length === 0 ? (
              <EmptyStateCard
                title="Nothing is downloading"
                explanation="Downloads started by hand or by an auto-download rule appear here while they run, with what has moved so far. Finished ones are listed below."
                action={<Link to="/packages"><Button type="primary">Find a package to download</Button></Link>}
              />
            ) : (
              <Table<Transfer>
                size="small"
                pagination={false}
                dataSource={ongoing}
                rowKey={(t) => t.id}
                scroll={{ x: 1180 }}
                columns={[
                  { title: 'Product', width: 140, render: (_, t) => t.product },
                  { title: 'Package', width: 210, render: (_, t) => <PackageCell source={t.source} /> },
                  {
                    title: 'Version',
                    width: 160,
                    render: (_, t) => (
                      <Link to={`/downloads/${t.id}`} style={{ fontFamily: mono }}>
                        {transferVersion(t)}
                      </Link>
                    ),
                  },
                  { title: 'State', width: 130, render: (_, t) => <TransferStateTag state={t.state} /> },
                  {
                    // How far, how long, and how much longer — one cell,
                    // because each of the three is misleading without the
                    // other two.
                    title: 'Progress',
                    width: 220,
                    render: (_, t) => (
                      <DownloadProgress
                        // Distinct content, the same axis the download page
                        // draws: bytes weighed once however many repositories
                        // they reach, because the second copy is a mount.
                        transferred={bytes(t.progress?.contentMovedBytes)
                          ?? bytes(t.progress?.bytesTransferred)}
                        total={bytes(t.progress?.contentBytes)
                          ?? bytes(t.progress?.plannedBytes)}
                        saved={bytes(t.progress?.contentPresentBytes)
                          ?? bytes(t.progress?.savedBytes)}
                        strategy={t.strategy ?? 'copy'}
                        elapsedSeconds={elapsedSeconds(t.startedAt)}
                        live={isLive(t.state)}
                      />
                    ),
                  },
                  { title: 'Priority', width: 90, align: 'right', render: (_, t) => <PriorityControl transfer={t} /> },
                  { title: 'Actions', width: 190, render: (_, t) => <QueueControls transfer={t} /> },
                ]}
              />
            )}
          </Card>
        </Col>

        <Col span={24}>
          <Card title="Recent downloads" loading={transfers.isLoading}>
            <Typography.Paragraph type="secondary" style={{ fontSize: 13 }}>
              Downloads that have finished — succeeded, failed or stopped. Anything still running is
              in the card above.
            </Typography.Paragraph>
            {!transfers.isLoading && finished.length === 0 ? (
              <EmptyStateCard
                title="No download has finished yet"
                explanation="Once a download completes it is listed here with what it cost and how long it took."
                action={<Link to="/packages"><Button>Find a package to download</Button></Link>}
              />
            ) : (
              <Table<Transfer>
                size="small"
                pagination={{ pageSize: 10, hideOnSinglePage: true, size: 'small' }}
                dataSource={finished}
                rowKey={(t) => t.id}
                scroll={{ x: 1080 }}
                columns={[
                  { title: 'Product', width: 140, render: (_, t) => t.product },
                  { title: 'Package', width: 210, render: (_, t) => <PackageCell source={t.source} /> },
                  {
                    title: 'Version',
                    width: 160,
                    render: (_, t) => (
                      <Link to={`/downloads/${t.id}`} style={{ fontFamily: mono }}>
                        {transferVersion(t)}
                      </Link>
                    ),
                  },
                  { title: 'State', width: 130, render: (_, t) => <TransferStateTag state={t.state} /> },
                  {
                    title: 'Time',
                    width: 90,
                    align: 'right',
                    render: (_, t) => (
                      <Value>{formatDuration(elapsedSeconds(t.startedAt, t.completedAt))}</Value>
                    ),
                  },
                  {
                    title: 'Downloaded',
                    width: 110,
                    align: 'right',
                    render: (_, t) => <Value>{formatBytes(bytes(t.progress?.bytesTransferred))}</Value>,
                  },
                  { title: 'When', width: 100, render: (_, t) => <TimeAgo at={t.completedAt || t.createdAt} /> },
                  { title: 'Actions', width: 150, render: (_, t) => <QueueControls transfer={t} /> },
                ]}
              />
            )}
          </Card>
        </Col>

        <Col span={24}>
          <Card title="How a download runs" extra={<ManagedInGit />} loading={downloadsPerProduct.some((q) => q.isLoading)}>
            <Typography.Paragraph type="secondary" style={{ fontSize: 13 }}>
              What happens when a product is downloaded — where the software goes, in what order,
              and what has to verify on the way.
            </Typography.Paragraph>
            <Table<WithProduct<DownloadView>>
              size="small"
              pagination={false}
              dataSource={downloads}
              rowKey={(d) => `${d.product}-${d.name || 'default'}`}
              scroll={{ x: 900 }}
              columns={[
                { title: 'Product', width: 150, render: (_, d) => d.product },
                {
                  title: 'Download',
                  width: 140,
                  render: (_, d) => d.name || <Typography.Text type="secondary">(default)</Typography.Text>,
                },
                {
                  title: 'Chain',
                  render: (_, d) => (d.chainError
                    ? <Typography.Text type="danger">{d.chainError}</Typography.Text>
                    : chainFlow(d.chain)),
                },
                {
                  title: 'Verification gates',
                  width: 250,
                  render: (_, d) => (
                    <Space size={4} wrap>
                      <Tooltip title="Whether the vendor signature must verify before the software is brought in.">
                        <Tag color={d.verifyBefore === 'true' ? 'green' : undefined}>
                          Before: {d.verifyBefore ?? 'inherit'}
                        </Tag>
                      </Tooltip>
                      <Tooltip title="Whether the signature must verify at the destination before the next step runs.">
                        <Tag color={d.verifyAfter === 'true' ? 'green' : undefined}>
                          After: {d.verifyAfter ?? 'inherit'}
                        </Tag>
                      </Tooltip>
                    </Space>
                  ),
                },
                { title: 'Priority', width: 90, align: 'right', render: (_, d) => d.priority },
              ]}
            />
          </Card>
        </Col>

        <Col span={24}>
          <Card title="Auto-download rules" extra={<ManagedInGit />} loading={rulesPerProduct.some((q) => q.isLoading)}>
            <Typography.Paragraph type="secondary" style={{ fontSize: 13 }}>
              When a download happens by itself. A rule matches a version pattern and triggers one of
              the downloads above — it performs nothing of its own.
            </Typography.Paragraph>

            {!rulesPerProduct.some((q) => q.isLoading) && rules.length === 0 ? (
              <EmptyStateCard
                title="No auto-download rules"
                explanation="Nothing is downloaded automatically. Rules are defined in Git; adding one there will show it here."
                action={<Link to="/packages"><Button>Download a package by hand</Button></Link>}
              />
            ) : (
              <Table<AutoDownloadRuleView>
                size="small"
                pagination={false}
                dataSource={rules}
                rowKey={(r) => `${r.product}-${r.name}`}
                scroll={{ x: 800 }}
                columns={[
                  { title: 'Product', width: 150, render: (_, r) => r.product },
                  { title: 'Rule', width: 150, render: (_, r) => r.name },
                  {
                    title: 'Matches versions',
                    width: 190,
                    render: (_, r) => <span style={{ fontFamily: mono, fontSize: 12 }}>{r.tagPattern}</span>,
                  },
                  {
                    title: 'Triggers',
                    width: 150,
                    render: (_, r) => r.download || <Typography.Text type="secondary">(default)</Typography.Text>,
                  },
                  {
                    title: 'State',
                    width: 130,
                    render: (_, r) =>
                      r.enabled ? (
                        <Tag color="green">Enabled</Tag>
                      ) : (
                        <Tooltip title="A rule is turned off in Git and nowhere else. There is no runtime override, so there is no toggle here.">
                          <Tag style={{ marginInlineEnd: 0 }}>Disabled</Tag>
                        </Tooltip>
                      ),
                  },
                ]}
              />
            )}
          </Card>
        </Col>
      </Row>
    </>
  )
}
