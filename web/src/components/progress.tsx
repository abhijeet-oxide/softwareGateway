import { Progress, Tag, Tooltip, Typography } from 'antd'
import {
  CheckCircleFilled, ClockCircleOutlined, CloseCircleFilled, SyncOutlined,
} from '@ant-design/icons'
import type { Strategy } from '../api/types'
import { formatBytes, formatCount, formatSpeed, formatAbsolute, formatDuration } from '../domain/format'
import { NA } from './value'
import { semantic } from '../theme'

/**
 * The honest-numbers rule, made structural (docs/design/18 §6.1, 19 §6).
 *
 * There are two components here and they are not interchangeable:
 *
 *   <MeasuredProgress> takes REAL BYTE COUNTS and is the only thing in this
 *   application that may render a bar, a percentage, a speed or an ETA.
 *
 *   <StateStrip> takes TIMESTAMPS and a state, and cannot be made to look like
 *   a bar however it is styled.
 *
 * For the JFrog step we move the bytes, so we count them. For the Quay step
 * Quay pulls the content itself once we have configured it: we can see THAT a
 * sync started, THAT it finished and WHAT it produced, but not how many bytes
 * moved. A progress bar there would be a number derived from a timer, and
 * somebody would make a decision from it.
 *
 * MeasuredProgress refuses to render for a non-`copy` strategy, so the rule
 * cannot be broken by a caller who has not read this comment.
 *
 * A third case sits between them: work that IS ours and IS running, but whose
 * extent nobody knows yet — measuring a release walks a manifest tree whose
 * size is the thing being discovered. <WorkingBar> is for that. It animates,
 * so it reads as activity, and it carries no number, so it cannot be read as
 * a position.
 */

interface MeasuredProps {
  /** Bytes actually transferred. */
  transferred: number | undefined
  /** Bytes planned. */
  total: number | undefined
  /**
   * Bytes that did not have to move because the destination already had them.
   *
   * DONE WORK, and the bar has to say so. A release the destination already
   * holds is finished the moment planning discovers that, and a bar reading
   * `0 B of 63.7 GB · 0%` beside a summary reading `Saved 56.5 GB` was
   * describing a download that had nothing left to do as one that had not
   * started. The saved portion is drawn in its own colour, so "already there"
   * and "we moved it" stay distinguishable.
   */
  saved?: number | undefined
  /**
   * How the transfer is being performed. Anything but `copy` means the bytes
   * were not ours to count, and this component renders nothing.
   */
  strategy?: Strategy
  showText?: boolean
  speedBytesPerSecond?: number | undefined
}

export function MeasuredProgress({
  transferred, total, saved, strategy = 'copy', showText = true, speedBytesPerSecond,
}: MeasuredProps) {
  if (strategy !== 'copy') {
    // Not a fallback — a refusal. A caller reaching here has asked for a bar
    // over work whose bytes nobody counted.
    return (
      <NA reason="This work is performed by the destination registry, so there are no bytes for us to count. Its state is shown instead." />
    )
  }
  if (transferred === undefined || total === undefined || total <= 0) {
    return (
      <NA reason="The size of this work has not been established yet, so there is no percentage to show." />
    )
  }

  const alreadyThere = Math.max(0, Math.min(saved ?? 0, total))
  const done = Math.min(total, transferred + alreadyThere)
  const percent = (done / total) * 100
  const savedPercent = (alreadyThere / total) * 100

  return (
    <div>
      <Progress
        percent={Number(percent.toFixed(1))}
        // The green portion is what was already there. Ant Design draws it
        // from the left inside the same track, which is the right shape: it is
        // part of the same total, not a second bar.
        success={alreadyThere > 0 ? { percent: Number(savedPercent.toFixed(1)) } : undefined}
        size="small"
        strokeColor={percent >= 100 ? semantic.success : undefined}
      />
      {showText && (
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
          {formatBytes(done)} of {formatBytes(total)}
          {alreadyThere > 0 && ` · ${formatBytes(alreadyThere)} already there`}
          {speedBytesPerSecond !== undefined && ` · ${formatSpeed(speedBytesPerSecond)}`}
        </Typography.Text>
      )}
    </div>
  )
}

/**
 * The CONTENT half of a download: bytes, moved or found already there.
 *
 * A thin wrapper over MeasuredProgress that names what it is measuring, so the
 * bar beneath it can name what IT is measuring and the two cannot be read as
 * one number disagreeing with itself.
 */
