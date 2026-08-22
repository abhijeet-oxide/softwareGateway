import { useMemo, useState } from 'react'
import {
  Button, Card, Checkbox, Col, Input, Row, Segmented, Space, Table, Tooltip, Typography,
} from 'antd'
import { ReloadOutlined } from '@ant-design/icons'
import { usePackageSecurity, useRefreshPackageSecurity, useSecurityProgress, packageSecurityExportUrl } from '../api/queries'
import { SEVERITIES } from '../api/types'
import type {
  PackageSecurityResponse, SecurityFinding, SecurityReport, Severity,
} from '../api/types'
import {
  ComponentCell, CoverageMeter, CveCell, FindingsEmpty, FixCell, ScanStatusTag,
  SecurityExportMenu, SecurityNotConfigured, SecurityProgressPanel, SecurityStateNotice,
  SeverityBar, SeverityCountsRow, SeverityTag, VulnerabilityCell,
} from './security'
import { mono, palette, semantic, severity as severityColour } from '../theme'
import { formatRelative } from '../domain/format'

/**
 * A release's security posture: the whole of the "package-level security"
 * requirement in one panel.
 *
 * # The order of what is on screen, and why it is that order
 *
 * State notice, then totals, then coverage, then artifacts, then findings.
 * That is deliberately the order of DECREASING trust: the first thing a reader
 * meets is whether these numbers can be believed, and only then the numbers.
 * The same panel with the caveat at the bottom is a panel whose totals get
 * quoted in a release meeting without it.
 *
 * # Two levels in one panel
 *
 * The tiles and the distribution bar are the simple view - readable without
 * knowing what a CVE is. The two tables under them are the detailed view. They
 * are one component rather than two pages because they are one question at two
 * depths, and a reader who needs the second should not have to navigate away
 * from the first to find it.
 */
export function SecurityPanel({ product, reference, repository }: {
  product: string
  reference: string
  repository?: string
}) {
  /*
   * A token minted once per mount, so the progress side channel has something
   * to key on. The retrieval's response IS the answer, so there is nothing to
   * hand an id back in - the client supplies one and polls beside it.
   */
  const [token] = useState(() => `sec-${Math.random().toString(36).slice(2, 10)}`)

  const security = usePackageSecurity(product, reference, {
    repository, detail: true, progressToken: token,
  })
  const refresh = useRefreshPackageSecurity()
  const busy = security.isLoading || refresh.isPending
  const progress = useSecurityProgress(token, busy)

  const data = security.data

  if (busy && !data) {
    return (
      <Card title="Security">
        <SecurityProgressPanel
          progress={progress.data}
          fallback="Retrieving security data for this release"
        />
      </Card>
    )
  }

  /*
   * A 404 here means the deployment has no scanner wired at all - the route is
   * deliberately absent rather than present and always failing. That is not an
   * error to show in red; it is a fact about the deployment.
   */
  if (!data) {
    return <Card title="Security"><SecurityNotConfigured /></Card>
  }

  return (
    <Card
      title="Security"
      extra={
        <Space size={8}>
          {data.provider && (
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              {data.provider === 'jfrog-xray' ? 'JFrog Xray' : data.provider}
              {data.scannedAt && ` · scanned ${formatRelative(data.scannedAt)}`}
              {data.fromCache > 0 && ` · ${data.fromCache} from cache`}
            </Typography.Text>
          )}
          <SecurityExportMenu
            disabled={!data.enabled}
            urlFor={(format, view) => packageSecurityExportUrl(product, reference, {
              format, view, repository,
            })}
          />
          <Tooltip title="Ask the scanner again, ignoring anything cached.">
            <Button
              icon={<ReloadOutlined />}
              loading={refresh.isPending}
              onClick={() => refresh.mutate({ product, ref: reference, repository, detail: true, progressToken: token })}
            >
              Refresh
            </Button>
          </Tooltip>
        </Space>
      }
    >
      <SecurityStateNotice
        state={data.state}
        message={data.message}
        onRefresh={data.state === 'unavailable'
          ? () => refresh.mutate({ product, ref: reference, repository, detail: true })
          : undefined}
      />

      {refresh.isPending && (
        <SecurityProgressPanel progress={progress.data} fallback="Asking the scanner again" />
      )}

      <SummaryTiles data={data} />

      <FindingsSection data={data} product={product} reference={reference} repository={repository} />
    </Card>
  )
}

