import { useEffect, useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import {
  App, Button, Card, Descriptions, Drawer, Empty, Input, Segmented, Select, Skeleton,
  Space, Tabs, Tag, Tooltip, Typography,
} from 'antd'
// The working-surface table: resizable, reorderable, pinnable columns whose
// layout each person keeps. See `tablekit/README.md` for which tables get it.
import { Table as DataTable } from '../tablekit'
import { HelmOutlined, LoadingOutlined, SearchOutlined } from '../icons'
import {
  useCancelCompliance, useInspectPackage, usePackageCompliance,
  useRenderedManifests, useRunCompliance,
} from '../api/queries'
import { EmptyStateCard } from './layout'
import {
  CheckSeverityTag, ComplianceSummary, DeterminacyTag,
  HelmMissingNotice, OutcomePill, ResultAddress, RunFailedNotice,
  RunProvenance, TruncatedNotice, VerdictPill,
} from './compliance'
import type { SummaryKey } from './compliance'
import { ComplianceRunLogButton, ComplianceRunPanel } from './complianceprogress'
import {
  DownloadManifestsButton, EvidencePanel, ManifestDrawer, ManifestLinks,
  NoEvidenceNotice,
} from './complianceevidence'
import { formatCount } from '../domain/format'
import { c, mono } from '../uikit'
import type { ComplianceChart, ComplianceCounts, ComplianceResult } from '../api/types'

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
}> = {
  critical: {
    label: 'Critical',
    noun: 'critical findings',
    outcome: ['fail'],
    severity: ['block'],
    count: (c) => c?.blocking ?? 0,
  },
  warning: {
    label: 'Warning',
    noun: 'warnings',
    outcome: ['fail'],
    severity: ['warn'],
    count: (c) => c?.warning ?? 0,
  },
  info: {
    label: 'Info',
    noun: 'informational findings',
    outcome: ['fail'],
    severity: ['info'],
    count: (c) => c?.info ?? 0,
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
  const [manifest, setManifest] = useState<string | null>(null)

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
  })
  const run = useRunCompliance()
  const cancel = useCancelCompliance()
  // Analysing, offered from HERE. See the empty state below for why this tab
  // needs it at all, and why sending the reader to another tab to press it was
  // the wrong way to ask.
  const inspect = useInspectPackage(product, reference, repository)
  const data = compliance.data

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

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      {data && <HelmMissingNotice helm={data.helm} />}
      {data?.run && <RunFailedNotice run={data.run} />}
      {data?.run && <TruncatedNotice run={data.run} />}

      {/* The verdict and the numbers. */}
      {data?.run && (
        <Card
          title={
            <VerdictPill verdict={data.run.verdict} label={data.run.verdictLabel} />
          }
          extra={
            <Space size={8} wrap>
              {/*
                The rulebook, one click from a finding. Somebody arguing about
                a result wants to read the rule, and making them find a page
                they have never visited is how a disagreement becomes a
                complaint about the tool.
              */}
              <Link to="/policies" style={{ fontSize: 12, color: c.brand }}>
                View the rulebook
              </Link>
              {/*
                The transcript, after the run. It was on screen for the whole
                check and then vanished with it - and "which charts refused,
                and what did the nine minutes go on" is asked afterwards. The
                vulnerability sync has had this button since it shipped.
              */}
              <ComplianceRunLogButton run={data.run} />
              <Button size="small" loading={run.isPending} disabled={running} onClick={start}>
                Re-check
              </Button>
            </Space>
          }
        >
          <Space direction="vertical" size={12} style={{ width: '100%' }}>
            <ComplianceSummary
              counts={data.run.counts}
              selected={tab === 'findings' ? SUMMARY_FOR_VIEW[view] : undefined}
              onSelect={(what) => {
                setTab('findings')
                setView(VIEW_FOR_SUMMARY[what])
              }}
            />
            <RunProvenance run={data.run} />
          </Space>
        </Card>
      )}

      {/*
        FINDINGS FIRST, CHARTS BEHIND THEM.
        Two cards side by side asked the reader to decide which mattered on
        every visit. Findings are why anybody opens this tab; coverage is what
        they check when a number looks wrong - so coverage is one click away
        and never in front.
      */}
      <Card
        size="small"
        styles={{ body: { paddingTop: 16 } }}
        title={
          <Tabs
            size="small"
            activeKey={tab}
            onChange={(k) => setTab(k as 'findings' | 'charts')}
            style={{ marginBottom: -16 }}
            items={[
              {
                key: 'findings',
                label: (
                  <Space size={6}>
                    <span>Findings</span>
                    <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                      {formatCount(data?.run?.counts.blocking ?? 0)}
                    </Typography.Text>
                  </Space>
                ),
              },
              {
                key: 'charts',
                label: (
                  <Space size={6}>
                    <HelmOutlined style={{ color: c.text3 }} />
                    <span>Charts</span>
                    <Typography.Text
                      type={brokenCharts(charts) > 0 ? 'warning' : 'secondary'}
                      style={{ fontSize: 12 }}
                    >
                      {charts.length - brokenCharts(charts)}/{charts.length}
                    </Typography.Text>
                  </Space>
                ),
              },
            ]}
          />
        }
      >
        {tab === 'charts' ? (
          <ChartCoverage
            charts={charts}
            product={product}
            reference={reference}
            repository={repository}
            loading={compliance.isFetching}
            onOpenManifest={setManifest}
          />
        ) : (
          <Space direction="vertical" size={12} style={{ width: '100%' }}>
            {/*
              The controls, on the LEFT and in the order the page's other
              listings use them: the search box first, then the narrowing
              selects, then the sort. Packages does this and a reader who has
              learned it there should not have to learn it again here.
            */}
            <Space size={10} wrap style={{ width: '100%' }}>
              <Input
                allowClear
                style={{ width: 280 }}
                prefix={<SearchOutlined style={{ color: c.text3 }} />}
                placeholder="Search resource, chart, file or check"
                value={draft}
                onChange={(e) => setDraft(e.target.value)}
              />
              <Select
                allowClear
                placeholder="Any chart"
                style={{ minWidth: 180 }}
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
                style={{ minWidth: 160 }}
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
                style={{ minWidth: 210 }}
                value={determinacy}
                onChange={setDeterminacy}
                options={[
                  { label: 'The vendor must fix it', value: 'fixed' },
                  { label: 'A values file can fix it', value: 'configurable' },
                  { label: 'Ownership not established', value: 'unknown' },
                ]}
              />
              <Select
                value={sort}
                onChange={setSort}
                style={{ minWidth: 190 }}
                options={[
                  { label: 'Sort by severity', value: 'severity' },
                  { label: 'Sort by chart', value: 'chart' },
                  { label: 'Sort by check', value: 'check' },
                  { label: 'Sort by resource', value: 'resource' },
                ]}
              />
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                {formatCount(data?.total ?? 0)} {slice.noun}
                {(data?.total ?? 0) > results.length && (
                  <> · showing {formatCount(results.length)}</>
                )}
              </Typography.Text>
            </Space>

            {/*
              THE OUTCOME SLICE. Critical first because it is what a release
              decision turns on, and Passed is present because the passes are
              the denominator - "40 workloads, all compliant" and "the traversal
              never reached them" are the same empty list otherwise.
            */}
            <Segmented
              size="small"
              value={view}
              onChange={(v) => setView(v as ResultView)}
              options={VIEW_ORDER.map((k) => ({
                label: (
                  <Space size={6}>
                    <span>{VIEWS[k].label}</span>
                    <Typography.Text type="secondary" style={{ fontSize: 11 }}>
                      {formatCount(VIEWS[k].count(data?.run?.counts))}
                    </Typography.Text>
                  </Space>
                ),
                value: k,
              }))}
            />

            <ResultsTable
              results={sortResults(results, sort)}
              loading={compliance.isFetching}
              onOpen={setDetail}
              emptyText={emptyTextFor(view, Boolean(search || chart || determinacy || kind))}
            />
          </Space>
        )}
      </Card>

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
function ChartCoverage({ charts, product, reference, repository, loading, onOpenManifest }: {
  charts: ComplianceChart[]
  product: string
  reference: string
  repository?: string
  loading?: boolean
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

  // What the run KEPT, which is not the same list as what it rendered: a
  // deployment can turn evidence off, and a large release can exhaust the
  // budget partway through. Read here rather than per row so the table shows
  // one truth about which charts can actually be opened.
  const kept = useRenderedManifests(product, reference, { repository })
  const available = new Set((kept.data?.documents ?? []).map((d) => d.document))
  // A run that rendered charts and kept none of them. Said once, at the top,
  // rather than a sentence in every finding somebody opens: "no evidence" and
  // "no findings" are different statements, and a reader who expects the
  // manifest deserves to have been told before they go looking for it.
  const noneKept = kept.isSuccess && available.size === 0
    && (charts ?? []).some((ch) => ch.status === 'ok')

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
        <DownloadManifestsButton
          product={product}
          reference={reference}
          repository={repository}
          documents={kept.data?.documents.length ?? 0}
          bytes={kept.data?.totalBytes ?? 0}
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
      case 'chart': return `${r.chart ?? ''} ${r.sourceFile ?? ''} ${r.check}`
      case 'check': return `${r.check} ${r.chart ?? ''} ${r.name ?? ''}`
      default: return `${r.kind ?? ''} ${r.name ?? ''} ${r.check}`
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
