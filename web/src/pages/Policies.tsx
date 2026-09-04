import { useEffect, useMemo, useState } from 'react'
import { Alert, Button, Card, Input, Select, Space, Tabs, Tooltip, Typography } from 'antd'
// The working-surface table: resizable, reorderable, pinnable columns whose
// layout each person keeps. See `tablekit/README.md` for which tables get it.
import { Table as DataTable } from '../tablekit'
import {
  ApiOutlined, BarChartOutlined, ClusterOutlined, CompareOutlined,
  CopyOutlined, HddOutlined, PackageOutlined, SearchOutlined,
  ScaleOutlined, SafetyOutlined, SettingOutlined,
  BookOutlined,
} from '../icons'
import { usePolicies } from '../api/queries'
import { ErrorState, PageHeader } from '../components/layout'
import { CheckSeverityTag } from '../components/compliance'
import { c, mono } from '../uikit'
import type { PolicyCatalogueResponse, PolicyCheck } from '../api/types'

const categoryInfo: Record<string, { meaning: string; Icon: typeof ScaleOutlined }> = {
  'Configuration & Secrets': { meaning: 'Configuration and secret handling', Icon: SettingOutlined },
  'Identity & Access': { meaning: 'Identity and access control', Icon: SafetyOutlined },
  'Metadata': { meaning: 'Labels and annotations', Icon: PackageOutlined },
  'Networking': { meaning: 'Network reachability and exposure', Icon: ApiOutlined },
  'Observability': { meaning: 'Monitoring and failure visibility', Icon: BarChartOutlined },
  'Probes': { meaning: 'Health checks and lifecycle', Icon: SafetyOutlined },
  'RBAC': { meaning: 'Role-based access control', Icon: SafetyOutlined },
  'Resources': { meaning: 'Resource requests and limits', Icon: ClusterOutlined },
  'Scheduling & Placement': { meaning: 'Scheduling and workload placement', Icon: CompareOutlined },
  'Security': { meaning: 'Workload security posture', Icon: SafetyOutlined },
  'Storage': { meaning: 'Persistent storage and data', Icon: HddOutlined },
  'Supply Chain': { meaning: 'Artifact provenance and integrity', Icon: PackageOutlined },
  'Upgrade': { meaning: 'Upgrade and rollout readiness', Icon: ScaleOutlined },
}

function CategoryLabel({ category }: { category?: string }) {
  if (!category) return null
  const info = categoryInfo[category]
  const Icon = info?.Icon ?? ScaleOutlined
  return (
    <Tooltip title={info?.meaning ?? 'Policy category'}>
      <Space size={6}>
        <Icon style={{ color: c.brand }} />
        <span>{category}</span>
      </Space>
    </Tooltip>
  )
}

const prefixCategories: Record<string, string> = {
  CFG: 'Configuration & Secrets', MTA: 'Metadata', NET: 'Networking', OBS: 'Observability',
  PRB: 'Probes', RBAC: 'RBAC', RES: 'Resources', SCH: 'Scheduling & Placement',
  SEC: 'Security', STO: 'Storage', SUP: 'Supply Chain', UPG: 'Upgrade',
}

