import { useEffect, useMemo, useState } from 'react'
import type { ReactNode } from 'react'
import {
  Alert,
  App, Button, Card, Col, Collapse, Descriptions, Drawer, Input, Row, Segmented, Select,
  Space, Spin, Table, Tag, Tooltip, Typography,
} from 'antd'
import { CopyOutlined, ExportOutlined } from '@ant-design/icons'
import {
  packageSecurityExportUrl, useCancelPackageSecuritySync, usePackageSecurity,
  useSyncPackageSecurity,
} from '../api/queries'
import { SEVERITIES } from '../api/types'
import type {
  PackageSecurityResponse, SecurityCounts, SecurityFinding, SecurityReport, SecuritySeverityCounts, Severity,
} from '../api/types'
import {
  ComponentCell, CveCell, DescriptionCell, FindingsEmpty, FixCell, ScanStatusTag,
  SecurityExportMenu, SecurityNotConfigured, SecurityProgressPanel, SecurityStateNotice,
  SeverityBar, SeverityTag, StopSyncButton, SyncButton, SyncedAgo, SyncInterrupted, SyncLogButton,
} from './security'
import { formatAbsolute, formatRelative } from '../domain/format'
import { c, mono, severity as severityColour } from '../uikit'

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
  const cancel = useCancelPackageSecuritySync()
  const data = security.data

  /*
   * The view lives up here rather than inside the tables, because the banner at
   * the top has to be able to open one of them. A notice that says 250
   * artifacts have no result and cannot take the reader to them is a notice
   * that has only made them anxious.
   */
  const [tab, setTab] = useState<FindingsTab>('vulnerabilities')

  const problemCount = useMemo(
    () => (data?.reports ?? []).filter((r) => r.status !== 'scanned' && r.status !== 'unsupported').length,
    [data?.reports],
  )

  const showProblems = () => {
    setTab('problems')
    document.getElementById('security-findings')?.scrollIntoView({ behavior: 'smooth', block: 'start' })
  }

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

  const stopSync = () => {
    cancel.mutate({ product, ref: reference, repository }, {
      onSuccess: (res) => {
        message.info(res.stopped
          ? 'The sync was stopped. This release keeps whatever its last completed sync recorded.'
          : 'That sync had already finished.')
        void security.refetch()
      },
      onError: (e) => message.error(e instanceof Error ? e.message : 'The sync could not be stopped.'),
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

  // A sync is only running if something is running it. See sync.stalled: a
  // claim whose Coordinator went away leaves the row saying `syncing` forever.
  const syncing = data.sync.state === 'syncing' && !data.sync.stalled

  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <div
        style={{
          width: '100%',
          display: 'flex',
          flexWrap: 'wrap',
          alignItems: 'center',
          gap: 12,
        }}
      >
        <SyncedAgo sync={data.sync} />
        {/*
          Only the sync and the log here. The export lives with the filters
          below, because an export respects them - two export buttons on one
          screen, one of which quietly ignores the filter the reader just set,
          is a file that looks complete and answers a different question.
        */}
        <Space
          size={8}
          style={{
            marginInlineStart: 'auto',
            marginTop: 8,
            justifyContent: 'flex-end',
          }}
        >
          <SyncLogButton sync={data.sync} />
          <StopSyncButton sync={data.sync} onStop={stopSync} pending={cancel.isPending} />
          <SyncButton sync={data.sync} onSync={startSync} pending={sync.isPending} />
        </Space>
      </div>

      {/*
        THREE states, not two. A row marked `syncing` whose claim has stopped
        beating is not a sync in progress - it is a Coordinator that was killed
        mid-sync - and rendering the progress panel for it drew a bar at nothing
        under a sentence saying the work was happening elsewhere.
      */}
      {syncing
        ? (
          <Card>
            <SecurityProgressPanel
              sync={data.sync}
              onStop={stopSync}
              stopping={cancel.isPending}
            />
          </Card>
        )
        : data.sync.stalled
          ? <SyncInterrupted sync={data.sync} onSync={startSync} pending={sync.isPending} />
          : (
            <SecurityStateNotice
              state={data.state}
              message={data.message}
              onRefresh={data.sync.canSync && data.state !== 'not_synced' ? startSync : undefined}
              onShowProblems={data.reports.length > 0 ? showProblems : undefined}
              problemCount={problemCount}
            />
          )}

      {data.sync.state === '' && data.sync.canSync && <NeverSynced onSync={startSync} pending={sync.isPending} />}

      {(data.sync.state === 'synced' || data.sync.syncedAt) && (
        <>
          <SummaryCards data={data} syncing={syncing} />
          <FindingsSection
            data={data}
            product={product}
            reference={reference}
            repository={repository}
            tab={tab}
            onTabChange={setTab}
            syncing={syncing}
          />
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
 * The simple view: four cards, each answering a different question.
 *
 * # Why the totals card does not list the severities
 *
 * It did, and the card beside it listed them again with the fixable split - the
 * same five numbers twice, in two visual languages, taking half the width of
 * the page to say one thing. The totals card now carries the SHAPE (one bar)
 * and the two numbers only it can give: how many findings, and how many
 * distinct problems they are. The breakdown lives in one place.
 *
 * Fixable is a card of its own rather than a column somewhere, because it is
 * the number that decides what somebody does this afternoon. A release with 900
 * non-fixable findings and 4 fixable ones has four pieces of work in it, and a
 * panel reporting 904 hides all four.
 */
/**
 * The numbers the cards show, derived from the SAME rows the tables show.
 *
 * # Why not the stored counts
 *
 * Because they answer a subtly different question and the page put both on
 * screen at once. The card read "8,479 · 2,805 unique CVEs" over a table
 * offering "Unique CVEs (1,499)" and "All findings (8,152)", and none of the
 * four numbers matched: the stored total is what the sync summed, and the
 * stored distinct total counts (CVE, package) PAIRS - openssl and libssl3 with
 * the same advisory are two - where a reader hearing "unique CVEs" counts one.
 *
 * So the cards are computed from the findings, and fall back to the stored
 * counts only when the per-artifact rows are not loaded. A page that quotes two
 * different totals teaches a reader to trust neither.
 */
function summarise(data: PackageSecurityResponse) {
  const zero = (): Record<Severity, number> => ({ critical: 0, high: 0, medium: 0, low: 0, unknown: 0 })
  const cves = new Set<string>()
  const bySeverity = zero()
  const fixableBySeverity = zero()
  let total = 0
  let fixable = 0
  let affected = 0

  for (const r of data.reports) {
    if (r.counts.total > 0) affected += 1
    for (const f of r.findings ?? []) {
      total += 1
      cves.add(f.cve || f.id || 'unknown')
      bySeverity[f.severity] += 1
      if (f.fixable) {
        fixable += 1
        fixableBySeverity[f.severity] += 1
      }
    }
  }

  if (total === 0) {
    return {
      unique: data.distinctTotal,
      total: data.counts.total,
      fixable: data.counts.fixable,
      nonFixable: data.counts.nonFixable,
      bySeverity: data.counts.bySeverity,
      fixableBySeverity: data.counts.fixableBySeverity,
      affected,
    }
  }
  return {
    unique: cves.size,
    total,
    fixable,
    nonFixable: total - fixable,
    bySeverity,
    fixableBySeverity,
    affected,
  }
}

/**
 * The cards, and what they are allowed to say while a sync is running.
 *
 * # Why `syncing` reaches this far down
 *
 * Because a sync REPLACES these numbers, and it does it in two steps: the
 * per-artifact rows go first and the release's summary is rewritten at the end.
 * In between, every number here is either the last sync's or a zero computed
 * from rows that are no longer there - and the page rendered both as though
 * they were this sync's results. "0 fixable" over a spinner is the reader being
 * told there is no work to do, in the one place this feature exists to get
 * right.
 *
 * So while a sync runs the cards say what they are: numbers being retrieved,
 * with the previous sync's figure named as the previous sync's. A dash and a
 * date beat a confident wrong number.
 */
function SummaryCards({ data, syncing }: { data: PackageSecurityResponse; syncing?: boolean }) {
  const { coverage } = data
  const stats = useMemo(() => summarise(data), [data])
  const fixablePercent = stats.total > 0 ? Math.round((stats.fixable / stats.total) * 100) : 0
  const scannedPercent = coverage.scannable > 0
    ? Math.round((coverage.scanned / coverage.scannable) * 100)
    : 0
  const previous = data.syncedAt ? `from the sync ${formatRelative(data.syncedAt)}` : undefined

  if (syncing) {
    return (
      <Row gutter={[16, 16]}>
        <Col xs={24} lg={12}>
          <PendingCard
            title="Vulnerabilities"
            previous={stats.total > 0
              ? `${stats.unique.toLocaleString()} unique CVEs in ${stats.total.toLocaleString()} findings`
              : undefined}
            note={previous}
          />
        </Col>
        <Col xs={24} md={12} lg={6}>
          <PendingCard
            title="Fixable"
            previous={stats.total > 0 ? `${stats.fixable.toLocaleString()} with a fixed version` : undefined}
            note={previous}
          />
        </Col>
        <Col xs={24} md={12} lg={6}>
          <PendingCard
            title="Scan coverage"
            previous={coverage.scannable > 0
              ? `${coverage.scanned.toLocaleString()} of ${coverage.scannable.toLocaleString()} images scanned`
              : undefined}
            note={previous}
          />
        </Col>
      </Row>
    )
  }

  return (
    /*
      ONE BAND, THREE ZONES - not three cards.

      This was a card of totals, a card with a ring reading "80%", and a card
      with a ring reading "100%". Three boxes of equal visual weight, two of
      them dominated by a circle whose only job was to restate a percentage
      written next to it. A ring at 100% is decoration: it draws a full circle
      to say the same thing the word "all" says, and it earns its space only
      when the shape of the number is the point.

      The three facts are one posture, read left to right in the order somebody
      asks them: how bad is it, what is it made of, and can I trust the answer.
      Hairlines between them, because they belong to each other.
    */
    <Card size="small" styles={{ body: { padding: 0 } }}>
      <div
        className="slm-band"
        style={{
          gridTemplateColumns: 'minmax(230px, 0.85fr) minmax(280px, 1.15fr) minmax(230px, 0.9fr)',
        }}
      >
        {/* -------------------------------------------------- how bad it is -- */}
        <div style={{ padding: '18px 22px', minWidth: 0 }}>
          <ZoneLabel>Vulnerabilities</ZoneLabel>
          {/*
            Unique first. The total is the same advisory counted once per image
            it appears in - a measure of how much replacing there is to do - and
            it is not the number somebody means when they ask how many
            vulnerabilities a release has.
          */}
          <div
            style={{
              fontSize: 44, fontWeight: 600, lineHeight: 1, letterSpacing: '-0.03em',
              color: c.text, fontVariantNumeric: 'tabular-nums',
            }}
          >
            {stats.unique.toLocaleString()}
          </div>
          <Typography.Text type="secondary" style={{ fontSize: 12.5 }}>
            unique CVEs
          </Typography.Text>

          <div style={{ marginTop: 16 }}>
            <SeverityBar
              counts={{
                total: stats.total,
                fixable: stats.fixable,
                nonFixable: stats.nonFixable,
                bySeverity: stats.bySeverity,
                fixableBySeverity: stats.fixableBySeverity,
              }}
              height={8}
            />
          </div>
          <Typography.Text type="secondary" style={{ fontSize: 12, display: 'block', marginTop: 8 }}>
            {stats.total.toLocaleString()} findings
            {stats.affected > 0
              && ` across ${stats.affected.toLocaleString()} of ${coverage.scanned.toLocaleString()} images`}
          </Typography.Text>
        </div>

        {/* ------------------------------------------- what it is made of -- */}
        <div
          style={{
            padding: '18px 22px', minWidth: 0,
            borderInlineStart: `1px solid ${c.border}`,
          }}
        >
          <ZoneLabel>By severity</ZoneLabel>
          <div style={{ display: 'grid', gap: 10 }}>
            {SEVERITIES.filter((sev) => stats.bySeverity[sev] > 0 || sev !== 'unknown').map((sev) => {
              const total = stats.bySeverity[sev]
              const fixable = stats.fixableBySeverity[sev]
              const share = stats.total > 0 ? (total / stats.total) * 100 : 0
              return (
                <div key={sev}>
                  <div
                    style={{
                      display: 'flex', alignItems: 'baseline', gap: 8, marginBottom: 5,
                      fontVariantNumeric: 'tabular-nums',
                    }}
                  >
                    <SeverityTag value={sev} />
                    <span
                      style={{
                        marginInlineStart: 'auto', fontSize: 13, fontWeight: 600,
                        color: total > 0 ? c.text : c.text2,
                      }}
                    >
                      {total.toLocaleString()}
                    </span>
                    <span
                      style={{ fontSize: 11, color: c.text2, minWidth: 66, textAlign: 'right' }}
                    >
                      {total > 0 ? `${fixable.toLocaleString()} fixable` : ''}
                    </span>
                  </div>
                  {/*
                    Proportional to the whole release, so the row lengths are
                    comparable with each other. A bar normalised per severity
                    would draw four full-width bars and say nothing.
                  */}
                  <div style={{ height: 5, background: c.surface2, borderRadius: 3 }}>
                    <div
                      className="slm-meter-seg"
                      style={{
                        width: `${share}%`, height: '100%', borderRadius: 3,
                        background: severityColour[sev], transformOrigin: 'left',
                        animation: 'slm-grow 420ms cubic-bezier(0.16,1,0.3,1) both',
                      }}
                    />
                  </div>
                </div>
              )
            })}
          </div>
        </div>

        {/* ------------------------------ whether the answer can be trusted -- */}
        <div
          style={{
            padding: '18px 22px', minWidth: 0,
            borderInlineStart: `1px solid ${c.border}`,
            background: c.surface2,
          }}
        >
          <ZoneLabel>Confidence</ZoneLabel>

          <Meter
            value={fixablePercent}
            colour={c.ok}
            headline={`${stats.fixable.toLocaleString()} of ${stats.total.toLocaleString()} have a fix`}
            detail={stats.nonFixable > 0
              ? `${stats.nonFixable.toLocaleString()} have no fixed version yet`
              : 'Every finding has a fixed version'}
          />

          <div style={{ height: 16 }} />

          <Meter
            value={scannedPercent}
            colour={coverage.complete ? c.ok : c.pending}
            headline={`${coverage.scanned.toLocaleString()} of ${coverage.scannable.toLocaleString()} images scanned`}
            detail={coverage.complete
              ? 'Every scannable image has a result'
              : 'Some images have no result, so the count above is a floor'}
          />

          {/*
            EVERY bucket, named. This once said "1 not scanned" while omitting
            209 the scanner had refused, because the two were summed into one
            number somewhere and only one of them was shown. They have different
            fixes and they get different lines.

            What is NOT here: the charts and files. They are not a gap in
            coverage - the scanner is never asked about them - and a line saying
            "102 not applicable" invites a reader to subtract it from something.
          */}
          {(coverage.missing > 0 || coverage.unavailable > 0 || coverage.notScanned > 0) && (
            <div style={{ marginTop: 12 }}>
              {coverage.missing > 0 && (
                <CoverageLine n={coverage.missing} label="not found in the repository" colour={c.danger} />
              )}
              {coverage.unavailable > 0 && (
                <CoverageLine n={coverage.unavailable} label="not retrieved" colour={c.danger} />
              )}
              {coverage.notScanned > 0 && (
                <CoverageLine n={coverage.notScanned} label="awaiting a scan" colour={c.pending} />
              )}
            </div>
          )}
        </div>
      </div>
    </Card>
  )
}

/** The name of a zone within the posture band. */
function ZoneLabel({ children }: { children: ReactNode }) {
  return (
    <div
      style={{
        fontSize: 11, fontWeight: 600, letterSpacing: '0.06em', textTransform: 'uppercase',
        color: c.text2, marginBottom: 10,
      }}
    >
      {children}
    </div>
  )
}

/**
 * A proportion, as a sentence with a bar under it.
 *
 * This replaced a 78px progress ring. The ring drew a circle to restate a
 * percentage printed inside it, and at 100% - which is the common case - it
 * drew a complete circle to say "all", which the sentence already said. A
 * horizontal bar under the sentence is the same information in a shape that
 * can be compared with the bar under the sentence below it.
 */
function Meter({ value, colour, headline, detail }: {
  value: number
  colour: string
  headline: string
  detail: string
}) {
  return (
    <div>
      <div
        style={{
          display: 'flex', alignItems: 'baseline', gap: 8, marginBottom: 6,
          fontVariantNumeric: 'tabular-nums',
        }}
      >
        <span style={{ fontSize: 13, fontWeight: 600, color: c.text }}>{headline}</span>
        <span style={{ marginInlineStart: 'auto', fontSize: 12, fontWeight: 600, color: colour }}>
          {value}%
        </span>
      </div>
      <div style={{ height: 5, background: c.borderStrong, borderRadius: 3, overflow: 'hidden' }}>
        <div
          className="slm-meter-seg"
          style={{
            width: `${value}%`, height: '100%', background: colour, borderRadius: 3,
            transformOrigin: 'left',
            animation: 'slm-grow 480ms cubic-bezier(0.16,1,0.3,1) both',
          }}
        />
      </div>
      <Typography.Text type="secondary" style={{ fontSize: 11.5, display: 'block', marginTop: 5 }}>
        {detail}
      </Typography.Text>
    </div>
  )
}


/**
 * A number that is being fetched, said as one.
 *
 * The dash is deliberate and so is its size: it occupies the place the figure
 * will occupy, so the card does not resize when the sync lands, and it cannot
 * be misread as a zero. What the last sync said is kept beside it, named as
 * the last sync's - useful context, and never confusable with this one's.
 */
function PendingCard({ title, previous, note }: {
  title: string
  previous?: string
  note?: string
}) {
  return (
    <Card size="small" title={title} style={{ height: '100%' }}>
      <Space direction="vertical" size={2} style={{ width: '100%' }}>
        <Space size={10} align="center">
          <Spin size="small" />
          <Typography.Text type="secondary">Fetching details</Typography.Text>
        </Space>
        {previous && (
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            Previously {previous}
            {note ? `, ${note}` : ''}.
          </Typography.Text>
        )}
      </Space>
    </Card>
  )
}

/** One exception to full coverage: a count, a colour and what to call it. */
function CoverageLine({ n, label, colour }: { n: number; label: string; colour: string }) {
  return (
    <Space size={6}>
      <span
        aria-hidden
        style={{ display: 'inline-block', width: 6, height: 6, borderRadius: '50%', background: colour }}
      />
      <Typography.Text style={{ fontSize: 12 }}>
        <strong>{n.toLocaleString()}</strong>
        <Typography.Text type="secondary" style={{ fontSize: 12 }}> {label}</Typography.Text>
      </Typography.Text>
    </Space>
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
  // `index` is the release manifest, not an image: the images it lists are
  // separate rows and it is the only thing that would make Images a lie.
  if (k === 'image') return 'image'
  if (!k) return 'all'
  return 'file'
}

/** One row of the flattened findings table. */
type FlatFinding = SecurityFinding & { artifactName: string; artifactTag?: string; artifactDigest?: string }

/** The counts one image's row shows, rebuilt from a (possibly filtered) findings list. */
function summariseCounts(findings: SecurityFinding[]): SecurityCounts {
  const zero = (): SecuritySeverityCounts => ({ critical: 0, high: 0, medium: 0, low: 0, unknown: 0 })
  const bySeverity = zero()
  const fixableBySeverity = zero()
  let fixable = 0
  for (const f of findings) {
    bySeverity[f.severity] += 1
    if (f.fixable) {
      fixable += 1
      fixableBySeverity[f.severity] += 1
    }
  }
  return {
    total: findings.length,
    fixable,
    nonFixable: findings.length - fixable,
    bySeverity,
    fixableBySeverity,
  }
}

/** Which of the three tables is on screen. */
export type FindingsTab = 'vulnerabilities' | 'artifacts' | 'problems'

/**
 * The detailed view: artifacts, and every finding in them.
 *
 * The filters are applied to what is on screen AND handed to the export, so a
 * file somebody downloads matches the table they were looking at. An export
 * that quietly ignored the filter would be a file that looked complete and was
 * a different question's answer.
 */
function FindingsSection({ data, product, reference, repository, tab, onTabChange, syncing }: {
  data: PackageSecurityResponse
  product: string
  reference: string
  repository?: string
  tab: FindingsTab
  onTabChange: (tab: FindingsTab) => void
  syncing?: boolean
}) {
  type KindCounts = Record<ArtifactKind, number>

  const [severities, setSeverities] = useState<Severity[]>([])
  const [fixability, setFixability] = useState<'all' | 'fixable' | 'non-fixable'>('all')
  const [kind, setKind] = useState<ArtifactKind>('all')
  const [grouping, setGrouping] = useState<'unique' | 'all'>('unique')
  const [q, setQ] = useState('')

  /** The artifact kinds this release actually holds. */
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

  /*
   * The images, which are the whole subject of this tab.
   *
   * Xray scans container images. The charts and files in a release are listed
   * on the package's own Contents tab, where they belong; here they were a
   * hundred rows saying "not applicable" in a table about what is wrong with a
   * release, and a Charts filter that could only ever return nothing.
   */
  const imageReports = useMemo(
    () => data.reports.filter((r) => kindOf(r.artifact.kind) === 'image'),
    [data.reports],
  )

  /*
   * The images table, filtered the same way the vulnerabilities table is.
   *
   * Severity, fixability and search narrow WHAT COUNTS AS A FINDING, and an
   * image's row is built out of its findings - so a filter that changed one
   * table and not the other was the same release disagreeing with itself:
   * "No fix" showing 2,930 vulnerabilities under a total of 107 fixable ones.
   */
  const filteredImageReports = useMemo(() => {
    if (severities.length === 0 && fixability === 'all' && !q) return imageReports
    return imageReports.map((r) => {
      const kept = (r.findings ?? []).filter((f) => {
        if (severities.length > 0 && !severities.includes(f.severity)) return false
        if (fixability === 'fixable' && !f.fixable) return false
        if (fixability === 'non-fixable' && f.fixable) return false
        if (q) {
          const hay = `${f.cve ?? ''} ${f.id ?? ''} ${f.component.name} ${r.artifact.name} ${f.summary ?? ''}`
            .toLowerCase()
          if (!hay.includes(q.toLowerCase())) return false
        }
        return true
      })
      if (kept.length === (r.findings ?? []).length) return r
      return { ...r, findings: kept, counts: summariseCounts(kept) }
    })
  }, [imageReports, severities, fixability, q])

  const artifactCounts = useMemo<KindCounts>(
    () => ({ all: imageReports.length, image: imageReports.length, chart: 0, file: 0 }),
    [imageReports],
  )

  const vulnerabilityCounts = useMemo<KindCounts>(() => {
    const out: KindCounts = { all: 0, image: 0, chart: 0, file: 0 }
    for (const report of data.reports) {
      const n = report.counts.total
      out.all += n
      const k = kindOf(report.artifact.kind)
      if (k !== 'all') out[k] += n
    }
    return out
  }, [data.reports])

  /*
   * Counted as a problem only if somebody could do something about it. A Helm
   * chart the scanner does not cover is not a gap in this release, and putting
   * 150 of them in a tab called Problems is how a number nobody can act on
   * teaches a reader to ignore the tab.
   */
  const problemCounts = useMemo<KindCounts>(() => {
    const out: KindCounts = { all: 0, image: 0, chart: 0, file: 0 }
    for (const report of data.reports) {
      if (report.status === 'scanned' || report.status === 'unsupported') continue
      out.all += 1
      const k = kindOf(report.artifact.kind)
      if (k !== 'all') out[k] += 1
    }
    return out
  }, [data.reports])

  const kindCounts = tab === 'vulnerabilities'
    ? vulnerabilityCounts
    : tab === 'problems' ? problemCounts : artifactCounts

  /*
   * Offered per TAB, not per release.
   *
   * The scanner covers container images, so on the Vulnerabilities tab a
   * release full of Helm charts still shows "Charts (0)" - a control that can
   * only ever return nothing, sitting next to ones that do, which teaches a
   * reader to distrust all of them. The charts are still on the Artifacts tab,
   * where they exist, and named on the Problems tab, where they are explained.
   */
  const offeredKinds = useMemo(
    () => kinds.filter((k) => kindCounts[k] > 0),
    [kinds, kindCounts],
  )

  useEffect(() => {
    if (kind !== 'all' && !offeredKinds.includes(kind)) setKind('all')
  }, [kind, offeredKinds])

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

  const vulnerabilityTotal = useMemo(
    () => reports.reduce((sum, r) => sum + r.counts.total, 0),
    [reports],
  )

  /*
   * The same CVE in ten images is ten rows and one problem.
   *
   * Both numbers are true and they answer different questions - "how much work
   * is there" and "how many things are wrong" - so the table offers both and
   * opens on the smaller one. A reader meeting 8,479 rows, four of which are
   * the same OpenSSL advisory in four images, cannot see that they have one
   * upgrade to do.
   */
  const cveGroups = useMemo(() => groupByCve(filtered), [filtered])

  /*
   * The release's own stored total, which is the number the cards above show.
   *
   * It has to be part of this test. The per-artifact reports come from a cache
   * shared by every release holding the same artifact, and a later sync that
   * could not reach the scanner replaces those rows - so BOTH the rows and
   * their counts go to zero while the release's stored summary still says 241.
   * A test that only looked at the reports' sum saw zero against zero, agreed
   * with itself, and rendered "Nothing to show" under a card reading 241.
   */
  const summaryTotal = data.counts?.total ?? 0
  const expectedTotal = Math.max(vulnerabilityTotal, kind === 'all' ? summaryTotal : 0)
  // Never during a sync: the rows are missing because they are being rewritten.
  const detailRowsUnavailable = !syncing && findings.length === 0 && expectedTotal > 0

  /*
   * The distinct things the scanner said, and which artifacts it said them
   * about. Grouped by the sentence: 250 failures in this release are three
   * reasons, and three reasons with counts is something a reader can act on
   * where 250 rows is something they scroll past.
   *
   * Charts and files are not here. Xray is never asked about them, so there is
   * nothing it said - listing a hundred of them under "Problems" described the
   * release's contents rather than anything that went wrong.
   */
  const problems = useMemo<Problem[]>(() => {
    const byKey = new Map<string, Problem>()
    for (const report of reports) {
      if (report.status === 'scanned' || report.status === 'unsupported') continue
      const reason = report.message?.trim() || 'The scanner gave no reason.'
      const key = `${report.status}|${reason}`
      const existing = byKey.get(key)
      if (existing) {
        existing.reports.push(report)
        continue
      }
      byKey.set(key, { status: report.status, reason, reports: [report] })
    }
    return [...byKey.values()].sort((a, b) => (
      problemRank(a.status) !== problemRank(b.status)
        ? problemRank(b.status) - problemRank(a.status)
        : b.reports.length - a.reports.length
    ))
  }, [reports])

  const scannedImages = useMemo(
    () => imageReports.filter((r) => r.status === 'scanned').length,
    [imageReports],
  )

  /** The scanner's own page for an image, by the name the findings carry. */
  const scanUrlByImage = useMemo(() => {
    const out = new Map<string, string>()
    for (const r of data.reports) {
      if (r.scanUrl) out.set(r.artifact.name, r.scanUrl)
    }
    return out
  }, [data.reports])
  const scanUrlFor = (name: string) => scanUrlByImage.get(name)

  const exportFilters = {
    severity: severities.length > 0 ? severities.join(',') : undefined,
    fixable: fixability === 'all' ? undefined : fixability === 'fixable',
    q: q || undefined,
  }

  /*
   * A sync clears the per-artifact rows before it refills them, so mid-sync
   * every table here is empty and every tab reads (0). Rendered as a result
   * that is what the previous build showed: three empty tables and a warning
   * saying the detailed rows had gone missing, over cards still reporting
   * 5,712 vulnerabilities. It is not a result. It is a result being fetched.
   */
  if (syncing && data.reports.length === 0) {
    return (
      <Card size="small" id="security-findings">
        <Space direction="vertical" size={10} align="center" style={{ width: '100%', padding: '36px 0' }}>
          <Spin />
          <Typography.Text strong>Retrieving results</Typography.Text>
          <Typography.Text type="secondary" style={{ maxWidth: 460, textAlign: 'center' }}>
            The vulnerability, image and problem tables are rebuilt when the sync finishes. The
            totals above are from the previous sync.
          </Typography.Text>
        </Space>
      </Card>
    )
  }

  return (
    <Card size="small" id="security-findings" styles={{ body: { paddingTop: 12 } }}>
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
          onChange={(v) => onTabChange(v as FindingsTab)}
          options={[
            {
              value: 'vulnerabilities',
              label: `Vulnerabilities (${(grouping === 'unique'
                ? cveGroups.length
                : vulnerabilityTotal).toLocaleString()})`,
            },
            /*
              The fraction, not the total. "Images (160)" over a table where 140
              rows say "Not in JFrog" reads as a release of 160 scanned images,
              and the number a reader carries away is the one on the tab.
            */
            {
              value: 'artifacts',
              label: scannedImages === imageReports.length
                ? `Images (${imageReports.length.toLocaleString()})`
                : `Images (${scannedImages.toLocaleString()}/${imageReports.length.toLocaleString()})`,
            },
            /*
              A view of its own, and not a filter on the artifacts table.

              What went wrong is a different question from what is in the
              release, it has a different shape - a few reasons, many artifacts
              each - and it was previously answerable only by filtering a table
              nobody thought to filter and hovering each row for a tooltip.
            */
            {
              value: 'problems',
              label: `Problems (${problemCounts.all.toLocaleString()})`,
              disabled: problems.length === 0,
            },
          ]}
        />

        {tab !== 'problems' && (
          <SecurityExportMenu
            urlFor={(format, view) => packageSecurityExportUrl(product, reference, {
              format, view, repository, ...exportFilters,
            })}
          />
        )}
      </div>

      <div
        style={{
          display: 'flex', flexWrap: 'wrap', gap: 8,
          alignItems: 'center', marginBottom: 12,
        }}
      >
        {/* Search first: it is the control somebody arrives with a CVE for. */}
        {tab !== 'problems' && (
          <Input.Search
            allowClear
            placeholder="CVE, package or image"
            value={q}
            onChange={(e) => setQ(e.target.value)}
            style={{ width: 260 }}
          />
        )}

        {tab === 'vulnerabilities' && (
          <Segmented
            value={grouping}
            onChange={(v) => setGrouping(v as typeof grouping)}
            options={[
              { value: 'unique', label: `Unique CVEs (${cveGroups.length.toLocaleString()})` },
              { value: 'all', label: `All findings (${filtered.length.toLocaleString()})` },
            ]}
          />
        )}

        {/*
          The order is the order somebody narrows in: what am I looking at
          (unique or every occurrence), then can anything be done about it, then
          how bad. Fixability sat AFTER the severity select, which put the
          coarsest control - three buttons, always visible - behind the fussiest
          one, and left the two segmented controls either side of a dropdown.
        */}
        {tab !== 'problems' && (
          <Segmented
            value={fixability}
            onChange={(v) => setFixability(v as typeof fixability)}
            options={[
              { value: 'all', label: 'All' },
              { value: 'fixable', label: 'Fixable' },
              { value: 'non-fixable', label: 'No fix' },
            ]}
          />
        )}

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
          style={{ minWidth: 190, display: tab === 'problems' ? 'none' : undefined }}
          maxTagCount="responsive"
          options={SEVERITIES.map((sev) => ({
            value: sev,
            label: <SeverityTag value={sev} />,
          }))}
        />

        {offeredKinds.length > 1 && (
          <Segmented
            value={kind}
            onChange={(v) => setKind(v as ArtifactKind)}
            options={[
              { value: 'all', label: `All (${kindCounts.all.toLocaleString()})` },
              ...offeredKinds.map((k) => ({
                value: k,
                label: `${KIND_LABEL[k]} (${kindCounts[k].toLocaleString()})`,
              })),
            ]}
          />
        )}
      </div>

      {tab === 'problems' && (
        <ProblemsPanel
          problems={problems}
          scanned={reports.filter((r) => r.status === 'scanned').length}
          notApplicable={data.coverage.unsupported}
          repository={data.sync.repository}
        />
      )}

      {tab === 'vulnerabilities'
        ? (
          <>
            {detailRowsUnavailable && (
              <Alert
                type="warning"
                showIcon
                style={{ marginBottom: 12 }}
                message="Detailed vulnerability rows are unavailable"
                description={
                  `This release reports ${expectedTotal.toLocaleString()} vulnerabilities in summary counts, `
                  + 'but no per-CVE rows are stored for its artifacts any more. '
                  + 'Sync this release again to fetch them.'
                }
              />
            )}
            {grouping === 'unique'
              ? (
                <UniqueCveTable
                  groups={cveGroups}
                  state={data.state}
                  detailRowsUnavailable={detailRowsUnavailable}
                  scanUrlFor={scanUrlFor}
                />
              )
              : (
                <VulnerabilityTable
                  rows={filtered}
                  state={data.state}
                  detailRowsUnavailable={detailRowsUnavailable}
                  scanUrlFor={scanUrlFor}
                />
              )}
          </>
        )
        : tab === 'artifacts'
          ? (
            <Space direction="vertical" size={8} style={{ width: '100%' }}>
              {scannedImages < imageReports.length && (
                <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                  {scannedImages.toLocaleString()} of {imageReports.length.toLocaleString()} images
                  have a scan result. The vulnerability totals cover only those.
                  {data.coverage.missing > 0
                    && ` ${data.coverage.missing.toLocaleString()} are not in `
                      + `${data.sync.repository ?? 'the scanned repository'}.`}
                </Typography.Text>
              )}
              <ArtifactTable reports={filteredImageReports} />
            </Space>
          )
          : null}
    </Card>
  )
}

/** One reason the scanner gave, and every image it gave it for. */
type Problem = {
  status: SecurityReport['status']
  reason: string
  reports: SecurityReport[]
}

/**
 * Worst first, and "worst" is which one somebody can do something about.
 *
 * `unavailable` is a scanner or a network that failed and will probably work on
 * the next attempt. `not_scanned` needs somebody to index the image in Xray,
 * which is a different job on a different day.
 */
const PROBLEM_RANK: Record<string, number> = {
  unavailable: 3,
  not_scanned: 2,
  not_found: 1,
  disabled: 1,
}

function problemRank(status: string): number {
  return PROBLEM_RANK[status] ?? 0
}

const PROBLEM_ADVICE: Record<string, string> = {
  unavailable: 'A transient failure rather than a refusal. Sync again to retry these.',
  not_scanned: 'JFrog Xray holds these images but has not indexed them yet. Syncing again will report '
    + 'the same until it does.',
  not_found: 'This is a transfer to run, not a scan to wait for. Replicate the release, then sync again.',
  disabled: 'Enable a scanner on the repository these images are in.',
}

/**
 * The group's headline: what happened, to how many, and where.
 *
 * A sentence rather than a status word and a count. "25 images" beside a tag
 * reading "Not scanned" made a reader assemble the fact themselves, and the two
 * facts that matter most - not in the repository, and not yet scanned - read
 * almost identically that way.
 */
function problemHeadline(status: string, n: number, repository?: string): string {
  const images = `${n.toLocaleString()} ${n === 1 ? 'image' : 'images'}`
  const where = repository ? ` in ${repository}` : ''
  switch (status) {
    case 'not_found':
      return `${images} were not found${where}`
    case 'not_scanned':
      return `${images} have not been scanned by JFrog Xray`
    case 'unavailable':
      return `${images} could not be retrieved from JFrog Xray`
    case 'disabled':
      return `${images} are in a repository with no scanner`
    default:
      return `${images} have no scan result`
  }
}

/**
 * What went wrong, in as many lines as there were reasons.
 *
 * # Why this is not a table of artifacts
 *
 * Because the interesting axis is the REASON. A release that comes back 12%
 * scanned has two or three distinct sentences behind it and a couple of hundred
 * images distributed across them, and the reader's first question is which
 * sentence, not which image. The images are one click below each one, and
 * paginated, so a group of two hundred costs a row on screen rather than two
 * hundred.
 */
function ProblemsPanel({ problems, scanned, notApplicable, repository }: {
  problems: Problem[]
  scanned: number
  notApplicable: number
  repository?: string
}) {
  if (problems.length === 0) {
    return (
      <Space direction="vertical" size={4} style={{ padding: 24, width: '100%' }} align="center">
        <Typography.Text strong>No problems</Typography.Text>
        <Typography.Text type="secondary">
          Every image in this release returned a scan result.
        </Typography.Text>
      </Space>
    )
  }

  return (
    <Space direction="vertical" size={12} style={{ width: '100%' }}>
      <Typography.Text type="secondary" style={{ fontSize: 12 }}>
        {scanned.toLocaleString()} of {(scanned + problems.reduce((n, p) => n + p.reports.length, 0)).toLocaleString()}
        {' '}images were scanned.
        {notApplicable > 0 && ` The ${notApplicable.toLocaleString()} charts, files and signatures in `
          + 'this release are not listed: JFrog Xray does not scan them.'}
      </Typography.Text>

      <Collapse
        size="small"
        items={problems.map((p, i) => ({
          key: String(i),
          label: (
            <Space size={10} align="start" wrap={false} style={{ width: '100%' }}>
              <ScanStatusTag status={p.status} />
              <Space direction="vertical" size={0}>
                <Typography.Text strong>
                  {problemHeadline(p.status, p.reports.length, repository)}
                </Typography.Text>
                <Typography.Text type="secondary" style={{ fontSize: 12 }}>{p.reason}</Typography.Text>
              </Space>
            </Space>
          ),
          children: (
            <Space direction="vertical" size={8} style={{ width: '100%' }}>
              {PROBLEM_ADVICE[p.status] && (
                <Typography.Text type="secondary" style={{ fontSize: 12 }}>
                  {PROBLEM_ADVICE[p.status]}
                </Typography.Text>
              )}
              <Table<SecurityReport>
                size="small"
                rowKey={(r) => r.artifact.digest || r.artifact.name}
                dataSource={p.reports}
                pagination={p.reports.length > 10
                  ? { pageSize: 10, size: 'small', showSizeChanger: false }
                  : false}
                showHeader={false}
                columns={[
                  {
                    title: 'Artifact',
                    render: (_, r) => (
                      <Space direction="vertical" size={0}>
                        <Typography.Text style={{ fontFamily: mono, fontSize: 12 }}>
                          {r.artifact.display || r.artifact.name}
                        </Typography.Text>
                        <Typography.Text type="secondary" style={{ fontSize: 11 }}>
                          {[r.artifact.repository, r.artifact.kind].filter(Boolean).join(' · ')}
                        </Typography.Text>
                      </Space>
                    ),
                  },
                  {
                    title: 'Digest',
                    width: 190,
                    render: (_, r) => (
                      <Typography.Text type="secondary" style={{ fontFamily: mono, fontSize: 11 }}>
                        {r.artifact.digest ? r.artifact.digest.slice(0, 19) : '-'}
                      </Typography.Text>
                    ),
                  },
                ]}
              />
            </Space>
          ),
        }))}
      />
    </Space>
  )
}

/** One CVE, and every place it was found. */
type CveGroup = {
  key: string
  cve?: string
  id?: string
  severity: Severity
  fixable: boolean
  fixedIn: string[]
  summary?: string
  description?: string
  /*
   * The advisory's own facts, carried up from the findings.
   *
   * They belong to the CVE rather than to the occurrence - the score and the
   * publication date of an advisory do not change because it turned up in a
   * second image - so the group keeps the first one that has them. Without
   * these the grouped view's detail panel could show less than the flat view's
   * for the same advisory, which is the wrong way round.
   */
  cvssScore?: number
  cvssVector?: string
  references: string[]
  published?: string
  provider?: string
  policy?: string
  packages: string[]
  images: string[]
  rows: FlatFinding[]
}

/**
 * Collapses findings onto the CVE.
 *
 * The severity kept is the WORST any image graded it, and fixable is true if
 * any affected package has a fix. Both are the honest roll-up: a CVE that is
 * critical in one image and medium in another is a critical problem, and one
 * with a fix in three of four packages is work somebody can start.
 */
function groupByCve(findings: FlatFinding[]): CveGroup[] {
  const byKey = new Map<string, CveGroup>()

  for (const f of findings) {
    const key = f.cve || f.id || 'unknown'
    let g = byKey.get(key)
    if (!g) {
      g = {
        key,
        cve: f.cve,
        id: f.id,
        severity: f.severity,
        fixable: false,
        fixedIn: [],
        summary: f.summary,
        description: f.description,
        references: [],
        packages: [],
        images: [],
        rows: [],
      }
      byKey.set(key, g)
    }
    if (g.cvssScore === undefined && f.cvssScore !== undefined) g.cvssScore = f.cvssScore
    if (!g.cvssVector && f.cvssVector) g.cvssVector = f.cvssVector
    if (!g.published && f.published) g.published = f.published
    if (!g.provider && f.provider) g.provider = f.provider
    if (!g.policy && f.policy) g.policy = f.policy
    for (const r of f.references ?? []) {
      if (!g.references.includes(r)) g.references.push(r)
    }
    if (SEVERITIES.indexOf(f.severity) < SEVERITIES.indexOf(g.severity)) g.severity = f.severity
    if (f.fixable) g.fixable = true
    for (const v of f.fixedIn ?? []) {
      if (!g.fixedIn.includes(v)) g.fixedIn.push(v)
    }
    const pkg = f.component.name ? `${f.component.name}@${f.component.version ?? ''}` : ''
    if (pkg && !g.packages.includes(pkg)) g.packages.push(pkg)
    if (f.artifactName && !g.images.includes(f.artifactName)) g.images.push(f.artifactName)
    if (!g.summary) g.summary = f.summary
    if (!g.description) g.description = f.description
    g.rows.push(f)
  }

  return [...byKey.values()].sort((a, b) => (
    SEVERITIES.indexOf(a.severity) !== SEVERITIES.indexOf(b.severity)
      ? SEVERITIES.indexOf(a.severity) - SEVERITIES.indexOf(b.severity)
      : b.images.length - a.images.length
  ))
}

/**
 * One row per CVE, expandable into where it was found.
 *
 * # Why the expansion is a table and not a list of names
 *
 * Because the question underneath "CVE-2026-31789 is in four images" is
 * "which package, at which version, in which image, and is there a fix" - four
 * columns. A comma-separated list of image names answers a quarter of it and
 * sends the reader back to the flat view to find the rest.
 */
function UniqueCveTable({ groups, state, detailRowsUnavailable, scanUrlFor }: {
  groups: CveGroup[]
  state: PackageSecurityResponse['state']
  detailRowsUnavailable: boolean
  scanUrlFor: (name: string) => string | undefined
}) {
  const [open, setOpen] = useState<CveGroup | null>(null)

  return (
    <>
    <Table<CveGroup>
      size="small"
      rowKey={(g) => g.key}
      dataSource={groups}
      scroll={{ x: 'max-content' }}
      pagination={{ pageSize: 25, showSizeChanger: true, size: 'small' }}
      expandable={{
        expandedRowRender: (g) => (
          <Table<FlatFinding>
            size="small"
            rowKey={(r) => `${r.component.id}-${r.artifactName}-${r.artifactDigest ?? ''}`}
            dataSource={[...g.rows].sort((a, b) => Number(b.fixable) - Number(a.fixable))}
            pagination={g.rows.length > 10 ? { pageSize: 10, size: 'small', showSizeChanger: false } : false}
            columns={[
              {
                title: 'Package',
                width: 220,
                render: (_, r) => (
                  <ComponentCell name={r.component.name} version={r.component.version} type={r.component.type} />
                ),
              },
              {
                title: 'Image',
                width: 240,
                render: (_, r) => <ImageCell name={r.artifactName} tag={r.artifactTag} href={scanUrlFor(r.artifactName)} />,
              },
              {
                title: 'Fix',
                render: (_, r) => <FixCell fixable={r.fixable} fixedIn={r.fixedIn} />,
              },
            ]}
          />
        ),
        rowExpandable: (g) => g.rows.length > 0,
      }}
      locale={{
        emptyText: detailRowsUnavailable
          ? (
            <Space direction="vertical" size={4} style={{ padding: 24 }}>
              <Typography.Text strong>No detailed rows returned</Typography.Text>
              <Typography.Text type="secondary">
                Summary counts exist, but this response has no per-CVE detail rows to render.
              </Typography.Text>
            </Space>
          )
          : <FindingsEmpty status={state} />,
      }}
      columns={[
        {
          title: 'CVE',
          width: 160,
          render: (_, g) => (
            <a onClick={() => setOpen(g)} style={{ display: 'block' }}>
              <CveCell cve={g.cve} id={g.id} link />
            </a>
          ),
        },
        {
          title: 'Severity',
          width: 110,
          sorter: (a, b) => SEVERITIES.indexOf(a.severity) - SEVERITIES.indexOf(b.severity),
          defaultSortOrder: 'ascend',
          render: (_, g) => <SeverityTag value={g.severity} />,
        },
        {
          title: 'Affects',
          width: 170,
          sorter: (a, b) => a.images.length - b.images.length,
          render: (_, g) => (
            <Space direction="vertical" size={0}>
              <Typography.Text style={{ fontSize: 12 }}>
                <strong>{g.packages.length}</strong> {g.packages.length === 1 ? 'package' : 'packages'}
                {' in '}
                <strong>{g.images.length}</strong> {g.images.length === 1 ? 'image' : 'images'}
              </Typography.Text>
              <Typography.Text
                type="secondary"
                style={{ fontSize: 11, fontFamily: mono, maxWidth: 170 }}
                ellipsis={{ tooltip: g.packages.map((p) => p.split('@')[0]).join(', ') }}
              >
                {g.packages.map((p) => p.split('@')[0]).slice(0, 2).join(', ')}
                {g.packages.length > 2 && ` +${g.packages.length - 2}`}
              </Typography.Text>
            </Space>
          ),
        },
        {
          title: 'Fix',
          width: 130,
          render: (_, g) => <FixCell fixable={g.fixable} fixedIn={g.fixedIn} />,
        },
        {
          title: 'Description',
          width: 340,
          render: (_, g) => (
            <DescriptionCell
              summary={g.summary}
              description={g.description}
              title={g.cve || g.id}
              onOpen={() => setOpen(g)}
            />
          ),
        },
      ]}
    />
    <CveDetailDrawer group={open} scanUrlFor={scanUrlFor} onClose={() => setOpen(null)} />
    </>
  )
}

/**
 * Everything known about one CVE, beside the table rather than over it.
 *
 * # Why a drawer and not a wider Description column
 *
 * Because the description is eight paragraphs for some advisories and one line
 * for others, and a column sized for the first wastes the width of the page on
 * the second. Clamped to two lines in the table with the rest here, the table
 * stays a table and the prose gets the room it needs.
 */
function CveDetailDrawer({ group, scanUrlFor, onClose }: {
  group: CveGroup | null
  scanUrlFor: (name: string) => string | undefined
  onClose: () => void
}) {
  // Fixable first: that's the row someone can act on today.
  const rows = useMemo(
    () => (group ? [...group.rows].sort((a, b) => Number(b.fixable) - Number(a.fixable)) : []),
    [group],
  )
  return (
    <AdvisoryDrawer
      open={Boolean(group)}
      onClose={onClose}
      identifier={group?.cve || group?.id}
      alternateId={group?.cve && group?.id && group.id !== group.cve ? group.id : undefined}
      severity={group?.severity}
      fixable={group?.fixable}
      fixedIn={group?.fixedIn}
      summary={group?.summary}
      description={group?.description}
      cvssScore={group?.cvssScore}
      cvssVector={group?.cvssVector}
      published={group?.published}
      provider={group?.provider}
      policy={group?.policy}
      references={group?.references}
      subtitle={group
        ? `${group.packages.length} ${group.packages.length === 1 ? 'package' : 'packages'} in `
          + `${group.images.length} ${group.images.length === 1 ? 'image' : 'images'}`
        : undefined}
    >
      {group && (
        <Section
          title={`Impacted Packages - ${group.packages.length} `
            + `${group.packages.length === 1 ? 'package' : 'packages'} in ${group.images.length} `
            + `${group.images.length === 1 ? 'image' : 'images'}`}
        >
          <Table<FlatFinding>
            size="small"
            rowKey={(r) => `${r.component.id}-${r.artifactName}-${r.artifactDigest ?? ''}`}
            dataSource={rows}
            pagination={rows.length > 12
              ? { pageSize: 12, size: 'small', showSizeChanger: false }
              : false}
            columns={[
              {
                title: 'Package',
                render: (_, r) => (
                  <ComponentCell name={r.component.name} version={r.component.version} type={r.component.type} />
                ),
              },
              {
                title: 'Image',
                render: (_, r) => <ImageCell name={r.artifactName} tag={r.artifactTag} href={scanUrlFor(r.artifactName)} />,
              },
              // Where in the image the scanner found it, when it says. Offered
              // only when something has one: a column of dashes is worse than
              // the column not being there.
              ...(rows.some((r) => r.component.path)
                ? [{
                  title: 'Path',
                  render: (_: unknown, r: FlatFinding) => (
                    r.component.path
                      ? (
                        <Typography.Text
                          style={{ fontFamily: mono, fontSize: 11 }}
                          ellipsis={{ tooltip: r.component.path }}
                        >
                          {r.component.path}
                        </Typography.Text>
                      )
                      : <Typography.Text type="secondary">-</Typography.Text>
                  ),
                }]
                : []),
              {
                title: 'Fix',
                width: 140,
                render: (_, r) => <FixCell fixable={r.fixable} fixedIn={r.fixedIn} />,
              },
            ]}
          />
        </Section>
      )}
    </AdvisoryDrawer>
  )
}

/**
 * One ROW's advisory - the same panel, for the view that does not group.
 *
 * # Why the flat view needs its own
 *
 * Because the grouped panel answers "where else is this", and in the flat view
 * that question is already answered by the row: this occurrence, this package,
 * this image. Reusing the grouped panel here would list every other image the
 * CVE appears in, which is the reader deliberately having left the grouped view
 * to get away from.
 *
 * So the shape is the same and the last section differs: what this finding is
 * against, rather than everywhere the advisory turns up.
 */
function FindingDetailDrawer({ finding, scanUrlFor, onClose }: {
  finding: FlatFinding | null
  scanUrlFor: (name: string) => string | undefined
  onClose: () => void
}) {
  return (
    <AdvisoryDrawer
      open={Boolean(finding)}
      onClose={onClose}
      identifier={finding?.cve || finding?.id}
      alternateId={finding?.cve && finding?.id && finding.id !== finding.cve ? finding.id : undefined}
      severity={finding?.severity}
      fixable={finding?.fixable}
      fixedIn={finding?.fixedIn}
      summary={finding?.summary}
      description={finding?.description}
      cvssScore={finding?.cvssScore}
      cvssVector={finding?.cvssVector}
      published={finding?.published}
      provider={finding?.provider}
      policy={finding?.policy}
      references={finding?.references}
      subtitle={finding ? `${finding.component.name} in ${finding.artifactName}` : undefined}
    >
      {finding && (
        <Section title="This finding">
          <Descriptions
            column={{ xs: 1, sm: 2 }}
            size="small"
            bordered
            items={[
              {
                key: 'package',
                label: 'Package',
                children: (
                  <ComponentCell
                    name={finding.component.name}
                    version={finding.component.version}
                    type={finding.component.type}
                  />
                ),
              },
              {
                key: 'image',
                label: 'Image',
                children: (
                  <ImageCell
                    name={finding.artifactName}
                    tag={finding.artifactTag}
                    href={scanUrlFor(finding.artifactName)}
                  />
                ),
              },
              {
                key: 'fix',
                label: 'Fix',
                children: <FixCell fixable={finding.fixable} fixedIn={finding.fixedIn} />,
              },
              {
                key: 'component-id',
                label: 'Component id',
                children: (
                  <Typography.Text copyable style={{ fontFamily: mono, fontSize: 12 }}>
                    {finding.component.id}
                  </Typography.Text>
                ),
              },
              // The scanner says where inside the image it found the package
              // when it knows, and that is what makes a finding checkable
              // against the image rather than taken on trust.
              ...(finding.component.path
                ? [{
                  key: 'path',
                  label: 'Path',
                  span: 2,
                  children: (
                    <Typography.Text copyable style={{ fontFamily: mono, fontSize: 12 }}>
                      {finding.component.path}
                    </Typography.Text>
                  ),
                }]
                : []),
              ...(finding.artifactDigest
                ? [{
                  key: 'digest',
                  label: 'Image digest',
                  span: 2,
                  children: (
                    <Typography.Text copyable style={{ fontFamily: mono, fontSize: 12 }}>
                      {finding.artifactDigest}
                    </Typography.Text>
                  ),
                }]
                : []),
            ]}
          />
        </Section>
      )}
    </AdvisoryDrawer>
  )
}

/**
 * The advisory panel both views open.
 *
 * # What was wrong with the one before it
 *
 * Three things, and they were all about the shape rather than the content. The
 * identifier, the severity and the fix sat in one wrapping row of unlike
 * things - a tag, a two-line cell, a monospace id - which read as a cluster
 * rather than as facts. The description was a paragraph in a vertical Space and
 * stopped well short of the panel's right edge, so the widest thing on screen
 * used the least of it. And what the scanner had said beyond the prose - the
 * CVSS score and vector, when the advisory was published, which watch flagged
 * it, the advisory links - was simply not shown, in a panel called "everything
 * about this CVE".
 *
 * So: a header that states the identity once, a grid of the facts, then the
 * prose across the full width, then the links, then whatever the caller has to
 * add about where it was found.
 */
function AdvisoryDrawer({
  open, onClose, identifier, alternateId, severity, fixable, fixedIn, summary, description,
  cvssScore, cvssVector, published, provider, policy, references, subtitle, children,
}: {
  open: boolean
  onClose: () => void
  identifier?: string
  alternateId?: string
  severity?: Severity
  fixable?: boolean
  fixedIn?: string[]
  summary?: string
  description?: string
  cvssScore?: number
  cvssVector?: string
  published?: string
  provider?: string
  policy?: string
  references?: string[]
  subtitle?: string
  children?: ReactNode
}) {
  const prose = description || summary
  const facts = [
    severity ? { key: 'severity', label: 'Severity', children: <SeverityTag value={severity} /> } : null,
    {
      key: 'fix',
      label: 'Fix',
      children: <FixCell fixable={Boolean(fixable)} fixedIn={fixedIn} />,
    },
    cvssScore !== undefined && cvssScore > 0
      ? { key: 'cvss', label: 'CVSS score', children: <Typography.Text strong>{cvssScore}</Typography.Text> }
      : null,
    published
      ? {
        key: 'published',
        label: 'Advisory published',
        children: <Typography.Text>{formatAbsolute(published) ?? published}</Typography.Text>,
      }
      : null,
    provider
      ? {
        key: 'provider',
        label: 'Reported by',
        children: <Typography.Text>{provider === 'jfrog-xray' ? 'JFrog Xray' : provider}</Typography.Text>,
      }
      : null,
    // Xray's own watch or policy. Informational, and the only thing on this
    // panel that says why a finding is being shown to THIS organisation.
    policy ? { key: 'policy', label: 'Policy', children: <Typography.Text>{policy}</Typography.Text> } : null,
    cvssVector
      ? {
        key: 'vector',
        label: 'CVSS vector',
        span: 2,
        children: (
          <Typography.Text copyable style={{ fontFamily: mono, fontSize: 12 }}>{cvssVector}</Typography.Text>
        ),
      }
      : null,
    fixedIn && fixedIn.length > 0
      ? {
        key: 'fixed-in',
        label: 'Fixed in',
        span: 2,
        children: (
          <Space size={[6, 6]} wrap>
            {fixedIn.map((v) => (
              <Tag key={v} style={{ marginInlineEnd: 0, fontFamily: mono }}>{v}</Tag>
            ))}
          </Space>
        ),
      }
      : null,
  ].filter(Boolean) as { key: string; label: string; span?: number; children: ReactNode }[]

  return (
    <Drawer
      // Wide, because the widest thing in here is an advisory's prose and the
      // panel exists to give it room. Bounded by the viewport so it never
      // becomes the whole screen on a laptop.
      width="min(920px, 92vw)"
      open={open}
      onClose={onClose}
      title={
        <Space direction="vertical" size={0}>
          <Space size={10} align="center" wrap>
            <Typography.Text strong style={{ fontFamily: mono, fontSize: 15 }}>
              {identifier ?? 'Vulnerability'}
            </Typography.Text>
            {severity && <SeverityTag value={severity} />}
            {alternateId && (
              <Typography.Text type="secondary" style={{ fontFamily: mono, fontSize: 12 }}>
                {alternateId}
              </Typography.Text>
            )}
          </Space>
          {subtitle && (
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>{subtitle}</Typography.Text>
          )}
        </Space>
      }
      extra={
        identifier && (
          <Space size={8}>
            <Button
              size="small"
              icon={<CopyOutlined />}
              onClick={() => void navigator.clipboard?.writeText(
                prose ? `${identifier}\n\n${prose}` : identifier)}
            >
              Copy
            </Button>
            {/*
              The public record, for an identifier that has one. A scanner's
              own id has no page anywhere but the scanner, and the image rows
              below already link there.
            */}
            {identifier.toUpperCase().startsWith('CVE-') && (
              <Button
                size="small"
                icon={<ExportOutlined />}
                href={`https://nvd.nist.gov/vuln/detail/${encodeURIComponent(identifier)}`}
                target="_blank"
                rel="noreferrer"
              >
                NVD
              </Button>
            )}
          </Space>
        )
      }
    >
      <Space direction="vertical" size={20} style={{ width: '100%' }}>
        <Descriptions column={{ xs: 1, sm: 2 }} size="small" bordered items={facts} />

        <Section title="Description">
          {prose
            ? (
              <Typography.Paragraph>
                {prose}
              </Typography.Paragraph>
            )
            : (
              <Typography.Text type="secondary">
                The scanner supplied no description for this advisory.
              </Typography.Text>
            )}
          {/*
            Both, when they differ. The summary is the scanner's one-line
            headline and the description is the advisory - a panel that showed
            only the longer one dropped the sentence somebody was scanning for.
          */}
          {summary && description && summary !== description && (
            <Typography.Text type="secondary" style={{ fontSize: 12, display: 'block', marginTop: 10 }}>
              Summary: {summary}
            </Typography.Text>
          )}
        </Section>

        {references && references.length > 0 && (
          <Section title="References">
            <Space direction="vertical" size={4} style={{ width: '100%' }}>
              {references.map((r) => (
                <a key={r} href={r} target="_blank" rel="noreferrer" style={{ fontSize: 13, wordBreak: 'break-all' }}>
                  {r} <ExportOutlined style={{ fontSize: 10 }} />
                </a>
              ))}
            </Space>
          </Section>
        )}

        {children}
      </Space>
    </Drawer>
  )
}

/** One labelled block of the detail panel. */
function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div style={{ width: '100%' }}>
      <Typography.Text strong style={{ display: 'block', marginBottom: 8 }}>{title}</Typography.Text>
      {children}
    </div>
  )
}

/**
 * An image, linked to the scanner's own page for it where there is one.
 *
 * The link comes from the server, which knows the configured platform host and
 * repository. Building it here would mean the interface holding a copy of the
 * deployment's topology.
 */
function ImageCell({ name, tag, href }: { name: string; tag?: string; href?: string }) {
  return (
    <Space direction="vertical" size={0}>
      {href
        ? (
          <a href={href} target="_blank" rel="noreferrer" style={{ fontFamily: mono, fontSize: 12 }}>
            {name} <ExportOutlined style={{ fontSize: 10 }} />
          </a>
        )
        : <Typography.Text style={{ fontFamily: mono, fontSize: 12 }}>{name}</Typography.Text>}
      {tag && (
        <Typography.Text type="secondary" style={{ fontSize: 11, fontFamily: mono }}>{tag}</Typography.Text>
      )}
    </Space>
  )
}

function VulnerabilityTable({
  rows,
  state,
  detailRowsUnavailable,
  scanUrlFor,
}: {
  rows: FlatFinding[]
  state: PackageSecurityResponse['state']
  detailRowsUnavailable: boolean
  scanUrlFor: (name: string) => string | undefined
}) {
  /*
   * The same gesture as the grouped view, because it is the same table to a
   * reader: switching the segment above it changed what a click on a CVE did -
   * from "open the advisory" to nothing at all - which reads as a broken page
   * rather than as a different view.
   */
  const [open, setOpen] = useState<FlatFinding | null>(null)

  return (
    <>
    <Table<FlatFinding>
      size="small"
      rowKey={(r) => `${r.cve ?? r.id}-${r.component.id}-${r.artifactName}`}
      dataSource={rows}
      scroll={{ x: 'max-content' }}
      pagination={{ pageSize: 25, showSizeChanger: true, size: 'small' }}
      locale={{
        emptyText: detailRowsUnavailable
          ? (
            <Space direction="vertical" size={4} style={{ padding: 24 }}>
              <Typography.Text strong>No detailed rows returned</Typography.Text>
              <Typography.Text type="secondary">
                Summary counts exist, but this response has no per-CVE detail rows to render.
              </Typography.Text>
            </Space>
          )
          : <FindingsEmpty status={state} />,
      }}
      columns={[
        {
          title: 'CVE',
          width: 150,
          render: (_, r) => (
            <a onClick={() => setOpen(r)} style={{ display: 'block' }}>
              <CveCell cve={r.cve} id={r.id} link />
            </a>
          ),
        },
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
          render: (_, r) => <ImageCell name={r.artifactName} tag={r.artifactTag} href={scanUrlFor(r.artifactName)} />,
        },
        { title: 'Fix', width: 130, render: (_, r) => <FixCell fixable={r.fixable} fixedIn={r.fixedIn} /> },
        {
          /*
            Narrow enough that the six columns fit the card at a laptop width.
            The description was 520px wide, which pushed the table 180px past
            its own card and left every row's last words behind a horizontal
            scrollbar. Two clamped lines with the rest in a popover says as
            much, in a table that ends where the card does.
          */
          title: 'Description',
          width: 340,
          render: (_, r) => (
            <DescriptionCell
              summary={r.summary}
              description={r.description}
              title={r.cve || r.id}
              onOpen={() => setOpen(r)}
            />
          ),
        },
      ]}
    />
    <FindingDetailDrawer finding={open} scanUrlFor={scanUrlFor} onClose={() => setOpen(null)} />
    </>
  )
}

/**
 * The images of a release, with what is wrong in each.
 *
 * Every image appears, including the ones with nothing to report and the ones
 * nobody scanned. A table listing only the images with findings would be a
 * table where an unscanned image is invisible.
 */
function ArtifactTable({ reports }: { reports: SecurityReport[] }) {
  /*
   * The statuses this table actually contains, in the order it shows them.
   *
   * Offered rather than hardcoded. A filter for "Unavailable" on a release with
   * no unavailable images can only ever empty the table, and a menu of four
   * options where two do nothing teaches a reader to distrust the two that do.
   */
  const statusFilters = useMemo(() => {
    const seen = new Map<string, string>()
    for (const r of reports) {
      if (!seen.has(r.status)) seen.set(r.status, r.statusLabel || r.status)
    }
    return [...seen].map(([value, text]) => ({ text, value }))
  }, [reports])

  // Same gesture as a CVE row: click the name, get everything about it beside the table.
  const [open, setOpen] = useState<SecurityReport | null>(null)

  return (
    <>
    <Table<SecurityReport>
      size="small"
      rowKey={(r) => r.artifact.digest || r.artifact.name}
      dataSource={reports}
      scroll={{ x: 'max-content' }}
      pagination={{ pageSize: 20, showSizeChanger: true, size: 'small' }}
      columns={[
        {
          title: 'Image',
          render: (_, r) => (
            <Space direction="vertical" size={0}>
              <a
                onClick={() => setOpen(r)}
                style={{
                  fontFamily: mono,
                  display: 'block',
                  color: c.brand,
                  textDecoration: 'underline',
                  textDecorationStyle: 'dotted',
                  textUnderlineOffset: 3,
                }}
              >
                {r.artifact.display || r.artifact.name}
              </a>
              {/* Only the platform, since this table is already scoped to images. */}
              {r.artifact.platform && (
                <Typography.Text type="secondary" style={{ fontSize: 11 }}>
                  {r.artifact.platform}
                </Typography.Text>
              )}
            </Space>
          ),
        },
        {
          title: 'Scan',
          width: 130,
          filters: statusFilters.length > 1 ? statusFilters : undefined,
          onFilter: (value, r) => r.status === value,
          render: (_, r) => (
            <Tooltip title={r.message}>
              <span><ScanStatusTag status={r.status} /></span>
            </Tooltip>
          ),
        },
        {
          title: 'Vulnerabilities',
          width: 300,
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
                      {SEVERITIES.slice(0, 3).map((s) => (
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
    <ImageDetailDrawer report={open} onClose={() => setOpen(null)} />
    </>
  )
}

/**
 * Everything known about one image, beside the table rather than over it.
 *
 * The same shape as the CVE drawer, the other way round: instead of one
 * advisory and every place it turns up, this is one image and every advisory
 * it has. A reader who opened the Images tab is asking "what is wrong with
 * THIS one", and the vulnerabilities table below answers it without a trip
 * back to the CVE view to filter by image.
 */
function ImageDetailDrawer({ report, onClose }: {
  report: SecurityReport | null
  onClose: () => void
}) {
  const [finding, setFinding] = useState<FlatFinding | null>(null)

  // Fixable first, same as everywhere else this question comes up.
  const rows = useMemo<FlatFinding[]>(() => {
    if (!report) return []
    return (report.findings ?? [])
      .map((f) => ({
        ...f,
        artifactName: report.artifact.name,
        artifactTag: report.artifact.tag,
        artifactDigest: report.artifact.digest,
      }))
      .sort((a, b) => Number(b.fixable) - Number(a.fixable))
  }, [report])

  const scanUrlFor = () => report?.scanUrl

  return (
    <>
    <Drawer
      width="min(920px, 92vw)"
      open={Boolean(report)}
      onClose={onClose}
      title={
        <Space direction="vertical" size={0}>
          <Space size={10} align="center" wrap>
            <Typography.Text strong style={{ fontFamily: mono, fontSize: 15 }}>
              {report?.artifact.display || report?.artifact.name}
            </Typography.Text>
            {report && <ScanStatusTag status={report.status} />}
          </Space>
          {report?.artifact.tag && (
            <Typography.Text type="secondary" style={{ fontFamily: mono, fontSize: 12 }}>
              {report.artifact.tag}
            </Typography.Text>
          )}
        </Space>
      }
      extra={
        report?.scanUrl && (
          <Button size="small" icon={<ExportOutlined />} href={report.scanUrl} target="_blank" rel="noreferrer">
            JFrog Xray
          </Button>
        )
      }
    >
      {report && (
        <Space direction="vertical" size={20} style={{ width: '100%' }}>
          <Descriptions
            column={{ xs: 1, sm: 2 }}
            size="small"
            bordered
            items={[
              {
                key: 'vulnerabilities',
                label: 'Vulnerabilities',
                children: report.status === 'scanned'
                  ? <Typography.Text strong>{report.counts.total.toLocaleString()}</Typography.Text>
                  : <Typography.Text type="secondary">-</Typography.Text>,
              },
              {
                key: 'fixable',
                label: 'Fixable',
                children: report.status === 'scanned'
                  ? <Typography.Text strong>{report.counts.fixable.toLocaleString()}</Typography.Text>
                  : <Typography.Text type="secondary">-</Typography.Text>,
              },
              {
                key: 'source',
                label: 'Source',
                children: (
                  <Typography.Text>
                    {report.provider === 'jfrog-xray' ? 'JFrog Xray' : (report.provider || '-')}
                  </Typography.Text>
                ),
              },
              ...(report.message
                ? [{ key: 'message', label: 'Message', span: 2, children: <Typography.Text>{report.message}</Typography.Text> }]
                : []),
              ...(report.artifact.digest
                ? [{
                  key: 'digest',
                  label: 'Digest',
                  span: 2,
                  children: (
                    <Typography.Text copyable style={{ fontFamily: mono, fontSize: 12 }}>
                      {report.artifact.digest}
                    </Typography.Text>
                  ),
                }]
                : []),
            ]}
          />

          <Section title={`Vulnerabilities - ${rows.length.toLocaleString()}`}>
            <Table<FlatFinding>
              size="small"
              rowKey={(r) => `${r.cve ?? r.id}-${r.component.id}`}
              dataSource={rows}
              pagination={rows.length > 12 ? { pageSize: 12, size: 'small', showSizeChanger: false } : false}
              locale={{
                emptyText: (
                  <Typography.Text type="secondary">
                    {report.status === 'scanned' ? 'No vulnerabilities found.' : 'This image has no scan result.'}
                  </Typography.Text>
                ),
              }}
              columns={[
                {
                  title: 'CVE',
                  width: 150,
                  render: (_, r) => (
                    <a onClick={() => setFinding(r)} style={{ display: 'block' }}>
                      <CveCell cve={r.cve} id={r.id} link />
                    </a>
                  ),
                },
                {
                  title: 'Severity',
                  width: 110,
                  sorter: (a, b) => SEVERITIES.indexOf(a.severity) - SEVERITIES.indexOf(b.severity),
                  defaultSortOrder: 'ascend',
                  render: (_, r) => <SeverityTag value={r.severity} />,
                },
                {
                  title: 'Package',
                  render: (_, r) => (
                    <ComponentCell name={r.component.name} version={r.component.version} type={r.component.type} />
                  ),
                },
                { title: 'Fix', width: 130, render: (_, r) => <FixCell fixable={r.fixable} fixedIn={r.fixedIn} /> },
                {
                  title: 'Description',
                  render: (_, r) => (
                    <DescriptionCell
                      summary={r.summary}
                      description={r.description}
                      title={r.cve || r.id}
                      onOpen={() => setFinding(r)}
                    />
                  ),
                },
              ]}
            />
          </Section>
        </Space>
      )}
    </Drawer>
    <FindingDetailDrawer finding={finding} scanUrlFor={scanUrlFor} onClose={() => setFinding(null)} />
    </>
  )
}

