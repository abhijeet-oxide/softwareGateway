import { useEffect, useMemo, useState } from 'react'
import { App, Button, Card, Dropdown, Segmented, Select, Space, Tooltip, Typography } from 'antd'
// The working-surface table: resizable, reorderable, pinnable columns whose
// layout each person keeps. See `tablekit/README.md` for which tables get it.
import { Table as DataTable, useTablePageSize } from '../tablekit'
import type { MenuProps } from 'antd'
import { ClusterOutlined, CompareOutlined, MoreOutlined, SafetyCertificateOutlined } from '../icons'
import { Link, useNavigate, useSearchParams } from 'react-router-dom'
import {
  usePackageListing, useProducts, useRunDownload, useSyncPackageSecurity,
} from '../api/queries'
import { useCan } from '../auth/permissions'
import { ActionButton } from '../components/access'
import {
  deriveLocations, deriveStatus, failureReason, isLive, isPromotion,
  hasSecurityData, packageReference, promotableTargets, releaseHref,
  verification, version,
} from '../domain/derive'
import type { Package, PackageTransfer, Product } from '../api/types'
import {
  AnalysisTag, LocationChip, PackageName, StatusBadge, TimeAgo, VerificationMark, VersionToken,
} from '../components/chips'
import { CellStack } from '../components/cell'
import { EmptyStateCard, ErrorState, SearchBar } from '../components/layout'
import { CompareSelectionBar } from '../components/compareselect'
import {
  COMPARISON_PRODUCT_FILTER, pickOf, samePick, useComparisonSelection,
} from '../domain/compare'
import { Icon, NokiaNIcon, PackageIcon } from '../components/icons'
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
function RowActions({ product, pkg, config }: {
  product: string
  pkg: Package
  /** The product's configuration, so the row knows where this could still go. */
  config?: Product
}) {
  const detail = releaseHref(product, pkg)

  // WHAT HAS HAPPENED TO THIS RELEASE, split by kind. Everything below reads
  // from this rather than from `pkg.transfers` directly: the button and the
  // menu have to agree about which transfer they mean, and they used to
  // recompute it separately.
  const history = releaseHistory(pkg)

  /*
    WHAT IS NOT ALREADY INSIDE THE RELEASE.

    This menu had grown to six entries, and four of them led somewhere the
    release's own page already offers: "View vulnerabilities" and the two sync
    entries are the Security tab, and "Compare with another release" is the
    Compare packages button at the top of this very table. A menu that mostly
    restates the page it sits on is a menu a reader learns to ignore, and it
    made the two entries that ARE only here - the transfers this release came
    out of - the hardest to find.

    "Compare across locations" is a real question this table cannot ask
    (one release, several places, did it arrive intact) but it is not wanted
    yet, so it is not offered yet.
  */
  const items: MenuProps['items'] = [
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
        promotable={promotableTargets(pkg, config).length > 0}
      />
      {/*
        No entries, no button. A release nothing has happened to yet has
        nothing behind the dots, and a control that opens an empty menu is a
        control that has to be tried before it can be dismissed.
      */}
      {items.length > 0 && (
        <Dropdown menu={{ items }} trigger={['click']} placement="bottomRight">
          <Button size="small" icon={<MoreOutlined />} aria-label="More actions" />
        </Dropdown>
      )}
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
  product, pkg, history, promotable,
}: {
  product: string
  pkg: Package
  history: ReleaseHistory
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

  // No catch: the failure reaches the reader through the query client, with the
  // Coordinator's own sentence, its code and its request id. See
  // components/feedback.
  const start = async () => {
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
  }

  /*
    The row's primary, and the only filled button in it. Green was a second
    accent that existed nowhere else in the product and that no palette could
    reach; what makes this button the loud one is that it is the step the row is
    actually on, which the brand colour is for.

    DISABLED rather than hidden, unusually: this is one cell of a table column,
    and a column whose cells appear on some rows and not others reads as a
    rendering fault rather than as a permission. The reason is on the hover.
  */
  return (
    <ActionButton
      permission="software_download.request"
      scope={{ product }}
      whenDenied="disable"
      action="Download"
      size="small"
      type="primary"
      busy={run.isPending}
      title={`Download ${version(pkg)} whole into the internal repositories. Artifacts already present are skipped.`}
      onClick={start}
    >
      Download
    </ActionButton>
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
  // Syncing a release's security state reaches a scanner and writes what it
  // says, which is an inspection rather than a read.
  const mayOperate = useCan('package.inspect', { product })
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
  /*
    TWO PIECES OF SEARCH STATE, and the second is not redundant.

    `draft` is what the box shows and `search` is what the SERVER is asked.
    The search used to be neither: the page fetched a hundred rows per product
    and ran a substring test over them here, which searches what happened to
    be loaded rather than what exists - so a release on the second page came
    back as "nothing matches", confidently. Now the database answers it, and
    the debounce below is what makes typing still feel like typing rather than
    like a request per key.
  */
  const [draft, setDraft] = useState('')
  const [search, setSearch] = useState('')
  const products = useProducts()
  const productList = products.data?.products ?? []

  const selected = params.get('product') ?? undefined
  const status = params.get('status')
  const tag = params.get('tag') ?? undefined

  /*
    WHICH PAGE, AND HOW BIG, both server-side.

    The page size is the table's own selector - the same remembered value every
    other paginated table in the product uses - and it is part of the REQUEST
    here rather than a slice of rows already in hand. That is the whole point:
    a listing of two hundred rows costs a page of them, not all of them.

    `pageToken` for this API is an offset, so any page can be asked for
    directly rather than walked to. See parseOffset.
  */
  const [pageSize, setPageSize] = useTablePageSize('packages', 25)
  const [page, setPage] = useState(1)

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
    THE STATUS FILTER IS DERIVED, and it is the one filter the server cannot
    answer.

    A release's status comes from what has been ATTEMPTED with it, against the
    product's own idea of which target is production - so it is computed from
    the row and its transfer history rather than stored anywhere to filter on.
    That means it narrows the rows that were fetched, and a page of twenty-five
    is a thin thing to narrow. The same is true of the vulnerabilities intent,
    which hides a release no scanner has answered for.

    So either of them WIDENS the fetch. The table still shows `pageSize` rows
    at a time; what changes is how many releases the filter got to look at, and
    the line under the toolbar says what that was. An unfiltered listing - the
    ordinary case - still fetches exactly one page.

    Two hundred rather than the five hundred the server will serve, because
    each of those rows carries its transfer history and that history is read
    under one shared bound: a wider page spreads the same allowance thinner,
    until a release's own attempts start falling off the end and its status
    reads as though nothing had been tried. See attachTransfers.
  */
  const filtering = Boolean(status) || forVulnerabilities
  const fetchSize = filtering ? 200 : pageSize
  const fetchPage = filtering ? 1 : page

  // Debounced, so the server sees one query per pause rather than one per key.
  // 300ms: long enough that a typed word is one request, short enough that the
  // table has moved before the finger leaves the key.
  useEffect(() => {
    const t = setTimeout(() => setSearch(draft), 300)
    return () => clearTimeout(t)
  }, [draft])

  // Any change to WHAT is being listed starts again at the first page. A page
  // four that survived a new search term would be a page four of a different
  // listing, which is how a filtered table comes back empty for no visible
  // reason.
  useEffect(() => {
    setPage(1)
  }, [search, selected, status, tag, pageSize])

  const product = productList.find((p) => p.productId === selected)
  const packages = usePackageListing(selected, {
    pageSize: fetchSize,
    pageToken: fetchPage > 1 ? String((fetchPage - 1) * fetchSize) : undefined,
    q: search.trim() || undefined,
    tag,
  })
  const listed = packages.data?.packages ?? []
  /*
    THE FIRST LOAD, or a page the reader is waiting for.

    Not `isFetching`, which is also true of the five-second poll a release
    being analysed sets off - that would spin the table at somebody reading it.
    `isPlaceholderData` is true only while the rows on screen belong to the
    PREVIOUS query key, which is exactly the search, the page turn and the
    filter change. The delay keeps a fast answer from flashing a spinner.
  */
  const loading = packages.isLoading
  const waiting = { spinning: loading || (packages.isPlaceholderData && packages.isFetching), delay: 250 }

  /*
   * Every release this page has loaded, BEFORE the status filter.
   *
   * Split out from `rows` for one reason, and it is the reason the whole
   * selection lives in the URL: a comparison in progress has to be able to
   * NAME the two releases it holds even when neither is on screen. Resolving
   * them against the filtered rows meant that typing in the search box emptied
   * the bar back to "select the first package" - the selection was intact, and
   * the page said it was gone, which is worse than losing it.
   *
   * No transfer join any more. A listing row carries its own history now, so
   * the two-hundred-transfer fetch this page used to make to derive a status -
   * and which was wrong for any page but the first, because page four's
   * releases were downloaded long before the two-hundredth most recent
   * transfer - is gone. See attachTransfers.
   */
  const allRows = useMemo(
    () => listed.map((pkg) => {
      const config = selected
        ? product
        : productList.find((p) => p.productId === pkg.product)
      return {
        pkg,
        // A release whose product is not in the configuration listing still has
        // to render: the row is real, and dropping it would report the estate as
        // smaller than it is. The id is what a product chip has to show then.
        product: config ?? { productId: pkg.product } as Product,
        status: deriveStatus(pkg, config),
      }
    }),
    [listed, selected, product, productList],
  )

  const rows = useMemo(() => {
    /*
      THE TWO FILTERS THE SERVER CANNOT ANSWER, and nothing else.

      The search left this function entirely - the database does it now, over
      every release rather than over the page that happened to be loaded. What
      remains is derived from the row: a status computed from its transfer
      history against the product's targets, and whether a scanner has ever
      answered for it. Both are applied over the widened fetch a filter asks
      for; the line under the toolbar says how wide that was.
    */
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
    return forVulnerabilities ? byStatus.filter((r) => hasSecurityData(r.pkg)) : byStatus
  }, [allRows, status, forVulnerabilities])

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
          {/*
            THE BOX SHOWS `draft`; THE SERVER IS ASKED `search`.

            No matched/total counts on it any more, and their absence is the
            honest change: they used to say "12 of 300 shown", which was a
            statement about the rows this page had loaded rather than about the
            estate. The database answers the search now, so what a match count
            would have to say is "12 of however many there are", and the pager
            below says exactly that.
          */}
          <SearchBar
            value={draft}
            onChange={setDraft}
            placeholder="Search by package or version"
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
              // Last, because it is the only one describing what the VENDOR
              // did rather than what this system did.
              { value: 'ARCHIVED', label: 'Archived at the source' },
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
          <Button icon={<CompareOutlined />} onClick={() => start()}>Compare packages</Button>
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

      {/*
        WHAT THE STATUS FILTER GOT TO LOOK AT.

        Only when one is on, and it is the one thing a derived filter has to
        say about itself: it is computed from each row's transfer history
        rather than asked of the database, so it can only narrow releases that
        were fetched. The fetch is widened for it - see `fetchSize` - and this
        line states the size of that window instead of leaving the reader to
        assume the filter covered everything.
      */}
      {filtering && !loading && (
        <Typography.Text type="secondary" style={{ fontSize: 12, display: 'block', marginBottom: 8 }}>
          {rows.length.toLocaleString()} of the{' '}
          {allRows.length.toLocaleString()} most recent release
          {allRows.length === 1 ? '' : 's'}
          {packages.data?.nextPageToken
            ? '. Older releases are not included in this filter - narrow by product or search for one by name.'
            : selected ? ' of this product.' : ' in the estate.'}
        </Typography.Text>
      )}

      {!loading && rows.length === 0 ? (
        <EmptyStateCard
          title={search.trim() || status ? 'Nothing matches this filter' : 'No packages discovered yet'}
          explanation={
            search.trim()
              ? 'No release matches what you typed. The search covers the package name and the version, in either the vendor\'s spelling or the shortened one.'
              : status
                ? `No release among the most recent has this status. Clear the filter to see everything discovered${selected ? ' for this product' : ''}.`
                : 'Discovery polls the vendor registries on a schedule. Run it from the Overview to look immediately.'
          }
          action={
            search.trim()
              ? <Button onClick={() => setDraft('')}>Clear search</Button>
              : status
                ? <Button onClick={() => update('status', undefined)}>Clear filter</Button>
                : <Link to="/"><Button type="primary">Go to Overview</Button></Link>
          }
        />
      ) : (
        <Card className={comparing ? 'slm-compare-table' : undefined} styles={{ body: { padding: 0 } }}>
          <DataTable
            tableEnhancedKey="packages"
            // allow_export
            // show_column_visibility
            // toolbarPlacement="outside"
            loading={waiting}
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
            /*
              SERVER-SIDE PAGING, so the page holds what is on screen and
              nothing else.

              `total` is the honest shape of a token-paged API: it does not
              return a count, and inventing one would put a page number on the
              pager that leads nowhere. A next-page token means "there is at
              least one more", which is one row past the end of this page; no
              token means this page is the last, so the total is where it ends.
              The pager draws the right number of steps either way.

              With a status filter on, the fetch is one wide page and the table
              pages through it here - the derived filter has to see the rows to
              narrow them, so there is nothing left for the server to page.
            */
            pagination={filtering ? {
              pageSize,
              onShowSizeChange: (_current, size) => setPageSize(size),
            } : {
              current: page,
              pageSize,
              total: packages.data?.nextPageToken
                ? page * pageSize + 1
                : (page - 1) * pageSize + rows.length,
              onShowSizeChange: (_current, size) => setPageSize(size),
              onChange: (next) => setPage(next),
            }}
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
              {
                /*
                  ONE COLUMN CARRIES THE IDENTITY: what it is, whose it is,
                  which release.

                  These were three columns - Product, Name, Version - and
                  between them they took over half the width of a table already
                  wider than a laptop window. The cost was paid by the columns
                  a reader is actually deciding on: Status, Vulnerabilities and
                  Location fell off the right-hand end, under the pinned
                  Actions column, and answering "is this one safe to ship"
                  meant scrolling a table sideways.

                  Stacked, the same three facts cost one column. The name leads
                  because it is what tells two rows apart - a product publishes
                  one version tag into every repository it watches - and the
                  product and version sit under it in the smaller type
                  CellStack gives every stacked cell in the product, so this
                  table's rows are the same height as the Policies table's.

                  The product line appears only when the listing spans more
                  than one, for the reason the Product COLUMN used to appear
                  only then: a value identical on every row tells nobody
                  anything.
                */
                title: 'Release',
                fixed: 'left',
                /*
                  A WIDTH, so the column can be resized.

                  Without one the tablekit has no number to resize FROM, and
                  the drag handle on this column did nothing while every other
                  column moved. The stacked cell ellipsises rather than wraps,
                  so narrowing it shortens the name instead of making the row
                  two lines taller.
                */
                width: 300,
                render: (_, r) => (
                  <Link
                    to={releaseHref(r.product.productId, r.pkg)}
                    style={{ display: 'block', width: '100%', minWidth: 0, maxWidth: '100%' }}
                  >
                    <CellStack
                      title={
                        <span style={{ display: 'flex', alignItems: 'center', gap: 6, minWidth: 0 }}>
                          <span style={{ minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis' }}>
                            <PackageName pkg={r.pkg} />
                          </span>
                          {/*
                            The signature, on the name it is a signature OF.
                            A glyph rather than the full pill: see
                            VerificationMark. It cost a 120px column before,
                            and sat two columns away from the thing it was
                            asserting about.
                          */}
                          <VerificationMark state={verification(r.pkg)} />
                        </span>
                      }
                      lines={[
                        /*
                          WHOSE IT IS and WHICH RELEASE, drawn differently, on
                          ONE line.

                          These were two pieces of grey text with a dot
                          between them, so the two facts a reader most needs
                          to separate looked identical. The product keeps an
                          icon and stays prose; the version becomes a mono
                          token, because it is an identifier and that is how
                          this product draws identifiers everywhere else. One
                          line rather than two entries, because once the two
                          look different the separator between them is just
                          another mark to read.
                        */
                        <span
                          key="meta"
                          style={{ display: 'inline-flex', alignItems: 'center', gap: 7, minWidth: 0 }}
                        >
                          {!selected && (
                            <span style={{ display: 'inline-flex', alignItems: 'center', gap: 4, minWidth: 0 }}>
                              <Icon as={PackageIcon} title="Product" size={11} />
                              <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                                {r.product.displayName || r.product.productId}
                              </span>
                            </span>
                          )}
                          <VersionToken version={version(r.pkg)} />
                        </span>,
                      ]}
                    />
                  </Link>
                ),
              },
              {
                title: 'Published',
                width: 118,
                render: (_, r) => <TimeAgo at={r.pkg.publishedAt || r.pkg.discoveredAt} />,
              },
              {
                /*
                  WHERE THIS RELEASE IS IN ITS LIFE.

                  Signed used to be a column of its own, then briefly a third
                  pill here. It is neither now: it belongs on the NAME, because
                  it is an assertion about that exact artifact rather than a
                  step in its progress, and two pills per row wrapped this
                  column onto two lines on any row with a long status.
                */
                title: 'Status',
                width: 170,
                render: (_, r) => (
                  <Space size={4} wrap>
                    <StatusBadge status={r.status} reason={failureReason(r.pkg)} />
                    <AnalysisTag pkg={r.pkg} />
                  </Space>
                ),
              },
              {
                /*
                  Always on.

                  The counts come from the listing response itself, written by
                  a sync rather than fetched per row - which is what made this
                  a toggle before, and a toggle is a design apologising for
                  itself. It used to be hoisted ahead of the other columns
                  while choosing two releases to compare on vulnerabilities,
                  because whatever sat last was covered by the pinned Actions
                  column; folding three identity columns into one left room for
                  every column at once, so there is nothing left to hoist.
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
                title: 'Actions',
                fixed: 'right',
                /*
                  Wide enough for the widest row it can draw: View, then "View
                  download", then the dots. At 190 the dots were clipped by a
                  few pixels - invisible while the table let itself be scrolled
                  those few pixels, and a visibly cut-off button once it
                  stopped.
                */
                width: 214,
                render: (_, r) => (
                  <RowActions
                    product={r.product.productId}
                    pkg={r.pkg}
                    config={r.product}
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
