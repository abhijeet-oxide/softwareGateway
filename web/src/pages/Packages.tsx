import { useMemo, useState } from 'react'
import { App, Button, Card, Select, Space, Table, Tooltip } from 'antd'
import { Link, useNavigate, useSearchParams } from 'react-router-dom'
import { usePackages, useProducts, useRunDownload, useTransfers } from '../api/queries'
import { useCan } from '../auth/permissions'
import {
  deriveLocations, deriveStatus, downloadedAt, downloadSeconds, failureReason, isLive, matches,
  packageReference, releaseHref, transferIndex, verification, version, withTransfers,
} from '../domain/derive'
import type { Package } from '../api/types'
import { formatDuration } from '../domain/format'
import { Value } from '../components/value'
import {
  AnalysisTag, LocationChip, PackageName, ProductChip, StatusBadge, TimeAgo, VerificationBadge,
  VersionChip,
} from '../components/chips'
import { EmptyStateCard, ErrorState, PageHeader, SearchBar } from '../components/layout'

/**
 * Start a download, or go and watch the one that is running.
 *
 * # Why this starts it rather than linking to a page that can
 *
 * Because the button said Download and did not download. It took the reader to
 * the release page, where a second button with the same word did the thing —
 * two clicks and a page load to perform an action the row already had every
 * argument for. A release downloads WHOLE, so there is nothing to choose and
 * nothing to confirm.
 *
 * # And it never offers to start a second one
 *
 * A release already being downloaded gets a link to that download instead. The
 * server would collapse a duplicate request onto the existing transfer anyway,
 * so offering it would be a button whose honest outcome is "nothing happened".
 */
function DownloadAction({ product, pkg }: { product: string; pkg: Package }) {
  const { message } = App.useApp()
  const navigate = useNavigate()
  const run = useRunDownload(product)
  const mayOperate = useCan('operate', { product })

  const live = (pkg.transfers ?? []).find((t) => isLive(t.state))
  if (live) {
    return (
      <Link to={`/downloads/${live.id}`}>
        <Button size="small" type="primary">View download</Button>
      </Link>
    )
  }
  if (downloadedAt(pkg)) return null

  const start = async () => {
    try {
      // The REPOSITORY travels with the version. Nine packages of this product
      // carry this version; the row that was clicked is the only one that says
      // which, and sending the version alone threw that away.
      const result = await run.mutateAsync({ tags: [packageReference(pkg)] })
      message.success(
        result.created?.length
          ? `Download of ${version(pkg)} started.`
          : 'This release was already requested; the existing download continues.',
      )
      // The request fans out to one transfer per destination, so there is no
      // single download to land on. The listing is the honest destination and
      // the new rows are at the top of it.
      navigate('/downloads')
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'The download could not be started.')
    }
  }

  return (
    <Tooltip
      title={
        mayOperate
          ? `Download ${version(pkg)} whole into the internal repositories. Artifacts already present are skipped.`
          : 'You do not have permission to start a download.'
      }
    >
      <Button
        size="small"
        type="primary"
        disabled={!mayOperate}
        loading={run.isPending}
        onClick={() => void start()}
      >
        Download
      </Button>
    </Tooltip>
  )
}

/**
 * The Packages listing — where "View all packages" and the Overview KPI cards
 * land.
 *
 * Filters compose into the URL, so a filtered view can be pasted to a
 * colleague. The product filter is a real server-side filter; the status
 * filter is derived and therefore applied here, over one page of results, and
 * says so rather than pretending to be exhaustive.
 */
