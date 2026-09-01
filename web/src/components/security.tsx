import { useEffect, useState } from 'react'
import type { ReactNode } from 'react'
import {
  Alert, App, Button, Drawer, Dropdown, Popover, Progress, Space, Tag, Timeline, Tooltip, Typography,
} from 'antd'
import { formatAbsolute, formatRelative } from '../domain/format'
import { download } from '../api/client'
import {
  CheckCircleOutlined, CopyOutlined, DownloadOutlined, DownOutlined, ExclamationCircleOutlined,
  FileTextOutlined, LoadingOutlined, MinusCircleOutlined, QuestionCircleOutlined, StopOutlined,
  SyncOutlined, WarningOutlined,
} from '../icons'
import {
  c, mono, severity as severityColour, severitySurface, StatusPill, tokens,
  verdict as verdictColour, withAlpha,
} from '../uikit'
import { SEVERITIES } from '../api/types'
import type {
  PackageSecuritySummary, ScanStatus, SecurityCounts, SecurityCoverage,
  SecurityFreshness, SecurityLogEntry, SecurityState, SecuritySyncStatus, Severity, Verdict,
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
 *
 * # Why the word is not coloured
 *
 * Because insisting that it was is what made the whole scale look like army
 * surplus. A word has to clear 4.5:1 against white, and the only yellow that
 * dark is olive, the only orange that dark is brown - so the palette was being
 * chosen by a legibility constraint rather than by what the colours mean, and
 * medium and high came out mud.
 *
 * The MARK carries the hue and only has to clear 3:1, which a real gold and a
 * real vermilion do comfortably. The word is ordinary text, which is also the
 * more legible arrangement in a dense table: a column of five differently
 * coloured words is five colours competing with the data beside them.
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
      <span>{SEVERITY_LABEL[value]}</span>
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
 *
 * The segments grow from nothing on first paint, worst first. That is the one
 * authored moment on these pages: it draws the eye along the bar in the order
 * the severities matter, which is the order a reader should read them in.
 */
export function SeverityBar({ counts, height = 8 }: { counts: SecurityCounts; height?: number }) {
  const total = counts.total || 1
  return (
    <div
      style={{
        display: 'flex', width: '100%', height, borderRadius: height / 2,
        overflow: 'hidden', background: c.track,
      }}
    >
      {SEVERITIES.map((s, i) => {
        const n = counts.bySeverity[s]
        if (!n) return null
        return (
          <Tooltip key={s} title={`${SEVERITY_LABEL[s]}: ${n.toLocaleString()}`}>
            <div
              className="slm-meter-seg"
              style={{
                width: `${(n / total) * 100}%`,
                background: severityColour[s],
                transformOrigin: 'left',
                animation: `slm-grow 420ms cubic-bezier(0.16,1,0.3,1) ${i * 60}ms both`,
              }}
            />
          </Tooltip>
        )
      })}
    </div>
  )
}

/**
 * The listing's answer to "how bad is this release", in one glance.
 *
 * This replaced a two-column grid of five labelled counts. That grid was
 * accurate and unreadable: six lines of small text per row, in a column a
 * release manager scans down twenty rows of looking for the one that got
 * worse. Comparing "Critical 17" against "Critical 4" four rows below meant
 * reading two numbers out of two paragraphs.
 *
 * What a scan of that column actually asks is: is there anything critical, and
 * how big is the pile. So the pile is drawn - one proportional bar, worst on
 * the left - and only the two severities anybody acts on are written out. The
 * rest stay one hover away, on the bar itself, where the exact figures live.
 *
 * The total is the largest thing in the cell because it is the number a reader
 * carries to the next row.
 */
export function SeverityMeter({ counts, width, compact = false }: {
  counts: SecurityCounts
  width?: number
  /**
   * ONE LINE instead of three, for a listing.
   *
   * The full meter is a detail component: a big total, a bar under it, and the
   * two acted-on severities under that. It is right where a release is the
   * subject of the page, and wrong in a table, where it was 83px tall and
   * therefore set the height of EVERY row in the listing - eight rows to a
   * screen on a page whose whole job is scanning twenty.
   *
   * Compact keeps all three facts and spends one line on them: the total leads,
   * the bar takes the slack in the middle, and the critical and high counts sit
   * at the end as dots with numbers. The severity WORDS are what goes, because
   * they are the part the colour and the tooltip already say - and in a column
   * of twenty rows they are the same two words twenty times.
   */
  compact?: boolean
}) {
  const critical = counts.bySeverity.critical
  const high = counts.bySeverity.high

  if (compact) {
    return (
      <div
        style={{
          display: 'flex', alignItems: 'center', gap: 8, width: '100%',
          minWidth: 0, maxWidth: width ?? 260, fontVariantNumeric: 'tabular-nums',
        }}
      >
        <Tooltip title={`${counts.total.toLocaleString()} findings${
          counts.fixable > 0
            ? `, ${counts.fixable.toLocaleString()} with a fixed version available`
            : ''
        }`}
        >
          <span style={{ fontSize: 13, fontWeight: 600, color: c.text, lineHeight: 1 }}>
            {counts.total.toLocaleString()}
          </span>
        </Tooltip>
        <div style={{ flex: 1, minWidth: 24 }}>
          <SeverityBar counts={counts} height={5} />
        </div>
        <span style={{ display: 'inline-flex', gap: 10, flex: '0 0 auto', fontSize: 12 }}>
          <SeverityPip value="critical" count={critical} word={false} />
          <SeverityPip value="high" count={high} word={false} />
        </span>
      </div>
    )
  }

  /*
    Fluid, not fixed. A minimum width here is a promise the table cannot keep:
    the column is sized by the table, and a cell wider than its column is
    simply clipped by the pinned Actions column on its right - which is how
    "26 high" came to render as "26 hig".
  */
  return (
    <div style={{ width: '100%', minWidth: 0, maxWidth: width ?? 260 }}>
      <div
        style={{
          display: 'flex', alignItems: 'baseline', gap: 8, marginBottom: 6,
          fontVariantNumeric: 'tabular-nums',
        }}
      >
        <span style={{ fontSize: 17, fontWeight: 600, color: c.text, lineHeight: 1 }}>
          {counts.total.toLocaleString()}
        </span>
        <span style={{ fontSize: 11, color: c.text2, letterSpacing: '0.02em' }}>
          {counts.total === 1 ? 'finding' : 'findings'}
        </span>
        {counts.fixable > 0 && (
          <Tooltip
            title={`${counts.fixable.toLocaleString()} of ${counts.total.toLocaleString()} have a fixed version available`}
          >
            <span
              style={{
                fontSize: 11, color: c.ok, marginInlineStart: 'auto',
                whiteSpace: 'nowrap',
              }}
            >
              {Math.round((counts.fixable / (counts.total || 1)) * 100)}% fixable
            </span>
          </Tooltip>
        )}
      </div>

      <SeverityBar counts={counts} height={6} />

      {/*
        Only the two that carry an action. A zero is still shown - a release
        with no criticals is telling you something, and a row that omits the
        zero reads as a row that forgot to mention them.
      */}
      <div
        style={{
          display: 'flex', gap: 14, marginTop: 7, fontSize: 12,
          fontVariantNumeric: 'tabular-nums', whiteSpace: 'nowrap',
        }}
      >
        <SeverityPip value="critical" count={critical} />
        <SeverityPip value="high" count={high} />
      </div>
    </div>
  )
}

/** One severity as a dot, its word, and its count - the meter's unit. The word
 *  is dropped in a listing, where the colour and the tooltip carry it and the
 *  same two words would otherwise repeat down every row. */
function SeverityPip({ value, count, word = true }: {
  value: Severity
  count: number
  word?: boolean
}) {
  const filled = value === 'critical' || value === 'high'
  const muted = count === 0
  return (
    <Tooltip title={`${SEVERITY_LABEL[value]}: ${count.toLocaleString()}`}>
      <span style={{ display: 'inline-flex', alignItems: 'center', gap: 5 }}>
        <span
          aria-hidden
          style={{
            display: 'inline-block', width: 7, height: 7, borderRadius: '50%',
            background: muted ? severitySurface[value]
              : filled ? severityColour[value] : severitySurface[value],
            border: `1.5px solid ${muted ? c.borderStrong : severityColour[value]}`,
          }}
        />
        <span style={{ color: muted ? c.text2 : c.text, fontWeight: muted ? 400 : 600 }}>
          {count.toLocaleString()}
        </span>
        {word && (
          <span style={{ color: c.text2 }}>{SEVERITY_LABEL[value].toLowerCase()}</span>
        )}
      </span>
    </Tooltip>
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
 * Takes the whole stored SUMMARY rather than counts, because a listing is
 * exactly where a blank cell would be read as "nothing wrong with this one".
 * Four of the five states it renders are not numbers at all.
 */
export function VulnerabilityCell({
  summary,
  onSyncNotSynced,
  notSyncedTooltip,
}: {
  summary?: PackageSecuritySummary
  onSyncNotSynced?: () => void
  notSyncedTooltip?: string
}) {
  if (!summary || !summary.canSync) {
    return (
      <Tooltip title={summary?.reason ?? 'No vulnerability scanner is configured for this product.'}>
        <Typography.Text type="secondary">No scanner</Typography.Text>
      </Tooltip>
    )
  }
  if (summary.state === 'syncing') {
    // A claim whose holder went away is not a sync in progress. A spinning
    // icon on a release nobody is syncing is a listing telling a reader to
    // wait for something that is never coming.
    if (summary.stalled) {
      return (
        <Tooltip title="A sync was started and the Coordinator running it stopped. Nothing is running now - open the release and sync it again.">
          <StatusPill tone="pending" style={{ marginInlineEnd: 0 }}>Sync interrupted</StatusPill>
        </Tooltip>
      )
    }
    return (
      <StatusPill tone="review" icon={<SyncOutlined />} style={{ marginInlineEnd: 0 }}>
        Syncing
      </StatusPill>
    )
  }
  if (summary.state === '') {
    const clickable = Boolean(onSyncNotSynced)
    const title = clickable
      ? (notSyncedTooltip ?? 'Click to sync')
      : (notSyncedTooltip ?? 'This release has not been scanned. An unscanned release is not a release without vulnerabilities.')
    return (
      <Tooltip title={title}>
        <span
          style={{ cursor: clickable ? 'pointer' : 'default' }}
          onClick={clickable ? (e) => {
            e.preventDefault()
            e.stopPropagation()
            onSyncNotSynced?.()
          } : undefined}
        >
          <StatusPill tone="neutral" style={{ marginInlineEnd: 0 }}>Not synced</StatusPill>
        </span>
      </Tooltip>
    )
  }
  if (summary.state === 'failed' && !summary.syncedAt) {
    return (
      <Tooltip title={summary.error}>
        <StatusPill tone="danger" style={{ marginInlineEnd: 0 }}>Sync failed</StatusPill>
      </Tooltip>
    )
  }

  const stale = summary.state === 'failed'

  // Zero is two different facts and only the coverage tells them apart. A
  // release where nothing was scanned is not a release with no vulnerabilities,
  // and a green tick beside that number is the exact failure this whole feature
  // exists to prevent.
  if (summary.scanned === 0) {
    return (
      <Tooltip title="No artifact in this release was scanned, so there is no result. No result is not the same as no vulnerabilities.">
        <Space size={4}>
          <QuestionCircleOutlined style={{ color: c.text2 }} />
          <Typography.Text type="secondary">No results</Typography.Text>
        </Space>
      </Tooltip>
    )
  }

  if (summary.counts.total === 0) {
    return (
      <Space direction="vertical" size={2}>
        <Space size={4}>
          <CheckCircleOutlined style={{ color: c.ok }} />
          <Typography.Text>None found</Typography.Text>
        </Space>
        {!summary.complete && (
          <Typography.Text type="secondary" style={{ fontStyle: 'italic', fontSize: 11 }}>
            Not all artifacts were scanned.
          </Typography.Text>
        )}
      </Space>
    )
  }

  /*
    The two caveats become a MARK rather than two sentences.

    Each of them is real and neither is news the reader needs on every row: at
    two lines of amber text apiece they were most of what made this cell three
    times the height of every other cell in the row, and a listing that spends
    sixty pixels per row on a footnote shows a third as much of the table. The
    warning triangle says something qualifies the number; the tooltip says what.
  */
  const caveats = [
    !summary.complete ? 'Not all artifacts in this release were scanned.' : '',
    stale ? 'The last sync failed, so these details may be outdated.' : '',
  ].filter(Boolean)

  return (
    <div style={{ display: 'flex', alignItems: 'center', gap: 6, width: '100%', minWidth: 0 }}>
      <SeverityMeter counts={summary.counts} compact />
      {caveats.length > 0 && (
        <Tooltip title={caveats.join(' ')}>
          <span style={{ display: 'inline-flex', flex: '0 0 auto' }}>
            <WarningOutlined style={{ color: c.pending }} />
          </span>
        </Tooltip>
      )}
    </div>
  )
}

// ---------------------------------------------------------------------------
// Coverage and state
// ---------------------------------------------------------------------------

const STATUS_LABEL: Record<ScanStatus, string> = {
  scanned: 'Scanned',
  not_scanned: 'Not indexed',
  not_found: 'Not in JFrog',
  unsupported: 'Not applicable',
  disabled: 'Xray disabled',
  unavailable: 'Unavailable',
}

/** One artifact's scan status, as a tag whose word carries the meaning. */
export function ScanStatusTag({ status }: { status: ScanStatus }) {
  switch (status) {
    case 'scanned':
      return <StatusPill tone="ok">{STATUS_LABEL.scanned}</StatusPill>
    case 'not_scanned':
      return <StatusPill tone="pending">{STATUS_LABEL.not_scanned}</StatusPill>
    // Not a scanning problem at all: the image was never shipped here, so it
    // gets its own word rather than being rounded to "not scanned".
    case 'not_found':
      return <StatusPill tone="neutral">{STATUS_LABEL.not_found}</StatusPill>
    case 'unavailable':
      return <StatusPill tone="danger">{STATUS_LABEL.unavailable}</StatusPill>
    case 'disabled':
      return <StatusPill tone="neutral">{STATUS_LABEL.disabled}</StatusPill>
    default:
      return <StatusPill tone="neutral">{STATUS_LABEL.unsupported}</StatusPill>
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
        strokeColor={coverage.complete ? c.ok : c.pending}
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
export function SecurityStateNotice({ state, message, onRefresh, onShowProblems, problemCount }: {
  state: SecurityState
  message?: string
  onRefresh?: () => void
  /** Opens the list of what the scanner would not answer for, and why. */
  onShowProblems?: () => void
  problemCount?: number
}) {
  if (state === 'ok') return null

  const config: Record<Exclude<SecurityState, 'ok'>, { type: 'warning' | 'error' | 'info'; title: string }> = {
    partial: { type: 'warning', title: 'Partially Scanned' },
    unavailable: { type: 'error', title: 'No results available' },
    disabled: { type: 'info', title: 'No scanner configured' },
    not_synced: { type: 'info', title: 'Not synced' },
    syncing: { type: 'info', title: 'Sync in progress' },
    stale: { type: 'warning', title: 'Last sync failed' },
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
            An image with no scan result is not the same as an image with no vulnerabilities.
          </Typography.Text>
          {/*
            The way OUT of the banner. It named a number and then offered a
            button that ran the whole thing again, which is a page telling
            somebody their sync went wrong and refusing to say which images.
          */}
          {onShowProblems && (problemCount ?? 0) > 0 && (
            <Button type="link" size="small" style={{ padding: 0, height: 'auto' }} onClick={onShowProblems}>
              View details
            </Button>
          )}
        </Space>
      }
      action={onRefresh && <Button size="small" onClick={onRefresh}>Sync again</Button>}
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
      message="No vulnerability scanner is configured"
      description={
        <>
          {what} has no scanner configured, so no results are available. Scanning is enabled per
          repository: set <code>type: jfrog</code> and <code>xrayEnabled: true</code> on the JFrog
          repository this release is replicated to.
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
    /*
      The verdict is the page's answer, so it is the one surface that carries
      colour rather than sitting on white.

      It had a 4px coloured left border on a white card - a stripe that reads as
      a decoration applied to a box rather than as the box meaning something. A
      tinted ground and a matching hairline say the same thing with the whole
      shape instead of a strip of one edge, and the icon and the word carry the
      meaning for anybody who does not see the hue at all.
    */
    <div
      style={{
        /*
          withAlpha, NOT two more hex digits.

          These read `${colour}2E` and `${colour}0A`, which worked while the
          verdict colours were literal hexes. They are `var(--v-better)` and the
          rest now, so the browser was being handed `var(--v-better)2E` - not a
          colour - and dropping BOTH declarations. The banner had been rendering
          with no tint and no border at all: a paragraph loose on the page,
          where the whole point of this surface is that it is the one thing that
          carries the answer's colour.
        */
        border: `1px solid ${withAlpha(colour, 0.22)}`,
        borderRadius: 'var(--r-lg)',
        background: withAlpha(colour, 0.06),
        padding: '18px 20px',
        marginBottom: 16,
      }}
    >
      <Space direction="vertical" size={10} style={{ width: '100%' }}>
        <Space align="start" style={{ width: '100%', justifyContent: 'space-between' }}>
          <Space size={10} align="center">
            <span
              style={{
                color: colour, fontSize: 18, lineHeight: 1, display: 'inline-flex',
                alignItems: 'center', justifyContent: 'center',
                width: 32, height: 32, borderRadius: '50%', background: withAlpha(colour, 0.12),
              }}
            >
              {VERDICT_ICON[verdict]}
            </span>
            <Typography.Title level={4} style={{ margin: 0, color: colour, letterSpacing: '-0.01em' }}>
              {verdict === 'inconclusive' ? 'Not enough information' : VERDICT_WORD[verdict]}
            </Typography.Title>
          </Space>
          {extra}
        </Space>

        <Typography.Paragraph style={{ margin: 0, fontSize: 15 }}>
          {explanation || headline}
        </Typography.Paragraph>

        {/*
          ONE mark for the whole group, not one per line.

          Three caveats meant three amber triangles stacked down the left of the
          banner, which read as three warnings about three different things
          rather than as the qualifications on one answer. The mark says "this
          answer has conditions"; the lines say what they are.
        */}
        {caveats && caveats.length > 0 && (
          <div style={{ display: 'flex', gap: 8, alignItems: 'flex-start' }}>
            <WarningOutlined style={{ color: c.pending, marginTop: 3, flexShrink: 0 }} />
            <Space direction="vertical" size={2}>
              {caveats.map((caveat) => (
                <Typography.Text key={caveat} type="secondary">{caveat}</Typography.Text>
              ))}
            </Space>
          </div>
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
    // A severity that moved is a VERDICT, not a severity: these two tiles say
    // the comparison got worse or better, which is what the two above them
    // already say in the same two colours. Borrowing the high and low hues put
    // a fill colour on a 24px number and said "high" where it meant "worse".
    { label: 'Became more severe', value: moreSevere, colour: verdictColour.worse },
    { label: 'Became less severe', value: lessSevere, colour: verdictColour.better },
    { label: 'Unchanged', value: unchanged, colour: c.text2 },
  ]

  return (
    <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(160px, 1fr))', gap: 12 }}>
      {tiles.map((t) => (
        <div
          key={t.label}
          style={{
            border: `1px solid ${c.borderStrong}`,
            borderRadius: tokens.shape.borderRadius,
            padding: '12px 14px',
            background: c.surface,
          }}
        >
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>{t.label}</Typography.Text>
          <div style={{ fontSize: 24, fontWeight: 600, color: t.colour, lineHeight: 1.3 }}>
            {t.value.toLocaleString()}
          </div>
          {t.counts && t.counts.total > 0 && (
            <Space size={8} wrap style={{ marginTop: 4 }}>
              {SEVERITIES.filter((s) => t.counts!.bySeverity[s] > 0).map((s) => (
                <Typography.Text key={s} type="secondary" style={{ fontSize: 12 }}>
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
 * What the sync is doing, in words, with numbers.
 *
 * # What was wrong with the first one
 *
 * It said "Working out what to scan", then "Fetching from JFrog Xray", then
 * "Fetching from JFrog Xray" again - the headline repeating the row beneath it -
 * and a bar with no denominator. Three lines of chrome that told a watcher
 * nothing they could not have guessed from the fact that they had pressed a
 * button.
 *
 * What somebody staring at a two-minute sync actually wants is: how far, how
 * long, how much has already gone wrong, and against which scanner. All four
 * are known, and all four are here.
 */
export function SecurityProgressPanel({ sync, onStop, stopping }: {
  sync: SecuritySyncStatus
  /** Offered whenever a sync can be stopped, which is whenever one is running. */
  onStop?: () => void
  stopping?: boolean
}) {
  const stages = sync.stages ?? []
  const notes = sync.notes ?? []

  const fetching = stages.find((st) => st.name === 'fetching')
  const failing = stages.find((st) => st.name === 'failing')
  const cached = stages.find((st) => st.name === 'cached')
  const resolving = stages.find((st) => st.name === 'resolving')

  // The one bar worth drawing: artifacts answered for, out of artifacts asked
  // about. Everything else is a counter, because a second bar next to a first
  // one invites the reader to compare two things that are not comparable.
  const total = fetching?.total ?? resolving?.total ?? 0
  const done = fetching?.done ?? 0
  const percent = total > 0 ? Math.round((done / total) * 100) : 0

  const elapsed = useElapsed(sync.startedAt, sync.state === 'syncing' && !sync.stalled)

  return (
    <Space direction="vertical" size={12} style={{ width: '100%', padding: '4px 0' }}>
      <Space style={{ width: '100%', justifyContent: 'space-between' }} align="start">
        <Space size={8}>
          <SyncOutlined spin style={{ color: c.brand }} />
          {/*
            What the sync is DOING, in a sentence somebody can act on.

            "Resolving the release" named a stage rather than a step, and read
            as though the release itself were in question. What is happening is
            that the platform is working out which of the release's artifacts
            the scanner can be asked about, and there is no number for it yet
            because the size of that list is the thing being established.
          */}
          <Typography.Text strong>
            {total > 0
              ? `Retrieving results for ${total.toLocaleString()} images from ${scannerName(sync)}`
              // No stages at all is a sync running on another Coordinator, and
              // this replica does not know what step it has reached. Saying it
              // is resolving artifacts would be inventing a position.
              : stages.length === 0
                ? `Vulnerability sync in progress on another Coordinator`
                : 'Working out which of this release’s artifacts to ask about'}
          </Typography.Text>
        </Space>
        <Space size={8} align="center">
          {elapsed && (
            <Typography.Text type="secondary" style={{ fontFamily: mono, fontSize: 12 }}>{elapsed}</Typography.Text>
          )}
          {onStop && (
            <Tooltip title="Stops the sync. Nothing already stored is lost - the release keeps whatever its last completed sync recorded.">
              <Button size="small" danger icon={<StopOutlined />} loading={stopping} onClick={onStop}>
                Stop
              </Button>
            </Tooltip>
          )}
        </Space>
      </Space>

      {/*
        The bar only where there is a position to draw. A sync on another
        Coordinator has none, and a bar sitting at zero for two minutes says
        the work is stuck rather than that it is elsewhere.
      */}
      {stages.length > 0 && (
      <div>
        <Progress
          percent={percent}
          status="active"
          showInfo={false}
          strokeColor={c.brand}
        />
        {/*
          SEPARATED, because these are four unrelated facts and whitespace alone
          did not say so: "Preparing" ran into "JFrog Xray · cfx-jfrog-lab" as
          though the scanner's name were part of the sentence before it.
        */}
        <Meta
          style={{ marginTop: 4 }}
          items={[
            total > 0
              ? `${done.toLocaleString()} of ${total.toLocaleString()} images`
              // The counterpart of the headline: no denominator exists yet, so
              // this says what is being counted rather than pretending to a
              // position.
              : 'Listing the release’s artifacts',
            cached && cached.done > 0
              ? `${cached.done.toLocaleString()} read from storage`
              : null,
            /*
              Failures shown AS THEY HAPPEN, not at the end. A sync against a
              scanner that is timing out looks identical to a healthy one for
              two minutes, and then delivers the bad news all at once - by which
              time the person who could have stopped it has walked away.
            */
            failing && failing.done > 0
              ? { text: `${failing.done.toLocaleString()} not retrieved`, colour: c.danger }
              : null,
            sync.repository ? `${scannerName(sync)} · ${sync.repository}` : null,
          ]}
        />
      </div>
      )}

      {notes.length > 0 && (
        <Space direction="vertical" size={2} style={{ width: '100%' }}>
          {notes.slice(-2).map((n, i) => (
            <Typography.Text key={`${n}-${i}`} type="secondary" style={{ fontSize: 12 }}>{n}</Typography.Text>
          ))}
        </Space>
      )}

      {stages.length === 0 && (
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
          {/*
            No live position means one of two things, and until the sync started
            beating this could only guess at the friendlier one.

            A sync that is still beating IS running somewhere else, and its
            state is authoritative - the honest thing is to say where it is
            happening rather than draw a bar at nothing. A sync that has stopped
            beating is not running anywhere, and is handled above by the caller:
            this line is never the answer for it.
          */}
          This sync is running on another Coordinator - it last reported
          {sync.heartbeatAt ? ` ${formatRelative(sync.heartbeatAt)}` : ' recently'}.
          Its progress is not shown here; the result appears once it completes.
        </Typography.Text>
      )}
    </Space>
  )
}

/**
 * A row of small facts with separators between them.
 *
 * The separator is the whole point. Four `<Text>`s in a Space are four facts
 * with a gap between them, and a gap reads as a space in a sentence: "Preparing
 * JFrog Xray · cfx-jfrog-lab" was three unrelated things that looked like one.
 */
function Meta({ items, style }: {
  items: (string | { text: string; colour?: string } | null | undefined | false)[]
  style?: React.CSSProperties
}) {
  const shown = items.filter(Boolean).map((i) => (typeof i === 'string' ? { text: i } : i as { text: string; colour?: string }))
  if (shown.length === 0) return null

  return (
    <div style={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: '2px 8px', ...style }}>
      {shown.map((item, i) => (
        <span key={item.text} style={{ display: 'inline-flex', alignItems: 'center', gap: 8 }}>
          {i > 0 && <span aria-hidden style={{ color: c.text3, fontSize: 12 }}>·</span>}
          <Typography.Text
            type={item.colour ? undefined : 'secondary'}
            style={{ fontSize: 12, color: item.colour }}
          >
            {item.text}
          </Typography.Text>
        </span>
      ))}
    </div>
  )
}

/**
 * A sync whose Coordinator went away.
 *
 * # Why this is its own state and not "syncing"
 *
 * Because it is the opposite of syncing: nothing is running. The row says
 * `syncing` because a run started and never got to say how it ended - a killed
 * process leaves exactly what a healthy one leaves - and the interface used to
 * read that as work in progress elsewhere and tell the reader to wait for a
 * result that was never coming, while refusing them a new sync.
 *
 * The heartbeat is what makes the difference visible, and this is what it is
 * for: say it stopped, and offer the button that starts it again.
 */
export function SyncInterrupted({ sync, onSync, pending }: {
  sync: SecuritySyncStatus
  onSync?: () => void
  pending?: boolean
}) {
  return (
    <Alert
      type="warning"
      showIcon
      message="The last sync was interrupted"
      description={
        <Space direction="vertical" size={4}>
          <Typography.Text>
            The Coordinator running it stopped
            {sync.heartbeatAt ? ` - it last reported ${formatRelative(sync.heartbeatAt)}` : ''}
            {sync.startedAt ? `, having started ${formatRelative(sync.startedAt)}` : ''}.
            Nothing is running now, and nothing already stored was lost.
          </Typography.Text>
          <Typography.Text type="secondary">
            Whatever the run had recorded before it stopped is kept. Syncing again asks the scanner
            about the whole release from the start.
          </Typography.Text>
        </Space>
      }
      action={onSync && (
        <Button size="small" loading={pending} onClick={onSync}>Sync again</Button>
      )}
    />
  )
}

function scannerName(sync: SecuritySyncStatus): string {
  if (sync.provider === 'jfrog-xray') return 'JFrog Xray'
  return sync.provider || 'the scanner'
}

/**
 * How long the sync has been running, ticking.
 *
 * A duration is the cheapest signal that something is still alive, and the only
 * one that distinguishes "slow" from "stopped" when the position has not moved
 * for thirty seconds.
 */
function useElapsed(startedAt: string | undefined, running: boolean): string | undefined {
  const [now, setNow] = useState(() => Date.now())

  useEffect(() => {
    if (!running || !startedAt) return
    const id = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(id)
  }, [running, startedAt])

  if (!startedAt) return undefined
  const started = Date.parse(startedAt)
  if (Number.isNaN(started)) return undefined

  const seconds = Math.max(0, Math.round((now - started) / 1000))
  if (seconds < 60) return `${seconds}s`
  return `${Math.floor(seconds / 60)}m ${String(seconds % 60).padStart(2, '0')}s`
}

/**
 * The one control that talks to a scanner.
 *
 * Its label changes with the state, because "Sync vulnerabilities" and "Sync
 * again" are different offers: the first is the only way to get any answer at
 * all, and the second is a refresh of one that exists. A button that said the
 * same thing in both places would make the first look optional.
 */
export function SyncButton({ sync, onSync, pending, size = 'middle', freshness }: {
  sync: SecuritySyncStatus
  /**
   * `force` asks the scanner about every image, ignoring what is stored.
   *
   * The plain press does not. Stored answers are keyed by image and releases
   * of one product share nearly all of theirs, so an ordinary sync asks about
   * the images that are missing or past the age limit and reuses the rest -
   * which is the difference between a sync of seven images and one of a
   * hundred and fifty-seven against somebody else's rate limit.
   */
  onSync: (force?: boolean) => void
  pending?: boolean
  size?: 'small' | 'middle'
  freshness?: SecurityFreshness
}) {
  if (!sync.canSync) {
    return (
      <Tooltip title={sync.reason}>
        <Button size={size} icon={<SyncOutlined />} disabled>Sync vulnerabilities</Button>
      </Tooltip>
    )
  }
  // A stalled claim is not a running sync, and a button that refuses because of
  // one is a release nobody can sync until a sweeper notices. The server takes
  // the claim from a run that has stopped beating, so this offer is real.
  const running = sync.state === 'syncing' && !sync.stalled
  const scanner = sync.provider === 'jfrog-xray' ? 'JFrog Xray' : 'the scanner'
  const where = sync.repository ? ` in ${sync.repository}` : ''
  const reuse = (freshness?.maxAgeSeconds ?? 0) > 0

  const button = (
    <Tooltip
      title={running
        ? 'A sync is already running for this release.'
        : reuse
          ? `Asks ${scanner}${where} about the images this release has no answer for, `
            + `or whose answer is older than ${describeAge(freshness?.maxAgeSeconds ?? 0)}. `
            + 'Images already answered for are reused.'
          : `Asks ${scanner}${where} about every artifact in this release and stores the answer.`}
    >
      <Button
        size={size}
        type={sync.state === '' ? 'primary' : 'default'}
        icon={<SyncOutlined spin={running || pending} />}
        loading={pending}
        disabled={running}
        onClick={() => onSync(false)}
      >
        {running ? 'Syncing' : sync.state === '' ? 'Sync vulnerabilities' : 'Sync again'}
      </Button>
    </Tooltip>
  )

  // Nothing to reuse means nothing to force past, so the second option would
  // be two names for one action.
  if (!reuse || running) return button

  return (
    <Space.Compact>
      {button}
      <Dropdown
        disabled={pending}
        menu={{
          items: [{
            key: 'force',
            icon: <SyncOutlined />,
            label: 'Re-fetch every image',
          }],
          onClick: () => onSync(true),
        }}
      >
        <Button size={size} icon={<DownOutlined />} aria-label="More sync options" />
      </Dropdown>
    </Space.Compact>
  )
}

/**
 * The way out of a sync somebody started by mistake.
 *
 * Offered only while one is running here or elsewhere, because there is nothing
 * to stop otherwise. Stopping releases the claim rather than killing a
 * goroutine: the run notices at its next heartbeat and stands down, which is
 * what makes this work against a sync on another Coordinator.
 */
export function StopSyncButton({ sync, onStop, pending, size = 'middle' }: {
  sync: SecuritySyncStatus
  onStop: () => void
  pending?: boolean
  size?: 'small' | 'middle'
}) {
  if (sync.state !== 'syncing' || sync.stalled) return null
  return (
    <Tooltip title="Stops this sync. The release keeps whatever its last completed sync recorded.">
      <Button size={size} danger icon={<StopOutlined />} loading={pending} onClick={onStop}>
        Stop sync
      </Button>
    </Tooltip>
  )
}

/** When a release was last synced, and by which scanner. */
export function SyncedAgo({ sync, freshness }: {
  sync: SecuritySyncStatus
  /**
   * The deployment's rule about how old is too old.
   *
   * The age was always here; what was missing was whether it MATTERS. "synced
   * 11 days ago" reads as a fact about the past until something says the
   * deployment considers a week old, and then it reads as a thing to do.
   */
  freshness?: SecurityFreshness
}) {
  if (!sync.syncedAt) return null
  return (
    <Space size={8} align="center" wrap>
      <Tooltip title={formatAbsolute(sync.syncedAt)}>
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
          {sync.provider === 'jfrog-xray' ? 'JFrog Xray' : sync.provider}
          {sync.repository && ` · ${sync.repository}`}
          {` · retrieved ${formatRelative(sync.syncedAt)}`}
        </Typography.Text>
      </Tooltip>
      {freshness?.stale && (
        <Tooltip
          title={
            'This deployment treats a vulnerability answer older than '
            + describeAge(freshness.maxAgeSeconds ?? 0)
            + ' as out of date. Nothing has been discarded - these are still the '
            + 'stored results, and a sync replaces them.'
          }
        >
          <Tag color="warning" style={{ marginInlineEnd: 0 }}>Out of date</Tag>
        </Tooltip>
      )}
    </Space>
  )
}

/** A configured duration in the words somebody would say it in. */
function describeAge(seconds: number): string {
  if (seconds <= 0) return 'any age'
  const days = Math.round(seconds / 86400)
  if (days >= 1) return days === 1 ? 'a day' : `${days} days`
  const hours = Math.round(seconds / 3600)
  if (hours >= 1) return hours === 1 ? 'an hour' : `${hours} hours`
  return `${Math.round(seconds / 60)} minutes`
}

// ---------------------------------------------------------------------------
// The sync's own log
// ---------------------------------------------------------------------------

/**
 * A dot per level, and the levels mean what a reader expects them to mean.
 *
 * Every line used to be one of three greys with an amber for warnings, and
 * every NOTE was written at warning level - so "requesting scan results for 157
 * images, skipping 103 that are not container images", which is a sync doing
 * exactly what it should, arrived the same colour as a scanner that could not
 * be reached. A reader who learns that the normal case looks like a problem
 * stops reading the ones that are.
 *
 * Blue is something happening. Green is something that worked. Amber is
 * something that went wrong and did not stop the sync. Red stopped it.
 */
const LOG_COLOUR: Record<string, string> = {
  error: c.danger,
  warning: c.pending,
  success: c.ok,
  info: c.brand,
}

/** The word beside the dot, for a reader who cannot rely on colour. */
const LOG_LABEL: Record<string, string> = {
  error: 'Failed',
  warning: 'Warning',
  success: 'Done',
  info: 'Info',
}

/**
 * The transcript of the sync that produced what is on screen.
 *
 * # Why a job needs one at all
 *
 * A sync is a job that takes minutes, talks to something outside this platform
 * and half-succeeds routinely. Everything it learned used to be reduced, at the
 * end, to a state and some counts: "11 of 261 scanned". A reader looking at
 * that has no way to ask what happened to the other 250, and the only offer on
 * the screen was to run the whole thing again.
 *
 * So the run writes down what it did and it is kept with the result. Grouped
 * rather than per artifact - 250 failures are three sentences, and a log with
 * 250 lines in it is one nobody reads.
 */
export function SyncLogButton({ sync, size = 'middle' }: {
  sync: SecuritySyncStatus
  size?: 'small' | 'middle'
}) {
  const [open, setOpen] = useState(false)
  const entries = sync.log ?? []
  const running = sync.state === 'syncing' && !sync.stalled

  if (entries.length === 0 && !running) {
    return null
  }

  const plain = entries
    .map((e) => `${e.at ? new Date(e.at).toISOString() : ''} [${e.level}] ${e.message}`
      + (e.repeat ? ` (x${e.repeat + 1})` : ''))
    .join('\n')

  /*
   * Whether the times need their date.
   *
   * A sync that ran this morning is read as clock times and the date is noise.
   * One from last Tuesday read as "02:14:28" with no date at all, which is not
   * a wrong time so much as a time about nothing - and a reader comparing it
   * with a release that shipped since has no way to tell which came first.
   *
   * So the date appears when any line is not from today, rather than always or
   * never: the common case stays clean and the stale case stops lying.
   */
  const showDates = entries.some((e) => e.at && !isToday(e.at))

  return (
    <>
      <Button size={size} icon={<FileTextOutlined />} onClick={() => setOpen(true)}>
        Sync log
      </Button>
      <Drawer
        title="Vulnerability sync log"
        width={720}
        open={open}
        onClose={() => setOpen(false)}
        extra={
          <Button
            size="small"
            icon={<CopyOutlined />}
            onClick={() => void navigator.clipboard?.writeText(plain)}
          >
            Copy
          </Button>
        }
      >
        <Space direction="vertical" size={16} style={{ width: '100%' }}>
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            {running
              ? 'This sync is running. The log updates as it goes.'
              : sync.syncedAt
                ? `From the run that finished ${formatRelative(sync.syncedAt)}.`
                : 'From the last run.'}
          </Typography.Text>

          {entries.length === 0
            ? (
              <Typography.Text type="secondary">
                Nothing has been written yet. Lines appear as the sync reaches each stage.
              </Typography.Text>
            )
            : (
              /*
                A timeline, because a sync IS one: a sequence of things that
                happened, in order, with gaps between them that mean something.
                It was a list of rows with a coloured dot floating between the
                time and the text, connected to nothing - so a reader could see
                the events and not the shape of the run, which is what tells a
                slow sync from a stuck one.
              */
              <Timeline
                mode="left"
                items={entries.map((e, i) => ({
                  key: `${e.at ?? ''}-${i}`,
                  color: LOG_COLOUR[e.level] ?? c.text2,
                  children: <LogLine entry={e} showDate={showDates} />,
                }))}
              />
            )}
        </Space>
      </Drawer>
    </>
  )
}

/** Whether a timestamp falls on today's date, in the reader's own zone. */
function isToday(at: string): boolean {
  const then = new Date(at)
  if (Number.isNaN(then.getTime())) return true
  const now = new Date()
  return then.getFullYear() === now.getFullYear()
    && then.getMonth() === now.getMonth()
    && then.getDate() === now.getDate()
}

/**
 * One line of the transcript: when, how important, and what happened.
 *
 * The level is a WORD as well as a colour. Everything on this page reads
 * correctly in greyscale, and a timeline whose only signal is the colour of a
 * six-pixel dot is the easiest place in the interface to forget that.
 */
function LogLine({ entry, showDate }: { entry: SecurityLogEntry; showDate: boolean }) {
  const when = entry.at ? new Date(entry.at) : undefined
  const valid = when && !Number.isNaN(when.getTime())
  const colour = LOG_COLOUR[entry.level] ?? c.text2

  return (
    <Space direction="vertical" size={2} style={{ width: '100%' }}>
      <Space size={8} wrap style={{ lineHeight: 1.2 }}>
        <Typography.Text
          type="secondary"
          style={{ fontFamily: mono, fontSize: 11, whiteSpace: 'nowrap' }}
          // The full timestamp on hover, always, whatever the line shows.
          title={valid ? formatAbsolute(entry.at) ?? when.toISOString() : undefined}
        >
          {valid
            ? showDate
              ? `${when.toLocaleDateString(undefined, { day: '2-digit', month: 'short' })} ${when.toLocaleTimeString()}`
              : when.toLocaleTimeString()
            : '--:--:--'}
        </Typography.Text>
        <Typography.Text
          style={{
            fontSize: 10, textTransform: 'uppercase', letterSpacing: '0.04em',
            color: colour, fontWeight: 600,
          }}
        >
          {LOG_LABEL[entry.level] ?? entry.level}
        </Typography.Text>
        {entry.repeat ? (
          <Typography.Text type="secondary" style={{ fontSize: 11 }}>
            x{entry.repeat + 1}
          </Typography.Text>
        ) : null}
      </Space>
      <Typography.Text style={{ color: entry.level === 'error' ? c.danger : undefined }}>
        {entry.message}
      </Typography.Text>
    </Space>
  )
}

// ---------------------------------------------------------------------------
// Export
// ---------------------------------------------------------------------------

/**
 * The export control: two answers, and one of them is running.
 *
 * # Why two, when there were nine
 *
 * It offered summary and full breakdown, each as CSV, Excel and JSON - and then
 * "this table" in two more formats. Nine items for a question with two real
 * answers: do you want to WORK on this, or do you want to SEND it. A workbook
 * is the first; the bundle, with the scanner's own responses beside the tables,
 * is the second. CSV and JSON remain on the API for anything scripted, where
 * a format parameter is the natural way to ask.
 *
 * # Why the download goes through fetch rather than an anchor
 *
 * Because a link cannot say it is working. The browser streams the file and
 * names it from the response - all correct - but the page has no idea, so an
 * export of a large release was a button that did nothing for eleven seconds,
 * and a reader who clicks twice starts two exports of ninety thousand rows.
 *
 * The button holds a spinner until the file has arrived, and the menu is
 * disabled while it does. See `download` in api/client: the filename still
 * comes from the server, because a client that invented one would drift from
 * the CLI's.
 */
export function SecurityExportMenu({
  urlFor, disabled, label = 'Export', workbookNote, bundleNote,
}: {
  /** Builds a URL for a format. `view` stays for the API's sake; both use `detailed`. */
  urlFor: (format: string, view: string) => string
  disabled?: boolean
  label?: string
  /**
   * What each file actually contains, in this context.
   *
   * Defaulted for a release's own security, and overridden by the comparison
   * view - whose bundle holds the change tables and NOT a directory of scanner
   * responses per image. A menu item that promised those would be describing a
   * file the reader is not about to get.
   */
  workbookNote?: string
  bundleNote?: string
}) {
  const [running, setRunning] = useState<string | null>(null)
  const { message } = App.useApp()

  const start = async (format: string, what: string) => {
    if (running) return
    setRunning(format)
    try {
      await download(urlFor(format, 'detailed'))
    } catch (err) {
      // Said out loud. A download that fails silently is indistinguishable
      // from one the browser is still thinking about, and the reader waits.
      message.error(
        err instanceof Error ? `${what} could not be exported: ${err.message}` : `${what} could not be exported`,
      )
    } finally {
      setRunning(null)
    }
  }

  const items = [
    {
      key: 'xlsx',
      icon: running === 'xlsx' ? <LoadingOutlined /> : <FileTextOutlined />,
      label: (
        <Space direction="vertical" size={0}>
          <Typography.Text>Excel workbook</Typography.Text>
          <Typography.Text type="secondary" style={{ fontSize: 11 }}>
            {workbookNote ?? 'A summary page, then every table on this tab'}
          </Typography.Text>
        </Space>
      ),
      onClick: () => void start('xlsx', 'The workbook'),
    },
    {
      key: 'zip',
      icon: running === 'zip' ? <LoadingOutlined /> : <DownloadOutlined />,
      label: (
        <Space direction="vertical" size={0}>
          <Typography.Text>Evidence bundle (ZIP)</Typography.Text>
          <Typography.Text type="secondary" style={{ fontSize: 11 }}>
            {bundleNote ?? 'The same tables, plus the scanner\u2019s own raw output per image'}
          </Typography.Text>
        </Space>
      ),
      onClick: () => void start('zip', 'The bundle'),
    },
  ]

  return (
    <Dropdown menu={{ items }} disabled={disabled || Boolean(running)} trigger={['click']}>
      <Button
        icon={running ? <LoadingOutlined /> : <DownloadOutlined />}
        loading={false}
        disabled={disabled}
      >
        {running ? 'Preparing…' : label}
      </Button>
    </Dropdown>
  )
}

// ---------------------------------------------------------------------------
// Shared table pieces
// ---------------------------------------------------------------------------

/**
 * A CVE identifier, monospace, with the scanner's own id behind it.
 *
 * `link` styles it as what it is: in both findings tables the identifier OPENS
 * the advisory beside the table, and it was rendered as plain body text - a
 * thing that does something, dressed as a thing that does not. Nobody clicks
 * what does not look clickable.
 */
export function CveCell({ cve, id, link }: { cve?: string; id?: string; link?: boolean }) {
  if (!cve && !id) return <Typography.Text type="secondary">-</Typography.Text>
  return (
    <Space direction="vertical" size={0}>
      <Typography.Text
        style={{
          fontFamily: mono,
          color: link ? c.brand : undefined,
          textDecoration: link ? 'underline' : undefined,
          textDecorationStyle: link ? 'dotted' : undefined,
          textUnderlineOffset: 3,
        }}
      >
        {cve || id}
      </Typography.Text>
      {cve && id && id !== cve && (
        <Typography.Text type="secondary" style={{ fontSize: 11, fontFamily: mono }}>{id}</Typography.Text>
      )}
    </Space>
  )
}

/**
 * An advisory's text, as much of it as fits, with the rest one click away.
 *
 * # Why a popover and not a tooltip
 *
 * The tooltip that used to carry the full text was scrollable, which is a
 * gesture a tooltip cannot support: it disappears when the pointer leaves the
 * cell, so reaching its scrollbar dismissed it. And an advisory is the one
 * thing on this page somebody wants to paste into a ticket - a tooltip has
 * nothing to copy from.
 *
 * A popover stays while it is being read, holds its own scroll, and carries the
 * copy button. The two clamped lines in the cell are unchanged: the table stays
 * a table.
 */
export function DescriptionCell({ summary, description, title, onOpen }: {
  summary?: string
  description?: string
  /** What the popover is about - the CVE, so a detached popover still says. */
  title?: string
  /** Opens the full detail, for a reader who wants everything rather than this. */
  onOpen?: () => void
}) {
  const [copied, setCopied] = useState(false)
  // The LONGER of the two: a summary is one line and a description is the
  // advisory. Whichever exists is what somebody came to read.
  const full = description || summary
  const short = summary || description

  if (!full) return <Typography.Text type="secondary">-</Typography.Text>

  const copy = () => {
    void navigator.clipboard?.writeText(full)
    setCopied(true)
    setTimeout(() => setCopied(false), 1600)
  }

  return (
    <Popover
      trigger="hover"
      placement="leftTop"
      mouseEnterDelay={0.35}
      title={
        <Space style={{ width: '100%', justifyContent: 'space-between' }} size={16}>
          <Typography.Text style={{ fontFamily: mono, fontSize: 12 }}>{title ?? 'Description'}</Typography.Text>
          <Space size={4}>
            <Button size="small" type="text" icon={<CopyOutlined />} onClick={copy}>
              {copied ? 'Copied' : 'Copy'}
            </Button>
            {onOpen && (
              <Button size="small" type="text" onClick={onOpen}>Details</Button>
            )}
          </Space>
        </Space>
      }
      content={
        <div style={{ width: 'min(520px, calc(100vw - 64px))', maxHeight: 320, overflow: 'auto' }}>
          <Typography.Paragraph style={{ margin: 0, whiteSpace: 'pre-wrap', fontSize: 13 }}>
            {full}
          </Typography.Paragraph>
        </div>
      }
    >
      <Typography.Paragraph
        style={{ margin: 0, cursor: onOpen ? 'pointer' : 'default' }}
        onClick={onOpen}
        ellipsis={{ rows: 2 }}
      >
        {short}
      </Typography.Paragraph>
    </Popover>
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
  if (!fixedIn || fixedIn.length === 0) return <StatusPill tone="ok">Fixable</StatusPill>
  return (
    <Tooltip title={fixedIn.join(', ')}>
      <Space direction="vertical" size={0}>
        <StatusPill tone="ok" style={{ marginInlineEnd: 0 }}>Fixable</StatusPill>
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
        <CheckCircleOutlined style={{ fontSize: 22, color: c.ok }} />
        <Typography.Text strong>No vulnerabilities found</Typography.Text>
        <Typography.Text type="secondary">All artifacts in scope were scanned and returned no findings.</Typography.Text>
      </Space>
    )
  }
  return (
    <Space direction="vertical" size={4} style={{ padding: 24 }}>
      <QuestionCircleOutlined style={{ fontSize: 22, color: c.text2 }} />
      <Typography.Text strong>No results available</Typography.Text>
      <Typography.Text type="secondary">
        This is an absence of scan results, not an absence of vulnerabilities.
      </Typography.Text>
    </Space>
  )
}