/**
 * The simple view: four numbers and a distribution.
 *
 * Fixable is a tile of its own rather than a column somewhere, because it is
 * the number that decides what somebody does this afternoon. A release with 900
 * non-fixable findings and 4 fixable ones has four pieces of work in it, and a
 * panel that reports 904 hides all four.
 */
function SummaryTiles({ data }: { data: PackageSecurityResponse }) {
  const { counts, uniqueCounts, coverage } = data

  return (
    <Row gutter={[16, 16]} style={{ marginBottom: 20 }}>
      <Col xs={24} lg={10}>
        <div style={{ border: `1px solid ${palette.border}`, borderRadius: palette.borderRadius, padding: 16, height: '100%' }}>
          <Typography.Text type="secondary">Vulnerabilities in this release</Typography.Text>
          <div style={{ fontSize: 34, fontWeight: 600, lineHeight: 1.2 }}>
            {counts.total.toLocaleString()}
          </div>
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            {/*
              Two numbers because they answer two questions. The same base-image
              CVE in ten images is ten things to fix in ten places, and also one
              problem - and quoting either alone is misleading in a different
              direction.
            */}
            {uniqueCounts.total.toLocaleString()} distinct across{' '}
            {coverage.scanned.toLocaleString()} scanned artifacts
          </Typography.Text>
          <div style={{ marginTop: 12 }}>
            <SeverityBar counts={counts} />
          </div>
          <div style={{ marginTop: 10 }}>
            <SeverityCountsRow counts={counts} />
          </div>
        </div>
      </Col>

      <Col xs={24} md={12} lg={7}>
        <div style={{ border: `1px solid ${palette.border}`, borderRadius: palette.borderRadius, padding: 16, height: '100%' }}>
          <Typography.Text type="secondary">Something can be done about</Typography.Text>
          <div style={{ fontSize: 34, fontWeight: 600, lineHeight: 1.2, color: semantic.success }}>
            {counts.fixable.toLocaleString()}
          </div>
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            a fixed version is available · {counts.nonFixable.toLocaleString()} have none
          </Typography.Text>
          <div style={{ marginTop: 12 }}>
            <Space size={14} wrap>
              {SEVERITIES.filter((s) => counts.fixableBySeverity[s] > 0).map((s) => (
                <Typography.Text key={s} style={{ color: severityColour[s], fontSize: 12 }}>
                  {counts.fixableBySeverity[s]} {s}
                </Typography.Text>
              ))}
            </Space>
          </div>
        </div>
      </Col>

      <Col xs={24} md={12} lg={7}>
        <div style={{ border: `1px solid ${palette.border}`, borderRadius: palette.borderRadius, padding: 16, height: '100%' }}>
          <Typography.Text type="secondary">How much of the release was scanned</Typography.Text>
          <div style={{ marginTop: 12 }}>
            <CoverageMeter coverage={coverage} />
          </div>
          {coverage.notScanned > 0 && (
            <Typography.Text type="secondary" style={{ fontSize: 12, display: 'block', marginTop: 8 }}>
              {coverage.notScanned} artifacts have no result yet.
            </Typography.Text>
          )}
        </div>
      </Col>
    </Row>
  )
}

/** One row of the flattened findings table. */
type FlatFinding = SecurityFinding & { artifactName: string; artifactTag?: string; artifactDigest?: string }

/**
 * The detailed view: artifacts, and every finding in them.
 *
 * The filters are applied to what is on screen AND handed to the export, so a
 * file somebody downloads matches the table they were looking at. An export
 * that quietly ignored the filter would be a file that looked complete and was
 * a different question's answer.
 */
