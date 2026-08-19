import { useEffect, useMemo, useState } from 'react'
import {
  Alert, App, Button, Card, Checkbox, Col, Empty, Popover, Progress, Row, Segmented, Select,
  Space, Statistic, Table, Tag, Tooltip, Typography,
} from 'antd'
import { SwapOutlined } from '@ant-design/icons'
import { useSearchParams } from 'react-router-dom'
import {
  useCompare, useCompareProgress, usePackages, useProduct, useProducts,
} from '../api/queries'
import { kindName, matches, version } from '../domain/derive'
import { bytes, formatBytes, formatCount, formatDuration } from '../domain/format'
import { NA, Value } from '../components/value'
import { ErrorState, PageHeader, SearchBar } from '../components/layout'
import { WorkingBar } from '../components/progress'
import { ARTIFACT_ICONS, Icon } from '../components/icons'
import { mono, semantic } from '../theme'
import type {
  CompareResponse, CompareRow, CompareVerdict, Package, Repository,
} from '../api/types'

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

/**
 * The four impacts, in the words a reader uses about a release.
 *
 * `statColour` is separate from `colour` because a tag and a headline number
 * are read differently: a tag is a label and takes Ant Design's palette name, a
 * statistic is a quantity and takes the semantic colour it means.
 */
const VERDICT: Record<CompareVerdict, { label: string; colour: string; statColour?: string }> = {
  same: { label: 'Unchanged', colour: 'default' },
  changed: { label: 'Changed', colour: 'orange', statColour: semantic.warning },
  'only-a': { label: 'Removed', colour: 'red', statColour: semantic.error },
  'only-b': { label: 'Added', colour: 'green', statColour: semantic.success },
}

type Mode = 'versions' | 'locations'


/**
 * The answer, once there is one.
 *
 * # What a comparison is actually asked
 *
 * Not "how many things differ" — that is a number somebody reads once and can
 * act on never. It is asked because a release is about to be shipped, or has
 * just landed somewhere and might be wrong, and the questions are: what KIND of
 * thing changed, HOW MUCH of it, and WHICH ones. So the summary counts by
 * bucket and by kind and by size, and the table lists every component rather
 * than the differing ones with a footnote about the rest.
 */
