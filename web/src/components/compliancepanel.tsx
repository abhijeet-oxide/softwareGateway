import { useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import {
  App, Button, Card, Descriptions, Drawer, Empty, Input, Segmented, Select, Skeleton,
  Space, Table, Tag, Tooltip, Typography,
} from 'antd'
import type { ReactNode } from 'react'
// The working-surface table: resizable, reorderable, pinnable columns whose
// layout each person keeps. See `tablekit/README.md` for which tables get it.
import { Table as DataTable } from '../tablekit'
import {
  BookOutlined, FileTextOutlined, HelmOutlined, LoadingOutlined, SearchOutlined, SyncOutlined,
} from '../icons'
import {
  useCancelCompliance, useInspectPackage, usePackageCompliance,
  useRenderedManifests, useRunCompliance,
} from '../api/queries'
import { EmptyStateCard } from './layout'
import {
  CheckSeverityTag, ComplianceSummary, DeterminacyTag,
  HelmMissingNotice, OutcomePill, ResultAddress, RunFailedNotice,
  TruncatedNotice,
} from './compliance'
import type { SummaryKey } from './compliance'
import { ComplianceRunLogButton, ComplianceRunPanel } from './complianceprogress'
import {
  DownloadManifestsButton, EvidencePanel, ManifestDrawer, ManifestLinks,
  NoEvidenceNotice,
} from './complianceevidence'
import { formatCount, formatRelative } from '../domain/format'
import { c, mono } from '../uikit'
import type {
  ComplianceChart, ComplianceCounts, ComplianceResult, ComplianceRun,
} from '../api/types'

/**
 * The Compliance tab of a release: does this follow our own Kubernetes and CNF
 * standards.
 *
 * # The order of what is on screen, and why
 *
 * The caveats come first, then the verdict, then the numbers, then the rows.
 * That is the order of DECREASING trust, the same order the Security tab uses,
 * and for the same reason: the first thing a reader meets is whether this
 * result covers the whole release. A tab with the caveat at the bottom is a tab
 * whose verdict gets quoted in a release meeting without it.
 *
 * The one caveat that is NOT a banner is the unchecked count. It used to be,
 * and on a real orb every release has one, so the banner was on every screen -
 * which is how a caveat stops being read and starts being scrolled past. It is
 * a coloured, clickable tile beside the verdict instead, in the summary that
 * qualifies.
 *
 * # Why the default view is failures
 *
 * A release produces ten to fifteen thousand results, most of them passes.
 * Opening on all of them buries the four rows somebody has to act on. The
 * counts stay on screen either way, so the short list is never mistaken for a
 * small denominator, and one control switches to the coverage view.
 */
/**
 * The outcome slices a reader moves between, and what each one costs to fetch.
 *
 * # Why these are server-side filters and not tabs over one page
 *
 * A release produces ten to fifteen thousand results. Fetching the failures and
 * filtering them in the browser for "passed" would show a subset of a subset
 * with no way to tell - the count would be right and the rows would not. Each
 * slice is a query, and the count beside each label comes from the run's own
 * tally rather than from the rows on screen.
 *
 * `Passed` is here because the passes are the DENOMINATOR. "40 workloads, all
 * compliant" and "the traversal never reached them" are the same empty list,
 * and only one of them is a reason to ship.
 *
 * # Why the severities are three slices and not two
 *
 * They were Blocking and Warnings, and Warnings quietly held the info results
 * as well - so a reader narrowing to warnings got a list padded with rows
 * nobody has to do anything about, and the info count was on no screen at all.
 * Critical, Warning and Info is the scale the severities already are.
 */
type ResultView = 'critical' | 'warning' | 'info' | 'passed' | 'unchecked' | 'all'

const VIEW_ORDER: ResultView[] = ['critical', 'warning', 'info', 'passed', 'unchecked', 'all']

const VIEWS: Record<ResultView, {
  label: string
  noun: string
  all?: boolean
  outcome?: string[]
  severity?: string[]
  count: (c?: ComplianceCounts) => number
  /**
   * How many DISTINCT checks this slice holds, when the server can say.
   *
   * Absent for Passed, Unchecked and All: a check is distinct per severity on
   * the wire, and there is no stored distinct count for an outcome. Those fall
   * back to counting the groups actually built, which is a count of the page -
   * and the view says so.
   */
  unique?: (c?: ComplianceCounts) => number
}> = {
  critical: {
    label: 'Critical',
    noun: 'critical findings',
    outcome: ['fail'],
    severity: ['block'],
    count: (c) => c?.blocking ?? 0,
    unique: (c) => c?.uniqueBlocking ?? 0,
  },
  warning: {
    label: 'Warning',
    noun: 'warnings',
    outcome: ['fail'],
    severity: ['warn'],
    count: (c) => c?.warning ?? 0,
    unique: (c) => c?.uniqueWarning ?? 0,
  },
  info: {
    label: 'Info',
    noun: 'informational findings',
    outcome: ['fail'],
    severity: ['info'],
    count: (c) => c?.info ?? 0,
    unique: (c) => c?.uniqueInfo ?? 0,
  },
  passed: {
    label: 'Passed',
    noun: 'passing checks',
    all: true,
    outcome: ['pass'],
    count: (c) => c?.pass ?? 0,
  },
  unchecked: {
    label: 'Unchecked',
    noun: 'checks not decided',
    outcome: ['error'],
    count: (c) => c?.error ?? 0,
  },
  all: {
    label: 'All',
    noun: 'results',
    all: true,
    // Every result the run recorded, which is the sum of the outcomes and not
    // a field: a "total" the server did not compute is a number that can
    // silently stop matching the rows beneath it.
    count: (c) => (c ? c.pass + c.fail + c.skip + c.error + c.waived : 0),
  },
}

/** Where a summary tile sends the reader. */
const VIEW_FOR_SUMMARY: Record<SummaryKey, ResultView> = {
  blocking: 'critical',
  warning: 'warning',
  info: 'info',
  error: 'unchecked',
  pass: 'passed',
}

/** Which tile is ringed for the slice on screen. The inverse of the map above. */
const SUMMARY_FOR_VIEW: Partial<Record<ResultView, SummaryKey>> = {
  critical: 'blocking',
  warning: 'warning',
  info: 'info',
  unchecked: 'error',
  passed: 'pass',
}

/** How the rows on screen are ordered. */
type ResultSort = 'severity' | 'chart' | 'check' | 'resource'

export function ComplianceTab({ product, reference, repository }: {
  product: string
  reference: string
  repository?: string
}) {
  const { message } = App.useApp()

  /*
   * WHAT IS ON SCREEN, and how much of it the server has to answer for.
   *
   * `view` is the outcome slice - the split a reader makes first and returns to
   * constantly - and it is a real server-side filter rather than a client one,
   * because a release produces ten to fifteen thousand results and filtering a
   * page of five hundred would show a subset of a subset without saying so.
   */
  const [tab, setTab] = useState<'findings' | 'charts'>('findings')
  const [view, setView] = useState<ResultView>('critical')
  const [chart, setChart] = useState<string | undefined>()
  const [determinacy, setDeterminacy] = useState<string | undefined>()
  const [kind, setKind] = useState<string | undefined>()
  const [sort, setSort] = useState<ResultSort>('severity')
  /*
   * TWO PIECES OF SEARCH STATE, and the second is not redundant.
   *
   * `draft` is what the box shows and `search` is what the server is asked. A
   * release produces ten to fifteen thousand results, so a query per keystroke
   * is a query per keystroke against all of them; the debounce below is what
   * makes typing feel like typing.
   */
  const [draft, setDraft] = useState('')
  const [search, setSearch] = useState('')
  const [detail, setDetail] = useState<ComplianceResult | null>(null)
  const [group, setGroup] = useState<CheckGroup | null>(null)
  const [manifest, setManifest] = useState<string | null>(null)
  /*
   * ONE ROW PER CHECK, or one per occurrence. Unique by default: 171 critical
   * findings on a real orb are eight rules, each broken in twenty places, and a
   * flat list of them reads as 171 problems.
   */
  const [grouping, setGrouping] = useState<'unique' | 'all'>('unique')

  // Debounced, so the server sees one query per pause rather than one per key.
  useEffect(() => {
    const t = setTimeout(() => setSearch(draft), 250)
    return () => clearTimeout(t)
  }, [draft])

  const slice = VIEWS[view]
  const compliance = usePackageCompliance(product, reference, {
    repository,
    all: slice.all,
    outcome: slice.outcome,
    severity: slice.severity,
    chart: chart ? [chart] : undefined,
    determinacy: determinacy ? [determinacy] : undefined,
    kind: kind ? [kind] : undefined,
    search: search.trim() || undefined,
    /*
     * ENOUGH ROWS TO GROUP, which the default page is not.
     *
     * The grouping is done in the browser, so a group saying "fires in 43
     * places" is only true if all 43 arrived. 2,000 covers Critical, Warning,
     * Info and Unchecked whole on the release this is built against; Passed and
     * All are still a page, and the view says so rather than quoting a count
     * assembled from one.
     */
    limit: 2000,
  })
  const run = useRunCompliance()
  const cancel = useCancelCompliance()
  // Analysing, offered from HERE. See the empty state below for why this tab
  // needs it at all, and why sending the reader to another tab to press it was
  // the wrong way to ask.
  const inspect = useInspectPackage(product, reference, repository)
  const data = compliance.data

  /*
   * What the run KEPT, which is not the same list as what it rendered: a
   * deployment can turn evidence off, and a large release can exhaust the
   * budget partway through. Read once here rather than inside the coverage
   * table, because the download it feeds is offered beside the view switch -
   * where the Security tab puts its export - and not only on the Charts tab.
   */
  const manifests = useRenderedManifests(product, reference, { repository })
  const manifestNames = useMemo(
    () => new Set((manifests.data?.documents ?? []).map((d) => d.document)),
    [manifests.data],
  )

  const start = () => {
    run.mutate({ product, ref: reference, repository }, {
      onError: (e) => message.error(String(e)),
    })
  }

  /*
   * THE FIRST LOAD IS A SKELETON, not an empty page.
   *
   * Only the first: every later fetch keeps the previous answer on screen (see
   * usePackageCompliance), so narrowing a filter no longer blanks the verdict
   * and the summary for a round trip. This branch is the one moment there is
   * genuinely nothing to hold.
   */
  if (compliance.isLoading && !data) {
    return (
      <Space direction="vertical" size={16} style={{ width: '100%' }}>
        <Card>
          <Skeleton active paragraph={{ rows: 3 }} />
        </Card>
        <Card>
          <Skeleton active paragraph={{ rows: 6 }} />
        </Card>
      </Space>
    )
  }

  /*
   * NOT ANALYSED - which is not the same as "no charts", and the difference is
   * the whole of this branch.
   *
   * The charts ARE listed on the Contents tab of an unanalysed release, and
   * that is not a contradiction: a release's index NAMES its children, so what
   * a chart is called and how many there are is known from discovery alone.
   * What is not known is where the chart's bytes live. A chart's content sits
   * in a LAYER of its manifest, and layer digests are recorded by walking the
   * tree - so a check has nothing to fetch until that walk has run, and would
   * report "no charts" over a release visibly full of them.
   *
   * Said before the button rather than after it: pressing one that cannot work
   * and reading a recorded failure is a worse way to learn this than being
   * told. And the walk is offered here, because "go to another tab and press a
   * different button" is a detour for something this tab can start itself. The
   * result arrives without being asked for - the release's own query notices
   * the walk ending and refreshes this one.
   */
  if (data && !data.analysed && !data.run && !data.progress) {
    const started = Boolean(inspect.data?.started) || inspect.isPending
    return (
      <Space direction="vertical" size={16} style={{ width: '100%' }}>
        <HelmMissingNotice helm={data.helm} />
        <EmptyStateCard
          title={
            started
              ? 'Analysing this release'
              : 'This release has to be analysed first'
          }
          explanation={
            started
              ? 'Reading the manifest tree from the vendor registry. The walk continues '
                + 'after this page is closed; the check is offered here once it completes.'
              : 'Its charts are already listed - the release index names them. Where each '
                + "chart's content lives is not: that is a layer inside the chart's own "
                + 'manifest, recorded only by walking the release. A check reads the charts '
                + 'themselves, so there is nothing to open until then.'
          }
          action={
            <Space direction="vertical" size={10}>
              {started ? (
                <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                  <LoadingOutlined spin style={{ color: c.brand, marginRight: 6 }} />
                  Walking the manifest tree. No artifact bytes are transferred.
                </Typography.Text>
              ) : (
                <Button
                  type="primary"
                  loading={inspect.isPending}
                  onClick={() => inspect.mutate()}
                >
                  Analyse this release
                </Button>
              )}
              {inspect.isError && (
                <Typography.Text type="danger" style={{ fontSize: 12 }}>
                  {inspect.error instanceof Error
                    ? inspect.error.message
                    : 'The registry did not answer.'}
                </Typography.Text>
              )}
              <Link to="/policies" style={{ fontSize: 12, color: c.brand }}>
                See what would be checked
              </Link>
            </Space>
          }
        />
      </Space>
    )
  }

  /*
   * A release with no result at all.
   *
   * The empty state OFFERS the action rather than describing the absence,
   * because the reader's next move is always the same one - and it shows the
   * press. `run.isPending` is the round trip that takes the claim; until it was
   * on this button the card sat unchanged for a second and the button appeared
   * not to have worked.
   */
  if (data && !data.run && !data.progress) {
    return (
      <Space direction="vertical" size={16} style={{ width: '100%' }}>
        <HelmMissingNotice helm={data.helm} />
        <EmptyStateCard
          title={run.isPending ? 'Starting the compliance check' : 'This release has not been checked'}
          explanation={
            run.isPending
              ? 'Claiming the release, so no second check can run against it at the same time. '
                + 'Progress appears here as soon as the claim is taken.'
              : 'Nothing here has been compared against your standards yet. That is not the same '
                + 'as passing them.'
          }
          action={
            <Space direction="vertical" size={10}>
              <Button type="primary" loading={run.isPending} onClick={start}>
                {run.isPending ? 'Starting the check' : 'Check this release'}
              </Button>
              <Link to="/policies" style={{ fontSize: 12, color: c.brand }}>
                See what would be checked
              </Link>
            </Space>
          }
        />
      </Space>
    )
  }

  const charts = data?.charts ?? []
  const results = data?.results ?? []
  const running = Boolean(data?.progress)
  // A run that rendered charts and kept none of them. Said once, at the top of
  // the coverage table: "no evidence" and "no findings" are different
  // statements, and a reader who expects the manifest deserves to have been
  // told before they go looking for it.
  const noneKept = manifests.isSuccess && manifestNames.size === 0
    && charts.some((ch) => ch.status === 'ok')

  /*
   * WHILE A RUN IS GOING, THE RUN IS THE WHOLE PAGE.
   *
   * The verdict card, the coverage table and the findings table all read from
   * the LATEST run - which, the moment somebody presses the button, is the one
   * that has just started and has nothing in it. So the tab showed "Not
   * checked" over four zeros and an empty findings table that redrew itself
   * every two seconds, for the several minutes the check took. Every one of
   * those is a true statement about a run that has not finished and a false
   * impression of the release.
   *
   * The previous run's verdict is not shown either, and that is the harder
   * call: it is real, and it is about to be replaced. Showing it beside a
   * running check is how a stale verdict gets read out in a release meeting.
   * It is one press of Re-check away from being current, and until then the
   * honest thing on screen is the check that is running.
   */
  if (running && data?.progress) {
    return (
      <Space direction="vertical" size={16} style={{ width: '100%' }}>
        {data && <HelmMissingNotice helm={data.helm} />}
        <ComplianceRunPanel
          progress={data.progress}
          cancelling={cancel.isPending}
          onCancel={() => cancel.mutate({ product, ref: reference, repository })}
        />
      </Space>
    )
  }

  const grouped = groupByCheck(results)
  /*
   * How many distinct checks this slice holds.
   *
   * The server's number where it has one, because the grouping runs over the
   * page and the page is not always the slice. Falling back to the groups built
   * is the honest second best, and the line under the controls says when that
   * is what is on screen.
   */
  const uniqueInSlice = slice.unique
    ? slice.unique(data?.run?.counts)
    : grouped.length
  const uniqueFindings = (data?.run?.counts.uniqueBlocking ?? 0)
    + (data?.run?.counts.uniqueWarning ?? 0)
    + (data?.run?.counts.uniqueInfo ?? 0)
  /*
   * Whether the page the server returned is the whole slice.
   *
   * The grouping is done in the browser over the rows that arrived, so a group
   * saying "fires in 43 places" is only true if all 43 arrived. The slices a
   * reader spends their time in - Critical, Warning, Info - are complete at the
   * limit this query asks for; Passed and All are not, and say so rather than
   * quoting a count assembled from a page.
   */
  const partial = (data?.total ?? 0) > results.length

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      {/*
        THE PROVENANCE ROW, above the cards and outside them.

        This is the Security tab's header, in the same place with the same
        shape: what produced the answer on the left as muted text, the controls
        that change it on the right as buttons. It used to be the verdict pill
        in a card title with the controls in that card's `extra`, which made the
        first thing on the tab a box rather than a fact, and put the buttons in
        a different place from the ones a reader had just used one tab over.
      */}
      <div
        style={{
          width: '100%', display: 'flex', flexWrap: 'wrap',
          alignItems: 'center', gap: 12,
        }}
      >
        {data?.run && <CheckedAgo run={data.run} />}
        <Space size={8} style={{ marginInlineStart: 'auto', marginTop: 8 }}>
          <Link to="/policies">
            <Button icon={<BookOutlined />}>Rulebook</Button>
          </Link>
          {data?.run && <ComplianceRunLogButton run={data.run} size="middle" />}
          <Tooltip
            title={
              'Renders every chart in this release with the pinned Kubernetes version and '
              + 'evaluates the rulebook against the objects. Identical chart bytes rendered '
              + 'before under identical inputs are reused.'
            }
          >
            <Button
              icon={<SyncOutlined spin={run.isPending} />}
              loading={run.isPending}
              disabled={running}
              onClick={start}
            >
              Re-check
            </Button>
          </Tooltip>
        </Space>
      </div>

      {data && <HelmMissingNotice helm={data.helm} />}
      {data?.run && <RunFailedNotice run={data.run} />}      {data?.run && <TruncatedNotice run={data.run} />}

      {/* The posture band: how bad, what of, and whether it can be trusted. */}
      {data?.run && (
        <ComplianceSummary
          counts={data.run.counts}
          charts={charts}
          verdict={data.run.verdict}
          verdictLabel={data.run.verdictLabel}
          selected={tab === 'findings' ? SUMMARY_FOR_VIEW[view] : undefined}
          grouped={grouping === 'unique'}
          onSelect={(what) => {
            setTab('findings')
            setView(VIEW_FOR_SUMMARY[what])
          }}
        />
      )}

      {/*
        FINDINGS FIRST, CHARTS BEHIND THEM.

        Two cards side by side asked the reader to decide which mattered on
        every visit. Findings are why anybody opens this tab; coverage is what
        they check when a number looks wrong - so coverage is one click away
        and never in front.

        The switch is a Segmented in the card BODY rather than antd Tabs in the
        card title, because that is what the Security tab's findings card uses
        and these two cards sit one tab apart. Same control, same place, same
        second row of filters under it.
      */}
      <Card size="small" styles={{ body: { paddingTop: 12 } }}>
        <div
          style={{
            display: 'flex', gap: 8, alignItems: 'center',
            justifyContent: 'space-between', marginBottom: 12, flexWrap: 'wrap',
          }}
        >
          <Segmented
            value={tab}
            onChange={(v) => setTab(v as 'findings' | 'charts')}
            options={[
              {
                value: 'findings',
                /*
                  WHAT THE TABLE WILL HOLD, which is why it follows the
                  grouping: distinct checks when the rows are grouped, every
                  occurrence when they are not. This is what the Security tab's
                  Vulnerabilities switch does, and a count that stayed on the
                  total while the table showed five rows would be the one number
                  on the card that disagreed with what is under it.

                  The release tab at the top of the page counts only the
                  critical findings, deliberately: that is the number a release
                  decision turns on.
                */
                label: `Findings (${formatCount(
                  grouping === 'unique'
                    ? uniqueFindings
                    : (data?.run?.counts.blocking ?? 0)
                      + (data?.run?.counts.warning ?? 0)
                      + (data?.run?.counts.info ?? 0),
                )})`,
              },
              {
                value: 'charts',
                label: brokenCharts(charts) > 0
                  ? `Charts (${charts.length - brokenCharts(charts)}/${charts.length})`
                  : `Charts (${charts.length})`,
              },
            ]}
          />
          <DownloadManifestsButton
            product={product}
            reference={reference}
            repository={repository}
            documents={manifests.data?.documents.length ?? 0}
            bytes={manifests.data?.totalBytes ?? 0}
          />
        </div>

        {tab === 'charts' ? (
          <ChartCoverage
            charts={charts}
            product={product}
            reference={reference}
            repository={repository}
            loading={compliance.isFetching}
            available={manifestNames}
            noneKept={noneKept}
            onOpenManifest={setManifest}
          />
        ) : (
          <>
            {/*
              The filters, in the order somebody narrows in and in the shape
              the Security tab uses: search first, then what am I looking at
              (one row per check, or every occurrence), then how bad, then the
              narrowing selects.
            */}
            <div
              style={{
                display: 'flex', flexWrap: 'wrap', gap: 8,
                alignItems: 'center', marginBottom: 12,
              }}
            >
              <Input
                allowClear
                style={{ width: 260 }}
                prefix={<SearchOutlined style={{ color: c.text3 }} />}
                placeholder="Check, resource, chart or file"
                value={draft}
                onChange={(e) => setDraft(e.target.value)}
              />

              {/*
                UNIQUE CHECKS OR EVERY OCCURRENCE, and unique is the default.

                171 critical findings on a real orb are eight rules, each broken
                in twenty places. The flat list is that fact spread over 171
                rows in which SEC-01 appears twenty-two times with a different
                chart name - which reads as 171 problems and is eight. This is
                the Security tab's Unique CVEs / All findings control, applied
                to the thing compliance repeats.
              */}
              <Segmented
                value={grouping}
                onChange={(v) => setGrouping(v as 'unique' | 'all')}
                options={[
                  { value: 'unique', label: `Unique findings (${formatCount(uniqueInSlice)})` },
                  { value: 'all', label: `All findings (${formatCount(data?.total ?? 0)})` },
                ]}
              />

              {/*
                THE SLICE COUNTS WHAT THE TABLE WILL HOLD.

                Grouped, "Critical (171)" sat over five rows. Every count on
                this card follows the grouping now, so nothing on screen can
                quote a number the rows under it do not add up to. Passed,
                Unchecked and All have no stored distinct count - a check is
                distinct per severity on the wire, not per outcome - so they
                keep their total either way.
              */}
              <Segmented
                value={view}
                onChange={(v) => setView(v as ResultView)}
                options={VIEW_ORDER.map((k) => {
                  const unique = VIEWS[k].unique
                  const n = grouping === 'unique' && unique
                    ? unique(data?.run?.counts)
                    : VIEWS[k].count(data?.run?.counts)
                  return { value: k, label: `${VIEWS[k].label} (${formatCount(n)})` }
                })}
              />

              <Select
                allowClear
                placeholder="Any chart"
                style={{ minWidth: 170 }}
                value={chart}
                onChange={setChart}
                showSearch
                optionFilterProp="label"
                options={charts
                  .filter((ch) => ch.name && ch.name !== '(not fetched)')
                  .map((ch) => ({ label: ch.name, value: ch.name }))}
              />
              <Select
                allowClear
                placeholder="Any kind"
                style={{ minWidth: 150 }}
                value={kind}
                onChange={setKind}
                showSearch
                optionFilterProp="label"
                options={kindOptions(results)}
              />
              {/* The split a reader makes first: whose problem is this. */}
              <Select
                allowClear
                placeholder="Anyone to fix"
                style={{ minWidth: 200 }}
                value={determinacy}
                onChange={setDeterminacy}
                options={[
                  { label: 'The vendor must fix it', value: 'fixed' },
                  { label: 'A values file can fix it', value: 'configurable' },
                  { label: 'Ownership not established', value: 'unknown' },
                ]}
              />
              {grouping === 'all' && (
                <Select
                  value={sort}
                  onChange={setSort}
                  style={{ minWidth: 170 }}
                  options={[
                    { label: 'Sort by severity', value: 'severity' },
                    { label: 'Sort by chart', value: 'chart' },
                    { label: 'Sort by check', value: 'check' },
                    { label: 'Sort by resource', value: 'resource' },
                  ]}
                />
              )}
            </div>

            {/*
              THE PAGE, when it is not the whole slice.

              The grouping is done here over the rows that arrived, so a group
              saying "fires in 43 places" is only true if all 43 did. Said
              rather than silently counting a page.
            */}
            {partial && grouping === 'unique' && (
              <Typography.Text
                type="secondary"
                style={{ fontSize: 12, display: 'block', marginBottom: 8 }}
              >
                Grouped from the first {formatCount(results.length)} of{' '}
                {formatCount(data?.total ?? 0)} {slice.noun}. Narrow the slice for exact counts.
              </Typography.Text>
            )}

            {grouping === 'unique'
              ? (
                <CheckGroupTable
                  groups={grouped}
                  loading={compliance.isFetching}
                  onOpenGroup={setGroup}
                  onOpenResult={setDetail}
                  emptyText={emptyTextFor(view, Boolean(search || chart || determinacy || kind))}
                />
              )
              : (
                <ResultsTable
                  results={sortResults(results, sort)}
                  loading={compliance.isFetching}
                  onOpen={setDetail}
                  emptyText={emptyTextFor(view, Boolean(search || chart || determinacy || kind))}
                />
              )}
          </>
        )}
      </Card>

      <CheckDrawer
        group={group}
        onClose={() => setGroup(null)}
        onOpenResult={(r) => { setGroup(null); setDetail(r) }}
      />

      <ResultDrawer
        result={detail}
        product={product}
        reference={reference}
        repository={repository}
        onClose={() => setDetail(null)}
        onOpenManifest={setManifest}
      />

      <ManifestDrawer
        document={manifest}
        product={product}
        reference={reference}
        repository={repository}
        onClose={() => setManifest(null)}
      />
    </Space>
  )
}