function FindingsSection({ data, product, reference, repository }: {
  data: PackageSecurityResponse
  product: string
  reference: string
  repository?: string
}) {
  const [tab, setTab] = useState<'artifacts' | 'vulnerabilities'>('vulnerabilities')
  const [severities, setSeverities] = useState<Severity[]>([])
  const [fixability, setFixability] = useState<'all' | 'fixable' | 'non-fixable'>('all')
  const [q, setQ] = useState('')

  const findings = useMemo<FlatFinding[]>(() => {
    const out: FlatFinding[] = []
    for (const report of data.reports) {
      for (const f of report.findings ?? []) {
        out.push({
          ...f,
          artifactName: report.artifact.name,
          artifactTag: report.artifact.tag,
          artifactDigest: report.artifact.digest,
        })
      }
    }
    return out
  }, [data.reports])

  const filtered = useMemo(() => findings.filter((f) => {
    if (severities.length > 0 && !severities.includes(f.severity)) return false
    if (fixability === 'fixable' && !f.fixable) return false
    if (fixability === 'non-fixable' && f.fixable) return false
    if (q) {
      const hay = `${f.cve ?? ''} ${f.id ?? ''} ${f.component.name} ${f.artifactName} ${f.summary ?? ''}`.toLowerCase()
      if (!hay.includes(q.toLowerCase())) return false
    }
    return true
  }), [findings, severities, fixability, q])

  const exportFilters = {
    severity: severities.length > 0 ? severities.join(',') : undefined,
    fixable: fixability === 'all' ? undefined : fixability === 'fixable',
    q: q || undefined,
  }

  return (
    <>
      <Space wrap size={12} style={{ marginBottom: 12, width: '100%', justifyContent: 'space-between' }}>
        <Space wrap size={12}>
          <Segmented
            value={tab}
            onChange={(v) => setTab(v as typeof tab)}
            options={[
              { value: 'vulnerabilities', label: `Vulnerabilities (${findings.length.toLocaleString()})` },
              { value: 'artifacts', label: `Artifacts (${data.reports.length.toLocaleString()})` },
            ]}
          />
          <Segmented
            value={fixability}
            onChange={(v) => setFixability(v as typeof fixability)}
            options={[
              { value: 'all', label: 'All' },
              { value: 'fixable', label: 'Fixable only' },
              { value: 'non-fixable', label: 'No fix available' },
            ]}
          />
          {/* Checkboxes rather than coloured chips: the severity is a WORD
              here, so the filter works for a reader who cannot tell the
              colours apart. */}
          <Checkbox.Group
            value={severities}
            onChange={(v) => setSeverities(v as Severity[])}
            options={SEVERITIES.map((s) => ({ label: <SeverityTag value={s} />, value: s }))}
          />
        </Space>
        <Space size={8}>
          <Input.Search
            allowClear
            placeholder="CVE, package or image"
            value={q}
            onChange={(e) => setQ(e.target.value)}
            style={{ width: 240 }}
          />
          <SecurityExportMenu
            urlFor={(format, view) => packageSecurityExportUrl(product, reference, {
              format, view, repository, ...exportFilters,
            })}
          />
        </Space>
      </Space>

      {tab === 'vulnerabilities'
        ? <VulnerabilityTable rows={filtered} state={data.state} />
        : <ArtifactTable reports={data.reports} />}
    </>
  )
}