function ComparisonReport({
  report, onChange,
}: {
  report: CompareResponse
  onChange: () => void
}) {
  const [search, setSearch] = useState('')
  const [impact, setImpact] = useState<'all' | CompareVerdict>('all')

  const rows = useMemo(() => {
    const byImpact = impact === 'all'
      ? report.rows
      : report.rows.filter((r) => r.verdict === impact)
    if (!search.trim()) return byImpact
    return byImpact.filter((r) => matches(
      search, r.name, r.type, r.a?.tag, r.b?.tag, r.a?.digest, r.b?.digest))
  }, [report.rows, impact, search])

  // Counted from the rows rather than from the report's totals, so the buckets
  // and their breakdowns cannot disagree with the table under them.
  const buckets = useMemo(() => summarise(report.rows), [report.rows])

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <Row gutter={[16, 16]}>
        {(['only-b', 'only-a', 'changed', 'same'] as const).map((verdict) => (
          <Col xs={12} lg={6} key={verdict}>
            <BucketCard bucket={buckets[verdict]} verdict={verdict} />
          </Col>
        ))}
      </Row>

      {(report.extraTagsA?.length || report.extraTagsB?.length) ? (
        <Alert
          type="warning"
          showIcon
          message="Content nobody in this comparison put there"
          description={
            <Typography.Text style={{ fontSize: 13 }}>
              {formatCount((report.extraTagsA?.length ?? 0) + (report.extraTagsB?.length ?? 0))} tags
              in these repositories point at content this release does not account for
              {(report.extraTruncatedA || report.extraTruncatedB) && ', and there may be more than were resolved'}.
            </Typography.Text>
          }
        />
      ) : null}

      <Card
        title={`Components (${formatCount(report.rows.length)})`}
        extra={
          <Space size={12} wrap>
            <Segmented
              size="small"
              value={impact}
              onChange={(v) => setImpact(v as typeof impact)}
              options={[
                { value: 'all', label: 'All' },
                { value: 'only-b', label: `Added ${buckets['only-b'].count}` },
                { value: 'only-a', label: `Removed ${buckets['only-a'].count}` },
                { value: 'changed', label: `Changed ${buckets.changed.count}` },
                { value: 'same', label: `Unchanged ${buckets.same.count}` },
              ]}
            />
            <Button size="small" onClick={onChange}>Compare something else</Button>
          </Space>
        }
        styles={{ body: { padding: 0 } }}
      >
        <div style={{ padding: '12px 16px 0' }}>
          <SearchBar
            value={search}
            onChange={setSearch}
            placeholder="Search by name, tag or digest"
            matched={rows.length}
            total={report.rows.length}
            width={340}
          />
        </div>

        <Table<CompareRow>
          size="small"
          dataSource={rows}
          rowKey={(r) => `${r.type}-${r.name}-${r.verdict}-${r.a?.digest ?? ''}-${r.b?.digest ?? ''}`}
          pagination={{ pageSize: 25, showSizeChanger: false, size: 'small' }}
          scroll={{ x: 1200 }}
          locale={{
            emptyText: (
              <Empty image={Empty.PRESENTED_IMAGE_SIMPLE} description="Nothing matches this filter" />
            ),
          }}
          expandable={{
            // Only where there is more than the row already says: the files
            // inside a component, and the facts behind a `changed` verdict.
            rowExpandable: (r) => Boolean(
              r.differences?.length || r.filesAdded?.length ||
              r.filesRemoved?.length || r.filesChanged?.length),
            expandedRowRender: (r) => <RowDetail row={r} />,
          }}
          columns={[
            {
              title: 'Name',
              width: 300,
              render: (_, r) => (
                <Space direction="vertical" size={0} style={{ minWidth: 0 }}>
                  <Tooltip title={r.name}>
                    <Typography.Text style={{ fontSize: 13, maxWidth: 280 }} ellipsis>
                      {r.name || <NA reason="This component carries no name of its own." />}
                    </Typography.Text>
                  </Tooltip>
                  {(r.b?.tag || r.a?.tag) && (
                    <Typography.Text type="secondary" style={{ fontFamily: mono, fontSize: 11 }}>
                      {r.b?.tag || r.a?.tag}
                    </Typography.Text>
                  )}
                </Space>
              ),
            },
            {
              title: 'Type',
              width: 130,
              render: (_, r) => {
                const name = kindName(r.type)
                const icon = ARTIFACT_ICONS[name as keyof typeof ARTIFACT_ICONS]
                return (
                  <Space size={6}>
                    {icon && <Icon as={icon} size={15} title={name} />}
                    <Typography.Text style={{ fontSize: 12 }}>{name}</Typography.Text>
                  </Space>
                )
              },
            },
            {
              // "Impact", not "verdict": the question is what this change does
              // to whoever consumes the release, not what the comparison ruled.
              title: 'Impact',
              width: 120,
              render: (_, r) => (
                <Tag color={VERDICT[r.verdict]?.colour} style={{ marginInlineEnd: 0 }}>
                  {VERDICT[r.verdict]?.label ?? r.verdict}
                </Tag>
              ),
            },
            {
              title: 'Size',
              width: 170,
              align: 'right',
              render: (_, r) => <SizeCell row={r} />,
            },
            {
              // The digests, which are the only unambiguous statement of what
              // changed — and for a `changed` row, both of them, because "it
              // changed" without saying from what to what is not a finding
              // anybody can act on.
              title: 'Digest',
              width: 250,
              render: (_, r) => <DigestCell row={r} />,
            },
            {
              title: 'Files',
              width: 120,
              render: (_, r) => {
                const n = (r.filesAdded?.length ?? 0) + (r.filesRemoved?.length ?? 0) +
                  (r.filesChanged?.length ?? 0)
                if (n > 0) {
                  return (
                    <Space size={4}>
                      <Typography.Text style={{ fontSize: 12 }}>{n} changed</Typography.Text>
                      {r.filesTruncated && (
                        <Tooltip title="Some layers of this component were left unopened, so there may be more inside them.">
                          <Typography.Text type="warning" style={{ fontSize: 12 }}>+</Typography.Text>
                        </Tooltip>
                      )}
                    </Space>
                  )
                }
                if (r.verdict === 'same') {
                  return <Typography.Text type="secondary" style={{ fontSize: 12 }}>—</Typography.Text>
                }
                if (r.filesTruncated) {
                  return (
                    <NA reason="This component's layers are archives with no names on them. Tick 'Also compare file contents' and run it again to open them and see which files differ." />
                  )
                }
                return <Typography.Text type="secondary" style={{ fontSize: 12 }}>—</Typography.Text>
              },
            },
          ]}
        />
      </Card>
    </Space>
  )
}