/** What a coverage row can be narrowed to. */
type ChartStatusFilter = 'all' | 'rendered' | 'failed' | 'skipped'

/**
 * What each chart contributed.
 *
 * Charts that did not render come first, because everything below them is a
 * smaller denominator than it looks.
 *
 * # Why this is no longer a card
 *
 * It was one, inside the card that holds the tab, behind a Show/Hide link -
 * two borders and a collapse around a table that is already the entire content
 * of the tab somebody chose. The tab IS the disclosure. What was on the card's
 * header - the rendered count, the download - belongs on the row of controls,
 * where the search and the filter are.
 */
function ChartCoverage({
  charts, product, reference, repository, loading, available, noneKept, onOpenManifest,
}: {
  charts: ComplianceChart[]
  product: string
  reference: string
  repository?: string
  loading?: boolean
  /** Which charts the run kept a manifest for. Read once by the caller, so the
   *  table and the release-wide download cannot disagree about it. */
  available: Set<string>
  noneKept: boolean
  /** Opens a chart's rendered manifest beside the table, without leaving it. */
  onOpenManifest: (document: string) => void
}) {
  const [draft, setDraft] = useState('')
  const [search, setSearch] = useState('')
  const [status, setStatus] = useState<ChartStatusFilter>('all')

  // Debounced like the findings box, so the two behave identically. Client-side
  // here - a release has tens of charts, not tens of thousands of results - but
  // a box that filtered on a different rhythm from the one on the tab beside it
  // would read as a different control.
  useEffect(() => {
    const t = setTimeout(() => setSearch(draft.trim().toLowerCase()), 200)
    return () => clearTimeout(t)
  }, [draft])

  /*
   * FAILURES FIRST. Everything below them is a smaller denominator than it
   * looks, and on a ninety-five chart release the thirteen that did not render
   * are otherwise thirteen rows somebody has to scroll past eighty-two others
   * to find. Within each group, by name, so a chart is where it was last time.
   */
  const rows = useMemo(() => {
    const matches = (ch: ComplianceChart) => {
      if (status === 'rendered' && ch.status !== 'ok') return false
      if (status === 'failed' && ch.status !== 'failed') return false
      if (status === 'skipped' && ch.status !== 'skipped') return false
      if (!search) return true
      // The error text and the key it names are searchable too: "which charts
      // want global.registry" is the question the coverage table exists to
      // answer, and it is one somebody types rather than scrolls for.
      return [ch.name, ch.version, ch.errorLabel, ch.errorValue, ch.errorFile, ch.error]
        .some((f) => f?.toLowerCase().includes(search))
    }
    return (charts ?? []).filter(matches).sort((a, b) => {
      const bad = (ch: ComplianceChart) => (ch.status === 'ok' ? 1 : 0)
      if (bad(a) !== bad(b)) return bad(a) - bad(b)
      return a.name.localeCompare(b.name)
    })
  }, [charts, search, status])

  const broken = (charts ?? []).filter((ch) => ch.status !== 'ok')

  return (
    <Space direction="vertical" size={12} style={{ width: '100%' }}>
      <Space size={10} wrap style={{ width: '100%' }}>
        <Input
          allowClear
          style={{ width: 280 }}
          prefix={<SearchOutlined style={{ color: c.text3 }} />}
          placeholder="Search chart, version, reason or value"
          value={draft}
          onChange={(e) => setDraft(e.target.value)}
        />
        <Select
          value={status}
          onChange={setStatus}
          style={{ minWidth: 170 }}
          options={[
            { label: 'Any outcome', value: 'all' },
            { label: 'Rendered', value: 'rendered' },
            { label: 'Failed to render', value: 'failed' },
            { label: 'Not fetched', value: 'skipped' },
          ]}
        />
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
          {(charts ?? []).length - broken.length} of {(charts ?? []).length} rendered
          {broken.length > 0 && (
            <span style={{ color: c.pending }}> · {broken.length} did not</span>
          )}
        </Typography.Text>
      </Space>

      {noneKept && <NoEvidenceNotice checked />}

      <DataTable<ComplianceChart>
        tableEnhancedKey="compliance-charts"
        size="small"
        loading={loading}
        rowKey={(ch) => `${ch.name}@${ch.version}@${ch.digest}`}
        dataSource={rows}
        pagination={rows.length > 50 ? { pageSize: 50, showSizeChanger: true, size: 'small' } : false}
        locale={{
          emptyText: (
            <Empty
              image={Empty.PRESENTED_IMAGE_SIMPLE}
              description={
                search || status !== 'all'
                  ? 'No charts match these filters.'
                  : 'This run recorded no charts.'
              }
            />
          ),
        }}
        columns={[
          {
            title: 'Chart', dataIndex: 'name', width: 280,
            sorter: (a: ComplianceChart, b: ComplianceChart) => a.name.localeCompare(b.name),
            render: (_: unknown, ch) => (
              <Space size={8} align="start">
                <HelmOutlined style={{ color: c.text2, fontSize: 14, marginTop: 1 }} />
                <Space direction="vertical" size={0}>
                  <span style={{ fontFamily: mono, fontSize: 12 }}>{ch.name}</span>
                  {ch.version && (
                    <span style={{ fontFamily: mono, fontSize: 11, color: c.text3 }}>
                      {ch.version}
                    </span>
                  )}
                </Space>
              </Space>
            ),
          },
          {
            title: 'Rendered', dataIndex: 'status', width: 120,
            sorter: (a: ComplianceChart, b: ComplianceChart) => a.status.localeCompare(b.status),
            render: (_: unknown, ch) => (
              <OutcomePill
                outcome={ch.status === 'ok' ? 'pass' : 'error'}
                label={ch.status === 'ok' ? 'Yes' : ch.status === 'skipped' ? 'Skipped' : 'Failed'}
              />
            ),
          },
          {
            title: 'Objects', dataIndex: 'resources', width: 90, align: 'right' as const,
            sorter: (a: ComplianceChart, b: ComplianceChart) => a.resources - b.resources,
            render: (n: number) => n.toLocaleString(),
          },
          {
            // WHY, in two words, before the paragraph. Seventeen charts that
            // failed four different ways are four conversations - three with
            // the vendor and one with us - and an undifferentiated column of
            // stack traces is how they become one complaint about the tool.
            title: 'Reason', dataIndex: 'errorLabel',
            render: (_: unknown, ch) => <ChartFailure chart={ch} />,
          },
          {
            // The manifest THIS chart rendered to. Offered per chart as well
            // as for the release, because a vendor engineer owns one chart
            // and does not want the other ninety-six.
            title: 'Manifest', dataIndex: 'name', width: 150,
            render: (_: unknown, ch) => (
              <ManifestLinks
                available={available.has(ch.name)}
                rendered={ch.status === 'ok'}
                document={ch.name}
                product={product}
                reference={reference}
                repository={repository}
                onOpen={onOpenManifest}
              />
            ),
          },
        ]}
      />
    </Space>
  )
}

