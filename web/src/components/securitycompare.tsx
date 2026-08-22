import { useMemo, useState } from 'react'
import {
  Alert, Button, Card, Checkbox, Col, Input, Row, Segmented, Space, Table, Tag, Tooltip, Typography,
} from 'antd'
import { securityComparisonExportUrl } from '../api/queries'
import { SEVERITIES } from '../api/types'
import type {
  ArtifactChange, ChangeType, SecurityArtifactDelta, SecurityChange,
  SecurityComparisonEnd, SecurityComparisonResponse, Severity,
} from '../api/types'
import {
  ComponentCell, CveCell, FixCell, SecurityExportMenu, SeverityBar, SeverityTag, VerdictBanner,
} from './security'
import { mono, palette, semantic, severity as severityColour, verdict as verdictColour } from '../theme'
import { formatRelative } from '../domain/format'

/**
 * "How did the security posture change from release A to release B?"
 *
 * # The two levels, one above the other
 *
 * The banner is the whole answer for somebody who does not read advisories: a
 * word and a sentence. Everything below it is the evidence, on the same page
 * rather than behind a link, because the person who needs the evidence is
 * usually the person who just read the sentence and wants to check it.
 *
 * # Why the artifact delta gets its own table
 *
 * Two releases do not contain the same artifacts. A patch release is two images
 * against a base release's ten, and a comparison presenting only findings would
 * leave a reader unable to tell "this CVE arrived in a new image" from "this CVE
 * arrived in an image that was already there" - the difference between a new
 * dependency and a regression.
 */
export function SecurityComparison({ product, baseRef, againstRef, report, repository, onSync }: {
  product: string
  baseRef: string
  againstRef: string
  report: SecurityComparisonResponse
  repository?: string
  /** Offered on an end nobody has synced, because that is the fix. */
  onSync?: (end: 'a' | 'b') => void
}) {
  const exportMenu = (
    <SecurityExportMenu
      urlFor={(format, view) => securityComparisonExportUrl(product, baseRef, againstRef, {
        format, view, repository,
      })}
    />
  )

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <Row gutter={[16, 16]}>
        <Col xs={24} lg={11}>
          <ReleaseCard title="Base release (old)" end={report.a} onSync={onSync && (() => onSync('a'))} />
        </Col>
        <Col xs={24} lg={2} style={{ display: 'flex', alignItems: 'center', justifyContent: 'center' }}>
          <span aria-hidden style={{ fontSize: 20, color: '#98A2B3' }}>→</span>
        </Col>
        <Col xs={24} lg={11}>
          <ReleaseCard title="Target release (new)" end={report.b} onSync={onSync && (() => onSync('b'))} />
        </Col>
      </Row>

      <VerdictBanner
        verdict={report.verdict}
        headline={report.headline}
        explanation={report.explanation}
        caveats={report.caveats}
        extra={exportMenu}
      />

      <Row gutter={[16, 16]}>
        <Col xs={24} xl={17}>
          <Space direction="vertical" size={16} style={{ width: '100%' }}>
            <VulnerabilityOverview report={report} />
            <ChangeBySeverity report={report} />
            <ArtifactDeltaCard report={report} />
          </Space>
        </Col>
        <Col xs={24} xl={7}>
          <Space direction="vertical" size={16} style={{ width: '100%' }}>
            <SummaryCard report={report} />
            <TopCves title="Top introduced" changes={report.changes} types={['introduced', 'severity_increased']} tone="worse" />
            <TopCves title="Top resolved" changes={report.changes} types={['resolved', 'severity_decreased']} tone="better" />
          </Space>
        </Col>
      </Row>

      {report.removedArtifact.total > 0 && (
        <Alert
          type="info"
          showIcon
          message={`${report.removedArtifact.total} findings left with artifacts that are no longer shipped`}
          description={
            'Shown separately rather than counted as fixed. The scan data does not confirm that dropping ' +
            'the artifact improved the release, so calling them resolved would credit a fix nobody made.'
          }
        />
      )}

      <ChangeTable
        report={report}
        product={product}
        baseRef={baseRef}
        againstRef={againstRef}
        repository={repository}
      />
    </Space>
  )
}

