import { useMemo, useState } from 'react'
import { App, Button, Card, Dropdown, Select, Space, Table, Tooltip } from 'antd'
import type { MenuProps } from 'antd'
import { MoreOutlined } from '@ant-design/icons'
import { Link, useNavigate, useSearchParams } from 'react-router-dom'
import {
  usePackages, useProducts, useRunDownload, useSyncPackageSecurity, useTransfers,
} from '../api/queries'
import ScaleIcon from '@iconify-react/lucide/scale';
import { useCan } from '../auth/permissions'
import {
  deriveLocations, deriveStatus, downloadSeconds, failureReason, isLive, matches,
  packageReference, releaseHref, transferIndex, verification, version, withTransfers,
} from '../domain/derive'
import type { Package } from '../api/types'
import { formatDuration } from '../domain/format'
import { Value } from '../components/value'
import {
  AnalysisTag, LocationChip, PackageName, StatusBadge, TimeAgo, VerificationBadge,
  VersionChip,
} from '../components/chips'
import { EmptyStateCard, ErrorState, SearchBar } from '../components/layout'
import { NokiaNIcon } from '../components/icons'
import { VulnerabilityCell } from '../components/security'

/**
 * One primary action, and everything else behind a menu.
 *
 * # Why this is not four buttons
 *
 * It was, and the fourth ran off the edge of the card. Four verbs per row is
 * also four decisions per row on a page of two hundred, when only one of them -
 * open this release - is what somebody is doing ninety percent of the time.
 *
 * So: View, the state-dependent primary verb (download it, or go and watch the
 * download), and a menu for the rest. The menu is one button wide whatever it
 * contains, which is what stops the column growing every time a verb is added.
 */
function RowActions({ product, pkg }: { product: string; pkg: Package }) {
  const { message } = App.useApp()
  const navigate = useNavigate()
  const sync = useSyncPackageSecurity()
  const mayOperate = useCan('operate', { product })

  const compareHref = `/packages/compare?product=${encodeURIComponent(product)}`
    // The REPOSITORY travels with the tag. One version tag exists in every
    // repository a product watches, so a compare link carrying only the tag
    // arrives at a page that cannot say which package was meant.
    + `&from=${encodeURIComponent(pkg.tag)}`
    + (pkg.sourceRepository ? `&repository=${encodeURIComponent(pkg.sourceRepository)}` : '')

  const detail = releaseHref(product, pkg)
  const securityHref = `${detail}${detail.includes('?') ? '&' : '?'}tab=security`
  const security = pkg.security
  // Not a sync whose Coordinator went away: see PackageSecuritySummary.stalled.
  const syncing = security?.state === 'syncing' && !security.stalled

  const startSync = () => sync.mutate(
    { product, ref: packageReference(pkg), repository: pkg.sourceRepository },
    {
      onSuccess: (res) => {
        message.info(res.started
          ? `Syncing ${res.artifacts} artifacts of ${version(pkg)}.`
          : 'A sync is already running for this release.')
        // Straight to where the progress is. A background job somebody cannot
        // watch is a background job they start twice.
        navigate(securityHref)
      },
      onError: (e) => message.error(e instanceof Error ? e.message : 'The sync could not be started.'),
    },
  )

  const syncItem: MenuProps['items'] = syncing
    ? [{ key: 'progress', label: <Link to={securityHref}>View sync progress</Link> }]
    : security?.canSync
      ? [{
          key: 'sync',
          label: security.state === '' ? 'Sync vulnerabilities' : 'Sync vulnerabilities again',
          disabled: !mayOperate,
          onClick: startSync,
        }]
      : [{
          key: 'sync-off',
          label: <Tooltip title={security?.reason}><span>Sync vulnerabilities</span></Tooltip>,
          disabled: true,
        }]

  const items: MenuProps['items'] = [
    { key: 'security', label: <Link to={securityHref}>View vulnerabilities</Link> },
    ...syncItem,
    { type: 'divider' },
    { key: 'compare', label: <Link to={compareHref}>Compare with another release</Link> },
  ]

  return (
    <Space size={4}>
      <Link to={detail}>
        <Button size="small" type="primary">View</Button>
      </Link>
      <DownloadAction product={product} pkg={pkg} />
      <Dropdown menu={{ items }} trigger={['click']} placement="bottomRight">
        <Button size="small" icon={<MoreOutlined />} aria-label="More actions" loading={sync.isPending} />
      </Dropdown>
    </Space>
  )
}


/**
 * Start a download, or go and watch the one that is running.
 *
 * # Why this starts it rather than linking to a page that can
 *
 * Because the button said Download and did not download. It took the reader to
 * the release page, where a second button with the same word did the thing -
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

  const status = deriveStatus(pkg)
  const live = (pkg.transfers ?? []).find((t) => isLive(t.state))
  const failed = (pkg.transfers ?? []).find((t) => t.state === 'FAILED')
  // A finished download still gets a link to it, not nothing - the release
  // page said "downloaded" and gave nowhere to see it.
  const succeeded = (pkg.transfers ?? []).find((t) => t.state === 'SUCCEEDED')
  const existing = live ?? failed ?? succeeded
  const downloaded = status === 'DOWNLOADED' || status === 'READY FOR PRODUCTION' || status === 'PRODUCTION'
  if (existing || downloaded) {
    return (
      <Link to={existing ? `/downloads/${existing.id}` : '/downloads'}>
        <Button
          size="small"
          color="green"
          variant="outlined"
        >
          View download
        </Button>
      </Link>
    )
  }

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
        color="green"
        variant="solid"
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
 * The Packages listing - where "View all packages" and the Overview KPI cards
 * land.
 *
 * Filters compose into the URL, so a filtered view can be pasted to a
 * colleague. The product filter is a real server-side filter; the status
 * filter is derived and therefore applied here, over one page of results, and
 * says so rather than pretending to be exhaustive.
 */
