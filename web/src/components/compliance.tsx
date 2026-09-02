import type { ReactNode } from 'react'
import { Alert, Col, Row, Space, Tooltip, Typography } from 'antd'
import { StatusPill, StatTile, c, mono } from '../uikit'
import type { PillTone } from '../uikit'
import { formatRelative } from '../domain/format'
import type {
  ComplianceCounts, ComplianceDeterminacy, ComplianceHelm,
  ComplianceOutcome, ComplianceRun, ComplianceVerdict,
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

/**
 * The word a severity is read as.
 *
 * `block` reads as "Critical". The wire value stays `block` - it is what every
 * policy pack, every stored result and every export already says - but
 * "Blocking" ranked the severity against nothing, where Critical, Warning and
 * Info are a scale a reader already knows the shape of.
 */
const SEVERITY_WORD: Record<string, string> = {
  block: 'Critical',
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
 * The five numbers, in the order somebody reads them.
 *
 * Critical, warning, info, unchecked, passed. The fourth is not optional and
 * not folded into the others: a release with three hundred passes and one
 * unchecked check has not been shown to comply with anything, and a summary
 * that omitted it would draw that release as clean.
 *
 * # Why this is the only place the caveat is now made
 *
 * It used to be an alert above the card as well - a yellow banner saying "657
 * checks could not be decided; this release has not been shown to meet the
 * standards". True, and on every screen of every inconclusive release, which on
 * a real orb is all of them. A caveat that is always there is a caveat nobody
 * reads, and it pushed the verdict below the fold to say what the tile beside
 * the verdict already says. The number is here, coloured, clickable, next to
 * the ones it qualifies.
 */
export function ComplianceSummary({ counts, selected, onSelect }: {
  counts: ComplianceCounts
  /** The slice currently on screen, so the tile that drives it reads as chosen. */
  selected?: SummaryKey
  onSelect?: (what: SummaryKey) => void
}) {
  const tiles: Array<{
    key: SummaryKey
    label: string
    value: number
    sub: string
    colour?: string
  }> = [
    {
      key: 'blocking', label: 'Critical', value: counts.blocking,
      sub: 'must be fixed before this ships', colour: c.danger,
    },
    {
      key: 'warning', label: 'Warnings', value: counts.warning,
      sub: 'worth a conversation with the vendor', colour: c.review,
    },
    {
      key: 'info', label: 'Info', value: counts.info,
      sub: 'recorded, no action required',
    },
    {
      key: 'error', label: 'Unchecked', value: counts.error,
      sub: counts.error > 0 ? 'so this result is incomplete' : 'every check was decided',
      colour: counts.error > 0 ? c.pending : undefined,
    },
    {
      key: 'pass', label: 'Passed', value: counts.pass,
      sub: 'checks satisfied', colour: c.ok,
    },
  ]

  /*
   * FLEX rather than a 24-column grid, because there are five of them.
   *
   * Five does not divide 24, so a span-based row left a quarter-tile of dead
   * space at the end of every line. `flex: 1 1 150px` fills the row evenly at
   * any width and wraps to two, three or five across on its own.
   */
  return (
    <Row gutter={[12, 12]}>
      {tiles.map((t) => (
        <Col key={t.key} flex="1 1 150px" style={{ minWidth: 150 }}>
          <StatTile
            label={t.label}
            value={
              <span style={t.colour ? { color: t.colour } : undefined}>
                {t.value.toLocaleString()}
              </span>
            }
            sub={t.sub}
            /*
              The chosen slice, marked with a ring rather than a fill: these
              tiles carry a coloured number and a second colour behind it would
              fight the one that means something. Drawn through `style` because
              StatTile belongs to the shared design system, and a selected state
              is this page's idea rather than the kit's.
            */
            style={
              selected === t.key
                ? { boxShadow: `inset 0 0 0 2px ${t.colour ?? c.brand}` }
                : undefined
            }
            onClick={onSelect ? () => onSelect(t.key) : undefined}
          />
        </Col>
      ))}
    </Row>
  )
}

/** The slices the summary can send a reader to. */
export type SummaryKey = 'blocking' | 'warning' | 'info' | 'error' | 'pass'

// ---------------------------------------------------------------------------
// Notices
// ---------------------------------------------------------------------------

/**
 * Why every rendered check came back unchecked.
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
          Kubernetes objects and every check that needs them reports as unchecked - never as a
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