export function ContentProgress(props: MeasuredProps) {
  return (
    <div>
      <Typography.Text type="secondary" style={{ fontSize: 12 }}>Content</Typography.Text>
      <MeasuredProgress {...props} />
    </div>
  )
}

/**
 * The COMPONENT half: how many of a release's parts have actually landed.
 *
 * # Why one bar was not enough
 *
 * A download moves content first — blobs, where all the bytes are and where all
 * the skipping happens — and then the manifests that name them. A release of
 * two hundred and sixty components has two hundred and sixty manifests of a few
 * kilobytes each, and until those land nothing is pullable.
 *
 * So the byte bar reaches 100% while the download is genuinely a third done. It
 * is not wrong; it is answering "how many bytes are left", and somebody
 * watching it is asking "how much longer". This answers the second question,
 * and the pair of them is the honest account: the bytes are there, the naming
 * is still happening.
 *
 * Jobs rather than components, because that is what the transfer counts and
 * what settles one at a time. The Contents table beside it says which KINDS
 * have landed, which is the same progress at the resolution somebody acts on.
 */
export function ComponentProgress({
  progress, live,
}: {
  progress?: {
    jobsPlanned?: number
    jobsDone?: number
    jobsFailed?: number
    jobsInFlight?: number
  }
  live: boolean
}) {
  const total = progress?.jobsPlanned ?? 0
  if (total <= 0) {
    return (
      <div>
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>Components</Typography.Text>
        <div>
          <NA reason="This download has not been planned yet, so there is no count of its parts." />
        </div>
      </div>
    )
  }

  const done = Math.min(total, progress?.jobsDone ?? 0)
  const failed = progress?.jobsFailed ?? 0
  const percent = (done / total) * 100

  return (
    <div>
      <Typography.Text type="secondary" style={{ fontSize: 12 }}>Components</Typography.Text>
      <Progress
        percent={Number(percent.toFixed(1))}
        size="small"
        status={failed > 0 ? 'exception' : percent >= 100 ? 'success' : live ? 'active' : 'normal'}
      />
      <Typography.Text type="secondary" style={{ fontSize: 12 }}>
        {formatCount(done)} of {formatCount(total)} parts in place
        {(progress?.jobsInFlight ?? 0) > 0 && ` · ${progress?.jobsInFlight} moving now`}
        {failed > 0 && ` · ${formatCount(failed)} failed`}
      </Typography.Text>
    </div>
  )
}

/**
 * A download's progress in ONE CELL: how far, how long, how much longer.
 *
 * The three belong together. A bar alone says how far without saying whether
 * that took a minute or an afternoon; an elapsed column alone says how long
 * without saying how far. And the number an operator actually wants — when it
 * will be done — is the one that is never on screen, because it is the only
 * one of the three that has to be derived.
 *
 * The ETA is derived HONESTLY or not at all: bytes we moved, over the time we
 * spent moving them, applied to what is left. It is absent for a settled
 * download (there is nothing left), for a delegated one (the bytes were not
 * ours to count), and while nothing has moved yet (a rate from a zero
 * numerator is a guess wearing a number's clothes).
 */
export function DownloadProgress({
  transferred, total, saved, strategy = 'copy', elapsedSeconds, live,
}: {
  transferred: number | undefined
  total: number | undefined
  /** Bytes the destination already had. Done work — see MeasuredProgress. */
  saved?: number | undefined
  strategy?: Strategy
  /** Seconds spent moving bytes so far. */
  elapsedSeconds: number | undefined
  /** Whether this download is still going. */
  live: boolean
}) {
  const speed = elapsedSeconds && transferred && elapsedSeconds > 0
    ? transferred / elapsedSeconds
    : undefined
  // What is LEFT, which is neither moved nor already there. Counting the saved
  // bytes as remaining is how a download with nothing left to do came to
  // report an ETA of forever.
  const remaining = total !== undefined && transferred !== undefined
    ? Math.max(0, total - transferred - (saved ?? 0))
    : undefined
  const eta = live && speed && remaining !== undefined && remaining > 0
    ? remaining / speed
    : undefined

  return (
    <div style={{ minWidth: 180 }}>
      <MeasuredProgress
        transferred={transferred}
        total={total}
        saved={saved}
        strategy={strategy}
        showText={false}
      />
      <Typography.Text type="secondary" style={{ fontSize: 11 }}>
        {formatDuration(elapsedSeconds) ?? 'not started'} elapsed
        {eta !== undefined ? ` · ~${formatDuration(eta)} left` : ''}
        {/*
          "Estimating" only while there is something left to estimate. A
          download whose content the destination already holds has nothing
          remaining, and saying it is estimating an arrival is describing a
          wait that is over.
        */}
        {live && eta === undefined && remaining === 0 ? ' · nothing left to move' : ''}
        {live && eta === undefined && remaining !== 0 && strategy === 'copy'
          ? ' · estimating…'
          : ''}
      </Typography.Text>
    </div>
  )
}