/**
 * One end of the comparison, as a card.
 *
 * Carries its own sync state, because a comparison against a release nobody has
 * scanned is inconclusive and the useful thing to put on screen is the button
 * that changes that - not a verdict repeating that it cannot say.
 */
function ReleaseCard({ title, end, onSync }: {
  title: string
  end: SecurityComparisonEnd
  onSync?: () => void
}) {
  const synced = end.sync.state === 'synced'
  return (
    <Card size="small" style={{ height: '100%' }}>
      <Space direction="vertical" size={8} style={{ width: '100%' }}>
        <Typography.Text type="secondary" style={{ fontSize: 11, letterSpacing: 0.4, textTransform: 'uppercase' }}>
          {title}
        </Typography.Text>
        <Typography.Text strong style={{ fontFamily: mono, fontSize: 16 }}>{end.label}</Typography.Text>

        {synced ? (
          <>
            <Space size={18} wrap>
              <Stat label="Vulnerabilities" value={end.counts.total} />
              <Stat label="Fixable" value={end.counts.fixable} />
              <Stat label="Scanned" value={end.coverage.scanned} suffix={`/ ${end.coverage.scannable}`} />
            </Space>
            <SeverityBar counts={end.counts} />
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              {end.sync.syncedAt && `synced ${formatRelative(end.sync.syncedAt)}`}
              {end.repository && ` · ${end.repository}`}
            </Typography.Text>
          </>
        ) : (
          <Space direction="vertical" size={8}>
            <Typography.Text type="secondary">
              {end.sync.canSync
                ? 'This release has not been scanned, so it cannot be compared.'
                : end.sync.reason}
            </Typography.Text>
            {end.sync.canSync && onSync && (
              <Button size="small" type="primary" onClick={onSync}>Sync vulnerabilities</Button>
            )}
          </Space>
        )}
      </Space>
    </Card>
  )
}

function Stat({ label, value, colour, suffix }: {
  label: string
  value: number
  colour?: string
  suffix?: string
}) {
  return (
    <Space direction="vertical" size={0}>
      <Typography.Text type="secondary" style={{ fontSize: 12, textTransform: 'capitalize' }}>{label}</Typography.Text>
      <Typography.Text strong style={{ fontSize: 18, color: colour }}>
        {value.toLocaleString()}
        {suffix && <Typography.Text type="secondary" style={{ fontSize: 12 }}> {suffix}</Typography.Text>}
      </Typography.Text>
    </Space>
  )
}

/**
 * The three sets, and what is in each.
 *
 * Unique to old, common, unique to new - which is the shape of the question. A
 * pair of totals cannot express it: two releases with 1,126 and 1,286 findings
 * may share all 1,126 or none of them, and those are opposite situations with
 * the same two numbers.
 */
function VulnerabilityOverview({ report }: { report: SecurityComparisonResponse }) {
  const uniqueToOld = report.resolved.total + report.removedArtifact.total
  const common = report.unchanged.total + report.severityIncreased.total
    + report.severityDecreased.total + report.remediationChanged.total
  const uniqueToNew = report.introduced.total

  return (
    <Card size="small" title="Vulnerability overview">
      <Row gutter={[16, 16]} align="middle">
        <Col xs={24} md={8}>
          <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'center', gap: 0 }}>
            <SetCircle label="Only in old" value={uniqueToOld} background="#EEF3FA" />
            <SetCircle label="In both" value={common} background="#E7EDF6" overlap />
            <SetCircle label="Only in new" value={uniqueToNew} background="#EAF3EE" overlap />
          </div>
        </Col>

        <Col xs={24} md={16}>
          <Row gutter={[12, 12]}>
            <Col xs={24} sm={8}>
              <DeltaTile
                label="Introduced"
                counts={report.introduced}
                colour={verdictColour.worse}
                symbol="↑"
              />
            </Col>
            <Col xs={24} sm={8}>
              <DeltaTile
                label="Resolved"
                counts={report.resolved}
                colour={verdictColour.better}
                symbol="✓"
              />
            </Col>
            <Col xs={24} sm={8}>
              <DeltaTile
                label="Unchanged"
                counts={report.unchanged}
                colour={semantic.neutral}
                symbol="−"
              />
            </Col>
          </Row>
        </Col>
      </Row>
    </Card>
  )
}

