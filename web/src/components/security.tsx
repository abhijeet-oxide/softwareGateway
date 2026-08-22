import type { ReactNode } from 'react'
import { Alert, Button, Dropdown, Progress, Space, Tag, Tooltip, Typography } from 'antd'
import {
  CheckCircleOutlined, DownloadOutlined, ExclamationCircleOutlined,
  MinusCircleOutlined, QuestionCircleOutlined, WarningOutlined,
} from '@ant-design/icons'
import { mono, palette, semantic, severity as severityColour, severitySurface, verdict as verdictColour } from '../theme'
import { SEVERITIES } from '../api/types'
import type {
  ScanStatus, SecurityCounts, SecurityCoverage, SecurityProgressResponse,
  SecurityState, Severity, Verdict,
} from '../api/types'

/**
 * The security vocabulary, designed once and used literally.
 *
 * Three rules run through every component here, and they are the difference
 * between a page a release manager can act on and a wall of red:
 *
 *  1. COLOUR IS NEVER THE ONLY SIGNAL. Every severity carries its word, every
 *     verdict carries a sentence, and every state carries an icon whose shape
 *     differs. The page reads correctly in greyscale and to anybody who does
 *     not distinguish red from green.
 *
 *  2. AN ABSENCE OF FINDINGS IS NEVER RENDERED AS SAFETY. "Scanned and clean"
 *     and "nobody looked" are both an empty list, and every component that can
 *     show an empty list takes a status and says which.
 *
 *  3. THE SIMPLE VIEW USES NO JARGON. The verdict banner does not contain the
 *     word CVE, a percentage, or a scanner's name. The detailed view keeps
 *     every one of them, for the people whose job they are.
 */

// ---------------------------------------------------------------------------
// Severity
// ---------------------------------------------------------------------------

const SEVERITY_LABEL: Record<Severity, string> = {
  critical: 'Critical', high: 'High', medium: 'Medium', low: 'Low', unknown: 'Unknown',
}

/**
 * A severity, as a dot and a word.
 *
 * The dot is filled for critical and high and hollow for the rest, so the two
 * that demand attention differ in SHAPE and not only in hue.
 */
export function SeverityTag({ value, count }: { value: Severity; count?: number }) {
  const filled = value === 'critical' || value === 'high'
  return (
    <Space size={6}>
      <span
        aria-hidden
        style={{
          display: 'inline-block', width: 9, height: 9, borderRadius: '50%',
          background: filled ? severityColour[value] : severitySurface[value],
          border: `1.5px solid ${severityColour[value]}`,
        }}
      />
      <span style={{ color: severityColour[value] }}>{SEVERITY_LABEL[value]}</span>
      {count !== undefined && <strong>{count.toLocaleString()}</strong>}
    </Space>
  )
}

/**
 * A severity breakdown as one horizontal bar.
 *
 * Proportional, so a release with 134 criticals among 1,286 findings does not
 * look like one with four. Every segment carries a tooltip with its own count,
 * because a bar answers "what is the shape of this" and never "how many".
 */
export function SeverityBar({ counts, height = 8 }: { counts: SecurityCounts; height?: number }) {
  const total = counts.total || 1
  return (
    <div style={{ display: 'flex', width: '100%', height, borderRadius: height / 2, overflow: 'hidden', background: '#EEF1F4' }}>
      {SEVERITIES.map((s) => {
        const n = counts.bySeverity[s]
        if (!n) return null
        return (
          <Tooltip key={s} title={`${SEVERITY_LABEL[s]}: ${n.toLocaleString()}`}>
            <div style={{ width: `${(n / total) * 100}%`, background: severityColour[s] }} />
          </Tooltip>
        )
      })}
    </div>
  )
}

/**
 * The five counts in a row, worst first.
 *
 * Zeroes are SHOWN rather than dropped. A release with no criticals is telling
 * you something, and a row that omits the zero reads as a row that forgot to
 * mention them.
 */
export function SeverityCountsRow({ counts, size = 'default' }: {
  counts: SecurityCounts
  size?: 'small' | 'default'
}) {
  return (
    <Space size={size === 'small' ? 10 : 18} wrap>
      {SEVERITIES.map((s) => (
        (s === 'unknown' && !counts.bySeverity.unknown) ? null : (
          <SeverityTag key={s} value={s} count={counts.bySeverity[s]} />
        )
      ))}
    </Space>
  )
}

/**
 * A compact vulnerability count for a table cell.
 *
 * Takes the STATUS as well as the counts, because a listing is exactly where a
 * blank cell would be read as "nothing wrong with this one".
 */
