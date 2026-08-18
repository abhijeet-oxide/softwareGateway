import { useState } from 'react'
import {
  App, Alert, Button, Card, Col, Modal, Row, Select, Space, Table, Tag, Tooltip, Typography,
} from 'antd'
import { ArrowRightOutlined, CloudDownloadOutlined } from '@ant-design/icons'
import { Link } from 'react-router-dom'
import {
  useAutoDownloadRules, useDownloads, usePackages, useProducts, useReplication,
  useRunDownload, useTransfers,
} from '../api/queries'
import { useCan } from '../auth/permissions'
import { isLive, transferVersion, version } from '../domain/derive'
import { bytes, formatBytes, formatDuration } from '../domain/format'
import { NA, Value } from '../components/value'
import { elapsedSeconds } from '../domain/format'
import { ManagedInGit, ProductChip, TimeAgo, TransferStateTag } from '../components/chips'
import { EmptyStateCard, ErrorState, PageHeader } from '../components/layout'
import { MeasuredProgress } from '../components/progress'
import { PriorityControl, QueueControls } from '../components/queuecontrols'
import { mono } from '../theme'
import type { RunDownloadResponse, Transfer } from '../api/types'

/**
 * Page 6 — Downloads.
 *
 * Answers: how does software get in, and what comes in automatically?
 *
 * # The distinction the whole page is built on
 *
 * A DOWNLOAD is what happens: where software goes, in what order, and what has
 * to verify on the way. It carries no pattern.
 *
 * An AUTO-DOWNLOAD RULE is when it happens by itself: a version pattern and
 * the name of the download it triggers. It performs nothing of its own.
 *
 * A rule fires the same download a person runs by hand. If this page makes
 * that look like two mechanisms, it has failed — so the download panel shows a
 * chain and never a pattern, and a rule row shows a pattern and never a chain
 * of its own.
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

/**
 * The repository a package came from, without the registry host.
 *
 * The API returns a transfer's source as `host/path`, which is right for one
 * transfer and far too wide for a column of them: the host is the same on
 * every row and the path is the only part that identifies anything.
 */
function repositoryOf(source: string | undefined): string {
  if (!source) return ''
  const slash = source.indexOf('/')
  return slash < 0 ? source : source.slice(slash + 1)
}

