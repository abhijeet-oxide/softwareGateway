import type { ReactNode } from 'react'
import { Alert, Col, Progress, Row, Space, Tooltip, Typography } from 'antd'
import { StatusPill, StatTile, c, mono } from '../uikit'
import type { PillTone } from '../uikit'
import { formatRelative } from '../domain/format'
import type {
  ComplianceChart, ComplianceCounts, ComplianceDeterminacy, ComplianceHelm,
  ComplianceOutcome, ComplianceProgress, ComplianceRun, ComplianceVerdict,
} from '../api/types'

/**
 * The small pieces of the compliance interface.
 *
 * # The one rule everything here follows
 *
 * A release nobody has checked and a release that passed must never look the
 * same. That is the whole reason this feature exists, and an interface is where
 * the distinction is easiest to lose: an empty list, a blank cell and a zero
 * all render as "nothing wrong" to somebody scanning a page.
 *
 * So absence is drawn explicitly, `inconclusive` is drawn as its own state
 * rather than as a milder failure, and the number of checks that could not be
 * decided is on screen beside the ones that could.
 */

// ---------------------------------------------------------------------------
// The verdict
// ---------------------------------------------------------------------------

const VERDICT_TONE: Record<string, PillTone> = {
  pass: 'ok',
  conditional: 'review',
  fail: 'danger',
  inconclusive: 'pending',
}

/**
 * The answer, in one word.
 *
 * `inconclusive` is `pending` rather than `danger` on purpose: nothing is known
 * to be wrong, and the honest reading is "we could not tell", which is a
 * different thing to act on than a failure.
 */
export function VerdictPill({ verdict, label, size }: {
  verdict?: ComplianceVerdict
  label?: string
  size?: 'small'
}) {
  if (!verdict) {
    return (
      <Tooltip title="No compliance check has been run against this release.">
        <StatusPill tone="neutral" dot={false}>Not checked</StatusPill>
      </Tooltip>
    )
  }
  return (
    <StatusPill tone={VERDICT_TONE[verdict] ?? 'neutral'}>
      <span style={size === 'small' ? { fontSize: 12 } : undefined}>{label ?? verdict}</span>
    </StatusPill>
  )
}

// ---------------------------------------------------------------------------
// Outcomes and determinacy
// ---------------------------------------------------------------------------

const OUTCOME_TONE: Record<ComplianceOutcome, PillTone> = {
  pass: 'ok',
  fail: 'danger',
  skip: 'neutral',
  error: 'pending',
  waived: 'review',
}

export function OutcomePill({ outcome, label }: { outcome: ComplianceOutcome; label?: string }) {
  return <StatusPill tone={OUTCOME_TONE[outcome] ?? 'neutral'}>{label ?? outcome}</StatusPill>
}

const SEVERITY_TONE: Record<string, PillTone> = {
  block: 'danger',
  warn: 'review',
  info: 'neutral',
}

const SEVERITY_WORD: Record<string, string> = {
  block: 'Blocking',
  warn: 'Warning',
  info: 'Info',
}

export function CheckSeverityTag({ severity }: { severity: string }) {
  return (
    <StatusPill tone={SEVERITY_TONE[severity] ?? 'neutral'} dot={false}>
      {SEVERITY_WORD[severity] ?? severity}
    </StatusPill>
  )
}

/**
 * Whose problem this is.
 *
 * The single most useful thing on a finding after the address: a value the
 * chart template fixes is the vendor's to change, and one a values file can
 * override is a question for whoever writes those values. A report that does
 * not draw the distinction sends both to the vendor, and the vendor learns to
 * discount the whole report.
 *
 * `unknown` is shown rather than hidden. It means the second render did not
 * happen, and a reader who cannot see that would take the silence for "fixed".
 */