function SourceCategories({ prefixes }: { prefixes?: string[] }) {
  return (
    <Space size={[8, 4]} wrap>
      {(prefixes ?? []).map((prefix) => (
        <CategoryLabel key={prefix} category={prefixCategories[prefix] ?? prefix} />
      ))}
    </Space>
  )
}

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
  // `draft` is what the box shows; `search` is what filters. Split so typing
  // is not a re-filter of the whole rulebook per keystroke.
  const [draft, setDraft] = useState('')
  const [search, setSearch] = useState('')
  const [category, setCategory] = useState<string | undefined>()
  const [severity, setSeverity] = useState<string | undefined>()
  const [activeTab, setActiveTab] = useState('policies')
  const [sourceToOpen, setSourceToOpen] = useState<string | undefined>()
  const [expandedSources, setExpandedSources] = useState<string[]>([])

  const checks = policies.data?.checks ?? []
  const packs = policies.data?.packs ?? []
  const broken = packs.filter((p) => (p.errors?.length ?? 0) > 0)

  useEffect(() => {
    const t = setTimeout(() => setSearch(draft), 200)
    return () => clearTimeout(t)
  }, [draft])

  const categories = useMemo(() => {
    const seen = new Set<string>()
    for (const ch of checks) if (ch.category) seen.add(ch.category)
    return [...seen].sort()
  }, [checks])

  const filtered = useMemo(() => {
    const q = search.trim().toLowerCase()
    return checks.filter((check) => {
      if (category && check.category !== category) return false
      if (severity && check.severity !== severity) return false
      if (!q) return true
      return [check.id, check.title, check.description, check.rationale, check.category,
        // The technical vocabulary. The title deliberately does not contain it,
        // so without these an engineer searching `toleration` or `RWX` finds
        // nothing at all.
        check.subcategory, ...(check.keywords ?? []), check.pack]
        .some((v) => v?.toLowerCase().includes(q))
    })
  }, [checks, search, category, severity])

  const sortedFiltered = useMemo(() => sortBySeverity(filtered), [filtered])
  const filteredPacks = useMemo(() => {
    const q = search.trim().toLowerCase()
    return packs.filter((pack) => {
      const ownedChecks = checks.filter((check) => check.pack === pack.name)
      if (category && !ownedChecks.some((check) => check.category === category)) return false
      if (severity && !ownedChecks.some((check) => check.severity === severity)) return false
      if (!q) return true
      return [pack.name, pack.version, pack.description, pack.maintainer, ...(pack.prefixes ?? [])]
        .some((value) => value?.toLowerCase().includes(q))
        || ownedChecks.some((check) => [check.id, check.title, check.description, check.rationale,
          check.subcategory, ...(check.keywords ?? [])]
          .some((value) => value?.toLowerCase().includes(q)))
    })
  }, [packs, checks, search, category, severity])

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


      <Space direction="vertical" size={0} style={{ width: '100%' }}>
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

        <Tabs
          activeKey={activeTab}
          onChange={setActiveTab}
          items={[
            {
              key: 'policies',
              label: 'Policies',
              icon: <ScaleOutlined />,
            },
            {
              key: 'sources',
              label: 'Sources',
              icon: <BookOutlined />,
            },
          ]}
        />

        <FilterBar
          categories={categories}
          category={category}
          draft={draft}
          activeTab={activeTab}
          onCategoryChange={setCategory}
          onDraftChange={setDraft}
          onSeverityChange={setSeverity}
          severity={severity}
        />

        <Card
          className="slm-policies-card"
          size="small"
          loading={policies.isLoading}
        >
          {activeTab === 'policies' ? (
            <PolicyTable
              filtered={sortedFiltered}
              onSourceClick={(pack) => {
                setSourceToOpen(pack)
                setActiveTab('sources')
              }}
            />
          ) : (
            <PackTable
              packs={filteredPacks}
                  checks={checks}
              search={search}
              category={category}
              severity={severity}
                  expandedSources={expandedSources}
                  onExpandedSourcesChange={(keys) => {
                    setExpandedSources(keys)
                    setSourceToOpen(undefined)
                  }}
                  sourceToOpen={sourceToOpen}
            />
          )}
        </Card>
      </Space>
    </>
  )
}

  const severityOrder: Record<PolicyCheck['severity'], number> = { block: 0, warn: 1, info: 2 }

  function sortBySeverity(checks: PolicyCheck[]) {
    return [...checks].sort((left, right) => severityOrder[left.severity] - severityOrder[right.severity])
  }

  function FilterBar({
    categories,
    category,
    draft,
    activeTab,
    severity,
    onCategoryChange,
    onDraftChange,
    onSeverityChange,
  }: {
    categories: string[]
    category: string | undefined
    draft: string
    activeTab: string
    severity: string | undefined
    onCategoryChange: (value: string | undefined) => void
    onDraftChange: (value: string) => void
    onSeverityChange: (value: string | undefined) => void
  }) {
    return (
      <Space className="slm-policies-toolbar" size={10} wrap>
        <Input
          allowClear
          style={{ width: 280 }}
          prefix={<SearchOutlined style={{ color: c.text3 }} />}
          placeholder={activeTab === 'policies' ? 'Search the rulebook' : 'Search sources'}
          value={draft}
          onChange={(event) => onDraftChange(event.target.value)}
        />
        <Select
          allowClear
          placeholder="Any category"
          style={{ minWidth: 200 }}
          value={category}
          onChange={onCategoryChange}
          showSearch
          optionFilterProp="label"
          options={categories.map((value) => ({
            label: <CategoryLabel category={value} />,
            value,
          }))}
        />
        <Select
          allowClear
          placeholder="Any severity"
          style={{ minWidth: 160 }}
          value={severity}
          onChange={onSeverityChange}
          options={[
            { label: 'Blocking', value: 'block' },
            { label: 'Warning', value: 'warn' },
            { label: 'Info', value: 'info' },
          ]}
        />
      </Space>
    )
  }

