import { useState } from 'react'
import {
  App, Button, Card, Col, Drawer, Progress, Row, Select, Space, Statistic, Table, Tabs, Tag, Typography,
} from 'antd'
import { SwapOutlined } from '@ant-design/icons'
import { useSearchParams } from 'react-router-dom'
import { useCompare, usePackages, useProduct, useProducts } from '../api/queries'
import { version } from '../domain/derive'
import { bytes, formatBytes, formatPercent } from '../domain/format'
import { Value } from '../components/value'
import { ErrorState, PageHeader } from '../components/layout'
import { mono, semantic } from '../theme'
import type { CompareRow } from '../api/types'

/**
 * Page 5 — Compare.
 *
 * Answers: what is different between these two, exactly?
 *
 * The path high-level difference → artifact → actual file diff must be
 * traversable in three clicks and reversible, so the diff opens in a drawer
 * over the summary rather than navigating away from it.
 */

const CHANGE_COLOUR: Record<string, string> = {
  ADDED: 'green',
  REMOVED: 'red',
  CHANGED: 'orange',
  UNCHANGED: 'default',
}

export default function Compare() {
  const [params] = useSearchParams()
  const { message } = App.useApp()

  const products = useProducts()
  const productList = products.data?.products ?? []
  const [product, setProduct] = useState(params.get('product') ?? undefined)
  const selectedProduct = product ?? productList[0]?.productId

  const detail = useProduct(selectedProduct)
  const packages = usePackages(selectedProduct, { pageSize: 100 })

  const [left, setLeft] = useState(params.get('from') ?? undefined)
  const [right, setRight] = useState<string>()
  const [leftEndpoint, setLeftEndpoint] = useState<string>()
  const [rightEndpoint, setRightEndpoint] = useState<string>()
  const [openRow, setOpenRow] = useState<CompareRow>()

  const compare = useCompare()
  const report = compare.data

  const endpoints = [
    { value: '', label: 'Vendor (where it was discovered)' },
    ...(detail.data?.targets ?? []).map((t) => ({
      value: t.name,
      label: `${t.name}${t.environment ? ` · ${t.environment}` : ''}`,
    })),
  ]

  const run = async () => {
    if (!selectedProduct || !left || !right) return
    try {
      await compare.mutateAsync({
        product: selectedProduct,
        ref: left,
        body: {
          to: right,
          fromEndpoint: leftEndpoint || undefined,
          toEndpoint: rightEndpoint || undefined,
          // Ask for real file-level differences rather than layer-level ones.
          fileBudget: 64 * 1024 * 1024,
        },
      })
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'The comparison could not be run.')
    }
  }

  const swap = () => {
    setLeft(right); setRight(left)
    setLeftEndpoint(rightEndpoint); setRightEndpoint(leftEndpoint)
  }

  const total = report ? report.added + report.removed + report.changed + report.unchanged : 0
  const pct = (n: number) => (total > 0 ? (n / total) * 100 : undefined)

  const byKind = (kind: string) => (report?.rows ?? []).filter((r) => r.kind.toLowerCase().includes(kind))

  const rowsTable = (rows: CompareRow[]) => (
    <Table
      size="small"
      dataSource={rows}
      rowKey={(r) => `${r.kind}-${r.name}`}
      pagination={{ pageSize: 12, hideOnSinglePage: true }}
      scroll={{ x: 700 }}
      columns={[
        { title: 'Name', render: (_, r) => <span style={{ fontFamily: mono, fontSize: 12 }}>{r.name}</span> },
        { title: 'Type', width: 110, render: (_, r) => r.kind },
        {
          title: 'Change',
          width: 120,
          render: (_, r) => <Tag color={CHANGE_COLOUR[r.change] ?? 'default'}>{r.change}</Tag>,
        },
        { title: 'Size', width: 110, align: 'right', render: (_, r) => <Value>{formatBytes(r.b?.sizeBytes ?? r.a?.sizeBytes)}</Value> },
        {
          title: 'Action',
          width: 90,
          render: (_, r) =>
            r.change === 'UNCHANGED' ? null : (
              <Button size="small" onClick={() => setOpenRow(r)}>View</Button>
            ),
        },
      ]}
    />
  )

  if (products.isError) {
    return (
      <>
        <PageHeader title="Compare Software" description="What is different between two versions or locations" />
        <ErrorState error={products.error} retry={() => void products.refetch()} />
      </>
    )
  }

  return (
    <>
      <PageHeader
        title="Compare Software"
        description="Compare two versions or locations of the same software"
      />

      <Card style={{ marginBottom: 16 }}>
        <Row gutter={16} align="bottom">
          <Col xs={24} md={7}>
            <Typography.Text type="secondary">Left (Source)</Typography.Text>
            <Space direction="vertical" size={8} style={{ width: '100%', marginTop: 6 }}>
              <Select
                style={{ width: '100%' }}
                placeholder="Location"
                value={leftEndpoint ?? ''}
                onChange={setLeftEndpoint}
                options={endpoints}
              />
              <Select
                style={{ width: '100%' }}
                placeholder="Version"
                value={left}
                onChange={setLeft}
                loading={packages.isLoading}
                options={(packages.data?.packages ?? []).map((p) => ({ value: p.tag, label: version(p) }))}
              />
            </Space>
          </Col>

          <Col xs={24} md={2} style={{ textAlign: 'center' }}>
            <Button icon={<SwapOutlined />} onClick={swap} aria-label="Swap sides">Swap</Button>
          </Col>

          <Col xs={24} md={7}>
            <Typography.Text type="secondary">Right (Target)</Typography.Text>
            <Space direction="vertical" size={8} style={{ width: '100%', marginTop: 6 }}>
              <Select
                style={{ width: '100%' }}
                placeholder="Location"
                value={rightEndpoint ?? ''}
                onChange={setRightEndpoint}
                options={endpoints}
              />
              <Select
                style={{ width: '100%' }}
                placeholder="Version"
                value={right}
                onChange={setRight}
                loading={packages.isLoading}
                options={(packages.data?.packages ?? []).map((p) => ({ value: p.tag, label: version(p) }))}
              />
            </Space>
          </Col>

          <Col xs={24} md={5}>
            <Space direction="vertical" size={8} style={{ width: '100%' }}>
              <Select
                style={{ width: '100%' }}
                value={selectedProduct}
                onChange={setProduct}
                loading={products.isLoading}
                options={productList.map((p) => ({ value: p.productId, label: p.displayName || p.productId }))}
              />
              <Button
                type="primary"
                block
                disabled={!left || !right}
                loading={compare.isPending}
                onClick={() => void run()}
              >
                Compare
              </Button>
            </Space>
          </Col>
        </Row>
      </Card>

      {compare.isError && <ErrorState error={compare.error} retry={() => void run()} />}

      {report && (
        <Row gutter={[16, 16]}>
          <Col xs={24} xl={14}>
            <Card title="High level summary">
              <Row gutter={16}>
                <Col span={6}><Statistic title="New" value={report.added} valueStyle={{ color: semantic.success }} /></Col>
                <Col span={6}><Statistic title="Removed" value={report.removed} valueStyle={{ color: semantic.error }} /></Col>
                <Col span={6}><Statistic title="Changed" value={report.changed} valueStyle={{ color: semantic.warning }} /></Col>
                <Col span={6}><Statistic title="Unchanged" value={report.unchanged} /></Col>
              </Row>
              <Space size={16} style={{ marginTop: 12 }}>
                <Typography.Text type="secondary">
                  New <Value>{formatPercent(pct(report.added))}</Value> ·
                  Removed <Value>{formatPercent(pct(report.removed))}</Value> ·
                  Changed <Value>{formatPercent(pct(report.changed))}</Value>
                </Typography.Text>
              </Space>
              {report.truncated && (
                <Typography.Paragraph type="warning" style={{ marginTop: 8, marginBottom: 0, fontSize: 12 }}>
                  The comparison reached its download budget, so some file-level differences were not
                  resolved. What is shown is accurate; it is not complete.
                </Typography.Paragraph>
              )}
            </Card>
          </Col>

          <Col xs={24} xl={10}>
            <Card title="Size comparison">
              <Space direction="vertical" size={10} style={{ width: '100%' }}>
                <div>
                  <Typography.Text type="secondary">{report.a.reference}</Typography.Text>
                  <Progress
                    percent={100}
                    showInfo={false}
                    strokeColor="#0057B8"
                  />
                  <Typography.Text><Value>{formatBytes(report.aTotalBytes)}</Value></Typography.Text>
                </div>
                <div>
                  <Typography.Text type="secondary">{report.b.reference}</Typography.Text>
                  <Progress
                    percent={
                      bytes(report.aTotalBytes) && bytes(report.bTotalBytes)
                        ? Math.min(100, (bytes(report.bTotalBytes)! / bytes(report.aTotalBytes)!) * 100)
                        : 0
                    }
                    showInfo={false}
                    strokeColor="#98A2B3"
                  />
                  <Typography.Text><Value>{formatBytes(report.bTotalBytes)}</Value></Typography.Text>
                </div>
              </Space>
            </Card>
          </Col>

          <Col span={24}>
            <Card styles={{ body: { paddingTop: 8 } }}>
              <Tabs
                items={[
                  { key: 'all', label: `Differences (${report.added + report.removed + report.changed})`,
                    children: rowsTable(report.rows.filter((r) => r.change !== 'UNCHANGED')) },
                  { key: 'images', label: 'Images', children: rowsTable(byKind('image')) },
                  { key: 'charts', label: 'Helm Charts', children: rowsTable(byKind('chart')) },
                  { key: 'files', label: 'Files', children: rowsTable(byKind('file')) },
                  { key: 'everything', label: 'Everything', children: rowsTable(report.rows) },
                ]}
              />
            </Card>
          </Col>
        </Row>
      )}

      <Drawer
        open={Boolean(openRow)}
        onClose={() => setOpenRow(undefined)}
        width={720}
        title={openRow?.name}
      >
        {openRow && (
          <Space direction="vertical" size={16} style={{ width: '100%' }}>
            <Tag color={CHANGE_COLOUR[openRow.change]}>{openRow.change}</Tag>

            <Row gutter={16}>
              {([['Left', openRow.a], ['Right', openRow.b]] as const).map(([label, side]) => (
                <Col span={12} key={label}>
                  <Card size="small" title={label}>
                    <Space direction="vertical" size={2} style={{ width: '100%' }}>
                      <Typography.Text type="secondary" style={{ fontSize: 12 }}>Digest</Typography.Text>
                      <Typography.Text style={{ fontFamily: mono, fontSize: 11 }}>
                        <Value>{side?.digest}</Value>
                      </Typography.Text>
                      <Typography.Text type="secondary" style={{ fontSize: 12 }}>Size</Typography.Text>
                      <Typography.Text><Value>{formatBytes(side?.sizeBytes)}</Value></Typography.Text>
                      <Typography.Text type="secondary" style={{ fontSize: 12 }}>Media type</Typography.Text>
                      <Typography.Text style={{ fontSize: 12 }}><Value>{side?.mediaType}</Value></Typography.Text>
                    </Space>
                  </Card>
                </Col>
              ))}
            </Row>

            {openRow.detail ? (
              <Card size="small" title="Difference">
                <div style={{ maxHeight: 400, overflow: 'auto' }}>
                  {openRow.detail.split('\n').map((line, i) => (
                    <div
                      key={i}
                      className={
                        `diff-line ${line.startsWith('+') ? 'diff-add' : line.startsWith('-') ? 'diff-remove' : ''}`
                      }
                    >
                      {line}
                    </div>
                  ))}
                </div>
              </Card>
            ) : (
              <Typography.Text type="secondary">
                No line-level difference is available for this artifact — it is a binary, or its
                content was outside the comparison's download budget. The metadata above is what
                differs.
              </Typography.Text>
            )}
          </Space>
        )}
      </Drawer>
    </>
  )
}
