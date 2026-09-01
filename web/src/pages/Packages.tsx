import { useMemo, useState } from 'react'
import { App, Button, Card, Dropdown, Segmented, Select, Space, Tooltip, Typography } from 'antd'
// The working-surface table: resizable, reorderable, pinnable columns whose
// layout each person keeps. See `tablekit/README.md` for which tables get it.
import { Table as DataTable } from '../tablekit'
import type { MenuProps } from 'antd'
import { ClusterOutlined, MoreOutlined, SafetyCertificateOutlined, ScaleOutlined } from '../icons'
import { Link, useNavigate, useSearchParams } from 'react-router-dom'
import {
  usePackages, usePackagesByProducts, useProducts, useRunDownload, useSyncPackageSecurity, useTransfers,
} from '../api/queries'
import { useCan } from '../auth/permissions'
import {
  deriveLocations, deriveStatus, downloadSeconds, failureReason, isLive, isPromotion, matches,
  hasSecurityData, packageReference, promotableTargets, publishedAt, releaseHref, transferIndex,
  verification, version, withTransfers,
} from '../domain/derive'
import type { Package, PackageTransfer, Product } from '../api/types'
import { formatDuration } from '../domain/format'
import { Value } from '../components/value'
import {
  AnalysisTag, LocationChip, PackageName, StatusBadge, TimeAgo, VerificationBadge,
  VersionChip,
} from '../components/chips'
import { EmptyStateCard, ErrorState, SearchBar } from '../components/layout'
import { CompareSelectionBar } from '../components/compareselect'
import {
  COMPARISON_PRODUCT_FILTER, pickOf, samePick, useComparisonSelection,
} from '../domain/compare'
import { NokiaNIcon } from '../components/icons'
import { VulnerabilityCell } from '../components/security'
import { PromoteButton } from '../components/promote'
import { c } from '../uikit'

/**
 * A row's place in the comparison: unticked, first, or second.
 *
 * # Why this is not a plain checkbox
 *
 * Because the two ends are not interchangeable. One is the base and the other
 * is what it is compared against, every verdict on the report is phrased in
 * that direction, and a grid of identical ticks cannot say which is which. The
 * badge shows the ORDER, so a reader glancing back at the table can see which
 * row is the baseline without reading the bar.
 *
 * An unticked row is still a box rather than an empty cell: a column of
 * nothing, with two numbers in it, does not read as something to click.
 */