function PolicyTable({
  filtered,
  onSourceClick,
}: {
  filtered: PolicyCheck[]
  onSourceClick: (pack: string) => void
}) {
  return (
    <>
      <DataTable<PolicyCheck>
        tableEnhancedKey="policy-catalogue"
        size="middle"
        rowKey="id"
        dataSource={filtered}
        scroll={{ x: '100%' }}
        pagination={{ pageSize: 50, showSizeChanger: true, size: 'small' }}
        expandable={{ expandedRowRender: (check) => <CheckDetail check={check} onSourceClick={onSourceClick} /> }}
        columns={[
          {
            title: 'ID', dataIndex: 'id', width: 110,
            render: (id: string) => <span style={{ fontFamily: mono, fontSize: 12 }}>{id}</span>,
          },
          {
            title: '', dataIndex: 'severity', width: 110,
            render: (value: string) => <CheckSeverityTag severity={value} />,
          },
          {
            title: 'Category', dataIndex: 'category', width: 220,
            render: (_: unknown, check: PolicyCheck) => (
              <Space direction="vertical" size={0}>
                <CategoryLabel category={check.category ?? ''} />
                {/*
                  The mechanism, under the section it belongs to. The section is
                  where the requirement came from; this is what the check is
                  about, and it is what an engineer is looking for.
                */}
                {check.subcategory && (
                  <span style={{ fontSize: 11, color: c.text3 }}>{check.subcategory}</span>
                )}
              </Space>
            ),
          },
          {
            title: 'What it requires', dataIndex: 'title',
            render: (_: unknown, check: PolicyCheck) => (
              <Space direction="vertical" size={2}>
                <span>{check.title}</span>
                {check.appliesTo && <span style={{ fontSize: 11, color: c.text3 }}>applies to {check.appliesTo}</span>}
              </Space>
            ),
          },
        ]}
      />
    </>
  )
}

/**
 * The triage vocabulary in the words a reader uses.
 *
 * The wire carries the enum so it can be filtered and grouped; the label is
 * what a person reads, and keeping the two apart is what stops a report saying
 * "chart-template" to somebody deciding whether to ship a release.
 */
const FIX_OWNER_LABEL: Record<string, string> = {
  'chart-template': "the chart's templates",
  'chart-values': 'the values file',
  'application': 'the application itself',
  'build-pipeline': 'the build pipeline',
  'platform-team': 'the platform team',
  'needs-decision': 'a decision, first',
}

const WHEN_LABEL: Record<string, string> = {
  'install': 'when the release is installed',
  'upgrade': 'on the next upgrade',
  'node-maintenance': 'when a server is taken out for maintenance',
  'under-load': 'under load',
  'on-failure': 'when something else has already failed',
  'continuously': 'the whole time the release is running',
}

