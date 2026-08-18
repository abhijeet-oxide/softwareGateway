import { useMemo } from 'react'
import { Button, Card, Col, Row, Space, Statistic, Table, Tooltip, Typography } from 'antd'
import { CloudDownloadOutlined, PlayCircleOutlined } from '@ant-design/icons'
import { Link, useNavigate } from 'react-router-dom'
import { useProducts, usePackages, useReports, useRunDiscovery, useTransfers } from '../api/queries'
import { useCan } from '../auth/permissions'
import {
  deriveLocations, deriveStatus, downloadSeconds, transferIndex, verification, version,
  withTransfers, type SoftwareStatus,
} from '../domain/derive'
import { formatBytes, formatCount, formatDuration, formatSpeed, UNKNOWN } from '../domain/format'
import {
  LocationChip, ProductChip, StatusBadge, TimeAgo, VerificationBadge, VersionChip,
} from '../components/chips'
import { AttentionBand, EmptyStateCard, ErrorState, PageHeader, type Attention } from '../components/layout'
import type { Package, Product } from '../api/types'

/**
 * Page 1 — Overview.
 *
 * Answers: what new software is available, what is downloading, and what needs
 * my attention?
 *
 * The Latest Software table is the centre of the page and the largest thing on
 * it, because that is the answer. "Saved" is deliberately NOT a card here — it
 * belongs to a release, not to the estate.
 */

interface Row {
  pkg: Package
  product: Product
  status: SoftwareStatus
}