/**
 * When this release was checked, and by what.
 *
 * The counterpart of the Security tab's `SyncedAgo`: muted text on the left of
 * the header row saying what produced the answer, so the reader meets the
 * provenance before the numbers. Rule 5 - a report that cannot say which
 * rulebook, which helm and which Kubernetes version produced it cannot be
 * re-derived, and re-deriving it is what happens when a vendor disputes a
 * finding.
 */
function CheckedAgo({ run }: { run: ComplianceRun }) {
  const bits: ReactNode[] = []
  if (run.bundleDigest) {
    bits.push(
      <span key="bundle">
        rulebook{' '}
        <code style={{ fontFamily: mono }}>
          {run.bundleDigest.replace(/^sha256:/, '').slice(0, 12)}
        </code>
      </span>,
    )
  }
  if (run.checks > 0) bits.push(<span key="checks">{run.checks} checks</span>)
  if (run.helmVersion) bits.push(<span key="helm">helm {run.helmVersion}</span>)
  if (run.kubeVersion) bits.push(<span key="kube">Kubernetes {run.kubeVersion}</span>)

  return (
    <Tooltip title={run.finishedAt ? new Date(run.finishedAt).toLocaleString() : undefined}>
      <Typography.Text type="secondary" style={{ fontSize: 12 }}>
        {run.finishedAt ? `Checked ${formatRelative(run.finishedAt)}` : 'Never checked'}
        {bits.map((b, i) => (
          <span key={i}> · {b}</span>
        ))}
      </Typography.Text>
    </Tooltip>
  )
}