export function VulnerabilityCell({ counts, state }: {
  counts?: SecurityCounts
  state?: SecurityState | ScanStatus
}) {
  if (!counts || state === 'disabled') {
    return <Typography.Text type="secondary">Not scanned</Typography.Text>
  }
  if (state === 'unavailable' || state === 'not_scanned') {
    return (
      <Tooltip title="No scan result. This is not the same as no vulnerabilities.">
        <Space size={4}>
          <QuestionCircleOutlined style={{ color: semantic.neutral }} />
          <Typography.Text type="secondary">Unknown</Typography.Text>
        </Space>
      </Tooltip>
    )
  }
  if (counts.total === 0) {
    return (
      <Space size={4}>
        <CheckCircleOutlined style={{ color: semantic.success }} />
        <Typography.Text>None found</Typography.Text>
      </Space>
    )
  }
  return (
    <Space direction="vertical" size={2} style={{ width: '100%', minWidth: 140 }}>
      <Space size={8}>
        <strong>{counts.total.toLocaleString()}</strong>
        {SEVERITIES.slice(0, 2).map((s) => (
          counts.bySeverity[s] > 0 ? (
            <Tooltip key={s} title={`${SEVERITY_LABEL[s]}: ${counts.bySeverity[s]}`}>
              <span style={{ color: severityColour[s], fontWeight: 500 }}>
                {counts.bySeverity[s]} {SEVERITY_LABEL[s].toLowerCase()}
              </span>
            </Tooltip>
          ) : null
        ))}
        {state === 'partial' && (
          <Tooltip title="Some artifacts in this release have not been scanned, so this number covers only part of it.">
            <WarningOutlined style={{ color: semantic.warning }} />
          </Tooltip>
        )}
      </Space>
      <SeverityBar counts={counts} height={5} />
    </Space>
  )
}

// ---------------------------------------------------------------------------
// Coverage and state
// ---------------------------------------------------------------------------

const STATUS_LABEL: Record<ScanStatus, string> = {
  scanned: 'Scanned',
  not_scanned: 'Not scanned',
  unsupported: 'Not applicable',
  disabled: 'Xray disabled',
  unavailable: 'Unavailable',
}

/** One artifact's scan status, as a tag whose word carries the meaning. */
export function ScanStatusTag({ status }: { status: ScanStatus }) {
  switch (status) {
    case 'scanned':
      return <Tag color="success">{STATUS_LABEL.scanned}</Tag>
    case 'not_scanned':
      return <Tag color="warning">{STATUS_LABEL.not_scanned}</Tag>
    case 'unavailable':
      return <Tag color="error">{STATUS_LABEL.unavailable}</Tag>
    case 'disabled':
      return <Tag>{STATUS_LABEL.disabled}</Tag>
    default:
      return <Tag>{STATUS_LABEL.unsupported}</Tag>
  }
}

/**
 * How much of a release the numbers cover.
 *
 * Always beside the counts, never on its own page. "1,286 vulnerabilities"
 * means one thing at full coverage and something else at 78%, and a reader who
 * has to go and find the second number will quote the first.
 */
export function CoverageMeter({ coverage }: { coverage: SecurityCoverage }) {
  const scannable = coverage.scannable || 1
  const percent = Math.round((coverage.scanned / scannable) * 100)
  return (
    <Space direction="vertical" size={4} style={{ width: '100%' }}>
      <Progress
        percent={percent}
        size="small"
        status={coverage.complete ? 'success' : 'active'}
        strokeColor={coverage.complete ? semantic.success : semantic.warning}
      />
      <Typography.Text type="secondary" style={{ fontSize: 12 }}>
        {coverage.scanned.toLocaleString()} of {scannable.toLocaleString()} artifacts scanned
        {coverage.unsupported > 0 && ` · ${coverage.unsupported} not applicable`}
      </Typography.Text>
    </Space>
  )
}

/**
 * The banner that says whether these numbers can be trusted.
 *
 * Rendered ABOVE the numbers and never below them, because its whole job is to
 * qualify what follows. A `partial` release with a warning underneath the
 * totals is a release whose totals get quoted.
 */