export function DeterminacyTag({ determinacy, label }: {
  determinacy?: ComplianceDeterminacy
  label?: string
}) {
  if (!determinacy || determinacy === 'na') return null
  const tone: PillTone =
    determinacy === 'fixed' ? 'danger'
      : determinacy === 'configurable' ? 'review'
        : 'neutral'
  const title =
    determinacy === 'fixed'
      ? 'The chart template sets this value. No values file can change it, so the vendor has to.'
      : determinacy === 'configurable'
        ? 'This came from the chart’s default values. A site values file can override it, so this may be a question for your own configuration rather than a defect.'
        : 'The second, perturbed render did not run for this chart, so whether a values file could change this was not established.'
  return (
    <Tooltip title={title}>
      <StatusPill tone={tone} dot={false}>
        <span style={{ fontSize: 11 }}>{label ?? determinacy}</span>
      </StatusPill>
    </Tooltip>
  )
}

// ---------------------------------------------------------------------------
// The summary
// ---------------------------------------------------------------------------

/**
 * The four numbers, in the order somebody reads them.
 *
 * Blocking, warning, undecided, passed. The third is not optional and not
 * folded into the others: a release with three hundred passes and one
 * undecided check has not been shown to comply with anything, and a summary
 * that omitted it would draw that release as clean.
 */
export function ComplianceSummary({ counts, onSelect }: {
  counts: ComplianceCounts
  onSelect?: (what: 'blocking' | 'warning' | 'error' | 'pass') => void
}) {
  const tiles: Array<{
    key: 'blocking' | 'warning' | 'error' | 'pass'
    label: string
    value: number
    sub: string
    colour?: string
  }> = [
    {
      key: 'blocking', label: 'Blocking', value: counts.blocking,
      sub: 'must be fixed before this ships', colour: c.danger,
    },
    {
      key: 'warning', label: 'Warnings', value: counts.warning,
      sub: 'worth a conversation with the vendor', colour: c.review,
    },
    {
      key: 'error', label: 'Could not be checked', value: counts.error,
      sub: counts.error > 0 ? 'so this result is incomplete' : 'every check was decided',
      colour: counts.error > 0 ? c.pending : undefined,
    },
    {
      key: 'pass', label: 'Passed', value: counts.pass,
      sub: 'checks satisfied', colour: c.ok,
    },
  ]

  // Four across on a normal screen, two on a narrow one. The grid rather than
  // a flex row because every other summary in this application uses it, and a
  // panel that reflows differently from the one beside it reads as a different
  // product.
  return (
    <Row gutter={[12, 12]}>
      {tiles.map((t) => (
        <Col key={t.key} xs={12} lg={6}>
          <StatTile
            label={t.label}
            value={
              <span style={t.colour ? { color: t.colour } : undefined}>
                {t.value.toLocaleString()}
              </span>
            }
            sub={t.sub}
            onClick={onSelect ? () => onSelect(t.key) : undefined}
          />
        </Col>
      ))}
    </Row>
  )
}

// ---------------------------------------------------------------------------
// Notices
// ---------------------------------------------------------------------------

/**
 * The caveat that has to be on screen before the numbers are.
 *
 * A run that could not decide some of its checks is inconclusive, and the
 * reason is almost always that charts did not render. Without this the reader
 * sees a short list of findings and concludes the release is nearly clean.
 */
