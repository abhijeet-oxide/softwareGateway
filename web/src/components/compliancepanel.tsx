import { useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import {
  App, Button, Card, Descriptions, Drawer, Input, Segmented, Select, Space, Tooltip, Typography,
} from 'antd'
// The working-surface table: resizable, reorderable, pinnable columns whose
// layout each person keeps. See `tablekit/README.md` for which tables get it.
import { Table as DataTable } from '../tablekit'
import { LoadingOutlined } from '../icons'
import {
  useCancelCompliance, useInspectPackage, usePackageCompliance, useRunCompliance,
} from '../api/queries'
import { EmptyStateCard } from './layout'
import {
  CheckSeverityTag, ComplianceProgressPanel, ComplianceSummary, DeterminacyTag,
  HelmMissingNotice, InconclusiveNotice, OutcomePill, ResultAddress, RunFailedNotice,
  RunProvenance, TruncatedNotice, VerdictPill,
} from './compliance'
import { c, mono } from '../uikit'
import type { ComplianceResult } from '../api/types'

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
 * # Why the default view is failures
 *
 * A release produces ten to fifteen thousand results, most of them passes.
 * Opening on all of them buries the four rows somebody has to act on. The
 * counts stay on screen either way, so the short list is never mistaken for a
 * small denominator, and one control switches to the coverage view.
 */
export function ComplianceTab({ product, reference, repository }: {
  product: string
  reference: string
  repository?: string
}) {
  const { message } = App.useApp()

  const [view, setView] = useState<'findings' | 'everything'>('findings')
  const [chart, setChart] = useState<string | undefined>()
  const [determinacy, setDeterminacy] = useState<string | undefined>()
  const [search, setSearch] = useState('')
  const [detail, setDetail] = useState<ComplianceResult | null>(null)

  const compliance = usePackageCompliance(product, reference, {
    repository,
    all: view === 'everything',
    chart: chart ? [chart] : undefined,
    determinacy: determinacy ? [determinacy] : undefined,
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
  if (!compliance.isLoading && data && !data.analysed && !data.run && !data.progress) {
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
              ? 'Reading the manifest tree from the vendor registry. It carries on if you leave '
                + 'this page, and this tab offers the check as soon as the walk finishes.'
              : 'Its charts are listed already - the release index names them. Where each '
                + "chart's content lives is not: that is a layer inside the chart's own "
                + 'manifest, and walking the release is what records it. A check reads the '
                + 'charts themselves, so it has nothing to open until then.'
          }
          action={
            <Space direction="vertical" size={10}>
              {started ? (
                <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                  <LoadingOutlined spin style={{ color: c.brand, marginRight: 6 }} />
                  Walking the manifest tree. Nothing is downloaded.
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

  // A release with no result at all. The empty state OFFERS the action rather
  // than describing the absence, because the reader's next move is always the
  // same one.
  if (!compliance.isLoading && data && !data.run && !data.progress) {
    return (
      <Space direction="vertical" size={16} style={{ width: '100%' }}>
        <HelmMissingNotice helm={data.helm} />
        <EmptyStateCard
          title="This release has not been checked"
          explanation={
            'Nothing here has been compared against your standards yet. That is not the same as '
            + 'passing them.'
          }
          action={
            <Space direction="vertical" size={10}>
              <Button type="primary" loading={run.isPending} onClick={start}>
                Check this release
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

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      {data && <HelmMissingNotice helm={data.helm} />}
      {data?.run && <RunFailedNotice run={data.run} />}
      {data?.run && <TruncatedNotice run={data.run} />}
      {data?.run && (
        <InconclusiveNotice
          run={data.run}
          charts={charts}
          onShowUndecided={() => { setView('findings'); setSearch('') }}
        />
      )}

      {/* The live position, while a run is going. */}
      {data?.progress && (
        <Card size="small">
          <ComplianceProgressPanel
            progress={data.progress}
            cancelling={cancel.isPending}
            onCancel={() => cancel.mutate({ product, ref: reference, repository })}
          />
        </Card>
      )}

      {/* The verdict and the numbers. */}
      {data?.run && (
        <Card
          loading={compliance.isLoading}
          title={
            <Space size={12}>
              <VerdictPill verdict={data.run.verdict} label={data.run.verdictLabel} />
              {running && (
                <Tooltip title="A compliance check is in progress">
                  <LoadingOutlined spin style={{ fontSize: 12 }} />
                </Tooltip>
              )}
            </Space>
          }
          extra={
            <Space size={12}>
              {/*
                The rulebook, one click from a finding. Somebody arguing about
                a result wants to read the rule, and making them find a page
                they have never visited is how a disagreement becomes a
                complaint about the tool.
              */}
              <Link to="/policies" style={{ fontSize: 12, color: c.brand }}>
                What is checked?
              </Link>
              <Button size="small" loading={run.isPending} disabled={running} onClick={start}>
                Re-check
              </Button>
            </Space>
          }
        >
          <Space direction="vertical" size={12} style={{ width: '100%' }}>
            <ComplianceSummary counts={data.run.counts} />
            <RunProvenance run={data.run} />
          </Space>
        </Card>
      )}

      {/* The charts: the run's denominator. A reader who cannot see that three
          of ninety-seven charts failed to render reads the finding count as
          the whole story. */}
      {charts.length > 0 && <ChartCoverage charts={charts} />}

      {/* The rows. */}
      <Card
        size="small"
        title={
          <Space size={10} wrap>
            <Segmented
              size="small"
              value={view}
              onChange={(v) => setView(v as 'findings' | 'everything')}
              options={[
                { label: 'Findings', value: 'findings' },
                { label: 'Everything', value: 'everything' },
              ]}
            />
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              {(data?.total ?? 0).toLocaleString()}
              {view === 'findings' ? ' needing attention' : ' results'}
            </Typography.Text>
          </Space>
        }
        extra={
          <Space size={8} wrap>
            <Select
              allowClear
              size="small"
              placeholder="Any chart"
              style={{ minWidth: 170 }}
              value={chart}
              onChange={setChart}
              options={charts
                .filter((ch) => ch.name && ch.name !== '(not fetched)')
                .map((ch) => ({ label: ch.name, value: ch.name }))}
            />
            {/* The split a reader makes first: whose problem is this. */}
            <Select
              allowClear
              size="small"
              placeholder="Anyone's to fix"
              style={{ minWidth: 190 }}
              value={determinacy}
              onChange={setDeterminacy}
              options={[
                { label: 'The vendor must fix', value: 'fixed' },
                { label: 'Overridable in values', value: 'configurable' },
                { label: 'Could not be established', value: 'unknown' },
              ]}
            />
            <Input.Search
              allowClear
              size="small"
              placeholder="Resource, file, check…"
              style={{ width: 210 }}
              onSearch={setSearch}
            />
          </Space>
        }
      >
        <ResultsTable
          results={results}
          loading={compliance.isFetching}
          onOpen={setDetail}
          emptyText={
            view === 'findings'
              ? 'Nothing needs attention in this view.'
              : 'No results match these filters.'
          }
        />
      </Card>

      <ResultDrawer result={detail} onClose={() => setDetail(null)} />
    </Space>
  )
}

/**
 * What each chart contributed.
 *
 * Charts that did not render come first, because everything below them is a
 * smaller denominator than it looks.
 */
function ChartCoverage({ charts }: { charts: NonNullable<ReturnType<typeof usePackageCompliance>['data']>['charts'] }) {
  const rows = charts ?? []
  const broken = rows.filter((ch) => ch.status !== 'ok')
  const [open, setOpen] = useState(broken.length > 0)

  return (
    <Card
      size="small"
      title={
        <Space size={10}>
          <span>Charts</span>
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            {rows.length - broken.length} of {rows.length} rendered
            {broken.length > 0 && (
              <span style={{ color: c.pending }}> · {broken.length} did not</span>
            )}
          </Typography.Text>
        </Space>
      }
      extra={
        <Typography.Link onClick={() => setOpen((v) => !v)}>
          {open ? 'Hide' : 'Show'}
        </Typography.Link>
      }
      styles={open ? undefined : { body: { display: 'none' } }}
    >
      {open && (
        <DataTable
          tableEnhancedKey="compliance-charts"
          size="small"
          rowKey={(ch) => `${ch.name}@${ch.version}@${ch.digest}`}
          dataSource={rows}
          pagination={false}
          columns={[
            {
              title: 'Chart', dataIndex: 'name', width: 260,
              render: (_: unknown, ch) => (
                <Space direction="vertical" size={0}>
                  <span style={{ fontFamily: mono, fontSize: 12 }}>{ch.name}</span>
                  {ch.version && (
                    <span style={{ fontFamily: mono, fontSize: 11, color: c.text3 }}>
                      {ch.version}
                    </span>
                  )}
                </Space>
              ),
            },
            {
              title: 'Rendered', dataIndex: 'status', width: 120,
              render: (_: unknown, ch) => (
                <OutcomePill
                  outcome={ch.status === 'ok' ? 'pass' : 'error'}
                  label={ch.status === 'ok' ? 'Yes' : ch.status === 'skipped' ? 'Skipped' : 'Failed'}
                />
              ),
            },
            {
              title: 'Objects', dataIndex: 'resources', width: 90, align: 'right' as const,
              render: (n: number) => n.toLocaleString(),
            },
            {
              title: 'Reason', dataIndex: 'error',
              render: (e?: string) =>
                e ? <span style={{ fontFamily: mono, fontSize: 11, color: c.text2 }}>{e}</span> : null,
            },
          ]}
        />
      )}
    </Card>
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
      title: 'What was found', dataIndex: 'message',
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
      locale={{ emptyText }}
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
function ResultDrawer({ result, onClose }: {
  result: ComplianceResult | null
  onClose: () => void
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

          <Descriptions column={1} size="small" bordered>
            <Descriptions.Item label="Chart">
              <span style={{ fontFamily: mono }}>
                {result.chart}
                {result.chartVersion && `:${result.chartVersion}`}
              </span>
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
              <Descriptions.Item label="Found">
                <span style={{ fontFamily: mono }}>{result.observed}</span>
              </Descriptions.Item>
            )}
            {result.expected && (
              <Descriptions.Item label="Expected">
                <span style={{ fontFamily: mono }}>{result.expected}</span>
              </Descriptions.Item>
            )}
            {result.determinacy && result.determinacy !== 'na' && (
              <Descriptions.Item label="Whose to fix">
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
            <Card size="small" title="What to do">
              <Typography.Paragraph style={{ marginBottom: 0 }}>
                {result.remediation}
              </Typography.Paragraph>
            </Card>
          )}

          {result.reference && (
            <Typography.Link href={result.reference} target="_blank" rel="noreferrer">
              The standard this comes from
            </Typography.Link>
          )}
        </Space>
      )}
    </Drawer>
  )
}
