import { useEffect, useState } from 'react'
import type { ReactNode } from 'react'
import {
  Alert, Button, Drawer, Progress, Space, Tag, Timeline, Tooltip, Typography,
} from 'antd'

import { CloseOutlined, CopyOutlined, FileTextOutlined, LoadingOutlined } from '../icons'
import { formatDuration } from '../domain/format'
import { RunTiles } from './runtiles'
import type { RunTile } from './runtiles'
import { c, mono } from '../uikit'

/**
 * THE panel every long-running job in this product draws itself with.
 *
 * # Why one component and not four
 *
 * There are four of these - the compliance check, the vulnerability sync, the
 * replication to Anchore, and whatever is added next - and they are the same
 * screen. Each is minutes of work against somebody else's system, each is
 * watched by the same person on the same release, and each is read to answer
 * the same two questions: is this working at all, and what is the answer going
 * to be missing.
 *
 * They used to answer those in three visual languages. The compliance run had a
 * stage route, counters of real things and a timeline; the sync had a bar and a
 * row of grey sentences; the replication had a button that went nowhere for
 * thirty seconds and then a toast. A reader who learned one learned nothing
 * about the others, and the two that were missing pieces were missing them
 * because nobody had noticed the third already had them.
 *
 * So the SHAPE lives here and the four supply their own facts:
 *
 *   headline      what it is doing, in a sentence somebody can act on
 *   position      done of total, with the bar and the estimate
 *   in flight     the names of the things being worked on RIGHT NOW
 *   route         the stages, with what each finished one cost
 *   counters      real objects, with failures beside successes
 *   transcript    what has happened, in order
 *
 * # Why "in flight" is the load-bearing part
 *
 * A bar and a count answer "how far". They do not answer the question somebody
 * actually asks in front of a job that has not moved for a minute, which is
 * whether anything is happening at all - and a list of names that changes every
 * few seconds answers that however still the bar is. It is also the only thing
 * on the panel that tells a slow registry from a wedged one.
 */

export type RunEventKind = 'info' | 'ok' | 'warn' | 'fail'

/** One line of a run's transcript. */
export interface RunEvent {
  /** Wall-clock time, for a log read a week later. On hover only. */
  at?: string
  /** Seconds from the start of the run, which is what the line is stamped with. */
  sec?: number
  kind: RunEventKind
  text: string
  /** Identical consecutive lines, collapsed. */
  repeat?: number
}

/** One stage of a run, and what it cost if it is over. */
export interface RunStep {
  key: string
  label: string
  /** Seconds. Present on a finished stage, which is what makes the route useful. */
  seconds?: number
  state: 'done' | 'current' | 'pending'
}

/** One thing being worked on right now. */
export interface RunChip {
  key: string
  label: string
  icon?: ReactNode
}

export interface RunPanelProps {
  /** "Compliance check in progress". The job, not the stage. */
  title: string
  /** The mark of whatever the job is talking to, beside the spinner. */
  titleIcon?: ReactNode
  /** Under the title - the repository, the scanner, whatever names the target. */
  subtitle?: ReactNode
  /** Seconds since the run started. The clock that proves the page is live. */
  elapsed?: number
  /**
   * When the run started, so the clock can tick between polls.
   *
   * Preferred over `elapsed`, which is only as fresh as the last response: at a
   * 1.5s poll the number froze between answers and read as a stalled job, which
   * is the one thing this clock exists to rule out.
   */
  startedAt?: string

  onStop?: () => void
  stopping?: boolean
  stopLabel?: string
  stopHint?: string

  /**
   * The run has ENDED, and this panel is its record.
   *
   * A panel that vanished the moment the work stopped took the transcript, the
   * counts and the reason with it - which is precisely when a failed run
   * becomes worth reading. Finished, it loses the spinner and the clock and
   * gains a close.
   */
  finished?: boolean
  onClose?: () => void

  /**
   * How the run ENDED, which is what the bar is coloured by once it has.
   *
   * Blue is "running". A bar left blue on a finished run says the work is still
   * going, and a full blue bar over "0 replicated, 154 rejected" reads as a
   * success at a glance - which is the reading this is here to prevent.
   */
  tone?: 'ok' | 'danger'

