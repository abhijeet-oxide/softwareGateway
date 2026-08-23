import { useEffect, useState } from 'react'
import type { ReactNode } from 'react'
import { Alert, Button, Drawer, Dropdown, Progress, Space, Tag, Tooltip, Typography } from 'antd'
import { formatRelative } from '../domain/format'
import {
  CheckCircleOutlined, CopyOutlined, DownloadOutlined, ExclamationCircleOutlined,
  FileTextOutlined, MinusCircleOutlined, QuestionCircleOutlined, SyncOutlined, WarningOutlined,
} from '@ant-design/icons'
import { mono, palette, semantic, severity as severityColour, severitySurface, verdict as verdictColour } from '../theme'
import { SEVERITIES } from '../api/types'
import type {
  PackageSecuritySummary, ScanStatus, SecurityCounts, SecurityCoverage,
  SecurityState, SecuritySyncStatus, Severity, Verdict,
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
    return (
      <Tag color="processing" icon={<SyncOutlined spin />} style={{ marginInlineEnd: 0 }}>
        Syncing
      </Tag>
    )
  }
  if (summary.state === '') {
    const clickable = Boolean(onSyncNotSynced)
    const title = clickable
      ? (notSyncedTooltip ?? 'Click to sync')
      : (notSyncedTooltip ?? 'This release has not been scanned. An unscanned release is not a release without vulnerabilities.')
    return (
      <Tooltip title={title}>
        <Tag
          color="default"
          style={{ marginInlineEnd: 0, cursor: clickable ? 'pointer' : 'default' }}
          onClick={clickable ? (e) => {
            e.preventDefault()
            e.stopPropagation()
            onSyncNotSynced?.()
          } : undefined}
        >
          Not synced
        </Tag>
      </Tooltip>
    )
  }
  if (summary.state === 'failed' && !summary.syncedAt) {
    return (
      <Tooltip title={summary.error}>
        <Tag color="error" style={{ marginInlineEnd: 0 }}>Sync failed</Tag>
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
          <QuestionCircleOutlined style={{ color: semantic.neutral }} />
          <Typography.Text type="secondary">No results</Typography.Text>
        </Space>
      </Tooltip>
    )
  }

  if (summary.counts.total === 0) {
    return (
      <Space direction="vertical" size={2}>
        <Space size={4}>
          <CheckCircleOutlined style={{ color: semantic.success }} />
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

  return (
    <Space direction="vertical" size={2} style={{ width: '100%', minWidth: 170 }}>
      <div
        style={{
          display: 'grid',
          gridTemplateColumns: 'repeat(2, minmax(0, 1fr))',
          columnGap: 10,
          rowGap: 4,
          fontSize: 12,
          lineHeight: 1.25,
        }}
      >
        {SEVERITIES.map((sev) => (
          (summary.counts.bySeverity[sev] > 0 || sev === 'unknown') ? (
            <Space key={sev} size={4} style={{ minWidth: 0 }}>
              <span
                aria-hidden
                style={{
                  display: 'inline-block',
                  width: 7,
                  height: 7,
                  borderRadius: '50%',
                  background: severityColour[sev],
                }}
              />
              <span style={{ color: '#6B7280' }}>{SEVERITY_LABEL[sev]}</span>
              <strong style={{ color: severityColour[sev], fontWeight: 600 }}>
                {summary.counts.bySeverity[sev].toLocaleString()}
              </strong>
            </Space>
          ) : null
        ))}
        <Space size={4} style={{ minWidth: 0 }}>
          <span style={{ color: '#6B7280' }}>Total</span>
          <strong>{summary.counts.total.toLocaleString()}</strong>
        </Space>
      </div>

      {!summary.complete && (
        <Typography.Text type="secondary" style={{ color: semantic.warning, fontStyle: 'italic', fontSize: 11 }}>
          Not all artifacts were scanned.
        </Typography.Text>
      )}
      {stale && (
        <Typography.Text type="secondary" style={{ color: semantic.warning, fontStyle: 'italic', fontSize: 11 }}>
          Last sync failed. These are the last good results.
        </Typography.Text>
      )}
    </Space>
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
      return <Tag color="success">{STATUS_LABEL.scanned}</Tag>
    case 'not_scanned':
      return <Tag color="warning">{STATUS_LABEL.not_scanned}</Tag>
    // Not a scanning problem at all: the image was never shipped here, so it
    // gets its own word rather than being rounded to "not scanned".
    case 'not_found':
      return <Tag color="default">{STATUS_LABEL.not_found}</Tag>
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
export function SecurityProgressPanel({ sync }: { sync: SecuritySyncStatus }) {
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

  const elapsed = useElapsed(sync.startedAt, sync.state === 'syncing')

  return (
    <Space direction="vertical" size={12} style={{ width: '100%', padding: '4px 0' }}>
      <Space style={{ width: '100%', justifyContent: 'space-between' }} align="start">
        <Space size={8}>
          <SyncOutlined spin style={{ color: palette.primary }} />
          <Typography.Text strong>
            {total > 0
              ? `Retrieving results for ${total.toLocaleString()} images from ${scannerName(sync)}`
              : 'Resolving the release'}
          </Typography.Text>
        </Space>
        {elapsed && (
          <Typography.Text type="secondary" style={{ fontFamily: mono, fontSize: 12 }}>{elapsed}</Typography.Text>
        )}
      </Space>

      <div>
        <Progress
          percent={percent}
          status="active"
          showInfo={false}
          strokeColor={palette.primary}
        />
        <Space size={16} wrap style={{ marginTop: 4 }}>
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            {total > 0
              ? `${done.toLocaleString()} of ${total.toLocaleString()} images`
              : 'Preparing'}
          </Typography.Text>
          {cached && cached.done > 0 && (
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              {cached.done.toLocaleString()} read from storage
            </Typography.Text>
          )}
          {/*
            Failures shown AS THEY HAPPEN, not at the end. A sync against a
            scanner that is timing out looks identical to a healthy one for two
            minutes, and then delivers the bad news all at once - by which time
            the person who could have cancelled it has walked away.
          */}
          {failing && failing.done > 0 && (
            <Typography.Text style={{ fontSize: 12, color: semantic.error }}>
              {failing.done.toLocaleString()} not retrieved
            </Typography.Text>
          )}
          {sync.repository && (
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              {scannerName(sync)} · {sync.repository}
            </Typography.Text>
          )}
        </Space>
      </div>

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
            No live position means the sync is running on another replica. Its
            state is still authoritative, so the honest thing is to say where it
            is happening rather than to draw a bar at nothing.
          */}
          This sync is running on another Coordinator. Its progress is not
          available here; the result will appear once it completes.
        </Typography.Text>
      )}
    </Space>
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
export function SyncButton({ sync, onSync, pending, size = 'middle' }: {
  sync: SecuritySyncStatus
  onSync: () => void
  pending?: boolean
  size?: 'small' | 'middle'
}) {
  if (!sync.canSync) {
    return (
      <Tooltip title={sync.reason}>
        <Button size={size} icon={<SyncOutlined />} disabled>Sync vulnerabilities</Button>
      </Tooltip>
    )
  }
  const running = sync.state === 'syncing'
  return (
    <Tooltip
      title={running
        ? 'A sync is already running for this release.'
        : `Asks ${sync.provider === 'jfrog-xray' ? 'JFrog Xray' : 'the scanner'}${sync.repository ? ` in ${sync.repository}` : ''} about every artifact in this release and stores the answer.`}
    >
      <Button
        size={size}
        type={sync.state === '' ? 'primary' : 'default'}
        icon={<SyncOutlined spin={running || pending} />}
        loading={pending}
        disabled={running}
        onClick={onSync}
      >
        {running ? 'Syncing' : sync.state === '' ? 'Sync vulnerabilities' : 'Sync again'}
      </Button>
    </Tooltip>
  )
}

/** When a release was last synced, and by which scanner. */
export function SyncedAgo({ sync }: { sync: SecuritySyncStatus }) {
  if (!sync.syncedAt) return null
  return (
    <Typography.Text type="secondary" style={{ fontSize: 12 }}>
      {sync.provider === 'jfrog-xray' ? 'JFrog Xray' : sync.provider}
      {sync.repository && ` · ${sync.repository}`}
      {` · synced ${formatRelative(sync.syncedAt)}`}
    </Typography.Text>
  )
}

// ---------------------------------------------------------------------------
// The sync's own log
// ---------------------------------------------------------------------------

const LOG_COLOUR: Record<string, string> = {
  error: semantic.error,
  warning: semantic.warning,
  info: semantic.neutral,
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
  const running = sync.state === 'syncing'

  if (entries.length === 0 && !running) {
    return null
  }

  const plain = entries
    .map((e) => `${e.at ? new Date(e.at).toISOString() : ''} [${e.level}] ${e.message}`
      + (e.repeat ? ` (x${e.repeat + 1})` : ''))
    .join('\n')

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
        <Space direction="vertical" size={12} style={{ width: '100%' }}>
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
            : entries.map((e, i) => (
              <div
                key={`${e.at ?? ''}-${i}`}
                style={{
                  display: 'flex', gap: 10, alignItems: 'baseline',
                  paddingBottom: 8, borderBottom: '1px solid #F0F0F0',
                }}
              >
                <Typography.Text
                  type="secondary"
                  style={{ fontFamily: mono, fontSize: 11, whiteSpace: 'nowrap' }}
                >
                  {e.at ? new Date(e.at).toLocaleTimeString() : '--:--:--'}
                </Typography.Text>
                <span
                  aria-hidden
                  style={{
                    display: 'inline-block', width: 8, height: 8, borderRadius: '50%',
                    background: LOG_COLOUR[e.level] ?? semantic.neutral, flex: '0 0 auto',
                  }}
                />
                <Typography.Text
                  style={{ color: e.level === 'error' ? semantic.error : undefined }}
                >
                  {e.message}
                  {e.repeat ? (
                    <Typography.Text type="secondary" style={{ fontSize: 11 }}>
                      {' '}(x{e.repeat + 1})
                    </Typography.Text>
                  ) : null}
                </Typography.Text>
              </div>
            ))}
        </Space>
      </Drawer>
    </>
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
        <Typography.Text type="secondary">All artifacts in scope were scanned and returned no findings.</Typography.Text>
      </Space>
    )
  }
  return (
    <Space direction="vertical" size={4} style={{ padding: 24 }}>
      <QuestionCircleOutlined style={{ fontSize: 22, color: semantic.neutral }} />
      <Typography.Text strong>No results available</Typography.Text>
      <Typography.Text type="secondary">
        This is an absence of scan results, not an absence of vulnerabilities.
      </Typography.Text>
    </Space>
  )
}
