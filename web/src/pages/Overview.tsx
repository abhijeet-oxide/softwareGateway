import { useMemo } from 'react'
import { Button, Card, Col, Row, Space, Statistic, Table, Typography } from 'antd'
import { CloudDownloadOutlined } from '@ant-design/icons'
import { Link, useNavigate } from 'react-router-dom'
import { useProducts, usePackagesByProducts, useReports, useTransfers } from '../api/queries'
import {
  deriveLocations, deriveStatus, downloadSeconds, isRecent, publishedAt, releaseHref, transferIndex,
  failureReason, verification, version, withTransfers, type SoftwareStatus,
} from '../domain/derive'
import { formatBytes, formatDuration, formatSpeed } from '../domain/format'
import { Stat, Value } from '../components/value'
import { DiscoveryPanel } from '../components/discovery'
import { SystemPanel } from '../components/system'
import {
  LocationChip, ProductChip, StatusBadge, TimeAgo, VerificationBadge, VersionChip,
} from '../components/chips'
import { AttentionBand, EmptyStateCard, ErrorState, type Attention } from '../components/layout'
import type { Package, Product, Transfer } from '../api/types'

/**
 * Page 1 - Overview.
 *
 * Answers: what new software is available, what is downloading, and what needs
 * my attention?
 *
 * The published-this-week table is the centre of the page and the largest thing on
 * it, because that is the answer. "Saved" is deliberately NOT a card here - it
 * belongs to a release, not to the estate.
 */

interface Row {
  pkg: Package
  product: Product
  status: SoftwareStatus
}

