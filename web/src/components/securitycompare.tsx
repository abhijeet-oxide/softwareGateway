import { useMemo, useState } from 'react'
import type { ReactNode } from 'react'
import { Alert, Button, Card, Checkbox, Col, Input, Row, Segmented, Space, Tag, Tooltip, Typography } from 'antd'
// The working-surface table: resizable, reorderable, pinnable columns whose
// layout each person keeps. See `tablekit/README.md` for which tables get it.
import { Table as DataTable } from '../tablekit'
import { ArrowUpOutlined, CheckOutlined, MinusOutlined } from '../icons'
import { securityComparisonExportUrl } from '../api/queries'
import { SEVERITIES } from '../api/types'
import type {
  ArtifactChange, ChangeType, SecurityArtifactDelta, SecurityChange,
  SecurityComparisonEnd, SecurityComparisonResponse, Severity,
} from '../api/types'
import {
  ComponentCell, CveCell, FixCell, SecurityExportMenu, SeverityBar, SeverityTag, VerdictBanner,
} from './security'
import { c, FieldLabel, mono, verdict as verdictColour } from '../uikit'
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
      workbookNote="The verdict, both releases' artifacts, and every change between them"
      bundleNote="The same tables as CSV files"
      urlFor={(format, view) => securityComparisonExportUrl(product, baseRef, againstRef, {
        format, view, repository,
      })}
    />
  )

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      {/*
        ONE BAND, not two cards with an arrow adrift between them.

        The two ends were separate cards either side of a 2-column gutter
        holding a single grey arrow. That gutter was dead space on the widest
        part of the page, the two cards read as unrelated, and the one number
        the whole comparison exists to produce - what changed - was nowhere
        near either of them: it lived in a Summary table three screens down.

        Now the two ends and the delta between them are one object, read left
        to right, and the delta is the largest thing in it.
      */}
      <Card size="small" styles={{ body: { padding: 0 } }}>
        <div
          className="slm-band"
          style={{ gridTemplateColumns: 'minmax(0, 1fr) minmax(140px, 0.42fr) minmax(0, 1fr)' }}
        >
          <ReleaseEnd
            title="Base release"
            end={report.a}
            onSync={onSync && (() => onSync('a'))}
          />
          <NetChangeZone a={report.a} b={report.b} verdict={report.verdict} />
          <ReleaseEnd
            title="New release"
            end={report.b}
            onSync={onSync && (() => onSync('b'))}
            align="end"
          />
        </div>
      </Card>

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
 * One end of the comparison.
 *
 * A zone within the comparison band rather than a card of its own: the two ends
 * and the delta between them are one fact and were three boxes.
 *
 * Carries its own sync state, because a comparison against a release nobody has
 * scanned is inconclusive and the useful thing to put on screen is the button
 * that changes that - not a verdict repeating that it cannot say.
 */
