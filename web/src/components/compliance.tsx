import type { ReactNode } from 'react'
import { Alert, Card, Space, Tooltip, Typography } from 'antd'
import { HelmOutlined } from '../icons'
import {
  FieldLabel, StatusPill, c, mono, severity as severityColour, severitySurface,
} from '../uikit'
import type { PillTone } from '../uikit'
import { formatRelative } from '../domain/format'
import type {
  ComplianceChart, ComplianceCounts, ComplianceDeterminacy, ComplianceHelm,
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

/**
 * THE SEVERITY SCALE, in the colours the Security tab draws.
 *
 * # Why the shared palette and not this tab's own
 *
 * Because they are the same scale on two tabs one click apart, and they were
 * drawn from two different sets. Compliance used the STATUS palette - `danger`
 * #d70015, `pending` #b25000 - which are a deeper red and a brown-orange than
 * Security's `sev-critical` #f43f43 and `sev-high` #fb8c00. Near enough to look
 * like a mistake rather than a distinction: a reader comparing the two tabs
 * sees two reds and wonders what the difference means, and the answer is that
 * there is none.
 *
 * `block` takes Security's critical and `warn` takes its high, because those
 * are the two it fills solid - the ones that are work. Info is blue: it is the
 * one step on this scale that is not a severity at all but a note, and the
 * shared scale's remaining steps (yellow, green) both read as verdicts about
 * how bad something is.
 */
const SEVERITY_COLOUR: Record<string, string> = {
  block: severityColour.critical,
  warn: severityColour.high,
  info: c.review,
}

const SEVERITY_SURFACE: Record<string, string> = {
  block: severitySurface.critical,
  warn: severitySurface.high,
  info: c.reviewBg,
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

/**
 * A severity, drawn exactly as the Security tab draws one.
 *
 * A dot and a word, the dot filled for the two that are work and outlined for
 * the one that is a note - which is `SeverityTag` in security.tsx, glyph for
 * glyph. It was a filled pill here, so the same scale had two shapes as well as
 * two palettes.
 */
export function CheckSeverityTag({ severity }: { severity: string }) {
  const colour = SEVERITY_COLOUR[severity] ?? c.text3
  const filled = severity === 'block' || severity === 'warn'
  return (
    <Space size={6}>
      <span
        aria-hidden
        style={{
          display: 'inline-block', width: 9, height: 9, borderRadius: '50%',
          background: filled ? colour : (SEVERITY_SURFACE[severity] ?? 'transparent'),
          border: `1.5px solid ${colour}`,
          flexShrink: 0,
        }}
      />
      <span>{SEVERITY_WORD[severity] ?? severity}</span>
    </Space>
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
 * The posture band: three zones, read left to right.
 *
 * # Why this is the Security tab's band and not five tiles
 *
 * Because they are the same screen answering the same three questions in the
 * same order - how bad is it, what is it made of, and can I trust the answer -
 * and they were drawn in two visual languages one tab apart. Security had a
 * single card with three hairline-separated zones; compliance had five bordered
 * tiles in a row. A reader who has learned one is told, by the layout alone,
 * that the other is a different kind of thing.
 *
 * The zones carry compliance's own facts. The third one is where they differ
 * most and matters most: Security's confidence is "how many images were
 * scanned", and compliance's is "how many charts rendered, and how many checks
 * that left undecided". Same question, different denominator.
 *
 * # Why the unchecked count is here and not a banner
 *
 * It was a banner - a warning alert above the verdict on every screen of every
 * inconclusive release, which on a real orb is all of them. A caveat that is
 * always there is a caveat nobody reads, and it pushed the verdict below the
 * fold to say what this band says. It is a coloured meter in the confidence
 * zone, beside the coverage it qualifies.
 */
export function ComplianceSummary({
  counts, charts, verdict, verdictLabel, selected, grouped = true, onSelect,
}: {
  counts: ComplianceCounts
  /** The run's denominator: what rendered, and what did not. */
  charts?: ComplianceChart[]
  verdict?: ComplianceVerdict
  verdictLabel?: string
  /** The slice currently on screen, so the row that drives it reads as chosen. */
  selected?: SummaryKey
  /**
   * Whether the table below is grouped.
   *
   * The severity zone leads with whichever number the table is showing, so the
   * card and the rows under it can never quote different ones - the whole
   * reason this pairing is drawn at all.
   */
  grouped?: boolean
  onSelect?: (what: SummaryKey) => void
}) {
  /*
   * WHAT COUNTS AS A FAILED CHECK, and info does not.
   *
   * An informational finding is recorded so somebody can look at it; it is not
   * work, it does not block, and nobody has to answer for it. Adding it to the
   * headline made a release with four hundred notes look worse than one with
   * four defects, which is the number this zone exists to get right. Info keeps
   * its own row in the zone beside this one.
   */
  const findings = counts.blocking + counts.warning
  const unique = counts.uniqueBlocking + counts.uniqueWarning
  const decided = counts.pass + counts.fail + counts.waived
  const total = decided + counts.error + counts.skip
  const rendered = (charts ?? []).filter((ch) => ch.status === 'ok').length
  const chartTotal = (charts ?? []).length
  const failedCharts = chartTotal - rendered
  const renderedPercent = chartTotal > 0 ? Math.round((rendered / chartTotal) * 100) : 100
  const decidedPercent = total > 0 ? Math.round((decided / total) * 100) : 0

  const severities: {
    key: SummaryKey
    label: string
    severity: string
    unique: number
    total: number
    colour: string
  }[] = [
    {
      key: 'blocking', label: 'Critical', severity: 'block',
      unique: counts.uniqueBlocking, total: counts.blocking, colour: SEVERITY_COLOUR.block!,
    },
    {
      key: 'warning', label: 'Warning', severity: 'warn',
      unique: counts.uniqueWarning, total: counts.warning, colour: SEVERITY_COLOUR.warn!,
    },
    {
      key: 'info', label: 'Info', severity: 'info',
      unique: counts.uniqueInfo, total: counts.info, colour: SEVERITY_COLOUR.info!,
    },
  ]

  return (
    <Card size="small" styles={{ body: { padding: 0 } }}>
      <div
        className="slm-band"
        style={{
          gridTemplateColumns: 'minmax(230px, 0.85fr) minmax(280px, 1.15fr) minmax(230px, 0.9fr)',
        }}
      >
        {/* -------------------------------------------------- how bad it is -- */}
        <div style={{ padding: '18px 22px', minWidth: 0 }}>
          <ZoneLabel>Compliance</ZoneLabel>
          <Space direction="vertical" size={10} style={{ width: '100%' }}>
            <VerdictPill verdict={verdict} label={verdictLabel} />
            {/*
              UNIQUE FIRST, and the total under it.

              The total is the same rule counted once per place it fires - a
              measure of how much editing there is to do - and it is not the
              number somebody means when they ask how many problems a release
              has. Five rules broken in a hundred and seventy-one places is five
              conversations with the vendor. This is the Security tab's zone,
              which leads with unique CVEs for exactly the same reason.
            */}
            <div>
              <div
                style={{
                  fontSize: 44, fontWeight: 600, lineHeight: 1, letterSpacing: '-0.03em',
                  color: unique > 0 ? c.text : c.ok, fontVariantNumeric: 'tabular-nums',
                }}
              >
                {unique.toLocaleString()}
              </div>
              <Typography.Text type="secondary" style={{ fontSize: 12.5 }}>
                {unique === 1 ? 'check failed' : 'checks failed'}
              </Typography.Text>
              {/*
                WHICH ones, in the same breath. "40 checks failed" is a number
                somebody has to open a tab to act on; "21 critical, 19 warning"
                is a decision, and it is the split the release meeting turns on.
              */}
              {unique > 0 && (
                <div style={{ marginTop: 4, fontSize: 12.5 }}>
                  {counts.uniqueBlocking > 0 && (
                    <span style={{ color: SEVERITY_COLOUR.block, fontWeight: 600 }}>
                      {counts.uniqueBlocking.toLocaleString()} critical
                    </span>
                  )}
                  {counts.uniqueBlocking > 0 && counts.uniqueWarning > 0 && (
                    <span style={{ color: c.text3 }}> · </span>
                  )}
                  {counts.uniqueWarning > 0 && (
                    <span style={{ color: SEVERITY_COLOUR.warn, fontWeight: 600 }}>
                      {counts.uniqueWarning.toLocaleString()} warning
                    </span>
                  )}
                </div>
              )}
            </div>
            {/*
              The shape of the findings, in the severity colours the rows use.
              Proportional to the findings alone rather than to every result:
              three thousand passes would flatten this to a hairline and say
              nothing about the work in front of somebody.
            */}
            <ComplianceSeverityBar counts={counts} />
            {/*
              How much editing there is to do, which is a different question
              from how many rules were broken and just as relevant - so it is
              read at the same size as a fact rather than as a footnote.
            */}
            <Typography.Text style={{ fontSize: 13, color: c.text2 }}>
              <strong style={{ color: c.text, fontVariantNumeric: 'tabular-nums' }}>
                {findings.toLocaleString()}
              </strong>
              {' '}{findings === 1 ? 'finding' : 'findings'}
              {chartTotal > 0 && (
                <>
                  {' across '}
                  <strong style={{ color: c.text, fontVariantNumeric: 'tabular-nums' }}>
                    {rendered.toLocaleString()}
                  </strong>
                  {` of ${chartTotal.toLocaleString()} charts rendered`}
                </>
              )}
            </Typography.Text>
          </Space>
        </div>

        {/* ------------------------------------------- what it is made of -- */}
        <div
          style={{
            padding: '18px 22px', minWidth: 0,
            borderInlineStart: `1px solid ${c.border}`,
          }}
        >
          <ZoneLabel
            count={
              <span style={{ fontWeight: 400 }}>
                {grouped ? 'checks | places' : 'places | checks'}
              </span>
            }
          >
            Failing checks by severity
          </ZoneLabel>
          <div style={{ display: 'grid', gap: 10 }}>
            {severities.map((sev) => {
              // Proportional to the whole release, so the row lengths are
              // comparable with each other. A bar normalised per severity would
              // draw three full-width bars and say nothing.
              const share = findings > 0 ? (sev.total / findings) * 100 : 0
              return (
                <div
                  key={sev.key}
                  onClick={onSelect ? () => onSelect(sev.key) : undefined}
                  style={{
                    cursor: onSelect ? 'pointer' : undefined,
                    opacity: selected && selected !== sev.key ? 0.62 : 1,
                  }}
                >
                  <div
                    style={{
                      display: 'flex', alignItems: 'baseline', gap: 8, marginBottom: 5,
                      fontVariantNumeric: 'tabular-nums',
                    }}
                  >
                    <CheckSeverityTag severity={sev.severity} />
                    {/*
                      CHECKS, then PLACES, in the shape the Security tab's
                      severity rows use for unique CVEs and total findings. One
                      number cannot say both, and which one it says changes the
                      answer by a factor of thirty.
                    */}
                    {/*
                      WHICHEVER NUMBER THE TABLE IS SHOWING, leading. Grouped
                      rows are checks, ungrouped rows are places, and a zone
                      that always led with one of them disagreed with the table
                      under it half the time. The other number stays, muted, so
                      the pair is always readable.
                    */}
                    <Tooltip
                      title={
                        `${sev.unique.toLocaleString()} distinct ${sev.label.toLowerCase()} `
                        + `${sev.unique === 1 ? 'check' : 'checks'}, failing in `
                        + `${sev.total.toLocaleString()} ${sev.total === 1 ? 'place' : 'places'}`
                      }
                    >
                      <span
                        style={{
                          marginInlineStart: 'auto', fontSize: 13, fontWeight: 600,
                          color: sev.total > 0 ? c.text : c.text2,
                        }}
                      >
                        {(grouped ? sev.unique : sev.total).toLocaleString()}
                        <span style={{ color: c.text3, fontWeight: 400 }}>
                          {' | '}{(grouped ? sev.total : sev.unique).toLocaleString()}
                        </span>
                      </span>
                    </Tooltip>
                  </div>
                  <div style={{ height: 5, background: c.track, borderRadius: 3 }}>
                    <div
                      className="slm-meter-seg"
                      style={{
                        width: `${share}%`, height: '100%', borderRadius: 3,
                        background: sev.colour, transformOrigin: 'left',
                        animation: 'slm-grow 420ms cubic-bezier(0.16,1,0.3,1) both',
                      }}
                    />
                  </div>
                </div>
              )
            })}
            <div
              style={{
                display: 'flex', alignItems: 'baseline', gap: 8,
                borderTop: `1px solid ${c.border}`, paddingTop: 8,
                fontVariantNumeric: 'tabular-nums',
              }}
            >
              <Typography.Text type="secondary">Checks passed</Typography.Text>
              <Typography.Text
                strong
                style={{ marginInlineStart: 'auto', cursor: onSelect ? 'pointer' : undefined }}
                onClick={onSelect ? () => onSelect('pass') : undefined}
              >
                {counts.pass.toLocaleString()}
              </Typography.Text>
            </div>
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

          <ComplianceMeter
            value={renderedPercent}
            colour={failedCharts === 0 ? c.ok : c.pending}
            icon={<HelmOutlined style={{ color: c.text3 }} />}
            headline={`${rendered.toLocaleString()} of ${chartTotal.toLocaleString()} charts rendered`}
            detail={failedCharts === 0
              ? 'Every chart in the release produced objects to check'
              : `${failedCharts.toLocaleString()} produced no objects, so nothing in them was checked`}
          />

          <div style={{ height: 16 }} />

          <ComplianceMeter
            value={decidedPercent}
            colour={counts.error === 0 ? c.ok : c.pending}
            headline={`${decided.toLocaleString()} of ${total.toLocaleString()} checks decided`}
            detail={counts.error === 0
              ? 'Every check reached a verdict'
              : 'The rest could not be decided, so this result is a floor'}
          />

          {/*
            THE CAVEAT, as a line rather than a banner. Clickable, because the
            reader's next move after seeing it is always the same one.
          */}
          {counts.error > 0 && (
            <div style={{ marginTop: 12 }}>
              <ComplianceCoverageLine
                n={counts.error}
                label="checks not decided"
                colour={c.pending}
                onClick={onSelect ? () => onSelect('error') : undefined}
              />
              {counts.skip > 0 && (
                <ComplianceCoverageLine n={counts.skip} label="checks not applicable" colour={c.text3} />
              )}
            </div>
          )}
        </div>
      </div>
    </Card>
  )
}

/** The shape of the findings, in the severity colours the rows use. */
function ComplianceSeverityBar({ counts }: { counts: ComplianceCounts }) {
  const total = counts.blocking + counts.warning
  // Info is not in the bar for the same reason it is not in the headline: it
  // is not work, and a release with four hundred notes must not draw wider
  // than one with four defects.
  const segments = [
    { label: 'Critical', n: counts.blocking, colour: SEVERITY_COLOUR.block! },
    { label: 'Warning', n: counts.warning, colour: SEVERITY_COLOUR.warn! },
  ]
  return (
    <div
      style={{
        display: 'flex', width: '100%', height: 8, borderRadius: 4,
        overflow: 'hidden', background: c.track,
      }}
    >
      {segments.map((seg, i) => (seg.n > 0 ? (
        <Tooltip key={seg.label} title={`${seg.label}: ${seg.n.toLocaleString()}`}>
          <div
            className="slm-meter-seg"
            style={{
              width: `${(seg.n / (total || 1)) * 100}%`,
              background: seg.colour,
              transformOrigin: 'left',
              animation: `slm-grow 420ms cubic-bezier(0.16,1,0.3,1) ${i * 60}ms both`,
            }}
          />
        </Tooltip>
      ) : null))}
    </div>
  )
}

/** The name of a zone within the posture band. */
function ZoneLabel({ children, count }: { children: ReactNode; count?: ReactNode }) {
  return (
    <div style={{ marginBottom: 10 }}>
      <FieldLabel count={count}>{children}</FieldLabel>
    </div>
  )
}

/**
 * A proportion, as a sentence with a bar under it.
 *
 * Byte for byte the shape the Security tab's confidence zone uses, because the
 * two zones answer the same question about two different denominators and a
 * reader should be able to compare them without re-learning the drawing.
 */
function ComplianceMeter({ value, colour, icon, headline, detail }: {
  value: number
  colour: string
  /** The mark for what is being counted, where the noun has one. */
  icon?: ReactNode
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
        {icon}
        <span style={{ fontSize: 13, fontWeight: 600, color: c.text }}>{headline}</span>
        <span style={{ marginInlineStart: 'auto', fontSize: 12, fontWeight: 600, color: colour }}>
          {value}%
        </span>
      </div>
      <div style={{ height: 6, background: c.track, borderRadius: 3 }}>
        <div
          className="slm-meter-seg"
          style={{
            width: `${value}%`, height: '100%', borderRadius: 3, background: colour,
            transformOrigin: 'left',
            animation: 'slm-grow 420ms cubic-bezier(0.16,1,0.3,1) both',
          }}
        />
      </div>
      <Typography.Text type="secondary" style={{ fontSize: 11.5, display: 'block', marginTop: 5 }}>
        {detail}
      </Typography.Text>
    </div>
  )
}

/** One exception to full coverage: a count, a colour and what to call it. */
function ComplianceCoverageLine({ n, label, colour, onClick }: {
  n: number
  label: string
  colour: string
  onClick?: () => void
}) {
  return (
    <div
      onClick={onClick}
      style={{
        display: 'flex', alignItems: 'center', gap: 8, marginTop: 4,
        cursor: onClick ? 'pointer' : undefined,
      }}
    >
      <span style={{ width: 6, height: 6, borderRadius: 3, background: colour, flexShrink: 0 }} />
      <Typography.Text style={{ fontSize: 12 }}>
        <strong style={{ fontVariantNumeric: 'tabular-nums' }}>{n.toLocaleString()}</strong>{' '}
        <span style={{ color: c.text2 }}>{label}</span>
      </Typography.Text>
    </div>
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
