import { useState } from 'react'
import { Card, Col, Row, Segmented, Select, Space, Statistic, Table, Tag, Tooltip, Typography } from 'antd'
import { useProducts, useReports } from '../api/queries'
import { bytes, formatBytes, formatCount, formatPercent, formatSpeed, UNKNOWN } from '../domain/format'
import { EmptyStateCard, ErrorState, PageHeader } from '../components/layout'
import { semantic } from '../theme'

/**
 * Page 9 — Reports.
 *
 * Answers: how is this operating over time?
 *
 * Operational metrics, not decorative charts. Every figure states its period,
 * which the server echoes back so a reader never assumes one.
 *
 * Two absences are deliberate and both are the honest-numbers rule:
 *   - AVERAGE DOWNLOAD SPEED is the performance metric, and average download
 *     TIME appears nowhere. Time measures how big the releases were.
 *   - There are no verification figures, because nothing writes verification
 *     records yet. A zero would read as "nothing failed" rather than "nothing
 *     was checked".
 */
export default function Reports() {
  const [period, setPeriod] = useState('30d')
  const [product, setProduct] = useState<string>()

  const products = useProducts()
  const reports = useReports({ period, product })
  const totals = reports.data?.totals

  if (reports.isError) {
    return (
      <>
        <PageHeader title="Reports" description="How this is operating over time" />
        <ErrorState error={reports.error} retry={() => void reports.refetch()} />
      </>
    )
  }

  const settled = (totals?.downloadsCompleted ?? 0) + (totals?.downloadsFailed ?? 0)
  const volume = reports.data?.volume ?? []
  const peak = Math.max(1, ...volume.map((v) => bytes(v.bytesTransferred) ?? 0))

  return (
    <>
      <PageHeader
        title="Reports"
        description="How this is operating over time"
        meta={
          <Space>
            <Select
              style={{ minWidth: 170 }}
              placeholder="All products"
              allowClear
              value={product}
              onChange={setProduct}
              loading={products.isLoading}
              options={(products.data?.products ?? []).map((p) => ({
                value: p.productId, label: p.displayName || p.productId,
              }))}
            />
            <Segmented
              value={period}
              onChange={(v) => setPeriod(String(v))}
              options={[
                { value: '7d', label: 'Last 7 days' },
                { value: '30d', label: 'Last 30 days' },
                { value: '90d', label: 'Last 90 days' },
              ]}
            />
          </Space>
        }
      />

      {!reports.isLoading && settled === 0 && (totals?.downloadsRunning ?? 0) === 0 ? (
        <EmptyStateCard
          title="Nothing has been downloaded in this period"
          explanation="These figures are computed from downloads that completed within the period shown. Widen the period, or start a download to produce something to measure."
          action={<Typography.Text type="secondary">Period: {reports.data?.period.label ?? period}</Typography.Text>}
        />
      ) : (
        <Row gutter={[16, 16]}>
          <Col xs={24} xl={16}>
            <Card
              title="Performance"
              extra={
                <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                  {reports.data?.period.label ? `Last ${reports.data.period.label.replace('d', ' days')}` : ''}
                </Typography.Text>
              }
              loading={reports.isLoading}
            >
              <Row gutter={[16, 16]}>
                <Col xs={12} md={6}>
                  <Tooltip title="Measured over downloads whose bytes our workers moved. Downloads performed by the destination registry are excluded, because we did not count those bytes.">
                    <Statistic
                      title="Average download speed"
                      value={totals?.averageBytesPerSecond ? formatSpeed(totals.averageBytesPerSecond) : UNKNOWN}
                    />
                  </Tooltip>
                  {!totals?.averageBytesPerSecond && (
                    <Typography.Text type="secondary" style={{ fontSize: 11 }}>
                      No download we timed completed in this period.
                    </Typography.Text>
                  )}
                </Col>
                <Col xs={12} md={6}>
                  <Statistic title="Total data downloaded" value={formatBytes(totals?.bytesTransferred)} />
                </Col>
                <Col xs={12} md={6}>
                  <Statistic
                    title="Saved (already present)"
                    value={formatBytes(totals?.savedBytes)}
                    valueStyle={{ color: semantic.success }}
                  />
                  <Typography.Text type="secondary" style={{ fontSize: 11 }}>
                    {totals?.savedPercent !== undefined
                      ? `${formatPercent(totals.savedPercent)} of everything in play`
                      : 'Nothing was in play to save against.'}
                  </Typography.Text>
                </Col>
                <Col xs={12} md={6}>
                  <Statistic
                    title="Success rate"
                    value={totals?.successRate !== undefined ? formatPercent(totals.successRate * 100) : UNKNOWN}
                  />
                  <Typography.Text type="secondary" style={{ fontSize: 11 }}>
                    {settled > 0 ? `${formatCount(settled)} downloads settled` : 'Nothing settled in this period.'}
                  </Typography.Text>
                </Col>
              </Row>
            </Card>
          </Col>

          <Col xs={24} xl={8}>
            <Card title="Downloads" loading={reports.isLoading}>
              <Row gutter={16}>
                <Col span={8}><Statistic title="Completed" value={totals?.downloadsCompleted ?? 0} /></Col>
                <Col span={8}>
                  <Statistic
                    title="Failed"
                    value={totals?.downloadsFailed ?? 0}
                    valueStyle={{ color: (totals?.downloadsFailed ?? 0) > 0 ? semantic.error : undefined }}
                  />
                </Col>
                <Col span={8}><Statistic title="Promoted" value={totals?.promotions ?? 0} /></Col>
              </Row>
            </Card>
          </Col>

          <Col xs={24} xl={16}>
            <Card title="Download volume by day" loading={reports.isLoading}>
              {volume.length === 0 ? (
                <Typography.Text type="secondary">
                  No download completed on any day in this period.
                </Typography.Text>
              ) : (
                <Space direction="vertical" size={6} style={{ width: '100%' }}>
                  {volume.map((v) => {
                    const value = bytes(v.bytesTransferred) ?? 0
                    return (
                      <div key={v.day} style={{ display: 'flex', alignItems: 'center', gap: 12 }}>
                        <span style={{ width: 88, fontSize: 12, color: '#5A6675' }}>{v.day}</span>
                        <div style={{ flex: 1, background: '#EEF2F6', borderRadius: 3, height: 18 }}>
                          <div
                            style={{
                              width: `${(value / peak) * 100}%`,
                              background: '#0057B8',
                              height: '100%',
                              borderRadius: 3,
                              minWidth: value > 0 ? 2 : 0,
                            }}
                          />
                        </div>
                        <span style={{ width: 90, textAlign: 'right', fontSize: 12 }}>
                          {formatBytes(value)}
                        </span>
                        <span style={{ width: 70, textAlign: 'right', fontSize: 12, color: '#5A6675' }}>
                          {formatCount(v.downloads)} dl
                        </span>
                      </div>
                    )
                  })}
                </Space>
              )}
            </Card>
          </Col>

          <Col xs={24} xl={8}>
            <Card title="Failures by cause" loading={reports.isLoading}>
              {(reports.data?.failureCauses ?? []).length === 0 ? (
                <Typography.Text type="secondary">
                  No download failed in this period.
                </Typography.Text>
              ) : (
                <Table
                  size="small"
                  pagination={false}
                  dataSource={reports.data?.failureCauses ?? []}
                  rowKey={(c) => `${c.product}-${c.class}`}
                  columns={[
                    { title: 'Cause', render: (_, c) => <Tag>{c.class}</Tag> },
                    { title: 'Product', width: 110, render: (_, c) => c.product },
                    { title: 'Artifacts', width: 90, align: 'right', render: (_, c) => formatCount(c.jobs) },
                  ]}
                />
              )}
            </Card>
          </Col>

          <Col span={24}>
            <Card title="By product" loading={reports.isLoading} styles={{ body: { padding: 0 } }}>
              <Table
                size="small"
                pagination={false}
                dataSource={reports.data?.products ?? []}
                rowKey={(p) => p.product}
                scroll={{ x: 900 }}
                columns={[
                  { title: 'Product', fixed: 'left', width: 150, render: (_, p) => p.product },
                  { title: 'Completed', width: 110, align: 'right', render: (_, p) => formatCount(p.totals.downloadsCompleted) },
                  { title: 'Failed', width: 90, align: 'right', render: (_, p) => formatCount(p.totals.downloadsFailed) },
                  { title: 'Promoted', width: 100, align: 'right', render: (_, p) => formatCount(p.totals.promotions) },
                  { title: 'Downloaded', width: 120, align: 'right', render: (_, p) => formatBytes(p.totals.bytesTransferred) },
                  { title: 'Saved', width: 120, align: 'right', render: (_, p) => formatBytes(p.totals.savedBytes) },
                  {
                    title: 'Avg speed',
                    width: 120,
                    align: 'right',
                    render: (_, p) =>
                      p.totals.averageBytesPerSecond ? formatSpeed(p.totals.averageBytesPerSecond) : (
                        <Tooltip title="No download whose bytes we moved completed for this product in this period.">
                          <Typography.Text type="secondary">{UNKNOWN}</Typography.Text>
                        </Tooltip>
                      ),
                  },
                ]}
              />
            </Card>
          </Col>
        </Row>
      )}
    </>
  )
}