function ReleaseEnd({ title, end, onSync, align }: {
  title: string
  end: SecurityComparisonEnd
  onSync?: () => void
  align?: 'end'
}) {
  const synced = end.sync.state === 'synced'
  return (
    <div style={{ padding: '18px 22px', minWidth: 0, textAlign: align === 'end' ? 'right' : 'left' }}>
      <FieldLabel>{title}</FieldLabel>
      <div
        style={{
          fontFamily: mono, fontSize: 19, fontWeight: 600, marginTop: 4,
          color: c.text, overflow: 'hidden', textOverflow: 'ellipsis',
          whiteSpace: 'nowrap',
        }}
        title={end.label}
      >
        {end.label}
      </div>

      {synced ? (
        <>
          <Space
            size={20}
            wrap
            style={{ marginTop: 14, justifyContent: align === 'end' ? 'flex-end' : 'flex-start', width: '100%' }}
          >
            <Stat label="Vulnerabilities" value={end.counts.total} />
            <Stat label="Fixable" value={end.counts.fixable} />
            <Stat label="Scanned" value={end.coverage.scanned} suffix={`/ ${end.coverage.scannable}`} />
          </Space>
          <div style={{ marginTop: 12 }}>
            <SeverityBar counts={end.counts} />
          </div>
          <Typography.Text type="secondary" style={{ fontSize: 12, display: 'block', marginTop: 8 }}>
            {end.sync.syncedAt && `synced ${formatRelative(end.sync.syncedAt)}`}
            {end.repository && ` · ${end.repository}`}
          </Typography.Text>
        </>
      ) : (
        <Space direction="vertical" size={8} style={{ marginTop: 12 }}>
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
    </div>
  )
}

/**
 * The number the whole comparison exists to produce, between the two ends that
 * produced it.
 *
 * Signed, always: "+7" and "7" are different facts and only one of them is
 * this one. The sign is the first character so it survives a glance, and the
 * colour agrees with the verdict rather than with the arithmetic - fewer
 * vulnerabilities is better whichever direction the number moved.
 */
function NetChangeZone({ a, b, verdict }: {
  a: SecurityComparisonEnd
  b: SecurityComparisonEnd
  verdict: SecurityComparisonResponse['verdict']
}) {
  const delta = b.counts.total - a.counts.total
  const tone = verdict === 'better' ? verdictColour.better
    : verdict === 'worse' ? verdictColour.worse
      : verdict === 'inconclusive' ? verdictColour.inconclusive
        : verdictColour.unchanged

  return (
    <div
      className="slm-band-mid"
      style={{
        padding: '18px 12px', minWidth: 0, textAlign: 'center',
        display: 'flex', flexDirection: 'column', alignItems: 'center', justifyContent: 'center',
        borderInline: `1px solid ${c.border}`,
        background: c.surface2,
      }}
    >
      <FieldLabel>Net change</FieldLabel>
      <div
        style={{
          fontSize: 32, fontWeight: 600, color: tone, lineHeight: 1.1, marginTop: 6,
          letterSpacing: '-0.02em', fontVariantNumeric: 'tabular-nums',
        }}
      >
        {delta > 0 ? '+' : ''}{delta.toLocaleString()}
      </div>
      <Typography.Text type="secondary" style={{ fontSize: 11.5, marginTop: 2 }}>
        vulnerabilities
      </Typography.Text>
    </div>
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
    <Card size="small" title="Vulnerability overview" styles={{ body: { padding: 0 } }}>
      {/*
        THE THREE SETS AS ONE BAR, not three circles.

        This was three pale discs overlapping by 22px. They looked like a Venn
        diagram and were not one - a real Venn's overlap is the intersection,
        and here the intersection had its own separate disc - so the picture
        made a claim about the data that the data did not support. Worse, the
        three discs were the same size whatever the numbers, so the one visual
        on the page could not be read: 39 and 1,286 drew identical circles.

        A single proportional bar is the same three numbers in a shape that is
        true. The middle segment IS the intersection, its width is its share,
        and two comparisons run a week apart can be held side by side.
      */}
      <div style={{ padding: '18px 22px' }}>
        <SetBar
          old={uniqueToOld}
          both={common}
          fresh={uniqueToNew}
        />
      </div>

      {/*
        Three zones, not three cards inside a card. A bordered box inside a
        bordered box inside a page of bordered boxes is a hierarchy nobody can
        read; a hairline says the same thing and says it once.
      */}
      <div
        className="slm-band"
        style={{
          gridTemplateColumns: 'repeat(3, minmax(0, 1fr))',
          borderTop: `1px solid ${c.border}`,
        }}
      >
        <DeltaZone
          label="Introduced"
          counts={report.introduced}
          colour={verdictColour.worse}
          icon={<ArrowUpOutlined />}
        />
        <DeltaZone
          label="Resolved"
          counts={report.resolved}
          colour={verdictColour.better}
          icon={<CheckOutlined />}
          divider
        />
        <DeltaZone
          label="Unchanged"
          counts={report.unchanged}
          colour={c.text2}
          icon={<MinusOutlined />}
          divider
        />
      </div>
    </Card>
  )
}

/**
 * What the two releases share, and what only one of them has.
 *
 * The middle segment is the intersection and the two ends are the differences,
 * so the bar's shape is the answer: a wide middle is an ordinary point release,
 * and a narrow one is a rebuild wearing a version number.
 */
function SetBar({ old, both, fresh }: { old: number; both: number; fresh: number }) {
  const total = old + both + fresh || 1
  const segments = [
    { key: 'old', n: old, colour: verdictColour.better, label: 'Only in the base release', hint: 'gone in the new one' },
    { key: 'both', n: both, colour: c.text3, label: 'In both releases', hint: 'carried over unchanged' },
    { key: 'new', n: fresh, colour: verdictColour.worse, label: 'Only in the new release', hint: 'arrived with it' },
  ]

  return (
    <div>
      <div
        style={{
          display: 'flex', width: '100%', height: 12, borderRadius: 6,
          overflow: 'hidden', background: c.track,
        }}
      >
        {segments.map((seg, i) => (seg.n === 0 ? null : (
          <Tooltip key={seg.key} title={`${seg.label}: ${seg.n.toLocaleString()} (${seg.hint})`}>
            <div
              className="slm-meter-seg"
              style={{
                width: `${(seg.n / total) * 100}%`, background: seg.colour,
                transformOrigin: 'left',
                animation: `slm-grow 460ms cubic-bezier(0.16,1,0.3,1) ${i * 70}ms both`,
              }}
            />
          </Tooltip>
        )))}
      </div>

      <div
        style={{
          display: 'grid', gridTemplateColumns: 'repeat(3, minmax(0, 1fr))',
          gap: 16, marginTop: 12,
        }}
      >
        {segments.map((seg) => (
          <div key={seg.key} style={{ minWidth: 0 }}>
            <div
              style={{
                display: 'flex', alignItems: 'baseline', gap: 7,
                fontVariantNumeric: 'tabular-nums',
              }}
            >
              <span
                aria-hidden
                style={{
                  width: 8, height: 8, borderRadius: 2, background: seg.colour,
                  display: 'inline-block', flex: 'none',
                }}
              />
              <span style={{ fontSize: 19, fontWeight: 600, color: c.text, lineHeight: 1 }}>
                {seg.n.toLocaleString()}
              </span>
            </div>
            <Typography.Text
              type="secondary"
              style={{ fontSize: 11.5, display: 'block', marginTop: 4, marginInlineStart: 15 }}
            >
              {seg.label}
            </Typography.Text>
          </div>
        ))}
      </div>
    </div>
  )
}

/**
 * One of the three outcomes, with its severity split and its fixability split.
 *
 * Both splits, because they answer different questions: the severity says how
 * bad, and the fixability says how much of it anybody can act on this week.
 */
function DeltaZone({ label, counts, colour, icon, divider }: {
  label: string
  counts: SecurityComparisonResponse['introduced']
  colour: string
  icon: ReactNode
  divider?: boolean
}) {
  return (
    <div
      style={{
        padding: '16px 22px', minWidth: 0,
        borderInlineStart: divider ? `1px solid ${c.border}` : undefined,
      }}
    >
      {/* A drawn icon, not a unicode arrow: the glyph a font happens to have
          for ↑ and − sits on a different baseline in each one. */}
      <FieldLabel icon={<span aria-hidden style={{ color: colour, fontSize: 12, lineHeight: 1 }}>{icon}</span>}>
        {label}
      </FieldLabel>
      <div
        style={{
          fontSize: 30, fontWeight: 600, color: colour, lineHeight: 1.15, marginTop: 4,
          letterSpacing: '-0.02em', fontVariantNumeric: 'tabular-nums',
        }}
      >
        {counts.total.toLocaleString()}
      </div>
      <Space size={10} wrap style={{ marginTop: 4 }}>
        {SEVERITIES.filter((sev) => counts.bySeverity[sev] > 0).map((sev) => (
          <Typography.Text
            key={sev}
            style={{ fontSize: 12, color: c.text2, fontVariantNumeric: 'tabular-nums' }}
          >
            <strong>{counts.bySeverity[sev]}</strong> {sev}
          </Typography.Text>
        ))}
      </Space>
      {counts.total > 0 && (
        <Typography.Text
          type="secondary"
          style={{ fontSize: 11, display: 'block', marginTop: 6, fontVariantNumeric: 'tabular-nums' }}
        >
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
      <DataTable<SeverityRow>
        tableEnhancedKey="security-compare-severity"
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
/**
 * What happened to the ARTIFACTS, which is the one thing the band at the top
 * does not say.
 *
 * It used to open with five more rows - both release labels, the count before,
 * the count after, and the net change - every one of them already stated, in
 * larger type, in the band at the top of this page. A summary that repeats the
 * headline teaches a reader to skip it, and then they skip the four lines here
 * that are not repeated anywhere.
 */
function SummaryCard({ report }: { report: SecurityComparisonResponse }) {
  const rows: [string, string][] = [
    ['Unchanged', String(report.artifactSummary.common)],
    ['Upgraded', String(report.artifactSummary.upgraded)],
    ['Added', String(report.artifactSummary.added)],
    ['Removed', String(report.artifactSummary.removed)],
  ]
  return (
    <Card size="small" title="Artifacts">
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
      <DataTable<SecurityArtifactDelta>
        tableEnhancedKey="security-compare-artifacts"
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
            render: (_, r) => <span style={{ color: r.resolved ? c.ok : undefined }}>{r.resolved}</span>,
          },
          {
            title: 'Introduced',
            width: 110,
            render: (_, r) => <span style={{ color: r.introduced ? c.danger : undefined }}>{r.introduced}</span>,
          },
          { title: 'Severity changed', width: 140, render: (_, r) => r.severityChanged },
          {
            title: '',
            width: 150,
            render: (_, r) => (
              r.comparable ? null : (
                <Tooltip title="One side of this artifact has no scan result, so the columns above are not a comparison.">
                  <Typography.Text type="secondary">Not comparable</Typography.Text>
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
          workbookNote="The verdict, both releases' artifacts, and every change between them"
          bundleNote="The same tables as CSV files"
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

      <DataTable<SecurityChange>
        tableEnhancedKey="security-compare-changes"
        show_column_visibility
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
                  <Typography.Text type="secondary" style={{ fontSize: 11 }}>No longer present</Typography.Text>
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
