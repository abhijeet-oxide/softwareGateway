import { useMemo, useState } from 'react'
import { App, Button, Card, Dropdown, Select, Space, Tooltip } from 'antd'
// The working-surface table: resizable, reorderable, pinnable columns whose
// layout each person keeps. See `tablekit/README.md` for which tables get it.
import { Table as DataTable } from '../tablekit'
import type { MenuProps } from 'antd'
import { MoreOutlined, ScaleOutlined } from '../icons'
import { Link, useNavigate, useSearchParams } from 'react-router-dom'
import {
  usePackages, usePackagesByProducts, useProducts, useRunDownload, useSyncPackageSecurity, useTransfers,
} from '../api/queries'
import { useCan } from '../auth/permissions'
import {
  deriveLocations, deriveStatus, downloadSeconds, failureReason, isLive, isPromotion, matches,
  packageReference, promotableTargets, publishedAt, releaseHref, transferIndex, verification,
  version, withTransfers,
} from '../domain/derive'
import type { Package, PackageTransfer, Product } from '../api/types'
import { formatDuration } from '../domain/format'
import { Value } from '../components/value'
import {
  AnalysisTag, LocationChip, PackageName, StatusBadge, TimeAgo, VerificationBadge,
  VersionChip,
} from '../components/chips'
import { EmptyStateCard, ErrorState, SearchBar } from '../components/layout'
import { NokiaNIcon } from '../components/icons'
import { VulnerabilityCell } from '../components/security'
import { PromoteButton } from '../components/promote'

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
function RowActions({ product, pkg, config }: {
  product: string
  pkg: Package
  /** The product's configuration, so the row knows where this could still go. */
  config?: Product
}) {
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

  // WHAT HAS HAPPENED TO THIS RELEASE, split by kind. Everything below reads
  // from this rather than from `pkg.transfers` directly: the button and the
  // menu have to agree about which transfer they mean, and they used to
  // recompute it separately.
  const history = releaseHistory(pkg)

  const items: MenuProps['items'] = [
    // "View download" lives HERE now rather than in the row.
    //
    // Promote took its place, and that is the right trade: once a release has
    // landed, promoting it is the thing somebody is about to do and looking at
    // the download that brought it is the thing they might. A row has space
    // for one of those.
    ...(history.download
      ? [{
          key: 'download',
          label: <Link to={`/downloads/${history.download.id}`}>View download</Link>,
        }]
      : []),
    ...(history.promotion
      ? [{
          key: 'promotion',
          label: <Link to={`/downloads/${history.promotion.id}`}>View promotion</Link>,
        }]
      : []),
    ...(history.download || history.promotion ? [{ type: 'divider' as const }] : []),
    { key: 'security', label: <Link to={securityHref}>View vulnerabilities</Link> },
    ...syncItem,
    { type: 'divider' },
    { key: 'compare', label: <Link to={compareHref}>Compare with another release</Link> },
  ]

  return (
    <Space size={4}>
      {/*
        NAVIGATION, not the row's verb - so it is an ordinary button.

        It used to be the primary, which put a solid blue button and a solid
        green one side by side in every row of a long table. Two saturated
        fills competing in the same 90 pixels is what made the listing read as
        a control panel rather than as data, and it left the row's actual next
        step - download, or promote - with no way to look more important than
        "go and read about it".
      */}
      <Link to={detail}>
        <Button size="small">View</Button>
      </Link>
      <NextStep
        product={product}
        pkg={pkg}
        history={history}
        mayOperate={mayOperate}
        promotable={promotableTargets(pkg, config).length > 0}
      />
      <Dropdown menu={{ items }} trigger={['click']} placement="bottomRight">
        <Button size="small" icon={<MoreOutlined />} aria-label="More actions" loading={sync.isPending} />
      </Dropdown>
    </Space>
  )
}