/**
 * One rule, and every place it was broken.
 *
 * The compliance answer to the Security tab's CVE group, and it exists for the
 * same reason: a release repeats the same finding across everything it ships.
 * 171 critical findings on a real orb are eight rules - "containers do not run
 * as root", "every image is pinned by digest" - each broken in twenty places,
 * and the flat list is that fact spread over 171 rows in which SEC-01 appears
 * twenty-two times with a different chart name. It reads as 171 problems and
 * it is eight pieces of work.
 */
type CheckGroup = {
  key: string
  check: string
  title?: string
  severity: string
  /** The worst outcome any occurrence reached. */
  outcome: ComplianceResult['outcome']
  outcomeLabel: string
  category?: string
  remediation?: string
  reference?: string
  message?: string
  /** Every determinacy the occurrences carried, so the roll-up can say "mixed". */
  determinacies: string[]
  charts: string[]
  kinds: string[]
  rows: ComplianceResult[]
}

/**
 * Collapses results onto the check.
 *
 * The severity is the check's own, so there is nothing to roll up; what is
 * rolled up is the DETERMINACY, and it is kept as a list rather than reduced to
 * one value. A rule the vendor must fix in three charts and a values file can
 * fix in two is both, and picking either would send half the work to the wrong
 * person - which is the distinction this whole model exists to keep.
 */