function SetCircle({ label, value, background, overlap }: {
  label: string
  value: number
  background: string
  overlap?: boolean
}) {
  return (
    <Tooltip title={label}>
      <div
        style={{
          width: 108, height: 108, borderRadius: '50%', background,
          border: `1px solid ${palette.border}`,
          display: 'flex', flexDirection: 'column', alignItems: 'center', justifyContent: 'center',
          marginLeft: overlap ? -22 : 0,
        }}
      >
        <span style={{ fontSize: 20, fontWeight: 600 }}>{value.toLocaleString()}</span>
        <span style={{ fontSize: 10, color: semantic.neutral, textAlign: 'center', padding: '0 8px' }}>
          {label}
        </span>
      </div>
    </Tooltip>
  )
}

/**
 * One of the three outcomes, with its severity split and its fixability split.
 *
 * Both splits, because they answer different questions: the severity says how
 * bad, and the fixability says how much of it anybody can act on this week.
 */
function DeltaTile({ label, counts, colour, symbol }: {
  label: string
  counts: SecurityComparisonResponse['introduced']
  colour: string
  symbol: string
}) {
  return (
    <div style={{ border: `1px solid ${palette.border}`, borderRadius: palette.borderRadius, padding: 12 }}>
      <Space size={6}>
        <span aria-hidden style={{ color: colour, fontSize: 14 }}>{symbol}</span>
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>{label}</Typography.Text>
      </Space>
      <div style={{ fontSize: 26, fontWeight: 600, color: colour, lineHeight: 1.3 }}>
        {counts.total.toLocaleString()}
      </div>
      <Space size={8} wrap>
        {SEVERITIES.filter((s) => counts.bySeverity[s] > 0).map((s) => (
          <Typography.Text key={s} style={{ fontSize: 12, color: severityColour[s] }}>
            {counts.bySeverity[s]} {s}
          </Typography.Text>
        ))}
      </Space>
      {counts.total > 0 && (
        <Typography.Text type="secondary" style={{ fontSize: 11, display: 'block', marginTop: 4 }}>
          {counts.fixable} fixable · {counts.nonFixable} not
        </Typography.Text>
      )}
    </div>
  )
}

/** One row of the severity grid, with the totals row sharing its shape. */
interface SeverityRow {
  key: string
  severity: Severity | 'total'
  newFixable: number
  newTotal: number
  resolvedFixable: number
  resolvedTotal: number
}

/**
 * The grid the requirement asks for: severity down, outcome across, fixability
 * inside each.
 *
 * Net change is signed and coloured, because "+66 medium" and "-66 medium" are
 * the two halves of the only question this table exists to answer, and a reader
 * scanning it should not have to subtract.
 */
