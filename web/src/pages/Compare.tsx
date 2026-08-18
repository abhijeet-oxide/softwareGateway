import { useEffect, useState } from 'react'
import {
  App, Button, Card, Checkbox, Col, Drawer, Empty, Progress, Row, Segmented, Select, Space,
  Statistic, Table, Tag, Tooltip, Typography,
} from 'antd'
import { SwapOutlined } from '@ant-design/icons'
import { useSearchParams } from 'react-router-dom'
import {
  useCompare, useCompareProgress, usePackages, useProduct, useProducts,
} from '../api/queries'
import { matches, version } from '../domain/derive'
import { formatBytes, formatCount, formatDuration } from '../domain/format'
import { NA, Value } from '../components/value'
import { ErrorState, PageHeader } from '../components/layout'
import { WorkingBar } from '../components/progress'
import { mono, semantic } from '../theme'
import type { CompareRow, CompareVerdict, Package, Repository } from '../api/types'

/**
 * Page 5 — Compare.
 *
 * Answers: what is different between these two, exactly?
 *
 * # The shape of the question
 *
 * A comparison has two axes and the API takes them separately: WHICH VERSION
 * (`against`) and WHERE (`from` and `to`). Naming only a version compares two
 * releases in one place; naming only a place compares one release in two;
 * naming both answers each at once. The form is arranged the same way, so what
 * is being asked stays legible as it is assembled.
 *
 * # Why the mode selector exists
 *
 * Most comparisons are one of two questions — "what changed between these
 * releases" and "did this release arrive intact" — and each needs only half
 * the form. Offering four selectors for both made the common case look like
 * the hard case.
 */

/**
 * The configured SOURCE a release was discovered in.
 *
 * The API needs an endpoint NAME, and a package records a repository PATH. A
 * product with more than one source will not infer which end is meant — it
 * refuses rather than guessing — so the path is matched back to the source
 * that declares it, which is knowable and saves asking the reader for
 * something the release already implies.
 */
function sourceNameFor(
  sources: Repository[] | undefined, repositoryPath: string | undefined,
): string | undefined {
  if (!repositoryPath) return undefined
  for (const s of sources ?? []) {
    if (s.repository === repositoryPath) return s.name
    if (s.repositories?.includes(repositoryPath)) return s.name
  }
  return undefined
}

/**
 * How a release is identified in these selects.
 *
 * `repository:tag`, which is exactly the reference the API resolves — and the
 * only unambiguous one. A vendor publishes one version tag into every
 * repository a product watches, so a select keyed on the tag alone had ten
 * options with the same value and the same label, and picking any of them
 * asked the server a question it correctly refused to answer.
 */
function refOf(pkg: Package): string {
  return pkg.sourceRepository ? `${pkg.sourceRepository}:${pkg.tag}` : pkg.tag
}

/** The option list for a release select: the name, the version, and both searchable. */
function releaseOptions(releases: Package[]) {
  return releases.map((r) => ({
    value: refOf(r),
    // Kept a plain string so the closed select shows one line. The two-line
    // form is in optionRender below, where there is room for it.
    label: `${r.displayRepository || r.sourceRepository || ''} · ${version(r)}`,
    name: r.displayRepository || r.sourceRepository || '',
    version: version(r),
    tag: r.tag,
  }))
}

type ReleaseOption = ReturnType<typeof releaseOptions>[number]

/** Name above, version below — the two things that identify a release. */
function renderReleaseOption(option: ReleaseOption) {
  return (
    <Space direction="vertical" size={0}>
      <Typography.Text style={{ fontFamily: mono, fontSize: 12 }}>{option.version}</Typography.Text>
      <Typography.Text type="secondary" style={{ fontSize: 11 }}>{option.name}</Typography.Text>
    </Space>
  )
}