export default function Downloads() {
  const { message } = App.useApp()
  const products = useProducts()
  const productList = products.data?.products ?? []
  const [selected, setSelected] = useState<string>()
  const product = selected ?? productList[0]?.productId

  const downloads = useDownloads(product)
  const rules = useAutoDownloadRules(product)
  const replication = useReplication(product)
  const packages = usePackages(product, { pageSize: 50 })
  // ESTATE-WIDE, not per product. "What is downloading" is a question about
  // the whole instance — a download of another product is still one of the
  // things occupying the workers — and scoping it to whichever product the
  // configuration cards below happen to be showing would hide exactly the row
  // somebody came here to find.
  const transfers = useTransfers({ pageSize: 100 })
  const all = transfers.data?.transfers ?? []
  const ongoing = all.filter((t) => isLive(t.state) || t.state === 'PAUSED')
  const finished = all.filter((t) => !isLive(t.state) && t.state !== 'PAUSED')

  const mayOperate = useCan('operate', { product })
  const runDownload = useRunDownload(product!)

  const [picking, setPicking] = useState(false)
  const [tags, setTags] = useState<string[]>([])
  const [preview, setPreview] = useState<RunDownloadResponse>()

  const drifted = (replication.data?.replication ?? []).filter((r) => r.drift?.drifted)

  const runPreview = async () => {
    try {
      setPreview(await runDownload.mutateAsync({ tags, validateOnly: true }))
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'The plan could not be rendered.')
    }
  }

  const run = async () => {
    try {
      const result = await runDownload.mutateAsync({ tags })
      setPicking(false)
      setPreview(undefined)
      setTags([])
      message.success(
        `${result.created?.length ?? 0} download(s) started` +
        (result.alreadyRequested?.length ? `, ${result.alreadyRequested.length} already requested` : ''),
      )
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'The download could not be started.')
    }
  }

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
        description="How software gets in, and what comes in automatically"
        meta={
          <Select
            style={{ minWidth: 180 }}
            loading={products.isLoading}
            value={product}
            onChange={setSelected}
            options={productList.map((p) => ({ value: p.productId, label: p.displayName || p.productId }))}
          />
        }
        extra={
          <Tooltip
            title={
              mayOperate
                ? 'Downloads software you name. There is no pattern here — patterns belong to rules.'
                : 'You do not have permission to start a download.'
            }
          >
            <Button
              type="primary"
              icon={<CloudDownloadOutlined />}
              disabled={!mayOperate}
              onClick={() => setPicking(true)}
            >
              Download…
            </Button>
          </Tooltip>
        }
      />

      {drifted.length > 0 && (
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: 16 }}
          message="The registry's configuration has drifted from what Git says"
          description={
            <Space direction="vertical" size={4}>
              {drifted.map((d) => (
                <Typography.Text key={d.target}>
                  <strong>{d.target}</strong>:{' '}
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
                action={<Link to="/software"><Button type="primary">Find a package to download</Button></Link>}
              />
            ) : (
              <Table<Transfer>
                size="small"
                pagination={false}
                dataSource={ongoing}
                rowKey={(t) => t.id}
                scroll={{ x: 1120 }}
                columns={[
                  { title: 'Product', width: 130, render: (_, t) => <ProductChip name={t.product} /> },
                  {
                    // WHICH package, not just which version. A vendor
                    // publishes one version tag into every repository of a
                    // product, so two rows reading `25.7_mp2604_2131` are two
                    // different packages and the repository is what says so.
                    title: 'Package',
                    width: 200,
                    render: (_, t) => (
                      <Tooltip title={t.source}>
                        <Typography.Text style={{ fontFamily: mono, fontSize: 12 }} ellipsis>
                          {repositoryOf(t.source)}
                        </Typography.Text>
                      </Tooltip>
                    ),
                  },
                  {
                    title: 'Version',
                    width: 170,
                    render: (_, t) => (
                      <Link to={`/downloads/${t.id}`} style={{ fontFamily: mono }}>
                        {transferVersion(t)}
                      </Link>
                    ),
                  },
                  { title: 'State', width: 130, render: (_, t) => <TransferStateTag state={t.state} /> },
                  {
                    title: 'Elapsed',
                    width: 90,
                    align: 'right',
                    render: (_, t) => (
                      <Value reason="No worker has taken a job yet, so nothing has been timed.">
                        {formatDuration(elapsedSeconds(t.startedAt))}
                      </Value>
                    ),
                  },
                  {
                    title: 'Progress',
                    width: 200,
                    render: (_, t) => (
                      <MeasuredProgress
                        transferred={bytes(t.progress?.bytesTransferred)}
                        total={bytes(t.progress?.plannedBytes)}
                        strategy={t.strategy ?? 'copy'}
                      />
                    ),
                  },
                  { title: 'Priority', width: 130, align: 'right', render: (_, t) => <PriorityControl transfer={t} /> },
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
                action={<Link to="/software"><Button>Find a package to download</Button></Link>}
              />
            ) : (
              <Table<Transfer>
                size="small"
                pagination={false}
                dataSource={finished.slice(0, 10)}
                rowKey={(t) => t.id}
                scroll={{ x: 1000 }}
                columns={[
                  { title: 'Product', width: 130, render: (_, t) => <ProductChip name={t.product} /> },
                  {
                    title: 'Version',
                    width: 180,
                    render: (_, t) => (
                      <Link to={`/downloads/${t.id}`} style={{ fontFamily: mono }}>
                        {transferVersion(t)}
                      </Link>
                    ),
                  },
                  { title: 'State', width: 130, render: (_, t) => <TransferStateTag state={t.state} /> },
                  {
                    title: 'Took',
                    width: 90,
                    align: 'right',
                    render: (_, t) => (
                      <Value>{formatDuration(elapsedSeconds(t.startedAt, t.completedAt))}</Value>
                    ),
                  },
                  { title: 'Downloaded', width: 110, align: 'right', render: (_, t) => <Value>{formatBytes(bytes(t.progress?.bytesTransferred))}</Value> },
                  { title: 'When', width: 100, render: (_, t) => <TimeAgo at={t.completedAt || t.createdAt} /> },
                  { title: 'Actions', width: 150, render: (_, t) => <QueueControls transfer={t} /> },
                ]}
              />
            )}
          </Card>
        </Col>

        <Col span={24}>
          <Card
            title="How a download runs"
            // The product picker lives HERE rather than in the page header,
            // because it scopes this card and the two below it and nothing
            // else: what is downloading right now is an estate-wide question
            // and a header control implying otherwise would be a lie about
            // the page.
            extra={
              <Space>
                <Select
                  size="small"
                  style={{ minWidth: 180 }}
                  loading={products.isLoading}
                  value={product}
                  onChange={setSelected}
                  options={productList.map((p) => ({ value: p.productId, label: p.displayName || p.productId }))}
                />
                <Tooltip
                  title={
                    mayOperate
                      ? 'Downloads software you name. There is no pattern here — patterns belong to rules.'
                      : 'You do not have permission to start a download.'
                  }
                >
                  <Button
                    size="small"
                    icon={<CloudDownloadOutlined />}
                    disabled={!mayOperate}
                    onClick={() => setPicking(true)}
                  >
                    Download…
                  </Button>
                </Tooltip>
                <ManagedInGit />
              </Space>
            }
            loading={downloads.isLoading}
          >
            <Typography.Paragraph type="secondary" style={{ fontSize: 13 }}>
              What happens when this product is downloaded — where the software goes, in what order,
              and what has to verify on the way.
            </Typography.Paragraph>
            <Table
              size="small"
              pagination={false}
              dataSource={downloads.data?.downloads ?? []}
              rowKey={(d) => d.name || 'default'}
              scroll={{ x: 800 }}
              columns={[
                {
                  title: 'Name',
                  width: 140,
                  render: (_, d) => d.name || <Typography.Text type="secondary">(default)</Typography.Text>,
                },
                { title: 'Chain', render: (_, d) => (d.chainError ? <Typography.Text type="danger">{d.chainError}</Typography.Text> : chainFlow(d.chain)) },
                {
                  title: 'Verification gates',
                  width: 260,
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

        <Col xs={24} xl={14}>
          <Card
            title="Auto-download rules"
            extra={<ManagedInGit />}
            loading={rules.isLoading}
          >
            <Typography.Paragraph type="secondary" style={{ fontSize: 13 }}>
              When a download happens by itself. A rule matches a version pattern and triggers the
              download above — it performs nothing of its own.
              {rules.data && !rules.data.enabled && ' Automatic firing is switched off for this product.'}
            </Typography.Paragraph>

            {!rules.isLoading && (rules.data?.rules ?? []).length === 0 ? (
              <EmptyStateCard
                title="No auto-download rules"
                explanation="Nothing is downloaded automatically for this product. Rules are defined in Git; adding one there will show it here."
                action={<Button onClick={() => setPicking(true)} disabled={!mayOperate}>Download by hand instead</Button>}
              />
            ) : (
              <Table
                size="small"
                pagination={false}
                dataSource={rules.data?.rules ?? []}
                rowKey={(r) => r.name}
                scroll={{ x: 600 }}
                columns={[
                  { title: 'Rule', width: 130, render: (_, r) => r.name },
                  {
                    title: 'Matches versions',
                    width: 170,
                    render: (_, r) => <span style={{ fontFamily: mono, fontSize: 12 }}>{r.tagPattern}</span>,
                  },
                  {
                    title: 'Triggers',
                    width: 140,
                    render: (_, r) => r.download || <Typography.Text type="secondary">(default)</Typography.Text>,
                  },
                  {
                    title: 'State',
                    width: 190,
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

      <Modal
        open={picking}
        title="Download software"
        width={620}
        okText={preview ? 'Run this download' : 'Preview'}
        okButtonProps={{ disabled: tags.length === 0 }}
        confirmLoading={runDownload.isPending}
        onOk={() => void (preview ? run() : runPreview())}
        onCancel={() => { setPicking(false); setPreview(undefined) }}
      >
        <Typography.Paragraph type="secondary">
          Choose the versions to download. This asks for software by name — there is no pattern
          here, because a pattern decides what to download when nobody is asking.
        </Typography.Paragraph>

        <Select
          mode="multiple"
          style={{ width: '100%' }}
          placeholder="Choose one or more versions"
          value={tags}
          onChange={(v) => { setTags(v); setPreview(undefined) }}
          loading={packages.isLoading}
          options={(packages.data?.packages ?? []).map((p) => ({
            value: p.tag,
            label: version(p),
          }))}
        />

        {preview && (
          <Card size="small" style={{ marginTop: 16 }} title="What will happen">
            <Space direction="vertical" size={8} style={{ width: '100%' }}>
              {chainFlow(preview.chain)}
              <Typography.Text type="secondary">
                {preview.requested?.length ?? 0} release(s) resolved
                {preview.alreadyRequested?.length
                  ? `, ${preview.alreadyRequested.length} already requested and will not be started again`
                  : ''}.
              </Typography.Text>
            </Space>
          </Card>
        )}
      </Modal>
    </>
  )
}