/** One check, in the words a vendor and a reviewer each need. */
function CheckDetail({ check, onSourceClick }: { check: PolicyCheck; onSourceClick: (pack: string) => void }) {
  const [copied, setCopied] = useState(false)

  const copyPolicy = async () => {
    const policy = [
      `${check.id}: ${check.title}`,
      check.description && `What it asserts\n${check.description}`,
      check.rationale && `Why we require it\n${check.rationale}`,
      check.remediation && `How to satisfy it\n${check.remediation}`,
      check.fixExample && `Example\n${check.fixExample}`,
      check.subcategory && `Mechanism\n${check.subcategory}`,
      check.keywords?.length && `Search terms\n${check.keywords.join(', ')}`,
      check.appliesTo && `Applies to\n${check.appliesTo}`,
      check.fixOwner && `Who fixes it\n${FIX_OWNER_LABEL[check.fixOwner] ?? check.fixOwner}`,
      check.fixEffort && `Effort\n${check.fixEffort}`,
      check.whenItBites && `When it bites\n${WHEN_LABEL[check.whenItBites] ?? check.whenItBites}`,
      check.category && `Category\n${check.category}`,
      check.severity && `Severity\n${check.severity}`,
      check.pack && `Pack\n${check.pack}`,
      check.tier && `Tier\n${check.tier}`,
      check.engine && `Engine\n${check.engine}`,
      check.reference && `Reference\n${check.reference}`,
    ].filter(Boolean).join('\n\n')

    try {
      await navigator.clipboard.writeText(policy)
      setCopied(true)
      window.setTimeout(() => setCopied(false), 1600)
    } catch {
      setCopied(false)
    }
  }

  return (
    <div className="slm-policy-detail">
      <div className="slm-policy-detail-heading">
        <span className="slm-policy-detail-kicker">Policy check</span>
        <Typography.Text className="slm-policy-detail-id" style={{ fontFamily: mono }}>{check.id}</Typography.Text>
        <Button
          className="slm-policy-copy"
          size="small"
          type="text"
          icon={<CopyOutlined />}
          onClick={() => void copyPolicy()}
        >
          {copied ? 'Policy copied' : 'Copy policy'}
        </Button>
      </div>
      <div className="slm-policy-detail-grid">
      {check.description && (
        <div className="slm-policy-detail-section">
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
        <div className="slm-policy-detail-section">
          <Typography.Text strong style={{ fontSize: 12 }}>Why we require it</Typography.Text>
          <Typography.Paragraph style={{ marginBottom: 0 }}>{check.rationale}</Typography.Paragraph>
        </div>
      )}
      {check.remediation && (
        <div className="slm-policy-detail-section slm-policy-detail-remediation">
          <Typography.Text strong style={{ fontSize: 12 }}>How to satisfy it</Typography.Text>
          <Typography.Paragraph style={{ marginBottom: 0 }}>{check.remediation}</Typography.Paragraph>
        </div>
      )}
      {/*
        The corrected configuration, not a description of it. A vendor asked to
        satisfy a rule copies this; prose about a fix is what they have to
        translate first.
      */}
      {check.fixExample && (
        <div className="slm-policy-detail-section">
          <Typography.Text strong style={{ fontSize: 12 }}>Example</Typography.Text>
          <pre style={{
            fontFamily: mono, fontSize: 11, marginBottom: 0,
            whiteSpace: 'pre-wrap', overflowX: 'auto',
          }}>
            {check.fixExample}
          </pre>
        </div>
      )}
      </div>
      <Space className="slm-policy-detail-meta" size={16} wrap>
        {check.pack && (
          <span>
            source{' '}
            <Typography.Link onClick={() => onSourceClick(check.pack!)}>
              <code style={{ fontFamily: mono }}>{check.pack}</code>
            </Typography.Link>
          </span>
        )}
        {/*
          How to read a finding from this check. A severity says how much this
          organization cares; these say what somebody should do about it.
        */}
        {check.fixOwner && <span>fixed in {FIX_OWNER_LABEL[check.fixOwner] ?? check.fixOwner}</span>}
        {check.fixEffort && <span>{check.fixEffort} effort</span>}
        {check.whenItBites && <span>bites {WHEN_LABEL[check.whenItBites] ?? check.whenItBites}</span>}
        {check.confidence && check.confidence !== 'confirmed' && (
          <span style={{ color: c.review }}>
            {check.confidence === 'needs-review'
              ? 'needs someone who knows the workload'
              : 'likely, unless the platform provides it'}
          </span>
        )}
        {check.keywords && check.keywords.length > 0 && (
          <span style={{ fontFamily: mono, fontSize: 11, color: c.text3 }}>
            {check.keywords.join('  ')}
          </span>
        )}
        {check.tier ? <span>tier {check.tier}</span> : null}
        {check.engine && <span>{check.engine}</span>}
        {check.deprecated && (
          <span className="slm-policy-detail-retired" style={{ color: c.review }}>
            retired{check.supersededBy ? ` — superseded by ${check.supersededBy}` : ''}
          </span>
        )}
      </Space>
      {/*
        Usually a clause of the source standard rather than a URL. Rendered as a
        link only when it is one - a link that goes nowhere is worse than the
        text it replaced.
      */}
      {check.reference && (
        check.reference.startsWith('http')
          ? (
            <Typography.Link className="slm-policy-detail-reference" href={check.reference} target="_blank" rel="noreferrer">
              The standard this comes from
            </Typography.Link>
          )
          : (
            <Typography.Text className="slm-policy-detail-reference" type="secondary" style={{ fontSize: 12 }}>
              From the standard: {check.reference}
            </Typography.Text>
          )
      )}
    </div>
  )
}