/**
 * What the comparison is actually doing.
 *
 * # Why a real bar and not an animation
 *
 * The work is countable: each side reads a known set of manifests and then
 * probes a known set of component names. An animation says only "something is
 * happening", which is exactly what it would say if nothing were — and on a
 * request that legitimately runs for minutes, that is the difference between
 * waiting and giving up.
 *
 * # Per side, because one of them is the slow one
 *
 * The two ends are walked concurrently against different registries. A single
 * merged number hides which of them is holding everything up, which is the
 * first thing worth knowing when a comparison is slow.
 *
 * The denominator during the manifest phase is what is KNOWN — a tree is
 * discovered by walking it — so it is marked as an estimate rather than
 * presented as a position it has not earned.
 */
function ComparisonProgress({
  token, elapsedSeconds,
}: {
  token: string | undefined
  elapsedSeconds: number
}) {
  const progress = useCompareProgress(token)
  const sides = progress.data?.sides ?? []

  const done = sides.reduce((n, s) => n + s.done, 0)
  const total = sides.reduce((n, s) => n + s.total, 0)
  const estimated = sides.some((s) => s.estimated)
  const percent = total > 0 ? Math.min(99, (done / total) * 100) : 0

  // Said only once it is true, and it is a statement about this comparison
  // rather than a warning attached to every one of them.
  const slow = elapsedSeconds > 120

  return (
    <Space direction="vertical" size={8} style={{ width: '100%' }}>
      {sides.length === 0 ? (
        <WorkingBar
          label="Analysing packages"
          detail="This may take a while"
          elapsedSeconds={elapsedSeconds}
        />
      ) : (
        <>
          <Progress
            percent={Number(percent.toFixed(1))}
            status="active"
            format={() => `${formatCount(done)} of ${formatCount(total)}${estimated ? '+' : ''}`}
          />
          <Space size={24} wrap>
            {sides.map((side) => (
              <Typography.Text key={side.side} type="secondary" style={{ fontSize: 12 }}>
                <strong>{side.side}</strong> — {side.phase} {formatCount(side.done)}
                {side.total > 0 && ` of ${formatCount(side.total)}`}
              </Typography.Text>
            ))}
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              {formatDuration(elapsedSeconds)} elapsed
            </Typography.Text>
          </Space>
        </>
      )}

      <Typography.Text type="secondary" style={{ fontSize: 12 }}>
        {slow
          ? 'Analysing packages — this is taking longer than expected. Large releases with many components read slowly from their registries; the counts above are still advancing.'
          : 'Analysing packages, this may take a while.'}
      </Typography.Text>
    </Space>
  )
}

const VERDICT: Record<CompareVerdict, { label: string; colour: string }> = {
  same: { label: 'Unchanged', colour: 'default' },
  changed: { label: 'Changed', colour: 'orange' },
  'only-a': { label: 'Removed', colour: 'red' },
  'only-b': { label: 'Added', colour: 'green' },
}

type Mode = 'versions' | 'locations'