/** One bucket of the summary: how many, how much, and of what. */
function BucketCard({ bucket, verdict }: { bucket: Bucket; verdict: CompareVerdict }) {
  const meta = VERDICT[verdict]
  const kinds = Object.entries(bucket.byKind)
    .filter(([, v]) => v.count > 0)
    .sort((a, b) => b[1].count - a[1].count)

  return (
    <Popover
      placement="bottom"
      title={`${meta.label} — what it is made of`}
      content={
        kinds.length === 0 ? (
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>Nothing in this bucket.</Typography.Text>
        ) : (
          <Space direction="vertical" size={4} style={{ minWidth: 220 }}>
            {kinds.map(([kind, v]) => {
              const name = kindName(kind)
              const icon = ARTIFACT_ICONS[name as keyof typeof ARTIFACT_ICONS]
              return (
                <Space key={kind} size={8} style={{ width: '100%', justifyContent: 'space-between' }}>
                  <Space size={6}>
                    {icon && <Icon as={icon} size={14} title={name} />}
                    <Typography.Text style={{ fontSize: 12 }}>{name}</Typography.Text>
                  </Space>
                  <Typography.Text style={{ fontSize: 12 }}>
                    {formatCount(v.count)} · <Value>{formatBytes(v.bytes)}</Value>
                  </Typography.Text>
                </Space>
              )
            })}
          </Space>
        )
      }
    >
      <Card size="small" style={{ cursor: 'default' }}>
        <Statistic
          title={meta.label}
          value={bucket.count}
          valueStyle={{ color: meta.statColour }}
        />
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
          <Value reason="Nothing in this bucket has a size we could read.">
            {formatBytes(bucket.bytes)}
          </Value>
        </Typography.Text>
      </Card>
    </Popover>
  )
}

/** What a component weighs, and what it weighed before. */
function SizeCell({ row }: { row: CompareRow }) {
  const a = bytes(row.a?.size)
  const b = bytes(row.b?.size)

  if (row.verdict === 'changed' && a !== undefined && b !== undefined && a !== b) {
    const delta = b - a
    return (
      <Space direction="vertical" size={0}>
        <Typography.Text style={{ fontSize: 12 }}>{formatBytes(b)}</Typography.Text>
        <Typography.Text
          type={delta > 0 ? 'warning' : 'success'}
          style={{ fontSize: 11 }}
        >
          {delta > 0 ? '+' : '−'}{formatBytes(Math.abs(delta))} from {formatBytes(a)}
        </Typography.Text>
      </Space>
    )
  }
  return <Value>{formatBytes(b ?? a)}</Value>
}

/**
 * The digests, short enough to read and complete enough to act on.
 *
 * A `changed` row shows both: "it changed" without saying from what to what is
 * not something anybody can take to a vendor. Added and removed show the one
 * digest that exists, labelled by which side it is on.
 */
function DigestCell({ row }: { row: CompareRow }) {
  const line = (label: string, digest: string | undefined, colour?: string) =>
    digest ? (
      <Tooltip title={digest} key={label}>
        <Typography.Text style={{ fontSize: 11 }} type={colour as never}>
          <Typography.Text type="secondary" style={{ fontSize: 10 }}>{label} </Typography.Text>
          <span style={{ fontFamily: mono }}>{shortDigest(digest)}</span>
        </Typography.Text>
      </Tooltip>
    ) : null

  switch (row.verdict) {
    case 'changed':
      return (
        <Space direction="vertical" size={0}>
          {line('was', row.a?.digest)}
          {line('now', row.b?.digest)}
        </Space>
      )
    case 'only-a':
      return line('removed', row.a?.digest) ?? <NA />
    case 'only-b':
      return line('added', row.b?.digest) ?? <NA />
    default:
      return line('both', row.a?.digest ?? row.b?.digest) ?? <NA />
  }
}