export function SecurityStateNotice({ state, message, onRefresh }: {
  state: SecurityState
  message?: string
  onRefresh?: () => void
}) {
  if (state === 'ok') return null

  const config: Record<Exclude<SecurityState, 'ok'>, { type: 'warning' | 'error' | 'info'; title: string }> = {
    partial: { type: 'warning', title: 'Some artifacts have not been scanned' },
    unavailable: { type: 'error', title: 'Security data could not be retrieved' },
    disabled: { type: 'info', title: 'Security scanning is switched off for this repository' },
  }
  const { type, title } = config[state]

  return (
    <Alert
      type={type}
      showIcon
      style={{ marginBottom: 16 }}
      message={title}
      description={
        <Space direction="vertical" size={4}>
          <span>{message}</span>
          {/* Said plainly, because it is the single most common misreading. */}
          <Typography.Text type="secondary">
            An artifact with no scan result is not the same as an artifact with no vulnerabilities.
          </Typography.Text>
        </Space>
      }
      action={onRefresh && <Button size="small" onClick={onRefresh}>Try again</Button>}
    />
  )
}

/**
 * What to show where a deployment has no scanner at all.
 *
 * Distinct from a disabled one: nothing is wrong and nothing needs fixing on
 * this release - the platform simply has no scanner wired - and offering a
 * "try again" button would be offering a button that cannot work.
 */
export function SecurityNotConfigured({ what = 'This deployment' }: { what?: string }) {
  return (
    <Alert
      type="info"
      showIcon
      icon={<MinusCircleOutlined />}
      message="No security scanner is configured"
      description={
        <>
          {what} has no security scanner set up, so there is nothing to report here.
          Security scanning is enabled per repository, on a JFrog repository, in the product configuration.
        </>
      }
    />
  )
}

// ---------------------------------------------------------------------------
// The simple view
// ---------------------------------------------------------------------------

const VERDICT_ICON: Record<Verdict, ReactNode> = {
  better: <CheckCircleOutlined />,
  worse: <ExclamationCircleOutlined />,
  unchanged: <MinusCircleOutlined />,
  inconclusive: <QuestionCircleOutlined />,
}

/**
 * The answer, in one word and one sentence.
 *
 * # What this is not allowed to contain
 *
 * A CVE identifier, a percentage, a scanner's name, or the word "delta". This
 * is the view for somebody who has to decide whether to ship a release this
 * afternoon and has never read an advisory. Everything technical is one click
 * away, in the breakdown, and putting any of it here would cost the audience
 * the view exists for.
 *
 * # Why inconclusive is loud
 *
 * Because it is the answer people ignore. A quiet grey "inconclusive" reads as
 * "no difference" - which is exactly the wrong reading, since it means nobody
 * knows - so it gets a colour of its own and an explanation of what to do.
 */
export function VerdictBanner({
  verdict, headline, explanation, caveats, extra,
}: {
  verdict: Verdict
  headline: string
  explanation: string
  caveats?: string[]
  extra?: ReactNode
}) {
  const colour = verdictColour[verdict]

  return (
    <div
      style={{
        border: `1px solid ${colour}33`,
        borderLeft: `4px solid ${colour}`,
        borderRadius: palette.borderRadius,
        background: '#FFFFFF',
        padding: '18px 20px',
        marginBottom: 16,
      }}
    >
      <Space direction="vertical" size={10} style={{ width: '100%' }}>
        <Space align="start" style={{ width: '100%', justifyContent: 'space-between' }}>
          <Space size={10} align="center">
            <span style={{ color: colour, fontSize: 20, lineHeight: 1 }}>{VERDICT_ICON[verdict]}</span>
            <Typography.Title level={4} style={{ margin: 0, color: colour }}>
              {verdict === 'inconclusive' ? 'Not enough information' : VERDICT_WORD[verdict]}
            </Typography.Title>
          </Space>
          {extra}
        </Space>

        <Typography.Paragraph style={{ margin: 0, fontSize: 15 }}>
          {explanation || headline}
        </Typography.Paragraph>

        {caveats && caveats.length > 0 && (
          <Space direction="vertical" size={2}>
            {caveats.map((c) => (
              <Typography.Text key={c} type="secondary">
                <WarningOutlined style={{ color: semantic.warning, marginRight: 6 }} />
                {c}
              </Typography.Text>
            ))}
          </Space>
        )}
      </Space>
    </div>
  )
}

const VERDICT_WORD: Record<Verdict, string> = {
  better: 'Better',
  worse: 'Worse',
  unchanged: 'No meaningful change',
  inconclusive: 'Not enough information',
}

/**
 * The four numbers behind the verdict, as tiles.
 *
 * Resolved and introduced first, because they are what the sentence above said.
 * A tile whose number is zero still appears - "0 introduced" is the good news.
 */
