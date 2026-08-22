import { useMemo, useState } from 'react'
import {
  App, Button, Card, Col, Input, Progress, Row, Segmented, Select, Space, Table,
  Tooltip, Typography,
} from 'antd'
import { usePackageSecurity, useSyncPackageSecurity, packageSecurityExportUrl } from '../api/queries'
import { SEVERITIES } from '../api/types'
import type {
  PackageSecurityResponse, SecurityFinding, SecurityReport, Severity,
} from '../api/types'
import {
  ComponentCell, CveCell, FindingsEmpty, FixCell, ScanStatusTag,
  SecurityExportMenu, SecurityNotConfigured, SecurityProgressPanel, SecurityStateNotice,
  SeverityBar, SeverityCountsRow, SeverityTag, SyncButton, SyncedAgo,
} from './security'
import { mono, semantic, severity as severityColour } from '../theme'

/**
 * The Security tab of a release.
 *
 * # The order of what is on screen, and why it is that order
 *
 * Sync state, then coverage, then totals, then the rows. That is deliberately
 * the order of DECREASING trust: the first thing a reader meets is whether
 * these numbers exist and cover the whole release, and only then the numbers.
 * The same tab with the caveat at the bottom is a tab whose totals get quoted
 * in a release meeting without it.
 *
 * # Two levels in one tab
 *
 * The cards and the distribution bar are the simple view - readable without
 * knowing what a CVE is. The two tables under them are the detailed view. One
 * component rather than two pages, because they are one question at two depths
 * and a reader who needs the second should not navigate away from the first.
 */
export function SecurityTab({ product, reference, repository }: {
  product: string
  reference: string
  repository?: string
}) {
  const { message } = App.useApp()

  const security = usePackageSecurity(product, reference, { repository, detail: true })
  const sync = useSyncPackageSecurity()
  const data = security.data

  const startSync = () => {
    sync.mutate({ product, ref: reference, repository }, {
      onSuccess: (res) => {
        message.info(res.started
          ? `Vulnerability sync started for ${res.artifacts} artifacts. This may take several minutes.`
          : 'A sync is already running for this release.')
        void security.refetch()
      },
      onError: (e) => message.error(e instanceof Error ? e.message : 'The sync could not be started.'),
    })
  }

  if (security.isLoading) {
    return <Card loading />
  }

  /*
   * A 404 here means the deployment has no security storage wired at all - the
   * route is deliberately absent rather than present and always failing. That
   * is not an error to show in red; it is a fact about the deployment.
   */
  if (!data) {
    return <SecurityNotConfigured />
  }

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <Space
        size={12}
        wrap
        style={{ width: '100%', justifyContent: 'space-between', alignItems: 'center' }}
      >
        <SyncedAgo sync={data.sync} />
        {/*
          Only the sync here. The export lives with the filters below, because
          an export respects them - two export buttons on one screen, one of
          which quietly ignores the filter the reader just set, is a file that
          looks complete and answers a different question.
        */}
        <SyncButton sync={data.sync} onSync={startSync} pending={sync.isPending} />
      </Space>

      {data.sync.state === 'syncing'
        ? <Card><SecurityProgressPanel sync={data.sync} /></Card>
        : (
          <SecurityStateNotice
            state={data.state}
            message={data.message}
            onRefresh={data.sync.canSync && data.state !== 'not_synced' ? startSync : undefined}
          />
        )}

      {data.sync.state === '' && data.sync.canSync && <NeverSynced onSync={startSync} pending={sync.isPending} />}

      {(data.sync.state === 'synced' || data.sync.syncedAt) && (
        <>
          <SummaryCards data={data} />
          <FindingsSection data={data} product={product} reference={reference} repository={repository} />
        </>
      )}
    </Space>
  )
}

/**
 * The empty state, which is an OFFER rather than a message.
 *
 * A release nobody has scanned is the normal state of a fresh estate, and the
 * only useful thing to put on this screen is the button that changes it.
 */
function NeverSynced({ onSync, pending }: { onSync: () => void; pending?: boolean }) {
  return (
    <Card>
      <Space direction="vertical" size={10} align="center" style={{ width: '100%', padding: '28px 0' }}>
        <Typography.Title level={5} style={{ margin: 0 }}>
          This release has not been scanned yet
        </Typography.Title>
        <Typography.Text type="secondary" style={{ maxWidth: 520, textAlign: 'center' }}>
          A sync retrieves results for every artifact in this release and stores them, so the counts,
          the release comparison and the vulnerability search are served without contacting the
          scanner again.
        </Typography.Text>
        <Button type="primary" loading={pending} onClick={onSync}>Sync vulnerabilities</Button>
      </Space>
    </Card>
  )
}