  /** The current stage, as a sentence. */
  label: string
  detail?: string
  done: number
  total: number
  /** Seconds remaining IN THIS STAGE. Never a whole-run guess - see below. */
  estimate?: number

  /**
   * Draw a travelling stripe rather than a bar at zero.
   *
   * A determinate bar at 0% is a claim, and the claim it makes is "nothing has
   * happened". The first request to a scanner about fifty images takes as long
   * as it takes, and for that whole minute the bar sat at zero - which is what
   * a stuck job looks like. It was not stuck; there was nothing to report a
   * position for yet, and the stripe says exactly that.
   */
  indeterminate?: boolean

  concurrency?: number
  concurrencyLabel?: string
  concurrencyHint?: string
  active?: RunChip[]
  /** How many chips before the row collapses to "+N more". */
  activeLimit?: number

  steps?: RunStep[]
  tiles?: RunTile[]
  /** What is going WRONG. Positions belong in the bar, not here. */
  notes?: string[]
  events?: RunEvent[]
  /** Heading over the transcript. "Run log" by default. */
  eventsLabel?: string
}

/**
 * The body of a run panel: everything but the card around it.
 *
 * Separate from RunCard because the sync draws this inside a card the Security
 * tab already owns, and the compliance run draws its own. One component that
 * sometimes drew a card and sometimes did not would need a prop to say which,
 * and that prop is this split.
 */
export function RunPanel(props: RunPanelProps) {
  return (
    <Space direction="vertical" size={16} style={{ width: '100%' }}>
      <RunHeadline {...props} />
      <RunBar {...props} />
      <RunActive {...props} />
      <RunRoute steps={props.steps} />
      {props.tiles && props.tiles.length > 0 && <RunTiles tiles={props.tiles} />}
      <RunNotes notes={props.notes} />
      <RunEventLog events={props.events} label={props.eventsLabel} />
    </Space>
  )
}

/**
 * How long a run has been going, in seconds, ticking.
 *
 * # Why this is not the server's number
 *
 * The server sends one too, and it is correct at the instant it is computed.
 * But it only reaches the page on a poll, so between answers the clock stood
 * still - and a duration that stops moving is exactly what a stalled job looks
 * like, which is the one reading this clock exists to rule out. Counting from
 * the start time here means it advances every second whatever the network is
 * doing, and the poll only ever corrects it.
 *
 * Returns undefined without a start time, so a caller can fall back.
 */
export function useElapsedSeconds(
  startedAt: string | undefined, running: boolean,
): number | undefined {
  const [now, setNow] = useState(() => Date.now())

  useEffect(() => {
    if (!running || !startedAt) return
    const id = setInterval(() => setNow(Date.now()), 1000)
    return () => clearInterval(id)
  }, [running, startedAt])

  if (!startedAt) return undefined
  const started = Date.parse(startedAt)
  if (Number.isNaN(started)) return undefined
  return Math.max(0, Math.round((now - started) / 1000))
}

/** The same panel, inside the card the compliance run and the replication use. */
export function RunCard(props: RunPanelProps) {
  return (
    <div
      style={{
        border: `1px solid ${c.border}`,
        borderRadius: 10,
        background: c.surface,
        padding: 16,
      }}
    >
      <RunPanel {...props} />
    </div>
  )
}

/** The job, the clock, and the stop. */
function RunHeadline({
  title, titleIcon, subtitle, elapsed, startedAt, onStop, stopping, stopLabel, stopHint,
  finished, onClose,
}: RunPanelProps) {
  // Ticking, and from the START TIME rather than from the server's count: the
  // count is only as fresh as the last poll, so it stood still between answers.
  const ticking = useElapsedSeconds(startedAt, !finished)
  const shown = ticking ?? elapsed

  return (
    <Space style={{ width: '100%', justifyContent: 'space-between' }} align="start" wrap>
      <Space direction="vertical" size={0}>
        <Space size={10}>
          {/* No spinner over work that has stopped, and no clock: a duration
              that ticks on a finished run is the panel claiming to be live. */}
          {!finished && <LoadingOutlined spin style={{ color: c.brand }} />}
          {titleIcon}
          <Typography.Text strong>{title}</Typography.Text>
          {!finished && shown !== undefined && (
            <Typography.Text type="secondary" style={{ fontSize: 12, fontFamily: mono }}>
              {formatDuration(shown) ?? '0s'} elapsed
            </Typography.Text>
          )}
        </Space>
        {subtitle && (
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            {subtitle}
          </Typography.Text>
        )}
      </Space>
      {finished && onClose ? (
        <Tooltip title="Dismisses this record. The transcript stays available from the log.">
          <Button size="small" danger icon={<CloseOutlined />} onClick={onClose}>
            Close
          </Button>
        </Tooltip>
      ) : onStop ? (
        <Tooltip title={stopHint}>
          <Button size="small" danger loading={stopping} onClick={onStop}>
            {stopLabel ?? 'Stop'}
          </Button>
        </Tooltip>
      ) : null}
    </Space>
  )
}