export default function Overview() {
  const navigate = useNavigate()
  const mayOperate = useCan('operate')
  const discover = useRunDiscovery()

  const products = useProducts()
  const productList = products.data?.products ?? []

  // One request per product. The API has no estate-wide package listing, and
  // inventing one client-side by fetching everything and sorting is exactly
  // what "no client-side aggregation of anything the API can aggregate"
  // forbids — so this is bounded by the product count, which is 5–50.
  const first = usePackages(productList[0]?.productId, { pageSize: 20 })
  const second = usePackages(productList[1]?.productId, { pageSize: 20 })
  const third = usePackages(productList[2]?.productId, { pageSize: 20 })

  const transfers = useTransfers({ pageSize: 100 })
  const reports = useReports({ period: '7d' })

  const rows = useMemo<Row[]>(() => {
    // A package listing carries no transfer history, so the status is joined in
    // from the transfer listing rather than derived from the package alone —
    // otherwise every row reads NEW, including releases already in production.
    const index = transferIndex(transfers.data?.transfers ?? [])
    const out: Row[] = []
    const sources = [first, second, third]
    productList.slice(0, 3).forEach((product, i) => {
      for (const listed of sources[i]?.data?.packages ?? []) {
        const pkg = withTransfers(listed, index)
        out.push({ pkg, product, status: deriveStatus(pkg, product) })
      }
    })
    return out
      .sort((a, b) =>
        (b.pkg.publishedAt || b.pkg.discoveredAt).localeCompare(
          a.pkg.publishedAt || a.pkg.discoveredAt))
      .slice(0, 10)
  }, [productList, first.data, second.data, third.data, transfers.data])

  const counts = useMemo(() => {
    const all = rows.map((r) => r.status)
    return {
      new: all.filter((s) => s === 'NEW').length,
      downloading: (transfers.data?.transfers ?? []).filter(
        (t) => t.state === 'RUNNING' || t.state === 'PLANNING' || t.state === 'READY').length,
      downloaded: all.filter((s) => s === 'DOWNLOADED').length,
      readyForProduction: all.filter((s) => s === 'READY FOR PRODUCTION').length,
      verificationIssues: rows.filter(
        (r) => verification(r.pkg) === 'VERIFICATION_FAILED').length,
    }
  }, [rows, transfers.data])

  const attention = useMemo<Attention[]>(() => {
    const items: Attention[] = []
    for (const t of transfers.data?.transfers ?? []) {
      if (t.state !== 'FAILED') continue
      items.push({
        key: t.id,
        severity: 'error',
        message: `${t.product} ${t.displayTag || t.tag} — download failed`,
        detail: t.failureReason
          ? `${t.failureReason} The download is stopped. Open it to see what failed and retry from where it stopped.`
          : 'The download stopped before it finished. Open it to see what failed and retry from where it stopped.',
        action: { label: 'Open download', to: `/downloads/${t.id}` },
      })
    }
    for (const r of rows) {
      if (verification(r.pkg) !== 'VERIFICATION_FAILED') continue
      items.push({
        key: `verify-${r.pkg.packageId}`,
        severity: 'error',
        message: `${r.product.productId} ${version(r.pkg)} — verification failed`,
        detail: 'The vendor signature did not verify. Do not promote this release until it is explained.',
        action: {
          label: 'View release',
          to: `/software/${encodeURIComponent(r.product.productId)}/${encodeURIComponent(r.pkg.tag)}`,
        },
      })
    }
    return items
  }, [transfers.data, rows])

  if (products.isError) {
    return (
      <>
        <PageHeader title="Overview" description="Track and manage software from vendor to production" />
        <ErrorState error={products.error} retry={() => void products.refetch()} />
      </>
    )
  }

  const loading = products.isLoading || first.isLoading
  const totals = reports.data?.totals

  return (
    <>
      <PageHeader
        title="Overview"
        description="Track and manage software from vendor to production"
        extra={
          <Tooltip title={mayOperate ? undefined : 'You do not have permission to run discovery.'}>
            <Button
              type="primary"
              icon={<PlayCircleOutlined />}
              disabled={!mayOperate}
              loading={discover.isPending}
              onClick={() => discover.mutate(undefined)}
            >
              Run Discovery
            </Button>
          </Tooltip>
        }
      />

      <AttentionBand items={attention} />

      <Row gutter={[16, 16]} style={{ marginBottom: 16 }}>
        {[
          { title: 'New Software', value: counts.new, to: '/software?status=NEW' },
          { title: 'Downloading', value: counts.downloading, to: '/downloads' },
          { title: 'Downloaded', value: counts.downloaded, to: '/software?status=DOWNLOADED' },
          { title: 'Production Ready', value: counts.readyForProduction, to: '/software?status=READY' },
          { title: 'Verification Issues', value: counts.verificationIssues, to: '/software?verification=failed' },
        ].map((card) => (
          <Col key={card.title} flex="1 1 190px">
            <Card
              hoverable
              size="small"
              onClick={() => navigate(card.to)}
              style={{ cursor: 'pointer' }}
            >
              <Statistic title={card.title} value={card.value} loading={loading} />
            </Card>
          </Col>
        ))}
      </Row>

      <Row gutter={[16, 16]}>
        <Col xs={24} xl={17}>
          <Card
            title="Latest Software"
            extra={<Link to="/software">View all software</Link>}
            styles={{ body: { padding: 0 } }}
          >
            {!loading && rows.length === 0 ? (
              <div style={{ padding: 24 }}>
                <EmptyStateCard
                  title="No software discovered yet"
                  explanation="Discovery polls the vendor registries on a schedule. Run it now to look immediately."
                  action={
                    <Button
                      type="primary"
                      disabled={!mayOperate}
                      loading={discover.isPending}
                      onClick={() => discover.mutate(undefined)}
                    >
                      Run Discovery
                    </Button>
                  }
                />
              </div>
            ) : (
              <Table<Row>
                loading={loading}
                dataSource={rows}
                rowKey={(r) => r.pkg.packageId}
                pagination={false}
                size="middle"
                scroll={{ x: 1180 }}
                columns={[
                  {
                    title: 'Product',
                    fixed: 'left',
                    width: 130,
                    render: (_, r) => <ProductChip name={r.product.productId} display={r.product.displayName} />,
                  },
                  {
                    title: 'Version',
                    width: 110,
                    render: (_, r) => (
                      <VersionChip product={r.product.productId} version={version(r.pkg)} reference={r.pkg.tag} />
                    ),
                  },
                  {
                    title: 'Published',
                    width: 110,
                    render: (_, r) => <TimeAgo at={r.pkg.publishedAt || r.pkg.discoveredAt} />,
                  },
                  {
                    title: 'Verified',
                    width: 140,
                    render: (_, r) => <VerificationBadge state={verification(r.pkg)} />,
                  },
                  {
                    title: 'Status',
                    width: 200,
                    render: (_, r) => <StatusBadge status={r.status} />,
                  },
                  {
                    title: 'Location',
                    width: 180,
                    render: (_, r) => <LocationChip locations={deriveLocations(r.pkg, r.product)} />,
                  },
                  {
                    title: 'Download time',
                    width: 120,
                    render: (_, r) => {
                      const s = downloadSeconds(r.pkg)
                      return s === undefined ? (
                        <Tooltip title="This release has not been downloaded, so there is no time to report.">
                          <Typography.Text type="secondary">{UNKNOWN}</Typography.Text>
                        </Tooltip>
                      ) : (
                        formatDuration(s)
                      )
                    },
                  },
                  {
                    title: 'Actions',
                    fixed: 'right',
                    width: 130,
                    render: (_, r) =>
                      r.status === 'NEW' ? (
                        <Link to={`/software/${encodeURIComponent(r.product.productId)}/${encodeURIComponent(r.pkg.tag)}`}>
                          <Button size="small" type="primary" icon={<CloudDownloadOutlined />}>
                            Download
                          </Button>
                        </Link>
                      ) : (
                        <Link to={`/software/${encodeURIComponent(r.product.productId)}/${encodeURIComponent(r.pkg.tag)}`}>
                          <Button size="small">View Details</Button>
                        </Link>
                      ),
                  },
                ]}
              />
            )}
          </Card>
        </Col>

        <Col xs={24} xl={7}>
          <Space direction="vertical" size={16} style={{ width: '100%' }}>
            <Card title="Products" extra={<Link to="/products">View all</Link>} styles={{ body: { padding: 0 } }}>
              <Table
                loading={products.isLoading}
                dataSource={productList}
                rowKey={(p) => p.productId}
                pagination={false}
                size="small"
                columns={[
                  {
                    title: 'Product',
                    render: (_, p) => <ProductChip name={p.productId} display={p.displayName} />,
                  },
                  {
                    title: 'Targets',
                    align: 'right',
                    render: (_, p) => formatCount(p.targets?.length ?? 0),
                  },
                  {
                    title: 'Auto-download',
                    align: 'right',
                    render: (_, p) => (p.autoDownload?.enabled ? 'On' : 'Off'),
                  },
                ]}
              />
            </Card>

            <Card title="Download Performance" extra={<Link to="/reports">View report</Link>}>
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                Last 7 days
              </Typography.Text>
              <Row gutter={16} style={{ marginTop: 12 }}>
                <Col span={12}>
                  <Statistic
                    title="Average download speed"
                    value={
                      totals?.averageBytesPerSecond
                        ? formatSpeed(totals.averageBytesPerSecond)
                        : UNKNOWN
                    }
                    loading={reports.isLoading}
                  />
                  {!totals?.averageBytesPerSecond && !reports.isLoading && (
                    <Typography.Text type="secondary" style={{ fontSize: 11 }}>
                      No download whose bytes we moved completed in this period.
                    </Typography.Text>
                  )}
                </Col>
                <Col span={12}>
                  <Statistic
                    title="Total data downloaded"
                    value={formatBytes(totals?.bytesTransferred)}
                    loading={reports.isLoading}
                  />
                </Col>
              </Row>
            </Card>
          </Space>
        </Col>
      </Row>
    </>
  )
}
