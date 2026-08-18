import { useState } from 'react'
import { Button, Card, Space, Table, Tag, Tooltip, Typography } from 'antd'
import { useParams } from 'react-router-dom'
import { usePackages, useProducts, useTransfers } from '../api/queries'
import { RunDiscoveryButton } from '../components/discovery'
import {
  deriveLifecycle, deriveLocations, deriveStatus, downloadSeconds, matches, transferIndex,
  verification, version, withTransfers,
} from '../domain/derive'
import { formatDuration } from '../domain/format'
import { NA, Value } from '../components/value'
import {
  LocationChip, StatusBadge, TimeAgo, VerificationBadge, VersionChip,
} from '../components/chips'
import {
  EmptyStateCard, ErrorState, LifecycleCell, PageHeader, SearchBar,
} from '../components/layout'
import type { Product } from '../api/types'
import { TargetTag } from '../components/chips'

/**
 * Page 2 — Products.
 *
 * Answers: for each product, what is the newest release, what have we got, and
 * what is in production?
 *
 * Clicking a product opens its version history IN PLACE rather than navigating
 * away, so the comparison between products stays on screen.
 */

function VersionHistory({ product }: { product: Product }) {
  const packages = usePackages(product.productId, { pageSize: 25 })
  const transfers = useTransfers({ product: product.productId, pageSize: 200 })

  if (packages.isError) {
    return <ErrorState error={packages.error} retry={() => void packages.refetch()} />
  }

  // The transfer listing supplies the history a package listing omits.
  const index = transferIndex(transfers.data?.transfers ?? [])
  const rows = (packages.data?.packages ?? []).map((listed) => withTransfers(listed, index))
  if (!packages.isLoading && rows.length === 0) {
    return (
      <EmptyStateCard
        title={`No releases discovered for ${product.displayName || product.productId}`}
        explanation="Discovery polls this product's vendor registries on a schedule. Run it now to look immediately."
        action={<RunDiscoveryButton products={[product]} product={product.productId} />}
      />
    )
  }

  return (
    <Table
      size="small"
      loading={packages.isLoading}
      dataSource={rows}
      rowKey={(p) => p.packageId}
      pagination={{ pageSize: 10, hideOnSinglePage: true }}
      scroll={{ x: 1240 }}
      columns={[
        {
          title: 'Version',
          fixed: 'left',
          width: 120,
          render: (_, pkg) => (
            <VersionChip product={product.productId} version={version(pkg)} pkg={pkg} />
          ),
        },
        { title: 'Published', width: 110, render: (_, pkg) => <TimeAgo at={pkg.publishedAt || pkg.discoveredAt} /> },
        { title: 'Verified', width: 140, render: (_, pkg) => <VerificationBadge state={verification(pkg)} /> },
        { title: 'Status', width: 200, render: (_, pkg) => <StatusBadge status={deriveStatus(pkg, product)} /> },
        {
          // The stage, with the timeline one hover away. A stepper per row is
          // four columns of furniture repeated down the page, and the part
          // that actually differs between rows is the hardest of it to read.
          title: 'Lifecycle',
          width: 160,
          render: (_, pkg) => <LifecycleCell steps={deriveLifecycle(pkg, product)} />,
        },
        { title: 'Location', width: 170, render: (_, pkg) => <LocationChip locations={deriveLocations(pkg, product)} /> },
        {
          title: 'Download time',
          width: 120,
          render: (_, pkg) => {
            const s = downloadSeconds(pkg)
            return s === undefined
              ? <NA reason="This release has not been downloaded." />
              : <Value>{formatDuration(s)}</Value>
          },
        },
      ]}
    />
  )
}