/**
 * What has happened to a release, by KIND.
 *
 * A promotion and a download are both transfers and were both being found by
 * the same `find(t => t.state === 'SUCCEEDED')`, so a promoted release's row
 * linked to its promotion under the words "View download". Splitting them here
 * is what lets every caller below be exact.
 */
interface ReleaseHistory {
  /** The download worth linking to: running, else failed, else finished. */
  download?: PackageTransfer
  /** The promotion worth linking to, on the same rule. */
  promotion?: PackageTransfer
  /** A download is in flight. */
  downloading: boolean
  /** A download failed and its destination was never reached. */
  downloadFailed: boolean
  /** The release is at a target, so promoting it is a thing that can happen. */
  landed: boolean
}

function releaseHistory(pkg: Package): ReleaseHistory {
  const transfers = pkg.transfers ?? []
  const downloads = transfers.filter((t) => !isPromotion(t))
  const promotions = transfers.filter((t) => isPromotion(t))

  const pick = (list: PackageTransfer[]) =>
    list.find((t) => isLive(t.state))
    ?? list.find((t) => t.state === 'FAILED')
    ?? list.find((t) => t.state === 'SUCCEEDED')

  const status = deriveStatus(pkg)
  const download = pick(downloads)

  return {
    download,
    promotion: pick(promotions),
    downloading: downloads.some((t) => isLive(t.state)),
    downloadFailed: Boolean(download && download.state === 'FAILED'),
    // The package's own state carries the answer on a listing where transfers
    // were not expanded, which is most of them.
    landed: downloads.some((t) => t.state === 'SUCCEEDED')
      || status === 'DOWNLOADED' || status === 'READY FOR PRODUCTION'
      || status === 'PROMOTING' || status === 'PRODUCTION',
  }
}

/**
 * The one thing this release is waiting for somebody to do.
 *
 * Three answers and never two at once: download it, watch the download that is
 * running or went wrong, or promote it. That is the release's life in order,
 * and the row shows the step it is actually on.
 */
