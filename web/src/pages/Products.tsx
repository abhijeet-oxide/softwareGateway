import { useState } from 'react'
import { Button, Card, Space, Table, Tag, Tooltip, Typography } from 'antd'
import { useParams } from 'react-router-dom'
import { usePackages, useProducts, useTransfers } from '../api/queries'
import { RunDiscoveryButton } from '../components/discovery'
import {
  deriveLifecycle, deriveLocations, deriveStatus, downloadSeconds, transferIndex,
  verification, version, withTransfers,
} from '../domain/derive'
import { formatDuration } from '../domain/format'
import { NA, Value } from '../components/value'
import {
  LocationChip, StatusBadge, TimeAgo, VerificationBadge, VersionChip,
} from '../components/chips'
import {
  EmptyStateCard, ErrorState, LifecycleIndicator, PageHeader,
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
            <VersionChip product={product.productId} version={version(pkg)} reference={pkg.tag} />
          ),
        },
        { title: 'Published', width: 110, render: (_, pkg) => <TimeAgo at={pkg.publishedAt || pkg.discoveredAt} /> },
        { title: 'Verified', width: 140, render: (_, pkg) => <VerificationBadge state={verification(pkg)} /> },
        { title: 'Status', width: 200, render: (_, pkg) => <StatusBadge status={deriveStatus(pkg, product)} /> },
        {
          title: 'Lifecycle',
          width: 320,
          render: (_, pkg) => <LifecycleIndicator steps={deriveLifecycle(pkg, product)} />,
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

  const [expanded, setExpanded] = useState<string[]>(routeProduct ? [routeProduct] : [])

  if (products.isError) {
    return (
      <>
        <PageHeader title="Products" description="What we track, and where each product has reached" />
        <ErrorState error={products.error} retry={() => void products.refetch()} />
      </>
    )
  }

  const rows = products.data?.products ?? []

  return (
    <>
      <PageHeader
        title="Products"
        description="What we track, and where each product has reached"
        extra={<RunDiscoveryButton products={rows} />}
      />

      {!products.isLoading && rows.length === 0 ? (
        <EmptyStateCard
          title="No products are configured"
          explanation="Products are defined in Git and reconciled into this instance. Add one to the configuration repository and it will appear here."
          action={<Button href="/settings">Open Settings</Button>}
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
                    <Typography.Text strong>{p.displayName || p.productId}</Typography.Text>
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
                title: 'Locations',
                width: 220,
                render: (_, p) => (
                  <Space size={4} wrap>
                    {(p.targets ?? []).map((t) => <TargetTag key={t.name} target={t} />)}
                  </Space>
                ),
              },
              {
                title: 'Auto-download',
                width: 140,
                render: (_, p) =>
                  p.autoDownload?.enabled ? (
                    <Tag color="blue">{p.autoDownload.rules?.length ?? 0} rules</Tag>
                  ) : (
                    <Tooltip title="Automatic downloads are switched off in configuration. Downloading by hand is unaffected.">
                      <Tag>Disabled in configuration</Tag>
                    </Tooltip>
                  ),
              },
              {
                title: 'Discovery',
                width: 120,
                render: (_, p) => (p.enabled ? <Tag color="green">Enabled</Tag> : <Tag>Disabled</Tag>),
              },
            ]}
          />
        </Card>
      )}
    </>
  )
}
