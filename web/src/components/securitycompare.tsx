import { useMemo, useState } from 'react'
import {
  Alert, Card, Checkbox, Col, Input, Row, Segmented, Space, Table, Tag, Tooltip, Typography,
} from 'antd'
import { securityComparisonExportUrl } from '../api/queries'
import { SEVERITIES } from '../api/types'
import type {
  ArtifactChange, ChangeType, SecurityArtifactDelta, SecurityChange,
  SecurityComparisonResponse, Severity,
} from '../api/types'
import {
  ComparisonTiles, ComponentCell, CoverageMeter, CveCell, FixCell,
  SecurityExportMenu, SeverityTag, VerdictBanner,
} from './security'
import { mono, semantic, severity as severityColour } from '../theme'

/**
 * "How did the security posture change from release A to release B?"
 *
 * # The two levels, one above the other
 *
 * The banner and the tiles are the whole answer for somebody who does not read
 * advisories: a word, a sentence, and five numbers. Everything below them is
 * the evidence, and it is on the same page rather than behind a link because
 * the person who needs the evidence is usually the person who just read the
 * sentence and wants to check it.
 *
 * # Why the artifact delta gets its own table
 *
 * Two releases do not contain the same artifacts. A patch release is two images
 * against a base release's ten, and a comparison that presented only findings
 * would leave a reader unable to tell "this CVE arrived in a new image" from
 * "this CVE arrived in an image that was already there" - which is the
 * difference between a new dependency and a regression.
 */
