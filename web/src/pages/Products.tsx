import { useEffect, useRef, useState } from 'react'
import { InlineNotice, StatusPill } from '../uikit'
import { Button, Card, Space, Tag, Tooltip, Typography } from 'antd'
// The working-surface table: resizable, reorderable, pinnable columns whose
// layout each person keeps. See `tablekit/README.md` for which tables get it.
import { Table as DataTable } from '../tablekit'
import { useParams } from 'react-router-dom'
import { usePackages, useProducts, useTransfers } from '../api/queries'
import { RunDiscoveryButton } from '../components/discovery'
import {
  deriveLifecycle, deriveLocations, deriveStatus, downloadSeconds, failureReason, matches,
  transferIndex, verification, version, withTransfers,
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
import { ConfigErrorDetail, ConfigErrorPill, isNotLoaded } from '../components/configerror'

/**
 * Page 2 - Products.
 *
 * Answers: for each product, what is the newest release, what have we got, and
 * what is in production?
 *
 * Clicking a product opens its version history IN PLACE rather than navigating
 * away, so the comparison between products stays on screen.
 */

/** What to call a product in a sentence. A rejected document may never have
 *  yielded a display name, so the slug is what is left. */
const label = (p?: Product) => p?.displayName || p?.productId || 'A product'

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
    <DataTable
      tableEnhancedKey="product-releases"
      size="small"
      loading={packages.isLoading}
      dataSource={rows}
      rowKey={(p) => p.packageId}
      pagination={{ pageSize: 10, hideOnSinglePage: true }}
      scroll={ {
                  /*
                    `max-content`, not a number. A hardcoded width has to be
                    kept in step with the sum of the column widths by hand, and
                    when it drifts below that sum antd squeezes the table to the
                    smaller figure and the pinned column lands on top of the one
                    before it. Letting the browser measure cannot drift.
                  */
                  x: 'max-content'
                } }
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
        {
          title: 'Status',
          width: 200,
          render: (_, pkg) => (
            <StatusBadge status={deriveStatus(pkg, product)} reason={failureReason(pkg)} />
          ),
        },
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
              : (
                  <Tooltip title="Time a worker was actually moving this release. Any period it spent waiting for one - or waiting out an outage - is not counted.">
                    <span><Value>{formatDuration(s)}</Value></span>
                  </Tooltip>
                )
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
  // A rejected product OPENS ITSELF, once, the first time it is seen.
  //
  // The row can only carry a pill and a filename; the reason is in the
  // expansion, and a reason nobody expands is a reason nobody reads. Tracked in
  // a ref rather than recomputed, so a reader who closes one keeps it closed
  // through every refetch - the point is to show it, not to insist on it.
  const opened = useRef(new Set<string>())
  const broken = (products.data?.products ?? []).filter((p) => p.configError)
  useEffect(() => {
    const fresh = broken.map((p) => p.productId).filter((id) => !opened.current.has(id))
    if (fresh.length === 0) return
    fresh.forEach((id) => opened.current.add(id))
    setExpanded((prev) => [...prev, ...fresh.filter((id) => !prev.includes(id))])
  }, [broken.map((p) => p.productId).join(',')]) // eslint-disable-line react-hooks/exhaustive-deps

  if (products.isError) {
    return (
      <>
        <ErrorState error={products.error} retry={() => void products.refetch()} />
      </>
    )
  }

  const all = products.data?.products ?? []
  // Switched off ON PURPOSE, which is the only kind that is hideable. A product
  // that is off because its document was REFUSED is not a deliberate pause and
  // is counted separately below.
  const disabledCount = all.filter((p) => !p.enabled && !p.configError).length
  // Counted APART, because they are not the same news. One set does nothing at
  // all; the other is working fine and merely ignoring somebody's last edit.
  // One sentence covering both would have to be vague enough to be true of
  // either, which makes it useless for acting on.
  const notLoaded = all.filter((p) => p.configError && !p.configError.loaded)
  const stale = all.filter((p) => p.configError?.loaded)
  const brokenCount = notLoaded.length + stale.length

  // A disabled product still loads, still validates and is still listed by the
  // API - it simply does nothing. That makes it worth keeping and worth hiding:
  // on a page answering "where has each product reached", something that has
  // reached nowhere on purpose is noise until somebody asks for it.
  //
  // A REJECTED product is the opposite and is never hidden, whatever this
  // toggle says. It also arrives with `enabled: false` - nothing about it runs
  // - so filtering on that flag alone swept it back out of sight, which is the
  // exact failure this row exists to end. Somebody has to see it to fix it.
  const listed = showDisabled ? all : all.filter((p) => p.enabled || p.configError)
  const rows = search.trim()
    ? listed.filter((p) => matches(search, p.productId, p.displayName, p.description))
    : listed

  return (
    <>
      <PageHeader
        meta={
          disabledCount > 0 && (
            <Button onClick={() => setShowDisabled((v) => !v)}>
              {showDisabled
                ? `Hide disabled (${disabledCount})`
                : `Show disabled products (${disabledCount})`}
            </Button>
          )
        }
      />

      {/*
        Said once at the top as well as on every row. A rejected document is
        the one thing on this page that is nobody's intention, and the row
        alone relies on the reader happening to look at it - on a long list,
        behind a search, or below the fold, they will not.
      */}
      {notLoaded.length > 0 && (
        <InlineNotice tone="danger" className="ui-fade-in">
          {notLoaded.length === 1
            ? `${label(notLoaded[0])} is configured but not running: its document was rejected. It is listed below with the reason.`
            : `${notLoaded.length} products are configured but not running: their documents were rejected. They are listed below with their reasons.`}
        </InlineNotice>
      )}
      {stale.length > 0 && (
        <InlineNotice tone="warn" className="ui-fade-in">
          {stale.length === 1
            ? `${label(stale[0])} is running its previous configuration: its most recent edit was rejected.`
            : `${stale.length} products are running their previous configuration: their most recent edits were rejected.`}
        </InlineNotice>
      )}

      <div
        style={{
          display: 'flex', alignItems: 'center', justifyContent: 'space-between',
          gap: 12, marginBottom: 12, marginTop: brokenCount > 0 ? 12 : 0, flexWrap: 'wrap',
        }}
      >
        <SearchBar
          value={search}
          onChange={setSearch}
          placeholder="Search products by name or description"
          matched={rows.length}
          total={listed.length}
          style={{ marginBottom: 0 }}
        />
        <RunDiscoveryButton products={rows} />
      </div>

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
          <DataTable
            tableEnhancedKey="products"
            loading={products.isLoading}
            dataSource={rows}
            rowKey={(p) => p.productId}
            pagination={false}
            scroll={ {
                  /*
                    `max-content`, not a number. A hardcoded width has to be
                    kept in step with the sum of the column widths by hand, and
                    when it drifts below that sum antd squeezes the table to the
                    smaller figure and the pinned column lands on top of the one
                    before it. Letting the browser measure cannot drift.
                  */
                  x: 'max-content'
                } }
            expandable={{
              expandedRowKeys: expanded,
              onExpandedRowsChange: (keys) => setExpanded(keys as string[]),
              // The expansion answers "tell me more about this row", and for a
              // rejected product that is the rejection itself - every failure,
              // each naming its own field - not a release history it cannot
              // have. A product that is RUNNING an older configuration gets
              // both: the edit that was refused, and then its releases, which
              // are real.
              expandedRowRender: (product) => (
                product.configError ? (
                  <div style={{ display: 'flex', flexDirection: 'column', gap: 16 }}>
                    <ConfigErrorDetail error={product.configError} />
                    {product.configError.loaded && <VersionHistory product={product} />}
                  </div>
                ) : (
                  <VersionHistory product={product} />
                )
              ),
            }}
            columns={[
              {
                title: 'Product',
                fixed: 'left',
                width: 200,
                render: (_, p) => (
                  <Space direction="vertical" size={0}>
                    <Space size={6}>
                      {/*
                        A rejected product keeps its name in FULL contrast. It
                        is off, but not quietly and not on purpose: greying it
                        out alongside the deliberately disabled ones is how the
                        one row somebody has to act on ends up looking like the
                        rows they chose to switch off.
                      */}
                      <Typography.Text
                        strong={p.enabled || Boolean(p.configError)}
                        type={p.enabled || p.configError ? undefined : 'secondary'}
                      >
                        {p.displayName || p.productId}
                      </Typography.Text>
                      {p.configError ? (
                        <ConfigErrorPill error={p.configError} />
                      ) : !p.enabled && (
                        <Tooltip title="This product is switched off in configuration. It is still loaded and validated, and it does nothing - nothing is discovered, nothing is downloaded.">
                          <Tag style={{ marginInlineEnd: 0 }}>Disabled</Tag>
                        </Tooltip>
                      )}
                    </Space>
                    {p.description && (
                      <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                        {p.description}
                      </Typography.Text>
                    )}
                    {/*
                      A rejected product that never loaded has no display name
                      either, so the row would otherwise carry only the slug.
                      Naming the file it came from is what turns the row into
                      something the reader can act on without leaving the page.
                    */}
                    {isNotLoaded(p) && p.configError?.file && (
                      <Typography.Text type="secondary" style={{ fontSize: 11 }}>
                        {p.configError.file}
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
                  isNotLoaded(p)
                    ? <NA reason="Unknown: this product's configuration was rejected, so its repositories were never read." />
                    : (
                      <Space size={4} wrap>
                        {(p.targets ?? []).map((t) => <TargetTag key={t.name} target={t} />)}
                      </Space>
                    )
                ),
              },
              {
                title: 'Discovery',
                width: 130,
                render: (_, p) => {
                  // UNKNOWN, not "Disabled". A rejected document was never
                  // read, so its sources were never resolved: saying discovery
                  // is switched off asserts something nobody has established,
                  // and it happens to be the reassuring version of it.
                  if (isNotLoaded(p)) {
                    return (
                      <Tooltip title="Unknown: this product's configuration was rejected, so its sources were never read.">
                        <StatusPill tone="neutral" dot={false} style={{ marginInlineEnd: 0 }}>
                          Unknown
                        </StatusPill>
                      </Tooltip>
                    )
                  }
                  // Discovery is enabled per SOURCE, not per product. This
                  // column previously rendered the product's own `enabled`
                  // flag, which is a different fact under the wrong heading.
                  const polled = (p.sources ?? []).filter((src) => src.discovery?.enabled)
                  if (polled.length === 0) {
                    return <StatusPill tone="neutral">Disabled</StatusPill>
                  }
                  return (
                    <Tooltip title={`Polled sources: ${polled.map((src) => src.name).join(', ')}`}>
                      <StatusPill tone="ok" style={{ marginInlineEnd: 0 }}>
                        {polled.length} {polled.length === 1 ? 'source' : 'sources'}
                      </StatusPill>
                    </Tooltip>
                  )
                },
              },
              {
                title: 'Auto-download',
                width: 140,
                render: (_, p) =>
                  isNotLoaded(p) ? (
                    <Tooltip title="Unknown: this product's configuration was rejected, so its download policy was never read.">
                      <StatusPill tone="neutral" dot={false} style={{ marginInlineEnd: 0 }}>
                        Unknown
                      </StatusPill>
                    </Tooltip>
                  ) : p.autoDownload?.enabled ? (
                    <StatusPill tone="ok" style={{ marginInlineEnd: 0 }}>
                      {p.autoDownload.rules?.length ?? 0}
                      {(p.autoDownload.rules?.length ?? 0) === 1 ? ' rule' : ' rules'}
                    </StatusPill>
                  ) : (
                    /*
                      NEUTRAL, not red. Automatic downloads being switched off is
                      a configuration choice, and the tooltip says so in the next
                      breath: downloading by hand is unaffected. A red pill in a
                      column of green ones reads as something to go and fix.
                    */
                    <Tooltip title="Automatic downloads are switched off in configuration. Downloading by hand is unaffected.">
                      <StatusPill tone="neutral" style={{ marginInlineEnd: 0 }}>Disabled</StatusPill>
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
