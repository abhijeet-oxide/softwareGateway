import { useEffect, useState } from 'react'
import {
  App, Button, Card, Col, Drawer, Empty, Row, Segmented, Select, Space, Statistic,
  Table, Tag, Tooltip, Typography,
} from 'antd'
import { SwapOutlined } from '@ant-design/icons'
import { useSearchParams } from 'react-router-dom'
import { useCompare, usePackages, useProduct, useProducts } from '../api/queries'
import { version } from '../domain/derive'
import { formatBytes, formatCount } from '../domain/format'
import { NA, Value } from '../components/value'
import { ErrorState, PageHeader } from '../components/layout'
import { mono, semantic } from '../theme'
import type { CompareRow, CompareVerdict, Repository } from '../api/types'

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

  // The first release is almost always the one somebody means, and having to
  // pick it before the page does anything is a step with no decision in it.
  useEffect(() => {
    if (!left && releases.length > 0) setLeft(releases[0]!.tag)
  }, [releases, left])

  const compare = useCompare()
  const report = compare.data

  const endpoints = [
    { value: '', label: 'Vendor (where it was discovered)' },
    ...(detail.data?.targets ?? []).map((t) => ({
      value: t.name,
      label: `${t.name}${t.environment ? ` · ${t.environment}` : ''}`,
    })),
  ]

  const leftPkg = releases.find((r) => r.tag === left)
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
          // Enough budget to open layer archives and say which FILES differ,
          // which is the answer "two layers changed" cannot give.
          fileBudgetBytes: 64 * 1024 * 1024,
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
        <PageHeader title="Compare Software" description="What is different between two versions or locations" />
        <ErrorState error={products.error} retry={() => void products.refetch()} />
      </>
    )
  }

  return (
    <>
      <PageHeader
        title="Compare Software"
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
                <Select
                  style={{ width: '100%' }}
                  placeholder="Choose a release"
                  value={left}
                  onChange={setLeft}
                  showSearch
                  optionFilterProp="label"
                  loading={packages.isLoading}
                  options={releases.map((r) => ({ value: r.tag, label: version(r) }))}
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
                <Select
                  style={{ width: '100%' }}
                  placeholder="Choose a release"
                  value={right}
                  onChange={setRight}
                  showSearch
                  optionFilterProp="label"
                  loading={packages.isLoading}
                  options={releases.map((r) => ({ value: r.tag, label: version(r) }))}
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

          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            {mode === 'versions'
              ? 'Compares two releases of this product where they were discovered. Opening layer archives to name the files that differ is included.'
              : 'Compares one release in two places — the vendor against an internal repository, or one internal repository against another — which is how you confirm a download arrived intact.'}
          </Typography.Text>
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