export function InconclusiveNotice({ run, charts, onShowUndecided }: {
  run: ComplianceRun
  charts?: ComplianceChart[]
  onShowUndecided?: () => void
}) {
  if (run.counts.error === 0) return null
  const broken = (charts ?? []).filter((ch) => ch.status !== 'ok')

  return (
    <Alert
      type="warning"
      showIcon
      message={`${run.counts.error.toLocaleString()} check${run.counts.error === 1 ? '' : 's'} could not be decided`}
      description={
        <Space direction="vertical" size={6} style={{ width: '100%' }}>
          <span>
            This release has <strong>not</strong> been shown to meet the standards it was checked
            against. The findings below are what could be established; they are not the whole
            picture.
          </span>
          {broken.length > 0 && (
            <span>
              {broken.length} chart{broken.length === 1 ? '' : 's'} did not render:{' '}
              {broken.slice(0, 3).map((ch) => (
                <Tooltip key={ch.name + ch.version} title={ch.error}>
                  <code style={{ fontFamily: mono, marginRight: 6 }}>{ch.name}</code>
                </Tooltip>
              ))}
              {broken.length > 3 && <span>and {broken.length - 3} more</span>}
            </span>
          )}
          {onShowUndecided && (
            <Typography.Link onClick={onShowUndecided}>
              Show what could not be checked
            </Typography.Link>
          )}
        </Space>
      }
    />
  )
}

/**
 * Why every rendered check came back undecided.
 *
 * Without this the tab is a wall of "could not be checked" with no explanation
 * on screen, and the reader's next move is to file a bug against this tool.
 */
export function HelmMissingNotice({ helm }: { helm: ComplianceHelm }) {
  if (helm.available) return null
  return (
    <Alert
      type="info"
      showIcon
      message="Charts cannot be rendered on this Coordinator"
      description={
        <>
          The <code>helm</code> binary is not available, so Helm charts cannot be turned into
          Kubernetes objects and every check that needs them reports as undecided - never as a
          pass. Install helm on the Coordinator, or set{' '}
          <code>coordinator.compliance.helmBinary</code>.
          {helm.reason && (
            <div style={{ marginTop: 6, color: c.text3, fontFamily: mono, fontSize: 12 }}>
              {helm.reason}
            </div>
          )}
        </>
      }
    />
  )
}

/** A report the engine cut short. A truncated report LOOKS complete. */
export function TruncatedNotice({ run }: { run: ComplianceRun }) {
  if (!run.truncated) return null
  return (
    <Alert
      type="warning"
      showIcon
      message="This report is incomplete"
      description={
        'The run produced more results than it is allowed to store, so the list was cut short. '
        + 'Raise coordinator.compliance.maxResults, or check fewer charts at a time.'
      }
    />
  )
}

/** The last attempt failed. Not the same as never having been checked. */
export function RunFailedNotice({ run }: { run: ComplianceRun }) {
  if (run.state !== 'failed' && run.state !== 'cancelled') return null
  return (
    <Alert
      type={run.state === 'cancelled' ? 'info' : 'error'}
      showIcon
      message={run.state === 'cancelled' ? 'The last check was cancelled' : 'The last check failed'}
      description={
        <Space direction="vertical" size={4} style={{ width: '100%' }}>
          {run.error && <span style={{ fontFamily: mono, fontSize: 12 }}>{run.error}</span>}
          <span style={{ color: c.text3 }}>
            This release has no result from that attempt. It is not the same as a release that
            passed, and it is not the same as one nobody has checked.
          </span>
        </Space>
      }
    />
  )
}

// ---------------------------------------------------------------------------
// Live progress
// ---------------------------------------------------------------------------

/**
 * What a run is doing right now.
 *
 * The bar is of the CURRENT stage, and the stage is named beside it: "12 of 97
 * charts rendered" is a number somebody can reason about, and one percentage
 * across four stages of wildly different cost is not.
 */
export function ComplianceProgressPanel({ progress, onCancel, cancelling }: {
  progress: ComplianceProgress
  onCancel?: () => void
  cancelling?: boolean
}) {
  const percent = progress.total > 0
    ? Math.min(100, Math.round((progress.done / progress.total) * 100))
    : 0

  return (
    <Space direction="vertical" size={10} style={{ width: '100%', padding: '4px 0' }}>
      <Space style={{ width: '100%', justifyContent: 'space-between' }} align="start">
        <Space direction="vertical" size={2}>
          <Typography.Text strong>{progress.label}</Typography.Text>
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            {progress.total > 0
              ? `${progress.done.toLocaleString()} of ${progress.total.toLocaleString()}`
              : 'starting'}
            {progress.note && <span style={{ fontFamily: mono }}> · {progress.note}</span>}
          </Typography.Text>
        </Space>
        {onCancel && (
          <Typography.Link disabled={cancelling} onClick={onCancel}>
            {cancelling ? 'Stopping…' : 'Stop'}
          </Typography.Link>
        )}
      </Space>
      <Progress
        percent={percent}
        status="active"
        showInfo={false}
        strokeColor={c.brand}
        trailColor={c.track}
      />
    </Space>
  )
}