function VulnerabilityTable({ rows, state }: { rows: FlatFinding[]; state: PackageSecurityResponse['state'] }) {
  return (
    <Table<FlatFinding>
      size="small"
      rowKey={(r, i) => `${r.cve ?? r.id}-${r.component.id}-${r.artifactName}-${i}`}
      dataSource={rows}
      scroll={{ x: 'max-content' }}
      pagination={{ pageSize: 25, showSizeChanger: true, size: 'small' }}
      locale={{ emptyText: <FindingsEmpty status={state} /> }}
      columns={[
        {
          title: 'CVE',
          width: 160,
          render: (_, r) => <CveCell cve={r.cve} id={r.id} />,
        },
        {
          title: 'Severity',
          width: 120,
          sorter: (a, b) => SEVERITIES.indexOf(b.severity) - SEVERITIES.indexOf(a.severity),
          defaultSortOrder: 'ascend',
          render: (_, r) => <SeverityTag value={r.severity} />,
        },
        {
          title: 'Package',
          width: 190,
          render: (_, r) => (
            <ComponentCell name={r.component.name} version={r.component.version} type={r.component.type} />
          ),
        },
        {
          title: 'Image',
          width: 190,
          render: (_, r) => (
            <Space direction="vertical" size={0}>
              <Typography.Text style={{ fontFamily: mono }}>{r.artifactName}</Typography.Text>
              {r.artifactTag && (
                <Typography.Text type="secondary" style={{ fontSize: 11, fontFamily: mono }}>
                  {r.artifactTag}
                </Typography.Text>
              )}
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
              style={{ margin: 0, maxWidth: 520 }}
              ellipsis={{ rows: 2, tooltip: r.description || r.summary }}
            >
              {r.summary || r.description || '-'}
            </Typography.Paragraph>
          ),
        },
      ]}
    />
  )
}

/**
 * The artifacts of a release, with what is wrong in each.
 *
 * Every artifact appears, including the ones with nothing to report and the
 * ones nobody scanned. A table that listed only the artifacts with findings
 * would be a table where an unscanned image is invisible.
 */
function ArtifactTable({ reports }: { reports: SecurityReport[] }) {
  return (
    <Table<SecurityReport>
      size="small"
      rowKey={(r) => r.artifact.digest || r.artifact.name}
      dataSource={reports}
      scroll={{ x: 'max-content' }}
      pagination={{ pageSize: 20, showSizeChanger: true, size: 'small' }}
      columns={[
        {
          title: 'Artifact',
          render: (_, r) => (
            <Space direction="vertical" size={0}>
              <Typography.Text style={{ fontFamily: mono }}>{r.artifact.display || r.artifact.name}</Typography.Text>
              <Typography.Text type="secondary" style={{ fontSize: 11 }}>
                {[r.artifact.kind, r.artifact.platform].filter(Boolean).join(' · ')}
              </Typography.Text>
            </Space>
          ),
        },
        {
          title: 'Scan',
          width: 130,
          filters: [
            { text: 'Scanned', value: 'scanned' },
            { text: 'Not scanned', value: 'not_scanned' },
            { text: 'Unavailable', value: 'unavailable' },
            { text: 'Not applicable', value: 'unsupported' },
          ],
          onFilter: (value, r) => r.status === value,
          render: (_, r) => (
            <Tooltip title={r.message}>
              <span><ScanStatusTag status={r.status} /></span>
            </Tooltip>
          ),
        },
        {
          title: 'Vulnerabilities',
          width: 220,
          sorter: (a, b) => a.counts.total - b.counts.total,
          render: (_, r) => <VulnerabilityCell counts={r.counts} state={r.status} />,
        },
        {
          title: 'Fixable',
          width: 110,
          render: (_, r) => (
            r.status === 'scanned'
              ? <Typography.Text>{r.counts.fixable.toLocaleString()}</Typography.Text>
              : <Typography.Text type="secondary">-</Typography.Text>
          ),
        },
        {
          title: 'Scanned',
          width: 140,
          render: (_, r) => (
            r.scannedAt
              ? <Typography.Text type="secondary">{formatRelative(r.scannedAt)}</Typography.Text>
              : <Typography.Text type="secondary">-</Typography.Text>
          ),
        },
        {
          title: 'Source',
          width: 110,
          render: (_, r) => (
            <Typography.Text type="secondary">
              {r.provider === 'jfrog-xray' ? 'JFrog Xray' : (r.provider || '-')}
            </Typography.Text>
          ),
        },
      ]}
    />
  )
}