export function ComparisonTiles({ resolved, introduced, moreSevere, lessSevere, unchanged }: {
  resolved: SecurityCounts
  introduced: SecurityCounts
  moreSevere: number
  lessSevere: number
  unchanged: number
}) {
  const tiles = [
    { label: 'Resolved', value: resolved.total, colour: verdictColour.better, counts: resolved },
    { label: 'Introduced', value: introduced.total, colour: verdictColour.worse, counts: introduced },
    { label: 'Became more severe', value: moreSevere, colour: severityColour.high },
    { label: 'Became less severe', value: lessSevere, colour: severityColour.low },
    { label: 'Unchanged', value: unchanged, colour: semantic.neutral },
  ]

  return (
    <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(160px, 1fr))', gap: 12 }}>
      {tiles.map((t) => (
        <div
          key={t.label}
          style={{
            border: `1px solid ${palette.border}`,
            borderRadius: palette.borderRadius,
            padding: '12px 14px',
            background: '#FFFFFF',
          }}
        >
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>{t.label}</Typography.Text>
          <div style={{ fontSize: 24, fontWeight: 600, color: t.colour, lineHeight: 1.3 }}>
            {t.value.toLocaleString()}
          </div>
          {t.counts && t.counts.total > 0 && (
            <Space size={8} wrap style={{ marginTop: 4 }}>
              {SEVERITIES.filter((s) => t.counts!.bySeverity[s] > 0).map((s) => (
                <Typography.Text key={s} style={{ fontSize: 12, color: severityColour[s] }}>
                  {t.counts!.bySeverity[s]} {SEVERITY_LABEL[s].toLowerCase()}
                </Typography.Text>
              ))}
            </Space>
          )}
        </div>
      ))}
    </div>
  )
}

// ---------------------------------------------------------------------------
// Progress
// ---------------------------------------------------------------------------

/**
 * What the system is doing, in words, while it does it.
 *
 * # Why not a spinner
 *
 * Retrieving a release's posture is a scanner query per batch across a few
 * hundred artifacts. A spinner says the same thing for those thirty seconds as
 * it says for a request that has silently stopped, and "is this working?" is
 * the only question anybody has while waiting. A user who cannot tell reloads
 * the page, which starts the whole retrieval again.
 *
 * A stage with no total shows an indeterminate bar rather than a fabricated
 * percentage. The denominator is genuinely unknown while a tree is being
 * walked, and inventing one is how a bar reaches 90% and stops.
 */
export function SecurityProgressPanel({ progress, fallback }: {
  progress?: SecurityProgressResponse
  fallback?: string
}) {
  const stages = progress?.stages ?? []
  const notes = progress?.notes ?? []
  const current = stages.at(-1)

  return (
    <div style={{ padding: '20px 4px' }}>
      <Space direction="vertical" size={14} style={{ width: '100%' }}>
        <Typography.Text strong>
          {current ? current.label : (fallback ?? 'Retrieving security data')}
        </Typography.Text>

        {stages.length === 0 && <Progress percent={100} status="active" showInfo={false} size="small" />}

        {stages.map((stage) => (
          <div key={stage.name}>
            <Space style={{ width: '100%', justifyContent: 'space-between' }}>
              <Typography.Text type="secondary">{stage.label}</Typography.Text>
              <Typography.Text type="secondary" style={{ fontFamily: mono, fontSize: 12 }}>
                {stage.total > 0 ? `${stage.done} / ${stage.total}` : `${stage.done}`}
              </Typography.Text>
            </Space>
            <Progress
              percent={stage.total > 0 ? Math.round((stage.done / stage.total) * 100) : 100}
              status={stage.total > 0 && stage.done >= stage.total ? 'success' : 'active'}
              showInfo={false}
              size="small"
            />
          </div>
        ))}

        {notes.length > 0 && (
          <Space direction="vertical" size={2}>
            {notes.slice(-3).map((n, i) => (
              <Typography.Text key={`${n}-${i}`} type="secondary" style={{ fontSize: 12 }}>{n}</Typography.Text>
            ))}
          </Space>
        )}
      </Space>
    </div>
  )
}

// ---------------------------------------------------------------------------
// Export
// ---------------------------------------------------------------------------

/**
 * The export control.
 *
 * Two axes - what (summary or breakdown) and how (CSV, Excel, JSON) - as one
 * menu rather than two controls, because they are one decision. Every item is a
 * LINK: the browser streams the file, names it from the response, and shows its
 * own progress, none of which a fetch-and-blob would do, and a large export
 * would be held whole in memory first.
 */
