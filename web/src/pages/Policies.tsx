import { useMemo, useState } from 'react'
import { Alert, Card, Collapse, Input, Select, Space, Tooltip, Typography } from 'antd'
// The working-surface table: resizable, reorderable, pinnable columns whose
// layout each person keeps. See `tablekit/README.md` for which tables get it.
import { Table as DataTable } from '../tablekit'
import { usePolicies } from '../api/queries'
import { ErrorState, PageHeader } from '../components/layout'
import { CheckSeverityTag } from '../components/compliance'
import { c, mono } from '../uikit'
import type { PolicyCheck } from '../api/types'

/**
 * The rulebook: every check this platform will apply, and why.
 *
 * # Who this page is for
 *
 * Three people, and the order matters.
 *
 * A VENDOR who has not shipped yet and wants to know what will be checked.
 * They have no release to point at, which is why this page is not scoped to
 * one - a rulebook you can only see by failing it is not a rulebook.
 *
 * A REVIEWER settling an argument about a finding. They need the rationale,
 * not the expression: "why does this organization require it" is the question,
 * and it is the one thing a YAML file cannot answer by being read.
 *
 * An OPERATOR checking that the pack they mounted actually loaded. A pack that
 * failed is a set of checks that will report `error` on every release, and the
 * only place that is visible is here.
 */
export default function Policies() {
  const policies = usePolicies()
  const [search, setSearch] = useState('')
  const [category, setCategory] = useState<string | undefined>()
  const [severity, setSeverity] = useState<string | undefined>()

  const checks = policies.data?.checks ?? []
  const packs = policies.data?.packs ?? []
  const broken = packs.filter((p) => (p.errors?.length ?? 0) > 0)

  const categories = useMemo(() => {
    const seen = new Set<string>()
    for (const ch of checks) if (ch.category) seen.add(ch.category)
    return [...seen].sort()
  }, [checks])

  const filtered = useMemo(() => {
    const q = search.trim().toLowerCase()
    return checks.filter((ch) => {
      if (category && ch.category !== category) return false
      if (severity && ch.severity !== severity) return false
      if (!q) return true
      return [ch.id, ch.title, ch.description, ch.rationale, ch.category]
        .some((v) => v?.toLowerCase().includes(q))
    })
  }, [checks, search, category, severity])

  if (policies.isError) {
    return (
      <>
        <PageHeader title="Policies" />
        <ErrorState
          error={policies.error}
          retry={() => void policies.refetch()}
        />
      </>
    )
  }

  return (
    <>
      <PageHeader
        title="Policies"
        description={
          'Every check a release is measured against. This is what a vendor is asked to satisfy, '
          + 'so it says what each rule requires and why this organization requires it.'
        }
      />

      <Space direction="vertical" size={16} style={{ width: '100%' }}>
        {/*
          A broken pack first, and unmissable. Its checks will report as
          undecided on every release, and a reader looking at those has no way
          to learn why except from here.
        */}
        {broken.length > 0 && (
          <Alert
            type="error"
            showIcon
            message={`${broken.length} policy pack${broken.length === 1 ? '' : 's'} did not load`}
            description={
              <Space direction="vertical" size={8} style={{ width: '100%' }}>
                <span>
                  The checks these packs own will report as undecided on every release, and any
                  release checked while they are broken is INCONCLUSIVE rather than compliant.
                </span>
                {broken.map((p) => (
                  <div key={p.name}>
                    <strong style={{ fontFamily: mono }}>{p.name}</strong>
                    <ul style={{ margin: '4px 0 0 18px' }}>
                      {(p.errors ?? []).map((e, i) => (
                        <li key={i} style={{ fontFamily: mono, fontSize: 12 }}>{e}</li>
                      ))}
                    </ul>
                  </div>
                ))}
              </Space>
            }
          />
        )}

        <Card
          size="small"
          loading={policies.isLoading}
          title={
            <Space size={12} wrap>
              <span>{checks.length.toLocaleString()} checks</span>
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                from {packs.length} pack{packs.length === 1 ? '' : 's'}
                {policies.data?.bundleDigest && (
                  <Tooltip title={policies.data.bundleDigest}>
                    <span>
                      {' · rulebook '}
                      <code style={{ fontFamily: mono }}>
                        {policies.data.bundleDigest.replace(/^sha256:/, '').slice(0, 12)}
                      </code>
                    </span>
                  </Tooltip>
                )}
              </Typography.Text>
            </Space>
          }
          extra={
            <Space size={8} wrap>
              <Select
                allowClear
                size="small"
                placeholder="Any category"
                style={{ minWidth: 200 }}
                value={category}
                onChange={setCategory}
                options={categories.map((v) => ({ label: v, value: v }))}
              />
              <Select
                allowClear
                size="small"
                placeholder="Any severity"
                style={{ minWidth: 150 }}
                value={severity}
                onChange={setSeverity}
                options={[
                  { label: 'Blocking', value: 'block' },
                  { label: 'Warning', value: 'warn' },
                  { label: 'Info', value: 'info' },
                ]}
              />
              <Input.Search
                allowClear
                size="small"
                placeholder="Search the rulebook"
                style={{ width: 240 }}
                onSearch={setSearch}
              />
            </Space>
          }
        >
          <DataTable<PolicyCheck>
            tableEnhancedKey="policy-catalogue"
            size="small"
            rowKey="id"
            dataSource={filtered}
            pagination={{ pageSize: 50, showSizeChanger: true, size: 'small' }}
            expandable={{
              /*
                The rationale is in the expansion rather than the row, because
                it is a paragraph and a table of paragraphs is a table nobody
                scans. It is the thing a reviewer opens the page for, so it is
                one click and not a second page.
              */
              expandedRowRender: (ch) => <CheckDetail check={ch} />,
            }}
            columns={[
              {
                title: 'ID', dataIndex: 'id', width: 110,
                render: (id: string) => (
                  <span style={{ fontFamily: mono, fontSize: 12 }}>{id}</span>
                ),
              },
              {
                title: '', dataIndex: 'severity', width: 110,
                render: (s: string) => <CheckSeverityTag severity={s} />,
              },
              { title: 'Category', dataIndex: 'category', width: 200 },
              {
                title: 'What it requires', dataIndex: 'title',
                render: (_: unknown, ch: PolicyCheck) => (
                  <Space direction="vertical" size={2}>
                    <span>{ch.title}</span>
                    {ch.appliesTo && (
                      <span style={{ fontSize: 11, color: c.text3 }}>applies to {ch.appliesTo}</span>
                    )}
                  </Space>
                ),
              },
            ]}
          />
        </Card>

        <PackList packs={packs} />
      </Space>
    </>
  )
}

