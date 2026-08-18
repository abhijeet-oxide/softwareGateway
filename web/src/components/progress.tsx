import { Progress, Tag, Tooltip, Typography } from 'antd'
import {
  CheckCircleFilled, ClockCircleOutlined, CloseCircleFilled, SyncOutlined,
} from '@ant-design/icons'
import type { Strategy } from '../api/types'
import { formatBytes, formatSpeed, formatAbsolute, formatDuration } from '../domain/format'
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
   * How the transfer is being performed. Anything but `copy` means the bytes
   * were not ours to count, and this component renders nothing.
   */
  strategy?: Strategy
  showText?: boolean
  speedBytesPerSecond?: number | undefined
}

export function MeasuredProgress({
  transferred, total, strategy = 'copy', showText = true, speedBytesPerSecond,
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

  const percent = Math.min(100, (transferred / total) * 100)
  return (
    <div>
      <Progress
        percent={Number(percent.toFixed(1))}
        size="small"
        strokeColor={percent >= 100 ? semantic.success : undefined}
      />
      {showText && (
        <Typography.Text type="secondary" style={{ fontSize: 12 }}>
          {formatBytes(transferred)} of {formatBytes(total)}
          {speedBytesPerSecond !== undefined && ` · ${formatSpeed(speedBytesPerSecond)}`}
        </Typography.Text>
      )}
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
