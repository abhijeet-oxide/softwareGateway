import { useEffect, useMemo, useState } from 'react'
import { Alert, Button, Card, Checkbox, Input, Segmented, Select, Space, Tag, Tooltip, Typography } from 'antd'
// The working-surface table: resizable, reorderable, pinnable columns whose
// layout each person keeps. See `tablekit/README.md` for which tables get it.
import { Table as DataTable } from '../tablekit'
import { Link, useSearchParams } from 'react-router-dom'
import { useProducts, useSecuritySearch, securitySearchExportUrl } from '../api/queries'
import type { SearchKind, SecuritySearchHit, Severity } from '../api/types'
import { SEVERITIES } from '../api/types'
import {
  ComponentCell, CveCell, SecurityExportMenu, SeverityTag,
} from '../components/security'
import { ErrorState, PageHeader } from '../components/layout'
import { c, EmptyState, mono } from '../uikit'

/**
 * Search across CVEs, packages and images, and navigate the relationships
 * between them.
 *
 * # What this page can and cannot answer
 *
 * It searches the index a SYNC wrote. It cannot answer "is this CVE anywhere in
 * my estate", only "is it in a release somebody has synced", and those are
 * different sentences. Answering the first would mean scanning every release in
 * the catalogue on every keystroke.
 *
 * # How it is fast, and how it is accurate
 *
 * Fast because it is one indexed SQL query against identifiers - CVE, component
 * name, artifact name - and never a scanner request. Accurate for what has been
 * synced, and honest about the rest: the note under every result names the
 * remedy, so a reader who finds nothing knows to sync rather than concluding
 * they are safe.
 *
 * That limit is stated on the page rather than left to be discovered. A search
 * that silently returned nothing would be read as "this CVE does not affect
 * us", which is the most dangerous thing this page could say wrongly.
 *
 * # Why one page and three kinds rather than three pages
 *
 * Because the answer is the same table. A CVE search groups by image, a package
 * search groups by image, an image search groups by package - one row type with
 * every relationship on it serves all three, and each result links onward to the
 * other two.
 */
