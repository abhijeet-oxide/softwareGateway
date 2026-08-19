import type { ReactNode } from 'react'
import { Statistic, Tooltip, Typography } from 'antd'
import { NOT_AVAILABLE } from '../domain/format'

/**
 * How this application says "we do not have this".
 *
 * One rendering, everywhere: italic, secondary, the words `N/A`. Never a dash
 * that could be mistaken for a minus sign or a placeholder, and never a zero -
 * "nothing was measured" and "the measurement was zero" are different facts and
 * the second one is a claim.
 *
 * Where the reason is known it goes on a tooltip, because "why is this blank"
 * is the immediate next question and the answer is usually one sentence.
 */
export function NA({ reason }: { reason?: string }) {
  const text = (
    <Typography.Text type="secondary" italic>
      {NOT_AVAILABLE}
    </Typography.Text>
  )
  return reason ? <Tooltip title={reason}>{text}</Tooltip> : text
}

/**
 * Renders a formatter's output, or `N/A` when it had nothing to report.
 *
 * Every formatter in domain/format.ts returns `string | null`, so wrapping its
 * output in this is the only thing a caller has to remember - and forgetting
 * does not typecheck.
 */
export function Value({
  children, reason, suffix,
}: {
  children: string | null | undefined
  /** One sentence explaining why the value is absent. */
  reason?: string
  /** Rendered after the value, and omitted entirely when there is none. */
  suffix?: ReactNode
}) {
  if (children === null || children === undefined || children === '') {
    return <NA reason={reason} />
  }
  return (
    <>
      {children}
      {suffix}
    </>
  )
}

/**
 * A Statistic whose value may be absent.
 *
 * Ant Design's Statistic takes a string, not a node, so the italic secondary
 * treatment is applied through valueStyle rather than by rendering <NA/>.
 */
export function Stat({
  title, value, reason, valueStyle, prefix, suffix,
}: {
  title: ReactNode
  value: string | null | undefined
  reason?: string
  valueStyle?: React.CSSProperties
  prefix?: ReactNode
  suffix?: ReactNode
}) {
  const absent = value === null || value === undefined || value === ''
  const stat = (
    <Statistic
      title={title}
      value={absent ? NOT_AVAILABLE : value}
      prefix={prefix}
      suffix={absent ? undefined : suffix}
      valueStyle={
        absent
          ? { ...valueStyle, fontStyle: 'italic', color: 'rgba(0,0,0,0.45)' }
          : valueStyle
      }
    />
  )
  return absent && reason ? <Tooltip title={reason}>{stat}</Tooltip> : stat
}
