import { useEffect, useMemo, useState } from 'react'
import { Button, Card, Checkbox, Input, Segmented, Select, Space, Tooltip, Typography } from 'antd'
// The working-surface table: resizable, reorderable, pinnable columns whose
// layout each person keeps. See `tablekit/README.md` for which tables get it.
import { Table as DataTable } from '../tablekit'
import { Link, useSearchParams } from 'react-router-dom'
import { useProducts, useSecuritySearch, securitySearchExportUrl } from '../api/queries'
import type { SearchKind, SecuritySearchHit, Severity } from '../api/types'
import { SEVERITIES } from '../api/types'
import {
  ComponentCell, CveCell, FixCell, SecurityExportMenu, SeverityTag,
} from '../components/security'
import { ErrorState, PageHeader } from '../components/layout'
import { EmptyState, InlineNotice, mono } from '../uikit'

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
        <Space size={12} wrap style={{ width: '100%' }}>
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
            {/*
              One control, not five checkboxes - the same filter the release's
              Security tab uses. Five checkboxes could not say "any" without
              being all unchecked, which reads as "none selected".

              It sits IN this row rather than under it, and is disabled rather
              than absent until there is something to filter. On its own line it
              was one orphan control under a row of five, and because it
              appeared only once a search had returned, the toolbar grew a
              second row the first time somebody used it and everything below
              jumped down.
            */}
            <Select<Severity[]>
              mode="multiple"
              allowClear
              disabled={!search.data || search.data.hits.length === 0}
              placeholder="Any severity"
              value={severities}
              onChange={setSeverities}
              style={{ minWidth: 190 }}
              maxTagCount="responsive"
              options={SEVERITIES.map((sev) => ({ label: <SeverityTag value={sev} />, value: sev }))}
            />

            {urlQuery && product && (
              <span style={{ marginInlineStart: 'auto' }}>
                <SecurityExportMenu
                  urlFor={(format) => securitySearchExportUrl(product, kind, urlQuery, format, exact)}
                />
              </span>
            )}
        </Space>

      </Card>

      {search.isError && <ErrorState error={search.error} retry={() => void search.refetch()} />}

      {search.data && (
        <>
          {/* One sentence gets a line, not a block: the Alert spent a band of
              the page on a description that restated its own title. */}
          {search.data.truncated && (
            <div style={{ marginBottom: 12 }}>
              <InlineNotice tone="info">
                This is a partial result. Narrow the search, or export it - an export contains the
                whole result rather than the page on screen.
              </InlineNotice>
            </div>
          )}

          <Card styles={{ body: { padding: 0 } }}>
            {/*
              NO table export here, deliberately, though every other working
              surface has one. This page already has one, in the toolbar above,
              and the two would answer different questions: that one exports the
              whole result the server matched, this one would export the
              twenty-five rows currently painted. A file that looks complete and
              is not is worse than no file.
            */}
            <DataTable<SecuritySearchHit>
              tableEnhancedKey="security-search"
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
                  /*
                    THE THING ITSELF IS THE WAY THROUGH.

                    Each of these three cells used to carry a sentence under it -
                    "all images with this CVE", "All findings for this package",
                    "All findings in this image" - so a screen of twenty-five
                    results held fifty repetitions of three phrases, in the
                    accent colour, and each row stood 108px tall to hold them.
                    Nine rows fit on a laptop.

                    A reader who wants every image with this CVE clicks the CVE.
                    That is what a link on an identifier means everywhere else,
                    it needs no sentence to explain it, and the tooltip names the
                    destination for anybody unsure. Rows are 44px now.
                  */
                  title: 'CVE',
                  width: 160,
                  render: (_, h) => (
                    h.cve && kind !== 'cve'
                      ? (
                        <Tooltip title="Every image and release carrying this CVE">
                          <Link to={linkTo(product, 'cve', h.cve)}>
                            <CveCell cve={h.cve} id={h.issueId} />
                          </Link>
                        </Tooltip>
                      )
                      : <CveCell cve={h.cve} id={h.issueId} />
                  ),
                },
                {
                  title: 'Severity',
                  // 140, because a sortable header is its label PLUS the
                  // arrows: at 110 this column was headed "Sever…".
                  width: 140,
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
                    h.component.name && kind !== 'package'
                      ? (
                        <Tooltip title="Every finding against this package">
                          <Link to={linkTo(product, 'package', h.component.name)}>
                            <ComponentCell
                              name={h.component.name}
                              version={h.component.version}
                              type={h.component.type}
                            />
                          </Link>
                        </Tooltip>
                      )
                      : (
                        <ComponentCell
                          name={h.component.name}
                          version={h.component.version}
                          type={h.component.type}
                        />
                      )
                  ),
                },
                {
                  title: 'Image',
                  width: 210,
                  render: (_, h) => {
                    // The digest moves to the tooltip. It is the row's proof
                    // rather than its identity: nobody scans a column of
                    // truncated hashes looking for one, and on a page where the
                    // same image recurs it was a second line of noise per row.
                    const label = (
                      <Typography.Text
                        style={{ fontFamily: mono, display: 'block' }}
                        ellipsis={{ tooltip: false }}
                      >
                        {h.artifact.display || h.artifact.name}
                      </Typography.Text>
                    )
                    const title = [
                      h.artifact.display || h.artifact.name,
                      h.artifact.digest,
                      kind !== 'image' ? 'Every finding in this image' : '',
                    ].filter(Boolean).join(' · ')
                    return kind !== 'image'
                      ? (
                        <Tooltip title={title}>
                          <Link to={linkTo(product, 'image', h.artifact.name)}>{label}</Link>
                        </Tooltip>
                      )
                      : <Tooltip title={title}>{label}</Tooltip>
                  },
                },
                {
                  // The same cell the release's own findings table draws, rather
                  // than a second copy of it that had already drifted: this one
                  // never showed the fixed version on its tooltip.
                  title: 'Fix',
                  width: 130,
                  render: (_, h) => (
                    <FixCell fixable={h.fixable} fixedIn={h.fixedIn ? [h.fixedIn] : undefined} />
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
                  width: 190,
                  render: (_, h) => {
                    const rels = h.releases ?? []
                    if (rels.length === 0) {
                      return <Typography.Text type="secondary">Not in a tracked release</Typography.Text>
                    }
                    // On ONE line. Stacked, three releases made the row three
                    // lines tall for a fact most rows state once.
                    return (
                      <Space size={6} wrap={false} style={{ minWidth: 0 }}>
                        {rels.slice(0, 2).map((rel) => (
                          <Link
                            key={rel.packageId}
                            to={`/packages/${encodeURIComponent(product ?? '')}/${encodeURIComponent(rel.tag)}`}
                            style={{ fontFamily: mono, fontSize: 12, whiteSpace: 'nowrap' }}
                          >
                            {rel.displayTag || rel.tag}
                          </Link>
                        ))}
                        {rels.length > 2 && (
                          <Tooltip title={rels.map((r) => r.displayTag || r.tag).join(', ')}>
                            <Typography.Text type="secondary" style={{ fontSize: 11, whiteSpace: 'nowrap' }}>
                              +{rels.length - 2}
                            </Typography.Text>
                          </Tooltip>
                        )}
                      </Space>
                    )
                  },
                },
                {
                  // Last, and narrow. It is the only column here whose
                  // absence costs nothing - the CVE identifier is the handle
                  // somebody works from - so it is the one that gives up width
                  // to keep the other six on screen.
                  title: 'Description',
                  width: 320,
                  render: (_, h) => (
                    <Typography.Paragraph
                      style={{ margin: 0 }}
                      ellipsis={{ rows: 1, tooltip: h.summary }}
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

      {/*
        The house empty state, not a card of stacked paragraphs. Nothing has
        been asked yet, which is a STATE - and every other unasked page in both
        tools draws it the same way: the drawing, one sentence, and the offer.
      */}
      {!urlQuery && (
        <Card>
          <EmptyState
            title="Search synced vulnerability data"
            hint="A CVE identifier lists every image and release carrying it. A package name lists the vulnerabilities affecting it. An image name lists its findings. Each result links through to the other two."
          >
            <Space size={8} wrap>
              <Button onClick={() => update({ kind: 'cve', q: 'CVE-2024' })}>Try CVE-2024</Button>
              <Button onClick={() => update({ kind: 'package', q: 'openssl' })}>Try openssl</Button>
            </Space>
          </EmptyState>
          <Typography.Paragraph
            type="secondary"
            style={{ textAlign: 'center', margin: 0, paddingBottom: 8, fontSize: 12 }}
          >
            Search covers releases whose vulnerabilities have been synced. To include a release,
            sync it from its Security tab or from the packages listing.
          </Typography.Paragraph>
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