function groupByCheck(results: ComplianceResult[]): CheckGroup[] {
  const byKey = new Map<string, CheckGroup>()

  for (const r of results) {
    let g = byKey.get(r.check)
    if (!g) {
      g = {
        key: r.check,
        check: r.check,
        title: r.title,
        severity: r.severity,
        outcome: r.outcome,
        outcomeLabel: r.outcomeLabel,
        category: r.category,
        remediation: r.remediation,
        reference: r.reference,
        message: r.message || r.error,
        determinacies: [],
        charts: [],
        kinds: [],
        rows: [],
      }
      byKey.set(r.check, g)
    }
    if (!g.title && r.title) g.title = r.title
    if (!g.remediation && r.remediation) g.remediation = r.remediation
    if (!g.reference && r.reference) g.reference = r.reference
    if (!g.message && (r.message || r.error)) g.message = r.message || r.error
    // A failure outranks a pass: a rule broken anywhere is a rule broken.
    if (outcomeRank(r.outcome) < outcomeRank(g.outcome)) {
      g.outcome = r.outcome
      g.outcomeLabel = r.outcomeLabel
    }
    if (r.determinacy && r.determinacy !== 'na' && !g.determinacies.includes(r.determinacy)) {
      g.determinacies.push(r.determinacy)
    }
    if (r.chart && !g.charts.includes(r.chart)) g.charts.push(r.chart)
    if (r.kind && !g.kinds.includes(r.kind)) g.kinds.push(r.kind)
    g.rows.push(r)
  }

  return [...byKey.values()].sort((a, b) => (
    severityRank(a.severity) !== severityRank(b.severity)
      ? severityRank(a.severity) - severityRank(b.severity)
      : b.rows.length - a.rows.length
  ))
}

/**
 * Worst first, so a group takes the outcome that decides what to do about it.
 *
 * An unrecognised value sorts last rather than first: a value this build has
 * never seen must not be able to promote itself above a failure.
 */
function outcomeRank(outcome: string): number {
  return { fail: 0, error: 1, waived: 2, skip: 3, pass: 4 }[outcome] ?? 5
}

function severityRank(severity: string): number {
  return { block: 0, warn: 1, info: 2 }[severity] ?? 3
}

/**
 * One row per check, expandable into where it was broken.
 *
 * # Why the expansion is a table and not a list of chart names
 *
 * Because the question underneath "SEC-01 is broken in twenty-two places" is
 * "which resource, in which chart, on which container, and what was found" -
 * four columns. A comma-separated list of chart names answers a quarter of it
 * and sends the reader back to the flat view for the rest. This is the shape
 * the Security tab's unique-CVE table uses, and for the same reason.
 */