export default function Packages() {
  const { message } = App.useApp()
  const syncSecurity = useSyncPackageSecurity()

  const [params, setParams] = useSearchParams()
  const [search, setSearch] = useState('')
  const products = useProducts()
  const productList = products.data?.products ?? []

  const selected = params.get('product') ?? productList[0]?.productId
  const mayOperateSelected = useCan('operate', selected ? { product: selected } : undefined)
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
    // The version as shown AND as the vendor spells it, plus the repository -
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

  const syncNotSynced = (pkg: Package) => {
    if (!selected) return
    syncSecurity.mutate(
      { product: selected, ref: packageReference(pkg), repository: pkg.sourceRepository },
      {
        onSuccess: (res) => {
          message.info(res.started
            ? `Syncing ${res.artifacts} artifacts of ${version(pkg)}.`
            : 'A sync is already running for this release.')
        },
        onError: (e) => message.error(e instanceof Error ? e.message : 'The sync could not be started.'),
      },
    )
  }

  if (products.isError) {
    return (
      <>
        <ErrorState error={products.error} retry={() => void products.refetch()} />
      </>
    )
  }

  return (
    <>
      <div
        style={{
          display: 'flex', alignItems: 'center', justifyContent: 'space-between',
          gap: 16, marginBottom: 12, flexWrap: 'wrap',
        }}
      >
        <Space size={12} align="center" wrap>
          <SearchBar
            value={search}
            onChange={setSearch}
            placeholder="Search by version or repository"
            matched={rows.length}
            total={packages.data?.packages?.length ?? 0}
            width={280}
            style={{ marginBottom: 0 }}
          />
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
        <Link to="/packages/compare">
          <Button icon={<ScaleIcon style={{ width: '1em', height: '1em' }} />}>Compare packages</Button>
        </Link>
      </div>

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
            /*
              `max-content` rather than a number.
              A hardcoded width has to be kept in step with the sum of the
              column widths by hand, and when it drifts below that sum antd
              squeezes the table to the smaller figure and the pinned Actions
              column lands on top of the one before it. Letting the browser
              measure cannot drift.
            */
            scroll={{ x: 'max-content' }}
            columns={[
              {
                title: 'Product',
                width: 210,
                render: (_, r) => product && (
                  <span>{product.displayName}</span>
                ),
              },
              {
                /*
                  The package's own name, in its own column. It used to sit
                  under the version as a subtitle, which read as a footnote -
                  and it is not one: a product publishes one version tag into
                  every repository it watches, so this is frequently the only
                  thing telling two rows apart.

                  Pinned, and first, because a column called Product used to be.
                  That column drew the same chip on every row: the listing is
                  scoped to exactly ONE product by the select above it, so the
                  chip could not tell two rows apart and cost 130px of a table
                  that was already 400px wider than the window - which is what
                  pushed the pinned Actions column on top of its neighbour.
                */
                title: 'Name',
                fixed: 'left',
                width: 210,
                render: (_, r) => product && (
                  <Link to={releaseHref(product.productId, r.pkg)}>
                    <PackageName pkg={r.pkg} width={220} />
                  </Link>
                ),
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
              { title: 'Published', width: 95, render: (_, r) => <TimeAgo at={r.pkg.publishedAt || r.pkg.discoveredAt} /> },
              { title: 'Signed', width: 120, render: (_, r) => <VerificationBadge state={verification(r.pkg)} /> },
              {
                title: 'Status',
                width: 130,
                render: (_, r) => (
                  <Space size={4} wrap>
                    <StatusBadge status={r.status} reason={failureReason(r.pkg)} />
                    <AnalysisTag pkg={r.pkg} />
                  </Space>
                ),
              },
              {
                /*
                  Always on, and ahead of Location and Download time.

                  The table is wider than a laptop window, so column order is a
                  priority order: whatever sits last is what the pinned Actions
                  column covers. This one was last but one, which made the
                  column this whole feature exists for the one nobody could
                  see.

                  It costs nothing to keep.
                  The counts come from the listing response itself, written by
                  a sync rather than fetched per row - which is what made this
                  a toggle before, and a toggle is a design apologising for
                  itself.
                */
                title: 'Vulnerabilities',
                width: 185,
                render: (_, r) => {
                  const security = r.pkg.security
                  const maySync = Boolean(security?.state === '' && security?.canSync && mayOperateSelected)
                  return (
                    <VulnerabilityCell
                      summary={security}
                      onSyncNotSynced={maySync ? () => syncNotSynced(r.pkg) : undefined}
                      notSyncedTooltip={maySync
                        ? 'Click to sync'
                        : (security?.reason ?? 'Nobody has scanned this release yet.')}
                    />
                  )
                },
              },
              {
                title: 'Location',
                width: 150,
                render: (_, r) => (
                  <LocationChip
                    locations={deriveLocations(r.pkg, product)}
                    vendorIcon={NokiaNIcon}
                  />
                ),
              },
              {
                title: 'Download Time',
                width: 140,
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
                width: 190,
                render: (_, r) => product && <RowActions product={product.productId} pkg={r.pkg} />,
              },
            ]}
          />
        </Card>
      )}
    </>
  )
}
