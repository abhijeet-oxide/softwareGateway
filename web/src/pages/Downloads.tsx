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
import { formatDuration } from '../domain/format'
import { NA, Value } from '../components/value'
import { elapsedSeconds } from '../domain/format'
import { ManagedInGit, TimeAgo } from '../components/chips'
import { EmptyStateCard, ErrorState, PageHeader } from '../components/layout'
import { PriorityControl, QueueControls } from '../components/queuecontrols'
import { mono } from '../theme'
import type { RunDownloadResponse } from '../api/types'

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
  const transfers = useTransfers({ product, pageSize: 25 })

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
        <PageHeader title="Downloads" description="How software gets in, and what comes in automatically" />
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
            title="Download"
            extra={<ManagedInGit />}
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

        <Col xs={24} xl={10}>
          <Card title="Recent downloads" loading={transfers.isLoading}>
            <Table
              size="small"
              pagination={false}
              dataSource={(transfers.data?.transfers ?? []).slice(0, 8)}
              rowKey={(t) => t.id}
              columns={[
                {
                  title: 'Version',
                  render: (_, t) => (
                    <Link to={`/downloads/${t.id}`} style={{ fontFamily: mono }}>
                      {transferVersion(t)}
                    </Link>
                  ),
                },
                { title: 'State', width: 110, render: (_, t) => <Tag color={t.state === 'SUCCEEDED' ? 'green' : t.state === 'FAILED' ? 'red' : 'blue'}>{t.state}</Tag> },
                {
                  title: 'Took',
                  width: 90,
                  align: 'right',
                  render: (_, t) =>
                    isLive(t.state) ? (
                      <Value>{formatDuration(elapsedSeconds(t.startedAt))}</Value>
                    ) : (
                      <Value>{formatDuration(elapsedSeconds(t.startedAt, t.completedAt))}</Value>
                    ),
                },
                {
                  // The queue's own order, and the only way to change it
                  // without stopping anything. A download waiting behind work
                  // that matters less is the commonest complaint this page
                  // gets, and pausing the thing in front is not the answer:
                  // it stops work rather than reordering it.
                  title: 'Priority',
                  width: 130,
                  align: 'right',
                  render: (_, t) => <PriorityControl transfer={t} />,
                },
                { title: 'When', width: 90, render: (_, t) => <TimeAgo at={t.completedAt || t.createdAt} /> },
                {
                  title: 'Actions',
                  width: 180,
                  render: (_, t) => <QueueControls transfer={t} />,
                },
              ]}
            />
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