/** What it is doing, how far, and how much longer. */
function RunBar({ label, detail, done, total, estimate, indeterminate, finished, tone }: RunPanelProps) {
  const percent = total > 0 ? Math.min(100, Math.round((done / total) * 100)) : 0
  const stripe = !finished && (indeterminate ?? (total === 0 || done === 0))
  const stroke = tone === 'danger' ? c.danger : tone === 'ok' ? c.ok : c.brand

  return (
    <Space direction="vertical" size={6} style={{ width: '100%' }}>
      <Space style={{ width: '100%', justifyContent: 'space-between' }} align="start" wrap>
        <Space direction="vertical" size={0}>
          <Typography.Text strong>{label}</Typography.Text>
          {detail && (
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              {detail}
            </Typography.Text>
          )}
        </Space>
        <Space direction="vertical" size={0} align="end">
          <Typography.Text style={{ fontFamily: mono, fontSize: 13 }}>
            {total > 0 ? `${done.toLocaleString()} of ${total.toLocaleString()}` : 'Starting'}
          </Typography.Text>
          {/*
            OF THIS STAGE, said out loud, and only for a whole second.

            An estimate that looks like it covers the whole run and turns out to
            cover a fifth of it is worse than none: somebody leaves, comes back,
            and trusts nothing the page says afterwards. And one that rounds to
            zero renders as "0s remaining" beside a bar that is still moving.
          */}
          {estimate !== undefined && estimate >= 1 && (
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              Estimated {formatDuration(estimate)} remaining in this stage
            </Typography.Text>
          )}
        </Space>
      </Space>

      {stripe ? <WorkingStripe label={label} /> : (
        <Progress
          percent={percent}
          status={finished ? 'normal' : 'active'}
          showInfo={false}
          strokeColor={stroke}
          trailColor={c.track}
        />
      )}
    </Space>
  )
}

/** A travelling stripe for work whose extent is known and whose position is not. */
function WorkingStripe({ label }: { label: string }) {
  return (
    <div
      role="progressbar"
      aria-label={label}
      aria-busy="true"
      style={{
        height: 8,
        borderRadius: 4,
        background: c.brandSoft,
        overflow: 'hidden',
        position: 'relative',
      }}
    >
      <div
        style={{
          position: 'absolute',
          inset: 0,
          width: '30%',
          borderRadius: 4,
          background: c.brand,
          animation: 'slm-working 1.4s ease-in-out infinite',
        }}
      />
    </div>
  )
}

/** What is in a worker's hands right now, and how many may be. */
function RunActive({ active, concurrency, concurrencyLabel, concurrencyHint, activeLimit }: RunPanelProps) {
  const chips = active ?? []
  const limit = activeLimit ?? 6
  if (chips.length === 0 && !concurrency) return null

  return (
    <Space size={6} wrap style={{ rowGap: 4 }}>
      {concurrency ? (
        <Tooltip title={concurrencyHint}>
          <Tag style={{ margin: 0, fontSize: 11 }}>
            {concurrency} {concurrencyLabel ?? 'in parallel'}
          </Tag>
        </Tooltip>
      ) : null}
      {chips.slice(0, limit).map((chip) => (
        <Tag
          key={chip.key}
          icon={chip.icon}
          style={{ margin: 0, fontFamily: mono, fontSize: 11 }}
        >
          {chip.label}
        </Tag>
      ))}
      {chips.length > limit && (
        <Typography.Text type="secondary" style={{ fontSize: 11 }}>
          +{chips.length - limit} more
        </Typography.Text>
      )}
    </Space>
  )
}