function ChangeBySeverity({ report }: { report: SecurityComparisonResponse }) {
  const rows: SeverityRow[] = SEVERITIES
    .filter((s) => s !== 'unknown'
      || report.introduced.bySeverity.unknown > 0
      || report.resolved.bySeverity.unknown > 0)
    .map((s) => ({
      key: s,
      severity: s,
      newFixable: report.introduced.fixableBySeverity[s],
      newTotal: report.introduced.bySeverity[s],
      resolvedFixable: report.resolved.fixableBySeverity[s],
      resolvedTotal: report.resolved.bySeverity[s],
    }))

  // The totals row is part of the table rather than a footer, so it sorts,
  // exports and copies with the rest of it.
  const data: SeverityRow[] = [...rows, {
    key: 'total',
    severity: 'total',
    newFixable: report.introduced.fixable,
    newTotal: report.introduced.total,
    resolvedFixable: report.resolved.fixable,
    resolvedTotal: report.resolved.total,
  }]

  return (
    <Card size="small" title="Change by severity and fixability">
      <Table<SeverityRow>
        size="small"
        pagination={false}
        rowKey="key"
        dataSource={data}
        scroll={{ x: 'max-content' }}
        columns={[
          {
            title: 'Severity',
            width: 120,
            render: (_, r) => (r.severity === 'total'
              ? <Typography.Text strong>Total</Typography.Text>
              : <SeverityTag value={r.severity as Severity} />),
          },
          {
            title: 'Introduced',
            children: [
              { title: 'Fixable', width: 90, render: (_: unknown, r: SeverityRow) => r.newFixable },
              {
                title: 'Not fixable',
                width: 110,
                render: (_: unknown, r: SeverityRow) => r.newTotal - r.newFixable,
              },
              {
                title: 'Total',
                width: 90,
                render: (_: unknown, r: SeverityRow) => <strong>{r.newTotal}</strong>,
              },
            ],
          },
          {
            title: 'Resolved',
            children: [
              { title: 'Fixable', width: 90, render: (_: unknown, r: SeverityRow) => r.resolvedFixable },
              {
                title: 'Not fixable',
                width: 110,
                render: (_: unknown, r: SeverityRow) => r.resolvedTotal - r.resolvedFixable,
              },
              {
                title: 'Total',
                width: 90,
                render: (_: unknown, r: SeverityRow) => <strong>{r.resolvedTotal}</strong>,
              },
            ],
          },
          {
            title: 'Net change',
            width: 120,
            render: (_, r) => <NetChange value={r.newTotal - r.resolvedTotal} />,
          },
        ]}
      />
      <Typography.Text type="secondary" style={{ fontSize: 12 }}>
        A positive net change means more vulnerabilities in the new release.
      </Typography.Text>
    </Card>
  )
}

function NetChange({ value }: { value: number }) {
  if (value === 0) return <Typography.Text type="secondary">0</Typography.Text>
  const worse = value > 0
  return (
    <Typography.Text strong style={{ color: worse ? verdictColour.worse : verdictColour.better }}>
      {worse ? '+' : ''}{value}
    </Typography.Text>
  )
}

/** The numbers a reader quotes, in one column. */
function SummaryCard({ report }: { report: SecurityComparisonResponse }) {
  const rows: [string, string][] = [
    ['Base release', report.a.label],
    ['New release', report.b.label],
    ['Vulnerabilities before', report.a.counts.total.toLocaleString()],
    ['Vulnerabilities after', report.b.counts.total.toLocaleString()],
    ['Net change', `${report.b.counts.total - report.a.counts.total > 0 ? '+' : ''}${report.b.counts.total - report.a.counts.total}`],
    ['Artifacts unchanged', String(report.artifactSummary.common)],
    ['Artifacts upgraded', String(report.artifactSummary.upgraded)],
    ['Artifacts added', String(report.artifactSummary.added)],
    ['Artifacts removed', String(report.artifactSummary.removed)],
  ]
  return (
    <Card size="small" title="Summary">
      <Space direction="vertical" size={6} style={{ width: '100%' }}>
        {rows.map(([label, value]) => (
          <Space key={label} style={{ width: '100%', justifyContent: 'space-between' }}>
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>{label}</Typography.Text>
            <Typography.Text style={{ fontSize: 12 }}>{value}</Typography.Text>
          </Space>
        ))}
      </Space>
    </Card>
  )
}

/**
 * The handful of CVEs worth naming, ranked by how many artifacts carry them.
 *
 * A comparison of two large releases has hundreds of changes and nobody reads
 * a table of hundreds. These are the ones a release note would mention.
 */