export type StripState = 'pending' | 'running' | 'done' | 'failed'

interface StripProps {
  state: StripState
  /** What is true, in words. Never a number we did not measure. */
  label: string
  /** The timestamps that ARE the report for this kind of work. */
  events?: { label: string; at?: string }[]
  /** The destination's own message, verbatim, when it failed. */
  message?: string
}

/**
 * State and timestamps, for work performed by somebody else.
 *
 * Deliberately a row of labelled moments rather than anything with a track or
 * a fill, so it can never be misread as measured progress at a glance.
 */
export function StateStrip({ state, label, events = [], message }: StripProps) {
  const icon = {
    pending: <ClockCircleOutlined style={{ color: semantic.neutral }} />,
    running: <SyncOutlined spin style={{ color: '#0057B8' }} />,
    done: <CheckCircleFilled style={{ color: semantic.success }} />,
    failed: <CloseCircleFilled style={{ color: semantic.error }} />,
  }[state]

  return (
    <div
      style={{
        border: '1px dashed #D6DCE5',
        borderRadius: 4,
        padding: '10px 12px',
        background: '#FBFCFD',
      }}
    >
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: events.length ? 8 : 0 }}>
        {icon}
        <Typography.Text strong>{label}</Typography.Text>
      </div>

      {events.length > 0 && (
        <div style={{ display: 'flex', flexWrap: 'wrap', gap: 8 }}>
          {events.map((e) => (
            <Tooltip key={e.label} title={e.at ? formatAbsolute(e.at) : 'Not reached yet'}>
              <Tag color={e.at ? 'default' : undefined} style={{ marginInlineEnd: 0 }}>
                {e.label}: {e.at ? new Date(e.at).toLocaleTimeString('en-GB', {
                  hour: '2-digit', minute: '2-digit',
                }) : <NA />}
              </Tag>
            </Tooltip>
          ))}
        </div>
      )}

      {message && (
        <Typography.Paragraph
          type={state === 'failed' ? 'danger' : 'secondary'}
          style={{ marginTop: 8, marginBottom: 0, fontSize: 12 }}
        >
          {message}
        </Typography.Paragraph>
      )}
    </div>
  )
}


/**
 * Work in progress whose extent is not known.
 *
 * Deliberately NOT a percentage. Measuring a release walks a manifest tree to
 * find out how big it is — the total is the answer, not the input — so any
 * percentage would be invented. What is true and worth showing is that it is
 * running, what it is doing, and how long it has been doing it.
 *
 * The stripe slides rather than fills, so it cannot be mistaken for a position
 * at a glance. Elapsed time is the honest counterpart to a percentage: it does
 * not say how far along, but it does tell somebody whether to keep waiting.
 */
export function WorkingBar({
  label, detail, elapsedSeconds,
}: {
  label: string
  detail?: string
  elapsedSeconds?: number
}) {
  return (
    <div>
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 6 }}>
        <SyncOutlined spin style={{ color: '#0057B8' }} />
        <Typography.Text strong style={{ fontSize: 13 }}>{label}</Typography.Text>
        {elapsedSeconds !== undefined && (
          <Typography.Text type="secondary" style={{ fontSize: 12 }}>
            {formatDuration(elapsedSeconds)} elapsed
          </Typography.Text>
        )}
      </div>

      <div
        role="progressbar"
        aria-label={label}
        aria-busy="true"
        style={{
          height: 6,
          borderRadius: 3,
          background: '#EEF2F6',
          overflow: 'hidden',
        }}
      >
        <div
          style={{
            height: '100%',
            width: '35%',
            borderRadius: 3,
            background: '#0057B8',
            animation: 'slm-working 1.4s ease-in-out infinite',
          }}
        />
      </div>

      {detail && (
        <Typography.Text type="secondary" style={{ fontSize: 12, display: 'block', marginTop: 6 }}>
          {detail}
        </Typography.Text>
      )}
    </div>
  )
}