export function SecurityExportMenu({ urlFor, disabled }: {
  urlFor: (format: string, view: string) => string
  disabled?: boolean
}) {
  const items = [
    { key: 'h1', type: 'group' as const, label: 'Summary', children: [
      { key: 'summary-csv', label: <a href={urlFor('csv', 'summary')}>CSV</a> },
      { key: 'summary-xlsx', label: <a href={urlFor('xlsx', 'summary')}>Excel</a> },
      { key: 'summary-json', label: <a href={urlFor('json', 'summary')}>JSON</a> },
    ] },
    { key: 'h2', type: 'group' as const, label: 'Full breakdown', children: [
      { key: 'detailed-csv', label: <a href={urlFor('csv', 'detailed')}>CSV</a> },
      { key: 'detailed-xlsx', label: <a href={urlFor('xlsx', 'detailed')}>Excel</a> },
      { key: 'detailed-json', label: <a href={urlFor('json', 'detailed')}>JSON</a> },
    ] },
  ]

  return (
    <Dropdown menu={{ items }} disabled={disabled} trigger={['click']}>
      <Button icon={<DownloadOutlined />}>Export</Button>
    </Dropdown>
  )
}

// ---------------------------------------------------------------------------
// Shared table pieces
// ---------------------------------------------------------------------------

/** A CVE identifier, monospace, with the scanner's own id behind it. */
export function CveCell({ cve, id }: { cve?: string; id?: string }) {
  if (!cve && !id) return <Typography.Text type="secondary">-</Typography.Text>
  return (
    <Space direction="vertical" size={0}>
      <Typography.Text style={{ fontFamily: mono }}>{cve || id}</Typography.Text>
      {cve && id && id !== cve && (
        <Typography.Text type="secondary" style={{ fontSize: 11, fontFamily: mono }}>{id}</Typography.Text>
      )}
    </Space>
  )
}

/** A package and its version, with the version as the secondary fact. */
export function ComponentCell({ name, version, type }: { name?: string; version?: string; type?: string }) {
  if (!name) return <Typography.Text type="secondary">-</Typography.Text>
  return (
    <Space direction="vertical" size={0}>
      <Typography.Text style={{ fontFamily: mono }}>{name}</Typography.Text>
      <Typography.Text type="secondary" style={{ fontSize: 11 }}>
        {[version, type].filter(Boolean).join(' · ')}
      </Typography.Text>
    </Space>
  )
}

/**
 * Whether anything can be done about a finding, in words.
 *
 * "Fixable" alone was ambiguous - fixable by whom, how? - so the cell names the
 * version to move to when the scanner supplies one. That is the difference
 * between a row somebody reads and a row somebody acts on.
 */
export function FixCell({ fixable, fixedIn }: { fixable: boolean; fixedIn?: string[] }) {
  if (!fixable) return <Typography.Text type="secondary">No fix available</Typography.Text>
  if (!fixedIn || fixedIn.length === 0) return <Tag color="success">Fixable</Tag>
  return (
    <Tooltip title={fixedIn.join(', ')}>
      <Space direction="vertical" size={0}>
        <Tag color="success" style={{ marginInlineEnd: 0 }}>Fixable</Tag>
        <Typography.Text type="secondary" style={{ fontFamily: mono, fontSize: 11 }}>
          {fixedIn[0]}
          {fixedIn.length > 1 && ` +${fixedIn.length - 1}`}
        </Typography.Text>
      </Space>
    </Tooltip>
  )
}

/** The empty state of a findings table, which must never read as "all clear". */
export function FindingsEmpty({ status }: { status?: ScanStatus | SecurityState }) {
  if (status === 'scanned' || status === 'ok') {
    return (
      <Space direction="vertical" size={4} style={{ padding: 24 }}>
        <CheckCircleOutlined style={{ fontSize: 22, color: semantic.success }} />
        <Typography.Text strong>No vulnerabilities found</Typography.Text>
        <Typography.Text type="secondary">Everything here was scanned and came back clean.</Typography.Text>
      </Space>
    )
  }
  return (
    <Space direction="vertical" size={4} style={{ padding: 24 }}>
      <QuestionCircleOutlined style={{ fontSize: 22, color: semantic.neutral }} />
      <Typography.Text strong>Nothing to show</Typography.Text>
      <Typography.Text type="secondary">
        This is an absence of scan results, not an absence of vulnerabilities.
      </Typography.Text>
    </Space>
  )
}