// ---------------------------------------------------------------------------
// Provenance
// ---------------------------------------------------------------------------

/**
 * What produced this result.
 *
 * On screen because a finding a vendor disputes has to be re-derivable, and
 * re-deriving it means knowing which rulebook, which helm and which Kubernetes
 * version were used. A report that cannot state them is an opinion.
 */
export function RunProvenance({ run }: { run: ComplianceRun }) {
  const bits: ReactNode[] = []
  if (run.finishedAt) {
    bits.push(
      <Tooltip key="when" title={new Date(run.finishedAt).toLocaleString()}>
        <span>Checked {formatRelative(run.finishedAt)}</span>
      </Tooltip>,
    )
  }
  if (run.bundleDigest) {
    bits.push(
      <Tooltip key="bundle" title={`Policy bundle ${run.bundleDigest}`}>
        <span>
          rulebook{' '}
          <code style={{ fontFamily: mono }}>
            {run.bundleDigest.replace(/^sha256:/, '').slice(0, 12)}
          </code>
        </span>
      </Tooltip>,
    )
  }
  if (run.checks > 0) bits.push(<span key="checks">{run.checks} checks</span>)
  if (run.helmVersion) bits.push(<span key="helm">helm {run.helmVersion}</span>)
  if (run.kubeVersion) bits.push(<span key="kube">Kubernetes {run.kubeVersion}</span>)

  if (bits.length === 0) return null
  return (
    <Typography.Text type="secondary" style={{ fontSize: 12 }}>
      {bits.map((b, i) => (
        <span key={i}>
          {i > 0 && ' · '}
          {b}
        </span>
      ))}
    </Typography.Text>
  )
}

// ---------------------------------------------------------------------------
// The address
// ---------------------------------------------------------------------------

/**
 * Where a finding is, as one line a person navigates by.
 *
 * Outside-in, the way somebody walks to it: the chart, the template inside it,
 * the object, the container, the field. This is the difference between a
 * finding a vendor fixes in a minute and one they have to investigate.
 */
export function ResultAddress({ result, showChart = true }: {
  result: {
    chart?: string
    chartVersion?: string
    sourceFile?: string
    apiVersion?: string
    kind?: string
    namespace?: string
    name?: string
    container?: string
    locus?: string
  }
  showChart?: boolean
}) {
  const object = result.kind
    ? `${result.kind} ${result.namespace ? result.namespace + '/' : ''}${result.name ?? ''}`
    : ''

  return (
    <Space direction="vertical" size={0} style={{ lineHeight: 1.45 }}>
      {showChart && result.chart && (
        <span style={{ fontFamily: mono, fontSize: 12 }}>
          {result.chart}
          {result.chartVersion && <span style={{ color: c.text3 }}>:{result.chartVersion}</span>}
        </span>
      )}
      {result.sourceFile && (
        <span style={{ fontFamily: mono, fontSize: 12, color: c.text2 }}>{result.sourceFile}</span>
      )}
      {object && (
        <span style={{ fontSize: 12 }}>
          {object}
          {result.container && (
            <span style={{ color: c.text2 }}> · container {result.container}</span>
          )}
        </span>
      )}
      {result.locus && (
        <span style={{ fontFamily: mono, fontSize: 11, color: c.text3 }}>{result.locus}</span>
      )}
    </Space>
  )
}