/** The facts behind a verdict, and the files inside the component. */
function RowDetail({ row }: { row: CompareRow }) {
  return (
    <Space direction="vertical" size={12} style={{ width: '100%', paddingBlock: 4 }}>
      {row.differences?.length ? (
        <div>
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>What differs</Typography.Text>
          <ul style={{ margin: '4px 0 0', paddingInlineStart: 18 }}>
            {row.differences.map((d) => (
              <li key={d}><Typography.Text style={{ fontSize: 12 }}>{d}</Typography.Text></li>
            ))}
          </ul>
        </div>
      ) : null}

      {([
        ['Added', row.filesAdded, semantic.success],
        ['Removed', row.filesRemoved, semantic.error],
        ['Changed', row.filesChanged, semantic.warning],
      ] as const).map(([label, files, colour]) =>
        files?.length ? (
          <div key={label}>
            <Typography.Text strong style={{ color: colour, fontSize: 12 }}>
              {label} ({files.length})
            </Typography.Text>
            <div style={{ maxHeight: 200, overflow: 'auto', marginTop: 4 }}>
              {files.map((f) => (
                <div key={f} className="diff-line" style={{ fontFamily: mono, fontSize: 11 }}>{f}</div>
              ))}
            </div>
          </div>
        ) : null,
      )}

      {row.filesTruncated && (
        <Typography.Text type="warning" style={{ fontSize: 12 }}>
          A layer was left unopened, so this is a partial account of what differs inside.
        </Typography.Text>
      )}
    </Space>
  )
}

/** One bucket's totals, and the same totals per kind. */
interface Bucket {
  count: number
  bytes: number
  byKind: Record<string, { count: number; bytes: number }>
}

/**
 * Counts and sizes per bucket, and per kind within each bucket.
 *
 * The side each bucket is measured from is the side it EXISTS on: a removed
 * component's size is what the first side had, an added one's is what the
 * second has, and a changed one is measured by what it is now — the size of
 * what somebody would be shipping.
 */
function summarise(rows: CompareRow[]): Record<CompareVerdict, Bucket> {
  const empty = (): Bucket => ({ count: 0, bytes: 0, byKind: {} })
  const out: Record<CompareVerdict, Bucket> = {
    same: empty(), changed: empty(), 'only-a': empty(), 'only-b': empty(),
  }

  for (const row of rows) {
    const bucket = out[row.verdict]
    if (!bucket) continue
    const size = row.verdict === 'only-a'
      ? bytes(row.a?.size) ?? 0
      : bytes(row.b?.size) ?? bytes(row.a?.size) ?? 0

    bucket.count += 1
    bucket.bytes += size
    const kind = bucket.byKind[row.type] ?? { count: 0, bytes: 0 }
    kind.count += 1
    kind.bytes += size
    bucket.byKind[row.type] = kind
  }
  return out
}

/** sha256:abcd… — enough to recognise, short enough for a column. */
function shortDigest(digest: string): string {
  const hex = digest.includes(':') ? digest.split(':')[1] ?? '' : digest
  return hex.slice(0, 12)
}

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
  const [withFiles, setWithFiles] = useState(false)
  // Collapsed once there is a report. The form is scaffolding for the answer:
  // leaving four selectors above it pushes the thing somebody waited minutes
  // for below the fold, and every one of those selectors is now describing a
  // comparison that has already been run.
  const [settled, setSettled] = useState(false)
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
      setSettled(true)
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

      {settled && report ? (
        <Card size="small" style={{ marginBottom: 16 }}>
          <Space size={16} wrap style={{ width: '100%', justifyContent: 'space-between' }}>
            <Space size={8} wrap>
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>Comparing</Typography.Text>
              <Typography.Text strong style={{ fontFamily: mono, fontSize: 13 }}>
                {report.a.label}
              </Typography.Text>
              <SwapOutlined style={{ color: '#98A2B3' }} />
              <Typography.Text strong style={{ fontFamily: mono, fontSize: 13 }}>
                {report.b.label}
              </Typography.Text>
              {!withFiles && (
                <Tag style={{ marginInlineStart: 8 }}>components only</Tag>
              )}
            </Space>
            <Button onClick={() => setSettled(false)}>Change selection</Button>
          </Space>
        </Card>
      ) : (
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
      )}

      {compare.isError && <ErrorState error={compare.error} retry={() => void run()} />}

      {report && <ComparisonReport report={report} onChange={() => setSettled(false)} />}

    </>
  )
}