function ComparePick({ slot, blocked, onToggle }: {
  slot: number
  blocked: boolean
  onToggle: () => void
}) {
  if (blocked) {
    return (
      <Tooltip title="A comparison covers one product. Clear the selection to compare releases of this one.">
        <span
          style={{
            display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
            width: 22, height: 22, borderRadius: 6,
            border: `1px dashed ${c.border}`, color: c.text3, fontSize: 11,
            cursor: 'not-allowed',
          }}
        >
          -
        </span>
      </Tooltip>
    )
  }

  return (
    <Tooltip title={slot ? 'Remove from the comparison' : 'Add to the comparison'}>
      <button
        type="button"
        onClick={onToggle}
        aria-pressed={slot > 0}
        aria-label={slot ? `Selected as package ${slot}` : 'Select for comparison'}
        style={{
          display: 'inline-flex', alignItems: 'center', justifyContent: 'center',
          width: 22, height: 22, borderRadius: 6, cursor: 'pointer',
          fontSize: 11, fontWeight: 600, lineHeight: 1,
          background: slot ? c.brand : 'transparent',
          color: slot ? '#fff' : 'transparent',
          border: slot ? 'none' : `1px solid ${c.border}`,
          transition: 'background 120ms ease, border-color 120ms ease',
        }}
      >
        {slot || '+'}
      </button>
    </Tooltip>
  )
}

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
function RowActions({ product, pkg, config, autoProductFilter }: {
  product: string
  pkg: Package
  /** The product's configuration, so the row knows where this could still go. */
  config?: Product
  autoProductFilter: boolean
}) {
  const { message } = App.useApp()
  const navigate = useNavigate()
  const sync = useSyncPackageSecurity()
  const mayOperate = useCan('operate', { product })

  // "Compare with another release" now PRE-SELECTS this one and stays here.
  //
  // It used to leave for a page whose first job was to ask which release the
  // reader meant - which they had just told it by clicking this row - and whose
  // second was to ask for the other one from a dropdown of two hundred. Both
  // halves of that are this listing's job, and it is already open.
  const compareHref = `/packages?compare=1`
    + `&cmp=${encodeURIComponent(product)}`
    + `&product=${encodeURIComponent(product)}`
    + (autoProductFilter ? `&${COMPARISON_PRODUCT_FILTER}=1` : '')
    // The REPOSITORY travels with the tag. One version tag exists in every
    // repository a product watches, so a reference carrying only the tag does
    // not name a package.
    + `&a=${encodeURIComponent(packageReference(pkg))}`

  const locationsHref = `/packages/compare?mode=locations`
    + `&product=${encodeURIComponent(product)}`
    + `&a=${encodeURIComponent(packageReference(pkg))}`

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
    // The OTHER comparison, and it is a different question: not "what changed
    // between these two releases" but "did this one arrive intact". It is about
    // ONE release, so it cannot be expressed by ticking two rows - it keeps a
    // small form of its own, on a page that already knows which release.
    {
      key: 'compare-locations',
      label: <Link to={locationsHref}>Compare across locations</Link>,
    },
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

  /*
   * Choosing two releases to compare, IN this listing.
   *
   * See domain/compare.ts for why the selection lives in the URL: it has to
   * survive the search box, the product filter and the round trip to the
   * report, and every one of those unmounts component state.
   */
  const {
    selection, toggle, start, reset, cancel, swap, setIntent,
  } = useComparisonSelection()
  const comparing = selection.active
  // Only the vulnerabilities intent narrows the list. Comparing contents is a
  // question every release can answer, so nothing is hidden for it.
  const forVulnerabilities = comparing && selection.intent === 'vulnerabilities'

  /*
   * Every release this page has loaded, BEFORE the search and status filters.
   *
   * Split out from `rows` for one reason, and it is the reason the whole
   * selection lives in the URL: a comparison in progress has to be able to
   * NAME the two releases it holds even when neither is on screen. Resolving
   * them against the filtered rows meant that typing in the search box emptied
   * the bar back to "select the first package" - the selection was intact, and
   * the page said it was gone, which is worse than losing it.
   */
  const allRows = useMemo(() => {
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
    return [...all].sort((a, b) => {
      const left = Date.parse(publishedAt(a.pkg) || '') || 0
      const right = Date.parse(publishedAt(b.pkg) || '') || 0
      return right - left
    })
  }, [selected, product, productList, packages.data, packagesByProducts, transfers.data])

  const rows = useMemo(() => {
    const byStatus = !status
      ? allRows
      : status === 'UNSIGNED'
        ? allRows.filter((r) => verification(r.pkg) === 'NOT_SIGNED')
      : status === 'READY'
        ? allRows.filter((r) => r.status === 'READY FOR PRODUCTION')
        : allRows.filter((r) => r.status === status)

    /*
      In the vulnerabilities intent, a release with no findings is not a choice
      that happens to be unavailable - it cannot answer the question, so it is
      not offered. The count line above the table says how many that is, which
      is the whole explanation this filter needs.
    */
    const relevant = forVulnerabilities ? byStatus.filter((r) => hasSecurityData(r.pkg)) : byStatus

    if (!search.trim()) return relevant
    // The version as shown AND as the vendor spells it, plus the repository -
    // a product publishes one version tag into every repository it watches, so
    // the repository is frequently the only thing telling two rows apart.
    return relevant.filter((r) => matches(
      search, version(r.pkg), r.pkg.tag, r.pkg.displayRepository, r.pkg.sourceRepository))
  }, [allRows, status, search, forVulnerabilities])

  const update = (key: string, value?: string) => {
    const next = new URLSearchParams(params)
    if (value) next.set(key, value)
    else next.delete(key)
    if (key === 'product') next.delete(COMPARISON_PRODUCT_FILTER)
    setParams(next)
  }

  // The product a selection is locked to, once one end is chosen. Two products
  // are two sets of repositories under two sets of credentials, so a comparison
  // across them is not a thing the API can answer - and the rows that would
  // make one are disabled with the reason on them, which teaches the rule
  // better than a product dropdown ever did.
  const lockedProduct = selection.a?.product ?? selection.b?.product

  /**
   * How many releases the vulnerabilities intent is holding back.
   *
   * Counted against the STATUS-filtered set rather than everything loaded, so
   * the number describes what this filter removed and not what some other one
   * did - two explanations for one missing row is how a reader stops trusting
   * either.
   */
  const hiddenWithoutScan = useMemo(() => {
    if (!forVulnerabilities) return 0
    const eligible = !status
      ? allRows
      : status === 'UNSIGNED'
        ? allRows.filter((r) => verification(r.pkg) === 'NOT_SIGNED')
      : status === 'READY'
        ? allRows.filter((r) => r.status === 'READY FOR PRODUCTION')
        : allRows.filter((r) => r.status === status)
    return eligible.filter((r) => !hasSecurityData(r.pkg)).length
  }, [allRows, status, forVulnerabilities])

  /**
   * Resolves a chosen reference back to the release it names.
   *
   * Against `allRows` and never the filtered ones - see the comment there. A
   * selection has to keep its name while the reader searches for the other end,
   * which is the whole shape of choosing two things out of two hundred.
   */
  const pickedPackage = (pick: { product: string; ref: string }) =>
    allRows.find((r) => r.product.productId === pick.product
      && packageReference(r.pkg) === pick.ref)?.pkg

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
      {comparing && (
        /*
          The page says what it is FOR while it is in this mode, and asks the
          one question that changes which releases are worth offering.

          A table that has grown a column of plus signs is a table with an
          unexplained column; a title saying "select two packages to compare" is
          the whole instruction. The switch beside it is the intent - see
          domain/compare.ts - and it is asked HERE rather than on the report
          because it decides what the list should contain.
        */
        <Space direction="vertical" size={0} style={{ marginBottom: 4 }}>
          <Typography.Title level={4} style={{ margin: 0 }}>
            Select two packages to compare
          </Typography.Title>
          {/*
            One line, and only when the filter is actually hiding something. It
            explains the missing rows and nothing else - a reader who has just
            narrowed a list wants to know what left it, not to read a paragraph
            about why.
          */}
          {forVulnerabilities && hiddenWithoutScan > 0 && (
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              {hiddenWithoutScan.toLocaleString()} release
              {hiddenWithoutScan === 1 ? '' : 's'} without a vulnerability sync
              {hiddenWithoutScan === 1 ? ' is' : ' are'} hidden.
            </Typography.Text>
          )}
        </Space>
      )}

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
        {/*
          The far end of the row, where the control that STARTS a comparison
          sits and, once one is being chosen, the control that says what it is
          for. One position, one job at a time: the two never both apply, and
          keeping them in the same place means the row does not reflow under
          the reader when they begin.

          Icons because the two words are near-synonyms at a glance beside four
          other controls, and the shield is the same one the release's own
          Security tab uses - the same picture meaning the same thing.
        */}
        {comparing ? (
          <Segmented
            value={selection.intent}
            onChange={(v) => setIntent(
              v as 'contents' | 'vulnerabilities',
              // Whether a release can answer the new question is the listing's
              // knowledge, so the listing supplies the test.
              (pick) => {
                const row = allRows.find((r) => r.product.productId === pick.product
                  && packageReference(r.pkg) === pick.ref)
                return Boolean(row && hasSecurityData(row.pkg))
              },
            )}
            options={[
              { value: 'contents', label: 'Contents', icon: <ClusterOutlined /> },
              {
                value: 'vulnerabilities',
                label: 'Vulnerabilities',
                icon: <SafetyCertificateOutlined />,
              },
            ]}
          />
        ) : (
          /*
            Starts selection mode HERE rather than navigating.

            It was a link to a page whose whole content was a form asking which
            two releases - a question this table answers better than any
            dropdown can, because a reader deciding what to compare is reading
            status, dates and vulnerability counts, and none of those fit in a
            select.
          */
          <Button icon={<ScaleOutlined />} onClick={() => start()}>Compare packages</Button>
        )}
      </div>

      {comparing && (
        <CompareSelectionBar
          selection={selection}
          packagesFor={pickedPackage}
          productName={lockedProduct
            ? productList.find((p) => p.productId === lockedProduct)?.displayName ?? lockedProduct
            : undefined}
          onReset={reset}
          onCancel={cancel}
          onSwap={swap}
        />
      )}

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
            /*
              The whole row is the target while choosing.

              A 22-pixel box at the left edge of a table 1,600 pixels wide is a
              small target hit twice per comparison, and the row is already the
              thing the reader is looking at. The links and buttons inside it
              keep working - see the guard in onClick - so this adds a way to
              select without taking away a way to navigate.
            */
            onRow={comparing ? (r) => ({
              onClick: (event) => {
                const target = event.target as HTMLElement
                if (target.closest('a, button, input, .ant-dropdown-trigger')) return
                const pick = pickOf(r.product.productId, r.pkg)
                if (lockedProduct && r.product.productId !== lockedProduct
                  && !samePick(selection.a, pick) && !samePick(selection.b, pick)) return
                toggle(pick)
              },
              style: { cursor: 'pointer' },
            }) : undefined}
            rowClassName={comparing
              ? (r) => {
                const pick = pickOf(r.product.productId, r.pkg)
                return samePick(selection.a, pick) || samePick(selection.b, pick)
                  ? 'slm-row-selected'
                  : ''
              }
              : undefined}
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
            /*
              THE SELECTION IS A COLUMN, not antd's rowSelection.

              rowSelection gives a "select all" box in the header and unbounded
              multi-select, and this is neither: exactly two, in an order that
              means something. A header checkbox that ticks two hundred rows,
              on a control that accepts two, is a control that lies about what
              it does - and the numbered badges below are what make the ORDER
              visible, which a uniform grid of checkboxes cannot.
            */
            columns={[
              ...(comparing ? [{
                title: '',
                fixed: 'left' as const,
                width: 52,
                render: (_: unknown, r: (typeof rows)[number]) => {
                  const pick = pickOf(r.product.productId, r.pkg)
                  const slot = samePick(selection.a, pick)
                    ? 1
                    : samePick(selection.b, pick) ? 2 : 0
                  // Locked to one product once an end is chosen: two products
                  // are two sets of credentials, and the API cannot compare
                  // across them. Disabled with the reason on it beats a
                  // dropdown that silently narrows the list.
                  const blocked = Boolean(lockedProduct)
                    && r.product.productId !== lockedProduct
                    && slot === 0
                  return (
                    <ComparePick
                      slot={slot}
                      blocked={blocked}
                      onToggle={() => toggle(pick)}
                    />
                  )
                },
              }] : []),
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

                  No hard width here: the tablekit owns the width and resize
                  behaviour, and the label should widen or narrow with the column
                  instead of staying locked to a pixel value.
                */
                title: 'Name',
                fixed: 'left',
                render: (_, r) => (
                  <Link
                    to={releaseHref(r.product.productId, r.pkg)}
                    style={{
                      display: 'block',
                      width: '100%',
                      minWidth: 0,
                      maxWidth: '100%',
                    }}
                  >
                    <PackageName pkg={r.pkg} />
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
              /*
                THE COLUMN THE DECISION IS BEING MADE ON COMES SECOND.

                Column order is a priority order here - the table is wider than
                a laptop window and whatever sits last is what the pinned
                Actions column covers. Somebody choosing two releases to compare
                VULNERABILITIES is reading the counts, and leaving them where
                they are for the contents intent put the deciding column half
                behind another one.
              */
              ...(forVulnerabilities ? [{
                title: 'Vulnerabilities',
                width: 240,
                render: (_: unknown, r: (typeof rows)[number]) => (
                  <RowVulnerability
                    product={r.product.productId}
                    pkg={r.pkg}
                    onSync={() => syncNotSynced(r.product.productId, r.pkg)}
                  />
                ),
              }] : []),
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
                // Moved to second while choosing on vulnerabilities, so it is
                // rendered once either way.
                hidden: forVulnerabilities,
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
                render: (_, r) => (
                  <RowActions
                    product={r.product.productId}
                    pkg={r.pkg}
                    config={r.product}
                    autoProductFilter={!selected}
                  />
                ),
              },
            ]}
          />
        </Card>
      )}
    </>
  )
}
