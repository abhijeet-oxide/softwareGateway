import { useState } from 'react'
import type { ReactNode } from 'react'
import {
  Alert, Button, Drawer, Dropdown, Popover, Progress, Space, Tag, Timeline, Tooltip, Typography,
} from 'antd'
import { formatAbsolute, formatRelative } from '../domain/format'
import { ExportMenu } from './exportmenu'
import {
  CheckCircleOutlined, CopyOutlined, DownOutlined, ExclamationCircleOutlined,
  FileTextOutlined, MinusCircleOutlined, QuestionCircleOutlined, StopOutlined,
  SyncOutlined, WarningOutlined, FolderZip24RegularIcon,
} from '../icons'
import {
  c, mono, severity as severityColour, severitySurface, StatusPill, tokens,
  verdict as verdictColour, withAlpha,
} from '../uikit'
import { RunPanel, runEventsFromLog } from './runpanel'
import { ScannerMark } from './icons'
import { kevColour, KevTag } from './securitykev'
import type { RunTile } from './runtiles'
import { SEVERITIES } from '../api/types'
import type {
  PackageSecuritySummary, ScanStatus, SecurityCounts, SecurityCoverage,
  SecurityFreshness, SecurityLogEntry, SecurityState, SecuritySyncStatus, Severity, Verdict,
  SecurityRegistration,
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
export function SeverityMeter({ counts, width, compact = false, secondaryLabel }: {
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
  secondaryLabel?: string
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
          <span style={{ display: 'inline-flex', alignItems: 'baseline', gap: 4 }}>
            <span style={{ fontSize: 13, fontWeight: 600, color: c.text, lineHeight: 1 }}>
              {counts.total.toLocaleString()}
            </span>
            {secondaryLabel && (
              <span style={{ fontSize: 10, color: c.text3 }}>{secondaryLabel}</span>
            )}
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
      <SeverityMeter counts={summary.uniqueCveCounts} compact secondaryLabel="UNIQUE" />
      {/*
        The exploited count, on the LISTING and not only on the release page.

        # Why it belongs in a cell this narrow

        Because the listing is where somebody scans twenty releases looking for
        the one to worry about, and severity alone cannot tell them: every
        release of a large product has criticals, and the one with a KEV is a
        different kind of row. It is the only thing in this cell that is
        usually absent, which is what makes it readable when it is not.

        Never at zero. A badge reading "0 exploited" on nineteen rows is
        nineteen rows of noise around the one that matters.
      */}
      {summary.kevs > 0 && (
        <Tooltip
          title={
            `${summary.kevs.toLocaleString()} known-exploited `
            + `${summary.kevs === 1 ? 'vulnerability' : 'vulnerabilities'} in this release`
            + (summary.kevFixable > 0 ? `, ${summary.kevFixable.toLocaleString()} with a fix.` : '.')
            + ' These are advisories somebody has already been attacked through, not a prediction'
            + ' that this release could be.'
          }
        >
          <Tag
            color={kevColour.fill}
            style={{ marginInlineEnd: 0, flex: '0 0 auto', fontSize: 10.5, fontWeight: 600, lineHeight: '16px' }}
          >
            {summary.kevs} KEV
          </Tag>
        </Tooltip>
      )}
      {caveats.length > 0 && (
        <Tooltip title={caveats.join(' ')}>
          <span style={{ display: 'inline-flex', flex: '0 0 auto' }}>
            <WarningOutlined style={{ color: c.pending }} />
          </span>
        </Tooltip>
      )}
      {/*
        Which scanners these numbers came from, once there is more than one.

        A dot rather than the names: the cell is narrow and "Xray + Anchore" is
        wider than the meter it would sit beside. What matters on a listing is
        that a release synced by both is not being compared like-for-like with
        one synced by a single scanner, and the tooltip says which.
      */}
      {(summary.providers?.length ?? 0) > 1 && (
        <Tooltip title={`Scanned by ${(summary.providers ?? []).map(scannerName).join(' and ')}.`}>
          <Typography.Text type="secondary" style={{ fontSize: 10, flex: '0 0 auto' }}>
            {summary.providers?.length}x
          </Typography.Text>
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
  // Named for the FACT, not for one scanner. The image is not where the
  // scanner that answered pulls from - which for Anchore need not be JFrog at
  // all - and the row's message names the actual repository.
  not_found: 'Not in registry',
  unsupported: 'Not applicable',
  disabled: 'Scanner off',
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
          repository: set <code>xrayEnabled: true</code> on the JFrog repository this release is
          replicated to, or <code>anchoreEnabled: true</code> on the repository Anchore should
          analyse. Both may be on at once, and their findings are merged.
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
  verdict, headline, explanation, caveats, extra, title,
}: {
  verdict: Verdict
  headline: string
  explanation: string
  caveats?: string[]
  extra?: ReactNode
  /**
   * Overrides the word the verdict maps to.
   *
   * For the one case where the verdict is too weak a word for what was found:
   * two releases of the same bytes are not "no meaningful change", they are
   * the same software, and a reader who is told the first has to work out the
   * second from a table of noughts.
   */
  title?: string
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
              {title ?? (verdict === 'inconclusive' ? 'Not enough information' : VERDICT_WORD[verdict])}
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
export function SecurityProgressPanel({ sync, onStop, stopping, starting }: {
  sync: SecuritySyncStatus
  /** Offered whenever a sync can be stopped, which is whenever one is running. */
  onStop?: () => void
  stopping?: boolean
  /**
   * The request that takes the claim is still in flight.
   *
   * Between the press and the server's answer there is a round trip, and until
   * this existed the tab was unchanged for all of it - so on a busy Coordinator
   * the button appeared not to have worked and people pressed it again. The
   * panel appears on the press and fills in when the claim comes back.
   */
  starting?: boolean
}) {
  const stages = sync.stages ?? []
  const notes = sync.notes ?? []
  const log = sync.log ?? []

  const fetching = stages.find((st) => st.name === 'fetching')
  const failing = stages.find((st) => st.name === 'failing')
  const cached = stages.find((st) => st.name === 'cached')
  const resolving = stages.find((st) => st.name === 'resolving')

  // The one bar worth drawing: artifacts answered for, out of artifacts asked
  // about. Everything else is a counter, because a second bar next to a first
  // one invites the reader to compare two things that are not comparable.
  const total = fetching?.total ?? resolving?.total ?? 0
  const done = fetching?.done ?? 0

  /*
   * THIS sync's clock, not the last one's.
   *
   * `startedAt` still holds the previous run until the claim comes back, so
   * counting from it during the press read "9579m 18s elapsed" - a duration
   * since a sync six days ago, printed beside a bar for one that had not begun.
   * A run that has not started has no elapsed time, and no number is the honest
   * thing to show for one.
   */
  const live = sync.state === 'syncing' && !sync.stalled

  const scanner = syncScannerName(sync)
  const headline = starting && total === 0
    ? `Starting the vulnerability sync against ${scanner}`
    : total > 0
      ? `Retrieving results for ${total.toLocaleString()} images from ${scanner}`
      // No stages at all is a sync running on another Coordinator, and this
      // replica does not know what step it has reached. Saying it is resolving
      // artifacts would be inventing a position.
      : stages.length === 0
        ? 'Vulnerability sync in progress on another Coordinator'
        : 'Working out which of this release\u2019s artifacts to ask about'

  return (
    <RunPanel
      title={`Vulnerability sync against ${scanner}`}
      titleIcon={<ScannerMark provider={sync.provider} size={16} />}
      // The repository only. The headline already names the scanner, and
      // "JFrog Xray" under "…from JFrog Xray" is the same fact twice.
      subtitle={sync.repository}
      // THIS sync's clock, not the last one's. `startedAt` still holds the
      // previous run until the claim comes back, and counting from it during
      // the press read "9579m 18s elapsed" - a duration since a sync six days
      // ago, beside a bar for one that had not begun.
      startedAt={live ? sync.startedAt : undefined}
      onStop={onStop}
      stopping={stopping}
      stopLabel="Stop sync"
      stopHint={
        'Stops the sync. Nothing already stored is lost - the release keeps whatever its '
        + 'last completed sync recorded.'
      }
      label={headline}
      detail={
        stages.length === 0 && !starting && sync.heartbeatAt
          ? `Running elsewhere; it last reported ${formatRelative(sync.heartbeatAt)}. `
            + 'The result appears here once it completes.'
          : undefined
      }
      done={done}
      total={total}
      estimate={live ? estimateRemainingSeconds(sync.startedAt, done, total) : undefined}
      indeterminate={done === 0}
      tiles={syncTiles({ total, done, cached: cached?.done ?? 0, failed: failing?.done ?? 0 })}
      notes={notes.slice(-2)}
      // The transcript of THIS sync. While the claim is still being taken the
      // stored log is the previous run's, and a list of finished lines under a
      // bar for a sync that has not begun reads as progress it has already made.
      events={live ? runEventsFromLog(log, sync.startedAt).slice(-40) : []}
      eventsLabel="Sync log"
    />
  )
}

/**
 * The four numbers a sync produces, as counters.
 *
 * Images read from storage is here beside the ones fetched because it is what
 * explains a sync that finishes in twenty seconds after promising three hundred
 * requests - without it, a fast sync reads as a sync that did not run.
 */
function syncTiles({ total, done, cached, failed }: {
  total: number
  done: number
  cached: number
  failed: number
}): RunTile[] {
  const tiles: RunTile[] = []
  if (total > 0) {
    tiles.push({
      label: 'Images to scan', value: total.toLocaleString(),
      hint: 'Artifacts of this release the scanner can be asked about',
    })
    tiles.push({
      label: 'Results retrieved', value: done.toLocaleString(),
      hint: 'Artifacts the scanner has answered for so far',
    })
  }
  if (cached > 0) {
    tiles.push({
      label: 'Read from storage', value: cached.toLocaleString(), tone: c.ok,
      hint: 'Already answered for by an earlier sync, so the scanner was not asked again. '
        + 'This is why a sync over three hundred images can finish in twenty seconds.',
    })
  }
  if (failed > 0) {
    tiles.push({
      label: 'Not retrieved', value: failed.toLocaleString(), tone: c.danger,
      hint: 'The scanner did not answer for these. They are recorded as unscanned - an '
        + 'artifact with no result is not an artifact with no vulnerabilities.',
    })
  }
  return tiles
}

/**
 * How much longer, from this sync's own rate, IN SECONDS.
 *
 * Withheld until a tenth of the work is done or five images have come back,
 * whichever is sooner: an estimate extrapolated from one slow first request is
 * a number that turns out four times wrong, and a confident wrong number is
 * worse than none. Absent once the work is finished, because zero remaining is
 * not an estimate.
 *
 * Seconds rather than a formatted string, because the panel that draws it
 * formats every other duration on the page through one function and a second
 * spelling of "4m 12s" beside it is how two of them come to disagree.
 */
function estimateRemainingSeconds(
  startedAt: string | undefined, done: number, total: number,
): number | undefined {
  if (!startedAt || total <= 0 || done <= 0 || done >= total) return undefined
  if (done < Math.min(5, Math.ceil(total / 10))) return undefined
  const started = Date.parse(startedAt)
  if (Number.isNaN(started)) return undefined
  const perItem = (Date.now() - started) / done
  const seconds = Math.round((perItem * (total - done)) / 1000)
  return seconds > 0 ? seconds : undefined
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

/**
 * The scanner behind a sync, in the words the interface shows.
 *
 * Wraps scannerName so a sync with no recorded provider - a release nobody has
 * synced - still has a noun to put in a sentence.
 */
function syncScannerName(sync: SecuritySyncStatus): string {
  return sync.provider ? scannerName(sync.provider) : 'the scanner'
}

/** A scanner's name in the words the interface shows. */
export function scannerName(provider: string): string {
  switch (provider) {
    case 'jfrog-xray': return 'JFrog Xray'
    case 'anchore': return 'Anchore'
    case 'astra': return 'Astra'
    default: return provider
  }
}

/**
 * The one control that talks to a scanner.
 *
 * Its label changes with the state, because "Sync vulnerabilities" and "Sync
 * again" are different offers: the first is the only way to get any answer at
 * all, and the second is a refresh of one that exists. A button that said the
 * same thing in both places would make the first look optional.
 */
export function SyncButton({ sync, onSync, pending, size = 'middle', freshness, providers }: {
  sync: SecuritySyncStatus
  /**
   * `force` asks the scanner about every image, ignoring what is stored.
   *
   * The plain press does not. Stored answers are keyed by image and releases
   * of one product share nearly all of theirs, so an ordinary sync asks about
   * the images that are missing or past the age limit and reuses the rest -
   * which is the difference between a sync of seven images and one of a
   * hundred and fifty-seven against somebody else's rate limit.
   *
   * `provider` narrows it to ONE scanner. See the menu below for why that is
   * worth offering.
   */
  onSync: (force?: boolean, provider?: string) => void
  pending?: boolean
  size?: 'small' | 'middle'
  freshness?: SecurityFreshness
  /**
   * Every scanner configured for this release.
   *
   * Fewer than two draws no per-scanner menu: a "sync JFrog Xray" item beside
   * a "sync" button, on a deployment with only Xray, is two names for one
   * action.
   */
  providers?: string[]
}) {
  if (!sync.canSync) {
    return (
      <Tooltip title={sync.reason}>
        {/* spin={false}: the sync glyph turns by default, and a disabled
            control that animates reads as work already under way. */}
        <Button size={size} icon={<SyncOutlined spin={false} />} disabled>Sync vulnerabilities</Button>
      </Tooltip>
    )
  }
  // A stalled claim is not a running sync, and a button that refuses because of
  // one is a release nobody can sync until a sweeper notices. The server takes
  // the claim from a run that has stopped beating, so this offer is real.
  const running = sync.state === 'syncing' && !sync.stalled
  const named = providers ?? []
  const scanner = named.length > 0
    ? named.map(scannerName).join(' and ')
    : sync.provider ? scannerName(sync.provider) : 'the scanner'
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
        onClick={() => onSync(sync.state !== '')}
      >
        {running ? 'Syncing' : sync.state === '' ? 'Sync vulnerabilities' : 'Sync again'}
      </Button>
    </Tooltip>
  )

  // Nothing to reuse and nothing to narrow means the menu would be two names
  // for one action.
  if ((!reuse && named.length < 2) || running) return button

  /*
   * ONE SCANNER AT A TIME, and why that is worth a menu item each.
   *
   * The scanners here fail and finish at very different speeds. Xray answers
   * about a release in tens of seconds because it indexes a repository;
   * Anchore's first sync of a release submits every image and then waits
   * minutes for analysis it does not control. A reader who wants Xray's view
   * refreshed after a re-transfer should not have to sit through the other
   * half - and somebody whose Anchore is mid-analysis should be able to ask it
   * again without re-fetching a scan that is already current.
   *
   * The plain button still syncs everything, because that is what "sync this
   * release" means and it is the right default nine times out of ten.
   */
  const items = [
    ...(reuse
      // spin={false} on every one of these: the sync glyph turns by default,
      // so a closed menu opened onto three items animating as though all three
      // were already running.
      ? [{ key: 'force', icon: <SyncOutlined spin={false} />, label: 'Re-fetch every image' }]
      : []),
    ...(named.length > 1
      ? [{
        type: 'divider' as const,
        key: 'divider',
      }, ...named.map((p) => ({
        key: `only:${p}`,
        // The scanner's OWN mark. This menu exists because the two scanners
        // behave differently, and two identical glyphs beside two names is the
        // one place that difference should be visible before the label is read.
        icon: <ScannerMark provider={p} />,
        label: `Sync ${scannerName(p)} only`,
      }))]
      : []),
  ]

  return (
    <Space.Compact>
      {button}
      <Dropdown
        disabled={pending}
        menu={{
          items,
          onClick: ({ key }) => {
            if (key === 'force') {
              onSync(true)
              return
            }
            if (key.startsWith('only:')) onSync(true, key.slice(5))
          },
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
export function SyncedAgo({ sync, freshness, registrations, providers }: {
  sync: SecuritySyncStatus
  /**
   * The deployment's rule about how old is too old.
   *
   * The age was always here; what was missing was whether it MATTERS. "synced
   * 11 days ago" reads as a fact about the past until something says the
   * deployment considers a week old, and then it reads as a thing to do.
   */
  freshness?: SecurityFreshness
  registrations?: SecurityRegistration[]
  providers?: string[]
}) {
  const anchore = (registrations ?? []).find((registration) => registration.provider === 'anchore' && registration.registeredAt)
  const active = sync.state === 'syncing' && !sync.stalled
  const activeProvider = sync.provider
  const priorProvider = (providers ?? []).find((provider) => provider !== activeProvider) ?? activeProvider
  if (!sync.syncedAt && !anchore) return null
  return (
    <Space size={8} align="center" wrap>
      {sync.syncedAt && (
        <Tooltip title={formatAbsolute(sync.syncedAt)}>
          <Space size={4}>
            <ScannerMark provider={active ? priorProvider : sync.provider} size={14} />
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              {active ? (priorProvider ? scannerName(priorProvider) : 'The scanner') : syncScannerName(sync)} retrieved {formatRelative(sync.syncedAt)}
            </Typography.Text>
          </Space>
        </Tooltip>
      )}
      {active && activeProvider && (
        <Space size={4}>
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>·</Typography.Text>
          <ScannerMark provider={activeProvider} size={14} />
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            {scannerName(activeProvider)} syncing now
          </Typography.Text>
        </Space>
      )}
      {anchore?.registeredAt && !active && (
        <Tooltip title={formatAbsolute(anchore.registeredAt)}>
          <Space size={4}>
            <ScannerMark provider="anchore" size={14} />
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              Anchore updated {formatRelative(anchore.registeredAt)}
            </Typography.Text>
          </Space>
        </Tooltip>
      )}
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
  return (
    <ExportMenu
      label={label}
      disabled={disabled}
      choices={[
        {
          key: 'xlsx',
          icon: <FileTextOutlined />,
          label: 'Excel workbook',
          note: workbookNote ?? 'A summary page, then every table on this tab',
          href: urlFor('xlsx', 'detailed'),
          noun: 'The workbook',
        },
        {
          key: 'zip',
          icon: <FolderZip24RegularIcon />,
          label: 'Evidence bundle (ZIP)',
          note: bundleNote ?? 'The same tables, plus the scanner\u2019s own raw output per image',
          href: urlFor('zip', 'detailed'),
          noun: 'The bundle',
        },
      ]}
    />
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
export function CveCell({ cve, id, link, kev, kevSource }: {
  cve?: string
  id?: string
  link?: boolean
  /**
   * Known-exploited, badged in line with the identifier.
   *
   * # Why here rather than in a column of its own
   *
   * Because a column is a thing you read when you get to it, and this has to be
   * read at the same instant as the CVE it qualifies. It is also the badge that
   * survives every one of this table's layouts - the columns are reorderable
   * and hideable, and an exploited flag somebody had dragged off screen would
   * be a fact the page can lose.
   */
  kev?: boolean
  kevSource?: string
}) {
  if (!cve && !id) return <Typography.Text type="secondary">-</Typography.Text>
  return (
    <Space direction="vertical" size={0}>
      <Space size={6} align="center">
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
        {kev && <KevTag source={kevSource} compact />}
      </Space>
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