export default function Compare() {
  const [params, setParams] = useSearchParams()
  const { message } = App.useApp()

  const products = useProducts()
  const productList = products.data?.products ?? []
  const product = params.get('product') ?? productList[0]?.productId

  const detail = useProduct(product)
  const packages = usePackages(product, { pageSize: 200 })
  const releases = packages.data?.packages ?? []

  const [mode, setMode] = useState<Mode>('versions')
  const [left, setLeft] = useState<string | undefined>(params.get('from') ?? undefined)
  const [right, setRight] = useState<string>()
  const [fromEndpoint, setFromEndpoint] = useState<string>()
  const [toEndpoint, setToEndpoint] = useState<string>()
  const [sourceOverride, setSourceOverride] = useState<string>()
  const [open, setOpen] = useState<CompareRow>()
  const [withFiles, setWithFiles] = useState(false)
  // Elapsed while it runs. Both releases are read live from their registries,
  // so a comparison takes as long as those registries take — and a button that
  // says `loading` for four minutes with nothing beside it is the shape of
  // something broken.
  const [startedAt, setStartedAt] = useState<number>()
  const [elapsed, setElapsed] = useState(0)
  // Minted here, per run: the comparison's response is the report, so there is
  // nothing for the server to hand an id back in. Sending one is what makes
  // progress readable while the request is open.
  const [token, setToken] = useState<string>()
  // The first release is almost always the one somebody means, and having to
  // pick it before the page does anything is a step with no decision in it.
  useEffect(() => {
    if (!left && releases.length > 0) setLeft(refOf(releases[0]!))
  }, [releases, left])

  const compare = useCompare()
  const compareRunning = compare.isPending
  const report = compare.data

  useEffect(() => {
    if (!compareRunning || !startedAt) return
    const id = setInterval(() => setElapsed((Date.now() - startedAt) / 1000), 500)
    return () => clearInterval(id)
  }, [compareRunning, startedAt])

  const endpoints = [
    { value: '', label: 'Vendor (where it was discovered)' },
    ...(detail.data?.targets ?? []).map((t) => ({
      value: t.name,
      label: `${t.name}${t.environment ? ` · ${t.environment}` : ''}`,
    })),
  ]

  const leftPkg = releases.find((r) => refOf(r) === left)
  const sources = detail.data?.sources ?? []
  // Where the release was found, matched back to its configured source. The
  // default end for a version comparison, overridable below for a product
  // whose sources this cannot match — one that discovers its repositories
  // from the registry catalog declares none to match against.
  const discoveredIn = sourceNameFor(sources, leftPkg?.sourceRepository)
  const versionEnd = sourceOverride ?? discoveredIn ?? sources[0]?.name

  const ready = Boolean(
    product && left &&
    (mode === 'versions' ? right && versionEnd : toEndpoint !== undefined),
  )

  const run = async () => {
    if (!product || !left) return
    setStartedAt(Date.now())
    setElapsed(0)
    const progressToken = crypto.randomUUID()
    setToken(progressToken)
    try {
      await compare.mutateAsync({
        product,
        ref: left,
        repository: leftPkg?.sourceRepository,
        body: {
          // A version comparison names the other release; a location
          // comparison names the other place. Both may be sent, and then the
          // answer covers both at once.
          // A version comparison still has to NAME its end: a product with
          // several sources will not guess which one, and both sides of a
          // version comparison are the same place.
          against: mode === 'versions' ? right : undefined,
          from: mode === 'versions' ? versionEnd : (fromEndpoint || undefined),
          to: mode === 'versions' ? versionEnd : (toEndpoint || undefined),
          // OFF by default, and this is the difference between a comparison
          // that answers in seconds and one that reads like a hang. Opening
          // layer archives to name the files inside them means downloading
          // them; on a release of a few hundred components that is the whole
          // cost of the request. Asked for explicitly, below.
          fileBudgetBytes: withFiles ? 64 * 1024 * 1024 : -1,
          progressToken,
        },
      })
    } catch (e) {
      message.error(e instanceof Error ? e.message : 'The comparison could not be run.')
    }
  }

  const swap = () => {
    if (mode === 'versions') {
      setLeft(right)
      setRight(left)
    } else {
      setFromEndpoint(toEndpoint)
      setToEndpoint(fromEndpoint)
    }
  }

  const update = (key: string, value?: string) => {
    const next = new URLSearchParams(params)
    if (value) next.set(key, value)
    else next.delete(key)
    setParams(next)
  }

  const total = report ? report.same + report.changed + report.onlyA + report.onlyB : 0
  const differing = report ? report.changed + report.onlyA + report.onlyB : 0

  const rowsTable = (rows: CompareRow[]) => (
    <Table<CompareRow>
      size="small"
      dataSource={rows}
      rowKey={(r) => `${r.type}-${r.name}-${r.verdict}`}
      pagination={{ pageSize: 12, hideOnSinglePage: true, showSizeChanger: false }}
      scroll={{ x: 720 }}
      locale={{ emptyText: <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="Nothing here" /> }}
      columns={[
        {
          title: 'Name',
          render: (_, r) => (
            <Value>{r.name}</Value>
          ),
        },
        { title: 'Type', width: 100, render: (_, r) => <Tag style={{ marginInlineEnd: 0 }}>{r.type}</Tag> },
        {
          title: 'Verdict',
          width: 120,
          render: (_, r) => (
            <Tag color={VERDICT[r.verdict]?.colour} style={{ marginInlineEnd: 0 }}>
              {VERDICT[r.verdict]?.label ?? r.verdict}
            </Tag>
          ),
        },
        {
          title: 'Size',
          width: 110,
          align: 'right',
          render: (_, r) => <Value>{formatBytes(r.b?.size ?? r.a?.size)}</Value>,
        },
        {
          title: 'Files',
          width: 130,
          render: (_, r) => {
            const n = (r.filesAdded?.length ?? 0) + (r.filesRemoved?.length ?? 0) +
              (r.filesChanged?.length ?? 0)
            if (!n) {
              return r.filesTruncated
                ? <NA reason="A layer was left unopened — past the download budget, or not an archive — so the files inside it are unknown." />
                : <Typography.Text type="secondary" style={{ fontSize: 12 }}>—</Typography.Text>
            }
            return <Typography.Text style={{ fontSize: 12 }}>{n} changed</Typography.Text>
          },
        },
        {
          title: 'Action',
          width: 90,
          fixed: 'right',
          render: (_, r) =>
            r.verdict === 'same' ? null : (
              <Button size="small" onClick={() => setOpen(r)}>View</Button>
            ),
        },
      ]}
    />
  )

  if (products.isError) {
    return (
      <>
        <PageHeader title="Compare packages" description="What is different between two versions or locations" />
        <ErrorState error={products.error} retry={() => void products.refetch()} />
      </>
    )
  }

  return (
    <>
      <PageHeader
        title="Compare packages"
        description="What is different between two versions or locations of the same software"
      />

      <Card style={{ marginBottom: 16 }}>
        <Space direction="vertical" size={14} style={{ width: '100%' }}>
          <Space size={12} wrap>
            <Select
              style={{ minWidth: 200 }}
              value={product}
              onChange={(v) => { update('product', v); setLeft(undefined); setRight(undefined) }}
              loading={products.isLoading}
              options={productList.map((p) => ({ value: p.productId, label: p.displayName || p.productId }))}
            />
            <Segmented
              value={mode}
              onChange={(v) => setMode(v as Mode)}
              options={[
                { value: 'versions', label: 'Two versions' },
                { value: 'locations', label: 'Two locations' },
              ]}
            />
          </Space>

          {/*
            Wraps rather than sitting in fixed thirds: at 1280 the three
            selectors and the button no longer fit on one line, and the page
            used to overflow instead of reflowing.
          */}
          <Row gutter={[12, 12]} align="bottom">
            <Col xs={24} md={10} lg={8}>
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                {mode === 'versions' ? 'Version' : 'From'}
              </Typography.Text>
              {mode === 'versions' ? (
                <Select<string, ReleaseOption>
                  style={{ width: '100%' }}
                  placeholder="Choose a release"
                  value={left}
                  onChange={setLeft}
                  showSearch
                  // Over the NAME and the version both. A product has several
                  // packages and each has many versions, so searching one of
                  // the two finds half of what somebody types.
                  filterOption={(input, option) =>
                    matches(input, option?.name, option?.version, option?.tag)}
                  optionRender={(option) => renderReleaseOption(option.data)}
                  loading={packages.isLoading}
                  options={releaseOptions(releases)}
                />
              ) : (
                <Select
                  style={{ width: '100%' }}
                  value={fromEndpoint ?? ''}
                  onChange={setFromEndpoint}
                  options={endpoints}
                />
              )}
            </Col>

            <Col xs={24} md={4} lg={2} style={{ textAlign: 'center' }}>
              <Button icon={<SwapOutlined />} onClick={swap} aria-label="Swap sides" block />
            </Col>

            <Col xs={24} md={10} lg={8}>
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                {mode === 'versions' ? 'Against version' : 'To'}
              </Typography.Text>
              {mode === 'versions' ? (
                <Select<string, ReleaseOption>
                  style={{ width: '100%' }}
                  placeholder="Choose a release"
                  value={right}
                  onChange={setRight}
                  showSearch
                  filterOption={(input, option) =>
                    matches(input, option?.name, option?.version, option?.tag)}
                  optionRender={(option) => renderReleaseOption(option.data)}
                  loading={packages.isLoading}
                  options={releaseOptions(releases)}
                />
              ) : (
                <Select
                  style={{ width: '100%' }}
                  value={toEndpoint ?? ''}
                  onChange={setToEndpoint}
                  options={endpoints}
                />
              )}
            </Col>

            {mode === 'versions' && sources.length > 1 && (
              <Col xs={24} md={10} lg={6}>
                <Typography.Text type="secondary" style={{ fontSize: 12 }}>In</Typography.Text>
                <Select
                  style={{ width: '100%' }}
                  value={versionEnd}
                  onChange={setSourceOverride}
                  options={sources.map((src) => ({ value: src.name, label: src.name }))}
                />
              </Col>
            )}

            <Col xs={24} lg={mode === 'versions' && sources.length > 1 ? 24 : 6}>
              <Button
                type="primary"
                block
                disabled={!ready}
                loading={compare.isPending}
                onClick={() => void run()}
              >
                Compare
              </Button>
            </Col>
          </Row>

          <Space size={16} wrap>
            <Checkbox checked={withFiles} onChange={(e) => setWithFiles(e.target.checked)}>
              <Space size={6}>
                Also compare file contents
                <Tooltip title="Opens each side's layer archives to name the files that differ, rather than reporting that a layer changed. It downloads those archives, so it is much slower — and on a large release it is most of the time the comparison takes.">
                  <Typography.Text type="secondary" style={{ fontSize: 12 }}>(slower)</Typography.Text>
                </Tooltip>
              </Space>
            </Checkbox>
          </Space>

          {compareRunning && <ComparisonProgress token={token} elapsedSeconds={elapsed} />}

        </Space>
      </Card>

      {compare.isError && <ErrorState error={compare.error} retry={() => void run()} />}

      {report && (
        <Row gutter={[16, 16]}>
          <Col span={24}>
            <Card>
              <Row gutter={[16, 16]} align="middle">
                <Col xs={24} lg={9}>
                  <Space direction="vertical" size={2}>
                    <Typography.Text type="secondary" style={{ fontSize: 12 }}>Comparing</Typography.Text>
                    <Typography.Text strong className="mono" style={{ fontSize: 13 }}>{report.a.label}</Typography.Text>
                    <Typography.Text type="secondary" style={{ fontSize: 12 }}>against</Typography.Text>
                    <Typography.Text strong className="mono" style={{ fontSize: 13 }}>{report.b.label}</Typography.Text>
                  </Space>
                </Col>
                <Col xs={12} sm={6} lg={3}>
                  <Statistic title="Added" value={report.onlyB} valueStyle={{ color: semantic.success }} />
                </Col>
                <Col xs={12} sm={6} lg={3}>
                  <Statistic title="Removed" value={report.onlyA} valueStyle={{ color: semantic.error }} />
                </Col>
                <Col xs={12} sm={6} lg={3}>
                  <Statistic title="Changed" value={report.changed} valueStyle={{ color: semantic.warning }} />
                </Col>
                <Col xs={12} sm={6} lg={3}>
                  <Statistic title="Unchanged" value={report.same} />
                </Col>
                <Col xs={24} lg={3}>
                  <Statistic title="Components" value={total} />
                </Col>
              </Row>

              {(report.extraTagsA?.length || report.extraTagsB?.length) && (
                <Typography.Paragraph type="warning" style={{ marginTop: 12, marginBottom: 0, fontSize: 12 }}>
                  Content nobody in this comparison put there:{' '}
                  {formatCount((report.extraTagsA?.length ?? 0) + (report.extraTagsB?.length ?? 0))} extra tags
                  {(report.extraTruncatedA || report.extraTruncatedB) && ', and more than were resolved'}.
                </Typography.Paragraph>
              )}
            </Card>
          </Col>

          <Col span={24}>
            <Card
              title={differing === 0 ? 'These are identical' : `${differing} differences`}
              extra={
                <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                  {formatCount(report.same)} unchanged, not listed
                </Typography.Text>
              }
              styles={{ body: { padding: 0 } }}
            >
              {differing === 0 ? (
                <div style={{ padding: 24 }}>
                  <Empty
                    image={Empty.PRESENTED_IMAGE_SIMPLE}
                    description="Every component matches on both sides, down to the digest."
                  />
                </div>
              ) : (
                rowsTable(report.rows.filter((r) => r.verdict !== 'same'))
              )}
            </Card>
          </Col>
        </Row>
      )}

      <Drawer
        open={Boolean(open)}
        onClose={() => setOpen(undefined)}
        width={680}
        title={open?.name}
      >
        {open && (
          <Space direction="vertical" size={16} style={{ width: '100%' }}>
            <Space>
              <Tag color={VERDICT[open.verdict]?.colour}>{VERDICT[open.verdict]?.label}</Tag>
              <Tag>{open.type}</Tag>
            </Space>

            {open.differences?.length ? (
              <Card size="small" title="What differs">
                <ul style={{ margin: 0, paddingInlineStart: 18 }}>
                  {open.differences.map((d) => (
                    <li key={d}><Typography.Text style={{ fontSize: 13 }}>{d}</Typography.Text></li>
                  ))}
                </ul>
              </Card>
            ) : null}

            <Row gutter={12}>
              {([['Left', open.a], ['Right', open.b]] as const).map(([label, side]) => (
                <Col xs={24} md={12} key={label}>
                  <Card size="small" title={label}>
                    <Space direction="vertical" size={2} style={{ width: '100%' }}>
                      <Typography.Text type="secondary" style={{ fontSize: 11 }}>Digest</Typography.Text>
                      <Typography.Text style={{ fontFamily: mono, fontSize: 11 }}>
                        <Value>{side?.digest}</Value>
                      </Typography.Text>
                      <Typography.Text type="secondary" style={{ fontSize: 11 }}>Size</Typography.Text>
                      <Typography.Text><Value>{formatBytes(side?.size)}</Value></Typography.Text>
                      <Typography.Text type="secondary" style={{ fontSize: 11 }}>Pullable as itself</Typography.Text>
                      <Typography.Text style={{ fontSize: 12 }}>
                        {side?.namedRepository
                          ? (side.namedPresent ? `Yes — ${side.namedRepository}` : `No — expected at ${side.namedRepository}`)
                          : <NA />}
                      </Typography.Text>
                    </Space>
                  </Card>
                </Col>
              ))}
            </Row>

            {(open.filesAdded?.length || open.filesRemoved?.length || open.filesChanged?.length) ? (
              <Card size="small" title="Files inside this component">
                {([
                  ['Added', open.filesAdded, semantic.success],
                  ['Removed', open.filesRemoved, semantic.error],
                  ['Changed', open.filesChanged, semantic.warning],
                ] as const).map(([label, files, colour]) =>
                  files?.length ? (
                    <div key={label} style={{ marginBottom: 10 }}>
                      <Typography.Text strong style={{ color: colour, fontSize: 12 }}>
                        {label} ({files.length})
                      </Typography.Text>
                      <div style={{ maxHeight: 180, overflow: 'auto', marginTop: 4 }}>
                        {files.map((f) => (
                          <div key={f} className="diff-line">{f}</div>
                        ))}
                      </div>
                    </div>
                  ) : null,
                )}
                {open.filesTruncated && (
                  <Typography.Text type="warning" style={{ fontSize: 12 }}>
                    A layer was left unopened, so this is a partial account of what differs inside.
                  </Typography.Text>
                )}
              </Card>
            ) : (
              <Tooltip title="Either the layers were not archives, or opening them would have exceeded the comparison's download budget.">
                <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                  No file-level difference was resolved for this component. What differs is the
                  metadata above.
                </Typography.Text>
              </Tooltip>
            )}
          </Space>
        )}
      </Drawer>
    </>
  )
}