/**
 * The simple view: four cards.
 *
 * Fixable is a card of its own rather than a column somewhere, because it is
 * the number that decides what somebody does this afternoon. A release with 900
 * non-fixable findings and 4 fixable ones has four pieces of work in it, and a
 * panel reporting 904 hides all four.
 */
function SummaryCards({ data }: { data: PackageSecurityResponse }) {
  const { counts, coverage } = data
  const fixablePercent = counts.total > 0 ? Math.round((counts.fixable / counts.total) * 100) : 0
  const scannedPercent = coverage.scannable > 0
    ? Math.round((coverage.scanned / coverage.scannable) * 100)
    : 0

  return (
    <Row gutter={[16, 16]}>
      <Col xs={24} lg={9}>
        <Card size="small" title="Overall vulnerabilities" style={{ height: '100%' }}>
          <Space align="start" size={20}>
            <div>
              <div style={{ fontSize: 34, fontWeight: 600, lineHeight: 1.2 }}>
                {counts.total.toLocaleString()}
              </div>
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                {/*
                  Two numbers because they answer two questions. The same
                  base-image CVE in ten images is ten things to fix in ten
                  places, and also one problem - quoting either alone misleads
                  in a different direction.
                */}
                {data.distinctTotal.toLocaleString()} distinct across{' '}
                {coverage.scanned.toLocaleString()} scanned artifacts
              </Typography.Text>
            </div>
          </Space>
          <div style={{ marginTop: 14 }}><SeverityBar counts={counts} /></div>
          <div style={{ marginTop: 12 }}><SeverityCountsRow counts={counts} /></div>
        </Card>
      </Col>

      <Col xs={24} md={12} lg={5}>
        <Card size="small" title="Fixable" style={{ height: '100%' }}>
          <Space align="center" size={16}>
            <Progress
              type="circle"
              size={72}
              percent={fixablePercent}
              strokeColor={semantic.success}
              format={() => `${fixablePercent}%`}
            />
            <Space direction="vertical" size={0}>
              <Typography.Text strong style={{ fontSize: 20, color: semantic.success }}>
                {counts.fixable.toLocaleString()}
              </Typography.Text>
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                with a fixed version
              </Typography.Text>
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                {counts.nonFixable.toLocaleString()} without
              </Typography.Text>
            </Space>
          </Space>
        </Card>
      </Col>

      <Col xs={24} md={12} lg={5}>
        <Card size="small" title="Scan status" style={{ height: '100%' }}>
          <Space align="center" size={16}>
            <Progress
              type="circle"
              size={72}
              percent={scannedPercent}
              strokeColor={coverage.complete ? semantic.success : semantic.warning}
            />
            <Space direction="vertical" size={0}>
              <Typography.Text strong style={{ fontSize: 20 }}>
                {coverage.scanned.toLocaleString()}
              </Typography.Text>
              <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                of {coverage.scannable.toLocaleString()} scanned
              </Typography.Text>
              {/*
                EVERY bucket, named. This card once said "1 not scanned" while
                omitting 209 the scanner had refused, because the two were
                summed into one number somewhere and only one of them was
                shown. They have different fixes and they get different lines.
              */}
              {coverage.unavailable > 0 && (
                <Typography.Text style={{ fontSize: 12, color: semantic.error }}>
                  {coverage.unavailable.toLocaleString()} failed
                </Typography.Text>
              )}
              {coverage.notScanned > 0 && (
                <Typography.Text style={{ fontSize: 12, color: semantic.warning }}>
                  {coverage.notScanned.toLocaleString()} not indexed
                </Typography.Text>
              )}
            </Space>
          </Space>
        </Card>
      </Col>

      <Col xs={24} lg={5}>
        <Card size="small" title="By severity" style={{ height: '100%' }}>
          <Space direction="vertical" size={6} style={{ width: '100%' }}>
            {SEVERITIES.filter((s) => counts.bySeverity[s] > 0 || s !== 'unknown').map((s) => {
              const total = counts.bySeverity[s]
              const fixable = counts.fixableBySeverity[s]
              return (
                <div key={s}>
                  <Space style={{ width: '100%', justifyContent: 'space-between' }}>
                    <SeverityTag value={s} />
                    <Typography.Text style={{ fontSize: 12 }}>
                      <strong>{total}</strong>
                      {total > 0 && (
                        <Typography.Text type="secondary" style={{ fontSize: 11 }}>
                          {' '}({fixable} fixable)
                        </Typography.Text>
                      )}
                    </Typography.Text>
                  </Space>
                  <div style={{ height: 4, background: '#EEF1F4', borderRadius: 2, marginTop: 3 }}>
                    <div
                      style={{
                        width: `${counts.total > 0 ? (total / counts.total) * 100 : 0}%`,
                        height: '100%', background: severityColour[s], borderRadius: 2,
                      }}
                    />
                  </div>
                </div>
              )
            })}
          </Space>
        </Card>
      </Col>
    </Row>
  )
}