function TopCves({ title, changes, types, tone }: {
  title: string
  changes: SecurityChange[]
  types: ChangeType[]
  tone: 'better' | 'worse'
}) {
  const top = useMemo(() => {
    const counts = new Map<string, { cve: string; severity: Severity; n: number }>()
    for (const c of changes) {
      if (!types.includes(c.type)) continue
      const id = c.cve || c.id || ''
      if (!id) continue
      const found = counts.get(id)
      if (found) found.n += 1
      else counts.set(id, { cve: id, severity: c.severity, n: 1 })
    }
    return [...counts.values()]
      .sort((a, b) => (SEVERITIES.indexOf(a.severity) - SEVERITIES.indexOf(b.severity)) || b.n - a.n)
      .slice(0, 5)
  }, [changes, types])

  if (top.length === 0) return null

  return (
    <Card size="small" title={title}>
      <Space direction="vertical" size={6} style={{ width: '100%' }}>
        {top.map((row) => (
          <Space key={row.cve} style={{ width: '100%', justifyContent: 'space-between' }}>
            <Space size={6}>
              <span
                aria-hidden
                style={{
                  width: 7, height: 7, borderRadius: '50%',
                  background: tone === 'worse' ? verdictColour.worse : verdictColour.better,
                  display: 'inline-block',
                }}
              />
              <Typography.Text style={{ fontFamily: mono, fontSize: 12 }}>{row.cve}</Typography.Text>
            </Space>
            <Tooltip title={`${row.n} affected artifacts`}>
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>{row.n}</Typography.Text>
            </Tooltip>
          </Space>
        ))}
      </Space>
    </Card>
  )
}

const ARTIFACT_CHANGE_LABEL: Record<ArtifactChange, string> = {
  common: 'Unchanged',
  upgraded: 'Upgraded',
  added: 'Added',
  removed: 'Removed',
}

/**
 * What happened to the artifacts themselves.
 *
 * The four counts first, because "eight of these ten images did not change" is
 * the fact that makes a two-image diff legible.
 */
function ArtifactDeltaCard({ report }: { report: SecurityComparisonResponse }) {
  const [only, setOnly] = useState<'all' | ArtifactChange>('all')
  const rows = useMemo(
    () => report.artifacts.filter((a) => only === 'all' || a.change === only),
    [report.artifacts, only],
  )

  const s = report.artifactSummary
  return (
    <Card
      size="small"
      title="Artifacts"
      extra={
        <Segmented
          size="small"
          value={only}
          onChange={(v) => setOnly(v as typeof only)}
          options={[
            { value: 'all', label: `All (${report.artifacts.length})` },
            { value: 'common', label: `Unchanged (${s.common})` },
            { value: 'upgraded', label: `Upgraded (${s.upgraded})` },
            { value: 'added', label: `Added (${s.added})` },
            { value: 'removed', label: `Removed (${s.removed})` },
          ]}
        />
      }
    >
      {s.notComparable > 0 && (
        <Alert
          type="warning"
          showIcon
          style={{ marginBottom: 12 }}
          message={`${s.notComparable} artifacts could not be compared`}
          description="They are present in both releases, but one side has no scan result, so nothing about them was classified."
        />
      )}
      <Table<SecurityArtifactDelta>
        size="small"
        rowKey="key"
        dataSource={rows}
        scroll={{ x: 'max-content' }}
        pagination={{ pageSize: 10, size: 'small', showSizeChanger: true }}
        columns={[
          {
            title: 'Artifact',
            render: (_, r) => <Typography.Text style={{ fontFamily: mono }}>{r.key}</Typography.Text>,
          },
          { title: 'Change', width: 120, render: (_, r) => <Tag>{ARTIFACT_CHANGE_LABEL[r.change]}</Tag> },
          {
            title: 'Before',
            width: 90,
            render: (_, r) => (r.a ? r.countsA.total.toLocaleString() : <Typography.Text type="secondary">-</Typography.Text>),
          },
          {
            title: 'After',
            width: 90,
            render: (_, r) => (r.b ? r.countsB.total.toLocaleString() : <Typography.Text type="secondary">-</Typography.Text>),
          },
          {
            title: 'Resolved',
            width: 100,
            render: (_, r) => <span style={{ color: r.resolved ? semantic.success : undefined }}>{r.resolved}</span>,
          },
          {
            title: 'Introduced',
            width: 110,
            render: (_, r) => <span style={{ color: r.introduced ? semantic.error : undefined }}>{r.introduced}</span>,
          },
          { title: 'Severity changed', width: 140, render: (_, r) => r.severityChanged },
          {
            title: '',
            width: 150,
            render: (_, r) => (
              r.comparable ? null : (
                <Tooltip title="One side of this artifact has no scan result, so the columns above are not a comparison.">
                  <Typography.Text type="secondary">not comparable</Typography.Text>
                </Tooltip>
              )
            ),
          },
        ]}
      />
    </Card>
  )
}