function CheckGroupTable({ groups, loading, onOpenGroup, onOpenResult, emptyText }: {
  groups: CheckGroup[]
  loading?: boolean
  onOpenGroup: (g: CheckGroup) => void
  onOpenResult: (r: ComplianceResult) => void
  emptyText: string
}) {
  return (
    <Table<CheckGroup>
      size="small"
      loading={loading}
      rowKey={(g) => g.key}
      dataSource={groups}
      scroll={{ x: 'max-content' }}
      pagination={groups.length > 25 ? { pageSize: 25, showSizeChanger: true, size: 'small' } : false}
      locale={{
        emptyText: <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={emptyText} />,
      }}
      expandable={{
        expandedRowRender: (g) => (
          <Table<ComplianceResult>
            size="small"
            rowKey={(r) => `${r.seq}`}
            dataSource={g.rows}
            pagination={g.rows.length > 10 ? { pageSize: 10, size: 'small', showSizeChanger: false } : false}
            onRow={(r) => ({ onClick: () => onOpenResult(r), style: { cursor: 'pointer' } })}
            /*
              FOUR NARROW FACTS, and not the message.

              Every row of a group is the same check, so every row's message is
              the same sentence with a different chart name in it - which is the
              thing already in the first column. Printing it made each row four
              lines tall and told a reader nothing they could not read from the
              columns beside it. What differs between occurrences is where it
              fired and what was there, so those are the columns.
            */
            scroll={{ x: 'max-content' }}
            columns={occurrenceColumns(onOpenResult)}
          />
        ),
        rowExpandable: (g) => g.rows.length > 0,
      }}
      columns={[
        {
          title: 'Check', width: 240,
          render: (_: unknown, g: CheckGroup) => (
            <Space direction="vertical" size={2}>
              <Space size={6}>
                <Typography.Link
                  onClick={() => onOpenGroup(g)}
                  style={{ fontFamily: mono, fontSize: 12 }}
                >
                  {g.check}
                </Typography.Link>
                <CheckSeverityTag severity={g.severity} />
              </Space>
              {g.title && <span style={{ fontSize: 12, color: c.text2 }}>{g.title}</span>}
            </Space>
          ),
        },
        {
          title: '', dataIndex: 'outcome', width: 120,
          render: (_: unknown, g: CheckGroup) => (
            <OutcomePill outcome={g.outcome} label={g.outcomeLabel} />
          ),
        },
        {
          title: 'Affects', width: 210,
          sorter: (a: CheckGroup, b: CheckGroup) => a.rows.length - b.rows.length,
          render: (_: unknown, g: CheckGroup) => (
            <Space direction="vertical" size={0}>
              <Typography.Text style={{ fontSize: 12 }}>
                <strong>{g.rows.length.toLocaleString()}</strong>
                {g.rows.length === 1 ? ' place' : ' places'}
                {' in '}
                <strong>{g.charts.length.toLocaleString()}</strong>
                {g.charts.length === 1 ? ' chart' : ' charts'}
              </Typography.Text>
              <Typography.Text
                type="secondary"
                style={{ fontSize: 11, fontFamily: mono, maxWidth: 200 }}
                ellipsis={{ tooltip: g.charts.join(', ') }}
              >
                {g.charts.slice(0, 2).join(', ')}
                {g.charts.length > 2 && ` +${g.charts.length - 2}`}
              </Typography.Text>
            </Space>
          ),
        },
        {
          title: 'Kinds', width: 150,
          render: (_: unknown, g: CheckGroup) => (
            <Typography.Text
              type="secondary"
              style={{ fontSize: 11, maxWidth: 140 }}
              ellipsis={{ tooltip: g.kinds.join(', ') }}
            >
              {g.kinds.slice(0, 2).join(', ') || '—'}
              {g.kinds.length > 2 && ` +${g.kinds.length - 2}`}
            </Typography.Text>
          ),
        },
        {
          // WHOSE PROBLEM, rolled up but never flattened. A rule the vendor
          // must fix in three charts and a values file can fix in two is both,
          // and picking either sends half the work to the wrong person.
          title: 'Owner',
          render: (_: unknown, g: CheckGroup) => (
            <Space size={4} wrap>
              {g.determinacies.length === 0
                ? <Typography.Text type="secondary" style={{ fontSize: 11 }}>—</Typography.Text>
                : g.determinacies.map((d) => (
                  <DeterminacyTag
                    key={d}
                    determinacy={d as ComplianceResult['determinacy']}
                    label={DETERMINACY_WORD[d] ?? d}
                  />
                ))}
            </Space>
          ),
        },
      ]}
    />
  )
}

/**
 * Where one occurrence of a check fired, and what was there.
 *
 * Shared by the expanded row and the check drawer, because they are the same
 * list read in two places and a column that differed between them would be a
 * reader learning the table twice.
 */
function occurrenceColumns(onOpen: (r: ComplianceResult) => void) {
  return [
    {
      /*
        NARROW, because a chart name is short. `cfx-adrf-chart2` is fifteen
        characters and the column was 210px of mostly nothing, taken from the
        two columns beside it that hold the part which differs between rows.
      */
      title: 'Chart', dataIndex: 'chart', width: 190,
      render: (_: unknown, r: ComplianceResult) => (
        <Space size={6} align="start">
          <HelmOutlined style={{ color: c.text3, marginTop: 2 }} />
          <Space direction="vertical" size={0}>
            <Typography.Text
              style={{ fontFamily: mono, fontSize: 12, maxWidth: 158 }}
              ellipsis={{ tooltip: r.chart }}
            >
              {r.chart}
            </Typography.Text>
            {r.sourceFile && (
              <Typography.Text
                type="secondary"
                style={{ fontFamily: mono, fontSize: 11, maxWidth: 158 }}
                ellipsis={{ tooltip: r.sourceFile }}
              >
                {templatePath(r)}
              </Typography.Text>
            )}
          </Space>
        </Space>
      ),
    },
    {
      /*
        THE OBJECT AND THE FIELD IN ONE CELL, and that is not crowding.

        The field was its own column, and within a group it is the same string
        on every row - SEC-01 is always `securityContext.runAsNonRoot` - so a
        column of it was 300px repeating one fact, and it pushed the one column
        that DOES differ off the right edge of the drawer. It belongs under the
        object it is a field of.
      */
      title: 'Resource', dataIndex: 'name',
      render: (_: unknown, r: ComplianceResult) => (
        <Space direction="vertical" size={0}>
          <span style={{ fontSize: 12 }}>
            {r.kind} {r.namespace ? `${r.namespace}/` : ''}{r.name}
            {r.container && <span style={{ color: c.text2 }}> · container {r.container}</span>}
          </span>
          {r.locus && (
            <Typography.Text
              type="secondary"
              style={{ fontFamily: mono, fontSize: 11, maxWidth: 240 }}
              ellipsis={{ tooltip: r.locus }}
            >
              {r.locus}
            </Typography.Text>
          )}
        </Space>
      ),
    },
    {
      title: 'Found', dataIndex: 'observed', width: 150,
      render: (_: unknown, r: ComplianceResult) => (
        <Space direction="vertical" size={2}>
          <span style={{ fontFamily: mono, fontSize: 11 }}>{r.observed || '(absent)'}</span>
          <DeterminacyTag determinacy={r.determinacy} label={r.determinacyLabel} />
        </Space>
      ),
    },
    {
      /*
        THE ACTION, PINNED TO THE RIGHT EDGE.

        The row itself opens the finding, and a row that is clickable without
        saying so is a row most people never click. `fixed: 'right'` keeps the
        control on screen when the table scrolls sideways, which the resource
        column makes it do on a narrow drawer - an action that scrolls out of
        view is an action that does not exist.
      */
      title: '', dataIndex: 'seq', width: 82, fixed: 'right' as const,
      render: (_: unknown, r: ComplianceResult) => (
        <Button
          size="small"
          type="text"
          icon={<FileTextOutlined />}
          onClick={(e) => { e.stopPropagation(); onOpen(r) }}
        >
          View
        </Button>
      ),
    },
  ]
}

/**
 * The template, relative to its chart.
 *
 * helm names it `cfx-adrf-chart2/templates/deployment.yaml`, and the chart's
 * name is on the line directly above - so the prefix wrapped the cell onto two
 * lines to repeat what the reader had just read. `templates/deployment.yaml`
 * is the part that differs between rows.
 */
function templatePath(r: ComplianceResult): string {
  const file = r.sourceFile ?? ''
  if (r.chart && file.startsWith(`${r.chart}/`)) return file.slice(r.chart.length + 1)
  return file
}

/** The determinacy in the two words the roll-up has room for. */
const DETERMINACY_WORD: Record<string, string> = {
  fixed: 'Vendor',
  configurable: 'Values file',
  unknown: 'Not established',
}

/**
 * One rule, in full, with every place it was broken.
 *
 * The counterpart of the Security tab's advisory panel: the rule itself, why
 * the organization requires it, what to do about it, and then the occurrences -
 * because the question underneath a grouped row is "where else is this".
 * Clicking an occurrence hands off to the single-finding drawer, which is where
 * the rendered manifest is.
 */
function CheckDrawer({ group, onClose, onOpenResult }: {
  group: CheckGroup | null
  onClose: () => void
  onOpenResult: (r: ComplianceResult) => void
}) {
  return (
    <Drawer
      open={Boolean(group)}
      onClose={onClose}
      width={820}
      title={
        group && (
          <Space size={10} wrap>
            <span style={{ fontFamily: mono }}>{group.check}</span>
            <CheckSeverityTag severity={group.severity} />
            <OutcomePill outcome={group.outcome} label={group.outcomeLabel} />
          </Space>
        )
      }
    >
      {group && (
        <Space direction="vertical" size={16} style={{ width: '100%' }}>
          <Typography.Title level={5} style={{ margin: 0 }}>{group.title}</Typography.Title>

          {/*
            THE RULE'S OWN FACTS, and deliberately not a finding's message.

            The message on a result names one chart - "Deployment
            cfx-adrf-chart2 container main: …" - and putting the first
            occurrence's message here read as a description of the rule while
            naming one of its forty-four occurrences. The rule is the title, the
            category and the remediation; where it fired is the table below.
          */}
          {group.category && <Tag style={{ margin: 0, fontSize: 11 }}>{group.category}</Tag>}

          {group.remediation && (
            <Card size="small" title="Remediation">
              <Typography.Paragraph style={{ marginBottom: 0 }}>
                {group.remediation}
              </Typography.Paragraph>
            </Card>
          )}

          <div>
            <Typography.Text type="secondary" style={{ fontSize: 11 }}>
              Broken in {group.rows.length.toLocaleString()}
              {group.rows.length === 1 ? ' place' : ' places'} across{' '}
              {group.charts.length.toLocaleString()}
              {group.charts.length === 1 ? ' chart' : ' charts'}
            </Typography.Text>
            <div style={{ marginTop: 8 }}>
              <Table<ComplianceResult>
                size="small"
                rowKey={(r) => `${r.seq}`}
                dataSource={group.rows}
                pagination={group.rows.length > 12
                  ? { pageSize: 12, size: 'small', showSizeChanger: false }
                  : false}
                onRow={(r) => ({ onClick: () => onOpenResult(r), style: { cursor: 'pointer' } })}
                scroll={{ x: 'max-content' }}
            columns={occurrenceColumns(onOpenResult)}
              />
            </div>
          </div>

          {group.reference && (
            <Typography.Link href={group.reference} target="_blank" rel="noreferrer">
              Source standard
            </Typography.Link>
          )}
        </Space>
      )}
    </Drawer>
  )
}