export function SecurityComparison({ product, baseRef, againstRef, report, repository }: {
  product: string
  baseRef: string
  againstRef: string
  report: SecurityComparisonResponse
  repository?: string
}) {
  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <VerdictBanner
        verdict={report.verdict}
        headline={report.headline}
        explanation={report.explanation}
        caveats={report.caveats}
        extra={
          <SecurityExportMenu
            urlFor={(format, view) => securityComparisonExportUrl(product, baseRef, againstRef, {
              format, view, repository,
            })}
          />
        }
      />

      <ComparisonTiles
        resolved={report.resolved}
        introduced={report.introduced}
        moreSevere={report.severityIncreased.total}
        lessSevere={report.severityDecreased.total}
        unchanged={report.unchanged.total}
      />

      {report.removedArtifact.total > 0 && (
        <Alert
          type="info"
          showIcon
          message={`${report.removedArtifact.total} findings left with artifacts that are no longer shipped`}
          description={
            'These are shown separately rather than counted as fixed. The scan data does not confirm ' +
            'that dropping the artifact improved the release, so calling them resolved would credit a fix nobody made.'
          }
        />
      )}

      <Row gutter={[16, 16]}>
        <Col xs={24} lg={12}><EndCard title="Base release" end={report.a} /></Col>
        <Col xs={24} lg={12}><EndCard title="New release" end={report.b} /></Col>
      </Row>

      <ArtifactDeltaCard report={report} />

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

function EndCard({ title, end }: { title: string; end: SecurityComparisonResponse['a'] }) {
  return (
    <Card size="small" title={<Space size={8}><span>{title}</span>
      <Typography.Text style={{ fontFamily: mono }}>{end.label}</Typography.Text></Space>}>
      <Space direction="vertical" size={10} style={{ width: '100%' }}>
        <Space size={18} wrap>
          <Stat label="Vulnerabilities" value={end.counts.total} />
          <Stat label="Fixable" value={end.counts.fixable} />
          {SEVERITIES.slice(0, 2).map((s) => (
            <Stat key={s} label={s} value={end.counts.bySeverity[s]} colour={severityColour[s]} />
          ))}
        </Space>
        <CoverageMeter coverage={end.coverage} />
        {!end.enabled && (
          <Typography.Text type="secondary">
            Security scanning is switched off for this release's repository.
          </Typography.Text>
        )}
      </Space>
    </Card>
  )
}

function Stat({ label, value, colour }: { label: string; value: number; colour?: string }) {
  return (
    <Space direction="vertical" size={0}>
      <Typography.Text type="secondary" style={{ fontSize: 12, textTransform: 'capitalize' }}>{label}</Typography.Text>
      <Typography.Text strong style={{ fontSize: 18, color: colour }}>{value.toLocaleString()}</Typography.Text>
    </Space>
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
 * the fact that makes a two-image diff legible. The table under them names
 * each one and what its own arithmetic was.
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
          {
            title: 'Change',
            width: 120,
            render: (_, r) => <Tag>{ARTIFACT_CHANGE_LABEL[r.change]}</Tag>,
          },
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
          {
            title: 'Severity changed',
            width: 140,
            render: (_, r) => r.severityChanged,
          },
          {
            title: '',
            width: 160,
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

const CHANGE_ORDER: ChangeType[] = [
  'severity_increased', 'introduced', 'removed_artifact',
  'resolved', 'severity_decreased', 'remediation_changed', 'unchanged',
]

const CHANGE_COLOUR: Record<ChangeType, string | undefined> = {
  introduced: 'error',
  resolved: 'success',
  severity_increased: 'error',
  severity_decreased: 'success',
  remediation_changed: undefined,
  removed_artifact: 'warning',
  unchanged: undefined,
}

/**
 * Every classified finding, filterable.
 *
 * Unchanged findings are excluded by DEFAULT rather than absent. A comparison
 * of two large releases is mostly unchanged - hundreds of rows that say
 * "nothing happened" - and putting them first would bury the twelve rows the
 * comparison was run for. They stay one click away, because "what carried
 * over" is a real question.
 */
function ChangeTable({ report, product, baseRef, againstRef, repository }: {
  report: SecurityComparisonResponse
  product: string
  baseRef: string
  againstRef: string
  repository?: string
}) {
  const [types, setTypes] = useState<ChangeType[]>(
    ['introduced', 'resolved', 'severity_increased', 'severity_decreased', 'removed_artifact', 'remediation_changed'],
  )
  const [severities, setSeverities] = useState<Severity[]>([])
  const [q, setQ] = useState('')

  const counts = useMemo(() => {
    const out = {} as Record<ChangeType, number>
    for (const c of report.changes) out[c.type] = (out[c.type] ?? 0) + 1
    return out
  }, [report.changes])

  const rows = useMemo(() => report.changes.filter((c) => {
    if (types.length > 0 && !types.includes(c.type)) return false
    if (severities.length > 0 && !severities.includes(c.severity)) return false
    if (q) {
      const hay = `${c.cve ?? ''} ${c.id ?? ''} ${c.component.name} ${c.artifact.name} ${c.summary ?? ''}`.toLowerCase()
      if (!hay.includes(q.toLowerCase())) return false
    }
    return true
  }), [report.changes, types, severities, q])

  return (
    <Card
      size="small"
      title={`Changes (${rows.length.toLocaleString()} of ${report.changes.length.toLocaleString()})`}
      extra={
        <Space size={8}>
          <Input.Search
            allowClear
            placeholder="CVE, package or image"
            value={q}
            onChange={(e) => setQ(e.target.value)}
            style={{ width: 220 }}
          />
          <SecurityExportMenu
            urlFor={(format, view) => securityComparisonExportUrl(product, baseRef, againstRef, {
              format, view, repository,
              change: types.length > 0 ? types.join(',') : undefined,
              severity: severities.length > 0 ? severities.join(',') : undefined,
              q: q || undefined,
            })}
          />
        </Space>
      }
    >
      <Space direction="vertical" size={10} style={{ width: '100%', marginBottom: 12 }}>
        <Checkbox.Group
          value={types}
          onChange={(v) => setTypes(v as ChangeType[])}
          options={CHANGE_ORDER.map((t) => ({
            value: t,
            label: `${CHANGE_LABEL[t]} (${counts[t] ?? 0})`,
          }))}
        />
        <Checkbox.Group
          value={severities}
          onChange={(v) => setSeverities(v as Severity[])}
          options={SEVERITIES.map((s) => ({ label: <SeverityTag value={s} />, value: s }))}
        />
      </Space>

      <Table<SecurityChange>
        size="small"
        rowKey={(r, i) => `${r.type}-${r.cve ?? r.id}-${r.component.id}-${r.artifact.name}-${i}`}
        dataSource={rows}
        scroll={{ x: 'max-content' }}
        pagination={{ pageSize: 25, showSizeChanger: true, size: 'small' }}
        columns={[
          {
            title: 'CVE',
            width: 160,
            render: (_, r) => <CveCell cve={r.cve} id={r.id} />,
          },
          {
            title: 'Change',
            width: 170,
            render: (_, r) => (
              <Space direction="vertical" size={0}>
                <Tag color={CHANGE_COLOUR[r.type]}>{r.typeLabel}</Tag>
                {r.viaRemoval && (
                  <Typography.Text type="secondary" style={{ fontSize: 11 }}>
                    no longer shipped
                  </Typography.Text>
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
          {
            title: 'Fix',
            width: 140,
            render: (_, r) => <FixCell fixable={r.fixable} fixedIn={r.fixedIn} />,
          },
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

const CHANGE_LABEL: Record<ChangeType, string> = {
  introduced: 'Introduced',
  resolved: 'Resolved',
  unchanged: 'Unchanged',
  severity_increased: 'More severe',
  severity_decreased: 'Less severe',
  remediation_changed: 'Fix availability changed',
  removed_artifact: 'On a removed artifact',
}