const CHANGE_LABEL: Record<ChangeType, string> = {
  introduced: 'New',
  resolved: 'Resolved',
  unchanged: 'Unchanged',
  severity_increased: 'More severe',
  severity_decreased: 'Less severe',
  remediation_changed: 'Fix availability changed',
  removed_artifact: 'On a removed artifact',
}

const CHANGE_COLOUR: Record<ChangeType, string | undefined> = {
  introduced: 'error',
  resolved: 'success',
  severity_increased: 'error',
  severity_decreased: 'success',
  remediation_changed: undefined,
  removed_artifact: 'warning',
  unchanged: undefined,
}

type ChangeTab = 'all' | 'new' | 'resolved' | 'unchanged'

const TAB_TYPES: Record<ChangeTab, ChangeType[]> = {
  all: ['introduced', 'resolved', 'severity_increased', 'severity_decreased', 'remediation_changed', 'removed_artifact'],
  new: ['introduced', 'severity_increased'],
  resolved: ['resolved', 'severity_decreased'],
  unchanged: ['unchanged', 'remediation_changed'],
}

/**
 * Every classified finding, in four tabs.
 *
 * "All changes" deliberately excludes the unchanged ones. A comparison of two
 * large releases is mostly unchanged - hundreds of rows saying nothing happened
 * - and putting them in the default view would bury the twelve rows the
 * comparison was run for. They have a tab of their own, because "what carried
 * over" is a real question.
 */