function ResultsTable({ results, loading, onOpen, emptyText }: {
  results: ComplianceResult[]
  loading?: boolean
  onOpen: (r: ComplianceResult) => void
  emptyText: string
}) {
  const columns = useMemo(() => [
    {
      title: '', dataIndex: 'outcome', width: 110,
      render: (_: unknown, r: ComplianceResult) => (
        <OutcomePill outcome={r.outcome} label={r.outcomeLabel} />
      ),
    },
    {
      title: 'Check', dataIndex: 'check', width: 210,
      render: (_: unknown, r: ComplianceResult) => (
        <Space direction="vertical" size={2}>
          <Space size={6}>
            <Typography.Link onClick={() => onOpen(r)} style={{ fontFamily: mono, fontSize: 12 }}>
              {r.check}
            </Typography.Link>
            <CheckSeverityTag severity={r.severity} />
          </Space>
          {r.title && (
            <span style={{ fontSize: 12, color: c.text2 }}>{r.title}</span>
          )}
        </Space>
      ),
    },
    {
      title: 'Where', dataIndex: 'name', width: 320,
      render: (_: unknown, r: ComplianceResult) => <ResultAddress result={r} />,
    },
    {
      title: 'Finding', dataIndex: 'message',
      render: (_: unknown, r: ComplianceResult) => (
        <Space direction="vertical" size={4}>
          <span style={{ fontSize: 13 }}>{r.message || r.error}</span>
          <Space size={6} wrap>
            <DeterminacyTag determinacy={r.determinacy} label={r.determinacyLabel} />
            {r.observed && (
              <span style={{ fontSize: 11, color: c.text3, fontFamily: mono }}>
                found {r.observed}
              </span>
            )}
          </Space>
        </Space>
      ),
    },
  ], [onOpen])

  return (
    <DataTable<ComplianceResult>
      tableEnhancedKey="compliance-results"
      size="small"
      loading={loading}
      rowKey={(r) => `${r.check}|${r.chart}|${r.sourceFile}|${r.kind}|${r.name}|${r.container}|${r.locus}`}
      dataSource={results}
      columns={columns}
      locale={{
        emptyText: <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description={emptyText} />,
      }}
      pagination={{ pageSize: 50, showSizeChanger: true, size: 'small' }}
      onRow={(r) => ({ onClick: () => onOpen(r), style: { cursor: 'pointer' } })}
    />
  )
}

/**
 * One finding, in full.
 *
 * Everything a vendor needs is here and nothing needs this screen open: the
 * rule, why the organization requires it, exactly where it fired, what was
 * found, what was expected, and what to do. A drawer rather than a modal
 * because the reader is working down a list.
 */
function ResultDrawer({ result, product, reference, repository, onClose, onOpenManifest }: {
  result: ComplianceResult | null
  product: string
  reference: string
  repository?: string
  onClose: () => void
  /** Opens the whole rendered manifest, on this page. */
  onOpenManifest: (document: string) => void
}) {
  return (
    <Drawer
      open={Boolean(result)}
      onClose={onClose}
      width={640}
      title={
        result && (
          <Space size={10} wrap>
            <span style={{ fontFamily: mono }}>{result.check}</span>
            <CheckSeverityTag severity={result.severity} />
            <OutcomePill outcome={result.outcome} label={result.outcomeLabel} />
          </Space>
        )
      }
    >
      {result && (
        <Space direction="vertical" size={16} style={{ width: '100%' }}>
          <Typography.Title level={5} style={{ margin: 0 }}>{result.title}</Typography.Title>

          {(result.message || result.error) && (
            <Typography.Paragraph style={{ marginBottom: 0 }}>
              {result.message || result.error}
            </Typography.Paragraph>
          )}

          {/*
            THE MANIFEST, ahead of the address table. "Show me" is the next
            question after "what is wrong"; the address is what somebody reads
            once they already believe it, and burying the evidence under it
            makes verifying a finding a thing you have to scroll for.
          */}
          <EvidencePanel
            product={product}
            reference={reference}
            repository={repository}
            result={result}
            onOpenManifest={onOpenManifest}
          />

          <Descriptions column={1} size="small" bordered>
            <Descriptions.Item label="Chart">
              <Space size={6}>
                <HelmOutlined style={{ color: c.text3 }} />
                <span style={{ fontFamily: mono }}>
                  {result.chart}
                  {result.chartVersion && `:${result.chartVersion}`}
                </span>
              </Space>
            </Descriptions.Item>
            {result.sourceFile && (
              <Descriptions.Item label="Template">
                <span style={{ fontFamily: mono }}>{result.sourceFile}</span>
              </Descriptions.Item>
            )}
            {result.kind && (
              <Descriptions.Item label="Resource">
                {/*
                  The namespace and its separator together, or neither: a
                  cluster-scoped object rendered as "Deployment /name" reads
                  like a path with a missing segment.
                */}
                <span style={{ fontFamily: mono }}>
                  {result.apiVersion} {result.kind}{' '}
                  {result.namespace ? `${result.namespace}/` : ''}{result.name}
                </span>
              </Descriptions.Item>
            )}
            {result.container && (
              <Descriptions.Item label="Container">
                <span style={{ fontFamily: mono }}>
                  {result.container}
                  {result.containerType && ` (${result.containerType})`}
                </span>
              </Descriptions.Item>
            )}
            {result.locus && (
              <Descriptions.Item label="Field">
                <span style={{ fontFamily: mono }}>{result.locus}</span>
              </Descriptions.Item>
            )}
            {result.observed && (
              <Descriptions.Item label="Observed">
                <span style={{ fontFamily: mono }}>{result.observed}</span>
              </Descriptions.Item>
            )}
            {result.expected && (
              <Descriptions.Item label="Expected">
                <span style={{ fontFamily: mono }}>{result.expected}</span>
              </Descriptions.Item>
            )}
            {result.determinacy && result.determinacy !== 'na' && (
              <Descriptions.Item label="Owner">
                <DeterminacyTag determinacy={result.determinacy} label={result.determinacyLabel} />
              </Descriptions.Item>
            )}
            {result.artifactRef && (
              <Descriptions.Item label="Artifact">
                <span style={{ fontFamily: mono, fontSize: 12 }}>{result.artifactRef}</span>
              </Descriptions.Item>
            )}
          </Descriptions>

          {result.remediation && (
            <Card size="small" title="Remediation">
              <Typography.Paragraph style={{ marginBottom: 0 }}>
                {result.remediation}
              </Typography.Paragraph>
            </Card>
          )}

          {result.reference && (
            <Typography.Link href={result.reference} target="_blank" rel="noreferrer">
              Source standard
            </Typography.Link>
          )}
        </Space>
      )}
    </Drawer>
  )
}


/** Charts that produced no manifests, which is the run's real denominator. */
function brokenCharts(charts: ComplianceChart[]): number {
  return charts.filter((ch) => ch.status !== 'ok').length
}

/**
 * The kinds present, for the narrowing select.
 *
 * Derived from the rows on screen rather than from a fixed list, because a
 * release ships custom resources for operators this platform has never heard
 * of, and a hard-coded list of Kubernetes kinds would hide exactly those.
 */