/**
 * The kinds a release is filtered by.
 *
 * Three, not the seven media types underneath: a reader looking for "the Helm
 * charts" does not care that one of them is an OCI artifact with a config
 * media type. Anything that is not clearly an image or a chart is a file.
 */
type ArtifactKind = 'all' | 'image' | 'chart' | 'file'

const KIND_ORDER: Exclude<ArtifactKind, 'all'>[] = ['image', 'chart', 'file']

const KIND_LABEL: Record<Exclude<ArtifactKind, 'all'>, string> = {
  image: 'Images',
  chart: 'Charts',
  file: 'Files',
}

function kindOf(kind?: string): ArtifactKind {
  const k = (kind ?? '').toLowerCase()
  if (k.includes('chart') || k.includes('helm')) return 'chart'
  if (k.includes('image') || k === 'index') return 'image'
  if (!k) return 'all'
  return 'file'
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
  const [tab, setTab] = useState<'vulnerabilities' | 'artifacts'>('vulnerabilities')
  const [severities, setSeverities] = useState<Severity[]>([])
  const [fixability, setFixability] = useState<'all' | 'fixable' | 'non-fixable'>('all')
  const [kind, setKind] = useState<ArtifactKind>('all')
  const [q, setQ] = useState('')

  /*
   * The artifact kinds this release actually holds.
   *
   * Offered rather than hardcoded: a release of images alone should not show a
   * Charts filter that can only ever return nothing, and a filter that is
   * always empty teaches a reader to distrust the ones that are not.
   */
  const kinds = useMemo(() => {
    const present = new Set<ArtifactKind>()
    for (const report of data.reports) {
      const k = kindOf(report.artifact.kind)
      if (k !== 'all') present.add(k)
    }
    return KIND_ORDER.filter((k) => present.has(k))
  }, [data.reports])

  const reports = useMemo(
    () => (kind === 'all' ? data.reports : data.reports.filter((r) => kindOf(r.artifact.kind) === kind)),
    [data.reports, kind],
  )

  const findings = useMemo<FlatFinding[]>(() => {
    const out: FlatFinding[] = []
    for (const report of reports) {
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
  }, [reports])

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
    <Card size="small" styles={{ body: { paddingTop: 12 } }}>
      {/*
        Two rows, and which control goes in which is the point.

        The first row is what you are looking at and what you take away with
        you: the view switch on the left, the export on the right. The second is
        how you narrow it. Putting all seven controls on one line looked tidy
        in a mockup and wrapped in a browser, which dropped the export onto a
        line of its own at an arbitrary place - so the button somebody clicks
        last moved every time a filter's label got longer.
      */}
      <div
        style={{
          display: 'flex', gap: 8, alignItems: 'center',
          justifyContent: 'space-between', marginBottom: 12,
        }}
      >
        <Segmented
          value={tab}
          onChange={(v) => setTab(v as typeof tab)}
          options={[
            { value: 'vulnerabilities', label: `Vulnerabilities (${findings.length.toLocaleString()})` },
            { value: 'artifacts', label: `Artifacts (${reports.length.toLocaleString()})` },
          ]}
        />

        <SecurityExportMenu
          urlFor={(format, view) => packageSecurityExportUrl(product, reference, {
            format, view, repository, ...exportFilters,
          })}
        />
      </div>

      <div
        style={{
          display: 'flex', flexWrap: 'wrap', gap: 8,
          alignItems: 'center', marginBottom: 12,
        }}
      >
        {kinds.length > 1 && (
          <Segmented
            value={kind}
            onChange={(v) => setKind(v as ArtifactKind)}
            options={[
              { value: 'all', label: `All (${data.reports.length})` },
              ...kinds.map((k) => ({
                value: k,
                label: `${KIND_LABEL[k]} (${data.reports.filter((r) => kindOf(r.artifact.kind) === k).length})`,
              })),
            ]}
          />
        )}

        <Segmented
          value={fixability}
          onChange={(v) => setFixability(v as typeof fixability)}
          options={[
            { value: 'all', label: 'All' },
            { value: 'fixable', label: 'Fixable' },
            { value: 'non-fixable', label: 'No fix' },
          ]}
        />

        {/*
          A multi-select rather than five checkboxes in a row.

          The checkboxes were 300px of chrome that pushed everything after them
          off the line, and they could not say "any" without being all
          unchecked - which reads as "none selected". A select shows the chosen
          severities, clears in one click, and occupies one control's width.
        */}
        <Select<Severity[]>
          mode="multiple"
          allowClear
          placeholder="Any severity"
          value={severities}
          onChange={setSeverities}
          style={{ minWidth: 190 }}
          maxTagCount="responsive"
          options={SEVERITIES.map((sev) => ({
            value: sev,
            label: <SeverityTag value={sev} />,
          }))}
        />

        <Input.Search
          allowClear
          placeholder="CVE, package or image"
          value={q}
          onChange={(e) => setQ(e.target.value)}
          style={{ width: 230 }}
        />
      </div>

      {tab === 'vulnerabilities'
        ? <VulnerabilityTable rows={filtered} state={data.state} />
        : <ArtifactTable reports={reports} />}
    </Card>
  )
}