export default function Packages() {
  const [params, setParams] = useSearchParams()
  const [search, setSearch] = useState('')
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
    const byStatus = !status
      ? all
      : status === 'READY'
        ? all.filter((r) => r.status === 'READY FOR PRODUCTION')
        : all.filter((r) => r.status === status)

    if (!search.trim()) return byStatus
    // The version as shown AND as the vendor spells it, plus the repository —
    // a product publishes one version tag into every repository it watches, so
    // the repository is frequently the only thing telling two rows apart.
    return byStatus.filter((r) => matches(
      search, version(r.pkg), r.pkg.tag, r.pkg.displayRepository, r.pkg.sourceRepository))
  }, [packages.data, transfers.data, product, status, search])

  const update = (key: string, value?: string) => {
    const next = new URLSearchParams(params)
    if (value) next.set(key, value)
    else next.delete(key)
    setParams(next)
  }

  if (products.isError) {
    return (
      <>
        <PageHeader title="Packages" description="Every release we know about, and where it is" />
        <ErrorState error={products.error} retry={() => void products.refetch()} />
      </>
    )
  }

  return (
    <>
      <PageHeader
        title="Packages"
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
                { value: 'NEW', label: 'New (last 7 days)' },
                { value: 'AVAILABLE', label: 'Available' },
                { value: 'DOWNLOADING', label: 'Downloading' },
                { value: 'DOWNLOADED', label: 'Downloaded' },
                { value: 'DOWNLOAD FAILED', label: 'Download failed' },
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

      <SearchBar
        value={search}
        onChange={setSearch}
        placeholder="Search by version or repository"
        matched={rows.length}
        total={packages.data?.packages?.length ?? 0}
        width={360}
      />

      {!packages.isLoading && rows.length === 0 ? (
        <EmptyStateCard
          title={search.trim() || status ? 'Nothing matches this filter' : 'No packages discovered yet'}
          explanation={
            search.trim()
              ? 'No release on this page matches what you typed. The search covers the version and the repository it came from.'
              : status
                ? 'No release currently has this status. Clear the filter to see everything discovered for this product.'
                : 'Discovery polls the vendor registries on a schedule. Run it from the Overview to look immediately.'
          }
          action={
            search.trim()
              ? <Button onClick={() => setSearch('')}>Clear search</Button>
              : status
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
            scroll={{ x: 1420 }}
            columns={[
              {
                title: 'Product',
                fixed: 'left',
                width: 130,
                render: () => product && <ProductChip name={product.productId} display={product.displayName} />,
              },
              {
                // The package's own name, in its own column. It used to sit
                // under the version as a subtitle, which read as a footnote —
                // and it is not one: a product publishes one version tag into
                // every repository it watches, so this is frequently the only
                // thing telling two rows apart.
                title: 'Name',
                width: 240,
                render: (_, r) => <PackageName pkg={r.pkg} width={220} />,
              },
              {
                title: 'Version',
                width: 160,
                render: (_, r) =>
                  product && (
                    <VersionChip
                      product={product.productId}
                      version={version(r.pkg)}
                      pkg={r.pkg}
                      showRepository={false}
                    />
                  ),
              },
              { title: 'Published', width: 110, render: (_, r) => <TimeAgo at={r.pkg.publishedAt || r.pkg.discoveredAt} /> },
              { title: 'Verified', width: 145, render: (_, r) => <VerificationBadge state={verification(r.pkg)} /> },
              {
                title: 'Status',
                width: 240,
                render: (_, r) => (
                  <Space size={4} wrap>
                    <StatusBadge status={r.status} reason={failureReason(r.pkg)} />
                    <AnalysisTag pkg={r.pkg} />
                  </Space>
                ),
              },
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
                width: 250,
                render: (_, r) =>
                  product && (
                    <Space size={4}>
                      {/*
                        DOWNLOAD FIRST, and only while there is one to start.
                        It was one button that turned into Details once a
                        release stopped being NEW, which hid the page's whole
                        purpose behind a word that changed meaning — and hid
                        Download on every release older than seven days, which
                        is most of them and exactly the ones somebody is
                        looking at when they need it.
                      */}
                      <DownloadAction product={product.productId} pkg={r.pkg} />
                      <Link to={releaseHref(product.productId, r.pkg)}>
                        <Button size="small">Details</Button>
                      </Link>
                      <Tooltip title={`Compare ${version(r.pkg)} against another version or another location`}>
                        <Link
                          // The REPOSITORY travels with the tag. One version
                          // tag exists in every repository a product watches,
                          // so a compare link carrying only the tag arrives at
                          // a page that cannot say which package was meant.
                          to={`/compare?product=${encodeURIComponent(product.productId)}` +
                              `&from=${encodeURIComponent(r.pkg.tag)}` +
                              (r.pkg.sourceRepository
                                ? `&repository=${encodeURIComponent(r.pkg.sourceRepository)}`
                                : '')}
                        >
                          <Button size="small">Compare</Button>
                        </Link>
                      </Tooltip>
                    </Space>
                  ),
              },
            ]}
          />
        </Card>
      )}
    </>
  )
}