function NextStep({
  product, pkg, history, mayOperate, promotable,
}: {
  product: string
  pkg: Package
  history: ReleaseHistory
  mayOperate: boolean
  /** There is somewhere left to promote it to. */
  promotable: boolean
}) {
  if (history.downloading || history.downloadFailed) {
    return (
      <Link to={history.download ? `/downloads/${history.download.id}` : '/downloads'}>
        <Button size="small">View download</Button>
      </Link>
    )
  }

  // Landed, and somewhere left to send it. A release every target already
  // holds gets no button: its status says PRODUCTION, and offering a promotion
  // whose only outcome is a dialog explaining there is nothing to do is worse
  // than offering nothing.
  if (history.landed && promotable) {
    return (
      <PromoteButton
        size="small"
        product={product}
        reference={packageReference(pkg)}
        repository={pkg.sourceRepository}
        packageLabel={`${pkg.displayRepository || pkg.sourceRepository || pkg.tag}:${version(pkg)}`}
        disabled={!mayOperate}
        disabledReason="You do not have permission to promote a release."
      />
    )
  }

  if (history.landed) {
    // Nowhere left to go, and nothing was ever downloaded to look at either -
    // the row simply has no next step, which the status column already says.
    return history.download
      ? (
        <Link to={`/downloads/${history.download.id}`}>
          <Button size="small">View download</Button>
        </Link>
      )
      : null
  }

  return <DownloadAction product={product} pkg={pkg} />
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
 * It is only rendered for a release nothing has been done to - NextStep owns
 * that decision now, and a release already downloading, downloaded or promoted
 * never reaches here. The server would collapse a duplicate request onto the
 * existing transfer anyway, so offering it would be a button whose honest
 * outcome is "nothing happened".
 */
function DownloadAction({ product, pkg }: { product: string; pkg: Package }) {
  const { message } = App.useApp()
  const navigate = useNavigate()
  const run = useRunDownload(product)
  const mayOperate = useCan('operate', { product })

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
      {/*
        The row's primary, and the only filled button in it. Green was a second
        accent that existed nowhere else in the product and that no palette
        could reach; what makes this button the loud one is that it is the step
        the row is actually on, which the brand colour is for.
      */}
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

function RowVulnerability({
  product,
  pkg,
  onSync,
}: {
  product: string
  pkg: Package
  onSync: () => void
}) {
  const mayOperate = useCan('operate', { product })
  const security = pkg.security
  const maySync = Boolean(security?.state === '' && security?.canSync && mayOperate)

  return (
    <VulnerabilityCell
      summary={security}
      onSyncNotSynced={maySync ? onSync : undefined}
      notSyncedTooltip={maySync
        ? 'Click to sync'
        : (security?.reason ?? 'Nobody has scanned this release yet.')}
    />
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

  const selected = params.get('product') ?? undefined
  const status = params.get('status')
  const tag = params.get('tag') ?? undefined

  const product = productList.find((p) => p.productId === selected)
  const packages = usePackages(selected, { pageSize: 100, tag })
  const packagesByProducts = usePackagesByProducts(
    selected ? [] : productList.map((p) => p.productId),
    { pageSize: 100, tag },
  )
  // EVERY kind of transfer, because this index is what gives a listed release
  // its history: a listing that fetched only downloads would report a promoted
  // release as one nothing had happened to since it landed.
  const transfers = useTransfers({ product: selected, pageSize: 200, view: 'summary' })

  const rows = useMemo(() => {
    // Joined from the transfer listing: a package listing carries no transfer
    // history, so deriving from the package alone would report every release
    // as NEW.
    const index = transferIndex(transfers.data?.transfers ?? [])
    const all = selected
      ? (() => {
        if (!product) return []
        return (packages.data?.packages ?? []).map((listed) => {
          const pkg = withTransfers(listed, index)
          return { pkg, product, status: deriveStatus(pkg, product) }
        })
      })()
      : productList.flatMap((p, i) => {
        const listedPackages = packagesByProducts[i]?.data?.packages ?? []
        return listedPackages.map((listed) => {
        const pkg = withTransfers(listed, index)
        return { pkg, product: p, status: deriveStatus(pkg, p) }
      })
      })
    const byStatus = !status
      ? all
      : status === 'UNSIGNED'
        ? all.filter((r) => verification(r.pkg) === 'NOT_SIGNED')
      : status === 'READY'
        ? all.filter((r) => r.status === 'READY FOR PRODUCTION')
        : all.filter((r) => r.status === status)

    const sorted = [...byStatus].sort((a, b) => {
      const left = Date.parse(publishedAt(a.pkg) || '') || 0
      const right = Date.parse(publishedAt(b.pkg) || '') || 0
      return right - left
    })

    if (!search.trim()) return sorted
    // The version as shown AND as the vendor spells it, plus the repository -
    // a product publishes one version tag into every repository it watches, so
    // the repository is frequently the only thing telling two rows apart.
    return sorted.filter((r) => matches(
      search, version(r.pkg), r.pkg.tag, r.pkg.displayRepository, r.pkg.sourceRepository))
  }, [selected, product, productList, packages.data, packagesByProducts, transfers.data, status, search])

  const update = (key: string, value?: string) => {
    const next = new URLSearchParams(params)
    if (value) next.set(key, value)
    else next.delete(key)
    setParams(next)
  }

  const syncNotSynced = (productId: string, pkg: Package) => {
    syncSecurity.mutate(
      { product: productId, ref: packageReference(pkg), repository: pkg.sourceRepository },
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
            total={selected
              ? (packages.data?.packages?.length ?? 0)
              : packagesByProducts.reduce((n, q) => n + (q.data?.packages?.length ?? 0), 0)}
            width={280}
            style={{ marginBottom: 0 }}
          />
          <Select
            style={{ minWidth: 180 }}
            placeholder="Product"
            loading={products.isLoading}
            allowClear
            showSearch
            optionFilterProp="label"
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
            showSearch
            optionFilterProp="label"
            value={status ?? undefined}
            onChange={(v) => update('status', v)}
            options={[
              { value: 'NEW', label: 'New (last 7 days)' },
              { value: 'AVAILABLE', label: 'Available' },
              { value: 'DOWNLOADING', label: 'Downloading' },
              { value: 'DOWNLOADED', label: 'Downloaded' },
              { value: 'DOWNLOAD FAILED', label: 'Download failed' },
              { value: 'READY', label: 'Ready for production' },
              { value: 'PROMOTING', label: 'Promoting' },
              { value: 'PROMOTION FAILED', label: 'Promotion failed' },
              { value: 'PRODUCTION', label: 'In production' },
              { value: 'UNSIGNED', label: 'Unsigned' },
              { value: 'VERIFICATION FAILED', label: 'Verification failed' },
            ]}
          />
        </Space>
        <Link to="/packages/compare">
          <Button icon={<ScaleOutlined />}>Compare packages</Button>
        </Link>
      </div>

      {!packages.isLoading && !packagesByProducts.some((q) => q.isLoading) && rows.length === 0 ? (
        <EmptyStateCard
          title={search.trim() || status ? 'Nothing matches this filter' : 'No packages discovered yet'}
          explanation={
            search.trim()
              ? 'No release on this page matches what you typed. The search covers the version and the repository it came from.'
              : status
                ? `No release currently has this status. Clear the filter to see everything discovered${selected ? ' for this product' : ''}.`
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
          <DataTable
            tableEnhancedKey="packages"
            allow_export
            show_column_visibility
            loading={packages.isLoading || packagesByProducts.some((q) => q.isLoading)}
            dataSource={rows}
            rowKey={(r) => `${r.product.productId}-${r.pkg.packageId}`}
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
              /*
                THE PRODUCT COLUMN EXISTS ONLY WHEN IT VARIES.

                Unscoped, this listing spans every product and the column is
                the only thing telling two identically-versioned rows apart.
                Scoped by the select above it, every row carries the same
                value - and that costs 235px of a table already wider than the
                window, which is paid for by the pinned Actions column
                covering whatever falls off the right-hand end. A column that
                cannot distinguish two rows is not worth a reader losing one
                that can.
              */
              ...(selected ? [] : [{
                title: 'Product',
                width: 190,
                render: (_: unknown, r: (typeof rows)[number]) => (
                  <span
                    style={{
                      display: 'block', whiteSpace: 'nowrap',
                      overflow: 'hidden', textOverflow: 'ellipsis',
                    }}
                    title={r.product.displayName || r.product.productId}
                  >
                    {r.product.displayName || r.product.productId}
                  </span>
                ),
              }]),
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
                render: (_, r) => (
                  <Link to={releaseHref(r.product.productId, r.pkg)}>
                    <PackageName pkg={r.pkg} width={220} />
                  </Link>
                ),
              },
              {
                title: 'Version',
                width: 160,
                render: (_, r) => (
                  <VersionChip
                    product={r.product.productId}
                    version={version(r.pkg)}
                    pkg={r.pkg}
                    showRepository={false}
                  />
                ),
              },
              {
                title: 'Published',
                width: 118,
                render: (_, r) => <TimeAgo at={r.pkg.publishedAt || r.pkg.discoveredAt} />,
              },
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
                width: 240,
                render: (_, r) => (
                  <RowVulnerability
                    product={r.product.productId}
                    pkg={r.pkg}
                    onSync={() => syncNotSynced(r.product.productId, r.pkg)}
                  />
                ),
              },
              {
                title: 'Location',
                width: 150,
                render: (_, r) => (
                  <LocationChip
                    locations={deriveLocations(r.pkg, r.product)}
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
                render: (_, r) => <RowActions product={r.product.productId} pkg={r.pkg} config={r.product} />,
              },
            ]}
          />
        </Card>
      )}
    </>
  )
}