function VulnerabilityTable({ rows, state }: { rows: FlatFinding[]; state: PackageSecurityResponse['state'] }) {
  return (
    <Table<FlatFinding>
      size="small"
      rowKey={(r) => `${r.cve ?? r.id}-${r.component.id}-${r.artifactName}`}
      dataSource={rows}
      scroll={{ x: 'max-content' }}
      pagination={{ pageSize: 25, showSizeChanger: true, size: 'small' }}
      locale={{ emptyText: <FindingsEmpty status={state} /> }}
      columns={[
        { title: 'CVE', width: 150, render: (_, r) => <CveCell cve={r.cve} id={r.id} /> },
        {
          title: 'Severity',
          width: 110,
          // Worst first, because SEVERITIES is ordered worst first: the
          // comparator was reversed, so sorting ascending put `low`
          // at the top of a table about what is wrong with a release.
          sorter: (a, b) => SEVERITIES.indexOf(a.severity) - SEVERITIES.indexOf(b.severity),
          defaultSortOrder: 'ascend',
          render: (_, r) => <SeverityTag value={r.severity} />,
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
          width: 170,
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
        { title: 'Fix', width: 130, render: (_, r) => <FixCell fixable={r.fixable} fixedIn={r.fixedIn} /> },
        {
          /*
            Narrow enough that the six columns fit the card at a laptop width.
            The description was 520px wide, which pushed the table 180px past
            its own card and left every row's last words behind a horizontal
            scrollbar. Two clamped lines with the rest in a tooltip says as
            much, in a table that ends where the card does.
          */
          title: 'Description',
          width: 340,
          render: (_, r) => (
            <Typography.Paragraph
              style={{ margin: 0 }}
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
 * ones nobody scanned. A table listing only the artifacts with findings would
 * be a table where an unscanned image is invisible.
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
              <Typography.Text style={{ fontFamily: mono }}>
                {r.artifact.display || r.artifact.name}
              </Typography.Text>
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
          width: 200,
          sorter: (a, b) => a.counts.total - b.counts.total,
          render: (_, r) => (
            r.status !== 'scanned'
              ? <Typography.Text type="secondary">-</Typography.Text>
              : r.counts.total === 0
                ? <Typography.Text>None found</Typography.Text>
                : (
                  <Space direction="vertical" size={2} style={{ width: '100%' }}>
                    <Space size={8}>
                      <strong>{r.counts.total.toLocaleString()}</strong>
                      {SEVERITIES.slice(0, 2).map((s) => (
                        r.counts.bySeverity[s] > 0 ? (
                          <span key={s} style={{ color: severityColour[s], fontSize: 12 }}>
                            {r.counts.bySeverity[s]} {s}
                          </span>
                        ) : null
                      ))}
                    </Space>
                    <SeverityBar counts={r.counts} height={4} />
                  </Space>
                )
          ),
        },
        {
          title: 'Fixable',
          width: 100,
          render: (_, r) => (
            r.status === 'scanned'
              ? <Typography.Text>{r.counts.fixable.toLocaleString()}</Typography.Text>
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