/** Where the checks came from, and who maintains each set. */
function PackTable({
  packs,
  checks,
  search,
  category,
  severity,
  sourceToOpen,
  expandedSources,
  onExpandedSourcesChange,
}: {
  packs: PolicyCatalogueResponse['packs']
  checks: PolicyCheck[]
  search: string
  category: string | undefined
  severity: string | undefined
  sourceToOpen?: string
  expandedSources: string[]
  onExpandedSourcesChange: (keys: string[]) => void
}) {
  const openSources = sourceToOpen && !expandedSources.includes(sourceToOpen)
    ? [...expandedSources, sourceToOpen]
    : expandedSources

  return (
    <DataTable
      tableEnhancedKey="policy-packs"
      size="middle"
      rowKey="name"
      dataSource={packs}
      pagination={false}
      expandable={{
        expandedRowKeys: openSources,
        onExpandedRowsChange: (keys) => onExpandedSourcesChange(keys.map(String)),
        expandedRowRender: (pack) => (
          <DataTable<PolicyCheck>
            tableEnhancedKey={`policy-source-checks-${pack.name}`}
            size="small"
            rowKey="id"
            dataSource={sortBySeverity(checks.filter((check) => {
              if (check.pack !== pack.name) return false
              if (category && check.category !== category) return false
              if (severity && check.severity !== severity) return false
              const q = search.trim().toLowerCase()
              if (!q) return true
              return [check.id, check.title, check.description, check.rationale, check.category, check.pack]
                .some((value) => value?.toLowerCase().includes(q))
            }))}
            pagination={false}
            columns={[
              {
                title: 'ID', dataIndex: 'id', width: 110,
                render: (id: string) => <span style={{ fontFamily: mono, fontSize: 12 }}>{id}</span>,
              },
              { title: 'Severity', dataIndex: 'severity', width: 110, render: (value: string) => <CheckSeverityTag severity={value} /> },
              { title: 'Category', dataIndex: 'category', width: 200, render: (value: string) => <CategoryLabel category={value} /> },
              { title: 'What it requires', dataIndex: 'title', width: 300 },
              { title: 'Why we require it', dataIndex: 'rationale', width: 420 },
            ]}
          />
        ),
      }}
      columns={[
              {
                title: 'Source', dataIndex: 'name', width: 220,
                render: (name: string) => (
                  <Space size={7}> 
                    <span style={{ fontFamily: mono, fontSize: 12 }}>{name}</span>
                  </Space>
                ),
              },
              { title: 'Version', dataIndex: 'version', width: 90 },
              {
                title: 'Categories', dataIndex: 'prefixes', width: 260,
                render: (p?: string[]) => <SourceCategories prefixes={p} />,
              },
              {
                title: 'Checks', dataIndex: 'checks', width: 80, align: 'right' as const,
                render: (n: number) => n.toLocaleString(),
              },
              { title: 'Maintainer', dataIndex: 'maintainer', width: 140 },
              { title: 'What it covers', dataIndex: 'description', width: 360 },
      ]}
    />
  )
}