function kindOptions(results: ComplianceResult[]): { label: string; value: string }[] {
  const seen = new Set<string>()
  for (const r of results) {
    if (r.kind) seen.add(r.kind)
  }
  return [...seen].sort().map((k) => ({ label: k, value: k }))
}

/**
 * The rows, ordered as the reader asked.
 *
 * Client-side, and that is a deliberate limit: it orders the PAGE the server
 * returned, which is the reading order the engine assigned - failures first,
 * then the unchecked, then the passes - narrowed to one outcome slice. Sorting
 * across a fifteen-thousand-row run would need a server-side order, and the
 * order that matters most is already the default.
 */
function sortResults(results: ComplianceResult[], sort: ResultSort): ComplianceResult[] {
  if (sort === 'severity') return results
  const key = (r: ComplianceResult): string => {
    switch (sort) {
      case 'chart': return `${r.chart ?? ''}\u0000${r.sourceFile ?? ''}\u0000${r.check}`
      case 'check': return `${r.check}\u0000${r.chart ?? ''}\u0000${r.name ?? ''}`
      default: return `${r.kind ?? ''}\u0000${r.name ?? ''}\u0000${r.check}`
    }
  }
  return [...results].sort((a, b) => key(a).localeCompare(key(b)))
}

/**
 * What an empty table means, which is never the same thing twice.
 *
 * "No results" over a filtered view is a statement about the filter; over an
 * unfiltered Critical view it is the whole point of the release. Saying which
 * is the difference between a reader clearing a filter and a reader shipping.
 */
function emptyTextFor(view: ResultView, filtered: boolean): string {
  if (filtered) return 'No results match these filters.'
  switch (view) {
    case 'critical':
      return 'No critical findings. Check the unchecked count above before reading that as a pass.'
    case 'warning':
      return 'No warnings.'
    case 'info':
      return 'No informational findings.'
    case 'unchecked':
      return 'Every check was decided. Nothing was skipped for want of a rendered chart.'
    case 'passed':
      return 'Nothing passed, which means nothing was evaluated.'
    default:
      return 'This run produced no results.'
  }
}

/**
 * Why one chart did not render, and whether a retry could have helped.
 *
 * # Why the classification is on screen and not only the message
 *
 * A real orb produced seventeen failures across four completely different
 * causes: subcharts requiring a `global.registry` that only an umbrella
 * supplies, a values.schema.json the vendor's own defaults violate, a template
 * dereferencing a nil, and a file that is not valid YAML. Three of those are
 * conversations with the vendor and one is a conversation with us, and a column
 * of undifferentiated helm stack traces is how all four become "the tool is
 * broken".
 *
 * # Why the missing key is its own chip
 *
 * Six of the eight charts that failed in that orb failed for one reason, and
 * their eight paragraphs of helm had nothing in common but the key. `global.
 * registry` on the row is what turns "these charts are broken" into "these
 * charts are subcharts, and a values file supplying one key would check them".
 *
 * # Why a test hook is called out
 *
 * `helm install` never applies `templates/tests/`. A chart failing only there
 * installs perfectly in a cluster, and telling a vendor "your chart does not
 * render" about a test job is how a true finding gets dismissed with the rest
 * of the report. It is stated rather than worked around: `--skip-tests` filters
 * test manifests out of the output AFTER the templates execute, so a `fail`
 * inside one still aborts the render. Measured, not assumed.
 *
 * The attempt count is here for the same reason. "Retried and failed again" and
 * "not retried, because a second render of the same bytes returns the same
 * error" are different facts, and a reader who is not told which will assume
 * the wrong one.
 */
function ChartFailure({ chart }: { chart: ComplianceChart }) {
  if (!chart.error && !chart.errorLabel) return null

  return (
    <Space direction="vertical" size={4} style={{ width: '100%' }}>
      <Space size={6} wrap>
        {chart.errorLabel && (
          <Tooltip title={chart.errorHint}>
            <Tag
              color={chart.errorKind === 'needs_values' ? 'orange' : 'red'}
              style={{ margin: 0, fontSize: 11 }}
            >
              {chart.errorLabel}
            </Tag>
          </Tooltip>
        )}
        {chart.errorValue && (
          <Tooltip
            title={
              `This chart cannot render until ${chart.errorValue} is supplied. A tier-1 check `
              + 'renders what the vendor shipped and nothing else, so a values file is not '
              + 'invented for it - the chart is reported as unchecked rather than judged '
              + 'against a value this platform made up.'
            }
          >
            <Tag color="gold" style={{ margin: 0, fontFamily: mono, fontSize: 11 }}>
              requires {chart.errorValue}
            </Tag>
          </Tooltip>
        )}
        {chart.errorInTest && (
          <Tooltip
            title={
              'The failure is in a helm test hook, which `helm install` never applies. This '
              + 'chart may install correctly and still not be checkable here. `helm template '
              + '--skip-tests` does not help: it filters test manifests out of the output '
              + 'after every template has run.'
            }
          >
            <Tag color="blue" style={{ margin: 0, fontSize: 11 }}>
              Helm test hook
            </Tag>
          </Tooltip>
        )}
        {/*
          A CHIP, like the two beside it. As plain grey text it wrapped onto a
          line of its own between the classification and the cause, where it
          read as a sentence fragment about the row rather than as one more
          fact in the row's chip set.
        */}
        {(chart.attempts ?? 0) > 1 && (
          <Tag style={{ margin: 0, fontSize: 11 }}>{chart.attempts} attempts</Tag>
        )}
        {(chart.attempts ?? 0) <= 1 && !chart.retryable && (
          <Tooltip
            title={
              'Not retried: helm template is a pure function of the chart and the pinned '
              + 'inputs, so a second render of the same bytes returns the same error.'
            }
          >
            <Tag style={{ margin: 0, fontSize: 11 }}>Not retried</Tag>
          </Tooltip>
        )}
      </Space>
      {/*
        THE CAUSE, then where. helm's message is a paragraph that names the
        chart, the word "Error", the file, the line, the column and then - last -
        what actually went wrong, and the row printed all of it under a file path
        it had already printed. Six lines per row over thirteen failed charts is
        a table nobody reads to the bottom of.

        The whole message is one hover away, because the frames matter when a
        vendor is opening the template.
      */}
      {chart.error && (
        <Tooltip title={<span style={{ whiteSpace: 'pre-wrap' }}>{chart.error}</span>}>
          <Typography.Text style={{ fontSize: 12 }}>
            {helmCause(chart.error)}
          </Typography.Text>
        </Tooltip>
      )}
      {chart.errorFile && (
        <Typography.Text style={{ fontFamily: mono, fontSize: 11, color: c.text3 }}>
          {chart.errorFile}
        </Typography.Text>
      )}
    </Space>
  )
}

/**
 * What actually went wrong, out of helm's paragraph.
 *
 * A real message reads:
 *
 *   helm template failed for cfx-adrf-chart: Error: execution error at
 *   (cfx-adrf-chart/templates/chart-check.yaml:2:4): global.registry must be
 *   specified
 *
 * Six words of that are the finding. The rest is the chart's name, which is in
 * the first column; the file and line, which are on the row beneath; and the
 * word "Error", which the red tag beside it already said. Stripping them is
 * what makes a coverage table of thirteen failures readable on one screen.
 *
 * Every removal is a prefix helm is known to emit, matched from the front, and
 * anything unrecognised is returned whole - so a message this has never seen
 * loses nothing.
 */
function helmCause(message: string): string {
  const head = (message.split('\n')[0] ?? message).trim()
  let s = head
  const prefixes: RegExp[] = [
    /^helm template failed for [^:]+:\s*/i,
    /^rendering [^:]+:\s*/i,
    /^Error:\s*/i,
    /^execution error at \([^)]*\)\s*/i,
    /^parse error at \([^)]*\)\s*/i,
    /^template: [^:]+:\d+:\d+:\s*/i,
    /^executing "[^"]*"\s*/i,
    /^at <[^>]*>:\s*/i,
    /^:\s*/,
  ]
  // Repeatedly, because helm nests them: a template error carries an execution
  // error which carries the frame which carries the cause.
  for (let pass = 0; pass < prefixes.length * 2; pass++) {
    const before = s
    for (const re of prefixes) s = s.replace(re, '')
    s = s.trim()
    if (s === before) break
  }
  return s || head
}