/**
 * The whole route, with what each finished stage cost.
 *
 * On screen because "eight minutes" is unreadable and "six of those eight were
 * the download" is a decision: it says the wait is somebody else's registry
 * rather than this Coordinator, and it is what a person quotes when they ask
 * for the concurrency to be raised.
 */
export function RunRoute({ steps }: { steps?: RunStep[] }) {
  if (!steps || steps.length === 0) return null
  return (
    <Space size={0} wrap style={{ rowGap: 6 }}>
      {steps.map((step, i) => (
        <span key={step.key} style={{ display: 'inline-flex', alignItems: 'center' }}>
          {i > 0 && <span style={{ color: c.text3, padding: '0 8px', fontSize: 11 }}>›</span>}
          <span
            style={{
              fontSize: 12,
              fontWeight: step.state === 'current' ? 600 : 400,
              color: step.state === 'current' ? c.text : step.state === 'done' ? c.text2 : c.text3,
            }}
          >
            {step.label}
            {step.seconds !== undefined && (
              <span style={{ color: c.text3, fontFamily: mono, fontSize: 11 }}>
                {' '}{formatDuration(step.seconds) ?? '0s'}
              </span>
            )}
          </span>
        </span>
      ))}
    </Space>
  )
}

/**
 * What is going WRONG, beside the bar.
 *
 * Positions do not belong here - the bar already says 96 of 157. This is for
 * the sentences a watcher can act on while there is still time to act.
 */
function RunNotes({ notes }: { notes?: string[] }) {
  if (!notes || notes.length === 0) return null
  return (
    <Space direction="vertical" size={8} style={{ width: '100%' }}>
      {notes.map((note) => (
        <Alert
          key={note}
          type="warning"
          showIcon
          message={note}
          // Ant's compact Alert is padded for a few words. These carry a
          // scanner's own error and wrap, and at the default the text sat
          // against the top border.
          style={{
            fontSize: 12.5,
            padding: '12px 16px',
            lineHeight: 1.55,
            alignItems: 'flex-start',
          }}
        />
      ))}
    </Space>
  )
}

/** The transcript, newest first, in a box that scrolls. */
function RunEventLog({ events, label }: { events?: RunEvent[]; label?: string }) {
  if (!events || events.length === 0) return null
  return (
    <div>
      <Typography.Text type="secondary" style={{ fontSize: 11 }}>
        {label ?? 'Run log'}
      </Typography.Text>
      {/*
        PADDED AT THE TOP, and that is not a nicety. The timeline's first dot
        sits slightly above its item's box, so a scroll container starting
        exactly at the list clipped the top of it - the newest line, which is
        the one somebody is watching.
      */}
      <div
        style={{
          marginTop: 8, maxHeight: 260, overflowY: 'auto',
          paddingRight: 8, paddingTop: 6,
        }}
      >
        <RunLog events={events} />
      </div>
    </div>
  )
}

/**
 * A run's transcript as a timeline, because a run IS one: a sequence of things
 * that happened, in order, with gaps between them that mean something.
 *
 * Newest first while the run is live, because the interesting line is the one
 * that just arrived; oldest first when it is over, because a finished
 * transcript is read as a sequence.
 */
export function RunLog({ events, newestFirst = true }: {
  events: RunEvent[]
  newestFirst?: boolean
}) {
  const ordered = newestFirst ? [...events].reverse() : events
  return (
    <Timeline
      mode="left"
      items={ordered.map((e, i) => ({
        key: `${e.sec ?? e.at ?? i}-${i}`,
        color: toneOf(e.kind),
        children: (
          <Space size={10} align="start" style={{ lineHeight: 1.35 }}>
            {/*
              The ELAPSED SECOND, not a clock time. A run is minutes long and
              every line is about how far into it something happened; wall-clock
              times would need subtracting before they said anything. The
              absolute time is on hover, for a log read a week later.
            */}
            <Typography.Text
              type="secondary"
              style={{
                fontFamily: mono, fontSize: 11, whiteSpace: 'nowrap',
                minWidth: '4.5ch', display: 'inline-block', textAlign: 'right',
              }}
              title={e.at ? new Date(e.at).toLocaleString() : undefined}
            >
              {e.sec !== undefined ? formatDuration(e.sec) ?? '0s' : shortClock(e.at)}
            </Typography.Text>
            {/*
              The kind is a WORD as well as a colour, on the lines where it
              changes what the line means. Everything on this page reads
              correctly in greyscale, and a transcript whose only signal is the
              colour of a six-pixel dot is the easiest place to forget that.
            */}
            {(e.kind === 'fail' || e.kind === 'warn') && (
              <Tag
                color={e.kind === 'fail' ? 'red' : 'orange'}
                style={{ margin: 0, fontSize: 10, lineHeight: '16px' }}
              >
                {e.kind === 'fail' ? 'Failed' : 'Warning'}
              </Tag>
            )}
            <span style={{ fontSize: 12, color: e.kind === 'fail' ? c.danger : c.text }}>
              {e.text}
              {e.repeat && e.repeat > 1 ? (
                <Typography.Text type="secondary" style={{ fontSize: 11 }}>
                  {' '}({e.repeat} times)
                </Typography.Text>
              ) : null}
            </span>
          </Space>
        ),
      }))}
    />
  )
}