export default function Security() {
  const [params, setParams] = useSearchParams()

  const products = useProducts()
  const productList = products.data?.products ?? []
  const product = params.get('product') ?? productList[0]?.productId

  const kind = (params.get('kind') as SearchKind | null) ?? 'cve'
  const urlQuery = params.get('q') ?? ''
  const exact = params.get('exact') === 'true'

  // The box is local and the URL is the committed search, so typing does not
  // push a history entry per keystroke and a result is still a link somebody
  // can send.
  const [draft, setDraft] = useState(urlQuery)
  useEffect(() => setDraft(urlQuery), [urlQuery])

  const [severities, setSeverities] = useState<Severity[]>([])

  const search = useSecuritySearch(product, kind, urlQuery, { exact, limit: 500 })

  const hits = useMemo(() => {
    const all = search.data?.hits ?? []
    if (severities.length === 0) return all
    return all.filter((h) => severities.includes(h.severity))
  }, [search.data, severities])

  const update = (next: Record<string, string | undefined>) => {
    const p = new URLSearchParams(params)
    for (const [k, v] of Object.entries(next)) {
      if (v) p.set(k, v)
      else p.delete(k)
    }
    setParams(p)
  }

  return (
    <>
      <PageHeader
        title="Security search"
        description="Locate a CVE, package or image across every release with synced vulnerability data."
      />

      <Card size="small" style={{ marginBottom: 16 }}>
        <Space direction="vertical" size={12} style={{ width: '100%' }}>
          <Space size={12} wrap>
            <Select
              style={{ minWidth: 200 }}
              value={product}
              loading={products.isLoading}
              onChange={(v) => update({ product: v })}
              options={productList.map((p) => ({ value: p.productId, label: p.displayName || p.productId }))}
            />
            <Segmented
              value={kind}
              onChange={(v) => update({ kind: v as string })}
              options={[
                { value: 'cve', label: 'CVE' },
                { value: 'package', label: 'Package' },
                { value: 'image', label: 'Image' },
              ]}
            />
            <Input.Search
              allowClear
              style={{ width: 360 }}
              value={draft}
              onChange={(e) => setDraft(e.target.value)}
              onSearch={(v) => update({ q: v || undefined })}
              placeholder={PLACEHOLDER[kind]}
              enterButton="Search"
              loading={search.isFetching}
            />
            <Tooltip title="Match the whole value rather than any part of it. Useful when a short name matches many packages.">
              <Checkbox
                checked={exact}
                onChange={(e) => update({ exact: e.target.checked ? 'true' : undefined })}
              >
                Exact match
              </Checkbox>
            </Tooltip>
            {urlQuery && product && (
              <SecurityExportMenu
                urlFor={(format) => securitySearchExportUrl(product, kind, urlQuery, format, exact)}
              />
            )}
          </Space>

          {/*
            One control, not five checkboxes - the same filter the release's
            Security tab uses. Five checkboxes could not say "any" without being
            all unchecked, which reads as "none selected", and they took 300px
            of a toolbar that has a search box in it.
          */}
          {search.data && search.data.hits.length > 0 && (
            <Select<Severity[]>
              mode="multiple"
              allowClear
              placeholder="Any severity"
              value={severities}
              onChange={setSeverities}
              style={{ minWidth: 190 }}
              maxTagCount="responsive"
              options={SEVERITIES.map((s) => ({ label: <SeverityTag value={s} />, value: s }))}
            />
          )}
        </Space>
      </Card>

      {search.isError && <ErrorState error={search.error} retry={() => void search.refetch()} />}

      {search.data && (
        <>
          {search.data.truncated && (
            <Alert
              type="info"
              showIcon
              style={{ marginBottom: 12 }}
              message="Showing a partial result"
              description="Narrow the search, or export it. An export contains the complete result rather than the page on screen."
            />
          )}

          <Card styles={{ body: { padding: 0 } }}>
            <DataTable<SecuritySearchHit>
              tableEnhancedKey="security-search"
              allow_export
              show_column_visibility
              size="small"
              rowKey={(h, i) => `${h.cve ?? h.issueId}-${h.component.id}-${h.artifact.digest}-${i}`}
              dataSource={hits}
              scroll={{ x: 'max-content' }}
              pagination={{ pageSize: 25, showSizeChanger: true, size: 'small' }}
              locale={{
                emptyText: (
                  <EmptyState
                    title="Nothing found in what has been retrieved"
                    hint={search.data?.searched.note}
                  />
                ),
              }}
              columns={[
                {
                  title: 'CVE',
                  width: 160,
                  render: (_, h) => (
                    <Space direction="vertical" size={2}>
                      <CveCell cve={h.cve} id={h.issueId} />
                      {h.cve && kind !== 'cve' && (
                        <Link to={linkTo(product, 'cve', h.cve)} style={{ fontSize: 11 }}>
                          all images with this CVE
                        </Link>
                      )}
                    </Space>
                  ),
                },
                {
                  title: 'Severity',
                  width: 110,
                  // Worst first, because SEVERITIES is ordered worst first: the
                  // comparator was reversed, so sorting ascending put `low`
                  // at the top of a table about what is wrong with a release.
                  sorter: (a, b) => SEVERITIES.indexOf(a.severity) - SEVERITIES.indexOf(b.severity),
                  render: (_, h) => <SeverityTag value={h.severity} />,
                },
                {
                  title: 'Package',
                  width: 200,
                  render: (_, h) => (
                    <Space direction="vertical" size={2}>
                      <ComponentCell
                        name={h.component.name}
                        version={h.component.version}
                        type={h.component.type}
                      />
                      {h.component.name && kind !== 'package' && (
                        <Link to={linkTo(product, 'package', h.component.name)} style={{ fontSize: 11 }}>
                          All findings for this package
                        </Link>
                      )}
                    </Space>
                  ),
                },
                {
                  title: 'Image',
                  width: 210,
                  render: (_, h) => (
                    <Space direction="vertical" size={2}>
                      <Typography.Text style={{ fontFamily: mono }}>
                        {h.artifact.display || h.artifact.name}
                      </Typography.Text>
                      {h.artifact.digest && (
                        <Typography.Text
                          type="secondary"
                          style={{ fontFamily: mono, fontSize: 11 }}
                          ellipsis={{ tooltip: h.artifact.digest }}
                        >
                          {h.artifact.digest.slice(0, 19)}
                        </Typography.Text>
                      )}
                      {kind !== 'image' && (
                        <Link to={linkTo(product, 'image', h.artifact.name)} style={{ fontSize: 11 }}>
                          All findings in this image
                        </Link>
                      )}
                    </Space>
                  ),
                },
                {
                  title: 'Fix',
                  width: 120,
                  render: (_, h) => (
                    h.fixable
                      ? (
                        <Space direction="vertical" size={0}>
                          <Tag color="success" style={{ marginInlineEnd: 0 }}>Fixable</Tag>
                          {h.fixedIn && (
                            <Typography.Text type="secondary" style={{ fontFamily: mono, fontSize: 11 }}>
                              {h.fixedIn}
                            </Typography.Text>
                          )}
                        </Space>
                      )
                      : <Typography.Text type="secondary">No fix available</Typography.Text>
                  ),
                },
                {
                  /*
                   * The edge that makes this a graph rather than a list. From a
                   * CVE to the packages, from a package to the images, from an
                   * image to the releases shipping it - and each release links
                   * to its own security view, which is where the journey ends.
                   */
                  title: 'Releases',
                  width: 200,
                  render: (_, h) => (
                    (h.releases ?? []).length === 0
                      ? <Typography.Text type="secondary">not in a tracked release</Typography.Text>
                      : (
                        <Space direction="vertical" size={2}>
                          {(h.releases ?? []).slice(0, 3).map((rel) => (
                            <Link
                              key={rel.packageId}
                              to={`/packages/${encodeURIComponent(product ?? '')}/${encodeURIComponent(rel.tag)}`}
                              style={{ fontFamily: mono, fontSize: 12 }}
                            >
                              {rel.displayTag || rel.tag}
                            </Link>
                          ))}
                          {(h.releases ?? []).length > 3 && (
                            <Typography.Text type="secondary" style={{ fontSize: 11 }}>
                              +{(h.releases ?? []).length - 3} more
                            </Typography.Text>
                          )}
                        </Space>
                      )
                  ),
                },
                {
                  // Last, and narrow. It is the only column here whose
                  // absence costs nothing - the CVE identifier is the handle
                  // somebody works from - so it is the one that gives up width
                  // to keep the other six on screen.
                  title: 'Description',
                  width: 280,
                  render: (_, h) => (
                    <Typography.Paragraph
                      style={{ margin: 0 }}
                      ellipsis={{ rows: 2, tooltip: h.summary }}
                    >
                      {h.summary || '-'}
                    </Typography.Paragraph>
                  ),
                },
              ]}
            />
          </Card>

          {/*
            Said under every result, including a full one. The limit is a
            property of the search rather than of this query, and a reader who
            learns it only when a search comes back empty has already drawn the
            wrong conclusion once.
          */}
          <Typography.Paragraph type="secondary" style={{ marginTop: 12, fontSize: 12 }}>
            {search.data.searched.note}
            {search.data.searched.artifacts > 0 && (
              ` Searched ${search.data.searched.artifacts} artifacts across ${search.data.searched.releases} releases.`
            )}
          </Typography.Paragraph>
        </>
      )}

      {!urlQuery && (
        <Card>
          <Space direction="vertical" size={8}>
            <Typography.Text strong>Search synced vulnerability data</Typography.Text>
            <Typography.Text type="secondary">
              Enter a CVE identifier to list every image and release that carries it, a package name to
              list the vulnerabilities affecting it, or an image name to list its findings. Each result
              links through to the other two views.
            </Typography.Text>
            <Space size={8} wrap>
              <Button size="small" onClick={() => update({ kind: 'cve', q: 'CVE-2024' })}>
                Example: CVE-2024
              </Button>
              <Button size="small" onClick={() => update({ kind: 'package', q: 'openssl' })}>
                Example: openssl
              </Button>
            </Space>
            <Typography.Text type="secondary" style={{ color: c.text2 }}>
              Search covers releases whose vulnerabilities have been synced. To include a release,
              sync it from its Security tab or from the packages listing.
            </Typography.Text>
          </Space>
        </Card>
      )}
    </>
  )
}

const PLACEHOLDER: Record<SearchKind, string> = {
  cve: 'CVE-2024-3094, or part of it',
  package: 'openssl, log4j, glibc',
  image: 'cfx-main, a tag, or a digest',
}

function linkTo(product: string | undefined, kind: SearchKind, q: string): string {
  const p = new URLSearchParams()
  if (product) p.set('product', product)
  p.set('kind', kind)
  p.set('q', q)
  return `/security?${p.toString()}`
}
