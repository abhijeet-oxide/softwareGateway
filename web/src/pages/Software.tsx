import { useMemo } from 'react'
import { Button, Card, Select, Space, Table } from 'antd'
import { Link, useSearchParams } from 'react-router-dom'
import { usePackages, useProducts, useTransfers } from '../api/queries'
import {
  deriveLocations, deriveStatus, downloadSeconds, releaseHref, transferIndex, verification, version,
  withTransfers,
} from '../domain/derive'
import { formatDuration } from '../domain/format'
import { Value } from '../components/value'
import {
  LocationChip, ProductChip, StatusBadge, TimeAgo, VerificationBadge, VersionChip,
} from '../components/chips'
import { EmptyStateCard, ErrorState, PageHeader } from '../components/layout'

/**
 * The Software listing — the "View all software" destination and where the
 * Overview KPI cards land.
 *
 * Filters compose into the URL, so a filtered view can be pasted to a
 * colleague. The product filter is a real server-side filter; the status
 * filter is derived and therefore applied here, over one page of results, and
 * says so rather than pretending to be exhaustive.
 */
export default function Software() {
  const [params, setParams] = useSearchParams()
  const products = useProducts()
  const productList = products.data?.products ?? []

  const selected = params.get('product') ?? productList[0]?.productId
  const status = params.get('status')
  const tag = params.get('tag') ?? undefined

  const product = productList.find((p) => p.productId === selected)
  const packages = usePackages(selected, { pageSize: 100, tag })
  const transfers = useTransfers({ product: selected, pageSize: 200 })

  const rows = useMemo(() => {
    // Joined from the transfer listing: a package listing carries no transfer
    // history, so deriving from the package alone would report every release
    // as NEW.
    const index = transferIndex(transfers.data?.transfers ?? [])
    const all = (packages.data?.packages ?? []).map((listed) => {
      const pkg = withTransfers(listed, index)
      return { pkg, status: deriveStatus(pkg, product) }
    })
    if (!status) return all
    if (status === 'READY') return all.filter((r) => r.status === 'READY FOR PRODUCTION')
    return all.filter((r) => r.status === status)
  }, [packages.data, transfers.data, product, status])

  const update = (key: string, value?: string) => {
    const next = new URLSearchParams(params)
    if (value) next.set(key, value)
    else next.delete(key)
    setParams(next)
  }

  if (products.isError) {
    return (
      <>
        <PageHeader title="Software" description="Every release we know about, and where it is" />
        <ErrorState error={products.error} retry={() => void products.refetch()} />
      </>
    )
  }

  return (
    <>
      <PageHeader
        title="Software"
        description="Every release we know about, and where it is"
        meta={
          <Space>
            <Select
              style={{ minWidth: 180 }}
              placeholder="Product"
              loading={products.isLoading}
              value={selected}
              onChange={(v) => update('product', v)}
              options={productList.map((p) => ({
                value: p.productId,
                label: p.displayName || p.productId,
              }))}
            />
            <Select
              style={{ minWidth: 200 }}
              placeholder="Any status"
              allowClear
              value={status ?? undefined}
              onChange={(v) => update('status', v)}
              options={[
                { value: 'NEW', label: 'New' },
                { value: 'DOWNLOADING', label: 'Downloading' },
                { value: 'DOWNLOADED', label: 'Downloaded' },
                { value: 'READY', label: 'Ready for production' },
                { value: 'PRODUCTION', label: 'In production' },
                { value: 'VERIFICATION FAILED', label: 'Verification failed' },
              ]}
            />
          </Space>
        }
        extra={
          <Link to="/compare">
            <Button>Compare versions</Button>
          </Link>
        }
      />

      {!packages.isLoading && rows.length === 0 ? (
        <EmptyStateCard
          title={status ? 'Nothing matches this filter' : 'No software discovered yet'}
          explanation={
            status
              ? 'No release currently has this status. Clear the filter to see everything discovered for this product.'
              : 'Discovery polls the vendor registries on a schedule. Run it from the Overview to look immediately.'
          }
          action={
            status
              ? <Button onClick={() => update('status', undefined)}>Clear filter</Button>
              : <Link to="/"><Button type="primary">Go to Overview</Button></Link>
          }
        />
      ) : (
        <Card styles={{ body: { padding: 0 } }}>
          <Table
            loading={packages.isLoading}
            dataSource={rows}
            rowKey={(r) => r.pkg.packageId}
            pagination={{ pageSize: 20, showSizeChanger: false }}
            scroll={{ x: 1220 }}
            columns={[
              {
                title: 'Product',
                fixed: 'left',
                width: 130,
                render: () => product && <ProductChip name={product.productId} display={product.displayName} />,
              },
              {
                title: 'Version',
                width: 190,
                render: (_, r) =>
                  product && <VersionChip product={product.productId} version={version(r.pkg)} pkg={r.pkg} />,
              },
              { title: 'Published', width: 110, render: (_, r) => <TimeAgo at={r.pkg.publishedAt || r.pkg.discoveredAt} /> },
              { title: 'Verified', width: 145, render: (_, r) => <VerificationBadge state={verification(r.pkg)} /> },
              { title: 'Status', width: 200, render: (_, r) => <StatusBadge status={r.status} /> },
              { title: 'Location', width: 180, render: (_, r) => <LocationChip locations={deriveLocations(r.pkg, product)} /> },
              {
                title: 'Download time',
                width: 120,
                render: (_, r) => {
                  const s = downloadSeconds(r.pkg)
                  return s === undefined
                    ? <Value reason="This release has not been downloaded.">{null}</Value>
                    : <Value>{formatDuration(s)}</Value>
                },
              },
              {
                title: 'Actions',
                fixed: 'right',
                width: 120,
                render: (_, r) =>
                  product && (
                    <Link to={releaseHref(product.productId, r.pkg)}>
                      <Button size="small" type={r.status === 'NEW' ? 'primary' : 'default'}>
                        {r.status === 'NEW' ? 'Download' : 'View Details'}
                      </Button>
                    </Link>
                  ),
              },
            ]}
          />
        </Card>
      )}
    </>
  )
}