/** One check, in the words a vendor and a reviewer each need. */
function CheckDetail({ check }: { check: PolicyCheck }) {
  return (
    <Space direction="vertical" size={12} style={{ width: '100%', padding: '4px 0 8px' }}>
      {check.description && (
        <div>
          <Typography.Text strong style={{ fontSize: 12 }}>What it asserts</Typography.Text>
          <Typography.Paragraph style={{ marginBottom: 0 }}>{check.description}</Typography.Paragraph>
        </div>
      )}
      {/*
        WHY. This is what stops a check being carried forward after the reason
        for it is gone, and it is the field a vendor argues with instead of
        arguing with the tool.
      */}
      {check.rationale && (
        <div>
          <Typography.Text strong style={{ fontSize: 12 }}>Why we require it</Typography.Text>
          <Typography.Paragraph style={{ marginBottom: 0 }}>{check.rationale}</Typography.Paragraph>
        </div>
      )}
      {check.remediation && (
        <div>
          <Typography.Text strong style={{ fontSize: 12 }}>How to satisfy it</Typography.Text>
          <Typography.Paragraph style={{ marginBottom: 0 }}>{check.remediation}</Typography.Paragraph>
        </div>
      )}
      <Space size={16} wrap style={{ fontSize: 12, color: c.text3 }}>
        {check.pack && <span>pack <code style={{ fontFamily: mono }}>{check.pack}</code></span>}
        {check.tier ? <span>tier {check.tier}</span> : null}
        {check.engine && <span>{check.engine}</span>}
        {check.deprecated && (
          <span style={{ color: c.review }}>
            retired{check.supersededBy ? ` — superseded by ${check.supersededBy}` : ''}
          </span>
        )}
      </Space>
      {check.reference && (
        <Typography.Link href={check.reference} target="_blank" rel="noreferrer">
          The standard this comes from
        </Typography.Link>
      )}
    </Space>
  )
}

/** Where the checks came from, and who maintains each set. */
function PackList({ packs }: { packs: NonNullable<ReturnType<typeof usePolicies>['data']>['packs'] }) {
  if (packs.length === 0) return null
  return (
    <Collapse
      size="small"
      items={[{
        key: 'packs',
        label: `Where these came from (${packs.length} pack${packs.length === 1 ? '' : 's'})`,
        children: (
          <DataTable
            tableEnhancedKey="policy-packs"
            size="small"
            rowKey="name"
            dataSource={packs}
            pagination={false}
            columns={[
              {
                title: 'Pack', dataIndex: 'name', width: 240,
                render: (name: string) => (
                  <span style={{ fontFamily: mono, fontSize: 12 }}>{name}</span>
                ),
              },
              { title: 'Version', dataIndex: 'version', width: 100 },
              {
                title: 'Owns', dataIndex: 'prefixes', width: 150,
                render: (p?: string[]) => (
                  <span style={{ fontFamily: mono, fontSize: 12 }}>{(p ?? []).join(', ')}</span>
                ),
              },
              {
                title: 'Checks', dataIndex: 'checks', width: 90, align: 'right' as const,
                render: (n: number) => n.toLocaleString(),
              },
              { title: 'Maintainer', dataIndex: 'maintainer', width: 180 },
              { title: 'What it covers', dataIndex: 'description' },
            ]}
          />
        ),
      }]}
    />
  )
}