export default function Products() {
  const { product: routeProduct } = useParams()
  const products = useProducts()
  const [showDisabled, setShowDisabled] = useState(false)
  const [search, setSearch] = useState('')

  const [expanded, setExpanded] = useState<string[]>(routeProduct ? [routeProduct] : [])

  if (products.isError) {
    return (
      <>
        <PageHeader title="Products" description="What we track, and where each product has reached" />
        <ErrorState error={products.error} retry={() => void products.refetch()} />
      </>
    )
  }

  const all = products.data?.products ?? []
  const disabledCount = all.filter((p) => !p.enabled).length

  // A disabled product still loads, still validates and is still listed by the
  // API — it simply does nothing. That makes it worth keeping and worth hiding:
  // on a page answering "where has each product reached", something that has
  // reached nowhere on purpose is noise until somebody asks for it.
  const listed = showDisabled ? all : all.filter((p) => p.enabled)
  const rows = search.trim()
    ? listed.filter((p) => matches(search, p.productId, p.displayName, p.description))
    : listed

  return (
    <>
      <PageHeader
        title="Products"
        description="What we track, and where each product has reached"
        meta={
          disabledCount > 0 && (
            <Button onClick={() => setShowDisabled((v) => !v)}>
              {showDisabled
                ? `Hide disabled (${disabledCount})`
                : `Show disabled products (${disabledCount})`}
            </Button>
          )
        }
        extra={<RunDiscoveryButton products={rows} />}
      />

      <SearchBar
        value={search}
        onChange={setSearch}
        placeholder="Search products by name or description"
        matched={rows.length}
        total={listed.length}
      />

      {!products.isLoading && rows.length === 0 ? (
        <EmptyStateCard
          title={search.trim() ? `Nothing matches "${search.trim()}"` : 'No products are configured'}
          explanation={
            search.trim()
              ? 'No product name or description contains that. Clear the search to see everything configured.'
              : 'Products are defined in Git and reconciled into this instance. Add one to the configuration repository and it will appear here.'
          }
          action={
            search.trim()
              ? <Button onClick={() => setSearch('')}>Clear search</Button>
              : <Button href="/settings">Open Settings</Button>
          }
        />
      ) : (
        <Card styles={{ body: { padding: 0 } }}>
          <Table
            loading={products.isLoading}
            dataSource={rows}
            rowKey={(p) => p.productId}
            pagination={false}
            scroll={{ x: 1240 }}
            expandable={{
              expandedRowKeys: expanded,
              onExpandedRowsChange: (keys) => setExpanded(keys as string[]),
              expandedRowRender: (product) => <VersionHistory product={product} />,
            }}
            columns={[
              {
                title: 'Product',
                fixed: 'left',
                width: 200,
                render: (_, p) => (
                  <Space direction="vertical" size={0}>
                    <Space size={6}>
                      <Typography.Text strong={p.enabled} type={p.enabled ? undefined : 'secondary'}>
                        {p.displayName || p.productId}
                      </Typography.Text>
                      {!p.enabled && (
                        <Tooltip title="This product is switched off in configuration. It is still loaded and validated, and it does nothing — nothing is discovered, nothing is downloaded.">
                          <Tag style={{ marginInlineEnd: 0 }}>Disabled</Tag>
                        </Tooltip>
                      )}
                    </Space>
                    {p.description && (
                      <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                        {p.description}
                      </Typography.Text>
                    )}
                  </Space>
                ),
              },
              {
                title: 'Vendor',
                width: 130,
                render: (_, p) => <Value>{p.sources?.[0]?.vendor || p.sources?.[0]?.name}</Value>,
              },
              { title: 'Owner', width: 160, render: (_, p) => <Value>{p.owner}</Value> },
              {
                title: 'Repositories',
                width: 220,
                render: (_, p) => (
                  <Space size={4} wrap>
                    {(p.targets ?? []).map((t) => <TargetTag key={t.name} target={t} />)}
                  </Space>
                ),
              },
              {
                title: 'Discovery',
                width: 130,
                render: (_, p) => {
                  // Discovery is enabled per SOURCE, not per product. This
                  // column previously rendered the product's own `enabled`
                  // flag, which is a different fact under the wrong heading.
                  const polled = (p.sources ?? []).filter((src) => src.discovery?.enabled)
                  if (polled.length === 0) return <Tag>Disabled</Tag>
                  return (
                    <Tooltip title={`Polled sources: ${polled.map((src) => src.name).join(', ')}`}>
                      <Tag color="green" style={{ marginInlineEnd: 0 }}>
                        {polled.length} {polled.length === 1 ? 'source' : 'sources'}
                      </Tag>
                    </Tooltip>
                  )
                },
              },
              {
                title: 'Auto-download',
                width: 140,
                render: (_, p) =>
                  p.autoDownload?.enabled ? (
                    <Tag color="blue" style={{ marginInlineEnd: 0 }}>
                      {p.autoDownload.rules?.length ?? 0} rules
                    </Tag>
                  ) : (
                    <Tooltip title="Automatic downloads are switched off in configuration. Downloading by hand is unaffected.">
                      <Tag style={{ marginInlineEnd: 0 }}>Disabled</Tag>
                    </Tooltip>
                  ),
              },
            ]}
          />
        </Card>
      )}
    </>
  )
}