/**
 * A finished run's transcript, behind a button.
 *
 * Every one of these jobs keeps its log, because "what happened when I pressed
 * that" is a question asked AFTER the run rather than during it - and a job
 * whose only durable output is one error sentence is one nobody can ask
 * anything about afterwards.
 *
 * Absent rather than disabled when there is no log: a control that cannot do
 * anything is a question the reader has to answer before ignoring it.
 */
export function RunLogButton({ events, title, label, note, truncatedNote, size = 'small' }: {
  events: RunEvent[]
  title: string
  /** The button's own text. "Run log" where the job has no better name. */
  label?: string
  note?: ReactNode
  truncatedNote?: string
  size?: 'small' | 'middle'
}) {
  const [open, setOpen] = useState(false)
  if (events.length === 0) return null

  const plain = events
    .map((e) => `${e.sec !== undefined ? formatDuration(e.sec) ?? '0s' : shortClock(e.at)} [${e.kind}] ${e.text}`)
    .join('\n')

  return (
    <>
      <Button size={size} icon={<FileTextOutlined />} onClick={() => setOpen(true)}>
        {label ?? 'Run log'}
      </Button>
      <Drawer
        title={title}
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
          {note && (
            <Typography.Text type="secondary" style={{ fontSize: 12 }}>
              {note}
            </Typography.Text>
          )}
          {truncatedNote && (
            <Alert type="info" showIcon message={truncatedNote} />
          )}
          <RunLog events={events} newestFirst={false} />
        </Space>
      </Drawer>
    </>
  )
}

/**
 * What colour a line's dot is.
 *
 * `ok` was once the body-text colour and `info` the muted one, so on a run
 * whose lines are almost all info and ok - which is every run that works - the
 * timeline was a column of grey dots and the two colours that meant something
 * were lost among fifty that meant nothing. A dot the same colour as the text
 * beside it is not a signal.
 */
function toneOf(kind: RunEventKind): string {
  switch (kind) {
    case 'fail': return c.danger
    case 'warn': return c.pending
    case 'ok': return c.ok
    default: return c.brand
  }
}

function shortClock(at?: string): string {
  if (!at) return '—'
  const d = new Date(at)
  return Number.isNaN(d.getTime())
    ? '—'
    : d.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit', second: '2-digit' })
}

/**
 * The security transcript's three levels, in the four the timeline speaks.
 *
 * `success` is a level the sync writes and the compliance run calls `ok`. One
 * mapping here rather than a conditional at each of the three call sites.
 */
export function runEventsFromLog(
  entries?: { at?: string; level: string; message: string; repeat?: number }[],
  startedAt?: string,
): RunEvent[] {
  if (!entries || entries.length === 0) return []
  const base = startedAt ? new Date(startedAt).getTime() : undefined
  return entries.map((e) => {
    const at = e.at ? new Date(e.at).getTime() : undefined
    return {
      at: e.at,
      sec: base !== undefined && at !== undefined && at >= base
        ? Math.round((at - base) / 1000)
        : undefined,
      kind: levelTone(e.level),
      text: e.message,
      repeat: e.repeat,
    }
  })
}

function levelTone(level: string): RunEventKind {
  switch (level) {
    case 'error': return 'fail'
    case 'warning': return 'warn'
    case 'success': return 'ok'
    default: return 'info'
  }
}