export default function Overview() {
  const navigate = useNavigate()
  const products = useProducts()
  // Disabled products do nothing on purpose - nothing is discovered for them
  // and nothing is downloaded - so they are noise on a page about what needs
  // attention. The Products page is where they can be shown deliberately.
  const productList = (products.data?.products ?? []).filter((p) => p.enabled)

  // One request per product. There is no estate-wide package listing endpoint,
  // so this composes product listings into one "recent releases" view.
  const packageLists = usePackagesByProducts(
    productList.map((p) => p.productId),
    { pageSize: 30 },
  )

  const transfers = useTransfers({ pageSize: 100 })
  const reports = useReports({ period: '7d' })

  const rows = useMemo<Row[]>(() => {
    // A package listing carries no transfer history, so the status is joined in
    // from the transfer listing rather than derived from the package alone -
    // otherwise every row reads NEW, including releases already in production.
    const index = transferIndex(transfers.data?.transfers ?? [])
    const out: Row[] = []
    productList.forEach((product, i) => {
      for (const listed of packageLists[i]?.data?.packages ?? []) {
        const pkg = withTransfers(listed, index)
        out.push({ pkg, product, status: deriveStatus(pkg, product) })
      }
    })
    // PUBLISHED RECENTLY, newest first. A dashboard answers "what is new",
    // and a list that falls back on old releases to fill ten rows answers a
    // different question quietly - the reader cannot tell which rows are the
    // news and which are the padding.
    return out
      .filter((r) => isRecent(r.pkg))
      .sort((a, b) => publishedAt(b.pkg).localeCompare(publishedAt(a.pkg)))
      .slice(0, 10)
  }, [productList, packageLists, transfers.data])

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
    const failed = new Map<string, Transfer[]>()
    for (const t of transfers.data?.transfers ?? []) {
      if (t.state !== 'FAILED') continue
      const key = `${t.packageId || t.packageName || t.product}\u0000${t.tag}`
      const group = failed.get(key)
      if (group) group.push(t)
      else failed.set(key, [t])
    }
    for (const group of failed.values()) {
      const first = group[0]!
      const destinations = group.length > 1 ? ` (${group.length} destinations)` : ''
      items.push({
        key: `download-${first.packageId || first.id}-${first.tag}`,
        severity: 'error',
        message: `${first.product} / ${first.packageName || 'package'}:${first.displayTag || first.tag} - download failed${destinations}`,
        detail: first.failureReason
          ? `${first.failureReason} The download is stopped. Open it to see what failed and retry from where it stopped.`
          : 'The download stopped before it finished. Open it to see what failed and retry from where it stopped.',
        action: { label: 'Open download', to: `/downloads/${first.id}` },
      })
    }
    for (const r of rows) {
      if (verification(r.pkg) !== 'VERIFICATION_FAILED') continue
      items.push({
        key: `verify-${r.pkg.packageId}`,
        severity: 'error',
        message: `${r.product.productId} ${version(r.pkg)} - verification failed`,
        detail: 'The vendor signature did not verify. Do not promote this release until it is explained.',
        action: {
          label: 'View release',
          to: releaseHref(r.product.productId, r.pkg),
        },
      })
    }
    return items
  }, [transfers.data, rows])

  if (products.isError) {
    return (
      <>
        <ErrorState error={products.error} retry={() => void products.refetch()} />
      </>
    )
  }

  const loading = products.isLoading || packageLists.some((q) => q.isLoading)
  const totals = reports.data?.totals

  return (
    <>

      <AttentionBand items={attention} />

      <div style={{ marginBottom: 16 }}>
        <DiscoveryPanel products={productList} />
      </div>

      <Row gutter={[16, 16]} style={{ marginBottom: 16 }}>
        {[
          { title: 'New Packages', value: counts.new, to: '/packages?status=NEW' },
          { title: 'Downloading', value: counts.downloading, to: '/downloads' },
          { title: 'Downloaded', value: counts.downloaded, to: '/packages?status=DOWNLOADED' },
          { title: 'Production Ready', value: counts.readyForProduction, to: '/packages?status=READY' },
          { title: 'Unverified', value: counts.verificationIssues, to: '/packages?verification=failed' },
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
            title="Packages published in the last 7 days"
            extra={<Link to="/packages">View all packages</Link>}
            styles={{ body: { padding: 0 } }}
          >
            {!loading && rows.length === 0 ? (
              <div style={{ padding: 24 }}>
                <EmptyStateCard
                  title="Nothing new this week"
                  explanation="No package was published in the last seven days. Older releases are on the Packages page; discovery runs on a schedule and its progress is reported above."
                  action={<Link to="/packages"><Button>View all packages</Button></Link>}
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
                    width: 170,
                    render: (_, r) => (
                      <VersionChip product={r.product.productId} version={version(r.pkg)} pkg={r.pkg} />
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
                    render: (_, r) => (
                      <StatusBadge status={r.status} reason={failureReason(r.pkg)} />
                    ),
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
                      return (
                        <Value reason="This release has not been downloaded, so there is no time to report.">
                          {formatDuration(s)}
                        </Value>
                      )
                    },
                  },
                  {
                    title: 'Actions',
                    fixed: 'right',
                    width: 130,
                    render: (_, r) =>
                      r.status === 'NEW' ? (
                        <Link to={releaseHref(r.product.productId, r.pkg)}>
                          <Button size="small" type="primary" icon={<CloudDownloadOutlined />}>
                            Download
                          </Button>
                        </Link>
                      ) : (
                        <Link to={releaseHref(r.product.productId, r.pkg)}>
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
            <SystemPanel />

            <Card title="Download Performance" extra={<Link to="/reports">View report</Link>}>
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                Last 7 days
              </Typography.Text>
              <Row gutter={16} style={{ marginTop: 12 }}>
                <Col span={12}>
                  <Stat
                    title="Average download speed"
                    value={formatSpeed(totals?.averageBytesPerSecond)}
                    reason="No download whose bytes we moved completed in this period."
                  />
                  {!totals?.averageBytesPerSecond && !reports.isLoading && (
                    <Typography.Text type="secondary" style={{ fontSize: 11 }}>
                      No download whose bytes we moved completed in this period.
                    </Typography.Text>
                  )}
                </Col>
                <Col span={12}>
                  <Stat
                    title="Total data downloaded"
                    value={formatBytes(totals?.bytesTransferred)}
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