function ChangeTable({ report, product, baseRef, againstRef, repository }: {
  report: SecurityComparisonResponse
  product: string
  baseRef: string
  againstRef: string
  repository?: string
}) {
  const [tab, setTab] = useState<ChangeTab>('all')
  const [severities, setSeverities] = useState<Severity[]>([])
  const [fixability, setFixability] = useState<'all' | 'fixable' | 'non-fixable'>('all')
  const [q, setQ] = useState('')

  const counts = useMemo(() => {
    const out = {} as Record<ChangeTab, number>
    for (const key of Object.keys(TAB_TYPES) as ChangeTab[]) {
      out[key] = report.changes.filter((c) => TAB_TYPES[key].includes(c.type)).length
    }
    return out
  }, [report.changes])

  const rows = useMemo(() => report.changes.filter((c) => {
    if (!TAB_TYPES[tab].includes(c.type)) return false
    if (severities.length > 0 && !severities.includes(c.severity)) return false
    if (fixability === 'fixable' && !c.fixable) return false
    if (fixability === 'non-fixable' && c.fixable) return false
    if (q) {
      const hay = `${c.cve ?? ''} ${c.id ?? ''} ${c.component.name} ${c.artifact.name} ${c.summary ?? ''}`.toLowerCase()
      if (!hay.includes(q.toLowerCase())) return false
    }
    return true
  }), [report.changes, tab, severities, fixability, q])

  return (
    <Card
      size="small"
      title={`Changes (${rows.length.toLocaleString()})`}
      extra={
        <SecurityExportMenu
          urlFor={(format, view) => securityComparisonExportUrl(product, baseRef, againstRef, {
            format, view, repository,
            change: TAB_TYPES[tab].join(','),
            severity: severities.length > 0 ? severities.join(',') : undefined,
            fixable: fixability === 'all' ? undefined : fixability === 'fixable',
            q: q || undefined,
          })}
        />
      }
    >
      <Space direction="vertical" size={10} style={{ width: '100%', marginBottom: 12 }}>
        <Space wrap size={12} style={{ width: '100%', justifyContent: 'space-between' }}>
          <Segmented
            value={tab}
            onChange={(v) => setTab(v as ChangeTab)}
            options={[
              { value: 'all', label: `All changes (${counts.all})` },
              { value: 'new', label: `New (${counts.new})` },
              { value: 'resolved', label: `Resolved (${counts.resolved})` },
              { value: 'unchanged', label: `Unchanged (${counts.unchanged})` },
            ]}
          />
          <Space size={8}>
            <Segmented
              value={fixability}
              onChange={(v) => setFixability(v as typeof fixability)}
              options={[
                { value: 'all', label: 'All' },
                { value: 'fixable', label: 'Fixable' },
                { value: 'non-fixable', label: 'No fix' },
              ]}
            />
            <Input.Search
              allowClear
              placeholder="CVE, package or image"
              value={q}
              onChange={(e) => setQ(e.target.value)}
              style={{ width: 220 }}
            />
          </Space>
        </Space>
        <Checkbox.Group
          value={severities}
          onChange={(v) => setSeverities(v as Severity[])}
          options={SEVERITIES.map((s) => ({ label: <SeverityTag value={s} />, value: s }))}
        />
      </Space>

      <Table<SecurityChange>
        size="small"
        rowKey={(r) => `${r.type}-${r.cve ?? r.id}-${r.component.id}-${r.artifact.name}`}
        dataSource={rows}
        scroll={{ x: 'max-content' }}
        pagination={{ pageSize: 25, showSizeChanger: true, size: 'small' }}
        columns={[
          { title: 'CVE', width: 160, render: (_, r) => <CveCell cve={r.cve} id={r.id} /> },
          {
            title: 'Change',
            width: 160,
            render: (_, r) => (
              <Space direction="vertical" size={0}>
                <Tag color={CHANGE_COLOUR[r.type]}>{CHANGE_LABEL[r.type]}</Tag>
                {r.viaRemoval && (
                  <Typography.Text type="secondary" style={{ fontSize: 11 }}>no longer shipped</Typography.Text>
                )}
              </Space>
            ),
          },
          {
            title: 'Severity',
            width: 170,
            render: (_, r) => (
              r.fromSeverity && r.toSeverity && r.fromSeverity !== r.toSeverity ? (
                <Space size={6}>
                  <SeverityTag value={r.fromSeverity} />
                  <span aria-hidden>→</span>
                  <SeverityTag value={r.toSeverity} />
                </Space>
              ) : <SeverityTag value={r.severity} />
            ),
          },
          {
            title: 'Package',
            width: 180,
            render: (_, r) => (
              <ComponentCell name={r.component.name} version={r.component.version} type={r.component.type} />
            ),
          },
          {
            title: 'Image',
            width: 200,
            render: (_, r) => (
              <Space direction="vertical" size={0}>
                <Typography.Text style={{ fontFamily: mono }}>{r.artifact.name}</Typography.Text>
                <Typography.Text type="secondary" style={{ fontSize: 11 }}>
                  {ARTIFACT_CHANGE_LABEL[r.artifactChange].toLowerCase()} artifact
                </Typography.Text>
              </Space>
            ),
          },
          { title: 'Fix', width: 140, render: (_, r) => <FixCell fixable={r.fixable} fixedIn={r.fixedIn} /> },
          {
            title: 'Description',
            render: (_, r) => (
              <Typography.Paragraph
                style={{ margin: 0, maxWidth: 460 }}
                ellipsis={{ rows: 2, tooltip: r.description || r.summary }}
              >
                {r.summary || r.description || '-'}
              </Typography.Paragraph>
            ),
          },
        ]}
      />
    </Card>
  )
}
